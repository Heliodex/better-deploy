package outbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/heliodex/better-deploy/wt/packages/v2/storage"
)

const (
	defaultTimeout    = 15 * time.Second
	defaultUserAgent  = "wt-outbox"
	timestampHeader   = "X-Webhook-Timestamp"
	deliveryIDHeader  = "X-Webhook-Delivery-Id"
	signatureHeader   = "X-Webhook-Signature"
	maxResponseBody   = 4 << 10
	maxLastErrorChars = 1024
)

// dueRecords loads up to limit pending messages whose next attempt is due, oldest first.
func dueRecords(ctx context.Context, client *storage.Client, table, nowText string, limit int) ([]Record, error) {
	rows, err := client.Query(ctx, fmt.Sprintf(`select %s from %s
		where status = 'pending' and next_attempt_at <= ?
		order by next_attempt_at asc, id asc limit ?`, recordColumns, table), nowText, limit)
	if err != nil {
		return nil, err
	}

	records := []Record{}

	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			_ = rows.Close()

			return nil, err
		}

		records = append(records, record)
	}

	if err := rows.Close(); err != nil {
		return nil, err
	}

	return records, nil
}

// claimRecord moves a pending message to sending under a lease. It reports false when another worker claimed it first.
func claimRecord(ctx context.Context, client *storage.Client, table, id, nowText, leaseText string) (bool, error) {
	claim, err := client.Exec(ctx, fmt.Sprintf(`update %s
		set status = 'sending', attempts = attempts + 1, lease_until = ?
		where id = ? and status = 'pending' and next_attempt_at <= ?`, table), leaseText, id, nowText)
	if err != nil {
		return false, err
	}

	return claim.RowsAffected > 0, nil
}

// recordMessage returns the message fields of a stored record.
func recordMessage(record Record) Message {
	return Message{
		ID:          record.ID,
		Event:       record.Event,
		DedupeKey:   record.DedupeKey,
		Payload:     record.Payload,
		URL:         record.URL,
		Headers:     record.Headers,
		Metadata:    record.Metadata,
		MaxAttempts: record.MaxAttempts,
		DeliveryID:  record.DeliveryID,
	}
}

// permanentFailure reports whether a status means the receiver will not accept the message later.
// Request timeouts and rate limits are retryable, so they are excluded from the 4xx range.
func permanentFailure(status int) bool {
	return status >= http.StatusBadRequest && status < http.StatusInternalServerError &&
		status != http.StatusRequestTimeout && status != http.StatusTooManyRequests
}

// attemptLimit returns the record's own attempt limit, or the outbox default when it has none.
func (o Outbox) attemptLimit(record Record) int {
	if record.MaxAttempts > 0 {
		return record.MaxAttempts
	}

	return o.maxAttempts()
}

// truncate bounds the stored error text.
func truncate(value string) string {
	if len(value) <= maxLastErrorChars {
		return value
	}

	return value[:maxLastErrorChars]
}

// sendErrText renders the stored failure reason.
func sendErrText(status int, err error) string {
	if err != nil {
		return err.Error()
	}

	return fmt.Sprintf("HTTP %d", status)
}

// markDelivered records a successful attempt.
func (o Outbox) markDelivered(ctx context.Context, client *storage.Client, table, id string, status int, now string) error {
	_, err := client.Exec(ctx, fmt.Sprintf(`update %s
		set status = 'delivered', delivered_at = ?, last_http_status = ?, last_error = '', lease_until = NULL
		where id = ?`, table), now, status, id)

	return err
}

// markFailed records a permanent failure.
func (o Outbox) markFailed(ctx context.Context, client *storage.Client, table, id string, status int, message, now string) error {
	_, err := client.Exec(ctx, fmt.Sprintf(`update %s
		set status = 'failed', next_attempt_at = ?, last_http_status = ?, last_error = ?, lease_until = NULL
		where id = ?`, table), now, status, truncate(message), id)

	return err
}

// markRetry records a retryable failure.
func (o Outbox) markRetry(ctx context.Context, client *storage.Client, table, id string, status int, message, next string) error {
	_, err := client.Exec(ctx, fmt.Sprintf(`update %s
		set status = 'pending', next_attempt_at = ?, last_http_status = ?, last_error = ?, lease_until = NULL
		where id = ?`, table), next, status, truncate(message), id)

	return err
}

// lease returns the configured claim lease.
func (o Outbox) lease() time.Duration {
	if o.Lease > 0 {
		return o.Lease
	}

	return DefaultLease
}

// headerOrDefault returns a configured header name or its default.
func headerOrDefault(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}

	return fallback
}

// HTTPSender posts one signed delivery with net/http.
type HTTPSender struct {
	Client          *http.Client
	TimestampHeader string
	DeliveryHeader  string
	SignatureHeader string
	UserAgent       string
}

// Send performs one HTTP POST and returns its status. Redirects follow the configured client.
func (s HTTPSender) Send(ctx context.Context, delivery Delivery) (int, error) {
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, delivery.URL, bytes.NewReader(delivery.Payload))
	if err != nil {
		return 0, err
	}

	for key, value := range delivery.Headers {
		req.Header.Set(key, value)
	}

	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	userAgent := s.UserAgent
	if userAgent == "" {
		userAgent = defaultUserAgent
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set(headerOrDefault(s.TimestampHeader, timestampHeader), delivery.Timestamp)
	req.Header.Set(headerOrDefault(s.DeliveryHeader, deliveryIDHeader), delivery.DeliveryID)
	req.Header.Set(headerOrDefault(s.SignatureHeader, signatureHeader), delivery.Signature)

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))

	return resp.StatusCode, nil
}

// sender returns the configured sender or the default HTTP sender.
//
//nolint:ireturn // The caller may supply any Sender implementation.
func (o Outbox) sender() Sender {
	if o.Sender != nil {
		return o.Sender
	}

	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}

	return HTTPSender{Client: client}
}

// deliverClaimed sends one message this worker has claimed, then records it as delivered, failed or queued for retry.
func (o Outbox) deliverClaimed(ctx context.Context, client *storage.Client, table string, sender Sender, record Record, now time.Time, summary *Summary) error {
	attempt := record.Attempts + 1
	timestamp, signature := o.Signer.Sign(record.DeliveryID, record.Payload)
	delivery := Delivery{
		Message:   recordMessage(record),
		Attempt:   attempt,
		Timestamp: timestamp,
		Signature: signature,
	}

	status, sendErr := sender.Send(ctx, delivery)
	if sendErr != nil && ctx.Err() != nil {
		return ctx.Err()
	}

	nowText := now.Format(timeFormat)
	if status >= http.StatusOK && status < http.StatusMultipleChoices {
		if err := o.markDelivered(ctx, client, table, record.ID, status, nowText); err != nil {
			return err
		}

		summary.Delivered++

		return nil
	}

	errorText := sendErrText(status, sendErr)
	if permanentFailure(status) || attempt >= o.attemptLimit(record) {
		if err := o.markFailed(ctx, client, table, record.ID, status, errorText, nowText); err != nil {
			return err
		}

		summary.Failed++

		return nil
	}

	next := now.Add(o.retryDelay(attempt)).Format(timeFormat)
	if err := o.markRetry(ctx, client, table, record.ID, status, errorText, next); err != nil {
		return err
	}

	summary.Retried++

	return nil
}

// DeliverDue reaps expired leases, claims due messages and sends them. A non-2xx response is retried, except for
// permanent 4xx responses other than 408 and 429, which are marked failed.
func (o Outbox) DeliverDue(ctx context.Context, client *storage.Client, limit int) (Summary, error) {
	if len(o.Signer.Secret) == 0 {
		return Summary{}, errors.New("outbox: signing secret is empty")
	}

	table, err := o.tableName()
	if err != nil {
		return Summary{}, err
	}

	if limit <= 0 || limit > 200 {
		limit = DefaultLimit
	}

	now := o.now()
	nowText := now.Format(timeFormat)
	leaseText := now.Add(o.lease()).Format(timeFormat)

	reaped, err := client.Exec(ctx, fmt.Sprintf(`update %s set status = 'pending', lease_until = NULL
		where status = 'sending' and lease_until is not null and lease_until <= ?`, table), nowText)
	if err != nil {
		return Summary{}, err
	}

	summary := Summary{Reaped: int(reaped.RowsAffected)}

	records, err := dueRecords(ctx, client, table, nowText, limit)
	if err != nil {
		return summary, err
	}

	sender := o.sender()

	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return summary, err
		}

		claimed, err := claimRecord(ctx, client, table, record.ID, nowText, leaseText)
		if err != nil {
			return summary, err
		}

		if !claimed {
			continue
		}

		if err := o.deliverClaimed(ctx, client, table, sender, record, now, &summary); err != nil {
			return summary, err
		}
	}

	return summary, nil
}

// Package outbox stores signed HTTP deliveries and sends them with retries.
//
// The caller owns the database migration: include Schema statements in the app's migration, enqueue a Message in
// the same transaction as the state change, and call DeliverDue from a schedule or job.
package outbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/heliodex/better-deploy/wt/packages/sign"
	"github.com/heliodex/better-deploy/wt/packages/storage"
)

const (
	// DefaultTable is the SQLite table used when Outbox.Table is empty.
	DefaultTable = "webhook_outbox"
	// DefaultLimit is the number of deliveries one DeliverDue call processes.
	DefaultLimit = 20
	// DefaultMaxAttempts is the retry limit for a message.
	DefaultMaxAttempts = 8
	// DefaultLease is how long a claimed delivery is reserved before another worker may retry it.
	DefaultLease = 2 * time.Minute

	timeFormat = "2006-01-02T15:04:05.000000000Z07:00"

	statusPending   = "pending"
	statusSending   = "sending"
	statusDelivered = "delivered"
	statusFailed    = "failed"
)

// Status values returned on a Record.
const (
	StatusPending   = statusPending
	StatusSending   = statusSending
	StatusDelivered = statusDelivered
	StatusFailed    = statusFailed
)

// DefaultRetry is the default retry ladder after the first failed attempt.
var DefaultRetry = []time.Duration{
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	6 * time.Hour,
	12 * time.Hour,
	24 * time.Hour,
}

var tablePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Message is one webhook delivery waiting to be sent.
type Message struct {
	ID          string
	Event       string
	DedupeKey   string
	Payload     []byte
	URL         string
	Headers     map[string]string
	Metadata    map[string]string
	RunAt       time.Time
	MaxAttempts int
	DeliveryID  string
}

// Delivery is one signed attempt handed to a Sender.
type Delivery struct {
	Message

	Attempt   int
	Timestamp string
	Signature string
}

// Record is the stored state of one message.
type Record struct {
	ID             string
	Event          string
	DedupeKey      string
	Payload        []byte
	URL            string
	Headers        map[string]string
	Metadata       map[string]string
	Status         string
	Attempts       int
	MaxAttempts    int
	NextAttemptAt  time.Time
	LastHTTPStatus int
	LastError      string
	DeliveryID     string
	CreatedAt      time.Time
	DeliveredAt    *time.Time
}

// Summary counts one DeliverDue run.
type Summary struct {
	Delivered int
	Retried   int
	Failed    int
	Reaped    int
}

// Outbox owns one delivery table and its retry policy.
type Outbox struct {
	Table       string
	Signer      sign.Signer
	HTTP        *http.Client
	Now         func() time.Time
	Retry       []time.Duration
	MaxAttempts int
	Lease       time.Duration
	Sender      Sender
}

// Sender performs one delivery attempt. status is the HTTP status; err is a transport error.
type Sender interface {
	Send(ctx context.Context, delivery Delivery) (status int, err error)
}

// Schema returns the migration statements for the outbox table.
func (o Outbox) Schema() ([]storage.Statement, error) {
	table, err := o.tableName()
	if err != nil {
		return nil, err
	}

	return []storage.Statement{
		{SQL: fmt.Sprintf(`create table if not exists %s (
			id text primary key,
			event text not null,
			dedupe_key text not null default '',
			payload text not null,
			url text not null,
			headers text not null default '{}',
			metadata text not null default '{}',
			status text not null default 'pending' check (status in ('pending','sending','delivered','failed')),
			attempts integer not null default 0,
			max_attempts integer not null default 8,
			next_attempt_at text not null,
			lease_until text,
			last_http_status integer not null default 0,
			last_error text not null default '',
			delivery_id text not null,
			created_at text not null,
			delivered_at text
		)`, table)},
		{SQL: fmt.Sprintf(`create unique index if not exists %s_dedupe_idx on %s(dedupe_key) where dedupe_key <> ''`, table, table)},
		{SQL: fmt.Sprintf(`create index if not exists %s_due_idx on %s(status, next_attempt_at)`, table, table)},
		{SQL: fmt.Sprintf(`create index if not exists %s_created_idx on %s(created_at)`, table, table)},
	}, nil
}

// Prepare fills defaults and returns the insert statement, so it can join the caller's transaction.
func (o Outbox) Prepare(msg Message) (Message, storage.Statement, error) {
	table, err := o.tableName()
	if err != nil {
		return Message{}, storage.Statement{}, err
	}

	if strings.TrimSpace(msg.Event) == "" {
		return Message{}, storage.Statement{}, errors.New("outbox: event is required")
	}

	if strings.TrimSpace(msg.URL) == "" {
		return Message{}, storage.Statement{}, errors.New("outbox: url is required")
	}

	if msg.ID == "" {
		msg.ID = newID("out")
	}

	if msg.DeliveryID == "" {
		msg.DeliveryID = newID("del")
	}

	if msg.MaxAttempts <= 0 {
		msg.MaxAttempts = o.maxAttempts()
	}

	if msg.RunAt.IsZero() {
		msg.RunAt = o.now()
	}

	headers, err := encodeMap(msg.Headers)
	if err != nil {
		return Message{}, storage.Statement{}, err
	}

	metadata, err := encodeMap(msg.Metadata)
	if err != nil {
		return Message{}, storage.Statement{}, err
	}

	statement := storage.Statement{
		SQL: fmt.Sprintf(`insert or ignore into %s
			(id, event, dedupe_key, payload, url, headers, metadata, status, attempts, max_attempts, next_attempt_at, delivery_id, created_at)
			values (?, ?, ?, ?, ?, ?, ?, 'pending', 0, ?, ?, ?, ?)`, table),
		Args: []any{
			msg.ID,
			msg.Event,
			msg.DedupeKey,
			string(msg.Payload),
			msg.URL,
			headers,
			metadata,
			msg.MaxAttempts,
			msg.RunAt.UTC().Format(timeFormat),
			msg.DeliveryID,
			o.nowText(),
		},
	}

	return msg, statement, nil
}

// PrepareTransaction is Prepare for a storage.Transaction statement list.
func (o Outbox) PrepareTransaction(msg Message) (Message, storage.TransactionStatement, error) {
	prepared, statement, err := o.Prepare(msg)
	if err != nil {
		return Message{}, storage.TransactionStatement{}, err
	}

	return prepared, storage.TransactionStatement{SQL: statement.SQL, Args: statement.Args}, nil
}

// Enqueue stores one message and returns its record. A matching DedupeKey returns the existing record.
func (o Outbox) Enqueue(ctx context.Context, client *storage.Client, msg Message) (Record, error) {
	prepared, statement, err := o.Prepare(msg)
	if err != nil {
		return Record{}, err
	}

	result, err := client.Exec(ctx, statement.SQL, statement.Args...)
	if err != nil {
		return Record{}, err
	}

	if result.RowsAffected == 0 && prepared.DedupeKey != "" {
		return o.getByDedupe(ctx, client, prepared.DedupeKey)
	}

	return o.Get(ctx, client, prepared.ID)
}

// Get returns one record by id.
func (o Outbox) Get(ctx context.Context, client *storage.Client, id string) (Record, error) {
	table, err := o.tableName()
	if err != nil {
		return Record{}, err
	}

	row := client.QueryRow(ctx, fmt.Sprintf(`select %s from %s where id = ?`, recordColumns, table), id)

	return scanRecord(row)
}

// getByDedupe returns the record for a dedupe key.
func (o Outbox) getByDedupe(ctx context.Context, client *storage.Client, key string) (Record, error) {
	table, err := o.tableName()
	if err != nil {
		return Record{}, err
	}

	row := client.QueryRow(ctx, fmt.Sprintf(`select %s from %s where dedupe_key = ?`, recordColumns, table), key)

	return scanRecord(row)
}

// List returns the newest records, newest first.
func (o Outbox) List(ctx context.Context, client *storage.Client, limit int) ([]Record, error) {
	table, err := o.tableName()
	if err != nil {
		return nil, err
	}

	if limit <= 0 || limit > 200 {
		limit = 50
	}

	rows, err := client.Query(ctx, fmt.Sprintf(`select %s from %s order by created_at desc limit ?`, recordColumns, table), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := []Record{}

	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}

		records = append(records, record)
	}

	return records, rows.Err()
}

// recordColumns is the selected column order used by scanRecord.
const recordColumns = `id, event, dedupe_key, payload, url, headers, metadata, status, attempts, max_attempts,
	next_attempt_at, coalesce(last_http_status, 0), coalesce(last_error, ''), delivery_id, created_at, coalesce(delivered_at, '')`

// scanRecord scans one row of recordColumns.
func scanRecord(row interface{ Scan(dest ...any) error }) (Record, error) {
	var (
		record                              Record
		payload, headers, metadata          string
		nextAttempt, createdAt, deliveredAt string
	)

	if err := row.Scan(
		&record.ID,
		&record.Event,
		&record.DedupeKey,
		&payload,
		&record.URL,
		&headers,
		&metadata,
		&record.Status,
		&record.Attempts,
		&record.MaxAttempts,
		&nextAttempt,
		&record.LastHTTPStatus,
		&record.LastError,
		&record.DeliveryID,
		&createdAt,
		&deliveredAt,
	); err != nil {
		return Record{}, err
	}

	record.Payload = []byte(payload)

	if err := decodeMap(headers, &record.Headers); err != nil {
		return Record{}, err
	}

	if err := decodeMap(metadata, &record.Metadata); err != nil {
		return Record{}, err
	}

	var err error

	record.NextAttemptAt, err = parseTime(nextAttempt)
	if err != nil {
		return Record{}, err
	}

	record.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return Record{}, err
	}

	if deliveredAt != "" {
		delivered, err := parseTime(deliveredAt)
		if err != nil {
			return Record{}, err
		}

		record.DeliveredAt = &delivered
	}

	return record, nil
}

// tableName returns the validated table name.
func (o Outbox) tableName() (string, error) {
	table := o.Table
	if table == "" {
		table = DefaultTable
	}

	if !tablePattern.MatchString(table) {
		return "", fmt.Errorf("outbox: invalid table name %q", table)
	}

	return table, nil
}

// maxAttempts returns the configured retry limit.
func (o Outbox) maxAttempts() int {
	if o.MaxAttempts > 0 {
		return o.MaxAttempts
	}

	return DefaultMaxAttempts
}

// retryDelay returns the delay after an attempt.
func (o Outbox) retryDelay(attempt int) time.Duration {
	retry := o.Retry
	if len(retry) == 0 {
		retry = DefaultRetry
	}

	if attempt < 1 {
		attempt = 1
	}

	if attempt > len(retry) {
		attempt = len(retry)
	}

	return retry[attempt-1]
}

// now returns the configured clock in UTC.
func (o Outbox) now() time.Time {
	if o.Now != nil {
		return o.Now().UTC()
	}

	return time.Now().UTC()
}

// nowText formats the current time for storage.
func (o Outbox) nowText() string {
	return o.now().Format(timeFormat)
}

// encodeMap encodes a string map as compact JSON.
func encodeMap(values map[string]string) (string, error) {
	if len(values) == 0 {
		return "{}", nil
	}

	data, err := json.Marshal(values)
	if err != nil {
		return "", err
	}

	return string(data), nil
}

// decodeMap decodes a string map, treating empty JSON as an empty map.
func decodeMap(value string, out *map[string]string) error {
	if value == "" {
		*out = map[string]string{}
		return nil
	}

	return json.Unmarshal([]byte(value), out)
}

// parseTime parses a stored timestamp.
func parseTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}

	return time.Parse(timeFormat, value)
}

// newID returns a prefixed random identifier.
func newID(prefix string) string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return prefix + "_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}

	return prefix + "_" + hex.EncodeToString(raw[:])
}

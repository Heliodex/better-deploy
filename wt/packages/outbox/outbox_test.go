//go:build !wasip1

package outbox

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/heliodex/better-deploy/wt/packages/sign"
	"github.com/heliodex/better-deploy/wt/packages/storage"
)

// recordingSender records each delivery and returns the configured statuses in order.
type recordingSender struct {
	statuses []int
	errs     []error
	calls    []Delivery
}

func (s *recordingSender) Send(_ context.Context, delivery Delivery) (int, error) {
	s.calls = append(s.calls, delivery)

	index := len(s.calls) - 1
	if index < len(s.errs) && s.errs[index] != nil {
		return 0, s.errs[index]
	}

	if index < len(s.statuses) {
		return s.statuses[index], nil
	}

	return http.StatusOK, nil
}

// testOutbox creates a migrated outbox over an in-memory storage service.
func testOutbox(t *testing.T) (*Outbox, *storage.Client, *time.Time) {
	t.Helper()

	server, err := storage.NewTestServer()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(server.Close)

	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	client := server.NewClient(server.Client())
	box := Outbox{
		Signer: sign.Signer{Secret: []byte("secret"), Now: func() time.Time { return now }},
		Now:    func() time.Time { return now },
	}

	schema, err := box.Schema()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.Batch(context.Background(), schema...); err != nil {
		t.Fatal(err)
	}

	return &box, client, &now
}

func testMessage(key string) Message {
	return Message{
		Event:     "ship.approved",
		DedupeKey: key,
		Payload:   []byte(`{"approved":true}`),
		URL:       "https://example.com/hook",
		Headers:   map[string]string{"X-Custom": "yes"},
		Metadata:  map[string]string{"submission": "sub_1"},
	}
}

func TestEnqueueAndDedupe(t *testing.T) {
	box, client, _ := testOutbox(t)

	first, err := box.Enqueue(context.Background(), client, testMessage("decision:1"))
	if err != nil {
		t.Fatal(err)
	}

	if first.Status != StatusPending || first.Attempts != 0 || first.MaxAttempts != DefaultMaxAttempts {
		t.Errorf("record = %+v", first)
	}

	if first.DeliveryID == "" || first.Headers["X-Custom"] != "yes" || first.Metadata["submission"] != "sub_1" {
		t.Errorf("record metadata = %+v", first)
	}

	second, err := box.Enqueue(context.Background(), client, testMessage("decision:1"))
	if err != nil {
		t.Fatal(err)
	}

	if second.ID != first.ID {
		t.Errorf("dedupe returned %q, want %q", second.ID, first.ID)
	}

	records, err := box.List(context.Background(), client, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(records) != 1 {
		t.Errorf("records = %d, want 1", len(records))
	}
}

func TestPrepareTransaction(t *testing.T) {
	box, _, _ := testOutbox(t)

	message, statement, err := box.PrepareTransaction(testMessage("transaction:1"))
	if err != nil {
		t.Fatal(err)
	}

	if message.ID == "" || statement.SQL == "" || len(statement.Args) == 0 {
		t.Errorf("message = %+v statement = %+v", message, statement)
	}
}

func TestDeliverDueSendsSignedPayload(t *testing.T) {
	box, client, _ := testOutbox(t)
	sender := &recordingSender{}
	box.Sender = sender

	record, err := box.Enqueue(context.Background(), client, testMessage(""))
	if err != nil {
		t.Fatal(err)
	}

	summary, err := box.DeliverDue(context.Background(), client, 10)
	if err != nil {
		t.Fatal(err)
	}

	if summary.Delivered != 1 || len(sender.calls) != 1 {
		t.Errorf("summary = %+v calls = %d", summary, len(sender.calls))
	}

	delivery := sender.calls[0]
	if delivery.Attempt != 1 || delivery.DeliveryID != record.DeliveryID {
		t.Errorf("delivery = %+v", delivery)
	}

	if err := box.Signer.Verify(delivery.Timestamp, delivery.DeliveryID, delivery.Signature, delivery.Payload); err != nil {
		t.Errorf("signature = %v", err)
	}

	stored, err := box.Get(context.Background(), client, record.ID)
	if err != nil {
		t.Fatal(err)
	}

	if stored.Status != StatusDelivered || stored.Attempts != 1 || stored.DeliveredAt == nil {
		t.Errorf("stored = %+v", stored)
	}
}

func TestDeliverDueRetriesAndFails(t *testing.T) {
	box, client, now := testOutbox(t)
	sender := &recordingSender{statuses: []int{http.StatusBadGateway, http.StatusOK}}
	box.Sender = sender

	record, err := box.Enqueue(context.Background(), client, testMessage(""))
	if err != nil {
		t.Fatal(err)
	}

	summary, err := box.DeliverDue(context.Background(), client, 10)
	if err != nil {
		t.Fatal(err)
	}

	if summary.Retried != 1 || len(sender.calls) != 1 {
		t.Fatalf("summary = %+v calls = %d", summary, len(sender.calls))
	}

	pending, err := box.Get(context.Background(), client, record.ID)
	if err != nil {
		t.Fatal(err)
	}

	if pending.Status != StatusPending || pending.Attempts != 1 || pending.LastHTTPStatus != http.StatusBadGateway {
		t.Errorf("pending = %+v", pending)
	}

	*now = now.Add(DefaultRetry[0])

	summary, err = box.DeliverDue(context.Background(), client, 10)
	if err != nil {
		t.Fatal(err)
	}

	if summary.Delivered != 1 || len(sender.calls) != 2 {
		t.Errorf("summary = %+v calls = %d", summary, len(sender.calls))
	}

	delivered, err := box.Get(context.Background(), client, record.ID)
	if err != nil {
		t.Fatal(err)
	}

	if delivered.Status != StatusDelivered || delivered.Attempts != 2 {
		t.Errorf("delivered = %+v", delivered)
	}
}

func TestDeliverDueMarksPermanentFailure(t *testing.T) {
	box, client, _ := testOutbox(t)
	box.Sender = &recordingSender{statuses: []int{http.StatusBadRequest}}

	record, err := box.Enqueue(context.Background(), client, testMessage(""))
	if err != nil {
		t.Fatal(err)
	}

	summary, err := box.DeliverDue(context.Background(), client, 10)
	if err != nil {
		t.Fatal(err)
	}

	if summary.Failed != 1 {
		t.Errorf("summary = %+v", summary)
	}

	stored, err := box.Get(context.Background(), client, record.ID)
	if err != nil {
		t.Fatal(err)
	}

	if stored.Status != StatusFailed || stored.Attempts != 1 || stored.LastError == "" {
		t.Errorf("stored = %+v", stored)
	}
}

func TestDeliverDueReapsExpiredLease(t *testing.T) {
	box, client, now := testOutbox(t)
	box.Sender = &recordingSender{}

	record, err := box.Enqueue(context.Background(), client, testMessage(""))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.Exec(context.Background(), `update webhook_outbox
		set status = 'sending', lease_until = ? where id = ?`, now.Add(-time.Minute).Format(timeFormat), record.ID); err != nil {
		t.Fatal(err)
	}

	summary, err := box.DeliverDue(context.Background(), client, 10)
	if err != nil {
		t.Fatal(err)
	}

	if summary.Reaped != 1 || summary.Delivered != 1 {
		t.Errorf("summary = %+v", summary)
	}
}

func TestDeliverDueRequiresSecretAndValidTable(t *testing.T) {
	box, client, _ := testOutbox(t)
	box.Signer.Secret = nil

	if _, err := box.DeliverDue(context.Background(), client, 10); err == nil {
		t.Error("empty signing secret was accepted")
	}

	box.Signer.Secret = []byte("secret")
	box.Table = "bad table"

	if _, err := box.DeliverDue(context.Background(), client, 10); err == nil {
		t.Error("invalid table was accepted")
	}
}

func TestHTTPSenderSetsSignedHeaders(t *testing.T) {
	var (
		gotHeaders http.Header
		gotBody    string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)

		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	signer := sign.Signer{Secret: []byte("secret"), Now: func() time.Time { return time.Unix(1000, 0) }}
	message := Message{
		Payload:    []byte(`{"ok":true}`),
		URL:        server.URL,
		Headers:    map[string]string{"X-Custom": "yes"},
		DeliveryID: "delivery-1",
	}
	delivery := Delivery{Message: message}
	delivery.Timestamp, delivery.Signature = signer.Sign(delivery.DeliveryID, delivery.Payload)

	status, err := (HTTPSender{}).Send(context.Background(), delivery)
	if err != nil {
		t.Fatal(err)
	}

	if status != http.StatusNoContent || gotBody != `{"ok":true}` {
		t.Errorf("status = %d body = %q", status, gotBody)
	}

	if gotHeaders.Get("X-Custom") != "yes" || gotHeaders.Get(timestampHeader) != delivery.Timestamp ||
		gotHeaders.Get(deliveryIDHeader) != delivery.DeliveryID || gotHeaders.Get(signatureHeader) != delivery.Signature {
		t.Errorf("headers = %v", gotHeaders)
	}

	if err := signer.Verify(delivery.Timestamp, delivery.DeliveryID, delivery.Signature, []byte(gotBody)); err != nil {
		t.Errorf("signature = %v", err)
	}
}

func TestSchemaRejectsInvalidTable(t *testing.T) {
	box := Outbox{Table: "outbox; drop table users"}
	if _, err := box.Schema(); err == nil {
		t.Error("invalid table name was accepted")
	}
}

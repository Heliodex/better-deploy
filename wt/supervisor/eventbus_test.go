package main

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEventBusPublishSubscribeAndCursor(t *testing.T) {
	bus := newEventBus()

	first, err := bus.publish("demo", "chat", []byte(`{"text":"one"}`))
	if err != nil {
		t.Fatal(err)
	}

	second, err := bus.publish("demo", "chat", []byte(`{"text":"two"}`))
	if err != nil {
		t.Fatal(err)
	}

	if first.ID != 1 || second.ID != 2 {
		t.Fatalf("ids = %d %d", first.ID, second.ID)
	}

	if _, err := bus.publish("demo", "other", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}

	since := int64(0)

	sub, replay, oldest, gap := bus.subscribe("demo", "chat", &since)
	if gap || oldest != 1 || len(replay) != 2 || replay[0].ID != 1 || replay[1].ID != 2 {
		t.Fatalf("replay = %+v oldest = %d gap = %v", replay, oldest, gap)
	}

	if _, err := bus.publish("demo", "chat", []byte(`{"text":"three"}`)); err != nil {
		t.Fatal(err)
	}

	select {
	case event := <-sub.ch:
		if event.Topic != "chat" || event.ID != 4 {
			t.Errorf("live event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("no live event")
	}

	bus.unsubscribe("demo", sub)

	for i := range eventBusMaxEvents + 5 {
		if _, err := bus.publish("demo", "flood", []byte(`{"i":`+strconv.Itoa(i)+`}`)); err != nil {
			t.Fatal(err)
		}
	}

	since = 0

	_, _, oldest, gap = bus.subscribe("demo", "flood", &since)
	if !gap || oldest <= 1 {
		t.Errorf("trimmed bus oldest = %d gap = %v", oldest, gap)
	}
}

func TestEventPublishAndSSEReplay(t *testing.T) {
	s := testSupervisor()
	s.bus = newEventBus()
	a := &app{name: "demo", cfg: appConfig{}}
	s.apps["demo"] = a

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/publish", strings.NewReader(`{"topic":"chat","data":{"text":"hello"}}`))
	handleEventPublish(s.bus, rec, req, "demo")

	if rec.Code != http.StatusAccepted {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleAppEvents(w, r, a)
	}))
	defer server.Close()

	resp, err := http.Get(server.URL + "/v1/events?topic=chat&since=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	foundData := false

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "hello") {
			foundData = true
			break
		}
	}

	if !foundData {
		t.Errorf("SSE replay did not contain the event: %v", scanner.Err())
	}
}

func TestStorageChangePublishesBusEvent(t *testing.T) {
	bus := newEventBus()
	storage := &storageServer{bus: bus}
	storage.publishStorageChange("demo", storageChange{
		RequestID:    "req-1",
		RowsAffected: 1,
		Rows: []rowChange{
			{Table: "items", Op: "insert"},
			{Table: "items", Op: "update"},
		},
	})

	since := int64(0)

	_, replay, _, _ := bus.subscribe("demo", "storage", &since)
	if len(replay) != 1 {
		t.Fatalf("storage events = %+v", replay)
	}

	data := string(replay[0].Data)
	for _, want := range []string{"items", "insert", "update", "req-1"} {
		if !strings.Contains(data, want) {
			t.Errorf("storage event data = %s, missing %q", data, want)
		}
	}
}

func TestAppFetchPublish(t *testing.T) {
	storage, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer storage.close()

	s := testSupervisor()
	s.storage = storage
	s.bus = newEventBus()
	s.apps["caller"] = &app{name: "caller"}
	s.apps["target"] = &app{name: "target", cfg: appConfig{AllowCallers: []string{"caller"}}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/apps/target/v1/publish", strings.NewReader(`{"topic":"news","data":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+storage.tokenFor("caller"))
	s.handleAppFetch(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("app fetch publish = %d: %s", rec.Code, rec.Body.String())
	}

	since := int64(0)

	_, replay, _, _ := s.bus.subscribe("target", "news", &since)
	if len(replay) != 1 || string(replay[0].Data) != `"hello"` {
		t.Errorf("target events = %+v", replay)
	}
}

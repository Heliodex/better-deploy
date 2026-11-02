package app

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublish(t *testing.T) {
	var gotAuth, gotTopic string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")

		var payload publishPayload
		if err := json.UnmarshalRead(r.Body, &payload); err != nil {
			t.Errorf("payload: %v", err)
		}

		gotTopic = payload.Topic
		writeTestJSON(w, map[string]any{"id": 7, "topic": payload.Topic, "time": "2026-01-01T00:00:00Z"})
	}))
	defer server.Close()

	t.Setenv(storageURLEnv, server.URL)
	t.Setenv(storageTokenEnv, "secret")

	event, err := Publish(t.Context(), server.Client(), "chat", map[string]string{"text": "hi"})
	if err != nil {
		t.Fatal(err)
	}

	if event.ID != 7 || event.Topic != "chat" || gotTopic != "chat" || gotAuth != "Bearer secret" {
		t.Errorf("event = %+v auth = %q topic = %q", event, gotAuth, gotTopic)
	}
}

func TestPublishTo(t *testing.T) {
	var gotPath, gotAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")

		writeTestJSON(w, map[string]any{"id": 9, "topic": "news", "time": "2026-01-01T00:00:00Z"})
	}))
	defer server.Close()

	t.Setenv(appFetchURLEnv, server.URL)
	t.Setenv(appTokenEnv, "app-secret")

	event, err := PublishTo(t.Context(), server.Client(), "target", "news", "hello")
	if err != nil {
		t.Fatal(err)
	}

	if event.ID != 9 || gotPath != "/apps/target/v1/publish" || gotAuth != "Bearer app-secret" {
		t.Errorf("event = %+v path = %q auth = %q", event, gotPath, gotAuth)
	}
}

func TestPublishRequiresStorageEnv(t *testing.T) {
	t.Setenv(storageURLEnv, "")
	t.Setenv(storageTokenEnv, "")

	if _, err := Publish(t.Context(), http.DefaultClient, "chat", nil); err == nil || !strings.Contains(err.Error(), storageURLEnv) {
		t.Errorf("Publish error = %v", err)
	}
}

func writeTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")

	data, _ := json.Marshal(value)
	_, _ = w.Write(data)
}

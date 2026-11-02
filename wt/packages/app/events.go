package app

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	storageURLEnv    = "WT_STORAGE_URL"
	storageTokenEnv  = "WT_STORAGE_TOKEN"
	eventPublishPath = "/v1/publish"
)

// Event is the accepted publish result. ID is the cursor to pass to GET /v1/events?since=.
type Event struct {
	ID    int64     `json:"id"`
	Topic string    `json:"topic"`
	Time  time.Time `json:"time"`
}

type publishPayload struct {
	Topic string `json:"topic"`
	Data  any    `json:"data"`
}

// eventResult decodes the publish response and turns a non-2xx status into an error.
func eventResult(resp *http.Response) (Event, error) {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return Event{}, fmt.Errorf("event publish returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var event Event
	if err := json.Unmarshal(body, &event); err != nil {
		return Event{}, fmt.Errorf("decode event publish: %w", err)
	}
	return event, nil
}

// Publish sends one event to the current app's bus through the storage loopback service.
func Publish(ctx context.Context, client *http.Client, topic string, data any) (Event, error) {
	base := strings.TrimSuffix(os.Getenv(storageURLEnv), "/")
	token := os.Getenv(storageTokenEnv)
	if base == "" || token == "" {
		return Event{}, fmt.Errorf("%s and %s are not set", storageURLEnv, storageTokenEnv)
	}
	body, err := json.Marshal(publishPayload{Topic: topic, Data: data})
	if err != nil {
		return Event{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+eventPublishPath, bytes.NewReader(body))
	if err != nil {
		return Event{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Event{}, err
	}
	return eventResult(resp)
}

// PublishTo sends one event to another app's bus. The target's allow_callers must name this app.
func PublishTo(ctx context.Context, client *http.Client, target, topic string, data any) (Event, error) {
	body, err := json.Marshal(publishPayload{Topic: topic, Data: data})
	if err != nil {
		return Event{}, err
	}
	req, err := NewAppRequest(ctx, http.MethodPost, target, eventPublishPath, bytes.NewReader(body))
	if err != nil {
		return Event{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Event{}, err
	}
	return eventResult(resp)
}

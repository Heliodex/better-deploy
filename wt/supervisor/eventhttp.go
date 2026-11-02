// Realtime event publishing over the storage loopback and SSE subscription on the app host.

package main

import (
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const eventKeepAlive = 20 * time.Second

type eventPublishRequest struct {
	Topic string         `json:"topic"`
	Data  jsontext.Value `json:"data"`
}

type eventPublishResponse struct {
	ID    int64     `json:"id"`
	Topic string    `json:"topic"`
	Time  time.Time `json:"time"`
}

// handleEventPublish is shared by the storage loopback service and the authenticated app-fetch publish path.
func handleEventPublish(bus *eventBus, w http.ResponseWriter, r *http.Request, app string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if bus == nil {
		writeError(w, http.StatusServiceUnavailable, "event bus is unavailable")
		return
	}
	var request eventPublishRequest
	if !readJSONBody(w, r, eventMaxBody+1024, &request) {
		return
	}
	event, err := bus.publish(app, request.Topic, request.Data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, eventPublishResponse{ID: event.ID, Topic: event.Topic, Time: event.Time})
}

// handleAppEvents streams an app's bus over SSE on the app host. It is public to the app's own origin, like the static files.
func (s *supervisor) handleAppEvents(w http.ResponseWriter, r *http.Request, a *app) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if s.bus == nil {
		writeError(w, http.StatusServiceUnavailable, "event bus is unavailable")
		return
	}
	stream, ok := newSSEStream(w, "no-cache")
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	topic := strings.TrimSpace(r.URL.Query().Get("topic"))
	if len(topic) > eventMaxTopic || strings.ContainsAny(topic, "\x00\r\n") {
		writeError(w, http.StatusBadRequest, "invalid topic")
		return
	}
	var since *int64
	if value := r.URL.Query().Get("since"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "since must be a non-negative event id")
			return
		}
		since = &parsed
	} else if value := r.Header.Get("Last-Event-ID"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err == nil && parsed >= 0 {
			since = &parsed
		}
	}

	sub, replay, oldest, gap := s.bus.subscribe(a.name, topic, since)
	defer s.bus.unsubscribe(a.name, sub)

	if !stream.retry(3000) {
		return
	}
	if gap && !stream.event("gap", fmt.Appendf(nil, `{"oldest":%d}`, oldest)) {
		return
	}
	for _, event := range replay {
		if !writeBusEvent(stream, event) {
			return
		}
	}

	ticker := time.NewTicker(eventKeepAlive)
	defer ticker.Stop()
	for {
		select {
		case event, ok := <-sub.ch:
			if !ok {
				return
			}
			if !writeBusEvent(stream, event) {
				return
			}
		case <-ticker.C:
			if !stream.ping() {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// writeBusEvent serialises and sends one bus event with its cursor id.
func writeBusEvent(stream *sseStream, event busEvent) bool {
	data := event.json()
	if data == nil {
		return true
	}
	return stream.idEvent(event.ID, "message", data)
}

// appEventPath reports whether a path belongs to the host event bus rather than the app's wasm component.
func appEventPath(path string) bool {
	return path == "/v1/events"
}

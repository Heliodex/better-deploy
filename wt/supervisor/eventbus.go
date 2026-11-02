// Per-app realtime event bus with a bounded ring and cursor.

package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	eventBusMaxEvents = 256
	eventBusMaxBytes  = 1 << 20
	eventSubBuffer    = 64
	eventMaxTopic     = 128
	eventMaxBody      = 64 << 10
)

// busEvent is one published value. The ID is monotonic per app and is the cursor a subscriber sends back as since or Last-Event-ID.
type busEvent struct {
	ID    int64          `json:"id"`
	Topic string         `json:"topic"`
	Time  time.Time      `json:"time"`
	Data  jsontext.Value `json:"data"`
}

type busSubscriber struct {
	topic string
	ch    chan busEvent
}

type appEventLog struct {
	mu   sync.Mutex
	next int64
	ring *boundedRing[busEvent]
	subs map[*busSubscriber]struct{}
}

// busEventSize approximates the memory one event occupies in the ring.
func busEventSize(event busEvent) int {
	return len(event.Data) + len(event.Topic) + 64
}

type eventBus struct {
	mu   sync.Mutex
	apps map[string]*appEventLog
}

func newEventBus() *eventBus {
	return &eventBus{apps: map[string]*appEventLog{}}
}

func (b *eventBus) log(app string) *appEventLog {
	b.mu.Lock()
	defer b.mu.Unlock()
	log := b.apps[app]
	if log == nil {
		log = &appEventLog{
			subs: map[*busSubscriber]struct{}{},
			ring: newBoundedRing(eventBusMaxEvents, eventBusMaxBytes, busEventSize),
		}
		b.apps[app] = log
	}
	return log
}

// publish appends an event to the app's ring and fans it out without ever blocking the publisher.
func (b *eventBus) publish(app, topic string, data jsontext.Value) (busEvent, error) {
	if b == nil {
		return busEvent{}, fmt.Errorf("event bus is unavailable")
	}
	topic = strings.TrimSpace(topic)
	if topic == "" || len(topic) > eventMaxTopic || strings.ContainsAny(topic, "\x00\r\n") {
		return busEvent{}, fmt.Errorf("topic must be 1 to %d bytes without control characters", eventMaxTopic)
	}
	if len(data) == 0 {
		data = jsontext.Value("null")
	}
	if len(data) > eventMaxBody {
		return busEvent{}, fmt.Errorf("event data is too large")
	}
	log := b.log(app)
	event := busEvent{Topic: topic, Time: time.Now().UTC(), Data: data}
	log.mu.Lock()
	log.next++
	event.ID = log.next
	log.ring.add(event)
	for sub := range log.subs {
		if sub.topic != "" && sub.topic != topic {
			continue
		}
		select {
		case sub.ch <- event:
		default:
			// A slow stream can resume from Last-Event-ID; do not let it hold the publisher.
		}
	}
	log.mu.Unlock()
	return event, nil
}

// subscribe atomically snapshots the replay window and registers the live channel.
// A nil since means live only; a non-nil since replays events newer than it.
func (b *eventBus) subscribe(app, topic string, since *int64) (sub *busSubscriber, replay []busEvent, oldest int64, gap bool) {
	log := b.log(app)
	sub = &busSubscriber{topic: topics(topic), ch: make(chan busEvent, eventSubBuffer)}
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.ring.len() > 0 {
		oldest = log.ring.at(0).ID
	}
	if since != nil {
		if log.ring.len() > 0 && *since < oldest-1 {
			gap = true
		}
		for i := 0; i < log.ring.len(); i++ {
			event := log.ring.at(i)
			if event.ID <= *since {
				continue
			}
			if sub.topic != "" && sub.topic != event.Topic {
				continue
			}
			replay = append(replay, event)
		}
	}
	log.subs[sub] = struct{}{}
	return sub, replay, oldest, gap
}

func topics(topic string) string {
	return strings.TrimSpace(topic)
}

func (b *eventBus) unsubscribe(app string, sub *busSubscriber) {
	if b == nil || sub == nil {
		return
	}
	b.mu.Lock()
	log := b.apps[app]
	b.mu.Unlock()
	if log == nil {
		return
	}
	log.mu.Lock()
	delete(log.subs, sub)
	log.mu.Unlock()
}

// remove drops an app's ring and closes live subscribers when the app is deleted.
func (b *eventBus) remove(app string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	log := b.apps[app]
	delete(b.apps, app)
	b.mu.Unlock()
	if log == nil {
		return
	}
	log.mu.Lock()
	for sub := range log.subs {
		close(sub.ch)
		delete(log.subs, sub)
	}
	log.ring.reset()
	log.mu.Unlock()
}

func (e busEvent) json() []byte {
	data, err := json.Marshal(e)
	if err != nil {
		return nil
	}
	return data
}

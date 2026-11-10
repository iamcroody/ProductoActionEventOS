// Package realtime implements the Server-Sent Events hub that pushes live
// updates (new prep tickets, status changes, stock/dashboard signals) to
// kitchen, bar, cashier and dashboard clients over the LAN.
//
// SSE is used rather than WebSockets because the flow is one-way (server ->
// client): it is plain HTTP, the browser EventSource reconnects automatically,
// and Last-Event-ID lets a phone resume without gaps after a Wi-Fi blip.
package realtime

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Event is one server-sent message. Topic routes it to subscribers (a station
// name like "cocina"/"bar", or "dashboard"). Seq is a monotonic id used for
// Last-Event-ID resume.
type Event struct {
	Seq   int64
	Topic string
	Type  string // e.g. "ticket", "ticket_update", "stock", "metrics"
	Data  string // JSON payload
}

type subscriber struct {
	topic string
	ch    chan Event
}

// Hub fans out events to subscribers filtered by topic. It keeps a small ring
// buffer per process so brief reconnects can replay missed events.
type Hub struct {
	mu     sync.RWMutex
	subs   map[*subscriber]struct{}
	seq    atomic.Int64
	ring   []Event
	ringSz int
}

// NewHub creates a hub with a ring buffer of the given size.
func NewHub(ringSize int) *Hub {
	if ringSize <= 0 {
		ringSize = 512
	}
	return &Hub{
		subs:   make(map[*subscriber]struct{}),
		ringSz: ringSize,
	}
}

// Publish assigns the next seq and delivers to matching subscribers.
// Slow subscribers that can't keep up are skipped for this event (they will
// recover via Last-Event-ID replay on reconnect).
func (h *Hub) Publish(topic, typ, data string) Event {
	ev := Event{Seq: h.seq.Add(1), Topic: topic, Type: typ, Data: data}
	h.mu.Lock()
	h.ring = append(h.ring, ev)
	if len(h.ring) > h.ringSz {
		h.ring = h.ring[len(h.ring)-h.ringSz:]
	}
	// Fan out while still holding the lock. remove() also closes channels under
	// the same lock, so a subscriber disconnecting concurrently can never cause
	// a send on a closed channel. Sends are non-blocking, so holding the lock is
	// bounded: a slow client is skipped (it recovers via Last-Event-ID replay).
	for s := range h.subs {
		if s.topic == topic || s.topic == "*" {
			select {
			case s.ch <- ev:
			default: // drop for slow client; resume handles the gap
			}
		}
	}
	h.mu.Unlock()
	return ev
}

func (h *Hub) add(s *subscriber) {
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) remove(s *subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	// Close under the lock so it is serialized with Publish's fan-out, which
	// also runs under the lock — this prevents a send on a closed channel.
	close(s.ch)
	h.mu.Unlock()
}

// replaySince returns buffered events for topic with seq greater than after.
func (h *Hub) replaySince(topic string, after int64) []Event {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []Event
	for _, ev := range h.ring {
		if ev.Seq > after && (ev.topicMatches(topic)) {
			out = append(out, ev)
		}
	}
	return out
}

func (e Event) topicMatches(topic string) bool {
	return topic == "*" || e.Topic == topic
}

// ServeHTTP streams events for the topic given by the "topic" query param.
// Honors the Last-Event-ID header for gap-free resume.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	topic := r.URL.Query().Get("topic")
	if topic == "" {
		topic = "*"
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	sub := &subscriber{topic: topic, ch: make(chan Event, 64)}
	h.add(sub)
	defer h.remove(sub)

	// Replay anything missed since Last-Event-ID.
	if lastID := r.Header.Get("Last-Event-ID"); lastID != "" {
		var after int64
		if _, err := fmt.Sscanf(lastID, "%d", &after); err == nil {
			for _, ev := range h.replaySince(topic, after) {
				writeEvent(w, ev)
			}
			flusher.Flush()
		}
	}

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-sub.ch:
			writeEvent(w, ev)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, ev Event) {
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, ev.Data)
}

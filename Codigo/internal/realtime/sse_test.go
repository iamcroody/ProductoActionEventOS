package realtime

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test helper: syncRecorder
//
// httptest.ResponseRecorder already satisfies http.Flusher; we wrap it to
// also signal a channel on every Flush so tests can synchronise without
// arbitrary sleeps when waiting for a live event to be written.
// ---------------------------------------------------------------------------

type syncRecorder struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
}

func newSyncRecorder() *syncRecorder {
	return &syncRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		flushed:          make(chan struct{}, 32),
	}
}

// Flush overrides the embedded method and signals the flushed channel.
func (s *syncRecorder) Flush() {
	s.ResponseRecorder.Flush()
	select {
	case s.flushed <- struct{}{}:
	default:
	}
}

// waitFlush blocks until at least one Flush call is signalled or d elapses.
func waitFlush(t *testing.T, s *syncRecorder, d time.Duration) {
	t.Helper()
	select {
	case <-s.flushed:
	case <-time.After(d):
		t.Fatalf("timed out waiting for Flush after %v", d)
	}
}

// ---------------------------------------------------------------------------
// 1. Publish assigns monotonically increasing Seq values
// ---------------------------------------------------------------------------

func TestPublishSeqMonotonic(t *testing.T) {
	h := NewHub(64)
	prev := int64(0)
	for i := 1; i <= 20; i++ {
		ev := h.Publish("topic", "type", `{}`)
		if ev.Seq <= prev {
			t.Fatalf("iteration %d: seq not monotonically increasing: got %d after %d",
				i, ev.Seq, prev)
		}
		if ev.Seq != int64(i) {
			t.Fatalf("iteration %d: expected seq=%d, got %d", i, i, ev.Seq)
		}
		prev = ev.Seq
	}
}

// ---------------------------------------------------------------------------
// 2. Topic filtering
//
// Matching rules (from Publish):
//   subscriber.topic == published_topic  →  receives event
//   subscriber.topic == "*"              →  receives every event
//
// A "cocina" subscriber must NOT receive a "bar" event.
// A "*" subscriber receives events for every topic.
// ---------------------------------------------------------------------------

func TestTopicFiltering(t *testing.T) {
	h := NewHub(64)

	cocinaSub := &subscriber{topic: "cocina", ch: make(chan Event, 8)}
	barSub := &subscriber{topic: "bar", ch: make(chan Event, 8)}
	wildSub := &subscriber{topic: "*", ch: make(chan Event, 8)}

	h.add(cocinaSub)
	h.add(barSub)
	h.add(wildSub)
	defer h.remove(cocinaSub)
	defer h.remove(barSub)
	defer h.remove(wildSub)

	cocinaEv := h.Publish("cocina", "ticket", `{"id":1}`)
	barEv := h.Publish("bar", "order", `{"id":2}`)

	// cocina subscriber should receive the cocina event.
	select {
	case ev := <-cocinaSub.ch:
		if ev.Seq != cocinaEv.Seq {
			t.Errorf("cocina sub: want seq %d, got %d", cocinaEv.Seq, ev.Seq)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("cocina subscriber did not receive cocina event")
	}

	// cocina subscriber must NOT receive the bar event.
	select {
	case ev := <-cocinaSub.ch:
		t.Errorf("cocina subscriber unexpectedly received event topic=%q seq=%d", ev.Topic, ev.Seq)
	case <-time.After(30 * time.Millisecond):
		// correct: nothing arrived
	}

	// bar subscriber should receive the bar event (and nothing else).
	select {
	case ev := <-barSub.ch:
		if ev.Seq != barEv.Seq {
			t.Errorf("bar sub: want seq %d, got %d", barEv.Seq, ev.Seq)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("bar subscriber did not receive bar event")
	}

	// bar subscriber must NOT have received the cocina event.
	select {
	case ev := <-barSub.ch:
		t.Errorf("bar subscriber unexpectedly received event topic=%q seq=%d", ev.Topic, ev.Seq)
	case <-time.After(30 * time.Millisecond):
		// correct
	}

	// Wildcard subscriber should receive both events, in publication order.
	got := make([]int64, 0, 2)
	for i := 0; i < 2; i++ {
		select {
		case ev := <-wildSub.ch:
			got = append(got, ev.Seq)
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("wildcard subscriber missing event #%d", i+1)
		}
	}
	if len(got) == 2 {
		if got[0] != cocinaEv.Seq {
			t.Errorf("wildcard first event: want seq %d, got %d", cocinaEv.Seq, got[0])
		}
		if got[1] != barEv.Seq {
			t.Errorf("wildcard second event: want seq %d, got %d", barEv.Seq, got[1])
		}
	}
}

// ---------------------------------------------------------------------------
// 3. ServeHTTP streams events
//
// Strategy: run ServeHTTP in a goroutine with a cancellable context.
// Wait 10 ms for the goroutine to register the subscriber, publish one event,
// wait for the Flush signal, then cancel to exit the loop and inspect the body.
// ---------------------------------------------------------------------------

func TestServeHTTPStreamsEvent(t *testing.T) {
	h := NewHub(64)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/?topic=cocina", nil)
	req = req.WithContext(ctx)

	rw := newSyncRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rw, req)
	}()

	// Allow the goroutine to call h.add(sub) before we publish.
	time.Sleep(10 * time.Millisecond)

	h.Publish("cocina", "ticket", `{"order":42}`)

	// ServeHTTP flushes after writing the event; wait for that signal.
	waitFlush(t, rw, time.Second)

	cancel()
	<-done

	body := rw.Body.String()

	if !strings.Contains(body, "event: ticket") {
		t.Errorf("expected 'event: ticket' in response body\nbody:\n%s", body)
	}
	if !strings.Contains(body, `{"order":42}`) {
		t.Errorf("expected JSON payload in response body\nbody:\n%s", body)
	}
	if !strings.Contains(body, "id: ") {
		t.Errorf("expected 'id: ' line in response body\nbody:\n%s", body)
	}
	// Verify Content-Type header.
	if ct := rw.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type: want text/event-stream, got %q", ct)
	}
}

// TestServeHTTPDefaultTopicIsWildcard confirms that omitting ?topic= subscribes
// with topic "*" and therefore receives events for any topic.
func TestServeHTTPDefaultTopicIsWildcard(t *testing.T) {
	h := NewHub(16)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// No ?topic= in the URL — should default to "*".
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(ctx)

	rw := newSyncRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rw, req)
	}()

	time.Sleep(10 * time.Millisecond)

	h.Publish("bar", "stock", `{"item":"cerveza"}`)

	waitFlush(t, rw, time.Second)

	cancel()
	<-done

	body := rw.Body.String()
	if !strings.Contains(body, "event: stock") {
		t.Errorf("wildcard stream should receive bar event; body:\n%s", body)
	}
}

// ---------------------------------------------------------------------------
// 4. Last-Event-ID replay
//
// Publish three events, then open a stream with Last-Event-ID=seq(e1).
// ServeHTTP must replay e2 and e3 (seq > e1.Seq) and must NOT replay e1.
// ---------------------------------------------------------------------------

func TestLastEventIDReplay(t *testing.T) {
	h := NewHub(64)

	e1 := h.Publish("cocina", "ticket", `{"id":1}`)
	e2 := h.Publish("cocina", "ticket", `{"id":2}`)
	e3 := h.Publish("cocina", "ticket", `{"id":3}`)

	ctx, cancel := context.WithCancel(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/?topic=cocina", nil)
	req.Header.Set("Last-Event-ID", fmt.Sprintf("%d", e1.Seq))
	req = req.WithContext(ctx)

	rw := newSyncRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rw, req)
	}()

	// Replay emits all missed events then calls Flush once.
	waitFlush(t, rw, time.Second)

	cancel()
	<-done

	body := rw.Body.String()

	// e1 must NOT appear (its seq was the Last-Event-ID; only seq > lastID replays).
	if strings.Contains(body, fmt.Sprintf("id: %d\n", e1.Seq)) {
		t.Errorf("e1 (seq=%d) should not be in replay output\nbody:\n%s", e1.Seq, body)
	}
	// e2 and e3 must be present.
	if !strings.Contains(body, fmt.Sprintf("id: %d\n", e2.Seq)) {
		t.Errorf("e2 (seq=%d) missing from replay\nbody:\n%s", e2.Seq, body)
	}
	if !strings.Contains(body, fmt.Sprintf("id: %d\n", e3.Seq)) {
		t.Errorf("e3 (seq=%d) missing from replay\nbody:\n%s", e3.Seq, body)
	}
}

// TestLastEventIDReplayTopicIsolation verifies that replay respects topic
// filtering: a "cocina" subscriber only replays "cocina" events even when
// other topics are in the ring buffer.
func TestLastEventIDReplayTopicIsolation(t *testing.T) {
	h := NewHub(64)

	cocinaEv := h.Publish("cocina", "ticket", `{"id":1}`)
	_ = h.Publish("bar", "order", `{"id":2}`) // different topic — should not replay
	e3 := h.Publish("cocina", "ticket", `{"id":3}`)

	ctx, cancel := context.WithCancel(context.Background())

	// Replay from before all events (seq 0 means "replay everything").
	req := httptest.NewRequest(http.MethodGet, "/?topic=cocina", nil)
	req.Header.Set("Last-Event-ID", "0")
	req = req.WithContext(ctx)

	rw := newSyncRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rw, req)
	}()

	waitFlush(t, rw, time.Second)

	cancel()
	<-done

	body := rw.Body.String()

	if !strings.Contains(body, fmt.Sprintf("id: %d\n", cocinaEv.Seq)) {
		t.Errorf("cocina event (seq=%d) missing from replay\nbody:\n%s", cocinaEv.Seq, body)
	}
	if !strings.Contains(body, fmt.Sprintf("id: %d\n", e3.Seq)) {
		t.Errorf("cocina event (seq=%d) missing from replay\nbody:\n%s", e3.Seq, body)
	}
	// bar event must NOT appear in a "cocina" replay.
	if strings.Contains(body, `{"id":2}`) {
		t.Errorf("bar event should not appear in cocina replay\nbody:\n%s", body)
	}
}

// ---------------------------------------------------------------------------
// 5. Slow/full subscriber — Publish must not block
//
// Register a subscriber with a buffer of 1 that is never drained.
// Publish many more events than that; all must complete promptly.
// ---------------------------------------------------------------------------

func TestPublishDoesNotBlockOnSlowSubscriber(t *testing.T) {
	h := NewHub(64)

	// Tiny buffer; after one event it is full and future sends hit `default`.
	slow := &subscriber{
		topic: "busy",
		ch:    make(chan Event, 1),
	}
	h.add(slow)
	defer h.remove(slow)

	const n = 300
	start := time.Now()
	for i := 0; i < n; i++ {
		h.Publish("busy", "tick", `{}`)
	}
	elapsed := time.Since(start)

	if elapsed > 200*time.Millisecond {
		t.Errorf("Publish blocked on slow subscriber: %d publishes took %v (want < 200ms)", n, elapsed)
	}
}

// ---------------------------------------------------------------------------
// 6. Concurrency — run Publish from many goroutines under the race detector
//
// Every goroutine publishes perG events concurrently. After joining, the
// monotonic counter must equal the total number of publishes and the ring
// buffer must contain exactly min(total, ringSz) events.
// ---------------------------------------------------------------------------

func TestPublishConcurrency(t *testing.T) {
	const (
		goroutines = 20
		perG       = 50
		total      = int64(goroutines * perG) // 1000
	)
	h := NewHub(512)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				h.Publish("topic", "t", fmt.Sprintf(`{"g":%d,"j":%d}`, id, j))
			}
		}(i)
	}
	wg.Wait()

	// Atomic counter must reflect every publish.
	if seq := h.seq.Load(); seq != total {
		t.Errorf("seq after concurrent publishes: want %d, got %d", total, seq)
	}

	// Ring must hold exactly min(total, ringSz) events.
	h.mu.RLock()
	ringLen := int64(len(h.ring))
	h.mu.RUnlock()

	wantRing := total
	if wantRing > int64(h.ringSz) {
		wantRing = int64(h.ringSz)
	}
	if ringLen != wantRing {
		t.Errorf("ring length: want %d, got %d", wantRing, ringLen)
	}
}

// TestPublishConcurrencyWithSubscribers adds active subscribers during a
// concurrent Publish storm to expose any races in the fan-out path.
func TestPublishConcurrencyWithSubscribers(t *testing.T) {
	const goroutines = 10
	const perG = 100

	h := NewHub(512)

	// Pre-register a few subscribers that drain as fast as possible.
	var subWg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := 0; i < 3; i++ {
		sub := &subscriber{topic: "topic", ch: make(chan Event, 128)}
		h.add(sub)
		subWg.Add(1)
		go func(s *subscriber) {
			defer subWg.Done()
			for {
				select {
				case <-ctx.Done():
					h.remove(s)
					// Drain remaining.
					for len(s.ch) > 0 {
						<-s.ch
					}
					return
				case <-s.ch:
				}
			}
		}(sub)
	}

	var pubWg sync.WaitGroup
	pubWg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer pubWg.Done()
			for j := 0; j < perG; j++ {
				h.Publish("topic", "t", fmt.Sprintf(`{"g":%d}`, id))
			}
		}(i)
	}
	pubWg.Wait()

	cancel()
	subWg.Wait()

	const total = int64(goroutines * perG)
	if seq := h.seq.Load(); seq != total {
		t.Errorf("seq after concurrent publish+subscribe: want %d, got %d", total, seq)
	}
}

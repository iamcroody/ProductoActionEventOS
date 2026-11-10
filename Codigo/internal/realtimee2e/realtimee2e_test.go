// Package realtimee2e exercises the SSE push stream end-to-end against the
// fully-wired httptest server.  It mirrors the harness pattern from
// internal/apitest and drives every scenario through real HTTP so that the
// realtime hub, the session middleware, and the domain modules are all live.
//
// The four tests cover:
//  1. A cocina subscriber receives the "ticket" event for a new order.
//  2. Marking an item ready publishes a "ticket_update" event.
//  3. A bar subscriber does NOT receive a cocina-station ticket.
//  4. Last-Event-ID resume: missed events are replayed on reconnect.
package realtimee2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/backup"
	"eventpos/internal/bootstrap"
	"eventpos/internal/caja"
	"eventpos/internal/catalog"
	"eventpos/internal/config"
	"eventpos/internal/customers"
	"eventpos/internal/db"
	"eventpos/internal/events"
	"eventpos/internal/kitchen"
	"eventpos/internal/oversight"
	"eventpos/internal/realtime"
	"eventpos/internal/reports"
	"eventpos/internal/sales"
	"eventpos/internal/server"
	"eventpos/internal/users"
	"eventpos/internal/writer"
)

// ---------------------------------------------------------------------------
// Harness: identical to internal/apitest.
// ---------------------------------------------------------------------------

type harness struct {
	ts     *httptest.Server
	client *http.Client
}

// newHarness boots the fully-wired server in-process against a fresh SQLite DB.
func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	d, err := db.Open(filepath.Join(t.TempDir(), "it.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	w := writer.New(d.Writer)
	t.Cleanup(w.Close)

	hub := realtime.NewHub(64)
	cfg := config.New(d)
	if err := cfg.EnsureDefaults(ctx); err != nil {
		t.Fatalf("ensure defaults: %v", err)
	}
	backupSvc, err := backup.New(d, t.TempDir())
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	authSvc := auth.New(d)

	application := &app.App{DB: d, Writer: w, Hub: hub, DataDir: t.TempDir()}

	if _, _, err := bootstrap.SeedAdmin(ctx, d); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	deps := server.Deps{App: application, Auth: authSvc, Config: cfg, Backup: backupSvc}
	mods := []server.Module{
		users.New(deps), events.New(deps), catalog.New(deps), customers.New(deps),
		caja.New(deps), sales.New(deps), kitchen.New(deps), reports.New(deps),
		oversight.New(deps),
	}

	srv := server.New(deps, fstest.MapFS{}, mods...)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &harness{ts: ts, client: &http.Client{Jar: jar}}
}

func (h *harness) req(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal %s %s: %v", method, path, err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, r)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body %s %s: %v", method, path, err)
	}
	return resp.StatusCode, data
}

func (h *harness) mustPost(t *testing.T, path string, body, out any) {
	t.Helper()
	code, data := h.req(t, "POST", path, body)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("POST %s: status %d, body %s", path, code, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("POST %s decode: %v (body %s)", path, err, data)
		}
	}
}

func (h *harness) mustGet(t *testing.T, path string, out any) {
	t.Helper()
	code, data := h.req(t, "GET", path, nil)
	if code != http.StatusOK {
		t.Fatalf("GET %s: status %d, body %s", path, code, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("GET %s decode: %v (body %s)", path, err, data)
		}
	}
}

func (h *harness) login(t *testing.T, username, secret string) {
	t.Helper()
	h.mustPost(t, "/api/auth/login", map[string]any{"username": username, "secret": secret}, nil)
}

// ---------------------------------------------------------------------------
// Wire shapes used in assertions.
// ---------------------------------------------------------------------------

type idResp struct {
	ID string `json:"id"`
}

type attendee struct {
	ID      string `json:"id"`
	QRToken string `json:"qr_token"`
	Balance int64  `json:"balance"`
}

// ---------------------------------------------------------------------------
// SSE infrastructure.
// ---------------------------------------------------------------------------

// sseEvent is one parsed SSE message (id / event / data triple).
type sseEvent struct {
	ID   string // value of the "id: " field
	Type string // value of the "event: " field
	Data string // value of the "data: " field
}

// sseClient wraps a live SSE stream connection.  Use Close (or rely on
// t.Cleanup) to terminate it.
type sseClient struct {
	events chan sseEvent
	cancel context.CancelFunc
	done   chan struct{}
}

// Close cancels the underlying HTTP request and waits for the reader goroutine
// to exit.
func (sc *sseClient) Close() {
	sc.cancel()
	<-sc.done
}

// openSSE opens GET /api/stream?topic=<topic> against the test server using
// the harness's cookie-jar client (so the session cookie is sent
// automatically).
//
// lastEventID MUST be non-empty for initial connections (use "0" for a
// brand-new stream).  Passing a non-empty value causes the server to call
// Flush immediately after the Last-Event-ID replay, which in turn makes
// http.Client.Do return promptly with the 200 response — ensuring the
// subscriber is registered in the Hub before this function returns.  Without
// a flush the client's Do call would block until the first event or the
// 20-second ping.
func (h *harness) openSSE(t *testing.T, topic, lastEventID string) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	url := h.ts.URL + "/api/stream?topic=" + topic
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		cancel()
		t.Fatalf("openSSE new request: %v", err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("openSSE connect to %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		t.Fatalf("openSSE: status %d, want 200", resp.StatusCode)
	}

	ch := make(chan sseEvent, 32)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer resp.Body.Close()
		readSSEEvents(ctx, resp.Body, ch)
	}()

	sc := &sseClient{events: ch, cancel: cancel, done: done}
	t.Cleanup(sc.Close)
	return sc
}

// readSSEEvents parses the text/event-stream format from r and sends complete
// events to ch.  It returns when r is exhausted or ctx is cancelled.
// Comment lines (": …") are silently ignored.
func readSSEEvents(ctx context.Context, r io.Reader, ch chan<- sseEvent) {
	scanner := bufio.NewScanner(r)
	var cur sseEvent
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		case line == "":
			// Empty line dispatches the accumulated event.
			if cur.Type != "" || cur.Data != "" {
				select {
				case ch <- cur:
				case <-ctx.Done():
					return
				}
			}
			cur = sseEvent{}
		}
		// Lines starting with ":" are comments/pings; leave cur unchanged.
	}
}

// awaitEvent blocks until an SSE event arrives or the timeout expires.
func awaitEvent(t *testing.T, ch <-chan sseEvent, timeout time.Duration) sseEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(timeout):
		t.Fatalf("timed out after %v waiting for SSE event", timeout)
		panic("unreachable")
	}
}

// noEvent asserts that no SSE event arrives within the given window.
func noEvent(t *testing.T, ch <-chan sseEvent, window time.Duration) {
	t.Helper()
	select {
	case ev := <-ch:
		t.Errorf("unexpected SSE event: type=%q data=%q", ev.Type, ev.Data)
	case <-time.After(window):
		// correct — nothing arrived
	}
}

// ---------------------------------------------------------------------------
// Test scenario setup helpers.
// ---------------------------------------------------------------------------

// draftEvent creates an event (which starts in 'draft' status) and returns its
// id. Configuration (catalog, attendees) must happen while draft; call activate
// once setup is complete to begin selling.
func (h *harness) draftEvent(t *testing.T) string {
	t.Helper()
	var ev idResp
	h.mustPost(t, "/api/events", map[string]any{
		"name": "SSEFiesta", "default_topup": 100000, "split_policy": "cascade",
	}, &ev)
	return ev.ID
}

// activate promotes a draft event to 'active' so orders are allowed.
func (h *harness) activate(t *testing.T, id string) {
	t.Helper()
	h.mustPost(t, "/api/events/"+id+"/activate", map[string]any{}, nil)
}

// createCategory creates a category and returns its id.
func (h *harness) createCategory(t *testing.T, name string) string {
	t.Helper()
	var c idResp
	h.mustPost(t, "/api/categories", map[string]any{"name": name, "sort_order": 0}, &c)
	return c.ID
}

// createProduct creates a product at the given prep station and returns its id.
func (h *harness) createProduct(t *testing.T, categoryID, name string, price int64, station string) string {
	t.Helper()
	var p idResp
	h.mustPost(t, "/api/products", map[string]any{
		"category_id":  categoryID,
		"name":         name,
		"price":        price,
		"prep_station": station,
		"track_stock":  0,
		"stock_qty":    0,
		"cost":         0,
	}, &p)
	return p.ID
}

// createAttendee creates an attendee with an initial top-up and returns it.
func (h *harness) createAttendee(t *testing.T, name string, topup int64) attendee {
	t.Helper()
	var a attendee
	h.mustPost(t, "/api/attendees", map[string]any{
		"display_name": name, "initial_topup": topup,
	}, &a)
	return a
}

// postOrder places an order for a single attendee and single product, returns
// the order id.
func (h *harness) postOrder(t *testing.T, reqID, productID, attendeeID string) string {
	t.Helper()
	var o idResp
	h.mustPost(t, "/api/orders", map[string]any{
		"request_id": reqID,
		"items":      []map[string]any{{"product_id": productID, "qty": 1}},
		"payers":     []map[string]any{{"attendee_id": attendeeID}},
	}, &o)
	return o.ID
}

// ---------------------------------------------------------------------------
// 1. Cocina subscriber receives the "ticket" event for a new order.
// ---------------------------------------------------------------------------

func TestSSECocinaTicket(t *testing.T) {
	h := newHarness(t)
	h.login(t, bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	evID := h.draftEvent(t)
	catID := h.createCategory(t, "Cocina")
	productID := h.createProduct(t, catID, "Enchilada", 12000, "cocina")
	att := h.createAttendee(t, "Patron", 100000)
	h.activate(t, evID)

	// Open a live SSE stream for topic=cocina.
	// Passing Last-Event-ID: 0 causes the server to flush immediately after the
	// (empty) replay so client.Do returns promptly and the subscriber is already
	// registered in the Hub before we post the order.
	sub := h.openSSE(t, "pendientes", "0")

	// Post the order — this triggers Hub.Publish("cocina", "ticket", …).
	orderID := h.postOrder(t, "rt-ticket-1", productID, att.ID)

	// The ticket event must arrive within a generous timeout.
	ev := awaitEvent(t, sub.events, 3*time.Second)

	if ev.Type != "ticket" {
		t.Fatalf("event type: got %q, want %q", ev.Type, "ticket")
	}
	if ev.ID == "" {
		t.Fatalf("event has no SSE id")
	}

	// Parse the payload and verify the order_id and item name.
	var payload struct {
		OrderID string `json:"order_id"`
		Items   []struct {
			Name string `json:"name"`
			Qty  int64  `json:"qty"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse ticket payload: %v\nraw: %s", err, ev.Data)
	}
	if payload.OrderID != orderID {
		t.Fatalf("ticket order_id: got %q, want %q", payload.OrderID, orderID)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("ticket items count: got %d, want 1", len(payload.Items))
	}
	if payload.Items[0].Name != "Enchilada" {
		t.Fatalf("ticket item name: got %q, want %q", payload.Items[0].Name, "Enchilada")
	}
	if payload.Items[0].Qty != 1 {
		t.Fatalf("ticket item qty: got %d, want 1", payload.Items[0].Qty)
	}
}

// ---------------------------------------------------------------------------
// 2. Marking an item ready publishes a "ticket_update" event.
// ---------------------------------------------------------------------------

func TestSSETicketUpdate(t *testing.T) {
	h := newHarness(t)
	h.login(t, bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	ev := h.draftEvent(t)
	catID := h.createCategory(t, "Cocina")
	productID := h.createProduct(t, catID, "Tamale", 9000, "cocina")
	att := h.createAttendee(t, "Mesa1", 100000)
	h.activate(t, ev)

	sub := h.openSSE(t, "pendientes", "0")

	h.postOrder(t, "rt-update-1", productID, att.ID)

	// Receive the ticket event to extract the kitchen item id.
	ticketEv := awaitEvent(t, sub.events, 3*time.Second)
	if ticketEv.Type != "ticket" {
		t.Fatalf("expected ticket event, got %q", ticketEv.Type)
	}

	var ticketPayload struct {
		OrderID string `json:"order_id"`
		Items   []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(ticketEv.Data), &ticketPayload); err != nil {
		t.Fatalf("parse ticket payload: %v", err)
	}
	if len(ticketPayload.Items) == 0 {
		t.Fatalf("ticket payload has no items")
	}
	itemID := ticketPayload.Items[0].ID
	if itemID == "" {
		t.Fatalf("ticket payload item id is empty")
	}

	// Deliver the item — completes the single 1-unit item and publishes a
	// ticket_update event with status "delivered".
	h.mustPost(t, "/api/pendientes/items/"+itemID+"/entregar", map[string]any{}, nil)

	updateEv := awaitEvent(t, sub.events, 3*time.Second)
	if updateEv.Type != "ticket_update" {
		t.Fatalf("event type: got %q, want %q", updateEv.Type, "ticket_update")
	}

	var updatePayload struct {
		OrderID string `json:"order_id"`
		ItemID  string `json:"item_id"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal([]byte(updateEv.Data), &updatePayload); err != nil {
		t.Fatalf("parse ticket_update payload: %v\nraw: %s", err, updateEv.Data)
	}
	if updatePayload.ItemID != itemID {
		t.Fatalf("ticket_update item_id: got %q, want %q", updatePayload.ItemID, itemID)
	}
	if updatePayload.Status != "delivered" {
		t.Fatalf("ticket_update status: got %q, want %q", updatePayload.Status, "delivered")
	}
	if updatePayload.OrderID != ticketPayload.OrderID {
		t.Fatalf("ticket_update order_id: got %q, want %q",
			updatePayload.OrderID, ticketPayload.OrderID)
	}
}

// ---------------------------------------------------------------------------
// 3. A bar subscriber does NOT receive a cocina-station ticket.
// ---------------------------------------------------------------------------

func TestSSETopicIsolation(t *testing.T) {
	h := newHarness(t)
	h.login(t, bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	ev := h.draftEvent(t)
	catID := h.createCategory(t, "Cocina")

	cocinaProductID := h.createProduct(t, catID, "Burrito", 11000, "cocina")
	att := h.createAttendee(t, "Anonimo", 100000)
	h.activate(t, ev)

	// Subscribe to cocina — we'll use this to confirm the event WAS published.
	cocinaSub := h.openSSE(t, "pendientes", "0")
	// Subscribe to bar — this subscriber must NOT receive cocina events.
	barSub := h.openSSE(t, "bar", "0")

	// Post order with ONLY a cocina item.
	h.postOrder(t, "rt-iso-1", cocinaProductID, att.ID)

	// Cocina subscriber must receive the ticket.
	cocinaEv := awaitEvent(t, cocinaSub.events, 3*time.Second)
	if cocinaEv.Type != "ticket" {
		t.Fatalf("cocina subscriber event type: got %q, want ticket", cocinaEv.Type)
	}

	// Bar subscriber must receive nothing within a reasonable window.
	// The event has already been published (postOrder returned), so if the bar
	// subscriber were going to receive it, the event would be in its channel now.
	noEvent(t, barSub.events, 300*time.Millisecond)
}

// ---------------------------------------------------------------------------
// 4. Last-Event-ID resume: missed events are replayed on reconnect.
// ---------------------------------------------------------------------------

func TestSSELastEventIDResume(t *testing.T) {
	h := newHarness(t)
	h.login(t, bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	ev := h.draftEvent(t)
	catID := h.createCategory(t, "Cocina")
	productID := h.createProduct(t, catID, "Quesadilla", 7000, "cocina")
	att := h.createAttendee(t, "Mesa2", 200000)
	h.activate(t, ev)

	// ---- First connection: receive one event and record its SSE id. ----
	sub1 := h.openSSE(t, "pendientes", "0")

	h.postOrder(t, "rt-resume-1", productID, att.ID)

	firstEv := awaitEvent(t, sub1.events, 3*time.Second)
	if firstEv.Type != "ticket" {
		t.Fatalf("first event type: got %q, want ticket", firstEv.Type)
	}
	lastSeenID := firstEv.ID
	if lastSeenID == "" {
		t.Fatalf("first event has no SSE id")
	}

	// Disconnect.
	sub1.Close()

	// ---- While disconnected: post a second order. ----
	// Hub.Publish stores the event in the ring buffer even with no subscribers.
	h.postOrder(t, "rt-resume-2", productID, att.ID)

	// ---- Reconnect with Last-Event-ID set to the last seen sequence. ----
	// The server replays all ring-buffered events with seq > lastSeenID for
	// topic=cocina and calls Flush immediately, so client.Do returns fast.
	sub2 := h.openSSE(t, "pendientes", lastSeenID)

	replayedEv := awaitEvent(t, sub2.events, 3*time.Second)
	if replayedEv.Type != "ticket" {
		t.Fatalf("replayed event type: got %q, want ticket", replayedEv.Type)
	}

	// The replayed event must have a strictly higher sequence number.
	if replayedEv.ID <= lastSeenID {
		t.Fatalf("replayed event id %q must be greater than last-seen id %q",
			replayedEv.ID, lastSeenID)
	}

	// Verify the ring buffer did NOT replay the first event.
	// Parse both payloads and confirm the order ids differ.
	var firstPayload, replayedPayload struct {
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal([]byte(firstEv.Data), &firstPayload); err != nil {
		t.Fatalf("parse first event payload: %v", err)
	}
	if err := json.Unmarshal([]byte(replayedEv.Data), &replayedPayload); err != nil {
		t.Fatalf("parse replayed event payload: %v", err)
	}
	if replayedPayload.OrderID == firstPayload.OrderID {
		t.Fatalf("replayed event has the same order_id as first event (%q); "+
			"expected the second order (Last-Event-ID replay should skip the first)",
			firstPayload.OrderID)
	}

	// Also verify the first event ID is not in the replay stream: the replayed
	// event must correspond to rt-resume-2, not rt-resume-1.
	if replayedEv.ID == lastSeenID {
		t.Fatalf("replay returned the already-seen event (id=%q)", lastSeenID)
	}
}

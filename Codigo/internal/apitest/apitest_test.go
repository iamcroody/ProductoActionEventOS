// Package apitest is a permanent end-to-end HTTP integration suite for Action
// EventOS. Each test boots the fully-wired server in-process via httptest with
// the exact module set from cmd/eventpos, then drives the event-operation flow
// over real HTTP — exercising cross-module behavior (events → catalog →
// customers → caja → sales → kitchen → reports → oversight) the way the SPA
// would.
package apitest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/backup"
	"eventpos/internal/bootstrap"
	"eventpos/internal/caja"
	"eventpos/internal/catalog"
	"eventpos/internal/config"
	"eventpos/internal/core"
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
// Harness: a fully-wired in-process server with a cookie-persisting client.
// ---------------------------------------------------------------------------

type harness struct {
	ts     *httptest.Server
	client *http.Client
}

// newHarness builds the same server cmd/eventpos builds (all nine modules) on a
// fresh temp SQLite DB, seeds admin/1234, and returns it behind httptest with a
// cookie-jar client so the session cookie persists across requests.
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

// req issues one HTTP request through the cookie-jar client and returns the
// status code and raw body.
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

// mustPost POSTs and fails unless the status is 200 or 201, decoding into out
// when non-nil.
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

// mustGet GETs and fails unless the status is 200, decoding into out when non-nil.
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
// Wire shapes (only the fields the assertions read).
// ---------------------------------------------------------------------------

type idResp struct {
	ID string `json:"id"`
}

type attendee struct {
	ID      string `json:"id"`
	QRToken string `json:"qr_token"`
	Balance int64  `json:"balance"`
}

type product struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	StockQty int64  `json:"stock_qty"`
}

type payment struct {
	Method string `json:"method"`
	Amount int64  `json:"amount"`
}

type order struct {
	ID              string    `json:"id"`
	Status          string    `json:"status"`
	Total           int64     `json:"total"`
	PaidFromBalance int64     `json:"paid_from_balance"`
	PaidCash        int64     `json:"paid_cash"`
	Payments        []payment `json:"payments"`
}

// ---------------------------------------------------------------------------
// Builder helpers (admin must be logged in).
// ---------------------------------------------------------------------------

// draftEvent creates an event (which starts in 'draft' status) and returns its
// id. Under the lock-down lifecycle, all configuration (catalog, attendees,
// top-ups, inventory) must happen while the event is draft; call activate once
// setup is complete to begin selling / delivery / reports.
func (h *harness) draftEvent(t *testing.T) string {
	t.Helper()
	var ev idResp
	h.mustPost(t, "/api/events", map[string]any{
		"name": "Fiesta", "default_topup": 50000, "split_policy": "cascade",
	}, &ev)
	return ev.ID
}

// activate promotes a draft event to 'active' so selling, delivery and reports
// are allowed.
func (h *harness) activate(t *testing.T, id string) {
	t.Helper()
	h.mustPost(t, "/api/events/"+id+"/activate", map[string]any{}, nil)
}

func (h *harness) createCategory(t *testing.T, name string) string {
	t.Helper()
	var c idResp
	h.mustPost(t, "/api/categories", map[string]any{"name": name, "sort_order": 0}, &c)
	return c.ID
}

// createProduct creates a product and returns it. All test products track stock
// so inventory assertions are observable.
func (h *harness) createProduct(t *testing.T, categoryID, name string, price, stock int64, station string) product {
	t.Helper()
	var p product
	h.mustPost(t, "/api/products", map[string]any{
		"category_id":  categoryID,
		"name":         name,
		"price":        price,
		"prep_station": station,
		"track_stock":  1,
		"stock_qty":    stock,
	}, &p)
	return p
}

func (h *harness) createAttendee(t *testing.T, name string, topup int64) attendee {
	t.Helper()
	var a attendee
	h.mustPost(t, "/api/attendees", map[string]any{
		"display_name": name, "initial_topup": topup,
	}, &a)
	return a
}

func (h *harness) attendeeBalance(t *testing.T, id string) int64 {
	t.Helper()
	var a attendee
	h.mustGet(t, "/api/attendees/"+id, &a)
	return a.Balance
}

func (h *harness) products(t *testing.T) []product {
	t.Helper()
	var ps []product
	h.mustGet(t, "/api/products", &ps)
	return ps
}

func findProduct(ps []product, name string) (product, bool) {
	for _, p := range ps {
		if p.Name == name {
			return p, true
		}
	}
	return product{}, false
}

// ---------------------------------------------------------------------------
// 1. Full sale flow across every module.
// ---------------------------------------------------------------------------

func TestFullSaleFlow(t *testing.T) {
	h := newHarness(t)
	h.login(t, bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	ev := h.draftEvent(t)
	cat := h.createCategory(t, "Bebidas")
	cerveza := h.createProduct(t, cat, "Cerveza", 8000, 10, "bar")
	hamburguesa := h.createProduct(t, cat, "Hamburguesa", 15000, 10, "cocina")
	att := h.createAttendee(t, "Juan", 50000)
	if att.Balance != 50000 {
		t.Fatalf("initial balance: got %d, want 50000", att.Balance)
	}

	// Configuration done while draft; activate before selling.
	h.activate(t, ev)

	h.mustPost(t, "/api/caja/open", map[string]any{"opening_float": 100000}, nil)

	var ord order
	h.mustPost(t, "/api/orders", map[string]any{
		"request_id": "req-full-1",
		"items": []map[string]any{
			{"product_id": cerveza.ID, "qty": 2},
			{"product_id": hamburguesa.ID, "qty": 1},
		},
		"payers": []map[string]any{{"attendee_id": att.ID}},
	}, &ord)

	if ord.Total != 31000 {
		t.Fatalf("total: got %d, want 31000", ord.Total)
	}
	if ord.PaidFromBalance != 31000 {
		t.Fatalf("paid_from_balance: got %d, want 31000", ord.PaidFromBalance)
	}
	if ord.PaidCash != 0 {
		t.Fatalf("paid_cash: got %d, want 0", ord.PaidCash)
	}

	if bal := h.attendeeBalance(t, att.ID); bal != 19000 {
		t.Fatalf("balance after sale: got %d, want 19000", bal)
	}

	ps := h.products(t)
	if p, ok := findProduct(ps, "Cerveza"); !ok || p.StockQty != 8 {
		t.Fatalf("Cerveza stock: got %d (found=%v), want 8", p.StockQty, ok)
	}
	if p, ok := findProduct(ps, "Hamburguesa"); !ok || p.StockQty != 9 {
		t.Fatalf("Hamburguesa stock: got %d (found=%v), want 9", p.StockQty, ok)
	}

	var board []struct {
		Items []json.RawMessage `json:"items"`
	}
	h.mustGet(t, "/api/pendientes", &board)
	if len(board) != 1 {
		t.Fatalf("pendientes board: got %d orders, want 1", len(board))
	}

	var dash struct {
		TotalSold int64 `json:"total_sold"`
	}
	h.mustGet(t, "/api/reports/dashboard", &dash)
	if dash.TotalSold != 31000 {
		t.Fatalf("dashboard total_sold: got %d, want 31000", dash.TotalSold)
	}

	var integ struct {
		OK    bool `json:"ok"`
		Audit struct {
			Intact bool `json:"intact"`
		} `json:"audit"`
	}
	h.mustGet(t, "/api/integrity", &integ)
	if !integ.OK {
		t.Fatalf("integrity not ok: %+v", integ)
	}
	if !integ.Audit.Intact {
		t.Fatalf("audit chain not intact")
	}

	var closed struct {
		ExpectedCash int64 `json:"expected_cash"`
		CountedCash  int64 `json:"counted_cash"`
		Variance     int64 `json:"variance"`
	}
	h.mustPost(t, "/api/caja/close", map[string]any{"counted_cash": 100000}, &closed)
	if closed.Variance != 0 {
		t.Fatalf("caja variance: got %d, want 0 (expected=%d counted=%d)",
			closed.Variance, closed.ExpectedCash, closed.CountedCash)
	}
}

// ---------------------------------------------------------------------------
// 2. Idempotent order: same request_id charges exactly once.
// ---------------------------------------------------------------------------

func TestIdempotentOrder(t *testing.T) {
	h := newHarness(t)
	h.login(t, bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	ev := h.draftEvent(t)
	cat := h.createCategory(t, "Bebidas")
	cerveza := h.createProduct(t, cat, "Cerveza", 8000, 10, "bar")
	att := h.createAttendee(t, "Ana", 50000)
	h.activate(t, ev)

	body := map[string]any{
		"request_id": "dupe-1",
		"items":      []map[string]any{{"product_id": cerveza.ID, "qty": 1}},
		"payers":     []map[string]any{{"attendee_id": att.ID}},
	}

	var first order
	h.mustPost(t, "/api/orders", body, &first)
	balAfterFirst := h.attendeeBalance(t, att.ID)
	if balAfterFirst != 42000 {
		t.Fatalf("balance after first: got %d, want 42000", balAfterFirst)
	}

	var second order
	h.mustPost(t, "/api/orders", body, &second)
	if second.ID != first.ID {
		t.Fatalf("idempotent replay returned different order: %s vs %s", second.ID, first.ID)
	}
	if bal := h.attendeeBalance(t, att.ID); bal != balAfterFirst {
		t.Fatalf("balance changed on replay: got %d, want %d", bal, balAfterFirst)
	}

	var list []idResp
	h.mustGet(t, "/api/orders", &list)
	if len(list) != 1 {
		t.Fatalf("order count after replay: got %d, want 1", len(list))
	}
}

// ---------------------------------------------------------------------------
// 3. Overage requires cash confirmation.
// ---------------------------------------------------------------------------

func TestOverageRequiresCash(t *testing.T) {
	h := newHarness(t)
	h.login(t, bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	ev := h.draftEvent(t)
	cat := h.createCategory(t, "Bebidas")
	cerveza := h.createProduct(t, cat, "Cerveza", 8000, 10, "bar")
	att := h.createAttendee(t, "Beto", 5000)
	h.activate(t, ev)

	// Without cash_confirmed the overage (8000 > 5000) must be rejected.
	code, data := h.req(t, "POST", "/api/orders", map[string]any{
		"request_id": "over-1",
		"items":      []map[string]any{{"product_id": cerveza.ID, "qty": 1}},
		"payers":     []map[string]any{{"attendee_id": att.ID}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("uncconfirmed overage: status %d, want 400 (body %s)", code, data)
	}
	var errBody struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &errBody)
	if errBody.Error != core.ErrCashNotConfirmed.Error() {
		t.Fatalf("error message: got %q, want %q", errBody.Error, core.ErrCashNotConfirmed.Error())
	}

	// With cash_confirmed the sale proceeds: balance is exhausted and the
	// residual is collected as cash.
	var ord order
	h.mustPost(t, "/api/orders", map[string]any{
		"request_id":     "over-2",
		"items":          []map[string]any{{"product_id": cerveza.ID, "qty": 1}},
		"payers":         []map[string]any{{"attendee_id": att.ID}},
		"cash_confirmed": true,
	}, &ord)

	if bal := h.attendeeBalance(t, att.ID); bal != 0 {
		t.Fatalf("balance after overage sale: got %d, want 0", bal)
	}
	var cash int64
	for _, p := range ord.Payments {
		if p.Method == "cash" {
			cash += p.Amount
		}
	}
	if cash != 3000 {
		t.Fatalf("cash payment recorded: got %d, want 3000", cash)
	}
}

// ---------------------------------------------------------------------------
// 4. RBAC: roles are enforced server-side.
// ---------------------------------------------------------------------------

func TestRBAC(t *testing.T) {
	h := newHarness(t)
	h.login(t, bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	ev := h.draftEvent(t)
	cat := h.createCategory(t, "Bebidas")
	cerveza := h.createProduct(t, cat, "Cerveza", 8000, 10, "bar")
	att := h.createAttendee(t, "Cliente", 50000)
	// Activate while still admin (config is locked once active; activation is
	// admin-only) before switching to the mesero session to sell.
	h.activate(t, ev)

	h.mustPost(t, "/api/users", map[string]any{
		"username": "mesero1", "full_name": "Mesero Uno", "role": "mesero", "pin": "4321",
	}, nil)

	// Switch session to the mesero (cookie is overwritten on login).
	h.login(t, "mesero1", "4321")

	// Mesero cannot create products (admin-only) -> 403.
	code, data := h.req(t, "POST", "/api/products", map[string]any{
		"category_id": cat, "name": "Refresco", "price": 5000,
		"prep_station": "bar", "track_stock": 0, "stock_qty": 0,
	})
	if code != http.StatusForbidden {
		t.Fatalf("mesero create product: status %d, want 403 (body %s)", code, data)
	}

	// Mesero can create orders (sell role).
	var ord order
	h.mustPost(t, "/api/orders", map[string]any{
		"request_id": "mesero-1",
		"items":      []map[string]any{{"product_id": cerveza.ID, "qty": 1}},
		"payers":     []map[string]any{{"attendee_id": att.ID}},
	}, &ord)
	if ord.Total != 8000 {
		t.Fatalf("mesero order total: got %d, want 8000", ord.Total)
	}

	// Unauthenticated request to a protected route -> 401.
	resp, err := http.Get(h.ts.URL + "/api/events")
	if err != nil {
		t.Fatalf("anon get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anon protected route: status %d, want 401", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// 5. QR lookup.
// ---------------------------------------------------------------------------

func TestQRLookup(t *testing.T) {
	h := newHarness(t)
	h.login(t, bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	// QR lookup is event-status independent; configuration stays in draft.
	h.draftEvent(t)
	att := h.createAttendee(t, "Dora", 10000)

	var got attendee
	h.mustGet(t, "/api/qr/"+att.QRToken, &got)
	if got.ID != att.ID {
		t.Fatalf("qr lookup id: got %s, want %s", got.ID, att.ID)
	}

	code, data := h.req(t, "GET", "/api/qr/does-not-exist", nil)
	if code != http.StatusNotFound {
		t.Fatalf("unknown qr token: status %d, want 404 (body %s)", code, data)
	}
}

// ---------------------------------------------------------------------------
// 6. Void reverses stock and balance; double-void is rejected.
// ---------------------------------------------------------------------------

func TestVoidReverses(t *testing.T) {
	h := newHarness(t)
	h.login(t, bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	ev := h.draftEvent(t)
	cat := h.createCategory(t, "Bebidas")
	cerveza := h.createProduct(t, cat, "Cerveza", 8000, 10, "bar")
	att := h.createAttendee(t, "Eva", 50000)
	h.activate(t, ev)

	var ord order
	h.mustPost(t, "/api/orders", map[string]any{
		"request_id": "void-1",
		"items":      []map[string]any{{"product_id": cerveza.ID, "qty": 2}},
		"payers":     []map[string]any{{"attendee_id": att.ID}},
	}, &ord)

	if bal := h.attendeeBalance(t, att.ID); bal != 34000 {
		t.Fatalf("balance after sale: got %d, want 34000", bal)
	}
	if p, _ := findProduct(h.products(t), "Cerveza"); p.StockQty != 8 {
		t.Fatalf("stock after sale: got %d, want 8", p.StockQty)
	}

	h.mustPost(t, "/api/orders/"+ord.ID+"/void", map[string]any{}, nil)

	if bal := h.attendeeBalance(t, att.ID); bal != 50000 {
		t.Fatalf("balance after void: got %d, want 50000 (refunded)", bal)
	}
	if p, _ := findProduct(h.products(t), "Cerveza"); p.StockQty != 10 {
		t.Fatalf("stock after void: got %d, want 10 (restored)", p.StockQty)
	}

	// Integrity must still hold after the compensating reversal.
	var integ struct {
		OK bool `json:"ok"`
	}
	h.mustGet(t, "/api/integrity", &integ)
	if !integ.OK {
		t.Fatalf("integrity not ok after void")
	}

	// Double void is rejected.
	code, data := h.req(t, "POST", "/api/orders/"+ord.ID+"/void", map[string]any{})
	if code == http.StatusOK {
		t.Fatalf("double void unexpectedly succeeded (body %s)", data)
	}
}

// ---------------------------------------------------------------------------
// 7. No active event: dashboard fails with ErrNoActiveEvent.
// ---------------------------------------------------------------------------

func TestNoActiveEvent(t *testing.T) {
	h := newHarness(t)
	h.login(t, bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	code, data := h.req(t, "GET", "/api/reports/dashboard", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("dashboard with no active event: status %d, want 400 (body %s)", code, data)
	}
	var errBody struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &errBody)
	if errBody.Error != core.ErrNoActiveEvent.Error() {
		t.Fatalf("error message: got %q, want %q", errBody.Error, core.ErrNoActiveEvent.Error())
	}
}

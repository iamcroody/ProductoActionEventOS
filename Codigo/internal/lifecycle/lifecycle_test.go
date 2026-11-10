// Package lifecycle is an exhaustive end-to-end HTTP integration suite for
// Action EventOS covering the full operation lifecycle of a single event:
// create → activate → catalog (categories + products with cost) → inventory
// restock → attendees (single + bulk import) → open caja → many sales (pure
// balance, overage+cash, multi-payer cascade split) → kitchen prep flow →
// dashboard (with numeric profit/margin assertions) → tips distribution
// (exact-sum, top-3+aseo bonus) → close caja (expected_cash/variance) →
// close event (auto-backup recorded) → second event isolation.
//
// The server is built in-process from the same wiring used by cmd/eventpos,
// driven exclusively through real HTTP via httptest + cookiejar.
package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
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
// Harness
// ---------------------------------------------------------------------------

// harness wraps the in-process httptest server together with a cookie-jar HTTP
// client (so the admin session persists) and the config/backup services (for
// direct assertions that have no HTTP route).
type harness struct {
	ts     *httptest.Server
	client *http.Client
	cfg    *config.Store
	bkp    *backup.Service
}

// newHarness boots the fully-wired server on a fresh temp SQLite DB, seeds the
// default admin (action/croody), and returns the test harness.
func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	d, err := db.Open(filepath.Join(t.TempDir(), "lc.db"))
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
	bkpSvc, err := backup.New(d, t.TempDir())
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	authSvc := auth.New(d)
	application := &app.App{DB: d, Writer: w, Hub: hub, DataDir: t.TempDir()}

	if _, _, err := bootstrap.SeedAdmin(ctx, d); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	deps := server.Deps{App: application, Auth: authSvc, Config: cfg, Backup: bkpSvc}
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
	return &harness{ts: ts, client: &http.Client{Jar: jar}, cfg: cfg, bkp: bkpSvc}
}

// req issues one HTTP request through the cookie-jar client, returning the
// status code and raw body bytes.
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

// mustPost POSTs, asserts 200 or 201, and decodes the response into out when
// non-nil.
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

// mustGet GETs, asserts 200, and decodes the response into out when non-nil.
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

// login switches the cookie-jar session to the given user.
func (h *harness) login(t *testing.T, username, secret string) {
	t.Helper()
	h.mustPost(t, "/api/auth/login", map[string]any{"username": username, "secret": secret}, nil)
}

// ---------------------------------------------------------------------------
// Response-shape structs (only the fields assertions read)
// ---------------------------------------------------------------------------

type idResp struct {
	ID string `json:"id"`
}

type productResp struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Cost     int64  `json:"cost"`
	StockQty int64  `json:"stock_qty"`
}

type attendeeResp struct {
	ID      string `json:"id"`
	QRToken string `json:"qr_token"`
	Balance int64  `json:"balance"`
}

type bulkImportResp struct {
	Count     int            `json:"count"`
	Attendees []attendeeResp `json:"attendees"`
}

type paymentResp struct {
	Method string `json:"method"`
	Amount int64  `json:"amount"`
}

type splitResp struct {
	AttendeeID      string `json:"attendee_id"`
	ShareAmount     int64  `json:"share_amount"`
	PaidFromBalance int64  `json:"paid_from_balance"`
	CashCollected   int64  `json:"cash_collected"`
}

type orderResp struct {
	ID              string        `json:"id"`
	Status          string        `json:"status"`
	Total           int64         `json:"total"`
	PaidFromBalance int64         `json:"paid_from_balance"`
	PaidCash        int64         `json:"paid_cash"`
	IsSplit         bool          `json:"is_split"`
	Payments        []paymentResp `json:"payments"`
	Splits          []splitResp   `json:"splits"`
}

type boardItemResp struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Qty          int64  `json:"qty"`
	DeliveredQty int64  `json:"delivered_qty"`
	Pending      int64  `json:"pending"`
}

type boardOrderResp struct {
	OrderID string          `json:"order_id"`
	Items   []boardItemResp `json:"items"`
}

type dashboardResp struct {
	EventID      string  `json:"event_id"`
	TotalSold    int64   `json:"total_sold"`
	TotalCost    int64   `json:"total_cost"`
	TotalProfit  int64   `json:"total_profit"`
	ProfitMargin float64 `json:"profit_margin"`
	OrderCount   int64   `json:"order_count"`
}

type tipShareResp struct {
	Name  string `json:"name"`
	Role  string `json:"role"`
	Top3  bool   `json:"top3"`
	Bonus bool   `json:"bonus"`
	Share int64  `json:"share"`
}

type tipsResp struct {
	Pool        int64          `json:"pool"`
	Meseros     []tipShareResp `json:"meseros"`
	Aseo        []tipShareResp `json:"aseo"`
	Distributed int64          `json:"distributed"`
}

type cajaSessionResp struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	OpeningFloat int64  `json:"opening_float"`
	ExpectedCash *int64 `json:"expected_cash"`
	CountedCash  *int64 `json:"counted_cash"`
	Variance     *int64 `json:"variance"`
}

// ---------------------------------------------------------------------------
// TestFullLifecycle: the single authoritative lifecycle test
// ---------------------------------------------------------------------------
//
// Scenario numbers below track the operation group.

func TestFullLifecycle(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.login(t, bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	// ==========================================================================
	// § 1  EVENT – create (starts in 'draft'; activation happens after setup)
	// ==========================================================================
	//
	// Under the lock-down lifecycle all configuration (catalog, inventory,
	// attendees, top-ups) is only permitted while the event is 'draft'. The
	// event is therefore created here and activated in § 6 once setup is done,
	// immediately before the first sale.

	var ev1 idResp
	h.mustPost(t, "/api/events", map[string]any{
		"name":          "Feria Test 2026",
		"default_topup": 100000,
		"split_policy":  "cascade",
	}, &ev1)
	if ev1.ID == "" {
		t.Fatal("event id is empty")
	}

	// The current (draft) event is reported by /api/events/current (the
	// draft-or-active endpoint); /api/events/active is empty until activation.
	var currentEvt struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	h.mustGet(t, "/api/events/current", &currentEvt)
	if currentEvt.ID != ev1.ID || currentEvt.Status != "draft" {
		t.Fatalf("current event: got id=%s status=%s, want id=%s status=draft", currentEvt.ID, currentEvt.Status, ev1.ID)
	}

	// ==========================================================================
	// § 2  CATALOG – categories + products with cost
	// ==========================================================================

	// Categories
	var catComidas, catBebidas idResp
	h.mustPost(t, "/api/categories", map[string]any{"name": "Comidas", "sort_order": 1}, &catComidas)
	h.mustPost(t, "/api/categories", map[string]any{"name": "Bebidas", "sort_order": 2}, &catBebidas)

	// Products (all track_stock=1 so stock assertions are observable)
	//
	//   Agua       Bebidas  price=3000   cost=1000  station=none   stock=50
	//   Cerveza    Bebidas  price=8000   cost=3000  station=bar    stock=20
	//   Hamburguesa Comidas price=15000  cost=6000  station=cocina stock=10
	var pAgua, pCerveza, pHamburguesa productResp
	h.mustPost(t, "/api/products", map[string]any{
		"category_id": catBebidas.ID, "name": "Agua",
		"price": 3000, "cost": 1000, "prep_station": "none",
		"track_stock": 1, "stock_qty": 50,
	}, &pAgua)
	h.mustPost(t, "/api/products", map[string]any{
		"category_id": catBebidas.ID, "name": "Cerveza",
		"price": 8000, "cost": 3000, "prep_station": "bar",
		"track_stock": 1, "stock_qty": 20,
	}, &pCerveza)
	h.mustPost(t, "/api/products", map[string]any{
		"category_id": catComidas.ID, "name": "Hamburguesa",
		"price": 15000, "cost": 6000, "prep_station": "cocina",
		"track_stock": 1, "stock_qty": 10,
	}, &pHamburguesa)

	if pAgua.ID == "" || pCerveza.ID == "" || pHamburguesa.ID == "" {
		t.Fatalf("product ids: agua=%s cerveza=%s hamburguesa=%s", pAgua.ID, pCerveza.ID, pHamburguesa.ID)
	}

	// Verify cost field is stored.
	if pCerveza.Cost != 3000 {
		t.Errorf("Cerveza.cost: got %d, want 3000", pCerveza.Cost)
	}
	if pHamburguesa.Cost != 6000 {
		t.Errorf("Hamburguesa.cost: got %d, want 6000", pHamburguesa.Cost)
	}

	// ==========================================================================
	// § 3  INVENTORY – restock Hamburguesa by +10
	// ==========================================================================

	h.mustPost(t, "/api/inventory/adjust", map[string]any{
		"product_id": pHamburguesa.ID,
		"delta":      10,
		"reason":     "restock",
	}, nil)

	// Verify stock is now 20.
	var allProds []productResp
	h.mustGet(t, "/api/products", &allProds)
	var prodByName = func(name string) (productResp, bool) {
		for _, p := range allProds {
			if p.Name == name {
				return p, true
			}
		}
		return productResp{}, false
	}
	if p, ok := prodByName("Hamburguesa"); !ok || p.StockQty != 20 {
		t.Fatalf("Hamburguesa stock after restock: got %d (found=%v), want 20", p.StockQty, ok)
	}

	// ==========================================================================
	// § 4  ATTENDEES – single create + bulk import
	// ==========================================================================

	// Single: Ana with 50000 initial balance.
	var ana attendeeResp
	h.mustPost(t, "/api/attendees", map[string]any{
		"display_name": "Ana", "initial_topup": 50000,
	}, &ana)
	if ana.Balance != 50000 {
		t.Fatalf("Ana initial balance: got %d, want 50000", ana.Balance)
	}

	// Bulk: Beto(30000) + Carlos(40000).
	var importResult bulkImportResp
	h.mustPost(t, "/api/attendees/import", map[string]any{
		"attendees": []map[string]any{
			{"display_name": "Beto", "initial_topup": 30000},
			{"display_name": "Carlos", "initial_topup": 40000},
		},
	}, &importResult)
	if importResult.Count != 2 {
		t.Fatalf("bulk import count: got %d, want 2", importResult.Count)
	}

	// Find Beto and Carlos from the import response.
	var beto, carlos attendeeResp
	for _, a := range importResult.Attendees {
		// Fetch each to get the balance (the import response includes it directly).
		switch {
		case a.Balance == 30000:
			beto = a
		case a.Balance == 40000:
			carlos = a
		}
	}
	if beto.ID == "" || carlos.ID == "" {
		t.Fatalf("could not identify Beto/Carlos from import: %+v", importResult.Attendees)
	}

	// Verify attendees are accessible via GET.
	var gotAna attendeeResp
	h.mustGet(t, "/api/attendees/"+ana.ID, &gotAna)
	if gotAna.Balance != 50000 {
		t.Errorf("Ana GET balance: got %d, want 50000", gotAna.Balance)
	}

	// Verify QR lookup works for Ana.
	var qrAna attendeeResp
	h.mustGet(t, "/api/qr/"+ana.QRToken, &qrAna)
	if qrAna.ID != ana.ID {
		t.Errorf("QR lookup: got id=%s, want %s", qrAna.ID, ana.ID)
	}

	// ==========================================================================
	// § 5  STAFF – mesero users for tips distribution test
	// ==========================================================================
	//
	// Create 4 mesero users. None will have orders attributed to them (all admin
	// orders). With all sales=0 the tips engine ranks them alphabetically by
	// username: mesero_tip_a..c → top-3 (bonus), mesero_tip_d → no bonus.

	meseroUsers := []struct{ username, pin string }{
		{"mesero_tip_a", "1001"},
		{"mesero_tip_b", "1002"},
		{"mesero_tip_c", "1003"},
		{"mesero_tip_d", "1004"},
	}
	for _, mu := range meseroUsers {
		h.mustPost(t, "/api/users", map[string]any{
			"username": mu.username, "full_name": mu.username,
			"role": "mesero", "pin": mu.pin,
		}, nil)
	}

	// Aseo staff are stored as a JSON array in app_config — no HTTP route, set
	// directly via the config service exposed on the harness.
	aseoJSON, _ := json.Marshal([]string{"Limpiador", "Trapero"})
	if err := h.cfg.Set(ctx, "aseo_staff", string(aseoJSON), "test"); err != nil {
		t.Fatalf("set aseo_staff: %v", err)
	}

	// ==========================================================================
	// § 6  ACTIVATE + CAJA – configuration complete, begin operations
	// ==========================================================================
	//
	// All configuration above ran while the event was draft. Activate it now so
	// selling, delivery and reports become available.
	h.mustPost(t, "/api/events/"+ev1.ID+"/activate", map[string]any{}, nil)

	// Verify event is now active.
	var activeEvt struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	h.mustGet(t, "/api/events/active", &activeEvt)
	if activeEvt.ID != ev1.ID || activeEvt.Status != "active" {
		t.Fatalf("active event: got id=%s status=%s, want id=%s status=active", activeEvt.ID, activeEvt.Status, ev1.ID)
	}

	const openingFloat = int64(200000)
	h.mustPost(t, "/api/caja/open", map[string]any{"opening_float": openingFloat}, nil)

	// ==========================================================================
	// § 7  SALES
	//
	//   Sale 1 – pure balance:  Ana buys 1×Cerveza(8000) + 1×Agua(3000) = 11000
	//   Sale 2 – overage+cash:  Beto buys 3×Hamburguesa(45000); balance=30000;
	//                           cash_confirmed=true; cash_due=15000
	//   Sale 3 – split:         Ana + Carlos each share 2×Cerveza(16000);
	//                           cascade → each pays 8000 from balance
	// ==========================================================================

	// --- Sale 1: pure balance ---
	var ord1 orderResp
	h.mustPost(t, "/api/orders", map[string]any{
		"request_id": "lifecycle-ord-1",
		"items":      []map[string]any{{"product_id": pCerveza.ID, "qty": 1}, {"product_id": pAgua.ID, "qty": 1}},
		"payers":     []map[string]any{{"attendee_id": ana.ID}},
	}, &ord1)

	if ord1.Total != 11000 {
		t.Errorf("sale1 total: got %d, want 11000", ord1.Total)
	}
	if ord1.PaidFromBalance != 11000 {
		t.Errorf("sale1 paid_from_balance: got %d, want 11000", ord1.PaidFromBalance)
	}
	if ord1.PaidCash != 0 {
		t.Errorf("sale1 paid_cash: got %d, want 0", ord1.PaidCash)
	}

	// Ana's balance after Sale 1: 50000 - 11000 = 39000
	var anaAfter1 attendeeResp
	h.mustGet(t, "/api/attendees/"+ana.ID, &anaAfter1)
	if anaAfter1.Balance != 39000 {
		t.Errorf("Ana balance after sale1: got %d, want 39000", anaAfter1.Balance)
	}

	// Stock: Cerveza 20-1=19, Agua 50-1=49
	h.mustGet(t, "/api/products", &allProds)
	if p, ok := prodByName("Cerveza"); !ok || p.StockQty != 19 {
		t.Errorf("Cerveza stock after sale1: got %d (found=%v), want 19", p.StockQty, ok)
	}
	if p, ok := prodByName("Agua"); !ok || p.StockQty != 49 {
		t.Errorf("Agua stock after sale1: got %d (found=%v), want 49", p.StockQty, ok)
	}

	// --- Sale 2: overage + cash ---
	//
	// Without cash_confirmed the server must reject the overage (45000 > 30000).
	codeNoConfirm, _ := h.req(t, "POST", "/api/orders", map[string]any{
		"request_id": "lifecycle-ord-2-noconf",
		"items":      []map[string]any{{"product_id": pHamburguesa.ID, "qty": 3}},
		"payers":     []map[string]any{{"attendee_id": beto.ID}},
	})
	if codeNoConfirm != http.StatusBadRequest {
		t.Errorf("overage without cash_confirmed: got status %d, want 400", codeNoConfirm)
	}

	var ord2 orderResp
	h.mustPost(t, "/api/orders", map[string]any{
		"request_id":     "lifecycle-ord-2",
		"items":          []map[string]any{{"product_id": pHamburguesa.ID, "qty": 3}},
		"payers":         []map[string]any{{"attendee_id": beto.ID}},
		"cash_confirmed": true,
	}, &ord2)

	if ord2.Total != 45000 {
		t.Errorf("sale2 total: got %d, want 45000", ord2.Total)
	}
	if ord2.PaidFromBalance != 30000 {
		t.Errorf("sale2 paid_from_balance: got %d, want 30000", ord2.PaidFromBalance)
	}
	if ord2.PaidCash != 15000 {
		t.Errorf("sale2 paid_cash: got %d, want 15000", ord2.PaidCash)
	}

	// Beto's balance after Sale 2: 30000 - 30000 = 0
	var betoAfter2 attendeeResp
	h.mustGet(t, "/api/attendees/"+beto.ID, &betoAfter2)
	if betoAfter2.Balance != 0 {
		t.Errorf("Beto balance after sale2: got %d, want 0", betoAfter2.Balance)
	}

	// Verify the cash payment amount.
	var cashFromSale2 int64
	for _, p := range ord2.Payments {
		if p.Method == "cash" {
			cashFromSale2 += p.Amount
		}
	}
	if cashFromSale2 != 15000 {
		t.Errorf("sale2 cash payment: got %d, want 15000", cashFromSale2)
	}

	// Stock: Hamburguesa 20-3=17
	h.mustGet(t, "/api/products", &allProds)
	if p, ok := prodByName("Hamburguesa"); !ok || p.StockQty != 17 {
		t.Errorf("Hamburguesa stock after sale2: got %d (found=%v), want 17", p.StockQty, ok)
	}

	// --- Sale 3: multi-payer cascade split ---
	//
	// Ana (balance=39000) + Carlos (balance=40000) buy 2×Cerveza (total=16000).
	// Cascade policy: base share = 8000 each; both can cover their share.
	var ord3 orderResp
	h.mustPost(t, "/api/orders", map[string]any{
		"request_id": "lifecycle-ord-3",
		"items":      []map[string]any{{"product_id": pCerveza.ID, "qty": 2}},
		"payers": []map[string]any{
			{"attendee_id": ana.ID},
			{"attendee_id": carlos.ID},
		},
	}, &ord3)

	if ord3.Total != 16000 {
		t.Errorf("sale3 total: got %d, want 16000", ord3.Total)
	}
	if ord3.PaidFromBalance != 16000 {
		t.Errorf("sale3 paid_from_balance: got %d, want 16000", ord3.PaidFromBalance)
	}
	if ord3.PaidCash != 0 {
		t.Errorf("sale3 paid_cash: got %d, want 0", ord3.PaidCash)
	}
	if !ord3.IsSplit {
		t.Error("sale3 is_split: got false, want true")
	}
	if len(ord3.Splits) != 2 {
		t.Errorf("sale3 splits: got %d, want 2", len(ord3.Splits))
	}

	// Per-payer split amounts.
	splitByAttendee := map[string]splitResp{}
	for _, s := range ord3.Splits {
		splitByAttendee[s.AttendeeID] = s
	}
	if s, ok := splitByAttendee[ana.ID]; !ok || s.ShareAmount != 8000 || s.PaidFromBalance != 8000 {
		t.Errorf("sale3 Ana split: ok=%v share=%d from_balance=%d", ok, s.ShareAmount, s.PaidFromBalance)
	}
	if s, ok := splitByAttendee[carlos.ID]; !ok || s.ShareAmount != 8000 || s.PaidFromBalance != 8000 {
		t.Errorf("sale3 Carlos split: ok=%v share=%d from_balance=%d", ok, s.ShareAmount, s.PaidFromBalance)
	}

	// Balance assertions after Sale 3.
	var anaAfter3, carlosAfter3 attendeeResp
	h.mustGet(t, "/api/attendees/"+ana.ID, &anaAfter3)
	h.mustGet(t, "/api/attendees/"+carlos.ID, &carlosAfter3)
	if anaAfter3.Balance != 31000 { // 39000 - 8000
		t.Errorf("Ana balance after sale3: got %d, want 31000", anaAfter3.Balance)
	}
	if carlosAfter3.Balance != 32000 { // 40000 - 8000
		t.Errorf("Carlos balance after sale3: got %d, want 32000", carlosAfter3.Balance)
	}

	// Stock: Cerveza 19-2=17
	h.mustGet(t, "/api/products", &allProds)
	if p, ok := prodByName("Cerveza"); !ok || p.StockQty != 17 {
		t.Errorf("Cerveza stock after sale3: got %d (found=%v), want 17", p.StockQty, ok)
	}

	// ==========================================================================
	// § 8  KITCHEN – prep flow (queued → preparing → ready → delivered)
	// ==========================================================================

	// --- Pendientes board: every sold item awaits delivery (no prep stations) ---
	var board []boardOrderResp
	h.mustGet(t, "/api/pendientes", &board)
	if len(board) == 0 {
		t.Fatalf("pendientes board: got 0 orders, want the sold orders to appear")
	}

	// Partial delivery: find the Hamburguesa item (from Sale 2) and deliver it.
	var hambItemID string
	for _, bo := range board {
		for _, it := range bo.Items {
			if it.Name == "Hamburguesa" {
				hambItemID = it.ID
			}
		}
	}
	if hambItemID == "" {
		t.Fatalf("pendientes board: Hamburguesa not found")
	}
	h.mustPost(t, "/api/pendientes/items/"+hambItemID+"/entregar-todo", map[string]any{}, nil)

	// The Hamburguesa must no longer be pending.
	h.mustGet(t, "/api/pendientes", &board)
	for _, bo := range board {
		for _, it := range bo.Items {
			if it.ID == hambItemID {
				t.Errorf("Hamburguesa still pending after entregar-todo")
			}
		}
	}

	// Deliver everything else; the board must end empty.
	for _, bo := range board {
		for _, it := range bo.Items {
			h.mustPost(t, "/api/pendientes/items/"+it.ID+"/entregar-todo", map[string]any{}, nil)
		}
	}
	var emptyBoard []boardOrderResp
	h.mustGet(t, "/api/pendientes", &emptyBoard)
	if len(emptyBoard) != 0 {
		t.Errorf("pendientes board after delivering all: got %d orders, want 0", len(emptyBoard))
	}

	// History should show delivered items.
	var history []json.RawMessage
	h.mustGet(t, "/api/pendientes/history", &history)
	if len(history) == 0 {
		t.Error("history: expected at least 1 delivered item")
	}

	// ==========================================================================
	// § 9  DASHBOARD – numeric profit assertions
	//
	//   total_sold   = 11000 + 45000 + 16000             = 72000
	//   total_cost   = (1×1000+1×3000) + 3×6000 + 2×3000 = 28000
	//   total_profit = 72000 − 28000                      = 44000
	//   margin       = 44000/72000                         ≈ 0.6111
	// ==========================================================================

	const (
		wantTotalSold   = int64(72000)
		wantTotalCost   = int64(28000)
		wantTotalProfit = int64(44000)
		wantOrderCount  = int64(3)
	)

	var dash dashboardResp
	h.mustGet(t, "/api/reports/dashboard", &dash)

	if dash.TotalSold != wantTotalSold {
		t.Errorf("dashboard total_sold: got %d, want %d", dash.TotalSold, wantTotalSold)
	}
	if dash.TotalCost != wantTotalCost {
		t.Errorf("dashboard total_cost: got %d, want %d", dash.TotalCost, wantTotalCost)
	}
	if dash.TotalProfit != wantTotalProfit {
		t.Errorf("dashboard total_profit: got %d, want %d", dash.TotalProfit, wantTotalProfit)
	}
	if dash.OrderCount != wantOrderCount {
		t.Errorf("dashboard order_count: got %d, want %d", dash.OrderCount, wantOrderCount)
	}

	// total_sold == total_cost + total_profit (identity check)
	if dash.TotalSold != dash.TotalCost+dash.TotalProfit {
		t.Errorf("dashboard identity: total_sold(%d) != total_cost(%d)+total_profit(%d)",
			dash.TotalSold, dash.TotalCost, dash.TotalProfit)
	}

	// profit_margin = total_profit / total_sold  (floating-point)
	wantMargin := float64(wantTotalProfit) / float64(wantTotalSold)
	if math.Abs(dash.ProfitMargin-wantMargin) > 1e-9 {
		t.Errorf("dashboard profit_margin: got %.12f, want %.12f", dash.ProfitMargin, wantMargin)
	}

	// Sanity: margin * total_sold ≈ total_profit (within 1 peso of rounding)
	reconstructed := int64(math.Round(dash.ProfitMargin * float64(dash.TotalSold)))
	if diff := reconstructed - dash.TotalProfit; diff < -1 || diff > 1 {
		t.Errorf("dashboard margin consistency: margin×total_sold=%d, total_profit=%d (diff=%d)",
			reconstructed, dash.TotalProfit, diff)
	}

	// ==========================================================================
	// § 10  TIPS – exact-sum distribution, top-3 + aseo bonus
	//
	// Pool = 100000 with 4 meseros (all 0 sales) + 2 aseo staff:
	//   bonus recipients:  mesero_tip_a, mesero_tip_b, mesero_tip_c, aseo×2 → 5 (weight 1.05)
	//   non-bonus:         mesero_tip_d                               → 1 (weight 1.0)
	//   sumW = 5×1.05 + 1×1.0 = 6.25
	//   bonus share  = floor(100000×1.05/6.25) = 16800
	//   non-bonus    = floor(100000×1.0/6.25)  = 16000
	//   total        = 5×16800 + 16000          = 100000  ✓ (no remainder needed)
	// ==========================================================================

	const tipsPool = int64(100000)
	var tips tipsResp
	h.mustGet(t, "/api/reports/tips?pool=100000", &tips)

	if tips.Pool != tipsPool {
		t.Errorf("tips pool: got %d, want %d", tips.Pool, tipsPool)
	}

	// Exact-sum invariant: distributed == pool and sum of all shares == pool.
	if tips.Distributed != tipsPool {
		t.Errorf("tips distributed: got %d, want %d", tips.Distributed, tipsPool)
	}
	var tipsSum int64
	for _, ts := range tips.Meseros {
		tipsSum += ts.Share
	}
	for _, ts := range tips.Aseo {
		tipsSum += ts.Share
	}
	if tipsSum != tipsPool {
		t.Errorf("tips sum of shares: got %d, want %d", tipsSum, tipsPool)
	}

	// 4 meseros expected.
	if n := len(tips.Meseros); n != 4 {
		t.Fatalf("tips meseros count: got %d, want 4", n)
	}
	// ORDER BY sales DESC, u.username → with all 0 sales, alphabetical: a,b,c,d.
	topMeseros := []string{"mesero_tip_a", "mesero_tip_b", "mesero_tip_c"}
	for i, name := range topMeseros {
		ms := tips.Meseros[i]
		if ms.Name != name {
			t.Errorf("tips meseros[%d].name: got %q, want %q", i, ms.Name, name)
		}
		if !ms.Top3 {
			t.Errorf("tips meseros[%d](%s).top3: got false, want true", i, ms.Name)
		}
		if !ms.Bonus {
			t.Errorf("tips meseros[%d](%s).bonus: got false, want true", i, ms.Name)
		}
		if ms.Share <= 0 {
			t.Errorf("tips meseros[%d](%s).share: got %d, want >0", i, ms.Name, ms.Share)
		}
	}
	// 4th mesero: no bonus.
	if ms := tips.Meseros[3]; ms.Name != "mesero_tip_d" || ms.Top3 || ms.Bonus {
		t.Errorf("tips meseros[3]: name=%q top3=%v bonus=%v; want name=mesero_tip_d top3=false bonus=false",
			ms.Name, ms.Top3, ms.Bonus)
	}
	// Bonus share must be strictly larger than non-bonus share.
	if tips.Meseros[0].Share <= tips.Meseros[3].Share {
		t.Errorf("tips bonus share (%d) should exceed non-bonus share (%d)",
			tips.Meseros[0].Share, tips.Meseros[3].Share)
	}

	// 2 aseo staff, all bonus.
	if n := len(tips.Aseo); n != 2 {
		t.Fatalf("tips aseo count: got %d, want 2", n)
	}
	for _, a := range tips.Aseo {
		if a.Role != "aseo" {
			t.Errorf("tips aseo %q role: got %q, want aseo", a.Name, a.Role)
		}
		if !a.Bonus {
			t.Errorf("tips aseo %q: bonus=false, want true", a.Name)
		}
	}

	// ==========================================================================
	// § 11  CLOSE CAJA – assert expected_cash and zero variance
	//
	//   opening_float = 200000
	//   cash from sales = 15000  (only Sale 2 produced cash)
	//   expected_cash = 215000
	//   counted_cash  = 215000
	//   variance      = 0
	// ==========================================================================

	const (
		wantExpectedCash = int64(215000)
		countedCash      = int64(215000)
	)

	var closedCaja cajaSessionResp
	h.mustPost(t, "/api/caja/close", map[string]any{"counted_cash": countedCash}, &closedCaja)

	if closedCaja.Status != "closed" {
		t.Errorf("caja status: got %q, want closed", closedCaja.Status)
	}
	if closedCaja.ExpectedCash == nil {
		t.Fatal("caja expected_cash is nil")
	}
	if *closedCaja.ExpectedCash != wantExpectedCash {
		t.Errorf("caja expected_cash: got %d, want %d", *closedCaja.ExpectedCash, wantExpectedCash)
	}
	if closedCaja.Variance == nil {
		t.Fatal("caja variance is nil")
	}
	if *closedCaja.Variance != 0 {
		t.Errorf("caja variance: got %d, want 0 (expected=%d counted=%d)",
			*closedCaja.Variance, *closedCaja.ExpectedCash, *closedCaja.CountedCash)
	}

	// ==========================================================================
	// § 12  CLOSE EVENT – auto-backup row is recorded
	// ==========================================================================

	// Verify audit chain is intact before closing.
	var integ struct {
		OK    bool `json:"ok"`
		Audit struct {
			Intact bool `json:"intact"`
		} `json:"audit"`
	}
	h.mustGet(t, "/api/integrity", &integ)
	if !integ.OK {
		t.Fatalf("integrity not ok before event close: %+v", integ)
	}
	if !integ.Audit.Intact {
		t.Error("audit chain not intact before event close")
	}

	// Close event 1.
	h.mustPost(t, "/api/events/"+ev1.ID+"/close", map[string]any{}, nil)

	// The close handler calls backup.Create non-fatally. Verify the backup was
	// recorded by querying the backup service directly (no HTTP route exists).
	backups, err := h.bkp.List(ctx)
	if err != nil {
		t.Fatalf("backup list: %v", err)
	}
	if len(backups) == 0 {
		t.Error("expected at least 1 backup row after event close, got 0")
	}
	// The auto backup triggered by event close should have kind="auto".
	var hasAutoBackup bool
	for _, b := range backups {
		if b.Kind == "auto" {
			hasAutoBackup = true
			break
		}
	}
	if !hasAutoBackup {
		t.Errorf("no auto-backup row found; backups: %+v", backups)
	}

	// Event 1 is now closed.
	var ev1List []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	h.mustGet(t, "/api/events", &ev1List)
	var ev1Status string
	for _, e := range ev1List {
		if e.ID == ev1.ID {
			ev1Status = e.Status
			break
		}
	}
	if ev1Status != "closed" {
		t.Errorf("event1 status after close: got %q, want closed", ev1Status)
	}

	// ==========================================================================
	// § 13  EVENT 2 – data isolation: event 1's data must not leak
	// ==========================================================================

	var ev2 idResp
	h.mustPost(t, "/api/events", map[string]any{
		"name":          "Feria Vacia",
		"default_topup": 50000,
		"split_policy":  "cascade",
	}, &ev2)
	if ev2.ID == ev1.ID {
		t.Fatalf("event 2 id same as event 1: %s", ev2.ID)
	}
	h.mustPost(t, "/api/events/"+ev2.ID+"/activate", map[string]any{}, nil)

	// Products for event 2 must be empty.
	var ev2Products []productResp
	h.mustGet(t, "/api/products", &ev2Products)
	if len(ev2Products) != 0 {
		t.Errorf("event2 products: got %d items, want 0 (event1 products must not leak)", len(ev2Products))
	}

	// Attendees for event 2 must be empty.
	var ev2Attendees []attendeeResp
	h.mustGet(t, "/api/attendees", &ev2Attendees)
	if len(ev2Attendees) != 0 {
		t.Errorf("event2 attendees: got %d items, want 0 (event1 attendees must not leak)", len(ev2Attendees))
	}

	// Orders for event 2 must be empty.
	var ev2Orders []orderResp
	h.mustGet(t, "/api/orders", &ev2Orders)
	if len(ev2Orders) != 0 {
		t.Errorf("event2 orders: got %d items, want 0 (event1 orders must not leak)", len(ev2Orders))
	}

	// Dashboard for event 2 must show zero revenue.
	var dash2 dashboardResp
	h.mustGet(t, "/api/reports/dashboard", &dash2)
	if dash2.TotalSold != 0 {
		t.Errorf("event2 dashboard total_sold: got %d, want 0", dash2.TotalSold)
	}
	if dash2.OrderCount != 0 {
		t.Errorf("event2 dashboard order_count: got %d, want 0", dash2.OrderCount)
	}
	// The dashboard should reference event 2, not event 1.
	if dash2.EventID != ev2.ID {
		t.Errorf("event2 dashboard event_id: got %s, want %s", dash2.EventID, ev2.ID)
	}
}

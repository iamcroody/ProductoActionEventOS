// Package numbersaudit is an in-process regression suite that verifies every
// number Action EventOS shows a director (the "cierre" / Dashboard figures) is
// arithmetically correct. It is test-only: it adds NO production code.
//
// Strategy: boot the real, fully-wired HTTP server (same modules cmd/eventpos
// uses for reports), then seed a DETERMINISTIC ground-truth scenario directly
// into SQLite with controlled created_at/paid_at/cost/qty. Because every input
// is known exactly, the reported numbers can be asserted EXACTLY against
// hand-computed expectations rather than against the production queries' own
// output (which would be circular). The reports code under audit is
// internal/reports/{reports.go,closing.go,closing_xlsx.go}.
//
// Money is INTEGER COP; timestamps are INTEGER unix-millis UTC.
package numbersaudit

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/xuri/excelize/v2"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/backup"
	"eventpos/internal/bootstrap"
	"eventpos/internal/config"
	"eventpos/internal/core"
	"eventpos/internal/db"
	"eventpos/internal/realtime"
	"eventpos/internal/reports"
	"eventpos/internal/server"
	"eventpos/internal/writer"
)

// ---------------------------------------------------------------------------
// Harness (mirrors internal/apitest): a real in-process server + cookie client,
// but with a direct DB handle so we can seed exact rows.
// ---------------------------------------------------------------------------

type harness struct {
	ts     *httptest.Server
	client *http.Client
	d      *db.DB
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	d, err := db.Open(filepath.Join(t.TempDir(), "audit.db"))
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
	srv := server.New(deps, fstest.MapFS{}, reports.New(deps))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	h := &harness{ts: ts, client: &http.Client{Jar: jar}, d: d}
	h.login(t)
	return h
}

func (h *harness) login(t *testing.T) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"username": bootstrap.DefaultAdminUser, "secret": bootstrap.DefaultAdminSecret,
	})
	resp, err := h.client.Post(h.ts.URL+"/api/auth/login", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("login status %d: %s", resp.StatusCode, body)
	}
}

func (h *harness) getRaw(t *testing.T, path string) []byte {
	t.Helper()
	resp, err := h.client.Get(h.ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status %d: %s", path, resp.StatusCode, body)
	}
	return body
}

func (h *harness) getJSON(t *testing.T, path string, out any) {
	t.Helper()
	if err := json.Unmarshal(h.getRaw(t, path), out); err != nil {
		t.Fatalf("GET %s decode: %v", path, err)
	}
}

func (h *harness) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := h.d.Writer.Exec(q, args...); err != nil {
		t.Fatalf("seed exec failed: %v\nquery: %s", err, q)
	}
}

// ---------------------------------------------------------------------------
// Deterministic seed builders.
// ---------------------------------------------------------------------------

func (h *harness) seedUser(t *testing.T, id, username, role string) {
	t.Helper()
	h.exec(t, `INSERT INTO users(id,username,full_name,role,pin_hash,active,created_at)
	           VALUES(?,?,?,?, 'x', 1, ?)`, id, username, username, role, core.NowMillis())
}

func (h *harness) seedEvent(t *testing.T, id, name, status string) {
	t.Helper()
	h.exec(t, `INSERT INTO events(id,name,status,default_topup,split_policy,created_at)
	           VALUES(?,?,?,50000,'cascade',?)`, id, name, status, core.NowMillis())
}

func (h *harness) seedProduct(t *testing.T, id, eventID, name string, price, cost, track, stock int64) {
	t.Helper()
	h.exec(t, `INSERT INTO products(id,event_id,category_id,name,price,cost,prep_station,track_stock,stock_qty,sort_order,active,created_at)
	           VALUES(?,?,NULL,?,?,?, 'none', ?, ?, 0, 1, ?)`,
		id, eventID, name, price, cost, track, stock, core.NowMillis())
}

func (h *harness) seedAttendee(t *testing.T, id, eventID, name string, balance int64) {
	t.Helper()
	h.exec(t, `INSERT INTO attendees(id,event_id,qr_token,display_name,balance,status,created_at)
	           VALUES(?,?,?,?,?, 'active', ?)`, id, eventID, "qr-"+id, name, balance, core.NowMillis())
}

func (h *harness) seedOrder(t *testing.T, id, eventID, operatorID, status string, total, pfb, pcash, createdAt int64, paidAt *int64) {
	t.Helper()
	h.exec(t, `INSERT INTO orders(id,event_id,operator_id,request_id,source,status,subtotal,total,paid_from_balance,paid_cash,created_at,paid_at)
	           VALUES(?,?,?,?, 'operator', ?, ?, ?, ?, ?, ?, ?)`,
		id, eventID, operatorID, "req-"+id, status, total, total, pfb, pcash, createdAt, paidAt)
}

func (h *harness) seedItem(t *testing.T, id, orderID, productID, name string, qty, unitPrice, unitCost int64) {
	t.Helper()
	h.exec(t, `INSERT INTO order_items(id,order_id,product_id,name,qty,delivered_qty,unit_price,unit_cost,line_total,prep_station,prep_status)
	           VALUES(?,?,?,?,?,0,?,?,?, 'none','queued')`,
		id, orderID, productID, name, qty, unitPrice, unitCost, qty*unitPrice)
}

func (h *harness) seedPayment(t *testing.T, id, orderID string, attendeeID *string, method string, amount, createdAt int64) {
	t.Helper()
	h.exec(t, `INSERT INTO payments(id,order_id,attendee_id,method,amount,created_at,user_id)
	           VALUES(?,?,?,?,?,?, 'u-sys')`, id, orderID, attendeeID, method, amount, createdAt)
}

func ms(v int64) *int64 { return &v }

// ---------------------------------------------------------------------------
// The single canonical ground-truth scenario, with hand-computed expectations.
// ---------------------------------------------------------------------------

const evID = "ev-main"

// expectations holds every figure we computed by hand for the seeded scenario.
type expectations struct {
	revenue, cost, profit, orderCount int64
	margin                            float64
	overageCash, balanceFromSales     int64
	registered, active                int64
	pendingBalance                    int64
	avgTicket, avgPerClient           float64 // dashboard avg_consumption vs per-client
	avgSecondsPerOrder                float64
	anaTotal, betoTotal               int64
	anaAvgSec, betoAvgSec             float64
	anaCount, betoCount               int64
	fastestUser                       string
	fastestAvg                        float64
	fastestCount                      int64
}

// seedScenario builds the deterministic event and returns the expected numbers.
//
// Operators: ana, beto (meseros). Admin "action" is seeded by bootstrap.
//
// Non-voided paid orders (deltas = paid_at-created_at):
//
//	O1 ana  total 31000  bal 31000           +10s  items 2xCerveza,1xHamburguesa
//	O2 ana  total 17000  cash 17000          +20s  items 1xCerveza,3xAgua
//	O3 ana  total 10000  cash 10000          +60s  items 2xServicio
//	O4 beto total  8000  cash  8000          + 5s  items 1xCerveza
//	O5 beto total  8000  cash  8000          + 7s  items 1xCerveza
//	O6 ana  VOIDED total 165000 (must be excluded everywhere)
func (h *harness) seedScenario(t *testing.T) expectations {
	t.Helper()
	h.seedEvent(t, evID, "Fiesta", "active")
	h.seedUser(t, "u-ana", "ana", "mesero")
	h.seedUser(t, "u-beto", "beto", "mesero")

	// Products: price, cost, track_stock, stock_qty.
	h.seedProduct(t, "p-cerveza", evID, "Cerveza", 8000, 3000, 1, 10)    // healthy stock
	h.seedProduct(t, "p-hambur", evID, "Hamburguesa", 15000, 9000, 1, 0) // tracked, out of stock
	h.seedProduct(t, "p-agua", evID, "Agua", 3000, 1000, 1, 5)           // tracked, low (==threshold)
	h.seedProduct(t, "p-serv", evID, "Servicio", 5000, 0, 0, 0)          // NOT tracked, qty 0 => not "agotado"
	h.seedProduct(t, "p-vino", evID, "Vino", 20000, 12000, 1, 6)         // tracked, just above low threshold

	// Attendees: registered=4; active = balance>0 OR made a non-voided payment.
	h.seedAttendee(t, "a-juan", evID, "Juan", 0)      // active via payment on O1
	h.seedAttendee(t, "a-maria", evID, "Maria", 5000) // active via balance>0, no consumption
	h.seedAttendee(t, "a-pedro", evID, "Pedro", 0)    // registered only
	h.seedAttendee(t, "a-zoe", evID, "Zoe", 0)        // registered only

	base := int64(1_700_000_000_000)

	// O1
	h.seedOrder(t, "o1", evID, "u-ana", "paid", 31000, 31000, 0, base, ms(base+10_000))
	h.seedItem(t, "i1a", "o1", "p-cerveza", "Cerveza", 2, 8000, 3000)
	h.seedItem(t, "i1b", "o1", "p-hambur", "Hamburguesa", 1, 15000, 9000)
	juan := "a-juan"
	h.seedPayment(t, "pay1", "o1", &juan, "balance", 31000, base)

	// O2
	h.seedOrder(t, "o2", evID, "u-ana", "paid", 17000, 0, 17000, base, ms(base+20_000))
	h.seedItem(t, "i2a", "o2", "p-cerveza", "Cerveza", 1, 8000, 3000)
	h.seedItem(t, "i2b", "o2", "p-agua", "Agua", 3, 3000, 1000)
	h.seedPayment(t, "pay2", "o2", nil, "cash", 17000, base)

	// O3
	h.seedOrder(t, "o3", evID, "u-ana", "paid", 10000, 0, 10000, base, ms(base+60_000))
	h.seedItem(t, "i3a", "o3", "p-serv", "Servicio", 2, 5000, 0)
	h.seedPayment(t, "pay3", "o3", nil, "cash", 10000, base)

	// O4
	h.seedOrder(t, "o4", evID, "u-beto", "paid", 8000, 0, 8000, base, ms(base+5_000))
	h.seedItem(t, "i4a", "o4", "p-cerveza", "Cerveza", 1, 8000, 3000)
	h.seedPayment(t, "pay4", "o4", nil, "cash", 8000, base)

	// O5
	h.seedOrder(t, "o5", evID, "u-beto", "paid", 8000, 0, 8000, base, ms(base+7_000))
	h.seedItem(t, "i5a", "o5", "p-cerveza", "Cerveza", 1, 8000, 3000)
	h.seedPayment(t, "pay5", "o5", nil, "cash", 8000, base)

	// O6 — VOIDED, deliberately huge so any leak is glaring.
	h.exec(t, `INSERT INTO orders(id,event_id,operator_id,request_id,source,status,subtotal,total,paid_from_balance,paid_cash,created_at,paid_at,voided_at)
	           VALUES('o6',?, 'u-ana','req-o6','operator','voided',165000,165000,0,165000,?,?,?)`,
		evID, base, base+1_000, base+2_000)
	h.seedItem(t, "i6a", "o6", "p-hambur", "Hamburguesa", 11, 15000, 9000)
	h.seedPayment(t, "pay6", "o6", nil, "cash", 165000, base)

	return expectations{
		revenue:            74000,
		cost:               27000,
		profit:             47000,
		orderCount:         5,
		margin:             47000.0 / 74000.0,
		overageCash:        43000, // cash: 17000+10000+8000+8000
		balanceFromSales:   31000,
		registered:         4,
		active:             2,
		pendingBalance:     5000,
		avgTicket:          74000.0 / 5.0, // dashboard avg_consumption (per order)
		avgPerClient:       74000.0 / 4.0, // closing avg_consumption (per registered client)
		avgSecondsPerOrder: (10 + 20 + 60 + 5 + 7) / 5.0,
		anaTotal:           58000,
		betoTotal:          16000,
		anaAvgSec:          (10 + 20 + 60) / 3.0,
		betoAvgSec:         (5 + 7) / 2.0,
		anaCount:           3,
		betoCount:          2,
		fastestUser:        "ana", // beto is faster (6s) but has only 2 orders (<3)
		fastestAvg:         30.0,
		fastestCount:       3,
	}
}

// ---------------------------------------------------------------------------
// Wire shapes (subset of the production JSON we assert against).
// ---------------------------------------------------------------------------

type fastestOp struct {
	Username   string  `json:"username"`
	AvgSeconds float64 `json:"avg_seconds"`
	OrderCount int64   `json:"order_count"`
}

type dashboardResp struct {
	TotalSold               int64      `json:"total_sold"`
	TotalCost               int64      `json:"total_cost"`
	TotalProfit             int64      `json:"total_profit"`
	ProfitMargin            float64    `json:"profit_margin"`
	OrderCount              int64      `json:"order_count"`
	AvgConsumption          float64    `json:"avg_consumption"`
	ActiveAttendees         int64      `json:"active_attendees"`
	OverageCash             int64      `json:"overage_cash"`
	BalanceFromSales        int64      `json:"balance_from_sales"`
	RegisteredAttendees     int64      `json:"registered_attendees"`
	AvgConsumptionPerClient float64    `json:"avg_consumption_per_client"`
	PendingBalance          int64      `json:"pending_balance"`
	AvgSecondsPerOrder      float64    `json:"avg_seconds_per_order"`
	FastestOperator         *fastestOp `json:"fastest_operator"`
	TopProducts             []struct {
		Name    string `json:"name"`
		QtySold int64  `json:"qty_sold"`
		Revenue int64  `json:"revenue"`
		Cost    int64  `json:"cost"`
		Profit  int64  `json:"profit"`
	} `json:"top_products"`
	SalesByOperator []struct {
		Username   string  `json:"username"`
		Total      int64   `json:"total"`
		Count      int64   `json:"count"`
		AvgSeconds float64 `json:"avg_seconds"`
	} `json:"sales_by_operator"`
	LowStock []struct {
		Name     string `json:"name"`
		StockQty int64  `json:"stock_qty"`
	} `json:"low_stock"`
	OutOfStock []struct {
		Name string `json:"name"`
	} `json:"out_of_stock"`
}

type closingResp struct {
	Totals struct {
		Revenue          int64   `json:"revenue"`
		Cost             int64   `json:"cost"`
		Profit           int64   `json:"profit"`
		Margin           float64 `json:"margin"`
		OrderCount       int64   `json:"order_count"`
		OverageCash      int64   `json:"overage_cash"`
		BalanceFromSales int64   `json:"balance_from_sales"`
	} `json:"totals"`
	Attendees struct {
		Registered     int64   `json:"registered"`
		Active         int64   `json:"active"`
		AvgConsumption float64 `json:"avg_consumption"`
		PendingBalance int64   `json:"pending_balance"`
	} `json:"attendees"`
	Timing struct {
		AvgSecondsPerOrder float64    `json:"avg_seconds_per_order"`
		FastestOperator    *fastestOp `json:"fastest_operator"`
	} `json:"timing"`
	Operators []struct {
		Username   string  `json:"username"`
		Total      int64   `json:"total"`
		Count      int64   `json:"count"`
		AvgSeconds float64 `json:"avg_seconds"`
	} `json:"operators"`
	TopProducts []struct {
		Name    string `json:"name"`
		QtySold int64  `json:"qty_sold"`
		Revenue int64  `json:"revenue"`
		Cost    int64  `json:"cost"`
		Profit  int64  `json:"profit"`
	} `json:"top_products"`
	Inventory struct {
		LowStock []struct {
			Name     string `json:"name"`
			StockQty int64  `json:"stock_qty"`
		} `json:"low_stock"`
		OutOfStock []struct {
			Name string `json:"name"`
		} `json:"out_of_stock"`
	} `json:"inventory"`
}

// ---------------------------------------------------------------------------
// assertion helpers
// ---------------------------------------------------------------------------

func eqI(t *testing.T, label string, got, want int64) {
	t.Helper()
	if got != want {
		t.Errorf("FAIL %-32s got=%d want=%d", label, got, want)
	}
}

func eqF(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("FAIL %-32s got=%.6f want=%.6f", label, got, want)
	}
}

// ---------------------------------------------------------------------------
// 1 + 2 + 3 + 6: Dashboard / closing JSON numbers.
// ---------------------------------------------------------------------------

func TestDashboardAndClosingNumbers(t *testing.T) {
	h := newHarness(t)
	exp := h.seedScenario(t)

	var dash dashboardResp
	h.getJSON(t, "/api/reports/dashboard?event_id="+evID, &dash)
	var cl closingResp
	h.getJSON(t, "/api/reports/closing?event_id="+evID, &cl)

	// --- Money totals (metric 1) ---
	eqI(t, "dashboard.revenue", dash.TotalSold, exp.revenue)
	eqI(t, "dashboard.cost", dash.TotalCost, exp.cost)
	eqI(t, "dashboard.profit", dash.TotalProfit, exp.profit)
	eqF(t, "dashboard.margin", dash.ProfitMargin, exp.margin)
	eqI(t, "dashboard.order_count", dash.OrderCount, exp.orderCount)
	eqI(t, "dashboard.overage_cash", dash.OverageCash, exp.overageCash)
	eqI(t, "dashboard.balance_from_sales", dash.BalanceFromSales, exp.balanceFromSales)

	eqI(t, "closing.revenue", cl.Totals.Revenue, exp.revenue)
	eqI(t, "closing.cost", cl.Totals.Cost, exp.cost)
	eqI(t, "closing.profit", cl.Totals.Profit, exp.profit)
	eqF(t, "closing.margin", cl.Totals.Margin, exp.margin)
	eqI(t, "closing.order_count", cl.Totals.OrderCount, exp.orderCount)
	eqI(t, "closing.overage_cash", cl.Totals.OverageCash, exp.overageCash)
	eqI(t, "closing.balance_from_sales", cl.Totals.BalanceFromSales, exp.balanceFromSales)

	// profit identity
	eqI(t, "identity profit=rev-cost", cl.Totals.Revenue-cl.Totals.Cost, cl.Totals.Profit)

	// --- Attendees: registered vs active distinction (metric 1) ---
	eqI(t, "closing.registered", cl.Attendees.Registered, exp.registered)
	eqI(t, "closing.active", cl.Attendees.Active, exp.active)
	eqI(t, "dashboard.registered", dash.RegisteredAttendees, exp.registered)
	eqI(t, "dashboard.active", dash.ActiveAttendees, exp.active)
	if cl.Attendees.Registered == cl.Attendees.Active {
		t.Errorf("FAIL registered/active not distinguished (both=%d)", cl.Attendees.Registered)
	}
	eqI(t, "closing.pending_balance", cl.Attendees.PendingBalance, exp.pendingBalance)
	eqI(t, "dashboard.pending_balance", dash.PendingBalance, exp.pendingBalance)

	// avg_consumption: dashboard = revenue/orders; closing = revenue/registered.
	eqF(t, "dashboard.avg_consumption(ticket)", dash.AvgConsumption, exp.avgTicket)
	eqF(t, "dashboard.avg_per_client", dash.AvgConsumptionPerClient, exp.avgPerClient)
	eqF(t, "closing.avg_consumption(per client)", cl.Attendees.AvgConsumption, exp.avgPerClient)

	// --- Timing (metric 2) ---
	eqF(t, "dashboard.avg_seconds_per_order", dash.AvgSecondsPerOrder, exp.avgSecondsPerOrder)
	eqF(t, "closing.avg_seconds_per_order", cl.Timing.AvgSecondsPerOrder, exp.avgSecondsPerOrder)
	if dash.FastestOperator == nil {
		t.Fatalf("FAIL dashboard.fastest_operator is nil; want %s", exp.fastestUser)
	}
	if dash.FastestOperator.Username != exp.fastestUser {
		t.Errorf("FAIL fastest_operator.username got=%s want=%s (>=3-order qualification: beto is faster at %.1fs but has only %d orders)",
			dash.FastestOperator.Username, exp.fastestUser, exp.betoAvgSec, exp.betoCount)
	}
	eqF(t, "fastest_operator.avg_seconds", dash.FastestOperator.AvgSeconds, exp.fastestAvg)
	eqI(t, "fastest_operator.order_count", dash.FastestOperator.OrderCount, exp.fastestCount)

	// --- Per-operator (metric 3): totals + avg_seconds; reconcile to revenue ---
	opTotal := map[string]int64{}
	opAvg := map[string]float64{}
	opCount := map[string]int64{}
	var sumOp int64
	for _, o := range cl.Operators {
		opTotal[o.Username] = o.Total
		opAvg[o.Username] = o.AvgSeconds
		opCount[o.Username] = o.Count
		sumOp += o.Total
	}
	eqI(t, "operator.ana.total", opTotal["ana"], exp.anaTotal)
	eqI(t, "operator.beto.total", opTotal["beto"], exp.betoTotal)
	eqI(t, "operator.ana.count", opCount["ana"], exp.anaCount)
	eqI(t, "operator.beto.count", opCount["beto"], exp.betoCount)
	eqF(t, "operator.ana.avg_seconds", opAvg["ana"], exp.anaAvgSec)
	eqF(t, "operator.beto.avg_seconds", opAvg["beto"], exp.betoAvgSec)
	eqI(t, "Σ sales_by_operator == revenue", sumOp, exp.revenue)

	// --- Top products (metric 3): ranking + per-row figures ---
	type pr struct {
		name                   string
		qty, rev, cost, profit int64
	}
	wantTop := []pr{
		{"Cerveza", 5, 40000, 15000, 25000},
		{"Agua", 3, 9000, 3000, 6000},
		{"Servicio", 2, 10000, 0, 10000},
		{"Hamburguesa", 1, 15000, 9000, 6000},
	}
	if len(cl.TopProducts) != len(wantTop) {
		t.Fatalf("FAIL top_products length got=%d want=%d (%+v)", len(cl.TopProducts), len(wantTop), cl.TopProducts)
	}
	var sumProdRev int64
	for i, w := range wantTop {
		g := cl.TopProducts[i]
		if g.Name != w.name {
			t.Errorf("FAIL top_products[%d].name (ranking) got=%s want=%s", i, g.Name, w.name)
		}
		eqI(t, "top["+w.name+"].qty", g.QtySold, w.qty)
		eqI(t, "top["+w.name+"].revenue", g.Revenue, w.rev)
		eqI(t, "top["+w.name+"].cost", g.Cost, w.cost)
		eqI(t, "top["+w.name+"].profit", g.Profit, w.profit)
		sumProdRev += g.Revenue
	}
	eqI(t, "Σ top_products.revenue == revenue", sumProdRev, exp.revenue)

	// --- Inventory: out_of_stock = exactly tracked stock==0; low_stock threshold ---
	if len(cl.Inventory.OutOfStock) != 1 || cl.Inventory.OutOfStock[0].Name != "Hamburguesa" {
		t.Errorf("FAIL out_of_stock got=%+v want exactly [Hamburguesa] (Servicio is qty0 but UNtracked => excluded)", cl.Inventory.OutOfStock)
	}
	// low_stock: tracked and stock<=5 => Hamburguesa(0), Agua(5); Vino(6) excluded.
	if len(cl.Inventory.LowStock) != 2 {
		t.Fatalf("FAIL low_stock length got=%d want=2 (%+v)", len(cl.Inventory.LowStock), cl.Inventory.LowStock)
	}
	eqI(t, "low_stock[0]=Hamburguesa(0)", cl.Inventory.LowStock[0].StockQty, 0)
	if cl.Inventory.LowStock[0].Name != "Hamburguesa" {
		t.Errorf("FAIL low_stock[0].name got=%s want=Hamburguesa", cl.Inventory.LowStock[0].Name)
	}
	eqI(t, "low_stock[1]=Agua(5 threshold)", cl.Inventory.LowStock[1].StockQty, 5)
	if cl.Inventory.LowStock[1].Name != "Agua" {
		t.Errorf("FAIL low_stock[1].name got=%s want=Agua", cl.Inventory.LowStock[1].Name)
	}
	for _, it := range cl.Inventory.LowStock {
		if it.Name == "Vino" {
			t.Errorf("FAIL Vino(stock=6) must NOT be low_stock (threshold is <=5)")
		}
	}

	// --- Conservation (metric 6) ---
	eqI(t, "overage_cash + balance == revenue", cl.Totals.OverageCash+cl.Totals.BalanceFromSales, exp.revenue)
	h.assertConservationSQL(t)
}

// assertConservationSQL checks invariants directly in the DB:
//   - Σ payments (non-voided orders) == Σ order totals (non-voided)
//   - per order: paid_from_balance + paid_cash == total
//   - per order: Σ payments == total
func (h *harness) assertConservationSQL(t *testing.T) {
	t.Helper()
	var sumTotals, sumPays int64
	if err := h.d.Reader.QueryRow(
		`SELECT COALESCE(SUM(total),0) FROM orders WHERE event_id=? AND status<>'voided'`, evID).
		Scan(&sumTotals); err != nil {
		t.Fatalf("sum totals: %v", err)
	}
	if err := h.d.Reader.QueryRow(`
		SELECT COALESCE(SUM(p.amount),0) FROM payments p
		JOIN orders o ON p.order_id=o.id
		WHERE o.event_id=? AND o.status<>'voided'`, evID).Scan(&sumPays); err != nil {
		t.Fatalf("sum pays: %v", err)
	}
	eqI(t, "conservation Σpayments==Σtotals", sumPays, sumTotals)

	rows, err := h.d.Reader.Query(`
		SELECT o.id, o.total, o.paid_from_balance, o.paid_cash,
		       (SELECT COALESCE(SUM(amount),0) FROM payments WHERE order_id=o.id)
		FROM orders o WHERE o.event_id=? AND o.status<>'voided'`, evID)
	if err != nil {
		t.Fatalf("per-order query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var total, pfb, pcash, pays int64
		if err := rows.Scan(&id, &total, &pfb, &pcash, &pays); err != nil {
			t.Fatalf("scan: %v", err)
		}
		eqI(t, "order["+id+"] bal+cash==total", pfb+pcash, total)
		eqI(t, "order["+id+"] Σpayments==total", pays, total)
	}
}

// ---------------------------------------------------------------------------
// 5: Exports (closing.csv + closing.xlsx) must carry the SAME numbers as JSON.
// ---------------------------------------------------------------------------

func TestExportsMatchAPI(t *testing.T) {
	h := newHarness(t)
	exp := h.seedScenario(t)

	var cl closingResp
	h.getJSON(t, "/api/reports/closing?event_id="+evID, &cl)

	// ---- CSV ----
	raw := h.getRaw(t, "/api/reports/closing.csv?event_id="+evID)
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) // strip UTF-8 BOM
	cr := csv.NewReader(bytes.NewReader(raw))
	cr.FieldsPerRecord = -1
	records, err := cr.ReadAll()
	if err != nil {
		t.Fatalf("parse closing.csv: %v", err)
	}
	csvVal := map[string]string{}   // first cell -> second cell (KPI rows)
	csvRow := map[string][]string{} // first cell -> whole row
	for _, rec := range records {
		if len(rec) >= 1 {
			csvRow[rec[0]] = rec
		}
		if len(rec) >= 2 {
			csvVal[rec[0]] = rec[1]
		}
	}
	csvEq := func(label, key, want string) {
		if got := csvVal[key]; got != want {
			t.Errorf("FAIL csv %-28s got=%q want=%q", label, got, want)
		}
	}
	csvEq("revenue", "Ingresos (COP)", "74000")
	csvEq("cost", "Costo (COP)", "27000")
	csvEq("profit", "Ganancia (COP)", "47000")
	csvEq("margin", "Margen", "0.6351")
	csvEq("order_count", "Pedidos", "5")
	csvEq("overage_cash", "Excedente en efectivo (COP)", "43000")
	csvEq("balance", "Saldo usado en ventas (COP)", "31000")
	csvEq("registered", "Clientes registrados", "4")
	csvEq("active", "Clientes activos", "2")
	csvEq("avg_per_client", "Consumo promedio por cliente (COP)", "18500.00")
	csvEq("pending", "Dinero pendiente (COP)", "5000")
	csvEq("avg_seconds", "Tiempo promedio por pedido (s)", "20.4")

	// CSV must agree with the JSON it was built from.
	if csvVal["Ingresos (COP)"] != itoa(cl.Totals.Revenue) {
		t.Errorf("FAIL csv revenue %q != json %d", csvVal["Ingresos (COP)"], cl.Totals.Revenue)
	}

	// fastest operator row: ana, 30.0 s, 3 pedidos
	if r := csvRow["Vendedor más rápido"]; len(r) < 4 || r[1] != "ana" || r[2] != "30.0 s" || r[3] != "3 pedidos" {
		t.Errorf("FAIL csv fastest row got=%v want=[_, ana, 30.0 s, 3 pedidos]", r)
	}
	// per-vendor + top-product rows
	if r := csvRow["ana"]; len(r) < 4 || r[1] != "58000" || r[2] != "3" || r[3] != "30.0" {
		t.Errorf("FAIL csv ana row got=%v want=[ana 58000 3 30.0]", r)
	}
	if r := csvRow["beto"]; len(r) < 4 || r[1] != "16000" || r[2] != "2" || r[3] != "6.0" {
		t.Errorf("FAIL csv beto row got=%v want=[beto 16000 2 6.0]", r)
	}
	if r := csvRow["Cerveza"]; len(r) < 5 || r[1] != "5" || r[2] != "40000" || r[3] != "15000" || r[4] != "25000" {
		t.Errorf("FAIL csv Cerveza row got=%v want=[Cerveza 5 40000 15000 25000]", r)
	}

	// ---- XLSX ----
	xraw := h.getRaw(t, "/api/reports/closing.xlsx?event_id="+evID)
	f, err := excelize.OpenReader(bytes.NewReader(xraw))
	if err != nil {
		t.Fatalf("open closing.xlsx: %v", err)
	}
	defer f.Close()

	// Read RAW cell values so the COP currency number-format ("$#,##0") does not
	// rewrite e.g. 74000 -> "$74,000"; we want the underlying integer.
	rawCell := func(sheet, ref string) string {
		v, _ := f.GetCellValue(sheet, ref, excelize.Options{RawCellValue: true})
		return v
	}
	cellB := func(row int) string { return rawCell("Resumen", cellRef(2, row)) }

	// Layout from writeResumenSheet (closing_xlsx.go): financial KPIs rows 5-11;
	// an extra blank precedes each section, so Clientes starts at row 13 (header)
	// with values 14-17, and Tiempos at row 19 (header) with values 20-23.
	xEq := func(label string, row int, want string) {
		if got := cellB(row); got != want {
			t.Errorf("FAIL xlsx Resumen!B%d %-22s got=%q want=%q", row, label, got, want)
		}
	}
	xEq("Ingresos", 5, "74000")
	xEq("Costo", 6, "27000")
	xEq("Ganancia", 7, "47000")
	xEq("Margen", 8, "63.5%")
	xEq("Pedidos", 9, "5")
	xEq("Excedente", 10, "43000")
	xEq("Saldo ventas", 11, "31000")
	xEq("Clientes reg", 14, "4")
	xEq("Clientes activos", 15, "2")
	xEq("Consumo/cliente", 16, "18500") // int64(18500.0+0.5)
	xEq("Dinero pendiente", 17, "5000")
	xEq("Tiempo prom", 20, "20.4 s")
	xEq("Vendedor rápido", 21, "ana")
	xEq("· tiempo prom", 22, "30.0 s")
	xEq("· pedidos", 23, "3")

	// xlsx integer money must exactly equal JSON (no float drift).
	if cellB(5) != itoa(cl.Totals.Revenue) {
		t.Errorf("FAIL xlsx revenue %q != json %d", cellB(5), cl.Totals.Revenue)
	}

	// Productos sheet: header is row 4, data starts row 5 (titleRows->4, writeHeader->5).
	prodName := rawCell("Productos", "A5")
	prodQty := rawCell("Productos", "B5")
	prodRev := rawCell("Productos", "C5")
	if prodName != "Cerveza" || prodQty != "5" || prodRev != "40000" {
		t.Errorf("FAIL xlsx Productos row1 got=[%s %s %s] want=[Cerveza 5 40000]", prodName, prodQty, prodRev)
	}

	// Por vendedor sheet: row 5 first operator (ranked by total) = ana 58000 3.
	vName := rawCell("Por vendedor", "A5")
	vTotal := rawCell("Por vendedor", "B5")
	vCount := rawCell("Por vendedor", "C5")
	if vName != "ana" || vTotal != "58000" || vCount != "3" {
		t.Errorf("FAIL xlsx Por vendedor row1 got=[%s %s %s] want=[ana 58000 3]", vName, vTotal, vCount)
	}

	_ = exp
}

func itoa(v int64) string {
	return excelizeItoa(v)
}

// cellRef wraps excelize coordinate conversion for tests.
func cellRef(col, row int) string {
	c, _ := excelize.CoordinatesToCellName(col, row)
	return c
}

func excelizeItoa(v int64) string {
	// small helper to avoid importing strconv twice; reuse FormatInt semantics.
	neg := v < 0
	if neg {
		v = -v
	}
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ---------------------------------------------------------------------------
// 4: Tips (propinas) distribution — top-3 meseros + aseo get +5%, rest split
// equally, total distributed EXACTLY equals the pool (no rounding leak/gain).
// ---------------------------------------------------------------------------

type tipShare struct {
	Name  string `json:"name"`
	Role  string `json:"role"`
	Sales int64  `json:"sales"`
	Top3  bool   `json:"top3"`
	Bonus bool   `json:"bonus"`
	Share int64  `json:"share"`
}

type tipsResp struct {
	Pool        int64      `json:"pool"`
	BonusPct    float64    `json:"bonus_pct"`
	Meseros     []tipShare `json:"meseros"`
	Aseo        []tipShare `json:"aseo"`
	Distributed int64      `json:"distributed"`
}

func (h *harness) seedTipsScenario(t *testing.T) {
	t.Helper()
	h.seedEvent(t, evID, "Fiesta", "active")
	// 5 meseros with distinct sales so ranking is deterministic.
	type m struct {
		id, name string
		sales    int64
	}
	meseros := []m{
		{"m1", "m1", 100000},
		{"m2", "m2", 90000},
		{"m3", "m3", 80000},
		{"m4", "m4", 70000},
		{"m5", "m5", 60000},
	}
	base := int64(1_700_000_000_000)
	for i, mm := range meseros {
		h.seedUser(t, "u-"+mm.id, mm.name, "mesero")
		// one paid order carrying the operator's total sales
		oid := "to" + mm.name
		h.seedOrder(t, oid, evID, "u-"+mm.id, "paid", mm.sales, 0, mm.sales, base, ms(base+1000))
		_ = i
	}
	// Aseo staff via app_config JSON array; both always receive the bonus.
	h.exec(t, `INSERT INTO app_config(key,value,updated_at,updated_by) VALUES('aseo_staff',?,?, 'test')`,
		`["Aseo1","Aseo2"]`, core.NowMillis())
}

func TestTipsDistribution(t *testing.T) {
	h := newHarness(t)
	h.seedTipsScenario(t)

	// --- Structural checks on a representative pool ---
	var tp tipsResp
	h.getJSON(t, "/api/reports/tips?event_id="+evID+"&pool=100000", &tp)

	eqF(t, "tips.bonus_pct", tp.BonusPct, 0.05)
	eqI(t, "tips.pool", tp.Pool, 100000)
	if len(tp.Meseros) != 5 {
		t.Fatalf("FAIL meseros len got=%d want=5", len(tp.Meseros))
	}
	if len(tp.Aseo) != 2 {
		t.Fatalf("FAIL aseo len got=%d want=2", len(tp.Aseo))
	}

	// Ranking by sales desc: m1..m5.
	for i, want := range []string{"m1", "m2", "m3", "m4", "m5"} {
		if tp.Meseros[i].Name != want {
			t.Errorf("FAIL meseros ranking[%d] got=%s want=%s", i, tp.Meseros[i].Name, want)
		}
	}
	// Top-3 flagged; rest not. Top-3 + aseo carry the bonus.
	for i, m := range tp.Meseros {
		wantTop := i < 3
		if m.Top3 != wantTop {
			t.Errorf("FAIL meseros[%d=%s].top3 got=%v want=%v", i, m.Name, m.Top3, wantTop)
		}
		if m.Bonus != wantTop {
			t.Errorf("FAIL meseros[%d=%s].bonus got=%v want=%v", i, m.Name, m.Bonus, wantTop)
		}
	}
	for _, a := range tp.Aseo {
		if !a.Bonus {
			t.Errorf("FAIL aseo[%s].bonus got=false want=true", a.Name)
		}
	}

	// Distributed equals pool EXACTLY (no leak/gain).
	eqI(t, "tips.distributed==pool", tp.Distributed, tp.Pool)
	var sum int64
	for _, m := range tp.Meseros {
		sum += m.Share
	}
	for _, a := range tp.Aseo {
		sum += a.Share
	}
	eqI(t, "Σ shares == pool", sum, tp.Pool)

	// Bonus recipients must each get >= every non-bonus recipient (5% more),
	// and within a band of at most 1 COP of each other (largest-remainder).
	var minBonus, maxBonus int64 = math.MaxInt64, 0
	var maxNonBonus int64 = 0
	var nonBonusShares []int64
	for _, s := range append(append([]tipShare{}, tp.Meseros...), tp.Aseo...) {
		if s.Bonus {
			if s.Share < minBonus {
				minBonus = s.Share
			}
			if s.Share > maxBonus {
				maxBonus = s.Share
			}
		} else {
			if s.Share > maxNonBonus {
				maxNonBonus = s.Share
			}
			nonBonusShares = append(nonBonusShares, s.Share)
		}
	}
	if minBonus < maxNonBonus {
		t.Errorf("FAIL a bonus share (%d) < a non-bonus share (%d): bonus not honored", minBonus, maxNonBonus)
	}
	if maxBonus-minBonus > 1 {
		t.Errorf("FAIL bonus shares spread >1 COP (min=%d max=%d)", minBonus, maxBonus)
	}
	// "rest split equally": non-bonus shares differ by at most 1 (largest remainder).
	for _, s := range nonBonusShares {
		if d := s - nonBonusShares[0]; d > 1 || d < -1 {
			t.Errorf("FAIL non-bonus shares not ~equal: %v", nonBonusShares)
			break
		}
	}

	// --- Edge pools: distributed must always equal pool exactly (rounding) ---
	for _, pool := range []int64{0, 1, 2, 3, 7, 13, 101, 99999, 1_000_000, 1_234_567} {
		var e tipsResp
		h.getJSON(t, "/api/reports/tips?event_id="+evID+"&pool="+excelizeItoa(pool), &e)
		var s int64
		for _, m := range e.Meseros {
			s += m.Share
		}
		for _, a := range e.Aseo {
			s += a.Share
		}
		eqI(t, "edge pool="+excelizeItoa(pool)+" distributed", e.Distributed, pool)
		eqI(t, "edge pool="+excelizeItoa(pool)+" Σshares", s, pool)
	}
}

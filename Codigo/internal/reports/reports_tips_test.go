// Package reports – tests for profit-metrics and tips-distribution features.
// This file must not redeclare any name exported in reports_test.go or
// reports_extra_test.go (openTestDB, newReportsMod, parseCSVData,
// seedEventAndUser, insertOrder, seedFullExportData).
package reports

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"eventpos/internal/app"
	"eventpos/internal/config"
	"eventpos/internal/core"
	"eventpos/internal/db"
	"eventpos/internal/server"
)

// ---------------------------------------------------------------------------
// Test-local helpers
// ---------------------------------------------------------------------------

// newTipsMod creates a Module with both App and Config wired. Config is
// required by the tips handler which reads aseo_staff from app_config.
func newTipsMod(t *testing.T, d *db.DB) *Module {
	t.Helper()
	return New(server.Deps{
		App:    &app.App{DB: d},
		Config: config.New(d),
	})
}

// setAseoStaff upserts the aseo_staff JSON-array value in app_config.
func setAseoStaff(t *testing.T, d *db.DB, names []string) {
	t.Helper()
	raw, err := json.Marshal(names)
	if err != nil {
		t.Fatalf("setAseoStaff marshal: %v", err)
	}
	if _, err := d.Writer.ExecContext(context.Background(),
		`INSERT INTO app_config(key, value, updated_at, updated_by) VALUES (?,?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value,
		                                updated_at=excluded.updated_at,
		                                updated_by=excluded.updated_by`,
		aseoStaffKey, string(raw), core.NowMillis(), "test"); err != nil {
		t.Fatalf("setAseoStaff exec: %v", err)
	}
}

// insertMesero inserts an active user with role='mesero'.
func insertMesero(t *testing.T, d *db.DB, userID, username string) {
	t.Helper()
	if _, err := d.Writer.ExecContext(context.Background(),
		`INSERT INTO users(id, username, role, pin_hash, active, created_at)
		 VALUES (?,?,?,?,?,?)`,
		userID, username, "mesero", "argon2id$s$k", 1, core.NowMillis()); err != nil {
		t.Fatalf("insertMesero %s: %v", username, err)
	}
}

// insertItemWithCost inserts an order_item row with an explicit unit_cost
// snapshot (the column added for COGS tracking).
func insertItemWithCost(t *testing.T, d *db.DB,
	itemID, orderID, productID, name string,
	qty, unitPrice, unitCost, lineTotal int64,
) {
	t.Helper()
	if _, err := d.Writer.ExecContext(context.Background(),
		`INSERT INTO order_items(id, order_id, product_id, name, qty, unit_price, unit_cost, line_total)
		 VALUES (?,?,?,?,?,?,?,?)`,
		itemID, orderID, productID, name, qty, unitPrice, unitCost, lineTotal); err != nil {
		t.Fatalf("insertItemWithCost %s: %v", itemID, err)
	}
}

// insertEventOnly inserts a bare event row without creating an operator user.
func insertEventOnly(t *testing.T, d *db.DB, eventID, status string) {
	t.Helper()
	if _, err := d.Writer.ExecContext(context.Background(),
		`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
		 VALUES (?,?,?,?,?,?)`,
		eventID, "Event "+eventID, status, 50000, "cascade", core.NowMillis()); err != nil {
		t.Fatalf("insertEventOnly %s: %v", eventID, err)
	}
}

// callTips fires m.tips directly (bypassing auth middleware) with rawURL,
// asserts HTTP 200, and returns the decoded TipsResponse.
func callTips(t *testing.T, m *Module, rawURL string) TipsResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, rawURL, nil)
	rr := httptest.NewRecorder()
	m.tips(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("tips %s: got HTTP %d, body: %s", rawURL, rr.Code, rr.Body.String())
	}
	var resp TipsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode tips response: %v", err)
	}
	return resp
}

// tipsSum returns the sum of all TipShare.Share values across meseros and aseo.
func tipsSum(resp TipsResponse) int64 {
	var s int64
	for _, ts := range resp.Meseros {
		s += ts.Share
	}
	for _, ts := range resp.Aseo {
		s += ts.Share
	}
	return s
}

// ---------------------------------------------------------------------------
// Test 1: Profit math
// ---------------------------------------------------------------------------

// TestProfitMath verifies that queryDashboard computes total_cost, total_profit,
// profit_margin, and per-product cost/profit correctly from order_items.unit_cost
// snapshots, and that a voided order is excluded from all cost/profit figures.
func TestProfitMath(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	now := core.NowMillis()

	const (
		eventID = "evt-prf-001"
		userID  = "usr-prf-001"
		prod1ID = "prd-prf-001" // Burger: price=10000, cost=4000
		prod2ID = "prd-prf-002" // Beer:   price=5000,  cost=2000
		ord1ID  = "ord-prf-001" // paid:   2×Burger + 1×Beer = 25000
		ord2ID  = "ord-prf-002" // paid:   1×Beer = 5000
		ord3ID  = "ord-prf-003" // voided: 1×Burger = 10000  [must be excluded]
	)

	// seedEventAndUser is from reports_extra_test.go; creates caja operator.
	seedEventAndUser(t, d, eventID, userID, "active")

	// Products with the cost column populated (additive migration column).
	for _, p := range []struct {
		id, name    string
		price, cost int64
	}{
		{prod1ID, "Burger", 10000, 4000},
		{prod2ID, "Beer", 5000, 2000},
	} {
		if _, err := d.Writer.ExecContext(ctx,
			`INSERT INTO products(id, event_id, name, price, cost, track_stock, stock_qty, created_at)
			 VALUES (?,?,?,?,?,?,?,?)`,
			p.id, eventID, p.name, p.price, p.cost, 0, 0, now); err != nil {
			t.Fatalf("insert product %s: %v", p.name, err)
		}
	}

	// insertOrder is from reports_extra_test.go.
	insertOrder(t, d, eventID, userID, ord1ID, "paid", 25000, now)
	insertOrder(t, d, eventID, userID, ord2ID, "paid", 5000, now)
	insertOrder(t, d, eventID, userID, ord3ID, "voided", 10000, now)

	// Order items with unit_cost snapshots.
	// ord1: 2×Burger (unit_cost=4000 each, line=20000) + 1×Beer (unit_cost=2000, line=5000)
	//       → item COGS = 2×4000 + 1×2000 = 10000
	insertItemWithCost(t, d, "item-prf-1a", ord1ID, prod1ID, "Burger", 2, 10000, 4000, 20000)
	insertItemWithCost(t, d, "item-prf-1b", ord1ID, prod2ID, "Beer", 1, 5000, 2000, 5000)
	// ord2: 1×Beer (unit_cost=2000, line=5000)  → item COGS = 2000
	insertItemWithCost(t, d, "item-prf-2a", ord2ID, prod2ID, "Beer", 1, 5000, 2000, 5000)
	// ord3 (voided): 1×Burger — MUST NOT contribute to cost/profit.
	insertItemWithCost(t, d, "item-prf-3a", ord3ID, prod1ID, "Burger", 1, 10000, 4000, 10000)

	dash, err := queryDashboard(ctx, d.Reader, eventID)
	if err != nil {
		t.Fatalf("queryDashboard: %v", err)
	}

	// Revenue = 25000 + 5000 = 30000  (voided 10000 excluded).
	if want := int64(30000); dash.TotalSold != want {
		t.Errorf("total_sold: got %d, want %d", dash.TotalSold, want)
	}

	// COGS = (2×4000 + 1×2000) + (1×2000) = 12000  (voided 4000 excluded).
	if want := int64(12000); dash.TotalCost != want {
		t.Errorf("total_cost: got %d, want %d", dash.TotalCost, want)
	}

	// Profit = 30000 − 12000 = 18000.
	if want := int64(18000); dash.TotalProfit != want {
		t.Errorf("total_profit: got %d, want %d", dash.TotalProfit, want)
	}

	// Margin = 18000/30000 = 0.6.
	if want := 0.6; math.Abs(dash.ProfitMargin-want) > 1e-9 {
		t.Errorf("profit_margin: got %.12f, want %.12f", dash.ProfitMargin, want)
	}

	// Per-product assertions — keyed by product ID so order in the slice doesn't matter.
	if n := len(dash.TopProducts); n != 2 {
		t.Fatalf("top_products len: got %d, want 2", n)
	}
	byProd := make(map[string]TopProduct, 2)
	for _, p := range dash.TopProducts {
		byProd[p.ProductID] = p
	}

	// Burger: qty=2 (only from non-voided ord1), revenue=20000, cost=8000, profit=12000.
	burger, ok := byProd[prod1ID]
	if !ok {
		t.Fatal("Burger not found in top_products")
	}
	if burger.QtySold != 2 {
		t.Errorf("Burger qty_sold: got %d, want 2", burger.QtySold)
	}
	if burger.Revenue != 20000 {
		t.Errorf("Burger revenue: got %d, want 20000", burger.Revenue)
	}
	if burger.Cost != 8000 {
		t.Errorf("Burger cost: got %d, want 8000", burger.Cost)
	}
	if burger.Profit != 12000 {
		t.Errorf("Burger profit: got %d, want 12000", burger.Profit)
	}

	// Beer: qty=2 (1 from ord1 + 1 from ord2), revenue=10000, cost=4000, profit=6000.
	beer, ok := byProd[prod2ID]
	if !ok {
		t.Fatal("Beer not found in top_products")
	}
	if beer.QtySold != 2 {
		t.Errorf("Beer qty_sold: got %d, want 2", beer.QtySold)
	}
	if beer.Revenue != 10000 {
		t.Errorf("Beer revenue: got %d, want 10000", beer.Revenue)
	}
	if beer.Cost != 4000 {
		t.Errorf("Beer cost: got %d, want 4000", beer.Cost)
	}
	if beer.Profit != 6000 {
		t.Errorf("Beer profit: got %d, want 6000", beer.Profit)
	}
}

// ---------------------------------------------------------------------------
// Test 2: Tips happy path
// ---------------------------------------------------------------------------

// TestTipsHappyPath seeds 4 meseros with distinct non-voided sales and 2 aseo
// staff, then verifies:
//   - The top-3 meseros and both aseo staff receive bonus=true.
//   - The 4th mesero receives bonus=false.
//   - The sum of all shares equals the pool exactly.
//   - Every bonus share is >= the non-bonus share.
func TestTipsHappyPath(t *testing.T) {
	d := openTestDB(t)
	now := core.NowMillis()

	const (
		eventID = "evt-tips-hp"
		pool    = int64(100000)
	)

	insertEventOnly(t, d, eventID, "active")

	// 4 meseros with clearly-descending, distinct sales so that
	// ORDER BY sales DESC, u.username produces a deterministic ranking.
	type msSpec struct {
		id, name string
		sales    int64
	}
	meseroSpecs := []msSpec{
		{"usr-hp-a", "alice", 100000},
		{"usr-hp-b", "bob", 80000},
		{"usr-hp-c", "carlos", 60000},
		{"usr-hp-d", "diego", 40000},
	}
	for i, s := range meseroSpecs {
		insertMesero(t, d, s.id, s.name)
		insertOrder(t, d, eventID, s.id,
			fmt.Sprintf("ord-hp-%d", i), "paid", s.sales, now)
	}

	// 2 aseo staff from config — they always get the bonus.
	setAseoStaff(t, d, []string{"Limpio", "Aseado"})

	m := newTipsMod(t, d)
	resp := callTips(t, m, fmt.Sprintf("/api/reports/tips?pool=%d&event_id=%s", pool, eventID))

	// Distributed == pool exactly.
	if resp.Distributed != pool {
		t.Errorf("distributed: got %d, want %d", resp.Distributed, pool)
	}
	if got := tipsSum(resp); got != pool {
		t.Errorf("sum of shares: got %d, want %d", got, pool)
	}

	// 4 meseros expected.
	if n := len(resp.Meseros); n != 4 {
		t.Fatalf("meseros len: got %d, want 4", n)
	}

	// The query orders by sales DESC then username ASC, so:
	// alice=[0] bob=[1] carlos=[2] diego=[3].
	wantMeseros := []struct {
		name  string
		bonus bool
		top3  bool
	}{
		{"alice", true, true},
		{"bob", true, true},
		{"carlos", true, true},
		{"diego", false, false},
	}
	for i, want := range wantMeseros {
		ms := resp.Meseros[i]
		if ms.Name != want.name {
			t.Errorf("meseros[%d].name: got %q, want %q", i, ms.Name, want.name)
		}
		if ms.Bonus != want.bonus {
			t.Errorf("meseros[%d](%s).bonus: got %v, want %v", i, ms.Name, ms.Bonus, want.bonus)
		}
		if ms.Top3 != want.top3 {
			t.Errorf("meseros[%d](%s).top3: got %v, want %v", i, ms.Name, ms.Top3, want.top3)
		}
	}

	// 2 aseo staff, all bonus, correct role.
	if n := len(resp.Aseo); n != 2 {
		t.Fatalf("aseo len: got %d, want 2", n)
	}
	for _, a := range resp.Aseo {
		if !a.Bonus {
			t.Errorf("aseo %q: bonus should be true", a.Name)
		}
		if a.Role != "aseo" {
			t.Errorf("aseo %q: role=%q, want 'aseo'", a.Name, a.Role)
		}
	}

	// Every bonus recipient must receive >= the non-bonus (diego's) share.
	// With pool=100000, 5 bonus + 1 non-bonus, sumW=6.25:
	// bonus = 100000*1.05/6.25 = 16800, non-bonus = 100000*1.0/6.25 = 16000.
	bonusShare := resp.Meseros[0].Share // alice
	nonBonus := resp.Meseros[3].Share   // diego
	if bonusShare < nonBonus {
		t.Errorf("bonus share (%d) < non-bonus share (%d)", bonusShare, nonBonus)
	}
}

// ---------------------------------------------------------------------------
// Test 3: Exact-sum invariant across awkward pool sizes
// ---------------------------------------------------------------------------

// TestTipsExactSumInvariant exercises the largest-remainder allocator with pool
// sizes that force non-trivial fractional remainders. The sum of distributed
// shares must always equal the pool and no individual share may exceed it.
func TestTipsExactSumInvariant(t *testing.T) {
	d := openTestDB(t)
	now := core.NowMillis()

	const eventID = "evt-tips-inv"
	insertEventOnly(t, d, eventID, "active")

	// 4 meseros (top-3 get bonus weight 1.05, 4th gets 1.0).
	// sumW = 3×1.05 + 1×1.0 = 4.15, which is irrational relative to most pool
	// values and forces the largest-remainder path non-trivially.
	for i, spec := range []struct {
		id, name string
		sales    int64
	}{
		{"usr-inv-a", "ana", 90000},
		{"usr-inv-b", "ben", 70000},
		{"usr-inv-c", "celi", 50000},
		{"usr-inv-d", "dani", 30000},
	} {
		insertMesero(t, d, spec.id, spec.name)
		insertOrder(t, d, eventID, spec.id,
			fmt.Sprintf("ord-inv-%d", i), "paid", spec.sales, now)
	}

	m := newTipsMod(t, d)

	for _, pool := range []int64{100000, 99999, 7, 1, 0} {
		pool := pool
		t.Run(fmt.Sprintf("pool_%d", pool), func(t *testing.T) {
			url := fmt.Sprintf("/api/reports/tips?pool=%d&event_id=%s", pool, eventID)
			resp := callTips(t, m, url)

			got := tipsSum(resp)
			if got != pool {
				t.Errorf("sum of shares=%d, want %d", got, pool)
			}
			if resp.Distributed != pool {
				t.Errorf("distributed=%d, want %d", resp.Distributed, pool)
			}
			// Sanity: no single share may exceed the pool.
			all := append(resp.Meseros, resp.Aseo...) //nolint:gocritic
			for _, ts := range all {
				if ts.Share > pool {
					t.Errorf("share for %q is %d > pool %d", ts.Name, ts.Share, pool)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test 4: No recipients
// ---------------------------------------------------------------------------

// TestTipsNoRecipients verifies that when there are no active meseros and no
// aseo_staff config the response is HTTP 200 with distributed=0 and empty lists.
func TestTipsNoRecipients(t *testing.T) {
	d := openTestDB(t)
	const eventID = "evt-tips-none"
	insertEventOnly(t, d, eventID, "active")
	// No mesero users and no aseo_staff config.

	m := newTipsMod(t, d)
	resp := callTips(t, m, "/api/reports/tips?pool=100000&event_id="+eventID)

	if len(resp.Meseros) != 0 {
		t.Errorf("meseros: got %d, want 0", len(resp.Meseros))
	}
	if len(resp.Aseo) != 0 {
		t.Errorf("aseo: got %d, want 0", len(resp.Aseo))
	}
	if resp.Distributed != 0 {
		t.Errorf("distributed: got %d, want 0", resp.Distributed)
	}
	if tipsSum(resp) != 0 {
		t.Errorf("sum of shares: got %d, want 0", tipsSum(resp))
	}
}

// ---------------------------------------------------------------------------
// Test 5: Aseo-only and meseros-only edge cases
// ---------------------------------------------------------------------------

// TestTipsAseoOnly verifies that when there are no mesero users and only aseo
// staff are present the distribution sums to pool and all aseo receive bonus.
func TestTipsAseoOnly(t *testing.T) {
	d := openTestDB(t)
	const (
		eventID = "evt-tips-aseo"
		pool    = int64(90000)
	)
	insertEventOnly(t, d, eventID, "active")
	setAseoStaff(t, d, []string{"Barredor", "Trapero"})
	// No mesero users.

	m := newTipsMod(t, d)
	resp := callTips(t, m, fmt.Sprintf("/api/reports/tips?pool=%d&event_id=%s", pool, eventID))

	if len(resp.Meseros) != 0 {
		t.Errorf("meseros: got %d, want 0", len(resp.Meseros))
	}
	if n := len(resp.Aseo); n != 2 {
		t.Fatalf("aseo len: got %d, want 2", n)
	}
	for _, a := range resp.Aseo {
		if !a.Bonus {
			t.Errorf("aseo %q: bonus should be true", a.Name)
		}
	}
	if got := tipsSum(resp); got != pool {
		t.Errorf("sum of shares: got %d, want %d", got, pool)
	}
	if resp.Distributed != pool {
		t.Errorf("distributed: got %d, want %d", resp.Distributed, pool)
	}
}

// TestTipsMeserosOnly verifies that when there is no aseo_staff config
// (only mesero users) the distribution sums to pool correctly and all meseros
// in a sub-3 group are correctly flagged top3.
func TestTipsMeserosOnly(t *testing.T) {
	d := openTestDB(t)
	now := core.NowMillis()
	const (
		eventID = "evt-tips-mes"
		pool    = int64(80000)
	)
	insertEventOnly(t, d, eventID, "active")
	// No aseo_staff config inserted.

	// 2 meseros — both end up in top-3 (i < 3) so both get bonus.
	for i, spec := range []struct {
		id, name string
		sales    int64
	}{
		{"usr-mo-a", "fernanda", 50000},
		{"usr-mo-b", "gabriel", 30000},
	} {
		insertMesero(t, d, spec.id, spec.name)
		insertOrder(t, d, eventID, spec.id,
			fmt.Sprintf("ord-mo-%d", i), "paid", spec.sales, now)
	}

	m := newTipsMod(t, d)
	resp := callTips(t, m, fmt.Sprintf("/api/reports/tips?pool=%d&event_id=%s", pool, eventID))

	if len(resp.Aseo) != 0 {
		t.Errorf("aseo: got %d, want 0", len(resp.Aseo))
	}
	if n := len(resp.Meseros); n != 2 {
		t.Fatalf("meseros len: got %d, want 2", n)
	}
	// Both are top-3 (only 2 meseros, both indices are < 3).
	for _, ms := range resp.Meseros {
		if !ms.Bonus {
			t.Errorf("mesero %q: bonus should be true (all are top-3 in a 2-person group)", ms.Name)
		}
		if !ms.Top3 {
			t.Errorf("mesero %q: top3 should be true", ms.Name)
		}
	}
	if got := tipsSum(resp); got != pool {
		t.Errorf("sum of shares: got %d, want %d", got, pool)
	}
	if resp.Distributed != pool {
		t.Errorf("distributed: got %d, want %d", resp.Distributed, pool)
	}
}

// ---------------------------------------------------------------------------
// Test 6: event_id scoping for mesero sales
// ---------------------------------------------------------------------------

// TestTipsEventIDScoping verifies that ?event_id= scopes mesero non-voided
// order totals to that event only, affecting sales ranking and (through it)
// bonus assignment.
func TestTipsEventIDScoping(t *testing.T) {
	d := openTestDB(t)
	now := core.NowMillis()

	const (
		evtA = "evt-sc-tips-a" // active
		evtB = "evt-sc-tips-b" // closed (avoids the unique-active-event constraint)
	)

	insertEventOnly(t, d, evtA, "active")
	insertEventOnly(t, d, evtB, "closed")

	// marta has orders only in evtA; nestor has orders only in evtB.
	insertMesero(t, d, "usr-sc-m1", "marta")
	insertMesero(t, d, "usr-sc-m2", "nestor")

	insertOrder(t, d, evtA, "usr-sc-m1", "ord-sc-a1", "paid", 200000, now)
	insertOrder(t, d, evtB, "usr-sc-m2", "ord-sc-b1", "paid", 200000, now)

	m := newTipsMod(t, d)

	// Scoped to evtA: marta=200000 sales, nestor=0 → marta ranks first.
	respA := callTips(t, m, "/api/reports/tips?pool=10000&event_id="+evtA)
	if n := len(respA.Meseros); n != 2 {
		t.Fatalf("evtA: meseros len=%d, want 2", n)
	}
	if respA.Meseros[0].Name != "marta" {
		t.Errorf("evtA top mesero: got %q, want 'marta'", respA.Meseros[0].Name)
	}
	if respA.Meseros[0].Sales != 200000 {
		t.Errorf("evtA marta.sales: got %d, want 200000", respA.Meseros[0].Sales)
	}
	if respA.Meseros[1].Name != "nestor" {
		t.Errorf("evtA second mesero: got %q, want 'nestor'", respA.Meseros[1].Name)
	}
	if respA.Meseros[1].Sales != 0 {
		t.Errorf("evtA nestor.sales: got %d, want 0 (no orders in evtA)", respA.Meseros[1].Sales)
	}
	if respA.Distributed != 10000 {
		t.Errorf("evtA distributed: got %d, want 10000", respA.Distributed)
	}
	if got := tipsSum(respA); got != 10000 {
		t.Errorf("evtA sum of shares: got %d, want 10000", got)
	}

	// Scoped to evtB: nestor=200000 sales, marta=0 → nestor ranks first.
	respB := callTips(t, m, "/api/reports/tips?pool=10000&event_id="+evtB)
	if n := len(respB.Meseros); n != 2 {
		t.Fatalf("evtB: meseros len=%d, want 2", n)
	}
	if respB.Meseros[0].Name != "nestor" {
		t.Errorf("evtB top mesero: got %q, want 'nestor'", respB.Meseros[0].Name)
	}
	if respB.Meseros[0].Sales != 200000 {
		t.Errorf("evtB nestor.sales: got %d, want 200000", respB.Meseros[0].Sales)
	}
	if respB.Meseros[1].Name != "marta" {
		t.Errorf("evtB second mesero: got %q, want 'marta'", respB.Meseros[1].Name)
	}
	if respB.Meseros[1].Sales != 0 {
		t.Errorf("evtB marta.sales: got %d, want 0 (no orders in evtB)", respB.Meseros[1].Sales)
	}
	if respB.Distributed != 10000 {
		t.Errorf("evtB distributed: got %d, want 10000", respB.Distributed)
	}
	if got := tipsSum(respB); got != 10000 {
		t.Errorf("evtB sum of shares: got %d, want 10000", got)
	}
}

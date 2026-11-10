// humanerror_test.go is an adversarial "fat-finger / human-error" suite for
// Action EventOS. Every case drives a realistic operator mistake over real HTTP
// against the fully-wired in-process server (the same harness as
// edgecases_test.go) and asserts the system degrades GRACEFULLY: a clean 4xx (or
// a documented 2xx), NEVER a 500/panic, and NEVER corrupt state. After each case
// GET /api/integrity must still report ok with an intact audit chain.
//
// These tests reuse the package-level harness, builders and helpers declared in
// edgecases_test.go (newHarness, h.req, h.mustPost, h.createAttendee, etc.).
package edgecases

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"eventpos/internal/bootstrap"
)

// adminUser / adminSecret are the seeded admin credentials, aliased here so the
// human-error cases read cleanly.
func adminUser() string   { return bootstrap.DefaultAdminUser }
func adminSecret() string { return bootstrap.DefaultAdminSecret }

// ---------------------------------------------------------------------------
// Shared assertions for this suite.
// ---------------------------------------------------------------------------

// assertIntegrityOK fails the test unless GET /api/integrity reports ok:true and
// an intact audit chain. This is the invariant every human-error case must keep.
func assertIntegrityOK(t *testing.T, h *harness) {
	t.Helper()
	code, data := h.req(t, "GET", "/api/integrity", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/integrity: status %d, body %s", code, data)
	}
	var res struct {
		OK                bool              `json:"ok"`
		BalanceMismatches []json.RawMessage `json:"balance_mismatches"`
		StockMismatches   []json.RawMessage `json:"stock_mismatches"`
		Audit             struct {
			Intact   bool   `json:"intact"`
			BrokenAt string `json:"broken_at"`
		} `json:"audit"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatalf("decode integrity: %v (body %s)", err, data)
	}
	if !res.OK {
		t.Fatalf("integrity ok=false: balance_mismatches=%d stock_mismatches=%d audit_intact=%v broken_at=%q",
			len(res.BalanceMismatches), len(res.StockMismatches), res.Audit.Intact, res.Audit.BrokenAt)
	}
	if !res.Audit.Intact {
		t.Fatalf("audit chain broken at %q", res.Audit.BrokenAt)
	}
}

// assertNo5xx fails if code is a server error (or a transport-level 0). A clean
// 4xx is the contract for bad input; a 5xx/panic is a defect.
func assertNo5xx(t *testing.T, label string, code int, body []byte) {
	t.Helper()
	if code >= 500 || code == 0 {
		t.Fatalf("%s: server error status %d (body %s) — expected a graceful 4xx", label, code, body)
	}
}

// ---------------------------------------------------------------------------
// H1. Duplicate display names are allowed by design; each is a DISTINCT
//     attendee, and search must find them all.
// ---------------------------------------------------------------------------

func TestHumanDuplicateNamesAreDistinct(t *testing.T) {
	h := newHarness(t)
	h.login(t, adminUser(), adminSecret())
	h.draftEvent(t, "cascade") // configuration (attendees) is allowed while draft

	names := []string{"José", "José", "José", "José Alejandro", "JOSE", "josé"}
	ids := make(map[string]bool, len(names))
	created := make([]attendeeResp, 0, len(names))
	for i, n := range names {
		code, data := h.req(t, "POST", "/api/attendees", map[string]any{
			"display_name": n, "initial_topup": int64(1000),
		})
		assertNo5xx(t, fmt.Sprintf("create %q (#%d)", n, i), code, data)
		if code != http.StatusOK && code != http.StatusCreated {
			t.Fatalf("create %q (#%d): status %d, body %s", n, i, code, data)
		}
		var a attendeeResp
		if err := json.Unmarshal(data, &a); err != nil {
			t.Fatalf("decode attendee %q: %v", n, err)
		}
		if ids[a.ID] {
			t.Fatalf("duplicate id %s returned for %q — registrations must be distinct", a.ID, n)
		}
		ids[a.ID] = true
		created = append(created, a)
	}
	if len(ids) != len(names) {
		t.Fatalf("distinct ids: got %d, want %d", len(ids), len(names))
	}

	// Each id must independently resolve to the right name.
	for i, a := range created {
		var full struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		}
		h.mustGet(t, "/api/attendees/"+a.ID, &full)
		if full.DisplayName != names[i] {
			t.Fatalf("attendee %s name: got %q, want %q", a.ID, full.DisplayName, names[i])
		}
	}

	// Search must surface every registration. lower()+LIKE is ASCII-case-
	// insensitive (accent-sensitive) in SQLite; every name lowercases to a
	// string containing "jos", so q="Jos" returns all six.
	var hits []attendeeResp
	h.mustGet(t, "/api/attendees?q=Jos", &hits)
	foundByID := map[string]bool{}
	for _, a := range hits {
		foundByID[a.ID] = true
	}
	for i, a := range created {
		if !foundByID[a.ID] {
			t.Fatalf("search q=Jos did not return attendee %s (%q)", a.ID, names[i])
		}
	}

	assertIntegrityOK(t, h)
}

// ---------------------------------------------------------------------------
// H2. Negative money on products is rejected on BOTH create and update.
// ---------------------------------------------------------------------------

func TestHumanNegativeProductMoney(t *testing.T) {
	h := newHarness(t)
	h.login(t, adminUser(), adminSecret())
	h.draftEvent(t, "cascade") // product config must happen while draft
	cat := h.createCategory(t, "Cat")

	// create with price = -100 -> 400.
	code, data := h.req(t, "POST", "/api/products", map[string]any{
		"category_id": cat, "name": "BadPrice", "price": int64(-100), "cost": int64(0),
		"prep_station": "none", "track_stock": 0, "stock_qty": int64(0),
	})
	assertNo5xx(t, "create price=-100", code, data)
	if code != http.StatusBadRequest {
		t.Fatalf("create price=-100: status %d, want 400 (body %s)", code, data)
	}

	// create with cost = -100 -> 400.
	code, data = h.req(t, "POST", "/api/products", map[string]any{
		"category_id": cat, "name": "BadCost", "price": int64(1000), "cost": int64(-100),
		"prep_station": "none", "track_stock": 0, "stock_qty": int64(0),
	})
	assertNo5xx(t, "create cost=-100", code, data)
	if code != http.StatusBadRequest {
		t.Fatalf("create cost=-100: status %d, want 400 (body %s)", code, data)
	}

	// A valid product to then mis-edit.
	prod := h.createProduct(t, cat, "Good", 5000, 0, "none", 0)

	// UPDATE price -> negative -> 400 (price validation was just added).
	code, data = h.req(t, "PUT", "/api/products/"+prod.ID, map[string]any{
		"name": "Good", "price": int64(-1), "cost": int64(0),
		"prep_station": "none", "track_stock": 0, "sort_order": 0, "active": 1,
	})
	assertNo5xx(t, "update price=-1", code, data)
	if code != http.StatusBadRequest {
		t.Fatalf("update price=-1: status %d, want 400 (body %s)", code, data)
	}

	// UPDATE cost -> negative -> 400.
	code, data = h.req(t, "PUT", "/api/products/"+prod.ID, map[string]any{
		"name": "Good", "price": int64(5000), "cost": int64(-50),
		"prep_station": "none", "track_stock": 0, "sort_order": 0, "active": 1,
	})
	assertNo5xx(t, "update cost=-50", code, data)
	if code != http.StatusBadRequest {
		t.Fatalf("update cost=-50: status %d, want 400 (body %s)", code, data)
	}

	// The product price must be untouched by the rejected edits.
	var ps []productResp
	h.mustGet(t, "/api/products", &ps)
	var found bool
	for _, p := range ps {
		if p.ID == prod.ID {
			found = true
			// price field is not in productResp; re-fetch raw to assert price.
		}
	}
	if !found {
		t.Fatalf("product %s vanished after rejected updates", prod.ID)
	}
	var raw []struct {
		ID    string `json:"id"`
		Price int64  `json:"price"`
		Cost  int64  `json:"cost"`
	}
	h.mustGet(t, "/api/products", &raw)
	for _, p := range raw {
		if p.ID == prod.ID {
			if p.Price != 5000 || p.Cost != 0 {
				t.Fatalf("product price/cost mutated by rejected edits: price=%d cost=%d, want 5000/0", p.Price, p.Cost)
			}
		}
	}

	assertIntegrityOK(t, h)
}

// ---------------------------------------------------------------------------
// H3. Inventory adjustment that would drive stock negative is rejected; no
//     negative stock is ever persisted.
// ---------------------------------------------------------------------------

func TestHumanInventoryNoNegativeStock(t *testing.T) {
	h := newHarness(t)
	h.login(t, adminUser(), adminSecret())
	h.draftEvent(t, "cascade") // inventory adjust is locked once the event is active
	cat := h.createCategory(t, "Cat")
	prod := h.createProduct(t, cat, "Stocked", 1000, 10, "none", 1) // tracked, stock=10

	// -100 against stock 10 -> would be -90 -> rejected with 400.
	code, data := h.req(t, "POST", "/api/inventory/adjust", map[string]any{
		"product_id": prod.ID, "delta": int64(-100), "reason": "adjustment",
	})
	assertNo5xx(t, "adjust -100 on stock 10", code, data)
	if code != http.StatusBadRequest {
		t.Fatalf("adjust -100 on stock 10: status %d, want 400 (body %s)", code, data)
	}
	if qty := h.productStockQty(t, prod.ID); qty != 10 {
		t.Fatalf("stock after rejected adjust: got %d, want 10 (unchanged)", qty)
	}

	// Sanity: a valid positive adjustment is accepted, proving the endpoint works.
	code, data = h.req(t, "POST", "/api/inventory/adjust", map[string]any{
		"product_id": prod.ID, "delta": int64(5), "reason": "restock",
	})
	assertNo5xx(t, "adjust +5", code, data)
	if code != http.StatusOK {
		t.Fatalf("adjust +5: status %d, want 200 (body %s)", code, data)
	}
	if qty := h.productStockQty(t, prod.ID); qty != 15 {
		t.Fatalf("stock after +5: got %d, want 15", qty)
	}

	// And the over-draw is still rejected at the new level.
	code, data = h.req(t, "POST", "/api/inventory/adjust", map[string]any{
		"product_id": prod.ID, "delta": int64(-1000), "reason": "waste",
	})
	assertNo5xx(t, "adjust -1000 on stock 15", code, data)
	if code != http.StatusBadRequest {
		t.Fatalf("adjust -1000: status %d, want 400 (body %s)", code, data)
	}
	if qty := h.productStockQty(t, prod.ID); qty != 15 {
		t.Fatalf("stock after rejected over-draw: got %d, want 15", qty)
	}

	assertIntegrityOK(t, h)
}

// ---------------------------------------------------------------------------
// H4. Sale quantities at the extremes (<=0 and absurdly huge) are rejected, the
//     cap is enforced, and no charge/order is produced.
// ---------------------------------------------------------------------------

func TestHumanSaleQtyExtremes(t *testing.T) {
	h := newHarness(t)
	h.login(t, adminUser(), adminSecret())
	ev := h.draftEvent(t, "cascade")
	cat := h.createCategory(t, "Cat")
	prod := h.createProduct(t, cat, "Cosa", 100, 999, "none", 0) // untracked: isolate qty validation
	att := h.createAttendee(t, "Bigwallet", 1_000_000_000)
	h.activate(t, ev)

	for i, qty := range []int64{0, -1, -1_000, 1_000_000_000 /* 1e9 */} {
		code, data := h.req(t, "POST", "/api/orders", map[string]any{
			"request_id": fmt.Sprintf("qty-extreme-%d", i),
			"items":      []map[string]any{{"product_id": prod.ID, "qty": qty}},
			"payers":     []map[string]any{{"attendee_id": att.ID}},
		})
		assertNo5xx(t, fmt.Sprintf("qty=%d", qty), code, data)
		if code != http.StatusBadRequest {
			t.Fatalf("qty=%d: status %d, want 400 (body %s)", qty, code, data)
		}
	}

	// No charge happened and no order was created.
	if bal := h.attendeeBalance(t, att.ID); bal != 1_000_000_000 {
		t.Fatalf("balance after rejected extreme-qty orders: got %d, want 1e9", bal)
	}
	var orders []idResp
	h.mustGet(t, "/api/orders", &orders)
	if len(orders) != 0 {
		t.Fatalf("orders created by rejected extreme-qty requests: got %d, want 0", len(orders))
	}

	assertIntegrityOK(t, h)
}

// ---------------------------------------------------------------------------
// H5. Empty attendee name is rejected.
// ---------------------------------------------------------------------------

func TestHumanEmptyAttendeeName(t *testing.T) {
	h := newHarness(t)
	h.login(t, adminUser(), adminSecret())
	h.draftEvent(t, "cascade")

	// Explicit empty string.
	code, data := h.req(t, "POST", "/api/attendees", map[string]any{
		"display_name": "", "initial_topup": int64(0),
	})
	assertNo5xx(t, "empty name", code, data)
	if code != http.StatusBadRequest {
		t.Fatalf("empty name: status %d, want 400 (body %s)", code, data)
	}

	// Name field omitted entirely (decodes to "").
	code, data = h.req(t, "POST", "/api/attendees", map[string]any{
		"initial_topup": int64(5000),
	})
	assertNo5xx(t, "missing name", code, data)
	if code != http.StatusBadRequest {
		t.Fatalf("missing name: status %d, want 400 (body %s)", code, data)
	}

	assertIntegrityOK(t, h)
}

// ---------------------------------------------------------------------------
// H6. "Cobrar 500 millones" — an order whose total wildly exceeds the payer's
//     balance must be graceful: the overage path requires cash_confirmed, and
//     WITHOUT it there is a clean 4xx with NO partial charge and NO negative
//     balance. WITH it the balance is exhausted (never negative) and the
//     residual is collected as cash.
// ---------------------------------------------------------------------------

func TestHumanCobrar500Millones(t *testing.T) {
	h := newHarness(t)
	h.login(t, adminUser(), adminSecret())
	ev := h.draftEvent(t, "cascade")
	cat := h.createCategory(t, "Cat")
	// Untracked so the absurd total isn't pre-empted by a stock check. price=5000,
	// qty=100000 (the cap) -> total = 500,000,000.
	prod := h.createProduct(t, cat, "Mega", 5000, 0, "none", 0)
	att := h.createAttendee(t, "Pobre", 10_000)
	h.activate(t, ev)

	const total = int64(5000) * 100_000 // 500,000,000

	// Without cash_confirmed -> clean 400, balance untouched, no order.
	code, data := h.req(t, "POST", "/api/orders", map[string]any{
		"request_id": "cobrar-500m-nocash",
		"items":      []map[string]any{{"product_id": prod.ID, "qty": int64(100_000)}},
		"payers":     []map[string]any{{"attendee_id": att.ID}},
	})
	assertNo5xx(t, "500M no cash", code, data)
	if code != http.StatusBadRequest {
		t.Fatalf("500M without cash_confirmed: status %d, want 400 (body %s)", code, data)
	}
	if bal := h.attendeeBalance(t, att.ID); bal != 10_000 {
		t.Fatalf("balance after rejected 500M order: got %d, want 10000 (no partial charge)", bal)
	}
	var orders []idResp
	h.mustGet(t, "/api/orders", &orders)
	if len(orders) != 0 {
		t.Fatalf("order created by rejected 500M request: got %d, want 0", len(orders))
	}

	// With cash_confirmed -> proceeds; balance exhausted to exactly 0 (never
	// negative); residual is cash.
	var ord orderResp
	h.mustPost(t, "/api/orders", map[string]any{
		"request_id":     "cobrar-500m-cash",
		"items":          []map[string]any{{"product_id": prod.ID, "qty": int64(100_000)}},
		"payers":         []map[string]any{{"attendee_id": att.ID}},
		"cash_confirmed": true,
	}, &ord)
	if ord.Total != total {
		t.Fatalf("500M total: got %d, want %d", ord.Total, total)
	}
	if bal := h.attendeeBalance(t, att.ID); bal != 0 {
		t.Fatalf("balance after 500M overage sale: got %d, want 0 (never negative)", bal)
	}
	var cash int64
	for _, p := range ord.Payments {
		if p.Method == "cash" {
			cash += p.Amount
		}
	}
	if cash != total-10_000 {
		t.Fatalf("cash collected: got %d, want %d", cash, total-10_000)
	}

	assertIntegrityOK(t, h)
}

// ---------------------------------------------------------------------------
// H7. Soft-delete only: a product is never hard-deleted, so prior orders'
//     item snapshots survive and integrity holds.
// ---------------------------------------------------------------------------

func TestHumanProductSoftDeleteKeepsHistory(t *testing.T) {
	h := newHarness(t)
	h.login(t, adminUser(), adminSecret())

	t.Run("delete_during_active_is_graceful_and_history_intact", func(t *testing.T) {
		ev := h.draftEvent(t, "cascade")
		cat := h.createCategory(t, "Bar")
		prod := h.createProduct(t, cat, "Cerveza Especial", 8000, 10, "none", 1)
		att := h.createAttendee(t, "Cliente", 50_000)
		h.activate(t, ev)

		var ord orderResp
		h.mustPost(t, "/api/orders", map[string]any{
			"request_id": "softdel-sale",
			"items":      []map[string]any{{"product_id": prod.ID, "qty": 2}},
			"payers":     []map[string]any{{"attendee_id": att.ID}},
		}, &ord)

		// Attempting to delete a product mid-event is rejected with a graceful
		// conflict (config is frozen once active) — NEVER a 500 and NEVER a
		// destructive removal that would orphan the order's snapshot.
		code, data := h.req(t, "DELETE", "/api/products/"+prod.ID, nil)
		assertNo5xx(t, "delete product during active", code, data)
		if code == http.StatusOK || code == http.StatusCreated {
			t.Fatalf("delete during active unexpectedly succeeded (status %d) — config must be locked", code)
		}
		if code != http.StatusConflict {
			t.Logf("note: delete during active returned %d (expected 409 event-locked); body %s", code, data)
		}

		// The product row still exists (no hard delete).
		var ps []productResp
		h.mustGet(t, "/api/products", &ps)
		var present bool
		for _, p := range ps {
			if p.ID == prod.ID {
				present = true
			}
		}
		if !present {
			t.Fatalf("product %s was removed — must be soft-delete only", prod.ID)
		}

		// The order's item snapshot is intact (name + price preserved).
		var detail struct {
			Items []struct {
				Name      string `json:"name"`
				UnitPrice int64  `json:"unit_price"`
				Qty       int64  `json:"qty"`
			} `json:"items"`
		}
		h.mustGet(t, "/api/orders/"+ord.ID, &detail)
		if len(detail.Items) != 1 || detail.Items[0].Name != "Cerveza Especial" || detail.Items[0].UnitPrice != 8000 {
			t.Fatalf("order snapshot corrupted: %+v", detail.Items)
		}
		assertIntegrityOK(t, h)
	})

	t.Run("soft_delete_keeps_row", func(t *testing.T) {
		h2 := newHarness(t)
		h2.login(t, adminUser(), adminSecret())
		h2.draftEvent(t, "cascade") // delete allowed while draft
		cat := h2.createCategory(t, "Cat")
		prod := h2.createProduct(t, cat, "Descartable", 1000, 5, "none", 1)

		code, data := h2.req(t, "DELETE", "/api/products/"+prod.ID, nil)
		assertNo5xx(t, "soft delete draft", code, data)
		if code != http.StatusOK {
			t.Fatalf("soft delete (draft): status %d, want 200 (body %s)", code, data)
		}

		// The row survives with active=0 — proving soft, not hard, delete.
		var ps []struct {
			ID     string `json:"id"`
			Active int    `json:"active"`
		}
		h2.mustGet(t, "/api/products", &ps)
		var found bool
		for _, p := range ps {
			if p.ID == prod.ID {
				found = true
				if p.Active != 0 {
					t.Fatalf("deleted product active=%d, want 0", p.Active)
				}
			}
		}
		if !found {
			t.Fatalf("product %s hard-deleted — must remain as soft delete", prod.ID)
		}
		assertIntegrityOK(t, h2)
	})
}

// ---------------------------------------------------------------------------
// H8. There is NO destructive attendee delete endpoint that could orphan
//     orders. DELETE on the attendee resource must NOT succeed, and the
//     attendee must survive intact.
// ---------------------------------------------------------------------------

func TestHumanNoAttendeeHardDelete(t *testing.T) {
	h := newHarness(t)
	h.login(t, adminUser(), adminSecret())
	h.draftEvent(t, "cascade")
	att := h.createAttendee(t, "Permanente", 25_000)

	code, data := h.req(t, "DELETE", "/api/attendees/"+att.ID, nil)
	assertNo5xx(t, "DELETE attendee", code, data)
	if code == http.StatusOK || code == http.StatusCreated || code == http.StatusNoContent {
		t.Fatalf("DELETE /api/attendees/{id} unexpectedly succeeded (status %d) — no destructive delete must exist", code)
	}
	// The route is simply not registered for DELETE -> 405 Method Not Allowed.
	if code != http.StatusMethodNotAllowed {
		t.Logf("note: DELETE attendee returned %d (expected 405 route-absent); body %s", code, data)
	}

	// The attendee is untouched.
	var got attendeeResp
	h.mustGet(t, "/api/attendees/"+att.ID, &got)
	if got.ID != att.ID || got.Balance != 25_000 {
		t.Fatalf("attendee mutated/removed after DELETE attempt: %+v", got)
	}

	assertIntegrityOK(t, h)
}

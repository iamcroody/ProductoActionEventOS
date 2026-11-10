package catalog

// catalog_lockdown_test.go — proves the lifecycle lock-down: catalog
// configuration mutations are rejected with HTTP 409 + core.ErrEventLocked once
// the current event is 'active' (started), while reads keep working. Helpers
// (newMux, serve, seedSession, seedStartedEvent, seedCoverProduct,
// seedCoverCategory) live in catalog_test.go / catalog_cover_test.go.

import (
	"encoding/json"
	"net/http"
	"testing"

	"eventpos/internal/core"
)

// expectLocked asserts a 409 Conflict whose body error equals ErrEventLocked.
func expectLocked(t *testing.T, code int, body string) {
	t.Helper()
	if code != http.StatusConflict {
		t.Fatalf("locked mutation: got %d, want 409; body: %s", code, body)
	}
	var resp map[string]string
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, body)
	}
	if resp["error"] != core.ErrEventLocked.Error() {
		t.Errorf("error = %q, want %q", resp["error"], core.ErrEventLocked.Error())
	}
}

func TestCreateCategory_LockedWhenActive(t *testing.T) {
	d, mux := newMux(t)
	seedStartedEvent(t, d)
	tok := seedSession(t, d, "")

	rr := serve(mux, http.MethodPost, "/api/categories", `{"name":"Bebidas"}`, tok)
	expectLocked(t, rr.Code, rr.Body.String())
}

func TestUpdateCategory_LockedWhenActive(t *testing.T) {
	d, mux := newMux(t)
	evID := seedStartedEvent(t, d)
	tok := seedSession(t, d, "")
	catID := seedCoverCategory(t, d, evID, "OldName")

	rr := serve(mux, http.MethodPut, "/api/categories/"+catID,
		`{"name":"NewName","sort_order":1,"active":1}`, tok)
	expectLocked(t, rr.Code, rr.Body.String())
}

func TestCreateProduct_LockedWhenActive(t *testing.T) {
	d, mux := newMux(t)
	seedStartedEvent(t, d)
	tok := seedSession(t, d, "")

	rr := serve(mux, http.MethodPost, "/api/products",
		`{"name":"Cerveza","price":5000,"prep_station":"bar"}`, tok)
	expectLocked(t, rr.Code, rr.Body.String())
}

func TestUpdateProduct_LockedWhenActive(t *testing.T) {
	d, mux := newMux(t)
	evID := seedStartedEvent(t, d)
	tok := seedSession(t, d, "")
	pid := seedCoverProduct(t, d, evID, "Prod", 0, 0)

	rr := serve(mux, http.MethodPut, "/api/products/"+pid,
		`{"name":"X","price":100,"prep_station":"none","track_stock":0,"active":1}`, tok)
	expectLocked(t, rr.Code, rr.Body.String())
}

func TestDeleteProduct_LockedWhenActive(t *testing.T) {
	d, mux := newMux(t)
	evID := seedStartedEvent(t, d)
	tok := seedSession(t, d, "")
	pid := seedCoverProduct(t, d, evID, "Prod", 0, 0)

	rr := serve(mux, http.MethodDelete, "/api/products/"+pid, "", tok)
	expectLocked(t, rr.Code, rr.Body.String())
}

func TestAdjustStock_LockedWhenActive(t *testing.T) {
	d, mux := newMux(t)
	evID := seedStartedEvent(t, d)
	tok := seedSession(t, d, "")
	pid := seedCoverProduct(t, d, evID, "Beer", 1, 10)

	rr := serve(mux, http.MethodPost, "/api/inventory/adjust",
		`{"product_id":"`+pid+`","delta":5,"reason":"restock"}`, tok)
	expectLocked(t, rr.Code, rr.Body.String())

	// The rejected adjust must not have mutated stock.
	var qty int64
	if err := d.Reader.QueryRow(`SELECT stock_qty FROM products WHERE id=?`, pid).Scan(&qty); err != nil {
		t.Fatalf("read stock: %v", err)
	}
	if qty != 10 {
		t.Fatalf("locked adjust mutated stock: got %d, want 10", qty)
	}
}

// TestReadsWorkWhenActive confirms reads are NOT locked: listing products and
// categories succeeds against a started (active) event.
func TestReadsWorkWhenActive(t *testing.T) {
	d, mux := newMux(t)
	evID := seedStartedEvent(t, d)
	tok := seedSession(t, d, "")
	seedCoverProduct(t, d, evID, "ReadBeer", 1, 10)
	seedCoverCategory(t, d, evID, "ReadCat")

	if rr := serve(mux, http.MethodGet, "/api/products", "", tok); rr.Code != http.StatusOK {
		t.Fatalf("list products while active: got %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if rr := serve(mux, http.MethodGet, "/api/categories", "", tok); rr.Code != http.StatusOK {
		t.Fatalf("list categories while active: got %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
}

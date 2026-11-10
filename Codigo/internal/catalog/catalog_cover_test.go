package catalog

// catalog_cover_test.go – HTTP-level coverage tests for every route registered
// by (*Module).Register. All helpers in catalog_test.go (openTestDB, buildDeps,
// seedSession, authedReq) are reused because both files share the same package.
//
// Strategy: spin up a fresh DB+writer+mux per test case, seed the minimum
// required state, drive requests through the real ServeMux, and assert HTTP
// status codes (plus a few body checks for the happy paths).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"eventpos/internal/core"
	"eventpos/internal/db"
	"eventpos/internal/writer"
)

// ---------------------------------------------------------------------------
// Helpers unique to this file
// ---------------------------------------------------------------------------

// seedActiveEvent inserts a single current event in 'draft' status (the state in
// which catalog configuration is permitted) and returns its ID. Despite the
// historical name, it seeds a draft: under the lifecycle lock-down, mutations are
// rejected once the event is 'active'. Use seedStartedEvent for that case.
func seedActiveEvent(t *testing.T, d *db.DB) string {
	t.Helper()
	ctx := context.Background()
	now := core.NowMillis()
	eventID := core.NewID()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
		 VALUES (?,?,?,?,?,?)`,
		eventID, "Cover Test Event", "draft", 50000, "cascade", now); err != nil {
		t.Fatalf("seedActiveEvent: %v", err)
	}
	return eventID
}

// seedStartedEvent inserts a current event already in 'active' (started) status,
// in which catalog configuration is locked. Returns its ID.
func seedStartedEvent(t *testing.T, d *db.DB) string {
	t.Helper()
	ctx := context.Background()
	now := core.NowMillis()
	eventID := core.NewID()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
		 VALUES (?,?,?,?,?,?)`,
		eventID, "Started Cover Event", "active", 50000, "cascade", now); err != nil {
		t.Fatalf("seedStartedEvent: %v", err)
	}
	return eventID
}

// seedRoleSession creates a user with the given role and an associated session.
// rawToken is used to derive the session hash and must be unique within the DB.
func seedRoleSession(t *testing.T, d *db.DB, rawToken, role string) {
	t.Helper()
	ctx := context.Background()
	now := core.NowMillis()
	userID := core.NewID()
	username := role + "-" + core.NewID()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO users(id, username, role, pin_hash, active, created_at)
		 VALUES (?,?,?,?,1,?)`,
		userID, username, role, "argon2id$dummysalt$dummykey", now); err != nil {
		t.Fatalf("seedRoleSession user (%s): %v", role, err)
	}
	sum := sha256.Sum256([]byte(rawToken))
	sessionHash := hex.EncodeToString(sum[:])
	exp := now + 24*3600*1000
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO sessions(id, user_id, ip, created_at, expires_at)
		 VALUES (?,?,?,?,?)`,
		sessionHash, userID, "127.0.0.1", now, exp); err != nil {
		t.Fatalf("seedRoleSession session (%s): %v", role, err)
	}
}

// newMux spins up a fresh database + writer + module and returns the DB and mux.
func newMux(t *testing.T) (*db.DB, *http.ServeMux) {
	t.Helper()
	d := openTestDB(t)
	w := writer.New(d.Writer)
	t.Cleanup(func() { w.Close() })
	deps := buildDeps(d, w)
	mod := New(deps)
	mux := http.NewServeMux()
	mod.Register(mux, deps)
	return d, mux
}

// seedCoverProduct inserts a product row directly and returns its ID.
func seedCoverProduct(t *testing.T, d *db.DB, eventID, name string, trackStock int, stockQty int64) string {
	t.Helper()
	ctx := context.Background()
	now := core.NowMillis()
	pid := core.NewID()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO products(id, event_id, name, price, prep_station,
		  track_stock, stock_qty, stock_version, sort_order, active, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		pid, eventID, name, 1000, "none", trackStock, stockQty, 0, 0, 1, now); err != nil {
		t.Fatalf("seedCoverProduct %q: %v", name, err)
	}
	return pid
}

// seedCoverCategory inserts a category row directly and returns its ID.
func seedCoverCategory(t *testing.T, d *db.DB, eventID, name string) string {
	t.Helper()
	ctx := context.Background()
	catID := core.NewID()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO categories(id, event_id, name, sort_order) VALUES (?,?,?,?)`,
		catID, eventID, name, 0); err != nil {
		t.Fatalf("seedCoverCategory %q: %v", name, err)
	}
	return catID
}

// doUnauth fires a request with no session cookie.
func doUnauth(mux *http.ServeMux, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(""))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

// serve builds an authed request and calls mux.ServeHTTP, returning the recorder.
func serve(mux *http.ServeMux, method, target, body, token string) *httptest.ResponseRecorder {
	req := authedReq(method, target, body, token)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

// ---------------------------------------------------------------------------
// Category handler tests
// ---------------------------------------------------------------------------

func TestListCategories_HappyPath(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)
	seedCoverCategory(t, d, eventID, "Bebidas")

	rr := serve(mux, http.MethodGet, "/api/categories", "", tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("list categories: got %d: %s", rr.Code, rr.Body.String())
	}
	var cats []Category
	if err := json.NewDecoder(rr.Body).Decode(&cats); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cats) != 1 || cats[0].Name != "Bebidas" {
		t.Errorf("expected 1 category 'Bebidas', got %+v", cats)
	}
}

func TestListCategories_NoActiveEvent(t *testing.T) {
	d, mux := newMux(t)
	tok := seedSession(t, d, "")
	rr := serve(mux, http.MethodGet, "/api/categories", "", tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("no-event list categories: got %d", rr.Code)
	}
}

func TestListCategories_Unauthenticated(t *testing.T) {
	_, mux := newMux(t)
	rr := doUnauth(mux, http.MethodGet, "/api/categories")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth list categories: got %d", rr.Code)
	}
}

func TestCreateCategory_HappyPath(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)

	rr := serve(mux, http.MethodPost, "/api/categories", `{"name":"Comida","sort_order":1}`, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("create category: got %d: %s", rr.Code, rr.Body.String())
	}
	var cat Category
	if err := json.NewDecoder(rr.Body).Decode(&cat); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cat.Name != "Comida" || cat.EventID != eventID {
		t.Errorf("unexpected category: %+v", cat)
	}
}

func TestCreateCategory_EmptyName(t *testing.T) {
	d, mux := newMux(t)
	// createCategory calls activeEventID first, so we need an event seeded.
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)

	rr := serve(mux, http.MethodPost, "/api/categories", `{"name":""}`, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty name: got %d", rr.Code)
	}
}

func TestCreateCategory_MalformedJSON(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)

	rr := serve(mux, http.MethodPost, "/api/categories", `not-json`, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON create category: got %d", rr.Code)
	}
}

func TestCreateCategory_NoActiveEvent(t *testing.T) {
	d, mux := newMux(t)
	// createCategory calls activeEventID before Decode, so no event → 400.
	tok := seedSession(t, d, "")
	rr := serve(mux, http.MethodPost, "/api/categories", `{"name":"Test"}`, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("no-event create category: got %d", rr.Code)
	}
}

func TestCreateCategory_CajaForbidden(t *testing.T) {
	d, mux := newMux(t)
	// createCategory is admin-only; caja must receive 403.
	const cajaToken = "caja-cat-create-cover-token"
	seedRoleSession(t, d, cajaToken, "caja")

	rr := serve(mux, http.MethodPost, "/api/categories", `{"name":"X"}`, cajaToken)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("caja create category: got %d (want 403)", rr.Code)
	}
}

func TestUpdateCategory_HappyPath(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)
	catID := seedCoverCategory(t, d, eventID, "OldName")

	rr := serve(mux, http.MethodPut, "/api/categories/"+catID,
		`{"name":"NewName","sort_order":2,"active":1}`, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("update category: got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestUpdateCategory_NotFound(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)

	rr := serve(mux, http.MethodPut, "/api/categories/nonexistent-cat-id",
		`{"name":"X","sort_order":0,"active":1}`, tok)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("not-found update category: got %d", rr.Code)
	}
}

func TestUpdateCategory_MalformedJSON(t *testing.T) {
	d, mux := newMux(t)
	// updateCategory calls Decode before activeEventID — no event needed.
	tok := seedSession(t, d, "")

	rr := serve(mux, http.MethodPut, "/api/categories/some-id", `bad-json`, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON update category: got %d", rr.Code)
	}
}

func TestUpdateCategory_NoActiveEvent(t *testing.T) {
	d, mux := newMux(t)
	// Decode runs first (succeeds), then activeEventID → no event → 400.
	tok := seedSession(t, d, "")

	rr := serve(mux, http.MethodPut, "/api/categories/some-id",
		`{"name":"X","sort_order":0,"active":1}`, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("no-event update category: got %d", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// Product handler tests
// ---------------------------------------------------------------------------

func TestListProducts_HappyPath(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)
	seedCoverProduct(t, d, eventID, "Cerveza", 0, 0)

	rr := serve(mux, http.MethodGet, "/api/products", "", tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("list products: got %d: %s", rr.Code, rr.Body.String())
	}
	var products []Product
	if err := json.NewDecoder(rr.Body).Decode(&products); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(products) != 1 || products[0].Name != "Cerveza" {
		t.Errorf("expected 1 product 'Cerveza', got %+v", products)
	}
}

func TestListProducts_WithCategoryFilter(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)
	catID := seedCoverCategory(t, d, eventID, "Licores")

	ctx := context.Background()
	now := core.NowMillis()
	pid1 := core.NewID()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO products(id, event_id, category_id, name, price, prep_station,
		  track_stock, stock_qty, stock_version, sort_order, active, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		pid1, eventID, catID, "Ron", 3000, "bar", 0, 0, 0, 0, 1, now); err != nil {
		t.Fatalf("seed product with category: %v", err)
	}
	seedCoverProduct(t, d, eventID, "Agua", 0, 0) // no category

	rr := serve(mux, http.MethodGet, "/api/products?category="+catID, "", tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("list products (category filter): got %d: %s", rr.Code, rr.Body.String())
	}
	var products []Product
	if err := json.NewDecoder(rr.Body).Decode(&products); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(products) != 1 || products[0].ID != pid1 {
		t.Errorf("expected 1 product for catID, got %+v", products)
	}
}

func TestListProducts_WithActiveFilter(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)

	ctx := context.Background()
	now := core.NowMillis()
	pidInactive := core.NewID()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO products(id, event_id, name, price, prep_station,
		  track_stock, stock_qty, stock_version, sort_order, active, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		core.NewID(), eventID, "ActiveProd", 1000, "none", 0, 0, 0, 0, 1, now); err != nil {
		t.Fatalf("seed active product: %v", err)
	}
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO products(id, event_id, name, price, prep_station,
		  track_stock, stock_qty, stock_version, sort_order, active, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		pidInactive, eventID, "InactiveProd", 1000, "none", 0, 0, 0, 0, 0, now); err != nil {
		t.Fatalf("seed inactive product: %v", err)
	}

	rr := serve(mux, http.MethodGet, "/api/products?active=0", "", tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("list products (active filter): got %d: %s", rr.Code, rr.Body.String())
	}
	var products []Product
	if err := json.NewDecoder(rr.Body).Decode(&products); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(products) != 1 || products[0].ID != pidInactive {
		t.Errorf("expected 1 inactive product, got %+v", products)
	}
}

func TestListProducts_NoActiveEvent(t *testing.T) {
	d, mux := newMux(t)
	tok := seedSession(t, d, "")
	rr := serve(mux, http.MethodGet, "/api/products", "", tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("no-event list products: got %d", rr.Code)
	}
}

func TestListProducts_Unauthenticated(t *testing.T) {
	_, mux := newMux(t)
	rr := doUnauth(mux, http.MethodGet, "/api/products")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth list products: got %d", rr.Code)
	}
}

func TestCreateProduct_HappyPath(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)

	body := `{"name":"Hamburguesa","price":5000,"prep_station":"cocina","track_stock":0}`
	rr := serve(mux, http.MethodPost, "/api/products", body, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("create product: got %d: %s", rr.Code, rr.Body.String())
	}
	var p Product
	if err := json.NewDecoder(rr.Body).Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Name != "Hamburguesa" || p.Price != 5000 || p.PrepStation != "cocina" {
		t.Errorf("unexpected product: %+v", p)
	}
}

func TestCreateProduct_EmptyName(t *testing.T) {
	d, mux := newMux(t)
	// createProduct calls activeEventID first, so we need an event.
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)

	rr := serve(mux, http.MethodPost, "/api/products", `{"name":"","price":1000}`, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty name: got %d", rr.Code)
	}
}

func TestCreateProduct_MalformedJSON(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)

	rr := serve(mux, http.MethodPost, "/api/products", `{bad json}`, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON create product: got %d", rr.Code)
	}
}

func TestCreateProduct_DefaultPrepStation(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)

	// No prep_station in body → handler defaults to "none".
	body := `{"name":"Fries","price":2000}`
	rr := serve(mux, http.MethodPost, "/api/products", body, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("default prep_station: got %d: %s", rr.Code, rr.Body.String())
	}
	var p Product
	if err := json.NewDecoder(rr.Body).Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.PrepStation != "none" {
		t.Errorf("prep_station = %q, want 'none'", p.PrepStation)
	}
}

func TestCreateProduct_WithCategory(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)
	catID := seedCoverCategory(t, d, eventID, "Snacks")

	body := `{"name":"Nachos","price":3000,"category_id":"` + catID + `"}`
	rr := serve(mux, http.MethodPost, "/api/products", body, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("create product with category: got %d: %s", rr.Code, rr.Body.String())
	}
	var p Product
	if err := json.NewDecoder(rr.Body).Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.CategoryID == nil || *p.CategoryID != catID {
		t.Errorf("category_id = %v, want %q", p.CategoryID, catID)
	}
}

func TestCreateProduct_NoActiveEvent(t *testing.T) {
	d, mux := newMux(t)
	tok := seedSession(t, d, "")
	rr := serve(mux, http.MethodPost, "/api/products", `{"name":"Test","price":1000}`, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("no-event create product: got %d", rr.Code)
	}
}

func TestUpdateProduct_NotFound(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)

	body := `{"name":"X","price":100,"prep_station":"none","track_stock":0,"active":1}`
	rr := serve(mux, http.MethodPut, "/api/products/nonexistent-product-id", body, tok)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("update not-found product: got %d", rr.Code)
	}
}

func TestUpdateProduct_MalformedJSON(t *testing.T) {
	d, mux := newMux(t)
	// updateProduct calls Decode before activeEventID — no event needed.
	tok := seedSession(t, d, "")

	rr := serve(mux, http.MethodPut, "/api/products/some-id", `{bad json}`, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON update product: got %d", rr.Code)
	}
}

func TestUpdateProduct_NoActiveEvent(t *testing.T) {
	d, mux := newMux(t)
	tok := seedSession(t, d, "")

	body := `{"name":"X","price":100,"prep_station":"none","track_stock":0,"active":1}`
	rr := serve(mux, http.MethodPut, "/api/products/some-id", body, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("no-event update product: got %d", rr.Code)
	}
}

func TestDeleteProduct_HappyPath(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)
	pid := seedCoverProduct(t, d, eventID, "DeleteMe", 0, 0)

	rr := serve(mux, http.MethodDelete, "/api/products/"+pid, "", tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete product: got %d: %s", rr.Code, rr.Body.String())
	}

	var active int
	if err := d.Reader.QueryRowContext(context.Background(),
		`SELECT active FROM products WHERE id=?`, pid).Scan(&active); err != nil {
		t.Fatalf("read active: %v", err)
	}
	if active != 0 {
		t.Errorf("deleted product active = %d, want 0", active)
	}
}

func TestDeleteProduct_NotFound(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)

	rr := serve(mux, http.MethodDelete, "/api/products/nonexistent-delete-id", "", tok)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("delete not-found: got %d", rr.Code)
	}
}

func TestDeleteProduct_Unauthenticated(t *testing.T) {
	_, mux := newMux(t)
	rr := doUnauth(mux, http.MethodDelete, "/api/products/some-id")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth delete: got %d", rr.Code)
	}
}

func TestDeleteProduct_CajaForbidden(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	pid := seedCoverProduct(t, d, eventID, "SomeProd", 0, 0)
	const cajaToken = "caja-delete-forbidden-cover-token"
	seedRoleSession(t, d, cajaToken, "caja")

	rr := serve(mux, http.MethodDelete, "/api/products/"+pid, "", cajaToken)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("caja delete product: got %d (want 403)", rr.Code)
	}
}

func TestDeleteProduct_NoActiveEvent(t *testing.T) {
	d, mux := newMux(t)
	tok := seedSession(t, d, "")

	rr := serve(mux, http.MethodDelete, "/api/products/some-id", "", tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("no-event delete product: got %d", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// Inventory adjust handler tests
// ---------------------------------------------------------------------------

func TestAdjustStock_UnknownProduct(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)

	// The product ID doesn't exist → ErrNotFound inside Writer.Do.
	body := `{"product_id":"nonexistent-product-id","delta":5,"reason":"restock"}`
	rr := serve(mux, http.MethodPost, "/api/inventory/adjust", body, tok)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown product adjust: got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAdjustStock_BadReason(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)
	pid := seedCoverProduct(t, d, eventID, "BeerBad", 1, 10)

	body := `{"product_id":"` + pid + `","delta":1,"reason":"garbage"}`
	rr := serve(mux, http.MethodPost, "/api/inventory/adjust", body, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad reason: got %d", rr.Code)
	}
}

func TestAdjustStock_EmptyProductID(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)

	body := `{"product_id":"","delta":1,"reason":"restock"}`
	rr := serve(mux, http.MethodPost, "/api/inventory/adjust", body, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty product_id: got %d", rr.Code)
	}
}

func TestAdjustStock_EmptyReason(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)
	pid := seedCoverProduct(t, d, eventID, "BeerReason", 1, 10)

	body := `{"product_id":"` + pid + `","delta":1,"reason":""}`
	rr := serve(mux, http.MethodPost, "/api/inventory/adjust", body, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty reason: got %d", rr.Code)
	}
}

func TestAdjustStock_MalformedJSON(t *testing.T) {
	d, mux := newMux(t)
	// adjustStock calls Decode before any other check; no event needed.
	tok := seedSession(t, d, "")

	rr := serve(mux, http.MethodPost, "/api/inventory/adjust", `{bad}`, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON adjust: got %d", rr.Code)
	}
}

func TestAdjustStock_NoActiveEvent(t *testing.T) {
	d, mux := newMux(t)
	// Validation passes, but activeEventID finds no event → 400.
	tok := seedSession(t, d, "")

	body := `{"product_id":"some-fake-id","delta":1,"reason":"restock"}`
	rr := serve(mux, http.MethodPost, "/api/inventory/adjust", body, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("no-event adjust: got %d", rr.Code)
	}
}

func TestAdjustStock_CajaAllowed(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	pid := seedCoverProduct(t, d, eventID, "CajaBeer", 1, 20)
	const cajaToken = "caja-adjust-allowed-cover-token"
	seedRoleSession(t, d, cajaToken, "caja")

	body := `{"product_id":"` + pid + `","delta":-1,"reason":"waste"}`
	rr := serve(mux, http.MethodPost, "/api/inventory/adjust", body, cajaToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("caja adjust: got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAdjustStock_ViewerForbidden(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	pid := seedCoverProduct(t, d, eventID, "ViewerBeer", 1, 10)
	const viewerToken = "viewer-adjust-forbidden-cover-token"
	seedRoleSession(t, d, viewerToken, "mesero")

	body := `{"product_id":"` + pid + `","delta":1,"reason":"restock"}`
	rr := serve(mux, http.MethodPost, "/api/inventory/adjust", body, viewerToken)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("viewer adjust: got %d (want 403)", rr.Code)
	}
}

func TestAdjustStock_Unauthenticated(t *testing.T) {
	_, mux := newMux(t)
	rr := doUnauth(mux, http.MethodPost, "/api/inventory/adjust")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth adjust: got %d", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// Movements handler tests
// ---------------------------------------------------------------------------

func TestListMovements_HappyPath(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)
	pid := seedCoverProduct(t, d, eventID, "MovProd", 1, 10)

	// Seed a movement row directly (FK requires a real product).
	ctx := context.Background()
	now := core.NowMillis()
	movID := core.NewID()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO stock_movements(id, product_id, delta, reason, balance_after, user_id, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		movID, pid, 10, "initial", 10, "test-user-id", now); err != nil {
		t.Fatalf("seed movement: %v", err)
	}

	rr := serve(mux, http.MethodGet, "/api/inventory/movements?product_id="+pid, "", tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("list movements: got %d: %s", rr.Code, rr.Body.String())
	}
	var movements []StockMovement
	if err := json.NewDecoder(rr.Body).Decode(&movements); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(movements) != 1 || movements[0].ID != movID {
		t.Errorf("expected 1 movement (id=%s), got %+v", movID, movements)
	}
}

func TestListMovements_MissingProductID(t *testing.T) {
	d, mux := newMux(t)
	// listMovements checks product_id before any DB query; no event needed.
	tok := seedSession(t, d, "")
	rr := serve(mux, http.MethodGet, "/api/inventory/movements", "", tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing product_id: got %d", rr.Code)
	}
}

func TestListMovements_Unauthenticated(t *testing.T) {
	_, mux := newMux(t)
	rr := doUnauth(mux, http.MethodGet, "/api/inventory/movements?product_id=some-id")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth list movements: got %d", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// Low-stock handler tests
// ---------------------------------------------------------------------------

func TestLowStock_NoActiveEvent(t *testing.T) {
	d, mux := newMux(t)
	tok := seedSession(t, d, "")
	rr := serve(mux, http.MethodGet, "/api/inventory/low-stock", "", tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("no-event low-stock: got %d", rr.Code)
	}
}

func TestLowStock_Unauthenticated(t *testing.T) {
	_, mux := newMux(t)
	rr := doUnauth(mux, http.MethodGet, "/api/inventory/low-stock")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth low-stock: got %d", rr.Code)
	}
}

func TestLowStock_DefaultThreshold(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)
	// stock=3 is below the default threshold of 5.
	seedCoverProduct(t, d, eventID, "LowBeer", 1, 3)

	rr := serve(mux, http.MethodGet, "/api/inventory/low-stock", "", tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("low-stock default threshold: got %d: %s", rr.Code, rr.Body.String())
	}
	var products []Product
	if err := json.NewDecoder(rr.Body).Decode(&products); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(products) != 1 {
		t.Errorf("expected 1 product below default threshold=5, got %d", len(products))
	}
}

func TestLowStock_CustomThreshold(t *testing.T) {
	d, mux := newMux(t)
	eventID := seedActiveEvent(t, d)
	tok := seedSession(t, d, eventID)
	// stock=8 is above default threshold (5) but below custom threshold (10).
	seedCoverProduct(t, d, eventID, "MediumBeer", 1, 8)

	rr := serve(mux, http.MethodGet, "/api/inventory/low-stock?threshold=10", "", tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("low-stock custom threshold: got %d: %s", rr.Code, rr.Body.String())
	}
	var products []Product
	if err := json.NewDecoder(rr.Body).Decode(&products); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(products) != 1 {
		t.Errorf("expected 1 product below threshold=10, got %d", len(products))
	}
}

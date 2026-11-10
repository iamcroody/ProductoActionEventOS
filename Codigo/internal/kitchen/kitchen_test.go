package kitchen

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/core"
	"eventpos/internal/db"
	"eventpos/internal/realtime"
	"eventpos/internal/server"
	"eventpos/internal/writer"
)

// harness builds the module behind a mux with an authenticated admin cookie,
// plus an active event and one order with a single 6-unit pending item.
type harness struct {
	t      *testing.T
	app    *app.App
	mux    *http.ServeMux
	cookie *http.Cookie
	itemID string
	order  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	w := writer.New(d.Writer)
	t.Cleanup(func() { w.Close() })
	a := &app.App{DB: d, Writer: w, Hub: realtime.NewHub(64)}
	authSvc := auth.New(d)
	deps := server.Deps{App: a, Auth: authSvc}
	mux := http.NewServeMux()
	New(deps).Register(mux, deps)

	ctx := context.Background()
	eventID, orderID, itemID, userID := core.NewID(), core.NewID(), core.NewID(), core.NewID()
	prodID := core.NewID()
	now := core.NowMillis()
	pin, _ := auth.HashSecret("1234")
	_, err = w.Do(ctx, func(tx *sql.Tx) (any, error) {
		tx.Exec(`INSERT INTO users(id,username,full_name,role,pin_hash,active,created_at) VALUES (?,?,?,?,?,1,?)`,
			userID, "admin", "Admin", "admin", pin, now)
		tx.Exec(`INSERT INTO events(id,name,status,default_topup,split_policy,created_at) VALUES (?,?,'active',50000,'cascade',?)`,
			eventID, "Test", now)
		tx.Exec(`INSERT INTO products(id,event_id,name,price,track_stock,stock_qty,active,created_at) VALUES (?,?,?,1000,0,0,1,?)`,
			prodID, eventID, "Agua", now)
		tx.Exec(`INSERT INTO orders(id,event_id,operator_id,status,created_at) VALUES (?,?,?,'preparing',?)`,
			orderID, eventID, userID, now)
		_, e := tx.Exec(`INSERT INTO order_items(id,order_id,product_id,name,qty,delivered_qty,unit_price,line_total,prep_status) VALUES (?,?,?,'Agua',6,0,1000,6000,'queued')`,
			itemID, orderID, prodID)
		return nil, e
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	token, _, err := authSvc.Login(ctx, "admin", "1234", "127.0.0.1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return &harness{t: t, app: a, mux: mux,
		cookie: &http.Cookie{Name: "aeos_session", Value: token}, itemID: itemID, order: orderID}
}

func (h *harness) post(path, body string) (int, map[string]any) {
	h.t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest("POST", path, nil)
	} else {
		r = httptest.NewRequest("POST", path, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.AddCookie(h.cookie)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, r)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// boardPending returns the total pending units across the board.
func (h *harness) boardPending() int64 {
	h.t.Helper()
	r := httptest.NewRequest("GET", "/api/pendientes", nil)
	r.AddCookie(h.cookie)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, r)
	var orders []BoardOrder
	_ = json.Unmarshal(rec.Body.Bytes(), &orders)
	var total int64
	for _, o := range orders {
		for _, it := range o.Items {
			total += it.Pending
		}
	}
	return total
}

func (h *harness) orderStatus() string {
	var s string
	h.app.DB.Reader.QueryRow(`SELECT status FROM orders WHERE id=?`, h.order).Scan(&s)
	return s
}

// TestPartialDelivery delivers a 6-unit item in stages and verifies the board
// pending count and order promotion.
func TestPartialDelivery(t *testing.T) {
	h := newHarness(t)
	if h.boardPending() != 6 {
		t.Fatalf("initial pending = %d, want 6", h.boardPending())
	}

	if code, _ := h.post("/api/pendientes/items/"+h.itemID+"/entregar", `{"qty":2}`); code != 200 {
		t.Fatalf("entregar 2: code %d", code)
	}
	if h.boardPending() != 4 {
		t.Fatalf("after 2: pending = %d, want 4", h.boardPending())
	}

	if code, _ := h.post("/api/pendientes/items/"+h.itemID+"/entregar", `{"qty":2}`); code != 200 {
		t.Fatalf("entregar 2 again: code %d", code)
	}
	if h.boardPending() != 2 {
		t.Fatalf("after 4: pending = %d, want 2", h.boardPending())
	}
	if h.orderStatus() != "preparing" {
		t.Fatalf("order should still be pending, got %q", h.orderStatus())
	}

	// Deliver the rest in one tap.
	if code, out := h.post("/api/pendientes/items/"+h.itemID+"/entregar-todo", ""); code != 200 || out["fully"] != true {
		t.Fatalf("entregar-todo: code %d out %v", code, out)
	}
	if h.boardPending() != 0 {
		t.Fatalf("after all: pending = %d, want 0", h.boardPending())
	}
	if h.orderStatus() != "delivered" {
		t.Fatalf("order should be delivered, got %q", h.orderStatus())
	}
}

// TestEntregarClampsToRemaining verifies delivering more than remaining is
// clamped (forgiving) rather than over-delivering.
func TestEntregarClampsToRemaining(t *testing.T) {
	h := newHarness(t)
	if code, out := h.post("/api/pendientes/items/"+h.itemID+"/entregar", `{"qty":100}`); code != 200 || out["fully"] != true {
		t.Fatalf("over-deliver: code %d out %v", code, out)
	}
	if h.boardPending() != 0 {
		t.Fatalf("pending after clamp = %d, want 0", h.boardPending())
	}
	var delivered int64
	h.app.DB.Reader.QueryRow(`SELECT delivered_qty FROM order_items WHERE id=?`, h.itemID).Scan(&delivered)
	if delivered != 6 {
		t.Fatalf("delivered_qty = %d, want 6 (clamped to qty)", delivered)
	}
}

// TestDeliverGuards covers already-delivered (conflict) and unknown item (404).
func TestDeliverGuards(t *testing.T) {
	h := newHarness(t)
	h.post("/api/pendientes/items/"+h.itemID+"/entregar-todo", "")
	if code, _ := h.post("/api/pendientes/items/"+h.itemID+"/entregar", `{"qty":1}`); code != http.StatusConflict {
		t.Fatalf("deliver already-complete: code %d, want 409", code)
	}
	if code, _ := h.post("/api/pendientes/items/00000000-0000-0000-0000-000000000000/entregar", `{"qty":1}`); code != http.StatusNotFound {
		t.Fatalf("unknown item: code %d, want 404", code)
	}
}

var _ = core.ErrNotFound

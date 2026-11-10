// Package clientapi tests boot an in-process server with ONLY the clientapi
// module mounted, seed an active event + a pre-registered attendee directly via
// SQL, and drive the self-service flow over real HTTP through a cookie-jar
// client. They assert the security/isolation model (identity matching, client
// cookie gating) and the atomic money guarantees (exact debit, stock
// decrement, idempotency, no overspend under concurrency with -race).
package clientapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/backup"
	"eventpos/internal/bootstrap"
	"eventpos/internal/config"
	"eventpos/internal/core"
	"eventpos/internal/db"
	"eventpos/internal/realtime"
	"eventpos/internal/server"
	"eventpos/internal/writer"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	ts      *httptest.Server
	client  *http.Client
	d       *db.DB
	eventID string
}

// newHarness boots a server with only the clientapi module and an active event.
func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	d, err := db.Open(filepath.Join(t.TempDir(), "client.db"))
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
	srv := server.New(deps, fstest.MapFS{}, New(deps))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	h := &harness{ts: ts, client: &http.Client{Jar: jar}, d: d}
	h.eventID = h.seedActiveEvent(t)
	return h
}

func (h *harness) seedActiveEvent(t *testing.T) string {
	t.Helper()
	id := core.NewID()
	_, err := h.d.Writer.ExecContext(context.Background(),
		`INSERT INTO events (id, name, status, default_topup, split_policy, created_at)
		 VALUES (?, 'Fiesta', 'active', 50000, 'cascade', ?)`, id, core.NowMillis())
	if err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return id
}

// seedAttendee inserts a pre-registered attendee in the active event.
func (h *harness) seedAttendee(t *testing.T, name, doc, email, phone string, balance, noRedeem int64, status string) (id, qr string) {
	t.Helper()
	id = core.NewID()
	qr = "qr-" + id
	_, err := h.d.Writer.ExecContext(context.Background(),
		`INSERT INTO attendees
		 (id, event_id, qr_token, display_name, doc_id, email, phone, no_redeem, balance, balance_version, status, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,0,?,?)`,
		id, h.eventID, qr, name, doc, email, phone, noRedeem, balance, status, core.NowMillis())
	if err != nil {
		t.Fatalf("seed attendee: %v", err)
	}
	return id, qr
}

// seedProduct inserts an active product (track_stock=1) in the active event.
func (h *harness) seedProduct(t *testing.T, name string, price, stock int64) string {
	t.Helper()
	id := core.NewID()
	_, err := h.d.Writer.ExecContext(context.Background(),
		`INSERT INTO products
		 (id, event_id, name, price, cost, prep_station, track_stock, stock_qty, stock_version, active, created_at)
		 VALUES (?,?,?,?,0,'bar',1,?,0,1,?)`,
		id, h.eventID, name, price, stock, core.NowMillis())
	if err != nil {
		t.Fatalf("seed product: %v", err)
	}
	return id
}

func (h *harness) req(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func (h *harness) balance(t *testing.T, id string) int64 {
	t.Helper()
	var bal int64
	if err := h.d.Reader.QueryRowContext(context.Background(),
		`SELECT balance FROM attendees WHERE id = ?`, id).Scan(&bal); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return bal
}

func (h *harness) stock(t *testing.T, id string) int64 {
	t.Helper()
	var s int64
	if err := h.d.Reader.QueryRowContext(context.Background(),
		`SELECT stock_qty FROM products WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatalf("read stock: %v", err)
	}
	return s
}

// ---------------------------------------------------------------------------
// acceso: identity matching
// ---------------------------------------------------------------------------

func TestAccesoMatchByCedula(t *testing.T) {
	h := newHarness(t)
	h.seedAttendee(t, "Juan Perez", "123", "juan@x.com", "3001112222", 50000, 0, "active")

	// Case-insensitive + trimmed match by cédula.
	code, data := h.req(t, "POST", "/api/cliente/acceso", map[string]any{
		"nombre": " juan perez ", "correo": "JUAN@X.COM", "cedula": "123", "celular": "3001112222",
	})
	if code != http.StatusOK {
		t.Fatalf("acceso: status %d, body %s", code, data)
	}
	var me meResp
	json.Unmarshal(data, &me)
	if me.Nombre != "Juan Perez" || me.Balance != 50000 || me.NoRedeem {
		t.Fatalf("acceso resp: %+v", me)
	}
}

func TestAccesoMatchByQR(t *testing.T) {
	h := newHarness(t)
	_, qr := h.seedAttendee(t, "Ana Lopez", "456", "ana@x.com", "3003334444", 20000, 0, "active")

	code, data := h.req(t, "POST", "/api/cliente/acceso", map[string]any{
		"qr_token": qr, "nombre": "Ana Lopez", "correo": "ana@x.com", "cedula": "456", "celular": "3003334444",
	})
	if code != http.StatusOK {
		t.Fatalf("acceso by qr: status %d, body %s", code, data)
	}
}

func TestAccesoFieldMismatch(t *testing.T) {
	h := newHarness(t)
	h.seedAttendee(t, "Juan Perez", "123", "juan@x.com", "3001112222", 50000, 0, "active")

	base := map[string]any{"nombre": "Juan Perez", "correo": "juan@x.com", "cedula": "123", "celular": "3001112222"}
	for _, field := range []string{"nombre", "correo", "celular"} {
		body := map[string]any{}
		for k, v := range base {
			body[k] = v
		}
		body[field] = "WRONG"
		code, _ := h.req(t, "POST", "/api/cliente/acceso", body)
		if code != http.StatusForbidden {
			t.Fatalf("mismatch on %s: status %d, want 403", field, code)
		}
	}

	// Wrong cédula locates nothing -> still 403 (no leak).
	bad := map[string]any{"nombre": "Juan Perez", "correo": "juan@x.com", "cedula": "999", "celular": "3001112222"}
	if code, _ := h.req(t, "POST", "/api/cliente/acceso", bad); code != http.StatusForbidden {
		t.Fatalf("wrong cedula: status %d, want 403", code)
	}
}

func TestAccesoMissingStoredField(t *testing.T) {
	h := newHarness(t)
	// Stored email is NULL -> any acceso must 403.
	h.seedAttendee(t, "Sin Correo", "777", "", "3005556666", 10000, 0, "active")
	// doc_id present, email empty string stored as '' -> norm empty -> mismatch.
	code, _ := h.req(t, "POST", "/api/cliente/acceso", map[string]any{
		"nombre": "Sin Correo", "correo": "", "cedula": "777", "celular": "3005556666",
	})
	if code != http.StatusForbidden {
		t.Fatalf("missing stored email: status %d, want 403", code)
	}
}

func TestAccesoBlocked(t *testing.T) {
	h := newHarness(t)
	h.seedAttendee(t, "Bloqueado", "888", "b@x.com", "3007778888", 10000, 0, "blocked")
	code, _ := h.req(t, "POST", "/api/cliente/acceso", map[string]any{
		"nombre": "Bloqueado", "correo": "b@x.com", "cedula": "888", "celular": "3007778888",
	})
	if code != http.StatusForbidden {
		t.Fatalf("blocked attendee: status %d, want 403", code)
	}
}

func TestAccesoNoActiveEvent(t *testing.T) {
	h := newHarness(t)
	// Close the active event.
	if _, err := h.d.Writer.ExecContext(context.Background(),
		`UPDATE events SET status = 'closed' WHERE id = ?`, h.eventID); err != nil {
		t.Fatalf("close event: %v", err)
	}
	code, _ := h.req(t, "POST", "/api/cliente/acceso", map[string]any{
		"nombre": "x", "correo": "x", "cedula": "x", "celular": "x",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("no active event: status %d, want 400", code)
	}
}

// ---------------------------------------------------------------------------
// Cookie gating: menu/yo require the client cookie
// ---------------------------------------------------------------------------

func TestProtectedRoutesRequireCookie(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/api/cliente/menu", "/api/cliente/yo"} {
		code, _ := h.req(t, "GET", path, nil)
		if code != http.StatusUnauthorized {
			t.Fatalf("GET %s without cookie: status %d, want 401", path, code)
		}
	}
	code, _ := h.req(t, "POST", "/api/cliente/pedido", map[string]any{"items": []any{}})
	if code != http.StatusUnauthorized {
		t.Fatalf("POST pedido without cookie: status %d, want 401", code)
	}
}

func TestForgedCookieRejected(t *testing.T) {
	h := newHarness(t)
	id, _ := h.seedAttendee(t, "Juan Perez", "123", "juan@x.com", "3001112222", 50000, 0, "active")
	// A handcrafted cookie with a valid attendee id but a bogus signature.
	req, _ := http.NewRequest("GET", h.ts.URL+"/api/cliente/yo", nil)
	req.AddCookie(&http.Cookie{Name: "aeos_client", Value: id + "|9999999999999.deadbeef"})
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged cookie: status %d, want 401", resp.StatusCode)
	}
}

// accede logs the attendee in and leaves the cookie in the jar.
func (h *harness) accede(t *testing.T, name, doc, email, phone string) {
	t.Helper()
	code, data := h.req(t, "POST", "/api/cliente/acceso", map[string]any{
		"nombre": name, "correo": email, "cedula": doc, "celular": phone,
	})
	if code != http.StatusOK {
		t.Fatalf("accede: status %d, body %s", code, data)
	}
}

func TestMenuAndYo(t *testing.T) {
	h := newHarness(t)
	id, _ := h.seedAttendee(t, "Juan Perez", "123", "juan@x.com", "3001112222", 50000, 0, "active")
	h.seedProduct(t, "Cerveza", 8000, 10)
	h.accede(t, "Juan Perez", "123", "juan@x.com", "3001112222")

	var menu []menuItem
	code, data := h.req(t, "GET", "/api/cliente/menu", nil)
	if code != http.StatusOK {
		t.Fatalf("menu: status %d, body %s", code, data)
	}
	json.Unmarshal(data, &menu)
	if len(menu) != 1 || menu[0].Name != "Cerveza" || menu[0].Price != 8000 {
		t.Fatalf("menu: %+v", menu)
	}

	var me meResp
	code, data = h.req(t, "GET", "/api/cliente/yo", nil)
	if code != http.StatusOK {
		t.Fatalf("yo: status %d, body %s", code, data)
	}
	json.Unmarshal(data, &me)
	if me.Nombre != "Juan Perez" || me.Balance != h.balance(t, id) {
		t.Fatalf("yo: %+v", me)
	}
}

// ---------------------------------------------------------------------------
// pedido: atomic balance-only order
// ---------------------------------------------------------------------------

func TestPedidoDebitsAndDecrements(t *testing.T) {
	h := newHarness(t)
	id, _ := h.seedAttendee(t, "Juan Perez", "123", "juan@x.com", "3001112222", 50000, 0, "active")
	prod := h.seedProduct(t, "Cerveza", 8000, 10)
	h.accede(t, "Juan Perez", "123", "juan@x.com", "3001112222")

	var ord OrderOut
	code, data := h.req(t, "POST", "/api/cliente/pedido", map[string]any{
		"request_id": "p-1",
		"items":      []map[string]any{{"product_id": prod, "qty": 2}},
	})
	if code != http.StatusCreated {
		t.Fatalf("pedido: status %d, body %s", code, data)
	}
	json.Unmarshal(data, &ord)
	if ord.Source != "cliente" {
		t.Fatalf("source: got %q, want cliente", ord.Source)
	}
	if ord.Status != "preparing" {
		t.Fatalf("status: got %q, want preparing", ord.Status)
	}
	if ord.Total != 16000 || ord.PaidFromBalance != 16000 {
		t.Fatalf("totals: %+v", ord)
	}
	if bal := h.balance(t, id); bal != 34000 {
		t.Fatalf("balance after: got %d, want 34000", bal)
	}
	if s := h.stock(t, prod); s != 8 {
		t.Fatalf("stock after: got %d, want 8", s)
	}

	// Verify the order row carries source='cliente' and a balance payment exists.
	var src, method string
	var amt int64
	h.d.Reader.QueryRowContext(context.Background(),
		`SELECT source FROM orders WHERE id = ?`, ord.ID).Scan(&src)
	h.d.Reader.QueryRowContext(context.Background(),
		`SELECT method, amount FROM payments WHERE order_id = ?`, ord.ID).Scan(&method, &amt)
	if src != "cliente" || method != "balance" || amt != 16000 {
		t.Fatalf("db rows: src=%q method=%q amt=%d", src, method, amt)
	}
}

func TestPedidoInsufficientBalance(t *testing.T) {
	h := newHarness(t)
	h.seedAttendee(t, "Pobre", "123", "p@x.com", "3001112222", 5000, 0, "active")
	prod := h.seedProduct(t, "Cerveza", 8000, 10)
	h.accede(t, "Pobre", "123", "p@x.com", "3001112222")

	code, data := h.req(t, "POST", "/api/cliente/pedido", map[string]any{
		"request_id": "p-x", "items": []map[string]any{{"product_id": prod, "qty": 1}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("insufficient: status %d, body %s", code, data)
	}
	var e struct{ Error string }
	json.Unmarshal(data, &e)
	if e.Error != core.ErrInsufficientFunds.Error() {
		t.Fatalf("error: got %q, want %q", e.Error, core.ErrInsufficientFunds.Error())
	}
}

func TestPedidoNoRedeemRejected(t *testing.T) {
	h := newHarness(t)
	h.seedAttendee(t, "NoRedime", "123", "n@x.com", "3001112222", 50000, 1, "active")
	prod := h.seedProduct(t, "Cerveza", 8000, 10)
	h.accede(t, "NoRedime", "123", "n@x.com", "3001112222")

	code, _ := h.req(t, "POST", "/api/cliente/pedido", map[string]any{
		"request_id": "p-nr", "items": []map[string]any{{"product_id": prod, "qty": 1}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("no_redeem: status %d, want 400", code)
	}
}

func TestPedidoZeroBalanceRejected(t *testing.T) {
	h := newHarness(t)
	h.seedAttendee(t, "Cero", "123", "c@x.com", "3001112222", 0, 0, "active")
	prod := h.seedProduct(t, "Cerveza", 8000, 10)
	h.accede(t, "Cero", "123", "c@x.com", "3001112222")

	code, _ := h.req(t, "POST", "/api/cliente/pedido", map[string]any{
		"request_id": "p-0", "items": []map[string]any{{"product_id": prod, "qty": 1}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("zero balance: status %d, want 400", code)
	}
}

func TestPedidoIdempotent(t *testing.T) {
	h := newHarness(t)
	id, _ := h.seedAttendee(t, "Juan Perez", "123", "juan@x.com", "3001112222", 50000, 0, "active")
	prod := h.seedProduct(t, "Cerveza", 8000, 10)
	h.accede(t, "Juan Perez", "123", "juan@x.com", "3001112222")

	body := map[string]any{"request_id": "dup-1", "items": []map[string]any{{"product_id": prod, "qty": 1}}}

	var first OrderOut
	code, data := h.req(t, "POST", "/api/cliente/pedido", body)
	if code != http.StatusCreated {
		t.Fatalf("first: status %d, body %s", code, data)
	}
	json.Unmarshal(data, &first)
	balAfter := h.balance(t, id)

	var second OrderOut
	code, data = h.req(t, "POST", "/api/cliente/pedido", body)
	if code != http.StatusOK {
		t.Fatalf("replay: status %d, body %s", code, data)
	}
	json.Unmarshal(data, &second)
	if second.ID != first.ID {
		t.Fatalf("replay returned different order: %s vs %s", second.ID, first.ID)
	}
	if bal := h.balance(t, id); bal != balAfter {
		t.Fatalf("balance changed on replay: got %d, want %d", bal, balAfter)
	}
	if s := h.stock(t, prod); s != 9 {
		t.Fatalf("stock changed on replay: got %d, want 9", s)
	}
}

// ---------------------------------------------------------------------------
// Concurrency: many concurrent pedidos never overspend balance (-race)
// ---------------------------------------------------------------------------

func TestConcurrentPedidosNeverOverspend(t *testing.T) {
	h := newHarness(t)
	// Balance covers exactly 10 orders of 1000 each; stock is plentiful.
	id, _ := h.seedAttendee(t, "Juan Perez", "123", "juan@x.com", "3001112222", 10000, 0, "active")
	prod := h.seedProduct(t, "Cerveza", 1000, 1000)
	h.accede(t, "Juan Perez", "123", "juan@x.com", "3001112222")

	const n = 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	var created int
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct request_id so each is a genuine (non-idempotent) attempt.
			code, _ := h.req(t, "POST", "/api/cliente/pedido", map[string]any{
				"request_id": "c-" + core.NewID(),
				"items":      []map[string]any{{"product_id": prod, "qty": 1}},
			})
			if code == http.StatusCreated {
				mu.Lock()
				created++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if created != 10 {
		t.Fatalf("orders created: got %d, want 10 (balance must cap spend)", created)
	}
	if bal := h.balance(t, id); bal != 0 {
		t.Fatalf("final balance: got %d, want 0 (no overspend, no underspend)", bal)
	}

	// Ledger debits must sum to exactly the starting balance.
	var debited int64
	h.d.Reader.QueryRowContext(context.Background(),
		`SELECT COALESCE(-SUM(amount),0) FROM balance_ledger WHERE attendee_id = ? AND type = 'debit'`, id).Scan(&debited)
	if debited != 10000 {
		t.Fatalf("total debited: got %d, want 10000", debited)
	}
}

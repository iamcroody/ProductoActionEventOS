// Package clientapi is the self-service ordering backend for Action EventOS:
// the routes an attendee's phone talks to after scanning the table/event QR.
// It lets a pre-registered attendee identify themselves, browse the menu, and
// place an order paid ENTIRELY from their own prepaid balance — no cash, no
// operator, no management surface.
//
// # Security & isolation model
//
// This package is a hermetically separate authentication and authorization
// domain from the operator console. The two never share trust:
//
//   - Operators authenticate with package auth, which mints a server-side
//     session keyed by the `aeos_session` cookie. Those sessions are validated
//     only by auth.Service.Middleware, which this package never calls. A client
//     therefore can never satisfy any operator route (/api/* under the auth
//     middleware): they simply have no `aeos_session`.
//   - Clients authenticate here with a SEPARATE, stateless, HMAC-SHA256 signed
//     cookie named `aeos_client`. It carries only {attendee_id, expiry} and is
//     signed with a per-process random secret generated in New() via crypto/rand
//     (never persisted, rotates on restart). The clientOnly middleware is the
//     ONLY code that trusts this cookie, and it gates ONLY the /api/cliente/*
//     routes. It never inspects `aeos_session`, so an operator session is inert
//     here.
//   - Because the secret is process-local and the cookie payload is just an
//     attendee id, a forged or tampered cookie fails the constant-time HMAC
//     check (hmac.Equal) and is rejected. There is no client row in the users
//     table and no operator role is ever attached to a client request.
//
// Net effect: clients can reach self-service and nothing else; operators'
// cookies grant nothing here. The two cookie namespaces are disjoint.
//
// # Integrity
//
// The ordering path reuses the exact atomic guarantees of package sales: the
// whole order runs inside ONE writer.Do transaction (globally serialized single
// writer), product stock/price is re-read inside the tx, stock is decremented
// under a version+quantity guard, the balance is debited under a version+amount
// guard, the debit is ledgered with a UNIQUE ref_payment_id (structurally
// un-double-able), request_id gives idempotency, and money is integer pesos.
package clientapi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"eventpos/internal/app"
	"eventpos/internal/core"
	"eventpos/internal/httpx"
	"eventpos/internal/server"
)

const (
	// clientCookie is the self-service session cookie. It is wholly distinct
	// from the operator `aeos_session` cookie and is validated only here.
	clientCookie = "aeos_client"
	clientTTL    = 6 * time.Hour

	// systemUserID/systemUsername back a non-loginable placeholder operator used
	// only to satisfy orders.operator_id (NOT NULL REFERENCES users(id)) for
	// self-service orders. It is created with active=0 and an unusable pin_hash,
	// so it can never authenticate into the operator console.
	systemUserID   = "00000000-0000-7000-8000-00000000c11e"
	systemUsername = "cliente_self_service"
)

// pendientesTopic is the single SSE topic for delivery-pending items, shared
// with package sales so client orders appear on the same board.
const pendientesTopic = "pendientes"

// Module mounts the self-service (cliente) routes.
type Module struct {
	d      server.Deps
	secret []byte // per-process HMAC key for the aeos_client cookie
	limit  *loginLimiter
}

// New constructs the client module, generating a fresh random cookie-signing
// secret. The secret lives only in memory and rotates on every restart, so
// previously issued client cookies are invalidated across restarts.
func New(d server.Deps) *Module {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		// crypto/rand failing is catastrophic and unrecoverable; without a
		// secret no cookie can be trusted, so refuse to start.
		panic("clientapi: cannot generate cookie secret: " + err.Error())
	}
	return &Module{d: d, secret: secret, limit: newLoginLimiter()}
}

// loginLimiter throttles identity-guessing on /api/cliente/acceso: after too
// many failed attempts from one IP within a sliding window, further attempts
// are refused (429) until the window passes. In-memory, per process.
type loginLimiter struct {
	mu     sync.Mutex
	fails  map[string]*failWindow
	max    int
	window time.Duration
}

type failWindow struct {
	count int
	start time.Time
}

// maxTrackedIPs bounds the failure map. A long event behind many client IPs
// would otherwise accumulate one entry per distinct IP forever, since entries
// are only removed on that IP's successful login. Once the map grows past this,
// fail() opportunistically sweeps windows whose sliding window has elapsed.
const maxTrackedIPs = 1024

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{fails: map[string]*failWindow{}, max: 10, window: time.Minute}
}

// sweepExpiredLocked drops windows whose sliding window has already elapsed.
// Such windows are inert — blocked() treats an expired window as absent and
// fail() overwrites one in place — so removing them changes no observable
// behavior; it only reclaims memory. The caller must hold l.mu.
func (l *loginLimiter) sweepExpiredLocked(now time.Time) {
	for ip, w := range l.fails {
		if now.Sub(w.start) > l.window {
			delete(l.fails, ip)
		}
	}
}

// blocked reports whether ip has exceeded the failure budget for the window.
func (l *loginLimiter) blocked(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.fails[ip]
	if w == nil || now.Sub(w.start) > l.window {
		return false
	}
	return w.count >= l.max
}

// fail records a failed attempt; success clears the counter.
func (l *loginLimiter) fail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.fails[ip]
	if w == nil || now.Sub(w.start) > l.window {
		if len(l.fails) >= maxTrackedIPs {
			l.sweepExpiredLocked(now)
		}
		l.fails[ip] = &failWindow{count: 1, start: now}
		return
	}
	w.count++
}

func (l *loginLimiter) success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}

// Register wires the /api/cliente routes. acceso/salir are public (they are the
// login/logout); menu/yo/pedido require a valid client cookie via clientOnly.
func (m *Module) Register(mux *http.ServeMux, d server.Deps) {
	mux.Handle("POST /api/cliente/acceso", http.HandlerFunc(m.acceso))
	mux.Handle("POST /api/cliente/salir", http.HandlerFunc(m.salir))
	mux.Handle("GET /api/cliente/menu", m.clientOnly(http.HandlerFunc(m.menu)))
	mux.Handle("GET /api/cliente/yo", m.clientOnly(http.HandlerFunc(m.yo)))
	mux.Handle("POST /api/cliente/pedido", m.clientOnly(http.HandlerFunc(m.pedido)))
}

// ---------------------------------------------------------------------------
// Client session: stateless HMAC-signed cookie (no DB table)
// ---------------------------------------------------------------------------

type ctxKey struct{}

// attendeeFromContext returns the authenticated client's attendee id.
func attendeeFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKey{}).(string)
	return id, ok
}

// sign produces "<b64 payload>.<b64 hmac>" where payload is "attendeeID|expMs".
func (m *Module) sign(attendeeID string, expMs int64) string {
	payload := attendeeID + "|" + strconv.FormatInt(expMs, 10)
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verify checks the signature and expiry, returning the attendee id.
func (m *Module) verify(value string) (string, bool) {
	dot := strings.LastIndexByte(value, '.')
	if dot < 0 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(value[:dot])
	if err != nil {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(value[dot+1:])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, m.secret)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", false
	}
	bar := strings.LastIndexByte(string(payload), '|')
	if bar < 0 {
		return "", false
	}
	exp, err := strconv.ParseInt(string(payload)[bar+1:], 10, 64)
	if err != nil || core.NowMillis() > exp {
		return "", false
	}
	id := string(payload)[:bar]
	if id == "" {
		return "", false
	}
	return id, true
}

func (m *Module) setCookie(w http.ResponseWriter, attendeeID string) {
	exp := time.Now().Add(clientTTL).UTC().UnixMilli()
	http.SetCookie(w, &http.Cookie{
		Name:     clientCookie,
		Value:    m.sign(attendeeID, exp),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(clientTTL.Seconds()),
	})
}

func (m *Module) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: clientCookie, Value: "", Path: "/", MaxAge: -1})
}

// clientOnly validates the aeos_client cookie and injects the attendee id. It
// deliberately ignores operator sessions: an `aeos_session` cookie carries no
// weight here, so operators cannot reach self-service routes through it.
func (m *Module) clientOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(clientCookie)
		if err != nil {
			httpx.JSON(w, http.StatusUnauthorized, map[string]string{"error": "no autenticado"})
			return
		}
		attendeeID, ok := m.verify(c.Value)
		if !ok {
			httpx.JSON(w, http.StatusUnauthorized, map[string]string{"error": "no autenticado"})
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, attendeeID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ---------------------------------------------------------------------------
// POST /api/cliente/acceso — identity validation + session issue
// ---------------------------------------------------------------------------

type accesoReq struct {
	QRToken string `json:"qr_token"`
	Nombre  string `json:"nombre"`
	Correo  string `json:"correo"`
	Cedula  string `json:"cedula"`
	Celular string `json:"celular"`
}

type meResp struct {
	Nombre   string `json:"nombre"`
	Balance  int64  `json:"balance"`
	NoRedeem bool   `json:"no_redeem"`
}

func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func (m *Module) acceso(w http.ResponseWriter, r *http.Request) {
	var in accesoReq
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}

	// Throttle identity-guessing: too many failures from one IP → 429.
	ip := httpx.ClientIP(r)
	now := time.Now()
	if m.limit.blocked(ip, now) {
		httpx.JSON(w, http.StatusTooManyRequests,
			map[string]string{"error": "demasiados intentos, espera un momento"})
		return
	}
	// deny records a failed attempt and returns the non-leaking 403.
	deny := func() {
		m.limit.fail(ip, now)
		httpx.JSON(w, http.StatusForbidden, map[string]string{"error": "datos no coinciden con el registro"})
	}

	// Active event (single source of truth for which registry to match against).
	var eventID string
	err := m.d.App.DB.Reader.QueryRowContext(r.Context(),
		`SELECT id FROM events WHERE status = 'active' LIMIT 1`).Scan(&eventID)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Error(w, core.ErrNoActiveEvent)
		return
	}
	if err != nil {
		httpx.Error(w, err)
		return
	}

	// Locate the pre-registered attendee: by qr_token if given, else by cédula.
	var (
		id, displayName, status string
		docID, email, phone     sql.NullString
		balance                 int64
		noRedeem                int64
		row                     *sql.Row
	)
	if strings.TrimSpace(in.QRToken) != "" {
		row = m.d.App.DB.Reader.QueryRowContext(r.Context(),
			`SELECT id, display_name, doc_id, email, phone, balance, no_redeem, status
			 FROM attendees WHERE qr_token = ? AND event_id = ?`, strings.TrimSpace(in.QRToken), eventID)
	} else if strings.TrimSpace(in.Cedula) != "" {
		row = m.d.App.DB.Reader.QueryRowContext(r.Context(),
			`SELECT id, display_name, doc_id, email, phone, balance, no_redeem, status
			 FROM attendees WHERE doc_id = ? AND event_id = ? LIMIT 1`, strings.TrimSpace(in.Cedula), eventID)
	} else {
		// Nothing to locate by.
		httpx.Error(w, core.ErrForbidden)
		return
	}
	if err := row.Scan(&id, &displayName, &docID, &email, &phone, &balance, &noRedeem, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Do not leak existence: a missing registration is the same 403 as a
			// mismatch.
			deny()
			return
		}
		httpx.Error(w, err)
		return
	}

	if status == "blocked" {
		deny()
		return
	}

	// Every identity field must be present in the stored record AND match the
	// submitted value (case-insensitive, trimmed). A missing stored field is a
	// mismatch.
	if !docID.Valid || !email.Valid || !phone.Valid ||
		norm(displayName) == "" || norm(docID.String) == "" || norm(email.String) == "" || norm(phone.String) == "" ||
		norm(in.Nombre) != norm(displayName) ||
		norm(in.Correo) != norm(email.String) ||
		norm(in.Cedula) != norm(docID.String) ||
		norm(in.Celular) != norm(phone.String) {
		deny()
		return
	}

	m.limit.success(ip)
	m.setCookie(w, id)
	httpx.JSON(w, http.StatusOK, meResp{Nombre: displayName, Balance: balance, NoRedeem: noRedeem == 1})
}

// ---------------------------------------------------------------------------
// POST /api/cliente/salir — clear the session cookie
// ---------------------------------------------------------------------------

func (m *Module) salir(w http.ResponseWriter, r *http.Request) {
	m.clearCookie(w)
	httpx.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------------------
// GET /api/cliente/yo — current client identity + balance
// ---------------------------------------------------------------------------

func (m *Module) yo(w http.ResponseWriter, r *http.Request) {
	attendeeID, _ := attendeeFromContext(r.Context())
	var (
		name     string
		balance  int64
		noRedeem int64
	)
	err := m.d.App.DB.Reader.QueryRowContext(r.Context(),
		`SELECT display_name, balance, no_redeem FROM attendees WHERE id = ?`, attendeeID).
		Scan(&name, &balance, &noRedeem)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Error(w, core.ErrNotFound)
		return
	}
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, meResp{Nombre: name, Balance: balance, NoRedeem: noRedeem == 1})
}

// ---------------------------------------------------------------------------
// GET /api/cliente/menu — active products of the active event (no costs)
// ---------------------------------------------------------------------------

type menuItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Price    int64  `json:"price"`
	Category string `json:"category"`
}

func (m *Module) menu(w http.ResponseWriter, r *http.Request) {
	var eventID string
	err := m.d.App.DB.Reader.QueryRowContext(r.Context(),
		`SELECT id FROM events WHERE status = 'active' LIMIT 1`).Scan(&eventID)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Error(w, core.ErrNoActiveEvent)
		return
	}
	if err != nil {
		httpx.Error(w, err)
		return
	}

	rows, err := m.d.App.DB.Reader.QueryContext(r.Context(),
		`SELECT p.id, p.name, p.price, COALESCE(c.name, '')
		 FROM products p LEFT JOIN categories c ON c.id = p.category_id
		 WHERE p.event_id = ? AND p.active = 1
		 ORDER BY p.sort_order, p.name`, eventID)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	defer rows.Close()

	out := []menuItem{}
	for rows.Next() {
		var it menuItem
		if err := rows.Scan(&it.ID, &it.Name, &it.Price, &it.Category); err != nil {
			httpx.Error(w, err)
			return
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// POST /api/cliente/pedido — atomic self-service order paid from balance
// ---------------------------------------------------------------------------

type pedidoReq struct {
	RequestID string `json:"request_id"`
	Items     []struct {
		ProductID string `json:"product_id"`
		Qty       int64  `json:"qty"`
	} `json:"items"`
}

// ItemOut is one order line in the pedido response.
type ItemOut struct {
	ID        string `json:"id"`
	ProductID string `json:"product_id"`
	Name      string `json:"name"`
	Qty       int64  `json:"qty"`
	UnitPrice int64  `json:"unit_price"`
	LineTotal int64  `json:"line_total"`
}

// OrderOut is the self-service order projection returned by pedido.
type OrderOut struct {
	ID              string    `json:"id"`
	Status          string    `json:"status"`
	Source          string    `json:"source"`
	Total           int64     `json:"total"`
	PaidFromBalance int64     `json:"paid_from_balance"`
	Balance         int64     `json:"balance"` // attendee balance after this order
	CreatedAt       int64     `json:"created_at"`
	Items           []ItemOut `json:"items"`
}

// lineSnap is a server-side priced order line (mirrors sales.lineSnap).
type lineSnap struct {
	itemID      string
	productID   string
	name        string
	qty         int64
	unitPrice   int64
	unitCost    int64
	lineTotal   int64
	prepStation string
	trackStock  int64
	stockQty    int64
	stockVer    int64
}

type pedidoResult struct {
	order      OrderOut
	ticket     string
	idempotent bool
}

func (m *Module) pedido(w http.ResponseWriter, r *http.Request) {
	attendeeID, _ := attendeeFromContext(r.Context())
	var in pedidoReq
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}
	if len(in.Items) == 0 {
		httpx.Error(w, core.ErrValidation)
		return
	}
	ip := httpx.ClientIP(r)

	out, err := m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		return processClientSale(r.Context(), tx, attendeeID, ip, in)
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	res := out.(*pedidoResult)

	// AFTER COMMIT: publish the pendientes ticket (skipped on idempotent replay
	// so a retried request never re-fires a ticket).
	if !res.idempotent && res.ticket != "" {
		m.d.App.Hub.Publish(pendientesTopic, "ticket", res.ticket)
	}
	status := http.StatusCreated
	if res.idempotent {
		status = http.StatusOK
	}
	httpx.JSON(w, status, res.order)
}

// processClientSale runs the whole self-service order inside the writer tx,
// mirroring sales.processSale but with a single balance payer (the client) and
// no cash path: the order is paid entirely from balance or rejected.
func processClientSale(ctx context.Context, tx *sql.Tx, attendeeID, ip string, in pedidoReq) (*pedidoResult, error) {
	// 1. IDEMPOTENCY: a repeated request_id returns the prior order untouched —
	// but ONLY if that order belongs to the SAME attendee. A request_id that
	// maps to someone else's order is rejected (409), never returned, so a
	// guessed/foreign key can't read another client's order lines (no IDOR).
	if in.RequestID != "" {
		var existingID, owner string
		err := tx.QueryRowContext(ctx, `
			SELECT o.id, COALESCE((SELECT p.attendee_id FROM payments p
			        WHERE p.order_id = o.id AND p.attendee_id IS NOT NULL LIMIT 1), '')
			FROM orders o WHERE o.request_id = ?`, in.RequestID).Scan(&existingID, &owner)
		if err == nil {
			if owner != attendeeID {
				return nil, core.ErrConflict
			}
			o, lerr := loadOrder(ctx, tx, existingID)
			if lerr != nil {
				return nil, lerr
			}
			return &pedidoResult{order: o, idempotent: true}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}

	// Active event, read inside the tx for a consistent snapshot.
	var eventID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM events WHERE status = 'active' LIMIT 1`).Scan(&eventID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrNoActiveEvent
	}
	if err != nil {
		return nil, err
	}

	// Re-read the attendee inside the tx; reject blocked / wrong-event.
	var (
		balance, balanceVer, noRedeem int64
		status, attEvent              string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT balance, balance_version, no_redeem, status, event_id FROM attendees WHERE id = ?`, attendeeID).
		Scan(&balance, &balanceVer, &noRedeem, &status, &attEvent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if status == "blocked" {
		return nil, core.ErrForbidden
	}
	if attEvent != eventID {
		// The client's registration belongs to a different (inactive) event.
		return nil, core.ErrForbidden
	}
	// no_redeem attendees hold no redeemable money — they can never self-order.
	if noRedeem == 1 || balance <= 0 {
		return nil, core.ErrInsufficientFunds
	}

	// Aggregate requested quantities per product so a product referenced twice is
	// decremented once with a single correct guard.
	type agg struct {
		productID string
		qty       int64
	}
	idx := map[string]int{}
	var aggs []agg
	for _, it := range in.Items {
		if it.ProductID == "" || it.Qty <= 0 || it.Qty > 100000 {
			return nil, core.ErrValidation
		}
		if j, ok := idx[it.ProductID]; ok {
			aggs[j].qty += it.Qty
		} else {
			idx[it.ProductID] = len(aggs)
			aggs = append(aggs, agg{it.ProductID, it.Qty})
		}
	}

	// Re-read each product inside the tx, validate stock, snapshot server price.
	var lines []lineSnap
	var subtotal int64
	for _, a := range aggs {
		var ls lineSnap
		ls.productID = a.productID
		ls.qty = a.qty
		err := tx.QueryRowContext(ctx,
			`SELECT name, price, cost, prep_station, track_stock, stock_qty, stock_version
			 FROM products WHERE id = ? AND event_id = ? AND active = 1`, a.productID, eventID).
			Scan(&ls.name, &ls.unitPrice, &ls.unitCost, &ls.prepStation, &ls.trackStock, &ls.stockQty, &ls.stockVer)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, core.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if ls.trackStock == 1 && ls.stockQty < ls.qty {
			return nil, core.ErrInsufficientStock
		}
		// Overflow guard (defense in depth; prices are capped at create time).
		if ls.unitPrice < 0 || (ls.qty != 0 && ls.unitPrice > math.MaxInt64/ls.qty) {
			return nil, core.ErrValidation
		}
		ls.lineTotal = ls.unitPrice * ls.qty // SERVER-SIDE price only
		if subtotal > math.MaxInt64-ls.lineTotal {
			return nil, core.ErrValidation
		}
		ls.itemID = core.NewID()
		subtotal += ls.lineTotal
		lines = append(lines, ls)
	}
	total := subtotal

	// Self-service is balance-only: no cash leg exists, so the balance must cover
	// the full total.
	if balance < total {
		return nil, core.ErrInsufficientFunds
	}

	now := core.NowMillis()
	orderID := core.NewID()

	// Ensure the non-loginable system operator exists to satisfy the NOT NULL FK
	// orders.operator_id. active=0 + unusable pin_hash means it can never log in.
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO users (id, username, full_name, role, pin_hash, active, created_at)
		 VALUES (?,?,?,?,?,0,?)`,
		systemUserID, systemUsername, "Cliente (autoservicio)", "caja", "x", now); err != nil {
		return nil, err
	}

	// INSERT order — source='cliente', status='preparing' (has pending deliveries).
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO orders
		 (id, event_id, session_id, operator_id, request_id, source, status, table_label,
		  subtotal, total, paid_from_balance, paid_cash, is_split, created_at, paid_at)
		 VALUES (?,?,NULL,?,?, 'cliente', 'preparing', NULL, ?,?,?,0,0,?,?)`,
		orderID, eventID, systemUserID, nz(in.RequestID), subtotal, total, total, now, now); err != nil {
		return nil, err
	}

	// INSERT order_items with name + price snapshot.
	for _, ls := range lines {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO order_items
			 (id, order_id, product_id, name, qty, unit_price, unit_cost, line_total, prep_station, prep_status)
			 VALUES (?,?,?,?,?,?,?,?,?, 'queued')`,
			ls.itemID, orderID, ls.productID, ls.name, ls.qty, ls.unitPrice, ls.unitCost, ls.lineTotal, ls.prepStation); err != nil {
			return nil, err
		}
	}

	// DECREMENT stock under a version+quantity guard; append a stock movement.
	for _, ls := range lines {
		if ls.trackStock != 1 {
			continue
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE products SET stock_qty = stock_qty - ?, stock_version = stock_version + 1
			 WHERE id = ? AND stock_version = ? AND stock_qty >= ?`,
			ls.qty, ls.productID, ls.stockVer, ls.qty)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil, core.ErrConflict
		}
		newStock := ls.stockQty - ls.qty
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO stock_movements
			 (id, product_id, delta, reason, ref_order_id, balance_after, user_id, created_at)
			 VALUES (?,?,?, 'sale', ?,?,?,?)`,
			core.NewID(), ls.productID, -ls.qty, orderID, newStock, systemUserID, now); err != nil {
			return nil, err
		}
	}

	// PAYMENT (method='balance') + version-guarded debit + ledger (UNIQUE ref_payment_id).
	paymentID := core.NewID()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO payments (id, order_id, attendee_id, method, amount, created_at, user_id)
		 VALUES (?,?,?, 'balance', ?,?,?)`,
		paymentID, orderID, attendeeID, total, now, systemUserID); err != nil {
		return nil, err
	}
	newBalance := balance - total
	res, err := tx.ExecContext(ctx,
		`UPDATE attendees SET balance = balance - ?, balance_version = balance_version + 1
		 WHERE id = ? AND balance_version = ? AND balance >= ?`,
		total, attendeeID, balanceVer, total)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// Lost the version race (a concurrent debit moved the balance/version):
		// abort so the writer retries against fresh state on the client's retry.
		return nil, core.ErrConflict
	}
	// UNIQUE(ref_payment_id) makes this debit structurally un-double-able.
	if _, err := app.AppendLedger(tx, attendeeID, "debit", -total, newBalance, orderID, paymentID, systemUserID); err != nil {
		return nil, err
	}

	// AUDIT: action='sale', detail carries "source":"cliente".
	if err := writeClientAudit(tx, eventID, attendeeID, ip, orderID, lines, total, balance, newBalance); err != nil {
		return nil, err
	}

	// Build response + pendientes ticket.
	o, err := loadOrder(ctx, tx, orderID)
	if err != nil {
		return nil, err
	}
	o.Balance = newBalance
	return &pedidoResult{order: o, ticket: buildTicket(orderID, now, lines)}, nil
}

// writeClientAudit appends the 'sale' audit row with a product snapshot and a
// money breakdown that records the self-service source.
func writeClientAudit(tx *sql.Tx, eventID, attendeeID, ip, orderID string, lines []lineSnap, total, before, after int64) error {
	type prodSnap struct {
		ProductID string `json:"product_id"`
		Name      string `json:"name"`
		Qty       int64  `json:"qty"`
		UnitPrice int64  `json:"unit_price"`
		LineTotal int64  `json:"line_total"`
	}
	prods := make([]prodSnap, 0, len(lines))
	for _, ls := range lines {
		prods = append(prods, prodSnap{ls.productID, ls.name, ls.qty, ls.unitPrice, ls.lineTotal})
	}
	productsJSON, _ := json.Marshal(prods)
	detail, _ := json.Marshal(map[string]any{
		"order_id":          orderID,
		"source":            "cliente",
		"total":             total,
		"paid_from_balance": total,
		"paid_cash":         0,
	})
	return app.WriteAudit(tx, app.AuditEntry{
		EventID: eventID, UserID: systemUserID, AttendeeID: attendeeID, IP: ip,
		Action: "sale", Entity: "order", EntityID: orderID,
		ProductsJSON: string(productsJSON), DetailJSON: string(detail),
		BalanceBefore: &before, BalanceAfter: &after,
	})
}

// buildTicket produces the single pendientes ticket carrying every item, with
// the same JSON shape sales uses (operator is the literal "Cliente").
func buildTicket(orderID string, now int64, lines []lineSnap) string {
	type tItem struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Qty  int64  `json:"qty"`
	}
	items := make([]tItem, 0, len(lines))
	for _, ls := range lines {
		items = append(items, tItem{ls.itemID, ls.name, ls.qty})
	}
	if len(items) == 0 {
		return ""
	}
	data, _ := json.Marshal(map[string]any{
		"order_id":    orderID,
		"table_label": "",
		"operator":    "Cliente",
		"items":       items,
		"created_at":  now,
	})
	return string(data)
}

// loadOrder reads a self-service order with its items. Works against the writer
// tx (for the creation response) or the reader pool.
func loadOrder(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, id string) (OrderOut, error) {
	var o OrderOut
	err := q.QueryRowContext(ctx,
		`SELECT id, status, source, total, paid_from_balance, created_at
		 FROM orders WHERE id = ?`, id).
		Scan(&o.ID, &o.Status, &o.Source, &o.Total, &o.PaidFromBalance, &o.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OrderOut{}, core.ErrNotFound
	}
	if err != nil {
		return OrderOut{}, err
	}
	o.Items = []ItemOut{}
	rows, err := q.QueryContext(ctx,
		`SELECT id, product_id, name, qty, unit_price, line_total
		 FROM order_items WHERE order_id = ? ORDER BY id`, id)
	if err != nil {
		return OrderOut{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var it ItemOut
		if err := rows.Scan(&it.ID, &it.ProductID, &it.Name, &it.Qty, &it.UnitPrice, &it.LineTotal); err != nil {
			return OrderOut{}, err
		}
		o.Items = append(o.Items, it)
	}
	if err := rows.Err(); err != nil {
		return OrderOut{}, err
	}
	return o, nil
}

// nz maps an empty string to a SQL NULL, otherwise the string itself.
func nz(s string) any {
	if s == "" {
		return nil
	}
	return s
}

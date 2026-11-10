// Package sales is the atomic prepaid-sale engine of Action EventOS — the
// Ventas/Consumos core. A sale debits attendee balances, decrements inventory,
// records immutable payments + ledger + audit rows, and (optionally) fires
// kitchen/bar tickets. Correctness under concurrency is the whole point.
//
// Every guarantee rests on a single rule: the ENTIRE sale runs inside ONE
// m.d.App.Writer.Do(...) transaction. The writer is a single goroutine bound to
// a single SQLite connection opened with _txlock=immediate, so all mutations are
// globally serialized. Re-reading stock/balance inside that serialized tx makes
// the last-unit and double-charge races physically impossible, and the version
// guards + UNIQUE(ref_payment_id) + CHECK constraints are belt-and-suspenders on
// top of that.
package sales

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/core"
	"eventpos/internal/httpx"
	"eventpos/internal/server"
)

// Module mounts the sales (orders) routes.
type Module struct{ d server.Deps }

// New constructs the sales module.
func New(d server.Deps) *Module { return &Module{d: d} }

// Register wires the order routes. Selling is allowed for admin/caja/mesero;
// listing for admin/caja/auditor; void is admin-only.
func (m *Module) Register(mux *http.ServeMux, d server.Deps) {
	sell := d.Auth.Middleware("admin", "caja", "mesero")
	read := d.Auth.Middleware("admin", "caja", "auditor")
	any := d.Auth.Middleware()
	admin := d.Auth.Middleware("admin")

	mux.Handle("POST /api/orders", sell(http.HandlerFunc(m.create)))
	mux.Handle("GET /api/orders", read(http.HandlerFunc(m.list)))
	mux.Handle("GET /api/orders/{id}", any(http.HandlerFunc(m.detail)))
	mux.Handle("POST /api/orders/{id}/void", admin(http.HandlerFunc(m.void)))
}

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

type createReq struct {
	RequestID  string `json:"request_id"`
	TableLabel string `json:"table_label"`
	Source     string `json:"source"` // operator|mesero|cliente; defaults to operator
	Items      []struct {
		ProductID string `json:"product_id"`
		Qty       int64  `json:"qty"`
	} `json:"items"`
	Payers []struct {
		AttendeeID string `json:"attendee_id"`
		QRToken    string `json:"qr_token"`
	} `json:"payers"`
	CashConfirmed bool `json:"cash_confirmed"`
}

// ItemOut is one order line in API responses.
type ItemOut struct {
	ID          string `json:"id"`
	ProductID   string `json:"product_id"`
	Name        string `json:"name"`
	Qty         int64  `json:"qty"`
	UnitPrice   int64  `json:"unit_price"`
	LineTotal   int64  `json:"line_total"`
	PrepStation string `json:"prep_station"`
	PrepStatus  string `json:"prep_status"`
}

// PaymentOut is one payment row in API responses.
type PaymentOut struct {
	ID         string  `json:"id"`
	AttendeeID *string `json:"attendee_id"`
	Method     string  `json:"method"`
	Amount     int64   `json:"amount"`
}

// SplitOut is one per-payer split row in API responses.
type SplitOut struct {
	AttendeeID      string `json:"attendee_id"`
	ShareAmount     int64  `json:"share_amount"`
	PaidFromBalance int64  `json:"paid_from_balance"`
	CashCollected   int64  `json:"cash_collected"`
}

// OrderSummary is the full order projection returned by every endpoint.
type OrderSummary struct {
	ID              string       `json:"id"`
	EventID         string       `json:"event_id"`
	Status          string       `json:"status"`
	TableLabel      string       `json:"table_label,omitempty"`
	Subtotal        int64        `json:"subtotal"`
	Total           int64        `json:"total"`
	PaidFromBalance int64        `json:"paid_from_balance"`
	PaidCash        int64        `json:"paid_cash"`
	IsSplit         bool         `json:"is_split"`
	CreatedAt       int64        `json:"created_at"`
	PaidAt          *int64       `json:"paid_at,omitempty"`
	VoidedAt        *int64       `json:"voided_at,omitempty"`
	Items           []ItemOut    `json:"items"`
	Payments        []PaymentOut `json:"payments"`
	Splits          []SplitOut   `json:"splits"`
}

// ---------------------------------------------------------------------------
// Internal sale-time structures
// ---------------------------------------------------------------------------

// lineSnap is a server-side priced order line.
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

// payerAlloc is the per-payer money allocation for a sale.
type payerAlloc struct {
	attendeeID string
	name       string
	balance    int64 // balance before this sale
	balanceVer int64
	share      int64 // assigned share of total
	debit      int64 // amount actually charged to this attendee's balance
	cash       int64 // residual of this payer's share collected as cash
}

// stationTicket is a kitchen/bar ticket queued for publication after commit.
type stationTicket struct {
	station string
	data    string
}

// saleResult is what the writer tx returns to the handler.
type saleResult struct {
	summary    OrderSummary
	tickets    []stationTicket
	idempotent bool
}

// queryer is satisfied by both *sql.Tx and *sql.DB, so loadOrder works inside
// the writer tx (for the creation response) and against the reader pool.
type queryer interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

// ---------------------------------------------------------------------------
// POST /api/orders
// ---------------------------------------------------------------------------

func (m *Module) create(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	var in createReq
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}
	if len(in.Items) == 0 || len(in.Payers) == 0 {
		httpx.Error(w, core.ErrValidation)
		return
	}
	ip := httpx.ClientIP(r)
	operator := u.FullName
	if operator == "" {
		operator = u.Username
	}

	// The whole sale is one atomic, globally-serialized transaction.
	out, err := m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		return processSale(r.Context(), tx, u, ip, operator, in)
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	res := out.(*saleResult)

	// AFTER COMMIT: publish kitchen/bar tickets. Skipped on idempotent replay so
	// a retried request never re-fires tickets.
	if !res.idempotent {
		for _, t := range res.tickets {
			m.d.App.Hub.Publish(t.station, "ticket", t.data)
		}
	}
	status := http.StatusCreated
	if res.idempotent {
		status = http.StatusOK
	}
	httpx.JSON(w, status, res.summary)
}

// processSale runs the full ALGORITHM inside the writer tx.
func processSale(ctx context.Context, tx *sql.Tx, u auth.User, ip, operator string, in createReq) (*saleResult, error) {
	// 1. IDEMPOTENCY: a repeated request_id returns the prior order untouched.
	if in.RequestID != "" {
		var existingID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM orders WHERE request_id = ?`, in.RequestID).Scan(&existingID)
		if err == nil {
			sum, lerr := loadOrder(ctx, tx, existingID)
			if lerr != nil {
				return nil, lerr
			}
			return &saleResult{summary: sum, idempotent: true}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}

	// Active event (read inside the tx for a consistent snapshot).
	var eventID, splitPolicy string
	err := tx.QueryRowContext(ctx, `SELECT id, split_policy FROM events WHERE status = 'active' LIMIT 1`).
		Scan(&eventID, &splitPolicy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrNoActiveEvent
	}
	if err != nil {
		return nil, err
	}

	// 2. Resolve payers by attendee_id or qr_token; reject blocked/dupes.
	payers, err := resolvePayers(ctx, tx, eventID, in)
	if err != nil {
		return nil, err
	}

	// Aggregate requested quantities per product so a product referenced by two
	// lines is decremented once with a single, correct version guard.
	type agg struct {
		productID string
		qty       int64
	}
	idx := map[string]int{}
	var aggs []agg
	for _, it := range in.Items {
		// Upper bound on qty: prevents int64 overflow in unit_price*qty for
		// untracked products and rejects absurd input. 100000 units is far
		// beyond any real event line item.
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

	// 3 + 4 + 5. Re-read each product inside the tx, validate stock, snapshot price.
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
		// Overflow guards: a tampered/huge price must never wrap line_total or
		// subtotal negative (which would silently zero the charge). Prices are
		// capped at core.MaxMoney on create, so this is defense in depth.
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

	// 6. SPLIT ALLOCATION.
	cashDue := allocate(payers, total, splitPolicy)
	if cashDue > 0 && !in.CashConfirmed {
		return nil, core.ErrCashNotConfirmed
	}

	now := core.NowMillis()
	orderID := core.NewID()

	// 7. INSERT order. Every order starts with pending deliveries (items are
	// handed over from bodega, possibly partially), so status begins 'preparing'
	// ("tiene pendientes") and becomes 'delivered' once every unit is handed over.
	status := "preparing"
	isSplit := 0
	if len(payers) > 1 {
		isSplit = 1
	}
	var sessionID string
	_ = tx.QueryRowContext(ctx,
		`SELECT id FROM cash_sessions WHERE operator_id = ? AND status = 'open' LIMIT 1`, u.ID).
		Scan(&sessionID) // nullable; ErrNoRows leaves it empty

	paidFromBalance := total - cashDue
	source := in.Source
	if source != "mesero" && source != "cliente" {
		source = "operator"
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO orders
		 (id, event_id, session_id, operator_id, request_id, source, status, table_label,
		  subtotal, total, paid_from_balance, paid_cash, is_split, created_at, paid_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		orderID, eventID, nz(sessionID), u.ID, nz(in.RequestID), source, status, nz(in.TableLabel),
		subtotal, total, paidFromBalance, cashDue, isSplit, now, now); err != nil {
		return nil, err
	}

	// 8. INSERT order_items (with name + price snapshot + prep metadata).
	for _, ls := range lines {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO order_items
			 (id, order_id, product_id, name, qty, unit_price, unit_cost, line_total, prep_station, prep_status)
			 VALUES (?,?,?,?,?,?,?,?,?, 'queued')`,
			ls.itemID, orderID, ls.productID, ls.name, ls.qty, ls.unitPrice, ls.unitCost, ls.lineTotal, ls.prepStation); err != nil {
			return nil, err
		}
	}

	// 9. DECREMENT STOCK with a version+quantity guard; append a stock movement.
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
			core.NewID(), ls.productID, -ls.qty, orderID, newStock, u.ID, now); err != nil {
			return nil, err
		}
	}

	// 10. PAYMENTS + LEDGER per payer, then a single cash payment for any residual.
	for i := range payers {
		p := &payers[i]
		if p.debit > 0 {
			paymentID := core.NewID()
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO payments (id, order_id, attendee_id, method, amount, created_at, user_id)
				 VALUES (?,?,?, 'balance', ?,?,?)`,
				paymentID, orderID, p.attendeeID, p.debit, now, u.ID); err != nil {
				return nil, err
			}
			newBalance := p.balance - p.debit
			res, err := tx.ExecContext(ctx,
				`UPDATE attendees SET balance = balance - ?, balance_version = balance_version + 1
				 WHERE id = ? AND balance_version = ? AND balance >= ?`,
				p.debit, p.attendeeID, p.balanceVer, p.debit)
			if err != nil {
				return nil, err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return nil, core.ErrConflict
			}
			// UNIQUE(ref_payment_id) makes this debit structurally un-double-able.
			if _, err := app.AppendLedger(tx, p.attendeeID, "debit", -p.debit, newBalance, orderID, paymentID, u.ID); err != nil {
				return nil, err
			}
		}
		if isSplit == 1 {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO order_splits
				 (id, order_id, attendee_id, share_amount, paid_from_balance, cash_collected)
				 VALUES (?,?,?,?,?,?)`,
				core.NewID(), orderID, p.attendeeID, p.share, p.debit, p.cash); err != nil {
				return nil, err
			}
		}
	}
	if cashDue > 0 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO payments (id, order_id, attendee_id, method, amount, created_at, user_id)
			 VALUES (?,?, NULL, 'cash', ?,?,?)`,
			core.NewID(), orderID, cashDue, now, u.ID); err != nil {
			return nil, err
		}
	}

	// 11. AUDIT with product snapshot + per-payer breakdown.
	if err := writeSaleAudit(tx, eventID, u.ID, ip, orderID, lines, payers, total, paidFromBalance, cashDue, splitPolicy); err != nil {
		return nil, err
	}

	// 12. Build response + tickets.
	sum, err := loadOrder(ctx, tx, orderID)
	if err != nil {
		return nil, err
	}
	return &saleResult{summary: sum, tickets: buildTickets(orderID, in.TableLabel, operator, now, lines)}, nil
}

// resolvePayers resolves each payer by attendee_id (preferred) or qr_token, in
// the active event, rejecting blocked attendees and duplicates.
func resolvePayers(ctx context.Context, tx *sql.Tx, eventID string, in createReq) ([]payerAlloc, error) {
	seen := map[string]bool{}
	out := make([]payerAlloc, 0, len(in.Payers))
	for _, pr := range in.Payers {
		var p payerAlloc
		var status string
		var row *sql.Row
		switch {
		case pr.AttendeeID != "":
			row = tx.QueryRowContext(ctx,
				`SELECT id, display_name, balance, balance_version, status
				 FROM attendees WHERE id = ? AND event_id = ?`, pr.AttendeeID, eventID)
		case pr.QRToken != "":
			row = tx.QueryRowContext(ctx,
				`SELECT id, display_name, balance, balance_version, status
				 FROM attendees WHERE qr_token = ? AND event_id = ?`, pr.QRToken, eventID)
		default:
			return nil, core.ErrValidation
		}
		if err := row.Scan(&p.attendeeID, &p.name, &p.balance, &p.balanceVer, &status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, core.ErrNotFound
			}
			return nil, err
		}
		if status == "blocked" {
			return nil, core.ErrValidation
		}
		if seen[p.attendeeID] {
			return nil, core.ErrValidation // a payer may appear at most once
		}
		seen[p.attendeeID] = true
		out = append(out, p)
	}
	return out, nil
}

// allocate fills each payer's share/debit/cash and returns the order-level
// cash_due. Shares sum EXACTLY to total (remainder pesos go to the first
// payers). Each payer first spends their own balance; under the 'cascade'
// policy, payers with spare balance cover the others' shortfalls. The residual
// that no balance can cover is the cash due.
func allocate(payers []payerAlloc, total int64, policy string) int64 {
	n := int64(len(payers))
	if n == 0 {
		// Defense-in-depth: the HTTP handler already rejects empty payers, but
		// a zero divisor here would panic the single writer goroutine and crash
		// the whole process. Treat as the full amount due in cash.
		return total
	}
	base := total / n
	rem := total % n
	for i := range payers {
		share := base
		if int64(i) < rem {
			share++
		}
		payers[i].share = share
		own := min64(share, payers[i].balance)
		payers[i].debit = own        // grows if this payer lends to others
		payers[i].cash = share - own // shrinks as others' balance covers it
	}
	if policy != "strict_equal" {
		// Cascade: cover each shortfall from any payer with remaining balance.
		for j := range payers {
			for k := range payers {
				if payers[j].cash == 0 {
					break
				}
				if k == j {
					continue
				}
				avail := payers[k].balance - payers[k].debit
				if avail <= 0 {
					continue
				}
				take := min64(avail, payers[j].cash)
				payers[k].debit += take
				payers[j].cash -= take
			}
		}
	}
	var cashDue int64
	for i := range payers {
		cashDue += payers[i].cash
	}
	return cashDue
}

// writeSaleAudit appends the 'sale' audit row with a product snapshot and a full
// money breakdown. For a single payer it records balance_before/after.
func writeSaleAudit(tx *sql.Tx, eventID, userID, ip, orderID string, lines []lineSnap, payers []payerAlloc,
	total, paidFromBalance, cashDue int64, policy string) error {

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

	type payerSnap struct {
		AttendeeID  string `json:"attendee_id"`
		Share       int64  `json:"share"`
		FromBalance int64  `json:"from_balance"`
		Cash        int64  `json:"cash"`
	}
	pb := make([]payerSnap, 0, len(payers))
	for _, p := range payers {
		pb = append(pb, payerSnap{p.attendeeID, p.share, p.debit, p.cash})
	}
	detail, _ := json.Marshal(map[string]any{
		"order_id":          orderID,
		"total":             total,
		"paid_from_balance": paidFromBalance,
		"paid_cash":         cashDue,
		"split_policy":      policy,
		"payers":            pb,
	})

	e := app.AuditEntry{
		EventID: eventID, UserID: userID, IP: ip,
		Action: "sale", Entity: "order", EntityID: orderID,
		ProductsJSON: string(productsJSON), DetailJSON: string(detail),
	}
	if len(payers) == 1 {
		before := payers[0].balance
		after := payers[0].balance - payers[0].debit
		e.AttendeeID = payers[0].attendeeID
		e.BalanceBefore = &before
		e.BalanceAfter = &after
	}
	return app.WriteAudit(tx, e)
}

// pendientesTopic is the single SSE topic for delivery-pending items. There is
// no food preparation: every sold item is something to hand over from bodega,
// tracked on one "Pendientes" board (with partial delivery).
const pendientesTopic = "pendientes"

// buildTickets produces a single pendientes ticket carrying every item of the
// order (each item is a delivery to fulfill). Empty if the order has no items.
func buildTickets(orderID, table, operator string, now int64, lines []lineSnap) []stationTicket {
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
		return nil
	}
	data, _ := json.Marshal(map[string]any{
		"order_id":    orderID,
		"table_label": table,
		"operator":    operator,
		"items":       items,
		"created_at":  now,
	})
	return []stationTicket{{station: pendientesTopic, data: string(data)}}
}

// ---------------------------------------------------------------------------
// GET /api/orders  and  GET /api/orders/{id}
// ---------------------------------------------------------------------------

func (m *Module) list(w http.ResponseWriter, r *http.Request) {
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

	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	statusFilter := r.URL.Query().Get("status")

	q := `SELECT id FROM orders WHERE event_id = ?`
	args := []any{eventID}
	if statusFilter != "" {
		q += ` AND status = ?`
		args = append(args, statusFilter)
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := m.d.App.DB.Reader.QueryContext(r.Context(), q, args...)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			httpx.Error(w, err)
			return
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		httpx.Error(w, err)
		return
	}

	out := make([]OrderSummary, 0, len(ids))
	for _, id := range ids {
		o, err := loadOrder(r.Context(), m.d.App.DB.Reader, id)
		if err != nil {
			httpx.Error(w, err)
			return
		}
		out = append(out, o)
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (m *Module) detail(w http.ResponseWriter, r *http.Request) {
	o, err := loadOrder(r.Context(), m.d.App.DB.Reader, r.PathValue("id"))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, o)
}

// loadOrder reads an order with its items, payments and splits. It works against
// either the writer tx (for the creation response) or the reader pool.
func loadOrder(ctx context.Context, q queryer, id string) (OrderSummary, error) {
	var o OrderSummary
	var table sql.NullString
	var isSplit int
	var paidAt, voidedAt sql.NullInt64
	err := q.QueryRowContext(ctx,
		`SELECT id, event_id, status, table_label, subtotal, total,
		        paid_from_balance, paid_cash, is_split, created_at, paid_at, voided_at
		 FROM orders WHERE id = ?`, id).
		Scan(&o.ID, &o.EventID, &o.Status, &table, &o.Subtotal, &o.Total,
			&o.PaidFromBalance, &o.PaidCash, &isSplit, &o.CreatedAt, &paidAt, &voidedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OrderSummary{}, core.ErrNotFound
	}
	if err != nil {
		return OrderSummary{}, err
	}
	o.TableLabel = table.String
	o.IsSplit = isSplit == 1
	if paidAt.Valid {
		o.PaidAt = &paidAt.Int64
	}
	if voidedAt.Valid {
		o.VoidedAt = &voidedAt.Int64
	}

	o.Items = []ItemOut{}
	irows, err := q.QueryContext(ctx,
		`SELECT id, product_id, name, qty, unit_price, line_total, prep_station, prep_status
		 FROM order_items WHERE order_id = ? ORDER BY id`, id)
	if err != nil {
		return OrderSummary{}, err
	}
	defer irows.Close()
	for irows.Next() {
		var it ItemOut
		if err := irows.Scan(&it.ID, &it.ProductID, &it.Name, &it.Qty, &it.UnitPrice,
			&it.LineTotal, &it.PrepStation, &it.PrepStatus); err != nil {
			return OrderSummary{}, err
		}
		o.Items = append(o.Items, it)
	}
	if err := irows.Err(); err != nil {
		return OrderSummary{}, err
	}

	o.Payments = []PaymentOut{}
	prows, err := q.QueryContext(ctx,
		`SELECT id, attendee_id, method, amount FROM payments WHERE order_id = ? ORDER BY created_at, id`, id)
	if err != nil {
		return OrderSummary{}, err
	}
	defer prows.Close()
	for prows.Next() {
		var p PaymentOut
		var att sql.NullString
		if err := prows.Scan(&p.ID, &att, &p.Method, &p.Amount); err != nil {
			return OrderSummary{}, err
		}
		if att.Valid {
			v := att.String
			p.AttendeeID = &v
		}
		o.Payments = append(o.Payments, p)
	}
	if err := prows.Err(); err != nil {
		return OrderSummary{}, err
	}

	o.Splits = []SplitOut{}
	srows, err := q.QueryContext(ctx,
		`SELECT attendee_id, share_amount, paid_from_balance, cash_collected
		 FROM order_splits WHERE order_id = ? ORDER BY id`, id)
	if err != nil {
		return OrderSummary{}, err
	}
	defer srows.Close()
	for srows.Next() {
		var s SplitOut
		if err := srows.Scan(&s.AttendeeID, &s.ShareAmount, &s.PaidFromBalance, &s.CashCollected); err != nil {
			return OrderSummary{}, err
		}
		o.Splits = append(o.Splits, s)
	}
	if err := srows.Err(); err != nil {
		return OrderSummary{}, err
	}
	return o, nil
}

// ---------------------------------------------------------------------------
// POST /api/orders/{id}/void
// ---------------------------------------------------------------------------

func (m *Module) void(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	id := r.PathValue("id")
	ip := httpx.ClientIP(r)

	_, err := m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		return processVoid(r.Context(), tx, u, ip, id)
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// processVoid reverses an order: status->voided, inventory restored via
// compensating movements, balance payments refunded to the ledger. Immutable
// tables are append-only, so reversal is always new rows, never UPDATE/DELETE.
func processVoid(ctx context.Context, tx *sql.Tx, u auth.User, ip, id string) (any, error) {
	var eventID, status string
	err := tx.QueryRowContext(ctx, `SELECT event_id, status FROM orders WHERE id = ?`, id).Scan(&eventID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if status == "voided" {
		return nil, core.ErrValidation // already voided
	}

	now := core.NowMillis()
	if _, err := tx.ExecContext(ctx,
		`UPDATE orders SET status = 'voided', voided_at = ? WHERE id = ?`, now, id); err != nil {
		return nil, err
	}

	// REVERSE inventory: each original 'sale' movement gets a compensating
	// 'void' movement and restores products.stock_qty under a version guard.
	mrows, err := tx.QueryContext(ctx,
		`SELECT product_id, delta FROM stock_movements WHERE ref_order_id = ? AND reason = 'sale'`, id)
	if err != nil {
		return nil, err
	}
	type rev struct {
		productID string
		qty       int64 // positive units to give back
	}
	var revs []rev
	for mrows.Next() {
		var pid string
		var delta int64
		if err := mrows.Scan(&pid, &delta); err != nil {
			mrows.Close()
			return nil, err
		}
		revs = append(revs, rev{pid, -delta})
	}
	if err := mrows.Err(); err != nil {
		mrows.Close()
		return nil, err
	}
	mrows.Close()
	for _, rv := range revs {
		var stockQty, stockVer int64
		if err := tx.QueryRowContext(ctx,
			`SELECT stock_qty, stock_version FROM products WHERE id = ?`, rv.productID).
			Scan(&stockQty, &stockVer); err != nil {
			return nil, err
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE products SET stock_qty = stock_qty + ?, stock_version = stock_version + 1
			 WHERE id = ? AND stock_version = ?`, rv.qty, rv.productID, stockVer)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil, core.ErrConflict
		}
		newStock := stockQty + rv.qty
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO stock_movements
			 (id, product_id, delta, reason, ref_order_id, balance_after, user_id, created_at)
			 VALUES (?,?,?, 'void', ?,?,?,?)`,
			core.NewID(), rv.productID, rv.qty, id, newStock, u.ID, now); err != nil {
			return nil, err
		}
	}

	// REFUND balances: each 'balance' payment is credited back via a 'refund'
	// ledger row, restoring attendees.balance under a version guard.
	prows, err := tx.QueryContext(ctx,
		`SELECT attendee_id, amount FROM payments WHERE order_id = ? AND method = 'balance'`, id)
	if err != nil {
		return nil, err
	}
	type refund struct {
		attendeeID string
		amount     int64
	}
	var refunds []refund
	for prows.Next() {
		var att sql.NullString
		var amount int64
		if err := prows.Scan(&att, &amount); err != nil {
			prows.Close()
			return nil, err
		}
		if att.Valid {
			refunds = append(refunds, refund{att.String, amount})
		}
	}
	if err := prows.Err(); err != nil {
		prows.Close()
		return nil, err
	}
	prows.Close()
	for _, rf := range refunds {
		var balance, ver int64
		if err := tx.QueryRowContext(ctx,
			`SELECT balance, balance_version FROM attendees WHERE id = ?`, rf.attendeeID).
			Scan(&balance, &ver); err != nil {
			return nil, err
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE attendees SET balance = balance + ?, balance_version = balance_version + 1
			 WHERE id = ? AND balance_version = ?`, rf.amount, rf.attendeeID, ver)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil, core.ErrConflict
		}
		newBalance := balance + rf.amount
		// No ref_payment_id: that key was consumed by the original debit, and a
		// refund is a distinct, additive ledger event.
		if _, err := app.AppendLedger(tx, rf.attendeeID, "refund", rf.amount, newBalance, id, "", u.ID); err != nil {
			return nil, err
		}
	}

	return nil, app.WriteAudit(tx, app.AuditEntry{
		EventID: eventID, UserID: u.ID, IP: ip,
		Action: "order_void", Entity: "order", EntityID: id,
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// nz maps an empty string to a SQL NULL, otherwise the string itself.
func nz(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

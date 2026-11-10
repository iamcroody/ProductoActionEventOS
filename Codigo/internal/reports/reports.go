// Package reports provides read-only analytics for the Dashboard, Reportes, and
// Exportaciones screens of Action EventOS. Every query runs against the reader
// connection; no writes are performed by this package.
//
// Routes follow the canonical Module pattern established by package events:
// a Module struct holds server.Deps, New constructs it, and Register wires
// routes on the shared mux. Because this is a read-only package, there is no
// writer or audit trail.
package reports

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"eventpos/internal/core"
	"eventpos/internal/httpx"
	"eventpos/internal/server"
)

// Module mounts the reports routes.
type Module struct{ d server.Deps }

// New constructs the reports module.
func New(d server.Deps) *Module { return &Module{d: d} }

// Register wires routes. All report endpoints require admin, caja, or auditor role.
func (m *Module) Register(mux *http.ServeMux, d server.Deps) {
	auth := d.Auth.Middleware("admin", "caja", "auditor")
	mux.Handle("GET /api/reports/dashboard", auth(http.HandlerFunc(m.dashboard)))
	mux.Handle("GET /api/reports/tips", auth(http.HandlerFunc(m.tips)))
	mux.Handle("GET /api/reports/sales", auth(http.HandlerFunc(m.sales)))
	mux.Handle("GET /api/reports/export.csv", auth(http.HandlerFunc(m.exportCSV)))
	mux.Handle("GET /api/reports/export.json", auth(http.HandlerFunc(m.exportJSON)))

	// Consolidated end-of-event "cierre" report. Admin-only.
	adminOnly := d.Auth.Middleware("admin")
	mux.Handle("GET /api/reports/closing", adminOnly(http.HandlerFunc(m.closing)))
	mux.Handle("GET /api/reports/closing.xlsx", adminOnly(http.HandlerFunc(m.closingXLSX)))
	mux.Handle("GET /api/reports/closing.csv", adminOnly(http.HandlerFunc(m.closingCSV)))
}

// ---------------------------------------------------------------------------
// Event resolution
// ---------------------------------------------------------------------------

// resolveEventID returns the event ID to scope the report to. If the request
// carries ?event_id=, that value is used directly, allowing reports on closed
// events. Otherwise the currently-active event is looked up; if none is active,
// core.ErrNoActiveEvent is returned.
func resolveEventID(ctx context.Context, r *http.Request, reader *sql.DB) (string, error) {
	if id := r.URL.Query().Get("event_id"); id != "" {
		return id, nil
	}
	var eventID string
	err := reader.QueryRowContext(ctx,
		`SELECT id FROM events WHERE status = 'active' LIMIT 1`).Scan(&eventID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", core.ErrNoActiveEvent
	}
	return eventID, err
}

// ---------------------------------------------------------------------------
// Dashboard
// ---------------------------------------------------------------------------

// DashboardResponse is the full analytics snapshot returned by
// GET /api/reports/dashboard.
type DashboardResponse struct {
	EventID          string           `json:"event_id"`
	TotalSold        int64            `json:"total_sold"`    // ingresos (revenue)
	TotalCost        int64            `json:"total_cost"`    // costo de lo vendido (COGS)
	TotalProfit      int64            `json:"total_profit"`  // ganancia = ingresos - costo
	ProfitMargin     float64          `json:"profit_margin"` // ganancia / ingresos (0..1)
	OrderCount       int64            `json:"order_count"`
	AvgConsumption   float64          `json:"avg_consumption"`
	ActiveAttendees  int64            `json:"active_attendees"`
	OverageCash      int64            `json:"overage_cash"`
	BalanceFromSales int64            `json:"balance_from_sales"`
	TopProducts      []TopProduct     `json:"top_products"`
	SalesByHour      []HourBucket     `json:"sales_by_hour"`
	SalesByOperator  []OperatorBucket `json:"sales_by_operator"`
	LowStock         []LowStockItem   `json:"low_stock"`

	// --- Extended "cierre" business metrics ---

	// RegisteredAttendees is the total attendee count for the event (distinct
	// from ActiveAttendees, which only counts those who spent or hold balance).
	RegisteredAttendees int64 `json:"registered_attendees"`
	// AvgConsumptionPerClient is revenue / registered attendees (consumo
	// promedio por cliente), guarded against divide-by-zero.
	AvgConsumptionPerClient float64 `json:"avg_consumption_per_client"`
	// PendingBalance is SUM(attendees.balance): unspent redeemable money still
	// owed to attendees (dinero pendiente).
	PendingBalance int64 `json:"pending_balance"`
	// AvgSecondsPerOrder is the global average of (paid_at − created_at) over
	// paid orders, in seconds (tiempo promedio por pedido).
	AvgSecondsPerOrder float64 `json:"avg_seconds_per_order"`
	// FastestOperator is the operator with the lowest average fulfilment time,
	// requiring at least minFastestOrders paid orders to qualify. nil when none
	// qualifies.
	FastestOperator *FastestOperator `json:"fastest_operator"`
	// OutOfStock lists tracked products with stock_qty = 0 (productos agotados).
	OutOfStock []OutOfStockItem `json:"out_of_stock"`
}

// minFastestOrders is the minimum number of paid orders an operator must have
// processed to be eligible as the "vendedor más rápido".
const minFastestOrders = 3

// FastestOperator is the quickest cashier by average order fulfilment time.
type FastestOperator struct {
	Username   string  `json:"username"`
	AvgSeconds float64 `json:"avg_seconds"`
	OrderCount int64   `json:"order_count"`
}

// OutOfStockItem is a tracked product that has run out (stock_qty = 0).
type OutOfStockItem struct {
	ProductID string `json:"product_id"`
	Name      string `json:"name"`
}

// TopProduct is one entry in the top-10 products ranked by quantity sold.
type TopProduct struct {
	ProductID string `json:"product_id"`
	Name      string `json:"name"`
	QtySold   int64  `json:"qty_sold"`
	Revenue   int64  `json:"revenue"`
	Cost      int64  `json:"cost"`   // costo total vendido de este producto
	Profit    int64  `json:"profit"` // ganancia de este producto
}

// HourBucket aggregates order totals and counts by hour-of-day (local time).
type HourBucket struct {
	Hour  string `json:"hour"` // "HH" zero-padded
	Total int64  `json:"total"`
	Count int64  `json:"count"`
}

// OperatorBucket aggregates order totals and counts per cashier.
type OperatorBucket struct {
	OperatorID string `json:"operator_id"`
	Username   string `json:"username"`
	Total      int64  `json:"total"`
	Count      int64  `json:"count"`
	// AvgSeconds is the average (paid_at − created_at) over this operator's paid
	// orders, in seconds (0 when none have been paid).
	AvgSeconds float64 `json:"avg_seconds"`
}

// LowStockItem is a tracked product at or below the low-stock threshold (≤ 5).
type LowStockItem struct {
	ProductID string `json:"product_id"`
	Name      string `json:"name"`
	StockQty  int64  `json:"stock_qty"`
}

func (m *Module) dashboard(w http.ResponseWriter, r *http.Request) {
	eventID, err := resolveEventID(r.Context(), r, m.d.App.DB.Reader)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	dash, err := queryDashboard(r.Context(), m.d.App.DB.Reader, eventID)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, dash)
}

// ---------------------------------------------------------------------------
// Tips (propinas) distribution + cleaning (aseo) staff
// ---------------------------------------------------------------------------

// aseoStaffKey is the app_config key holding a JSON array of cleaning-staff
// names eligible for tips (they do not log in to the POS).
const aseoStaffKey = "aseo_staff"

// tipBonus is the extra fraction the top-3 meseros and all aseo staff receive
// over the base equal share.
const tipBonus = 0.05

// TipShare is one recipient's computed cut of the tip pool.
type TipShare struct {
	Name  string `json:"name"`
	Role  string `json:"role"`  // "mesero" | "aseo"
	Sales int64  `json:"sales"` // mesero sales used for ranking (0 for aseo)
	Top3  bool   `json:"top3"`
	Bonus bool   `json:"bonus"` // receives the +5% bonus
	Share int64  `json:"share"`
}

// TipsResponse is the propina distribution. The top-3 meseros by sales and all
// aseo staff receive a 5% larger share than the rest, who split equally.
// Integer shares are allocated by largest remainder so they sum exactly to pool.
type TipsResponse struct {
	Pool        int64      `json:"pool"`
	BonusPct    float64    `json:"bonus_pct"`
	Meseros     []TipShare `json:"meseros"`
	Aseo        []TipShare `json:"aseo"`
	Distributed int64      `json:"distributed"`
}

func (m *Module) tips(w http.ResponseWriter, r *http.Request) {
	eventID, err := resolveEventID(r.Context(), r, m.d.App.DB.Reader)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	pool, _ := strconv.ParseInt(r.URL.Query().Get("pool"), 10, 64)
	if pool < 0 {
		pool = 0
	}
	// A tip pool above MaxMoney is nonsensical and would overflow the float
	// largest-remainder math (float64(MaxInt64) rounds past int64) — reject it.
	if pool > core.MaxMoney {
		httpx.Error(w, core.ErrValidation)
		return
	}

	// Meseros ranked by sales effectiveness (non-voided order totals).
	rows, err := m.d.App.DB.Reader.QueryContext(r.Context(), `
		SELECT u.username,
		       COALESCE(SUM(CASE WHEN o.status <> 'voided' THEN o.total END), 0) AS sales
		FROM users u
		LEFT JOIN orders o ON o.operator_id = u.id AND o.event_id = ?
		WHERE u.role = 'mesero' AND u.active = 1
		GROUP BY u.id
		ORDER BY sales DESC, u.username`, eventID)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	defer rows.Close()
	meseros := []TipShare{}
	for rows.Next() {
		t := TipShare{Role: "mesero"}
		if err := rows.Scan(&t.Name, &t.Sales); err != nil {
			httpx.Error(w, err)
			return
		}
		meseros = append(meseros, t)
	}
	if err := rows.Err(); err != nil {
		httpx.Error(w, err)
		return
	}

	// Aseo staff from config (JSON array of names) — they always get the bonus.
	aseo := []TipShare{}
	if raw, err := m.d.Config.Get(r.Context(), aseoStaffKey); err == nil && raw != "" {
		var names []string
		if json.Unmarshal([]byte(raw), &names) == nil {
			for _, n := range names {
				aseo = append(aseo, TipShare{Name: n, Role: "aseo", Bonus: true})
			}
		}
	}

	// Top-3 meseros by sales get the bonus too.
	for i := range meseros {
		if i < 3 {
			meseros[i].Top3 = true
			meseros[i].Bonus = true
		}
	}

	// Build the weighted recipient list and allocate integer shares by largest
	// remainder so the total distributed equals the pool exactly.
	all := make([]*TipShare, 0, len(meseros)+len(aseo))
	for i := range meseros {
		all = append(all, &meseros[i])
	}
	for i := range aseo {
		all = append(all, &aseo[i])
	}
	if pool > 0 && len(all) > 0 {
		weights := make([]float64, len(all))
		var sumW float64
		for i, a := range all {
			w := 1.0
			if a.Bonus {
				w = 1.0 + tipBonus
			}
			weights[i] = w
			sumW += w
		}
		type rem struct {
			idx  int
			frac float64
		}
		floors := make([]int64, len(all))
		rems := make([]rem, len(all))
		var assigned int64
		for i := range all {
			exact := float64(pool) * weights[i] / sumW
			fl := int64(exact)
			floors[i] = fl
			assigned += fl
			rems[i] = rem{i, exact - float64(fl)}
		}
		left := pool - assigned
		sort.Slice(rems, func(a, b int) bool { return rems[a].frac > rems[b].frac })
		for k := int64(0); k < left && int(k) < len(rems); k++ {
			floors[rems[k].idx]++
		}
		for i, a := range all {
			a.Share = floors[i]
		}
	}

	var distributed int64
	for _, a := range all {
		distributed += a.Share
	}
	httpx.JSON(w, http.StatusOK, TipsResponse{
		Pool: pool, BonusPct: tipBonus, Meseros: meseros, Aseo: aseo, Distributed: distributed,
	})
}

// queryDashboard fetches all dashboard metrics for eventID using read-only
// queries. It is a package-level function so tests can call it directly.
// Voided orders (status = 'voided') are excluded from every sales metric.
func queryDashboard(ctx context.Context, reader *sql.DB, eventID string) (DashboardResponse, error) {
	out := DashboardResponse{
		EventID:         eventID,
		TopProducts:     []TopProduct{},
		SalesByHour:     []HourBucket{},
		SalesByOperator: []OperatorBucket{},
		LowStock:        []LowStockItem{},
		OutOfStock:      []OutOfStockItem{},
	}

	// Core order metrics: total revenue, order count, and average ticket size.
	if err := reader.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(total), 0),
		       COUNT(*),
		       COALESCE(AVG(CAST(total AS REAL)), 0)
		FROM orders
		WHERE event_id = ? AND status <> 'voided'`, eventID).
		Scan(&out.TotalSold, &out.OrderCount, &out.AvgConsumption); err != nil {
		return out, fmt.Errorf("order metrics: %w", err)
	}

	// Cost of goods sold and profit, from the per-item cost snapshot.
	if err := reader.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(oi.unit_cost * oi.qty), 0)
		FROM order_items oi
		JOIN orders o ON oi.order_id = o.id
		WHERE o.event_id = ? AND o.status <> 'voided'`, eventID).
		Scan(&out.TotalCost); err != nil {
		return out, fmt.Errorf("cost metrics: %w", err)
	}
	out.TotalProfit = out.TotalSold - out.TotalCost
	if out.TotalSold > 0 {
		out.ProfitMargin = float64(out.TotalProfit) / float64(out.TotalSold)
	}

	// Payment method breakdown. Join payments → orders to scope by event.
	if err := reader.QueryRowContext(ctx, `
		SELECT
		    COALESCE(SUM(CASE WHEN p.method = 'cash'    THEN p.amount END), 0),
		    COALESCE(SUM(CASE WHEN p.method = 'balance' THEN p.amount END), 0)
		FROM payments p
		JOIN orders o ON p.order_id = o.id
		WHERE o.event_id = ? AND o.status <> 'voided'`, eventID).
		Scan(&out.OverageCash, &out.BalanceFromSales); err != nil {
		return out, fmt.Errorf("payment metrics: %w", err)
	}

	// Active attendees: balance > 0 OR made at least one non-voided payment.
	if err := reader.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT a.id)
		FROM attendees a
		WHERE a.event_id = ?
		  AND (a.balance > 0
		       OR EXISTS (
		           SELECT 1 FROM payments p
		           JOIN orders o ON p.order_id = o.id
		           WHERE p.attendee_id = a.id
		             AND o.event_id    = a.event_id
		             AND o.status     <> 'voided'
		       ))`, eventID).Scan(&out.ActiveAttendees); err != nil {
		return out, fmt.Errorf("active attendees: %w", err)
	}

	// Top 10 products by quantity sold. Name is the snapshot stored in
	// order_items so it reflects the name at sale time.
	rows, err := reader.QueryContext(ctx, `
		SELECT oi.product_id, oi.name,
		       SUM(oi.qty) AS qty_sold,
		       SUM(oi.line_total) AS revenue,
		       SUM(oi.unit_cost * oi.qty) AS cost
		FROM order_items oi
		JOIN orders o ON oi.order_id = o.id
		WHERE o.event_id = ? AND o.status <> 'voided'
		GROUP BY oi.product_id, oi.name
		ORDER BY qty_sold DESC
		LIMIT 10`, eventID)
	if err != nil {
		return out, fmt.Errorf("top products: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p TopProduct
		if err := rows.Scan(&p.ProductID, &p.Name, &p.QtySold, &p.Revenue, &p.Cost); err != nil {
			return out, err
		}
		p.Profit = p.Revenue - p.Cost
		out.TopProducts = append(out.TopProducts, p)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	// Sales by hour-of-day in local time. created_at is unix milliseconds so
	// we divide by 1000 before passing to strftime.
	rows2, err := reader.QueryContext(ctx, `
		SELECT strftime('%H', created_at / 1000, 'unixepoch', 'localtime') AS hr,
		       SUM(total),
		       COUNT(*)
		FROM orders
		WHERE event_id = ? AND status <> 'voided'
		GROUP BY hr
		ORDER BY hr`, eventID)
	if err != nil {
		return out, fmt.Errorf("sales by hour: %w", err)
	}
	defer rows2.Close()
	for rows2.Next() {
		var b HourBucket
		if err := rows2.Scan(&b.Hour, &b.Total, &b.Count); err != nil {
			return out, err
		}
		out.SalesByHour = append(out.SalesByHour, b)
	}
	if err := rows2.Err(); err != nil {
		return out, err
	}

	// Sales by cashier (operator), including average fulfilment time over the
	// operator's paid orders (NULL paid_at orders are ignored by AVG).
	rows3, err := reader.QueryContext(ctx, `
		SELECT o.operator_id, u.username, SUM(o.total), COUNT(*),
		       COALESCE(AVG(CASE WHEN o.paid_at IS NOT NULL
		                         THEN (o.paid_at - o.created_at) / 1000.0 END), 0)
		FROM orders o
		JOIN users u ON o.operator_id = u.id
		WHERE o.event_id = ? AND o.status <> 'voided'
		GROUP BY o.operator_id, u.username
		ORDER BY SUM(o.total) DESC`, eventID)
	if err != nil {
		return out, fmt.Errorf("sales by operator: %w", err)
	}
	defer rows3.Close()
	for rows3.Next() {
		var b OperatorBucket
		if err := rows3.Scan(&b.OperatorID, &b.Username, &b.Total, &b.Count, &b.AvgSeconds); err != nil {
			return out, err
		}
		out.SalesByOperator = append(out.SalesByOperator, b)
	}
	if err := rows3.Err(); err != nil {
		return out, err
	}

	// Low-stock products: tracked items at or below 5 units remaining.
	rows4, err := reader.QueryContext(ctx, `
		SELECT id, name, stock_qty
		FROM products
		WHERE event_id = ? AND track_stock = 1 AND stock_qty <= 5
		ORDER BY stock_qty ASC`, eventID)
	if err != nil {
		return out, fmt.Errorf("low stock: %w", err)
	}
	defer rows4.Close()
	for rows4.Next() {
		var item LowStockItem
		if err := rows4.Scan(&item.ProductID, &item.Name, &item.StockQty); err != nil {
			return out, err
		}
		out.LowStock = append(out.LowStock, item)
	}
	if err := rows4.Err(); err != nil {
		return out, err
	}

	// Out-of-stock products (productos agotados): tracked items at exactly zero.
	rows5, err := reader.QueryContext(ctx, `
		SELECT id, name
		FROM products
		WHERE event_id = ? AND track_stock = 1 AND stock_qty = 0
		ORDER BY name`, eventID)
	if err != nil {
		return out, fmt.Errorf("out of stock: %w", err)
	}
	defer rows5.Close()
	for rows5.Next() {
		var item OutOfStockItem
		if err := rows5.Scan(&item.ProductID, &item.Name); err != nil {
			return out, err
		}
		out.OutOfStock = append(out.OutOfStock, item)
	}
	if err := rows5.Err(); err != nil {
		return out, err
	}

	// Registered attendees (clientes registrados) and pending redeemable balance
	// (dinero pendiente). Both are scoped to the event.
	if err := reader.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(balance), 0)
		FROM attendees
		WHERE event_id = ?`, eventID).
		Scan(&out.RegisteredAttendees, &out.PendingBalance); err != nil {
		return out, fmt.Errorf("attendee metrics: %w", err)
	}
	// Consumo promedio por cliente = ingresos / clientes registrados.
	if out.RegisteredAttendees > 0 {
		out.AvgConsumptionPerClient = float64(out.TotalSold) / float64(out.RegisteredAttendees)
	}

	// Global average order fulfilment time (tiempo promedio por pedido), in
	// seconds, over paid orders only.
	if err := reader.QueryRowContext(ctx, `
		SELECT COALESCE(AVG((paid_at - created_at) / 1000.0), 0)
		FROM orders
		WHERE event_id = ? AND status <> 'voided' AND paid_at IS NOT NULL`, eventID).
		Scan(&out.AvgSecondsPerOrder); err != nil {
		return out, fmt.Errorf("avg order time: %w", err)
	}

	// Fastest operator (vendedor más rápido): lowest average fulfilment time
	// among operators with at least minFastestOrders paid orders.
	var fastest FastestOperator
	err = reader.QueryRowContext(ctx, `
		SELECT u.username,
		       AVG((o.paid_at - o.created_at) / 1000.0) AS avg_sec,
		       COUNT(*) AS cnt
		FROM orders o
		JOIN users u ON o.operator_id = u.id
		WHERE o.event_id = ? AND o.status <> 'voided' AND o.paid_at IS NOT NULL
		GROUP BY o.operator_id, u.username
		HAVING COUNT(*) >= ?
		ORDER BY avg_sec ASC, cnt DESC
		LIMIT 1`, eventID, minFastestOrders).
		Scan(&fastest.Username, &fastest.AvgSeconds, &fastest.OrderCount)
	switch {
	case err == nil:
		out.FastestOperator = &fastest
	case errors.Is(err, sql.ErrNoRows):
		out.FastestOperator = nil
	default:
		return out, fmt.Errorf("fastest operator: %w", err)
	}

	return out, nil
}

// ---------------------------------------------------------------------------
// Sales list
// ---------------------------------------------------------------------------

// SaleRow is one order in the detailed sales list returned by
// GET /api/reports/sales.
type SaleRow struct {
	ID              string `json:"id"`
	CreatedAt       int64  `json:"created_at"`
	OperatorID      string `json:"operator_id"`
	OperatorName    string `json:"operator_name"`
	Total           int64  `json:"total"`
	PaidFromBalance int64  `json:"paid_from_balance"`
	PaidCash        int64  `json:"paid_cash"`
	ItemCount       int64  `json:"item_count"`
}

func (m *Module) sales(w http.ResponseWriter, r *http.Request) {
	eventID, err := resolveEventID(r.Context(), r, m.d.App.DB.Reader)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	limit := 200
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err2 := strconv.Atoi(l); err2 == nil && n > 0 {
			limit = n
		}
	}
	rows, err := querySales(r.Context(), m.d.App.DB.Reader, eventID, limit)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, rows)
}

// querySales returns up to limit non-voided orders for an event, newest first.
func querySales(ctx context.Context, reader *sql.DB, eventID string, limit int) ([]SaleRow, error) {
	rows, err := reader.QueryContext(ctx, `
		SELECT o.id, o.created_at, o.operator_id, u.username,
		       o.total, o.paid_from_balance, o.paid_cash,
		       (SELECT COUNT(*) FROM order_items WHERE order_id = o.id)
		FROM orders o
		JOIN users u ON o.operator_id = u.id
		WHERE o.event_id = ? AND o.status <> 'voided'
		ORDER BY o.created_at DESC
		LIMIT ?`, eventID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SaleRow{}
	for rows.Next() {
		var s SaleRow
		if err := rows.Scan(&s.ID, &s.CreatedAt, &s.OperatorID, &s.OperatorName,
			&s.Total, &s.PaidFromBalance, &s.PaidCash, &s.ItemCount); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Exports (CSV and JSON)
// ---------------------------------------------------------------------------

func (m *Module) exportCSV(w http.ResponseWriter, r *http.Request) {
	eventID, err := resolveEventID(r.Context(), r, m.d.App.DB.Reader)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	report := r.URL.Query().Get("report")
	headers, dataRows, err := fetchExportRows(r.Context(), m.d.App.DB.Reader, eventID, report)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	filename := fmt.Sprintf("%s_%s.csv", report, time.Now().Format("20060102_150405"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	cw := csv.NewWriter(w)
	_ = cw.Write(headers)
	for _, row := range dataRows {
		_ = cw.Write(row)
	}
	cw.Flush()
}

func (m *Module) exportJSON(w http.ResponseWriter, r *http.Request) {
	eventID, err := resolveEventID(r.Context(), r, m.d.App.DB.Reader)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	report := r.URL.Query().Get("report")
	headers, dataRows, err := fetchExportRows(r.Context(), m.d.App.DB.Reader, eventID, report)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	// Convert to a slice of column-keyed maps; the frontend can render a
	// print-to-PDF view directly from this structure.
	records := make([]map[string]string, 0, len(dataRows))
	for _, row := range dataRows {
		rec := make(map[string]string, len(headers))
		for i, h := range headers {
			if i < len(row) {
				rec[h] = row[i]
			}
		}
		records = append(records, rec)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"report":   report,
		"event_id": eventID,
		"rows":     records,
	})
}

// fetchExportRows dispatches to the per-report builder and returns column
// headers and string-formatted data rows, ready for either CSV or JSON output.
func fetchExportRows(ctx context.Context, reader *sql.DB, eventID, report string) ([]string, [][]string, error) {
	switch report {
	case "sales":
		return exportSales(ctx, reader, eventID)
	case "products":
		return exportProducts(ctx, reader, eventID)
	case "attendees":
		return exportAttendees(ctx, reader, eventID)
	case "ledger":
		return exportLedger(ctx, reader, eventID)
	default:
		return nil, nil, core.ErrValidation
	}
}

// exportSales streams non-voided orders with operator and item-count columns.
func exportSales(ctx context.Context, reader *sql.DB, eventID string) ([]string, [][]string, error) {
	headers := []string{
		"id", "created_at", "operator_id", "operator_username",
		"total", "paid_from_balance", "paid_cash", "item_count",
	}
	rows, err := reader.QueryContext(ctx, `
		SELECT o.id, o.created_at, o.operator_id, u.username,
		       o.total, o.paid_from_balance, o.paid_cash,
		       (SELECT COUNT(*) FROM order_items WHERE order_id = o.id)
		FROM orders o
		JOIN users u ON o.operator_id = u.id
		WHERE o.event_id = ? AND o.status <> 'voided'
		ORDER BY o.created_at DESC`, eventID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		var id, operatorID, username string
		var createdAt, total, paidBalance, paidCash, itemCount int64
		if err := rows.Scan(&id, &createdAt, &operatorID, &username,
			&total, &paidBalance, &paidCash, &itemCount); err != nil {
			return nil, nil, err
		}
		out = append(out, []string{
			id,
			strconv.FormatInt(createdAt, 10),
			operatorID,
			username,
			strconv.FormatInt(total, 10),
			strconv.FormatInt(paidBalance, 10),
			strconv.FormatInt(paidCash, 10),
			strconv.FormatInt(itemCount, 10),
		})
	}
	return headers, out, rows.Err()
}

// exportProducts streams the product catalog for an event.
func exportProducts(ctx context.Context, reader *sql.DB, eventID string) ([]string, [][]string, error) {
	headers := []string{"id", "name", "category_id", "price", "track_stock", "stock_qty", "active"}
	rows, err := reader.QueryContext(ctx, `
		SELECT id, name, COALESCE(category_id, ''), price, track_stock, stock_qty, active
		FROM products
		WHERE event_id = ?
		ORDER BY sort_order, name`, eventID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		var id, name, categoryID string
		var price, trackStock, stockQty, active int64
		if err := rows.Scan(&id, &name, &categoryID, &price, &trackStock, &stockQty, &active); err != nil {
			return nil, nil, err
		}
		out = append(out, []string{
			id, name, categoryID,
			strconv.FormatInt(price, 10),
			strconv.FormatInt(trackStock, 10),
			strconv.FormatInt(stockQty, 10),
			strconv.FormatInt(active, 10),
		})
	}
	return headers, out, rows.Err()
}

// exportAttendees streams attendee records for an event.
func exportAttendees(ctx context.Context, reader *sql.DB, eventID string) ([]string, [][]string, error) {
	headers := []string{"id", "display_name", "doc_id", "phone", "balance", "status"}
	rows, err := reader.QueryContext(ctx, `
		SELECT id, display_name, COALESCE(doc_id, ''), COALESCE(phone, ''), balance, status
		FROM attendees
		WHERE event_id = ?
		ORDER BY display_name`, eventID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		var id, name, docID, phone, status string
		var balance int64
		if err := rows.Scan(&id, &name, &docID, &phone, &balance, &status); err != nil {
			return nil, nil, err
		}
		out = append(out, []string{
			id, name, docID, phone,
			strconv.FormatInt(balance, 10),
			status,
		})
	}
	return headers, out, rows.Err()
}

// exportLedger streams balance_ledger rows scoped to the event via the
// attendees join.
func exportLedger(ctx context.Context, reader *sql.DB, eventID string) ([]string, [][]string, error) {
	headers := []string{
		"id", "attendee_id", "type", "amount", "balance_after",
		"ref_order_id", "user_id", "created_at",
	}
	rows, err := reader.QueryContext(ctx, `
		SELECT bl.id, bl.attendee_id, bl.type, bl.amount, bl.balance_after,
		       COALESCE(bl.ref_order_id, ''), bl.user_id, bl.created_at
		FROM balance_ledger bl
		JOIN attendees a ON bl.attendee_id = a.id
		WHERE a.event_id = ?
		ORDER BY bl.created_at DESC`, eventID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		var id, attendeeID, ledgerType, refOrderID, userID string
		var amount, balanceAfter, createdAt int64
		if err := rows.Scan(&id, &attendeeID, &ledgerType, &amount, &balanceAfter,
			&refOrderID, &userID, &createdAt); err != nil {
			return nil, nil, err
		}
		out = append(out, []string{
			id, attendeeID, ledgerType,
			strconv.FormatInt(amount, 10),
			strconv.FormatInt(balanceAfter, 10),
			refOrderID, userID,
			strconv.FormatInt(createdAt, 10),
		})
	}
	return headers, out, rows.Err()
}

// closing.go implements the consolidated end-of-event business report (the
// "cierre") that the director uses to wrap up an event. It exposes the same
// snapshot in three shapes: JSON (GET /api/reports/closing), a polished
// multi-sheet Excel workbook (closing.xlsx) and a sectioned CSV (closing.csv).
//
// Everything here is read-only and admin-scoped; it reuses queryDashboard for
// the heavy lifting and only adds the event header plus the output encoders.
package reports

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"eventpos/internal/core"
	"eventpos/internal/httpx"
)

// ---------------------------------------------------------------------------
// Wire shape (the frontend is built against this exact structure).
// ---------------------------------------------------------------------------

// ClosingResponse is the consolidated end-of-event report.
type ClosingResponse struct {
	Event       ClosingEvent      `json:"event"`
	Totals      ClosingTotals     `json:"totals"`
	Attendees   ClosingAttendees  `json:"attendees"`
	Timing      ClosingTiming     `json:"timing"`
	Operators   []ClosingOperator `json:"operators"`
	TopProducts []ClosingProduct  `json:"top_products"`
	Inventory   ClosingInventory  `json:"inventory"`
}

// ClosingEvent identifies the event the report covers.
type ClosingEvent struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// ClosingTotals holds the headline money figures (all INTEGER COP).
type ClosingTotals struct {
	Revenue          int64   `json:"revenue"`
	Cost             int64   `json:"cost"`
	Profit           int64   `json:"profit"`
	Margin           float64 `json:"margin"`
	OrderCount       int64   `json:"order_count"`
	OverageCash      int64   `json:"overage_cash"`
	BalanceFromSales int64   `json:"balance_from_sales"`
}

// ClosingAttendees holds the customer-side figures.
type ClosingAttendees struct {
	Registered     int64   `json:"registered"`
	Active         int64   `json:"active"`
	AvgConsumption float64 `json:"avg_consumption"`
	PendingBalance int64   `json:"pending_balance"`
}

// ClosingTiming holds order fulfilment speed metrics.
type ClosingTiming struct {
	AvgSecondsPerOrder float64          `json:"avg_seconds_per_order"`
	FastestOperator    *FastestOperator `json:"fastest_operator"`
}

// ClosingOperator is one cashier's contribution (no operator_id in this shape).
type ClosingOperator struct {
	Username   string  `json:"username"`
	Total      int64   `json:"total"`
	Count      int64   `json:"count"`
	AvgSeconds float64 `json:"avg_seconds"`
}

// ClosingProduct is one top-selling product (no product_id in this shape).
type ClosingProduct struct {
	Name    string `json:"name"`
	QtySold int64  `json:"qty_sold"`
	Revenue int64  `json:"revenue"`
	Cost    int64  `json:"cost"`
	Profit  int64  `json:"profit"`
}

// ClosingInventory groups the stock health lists.
type ClosingInventory struct {
	LowStock   []LowStockItem   `json:"low_stock"`
	OutOfStock []OutOfStockItem `json:"out_of_stock"`
}

// ---------------------------------------------------------------------------
// Event resolution (active-or-most-recent).
// ---------------------------------------------------------------------------

// resolveClosingEventID picks the event to report on. ?event_id= wins; failing
// that it returns the active event, or the most recently created event if none
// is active. core.ErrNoActiveEvent is returned only when there are no events.
func resolveClosingEventID(ctx context.Context, r *http.Request, reader *sql.DB) (string, error) {
	if id := r.URL.Query().Get("event_id"); id != "" {
		return id, nil
	}
	var eventID string
	err := reader.QueryRowContext(ctx, `
		SELECT id FROM events
		ORDER BY (status = 'active') DESC, created_at DESC
		LIMIT 1`).Scan(&eventID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", core.ErrNoActiveEvent
	}
	return eventID, err
}

// ---------------------------------------------------------------------------
// Builder.
// ---------------------------------------------------------------------------

// queryClosing assembles the consolidated report for eventID. It reuses
// queryDashboard for the metrics and adds the event header.
func queryClosing(ctx context.Context, reader *sql.DB, eventID string) (ClosingResponse, error) {
	var ev ClosingEvent
	err := reader.QueryRowContext(ctx,
		`SELECT id, name, status FROM events WHERE id = ?`, eventID).
		Scan(&ev.ID, &ev.Name, &ev.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return ClosingResponse{}, core.ErrNotFound
	}
	if err != nil {
		return ClosingResponse{}, fmt.Errorf("event header: %w", err)
	}

	dash, err := queryDashboard(ctx, reader, eventID)
	if err != nil {
		return ClosingResponse{}, err
	}

	out := ClosingResponse{
		Event: ev,
		Totals: ClosingTotals{
			Revenue:          dash.TotalSold,
			Cost:             dash.TotalCost,
			Profit:           dash.TotalProfit,
			Margin:           dash.ProfitMargin,
			OrderCount:       dash.OrderCount,
			OverageCash:      dash.OverageCash,
			BalanceFromSales: dash.BalanceFromSales,
		},
		Attendees: ClosingAttendees{
			Registered:     dash.RegisteredAttendees,
			Active:         dash.ActiveAttendees,
			AvgConsumption: dash.AvgConsumptionPerClient,
			PendingBalance: dash.PendingBalance,
		},
		Timing: ClosingTiming{
			AvgSecondsPerOrder: dash.AvgSecondsPerOrder,
			FastestOperator:    dash.FastestOperator,
		},
		Operators:   make([]ClosingOperator, 0, len(dash.SalesByOperator)),
		TopProducts: make([]ClosingProduct, 0, len(dash.TopProducts)),
		Inventory: ClosingInventory{
			LowStock:   dash.LowStock,
			OutOfStock: dash.OutOfStock,
		},
	}
	for _, o := range dash.SalesByOperator {
		out.Operators = append(out.Operators, ClosingOperator{
			Username: o.Username, Total: o.Total, Count: o.Count, AvgSeconds: o.AvgSeconds,
		})
	}
	for _, p := range dash.TopProducts {
		out.TopProducts = append(out.TopProducts, ClosingProduct{
			Name: p.Name, QtySold: p.QtySold, Revenue: p.Revenue, Cost: p.Cost, Profit: p.Profit,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Handlers.
// ---------------------------------------------------------------------------

func (m *Module) closing(w http.ResponseWriter, r *http.Request) {
	eventID, err := resolveClosingEventID(r.Context(), r, m.d.App.DB.Reader)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	rep, err := queryClosing(r.Context(), m.d.App.DB.Reader, eventID)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, rep)
}

func (m *Module) closingXLSX(w http.ResponseWriter, r *http.Request) {
	eventID, err := resolveClosingEventID(r.Context(), r, m.d.App.DB.Reader)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	rep, err := queryClosing(r.Context(), m.d.App.DB.Reader, eventID)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	rows, err := querySales(r.Context(), m.d.App.DB.Reader, eventID, 100000)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	att, err := queryClosingAttendees(r.Context(), m.d.App.DB.Reader, eventID)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	inv, err := queryClosingInventory(r.Context(), m.d.App.DB.Reader, eventID)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	f, err := buildClosingWorkbook(rep, rows, att, inv)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	defer f.Close()

	filename := fmt.Sprintf("cierre_%s_%s.xlsx", sanitizeFilename(rep.Event.Name), time.Now().Format("20060102_150405"))
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	if err := f.Write(w); err != nil {
		httpx.Error(w, err)
		return
	}
}

func (m *Module) closingCSV(w http.ResponseWriter, r *http.Request) {
	eventID, err := resolveClosingEventID(r.Context(), r, m.d.App.DB.Reader)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	rep, err := queryClosing(r.Context(), m.d.App.DB.Reader, eventID)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	filename := fmt.Sprintf("cierre_%s_%s.csv", sanitizeFilename(rep.Event.Name), time.Now().Format("20060102_150405"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	// UTF-8 BOM so Excel renders accents (á, é, í…) correctly.
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})
	writeClosingCSV(w, rep)
}

// ---------------------------------------------------------------------------
// Supplementary queries for the workbook sheets.
// ---------------------------------------------------------------------------

// closingAttendee is one row of the Asistentes sheet.
type closingAttendee struct {
	Name    string
	DocID   string
	Phone   string
	Balance int64
	Status  string
}

func queryClosingAttendees(ctx context.Context, reader *sql.DB, eventID string) ([]closingAttendee, error) {
	rows, err := reader.QueryContext(ctx, `
		SELECT display_name, COALESCE(doc_id, ''), COALESCE(phone, ''), balance, status
		FROM attendees
		WHERE event_id = ?
		ORDER BY display_name`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []closingAttendee
	for rows.Next() {
		var a closingAttendee
		if err := rows.Scan(&a.Name, &a.DocID, &a.Phone, &a.Balance, &a.Status); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// closingInventoryRow is one row of the Inventario sheet (full catalog with the
// remaining quantity), independent of the low/out-of-stock summary lists.
type closingInventoryRow struct {
	Name       string
	TrackStock int64
	StockQty   int64
	Active     int64
}

func queryClosingInventory(ctx context.Context, reader *sql.DB, eventID string) ([]closingInventoryRow, error) {
	rows, err := reader.QueryContext(ctx, `
		SELECT name, track_stock, stock_qty, active
		FROM products
		WHERE event_id = ?
		ORDER BY track_stock DESC, stock_qty ASC, name`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []closingInventoryRow
	for rows.Next() {
		var p closingInventoryRow
		if err := rows.Scan(&p.Name, &p.TrackStock, &p.StockQty, &p.Active); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Consolidated CSV writer.
// ---------------------------------------------------------------------------

// writeClosingCSV emits a single sectioned CSV: a KPIs block followed by the
// operator, product and inventory tables, each preceded by a section header.
func writeClosingCSV(w http.ResponseWriter, rep ClosingResponse) {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	row := func(cells ...string) { _ = cw.Write(cells) }
	blank := func() { _ = cw.Write([]string{}) }
	money := func(v int64) string { return strconv.FormatInt(v, 10) }
	secs := func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

	row("CIERRE DE EVENTO")
	row("Evento", rep.Event.Name)
	row("Estado", rep.Event.Status)
	row("Generado", time.Now().Format("2006-01-02 15:04:05"))
	blank()

	row("INDICADORES")
	row("Métrica", "Valor")
	row("Ingresos (COP)", money(rep.Totals.Revenue))
	row("Costo (COP)", money(rep.Totals.Cost))
	row("Ganancia (COP)", money(rep.Totals.Profit))
	row("Margen", strconv.FormatFloat(rep.Totals.Margin, 'f', 4, 64))
	row("Pedidos", strconv.FormatInt(rep.Totals.OrderCount, 10))
	row("Excedente en efectivo (COP)", money(rep.Totals.OverageCash))
	row("Saldo usado en ventas (COP)", money(rep.Totals.BalanceFromSales))
	row("Clientes registrados", strconv.FormatInt(rep.Attendees.Registered, 10))
	row("Clientes activos", strconv.FormatInt(rep.Attendees.Active, 10))
	row("Consumo promedio por cliente (COP)", strconv.FormatFloat(rep.Attendees.AvgConsumption, 'f', 2, 64))
	row("Dinero pendiente (COP)", money(rep.Attendees.PendingBalance))
	row("Tiempo promedio por pedido (s)", secs(rep.Timing.AvgSecondsPerOrder))
	if rep.Timing.FastestOperator != nil {
		row("Vendedor más rápido", rep.Timing.FastestOperator.Username,
			secs(rep.Timing.FastestOperator.AvgSeconds)+" s",
			strconv.FormatInt(rep.Timing.FastestOperator.OrderCount, 10)+" pedidos")
	} else {
		row("Vendedor más rápido", "n/a")
	}
	blank()

	row("POR VENDEDOR")
	row("Vendedor", "Total (COP)", "Pedidos", "Tiempo prom. (s)")
	for _, o := range rep.Operators {
		row(o.Username, money(o.Total), strconv.FormatInt(o.Count, 10), secs(o.AvgSeconds))
	}
	blank()

	row("TOP PRODUCTOS")
	row("Producto", "Unidades", "Ingreso (COP)", "Costo (COP)", "Ganancia (COP)")
	for _, p := range rep.TopProducts {
		row(p.Name, strconv.FormatInt(p.QtySold, 10), money(p.Revenue), money(p.Cost), money(p.Profit))
	}
	blank()

	row("INVENTARIO BAJO")
	row("Producto", "Stock restante")
	for _, it := range rep.Inventory.LowStock {
		row(it.Name, strconv.FormatInt(it.StockQty, 10))
	}
	blank()

	row("PRODUCTOS AGOTADOS")
	row("Producto")
	for _, it := range rep.Inventory.OutOfStock {
		row(it.Name)
	}
}

// sanitizeFilename keeps only filename-safe characters for the download name.
func sanitizeFilename(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		case r == ' ', r == '-', r == '_':
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "evento"
	}
	return string(out)
}

// Package kitchen implements the real-time "Pendientes" (pending deliveries)
// board. There is no food preparation in this operation: every sold item is
// something to hand over to the attendee from bodega, and it may be delivered
// partially (e.g. someone buys 6 waters and receives them little by little).
//
// Each order_item tracks delivered_qty (0..qty). An item is fully delivered
// when delivered_qty == qty; an order becomes status='delivered' once all of
// its items are fully delivered. Deliveries are serialized through the single
// writer, so the "don't over-deliver" guard is race-free, and each delivery
// publishes a "ticket_update" SSE event on the "pendientes" topic so every
// open board refreshes without polling.
//
// (The package is named kitchen for historical reasons; it is never surfaced
// to users, who only ever see "Pendientes".)
package kitchen

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/core"
	"eventpos/internal/httpx"
	"eventpos/internal/server"
)

// pendientesTopic is the single SSE topic for pending-delivery updates.
const pendientesTopic = "pendientes"

// Module mounts the pendientes routes.
type Module struct{ d server.Deps }

// New constructs the module.
func New(d server.Deps) *Module { return &Module{d: d} }

// Register wires routes. Reading the board is open to any authenticated user;
// recording deliveries is for operational roles (not the read-only auditor).
func (m *Module) Register(mux *http.ServeMux, d server.Deps) {
	read := d.Auth.Middleware()
	write := d.Auth.Middleware("admin", "caja", "mesero", "cocina", "bar")

	mux.Handle("GET /api/pendientes", read(http.HandlerFunc(m.board)))
	mux.Handle("POST /api/pendientes/items/{id}/entregar", write(http.HandlerFunc(m.entregar)))
	mux.Handle("POST /api/pendientes/items/{id}/entregar-todo", write(http.HandlerFunc(m.entregarTodo)))
	mux.Handle("GET /api/pendientes/history", read(http.HandlerFunc(m.history)))
}

// activeEventID returns the id of the currently active event, or
// core.ErrNoActiveEvent when none is active.
func activeEventID(ctx context.Context, d server.Deps) (string, error) {
	var id string
	err := d.App.DB.Reader.QueryRowContext(ctx,
		`SELECT id FROM events WHERE status='active' LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", core.ErrNoActiveEvent
	}
	return id, err
}

// BoardItem is one order_item with its delivery progress.
type BoardItem struct {
	ID           string `json:"id"`
	OrderID      string `json:"order_id"`
	TableLabel   string `json:"table_label"`
	Name         string `json:"name"`
	Qty          int64  `json:"qty"`
	DeliveredQty int64  `json:"delivered_qty"`
	Pending      int64  `json:"pending"`
	CreatedAt    int64  `json:"created_at"`
}

// BoardOrder groups the pending items of one order for display. Source lets the
// UI highlight client self-orders (source='cliente') in yellow.
type BoardOrder struct {
	OrderID    string      `json:"order_id"`
	TableLabel string      `json:"table_label"` // ubicación (mesa/zona)
	Buyer      string      `json:"buyer"`       // a nombre de quién está el pedido
	Source     string      `json:"source"`
	CreatedAt  int64       `json:"created_at"`
	Items      []BoardItem `json:"items"`
}

// board returns every item still pending delivery (delivered_qty < qty) for the
// active event, grouped by order, oldest order first.
func (m *Module) board(w http.ResponseWriter, r *http.Request) {
	eventID, err := activeEventID(r.Context(), m.d)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	rows, err := m.d.App.DB.Reader.QueryContext(r.Context(), `
		SELECT oi.id, oi.order_id, COALESCE(o.table_label,''), o.source,
		       COALESCE((SELECT a.display_name FROM payments p JOIN attendees a ON a.id = p.attendee_id
		                 WHERE p.order_id = o.id AND p.attendee_id IS NOT NULL LIMIT 1), '') AS buyer,
		       oi.name, oi.qty, oi.delivered_qty, o.created_at
		FROM order_items oi
		JOIN orders o ON o.id = oi.order_id
		WHERE oi.delivered_qty < oi.qty
		  AND o.event_id = ?
		  AND o.status <> 'voided'
		ORDER BY o.created_at ASC`, eventID)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	defer rows.Close()

	orderList := []BoardOrder{}
	orderIdx := map[string]int{}
	for rows.Next() {
		var it BoardItem
		var source, buyer string
		if err := rows.Scan(&it.ID, &it.OrderID, &it.TableLabel, &source, &buyer, &it.Name,
			&it.Qty, &it.DeliveredQty, &it.CreatedAt); err != nil {
			httpx.Error(w, err)
			return
		}
		it.Pending = it.Qty - it.DeliveredQty
		idx, ok := orderIdx[it.OrderID]
		if !ok {
			idx = len(orderList)
			orderIdx[it.OrderID] = idx
			orderList = append(orderList, BoardOrder{
				OrderID: it.OrderID, TableLabel: it.TableLabel, Buyer: buyer, Source: source,
				CreatedAt: it.CreatedAt, Items: []BoardItem{},
			})
		}
		orderList[idx].Items = append(orderList[idx].Items, it)
	}
	if err := rows.Err(); err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, orderList)
}

// history returns recently fully-delivered items, newest first (default 50).
func (m *Module) history(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			limit = n
		}
	}
	eventID, err := activeEventID(r.Context(), m.d)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	rows, err := m.d.App.DB.Reader.QueryContext(r.Context(), `
		SELECT oi.id, oi.order_id, COALESCE(o.table_label,''), oi.name, oi.qty,
		       oi.delivered_qty, o.created_at
		FROM order_items oi
		JOIN orders o ON o.id = oi.order_id
		WHERE oi.delivered_qty >= oi.qty
		  AND o.event_id = ?
		  AND o.status <> 'voided'
		ORDER BY oi.delivered_at DESC
		LIMIT ?`, eventID, limit)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	defer rows.Close()
	out := []BoardItem{}
	for rows.Next() {
		var it BoardItem
		if err := rows.Scan(&it.ID, &it.OrderID, &it.TableLabel, &it.Name,
			&it.Qty, &it.DeliveredQty, &it.CreatedAt); err != nil {
			httpx.Error(w, err)
			return
		}
		it.Pending = it.Qty - it.DeliveredQty
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// entregar records the delivery of N units of an item (default 1).
func (m *Module) entregar(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Qty int64 `json:"qty"`
	}
	// Body is optional; default to 1 unit when absent or invalid.
	_ = httpx.Decode(r, &in)
	if in.Qty <= 0 {
		in.Qty = 1
	}
	m.deliver(w, r, in.Qty, false)
}

// entregarTodo delivers all remaining units of an item in one tap.
func (m *Module) entregarTodo(w http.ResponseWriter, r *http.Request) {
	m.deliver(w, r, 0, true)
}

// deliver applies a (partial or full) delivery to an item, serialized through
// the writer. all=true delivers every remaining unit; otherwise want units are
// delivered, clamped to what remains. Promotes the order to 'delivered' when
// every item is fully handed over, and publishes a pendientes SSE update.
func (m *Module) deliver(w http.ResponseWriter, r *http.Request, want int64, all bool) {
	u, _ := auth.FromContext(r.Context())
	id := r.PathValue("id")

	type result struct {
		orderID        string
		qty, delivered int64
		fully          bool
	}
	out, err := m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		var qty, delivered int64
		var orderID, eventID, name string
		err := tx.QueryRow(`
			SELECT oi.qty, oi.delivered_qty, oi.order_id, oi.name, o.event_id
			FROM order_items oi JOIN orders o ON o.id = oi.order_id
			WHERE oi.id = ?`, id).Scan(&qty, &delivered, &orderID, &name, &eventID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, core.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		remaining := qty - delivered
		if remaining <= 0 {
			return nil, core.ErrConflict // already fully delivered
		}
		add := want
		if all || add > remaining {
			add = remaining // forgiving: clamp to what's left
		}
		newDelivered := delivered + add
		fully := newDelivered >= qty
		now := core.NowMillis()

		if fully {
			if _, err := tx.Exec(
				`UPDATE order_items SET delivered_qty=?, prep_status='delivered', delivered_at=? WHERE id=?`,
				newDelivered, now, id); err != nil {
				return nil, err
			}
		} else {
			if _, err := tx.Exec(
				`UPDATE order_items SET delivered_qty=? WHERE id=?`, newDelivered, id); err != nil {
				return nil, err
			}
		}

		// Promote the order when every item is fully delivered.
		if fully {
			var pendingItems int
			if err := tx.QueryRow(
				`SELECT COUNT(*) FROM order_items WHERE order_id=? AND delivered_qty < qty`,
				orderID).Scan(&pendingItems); err != nil {
				return nil, err
			}
			if pendingItems == 0 {
				if _, err := tx.Exec(
					`UPDATE orders SET status='delivered' WHERE id=? AND status<>'voided'`, orderID); err != nil {
					return nil, err
				}
			}
		}

		if err := app.WriteAudit(tx, app.AuditEntry{
			EventID: eventID, UserID: u.ID, IP: httpx.ClientIP(r),
			Action: "entrega", Entity: "order_item", EntityID: id,
			DetailJSON: fmt.Sprintf(`{"order_id":%q,"item":%q,"entregado":%d,"total":%d,"de":%d}`,
				orderID, name, add, newDelivered, qty),
		}); err != nil {
			return nil, err
		}
		return result{orderID: orderID, qty: qty, delivered: newDelivered, fully: fully}, nil
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	res := out.(result)

	// Live update for every open Pendientes board.
	status := "pending"
	if res.fully {
		status = "delivered"
	}
	m.d.App.Hub.Publish(pendientesTopic, "ticket_update", fmt.Sprintf(
		`{"order_id":%q,"item_id":%q,"delivered_qty":%d,"qty":%d,"status":%q}`,
		res.orderID, id, res.delivered, res.qty, status))

	httpx.JSON(w, http.StatusOK, map[string]any{
		"ok": true, "delivered_qty": res.delivered, "qty": res.qty, "fully": res.fully,
	})
}

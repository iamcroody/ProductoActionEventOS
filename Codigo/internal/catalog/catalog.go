// Package catalog manages categories, products, and stock inventory for
// Action EventOS. All data is scoped to the currently active event.
//
// This package follows the canonical module pattern established in package
// events: a Module holding shared deps, mutations serialized through the
// single writer with an audit row, and reads served directly from the reader.
package catalog

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/core"
	"eventpos/internal/events"
	"eventpos/internal/httpx"
	"eventpos/internal/server"
)

// Category groups products within an event.
type Category struct {
	ID        string `json:"id"`
	EventID   string `json:"event_id"`
	Name      string `json:"name"`
	SortOrder int    `json:"sort_order"`
	Active    int    `json:"active"`
}

// Product is a saleable item scoped to an event.
type Product struct {
	ID           string  `json:"id"`
	EventID      string  `json:"event_id"`
	CategoryID   *string `json:"category_id"`
	CategoryName *string `json:"category_name"`
	Name         string  `json:"name"`
	Price        int64   `json:"price"`
	Cost         int64   `json:"cost"` // valor al por mayor (costo unitario)
	PrepStation  string  `json:"prep_station"`
	TrackStock   int     `json:"track_stock"`
	StockQty     int64   `json:"stock_qty"`
	StockVersion int64   `json:"stock_version"`
	SortOrder    int     `json:"sort_order"`
	ImagePath    *string `json:"image_path"`
	Active       int     `json:"active"`
	CreatedAt    int64   `json:"created_at"`
}

// StockMovement is an immutable record of a change to a product's stock level.
type StockMovement struct {
	ID           string  `json:"id"`
	ProductID    string  `json:"product_id"`
	Delta        int64   `json:"delta"`
	Reason       string  `json:"reason"`
	RefOrderID   *string `json:"ref_order_id"`
	BalanceAfter int64   `json:"balance_after"`
	UserID       string  `json:"user_id"`
	CreatedAt    int64   `json:"created_at"`
}

// Module mounts the catalog routes.
type Module struct{ d server.Deps }

// New constructs the catalog module.
func New(d server.Deps) *Module { return &Module{d: d} }

// Register wires routes. Reads require any authenticated user; category/product
// writes require admin; inventory adjustments allow admin or caja.
func (m *Module) Register(mux *http.ServeMux, d server.Deps) {
	any := d.Auth.Middleware()
	admin := d.Auth.Middleware("admin")
	adminCaja := d.Auth.Middleware("admin", "caja")

	// Categories.
	mux.Handle("GET /api/categories", any(http.HandlerFunc(m.listCategories)))
	mux.Handle("POST /api/categories", admin(http.HandlerFunc(m.createCategory)))
	mux.Handle("PUT /api/categories/{id}", admin(http.HandlerFunc(m.updateCategory)))

	// Products.
	mux.Handle("GET /api/products", any(http.HandlerFunc(m.listProducts)))
	mux.Handle("POST /api/products", admin(http.HandlerFunc(m.createProduct)))
	mux.Handle("PUT /api/products/{id}", admin(http.HandlerFunc(m.updateProduct)))
	mux.Handle("DELETE /api/products/{id}", admin(http.HandlerFunc(m.deleteProduct)))

	// Inventory.
	mux.Handle("POST /api/inventory/adjust", adminCaja(http.HandlerFunc(m.adjustStock)))
	mux.Handle("GET /api/inventory/movements", any(http.HandlerFunc(m.listMovements)))
	mux.Handle("GET /api/inventory/low-stock", any(http.HandlerFunc(m.lowStock)))
}

// currentEvent resolves the current event (status draft or active) and its
// status, or core.ErrNoActiveEvent when none exists. Configuration runs against
// this event; reads work in both draft and active, but mutations are rejected
// once the event is 'active' (see lockedIfActive).
func (m *Module) currentEvent(r *http.Request) (id string, status string, err error) {
	return events.CurrentEventID(r.Context(), m.d.App.DB.Reader)
}

// lockedIfActive writes a 409 Conflict carrying core.ErrEventLocked and returns
// true when the current event is already 'active' (configuration is frozen once
// the event has been started). httpx.Error is intentionally bypassed: httpx is a
// shared package we cannot edit, and its default mapping would return 500 for an
// unrecognised error. Writing the response here keeps the lock status code
// explicit and consistent (409) across every configuration handler.
func lockedIfActive(w http.ResponseWriter, status string) bool {
	if status == "active" {
		httpx.JSON(w, http.StatusConflict, map[string]string{"error": core.ErrEventLocked.Error()})
		return true
	}
	return false
}

// validPrepStation mirrors the schema CHECK so hostile input is rejected with a
// clean 400 instead of hitting the DB constraint and surfacing as a 500.
func validPrepStation(s string) bool {
	switch s {
	case "cocina", "bar", "none":
		return true
	}
	return false
}

// --- Category handlers ---

func (m *Module) listCategories(w http.ResponseWriter, r *http.Request) {
	evID, _, err := m.currentEvent(r)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	rows, err := m.d.App.DB.Reader.QueryContext(r.Context(),
		`SELECT id, event_id, name, sort_order, active
		 FROM categories WHERE event_id = ?
		 ORDER BY sort_order, name`, evID)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	defer rows.Close()
	out := []Category{}
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.EventID, &c.Name, &c.SortOrder, &c.Active); err != nil {
			httpx.Error(w, err)
			return
		}
		out = append(out, c)
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (m *Module) createCategory(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	evID, status, err := m.currentEvent(r)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if lockedIfActive(w, status) {
		return
	}
	var in struct {
		Name      string `json:"name"`
		SortOrder int    `json:"sort_order"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}
	if in.Name == "" {
		httpx.Error(w, core.ErrValidation)
		return
	}
	id := core.NewID()
	_, err = m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		if _, err := tx.Exec(
			`INSERT INTO categories(id, event_id, name, sort_order) VALUES (?,?,?,?)`,
			id, evID, in.Name, in.SortOrder); err != nil {
			return nil, err
		}
		return nil, app.WriteAudit(tx, app.AuditEntry{
			EventID: evID, UserID: u.ID, IP: httpx.ClientIP(r),
			Action: "category_create", Entity: "category", EntityID: id,
			DetailJSON: fmt.Sprintf(`{"name":%q}`, in.Name),
		})
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, Category{
		ID: id, EventID: evID, Name: in.Name, SortOrder: in.SortOrder, Active: 1,
	})
}

func (m *Module) updateCategory(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	id := r.PathValue("id")
	var in struct {
		Name      string `json:"name"`
		SortOrder int    `json:"sort_order"`
		Active    int    `json:"active"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}
	evID, status, err := m.currentEvent(r)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if lockedIfActive(w, status) {
		return
	}
	_, err = m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		res, err := tx.Exec(
			`UPDATE categories SET name=?, sort_order=?, active=? WHERE id=? AND event_id=?`,
			in.Name, in.SortOrder, in.Active, id, evID)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, core.ErrNotFound
		}
		return nil, app.WriteAudit(tx, app.AuditEntry{
			EventID: evID, UserID: u.ID, IP: httpx.ClientIP(r),
			Action: "category_update", Entity: "category", EntityID: id,
		})
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Product handlers ---

func (m *Module) listProducts(w http.ResponseWriter, r *http.Request) {
	evID, _, err := m.currentEvent(r)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	// Build a dynamic query honouring optional ?category= and ?active= filters.
	query := `SELECT p.id, p.event_id, p.category_id, c.name,
	          p.name, p.price, p.cost, p.prep_station, p.track_stock,
	          p.stock_qty, p.stock_version, p.sort_order, p.image_path, p.active, p.created_at
	          FROM products p
	          LEFT JOIN categories c ON c.id = p.category_id
	          WHERE p.event_id = ?`
	args := []any{evID}

	if cat := r.URL.Query().Get("category"); cat != "" {
		query += " AND p.category_id = ?"
		args = append(args, cat)
	}
	if activeQ := r.URL.Query().Get("active"); activeQ != "" {
		query += " AND p.active = ?"
		args = append(args, activeQ)
	}
	query += " ORDER BY p.sort_order, p.name"

	rows, err := m.d.App.DB.Reader.QueryContext(r.Context(), query, args...)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	defer rows.Close()
	out := []Product{}
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.EventID, &p.CategoryID, &p.CategoryName,
			&p.Name, &p.Price, &p.Cost, &p.PrepStation, &p.TrackStock,
			&p.StockQty, &p.StockVersion, &p.SortOrder, &p.ImagePath,
			&p.Active, &p.CreatedAt); err != nil {
			httpx.Error(w, err)
			return
		}
		out = append(out, p)
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (m *Module) createProduct(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	evID, status, err := m.currentEvent(r)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if lockedIfActive(w, status) {
		return
	}
	var in struct {
		CategoryID  *string `json:"category_id"`
		Name        string  `json:"name"`
		Price       int64   `json:"price"`
		Cost        int64   `json:"cost"`
		PrepStation string  `json:"prep_station"`
		TrackStock  int     `json:"track_stock"`
		StockQty    int64   `json:"stock_qty"`
		SortOrder   int     `json:"sort_order"`
		ImagePath   *string `json:"image_path"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}
	if in.Name == "" || in.Price < 0 || in.Cost < 0 || in.Price > core.MaxMoney || in.Cost > core.MaxMoney {
		httpx.Error(w, core.ErrValidation)
		return
	}
	if in.PrepStation == "" {
		in.PrepStation = "none"
	}
	if !validPrepStation(in.PrepStation) {
		httpx.Error(w, core.ErrValidation)
		return
	}
	id := core.NewID()
	now := core.NowMillis()
	_, err = m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		if _, err := tx.Exec(
			`INSERT INTO products(id, event_id, category_id, name, price, cost, prep_station,
			  track_stock, stock_qty, stock_version, sort_order, image_path, created_at)
			 VALUES (?,?,?,?,?,?,?,?,?,0,?,?,?)`,
			id, evID, in.CategoryID, in.Name, in.Price, in.Cost, in.PrepStation,
			in.TrackStock, in.StockQty, in.SortOrder, in.ImagePath, now); err != nil {
			return nil, err
		}
		// Record the initial stock movement when the product starts with stock.
		if in.TrackStock == 1 && in.StockQty > 0 {
			movID := core.NewID()
			if _, err := tx.Exec(
				`INSERT INTO stock_movements(id, product_id, delta, reason, balance_after, user_id, created_at)
				 VALUES (?,?,?,'initial',?,?,?)`,
				movID, id, in.StockQty, in.StockQty, u.ID, now); err != nil {
				return nil, err
			}
		}
		return nil, app.WriteAudit(tx, app.AuditEntry{
			EventID: evID, UserID: u.ID, IP: httpx.ClientIP(r),
			Action: "product_create", Entity: "product", EntityID: id,
			DetailJSON: fmt.Sprintf(`{"name":%q,"price":%d}`, in.Name, in.Price),
		})
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, Product{
		ID: id, EventID: evID, CategoryID: in.CategoryID, Name: in.Name,
		Price: in.Price, Cost: in.Cost, PrepStation: in.PrepStation, TrackStock: in.TrackStock,
		StockQty: in.StockQty, SortOrder: in.SortOrder, ImagePath: in.ImagePath,
		Active: 1, CreatedAt: now,
	})
}

func (m *Module) updateProduct(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	id := r.PathValue("id")
	// stock_qty is intentionally excluded: stock changes only through inventory endpoints.
	var in struct {
		CategoryID  *string `json:"category_id"`
		Name        string  `json:"name"`
		Price       int64   `json:"price"`
		Cost        int64   `json:"cost"`
		PrepStation string  `json:"prep_station"`
		TrackStock  int     `json:"track_stock"`
		SortOrder   int     `json:"sort_order"`
		ImagePath   *string `json:"image_path"`
		Active      int     `json:"active"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}
	if in.Name == "" || in.Price < 0 || in.Cost < 0 || in.Price > core.MaxMoney || in.Cost > core.MaxMoney {
		httpx.Error(w, core.ErrValidation)
		return
	}
	if in.PrepStation != "" && !validPrepStation(in.PrepStation) {
		httpx.Error(w, core.ErrValidation)
		return
	}
	evID, status, err := m.currentEvent(r)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if lockedIfActive(w, status) {
		return
	}
	_, err = m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		res, err := tx.Exec(
			`UPDATE products
			 SET category_id=?, name=?, price=?, cost=?, prep_station=?,
			     track_stock=?, sort_order=?, image_path=?, active=?
			 WHERE id=? AND event_id=?`,
			in.CategoryID, in.Name, in.Price, in.Cost, in.PrepStation,
			in.TrackStock, in.SortOrder, in.ImagePath, in.Active,
			id, evID)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, core.ErrNotFound
		}
		return nil, app.WriteAudit(tx, app.AuditEntry{
			EventID: evID, UserID: u.ID, IP: httpx.ClientIP(r),
			Action: "product_update", Entity: "product", EntityID: id,
		})
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (m *Module) deleteProduct(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	id := r.PathValue("id")
	evID, status, err := m.currentEvent(r)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if lockedIfActive(w, status) {
		return
	}
	_, err = m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		res, err := tx.Exec(
			`UPDATE products SET active=0 WHERE id=? AND event_id=?`, id, evID)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, core.ErrNotFound
		}
		return nil, app.WriteAudit(tx, app.AuditEntry{
			EventID: evID, UserID: u.ID, IP: httpx.ClientIP(r),
			Action: "product_delete", Entity: "product", EntityID: id,
		})
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Inventory handlers ---

// adjustStock applies a signed delta to a product's stock inside a single
// serialized writer transaction, using optimistic locking on stock_version to
// detect concurrent modifications. Returns ErrInsufficientStock when new qty
// would go below zero, and ErrConflict on a version mismatch.
func (m *Module) adjustStock(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	var in struct {
		ProductID string `json:"product_id"`
		Delta     int64  `json:"delta"`
		Reason    string `json:"reason"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}
	if in.ProductID == "" || in.Reason == "" {
		httpx.Error(w, core.ErrValidation)
		return
	}
	// Validate reason against the allowed inventory-adjust values.
	switch in.Reason {
	case "restock", "waste", "adjustment":
		// ok
	default:
		httpx.Error(w, core.ErrValidation)
		return
	}

	evID, status, err := m.currentEvent(r)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if lockedIfActive(w, status) {
		return
	}

	_, err = m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		// Re-read inside the tx so the read and write are atomic.
		var currentQty, currentVer int64
		if err := tx.QueryRow(
			`SELECT stock_qty, stock_version
			 FROM products WHERE id=? AND event_id=? AND track_stock=1`,
			in.ProductID, evID).Scan(&currentQty, &currentVer); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, core.ErrNotFound
			}
			return nil, err
		}

		newQty := currentQty + in.Delta
		if newQty < 0 {
			return nil, core.ErrInsufficientStock
		}

		// Optimistic locking: assert the version we read is still current.
		res, err := tx.Exec(
			`UPDATE products SET stock_qty=?, stock_version=stock_version+1
			 WHERE id=? AND stock_version=?`,
			newQty, in.ProductID, currentVer)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, core.ErrConflict
		}

		// Append the immutable stock movement.
		movID := core.NewID()
		now := core.NowMillis()
		if _, err := tx.Exec(
			`INSERT INTO stock_movements(id, product_id, delta, reason, balance_after, user_id, created_at)
			 VALUES (?,?,?,?,?,?,?)`,
			movID, in.ProductID, in.Delta, in.Reason, newQty, u.ID, now); err != nil {
			return nil, err
		}

		return nil, app.WriteAudit(tx, app.AuditEntry{
			EventID: evID, UserID: u.ID, IP: httpx.ClientIP(r),
			Action: "stock_adjust", Entity: "product", EntityID: in.ProductID,
			DetailJSON: fmt.Sprintf(`{"delta":%d,"reason":%q,"balance_after":%d}`,
				in.Delta, in.Reason, newQty),
		})
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (m *Module) listMovements(w http.ResponseWriter, r *http.Request) {
	productID := r.URL.Query().Get("product_id")
	if productID == "" {
		httpx.Error(w, core.ErrValidation)
		return
	}
	rows, err := m.d.App.DB.Reader.QueryContext(r.Context(),
		`SELECT id, product_id, delta, reason, ref_order_id, balance_after, user_id, created_at
		 FROM stock_movements WHERE product_id = ?
		 ORDER BY created_at DESC`, productID)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	defer rows.Close()
	out := []StockMovement{}
	for rows.Next() {
		var sm StockMovement
		if err := rows.Scan(&sm.ID, &sm.ProductID, &sm.Delta, &sm.Reason,
			&sm.RefOrderID, &sm.BalanceAfter, &sm.UserID, &sm.CreatedAt); err != nil {
			httpx.Error(w, err)
			return
		}
		out = append(out, sm)
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (m *Module) lowStock(w http.ResponseWriter, r *http.Request) {
	evID, _, err := m.currentEvent(r)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	threshold := int64(5)
	if t := r.URL.Query().Get("threshold"); t != "" {
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			threshold = n
		}
	}
	rows, err := m.d.App.DB.Reader.QueryContext(r.Context(),
		`SELECT p.id, p.event_id, p.category_id, c.name,
		        p.name, p.price, p.cost, p.prep_station, p.track_stock,
		        p.stock_qty, p.stock_version, p.sort_order, p.image_path, p.active, p.created_at
		 FROM products p
		 LEFT JOIN categories c ON c.id = p.category_id
		 WHERE p.event_id = ? AND p.track_stock = 1 AND p.stock_qty <= ?
		 ORDER BY p.stock_qty, p.name`, evID, threshold)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	defer rows.Close()
	out := []Product{}
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.EventID, &p.CategoryID, &p.CategoryName,
			&p.Name, &p.Price, &p.Cost, &p.PrepStation, &p.TrackStock,
			&p.StockQty, &p.StockVersion, &p.SortOrder, &p.ImagePath,
			&p.Active, &p.CreatedAt); err != nil {
			httpx.Error(w, err)
			return
		}
		out = append(out, p)
	}
	httpx.JSON(w, http.StatusOK, out)
}

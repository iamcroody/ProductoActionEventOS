// Package oversight implements the audit-log viewer, tamper-evidence
// verification, and data-integrity check endpoints for Action EventOS.
//
// All routes are read-only and restricted to the admin and auditor roles.
// Reads are served from DB.Reader so they never contend with the writer.
// The integrity check cross-references the immutable ledger sources against
// their cached columns: balance_ledger vs attendees.balance and
// stock_movements vs products.stock_qty.
package oversight

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"

	"eventpos/internal/app"
	"eventpos/internal/db"
	"eventpos/internal/httpx"
	"eventpos/internal/server"
)

// Module mounts the oversight routes.
type Module struct{ d server.Deps }

// New constructs the oversight module.
func New(d server.Deps) *Module { return &Module{d: d} }

// Register wires routes. All routes require the admin or auditor role.
func (m *Module) Register(mux *http.ServeMux, d server.Deps) {
	guard := d.Auth.Middleware("admin", "auditor")
	mux.Handle("GET /api/audit", guard(http.HandlerFunc(m.listAudit)))
	mux.Handle("GET /api/audit/verify", guard(http.HandlerFunc(m.verifyAudit)))
	mux.Handle("GET /api/audit/export.csv", guard(http.HandlerFunc(m.exportCSV)))
	mux.Handle("GET /api/integrity", guard(http.HandlerFunc(m.checkIntegrity)))
}

// AuditRow mirrors one row of audit_log, including chain_hash so the client
// can independently verify the tamper-evident chain.
type AuditRow struct {
	ID            string  `json:"id"`
	EventID       *string `json:"event_id"`
	At            int64   `json:"at"`
	UserID        *string `json:"user_id"`
	OperatorName  *string `json:"operator_name"`
	AttendeeID    *string `json:"attendee_id"`
	IP            *string `json:"ip"`
	Action        string  `json:"action"`
	Entity        *string `json:"entity"`
	EntityID      *string `json:"entity_id"`
	ProductsJSON  *string `json:"products_json"`
	BalanceBefore *int64  `json:"balance_before"`
	BalanceAfter  *int64  `json:"balance_after"`
	DetailJSON    *string `json:"detail_json"`
	PrevAuditID   *string `json:"prev_audit_id"`
	ChainHash     *string `json:"chain_hash"`
}

// listAudit handles GET /api/audit.
// Returns audit_log rows newest-first with optional filter params:
//
//	action      – exact match on the action column
//	user_id     – exact match on user_id
//	attendee_id – exact match on attendee_id
//	limit       – number of rows to return (default 200, capped at 1000)
func (m *Module) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	action := q.Get("action")
	userID := q.Get("user_id")
	attendeeID := q.Get("attendee_id")

	limit := 200
	if ls := q.Get("limit"); ls != "" {
		if n, err := strconv.Atoi(ls); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}

	// Build the query with optional predicates to avoid the N+1 filter problem.
	query := `SELECT a.id, a.event_id, a.at, a.user_id, u.username, a.attendee_id, a.ip,
	          a.action, a.entity, a.entity_id, a.products_json, a.balance_before, a.balance_after,
	          a.detail_json, a.prev_audit_id, a.chain_hash
	          FROM audit_log a LEFT JOIN users u ON u.id = a.user_id WHERE 1=1`
	args := []any{}
	if action != "" {
		query += ` AND a.action = ?`
		args = append(args, action)
	}
	if userID != "" {
		query += ` AND a.user_id = ?`
		args = append(args, userID)
	}
	if attendeeID != "" {
		query += ` AND a.attendee_id = ?`
		args = append(args, attendeeID)
	}
	query += ` ORDER BY a.at DESC, a.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := m.d.App.DB.Reader.QueryContext(r.Context(), query, args...)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	defer rows.Close()

	out := []AuditRow{}
	for rows.Next() {
		var a AuditRow
		if err := rows.Scan(
			&a.ID, &a.EventID, &a.At, &a.UserID, &a.OperatorName, &a.AttendeeID, &a.IP,
			&a.Action, &a.Entity, &a.EntityID, &a.ProductsJSON,
			&a.BalanceBefore, &a.BalanceAfter, &a.DetailJSON,
			&a.PrevAuditID, &a.ChainHash,
		); err != nil {
			httpx.Error(w, err)
			return
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// verifyResult is the JSON body returned by GET /api/audit/verify and
// embedded inside IntegrityResult.Audit.
type verifyResult struct {
	Intact   bool   `json:"intact"`
	BrokenAt string `json:"broken_at"` // empty when intact
}

// verifyAudit handles GET /api/audit/verify.
// Delegates to app.VerifyAuditChain which recomputes every hash in order.
func (m *Module) verifyAudit(w http.ResponseWriter, r *http.Request) {
	brokenAt, err := app.VerifyAuditChain(r.Context(), m.d.App.DB)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, verifyResult{
		Intact:   brokenAt == "",
		BrokenAt: brokenAt,
	})
}

// csvHeader defines the column order for the CSV export, matching audit_log.
var csvHeader = []string{
	"id", "event_id", "at", "user_id", "attendee_id", "ip",
	"action", "entity", "entity_id", "products_json",
	"balance_before", "balance_after", "detail_json",
	"prev_audit_id", "chain_hash",
}

// exportCSV handles GET /api/audit/export.csv.
// Streams audit_log oldest-first as a Content-Disposition attachment so it
// can be replayed. Nullable columns render as empty strings in the CSV.
func (m *Module) exportCSV(w http.ResponseWriter, r *http.Request) {
	rows, err := m.d.App.DB.Reader.QueryContext(r.Context(),
		`SELECT id, event_id, at, user_id, attendee_id, ip, action, entity, entity_id,
		 products_json, balance_before, balance_after, detail_json, prev_audit_id, chain_hash
		 FROM audit_log ORDER BY at ASC, id ASC`)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	defer rows.Close()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="audit_log.csv"`)
	w.WriteHeader(http.StatusOK)

	cw := csv.NewWriter(w)
	_ = cw.Write(csvHeader)

	for rows.Next() {
		var (
			id            string
			eventID       *string
			at            int64
			userID        *string
			attendeeID    *string
			ip            *string
			action        string
			entity        *string
			entityID      *string
			productsJSON  *string
			balanceBefore *int64
			balanceAfter  *int64
			detailJSON    *string
			prevAuditID   *string
			chainHash     *string
		)
		if err := rows.Scan(
			&id, &eventID, &at, &userID, &attendeeID, &ip,
			&action, &entity, &entityID, &productsJSON,
			&balanceBefore, &balanceAfter, &detailJSON,
			&prevAuditID, &chainHash,
		); err != nil {
			// Headers are already sent; skip bad rows rather than leaving a
			// truncated attachment with no indication of the error.
			continue
		}
		_ = cw.Write([]string{
			id,
			derefStr(eventID),
			strconv.FormatInt(at, 10),
			derefStr(userID),
			derefStr(attendeeID),
			derefStr(ip),
			action,
			derefStr(entity),
			derefStr(entityID),
			derefStr(productsJSON),
			derefInt64(balanceBefore),
			derefInt64(balanceAfter),
			derefStr(detailJSON),
			derefStr(prevAuditID),
			derefStr(chainHash),
		})
	}
	cw.Flush()
}

// derefStr returns the value of a nullable string pointer, or "" for nil.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// derefInt64 returns the decimal string of a nullable int64, or "" for nil.
func derefInt64(n *int64) string {
	if n == nil {
		return ""
	}
	return strconv.FormatInt(*n, 10)
}

// balanceMismatch describes a cached-vs-computed discrepancy for one attendee.
type balanceMismatch struct {
	AttendeeID string `json:"attendee_id"`
	Cached     int64  `json:"cached"`
	Computed   int64  `json:"computed"`
}

// stockMismatch describes a cached-vs-computed discrepancy for one product.
type stockMismatch struct {
	ProductID string `json:"product_id"`
	Cached    int64  `json:"cached"`
	Computed  int64  `json:"computed"`
}

// integrityResult is the JSON shape returned by GET /api/integrity.
type integrityResult struct {
	OK                bool              `json:"ok"`
	BalanceMismatches []balanceMismatch `json:"balance_mismatches"`
	StockMismatches   []stockMismatch   `json:"stock_mismatches"`
	Audit             verifyResult      `json:"audit"`
}

// checkIntegrity handles GET /api/integrity.
func (m *Module) checkIntegrity(w http.ResponseWriter, r *http.Request) {
	res, err := computeIntegrity(r.Context(), m.d.App.DB)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

// computeIntegrity recomputes the derived caches from their immutable sources
// and returns any discrepancies together with the audit-chain status.
// It is package-level (not a method) so tests can call it directly.
func computeIntegrity(ctx context.Context, d *db.DB) (integrityResult, error) {
	out := integrityResult{
		BalanceMismatches: []balanceMismatch{},
		StockMismatches:   []stockMismatch{},
	}

	// --- balance check --------------------------------------------------
	// SUM(balance_ledger.amount) must equal the cached attendees.balance.
	// Group by the primary key so a.balance is uniquely determined.
	bRows, err := d.Reader.QueryContext(ctx, `
		SELECT a.id, a.balance, COALESCE(SUM(l.amount), 0)
		FROM attendees a
		LEFT JOIN balance_ledger l ON l.attendee_id = a.id
		GROUP BY a.id
		HAVING a.balance != COALESCE(SUM(l.amount), 0)`)
	if err != nil {
		return out, fmt.Errorf("balance query: %w", err)
	}
	defer bRows.Close()
	for bRows.Next() {
		var mm balanceMismatch
		if err := bRows.Scan(&mm.AttendeeID, &mm.Cached, &mm.Computed); err != nil {
			return out, fmt.Errorf("balance scan: %w", err)
		}
		out.BalanceMismatches = append(out.BalanceMismatches, mm)
	}
	if err := bRows.Err(); err != nil {
		return out, fmt.Errorf("balance rows: %w", err)
	}

	// --- stock check ----------------------------------------------------
	// SUM(stock_movements.delta) must equal the cached products.stock_qty
	// for every product that has track_stock = 1.
	sRows, err := d.Reader.QueryContext(ctx, `
		SELECT p.id, p.stock_qty, COALESCE(SUM(sm.delta), 0)
		FROM products p
		LEFT JOIN stock_movements sm ON sm.product_id = p.id
		WHERE p.track_stock = 1
		GROUP BY p.id
		HAVING p.stock_qty != COALESCE(SUM(sm.delta), 0)`)
	if err != nil {
		return out, fmt.Errorf("stock query: %w", err)
	}
	defer sRows.Close()
	for sRows.Next() {
		var mm stockMismatch
		if err := sRows.Scan(&mm.ProductID, &mm.Cached, &mm.Computed); err != nil {
			return out, fmt.Errorf("stock scan: %w", err)
		}
		out.StockMismatches = append(out.StockMismatches, mm)
	}
	if err := sRows.Err(); err != nil {
		return out, fmt.Errorf("stock rows: %w", err)
	}

	// --- audit chain ----------------------------------------------------
	brokenAt, err := app.VerifyAuditChain(ctx, d)
	if err != nil {
		return out, fmt.Errorf("audit chain: %w", err)
	}
	out.Audit = verifyResult{Intact: brokenAt == "", BrokenAt: brokenAt}

	out.OK = len(out.BalanceMismatches) == 0 &&
		len(out.StockMismatches) == 0 &&
		brokenAt == ""

	return out, nil
}

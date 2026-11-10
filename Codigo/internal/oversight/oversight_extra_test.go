package oversight

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"eventpos/internal/app"
	"eventpos/internal/core"
	"eventpos/internal/db"
	"eventpos/internal/server"
	"eventpos/internal/writer"
)

// newOversightMod constructs a Module wired to d. The oversight package is
// read-only at the handler level; the full *db.DB is needed for verifyAudit
// (which reads from d.Reader) and checkIntegrity.
func newOversightMod(d *db.DB) *Module {
	return New(server.Deps{App: &app.App{DB: d}})
}

// seedMinimalEvent inserts an event (draft) required by attendees/products FKs.
func seedMinimalEvent(t *testing.T, w *writer.Writer, eventID string) {
	t.Helper()
	ctx := context.Background()
	now := core.NowMillis()
	if _, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
		_, err := tx.Exec(
			`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
			 VALUES (?,?,?,?,?,?)`,
			eventID, "Test Event", "draft", 50000, "cascade", now)
		return nil, err
	}); err != nil {
		t.Fatalf("seedMinimalEvent: %v", err)
	}
}

// seedUser inserts a minimal user (needed for WriteAudit user_id references
// and as FK origin for some entities).
func seedUser(t *testing.T, w *writer.Writer, userID string) {
	t.Helper()
	ctx := context.Background()
	now := core.NowMillis()
	if _, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
		_, err := tx.Exec(
			`INSERT INTO users(id, username, role, pin_hash, active, created_at)
			 VALUES (?,?,?,?,1,?)`,
			userID, "user-"+userID, "admin", "argon2id$s$k", now)
		return nil, err
	}); err != nil {
		t.Fatalf("seedUser: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Integrity: stock mismatch
// ---------------------------------------------------------------------------

// TestIntegrityStockMismatch seeds a product whose cached stock_qty (10) does
// not match the sum of its stock_movements (5) and asserts that
// computeIntegrity flags it and returns ok=false.
func TestIntegrityStockMismatch(t *testing.T) {
	d := openTestDB(t)
	w := writer.New(d.Writer)
	defer w.Close()
	ctx := context.Background()
	now := core.NowMillis()

	eventID := core.NewID()
	seedMinimalEvent(t, w, eventID)
	seedUser(t, w, "sys-user")

	productID := core.NewID()
	if _, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
		_, err := tx.Exec(
			`INSERT INTO products(id, event_id, name, price, track_stock, stock_qty, created_at)
			 VALUES (?,?,?,?,1,10,?)`,
			productID, eventID, "Tracked Product", 1000, now)
		return nil, err
	}); err != nil {
		t.Fatalf("insert product: %v", err)
	}

	// Insert a stock movement of delta=5 — cached says 10, so mismatch.
	if _, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
		_, err := tx.Exec(
			`INSERT INTO stock_movements(id, product_id, delta, reason, balance_after, user_id, created_at)
			 VALUES (?,?,?,?,?,?,?)`,
			core.NewID(), productID, 5, "initial", 5, "sys-user", now)
		return nil, err
	}); err != nil {
		t.Fatalf("insert stock movement: %v", err)
	}

	res, err := computeIntegrity(ctx, d)
	if err != nil {
		t.Fatalf("computeIntegrity: %v", err)
	}

	if len(res.StockMismatches) == 0 {
		t.Fatal("expected at least one stock mismatch, got none")
	}
	found := false
	for _, mm := range res.StockMismatches {
		if mm.ProductID != productID {
			continue
		}
		found = true
		if mm.Cached != 10 {
			t.Errorf("stock cached: got %d, want 10", mm.Cached)
		}
		if mm.Computed != 5 {
			t.Errorf("stock computed: got %d, want 5", mm.Computed)
		}
	}
	if !found {
		t.Fatalf("product %q not in stock_mismatches", productID)
	}
	if res.OK {
		t.Fatal("expected ok=false")
	}
}

// ---------------------------------------------------------------------------
// Integrity: both mismatches → ok=false; consistent data → ok=true
// ---------------------------------------------------------------------------

// TestIntegrityBothMismatchesViaHTTP seeds both a balance mismatch and a stock
// mismatch, then calls GET /api/integrity via the handler and asserts that the
// response has ok=false with both mismatch slices non-empty.
func TestIntegrityBothMismatchesViaHTTP(t *testing.T) {
	d := openTestDB(t)
	w := writer.New(d.Writer)
	defer w.Close()
	ctx := context.Background()
	now := core.NowMillis()

	eventID := core.NewID()
	seedMinimalEvent(t, w, eventID)
	seedUser(t, w, "sys-both")

	// Balance mismatch: attendee.balance=200, ledger sums to 100.
	attendeeID := core.NewID()
	if _, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
		_, err := tx.Exec(
			`INSERT INTO attendees(id, event_id, qr_token, display_name, balance, balance_version, created_at)
			 VALUES (?,?,?,?,200,0,?)`,
			attendeeID, eventID, core.NewID(), "Mismatch Alice", now)
		return nil, err
	}); err != nil {
		t.Fatalf("insert attendee: %v", err)
	}
	if _, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
		_, err := tx.Exec(
			`INSERT INTO balance_ledger(id, attendee_id, type, amount, balance_after, user_id, chain_hash, created_at)
			 VALUES (?,?,?,?,?,?,?,?)`,
			core.NewID(), attendeeID, "topup", 100, 100, "sys-both", "h1", now)
		return nil, err
	}); err != nil {
		t.Fatalf("insert ledger: %v", err)
	}

	// Stock mismatch: product.stock_qty=50, movement sum=25.
	productID := core.NewID()
	if _, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
		_, err := tx.Exec(
			`INSERT INTO products(id, event_id, name, price, track_stock, stock_qty, created_at)
			 VALUES (?,?,?,?,1,50,?)`,
			productID, eventID, "Stocked Widget", 500, now)
		return nil, err
	}); err != nil {
		t.Fatalf("insert product: %v", err)
	}
	if _, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
		_, err := tx.Exec(
			`INSERT INTO stock_movements(id, product_id, delta, reason, balance_after, user_id, created_at)
			 VALUES (?,?,?,?,?,?,?)`,
			core.NewID(), productID, 25, "initial", 25, "sys-both", now)
		return nil, err
	}); err != nil {
		t.Fatalf("insert movement: %v", err)
	}

	m := newOversightMod(d)
	req := httptest.NewRequest(http.MethodGet, "/api/integrity", nil)
	rr := httptest.NewRecorder()
	m.checkIntegrity(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("checkIntegrity: got %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	var result integrityResult
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode integrity: %v", err)
	}
	if result.OK {
		t.Fatal("expected ok=false")
	}
	if len(result.BalanceMismatches) == 0 {
		t.Fatal("expected balance_mismatches to be non-empty")
	}
	if len(result.StockMismatches) == 0 {
		t.Fatal("expected stock_mismatches to be non-empty")
	}
}

// TestIntegrityConsistentDataOKViaHTTP seeds data where every cached value
// matches its ledger source and asserts GET /api/integrity returns ok=true.
func TestIntegrityConsistentDataOKViaHTTP(t *testing.T) {
	d := openTestDB(t)
	w := writer.New(d.Writer)
	defer w.Close()
	ctx := context.Background()
	now := core.NowMillis()

	eventID := core.NewID()
	seedMinimalEvent(t, w, eventID)
	seedUser(t, w, "sys-ok")

	// Attendee with balance=0 and no ledger entries → SUM = 0 = cached balance.
	attendeeID := core.NewID()
	if _, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
		_, err := tx.Exec(
			`INSERT INTO attendees(id, event_id, qr_token, display_name, balance, balance_version, created_at)
			 VALUES (?,?,?,?,0,0,?)`,
			attendeeID, eventID, core.NewID(), "Consistent Bob", now)
		return nil, err
	}); err != nil {
		t.Fatalf("insert attendee: %v", err)
	}

	// Product with track_stock=1, stock_qty=0 and no movements → SUM(delta)=0.
	productID := core.NewID()
	if _, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
		_, err := tx.Exec(
			`INSERT INTO products(id, event_id, name, price, track_stock, stock_qty, created_at)
			 VALUES (?,?,?,?,1,0,?)`,
			productID, eventID, "Consistent Product", 100, now)
		return nil, err
	}); err != nil {
		t.Fatalf("insert product: %v", err)
	}

	m := newOversightMod(d)
	req := httptest.NewRequest(http.MethodGet, "/api/integrity", nil)
	rr := httptest.NewRecorder()
	m.checkIntegrity(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("checkIntegrity: got %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	var result integrityResult
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode integrity: %v", err)
	}
	if !result.OK {
		t.Fatalf("expected ok=true; mismatches: balance=%v stock=%v audit=%v",
			result.BalanceMismatches, result.StockMismatches, result.Audit)
	}
	if len(result.BalanceMismatches) != 0 {
		t.Errorf("expected no balance_mismatches, got %v", result.BalanceMismatches)
	}
	if len(result.StockMismatches) != 0 {
		t.Errorf("expected no stock_mismatches, got %v", result.StockMismatches)
	}
	if !result.Audit.Intact {
		t.Errorf("expected audit intact=true, got broken_at=%q", result.Audit.BrokenAt)
	}
}

// ---------------------------------------------------------------------------
// Audit log filtering
// ---------------------------------------------------------------------------

// TestAuditFilterByAction inserts audit rows with different actions and
// verifies that ?action= returns only the matching subset.
func TestAuditFilterByAction(t *testing.T) {
	d := openTestDB(t)
	w := writer.New(d.Writer)
	defer w.Close()
	ctx := context.Background()

	// Write 3 entries: two "sale_complete", one "topup".
	for i, action := range []string{"sale_complete", "sale_complete", "topup"} {
		action := action
		i := i
		if _, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
			return nil, app.WriteAudit(tx, app.AuditEntry{
				Action:   action,
				Entity:   "order",
				EntityID: core.NewID(),
				DetailJSON: func() string {
					if i == 0 {
						return `{"n":0}`
					}
					return `{"n":1}`
				}(),
			})
		}); err != nil {
			t.Fatalf("WriteAudit[%d]: %v", i, err)
		}
	}

	m := newOversightMod(d)

	// ?action=topup must return exactly 1 row.
	req := httptest.NewRequest(http.MethodGet, "/api/audit?action=topup", nil)
	rr := httptest.NewRecorder()
	m.listAudit(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("listAudit ?action=topup: got %d; body: %s", rr.Code, rr.Body.String())
	}
	var rows []AuditRow
	if err := json.Unmarshal(rr.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("?action=topup: got %d rows, want 1", len(rows))
	}
	if rows[0].Action != "topup" {
		t.Errorf("action: got %q, want 'topup'", rows[0].Action)
	}

	// ?action=sale_complete must return exactly 2 rows.
	req2 := httptest.NewRequest(http.MethodGet, "/api/audit?action=sale_complete", nil)
	rr2 := httptest.NewRecorder()
	m.listAudit(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("listAudit ?action=sale_complete: got %d", rr2.Code)
	}
	var rows2 []AuditRow
	if err := json.Unmarshal(rr2.Body.Bytes(), &rows2); err != nil {
		t.Fatalf("decode2: %v", err)
	}
	if len(rows2) != 2 {
		t.Fatalf("?action=sale_complete: got %d rows, want 2", len(rows2))
	}

	// ?action=nonexistent must return 0 rows (empty array, not error).
	req3 := httptest.NewRequest(http.MethodGet, "/api/audit?action=nonexistent", nil)
	rr3 := httptest.NewRecorder()
	m.listAudit(rr3, req3)
	if rr3.Code != http.StatusOK {
		t.Fatalf("listAudit ?action=nonexistent: got %d", rr3.Code)
	}
	var rows3 []AuditRow
	if err := json.Unmarshal(rr3.Body.Bytes(), &rows3); err != nil {
		t.Fatalf("decode3: %v", err)
	}
	if len(rows3) != 0 {
		t.Fatalf("nonexistent action: got %d rows, want 0", len(rows3))
	}
}

// TestAuditFilterByAttendeeID inserts audit rows tied to two attendee IDs and
// verifies that ?attendee_id= returns only the correct subset.
func TestAuditFilterByAttendeeID(t *testing.T) {
	d := openTestDB(t)
	w := writer.New(d.Writer)
	defer w.Close()
	ctx := context.Background()

	attA := "att-filter-aaa"
	attB := "att-filter-bbb"

	// Insert 3 rows: 2 for attA, 1 for attB.
	for _, e := range []app.AuditEntry{
		{Action: "topup", AttendeeID: attA},
		{Action: "debit", AttendeeID: attA},
		{Action: "topup", AttendeeID: attB},
	} {
		e := e
		if _, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
			return nil, app.WriteAudit(tx, e)
		}); err != nil {
			t.Fatalf("WriteAudit: %v", err)
		}
	}

	m := newOversightMod(d)

	// Filter by attA — expect 2 rows.
	req := httptest.NewRequest(http.MethodGet, "/api/audit?attendee_id="+attA, nil)
	rr := httptest.NewRecorder()
	m.listAudit(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("listAudit attA: got %d; body: %s", rr.Code, rr.Body.String())
	}
	var rowsA []AuditRow
	if err := json.Unmarshal(rr.Body.Bytes(), &rowsA); err != nil {
		t.Fatalf("decode A: %v", err)
	}
	if len(rowsA) != 2 {
		t.Fatalf("?attendee_id=attA: got %d rows, want 2", len(rowsA))
	}
	for _, r := range rowsA {
		if r.AttendeeID == nil || *r.AttendeeID != attA {
			t.Errorf("row attendee_id = %v, want %q", r.AttendeeID, attA)
		}
	}

	// Filter by attB — expect 1 row.
	req2 := httptest.NewRequest(http.MethodGet, "/api/audit?attendee_id="+attB, nil)
	rr2 := httptest.NewRecorder()
	m.listAudit(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("listAudit attB: got %d", rr2.Code)
	}
	var rowsB []AuditRow
	if err := json.Unmarshal(rr2.Body.Bytes(), &rowsB); err != nil {
		t.Fatalf("decode B: %v", err)
	}
	if len(rowsB) != 1 {
		t.Fatalf("?attendee_id=attB: got %d rows, want 1", len(rowsB))
	}

	// Combined filter: attA + action=debit → exactly 1.
	req3 := httptest.NewRequest(http.MethodGet, "/api/audit?attendee_id="+attA+"&action=debit", nil)
	rr3 := httptest.NewRecorder()
	m.listAudit(rr3, req3)
	if rr3.Code != http.StatusOK {
		t.Fatalf("combined filter: got %d", rr3.Code)
	}
	var rowsC []AuditRow
	if err := json.Unmarshal(rr3.Body.Bytes(), &rowsC); err != nil {
		t.Fatalf("decode C: %v", err)
	}
	if len(rowsC) != 1 {
		t.Fatalf("attA+debit: got %d rows, want 1", len(rowsC))
	}
	if rowsC[0].Action != "debit" {
		t.Errorf("action: got %q, want 'debit'", rowsC[0].Action)
	}
}

// ---------------------------------------------------------------------------
// Audit chain verify via HTTP
// ---------------------------------------------------------------------------

// TestVerifyAuditChainHTTP writes several audit rows through app.WriteAudit,
// then calls GET /api/audit/verify and asserts intact=true, broken_at="".
func TestVerifyAuditChainHTTP(t *testing.T) {
	d := openTestDB(t)
	w := writer.New(d.Writer)
	defer w.Close()
	ctx := context.Background()

	// Write 5 properly chained audit rows.
	for i := 0; i < 5; i++ {
		i := i
		if _, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
			return nil, app.WriteAudit(tx, app.AuditEntry{
				Action:   "verify_test",
				Entity:   "order",
				EntityID: core.NewID(),
				DetailJSON: func() string {
					return `{"seq":` + string(rune('0'+i)) + `}`
				}(),
			})
		}); err != nil {
			t.Fatalf("WriteAudit[%d]: %v", i, err)
		}
	}

	m := newOversightMod(d)
	req := httptest.NewRequest(http.MethodGet, "/api/audit/verify", nil)
	rr := httptest.NewRecorder()
	m.verifyAudit(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("verifyAudit: got %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	var result verifyResult
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.Intact {
		t.Fatalf("expected intact=true; broken_at=%q", result.BrokenAt)
	}
	if result.BrokenAt != "" {
		t.Errorf("broken_at: got %q, want empty", result.BrokenAt)
	}
}

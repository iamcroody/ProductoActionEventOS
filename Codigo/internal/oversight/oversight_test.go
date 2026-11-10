package oversight

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"eventpos/internal/app"
	"eventpos/internal/core"
	"eventpos/internal/db"
	"eventpos/internal/writer"
)

// openTestDB spins up a real on-disk SQLite (WAL) in a temp dir and registers
// cleanup. Mirrors the helper in internal/writer/writer_test.go.
func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// TestAuditChainIntact writes two audit rows through the writer and verifies
// that app.VerifyAuditChain reports the chain as intact (broken_at == "").
func TestAuditChainIntact(t *testing.T) {
	d := openTestDB(t)
	w := writer.New(d.Writer)
	defer w.Close()
	ctx := context.Background()

	// Write two audit entries; the hash chain must link them.
	for i := 0; i < 2; i++ {
		_, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
			return nil, app.WriteAudit(tx, app.AuditEntry{
				Action: "test_action",
				Entity: "test",
			})
		})
		if err != nil {
			t.Fatalf("WriteAudit[%d]: %v", i, err)
		}
	}

	brokenAt, err := app.VerifyAuditChain(ctx, d)
	if err != nil {
		t.Fatalf("VerifyAuditChain: %v", err)
	}
	if brokenAt != "" {
		t.Fatalf("expected intact chain, broken at %q", brokenAt)
	}
}

// TestIntegrityBalanceMismatch seeds an attendee whose cached balance (100)
// does not match the sum of its balance_ledger entries (50) and asserts that
// computeIntegrity flags it in BalanceMismatches and returns ok=false.
func TestIntegrityBalanceMismatch(t *testing.T) {
	d := openTestDB(t)
	w := writer.New(d.Writer)
	defer w.Close()
	ctx := context.Background()

	now := core.NowMillis()

	// Insert a minimal event required by the attendees foreign key.
	eventID := core.NewID()
	if _, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
		_, err := tx.Exec(
			`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
			 VALUES (?,?,?,?,?,?)`,
			eventID, "Test Event", "draft", 50000, "cascade", now)
		return nil, err
	}); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	// Insert an attendee whose cached balance is 100.
	attendeeID := core.NewID()
	if _, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
		_, err := tx.Exec(
			`INSERT INTO attendees(id, event_id, qr_token, display_name, balance, balance_version, created_at)
			 VALUES (?,?,?,?,?,?,?)`,
			attendeeID, eventID, core.NewID(), "Alice", 100, 0, now)
		return nil, err
	}); err != nil {
		t.Fatalf("insert attendee: %v", err)
	}

	// Insert a ledger entry whose amount is 50, so the computed balance (50)
	// disagrees with the cached column (100) – this is the deliberate mismatch.
	if _, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
		_, err := tx.Exec(
			`INSERT INTO balance_ledger(id, attendee_id, type, amount, balance_after, user_id, chain_hash, created_at)
			 VALUES (?,?,?,?,?,?,?,?)`,
			core.NewID(), attendeeID, "topup", 50, 50, "sys", "testhash", now)
		return nil, err
	}); err != nil {
		t.Fatalf("insert ledger: %v", err)
	}

	res, err := computeIntegrity(ctx, d)
	if err != nil {
		t.Fatalf("computeIntegrity: %v", err)
	}

	// At least one balance mismatch must be reported.
	if len(res.BalanceMismatches) == 0 {
		t.Fatal("expected at least one balance mismatch, got none")
	}

	// Locate the mismatch for our specific attendee.
	found := false
	for _, mm := range res.BalanceMismatches {
		if mm.AttendeeID != attendeeID {
			continue
		}
		found = true
		if mm.Cached != 100 {
			t.Errorf("cached: got %d, want 100", mm.Cached)
		}
		if mm.Computed != 50 {
			t.Errorf("computed: got %d, want 50", mm.Computed)
		}
	}
	if !found {
		t.Fatalf("attendee %q not found in balance_mismatches", attendeeID)
	}

	// The overall ok flag must be false because there is a mismatch.
	if res.OK {
		t.Fatal("expected ok=false")
	}
}

// crash_concurrent_test.go extends the recovery suite with an abrupt-close test
// taken mid-stream of CONCURRENT writes. Where TestWALCrashRecovery drops the
// handles after a single sequential batch, this test fires a burst of
// overlapping sales and top-ups, then — with NO graceful WAL checkpoint — drops
// the server + db handles (simulating a kill / power loss) and reopens the same
// file. It asserts the reopened database is structurally sound (PRAGMA
// integrity/quick check), every committed row survived, no row is torn, the
// derived caches still equal their immutable ledger/movement sources, and the
// audit hash chain is intact.
//
// It reuses the package harness and helpers from recovery_test.go
// (newHarnessAt, assertConsistent, countRows, pragmaText, dumpDB, firstDiff).
package recovery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"eventpos/internal/bootstrap"
	"eventpos/internal/db"
)

// doConcurrent issues one request through the harness cookie-jar client without
// touching *testing.T, so it is safe to call from many goroutines. It returns
// the status code (0 on transport error).
func (h *harness) doConcurrent(method, path string, body any) int {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, r)
	if err != nil {
		return 0
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestCrashDuringConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "aeos.db")
	backupDir := filepath.Join(dir, "backups")

	h := newHarnessAt(t, dbPath, backupDir)
	h.login(bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	// --- Configure a real event while draft, then activate. ---
	evID := h.draftEvent()
	cat := h.createCategory("Bebidas")
	prods := []product{
		h.createProduct(cat, "Cerveza", 8000, 4000, 1_000_000, "bar"),
		h.createProduct(cat, "Agua", 3000, 1000, 1_000_000, "none"),
		h.createProduct(cat, "Hamburguesa", 15000, 9000, 1_000_000, "cocina"),
	}
	const numAtt = 60
	atts := make([]attendee, numAtt)
	for i := 0; i < numAtt; i++ {
		atts[i] = h.createAttendee(fmt.Sprintf("Cliente-%d", i), 1_000_000)
	}
	h.activate(evID)
	h.mustPost("/api/caja/open", map[string]any{"opening_float": 100000}, nil)

	// --- Burst of overlapping sales + top-ups (all committed before we crash). ---
	const burst = 200
	var fivexx int64
	var wg sync.WaitGroup
	startGate := make(chan struct{})
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			att := atts[seed%numAtt]
			<-startGate
			var code int
			if seed%3 == 0 {
				code = h.doConcurrent("POST", "/api/attendees/"+att.ID+"/topup",
					map[string]any{"amount": int64(5000)})
			} else {
				p := prods[seed%len(prods)]
				code = h.doConcurrent("POST", "/api/orders", map[string]any{
					"request_id": fmt.Sprintf("crash-burst-%d", seed),
					"items":      []map[string]any{{"product_id": p.ID, "qty": int64(1 + seed%2)}},
					"payers":     []map[string]any{{"attendee_id": att.ID}},
				})
			}
			if code >= 500 || code == 0 {
				atomic.AddInt64(&fivexx, 1)
			}
		}(i)
	}
	close(startGate)
	wg.Wait()
	if n := atomic.LoadInt64(&fivexx); n != 0 {
		t.Errorf("%d server errors during pre-crash burst", n)
	}

	// Snapshot the committed state the running server reports, then verify the
	// caches agree before we yank the handles.
	assertConsistent(t, h.d, "pre-crash")
	if pragmaText(t, h.d, "PRAGMA integrity_check") != "ok" {
		t.Fatal("pre-crash integrity_check not ok")
	}
	wantDump := dumpDB(t, h.d)
	wantLedger := countRows(t, h.d, "balance_ledger")
	wantOrders := countRows(t, h.d, "orders")
	wantPayments := countRows(t, h.d, "payments")
	wantMovements := countRows(t, h.d, "stock_movements")
	wantAudit := countRows(t, h.d, "audit_log")
	if wantOrders == 0 || wantLedger == 0 || wantPayments == 0 || wantMovements == 0 || wantAudit == 0 {
		t.Fatalf("expected non-empty source tables: ledger=%d orders=%d payments=%d mov=%d audit=%d",
			wantLedger, wantOrders, wantPayments, wantMovements, wantAudit)
	}

	// --- CRASH: drop server + db handles with NO graceful checkpoint. ---
	h.shutdown()

	// --- Reopen the SAME file path. ---
	d2, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	t.Cleanup(func() { d2.Close() })

	// Structurally sound after WAL recovery.
	if got := pragmaText(t, d2, "PRAGMA integrity_check"); got != "ok" {
		t.Fatalf("post-recovery integrity_check: %q, want ok", got)
	}
	if got := pragmaText(t, d2, "PRAGMA quick_check"); got != "ok" {
		t.Fatalf("post-recovery quick_check: %q, want ok", got)
	}

	// Every committed row survived (no loss, no torn rows).
	if got := countRows(t, d2, "balance_ledger"); got != wantLedger {
		t.Fatalf("ledger rows after recovery: got %d, want %d", got, wantLedger)
	}
	if got := countRows(t, d2, "orders"); got != wantOrders {
		t.Fatalf("order rows after recovery: got %d, want %d", got, wantOrders)
	}
	if got := countRows(t, d2, "payments"); got != wantPayments {
		t.Fatalf("payment rows after recovery: got %d, want %d", got, wantPayments)
	}
	if got := countRows(t, d2, "stock_movements"); got != wantMovements {
		t.Fatalf("movement rows after recovery: got %d, want %d", got, wantMovements)
	}
	if got := countRows(t, d2, "audit_log"); got != wantAudit {
		t.Fatalf("audit rows after recovery: got %d, want %d", got, wantAudit)
	}

	// Caches agree with immutable sources; audit chain intact; balances match ledger.
	assertConsistent(t, d2, "post-recovery")

	// Byte-for-byte identical content across the crash.
	if gotDump := dumpDB(t, d2); gotDump != wantDump {
		t.Fatalf("database content changed across crash/recovery:\n%s", firstDiff(wantDump, gotDump))
	}
}

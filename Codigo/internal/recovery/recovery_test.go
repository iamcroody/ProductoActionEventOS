// Package recovery is a durability/recovery test suite for Action EventOS.
//
// It exercises crash-recovery and backup integrity against the real, fully
// wired in-process server (the exact module set from cmd/eventpos) using the
// same harness shape as internal/apitest: db.Open on a temp file, a single
// writer goroutine, realtime hub, config defaults, backup service, auth, the
// app, a seeded admin, all nine modules, and an httptest server driven over
// real HTTP with a cookie-jar client.
//
// The focus is RECOVERY/DURABILITY:
//
//   - A batch of committed sales and top-ups is pushed through the writer over
//     HTTP, the handles are dropped WITHOUT a graceful checkpoint, the same file
//     path is reopened, and we assert the WAL recovered cleanly, every committed
//     row persisted byte-for-byte, the derived caches (cached balance vs ledger,
//     cached stock vs movements) are still consistent, and the audit hash chain
//     is intact.
//   - backup.Create then "restore" into a fresh path (copy the self-contained
//     backup file and reopen it) and assert the data matches row-for-row.
//   - Verify() returns false once a backup file is byte-tampered in place.
package recovery

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/backup"
	"eventpos/internal/bootstrap"
	"eventpos/internal/caja"
	"eventpos/internal/catalog"
	"eventpos/internal/config"
	"eventpos/internal/customers"
	"eventpos/internal/db"
	"eventpos/internal/events"
	"eventpos/internal/kitchen"
	"eventpos/internal/oversight"
	"eventpos/internal/realtime"
	"eventpos/internal/reports"
	"eventpos/internal/sales"
	"eventpos/internal/server"
	"eventpos/internal/users"
	"eventpos/internal/writer"
)

// ---------------------------------------------------------------------------
// Harness — same wiring as internal/apitest, but with stable, caller-supplied
// paths so the *same* database file can be reopened after a simulated crash.
// ---------------------------------------------------------------------------

type harness struct {
	t         *testing.T
	ts        *httptest.Server
	client    *http.Client
	d         *db.DB
	w         *writer.Writer
	backupSvc *backup.Service
	dbPath    string
	backupDir string
	closed    bool
}

// newHarnessAt builds the full server against dbPath. The backup service writes
// into backupDir. It seeds the default admin (idempotent across reopens).
func newHarnessAt(t *testing.T, dbPath, backupDir string) *harness {
	t.Helper()
	ctx := context.Background()

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	w := writer.New(d.Writer)
	hub := realtime.NewHub(64)
	cfg := config.New(d)
	if err := cfg.EnsureDefaults(ctx); err != nil {
		t.Fatalf("ensure defaults: %v", err)
	}
	backupSvc, err := backup.New(d, backupDir)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	authSvc := auth.New(d)

	application := &app.App{DB: d, Writer: w, Hub: hub, DataDir: t.TempDir()}

	if _, _, err := bootstrap.SeedAdmin(ctx, d); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	deps := server.Deps{App: application, Auth: authSvc, Config: cfg, Backup: backupSvc}
	mods := []server.Module{
		users.New(deps), events.New(deps), catalog.New(deps), customers.New(deps),
		caja.New(deps), sales.New(deps), kitchen.New(deps), reports.New(deps),
		oversight.New(deps),
	}

	srv := server.New(deps, fstest.MapFS{}, mods...)
	ts := httptest.NewServer(srv.Handler())

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}

	h := &harness{
		t: t, ts: ts, client: &http.Client{Jar: jar},
		d: d, w: w, backupSvc: backupSvc, dbPath: dbPath, backupDir: backupDir,
	}
	t.Cleanup(h.shutdown)
	return h
}

// shutdown drops the HTTP server and the database handles. It deliberately does
// NOT checkpoint the WAL — this is the "no graceful shutdown" path the recovery
// tests rely on. It is idempotent so the t.Cleanup hook is safe after a manual
// shutdown.
func (h *harness) shutdown() {
	if h.closed {
		return
	}
	h.closed = true
	h.ts.Close()
	h.w.Close()
	_ = h.d.Close()
}

func (h *harness) req(method, path string, body any) (int, []byte) {
	h.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal %s %s: %v", method, path, err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, r)
	if err != nil {
		h.t.Fatalf("new request %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body %s %s: %v", method, path, err)
	}
	return resp.StatusCode, data
}

func (h *harness) mustPost(path string, body, out any) {
	h.t.Helper()
	code, data := h.req("POST", path, body)
	if code != http.StatusOK && code != http.StatusCreated {
		h.t.Fatalf("POST %s: status %d, body %s", path, code, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			h.t.Fatalf("POST %s decode: %v (body %s)", path, err, data)
		}
	}
}

func (h *harness) mustGet(path string, out any) {
	h.t.Helper()
	code, data := h.req("GET", path, nil)
	if code != http.StatusOK {
		h.t.Fatalf("GET %s: status %d, body %s", path, code, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			h.t.Fatalf("GET %s decode: %v (body %s)", path, err, data)
		}
	}
}

func (h *harness) login(username, secret string) {
	h.t.Helper()
	h.mustPost("/api/auth/login", map[string]any{"username": username, "secret": secret}, nil)
}

// ---------------------------------------------------------------------------
// Wire shapes / builders.
// ---------------------------------------------------------------------------

type idResp struct {
	ID string `json:"id"`
}

type attendee struct {
	ID      string `json:"id"`
	QRToken string `json:"qr_token"`
	Balance int64  `json:"balance"`
}

type product struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	StockQty int64  `json:"stock_qty"`
}

type order struct {
	ID    string `json:"id"`
	Total int64  `json:"total"`
}

// draftEvent creates an event (which starts in 'draft' status) and returns its
// id. Under the lock-down lifecycle, configuration (catalog, attendees, top-ups)
// is only allowed while draft; call activate before selling.
func (h *harness) draftEvent() string {
	h.t.Helper()
	var ev idResp
	h.mustPost("/api/events", map[string]any{
		"name": "Recovery Fiesta", "default_topup": 50000, "split_policy": "cascade",
	}, &ev)
	return ev.ID
}

// activate promotes a draft event to 'active' so selling is allowed.
func (h *harness) activate(id string) {
	h.t.Helper()
	h.mustPost("/api/events/"+id+"/activate", map[string]any{}, nil)
}

func (h *harness) createCategory(name string) string {
	h.t.Helper()
	var c idResp
	h.mustPost("/api/categories", map[string]any{"name": name, "sort_order": 0}, &c)
	return c.ID
}

func (h *harness) createProduct(categoryID, name string, price, cost, stock int64, station string) product {
	h.t.Helper()
	var p product
	h.mustPost("/api/products", map[string]any{
		"category_id":  categoryID,
		"name":         name,
		"price":        price,
		"cost":         cost,
		"prep_station": station,
		"track_stock":  1,
		"stock_qty":    stock,
	}, &p)
	return p
}

func (h *harness) createAttendee(name string, topup int64) attendee {
	h.t.Helper()
	var a attendee
	h.mustPost("/api/attendees", map[string]any{
		"display_name": name, "initial_topup": topup,
	}, &a)
	return a
}

// seedBatch drives a representative batch of committed work entirely over HTTP:
// an active event, catalog, stock-tracked products, several funded attendees, an
// open caja, a sale per attendee, and a follow-up top-up per attendee. This
// produces rows in every immutable source table (balance_ledger, payments,
// stock_movements, audit_log) plus the cached columns the integrity check
// recomputes.
func (h *harness) seedBatch() {
	h.t.Helper()

	// --- Configuration phase (event is 'draft'): catalog, attendees, top-ups. ---
	evID := h.draftEvent()
	cat := h.createCategory("Bebidas")
	cerveza := h.createProduct(cat, "Cerveza", 8000, 4000, 100, "bar")
	burger := h.createProduct(cat, "Hamburguesa", 15000, 9000, 100, "cocina")
	agua := h.createProduct(cat, "Agua", 3000, 1000, 100, "none")

	atts := make([]attendee, 5)
	for i := 0; i < 5; i++ {
		atts[i] = h.createAttendee(fmt.Sprintf("Cliente-%d", i), 50000)
		// A follow-up top-up so the ledger has multiple rows per attendee. Top-ups
		// are configuration and must run while the event is draft.
		h.mustPost("/api/attendees/"+atts[i].ID+"/topup", map[string]any{"amount": 10000}, nil)
	}

	// --- Operations phase (event is 'active'): caja + sales. ---
	h.activate(evID)
	h.mustPost("/api/caja/open", map[string]any{"opening_float": 100000}, nil)

	for i := 0; i < 5; i++ {
		var ord order
		h.mustPost("/api/orders", map[string]any{
			"request_id": fmt.Sprintf("batch-%d", i),
			"items": []map[string]any{
				{"product_id": cerveza.ID, "qty": 2},
				{"product_id": burger.ID, "qty": 1},
				{"product_id": agua.ID, "qty": 1},
			},
			"payers": []map[string]any{{"attendee_id": atts[i].ID}},
		}, &ord)
		if ord.Total != 2*8000+15000+3000 {
			h.t.Fatalf("order %d total: got %d, want %d", i, ord.Total, 2*8000+15000+3000)
		}
	}
}

// ---------------------------------------------------------------------------
// Integrity recompute helpers (direct queries against a db handle, mirroring
// oversight.computeIntegrity which is unexported). Used to verify a reopened or
// restored database without needing a running server.
// ---------------------------------------------------------------------------

func balanceMismatches(t *testing.T, d *db.DB) int {
	t.Helper()
	var n int
	err := d.Reader.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT a.id
			FROM attendees a
			LEFT JOIN balance_ledger l ON l.attendee_id = a.id
			GROUP BY a.id
			HAVING a.balance != COALESCE(SUM(l.amount), 0)
		)`).Scan(&n)
	if err != nil {
		t.Fatalf("balance mismatch query: %v", err)
	}
	return n
}

func stockMismatches(t *testing.T, d *db.DB) int {
	t.Helper()
	var n int
	err := d.Reader.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT p.id
			FROM products p
			LEFT JOIN stock_movements sm ON sm.product_id = p.id
			WHERE p.track_stock = 1
			GROUP BY p.id
			HAVING p.stock_qty != COALESCE(SUM(sm.delta), 0)
		)`).Scan(&n)
	if err != nil {
		t.Fatalf("stock mismatch query: %v", err)
	}
	return n
}

// assertConsistent asserts that the derived caches agree with their immutable
// sources and that the audit hash chain is intact.
func assertConsistent(t *testing.T, d *db.DB, label string) {
	t.Helper()
	if n := balanceMismatches(t, d); n != 0 {
		t.Fatalf("%s: %d attendee balance(s) disagree with the ledger", label, n)
	}
	if n := stockMismatches(t, d); n != 0 {
		t.Fatalf("%s: %d product stock(s) disagree with movements", label, n)
	}
	brokenAt, err := app.VerifyAuditChain(context.Background(), d)
	if err != nil {
		t.Fatalf("%s: verify audit chain: %v", label, err)
	}
	if brokenAt != "" {
		t.Fatalf("%s: audit chain broken at %s", label, brokenAt)
	}
}

// pragmaText runs a single-string-result PRAGMA and returns the value.
func pragmaText(t *testing.T, d *db.DB, pragma string) string {
	t.Helper()
	var s string
	if err := d.Reader.QueryRow(pragma).Scan(&s); err != nil {
		t.Fatalf("%s: %v", pragma, err)
	}
	return s
}

// dumpDB returns a deterministic textual dump of every user table (rowid order),
// skipping the named tables. Two dumps comparing equal proves row-for-row data
// equality across a crash/recovery or a backup/restore.
func dumpDB(t *testing.T, d *db.DB, skip ...string) string {
	t.Helper()
	skipSet := map[string]bool{}
	for _, s := range skip {
		skipSet[s] = true
	}

	rows, err := d.Reader.Query(
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatalf("scan table name: %v", err)
		}
		if !skipSet[name] {
			tables = append(tables, name)
		}
	}
	rows.Close()
	sort.Strings(tables)

	var b strings.Builder
	for _, tbl := range tables {
		b.WriteString("== ")
		b.WriteString(tbl)
		b.WriteString(" ==\n")
		b.WriteString(dumpTable(t, d, tbl))
	}
	return b.String()
}

func dumpTable(t *testing.T, d *db.DB, table string) string {
	t.Helper()
	// rowid is a stable, copy-preserved ordering; all EventPOS tables are rowid
	// tables.
	rows, err := d.Reader.Query(fmt.Sprintf("SELECT * FROM %s ORDER BY rowid", table))
	if err != nil {
		t.Fatalf("dump %s: %v", table, err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns %s: %v", table, err)
	}

	var b strings.Builder
	for rows.Next() {
		raw := make([]sql.RawBytes, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		for i, c := range cols {
			if i > 0 {
				b.WriteByte('\x1f')
			}
			b.WriteString(c)
			b.WriteByte('=')
			if raw[i] == nil {
				b.WriteString("<NULL>")
			} else {
				b.Write(raw[i])
			}
		}
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows %s: %v", table, err)
	}
	return b.String()
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, in, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

// ---------------------------------------------------------------------------
// 1. WAL crash recovery: drop the handles without a checkpoint, reopen the same
//    file, and assert nothing committed was lost and the caches stay consistent.
// ---------------------------------------------------------------------------

func TestWALCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "aeos.db")
	backupDir := filepath.Join(dir, "backups")

	h := newHarnessAt(t, dbPath, backupDir)
	h.login(bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)
	h.seedBatch()

	// Sanity: the running server reports the same consistency we will re-check
	// after recovery, and committed rows exist.
	assertConsistent(t, h.d, "pre-crash")
	if pragmaText(t, h.d, "PRAGMA integrity_check") != "ok" {
		t.Fatal("pre-crash integrity_check not ok")
	}

	wantDump := dumpDB(t, h.d)
	wantLedger := countRows(t, h.d, "balance_ledger")
	wantPayments := countRows(t, h.d, "payments")
	wantMovements := countRows(t, h.d, "stock_movements")
	wantOrders := countRows(t, h.d, "orders")
	wantAudit := countRows(t, h.d, "audit_log")
	if wantLedger == 0 || wantPayments == 0 || wantMovements == 0 || wantOrders == 0 || wantAudit == 0 {
		t.Fatalf("expected non-empty source tables: ledger=%d payments=%d mov=%d orders=%d audit=%d",
			wantLedger, wantPayments, wantMovements, wantOrders, wantAudit)
	}

	// Under WAL the committed frames live in the -wal sidecar until a checkpoint.
	// We do NOT checkpoint, proving recovery reads the WAL on reopen. (The file
	// may or may not still exist depending on the driver's connection-close
	// behaviour; either way the data must survive.)
	walPath := dbPath + "-wal"
	if fi, err := os.Stat(walPath); err == nil {
		t.Logf("pre-crash WAL sidecar present: %d bytes", fi.Size())
	}

	// Simulate a crash: drop server + db handles with NO graceful checkpoint.
	h.shutdown()

	// Reopen the SAME file path.
	d2, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	t.Cleanup(func() { d2.Close() })

	// The WAL must have recovered into a structurally sound database.
	if got := pragmaText(t, d2, "PRAGMA integrity_check"); got != "ok" {
		t.Fatalf("post-recovery integrity_check: %q, want ok", got)
	}
	if got := pragmaText(t, d2, "PRAGMA quick_check"); got != "ok" {
		t.Fatalf("post-recovery quick_check: %q, want ok", got)
	}

	// Every committed row persisted.
	if got := countRows(t, d2, "balance_ledger"); got != wantLedger {
		t.Fatalf("ledger rows after recovery: got %d, want %d", got, wantLedger)
	}
	if got := countRows(t, d2, "payments"); got != wantPayments {
		t.Fatalf("payment rows after recovery: got %d, want %d", got, wantPayments)
	}
	if got := countRows(t, d2, "stock_movements"); got != wantMovements {
		t.Fatalf("movement rows after recovery: got %d, want %d", got, wantMovements)
	}
	if got := countRows(t, d2, "orders"); got != wantOrders {
		t.Fatalf("order rows after recovery: got %d, want %d", got, wantOrders)
	}
	if got := countRows(t, d2, "audit_log"); got != wantAudit {
		t.Fatalf("audit rows after recovery: got %d, want %d", got, wantAudit)
	}

	// Caches still agree with the immutable sources; the audit chain is intact.
	assertConsistent(t, d2, "post-recovery")

	// And the database is byte-for-byte the same content as before the crash.
	if gotDump := dumpDB(t, d2); gotDump != wantDump {
		t.Fatalf("database content changed across crash/recovery:\n%s",
			firstDiff(wantDump, gotDump))
	}
}

// ---------------------------------------------------------------------------
// 2. Backup round-trip: Create a backup, "restore" it into a fresh path (copy
//    the self-contained file and reopen), and assert the data matches
//    row-for-row.
// ---------------------------------------------------------------------------

func TestBackupRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "aeos.db")
	backupDir := filepath.Join(dir, "backups")

	h := newHarnessAt(t, dbPath, backupDir)
	h.login(bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)
	h.seedBatch()

	rec, err := h.backupSvc.Create(context.Background(), "manual", "")
	if err != nil {
		t.Fatalf("backup create: %v", err)
	}
	if _, err := os.Stat(rec.Path); err != nil {
		t.Fatalf("backup file missing: %v", err)
	}

	// "Restore": copy the self-contained backup file (it was checkpointed before
	// the copy) into a fresh path and open it as a database.
	restorePath := filepath.Join(t.TempDir(), "restored.db")
	copyFile(t, rec.Path, restorePath)

	rdb, err := db.Open(restorePath)
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	t.Cleanup(func() { rdb.Close() })

	// The restored copy must be structurally sound and internally consistent.
	if got := pragmaText(t, rdb, "PRAGMA integrity_check"); got != "ok" {
		t.Fatalf("restored integrity_check: %q, want ok", got)
	}
	assertConsistent(t, rdb, "restored")

	// Row-for-row equality. The `backups` table is excluded: Create() inserts the
	// backup record into the LIVE db AFTER copying the file, so the copy can never
	// contain its own record.
	want := dumpDB(t, h.d, "backups")
	got := dumpDB(t, rdb, "backups")
	if want != got {
		t.Fatalf("restored data differs from source:\n%s", firstDiff(want, got))
	}

	// The live db is the one that records the backup metadata.
	if n := countRows(t, h.d, "backups"); n != 1 {
		t.Fatalf("live db backups rows: got %d, want 1", n)
	}
	if n := countRows(t, rdb, "backups"); n != 0 {
		t.Fatalf("restored db backups rows: got %d, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// 3. Verify() detects an in-place byte tamper of a backup file.
// ---------------------------------------------------------------------------

func TestBackupVerifyDetectsTamper(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "aeos.db")
	backupDir := filepath.Join(dir, "backups")

	h := newHarnessAt(t, dbPath, backupDir)
	h.login(bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)
	h.seedBatch()

	ctx := context.Background()
	rec, err := h.backupSvc.Create(ctx, "manual", "")
	if err != nil {
		t.Fatalf("backup create: %v", err)
	}

	// Intact backup verifies.
	ok, err := h.backupSvc.Verify(ctx, rec.ID)
	if err != nil {
		t.Fatalf("verify intact: %v", err)
	}
	if !ok {
		t.Fatal("intact backup should verify true")
	}

	// Tamper a single byte IN PLACE (same file size) so only the content hash —
	// not the size — can catch it.
	tamperByteInPlace(t, rec.Path)

	ok, err = h.backupSvc.Verify(ctx, rec.ID)
	if err != nil {
		t.Fatalf("verify tampered: %v", err)
	}
	if ok {
		t.Fatal("byte-tampered backup should verify false")
	}
}

// tamperByteInPlace flips one byte near the middle of the file without changing
// its length.
func tamperByteInPlace(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open for tamper: %v", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("stat for tamper: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatal("backup file is empty; cannot tamper")
	}
	off := fi.Size() / 2
	buf := make([]byte, 1)
	if _, err := f.ReadAt(buf, off); err != nil {
		t.Fatalf("read byte: %v", err)
	}
	buf[0] ^= 0xFF
	if _, err := f.WriteAt(buf, off); err != nil {
		t.Fatalf("write byte: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Small helpers.
// ---------------------------------------------------------------------------

func countRows(t *testing.T, d *db.DB, table string) int {
	t.Helper()
	var n int
	if err := d.Reader.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// firstDiff returns a compact view of where two dumps first diverge, for
// readable failure messages.
func firstDiff(a, b string) string {
	la := strings.Split(a, "\n")
	lb := strings.Split(b, "\n")
	n := len(la)
	if len(lb) < n {
		n = len(lb)
	}
	for i := 0; i < n; i++ {
		if la[i] != lb[i] {
			return fmt.Sprintf("line %d:\n  want: %q\n  got:  %q", i+1, la[i], lb[i])
		}
	}
	if len(la) != len(lb) {
		return fmt.Sprintf("line counts differ: want %d, got %d", len(la), len(lb))
	}
	return "(no line-level diff found)"
}

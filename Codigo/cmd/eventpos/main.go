// Command eventpos is the Action EventOS server — a single self-contained
// binary that runs the whole event-operations console locally. On start it
// initializes the database, detects the LAN IP, prints a QR code that any
// phone on the same network can scan, and opens the admin console.
//
//	"No desarrollamos software. Diseñamos tecnología que mejora la experiencia humana."
//	                                                          — CROODY
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/backup"
	"eventpos/internal/bootstrap"
	"eventpos/internal/clientapi"
	"eventpos/internal/config"
	"eventpos/internal/db"
	"eventpos/internal/monitor"
	"eventpos/internal/netutil"
	"eventpos/internal/realtime"
	"eventpos/internal/server"
	"eventpos/internal/writer"
	webassets "eventpos/web"
)

func main() {
	port := flag.Int("port", 8080, "puerto de la consola de gestión (staff)")
	clientPort := flag.Int("client-port", 8090, "puerto de la web de clientes (QR) — aislado de gestión")
	dataDir := flag.String("data", "data", "carpeta de datos y respaldos")
	openBrowser := flag.Bool("open", true, "abrir el navegador automáticamente")
	adminKey := flag.String("admin-key", "", "llave de acceso a gestión (vacío = aleatoria por arranque)")
	flag.Parse()

	key := *adminKey
	if key == "" {
		key = os.Getenv("AEOS_ADMIN_KEY")
	}
	if err := run(*port, *clientPort, *dataDir, *openBrowser, key); err != nil {
		log.Fatalf("Action EventOS: %v", err)
	}
}

func run(port, clientPort int, dataDir string, openBrowser bool, adminKey string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	dbPath := filepath.Join(dataDir, "eventpos.db")

	database, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer database.Close()

	w := writer.New(database.Writer)
	defer w.Close()

	hub := realtime.NewHub(1024)
	cfg := config.New(database)
	if err := cfg.EnsureDefaults(ctx); err != nil {
		return err
	}
	backupSvc, err := backup.New(database, filepath.Join(dataDir, "backups"))
	if err != nil {
		return err
	}
	authSvc := auth.New(database)

	ip := netutil.LANIP()
	clientBaseURL := fmt.Sprintf("http://%s:%d", ip, clientPort)
	application := &app.App{DB: database, Writer: w, Hub: hub, DataDir: dataDir, ClientBaseURL: clientBaseURL}

	// First-run admin (usuario "action" / contraseña "croody").
	if user, secret, err := bootstrap.SeedAdmin(ctx, database); err != nil {
		return err
	} else if user != "" {
		log.Printf("Cuenta de administrador: usuario \"%s\"  contraseña \"%s\"  (cámbiala en Operadores).", user, secret)
	}

	deps := server.Deps{App: application, Auth: authSvc, Config: cfg, Backup: backupSvc}

	// Startup integrity check: recompute cached balances/stock from the immutable
	// ledgers and log any drift (defense against a tampered/copied database).
	logIntegrity(ctx, database)

	// Startup backup → there is always a restore point from the moment we open.
	if _, err := backupSvc.Create(ctx, "auto", ""); err != nil {
		log.Printf("respaldo de arranque: %v", err)
	}
	// Periodic auto-backup so a power cut mid-event never loses more than a few
	// minutes of data (on top of the on-start, on-event-close and on-shutdown).
	stopBackups := startPeriodicBackup(backupSvc, 10*time.Minute)
	defer stopBackups()

	// serveCliente serves the single self-contained customer web page (keeps the
	// ?qr= query for the page's JS to read).
	clienteFS := http.FileServer(http.FS(webassets.ClienteFS()))
	serveCliente := func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		clienteFS.ServeHTTP(w, r2)
	}

	// Access key: makes the management surface INVISIBLE (404) to anyone on the
	// LAN who doesn't present it. Generated fresh per run unless provided. The
	// host machine (loopback) is always allowed; staff phones use the keyed QR.
	if adminKey == "" {
		adminKey = randomKey()
	}
	// Expose the keyed staff URL to admins (for the in-app "Acceso staff" QR).
	application.StaffURL = fmt.Sprintf("http://%s:%d/?k=%s", ip, port, adminKey)

	// Live network monitor: one shared recorder wraps BOTH servers so the
	// operator sees every request on the LAN (staff + cliente) in real time.
	monitorRec := monitor.NewRecorder(application.Hub)

	// --- Management server (staff): SPA + operator API (+ /api/cliente for dev). ---
	mgmt := server.New(deps, webassets.FS(), modules(deps)...)
	mgmtRoot := http.NewServeMux()
	monitor.NewModule(monitorRec).Register(mgmtRoot, deps) // admin-only /api/monitor/*
	mgmtRoot.HandleFunc("/cliente", serveCliente)
	mgmtRoot.HandleFunc("/cliente/", serveCliente)
	mgmtRoot.Handle("/", mgmt.Handler())
	// headers → record traffic → gate (404 black-hole without the key) → routes.
	mgmtSrv := &http.Server{Addr: fmt.Sprintf(":%d", port),
		Handler: securityHeaders(monitorRec.Middleware("staff")(gate(adminKey, mgmtRoot)))}

	// --- Customer server (separate port): ONLY the client web + /api/cliente/*.
	// It never serves the management SPA or the operator API, so a customer with
	// the QR URL cannot reach the console even by guessing paths. ---
	clientRoot := http.NewServeMux()
	clientapi.New(deps).Register(clientRoot, deps)
	// Unknown /api/* on the client port → clean 404 JSON (not the SPA fallback),
	// so typo'd/probed API paths don't return HTML and aren't fingerprintable.
	clientRoot.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no encontrado"}`))
	})
	clientRoot.HandleFunc("/", serveCliente) // QR opens here; everything else → client web
	clientSrv := &http.Server{Addr: fmt.Sprintf(":%d", clientPort),
		Handler: securityHeaders(monitorRec.Middleware("cliente")(clientRoot))}

	staffURL := fmt.Sprintf("http://%s:%d/?k=%s", ip, port, adminKey)
	printBanner(staffURL, clientBaseURL)

	go func() {
		if err := mgmtSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("servidor gestión: %v", err)
		}
	}()
	go func() {
		if err := clientSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("servidor cliente: %v", err)
		}
	}()

	if openBrowser {
		go launchBrowser(fmt.Sprintf("http://localhost:%d", port))
	}

	<-ctx.Done()
	log.Println("Cerrando Action EventOS…")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Best-effort safety backup on shutdown.
	if _, err := backupSvc.Create(shutCtx, "auto", ""); err != nil {
		log.Printf("respaldo de cierre: %v", err)
	}
	_ = clientSrv.Shutdown(shutCtx)
	return mgmtSrv.Shutdown(shutCtx)
}

// startPeriodicBackup runs an automatic backup every interval until the returned
// stop function is called. Failures are logged, never fatal.
func startPeriodicBackup(b *backup.Service, interval time.Duration) func() {
	t := time.NewTicker(interval)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-t.C:
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if _, err := b.Create(ctx, "auto", ""); err != nil {
					log.Printf("respaldo periódico: %v", err)
				}
				if err := b.Prune(ctx, 24); err != nil {
					log.Printf("limpieza de respaldos: %v", err)
				}
				cancel()
			case <-done:
				t.Stop()
				return
			}
		}
	}()
	return func() { close(done) }
}

// logIntegrity recomputes cached balances/stock from the immutable ledgers and
// logs any drift. A clean DB logs nothing; drift signals tampering or a bug.
func logIntegrity(ctx context.Context, d *db.DB) {
	var balBad, stockBad int
	_ = d.Reader.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM attendees a
		WHERE a.balance <> (SELECT COALESCE(SUM(amount),0) FROM balance_ledger WHERE attendee_id = a.id)
	`).Scan(&balBad)
	_ = d.Reader.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM products p WHERE p.track_stock = 1
		AND p.stock_qty <> (SELECT COALESCE(SUM(delta),0) FROM stock_movements WHERE product_id = p.id)
	`).Scan(&stockBad)
	if balBad > 0 || stockBad > 0 {
		log.Printf("ADVERTENCIA integridad: %d saldos y %d stocks no cuadran con el ledger (posible manipulación).", balBad, stockBad)
	}
}

// printBanner renders the startup banner with the access URL and an ASCII QR
// that an operator can scan from a phone.
// randomKey returns a 192-bit URL-safe random access key.
func randomKey() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// isLoopback reports whether the request came from the host machine itself.
func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// gate makes the management surface invisible to anyone without the access key.
// Loopback (the host PC) is always allowed. A LAN device must present ?k=<key>
// once (via the keyed staff QR); a Strict, HttpOnly cookie then carries it.
// Anything else gets a plain 404 — indistinguishable from "nothing here", so a
// curious client who guesses the port finds no login page and nothing to attack.
func gate(key string, next http.Handler) http.Handler {
	want := []byte(key)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isLoopback(r) {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie("aeos_gate"); err == nil &&
			subtle.ConstantTimeCompare([]byte(c.Value), want) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		if k := r.URL.Query().Get("k"); k != "" &&
			subtle.ConstantTimeCompare([]byte(k), want) == 1 {
			http.SetCookie(w, &http.Cookie{
				Name: "aeos_gate", Value: key, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteStrictMode,
			})
			http.Redirect(w, r, r.URL.Path, http.StatusFound) // strip key from the URL bar
			return
		}
		http.NotFound(w, r)
	})
}

// securityHeaders applies conservative hardening headers to every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func printBanner(mgmtURL, clientURL string) {
	fmt.Println()
	fmt.Println("  ╔══════════════════════════════════════════════╗")
	fmt.Println("  ║              ACTION  EVENTOS                  ║")
	fmt.Println("  ║          Consola de operación de eventos      ║")
	fmt.Println("  ╚══════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Printf("  Servidor iniciado.\n\n")

	// STAFF QR — PRIVATE: carries the secret access key. Without it the gestión
	// port responds 404 to every other device, so the console is invisible.
	fmt.Println("  ── STAFF (PRIVADO — solo muéstralo a tu personal) ──")
	fmt.Printf("  %s\n", mgmtURL)
	if qr, err := qrcode.New(mgmtURL, qrcode.Medium); err == nil {
		fmt.Println(qr.ToSmallString(false))
	}

	// CLIENT QR — PUBLIC: project/print it; isolated port, no management access.
	fmt.Println("  ── CLIENTES (PÚBLICO — proyéctalo o imprímelo) ──")
	fmt.Printf("  %s\n", clientURL)
	if qr, err := qrcode.New(clientURL, qrcode.Medium); err == nil {
		fmt.Println(qr.ToSmallString(false))
	}
	fmt.Printf("  %s\n\n", app.Signature)
}

// launchBrowser opens the default browser at url (best effort).
func launchBrowser(url string) {
	time.Sleep(400 * time.Millisecond)
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

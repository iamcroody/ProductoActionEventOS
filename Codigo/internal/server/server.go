// Package server wires the HTTP layer for Action EventOS: the embedded SPA
// with history-API fallback, the SSE stream, and the core platform routes
// (health, auth, config, backups). Domain modules implement the Module
// interface and are mounted here, keeping route registration in one place.
package server

import (
	"io/fs"
	"net/http"
	"strings"

	qrcode "github.com/skip2/go-qrcode"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/backup"
	"eventpos/internal/config"
	"eventpos/internal/httpx"
)

// Module is implemented by every domain package. Register adds the module's
// routes to the mux. Deps carries shared services.
type Module interface {
	Register(mux *http.ServeMux, deps Deps)
}

// Deps is the shared service set handed to each module.
type Deps struct {
	App    *app.App
	Auth   *auth.Service
	Config *config.Store
	Backup *backup.Service
}

// Server holds the root handler.
type Server struct {
	mux  *http.ServeMux
	deps Deps
}

// New builds the server, mounts the static UI, core routes, and modules.
func New(deps Deps, staticFS fs.FS, modules ...Module) *Server {
	s := &Server{mux: http.NewServeMux(), deps: deps}
	s.mountStatic(staticFS)
	s.mountCore()
	for _, m := range modules {
		m.Register(s.mux, deps)
	}
	return s
}

// Handler returns the root http.Handler.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) mountCore() {
	s.mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]any{
			"ok": true, "product": app.ProductName, "signature": app.Signature,
		})
	})

	// Auth
	s.mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	s.mux.Handle("GET /api/auth/me", s.deps.Auth.Middleware()(http.HandlerFunc(s.handleMe)))

	// Live stream (auth required; any role).
	s.mux.Handle("GET /api/stream", s.deps.Auth.Middleware()(http.HandlerFunc(s.deps.App.Hub.ServeHTTP)))

	// Customer-access info + QR (the event-wide QR attendees scan to self-order).
	s.mux.Handle("GET /api/client-info", s.deps.Auth.Middleware()(http.HandlerFunc(s.handleClientInfo)))
	s.mux.Handle("GET /api/client-qr.png", s.deps.Auth.Middleware()(http.HandlerFunc(s.handleClientQR)))

	// Staff-access QR (PRIVATE: carries the gate key). Admin only — used to
	// onboard staff phones by scanning it off the manager's screen.
	s.mux.Handle("GET /api/staff-info", s.deps.Auth.Middleware("admin")(http.HandlerFunc(s.handleStaffInfo)))
	s.mux.Handle("GET /api/staff-qr.png", s.deps.Auth.Middleware("admin")(http.HandlerFunc(s.handleStaffQR)))

	// Config (read: any; write: admin).
	s.mux.Handle("GET /api/config", s.deps.Auth.Middleware()(http.HandlerFunc(s.handleConfigGet)))
	s.mux.Handle("PUT /api/config", s.deps.Auth.Middleware("admin")(http.HandlerFunc(s.handleConfigSet)))

	// Backups (admin).
	s.mux.Handle("GET /api/backups", s.deps.Auth.Middleware("admin")(http.HandlerFunc(s.handleBackupList)))
	s.mux.Handle("POST /api/backups", s.deps.Auth.Middleware("admin")(http.HandlerFunc(s.handleBackupCreate)))
	s.mux.Handle("POST /api/backups/verify", s.deps.Auth.Middleware("admin")(http.HandlerFunc(s.handleBackupVerify)))
}

// mountStatic serves the embedded SPA with history-API fallback: unknown,
// non-API GET paths return index.html so client-side routing works.
func (s *Server) mountStatic(staticFS fs.FS) {
	fileServer := http.FileServer(http.FS(staticFS))
	s.mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		if _, err := fs.Stat(staticFS, strings.TrimPrefix(r.URL.Path, "/")); err != nil && r.URL.Path != "/" {
			// SPA fallback to index.html.
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

// --- core handlers ---

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Secret   string `json:"secret"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}
	token, u, err := s.deps.Auth.Login(r.Context(), in.Username, in.Secret, httpx.ClientIP(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	auth.SetCookie(w, token)
	httpx.JSON(w, http.StatusOK, u)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.deps.Auth.Logout(r.Context(), auth.TokenFromRequest(r))
	auth.ClearCookie(w)
	httpx.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	httpx.JSON(w, http.StatusOK, u)
}

// handleClientInfo returns the customer-web base URL (separate port) so the SPA
// can show it next to the QR for staff to project.
func (s *Server) handleClientInfo(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]string{"client_url": s.deps.App.ClientBaseURL})
}

// handleClientQR returns a PNG QR of the customer-web URL (the one attendees
// scan to self-order). Projecting/printing this lets clients access the
// isolated customer port without any management exposure.
func (s *Server) handleClientQR(w http.ResponseWriter, r *http.Request) {
	png, err := qrcode.Encode(s.deps.App.ClientBaseURL, qrcode.Medium, 512)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

// handleStaffInfo returns the PRIVATE keyed management URL (admin only) so the
// app can show it for onboarding staff phones.
func (s *Server) handleStaffInfo(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]string{"staff_url": s.deps.App.StaffURL})
}

// handleStaffQR returns a PNG QR of the keyed management URL (admin only). A
// staff phone scans it (off the manager's screen) to pass the access gate.
func (s *Server) handleStaffQR(w http.ResponseWriter, r *http.Request) {
	png, err := qrcode.Encode(s.deps.App.StaffURL, qrcode.Medium, 512)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	all, err := s.deps.Config.All(r.Context())
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, all)
}

func (s *Server) handleConfigSet(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	var in map[string]string
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}
	for k, v := range in {
		if err := s.deps.Config.Set(r.Context(), k, v, u.ID); err != nil {
			httpx.Error(w, err)
			return
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleBackupList(w http.ResponseWriter, r *http.Request) {
	recs, err := s.deps.Backup.List(r.Context())
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, recs)
}

func (s *Server) handleBackupCreate(w http.ResponseWriter, r *http.Request) {
	rec, err := s.deps.Backup.Create(r.Context(), "manual", "")
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, rec)
}

func (s *Server) handleBackupVerify(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}
	ok, err := s.deps.Backup.Verify(r.Context(), in.ID)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]bool{"valid": ok})
}

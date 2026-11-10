package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// okHandler is the protected target; reaching it means the gate allowed through.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func TestGate_BlackHoleWithoutKey(t *testing.T) {
	h := gate("secret-key", okHandler())
	for _, path := range []string{"/", "/login", "/api/health", "/api/users"} {
		r := httptest.NewRequest(http.MethodGet, path, nil) // RemoteAddr = 192.0.2.1 (non-loopback)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("path %s without key: got %d, want 404 (black hole)", path, w.Code)
		}
	}
}

func TestGate_WrongKeyIs404(t *testing.T) {
	h := gate("secret-key", okHandler())
	r := httptest.NewRequest(http.MethodGet, "/?k=nope", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("wrong key: got %d, want 404", w.Code)
	}
}

func TestGate_ValidKeySetsCookieAndRedirects(t *testing.T) {
	h := gate("secret-key", okHandler())
	r := httptest.NewRequest(http.MethodGet, "/admin?k=secret-key", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("valid key: got %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/admin" {
		t.Fatalf("redirect Location = %q, want /admin (key stripped)", loc)
	}
	var found bool
	for _, c := range w.Result().Cookies() {
		if c.Name == "aeos_gate" && c.Value == "secret-key" && c.HttpOnly {
			found = true
		}
	}
	if !found {
		t.Fatal("valid key did not set an HttpOnly aeos_gate cookie")
	}
}

func TestGate_ValidCookiePasses(t *testing.T) {
	h := gate("secret-key", okHandler())
	r := httptest.NewRequest(http.MethodGet, "/api/users", nil)
	r.AddCookie(&http.Cookie{Name: "aeos_gate", Value: "secret-key"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("valid cookie: got %d, want 200 (passed through)", w.Code)
	}
}

func TestGate_LoopbackBypasses(t *testing.T) {
	h := gate("secret-key", okHandler())
	r := httptest.NewRequest(http.MethodGet, "/api/users", nil)
	r.RemoteAddr = "127.0.0.1:54321" // the host machine
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("loopback: got %d, want 200 (host always allowed)", w.Code)
	}
}

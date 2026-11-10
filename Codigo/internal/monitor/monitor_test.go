package monitor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"eventpos/internal/realtime"
)

func newRec() *Recorder { return NewRecorder(realtime.NewHub(64)) }

// fire sends one request with the given remote IP through the middleware.
func fire(rec *Recorder, kind, ip, method, path string, status int) {
	h := rec.Middleware(kind)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = ip + ":40000"
	h.ServeHTTP(httptest.NewRecorder(), r)
}

func TestRecord_ClassifiesAndCounts(t *testing.T) {
	rec := newRec()
	fire(rec, "staff", "192.168.1.50", "GET", "/api/orders", 200)
	fire(rec, "cliente", "192.168.1.77", "POST", "/api/cliente/pedido", 201)
	fire(rec, "staff", "127.0.0.1", "GET", "/api/events", 200) // loopback → host
	fire(rec, "cliente", "192.168.1.77", "GET", "/api/cliente/menu", 404)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.total != 4 {
		t.Fatalf("total = %d, want 4", rec.total)
	}
	if rec.byKind["staff"] != 1 || rec.byKind["cliente"] != 2 || rec.byKind["host"] != 1 {
		t.Fatalf("byKind = %#v, want staff1 cliente2 host1", rec.byKind)
	}
	if rec.byStatus["2xx"] != 3 || rec.byStatus["4xx"] != 1 {
		t.Fatalf("byStatus = %#v, want 2xx3 4xx1", rec.byStatus)
	}
	if len(rec.devices) != 3 { // 3 distinct IPs
		t.Fatalf("devices = %d, want 3", len(rec.devices))
	}
	if d := rec.devices["127.0.0.1"]; d == nil || d.Kind != "host" {
		t.Fatalf("loopback device kind = %v, want host", d)
	}
	if d := rec.devices["192.168.1.77"]; d == nil || d.Count != 2 {
		t.Fatalf("client device count = %v, want 2", d)
	}
}

func TestMiddleware_SkipsMonitorAndStream(t *testing.T) {
	rec := newRec()
	fire(rec, "staff", "192.168.1.9", "GET", "/api/monitor/traffic", 200)
	fire(rec, "staff", "192.168.1.9", "GET", "/api/stream", 200)
	fire(rec, "staff", "192.168.1.9", "GET", "/api/orders", 200)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.total != 1 {
		t.Fatalf("total = %d, want 1 (monitor + stream skipped)", rec.total)
	}
}

func TestRing_CapEnforcedAndNewestFirst(t *testing.T) {
	rec := newRec()
	for i := 0; i < ringCap+50; i++ {
		fire(rec, "staff", "10.0.0.1", "GET", fmt.Sprintf("/api/x/%d", i), 200)
	}
	m := &Module{rec: rec}
	w := httptest.NewRecorder()
	m.traffic(w, httptest.NewRequest("GET", "/api/monitor/traffic", nil))
	var out []Event
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != ringCap {
		t.Fatalf("ring len = %d, want %d (capped)", len(out), ringCap)
	}
	// newest-first: the last fired path must be first.
	if out[0].Path != fmt.Sprintf("/api/x/%d", ringCap+49) {
		t.Fatalf("newest-first broken: head = %s", out[0].Path)
	}
}

func TestStats_PerMinAndActive(t *testing.T) {
	rec := newRec()
	fire(rec, "staff", "192.168.1.2", "GET", "/api/orders", 200)
	fire(rec, "cliente", "192.168.1.3", "GET", "/api/cliente/menu", 200)
	m := &Module{rec: rec}
	w := httptest.NewRecorder()
	m.stats(w, httptest.NewRequest("GET", "/api/monitor/stats", nil))
	var s map[string]any
	if err := json.NewDecoder(w.Body).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s["total"].(float64) != 2 || s["per_min"].(float64) != 2 || s["active"].(float64) != 2 {
		t.Fatalf("stats = %#v, want total/per_min/active all 2", s)
	}
}

func TestDevices_SortedByLastSeen(t *testing.T) {
	rec := newRec()
	fire(rec, "staff", "192.168.1.10", "GET", "/api/orders", 200)
	fire(rec, "cliente", "192.168.1.20", "GET", "/api/cliente/menu", 200)
	// Force a distinct, newer last_seen so the ordering assertion is not subject
	// to sub-millisecond ties between the two fires above.
	rec.mu.Lock()
	rec.devices["192.168.1.20"].LastSeen = rec.devices["192.168.1.10"].LastSeen + 1000
	rec.mu.Unlock()
	m := &Module{rec: rec}
	w := httptest.NewRecorder()
	m.devices(w, httptest.NewRequest("GET", "/api/monitor/devices", nil))
	var out []Device
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 2 || out[0].IP != "192.168.1.20" {
		t.Fatalf("devices not sorted by last_seen: %#v", out)
	}
}

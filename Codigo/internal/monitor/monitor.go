// Package monitor gives the operator a live view of ALL network traffic on the
// event LAN: every request to the management console and to the customer web,
// who made it (device IP + kind), the path, status and latency. It is built as
// a middleware that wraps both HTTP servers, keeps a small in-memory ring of
// recent requests + per-device stats, and publishes each request live over the
// existing SSE hub (topic "monitor") so the "Red en vivo" admin page updates in
// real time. It records only metadata (method/path/status/ip/ms) — never bodies
// or PII — so it is safe to surface to the operator.
package monitor

import (
	"encoding/json"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"eventpos/internal/httpx"
	"eventpos/internal/realtime"
	"eventpos/internal/server"
)

// monitorTopic is the SSE topic the live feed is published on.
const monitorTopic = "monitor"

// ringCap bounds the in-memory recent-request buffer.
const ringCap = 400

// Event is one observed request (metadata only).
type Event struct {
	At     int64  `json:"at"`
	IP     string `json:"ip"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	DurMs  int64  `json:"dur_ms"`
	Kind   string `json:"kind"` // staff | cliente | host
}

// Device is one network device seen on the LAN, with rolled-up stats.
type Device struct {
	IP        string `json:"ip"`
	Kind      string `json:"kind"`
	Count     int64  `json:"count"`
	FirstSeen int64  `json:"first_seen"`
	LastSeen  int64  `json:"last_seen"`
}

// Recorder accumulates traffic and fans it out live. Safe for concurrent use.
type Recorder struct {
	hub *realtime.Hub

	mu       sync.Mutex
	ring     []Event
	devices  map[string]*Device
	total    int64
	byKind   map[string]int64
	byStatus map[string]int64
}

// NewRecorder builds a recorder that publishes live events on hub.
func NewRecorder(hub *realtime.Hub) *Recorder {
	return &Recorder{
		hub:      hub,
		devices:  map[string]*Device{},
		byKind:   map[string]int64{},
		byStatus: map[string]int64{},
	}
}

// statusWriter captures the response status (and keeps Flush working for any
// streaming handler that happens to be wrapped).
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(c int) {
	if !s.wrote {
		s.status = c
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(c)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status = http.StatusOK
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Middleware wraps next, recording every request with the given kind label
// ("staff" for the management port, "cliente" for the customer port). The
// monitor's own endpoints and the SSE stream are passed through untouched (and
// unrecorded) so the feed never reports itself.
func (rec *Recorder) Middleware(kind string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/monitor") || r.URL.Path == "/api/stream" {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			rec.record(kind, r, sw.status, time.Since(start))
		})
	}
}

func ipOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func deviceKind(kind, ip string) string {
	if p := net.ParseIP(ip); p != nil && p.IsLoopback() {
		return "host"
	}
	return kind
}

func (rec *Recorder) record(kind string, r *http.Request, status int, dur time.Duration) {
	ip := ipOf(r)
	now := time.Now().UnixMilli()
	dk := deviceKind(kind, ip)
	ev := Event{At: now, IP: ip, Method: r.Method, Path: r.URL.Path, Status: status, DurMs: dur.Milliseconds(), Kind: dk}

	rec.mu.Lock()
	rec.ring = append(rec.ring, ev)
	if len(rec.ring) > ringCap {
		rec.ring = rec.ring[len(rec.ring)-ringCap:]
	}
	rec.total++
	rec.byKind[dk]++
	switch {
	case status >= 500:
		rec.byStatus["5xx"]++
	case status >= 400:
		rec.byStatus["4xx"]++
	default:
		rec.byStatus["2xx"]++
	}
	d := rec.devices[ip]
	if d == nil {
		d = &Device{IP: ip, Kind: dk, FirstSeen: now}
		rec.devices[ip] = d
	}
	d.Kind = dk
	d.Count++
	d.LastSeen = now
	rec.mu.Unlock()

	if b, err := json.Marshal(ev); err == nil {
		rec.hub.Publish(monitorTopic, "req", string(b))
	}
}

// --- HTTP module (admin-only read endpoints) ---

// Module exposes the monitor's read endpoints. It is registered directly in main
// with the shared Recorder (which also wraps both servers).
type Module struct{ rec *Recorder }

// NewModule wires the recorder's endpoints into a server.Module.
func NewModule(rec *Recorder) *Module { return &Module{rec: rec} }

// Register mounts the admin-only monitor endpoints.
func (m *Module) Register(mux *http.ServeMux, d server.Deps) {
	admin := d.Auth.Middleware("admin")
	mux.Handle("GET /api/monitor/traffic", admin(http.HandlerFunc(m.traffic)))
	mux.Handle("GET /api/monitor/devices", admin(http.HandlerFunc(m.devices)))
	mux.Handle("GET /api/monitor/stats", admin(http.HandlerFunc(m.stats)))
}

// traffic returns the recent requests, newest first.
func (m *Module) traffic(w http.ResponseWriter, r *http.Request) {
	m.rec.mu.Lock()
	out := make([]Event, len(m.rec.ring))
	for i, e := range m.rec.ring {
		out[len(m.rec.ring)-1-i] = e // reverse → newest first
	}
	m.rec.mu.Unlock()
	httpx.JSON(w, http.StatusOK, out)
}

// devices returns known devices, most-recently-active first.
func (m *Module) devices(w http.ResponseWriter, r *http.Request) {
	m.rec.mu.Lock()
	out := make([]Device, 0, len(m.rec.devices))
	for _, d := range m.rec.devices {
		out = append(out, *d)
	}
	m.rec.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen > out[j].LastSeen })
	httpx.JSON(w, http.StatusOK, out)
}

// stats returns rolled-up counters for the dashboard band.
func (m *Module) stats(w http.ResponseWriter, r *http.Request) {
	cutoff := time.Now().UnixMilli() - 60_000
	m.rec.mu.Lock()
	perMin := 0
	for _, e := range m.rec.ring {
		if e.At >= cutoff {
			perMin++
		}
	}
	active := 0
	for _, d := range m.rec.devices {
		if d.LastSeen >= cutoff {
			active++
		}
	}
	resp := map[string]any{
		"total":     m.rec.total,
		"per_min":   perMin,
		"devices":   len(m.rec.devices),
		"active":    active,
		"by_kind":   copyCounts(m.rec.byKind),
		"by_status": copyCounts(m.rec.byStatus),
	}
	m.rec.mu.Unlock()
	httpx.JSON(w, http.StatusOK, resp)
}

func copyCounts(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

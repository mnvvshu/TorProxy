package api

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"torproxymanager/internal/config"
	"torproxymanager/internal/logger"
	"torproxymanager/internal/manager"
)

type fixture struct {
	srv      *Server
	cfg      *config.Config
	handler  http.Handler
	host     string
	mu       sync.Mutex
	saved    []config.Settings
	shutdown chan struct{}
}

func newFixture(t *testing.T, webPort int) *fixture {
	t.Helper()
	dir := t.TempDir()
	s := config.DefaultSettings()
	if webPort != 0 {
		s.WebUIPort = webPort
	}
	s.DataDirectory = filepath.Join(dir, "data")
	cfg := config.NewFromSettings(s, filepath.Join(dir, "config.json"))
	log := logger.NewNop()
	mgr := manager.New(cfg, log, dir)

	f := &fixture{cfg: cfg, shutdown: make(chan struct{}, 1)}
	web := fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html><title>TPM</title>")},
		"app.js":     {Data: []byte("console.log(1)")},
	}
	f.srv = New(mgr, cfg, log, web, Hooks{
		OnConfigSaved: func(old, cur config.Settings) {
			f.mu.Lock()
			f.saved = append(f.saved, old, cur)
			f.mu.Unlock()
		},
		OnShutdown: func() { f.shutdown <- struct{}{} },
	})
	f.handler = f.srv.Handler()
	f.host = "127.0.0.1:" + strconv.Itoa(s.WebUIPort)
	return f
}

func (f *fixture) do(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Host = f.host
	if method != http.MethodGet {
		r.Header.Set(CSRFHeader, "1")
	}
	for k, v := range hdr {
		if v == "" {
			r.Header.Del(k)
		} else if k == "Host" {
			r.Host = v
		} else {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON (%d): %v\n%s", w.Code, err, w.Body.String())
	}
	return m
}

func TestHostHeaderValidation(t *testing.T) {
	f := newFixture(t, 0)
	cases := []struct {
		host string
		want int
	}{
		{"127.0.0.1:8470", 200},
		{"localhost:8470", 200},
		{"[::1]:8470", 200},
		{"127.0.0.1", 200},
		{"evil.example.com:8470", 403}, // DNS rebinding
		{"127.0.0.1.nip.io:8470", 403},
		{"127.0.0.1:9999", 403}, // wrong port
	}
	for _, c := range cases {
		w := f.do("GET", "/api/status", "", map[string]string{"Host": c.host})
		if w.Code != c.want {
			t.Errorf("Host %q: got %d, want %d", c.host, w.Code, c.want)
		}
	}
}

func TestCSRFProtection(t *testing.T) {
	f := newFixture(t, 0)

	w := f.do("POST", "/api/stop", "", map[string]string{CSRFHeader: ""})
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST without %s: got %d, want 403", CSRFHeader, w.Code)
	}
	w = f.do("POST", "/api/stop", "", map[string]string{"Origin": "https://evil.example.com"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST: got %d, want 403", w.Code)
	}
	w = f.do("POST", "/api/stop", "", map[string]string{"Origin": "http://127.0.0.1:8470"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("same-origin POST: got %d, want 202 (%s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("CORS header must never be sent, got %q", got)
	}
}

func TestSecurityHeadersAndStatic(t *testing.T) {
	f := newFixture(t, 0)
	w := f.do("GET", "/", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<title>TPM</title>") {
		t.Fatalf("GET /: %d %q", w.Code, w.Body.String())
	}
	h := w.Header()
	if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("weak CSP: %q", csp)
	}
	for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer"} {
		if h.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, h.Get(k), v)
		}
	}
	api := f.do("GET", "/api/status", "", nil)
	if api.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("API responses must be no-store")
	}
}

func TestStatusAndInstances(t *testing.T) {
	f := newFixture(t, 0)
	m := decode(t, f.do("GET", "/api/status", "", nil))
	if m["version"] != AppVersion {
		t.Errorf("version = %v", m["version"])
	}
	st := m["status"].(map[string]interface{})
	if st["manager_state"] != manager.StateStopped {
		t.Errorf("manager_state = %v", st["manager_state"])
	}

	w := f.do("GET", "/api/instances", "", nil)
	if w.Code != 200 {
		t.Fatalf("instances: %d", w.Code)
	}
	if w := f.do("GET", "/api/instances/5", "", nil); w.Code != 404 {
		t.Errorf("unknown instance: got %d, want 404", w.Code)
	}
	if w := f.do("GET", "/api/instances/abc", "", nil); w.Code != 400 {
		t.Errorf("invalid id: got %d, want 400", w.Code)
	}
	if w := f.do("POST", "/api/instances/1/restart", "", nil); w.Code != 404 {
		t.Errorf("action on unknown instance: got %d, want 404", w.Code)
	}
	if w := f.do("GET", "/api/does-not-exist", "", nil); w.Code != 404 {
		t.Errorf("unknown endpoint: got %d, want 404", w.Code)
	}
	if w := f.do("POST", "/api/restart-failed", "", nil); w.Code != 409 && w.Code != 200 {
		t.Errorf("restart-failed while stopped: got %d", w.Code)
	}
}

func TestConfigValidationAndSave(t *testing.T) {
	f := newFixture(t, 0)

	// Out-of-range value -> 400 with a list of errors, nothing persisted.
	w := f.do("POST", "/api/config", `{"endpoint_count": 1000}`, nil)
	if w.Code != 400 {
		t.Fatalf("invalid endpoint_count: got %d", w.Code)
	}
	if errs, _ := decode(t, w)["errors"].([]interface{}); len(errs) == 0 {
		t.Errorf("expected error list")
	}
	// Non-loopback bind address must be rejected.
	if w := f.do("POST", "/api/config", `{"bind_address": "0.0.0.0"}`, nil); w.Code != 400 {
		t.Errorf("public bind_address accepted: %d", w.Code)
	}
	// Unknown key.
	if w := f.do("POST", "/api/config", `{"endpoint_cuont": 10}`, nil); w.Code != 400 {
		t.Errorf("unknown key accepted: %d", w.Code)
	}
	// Malformed JSON.
	if w := f.do("POST", "/api/config", `{`, nil); w.Code != 400 {
		t.Errorf("malformed JSON accepted: %d", w.Code)
	}
	if _, err := os.Stat(f.cfg.FilePath()); err == nil {
		t.Fatalf("config must not be written by rejected updates")
	}

	// Valid update: 300 endpoints.
	w = f.do("POST", "/api/config", `{"endpoint_count": 300, "start_port": 10000}`, nil)
	if w.Code != 200 {
		t.Fatalf("valid update: %d %s", w.Code, w.Body.String())
	}
	if got := f.cfg.Snapshot(); got.EndpointCount != 300 || got.StartPort != 10000 {
		t.Fatalf("settings not applied: %+v", got)
	}
	reloaded, err := config.Load(f.cfg.FilePath())
	if err != nil || reloaded.Snapshot().EndpointCount != 300 {
		t.Fatalf("settings not persisted: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.saved) != 2 || f.saved[0].EndpointCount != 200 || f.saved[1].EndpointCount != 300 {
		t.Fatalf("OnConfigSaved hook not called correctly: %+v", f.saved)
	}

	m := decode(t, f.do("GET", "/api/config", "", nil))
	if m["settings"].(map[string]interface{})["endpoint_count"].(float64) != 300 {
		t.Errorf("GET /api/config does not reflect update")
	}
}

func TestProxiesExport(t *testing.T) {
	f := newFixture(t, 0)

	w := f.do("GET", "/api/proxies?format=url", "", nil)
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\r\n")
	if len(lines) != 200 || lines[0] != "socks5h://127.0.0.1:9050" || lines[199] != "socks5h://127.0.0.1:9249" {
		t.Fatalf("unexpected list: %d lines, first %q", len(lines), lines[0])
	}

	w = f.do("GET", "/api/proxies?format=json&running=1", "", nil)
	var list []string
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || list == nil || len(list) != 0 {
		t.Fatalf("running-only list while stopped should be [], got %q (%v)", w.Body.String(), err)
	}

	w = f.do("GET", "/api/proxies?download=1", "", nil)
	if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Errorf("download=1 must set Content-Disposition")
	}
}

func TestBrowserLaunchValidation(t *testing.T) {
	f := newFixture(t, 0)
	if w := f.do("POST", "/api/browser/launch", `{"port": 80}`, nil); w.Code != 400 {
		t.Errorf("port outside managed range accepted: %d", w.Code)
	}
	if w := f.do("POST", "/api/browser/launch", `{"port": 9050, "url": "file:///C:/Windows/win.ini"}`, nil); w.Code != 400 {
		t.Errorf("non-http URL accepted: %d", w.Code)
	}
	// The browser path is never accepted from the request; extra fields are ignored.
	if w := f.do("POST", "/api/browser/launch", `{"port": 9050, "path": "C:\\Windows\\System32\\calc.exe", "url": "ftp://x"}`, nil); w.Code != 400 {
		t.Errorf("expected rejection, got %d", w.Code)
	}
}

func TestDiagnosticsAndLogs(t *testing.T) {
	f := newFixture(t, 0)
	m := decode(t, f.do("GET", "/api/diagnostics", "", nil))
	if m["app_version"] == nil && m["AppVersion"] == nil && len(m) == 0 {
		t.Errorf("empty diagnostics report")
	}
	w := f.do("GET", "/api/diagnostics/export?format=text", "", nil)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Disposition"), ".txt") {
		t.Errorf("text export: %d %q", w.Code, w.Header().Get("Content-Disposition"))
	}
	w = f.do("GET", "/api/diagnostics/export", "", nil)
	if w.Code != 200 || !json.Valid(w.Body.Bytes()) {
		t.Errorf("json export invalid: %d", w.Code)
	}
	if w := f.do("GET", "/api/logs?limit=10", "", nil); w.Code != 200 {
		t.Errorf("logs: %d", w.Code)
	}
}

func TestShutdownHook(t *testing.T) {
	f := newFixture(t, 0)
	if w := f.do("POST", "/api/shutdown", "", nil); w.Code != http.StatusAccepted {
		t.Fatalf("shutdown: %d", w.Code)
	}
	select {
	case <-f.shutdown:
	case <-time.After(2 * time.Second):
		t.Fatal("OnShutdown hook not invoked")
	}
}

func TestLiveServerSSE(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	f := newFixture(t, port)
	if err := f.srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer f.srv.Stop(t.Context())

	if f.srv.URL() != "http://127.0.0.1:"+strconv.Itoa(port)+"/" {
		t.Errorf("URL() = %q", f.srv.URL())
	}

	req, _ := http.NewRequestWithContext(t.Context(), "GET", f.srv.URL()+"api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	deadline := time.AfterFunc(5*time.Second, func() { resp.Body.Close() })
	defer deadline.Stop()
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var msg map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &msg); err != nil {
			t.Fatalf("bad SSE payload: %v", err)
		}
		if msg["type"] != "snapshot" {
			t.Fatalf("first SSE message type = %v, want snapshot", msg["type"])
		}
		return
	}
	t.Fatal("no SSE snapshot received")
}

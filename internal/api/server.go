// Package api provides the HTTP server, REST API, and SSE event streaming for the web dashboard.
//
// Security model: the server binds to 127.0.0.1 only and additionally
//   - rejects requests whose Host header is not a loopback name (DNS-rebinding defence),
//   - never emits CORS headers, and requires a custom X-TPM-Request header on every
//     state-changing request so cross-site pages cannot forge them (CSRF defence),
//   - serves a strict Content-Security-Policy for the dashboard.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"torproxymanager/internal/browser"
	"torproxymanager/internal/config"
	"torproxymanager/internal/diagnostics"
	"torproxymanager/internal/health"
	"torproxymanager/internal/logger"
	"torproxymanager/internal/manager"
	"torproxymanager/internal/torinstance"
)

// AppVersion is overridden at build time via -ldflags "-X torproxymanager/internal/api.AppVersion=..."
var AppVersion = "1.0.0"

// CSRFHeader must be present (any value) on every non-GET API request.
const CSRFHeader = "X-TPM-Request"

// Hooks lets the host application react to API actions.
type Hooks struct {
	// OnConfigSaved is called after a configuration update is persisted.
	OnConfigSaved func(old, new config.Settings)
	// OnShutdown is called when the dashboard requests application exit.
	OnShutdown func()
}

// Server handles all HTTP API requests and serves the web dashboard.
type Server struct {
	mgr    *manager.Manager
	cfg    *config.Config
	log    *logger.Logger
	webFS  fs.FS
	hooks  Hooks
	server *http.Server
	addr   string
	port   int

	done     chan struct{}
	doneOnce sync.Once

	sseMu   sync.Mutex
	clients map[chan []byte]struct{}

	lastTestMu sync.Mutex
	lastTest   []health.TorCheckResult
	testing    bool
}

// New creates a new API server. webFS must contain index.html at its root.
func New(mgr *manager.Manager, cfg *config.Config, log *logger.Logger, webFS fs.FS, hooks Hooks) *Server {
	port := cfg.Snapshot().WebUIPort
	return &Server{
		mgr:     mgr,
		cfg:     cfg,
		log:     log.WithComponent("api"),
		webFS:   webFS,
		hooks:   hooks,
		port:    port,
		addr:    net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		done:    make(chan struct{}),
		clients: make(map[chan []byte]struct{}),
	}
}

// URL returns the dashboard URL.
func (s *Server) URL() string { return "http://" + s.addr + "/" }

// Addr returns the listen address.
func (s *Server) Addr() string { return s.addr }

// Handler builds the HTTP handler (exposed for tests).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/instances", s.handleInstances)
	mux.HandleFunc("GET /api/instances/{id}", s.handleInstance)
	mux.HandleFunc("GET /api/instances/{id}/log", s.handleInstanceLog)
	mux.HandleFunc("POST /api/instances/{id}/{action}", s.handleInstanceAction)
	mux.HandleFunc("POST /api/start", s.handleStart)
	mux.HandleFunc("POST /api/stop", s.handleStop)
	mux.HandleFunc("POST /api/restart-failed", s.handleRestartFailed)
	mux.HandleFunc("POST /api/newnym-all", s.handleNewnymAll)
	mux.HandleFunc("POST /api/test-all", s.handleTestAll)
	mux.HandleFunc("GET /api/test-all", s.handleTestAllResults)
	mux.HandleFunc("GET /api/config", s.handleGetConfig)
	mux.HandleFunc("POST /api/config", s.handleSaveConfig)
	mux.HandleFunc("GET /api/config/defaults", s.handleConfigDefaults)
	mux.HandleFunc("GET /api/diagnostics", s.handleDiagnostics)
	mux.HandleFunc("GET /api/diagnostics/export", s.handleDiagnosticsExport)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	mux.HandleFunc("GET /api/proxies", s.handleProxies)
	mux.HandleFunc("GET /api/browsers", s.handleBrowsers)
	mux.HandleFunc("POST /api/browser/launch", s.handleBrowserLaunch)
	mux.HandleFunc("POST /api/shutdown", s.handleShutdown)
	mux.HandleFunc("GET /api/events", s.handleSSE)
	apiNotFound := func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "unknown API endpoint")
	}
	mux.HandleFunc("GET /api/", apiNotFound)
	mux.HandleFunc("POST /api/", apiNotFound)

	if s.webFS != nil {
		mux.Handle("GET /", http.FileServer(http.FS(s.webFS)))
	}

	return s.securityMiddleware(mux)
}

// Start begins listening for HTTP requests.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.addr, err)
	}

	s.server = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // SSE streams and /api/test-all are long-lived
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	go s.broadcastLoop()
	go func() {
		if err := s.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("HTTP server error: %v", err)
		}
	}()

	s.log.Info("Dashboard available at %s", s.URL())
	return nil
}

// Stop gracefully shuts down the HTTP server and all SSE streams.
func (s *Server) Stop(ctx context.Context) error {
	s.doneOnce.Do(func() { close(s.done) })
	if s.server != nil {
		return s.server.Shutdown(ctx)
	}
	return nil
}

// --- Security ---

func (s *Server) allowedHost(host string) bool {
	h, p, err := net.SplitHostPort(host)
	if err != nil {
		h, p = host, ""
	}
	if p != "" && p != strconv.Itoa(s.port) {
		return false
	}
	switch strings.ToLower(strings.Trim(h, "[]")) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

func (s *Server) securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowedHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}

		hdr := w.Header()
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("X-Frame-Options", "DENY")
		hdr.Set("Referrer-Policy", "no-referrer")
		hdr.Set("Cross-Origin-Opener-Policy", "same-origin")
		hdr.Set("Cross-Origin-Resource-Policy", "same-origin")
		hdr.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
				"connect-src 'self'; font-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get(CSRFHeader) == "" {
				writeError(w, http.StatusForbidden, "missing "+CSRFHeader+" header")
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				o := strings.TrimPrefix(strings.TrimPrefix(origin, "http://"), "https://")
				if !s.allowedHost(o) {
					writeError(w, http.StatusForbidden, "cross-origin request rejected")
					return
				}
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		}

		if strings.HasPrefix(r.URL.Path, "/api/") {
			hdr.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// --- Handlers: status ---

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{
		"status":  s.mgr.GetOverallStatus(),
		"version": AppVersion,
	})
}

func (s *Server) handleInstances(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.mgr.SortedStatuses())
}

func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	st, found := s.mgr.GetInstanceStatus(id)
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("instance %d not found", id))
		return
	}
	writeJSON(w, st)
}

func (s *Server) handleInstanceLog(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	text, err := s.mgr.InstanceLog(id, 64<<10)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, text)
}

// --- Handlers: lifecycle ---

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if s.mgr.IsStarted() {
		writeError(w, http.StatusConflict, "already running")
		return
	}
	if errs := s.cfg.Validate(); len(errs) > 0 {
		writeErrors(w, errs)
		return
	}
	go func() {
		if err := s.mgr.StartAll(); err != nil {
			s.log.Error("Start all failed: %v", err)
		}
	}()
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": "starting"})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	go func() {
		if err := s.mgr.StopAll(); err != nil {
			s.log.Error("Stop all failed: %v", err)
		}
	}()
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": "stopping"})
}

func (s *Server) handleRestartFailed(w http.ResponseWriter, r *http.Request) {
	n, err := s.mgr.RestartFailed()
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"status": "restarting", "count": n})
}

func (s *Server) handleNewnymAll(w http.ResponseWriter, r *http.Request) {
	ok, failed := s.mgr.NewIdentityAll()
	writeJSON(w, map[string]interface{}{"status": "ok", "succeeded": ok, "failed": failed})
}

func (s *Server) handleInstanceAction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, found := s.mgr.GetInstanceStatus(id); !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("instance %d not found", id))
		return
	}

	action := r.PathValue("action")
	async := func(name string, fn func() error) {
		go func() {
			if err := fn(); err != nil && !errors.Is(err, torinstance.ErrAborted) {
				s.log.Warn("%s instance %d: %v", name, id, err)
			}
		}()
		writeJSONStatus(w, http.StatusAccepted, map[string]interface{}{"status": name, "instance_id": id})
	}

	switch action {
	case "restart":
		if !s.mgr.IsStarted() {
			writeError(w, http.StatusConflict, "manager is not running")
			return
		}
		async("restarting", func() error { return s.mgr.RestartInstance(id) })
	case "start":
		if !s.mgr.IsStarted() {
			writeError(w, http.StatusConflict, "manager is not running")
			return
		}
		async("starting", func() error { return s.mgr.StartInstance(id) })
	case "stop":
		async("stopping", func() error { return s.mgr.StopInstance(id) })
	case "newnym":
		if err := s.mgr.NewIdentity(id); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, map[string]interface{}{"status": "ok", "instance_id": id})
	case "test":
		res, err := s.mgr.TestInstance(id)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, res)
	default:
		writeError(w, http.StatusNotFound, "unknown action "+strconv.Quote(action))
	}
}

func (s *Server) handleTestAll(w http.ResponseWriter, r *http.Request) {
	s.lastTestMu.Lock()
	if s.testing {
		s.lastTestMu.Unlock()
		writeError(w, http.StatusConflict, "a verification run is already in progress")
		return
	}
	s.testing = true
	s.lastTestMu.Unlock()

	defer func() {
		s.lastTestMu.Lock()
		s.testing = false
		s.lastTestMu.Unlock()
	}()

	results := s.mgr.TestAll(r.Context())
	s.lastTestMu.Lock()
	s.lastTest = results
	s.lastTestMu.Unlock()

	tor, failed := 0, 0
	for _, res := range results {
		if res.IsTor {
			tor++
		} else {
			failed++
		}
	}
	writeJSON(w, map[string]interface{}{
		"tested":   len(results),
		"via_tor":  tor,
		"failed":   failed,
		"results":  results,
		"finished": time.Now(),
	})
}

func (s *Server) handleTestAllResults(w http.ResponseWriter, r *http.Request) {
	s.lastTestMu.Lock()
	defer s.lastTestMu.Unlock()
	writeJSON(w, map[string]interface{}{"running": s.testing, "results": s.lastTest})
}

// --- Handlers: configuration ---

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{
		"settings":    s.cfg.Snapshot(),
		"file":        s.cfg.FilePath(),
		"limits":      map[string]int{"min_endpoints": config.MinEndpoints, "max_endpoints": config.MaxEndpoints, "min_port": config.MinPort, "max_port": config.MaxPort},
		"tor_search":  config.TorSearchPaths(s.mgr.AppDir()),
		"active":      s.mgr.ActiveSettings(),
		"manager":     s.mgr.State(),
		"app_version": AppVersion,
	})
}

func (s *Server) handleConfigDefaults(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, config.DefaultSettings())
}

func (s *Server) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	var updates map[string]interface{}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&updates); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	old := s.cfg.Snapshot()
	if errs := s.cfg.Update(updates); len(errs) > 0 {
		writeErrors(w, errs)
		return
	}
	if err := s.cfg.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to save: %v", err))
		return
	}
	updated := s.cfg.Snapshot()
	s.log.Info("Configuration saved to %s", s.cfg.FilePath())
	if s.hooks.OnConfigSaved != nil {
		s.hooks.OnConfigSaved(old, updated)
	}

	writeJSON(w, map[string]interface{}{
		"status":           "saved",
		"restart_required": s.mgr.IsStarted() && updated != s.mgr.ActiveSettings(),
		"web_port_changed": updated.WebUIPort != old.WebUIPort,
		"settings":         updated,
	})
}

// --- Handlers: diagnostics / logs / exports ---

func (s *Server) buildReport(includeTest bool) *diagnostics.Report {
	st := s.mgr.GetOverallStatus()
	in := diagnostics.Input{
		AppVersion:    AppVersion,
		ManagerState:  st.ManagerState,
		TorPath:       st.TorPath,
		TorVersion:    st.TorVersion,
		UptimeSeconds: st.UptimeSeconds,
		Settings:      s.mgr.ActiveSettings(),
		Instances:     s.mgr.SortedStatuses(),
		RecentLogs:    s.log.Recent(300, 0),
	}
	if includeTest {
		s.lastTestMu.Lock()
		in.HealthResults = append([]health.TorCheckResult(nil), s.lastTest...)
		s.lastTestMu.Unlock()
	}
	return diagnostics.Generate(in)
}

func (s *Server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.buildReport(false))
}

func (s *Server) handleDiagnosticsExport(w http.ResponseWriter, r *http.Request) {
	report := s.buildReport(true)
	stamp := time.Now().Format("20060102-150405")
	switch r.URL.Query().Get("format") {
	case "text", "txt":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="torproxymanager-diagnostics-%s.txt"`, stamp))
		io.WriteString(w, report.ExportText())
	default:
		data, err := report.ExportJSON()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="torproxymanager-diagnostics-%s.json"`, stamp))
		w.Write(data)
	}
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, _ := strconv.ParseUint(q.Get("after"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	writeJSON(w, s.log.Recent(limit, after))
}

func (s *Server) handleProxies(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	onlyRunning := q.Get("running") == "1" || q.Get("running") == "true"
	list := s.mgr.ProxyList(onlyRunning)
	download := q.Get("download") == "1"

	switch q.Get("format") {
	case "json":
		writeJSON(w, list)
		return
	case "url":
		for i, hp := range list {
			list[i] = "socks5h://" + hp
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if download {
		w.Header().Set("Content-Disposition", `attachment; filename="tor-socks5-proxies.txt"`)
	}
	io.WriteString(w, strings.Join(list, "\r\n"))
	if len(list) > 0 {
		io.WriteString(w, "\r\n")
	}
}

// --- Handlers: browser ---

func (s *Server) handleBrowsers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{
		"configured": s.cfg.Snapshot().BrowserPath,
		"detected":   browser.Detect(),
	})
}

func (s *Server) handleBrowserLaunch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port int    `json:"port"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	active := s.mgr.ActiveSettings()
	if req.Port < active.StartPort || req.Port > active.LastPort() {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("port must be within the managed range %d-%d", active.StartPort, active.LastPort()))
		return
	}
	if req.URL != "" && !strings.HasPrefix(req.URL, "https://") && !strings.HasPrefix(req.URL, "http://") {
		writeError(w, http.StatusBadRequest, "url must start with http:// or https://")
		return
	}
	if req.URL == "" {
		req.URL = "https://check.torproject.org/"
	}

	// Security: the executable always comes from the saved configuration or
	// auto-detection — never from the request body.
	path := s.cfg.Snapshot().BrowserPath
	if path == "" {
		for _, d := range browser.Detect() {
			if d.Kind == browser.KindFirefox || d.Kind == browser.KindChromium {
				path = d.Path
				break
			}
		}
	}
	if path == "" {
		writeError(w, http.StatusNotFound, "no supported browser found — set browser_path in Settings")
		return
	}

	profileRoot := filepath.Join(active.ResolveDataDir(s.mgr.AppDir()), "browser-profiles")
	if _, err := browser.Launch(path, active.BindAddress, req.Port, profileRoot, req.URL); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("Launched %s through SOCKS5 port %d", filepath.Base(path), req.Port)
	writeJSON(w, map[string]string{
		"status":  "launched",
		"browser": filepath.Base(path),
		"message": fmt.Sprintf("%s launched with an isolated profile via 127.0.0.1:%d", filepath.Base(path), req.Port),
	})
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": "shutting_down"})
	if s.hooks.OnShutdown != nil {
		go func() {
			time.Sleep(200 * time.Millisecond) // let the response flush
			s.hooks.OnShutdown()
		}()
	}
}

// --- SSE ---

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan []byte, 256)
	s.sseMu.Lock()
	s.clients[ch] = struct{}{}
	s.sseMu.Unlock()
	defer func() {
		s.sseMu.Lock()
		delete(s.clients, ch)
		s.sseMu.Unlock()
	}()

	initial, _ := json.Marshal(map[string]interface{}{
		"type":      "snapshot",
		"status":    s.mgr.GetOverallStatus(),
		"instances": s.mgr.SortedStatuses(),
		"version":   AppVersion,
	})
	fmt.Fprintf(w, "retry: 3000\ndata: %s\n\n", initial)
	flusher.Flush()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-ping.C:
			io.WriteString(w, ": ping\n\n")
			flusher.Flush()
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		}
	}
}

// broadcastLoop forwards manager events and log entries to SSE clients.
// Instance updates are coalesced and flushed at most every 400ms so that
// 300 instances bootstrapping at once don't flood the browser.
func (s *Server) broadcastLoop() {
	events := s.mgr.Subscribe()
	defer s.mgr.Unsubscribe(events)
	logs := s.log.Subscribe()
	defer s.log.Unsubscribe(logs)

	flush := time.NewTicker(400 * time.Millisecond)
	defer flush.Stop()
	statusTick := time.NewTicker(2 * time.Second)
	defer statusTick.Stop()

	pending := map[int]torinstance.Status{}

	for {
		select {
		case <-s.done:
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			if evt.Type == manager.EventInstanceUpdate && evt.Instance != nil {
				pending[evt.InstanceID] = *evt.Instance
				continue
			}
			if evt.Type == manager.EventProgress {
				continue // conveyed by status snapshots
			}
			s.broadcastJSON(map[string]interface{}{"type": "event", "event": evt})
		case e, ok := <-logs:
			if !ok {
				return
			}
			s.broadcastJSON(map[string]interface{}{"type": "log", "entry": e})
		case <-flush.C:
			if len(pending) == 0 {
				continue
			}
			batch := make([]torinstance.Status, 0, len(pending))
			for _, st := range pending {
				batch = append(batch, st)
			}
			pending = map[int]torinstance.Status{}
			s.broadcastJSON(map[string]interface{}{"type": "instances", "instances": batch})
		case <-statusTick.C:
			s.broadcastJSON(map[string]interface{}{"type": "status", "status": s.mgr.GetOverallStatus()})
		}
	}
}

func (s *Server) broadcastJSON(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.sseMu.Lock()
	defer s.sseMu.Unlock()
	for ch := range s.clients {
		select {
		case ch <- data:
		default: // slow client: drop; periodic snapshots keep it consistent
		}
	}
}

// --- helpers ---

func pathID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid instance ID")
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, data interface{}) {
	writeJSONStatus(w, http.StatusOK, data)
}

func writeJSONStatus(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSONStatus(w, code, map[string]interface{}{"status": "error", "error": msg})
}

func writeErrors(w http.ResponseWriter, errs []error) {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	writeJSONStatus(w, http.StatusBadRequest, map[string]interface{}{
		"status": "error",
		"error":  msgs[0],
		"errors": msgs,
	})
}

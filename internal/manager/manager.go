// Package manager orchestrates multiple Tor instances with parallel startup,
// health monitoring, and coordinated shutdown.
package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"torproxymanager/internal/config"
	"torproxymanager/internal/health"
	"torproxymanager/internal/logger"
	"torproxymanager/internal/torinstance"
	"torproxymanager/internal/winsys"
)

// EventType classifies events emitted by the manager.
type EventType string

const (
	EventStarting       EventType = "starting"
	EventStarted        EventType = "started"
	EventStopping       EventType = "stopping"
	EventStopped        EventType = "stopped"
	EventInstanceUpdate EventType = "instance_update"
	EventProgress       EventType = "progress"
	EventError          EventType = "error"
	EventHealthUpdate   EventType = "health_update"
	EventInfo           EventType = "info"
)

// Manager states.
const (
	StateStopped  = "stopped"
	StateStarting = "starting"
	StateRunning  = "running"
	StateStopping = "stopping"
)

// Event is an observable event from the manager for UI/SSE consumption.
type Event struct {
	Type       EventType              `json:"type"`
	Message    string                 `json:"message,omitempty"`
	Data       map[string]interface{} `json:"data,omitempty"`
	Timestamp  time.Time              `json:"timestamp"`
	InstanceID int                    `json:"instance_id,omitempty"`
	Instance   *torinstance.Status    `json:"instance,omitempty"`
}

// OverallStatus aggregates the status of all managed instances.
type OverallStatus struct {
	ManagerState           string  `json:"manager_state"`
	TotalInstances         int     `json:"total_instances"`
	TotalPorts             int     `json:"total_ports"`
	RunningPorts           int     `json:"running_ports"`
	RunningInstances       int     `json:"running_instances"`
	HealthyInstances       int     `json:"healthy_instances"`
	FailedInstances        int     `json:"failed_instances"`
	StoppedInstances       int     `json:"stopped_instances"`
	StartingInstances      int     `json:"starting_instances"`
	BootstrappingInstances int     `json:"bootstrapping_instances"`
	PortRangeStart         int     `json:"port_range_start"`
	PortRangeEnd           int     `json:"port_range_end"`
	ControlPortStart       int     `json:"control_port_start"`
	TotalMemoryMB          float64 `json:"total_memory_mb"`
	TotalCPUPercent        float64 `json:"total_cpu_percent"`
	AvgStartupMillis       int64   `json:"avg_startup_ms"`
	UptimeSeconds          float64 `json:"uptime_seconds"`
	StartProgress          int     `json:"start_progress"`
	StartTotal             int     `json:"start_total"`
	TorPath                string  `json:"tor_path,omitempty"`
	TorVersion             string  `json:"tor_version,omitempty"`
	ConfigPendingRestart   bool    `json:"config_pending_restart"`
	LastError              string  `json:"last_error,omitempty"`
}

// Manager controls the lifecycle of all Tor instances.
type Manager struct {
	cfg    *config.Config
	log    *logger.Logger
	appDir string
	job    *winsys.Job

	mu         sync.RWMutex
	state      string
	settings   config.Settings // snapshot used for the current/last run
	instances  []*torinstance.Instance
	ctx        context.Context
	cancel     context.CancelFunc
	startTime  time.Time
	torPath    string
	torVersion string
	lastError  string
	startDone  chan struct{} // closed when the current StartAll returns

	eventMu   sync.RWMutex
	listeners map[chan Event]struct{}

	startProgress atomic.Int32
	startTotal    atomic.Int32

	bgStop chan struct{}
	bgWG   sync.WaitGroup
}

// New creates a new Manager. appDir is the directory used to resolve relative
// resources (tor\tor.exe, data\, configs\torrc.template).
func New(cfg *config.Config, log *logger.Logger, appDir string) *Manager {
	return &Manager{
		cfg:       cfg,
		log:       log.WithComponent("manager"),
		appDir:    appDir,
		state:     StateStopped,
		settings:  cfg.Snapshot(),
		listeners: make(map[chan Event]struct{}),
	}
}

// SetJob attaches a Job Object that every launched tor process is assigned to.
func (m *Manager) SetJob(j *winsys.Job) { m.job = j }

// AppDir returns the application directory.
func (m *Manager) AppDir() string { return m.appDir }

// Config returns the live configuration.
func (m *Manager) Config() *config.Config { return m.cfg }

// --- Events ---

// Subscribe returns a channel that receives manager events.
func (m *Manager) Subscribe() chan Event {
	ch := make(chan Event, 512)
	m.eventMu.Lock()
	m.listeners[ch] = struct{}{}
	m.eventMu.Unlock()
	return ch
}

// Unsubscribe removes an event listener and closes its channel.
func (m *Manager) Unsubscribe(ch chan Event) {
	m.eventMu.Lock()
	defer m.eventMu.Unlock()
	if _, ok := m.listeners[ch]; ok {
		delete(m.listeners, ch)
		close(ch)
	}
}

func (m *Manager) emit(evt Event) {
	evt.Timestamp = time.Now()
	m.eventMu.RLock()
	defer m.eventMu.RUnlock()
	for ch := range m.listeners {
		select {
		case ch <- evt:
		default: // drop for slow listeners; periodic status snapshots resync them
		}
	}
}

// --- Lifecycle ---

// StartAll launches all configured Tor instances and blocks until every
// instance has either bootstrapped or exhausted its retries.
func (m *Manager) StartAll() error {
	m.mu.Lock()
	if m.state != StateStopped {
		st := m.state
		m.mu.Unlock()
		return fmt.Errorf("manager is %s", st)
	}
	s := m.cfg.Snapshot()
	if errs := s.Validate(); len(errs) > 0 {
		m.mu.Unlock()
		return fmt.Errorf("invalid configuration: %v", errs[0])
	}
	m.state = StateStarting
	m.settings = s
	m.lastError = ""
	m.ctx, m.cancel = context.WithCancel(context.Background())
	ctx := m.ctx
	m.startTime = time.Now()
	m.startDone = make(chan struct{})
	done := m.startDone
	m.mu.Unlock()
	defer close(done)

	m.startProgress.Store(0)
	m.startTotal.Store(int32(s.ProcessCount()))

	m.log.Info("Starting %d Tor processes serving %d SOCKS ports (%d-%d, control %d-%d, concurrency %d)",
		s.ProcessCount(), s.EndpointCount, s.StartPort, s.LastPort(), s.ControlPortStart, s.LastControlPort(), s.StartupConcurrency)
	m.emit(Event{Type: EventStarting, Message: fmt.Sprintf("Starting %d Tor processes (%d ports)", s.ProcessCount(), s.EndpointCount)})

	instances, err := m.prepare(s)
	if err != nil {
		m.failStart(err)
		return err
	}

	m.mu.Lock()
	m.instances = instances
	m.mu.Unlock()

	started := time.Now()
	var okCount, failCount atomic.Int32
	runOne := func(inst *torinstance.Instance) {
		if err := inst.StartWithRetry(ctx); err != nil {
			if !errors.Is(err, torinstance.ErrAborted) && ctx.Err() == nil {
				failCount.Add(1)
				m.log.Error("Instance %d (port %d) failed to start: %v", inst.ID(), inst.SocksPort(), err)
			}
		} else {
			okCount.Add(1)
		}
		p := m.startProgress.Add(1)
		m.emit(Event{
			Type:       EventProgress,
			Message:    fmt.Sprintf("%d/%d instances processed", p, s.ProcessCount()),
			InstanceID: inst.ID(),
			Data:       map[string]interface{}{"progress": p, "total": s.ProcessCount()},
		})
	}

	// Phase 1: bootstrap a seed instance alone, then share its directory
	// cache with every other instance. This turns ~200 full consensus
	// downloads into one, cutting startup time and load on the Tor network.
	rest := instances
	if s.SeedDirectoryCache && len(instances) > 1 {
		seed := instances[0]
		runOne(seed)
		rest = instances[1:]
		if seed.State() == torinstance.StateRunning {
			m.queryTorVersion(seed)
			n := 0
			for _, inst := range rest {
				n += seedDirectoryCache(seed.DataDir(), inst.DataDir())
			}
			if n > 0 {
				m.log.Info("Seeded directory cache into %d instance directories (%d files)", len(rest), n)
			}
		}
	}

	// Phase 2: everything else, bounded by StartupConcurrency.
	sem := make(chan struct{}, s.StartupConcurrency)
	var wg sync.WaitGroup
	for _, inst := range rest {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(inst *torinstance.Instance) {
			defer wg.Done()
			defer func() { <-sem }()
			runOne(inst)
		}(inst)
	}
	wg.Wait()

	if ctx.Err() != nil {
		m.log.Info("Startup interrupted by stop request")
		return errors.New("startup cancelled")
	}

	m.mu.Lock()
	m.state = StateRunning
	m.mu.Unlock()
	if m.torVersion == "" && len(instances) > 0 {
		for _, inst := range instances {
			if inst.State() == torinstance.StateRunning {
				m.queryTorVersion(inst)
				break
			}
		}
	}

	m.startBackground(s)

	ok, failed := int(okCount.Load()), int(failCount.Load())
	m.log.Info("Startup complete in %v: %d/%d Tor processes running, %d failed",
		time.Since(started).Round(time.Millisecond), ok, s.ProcessCount(), failed)
	m.emit(Event{
		Type:    EventStarted,
		Message: fmt.Sprintf("%d of %d Tor processes running (%d ports), %d failed", ok, s.ProcessCount(), s.EndpointCount, failed),
		Data:    map[string]interface{}{"running": ok, "failed": failed, "total": s.ProcessCount()},
	})
	return nil
}

// prepare resolves paths, cleans up stale processes, checks ports and builds instances.
func (m *Manager) prepare(s config.Settings) ([]*torinstance.Instance, error) {
	torPath, err := s.ResolveTorPath(m.appDir)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.torPath = torPath
	m.mu.Unlock()
	m.log.Info("Using Tor executable: %s", torPath)

	dataDir := s.ResolveDataDir(m.appDir)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("cannot create data directory %s: %w", dataDir, err)
	}

	tmplSrc := ""
	if p := s.ResolveTorrcTemplate(m.appDir); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read torrc template %s: %w", p, err)
		}
		tmplSrc = string(b)
		m.log.Info("Using torrc template: %s", p)
	}

	// Kill tor processes left behind by a previous crash of this application.
	if n := m.cleanupStale(dataDir, s.ProcessCount()); n > 0 {
		m.log.Warn("Terminated %d stale tor process(es) from a previous session", n)
		time.Sleep(500 * time.Millisecond) // let the OS release the ports
	}

	if conflicts := FindPortConflicts(s); len(conflicts) > 0 {
		shown := conflicts
		if len(shown) > 5 {
			shown = shown[:5]
		}
		for _, c := range conflicts {
			m.log.Error("Port conflict: %s", c)
		}
		return nil, fmt.Errorf("%d port conflict(s): %s%s — change start_port/control_port_start or free the ports",
			len(conflicts), strings.Join(shown, "; "), map[bool]string{true: "; …", false: ""}[len(conflicts) > 5])
	}

	procs := s.ProcessCount()
	instances := make([]*torinstance.Instance, procs)
	for i := 0; i < procs; i++ {
		first, count := s.PortBlock(i)
		inst, err := torinstance.New(torinstance.Options{
			ID:               i + 1,
			SocksPort:        first,
			SocksPortCount:   count,
			ControlPort:      s.ControlPortStart + i,
			BindAddress:      s.BindAddress,
			TorPath:          torPath,
			DataDir:          filepath.Join(dataDir, fmt.Sprintf("instance_%d", i+1)),
			TorrcTemplate:    tmplSrc,
			ConnTimeout:      time.Duration(s.ConnectionTimeoutSec) * time.Second,
			BootstrapTimeout: time.Duration(s.BootstrapTimeoutSec) * time.Second,
			AutoRestart:      s.AutoRestart,
			RetryLimit:       s.RetryLimit,
			RetryBackoff:     time.Duration(s.RetryBackoffSec) * time.Second,
			BackoffMult:      s.RetryBackoffMultiplier,
			MaxBackoff:       time.Duration(s.RetryMaxBackoffSec) * time.Second,
			OnProcessStarted: m.assignToJob,
			OnChange:         m.onInstanceChange,
		}, m.log)
		if err != nil {
			return nil, err
		}
		instances[i] = inst
	}
	return instances, nil
}

func (m *Manager) failStart(err error) {
	m.log.Error("Startup failed: %v", err)
	m.mu.Lock()
	m.state = StateStopped
	m.lastError = err.Error()
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
	m.emit(Event{Type: EventError, Message: err.Error()})
	m.emit(Event{Type: EventStopped, Message: "Startup failed"})
}

func (m *Manager) assignToJob(pid int) {
	if m.job == nil {
		return
	}
	if err := m.job.Assign(pid); err != nil {
		m.log.Debug("Could not assign PID %d to job object: %v", pid, err)
	}
}

func (m *Manager) onInstanceChange(st torinstance.Status) {
	s := st
	m.emit(Event{Type: EventInstanceUpdate, InstanceID: st.InstanceID, Instance: &s})
}

func (m *Manager) queryTorVersion(inst *torinstance.Instance) {
	var v string
	err := inst.Control(func(c *torinstance.ControlConn) error {
		var err error
		v, err = c.GetInfo("version")
		return err
	})
	if err == nil && v != "" {
		m.mu.Lock()
		m.torVersion = v
		m.mu.Unlock()
		m.log.Info("Tor version: %s", v)
	}
}

// StopAll gracefully shuts down all managed instances. It is safe to call
// while StartAll is still in progress.
func (m *Manager) StopAll() error {
	m.mu.Lock()
	if m.state == StateStopped || m.state == StateStopping {
		m.mu.Unlock()
		return nil
	}
	m.state = StateStopping
	if m.cancel != nil {
		m.cancel()
	}
	instances := append([]*torinstance.Instance(nil), m.instances...)
	startDone := m.startDone
	m.mu.Unlock()

	m.log.Info("Stopping all %d instances", len(instances))
	m.emit(Event{Type: EventStopping, Message: "Stopping all instances"})

	m.stopBackground()

	var wg sync.WaitGroup
	sem := make(chan struct{}, 64)
	for _, inst := range instances {
		wg.Add(1)
		go func(inst *torinstance.Instance) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := inst.Stop(); err != nil {
				m.log.Error("Error stopping instance %d: %v", inst.ID(), err)
			}
		}(inst)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(45 * time.Second):
		m.log.Warn("Timeout waiting for instances to stop — remaining processes will be terminated by the job object on exit")
	}

	// Wait for an in-flight StartAll to unwind so state transitions are ordered.
	if startDone != nil {
		select {
		case <-startDone:
		case <-time.After(10 * time.Second):
		}
	}

	m.mu.Lock()
	m.state = StateStopped
	m.mu.Unlock()

	m.log.Info("All instances stopped")
	m.emit(Event{Type: EventStopped, Message: "All instances stopped"})
	return nil
}

// --- Per-instance operations ---

func (m *Manager) runningCtx() (context.Context, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.state != StateRunning && m.state != StateStarting {
		return nil, fmt.Errorf("manager is %s", m.state)
	}
	return m.ctx, nil
}

// RestartInstance restarts a specific instance by ID (1-indexed).
func (m *Manager) RestartInstance(id int) error {
	inst := m.getInstance(id)
	if inst == nil {
		return fmt.Errorf("instance %d not found", id)
	}
	ctx, err := m.runningCtx()
	if err != nil {
		return err
	}
	m.log.Info("Restarting instance %d", id)
	return inst.Restart(ctx)
}

// StartInstance starts a single stopped/failed instance.
func (m *Manager) StartInstance(id int) error {
	inst := m.getInstance(id)
	if inst == nil {
		return fmt.Errorf("instance %d not found", id)
	}
	ctx, err := m.runningCtx()
	if err != nil {
		return err
	}
	return inst.StartWithRetry(ctx)
}

// StopInstance stops a single instance.
func (m *Manager) StopInstance(id int) error {
	inst := m.getInstance(id)
	if inst == nil {
		return fmt.Errorf("instance %d not found", id)
	}
	return inst.Stop()
}

// RestartFailed restarts all instances in failed state and returns how many were scheduled.
func (m *Manager) RestartFailed() (int, error) {
	ctx, err := m.runningCtx()
	if err != nil {
		return 0, err
	}
	m.mu.RLock()
	instances := append([]*torinstance.Instance(nil), m.instances...)
	concurrency := m.settings.StartupConcurrency
	m.mu.RUnlock()

	var failed []*torinstance.Instance
	for _, inst := range instances {
		if inst.State() == torinstance.StateFailed {
			failed = append(failed, inst)
		}
	}
	if len(failed) == 0 {
		return 0, nil
	}
	m.log.Info("Restarting %d failed instance(s)", len(failed))
	go func() {
		sem := make(chan struct{}, concurrency)
		var wg sync.WaitGroup
		for _, inst := range failed {
			wg.Add(1)
			sem <- struct{}{}
			go func(inst *torinstance.Instance) {
				defer wg.Done()
				defer func() { <-sem }()
				if err := inst.Restart(ctx); err != nil && !errors.Is(err, torinstance.ErrAborted) {
					m.log.Error("Failed to restart instance %d: %v", inst.ID(), err)
				}
			}(inst)
		}
		wg.Wait()
		m.emit(Event{Type: EventInfo, Message: fmt.Sprintf("Restarted %d failed instance(s)", len(failed))})
	}()
	return len(failed), nil
}

// NewIdentity requests fresh circuits for one instance.
func (m *Manager) NewIdentity(id int) error {
	inst := m.getInstance(id)
	if inst == nil {
		return fmt.Errorf("instance %d not found", id)
	}
	return inst.NewIdentity()
}

// NewIdentityAll requests fresh circuits on every running instance.
func (m *Manager) NewIdentityAll() (ok int, failed int) {
	m.mu.RLock()
	instances := append([]*torinstance.Instance(nil), m.instances...)
	m.mu.RUnlock()

	var okN, failN atomic.Int32
	var wg sync.WaitGroup
	sem := make(chan struct{}, 32)
	for _, inst := range instances {
		if inst.State() != torinstance.StateRunning {
			continue
		}
		wg.Add(1)
		go func(inst *torinstance.Instance) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := inst.NewIdentity(); err != nil {
				failN.Add(1)
			} else {
				okN.Add(1)
			}
		}(inst)
	}
	wg.Wait()
	m.log.Info("New identity requested: %d ok, %d failed", okN.Load(), failN.Load())
	return int(okN.Load()), int(failN.Load())
}

// TestInstance runs a SOCKS health check plus a full Tor exit verification
// (check.torproject.org) through the instance, and records the exit IP.
func (m *Manager) TestInstance(id int) (*health.TorCheckResult, error) {
	inst := m.getInstance(id)
	if inst == nil {
		return nil, fmt.Errorf("instance %d not found", id)
	}
	inst.CheckHealth()
	m.mu.RLock()
	timeout := time.Duration(m.settings.ConnectionTimeoutSec) * time.Second
	host := m.settings.BindAddress
	m.mu.RUnlock()

	res := health.CheckPortOn(host, inst.SocksPort(), timeout)
	inst.RecordExitCheck(res.ExitIP, res.IsTor, res.Error)
	return &res, nil
}

// TestAll verifies Tor routing on every running instance with bounded concurrency.
func (m *Manager) TestAll(ctx context.Context) []health.TorCheckResult {
	m.mu.RLock()
	instances := append([]*torinstance.Instance(nil), m.instances...)
	timeout := time.Duration(m.settings.ConnectionTimeoutSec) * time.Second
	host := m.settings.BindAddress
	m.mu.RUnlock()

	var targets []*torinstance.Instance
	for _, inst := range instances {
		if inst.State() == torinstance.StateRunning {
			targets = append(targets, inst)
		}
	}

	results := make([]health.TorCheckResult, len(targets))
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for i, inst := range targets {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, inst *torinstance.Instance) {
			defer wg.Done()
			defer func() { <-sem }()
			r := health.CheckPortOn(host, inst.SocksPort(), timeout)
			inst.RecordExitCheck(r.ExitIP, r.IsTor, r.Error)
			results[i] = r
		}(i, inst)
	}
	wg.Wait()
	return results
}

// --- Status ---

// GetAllStatuses returns the status of every instance (in ID order).
func (m *Manager) GetAllStatuses() []torinstance.Status {
	m.mu.RLock()
	instances := m.instances
	m.mu.RUnlock()

	statuses := make([]torinstance.Status, len(instances))
	for i, inst := range instances {
		statuses[i] = inst.Status()
	}
	return statuses
}

// SortedStatuses returns statuses sorted by instance ID.
func (m *Manager) SortedStatuses() []torinstance.Status {
	statuses := m.GetAllStatuses()
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].InstanceID < statuses[j].InstanceID })
	return statuses
}

// GetInstanceStatus returns the status of a single instance.
func (m *Manager) GetInstanceStatus(id int) (torinstance.Status, bool) {
	inst := m.getInstance(id)
	if inst == nil {
		return torinstance.Status{}, false
	}
	return inst.Status(), true
}

// InstanceLog returns the tail of an instance's Tor log.
func (m *Manager) InstanceLog(id int, maxBytes int64) (string, error) {
	inst := m.getInstance(id)
	if inst == nil {
		return "", fmt.Errorf("instance %d not found", id)
	}
	return inst.TailLog(maxBytes)
}

// GetOverallStatus returns an aggregated status summary.
func (m *Manager) GetOverallStatus() OverallStatus {
	m.mu.RLock()
	state := m.state
	s := m.settings
	if state == StateStopped {
		s = m.cfg.Snapshot()
	}
	instances := m.instances
	startTime := m.startTime
	out := OverallStatus{
		ManagerState:         state,
		TotalInstances:       s.ProcessCount(),
		TotalPorts:           s.EndpointCount,
		PortRangeStart:       s.StartPort,
		PortRangeEnd:         s.LastPort(),
		ControlPortStart:     s.ControlPortStart,
		TorPath:              m.torPath,
		TorVersion:           m.torVersion,
		LastError:            m.lastError,
		ConfigPendingRestart: state != StateStopped && m.cfg.Snapshot() != m.settings,
	}
	m.mu.RUnlock()

	if state == StateRunning || state == StateStarting {
		out.UptimeSeconds = time.Since(startTime).Seconds()
	}
	out.StartProgress = int(m.startProgress.Load())
	out.StartTotal = int(m.startTotal.Load())

	var startupSum int64
	var startupN int64
	for _, inst := range instances {
		st := inst.Status()
		switch st.State {
		case torinstance.StateRunning:
			out.RunningInstances++
			out.RunningPorts += st.PortCount
			if st.Health == torinstance.HealthHealthy {
				out.HealthyInstances++
			}
		case torinstance.StateFailed:
			out.FailedInstances++
		case torinstance.StateStopped, torinstance.StateStopping:
			out.StoppedInstances++
		case torinstance.StateStarting:
			out.StartingInstances++
		case torinstance.StateBootstrapping:
			out.BootstrappingInstances++
		}
		if st.StartupMillis > 0 {
			startupSum += st.StartupMillis
			startupN++
		}
		out.TotalMemoryMB += st.MemoryMB
		out.TotalCPUPercent += st.CPUPercent
	}
	if startupN > 0 {
		out.AvgStartupMillis = startupSum / startupN
	}
	if len(instances) == 0 {
		out.StoppedInstances = out.TotalInstances
	}
	return out
}

// State returns the manager state.
func (m *Manager) State() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// IsStarted returns whether the manager is starting or running.
func (m *Manager) IsStarted() bool {
	st := m.State()
	return st == StateStarting || st == StateRunning
}

// StartProgress returns the current startup progress (completed, total).
func (m *Manager) StartProgress() (int, int) {
	return int(m.startProgress.Load()), int(m.startTotal.Load())
}

// ActiveSettings returns the settings snapshot of the current (or last) run.
func (m *Manager) ActiveSettings() config.Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.settings
}

// ProxyList returns the configured endpoints in the requested format:
// "url" (socks5h://host:port), "hostport" (host:port) or "json".
func (m *Manager) ProxyList(onlyRunning bool) []string {
	s := m.ActiveSettings()
	if m.State() == StateStopped {
		s = m.cfg.Snapshot()
	}
	statuses := m.GetAllStatuses()
	byPort := make(map[int]torinstance.State, len(statuses)*8)
	for _, st := range statuses {
		for p := st.SocksPort; p <= st.SocksPortEnd; p++ {
			byPort[p] = st.State
		}
	}
	out := make([]string, 0, s.EndpointCount)
	for p := s.StartPort; p <= s.LastPort(); p++ {
		if onlyRunning && byPort[p] != torinstance.StateRunning {
			continue
		}
		out = append(out, net.JoinHostPort(s.BindAddress, strconv.Itoa(p)))
	}
	return out
}

func (m *Manager) getInstance(id int) *torinstance.Instance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	idx := id - 1
	if idx < 0 || idx >= len(m.instances) {
		return nil
	}
	return m.instances[idx]
}

// --- Background monitoring ---

func (m *Manager) startBackground(s config.Settings) {
	m.bgStop = make(chan struct{})
	stop := m.bgStop

	m.bgWG.Add(2)
	go func() {
		defer m.bgWG.Done()
		t := time.NewTicker(time.Duration(s.HealthCheckIntervalSec) * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				m.runHealthChecks()
			}
		}
	}()
	go func() {
		defer m.bgWG.Done()
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		m.sampleStats()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				m.sampleStats()
			}
		}
	}()
}

func (m *Manager) stopBackground() {
	if m.bgStop != nil {
		close(m.bgStop)
		m.bgWG.Wait()
		m.bgStop = nil
	}
}

func (m *Manager) runHealthChecks() {
	m.mu.RLock()
	instances := append([]*torinstance.Instance(nil), m.instances...)
	m.mu.RUnlock()

	var unhealthy atomic.Int32
	var wg sync.WaitGroup
	sem := make(chan struct{}, 50)
	for _, inst := range instances {
		if inst.State() != torinstance.StateRunning {
			continue
		}
		wg.Add(1)
		go func(inst *torinstance.Instance) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if h := inst.CheckHealth(); h != torinstance.HealthHealthy && h != torinstance.HealthUnknown {
				unhealthy.Add(1)
				m.emit(Event{
					Type:       EventHealthUpdate,
					Message:    fmt.Sprintf("Instance %d (port %d) health: %s", inst.ID(), inst.SocksPort(), h),
					InstanceID: inst.ID(),
					Data:       map[string]interface{}{"health": string(h)},
				})
			}
		}(inst)
	}
	wg.Wait()
	if n := unhealthy.Load(); n > 0 {
		m.log.Warn("Health check: %d unhealthy instance(s)", n)
	}
}

func (m *Manager) sampleStats() {
	m.mu.RLock()
	instances := append([]*torinstance.Instance(nil), m.instances...)
	m.mu.RUnlock()
	for _, inst := range instances {
		inst.SampleStats()
	}
}

// --- Helpers ---

// FindPortConflicts verifies that none of the configured SOCKS/control ports are in use.
func FindPortConflicts(s config.Settings) []string {
	var conflicts []string
	for i := 0; i < s.EndpointCount; i++ {
		if p := s.StartPort + i; isPortInUse(s.BindAddress, p) {
			conflicts = append(conflicts, fmt.Sprintf("SOCKS port %d in use", p))
		}
	}
	for i := 0; i < s.ProcessCount(); i++ {
		if p := s.ControlPortStart + i; isPortInUse("127.0.0.1", p) {
			conflicts = append(conflicts, fmt.Sprintf("control port %d in use", p))
		}
	}
	return conflicts
}

func isPortInUse(host string, port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return true
	}
	ln.Close()
	return false
}

// cleanupStale terminates tor processes recorded in per-instance PID files.
func (m *Manager) cleanupStale(dataDir string, count int) int {
	killed := 0
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "instance_") {
			continue
		}
		pidFile := torinstance.PIDFilePath(filepath.Join(dataDir, e.Name()))
		b, err := os.ReadFile(pidFile)
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err == nil {
			if ok, _ := winsys.KillStaleTor(pid); ok {
				killed++
			}
		}
		_ = os.Remove(pidFile)
	}
	return killed
}

// Directory cache files that can be safely shared between Tor clients.
// Tor verifies the signatures of all of them on load.
var directoryCacheFiles = []string{
	"cached-certs",
	"cached-microdesc-consensus",
	"cached-microdescs",
	"cached-microdescs.new",
}

// seedDirectoryCache copies Tor's directory cache from src to dst when the
// source copy is newer. Returns the number of files copied.
func seedDirectoryCache(src, dst string) int {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return 0
	}
	n := 0
	for _, name := range directoryCacheFiles {
		sp := filepath.Join(src, name)
		si, err := os.Stat(sp)
		if err != nil || si.Size() == 0 {
			continue
		}
		dp := filepath.Join(dst, name)
		if di, err := os.Stat(dp); err == nil && !si.ModTime().After(di.ModTime()) {
			continue
		}
		if copyFile(sp, dp) == nil {
			n++
		}
	}
	return n
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tpm-tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

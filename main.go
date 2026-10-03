// Command TorProxyManager runs and supervises a pool of Tor processes, each
// exposing a local SOCKS5 endpoint (127.0.0.1:9050, 9051, ...), and serves a
// local web dashboard for monitoring and control.
//
// Typical usage:
//
//	TorProxyManager.exe                       GUI mode (tray icon + dashboard)
//	TorProxyManager.exe --headless            no tray/browser, start proxies, run until Ctrl+C
//	TorProxyManager.exe --list-proxies        print the configured endpoint list
//	TorProxyManager.exe --check-config        validate configuration, Tor path and ports
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"torproxymanager/internal/api"
	"torproxymanager/internal/config"
	"torproxymanager/internal/logger"
	"torproxymanager/internal/manager"
	"torproxymanager/internal/tray"
	"torproxymanager/internal/winsys"
	"torproxymanager/web"
)

const (
	appName      = "TorProxyManager"
	mutexName    = "TorProxyManager.SingleInstance" // AcquireSingleInstance adds the Local\ namespace
	autostartArg = "--autostart"
)

type options struct {
	configPath  string
	headless    bool
	noTray      bool
	noBrowser   bool
	start       bool
	autostart   bool
	version     bool
	listProxies bool
	checkConfig bool
	endpoints   int
	startPort   int
	webPort     int
	logLevel    string
}

func parseFlags(args []string, stderr io.Writer) (*options, error) {
	o := &options{}
	fs := flag.NewFlagSet(appName, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.configPath, "config", "", "path to config.json (default: <exe dir>\\config.json)")
	fs.BoolVar(&o.headless, "headless", false, "run without tray icon or browser and start all endpoints")
	fs.BoolVar(&o.noTray, "no-tray", false, "do not create a system tray icon")
	fs.BoolVar(&o.noBrowser, "no-browser", false, "do not open the dashboard in a browser on launch")
	fs.BoolVar(&o.start, "start", false, "start all endpoints immediately (overrides auto_start)")
	fs.BoolVar(&o.autostart, strings.TrimPrefix(autostartArg, "--"), false, "launched by Windows logon (implies --no-browser)")
	fs.BoolVar(&o.version, "version", false, "print version and exit")
	fs.BoolVar(&o.listProxies, "list-proxies", false, "print the configured SOCKS5 endpoints and exit")
	fs.BoolVar(&o.checkConfig, "check-config", false, "validate configuration, Tor executable and port availability, then exit")
	fs.IntVar(&o.endpoints, "endpoints", 0, "override endpoint_count for this run")
	fs.IntVar(&o.startPort, "start-port", 0, "override start_port for this run")
	fs.IntVar(&o.webPort, "web-port", 0, "override web_ui_port for this run")
	fs.StringVar(&o.logLevel, "log-level", "", "override log_level for this run (debug|info|warn|error)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "%s %s - multi-endpoint Tor SOCKS5 proxy manager\n\nUsage:\n  %s.exe [flags]\n\nFlags:\n",
			appName, api.AppVersion, appName)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if o.headless {
		o.noTray, o.noBrowser, o.start = true, true, true
	}
	if o.autostart {
		o.noBrowser = true
	}
	return o, nil
}

func main() { os.Exit(run()) }

func run() int {
	// GUI-subsystem builds have no console; attach to the parent's console if
	// launched from cmd/PowerShell so CLI output is visible.
	hasConsole := winsys.HasConsole() || winsys.AttachParentConsole()

	opts, err := parseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fatal(hasConsole, "Invalid command line", err.Error())
		return 2
	}

	if opts.version {
		inform(hasConsole, appName, fmt.Sprintf("%s %s", appName, api.AppVersion))
		return 0
	}

	appDir, err := executableDir()
	if err != nil {
		fatal(hasConsole, "Startup error", err.Error())
		return 1
	}

	cfg, err := loadConfig(opts, appDir)
	if err != nil {
		fatal(hasConsole, "Configuration error", err.Error())
		return 1
	}
	if errs := applyOverrides(cfg, opts); len(errs) > 0 {
		fatal(hasConsole, "Configuration error", joinErrors(errs))
		return 1
	}

	if opts.listProxies {
		return cmdListProxies(cfg.Snapshot())
	}
	if opts.checkConfig {
		return cmdCheckConfig(cfg.Snapshot(), appDir, hasConsole)
	}

	return runApp(opts, cfg, appDir, hasConsole)
}

// runApp is the long-running application mode.
func runApp(opts *options, cfg *config.Config, appDir string, hasConsole bool) int {
	s := cfg.Snapshot()
	dashboardURL := fmt.Sprintf("http://127.0.0.1:%d/", s.WebUIPort)

	release, already, err := winsys.AcquireSingleInstance(mutexName)
	if err != nil {
		fatal(hasConsole, "Startup error", "single-instance check failed: "+err.Error())
		return 1
	}
	if already {
		if hasConsole {
			fmt.Fprintf(os.Stderr, "%s is already running - dashboard: %s\n", appName, dashboardURL)
		}
		if !opts.noBrowser || !hasConsole {
			_ = winsys.OpenURL(dashboardURL)
		}
		return 0
	}
	defer release()

	// --- logging ---
	logDir := s.ResolveLogDir(appDir)
	log, err := logger.New(filepath.Join(logDir, "torproxymanager.log"), "main", s.LogMaxSizeMB, s.LogMaxFiles)
	if err != nil {
		fatal(hasConsole, "Logging error", err.Error())
		return 1
	}
	defer log.Close()
	if hasConsole {
		log.SetConsole(os.Stdout)
	} else {
		log.SetConsole(nil)
	}
	log.SetLevel(logger.ParseLevel(s.LogLevel))
	log.Info("%s %s starting (pid %d, dir %s)", appName, api.AppVersion, os.Getpid(), appDir)
	log.Info("Configuration: %s", cfg.FilePath())

	// --- process supervision ---
	mgr := manager.New(cfg, log, appDir)
	job, err := winsys.NewKillOnCloseJob()
	if err != nil {
		log.Warn("Job object unavailable (%v); tor processes may outlive a crash", err)
	} else {
		mgr.SetJob(job)
		defer job.Close() // kills any tor.exe still alive
	}

	reconcileAutoStart(log, cfg)

	// --- shutdown coordination ---
	quit := make(chan string, 1)
	var quitOnce sync.Once
	requestQuit := func(reason string) {
		quitOnce.Do(func() { quit <- reason })
	}

	// --- HTTP API / dashboard ---
	srv := api.New(mgr, cfg, log, web.Files, api.Hooks{
		OnConfigSaved: func(old, cur config.Settings) {
			if old.StartWithWindows != cur.StartWithWindows {
				syncAutoStart(log, cur.StartWithWindows)
			}
			if old.LogLevel != cur.LogLevel {
				log.SetLevel(logger.ParseLevel(cur.LogLevel))
				log.Info("Log level set to %s", cur.LogLevel)
			}
			if old.WebUIPort != cur.WebUIPort {
				log.Warn("web_ui_port changed to %d; takes effect after restarting the application", cur.WebUIPort)
			}
		},
		OnShutdown: func() { requestQuit("dashboard request") },
	})
	if err := srv.Start(); err != nil {
		log.Error("%v", err)
		fatal(hasConsole, "Cannot start dashboard",
			fmt.Sprintf("%v\n\nAnother program is using port %d. Change web_ui_port in\n%s\nor close the other program.",
				err, s.WebUIPort, cfg.FilePath()))
		return 1
	}

	// --- tray ---
	var tr *tray.Tray
	if s.EnableTray && !opts.noTray {
		tr, err = tray.New(appName+" - stopped", tray.Callbacks{
			OnOpenDashboard: func() { _ = winsys.OpenURL(dashboardURL) },
			OnCopyProxies: func() {
				list := mgr.ProxyList(true)
				if err := winsys.SetClipboardText(strings.Join(list, "\r\n")); err != nil {
					log.Warn("Copy proxy list: %v", err)
					return
				}
				log.Info("Copied %d proxy endpoints to the clipboard", len(list))
			},
			OnStartAll: func() { go startAll(mgr, log) },
			OnStopAll: func() {
				if err := mgr.StopAll(); err != nil {
					log.Error("Stop all: %v", err)
				}
			},
			OnRestartFailed: func() {
				if n, err := mgr.RestartFailed(); err != nil {
					log.Error("Restart failed instances: %v", err)
				} else {
					log.Info("Restarting %d failed instance(s)", n)
				}
			},
			OnNewIdentity: func() {
				ok, failed := mgr.NewIdentityAll()
				log.Info("New identity requested: %d ok, %d failed", ok, failed)
			},
			OnExit: func() { requestQuit("tray exit") },
		})
		if err != nil {
			log.Warn("System tray unavailable: %v", err)
			tr = nil
		} else {
			stopTray := make(chan struct{})
			defer close(stopTray)
			go trayUpdater(mgr, tr, stopTray)
		}
	}

	// --- signals ---
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		if sig, ok := <-sigCh; ok {
			requestQuit("signal " + sig.String())
		}
	}()

	if opts.start || s.AutoStart {
		go startAll(mgr, log)
	}
	if !opts.noBrowser && s.OpenDashboardOnLaunch {
		if err := winsys.OpenURL(dashboardURL); err != nil {
			log.Warn("Could not open browser: %v", err)
		}
	}
	if hasConsole {
		fmt.Fprintf(os.Stdout, "Dashboard: %s  (press Ctrl+C to stop)\n", dashboardURL)
	}

	reason := <-quit
	signal.Stop(sigCh)
	log.Info("Shutting down (%s)...", reason)
	if tr != nil {
		tr.SetStatus(tray.StatusStarting, appName+" - shutting down...")
	}

	stopped := make(chan struct{})
	go func() {
		if err := mgr.StopAll(); err != nil {
			log.Error("Stop all: %v", err)
		}
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(45 * time.Second):
		log.Warn("Timed out waiting for Tor instances to stop; terminating via job object")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = srv.Stop(ctx)
	cancel()
	if tr != nil {
		tr.Close()
	}
	log.Info("Goodbye")
	return 0
}

func startAll(mgr *manager.Manager, log *logger.Logger) {
	if err := mgr.StartAll(); err != nil {
		log.Error("Start all: %v", err)
	}
}

// trayUpdater keeps the tray icon tint, tooltip and menu in sync with manager state.
func trayUpdater(mgr *manager.Manager, tr *tray.Tray, stop <-chan struct{}) {
	events := mgr.Subscribe()
	defer mgr.Unsubscribe(events)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	var lastTip string
	var lastStatus tray.Status = -1
	update := func() {
		o := mgr.GetOverallStatus()
		st, tip := summarize(o)
		if st != lastStatus || tip != lastTip {
			tr.SetStatus(st, tip)
			lastStatus, lastTip = st, tip
		}
		header := fmt.Sprintf("%d / %d endpoints running", o.RunningInstances, o.TotalInstances)
		if o.TotalInstances == 0 {
			header = "Proxies stopped"
		}
		tr.SetMenuState(tray.MenuState{
			Header:     header,
			CanStart:   o.ManagerState == manager.StateStopped,
			CanStop:    o.ManagerState == manager.StateRunning || o.ManagerState == manager.StateStarting,
			HasFailed:  o.FailedInstances > 0,
			HasRunning: o.RunningInstances > 0,
		})
	}
	update()
	for {
		select {
		case <-stop:
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			switch ev.Type {
			case manager.EventStarted:
				update()
				level := 0
				if f, _ := ev.Data["failed"].(int); f > 0 {
					level = 1
				}
				tr.Notify(appName, ev.Message, level)
			case manager.EventError:
				tr.Notify(appName+" error", truncate(ev.Message, 250), 2)
			case manager.EventStarting, manager.EventStopped:
				update()
			}
		case <-tick.C:
			update() // coalesces bursts of per-instance updates
		}
	}
}

func summarize(o manager.OverallStatus) (tray.Status, string) {
	var st tray.Status
	switch {
	case o.ManagerState == manager.StateStopped && o.LastError != "":
		st = tray.StatusError
	case o.ManagerState == manager.StateStopped:
		st = tray.StatusStopped
	case o.ManagerState == manager.StateStarting || o.ManagerState == manager.StateStopping:
		st = tray.StatusStarting
	case o.RunningInstances == 0:
		st = tray.StatusError
	case o.FailedInstances > 0 || o.RunningInstances < o.TotalInstances:
		st = tray.StatusDegraded
	default:
		st = tray.StatusRunning
	}
	var tip string
	switch o.ManagerState {
	case manager.StateStopped:
		tip = appName + " - stopped"
	case manager.StateStarting:
		tip = fmt.Sprintf("%s - starting %d/%d", appName, o.StartProgress, o.StartTotal)
	case manager.StateStopping:
		tip = appName + " - stopping..."
	default:
		tip = fmt.Sprintf("%s - %d/%d running", appName, o.RunningInstances, o.TotalInstances)
		if o.FailedInstances > 0 {
			tip += fmt.Sprintf(", %d failed", o.FailedInstances)
		}
		tip += fmt.Sprintf("\nPorts %d-%d", o.PortRangeStart, o.PortRangeEnd)
	}
	return st, tip
}

// --- configuration ---

func executableDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot locate executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe), nil
}

// loadConfig loads the user config. On first run it is seeded from
// configs\default_config.json (if shipped) or from built-in defaults.
func loadConfig(opts *options, appDir string) (*config.Config, error) {
	path := opts.configPath
	if path == "" {
		path = filepath.Join(appDir, "config.json")
	} else if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		seed := filepath.Join(appDir, "configs", "default_config.json")
		if _, err := os.Stat(seed); err == nil {
			c, err := config.Load(seed)
			if err != nil {
				return nil, err
			}
			if err := c.SaveTo(path); err != nil {
				return nil, err
			}
			return c, nil
		}
	}
	return config.Load(path)
}

func applyOverrides(cfg *config.Config, o *options) []error {
	if o.endpoints == 0 && o.startPort == 0 && o.webPort == 0 && o.logLevel == "" {
		return cfg.Validate()
	}
	return cfg.Mutate(func(s *config.Settings) {
		if o.endpoints != 0 {
			s.EndpointCount = o.endpoints
		}
		if o.startPort != 0 {
			s.StartPort = o.startPort
		}
		if o.webPort != 0 {
			s.WebUIPort = o.webPort
		}
		if o.logLevel != "" {
			s.LogLevel = o.logLevel
		}
	})
}

func syncAutoStart(log *logger.Logger, enable bool) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if !enable && !winsys.IsAutoStartEnabled(appName) {
		return
	}
	// Re-registering on every launch keeps the path correct if the app was moved.
	if err := winsys.SetAutoStart(appName, exe, autostartArg, enable); err != nil {
		log.Warn("Could not update 'Start with Windows': %v", err)
	}
}

// reconcileAutoStart aligns the HKCU Run entry and start_with_windows at
// launch. An existing Run entry (e.g. created by the installer) is treated as
// the user's choice and reflected into the config rather than removed.
func reconcileAutoStart(log *logger.Logger, cfg *config.Config) {
	registered := winsys.IsAutoStartEnabled(appName)
	want := cfg.Snapshot().StartWithWindows
	switch {
	case registered && !want:
		if errs := cfg.Mutate(func(s *config.Settings) { s.StartWithWindows = true }); len(errs) == 0 {
			if err := cfg.Save(); err != nil {
				log.Warn("Could not save config: %v", err)
			}
		}
		syncAutoStart(log, true)
	case want:
		syncAutoStart(log, true)
	}
}

// --- CLI commands ---

func cmdListProxies(s config.Settings) int {
	host := s.BindAddress
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	var b strings.Builder
	for p := s.StartPort; p <= s.LastPort(); p++ {
		fmt.Fprintf(&b, "socks5h://%s:%d\n", host, p)
	}
	_, _ = os.Stdout.WriteString(b.String())
	return 0
}

func cmdCheckConfig(s config.Settings, appDir string, hasConsole bool) int {
	var b strings.Builder
	ok := true
	fmt.Fprintf(&b, "%s %s configuration check\n\n", appName, api.AppVersion)
	fmt.Fprintf(&b, "  SOCKS5 ports:   %s:%d-%d (%d endpoints)\n", s.BindAddress, s.StartPort, s.LastPort(), s.EndpointCount)
	fmt.Fprintf(&b, "  Control ports:  %d-%d\n", s.ControlPortStart, s.LastControlPort())
	fmt.Fprintf(&b, "  Dashboard:      http://127.0.0.1:%d/\n", s.WebUIPort)
	fmt.Fprintf(&b, "  Data directory: %s\n", s.ResolveDataDir(appDir))

	if torPath, err := s.ResolveTorPath(appDir); err != nil {
		ok = false
		fmt.Fprintf(&b, "\n[FAIL] Tor executable: %v\n", err)
	} else {
		fmt.Fprintf(&b, "  Tor executable: %s\n", torPath)
	}
	if t := s.ResolveTorrcTemplate(appDir); t != "" {
		fmt.Fprintf(&b, "  torrc template: %s\n", t)
	} else {
		fmt.Fprintf(&b, "  torrc template: (built-in)\n")
	}

	if conflicts := manager.FindPortConflicts(s); len(conflicts) > 0 {
		ok = false
		fmt.Fprintf(&b, "\n[FAIL] %d port(s) already in use:\n", len(conflicts))
		for i, c := range conflicts {
			if i == 10 {
				fmt.Fprintf(&b, "   ... and %d more\n", len(conflicts)-10)
				break
			}
			fmt.Fprintf(&b, "   - %s\n", c)
		}
	}
	if ok {
		fmt.Fprintf(&b, "\n[OK] Configuration is valid and all ports are free.\n")
	}
	if hasConsole {
		_, _ = os.Stdout.WriteString(b.String())
	} else {
		winsys.MessageBox(appName+" - configuration check", b.String(), !ok)
	}
	if !ok {
		return 1
	}
	return 0
}

// --- user feedback helpers ---

func fatal(hasConsole bool, title, msg string) {
	if hasConsole {
		fmt.Fprintf(os.Stderr, "%s: %s\n", title, msg)
		return
	}
	winsys.MessageBox(appName+" - "+title, msg, true)
}

func inform(hasConsole bool, title, msg string) {
	if hasConsole {
		fmt.Fprintln(os.Stdout, msg)
		return
	}
	winsys.MessageBox(title, msg, false)
}

func joinErrors(errs []error) string {
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = "- " + e.Error()
	}
	return strings.Join(parts, "\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

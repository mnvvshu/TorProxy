// Package config handles loading, validation, and persistence of application configuration.
//
// The configuration is split into two types:
//
//   - Settings: a plain value type holding every user-tunable option. It is
//     safe to copy and is what the rest of the application consumes.
//   - Config: a thread-safe wrapper around Settings that knows where it is
//     persisted on disk and serialises concurrent reads/updates.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Hard limits enforced by validation.
const (
	MinEndpoints = 1
	MaxEndpoints = 500
	MinPort      = 1024
	MaxPort      = 65535
)

// Settings holds all application settings with JSON serialization tags.
type Settings struct {
	StartPort              int     `json:"start_port"`
	EndpointCount          int     `json:"endpoint_count"`
	TorProcesses           int     `json:"tor_processes"`
	ControlPortStart       int     `json:"control_port_start"`
	TorExecutablePath      string  `json:"tor_executable_path"`
	TorrcTemplatePath      string  `json:"torrc_template_path"`
	DataDirectory          string  `json:"data_directory"`
	LogDirectory           string  `json:"log_directory"`
	StartupConcurrency     int     `json:"startup_concurrency"`
	ConnectionTimeoutSec   int     `json:"connection_timeout_seconds"`
	BootstrapTimeoutSec    int     `json:"bootstrap_timeout_seconds"`
	HealthCheckIntervalSec int     `json:"health_check_interval_seconds"`
	RetryLimit             int     `json:"retry_limit"`
	RetryBackoffSec        int     `json:"retry_backoff_seconds"`
	RetryBackoffMultiplier float64 `json:"retry_backoff_multiplier"`
	RetryMaxBackoffSec     int     `json:"retry_max_backoff_seconds"`
	AutoRestart            bool    `json:"auto_restart"`
	SeedDirectoryCache     bool    `json:"seed_directory_cache"`
	AutoStart              bool    `json:"auto_start"`
	StartWithWindows       bool    `json:"start_with_windows"`
	OpenDashboardOnLaunch  bool    `json:"open_dashboard_on_launch"`
	MinimizeToTray         bool    `json:"minimize_to_tray"`
	BrowserPath            string  `json:"browser_path"`
	WebUIPort              int     `json:"web_ui_port"`
	BindAddress            string  `json:"bind_address"`
	LogLevel               string  `json:"log_level"`
	LogMaxSizeMB           int     `json:"log_max_size_mb"`
	LogMaxFiles            int     `json:"log_max_files"`
	EnableTray             bool    `json:"enable_tray"`
}

// Config is a thread-safe, persistable wrapper around Settings.
type Config struct {
	mu       sync.RWMutex
	s        Settings
	filePath string
}

// DefaultSettings returns settings with safe production defaults.
func DefaultSettings() Settings {
	return Settings{
		StartPort:              9050,
		EndpointCount:          200,
		TorProcesses:           8,
		ControlPortStart:       30050,
		StartupConcurrency:     2,
		ConnectionTimeoutSec:   30,
		BootstrapTimeoutSec:    300,
		HealthCheckIntervalSec: 60,
		RetryLimit:             5,
		RetryBackoffSec:        5,
		RetryBackoffMultiplier: 2.0,
		RetryMaxBackoffSec:     300,
		AutoRestart:            true,
		SeedDirectoryCache:     true,
		AutoStart:              false,
		StartWithWindows:       false,
		OpenDashboardOnLaunch:  true,
		MinimizeToTray:         false,
		WebUIPort:              8470,
		BindAddress:            "127.0.0.1",
		LogLevel:               "info",
		LogMaxSizeMB:           10,
		LogMaxFiles:            5,
		EnableTray:             true,
	}
}

// DefaultConfig returns an unpersisted Config populated with defaults.
func DefaultConfig() *Config {
	return &Config{s: DefaultSettings()}
}

// NewFromSettings wraps existing settings (primarily for tests and CLI overrides).
func NewFromSettings(s Settings, path string) *Config {
	return &Config{s: s, filePath: path}
}

// Load reads configuration from a JSON file, falling back to defaults for
// missing fields. If the file does not exist, a default file is written.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()
	cfg.filePath = path

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if saveErr := cfg.Save(); saveErr != nil {
				return cfg, fmt.Errorf("config: failed to save default config: %w", saveErr)
			}
			return cfg, nil
		}
		return nil, fmt.Errorf("config: failed to read %s: %w", path, err)
	}

	// Strip a UTF-8 BOM (Notepad on older Windows versions adds one).
	data = []byte(strings.TrimPrefix(string(data), "\uFEFF"))

	s := DefaultSettings()
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("config: failed to parse %s: %w", path, err)
	}
	cfg.s = s
	return cfg, nil
}

// Save atomically writes the configuration to the file it was loaded from.
func (c *Config) Save() error {
	c.mu.RLock()
	path := c.filePath
	s := c.s
	c.mu.RUnlock()

	if path == "" {
		return errors.New("config: no file path set")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("config: failed to create directory %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(s, "", "    ")
	if err != nil {
		return fmt.Errorf("config: failed to marshal: %w", err)
	}

	// Write to a temp file then rename, so a crash never leaves a truncated config.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("config: failed to write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("config: failed to replace %s: %w", path, err)
	}
	return nil
}

// SaveTo writes configuration to a specific path and remembers it.
func (c *Config) SaveTo(path string) error {
	c.mu.Lock()
	c.filePath = path
	c.mu.Unlock()
	return c.Save()
}

// FilePath returns the path this config was loaded from / saved to.
func (c *Config) FilePath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.filePath
}

// Snapshot returns a copy of the current settings.
func (c *Config) Snapshot() Settings {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.s
}

// Set replaces all settings after validating them.
func (c *Config) Set(s Settings) []error {
	if errs := s.Validate(); len(errs) > 0 {
		return errs
	}
	c.mu.Lock()
	c.s = s
	c.mu.Unlock()
	return nil
}

// Mutate applies fn to a copy of the settings, validates, and commits on success.
func (c *Config) Mutate(fn func(s *Settings)) []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.s
	fn(&next)
	if errs := next.Validate(); len(errs) > 0 {
		return errs
	}
	c.s = next
	return nil
}

// Update applies a partial configuration update (from a decoded JSON object) and validates.
// Unknown keys are rejected so that typos do not silently do nothing.
func (c *Config) Update(updates map[string]interface{}) []error {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := json.Marshal(c.s)
	if err != nil {
		return []error{fmt.Errorf("config: failed to serialize current config: %w", err)}
	}

	var merged map[string]interface{}
	if err := json.Unmarshal(data, &merged); err != nil {
		return []error{fmt.Errorf("config: failed to deserialize current config: %w", err)}
	}

	var errs []error
	for k, v := range updates {
		if _, known := merged[k]; !known {
			errs = append(errs, fmt.Errorf("unknown configuration key %q", k))
			continue
		}
		merged[k] = v
	}
	if len(errs) > 0 {
		return errs
	}

	mergedData, err := json.Marshal(merged)
	if err != nil {
		return []error{fmt.Errorf("config: failed to serialize merged config: %w", err)}
	}

	var next Settings
	if err := json.Unmarshal(mergedData, &next); err != nil {
		return []error{fmt.Errorf("config: invalid value type: %w", err)}
	}

	if errs := next.Validate(); len(errs) > 0 {
		return errs
	}

	c.s = next
	return nil
}

// --- Convenience accessors on Config (thread-safe) ---

// LastPort returns the last SOCKS port in the configured range (inclusive).
func (c *Config) LastPort() int { s := c.Snapshot(); return s.LastPort() }

// Validate validates the current settings.
func (c *Config) Validate() []error { s := c.Snapshot(); return s.Validate() }

// --- Settings methods ---

// LastPort returns the last SOCKS port in the configured range (inclusive).
func (s Settings) LastPort() int {
	return s.StartPort + s.EndpointCount - 1
}

// LastControlPort returns the last control port in the configured range (inclusive).
func (s Settings) LastControlPort() int {
	return s.ControlPortStart + s.ProcessCount() - 1
}

// ProcessCount returns how many Tor processes serve the endpoint range. One
// Tor process can listen on many SOCKS ports, so far fewer processes than
// ports are needed (this is what keeps RAM and CPU low).
func (s Settings) ProcessCount() int {
	n := s.TorProcesses
	if n < 1 {
		n = 1
	}
	if s.EndpointCount > 0 && n > s.EndpointCount {
		n = s.EndpointCount
	}
	return n
}

// PortBlock returns the first SOCKS port and the port count served by the
// i-th (0-based) Tor process. Ports are split as evenly as possible.
func (s Settings) PortBlock(i int) (first, count int) {
	p := s.ProcessCount()
	base, rem := s.EndpointCount/p, s.EndpointCount%p
	first = s.StartPort + i*base + min(i, rem)
	count = base
	if i < rem {
		count++
	}
	return first, count
}

// Validate performs comprehensive validation of all settings.
// Returns a slice of validation errors (empty if valid).
func (s Settings) Validate() []error {
	var errs []error

	// Port range validation
	if s.StartPort < MinPort || s.StartPort > MaxPort {
		errs = append(errs, fmt.Errorf("start_port must be between %d and %d, got %d", MinPort, MaxPort, s.StartPort))
	}
	if s.EndpointCount < MinEndpoints || s.EndpointCount > MaxEndpoints {
		errs = append(errs, fmt.Errorf("endpoint_count must be between %d and %d, got %d", MinEndpoints, MaxEndpoints, s.EndpointCount))
	}
	if s.TorProcesses < 1 || s.TorProcesses > 64 {
		errs = append(errs, fmt.Errorf("tor_processes must be between 1 and 64, got %d", s.TorProcesses))
	}
	if s.LastPort() > MaxPort {
		errs = append(errs, fmt.Errorf("port range exceeds 65535: start_port=%d + endpoint_count=%d = last_port=%d",
			s.StartPort, s.EndpointCount, s.LastPort()))
	}

	// Control port validation
	if s.ControlPortStart < MinPort || s.ControlPortStart > MaxPort {
		errs = append(errs, fmt.Errorf("control_port_start must be between %d and %d, got %d", MinPort, MaxPort, s.ControlPortStart))
	}
	if s.LastControlPort() > MaxPort {
		errs = append(errs, fmt.Errorf("control port range exceeds 65535: control_port_start=%d + endpoint_count=%d = %d",
			s.ControlPortStart, s.EndpointCount, s.LastControlPort()))
	}

	// Detect overlapping port ranges
	socksStart, socksEnd := s.StartPort, s.LastPort()
	ctrlStart, ctrlEnd := s.ControlPortStart, s.LastControlPort()
	if socksStart <= ctrlEnd && ctrlStart <= socksEnd {
		errs = append(errs, fmt.Errorf("SOCKS port range [%d-%d] overlaps with control port range [%d-%d]",
			socksStart, socksEnd, ctrlStart, ctrlEnd))
	}

	// Bind address must be loopback
	if s.BindAddress != "127.0.0.1" && s.BindAddress != "localhost" && s.BindAddress != "::1" {
		ip := net.ParseIP(s.BindAddress)
		if ip == nil || !ip.IsLoopback() {
			errs = append(errs, fmt.Errorf(
				"bind_address %q is not a loopback address — exposing SOCKS5 ports publicly is a security risk; "+
					"set to 127.0.0.1", s.BindAddress))
		}
	}

	// Tor executable path
	if s.TorExecutablePath != "" {
		if !isCleanPath(s.TorExecutablePath) {
			errs = append(errs, fmt.Errorf("tor_executable_path contains suspicious characters: %q", s.TorExecutablePath))
		} else if info, err := os.Stat(s.TorExecutablePath); err != nil {
			errs = append(errs, fmt.Errorf("tor_executable_path not accessible: %w", err))
		} else if info.IsDir() {
			errs = append(errs, fmt.Errorf("tor_executable_path is a directory, not a file: %s", s.TorExecutablePath))
		} else if !strings.EqualFold(filepath.Ext(s.TorExecutablePath), ".exe") && os.PathSeparator == '\\' {
			errs = append(errs, fmt.Errorf("tor_executable_path must point to an .exe file: %s", s.TorExecutablePath))
		}
	}

	if s.TorrcTemplatePath != "" {
		if !isCleanPath(s.TorrcTemplatePath) {
			errs = append(errs, fmt.Errorf("torrc_template_path contains suspicious characters: %q", s.TorrcTemplatePath))
		} else if _, err := os.Stat(s.TorrcTemplatePath); err != nil {
			errs = append(errs, fmt.Errorf("torrc_template_path not accessible: %w", err))
		}
	}

	if s.BrowserPath != "" && !isCleanPath(s.BrowserPath) {
		errs = append(errs, fmt.Errorf("browser_path contains suspicious characters: %q", s.BrowserPath))
	}

	// Concurrency limits
	if s.StartupConcurrency < 1 || s.StartupConcurrency > 50 {
		errs = append(errs, fmt.Errorf("startup_concurrency must be between 1 and 50, got %d", s.StartupConcurrency))
	}

	// Timeout bounds
	if s.ConnectionTimeoutSec < 5 || s.ConnectionTimeoutSec > 300 {
		errs = append(errs, fmt.Errorf("connection_timeout_seconds must be between 5 and 300, got %d", s.ConnectionTimeoutSec))
	}
	if s.BootstrapTimeoutSec < 30 || s.BootstrapTimeoutSec > 3600 {
		errs = append(errs, fmt.Errorf("bootstrap_timeout_seconds must be between 30 and 3600, got %d", s.BootstrapTimeoutSec))
	}
	if s.HealthCheckIntervalSec < 10 || s.HealthCheckIntervalSec > 3600 {
		errs = append(errs, fmt.Errorf("health_check_interval_seconds must be between 10 and 3600, got %d", s.HealthCheckIntervalSec))
	}

	// Retry policy
	if s.RetryLimit < 0 || s.RetryLimit > 100 {
		errs = append(errs, fmt.Errorf("retry_limit must be between 0 and 100, got %d", s.RetryLimit))
	}
	if s.RetryBackoffSec < 1 || s.RetryBackoffSec > 60 {
		errs = append(errs, fmt.Errorf("retry_backoff_seconds must be between 1 and 60, got %d", s.RetryBackoffSec))
	}
	if s.RetryBackoffMultiplier < 1.0 || s.RetryBackoffMultiplier > 10.0 {
		errs = append(errs, fmt.Errorf("retry_backoff_multiplier must be between 1.0 and 10.0, got %.1f", s.RetryBackoffMultiplier))
	}
	if s.RetryMaxBackoffSec < s.RetryBackoffSec || s.RetryMaxBackoffSec > 3600 {
		errs = append(errs, fmt.Errorf("retry_max_backoff_seconds must be between retry_backoff_seconds (%d) and 3600, got %d",
			s.RetryBackoffSec, s.RetryMaxBackoffSec))
	}

	// Web UI port
	if s.WebUIPort < MinPort || s.WebUIPort > MaxPort {
		errs = append(errs, fmt.Errorf("web_ui_port must be between %d and %d, got %d", MinPort, MaxPort, s.WebUIPort))
	}
	if s.WebUIPort >= s.StartPort && s.WebUIPort <= s.LastPort() {
		errs = append(errs, fmt.Errorf("web_ui_port %d conflicts with SOCKS port range [%d-%d]",
			s.WebUIPort, s.StartPort, s.LastPort()))
	}
	if s.WebUIPort >= s.ControlPortStart && s.WebUIPort <= s.LastControlPort() {
		errs = append(errs, fmt.Errorf("web_ui_port %d conflicts with control port range [%d-%d]",
			s.WebUIPort, s.ControlPortStart, s.LastControlPort()))
	}

	// Log settings
	switch strings.ToLower(s.LogLevel) {
	case "", "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log_level must be one of debug, info, warn, error; got %q", s.LogLevel))
	}
	if s.LogMaxSizeMB < 1 || s.LogMaxSizeMB > 100 {
		errs = append(errs, fmt.Errorf("log_max_size_mb must be between 1 and 100, got %d", s.LogMaxSizeMB))
	}
	if s.LogMaxFiles < 1 || s.LogMaxFiles > 50 {
		errs = append(errs, fmt.Errorf("log_max_files must be between 1 and 50, got %d", s.LogMaxFiles))
	}

	// Path safety checks
	if s.DataDirectory != "" && !isCleanPath(s.DataDirectory) {
		errs = append(errs, fmt.Errorf("data_directory contains suspicious characters: %q", s.DataDirectory))
	}
	if s.LogDirectory != "" && !isCleanPath(s.LogDirectory) {
		errs = append(errs, fmt.Errorf("log_directory contains suspicious characters: %q", s.LogDirectory))
	}

	return errs
}

// ResolveDataDir returns the effective data directory.
func (s Settings) ResolveDataDir(appDir string) string {
	if s.DataDirectory != "" {
		return s.DataDirectory
	}
	return filepath.Join(appDir, "data")
}

// ResolveLogDir returns the effective log directory.
func (s Settings) ResolveLogDir(appDir string) string {
	if s.LogDirectory != "" {
		return s.LogDirectory
	}
	return filepath.Join(appDir, "logs")
}

// ResolveTorrcTemplate returns the path of a torrc template to use, or "" to
// use the built-in template.
func (s Settings) ResolveTorrcTemplate(appDir string) string {
	if s.TorrcTemplatePath != "" {
		return s.TorrcTemplatePath
	}
	for _, p := range []string{
		filepath.Join(appDir, "configs", "torrc.template"),
		filepath.Join(appDir, "torrc.template"),
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// TorSearchPaths returns the ordered list of locations searched for tor.exe.
func TorSearchPaths(appDir string) []string {
	exe := "tor.exe"
	if os.PathSeparator != '\\' {
		exe = "tor"
	}
	paths := []string{
		filepath.Join(appDir, "tor", exe),
		filepath.Join(appDir, "Tor", exe),
		filepath.Join(appDir, "tor", "tor", exe), // Expert Bundle zip layout
		filepath.Join(appDir, exe),
	}

	if profile := os.Getenv("USERPROFILE"); profile != "" {
		paths = append(paths,
			filepath.Join(profile, "Desktop", "Tor Browser", "Browser", "TorBrowser", "Tor", exe),
			filepath.Join(profile, "AppData", "Local", "Tor Browser", "Browser", "TorBrowser", "Tor", exe),
		)
	}
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		paths = append(paths,
			filepath.Join(pf, "Tor", exe),
			filepath.Join(pf, "Tor Browser", "Browser", "TorBrowser", "Tor", exe),
		)
	}
	if pf86 := os.Getenv("ProgramFiles(x86)"); pf86 != "" {
		paths = append(paths, filepath.Join(pf86, "Tor", exe))
	}
	return paths
}

// ResolveTorPath returns the Tor executable path, searching common locations if not set.
func (s Settings) ResolveTorPath(appDir string) (string, error) {
	if s.TorExecutablePath != "" {
		if info, err := os.Stat(s.TorExecutablePath); err == nil && !info.IsDir() {
			return s.TorExecutablePath, nil
		}
		return "", fmt.Errorf("configured Tor path not found: %s", s.TorExecutablePath)
	}

	for _, p := range TorSearchPaths(appDir) {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}

	return "", errors.New(
		"tor.exe not found; please either:\n" +
			"  1. Place tor.exe in the application directory under tor\\tor.exe\n" +
			"     (run scripts\\download-tor.ps1 to fetch the official Tor Expert Bundle)\n" +
			"  2. Set tor_executable_path in the configuration file\n" +
			"  3. Install the Tor Expert Bundle from https://www.torproject.org/download/tor/")
}

// isCleanPath validates that a path doesn't contain shell metacharacters or traversal attempts.
// Note: parentheses are permitted because "C:\Program Files (x86)" is a legitimate location.
func isCleanPath(p string) bool {
	dangerous := []string{"|", "&", ";", "`", "$", "{", "}", "<", ">", "!", "\"", "\n", "\r", "\x00"}
	for _, d := range dangerous {
		if strings.Contains(p, d) {
			return false
		}
	}
	// Prevent directory traversal: reject any ".." path element.
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return false
		}
	}
	return true
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hasErrContaining(errs []error, substr string) bool {
	for _, e := range errs {
		if e != nil && strings.Contains(e.Error(), substr) {
			return true
		}
	}
	return false
}

func TestDefaultSettings(t *testing.T) {
	s := DefaultSettings()
	if s.StartPort != 9050 {
		t.Errorf("expected default start_port 9050, got %d", s.StartPort)
	}
	if s.EndpointCount != 200 {
		t.Errorf("expected default endpoint_count 200, got %d", s.EndpointCount)
	}
	if s.BindAddress != "127.0.0.1" {
		t.Errorf("expected default bind_address 127.0.0.1, got %s", s.BindAddress)
	}
	if !s.AutoRestart {
		t.Error("expected auto_restart enabled by default")
	}
}

func TestLastPort(t *testing.T) {
	tests := []struct {
		name     string
		start    int
		count    int
		expected int
	}{
		{"200 endpoints from 9050", 9050, 200, 9249},
		{"300 endpoints from 9050", 9050, 300, 9349},
		{"2 endpoints from 9050", 9050, 2, 9051},
		{"1 endpoint from 9050", 9050, 1, 9050},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := DefaultSettings()
			s.StartPort = tt.start
			s.EndpointCount = tt.count
			if got := s.LastPort(); got != tt.expected {
				t.Errorf("LastPort() = %d, want %d", got, tt.expected)
			}
		})
	}
}

func TestValidate_ValidConfig(t *testing.T) {
	if errs := DefaultSettings().Validate(); len(errs) > 0 {
		t.Errorf("default config should be valid, got errors: %v", errs)
	}
}

func TestValidate_300Endpoints(t *testing.T) {
	s := DefaultSettings()
	s.EndpointCount = 300
	if errs := s.Validate(); len(errs) > 0 {
		t.Errorf("300 endpoints should be valid, got: %v", errs)
	}
}

func TestValidate_Table(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(s *Settings)
		want   string
	}{
		{"start port below 1024", func(s *Settings) { s.StartPort = 80 }, "start_port"},
		{"port overflow", func(s *Settings) { s.StartPort = 65300; s.EndpointCount = 500 }, "exceeds 65535"},
		{"too many endpoints", func(s *Settings) { s.EndpointCount = 501 }, "endpoint_count"},
		{"zero endpoints", func(s *Settings) { s.EndpointCount = 0 }, "endpoint_count"},
		{"overlapping ranges", func(s *Settings) { s.ControlPortStart = 9100 }, "overlaps"},
		{"non-loopback bind", func(s *Settings) { s.BindAddress = "0.0.0.0" }, "loopback"},
		{"web ui in socks range", func(s *Settings) { s.WebUIPort = 9100 }, "web_ui_port"},
		{"web ui in control range", func(s *Settings) { s.WebUIPort = 30052 }, "web_ui_port"},
		{"concurrency too high", func(s *Settings) { s.StartupConcurrency = 51 }, "startup_concurrency"},
		{"timeout too low", func(s *Settings) { s.ConnectionTimeoutSec = 1 }, "connection_timeout_seconds"},
		{"bad log level", func(s *Settings) { s.LogLevel = "verbose" }, "log_level"},
		{"bad multiplier", func(s *Settings) { s.RetryBackoffMultiplier = 0.5 }, "retry_backoff_multiplier"},
		{"max backoff below base", func(s *Settings) { s.RetryBackoffSec = 30; s.RetryMaxBackoffSec = 10 }, "retry_max_backoff_seconds"},
		{"missing tor path", func(s *Settings) { s.TorExecutablePath = `C:\definitely\missing\tor.exe` }, "tor_executable_path"},
		{"shell metachar in data dir", func(s *Settings) { s.DataDirectory = `C:\data&calc` }, "data_directory"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := DefaultSettings()
			tt.mutate(&s)
			errs := s.Validate()
			if !hasErrContaining(errs, tt.want) {
				t.Errorf("expected error containing %q, got %v", tt.want, errs)
			}
		})
	}
}

func TestValidate_LoopbackVariants(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "localhost", "::1", "127.0.0.2"} {
		s := DefaultSettings()
		s.BindAddress = addr
		if hasErrContaining(s.Validate(), "loopback") {
			t.Errorf("bind_address %q should be accepted as loopback", addr)
		}
	}
}

func TestLoadAndSave(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "test_config.json")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Snapshot().StartPort != 9050 {
		t.Errorf("expected default start_port 9050, got %d", cfg.Snapshot().StartPort)
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("expected default config to be written: %v", err)
	}

	if errs := cfg.Mutate(func(s *Settings) { s.EndpointCount = 300 }); len(errs) > 0 {
		t.Fatalf("Mutate failed: %v", errs)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	cfg2, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Reload failed: %v", err)
	}
	if cfg2.Snapshot().EndpointCount != 300 {
		t.Errorf("expected endpoint_count 300 after reload, got %d", cfg2.Snapshot().EndpointCount)
	}
}

func TestLoad_PartialFileUsesDefaults(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "partial.json")
	// UTF-8 BOM + partial content, as Notepad might produce.
	if err := os.WriteFile(cfgPath, []byte("\uFEFF{\"endpoint_count\": 250}"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	s := cfg.Snapshot()
	if s.EndpointCount != 250 {
		t.Errorf("expected endpoint_count 250, got %d", s.EndpointCount)
	}
	if s.StartPort != 9050 || s.WebUIPort != 8470 {
		t.Errorf("missing fields should fall back to defaults, got start=%d web=%d", s.StartPort, s.WebUIPort)
	}
}

func TestLoad_Malformed(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(cfgPath, []byte("{not json"), 0o644)
	if _, err := Load(cfgPath); err == nil {
		t.Error("expected parse error for malformed JSON")
	}
}

func TestUpdate(t *testing.T) {
	cfg := NewFromSettings(DefaultSettings(), filepath.Join(t.TempDir(), "update_test.json"))

	errs := cfg.Update(map[string]interface{}{
		"endpoint_count": float64(300),
		"start_port":     float64(10000),
	})
	if len(errs) > 0 {
		t.Fatalf("valid update returned errors: %v", errs)
	}
	s := cfg.Snapshot()
	if s.EndpointCount != 300 {
		t.Errorf("expected endpoint_count 300, got %d", s.EndpointCount)
	}
	if s.StartPort != 10000 {
		t.Errorf("expected start_port 10000, got %d", s.StartPort)
	}
}

func TestUpdate_InvalidLeavesConfigUnchanged(t *testing.T) {
	cfg := NewFromSettings(DefaultSettings(), "")
	errs := cfg.Update(map[string]interface{}{"start_port": float64(80)})
	if len(errs) == 0 {
		t.Fatal("expected validation error for invalid update")
	}
	if cfg.Snapshot().StartPort != 9050 {
		t.Error("invalid update must not modify the configuration")
	}
}

func TestUpdate_UnknownKey(t *testing.T) {
	cfg := NewFromSettings(DefaultSettings(), "")
	if errs := cfg.Update(map[string]interface{}{"strat_port": float64(9000)}); !hasErrContaining(errs, "unknown") {
		t.Errorf("expected unknown key error, got %v", errs)
	}
}

func TestUpdate_WrongType(t *testing.T) {
	cfg := NewFromSettings(DefaultSettings(), "")
	if errs := cfg.Update(map[string]interface{}{"start_port": "abc"}); len(errs) == 0 {
		t.Error("expected type error")
	}
}

func TestIsCleanPath(t *testing.T) {
	tests := []struct {
		path  string
		clean bool
	}{
		{`C:\Users\test\data`, true},
		{`C:\Users\test\..\admin`, false},
		{`C:\Users\test|malicious`, false},
		{`/var/lib/tor`, true},
		{`$(whoami)`, false},
		{`C:\normal\path\file.txt`, true},
		{`C:\Program Files (x86)\Tor\tor.exe`, true},
		{`C:\files..backup\x`, true},
		{"C:\\evil\npath", false},
	}

	for _, tt := range tests {
		if got := isCleanPath(tt.path); got != tt.clean {
			t.Errorf("isCleanPath(%q) = %v, want %v", tt.path, got, tt.clean)
		}
	}
}

func TestResolveTorPath_Configured(t *testing.T) {
	tmpDir := t.TempDir()
	fakeTor := filepath.Join(tmpDir, "tor.exe")
	os.WriteFile(fakeTor, []byte("fake"), 0o755)

	s := DefaultSettings()
	s.TorExecutablePath = fakeTor

	path, err := s.ResolveTorPath(tmpDir)
	if err != nil {
		t.Fatalf("ResolveTorPath failed: %v", err)
	}
	if path != fakeTor {
		t.Errorf("expected %s, got %s", fakeTor, path)
	}
}

func TestResolveTorPath_AppDirSearch(t *testing.T) {
	appDir := t.TempDir()
	torDir := filepath.Join(appDir, "tor")
	os.MkdirAll(torDir, 0o755)
	exe := filepath.Base(TorSearchPaths(appDir)[0])
	os.WriteFile(filepath.Join(torDir, exe), []byte("fake"), 0o755)

	path, err := DefaultSettings().ResolveTorPath(appDir)
	if err != nil {
		t.Fatalf("expected tor to be found in app dir: %v", err)
	}
	if filepath.Dir(path) != torDir {
		t.Errorf("unexpected tor path %s", path)
	}
}

func TestResolveTorPath_NotFound(t *testing.T) {
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("ProgramFiles", t.TempDir())
	t.Setenv("ProgramFiles(x86)", t.TempDir())
	if _, err := DefaultSettings().ResolveTorPath(t.TempDir()); err == nil {
		t.Error("expected error when tor.exe not found")
	}
}

func TestSaveIsAtomic(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "atomic.json")
	cfg := NewFromSettings(DefaultSettings(), cfgPath)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfgPath + ".tmp"); !os.IsNotExist(err) {
		t.Error("temporary file should not remain after save")
	}
}

package diagnostics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"torproxymanager/internal/config"
	"torproxymanager/internal/health"
	"torproxymanager/internal/logger"
	"torproxymanager/internal/torinstance"
)

func sampleInput() Input {
	s := config.DefaultSettings()
	s.EndpointCount = 3
	return Input{
		AppVersion:    "9.9.9",
		ManagerState:  "running",
		TorPath:       `C:\tor\tor.exe`,
		TorVersion:    "0.4.8.12",
		UptimeSeconds: 125,
		Settings:      s,
		Instances: []torinstance.Status{
			{InstanceID: 1, SocksPort: 9050, State: torinstance.StateRunning, MemoryMB: 25, StartupMillis: 4000, TotalRestarts: 1},
			{InstanceID: 2, SocksPort: 9051, State: torinstance.StateRunning, MemoryMB: 27, StartupMillis: 6000},
			{InstanceID: 3, SocksPort: 9052, State: torinstance.StateFailed, LastError: "bootstrap timed out after 300s on port 9052"},
		},
		HealthResults: []health.TorCheckResult{{Port: 9050, IsListening: true, SOCKSHandshake: true, IsTor: true, ExitIP: "198.51.100.7"}},
		RecentLogs:    []logger.Entry{{Time: time.Now(), Level: logger.LevelInfo.String(), Component: "manager", Message: "hello"}},
	}
}

func TestGenerateSummary(t *testing.T) {
	r := Generate(sampleInput())
	m := r.ManagerStatus
	if m.TotalInstances != 3 || m.RunningInstances != 2 || m.FailedInstances != 1 {
		t.Fatalf("counts wrong: %+v", m)
	}
	if m.PortRange != "9050-9052" {
		t.Errorf("PortRange = %q", m.PortRange)
	}
	if r.Summary.TotalTorMemoryMB != 52 || r.Summary.AvgStartupMillis != 5000 || r.Summary.MaxStartupMillis != 6000 {
		t.Errorf("summary wrong: %+v", r.Summary)
	}
	if r.Summary.TotalRestarts != 1 {
		t.Errorf("TotalRestarts = %d", r.Summary.TotalRestarts)
	}
	if len(r.Summary.TopErrors) != 1 || strings.ContainsAny(r.Summary.TopErrors[0].Error, "0123456789") {
		t.Errorf("errors should be grouped with digits normalised: %+v", r.Summary.TopErrors)
	}
	if r.Summary.ByState["running"] != 2 || r.Summary.ByState["failed"] != 1 {
		t.Errorf("ByState = %v", r.Summary.ByState)
	}
}

func TestNormaliseErrorGroupsPorts(t *testing.T) {
	a := normaliseError("connection refused on 127.0.0.1:9050")
	b := normaliseError("connection refused on 127.0.0.1:9177")
	if a != b {
		t.Fatalf("expected identical groups, got %q vs %q", a, b)
	}
}

func TestExportJSONAndText(t *testing.T) {
	r := Generate(sampleInput())
	data, err := r.ExportJSON()
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]interface{}
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if back["app_version"] != "9.9.9" {
		t.Errorf("app_version = %v", back["app_version"])
	}
	if _, leaked := back["Hostname"]; leaked {
		t.Errorf("hostname must never be exported")
	}

	txt := r.ExportText()
	for _, want := range []string{"Diagnostic Report", "9.9.9", "9050-9052", "Tor Verification", "198.51.100.7", "hello"} {
		if !strings.Contains(txt, want) {
			t.Errorf("text report missing %q", want)
		}
	}

	path := filepath.Join(t.TempDir(), "diag.json")
	if err := r.ExportToFile(path); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || !json.Valid(b) {
		t.Fatalf("ExportToFile wrote invalid data: %v", err)
	}
}

func TestEmptyInput(t *testing.T) {
	r := Generate(Input{Settings: config.DefaultSettings()})
	if r.ManagerStatus.TotalInstances != 0 || r.Summary.AvgStartupMillis != 0 {
		t.Fatalf("unexpected: %+v", r)
	}
	_ = r.ExportText()
}

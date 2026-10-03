// Package diagnostics generates exportable diagnostic reports.
package diagnostics

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"torproxymanager/internal/config"
	"torproxymanager/internal/health"
	"torproxymanager/internal/logger"
	"torproxymanager/internal/torinstance"
)

// Input collects everything needed to build a report.
type Input struct {
	AppVersion    string
	ManagerState  string
	TorPath       string
	TorVersion    string
	UptimeSeconds float64
	Settings      config.Settings
	Instances     []torinstance.Status
	HealthResults []health.TorCheckResult
	RecentLogs    []logger.Entry
}

// Report is a complete diagnostic snapshot.
type Report struct {
	GeneratedAt   string                  `json:"generated_at"`
	AppVersion    string                  `json:"app_version"`
	GoVersion     string                  `json:"go_version"`
	OS            string                  `json:"os"`
	Arch          string                  `json:"arch"`
	NumCPU        int                     `json:"num_cpu"`
	Hostname      string                  `json:"-"` // intentionally never exported
	MemoryStats   MemoryInfo              `json:"memory_stats"`
	ManagerStatus ManagerInfo             `json:"manager_status"`
	Settings      config.Settings         `json:"settings"`
	Summary       Summary                 `json:"summary"`
	Instances     []torinstance.Status    `json:"instances"`
	HealthResults []health.TorCheckResult `json:"health_results,omitempty"`
	RecentLogs    []string                `json:"recent_logs,omitempty"`
}

// MemoryInfo holds memory usage statistics of the manager process.
type MemoryInfo struct {
	AllocMB      float64 `json:"alloc_mb"`
	TotalAllocMB float64 `json:"total_alloc_mb"`
	SysMB        float64 `json:"sys_mb"`
	NumGC        uint32  `json:"num_gc"`
	Goroutines   int     `json:"goroutines"`
}

// ManagerInfo holds manager state.
type ManagerInfo struct {
	State            string  `json:"state"`
	TorPath          string  `json:"tor_path"`
	TorVersion       string  `json:"tor_version"`
	UptimeSeconds    float64 `json:"uptime_seconds"`
	TotalInstances   int     `json:"total_instances"`
	RunningInstances int     `json:"running_instances"`
	FailedInstances  int     `json:"failed_instances"`
	PortRange        string  `json:"port_range"`
	ControlPortRange string  `json:"control_port_range"`
}

// Summary aggregates per-instance metrics.
type Summary struct {
	ByState          map[string]int `json:"by_state"`
	ByHealth         map[string]int `json:"by_health"`
	TotalTorMemoryMB float64        `json:"total_tor_memory_mb"`
	AvgStartupMillis int64          `json:"avg_startup_ms"`
	MaxStartupMillis int64          `json:"max_startup_ms"`
	TotalRestarts    int            `json:"total_restarts"`
	TopErrors        []ErrorCount   `json:"top_errors,omitempty"`
}

// ErrorCount groups identical instance errors.
type ErrorCount struct {
	Error string `json:"error"`
	Count int    `json:"count"`
}

// Generate creates a full diagnostic report.
func Generate(in Input) *Report {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	sum := Summary{ByState: map[string]int{}, ByHealth: map[string]int{}}
	errCounts := map[string]int{}
	var startupTotal, startupN int64
	running, failed := 0, 0
	for _, inst := range in.Instances {
		sum.ByState[string(inst.State)]++
		sum.ByHealth[string(inst.Health)]++
		sum.TotalTorMemoryMB += inst.MemoryMB
		sum.TotalRestarts += inst.TotalRestarts
		if inst.StartupMillis > 0 {
			startupTotal += inst.StartupMillis
			startupN++
			if inst.StartupMillis > sum.MaxStartupMillis {
				sum.MaxStartupMillis = inst.StartupMillis
			}
		}
		if inst.LastError != "" {
			errCounts[normaliseError(inst.LastError)]++
		}
		switch inst.State {
		case torinstance.StateRunning:
			running++
		case torinstance.StateFailed:
			failed++
		}
	}
	if startupN > 0 {
		sum.AvgStartupMillis = startupTotal / startupN
	}
	for e, c := range errCounts {
		sum.TopErrors = append(sum.TopErrors, ErrorCount{Error: e, Count: c})
	}
	sort.Slice(sum.TopErrors, func(i, j int) bool { return sum.TopErrors[i].Count > sum.TopErrors[j].Count })
	if len(sum.TopErrors) > 10 {
		sum.TopErrors = sum.TopErrors[:10]
	}

	logs := make([]string, 0, len(in.RecentLogs))
	for _, e := range in.RecentLogs {
		logs = append(logs, e.Format())
	}

	s := in.Settings
	return &Report{
		GeneratedAt: time.Now().Format(time.RFC3339),
		AppVersion:  in.AppVersion,
		GoVersion:   runtime.Version(),
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		NumCPU:      runtime.NumCPU(),
		MemoryStats: MemoryInfo{
			AllocMB:      float64(ms.Alloc) / 1024 / 1024,
			TotalAllocMB: float64(ms.TotalAlloc) / 1024 / 1024,
			SysMB:        float64(ms.Sys) / 1024 / 1024,
			NumGC:        ms.NumGC,
			Goroutines:   runtime.NumGoroutine(),
		},
		ManagerStatus: ManagerInfo{
			State:            in.ManagerState,
			TorPath:          in.TorPath,
			TorVersion:       in.TorVersion,
			UptimeSeconds:    in.UptimeSeconds,
			TotalInstances:   len(in.Instances),
			RunningInstances: running,
			FailedInstances:  failed,
			PortRange:        fmt.Sprintf("%d-%d", s.StartPort, s.LastPort()),
			ControlPortRange: fmt.Sprintf("%d-%d", s.ControlPortStart, s.LastControlPort()),
		},
		Settings:      s,
		Summary:       sum,
		Instances:     in.Instances,
		HealthResults: in.HealthResults,
		RecentLogs:    logs,
	}
}

// normaliseError strips instance-specific details (ports, PIDs) so that
// identical root causes are grouped together.
func normaliseError(e string) string {
	var sb strings.Builder
	inDigits := false
	for _, r := range e {
		if r >= '0' && r <= '9' {
			if !inDigits {
				sb.WriteByte('N')
				inDigits = true
			}
			continue
		}
		inDigits = false
		sb.WriteRune(r)
	}
	out := sb.String()
	if len(out) > 200 {
		out = out[:200] + "…"
	}
	return out
}

// ExportJSON writes the report as formatted JSON.
func (r *Report) ExportJSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// ExportToFile writes the report to a file.
func (r *Report) ExportToFile(path string) error {
	data, err := r.ExportJSON()
	if err != nil {
		return fmt.Errorf("diagnostics: marshal failed: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}

// ExportText generates a human-readable text report.
func (r *Report) ExportText() string {
	var sb strings.Builder
	line := strings.Repeat("=", 78)
	sub := func(title string) {
		sb.WriteString("\n-- " + title + " " + strings.Repeat("-", 74-len(title)) + "\n")
	}

	sb.WriteString(line + "\n  TorProxyManager Diagnostic Report\n" + line + "\n")
	fmt.Fprintf(&sb, "  Generated:   %s\n", r.GeneratedAt)
	fmt.Fprintf(&sb, "  Version:     %s (%s, %s/%s, %d CPUs)\n", r.AppVersion, r.GoVersion, r.OS, r.Arch, r.NumCPU)
	fmt.Fprintf(&sb, "  Manager mem: %.1f MB alloc, %.1f MB sys, %d goroutines\n",
		r.MemoryStats.AllocMB, r.MemoryStats.SysMB, r.MemoryStats.Goroutines)

	sub("Manager")
	m := r.ManagerStatus
	fmt.Fprintf(&sb, "  State:        %s (uptime %s)\n", m.State, (time.Duration(m.UptimeSeconds) * time.Second).String())
	fmt.Fprintf(&sb, "  Tor:          %s\n", m.TorPath)
	fmt.Fprintf(&sb, "  Tor version:  %s\n", m.TorVersion)
	fmt.Fprintf(&sb, "  SOCKS ports:  %s\n", m.PortRange)
	fmt.Fprintf(&sb, "  Control:      %s\n", m.ControlPortRange)
	fmt.Fprintf(&sb, "  Running:      %d / %d   Failed: %d\n", m.RunningInstances, m.TotalInstances, m.FailedInstances)

	sub("Summary")
	fmt.Fprintf(&sb, "  Tor memory total:  %.1f MB\n", r.Summary.TotalTorMemoryMB)
	fmt.Fprintf(&sb, "  Startup avg / max: %d ms / %d ms\n", r.Summary.AvgStartupMillis, r.Summary.MaxStartupMillis)
	fmt.Fprintf(&sb, "  Total restarts:    %d\n", r.Summary.TotalRestarts)
	if len(r.Summary.TopErrors) > 0 {
		sb.WriteString("  Top errors:\n")
		for _, e := range r.Summary.TopErrors {
			fmt.Fprintf(&sb, "    %4dx %s\n", e.Count, e.Error)
		}
	}

	sub("Instances")
	fmt.Fprintf(&sb, "  %-5s %-7s %-14s %-12s %-8s %-6s %-8s %s\n", "ID", "Port", "State", "Health", "PID", "Boot%", "RAM MB", "Last error")
	sb.WriteString("  " + strings.Repeat("-", 76) + "\n")
	for _, inst := range r.Instances {
		errMsg := inst.LastError
		if len(errMsg) > 40 {
			errMsg = errMsg[:40] + "…"
		}
		fmt.Fprintf(&sb, "  %-5d %-7d %-14s %-12s %-8d %-6d %-8.1f %s\n",
			inst.InstanceID, inst.SocksPort, inst.State, inst.Health, inst.PID, inst.BootstrapProgress, inst.MemoryMB, errMsg)
	}

	if len(r.HealthResults) > 0 {
		sub("Tor Verification")
		fmt.Fprintf(&sb, "  %-7s %-9s %-7s %-6s %-10s %s\n", "Port", "Listening", "SOCKS5", "Tor", "Duration", "Exit IP / Error")
		for _, hr := range r.HealthResults {
			detail := hr.ExitIP
			if hr.Error != "" {
				detail = hr.Error
			}
			fmt.Fprintf(&sb, "  %-7d %-9v %-7v %-6v %-10s %s\n", hr.Port, hr.IsListening, hr.SOCKSHandshake, hr.IsTor, hr.Duration, detail)
		}
	}

	if len(r.RecentLogs) > 0 {
		sub("Recent Log Entries")
		for _, l := range r.RecentLogs {
			sb.WriteString("  " + l + "\n")
		}
	}

	sb.WriteString("\n" + line + "\n")
	return sb.String()
}

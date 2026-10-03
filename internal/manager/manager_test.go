package manager

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"torproxymanager/internal/config"
	"torproxymanager/internal/logger"
	"torproxymanager/internal/testutil"
	"torproxymanager/internal/torinstance"
	"torproxymanager/internal/winsys"
)

func testSettings(t *testing.T, count int) (config.Settings, string) {
	t.Helper()
	appDir := t.TempDir()
	block := testutil.FreePortBlock(t, count*2+1)
	s := config.DefaultSettings()
	s.EndpointCount = count
	s.TorProcesses = count
	s.StartPort = block
	s.ControlPortStart = block + count
	s.WebUIPort = block + count*2
	s.TorExecutablePath = testutil.FakeTorPath(t)
	s.DataDirectory = filepath.Join(appDir, "data")
	s.StartupConcurrency = 8
	s.ConnectionTimeoutSec = 10
	s.BootstrapTimeoutSec = 30
	s.RetryLimit = 2
	s.RetryBackoffSec = 1
	s.RetryMaxBackoffSec = 2
	s.HealthCheckIntervalSec = 10
	return s, appDir
}

func newTestManager(t *testing.T, s config.Settings, appDir string) *Manager {
	t.Helper()
	if errs := s.Validate(); len(errs) > 0 {
		t.Fatalf("invalid test settings: %v", errs)
	}
	cfg := config.NewFromSettings(s, filepath.Join(appDir, "config.json"))
	m := New(cfg, logger.NewNop(), appDir)
	job, err := winsys.NewKillOnCloseJob()
	if err == nil {
		m.SetJob(job)
		t.Cleanup(func() { job.Close() })
	}
	t.Cleanup(func() { m.StopAll() })
	return m
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestManager_StartAllStopAll(t *testing.T) {
	const n = 20
	s, appDir := testSettings(t, n)
	m := newTestManager(t, s, appDir)

	events := m.Subscribe()
	defer m.Unsubscribe(events)

	start := time.Now()
	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	t.Logf("started %d instances in %v", n, time.Since(start))

	st := m.GetOverallStatus()
	if st.ManagerState != StateRunning || st.RunningInstances != n || st.FailedInstances != 0 {
		t.Fatalf("unexpected overall status: %+v", st)
	}
	if st.TorVersion == "" {
		t.Error("tor version should be detected from the seed instance")
	}
	if p, total := m.StartProgress(); p != n || total != n {
		t.Errorf("progress = %d/%d", p, total)
	}

	statuses := m.SortedStatuses()
	for i, inst := range statuses {
		if inst.SocksPort != s.StartPort+i || inst.ControlPort != s.ControlPortStart+i {
			t.Errorf("instance %d has wrong ports: %+v", i+1, inst)
		}
		if torinstance.SOCKS5Handshake("127.0.0.1", inst.SocksPort, time.Second) != torinstance.HealthHealthy {
			t.Errorf("port %d not answering SOCKS5", inst.SocksPort)
		}
	}

	if list := m.ProxyList(true); len(list) != n || list[0] != "127.0.0.1:"+strconv.Itoa(s.StartPort) {
		t.Errorf("unexpected proxy list: %v", list)
	}

	if ok, failed := m.NewIdentityAll(); ok != n || failed != 0 {
		t.Errorf("NewIdentityAll = %d ok / %d failed", ok, failed)
	}

	sawProgress, sawStarted := false, false
drain:
	for {
		select {
		case e := <-events:
			switch e.Type {
			case EventProgress:
				sawProgress = true
			case EventStarted:
				sawStarted = true
			}
		default:
			break drain
		}
	}
	if !sawProgress || !sawStarted {
		t.Errorf("expected progress and started events (progress=%v started=%v)", sawProgress, sawStarted)
	}

	if err := m.StartAll(); err == nil {
		t.Error("second StartAll should fail while running")
	}

	stopStart := time.Now()
	if err := m.StopAll(); err != nil {
		t.Fatal(err)
	}
	t.Logf("stopped in %v", time.Since(stopStart))
	if m.State() != StateStopped {
		t.Errorf("state = %s", m.State())
	}
	for _, inst := range m.SortedStatuses() {
		if inst.State != torinstance.StateStopped {
			t.Errorf("instance %d state %s after StopAll", inst.InstanceID, inst.State)
		}
		if torinstance.SOCKS5Handshake("127.0.0.1", inst.SocksPort, 300*time.Millisecond) == torinstance.HealthHealthy {
			t.Errorf("port %d still open after StopAll", inst.SocksPort)
		}
	}

	// The manager must be restartable.
	if err := m.StartAll(); err != nil {
		t.Fatalf("restart StartAll: %v", err)
	}
	if got := m.GetOverallStatus().RunningInstances; got != n {
		t.Errorf("running after second start = %d", got)
	}
}

func TestManager_CrashRecoveryAndRestartFailed(t *testing.T) {
	s, appDir := testSettings(t, 4)
	s.AutoRestart = false
	m := newTestManager(t, s, appDir)
	if err := m.StartAll(); err != nil {
		t.Fatal(err)
	}

	st, _ := m.GetInstanceStatus(3)
	os.WriteFile(filepath.Join(s.DataDirectory, "instance_3", "crash_now"), nil, 0o600)
	waitFor(t, 5*time.Second, func() bool {
		st, _ := m.GetInstanceStatus(3)
		return st.State == torinstance.StateFailed
	}, "instance 3 to fail")
	if got := m.GetOverallStatus().FailedInstances; got != 1 {
		t.Errorf("failed instances = %d, want 1", got)
	}

	n, err := m.RestartFailed()
	if err != nil || n != 1 {
		t.Fatalf("RestartFailed = %d, %v", n, err)
	}
	waitFor(t, 10*time.Second, func() bool {
		cur, _ := m.GetInstanceStatus(3)
		return cur.State == torinstance.StateRunning && cur.PID != st.PID
	}, "instance 3 to be restarted")
}

func TestManager_PerInstanceControls(t *testing.T) {
	s, appDir := testSettings(t, 3)
	m := newTestManager(t, s, appDir)
	if err := m.StartAll(); err != nil {
		t.Fatal(err)
	}
	if err := m.StopInstance(2); err != nil {
		t.Fatal(err)
	}
	if st, _ := m.GetInstanceStatus(2); st.State != torinstance.StateStopped {
		t.Errorf("instance 2 state %s", st.State)
	}
	if err := m.StartInstance(2); err != nil {
		t.Fatalf("StartInstance: %v", err)
	}
	if err := m.RestartInstance(1); err != nil {
		t.Fatalf("RestartInstance: %v", err)
	}
	if err := m.NewIdentity(3); err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	if err := m.RestartInstance(99); err == nil {
		t.Error("expected error for unknown instance")
	}
	if got := m.GetOverallStatus().RunningInstances; got != 3 {
		t.Errorf("running = %d", got)
	}
	if logText, err := m.InstanceLog(1, 4096); err != nil || logText == "" {
		t.Errorf("InstanceLog: %q %v", logText, err)
	}
}

func TestManager_StopDuringStartup(t *testing.T) {
	t.Setenv("FAKETOR_BOOTSTRAP_MS", "3000")
	s, appDir := testSettings(t, 6)
	s.StartupConcurrency = 2
	m := newTestManager(t, s, appDir)

	errCh := make(chan error, 1)
	go func() { errCh <- m.StartAll() }()
	waitFor(t, 5*time.Second, func() bool { return m.GetOverallStatus().BootstrappingInstances > 0 }, "bootstrapping")

	if err := m.StopAll(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Error("StartAll should report cancellation")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("StartAll did not return after StopAll")
	}
	if m.State() != StateStopped {
		t.Errorf("state = %s", m.State())
	}
	for _, st := range m.SortedStatuses() {
		if torinstance.SOCKS5Handshake("127.0.0.1", st.SocksPort, 300*time.Millisecond) == torinstance.HealthHealthy {
			t.Errorf("orphan listening on %d", st.SocksPort)
		}
	}
}

func TestManager_PortConflictDetected(t *testing.T) {
	s, appDir := testSettings(t, 3)
	m := newTestManager(t, s, appDir)

	// Occupy one SOCKS port with a foreign listener.
	other := config.DefaultSettings()
	other.EndpointCount = 1
	other.StartPort = s.StartPort + 1
	if conflicts := FindPortConflicts(other); len(conflicts) != 0 {
		t.Fatalf("port should be free initially: %v", conflicts)
	}
	ln := listen(t, s.StartPort+1)
	defer ln.Close()

	err := m.StartAll()
	if err == nil {
		t.Fatal("expected port conflict error")
	}
	if m.State() != StateStopped {
		t.Errorf("state = %s after failed start", m.State())
	}
	if m.GetOverallStatus().LastError == "" {
		t.Error("LastError should describe the conflict")
	}
}

func TestManager_MissingTor(t *testing.T) {
	s, appDir := testSettings(t, 2)
	s.TorExecutablePath = ""
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("ProgramFiles", t.TempDir())
	t.Setenv("ProgramFiles(x86)", t.TempDir())
	m := newTestManager(t, s, appDir)
	if err := m.StartAll(); err == nil {
		t.Fatal("expected tor-not-found error")
	}
}

func TestManager_StaleProcessCleanup(t *testing.T) {
	s, appDir := testSettings(t, 2)
	m := newTestManager(t, s, appDir)

	// Simulate a tor.exe left over from a crashed session holding port #1.
	dir := filepath.Join(s.DataDirectory, "instance_1")
	os.MkdirAll(dir, 0o700)
	torrc := filepath.Join(dir, "torrc")
	os.WriteFile(torrc, []byte("SocksPort 127.0.0.1:"+strconv.Itoa(s.StartPort)+"\nControlPort 127.0.0.1:"+
		strconv.Itoa(s.ControlPortStart)+"\nDataDirectory "+filepath.ToSlash(dir)+"\n"), 0o600)
	cmd := exec.Command(s.TorExecutablePath, "-f", torrc)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	os.WriteFile(torinstance.PIDFilePath(dir), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	waitFor(t, 5*time.Second, func() bool {
		return torinstance.SOCKS5Handshake("127.0.0.1", s.StartPort, 200*time.Millisecond) == torinstance.HealthHealthy
	}, "stale process to listen")

	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll should clean up the stale process: %v", err)
	}
	if got := m.GetOverallStatus().RunningInstances; got != 2 {
		t.Errorf("running = %d", got)
	}
}

func TestSeedDirectoryCache(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "new")
	os.WriteFile(filepath.Join(src, "cached-certs"), []byte("certs"), 0o600)
	os.WriteFile(filepath.Join(src, "cached-microdesc-consensus"), []byte("consensus"), 0o600)
	os.WriteFile(filepath.Join(src, "state"), []byte("private"), 0o600)

	if n := seedDirectoryCache(src, dst); n != 2 {
		t.Fatalf("copied %d files, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(dst, "state")); !os.IsNotExist(err) {
		t.Error("per-instance state file must never be shared")
	}
	if n := seedDirectoryCache(src, dst); n != 0 {
		t.Errorf("second seed copied %d files, want 0 (already up to date)", n)
	}
}

func TestManager_MultiPortBlocks(t *testing.T) {
	s, appDir := testSettings(t, 6)
	s.TorProcesses = 2
	m := newTestManager(t, s, appDir)
	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	defer m.StopAll()
	st := m.GetOverallStatus()
	if st.TotalInstances != 2 || st.TotalPorts != 6 || st.RunningPorts != 6 {
		t.Fatalf("unexpected status: %+v", st)
	}
	list := m.ProxyList(true)
	if len(list) != 6 {
		t.Fatalf("proxy list = %v", list)
	}
	for p := s.StartPort; p < s.StartPort+6; p++ {
		if torinstance.SOCKS5Handshake("127.0.0.1", p, time.Second) != torinstance.HealthHealthy {
			t.Errorf("port %d not answering", p)
		}
	}
}

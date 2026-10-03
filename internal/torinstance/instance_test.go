package torinstance

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torproxymanager/internal/logger"
	"torproxymanager/internal/testutil"
)

func TestParseBootstrapPhase(t *testing.T) {
	bs := ParseBootstrapPhase(`NOTICE BOOTSTRAP PROGRESS=85 TAG=ap_conn_done SUMMARY="Connected to a relay to build circuits"`)
	if bs.Progress != 85 || bs.Tag != "ap_conn_done" || bs.Summary != "Connected to a relay to build circuits" {
		t.Errorf("unexpected parse result: %+v", bs)
	}
	bs = ParseBootstrapPhase(`WARN BOOTSTRAP PROGRESS=10 TAG=conn_done SUMMARY="Connected" WARNING="Connection refused \"x\""`)
	if bs.Progress != 10 || bs.Warning != `Connection refused "x"` {
		t.Errorf("unexpected parse result: %+v", bs)
	}
}

func TestBackoff(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 5 * time.Second},
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{10, 60 * time.Second}, // capped
	}
	for _, c := range cases {
		if got := Backoff(5*time.Second, 2, time.Minute, c.attempt); got != c.want {
			t.Errorf("Backoff(attempt=%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

func TestQuoteTorPath(t *testing.T) {
	if got := quoteTorPath("C:/Program Files/x"); got != `"C:/Program Files/x"` {
		t.Errorf("got %s", got)
	}
	if got := quoteTorPath("C:/data/x"); got != "C:/data/x" {
		t.Errorf("got %s", got)
	}
}

func TestGenerateTorrc(t *testing.T) {
	dir := t.TempDir()
	inst, err := New(Options{ID: 3, SocksPort: 9052, ControlPort: 30052, TorPath: "tor.exe", DataDir: dir}, logger.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.generateTorrc(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(inst.TorrcPath())
	s := string(b)
	for _, want := range []string{
		"SocksPort 127.0.0.1:9052",
		"ControlPort 127.0.0.1:30052",
		"CookieAuthentication 1",
		"ClientOnly 1",
		"DataDirectory " + filepath.ToSlash(dir),
	} {
		if !strings.Contains(s, want) {
			t.Errorf("torrc missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, `\`) {
		t.Errorf("torrc should only contain forward slashes:\n%s", s)
	}
}

func TestCustomTemplateMissingKeyFails(t *testing.T) {
	_, err := New(Options{ID: 1, TorrcTemplate: "SocksPort {{.Nope"}, logger.NewNop())
	if err == nil {
		t.Fatal("expected template parse error")
	}
	inst, err := New(Options{ID: 1, DataDir: t.TempDir(), TorrcTemplate: "SocksPort {{.Missing}}"}, logger.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.generateTorrc(); err == nil {
		t.Fatal("expected execution error for unknown template field")
	}
}

// fakeControlServer runs a scripted control-port server for protocol tests.
func fakeControlServer(t *testing.T, handler func(line string) []string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					for _, out := range handler(strings.TrimRight(l, "\r\n")) {
						c.Write([]byte(out + "\r\n"))
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

func TestControlConnProtocol(t *testing.T) {
	addr := fakeControlServer(t, func(line string) []string {
		switch {
		case line == "AUTHENTICATE":
			return []string{"250 OK"}
		case line == "GETINFO config-text":
			return []string{"250+config-text=", "SocksPort 1", "..dotted", ".", "250 OK"}
		case line == "GETINFO status/bootstrap-phase":
			return []string{`250-status/bootstrap-phase=NOTICE BOOTSTRAP PROGRESS=100 TAG=done SUMMARY="Done"`, "250 OK"}
		case line == "SIGNAL BOGUS":
			return []string{"552 Unrecognized signal code"}
		case strings.HasPrefix(line, "QUIT"):
			return []string{"250 closing connection"}
		}
		return []string{"510 Unrecognized command"}
	})

	c, err := DialControl(addr, "", 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	v, err := c.GetInfo("config-text")
	if err != nil || v != "SocksPort 1\n.dotted" {
		t.Errorf("data reply parse failed: %q %v", v, err)
	}
	bs, err := c.GetBootstrap()
	if err != nil || bs.Progress != 100 {
		t.Errorf("bootstrap: %+v %v", bs, err)
	}
	err = c.Signal("BOGUS")
	if ce, ok := err.(*ControlError); !ok || ce.Code != 552 {
		t.Errorf("expected ControlError 552, got %v", err)
	}
	if _, err := c.Command("GETINFO a\r\nSIGNAL HALT"); err == nil {
		t.Error("expected rejection of multi-line command (injection)")
	}
}

func TestControlConnBadCookie(t *testing.T) {
	addr := fakeControlServer(t, func(line string) []string { return []string{"515 Authentication failed"} })
	cookie := filepath.Join(t.TempDir(), "cookie")
	os.WriteFile(cookie, make([]byte, 32), 0o600)
	if _, err := DialControl(addr, cookie, time.Second); err == nil {
		t.Fatal("expected auth failure")
	}
	os.WriteFile(cookie, []byte("short"), 0o600)
	if _, err := DialControl(addr, cookie, time.Second); err == nil {
		t.Fatal("expected cookie length failure")
	}
}

// --- Lifecycle tests against faketor ---

func newTestInstance(t *testing.T, mutate func(o *Options)) *Instance {
	t.Helper()
	port := testutil.FreePortBlock(t, 2)
	opts := Options{
		ID:               1,
		SocksPort:        port,
		ControlPort:      port + 1,
		TorPath:          testutil.FakeTorPath(t),
		DataDir:          filepath.Join(t.TempDir(), "instance_1"),
		ConnTimeout:      10 * time.Second,
		BootstrapTimeout: 10 * time.Second,
		RetryLimit:       3,
		RetryBackoff:     100 * time.Millisecond,
		BackoffMult:      1,
		MaxBackoff:       time.Second,
	}
	if mutate != nil {
		mutate(&opts)
	}
	inst, err := New(opts, logger.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inst.Stop() })
	return inst
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

func TestLifecycle_StartHealthNewnymStop(t *testing.T) {
	var changes atomic.Int32
	inst := newTestInstance(t, func(o *Options) { o.OnChange = func(Status) { changes.Add(1) } })

	if err := inst.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	st := inst.Status()
	if st.State != StateRunning || st.Health != HealthHealthy || st.BootstrapProgress != 100 || st.PID == 0 {
		t.Fatalf("unexpected status after start: %+v", st)
	}
	if _, err := os.Stat(PIDFilePath(inst.DataDir())); err != nil {
		t.Errorf("pid file not written: %v", err)
	}
	if h := inst.CheckHealth(); h != HealthHealthy {
		t.Errorf("health = %s", h)
	}
	if err := inst.NewIdentity(); err != nil {
		t.Errorf("newnym: %v", err)
	}
	if inst.Status().LastNewIdentity == nil {
		t.Error("LastNewIdentity not recorded")
	}
	if err := inst.Start(context.Background()); err == nil {
		t.Error("second Start on running instance should fail")
	}

	start := time.Now()
	if err := inst.Stop(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("graceful stop took too long: %v", time.Since(start))
	}
	if st := inst.Status(); st.State != StateStopped || st.PID != 0 {
		t.Errorf("unexpected status after stop: %+v", st)
	}
	if SOCKS5Handshake("127.0.0.1", inst.SocksPort(), time.Second) == HealthHealthy {
		t.Error("SOCKS port still answering after stop")
	}
	if changes.Load() < 4 {
		t.Errorf("expected several OnChange notifications, got %d", changes.Load())
	}
}

func TestLifecycle_ForceKillWhenShutdownIgnored(t *testing.T) {
	t.Setenv("FAKETOR_IGNORE_SHUTDOWN", "1")
	inst := newTestInstance(t, nil)
	if err := inst.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	inst.Stop()
	if inst.State() != StateStopped {
		t.Fatal("instance should be stopped after forced kill")
	}
	if SOCKS5Handshake("127.0.0.1", inst.SocksPort(), time.Second) == HealthHealthy {
		t.Error("process survived forced kill")
	}
}

func TestLifecycle_StartFailureCapturesTorLog(t *testing.T) {
	t.Setenv("FAKETOR_FAIL", "1")
	inst := newTestInstance(t, nil)
	err := inst.Start(context.Background())
	if err == nil {
		t.Fatal("expected start failure")
	}
	st := inst.Status()
	if st.State != StateFailed {
		t.Errorf("state = %s, want failed", st.State)
	}
	if !strings.Contains(st.LastError, "Simulated fatal startup error") {
		t.Errorf("last error should include Tor log excerpt, got %q", st.LastError)
	}
}

func TestLifecycle_PortInUse(t *testing.T) {
	inst := newTestInstance(t, nil)
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(inst.SocksPort())))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := inst.Start(context.Background()); err == nil {
		t.Fatal("expected failure when SOCKS port is taken")
	}
	if !strings.Contains(inst.Status().LastError, "Could not bind") {
		t.Errorf("expected bind error excerpt, got %q", inst.Status().LastError)
	}
}

func TestLifecycle_BootstrapTimeout(t *testing.T) {
	t.Setenv("FAKETOR_BOOTSTRAP_MS", "60000")
	inst := newTestInstance(t, func(o *Options) { o.BootstrapTimeout = 1 * time.Second })
	err := inst.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bootstrap timed out") {
		t.Fatalf("expected bootstrap timeout, got %v", err)
	}
	if SOCKS5Handshake("127.0.0.1", inst.SocksPort(), time.Second) == HealthHealthy {
		t.Error("process should be killed after bootstrap timeout")
	}
}

func TestLifecycle_CrashAutoRestart(t *testing.T) {
	inst := newTestInstance(t, func(o *Options) { o.AutoRestart = true })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := inst.Start(ctx); err != nil {
		t.Fatal(err)
	}
	firstPID := inst.Status().PID

	os.WriteFile(filepath.Join(inst.DataDir(), "crash_now"), nil, 0o600)
	waitFor(t, 5*time.Second, func() bool { return inst.State() == StateFailed || inst.Status().PID != firstPID }, "crash detection")
	waitFor(t, 10*time.Second, func() bool {
		st := inst.Status()
		return st.State == StateRunning && st.PID != firstPID
	}, "auto restart")

	st := inst.Status()
	if st.TotalRestarts != 1 || st.RetryCount != 1 {
		t.Errorf("expected 1 restart / retry, got restarts=%d retry=%d", st.TotalRestarts, st.RetryCount)
	}
}

func TestLifecycle_NoAutoRestartWhenDisabled(t *testing.T) {
	inst := newTestInstance(t, func(o *Options) { o.AutoRestart = false })
	if err := inst.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(inst.DataDir(), "crash_now"), nil, 0o600)
	waitFor(t, 5*time.Second, func() bool { return inst.State() == StateFailed }, "failed state")
	time.Sleep(500 * time.Millisecond)
	if inst.State() != StateFailed {
		t.Errorf("instance restarted although auto-restart is disabled: %s", inst.State())
	}
	if !strings.Contains(inst.Status().LastError, "exited unexpectedly") {
		t.Errorf("unexpected last error %q", inst.Status().LastError)
	}
}

func TestLifecycle_StopDuringBootstrapAborts(t *testing.T) {
	t.Setenv("FAKETOR_BOOTSTRAP_MS", "60000")
	inst := newTestInstance(t, nil)
	errCh := make(chan error, 1)
	go func() { errCh <- inst.Start(context.Background()) }()
	waitFor(t, 5*time.Second, func() bool { return inst.State() == StateBootstrapping }, "bootstrapping state")

	inst.Stop()
	select {
	case err := <-errCh:
		if err == nil {
			t.Error("Start should report abort")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
	if inst.State() != StateStopped {
		t.Errorf("state = %s, want stopped", inst.State())
	}
	if SOCKS5Handshake("127.0.0.1", inst.SocksPort(), time.Second) == HealthHealthy {
		t.Error("orphaned process still listening")
	}
}

func TestLifecycle_Restart(t *testing.T) {
	inst := newTestInstance(t, nil)
	if err := inst.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	pid := inst.Status().PID
	if err := inst.Restart(context.Background()); err != nil {
		t.Fatalf("restart: %v", err)
	}
	st := inst.Status()
	if st.State != StateRunning || st.PID == pid {
		t.Errorf("unexpected status after restart: %+v", st)
	}
}

func TestStartWithRetryGivesUp(t *testing.T) {
	t.Setenv("FAKETOR_FAIL", "1")
	inst := newTestInstance(t, func(o *Options) { o.RetryLimit = 2 })
	start := time.Now()
	if err := inst.StartWithRetry(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	if inst.Status().RetryCount != 2 {
		t.Errorf("retry count = %d, want 2", inst.Status().RetryCount)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Error("expected backoff delays between attempts")
	}
}

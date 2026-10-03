// Package testutil provides helpers shared by integration tests.
package testutil

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	builtPath string
	buildErr  error
	buildDir  string
)

// FakeTorPath compiles internal/testutil/faketor once per test binary and
// returns the path of the resulting executable.
func FakeTorPath(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "faketor-*")
		if err != nil {
			buildErr = err
			return
		}
		buildDir = dir
		name := "tor"
		if runtime.GOOS == "windows" {
			name = "tor.exe"
		}
		out := filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-o", out, "torproxymanager/internal/testutil/faketor")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			buildErr = &buildError{err: err, out: string(b)}
			return
		}
		builtPath = out
	})
	if buildErr != nil {
		t.Fatalf("building faketor: %v", buildErr)
	}
	return builtPath
}

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string { return e.err.Error() + ": " + e.out }

// FreePortBlock finds a starting port such that n consecutive ports are free.
func FreePortBlock(t testing.TB, n int) int {
	t.Helper()
	for attempt := 0; attempt < 50; attempt++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		start := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		if start+n > 65000 {
			continue
		}
		ok := true
		var held []net.Listener
		for p := start; p < start+n; p++ {
			l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(p)))
			if err != nil {
				ok = false
				break
			}
			held = append(held, l)
		}
		for _, l := range held {
			l.Close()
		}
		if ok {
			return start
		}
	}
	t.Fatalf("could not find %d consecutive free ports", n)
	return 0
}

func itoa(i int) string {
	var b [8]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

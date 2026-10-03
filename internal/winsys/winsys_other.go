//go:build !windows

// Package winsys provides portable no-op / best-effort fallbacks so that the
// application and its tests can be compiled and run on non-Windows hosts.
package winsys

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Job is a no-op placeholder on non-Windows platforms.
type Job struct{}

// NewKillOnCloseJob returns a no-op job.
func NewKillOnCloseJob() (*Job, error) { return &Job{}, nil }

// Assign is a no-op.
func (j *Job) Assign(pid int) error { return nil }

// Close is a no-op.
func (j *Job) Close() error { return nil }

// AcquireSingleInstance always succeeds on non-Windows platforms.
func AcquireSingleInstance(name string) (func(), bool, error) { return func() {}, false, nil }

// SetAutoStart is unsupported outside Windows.
func SetAutoStart(appName, exePath, args string, enable bool) error {
	if enable {
		return errors.New("start with Windows is only supported on Windows")
	}
	return nil
}

// IsAutoStartEnabled always returns false.
func IsAutoStartEnabled(appName string) bool { return false }

// ProcessImageName is unsupported outside Windows.
func ProcessImageName(pid int) (string, error) { return "", errors.New("unsupported") }

// KillStaleTor sends SIGKILL if the process exists (best effort).
func KillStaleTor(pid int) (bool, error) {
	if pid <= 0 || pid == os.Getpid() {
		return false, nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false, nil
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return false, nil
	}
	return false, nil // don't kill unknown processes on non-Windows
}

// ProcessStats is unsupported outside Windows.
func ProcessStats(pid int) (uint64, time.Duration, error) { return 0, 0, errors.New("unsupported") }

// AttachParentConsole is a no-op (a console is always available).
func AttachParentConsole() bool { return true }

// HasConsole always returns true.
func HasConsole() bool { return true }

// MessageBox prints to stderr.
func MessageBox(title, text string, isError bool) { os.Stderr.WriteString(title + ": " + text + "\n") }

// OpenURL opens a URL with xdg-open / open.
func OpenURL(url string) error {
	for _, c := range []string{"xdg-open", "open"} {
		if p, err := exec.LookPath(c); err == nil {
			return exec.Command(p, url).Start()
		}
	}
	return errors.New("no URL opener found")
}

// SetClipboardText is unsupported outside Windows.
func SetClipboardText(text string) error { return errors.New("clipboard unsupported on this platform") }

//go:build windows

// Package winsys wraps the Windows APIs used by TorProxyManager:
//
//   - Job Objects, so every tor.exe child is killed automatically if the
//     manager exits or crashes (no orphaned processes holding ports).
//   - A named mutex for single-instance enforcement.
//   - HKCU\...\Run registration for "Start with Windows".
//   - Per-process memory / CPU sampling.
//   - Console attachment for CLI usage of a GUI-subsystem binary.
package winsys

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Job is a Windows Job Object configured with KILL_ON_JOB_CLOSE.
type Job struct {
	handle windows.Handle
}

// NewKillOnCloseJob creates a job object whose processes are terminated when
// the last handle to it is closed — including when this process dies.
func NewKillOnCloseJob() (*Job, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		h,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return &Job{handle: h}, nil
}

// Assign adds the process with the given PID to the job.
func (j *Job) Assign(pid int) error {
	if j == nil || j.handle == 0 {
		return errors.New("job object not initialised")
	}
	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer windows.CloseHandle(ph)
	if err := windows.AssignProcessToJobObject(j.handle, ph); err != nil {
		return fmt.Errorf("AssignProcessToJobObject(%d): %w", pid, err)
	}
	return nil
}

// Close closes the job handle, terminating all processes still in the job.
func (j *Job) Close() error {
	if j == nil || j.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(j.handle)
	j.handle = 0
	return err
}

// AcquireSingleInstance creates a named mutex. If another process already
// holds it, alreadyRunning is true. Call release when exiting.
func AcquireSingleInstance(name string) (release func(), alreadyRunning bool, err error) {
	namePtr, err := windows.UTF16PtrFromString(`Local\` + name)
	if err != nil {
		return func() {}, false, err
	}
	h, err := windows.CreateMutex(nil, false, namePtr)
	if err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			if h != 0 {
				windows.CloseHandle(h)
			}
			return func() {}, true, nil
		}
		return func() {}, false, fmt.Errorf("CreateMutex: %w", err)
	}
	return func() { windows.CloseHandle(h) }, false, nil
}

const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// SetAutoStart registers or removes the application under HKCU\...\Run.
func SetAutoStart(appName, exePath string, args string, enable bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return fmt.Errorf("open Run key: %w", err)
	}
	defer k.Close()

	if !enable {
		if err := k.DeleteValue(appName); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return fmt.Errorf("delete Run value: %w", err)
		}
		return nil
	}
	cmd := `"` + exePath + `"`
	if args != "" {
		cmd += " " + args
	}
	return k.SetStringValue(appName, cmd)
}

// IsAutoStartEnabled reports whether a Run entry exists for appName.
func IsAutoStartEnabled(appName string) bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(appName)
	return err == nil
}

// ProcessImageName returns the full executable path of a running process.
func ProcessImageName(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)

	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:size]), nil
}

// KillStaleTor terminates pid only if it is a running tor executable.
// Returns true if a process was terminated.
func KillStaleTor(pid int) (bool, error) {
	if pid <= 0 || pid == os.Getpid() {
		return false, nil
	}
	name, err := ProcessImageName(pid)
	if err != nil {
		return false, nil // not running (or not accessible) — nothing to do
	}
	if !strings.EqualFold(filepath.Base(name), "tor.exe") {
		return false, nil // PID reused by an unrelated process — never touch it
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		return false, err
	}
	_, _ = windows.WaitForSingleObject(h, 5000)
	return true, nil
}

// ProcessStats returns the working-set size in bytes and total CPU time used by pid.
func ProcessStats(pid int) (workingSet uint64, cpu time.Duration, err error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, 0, err
	}
	defer windows.CloseHandle(h)

	var mem processMemoryCounters
	mem.CB = uint32(unsafe.Sizeof(mem))
	if err := getProcessMemoryInfo(h, &mem); err != nil {
		return 0, 0, err
	}

	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return uint64(mem.WorkingSetSize), 0, err
	}
	total := filetimeTicks(kernel) + filetimeTicks(user) // 100ns units
	return uint64(mem.WorkingSetSize), time.Duration(total * 100), nil
}

func filetimeTicks(ft windows.Filetime) int64 {
	return int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)
}

var (
	modpsapi                 = windows.NewLazySystemDLL("psapi.dll")
	procGetProcessMemoryInfo = modpsapi.NewProc("GetProcessMemoryInfo")
	modkernel32              = windows.NewLazySystemDLL("kernel32.dll")
	procAttachConsole        = modkernel32.NewProc("AttachConsole")
	moduser32                = windows.NewLazySystemDLL("user32.dll")
	procMessageBoxW          = moduser32.NewProc("MessageBoxW")
	procOpenClipboard        = moduser32.NewProc("OpenClipboard")
	procCloseClipboard       = moduser32.NewProc("CloseClipboard")
	procEmptyClipboard       = moduser32.NewProc("EmptyClipboard")
	procSetClipboardData     = moduser32.NewProc("SetClipboardData")
	procGlobalAlloc          = modkernel32.NewProc("GlobalAlloc")
	procGlobalFree           = modkernel32.NewProc("GlobalFree")
	procGlobalLock           = modkernel32.NewProc("GlobalLock")
	procGlobalUnlock         = modkernel32.NewProc("GlobalUnlock")
	procRtlMoveMemory        = modkernel32.NewProc("RtlMoveMemory")
)

// processMemoryCounters mirrors PROCESS_MEMORY_COUNTERS from psapi.h.
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

func getProcessMemoryInfo(h windows.Handle, mem *processMemoryCounters) error {
	r, _, e := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(mem)), uintptr(mem.CB))
	if r == 0 {
		return e
	}
	return nil
}

// AttachParentConsole attaches to the console of the parent process (e.g. cmd.exe
// or PowerShell) so that a GUI-subsystem binary can print CLI output.
// Returns true when a console was attached and os.Stdout/os.Stderr were rebound.
func AttachParentConsole() bool {
	const attachParentProcess = ^uintptr(0) // (DWORD)-1
	r, _, _ := procAttachConsole.Call(attachParentProcess)
	if r == 0 {
		return false
	}
	if out, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout = out
		os.Stderr = out
	}
	return true
}

// HasConsole reports whether stdout refers to a valid handle.
func HasConsole() bool {
	h, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	return err == nil && h != 0 && h != windows.InvalidHandle
}

// MessageBox shows a modal message box (used for fatal errors in GUI mode).
func MessageBox(title, text string, isError bool) {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(text)
	flags := uintptr(0x00000040) // MB_ICONINFORMATION
	if isError {
		flags = 0x00000010 // MB_ICONERROR
	}
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), flags|0x00010000 /*MB_SETFOREGROUND*/)
}

// OpenURL opens a URL in the user's default browser.
func OpenURL(url string) error {
	verb, _ := windows.UTF16PtrFromString("open")
	target, err := windows.UTF16PtrFromString(url)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, verb, target, nil, nil, windows.SW_SHOWNORMAL)
}

// SetClipboardText places Unicode text on the Windows clipboard.
func SetClipboardText(text string) error {
	const (
		cfUnicodeText = 13
		gmemMoveable  = 0x0002
	)
	u, err := windows.UTF16FromString(text)
	if err != nil {
		return err
	}
	// The clipboard is owned by the calling thread between Open/Close.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var opened bool
	for i := 0; i < 10; i++ { // another app may briefly hold the clipboard
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			opened = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !opened {
		return errors.New("clipboard is in use by another application")
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()

	size := uintptr(len(u) * 2)
	h, _, e := procGlobalAlloc.Call(gmemMoveable, size)
	if h == 0 {
		return fmt.Errorf("GlobalAlloc: %v", e)
	}
	p, _, e := procGlobalLock.Call(h)
	if p == 0 {
		procGlobalFree.Call(h)
		return fmt.Errorf("GlobalLock: %v", e)
	}
	procRtlMoveMemory.Call(p, uintptr(unsafe.Pointer(&u[0])), size)
	procGlobalUnlock.Call(h)

	if r, _, e := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		procGlobalFree.Call(h)
		return fmt.Errorf("SetClipboardData: %v", e)
	}
	return nil // the system now owns h
}

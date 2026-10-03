//go:build windows

package torinstance

import (
	"os/exec"
	"syscall"
)

// hideConsoleWindow prevents tor.exe from opening a console window.
func hideConsoleWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}

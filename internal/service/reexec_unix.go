//go:build unix

package service

import (
	"os"
	"syscall"
)

// ReexecInstalled replaces the current process image with the installed
// executable, keeping the PID (systemd sees a continuous Type=simple service;
// the desktop's detached child stays the same process). Go opens every
// descriptor close-on-exec, so the IPC listener and state files are released
// and the new image opens them afresh. Only returns on failure.
func ReexecInstalled(path string) error {
	args := append([]string{path}, os.Args[1:]...)
	return syscall.Exec(path, args, os.Environ())
}

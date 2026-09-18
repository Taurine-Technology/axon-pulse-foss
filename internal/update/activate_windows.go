//go:build windows

package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

// Activate starts the verified new binary as a detached swap helper. Windows
// locks running executables, so the helper waits for this service to exit,
// performs the A/B replacement, health-checks it, and restarts the service.
func Activate(_ context.Context, staged, executable string) error {
	if !filepath.IsAbs(staged) || !filepath.IsAbs(executable) {
		return errors.New("update paths must be absolute on Windows")
	}
	command := exec.Command(staged, "apply-update", strconv.Itoa(os.Getpid()), executable)
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start Windows update helper: %w", err)
	}
	return nil
}

// ApplyPending performs the hidden Windows update-helper command.
func ApplyPending(args []string) error {
	if len(args) != 2 {
		return errors.New("invalid Windows update helper arguments")
	}
	pid, err := strconv.ParseUint(args[0], 10, 32)
	if err != nil || pid == 0 {
		return errors.New("invalid Windows update parent PID")
	}
	active := filepath.Clean(args[1])
	if !filepath.IsAbs(active) {
		return errors.New("update target must be absolute on Windows")
	}
	if err := waitForProcess(uint32(pid), 2*time.Minute); err != nil {
		return err
	}
	staged, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate staged updater: %w", err)
	}
	newPath := active + ".pulse-new"
	previousPath := active + ".pulse-previous"
	if err := copyWindowsExecutable(staged, newPath); err != nil {
		return err
	}
	_ = os.Remove(previousPath)
	if err := os.Rename(active, previousPath); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("archive active executable: %w", err)
	}
	if err := os.Rename(newPath, active); err != nil {
		_ = os.Rename(previousPath, active)
		return fmt.Errorf("activate staged executable: %w", err)
	}
	healthCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if output, healthErr := exec.CommandContext(healthCtx, active, "version").CombinedOutput(); healthErr != nil {
		failed := active + ".pulse-failed"
		_ = os.Remove(failed)
		moveFailed := os.Rename(active, failed)
		rollback := os.Rename(previousPath, active)
		return fmt.Errorf("new executable health check failed (%s): %w; rollback: %w", output, healthErr, errors.Join(moveFailed, rollback))
	}
	restartWindowsService(active)
	return nil
}

func waitForProcess(pid uint32, timeout time.Duration) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return fmt.Errorf("open update parent process: %w", err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	result, err := windows.WaitForSingleObject(handle, uint32(timeout/time.Millisecond))
	if err != nil {
		return fmt.Errorf("wait for update parent: %w", err)
	}
	if result == uint32(windows.WAIT_TIMEOUT) {
		return errors.New("timed out waiting for the old service to stop")
	}
	return nil
}

func copyWindowsExecutable(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }() // read-only; close error is not meaningful
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	syncErr, closeErr := output.Sync(), output.Close()
	return errors.Join(copyErr, syncErr, closeErr)
}

func restartWindowsService(active string) {
	for range 10 {
		if exec.Command("sc.exe", "start", "AxonPulse").Run() == nil {
			return
		}
		time.Sleep(time.Second)
	}
	command := exec.Command(active, "run")
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
	_ = command.Start()
}

//go:build windows

package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/mod/semver"
	"golang.org/x/sys/windows"
)

// ActivateDesktop copies the current signed desktop binary to a detached
// helper. The helper survives installer replacement, probes the new local IPC
// service, and restores the retained executable if the probe never succeeds.
func ActivateDesktop(ctx context.Context, staged, executable, expectedVersion string) error {
	name := strings.ToLower(filepath.Base(staged))
	if filepath.Ext(name) != ".exe" || !strings.Contains(name, "setup") {
		return errors.New("desktop update is not a signed Windows setup executable")
	}
	if err := verifyWindowsPublisher(ctx, staged, executable); err != nil {
		return err
	}
	helper := filepath.Join(filepath.Dir(staged), "pulse-desktop-update-helper.exe")
	if err := copyWindowsExecutable(executable, helper); err != nil {
		return fmt.Errorf("stage Windows update helper: %w", err)
	}
	command := exec.Command(
		helper,
		"--desktop-update-helper",
		strconv.Itoa(os.Getpid()),
		staged,
		executable,
		expectedVersion,
	)
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start Windows desktop update helper: %w", err)
	}
	return command.Process.Release()
}

func ApplyDesktopPending(args []string) error {
	if len(args) != 4 {
		return errors.New("invalid Windows desktop update helper arguments")
	}
	pid, err := strconv.ParseUint(args[0], 10, 32)
	if err != nil || pid == 0 {
		return errors.New("invalid Windows desktop update parent PID")
	}
	staged, active, expectedVersion := filepath.Clean(args[1]), filepath.Clean(args[2]), args[3]
	if !filepath.IsAbs(staged) || !filepath.IsAbs(active) {
		return errors.New("windows desktop update paths must be absolute")
	}
	if !semver.IsValid(canonicalVersion(expectedVersion)) {
		return errors.New("windows desktop update expected version is invalid")
	}
	if err := waitForProcess(uint32(pid), 2*time.Minute); err != nil {
		return err
	}
	helper, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate Windows desktop update helper: %w", err)
	}
	verifyCtx, cancelVerify := context.WithTimeout(context.Background(), 30*time.Second)
	verifyErr := verifyWindowsPublisher(verifyCtx, staged, helper)
	cancelVerify()
	if verifyErr != nil {
		return verifyErr
	}
	previous := active + ".pulse-previous"
	_ = os.Remove(previous)
	if err := copyWindowsExecutable(active, previous); err != nil {
		return fmt.Errorf("retain previous Windows desktop executable: %w", err)
	}

	installCtx, cancelInstall := context.WithTimeout(context.Background(), 10*time.Minute)
	installer := exec.CommandContext(
		installCtx,
		staged,
		"/VERYSILENT",
		"/SUPPRESSMSGBOXES",
		"/NORESTART",
		"/CLOSEAPPLICATIONS",
		"/NORESTARTAPPLICATIONS",
	)
	output, installErr := installer.CombinedOutput()
	cancelInstall()
	if installErr != nil {
		rollbackErr := restoreWindowsDesktop(active, previous)
		return fmt.Errorf("install Windows desktop update (%s): %w; rollback: %w", strings.TrimSpace(string(output)), installErr, rollbackErr)
	}

	service := exec.Command(active, "--service")
	service.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
	if err := service.Start(); err != nil {
		rollbackErr := restoreWindowsDesktop(active, previous)
		restartErr := errors.Join(startWindowsService(active), startWindowsDesktop(active))
		return fmt.Errorf("start updated Windows service: %w; rollback: %w; restart previous: %w", err, rollbackErr, restartErr)
	}
	if err := waitForDesktopHealth(active, expectedVersion, service.Process, 30*time.Second); err == nil {
		_ = service.Process.Release()
		return startWindowsDesktop(active)
	} else {
		killErr := service.Process.Kill()
		exitErr := waitForProcess(uint32(service.Process.Pid), 10*time.Second)
		if exitErr != nil {
			_ = service.Process.Release()
			return fmt.Errorf("updated Windows desktop failed local health: %w; stop replacement: %w", err, errors.Join(killErr, exitErr))
		}
		_, waitErr := service.Process.Wait()
		rollbackErr := restoreWindowsDesktop(active, previous)
		restartErr := errors.Join(startWindowsService(active), startWindowsDesktop(active))
		return fmt.Errorf("updated Windows desktop failed local health: %w; stop replacement: %w; rollback: %w; restart previous: %w", err, errors.Join(killErr, waitErr), rollbackErr, restartErr)
	}
}

func waitForDesktopHealth(executable, expectedVersion string, service *os.Process, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		running, err := windowsProcessRunning(service.Pid)
		if err != nil {
			return fmt.Errorf("inspect replacement service process: %w", err)
		}
		if !running {
			return errors.New("replacement service exited before becoming healthy")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		lastErr = exec.CommandContext(ctx, executable, "--health-check", expectedVersion).Run()
		cancel()
		if lastErr == nil {
			running, err = windowsProcessRunning(service.Pid)
			if err != nil {
				return fmt.Errorf("recheck replacement service process: %w", err)
			}
			if running {
				return nil
			}
			return errors.New("replacement service exited during its health check")
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("local IPC did not become healthy within %s: %w", timeout, lastErr)
}

func windowsProcessRunning(pid int) (bool, error) {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return false, nil
		}
		return false, err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	result, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return false, err
	}
	return result == uint32(windows.WAIT_TIMEOUT), nil
}

func restoreWindowsDesktop(active, previous string) error {
	failed := active + ".pulse-failed"
	_ = os.Remove(failed)
	var moveErr error
	if _, err := os.Stat(active); err == nil {
		moveErr = os.Rename(active, failed)
	} else if !errors.Is(err, os.ErrNotExist) {
		moveErr = err
	}
	restoreErr := os.Rename(previous, active)
	return errors.Join(moveErr, restoreErr)
}

func startWindowsService(active string) error {
	command := exec.Command(active, "--service")
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func startWindowsDesktop(active string) error {
	command := exec.Command(active)
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func verifyWindowsPublisher(ctx context.Context, staged, executable string) error {
	const script = `$ErrorActionPreference = 'Stop'
$staged = Get-AuthenticodeSignature -LiteralPath $env:AXON_PULSE_STAGED_UPDATE
$current = Get-AuthenticodeSignature -LiteralPath $env:AXON_PULSE_CURRENT_EXE
if ($staged.Status -ne 'Valid') { throw "staged Authenticode status is $($staged.Status)" }
if ($current.Status -ne 'Valid') { throw "installed Authenticode status is $($current.Status)" }
$stagedSubject = $staged.SignerCertificate.Subject
$currentSubject = $current.SignerCertificate.Subject
if ([string]::IsNullOrWhiteSpace($stagedSubject)) { throw 'staged publisher is empty' }
if ($stagedSubject -cne $currentSubject) { throw "publisher mismatch: staged=$stagedSubject installed=$currentSubject" }`
	command := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	command.Env = append(os.Environ(), "AXON_PULSE_STAGED_UPDATE="+staged, "AXON_PULSE_CURRENT_EXE="+executable)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("verify Windows update publisher (%s): %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

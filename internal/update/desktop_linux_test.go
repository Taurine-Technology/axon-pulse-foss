//go:build linux

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
	"testing"
	"time"
)

func TestUnixRelaunchStartsTrackedServiceBeforeHealthyGUI(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "pulse")
	events := filepath.Join(dir, "events")
	servicePID := filepath.Join(dir, "service.pid")
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
  --service)
    echo "service" >> %q
    echo "$$" > %q
    trap 'exit 0' TERM INT
    while true; do sleep 1; done
    ;;
  --health-check)
    [ "$2" = "1.2.3" ] && [ -s %q ]
    ;;
  *)
    [ -s %q ] || exit 1
    echo "gui" >> %q
    ;;
esac
`, events, servicePID, servicePID, servicePID, events)
	if err := os.WriteFile(target, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", unixRelaunchScript, "pulse-update", "999999", "1", target, "1.2.3", "30")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("relaunch helper failed (%s): %v", output, err)
	}
	pidBytes, err := os.ReadFile(servicePID)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	// The helper backgrounds the GUI and exits immediately, so the GUI's
	// event can land after the helper returns.
	deadline := time.Now().Add(2 * time.Second)
	var got []string
	for {
		eventBytes, _ := os.ReadFile(events)
		got = strings.Fields(string(eventBytes))
		if len(got) >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(got) != 2 || got[0] != "service" || got[1] != "gui" {
		t.Fatalf("launch order = %v, want service then GUI", got)
	}
}

func TestUnixRelaunchFailedHealthRestoresAndRelaunchesPreviousSlot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "pulse")
	previous := target + ".pulse-previous"
	events := filepath.Join(dir, "events")
	newPIDPath := filepath.Join(dir, "new-service.pid")
	newAlive := filepath.Join(dir, "new-service.alive")
	oldPIDPath := filepath.Join(dir, "old-service.pid")
	newScript := fmt.Sprintf(`#!/bin/sh
case "$1" in
  --service)
    echo "new-service" >> %q
    echo "$$" > %q
    touch %q
    trap 'rm -f %q; exit 0' TERM INT
    while true; do sleep 1; done
    ;;
  --health-check) exit 1 ;;
  *) echo "new-gui" >> %q ;;
esac
`, events, newPIDPath, newAlive, newAlive, events)
	oldScript := fmt.Sprintf(`#!/bin/sh
case "$1" in
  --service)
    echo "old-service" >> %q
    echo "$$" > %q
    trap 'exit 0' TERM INT
    while true; do sleep 1; done
    ;;
  --health-check) exit 0 ;;
  *) echo "old-gui" >> %q ;;
esac
`, events, oldPIDPath, events)
	if err := os.WriteFile(target, []byte(newScript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previous, []byte(oldScript), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", unixRelaunchScript, "pulse-update", "999999", "1", target, "1.2.3", "1")
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("rollback helper unexpectedly succeeded (%s)", output)
	} else if exitErr := (*exec.ExitError)(nil); !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("rollback helper error (%s): %v", output, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		eventBytes, _ := os.ReadFile(events)
		if strings.Contains(string(eventBytes), "old-gui") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("previous GUI was not relaunched; events=%q", eventBytes)
		}
		time.Sleep(time.Millisecond)
	}
	activeBytes, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(activeBytes) != oldScript {
		t.Fatal("failed update did not restore the previous executable")
	}
	if _, err := os.Stat(newAlive); !os.IsNotExist(err) {
		t.Fatalf("replacement service remained alive after rollback: %v", err)
	}
	newPIDBytes, err := os.ReadFile(newPIDPath)
	if err != nil {
		t.Fatal(err)
	}
	newPID, err := strconv.Atoi(strings.TrimSpace(string(newPIDBytes)))
	if err != nil {
		t.Fatal(err)
	}
	for syscall.Kill(newPID, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if syscall.Kill(newPID, 0) == nil {
		t.Fatal("replacement service PID survived rollback")
	}
	oldPIDBytes, err := os.ReadFile(oldPIDPath)
	if err != nil {
		t.Fatal(err)
	}
	oldPID, err := strconv.Atoi(strings.TrimSpace(string(oldPIDBytes)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(oldPID, syscall.SIGKILL) })
	eventBytes, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	eventText := string(eventBytes)
	if !strings.Contains(eventText, "new-service") || !strings.Contains(eventText, "old-service") || !strings.Contains(eventText, "old-gui") || strings.Contains(eventText, "new-gui") {
		t.Fatalf("rollback lifecycle events = %q", eventText)
	}
}

func TestUnixRelaunchUsesBoundedExactPIDRollback(t *testing.T) {
	t.Parallel()
	for _, required := range []string{"--service", `--health-check "$4"`, `terminate_pid "$2"`, `terminate_pid "$service_pid"`, "kill -KILL"} {
		if !strings.Contains(unixRelaunchScript, required) {
			t.Fatalf("relaunch helper omitted %q", required)
		}
	}
	if strings.Contains(unixRelaunchScript, "wait \"$service_pid\"") || strings.Contains(unixRelaunchScript, "pkill") {
		t.Fatal("relaunch helper uses an unbounded or non-specific process cleanup")
	}
}

func TestAPTBuildCannotReplaceHeadlessOrDesktopExecutables(t *testing.T) {
	previous := installMethod
	installMethod = "apt"
	t.Cleanup(func() { installMethod = previous })
	// An inherited APPIMAGE environment must not override package ownership.
	t.Setenv("APPIMAGE", filepath.Join(t.TempDir(), "pulse.AppImage"))
	if err := Activate(context.Background(), "missing", "missing"); !errors.Is(err, ErrPackageManaged) {
		t.Fatalf("headless activation error = %v", err)
	}
	if err := ActivateDesktop(context.Background(), "missing", "missing", "2.0.0"); !errors.Is(err, ErrPackageManaged) {
		t.Fatalf("desktop activation error = %v", err)
	}
}

//go:build darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDesktopRelaunchWaitsForOwningProcess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "opened")
	waiting := filepath.Join(dir, "waiting")
	// Substitute only the final open command; exercise the real PID wait
	// without opening or stopping the user's installed application.
	script := strings.Replace(desktopRelaunchScript, `exec /usr/bin/open "$2"`, `printf '%s' "$2" > "$3"`, 1)
	script = strings.Replace(script, "sleep 1", `touch "$4"; sleep 1`, 1)
	owner := exec.Command("/bin/sleep", "30")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Process.Kill() })
	helper := exec.CommandContext(t.Context(), "/bin/sh", "-c", script, "relaunch-test", strconv.Itoa(owner.Process.Pid), "/Applications/Axon Pulse.app", marker, waiting)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(waiting); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not wait for the desktop")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("desktop opened before the old instance exited: %v", err)
	}
	_ = owner.Process.Kill()
	_ = owner.Wait()
	if err := helper.Wait(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "/Applications/Axon Pulse.app" {
		t.Fatalf("relaunch target = %q, %v", got, err)
	}
}

func TestDesktopRelaunchDoesNotKillAnUnresponsiveOwner(t *testing.T) {
	t.Parallel()
	// Expire immediately against this live test process. The helper must
	// return an error instead of opening another instance or killing us.
	script := strings.Replace(desktopRelaunchScript, `-ge 30`, `-ge 0`, 1)
	command := exec.CommandContext(t.Context(), "/bin/sh", "-c", script, "relaunch-test", strconv.Itoa(os.Getpid()), "/unused.app")
	if err := command.Run(); err == nil {
		t.Fatal("relaunch accepted an owner that never exited")
	}
}

func TestDesktopBundlePreservesPathsWithSpaces(t *testing.T) {
	t.Parallel()
	bundle, err := desktopBundle("/Applications/Axon Pulse.app/Contents/MacOS/Axon Pulse")
	if err != nil || bundle != "/Applications/Axon Pulse.app" {
		t.Fatalf("bundle = %q, %v", bundle, err)
	}
	if _, err := desktopBundle("/tmp/axon-pulse-desktop"); err == nil {
		t.Fatal("accepted an unpackaged executable for relaunch")
	}
}

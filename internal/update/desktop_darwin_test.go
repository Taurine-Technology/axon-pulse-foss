//go:build darwin

package update

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseMacTeamIdentifier(t *testing.T) {
	t.Parallel()
	teamID, err := parseMacTeamIdentifier("Executable=/Applications/Axon Pulse.app\nTeamIdentifier=ABCDE12345\nRuntime Version=26.0.0\n")
	if err != nil || teamID != "ABCDE12345" {
		t.Fatalf("TeamIdentifier = %q, %v", teamID, err)
	}
	if _, err := parseMacTeamIdentifier("TeamIdentifier=not set\n"); err == nil {
		t.Fatal("accepted a signature without a TeamIdentifier")
	}
}

func TestMacSwapTracksReplacementServiceAndBindsExpectedVersion(t *testing.T) {
	t.Parallel()
	for _, required := range []string{"--service", `--health-check "$5"`, `--args --relaunch-after-update`, `terminate_pid "$service_pid"`, "kill -KILL", `/usr/sbin/lsof -t -- "$lock"`, `terminate_pid "$gui_pid"`, `[ -n "$7" ]`, `--relaunch-after-update "$2"`, `[ "$gui_pid" = "$(/usr/sbin/lsof -t -- "$lock" 2>/dev/null | head -n 1)" ]`} {
		if !strings.Contains(macSwapScript, required) {
			t.Fatalf("macOS swap helper omitted %q", required)
		}
	}
	if strings.Contains(macSwapScript, "pkill") || strings.Contains(macSwapScript, "wait \"$service_pid\"") {
		t.Fatal("macOS swap helper uses non-specific or unbounded process cleanup")
	}
	serviceIndex := strings.Index(macSwapScript, `--service >/dev/null`)
	openIndex := strings.Index(macSwapScript, `/usr/bin/open -n "$2"`)
	if serviceIndex < 0 || openIndex < 0 || serviceIndex > openIndex {
		t.Fatal("macOS swap helper launches the GUI before the replacement service passes health")
	}
	// The plain reopen must come after the hand-off attempt and only for a
	// GUI that failed to yield, never unconditionally.
	fallbackIndex := strings.Index(macSwapScript, `terminate_pid "$gui_pid"`)
	beforeSuccessExit, _, hasSuccessExit := strings.Cut(macSwapScript, "exit 0")
	if fallbackIndex < 0 || !hasSuccessExit {
		t.Fatal("macOS swap helper lost its fallback or success exit")
	}
	plainOpen := strings.LastIndex(beforeSuccessExit, `/usr/bin/open -n "$2"`)
	if fallbackIndex < openIndex || plainOpen < fallbackIndex {
		t.Fatal("macOS swap helper does not fall back to replacing a GUI that ignored the hand-off")
	}
}

// Exercise the actual swap script with temporary app bundles and services.
// Only LaunchServices is replaced so the installed desktop is never touched.
func TestMacSwapRelaunchesWithoutDesktopParent(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(strconv.FormatBool(rollback), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			bundle := filepath.Join(dir, "Axon Pulse.app")
			for _, candidate := range []struct{ path, version string }{{bundle, "1"}, {bundle + ".pulse-new", "2"}} {
				bin := filepath.Join(candidate.path, "Contents", "MacOS")
				if err := os.MkdirAll(bin, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(bin, "version"), []byte(candidate.version), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(bin, "Axon Pulse"), []byte(`#!/bin/sh
version=$(cat "$(dirname "$0")/version")
case "$1" in
  --service)
    echo "$$" > "$PULSE_TEST_DIR/service-$version.pid"
    exec /bin/sleep 30 ;;
  --health-check)
    [ "$PULSE_TEST_ROLLBACK" != true ] && [ "$2" = "$version" ] || exit 1
    echo "$version" > "$PULSE_TEST_DIR/healthy" ;;
  *) exit 2 ;;
esac
`), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				for _, version := range []string{"1", "2"} {
					data, err := os.ReadFile(filepath.Join(dir, "service-"+version+".pid"))
					if err != nil {
						continue
					}
					pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
					if err != nil {
						continue
					}
					if process, err := os.FindProcess(pid); err == nil {
						_ = process.Kill()
					}
				}
			})
			if err := os.WriteFile(filepath.Join(dir, "open"), []byte(`#!/bin/sh
printf '%s\n' "$@" > "$PULSE_TEST_DIR/opened"
`), 0o700); err != nil {
				t.Fatal(err)
			}
			// Supply an exited, reaped service PID. There is deliberately no
			// desktop parent argument: repeated updates must not depend on it.
			oldService := exec.Command("/usr/bin/true")
			if err := oldService.Run(); err != nil {
				t.Fatal(err)
			}
			script := strings.ReplaceAll(macSwapScript, "/usr/bin/open", `"$PULSE_TEST_DIR/open"`)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "/bin/sh", "-c", script, "swap-test", strconv.Itoa(oldService.Process.Pid), bundle, bundle+".pulse-new", "Axon Pulse", "2", "1")
			command.Env = append(os.Environ(), "PULSE_TEST_DIR="+dir, "PULSE_TEST_ROLLBACK="+strconv.FormatBool(rollback))
			output, err := command.CombinedOutput()
			if (err != nil) != rollback {
				t.Fatalf("swap error = %v, output = %s", err, output)
			}
			if ctx.Err() != nil {
				t.Fatal(ctx.Err())
			}
			opened, err := os.ReadFile(filepath.Join(dir, "opened"))
			if err != nil || string(opened) != "-n\n"+bundle+"\n--args\n--relaunch-after-update\n"+bundle+"\n" {
				t.Fatalf("relaunch = %q, %v", opened, err)
			}
			version, err := os.ReadFile(filepath.Join(bundle, "Contents", "MacOS", "version"))
			want := "2"
			if rollback {
				want = "1"
			}
			if err != nil || string(version) != want {
				t.Fatalf("installed version = %q, %v", version, err)
			}
			if !rollback {
				healthy, err := os.ReadFile(filepath.Join(dir, "healthy"))
				if err != nil || string(healthy) != "2\n" {
					t.Fatalf("health = %q, %v", healthy, err)
				}
			}
		})
	}
}

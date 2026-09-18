//go:build darwin

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
)

// ActivateDesktop verifies the notarized app inside the downloaded DMG, stages
// it beside the current bundle, and launches a detached A/B swap helper.
func ActivateDesktop(ctx context.Context, staged, executable, expectedVersion string) error {
	if strings.ToLower(filepath.Ext(staged)) != ".dmg" {
		return errors.New("macOS desktop update is not a DMG")
	}
	bundle, err := enclosingAppBundle(executable)
	if err != nil {
		return err
	}
	mount, err := os.MkdirTemp(filepath.Dir(staged), "pulse-dmg-")
	if err != nil {
		return fmt.Errorf("create DMG mount point: %w", err)
	}
	defer func() { _ = os.RemoveAll(mount) }()
	if output, err := exec.CommandContext(ctx, "/usr/bin/hdiutil", "attach", "-readonly", "-nobrowse", "-mountpoint", mount, staged).CombinedOutput(); err != nil {
		return fmt.Errorf("mount Pulse DMG (%s): %w", output, err)
	}
	defer func() { _ = exec.Command("/usr/bin/hdiutil", "detach", mount).Run() }() // best-effort detach
	entries, err := os.ReadDir(mount)
	if err != nil {
		return err
	}
	var source string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasSuffix(entry.Name(), ".app") {
			source = filepath.Join(mount, entry.Name())
			break
		}
	}
	if source == "" {
		return errors.New("downloaded DMG contains no application bundle")
	}
	newBundle := bundle + ".pulse-new"
	_ = os.RemoveAll(newBundle)
	if output, err := exec.CommandContext(ctx, "/usr/bin/ditto", source, newBundle).CombinedOutput(); err != nil {
		return fmt.Errorf("stage macOS app (%s): %w", output, err)
	}
	if output, err := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", newBundle).CombinedOutput(); err != nil {
		_ = os.RemoveAll(newBundle)
		return fmt.Errorf("verify staged macOS app (%s): %w", output, err)
	}
	if output, err := exec.CommandContext(ctx, "/usr/sbin/spctl", "--assess", "--type", "execute", "--verbose=4", newBundle).CombinedOutput(); err != nil {
		_ = os.RemoveAll(newBundle)
		return fmt.Errorf("assess staged macOS app trust (%s): %w", output, err)
	}
	currentTeamID, err := macTeamIdentifier(ctx, bundle)
	if err != nil {
		_ = os.RemoveAll(newBundle)
		return fmt.Errorf("read installed macOS signer: %w", err)
	}
	stagedTeamID, err := macTeamIdentifier(ctx, newBundle)
	if err != nil {
		_ = os.RemoveAll(newBundle)
		return fmt.Errorf("read staged macOS signer: %w", err)
	}
	if currentTeamID != stagedTeamID {
		_ = os.RemoveAll(newBundle)
		return fmt.Errorf("staged macOS signer Team ID %q does not match installed Team ID %q", stagedTeamID, currentTeamID)
	}
	newExecutable := filepath.Join(newBundle, "Contents", "MacOS", filepath.Base(executable))
	if output, err := exec.CommandContext(ctx, newExecutable, "version").CombinedOutput(); err != nil {
		_ = os.RemoveAll(newBundle)
		return fmt.Errorf("staged macOS app health check failed (%s): %w", output, err)
	}
	return startMacSwap(bundle, newBundle, filepath.Base(executable), expectedVersion)
}

func macTeamIdentifier(ctx context.Context, bundle string) (string, error) {
	output, err := exec.CommandContext(ctx, "/usr/bin/codesign", "-dv", "--verbose=4", bundle).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("inspect code signature (%s): %w", output, err)
	}
	return parseMacTeamIdentifier(string(output))
}

func parseMacTeamIdentifier(output string) (string, error) {
	for line := range strings.SplitSeq(output, "\n") {
		if teamID, found := strings.CutPrefix(strings.TrimSpace(line), "TeamIdentifier="); found && teamID != "" && teamID != "not set" {
			return teamID, nil
		}
	}
	return "", errors.New("code signature has no TeamIdentifier")
}

func enclosingAppBundle(executable string) (string, error) {
	path := filepath.Clean(executable)
	for path != filepath.Dir(path) {
		if strings.HasSuffix(strings.ToLower(path), ".app") {
			return path, nil
		}
		path = filepath.Dir(path)
	}
	return "", errors.New("running executable is not inside a macOS app bundle")
}

// The desktop may no longer be the service parent after an earlier update.
// Route relaunch through its single-instance handler instead of killing a
// PPID. A GUI older than that handler (0.0.7-alpha.2 and earlier) ignores the
// hand-off and would keep serving its stale interface forever, so the helper
// records the lock holder first and, if it is still alive after a bounded
// wait, terminates it and opens the new bundle plainly.
func startMacSwap(bundle, newBundle, executableName, expectedVersion string) error {
	command := exec.Command(
		"/bin/sh", "-c", macSwapScript, "pulse-update",
		strconv.Itoa(os.Getpid()), bundle, newBundle, executableName, expectedVersion, "30", DesktopInstanceID+".lock",
	)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return command.Start()
}

const (
	macSwapScript = `terminate_pid() {
  victim="$1"
  kill -TERM "$victim" 2>/dev/null || true
  attempts=0
  while kill -0 "$victim" 2>/dev/null && [ "$attempts" -lt 5 ]; do
    sleep 1
    attempts=$((attempts + 1))
  done
  if kill -0 "$victim" 2>/dev/null; then kill -KILL "$victim" 2>/dev/null || true; fi
}
while kill -0 "$1" 2>/dev/null; do sleep 1; done
old="$2.pulse-previous"
rm -rf "$old"
if mv "$2" "$old" && mv "$3" "$2"; then
  "$2/Contents/MacOS/$4" --service >/dev/null 2>&1 &
  service_pid=$!
  i=0
  while [ "$i" -lt "$6" ]; do
    if kill -0 "$service_pid" 2>/dev/null && "$2/Contents/MacOS/$4" --health-check "$5" >/dev/null 2>&1 && kill -0 "$service_pid" 2>/dev/null; then
      gui_pid=""
      lock="${TMPDIR:-/tmp}/$7"
      if [ -n "$7" ] && [ -f "$lock" ]; then gui_pid=$(/usr/sbin/lsof -t -- "$lock" 2>/dev/null | head -n 1); fi
      /usr/bin/open -n "$2" --args --relaunch-after-update "$2"
      j=0
      while [ -n "$gui_pid" ] && kill -0 "$gui_pid" 2>/dev/null && [ "$j" -lt 15 ]; do
        sleep 1
        j=$((j + 1))
      done
      if [ -n "$gui_pid" ] && kill -0 "$gui_pid" 2>/dev/null &&
         [ "$gui_pid" = "$(/usr/sbin/lsof -t -- "$lock" 2>/dev/null | head -n 1)" ]; then
        terminate_pid "$gui_pid"
        /usr/bin/open -n "$2"
      fi
      exit 0
    fi
    if ! kill -0 "$service_pid" 2>/dev/null; then break; fi
    sleep 1
    i=$((i + 1))
  done
  terminate_pid "$service_pid"
  failed="$2.pulse-failed"
  rm -rf "$failed"
  mv "$2" "$failed" || exit 1
  mv "$old" "$2" || exit 1
  "$2/Contents/MacOS/$4" --service >/dev/null 2>&1 &
  /usr/bin/open -n "$2" --args --relaunch-after-update "$2"
  exit 1
fi
if [ -d "$old" ] && [ ! -d "$2" ]; then mv "$old" "$2"; fi
exit 1`
)

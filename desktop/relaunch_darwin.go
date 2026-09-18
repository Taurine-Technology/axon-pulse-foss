//go:build darwin

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// The old desktop owns its PID and exits itself. The detached helper opens
// the replaced bundle only after that instance has released its instance lock.
const (
	desktopRelaunchScript = `attempts=0
while kill -0 "$1" 2>/dev/null; do
  if [ "$attempts" -ge 30 ]; then exit 1; fi
  sleep 1
  attempts=$((attempts + 1))
done
exec /usr/bin/open "$2"`
)

// prepareDesktopRelaunch spawns the detached helper that reopens the desktop
// once this process has exited. bundle is the replacement bundle named by the
// update helper; when empty (a user-driven relaunch, or an older helper) the
// bundle containing this executable is used. os.Executable reports the
// launch-time path, which the swap keeps for the replacement, so both agree
// after an update; the explicit path removes that dependency.
func prepareDesktopRelaunch(bundle string) (bool, error) {
	if !strings.HasSuffix(strings.ToLower(filepath.Clean(bundle)), ".app") {
		executable, err := os.Executable()
		if err != nil {
			return false, err
		}
		bundle, err = desktopBundle(executable)
		if err != nil {
			return false, err
		}
	}
	command := exec.Command("/bin/sh", "-c", desktopRelaunchScript, "pulse-desktop-relaunch", strconv.Itoa(os.Getpid()), bundle)
	detach(command)
	if err := command.Start(); err != nil {
		return false, err
	}
	go func() { _ = command.Wait() }()
	return true, nil
}

func desktopBundle(executable string) (string, error) {
	for path := filepath.Clean(executable); path != filepath.Dir(path); path = filepath.Dir(path) {
		if strings.HasSuffix(strings.ToLower(path), ".app") {
			return path, nil
		}
	}
	return "", errors.New("desktop executable is not inside a macOS app bundle")
}

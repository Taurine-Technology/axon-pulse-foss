//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"strconv"

	pulseupdate "github.com/Taurine-Technology/axon-pulse/internal/update"
)

const (
	// desktopRelaunchScript: the old desktop owns its PID and exits itself;
	// the detached helper starts the installed binary only after that
	// instance has released its single-instance lock. Used after an APT
	// upgrade replaced the package while this window was running (the
	// service restarts itself independently).
	desktopRelaunchScript = `attempts=0
while kill -0 "$1" 2>/dev/null; do
  if [ "$attempts" -ge 30 ]; then exit 1; fi
  sleep 1
  attempts=$((attempts + 1))
done
exec "$2"`
)

// prepareDesktopRelaunch spawns the detached helper that reopens the desktop
// once this process has exited. The bundle argument is a macOS concept; on
// Linux the installed executable path is always used, so a package upgrade
// that unlinked our original inode still relaunches the new build.
func prepareDesktopRelaunch(string) (bool, error) {
	executable, err := pulseupdate.InstalledExecutable()
	if err != nil {
		return false, err
	}
	info, err := os.Stat(executable)
	if err != nil {
		return false, err
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return false, errors.New("installed Pulse executable is not runnable")
	}
	command := exec.Command("/bin/sh", "-c", desktopRelaunchScript, "pulse-desktop-relaunch", strconv.Itoa(os.Getpid()), executable)
	detach(command)
	if err := command.Start(); err != nil {
		return false, err
	}
	go func() { _ = command.Wait() }()
	return true, nil
}

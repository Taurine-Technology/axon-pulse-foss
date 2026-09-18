//go:build linux

package update

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// ActivateDesktop atomically updates an AppImage and launches the replacement
// after the old service and its parent tray process have exited.
func ActivateDesktop(ctx context.Context, staged, _ string, expectedVersion string) error {
	if InstallMethod() == "apt" {
		return ErrPackageManaged
	}
	appImage := os.Getenv("APPIMAGE")
	if appImage == "" {
		return errors.New("desktop updates for deb installs are applied by the system package manager")
	}
	if err := Activate(ctx, staged, appImage); err != nil {
		return err
	}
	return startUnixRelaunch(appImage, expectedVersion)
}

func startUnixRelaunch(target, expectedVersion string) error {
	command := exec.Command("/bin/sh", "-c", unixRelaunchScript, "pulse-update", strconv.Itoa(os.Getpid()), strconv.Itoa(os.Getppid()), target, expectedVersion, "30")
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return command.Start()
}

const (
	unixRelaunchScript = `terminate_pid() {
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
if [ "$2" -gt 1 ]; then terminate_pid "$2"; fi
"$3" --service >/dev/null 2>&1 &
service_pid=$!
i=0
while [ "$i" -lt "$5" ]; do
  if kill -0 "$service_pid" 2>/dev/null && "$3" --health-check "$4" >/dev/null 2>&1 && kill -0 "$service_pid" 2>/dev/null; then
    "$3" >/dev/null 2>&1 &
    exit 0
  fi
  if ! kill -0 "$service_pid" 2>/dev/null; then break; fi
  sleep 1
  i=$((i + 1))
done
terminate_pid "$service_pid"
failed="$3.pulse-failed"
rm -f "$failed"
mv "$3" "$failed" || exit 1
mv "$3.pulse-previous" "$3" || exit 1
"$3" --service >/dev/null 2>&1 &
"$3" >/dev/null 2>&1 &
exit 1`
)

package update

import (
	"errors"
)

// installMethod is set to "apt" with -X when building Debian packages. The
// empty development/portable default retains the built-in updater. This is
// embedded in both desktop and headless binaries; no package-manager probes
// or environment overrides are needed on the measurement scheduler's path.
var (
	installMethod string

	ErrPackageManaged = errors.New("updates are managed by APT; use your system package manager to upgrade Pulse")
)

// InstallMethod identifies the authority responsible for replacing this binary.
func InstallMethod() string {
	if installMethod == "apt" {
		return "apt"
	}
	return "self"
}

package update

import (
	"os"
	"strings"
)

const (
	// deletedMarker is what Linux appends to /proc/self/exe once the file a
	// process was started from has been unlinked — which is exactly what dpkg
	// does when it upgrades a package underneath a running Pulse.
	deletedMarker = " (deleted)"
)

// InstalledExecutable returns the path this process was started from as it
// exists on disk now. After a package upgrade os.Executable still names the
// old, unlinked inode; callers that need to start or re-exec the *installed*
// binary (service respawn, desktop relaunch, APT restart) must use this.
func InstalledExecutable() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(executable, deletedMarker), nil
}

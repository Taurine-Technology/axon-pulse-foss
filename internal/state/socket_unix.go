//go:build !windows

package state

import (
	"path/filepath"
	"runtime"
)

func defaultSocketPath(dir string) string {
	if runtime.GOOS == "linux" && isPrivileged() {
		return "/run/axon-pulse/pulsed.sock"
	}
	return filepath.Join(dir, "pulsed.sock")
}

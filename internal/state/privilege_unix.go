//go:build !windows

package state

import (
	"os"
)

func isPrivileged() bool {
	return os.Geteuid() == 0
}

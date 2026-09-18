//go:build !windows

package state

import (
	"os"
)

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}

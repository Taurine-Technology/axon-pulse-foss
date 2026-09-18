//go:build !windows

package update

import (
	"errors"
)

var (
	// ErrApplyUnsupported reports that the update-helper subcommand exists
	// only on Windows, where a helper process swaps the executable.
	ErrApplyUnsupported = errors.New("the update-helper command is available only on Windows")
)

// ApplyPending is the non-Windows stub of the update helper.
func ApplyPending([]string) error {
	return ErrApplyUnsupported
}

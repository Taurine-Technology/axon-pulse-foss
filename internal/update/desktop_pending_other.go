//go:build !windows

package update

import (
	"errors"
)

func ApplyDesktopPending([]string) error {
	return errors.New("the desktop update helper is available only on Windows")
}

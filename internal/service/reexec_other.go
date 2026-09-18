//go:build !unix

package service

import "errors"

func ReexecInstalled(string) error {
	return errors.New("in-place restart is only supported on Unix package installs")
}

//go:build externalservice

package main

import "errors"

func runService(_, _ string) error {
	return errors.New("this desktop package uses the separately installed axon-pulse service")
}

func ensureService(string) {}

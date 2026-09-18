//go:build !linux && !darwin && !windows

package prober

import "errors"

func DefaultGateway() (string, error) {
	return "", errors.New("default gateway discovery unavailable on this platform")
}

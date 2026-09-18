//go:build linux

package prober

import (
	"fmt"
	"os"
)

func DefaultGateway() (string, error) {
	file, err := os.Open("/proc/net/route")
	if err != nil {
		return "", fmt.Errorf("open route table: %w", err)
	}
	defer func() { _ = file.Close() }() // read-only; close error is not meaningful
	return parseRoute(file)
}

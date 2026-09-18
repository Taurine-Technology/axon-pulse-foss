//go:build darwin

package prober

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

func DefaultGateway() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/sbin/route", "-n", "get", "default").Output()
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(output), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(key) == "gateway" {
			gateway := strings.TrimSpace(value)
			if gateway != "" {
				return gateway, nil
			}
		}
	}
	return "", errors.New("default gateway unavailable")
}

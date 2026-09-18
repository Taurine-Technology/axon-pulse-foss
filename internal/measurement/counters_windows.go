//go:build windows

package measurement

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func totalInterfaceBytes(ctx context.Context) (uint64, error) {
	// netstat occasionally wedges; an unbounded exec here would hold the
	// one-active-test lock forever.
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(bounded, "netstat", "-e").Output()
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		received, receiveErr := strconv.ParseUint(fields[1], 10, 64)
		sent, sendErr := strconv.ParseUint(fields[2], 10, 64)
		if receiveErr == nil && sendErr == nil {
			return received + sent, nil
		}
	}
	return 0, errors.New("interface counters unavailable on Windows")
}

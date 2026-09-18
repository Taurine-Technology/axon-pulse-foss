//go:build linux

package measurement

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func totalInterfaceBytes(_ context.Context) (uint64, error) {
	file, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }() // read-only; close error is not meaningful
	var total uint64
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		name, values, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "lo" {
			continue
		}
		fields := strings.Fields(values)
		if len(fields) < 9 {
			continue
		}
		received, receiveErr := strconv.ParseUint(fields[0], 10, 64)
		sent, sendErr := strconv.ParseUint(fields[8], 10, 64)
		if receiveErr == nil && sendErr == nil {
			total += received + sent
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read interface counters: %w", err)
	}
	return total, nil
}

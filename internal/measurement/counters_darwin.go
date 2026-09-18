//go:build darwin

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
	// netstat occasionally wedges around sleep/wake; an unbounded exec here
	// would hold the one-active-test lock forever.
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(bounded, "/usr/sbin/netstat", "-ibn").Output()
	if err != nil {
		return 0, err
	}
	lines := strings.Split(string(output), "\n")
	if len(lines) < 2 {
		return 0, errors.New("netstat returned no counters")
	}
	header := strings.Fields(lines[0])
	nameIndex, inIndex, outIndex := fieldIndex(header, "Name"), fieldIndex(header, "Ibytes"), fieldIndex(header, "Obytes")
	if nameIndex < 0 || inIndex < 0 || outIndex < 0 {
		return 0, errors.New("netstat counter columns unavailable")
	}
	maximum := map[string]uint64{}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) <= max(nameIndex, max(inIndex, outIndex)) || fields[nameIndex] == "lo0" {
			continue
		}
		inBytes, inErr := strconv.ParseUint(fields[inIndex], 10, 64)
		outBytes, outErr := strconv.ParseUint(fields[outIndex], 10, 64)
		if inErr == nil && outErr == nil && inBytes+outBytes > maximum[fields[nameIndex]] {
			maximum[fields[nameIndex]] = inBytes + outBytes
		}
	}
	var total uint64
	for _, value := range maximum {
		total += value
	}
	return total, nil
}

func fieldIndex(fields []string, wanted string) int {
	for index, field := range fields {
		if field == wanted {
			return index
		}
	}
	return -1
}

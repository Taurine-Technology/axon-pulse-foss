package diagnostics

import (
	"os"
	"strconv"
	"strings"
)

func readCPUBusyFile(path string) (CPUSample, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CPUSample{}, false
	}
	line, _, _ := strings.Cut(string(data), "\n")
	parts := strings.Fields(line)
	if len(parts) < 5 || parts[0] != "cpu" {
		return CPUSample{}, false
	}

	values := make([]uint64, 0, len(parts)-1)
	for _, raw := range parts[1:] {
		value, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return CPUSample{}, false
		}
		values = append(values, value)
	}
	idle := values[3]
	if len(values) > 4 {
		idle += values[4]
	}
	var total uint64
	for _, value := range values {
		total += value
	}
	return CPUSample{
		Busy:  total - idle,
		Total: total,
	}, true
}

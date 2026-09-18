package prober

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
)

func parseRoute(reader io.Reader) (string, error) {
	scanner := bufio.NewScanner(reader)
	if !scanner.Scan() {
		return "", errors.New("route table is empty")
	}
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 || fields[1] != "00000000" {
			continue
		}
		flags, err := hex.DecodeString(leftPad(fields[3], 4))
		if err != nil || len(flags) != 2 || flags[1]&0x02 == 0 {
			continue
		}
		raw, err := hex.DecodeString(fields[2])
		if err != nil || len(raw) != 4 {
			continue
		}
		return net.IPv4(raw[3], raw[2], raw[1], raw[0]).String(), nil
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read route table: %w", err)
	}
	return "", errors.New("default gateway unavailable")
}

func leftPad(value string, length int) string {
	if len(value) >= length {
		return value
	}
	return strings.Repeat("0", length-len(value)) + value
}

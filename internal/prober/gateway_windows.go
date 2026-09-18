//go:build windows

package prober

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"unsafe"
)

type (
	ipForwardRow struct {
		Destination, Mask, Policy, NextHop, InterfaceIndex uint32
		RouteType, Protocol, Age, NextHopAS                uint32
		Metric1, Metric2, Metric3, Metric4, Metric5        uint32
	}
)

const (
	errorInsufficientBuffer = 122
)

func DefaultGateway() (string, error) {
	procedure := syscall.NewLazyDLL("iphlpapi.dll").NewProc("GetIpForwardTable")
	var size uint32
	result, _, _ := procedure.Call(0, uintptr(unsafe.Pointer(&size)), 0)
	if result != errorInsufficientBuffer || size < 4 {
		return "", errors.New("default route table unavailable")
	}
	buffer := make([]byte, size)
	result, _, _ = procedure.Call(uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&size)), 0)
	if result != 0 {
		return "", fmt.Errorf("GetIpForwardTable failed: %d", result)
	}
	count := *(*uint32)(unsafe.Pointer(&buffer[0]))
	rowSize := unsafe.Sizeof(ipForwardRow{})
	first := unsafe.Add(unsafe.Pointer(&buffer[0]), 4)
	bestMetric := ^uint32(0)
	var best uint32
	for index := range count {
		row := (*ipForwardRow)(unsafe.Add(first, uintptr(index)*rowSize))
		if row.Destination == 0 && row.Mask == 0 && row.NextHop != 0 && row.Metric1 < bestMetric {
			best, bestMetric = row.NextHop, row.Metric1
		}
	}
	if best == 0 {
		return "", errors.New("default gateway unavailable")
	}
	return net.IPv4(byte(best), byte(best>>8), byte(best>>16), byte(best>>24)).String(), nil
}

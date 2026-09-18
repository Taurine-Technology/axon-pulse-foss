//go:build windows

package diagnostics

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	iphlpapi        = windows.NewLazySystemDLL("iphlpapi.dll")
	icmpCreateFile  = iphlpapi.NewProc("IcmpCreateFile")
	icmpCloseHandle = iphlpapi.NewProc("IcmpCloseHandle")
	icmpSendEcho    = iphlpapi.NewProc("IcmpSendEcho")
)

type (
	windowsPinger struct {
		handle windows.Handle
		addr   uint32
	}
	ipOptionInformation struct {
		TTL         byte
		TOS         byte
		Flags       byte
		OptionsSize byte
		OptionsData uintptr
	}
	icmpEchoReply struct {
		Address       uint32
		Status        uint32
		RoundTripTime uint32
		DataSize      uint16
		Reserved      uint16
		Data          uintptr
		Options       ipOptionInformation
	}
)

func openPlatformPinger(addr net.IP) (platformPinger, error) {
	ipv4Addr := addr.To4()
	if ipv4Addr == nil {
		return nil, fmt.Errorf("Windows ICMP echo currently requires IPv4")
	}
	handle, _, callErr := icmpCreateFile.Call()
	if windows.Handle(handle) == windows.InvalidHandle {
		return nil, fmt.Errorf("opening Windows ICMP handle: %w", callErr)
	}
	return &windowsPinger{
		handle: windows.Handle(handle),
		addr:   binary.LittleEndian.Uint32(ipv4Addr),
	}, nil
}

func (p *windowsPinger) Probe(ctx context.Context) (time.Duration, error) {
	timeout := pingProbeTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, max(time.Until(deadline), time.Millisecond))
	}

	request := []byte("axon-pulse")
	replySize := int(unsafe.Sizeof(icmpEchoReply{})) + len(request) + 8
	reply := make([]byte, replySize)
	started := time.Now()
	count, _, callErr := icmpSendEcho.Call(
		uintptr(p.handle),
		uintptr(p.addr),
		uintptr(unsafe.Pointer(&request[0])),
		uintptr(len(request)),
		0,
		uintptr(unsafe.Pointer(&reply[0])),
		uintptr(len(reply)),
		uintptr(timeout.Milliseconds()),
	)
	if count == 0 {
		return 0, fmt.Errorf("sending Windows ICMP echo: %w", callErr)
	}
	parsed := (*icmpEchoReply)(unsafe.Pointer(&reply[0]))
	if parsed.Status != 0 {
		return 0, fmt.Errorf("Windows ICMP echo status %d", parsed.Status)
	}
	if parsed.RoundTripTime > 0 {
		return time.Duration(parsed.RoundTripTime) * time.Millisecond, nil
	}
	return time.Since(started), nil
}

func (p *windowsPinger) Close() error {
	result, _, callErr := icmpCloseHandle.Call(uintptr(p.handle))
	if result == 0 {
		return fmt.Errorf("closing Windows ICMP handle: %w", callErr)
	}
	return nil
}

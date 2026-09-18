//go:build linux || darwin

package diagnostics

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"runtime"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const pingTokenSize = 16

type icmpPacketConn interface {
	ReadFrom([]byte) (int, net.Addr, error)
	WriteTo([]byte, net.Addr) (int, error)
	Close() error
	LocalAddr() net.Addr
	SetDeadline(time.Time) error
}

type unixPinger struct {
	conn        icmpPacketConn
	destination net.Addr
	messageType icmp.Type
	replyType   icmp.Type
	protocol    int
	id          int
	seq         int
	random      io.Reader
}

func openPlatformPinger(addr net.IP) (platformPinger, error) {
	network := "udp4"
	listenAddr := "0.0.0.0"
	destination := net.Addr(&net.UDPAddr{IP: addr})
	messageType := icmp.Type(ipv4.ICMPTypeEcho)
	replyType := icmp.Type(ipv4.ICMPTypeEchoReply)
	protocol := 1
	if addr.To4() == nil {
		network = "udp6"
		listenAddr = "::"
		messageType = ipv6.ICMPTypeEchoRequest
		replyType = ipv6.ICMPTypeEchoReply
		protocol = 58
	}

	conn, err := icmp.ListenPacket(network, listenAddr)
	if err != nil {
		return nil, fmt.Errorf("opening unprivileged ICMP socket: %w", err)
	}
	id, err := echoID(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &unixPinger{
		conn:        conn,
		destination: destination,
		messageType: messageType,
		replyType:   replyType,
		protocol:    protocol,
		id:          id,
		random:      rand.Reader,
	}, nil
}

func (p *unixPinger) Probe(ctx context.Context) (time.Duration, error) {
	p.seq++
	token := make([]byte, pingTokenSize)
	if _, err := io.ReadFull(p.random, token); err != nil {
		return 0, fmt.Errorf("creating ICMP probe token: %w", err)
	}
	message := icmp.Message{
		Type: p.messageType,
		Code: 0,
		Body: &icmp.Echo{
			ID:   p.id,
			Seq:  p.seq & 0xffff,
			Data: token,
		},
	}
	wire, err := message.Marshal(nil)
	if err != nil {
		return 0, fmt.Errorf("marshaling ICMP echo: %w", err)
	}

	deadline := time.Now().Add(pingProbeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok {
		if ctxDeadline.Before(deadline) {
			deadline = ctxDeadline
		}
	}
	if err := p.conn.SetDeadline(deadline); err != nil {
		return 0, fmt.Errorf("setting ICMP deadline: %w", err)
	}

	started := time.Now()
	if _, err := p.conn.WriteTo(wire, p.destination); err != nil {
		return 0, fmt.Errorf("sending ICMP echo: %w", err)
	}

	buffer := make([]byte, 1500)
	for {
		n, peer, err := p.conn.ReadFrom(buffer)
		if err != nil {
			return 0, fmt.Errorf("reading ICMP echo: %w", err)
		}
		if !samePingPeer(peer, p.destination) {
			continue
		}
		reply, err := icmp.ParseMessage(p.protocol, buffer[:n])
		if err != nil || reply.Type != p.replyType || reply.Code != 0 {
			continue
		}
		echo, ok := reply.Body.(*icmp.Echo)
		if !ok || echo.ID != p.id || echo.Seq != p.seq&0xffff || !bytes.Equal(echo.Data, token) {
			continue
		}
		return time.Since(started), nil
	}
}

func (p *unixPinger) Close() error {
	return p.conn.Close()
}

func echoID(conn icmpPacketConn) (int, error) {
	return echoIDForOS(runtime.GOOS, conn, rand.Reader)
}

func echoIDForOS(goos string, conn icmpPacketConn, random io.Reader) (int, error) {
	// Linux ping sockets use their local port as the ICMP identifier and
	// rewrite any identifier supplied in the message body.
	if goos == "linux" {
		local, ok := conn.LocalAddr().(*net.UDPAddr)
		if !ok || local.Port <= 0 || local.Port > 0xffff {
			return 0, fmt.Errorf("reading Linux ICMP socket identifier from %v", conn.LocalAddr())
		}
		return local.Port, nil
	}
	var encoded [2]byte
	if _, err := io.ReadFull(random, encoded[:]); err != nil {
		return 0, fmt.Errorf("creating ICMP echo identifier: %w", err)
	}
	return int(binary.BigEndian.Uint16(encoded[:])), nil
}

func samePingPeer(peer, destination net.Addr) bool {
	peerIP := pingAddrIP(peer)
	destinationIP := pingAddrIP(destination)
	return peerIP != nil && destinationIP != nil && peerIP.Equal(destinationIP)
}

func pingAddrIP(addr net.Addr) net.IP {
	switch value := addr.(type) {
	case *net.UDPAddr:
		return value.IP
	case *net.IPAddr:
		return value.IP
	default:
		return nil
	}
}

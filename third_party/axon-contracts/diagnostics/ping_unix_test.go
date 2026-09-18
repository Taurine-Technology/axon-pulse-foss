//go:build linux || darwin

package diagnostics

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

type packetReply struct {
	wire []byte
	peer net.Addr
	err  error
}

type scriptedPacketConn struct {
	local     net.Addr
	protocol  int
	replies   []packetReply
	build     func(*icmp.Echo) []packetReply
	readCount int
}

func (c *scriptedPacketConn) ReadFrom(buffer []byte) (int, net.Addr, error) {
	if c.readCount >= len(c.replies) {
		return 0, nil, errors.New("unexpected packet read")
	}
	reply := c.replies[c.readCount]
	c.readCount++
	copy(buffer, reply.wire)
	return len(reply.wire), reply.peer, reply.err
}

func (c *scriptedPacketConn) WriteTo(wire []byte, _ net.Addr) (int, error) {
	message, err := icmp.ParseMessage(c.protocol, wire)
	if err != nil {
		return 0, err
	}
	echo, ok := message.Body.(*icmp.Echo)
	if !ok {
		return 0, errors.New("request is not an ICMP echo")
	}
	c.replies = c.build(echo)
	return len(wire), nil
}

func (*scriptedPacketConn) Close() error                { return nil }
func (c *scriptedPacketConn) LocalAddr() net.Addr       { return c.local }
func (*scriptedPacketConn) SetDeadline(time.Time) error { return nil }

func TestUnixPingerIgnoresUnattributableReplies(t *testing.T) {
	t.Parallel()
	destination := net.ParseIP("192.0.2.10")
	wrongPeer := net.ParseIP("192.0.2.11")
	conn := &scriptedPacketConn{
		local: &net.UDPAddr{IP: net.IPv4zero, Port: 4242}, protocol: 1,
		build: func(request *icmp.Echo) []packetReply {
			matching := func(messageType icmp.Type, code, id, seq int, data []byte, peer net.IP) packetReply {
				wire, err := (&icmp.Message{
					Type: messageType,
					Code: code,
					Body: &icmp.Echo{ID: id, Seq: seq, Data: append([]byte(nil), data...)},
				}).Marshal(nil)
				if err != nil {
					t.Fatalf("marshal reply: %v", err)
				}
				return packetReply{wire: wire, peer: &net.UDPAddr{IP: peer}}
			}
			return []packetReply{
				matching(ipv4.ICMPTypeEchoReply, 0, request.ID, request.Seq, request.Data, wrongPeer),
				matching(ipv4.ICMPTypeEcho, 0, request.ID, request.Seq, request.Data, destination),
				matching(ipv4.ICMPTypeEchoReply, 1, request.ID, request.Seq, request.Data, destination),
				matching(ipv4.ICMPTypeEchoReply, 0, request.ID+1, request.Seq, request.Data, destination),
				matching(ipv4.ICMPTypeEchoReply, 0, request.ID, request.Seq+1, request.Data, destination),
				matching(ipv4.ICMPTypeEchoReply, 0, request.ID, request.Seq, []byte("wrong-token"), destination),
				// net.IP.Equal must accept an IPv4-mapped peer for an IPv4 destination.
				matching(ipv4.ICMPTypeEchoReply, 0, request.ID, request.Seq, request.Data, net.ParseIP("::ffff:192.0.2.10")),
			}
		},
	}
	pinger := &unixPinger{
		conn: conn, destination: &net.UDPAddr{IP: destination},
		messageType: ipv4.ICMPTypeEcho, replyType: ipv4.ICMPTypeEchoReply,
		protocol: 1, id: 4242, random: strings.NewReader("0123456789abcdef"),
	}

	if _, err := pinger.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if conn.readCount != 7 {
		t.Fatalf("read %d packets, want all six invalid replies ignored before the match", conn.readCount)
	}
}

func TestUnixPingerIgnoresUnattributableIPv6Replies(t *testing.T) {
	t.Parallel()
	destination := net.ParseIP("2001:db8::10")
	wrongPeer := net.ParseIP("2001:db8::11")
	conn := &scriptedPacketConn{
		local: &net.UDPAddr{IP: net.IPv6zero, Port: 4242}, protocol: 58,
		build: func(request *icmp.Echo) []packetReply {
			matching := func(messageType icmp.Type, code, id, seq int, data []byte, peer net.IP) packetReply {
				wire, err := (&icmp.Message{
					Type: messageType,
					Code: code,
					Body: &icmp.Echo{ID: id, Seq: seq, Data: append([]byte(nil), data...)},
				}).Marshal(nil)
				if err != nil {
					t.Fatalf("marshal reply: %v", err)
				}
				return packetReply{wire: wire, peer: &net.IPAddr{IP: peer}}
			}
			return []packetReply{
				matching(ipv6.ICMPTypeEchoReply, 0, request.ID, request.Seq, request.Data, wrongPeer),
				matching(ipv6.ICMPTypeEchoRequest, 0, request.ID, request.Seq, request.Data, destination),
				matching(ipv6.ICMPTypeEchoReply, 1, request.ID, request.Seq, request.Data, destination),
				matching(ipv6.ICMPTypeEchoReply, 0, request.ID+1, request.Seq, request.Data, destination),
				matching(ipv6.ICMPTypeEchoReply, 0, request.ID, request.Seq+1, request.Data, destination),
				matching(ipv6.ICMPTypeEchoReply, 0, request.ID, request.Seq, []byte("wrong-token"), destination),
				matching(ipv6.ICMPTypeEchoReply, 0, request.ID, request.Seq, request.Data, destination),
			}
		},
	}
	pinger := &unixPinger{
		conn: conn, destination: &net.UDPAddr{IP: destination},
		messageType: ipv6.ICMPTypeEchoRequest, replyType: ipv6.ICMPTypeEchoReply,
		protocol: 58, id: 4242, random: strings.NewReader("0123456789abcdef"),
	}

	if _, err := pinger.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if conn.readCount != 7 {
		t.Fatalf("read %d packets, want all six invalid IPv6 replies ignored before the match", conn.readCount)
	}
}

func TestUnixPingerTimesOutAfterMismatchedReply(t *testing.T) {
	t.Parallel()
	destination := net.ParseIP("2001:db8::10")
	timeout := &net.DNSError{Err: "timeout", IsTimeout: true}
	conn := &scriptedPacketConn{
		local: &net.UDPAddr{IP: net.IPv6zero, Port: 4242}, protocol: 58,
		build: func(request *icmp.Echo) []packetReply {
			wire, err := (&icmp.Message{
				Type: ipv6.ICMPTypeEchoReply,
				Code: 0,
				Body: &icmp.Echo{ID: request.ID, Seq: request.Seq, Data: request.Data},
			}).Marshal(nil)
			if err != nil {
				t.Fatalf("marshal reply: %v", err)
			}
			return []packetReply{
				{wire: wire, peer: &net.IPAddr{IP: net.ParseIP("2001:db8::11")}},
				{err: timeout},
			}
		},
	}
	pinger := &unixPinger{
		conn: conn, destination: &net.UDPAddr{IP: destination},
		messageType: ipv6.ICMPTypeEchoRequest, replyType: ipv6.ICMPTypeEchoReply,
		protocol: 58, id: 4242, random: strings.NewReader("0123456789abcdef"),
	}

	if _, err := pinger.Probe(context.Background()); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("Probe error = %v, want timeout after mismatched reply", err)
	}
	if conn.readCount != 2 {
		t.Fatalf("read %d packets, want mismatch followed by timeout", conn.readCount)
	}
}

func TestEchoIDForOS(t *testing.T) {
	t.Parallel()
	linuxConn := &scriptedPacketConn{local: &net.UDPAddr{IP: net.IPv4zero, Port: 4242}}
	if got, err := echoIDForOS("linux", linuxConn, strings.NewReader("ignored")); err != nil || got != 4242 {
		t.Fatalf("Linux echo ID = %d, %v; want local port 4242", got, err)
	}
	invalidLinuxConn := &scriptedPacketConn{local: &net.IPAddr{IP: net.IPv4zero}}
	if _, err := echoIDForOS("linux", invalidLinuxConn, strings.NewReader("ignored")); err == nil {
		t.Fatal("Linux echo ID accepted a local address without a ping socket port")
	}
	darwinConn := &scriptedPacketConn{local: &net.UDPAddr{IP: net.IPv4zero, Port: 4242}}
	if got, err := echoIDForOS("darwin", darwinConn, strings.NewReader("\x12\x34")); err != nil || got != 0x1234 {
		t.Fatalf("Darwin echo ID = %#x, %v; want random identifier 0x1234", got, err)
	}
}

func TestSamePingPeerHandlesAddressFamilies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		peer        net.Addr
		destination net.Addr
		want        bool
	}{
		{name: "IPv4 UDP to IP", peer: &net.UDPAddr{IP: net.ParseIP("192.0.2.1")}, destination: &net.IPAddr{IP: net.ParseIP("192.0.2.1")}, want: true},
		{name: "mapped IPv4", peer: &net.IPAddr{IP: net.ParseIP("::ffff:192.0.2.1")}, destination: &net.UDPAddr{IP: net.ParseIP("192.0.2.1")}, want: true},
		{name: "IPv6", peer: &net.UDPAddr{IP: net.ParseIP("2001:db8::1")}, destination: &net.IPAddr{IP: net.ParseIP("2001:db8::1")}, want: true},
		{name: "different IPv6", peer: &net.IPAddr{IP: net.ParseIP("2001:db8::2")}, destination: &net.UDPAddr{IP: net.ParseIP("2001:db8::1")}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := samePingPeer(test.peer, test.destination); got != test.want {
				t.Fatalf("samePingPeer(%v, %v) = %v, want %v", test.peer, test.destination, got, test.want)
			}
		})
	}
}

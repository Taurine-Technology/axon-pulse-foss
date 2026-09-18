package diagnostics

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sort"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

const (
	defaultTraceMaxHops        = 20
	defaultTraceQueriesPerHop  = 3
	defaultTraceProbeTimeoutMS = 1000
	defaultTraceBudgetMS       = 15000

	// Hard caps enforced regardless of what the command asks for.
	maxTraceHops          = 30
	maxTraceQueriesPerHop = 3
	maxTraceBudgetMS      = 20000

	// Give up after this many consecutive fully-silent hops — the rest of
	// the path is almost certainly black-holing probes and every silent hop
	// costs queries*timeout of wall clock.
	maxTraceSilentHops = 4

	// Classic traceroute base port; the probe sequence number is added so
	// each in-flight probe has a unique destination port to match on.
	traceUDPBasePort = 33434

	// Per-hop rDNS budget; hostname lookup is best-effort context only.
	traceRDNSTimeout = 300 * time.Millisecond

	traceProtocolUDP  = "udp"
	traceProtocolICMP = "icmp"
)

type (
	// PathHopResult is one TTL rung's aggregated outcome.
	PathHopResult struct {
		TTL      uint32
		Addr     string
		Hostname string
		RTTMS    []float64
		LossPct  float64
	}
	// PathTraceResult is the proto-agnostic path-trace outcome.
	PathTraceResult struct {
		TargetResolved string
		Hops           []PathHopResult
		Reached        bool
		DurationMS     uint32
		Error          string
	}
	// traceProbeOutcome is one probe's answer (or lack of one).
	traceProbeOutcome struct {
		Answered bool
		From     string
		Reached  bool
		RTT      time.Duration
		Err      string
	}
	// PathTraceOptions configures RunPathTrace. SendProbe is injectable for
	// tests; when nil a raw-socket prober is used. Callers without raw-socket
	// privileges receive a bounded error result.
	PathTraceOptions struct {
		Target            string
		MaxHops           uint32
		QueriesPerHop     uint32
		PerProbeTimeoutMS uint32
		TotalBudgetMS     uint32
		Protocol          string

		SendProbe     func(ctx context.Context, ttl int, seq int, timeout time.Duration) traceProbeOutcome
		ReverseLookup func(ctx context.Context, addr string) string
	}
	// =====================================================================
	// Raw-socket prober (UDP and ICMP-echo modes)
	// =====================================================================

	// rawProber sends one probe at a time and matches ICMP answers to it, so
	// reply attribution only needs the probe's identifiers (UDP destination
	// port, or echo id+seq), never timing heuristics.
	rawProber struct {
		mode     string
		target   netip.Addr
		icmpConn *icmp.PacketConn
		icmpPC   *ipv4.PacketConn

		udpConn  net.PacketConn
		udpPC    *ipv4.PacketConn
		udpLocal uint16

		echoID int
	}
)

// RunPathTrace maps the forwarding path to the target with a bounded TTL
// ramp. It enforces client-side hard caps, stops early on budget
// exhaustion, on reaching the target, or after a run of fully-silent hops,
// and never returns nil slices.
func RunPathTrace(ctx context.Context, opts PathTraceOptions) PathTraceResult {
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()

	maxHops := clampUint32(opts.MaxHops, defaultTraceMaxHops, maxTraceHops)
	queries := clampUint32(opts.QueriesPerHop, defaultTraceQueriesPerHop, maxTraceQueriesPerHop)
	probeTimeout := time.Duration(clampUint32(opts.PerProbeTimeoutMS, defaultTraceProbeTimeoutMS, 5000)) * time.Millisecond
	budget := time.Duration(clampUint32(opts.TotalBudgetMS, defaultTraceBudgetMS, maxTraceBudgetMS)) * time.Millisecond

	ctx, cancel := context.WithDeadline(ctx, started.Add(budget))
	defer cancel()

	target := opts.Target
	if target == "" {
		target = "1.1.1.1"
	}
	targetIP, err := resolveTraceTarget(ctx, target)
	if err != nil {
		return PathTraceResult{
			Hops:       []PathHopResult{},
			DurationMS: uint32(time.Since(started).Milliseconds()),
			Error:      fmt.Sprintf("resolve %q: %v", target, err),
		}
	}

	sendProbe := opts.SendProbe
	if sendProbe == nil {
		prober, proberErr := newRawProber(opts.Protocol, targetIP)
		if proberErr != nil {
			return PathTraceResult{
				TargetResolved: targetIP.String(),
				Hops:           []PathHopResult{},
				DurationMS:     uint32(time.Since(started).Milliseconds()),
				Error:          fmt.Sprintf("open probe sockets: %v", proberErr),
			}
		}
		defer prober.Close()
		sendProbe = prober.Probe
	}

	result := PathTraceResult{
		TargetResolved: targetIP.String(),
		Hops:           []PathHopResult{},
	}

	seq := 0
	silentRun := 0
	budgetTripped := false

ramp:
	for ttl := 1; ttl <= int(maxHops); ttl++ {
		hop := PathHopResult{TTL: uint32(ttl), RTTMS: []float64{}}
		sent := 0
		for q := 0; q < int(queries); q++ {
			remaining := time.Until(started.Add(budget))
			if remaining <= 0 || ctx.Err() != nil {
				budgetTripped = true
				if sent > 0 {
					finalizeHop(&hop, sent)
					result.Hops = append(result.Hops, hop)
				}
				break ramp
			}
			timeout := min(remaining, probeTimeout)
			seq++
			sent++
			outcome := sendProbe(ctx, ttl, seq, timeout)
			if outcome.Err != "" {
				result.Error = outcome.Err
				finalizeHop(&hop, sent)
				result.Hops = append(result.Hops, hop)
				break ramp
			}
			if outcome.Answered {
				hop.RTTMS = append(hop.RTTMS, float64(outcome.RTT.Microseconds())/1000.0)
				if hop.Addr == "" {
					hop.Addr = outcome.From
				}
				if outcome.Reached {
					result.Reached = true
				}
			}
		}
		finalizeHop(&hop, sent)
		result.Hops = append(result.Hops, hop)

		if hop.Addr == "" {
			silentRun++
			if silentRun >= maxTraceSilentHops {
				break
			}
		} else {
			silentRun = 0
		}
		if result.Reached {
			break
		}
	}

	if result.Error == "" {
		switch {
		case result.Reached:
			// STATUS_OK path; nothing to add.
		case budgetTripped:
			result.Error = "partial: total budget exhausted before reaching the target"
		case silentRun >= maxTraceSilentHops:
			result.Error = fmt.Sprintf("partial: stopped after %d consecutive silent hops", maxTraceSilentHops)
		default:
			result.Error = fmt.Sprintf("partial: max_hops (%d) reached without answer from target", maxHops)
		}
	}

	annotateHostnames(ctx, started.Add(budget), result.Hops, opts.ReverseLookup)

	result.DurationMS = uint32(time.Since(started).Milliseconds())
	return result
}

func finalizeHop(hop *PathHopResult, sent int) {
	if sent <= 0 {
		hop.LossPct = 0
		return
	}
	lost := sent - len(hop.RTTMS)
	hop.LossPct = float64(lost) / float64(sent) * 100.0
	sort.Float64s(hop.RTTMS)
}

// annotateHostnames fills best-effort rDNS names for answered hops when
// budget remains. It never blocks the ramp itself.
func annotateHostnames(ctx context.Context, deadline time.Time, hops []PathHopResult, lookup func(context.Context, string) string) {
	if lookup == nil {
		lookup = defaultReverseLookup
	}
	for i := range hops {
		if hops[i].Addr == "" {
			continue
		}
		if time.Until(deadline) < traceRDNSTimeout {
			return
		}
		lookupCtx, cancel := context.WithTimeout(ctx, traceRDNSTimeout)
		hops[i].Hostname = lookup(lookupCtx, hops[i].Addr)
		cancel()
	}
}

func defaultReverseLookup(ctx context.Context, addr string) string {
	names, err := net.DefaultResolver.LookupAddr(ctx, addr)
	if err != nil || len(names) == 0 {
		return ""
	}
	name := names[0]
	if len(name) > 0 && name[len(name)-1] == '.' {
		name = name[:len(name)-1]
	}
	return name
}

func resolveTraceTarget(ctx context.Context, target string) (netip.Addr, error) {
	if addr, err := netip.ParseAddr(target); err == nil {
		if !addr.Is4() {
			return netip.Addr{}, fmt.Errorf("only IPv4 targets are supported")
		}
		return addr, nil
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", target)
	if err != nil {
		return netip.Addr{}, err
	}
	if len(ips) == 0 {
		return netip.Addr{}, fmt.Errorf("no IPv4 address")
	}
	return ips[0].Unmap(), nil
}

func clampUint32(value, def, limit uint32) uint32 {
	if value == 0 {
		value = def
	}
	if value > limit {
		return limit
	}
	return value
}

func newRawProber(protocol string, target netip.Addr) (*rawProber, error) {
	mode := traceProtocolUDP
	if protocol == traceProtocolICMP {
		mode = traceProtocolICMP
	}
	p := &rawProber{
		mode:   mode,
		target: target,
		echoID: os.Getpid() & 0xffff,
	}
	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return nil, fmt.Errorf("icmp listen (root required): %w", err)
	}
	p.icmpConn = conn
	p.icmpPC = conn.IPv4PacketConn()

	if mode == traceProtocolUDP {
		udpConn, err := net.ListenPacket("udp4", ":0")
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("udp listen: %w", err)
		}
		p.udpConn = udpConn
		p.udpPC = ipv4.NewPacketConn(udpConn)
		if addr, ok := udpConn.LocalAddr().(*net.UDPAddr); ok {
			p.udpLocal = uint16(addr.Port)
		}
	}
	return p, nil
}

func (p *rawProber) Close() {
	if p.icmpConn != nil {
		_ = p.icmpConn.Close()
	}
	if p.udpConn != nil {
		_ = p.udpConn.Close()
	}
}

// Probe sends one TTL-limited probe and waits up to timeout for the
// matching ICMP answer.
func (p *rawProber) Probe(ctx context.Context, ttl int, seq int, timeout time.Duration) traceProbeOutcome {
	_ = ctx
	deadline := time.Now().Add(timeout)

	var sendErr error
	dstPort := uint16(traceUDPBasePort + (seq % 1000))
	switch p.mode {
	case traceProtocolUDP:
		sendErr = p.sendUDP(ttl, dstPort)
	default:
		sendErr = p.sendEcho(ttl, seq)
	}
	sentAt := time.Now()
	if sendErr != nil {
		return traceProbeOutcome{Err: fmt.Sprintf("send ttl=%d: %v", ttl, sendErr)}
	}

	buf := make([]byte, 1500)
	for time.Now().Before(deadline) {
		if err := p.icmpConn.SetReadDeadline(deadline); err != nil {
			return traceProbeOutcome{Err: fmt.Sprintf("set read deadline: %v", err)}
		}
		n, peer, err := p.icmpConn.ReadFrom(buf)
		if err != nil {
			// Deadline: this probe goes unanswered ("* " hop).
			return traceProbeOutcome{}
		}
		matched, reached := p.matchReply(buf[:n], peer, dstPort, seq)
		if !matched {
			continue // unrelated ICMP traffic; keep reading
		}
		return traceProbeOutcome{
			Answered: true,
			From:     peerIP(peer),
			Reached:  reached,
			RTT:      time.Since(sentAt),
		}
	}
	return traceProbeOutcome{}
}

func (p *rawProber) sendUDP(ttl int, dstPort uint16) error {
	if err := p.udpPC.SetTTL(ttl); err != nil {
		return err
	}
	dst := &net.UDPAddr{IP: p.target.AsSlice(), Port: int(dstPort)}
	_, err := p.udpConn.WriteTo([]byte("axon-diag"), dst)
	return err
}

func (p *rawProber) sendEcho(ttl int, seq int) error {
	if err := p.icmpPC.SetTTL(ttl); err != nil {
		return err
	}
	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Body: &icmp.Echo{ID: p.echoID, Seq: seq & 0xffff, Data: []byte("axon-diag")},
	}
	body, err := msg.Marshal(nil)
	if err != nil {
		return err
	}
	_, err = p.icmpConn.WriteTo(body, &net.IPAddr{IP: p.target.AsSlice()})
	return err
}

// matchReply decides whether an inbound ICMP packet answers the probe
// identified by dstPort (UDP mode) or echo seq (ICMP mode).
func (p *rawProber) matchReply(packet []byte, peer net.Addr, dstPort uint16, seq int) (matched bool, reached bool) {
	msg, err := icmp.ParseMessage(1, packet)
	if err != nil {
		return false, false
	}
	switch body := msg.Body.(type) {
	case *icmp.Echo:
		// Echo reply straight from the target (ICMP mode).
		if p.mode != traceProtocolICMP {
			return false, false
		}
		if msg.Type == ipv4.ICMPTypeEchoReply && body.ID == p.echoID && body.Seq == seq&0xffff {
			return true, true
		}
		return false, false
	case *icmp.TimeExceeded:
		return p.matchInner(body.Data, dstPort, seq), false
	case *icmp.DstUnreach:
		if !p.matchInner(body.Data, dstPort, seq) {
			return false, false
		}
		// Port-unreachable from the target terminates a UDP trace.
		code := msg.Code
		return true, p.mode == traceProtocolUDP && code == 3 && peerIP(peer) == p.target.String()
	default:
		return false, false
	}
}

// matchInner inspects the quoted original datagram inside a time-exceeded /
// unreachable body: IPv4 header + at least 8 bytes of the original payload.
func (p *rawProber) matchInner(data []byte, dstPort uint16, seq int) bool {
	hdr, err := ipv4.ParseHeader(data)
	if err != nil || hdr == nil {
		return false
	}
	if !hdr.Dst.Equal(p.target.AsSlice()) {
		return false
	}
	if len(data) < hdr.Len+8 {
		return false
	}
	quoted := data[hdr.Len : hdr.Len+8]
	switch p.mode {
	case traceProtocolUDP:
		srcPort := binary.BigEndian.Uint16(quoted[0:2])
		quotedDst := binary.BigEndian.Uint16(quoted[2:4])
		return srcPort == p.udpLocal && quotedDst == dstPort
	default:
		// Quoted ICMP echo header: type(1) code(1) cksum(2) id(2) seq(2).
		if quoted[0] != 8 { // original must be an echo request
			return false
		}
		id := binary.BigEndian.Uint16(quoted[4:6])
		quotedSeq := binary.BigEndian.Uint16(quoted[6:8])
		return int(id) == p.echoID && int(quotedSeq) == seq&0xffff
	}
}

func peerIP(peer net.Addr) string {
	switch a := peer.(type) {
	case *net.IPAddr:
		return a.IP.String()
	case *net.UDPAddr:
		return a.IP.String()
	default:
		if a == nil {
			return ""
		}
		host, _, err := net.SplitHostPort(a.String())
		if err != nil {
			return a.String()
		}
		return host
	}
}

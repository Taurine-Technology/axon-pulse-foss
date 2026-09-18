package diagnostics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"
)

const pingProbeTimeout = 2 * time.Second

type platformPinger interface {
	Probe(context.Context) (time.Duration, error)
	Close() error
}

// RunPing measures round-trip latency with the operating system's native
// unprivileged ICMP facility. If ICMP is unavailable, it falls back to a
// bounded TCP handshake probe so measurements still work on locked-down
// hosts. It never invokes an external binary.
func RunPing(ctx context.Context, target string, count uint32) LatencyResult {
	if ctx == nil {
		ctx = context.Background()
	}
	if target == "" || strings.HasPrefix(target, "-") {
		return LatencyResult{Error: fmt.Sprintf("invalid ping target %q", target)}
	}

	count = clampPingCount(count)
	addr, err := resolvePingTarget(ctx, target)
	if err != nil {
		return LatencyResult{
			Target: target,
			Error:  fmt.Sprintf("resolve ping target %q: %v", target, err),
		}
	}

	pinger, err := openPlatformPinger(addr)
	if err != nil {
		pinger = &tcpPinger{addr: addr}
	}

	samples := make([]float64, 0, count)
	var sent uint32
	var lastErr error
	for range count {
		if ctx.Err() != nil {
			break
		}

		probeStarted := time.Now()
		sent++
		rtt, probeErr := pinger.Probe(ctx)
		if probeErr == nil {
			samples = append(samples, float64(rtt.Microseconds())/1000)
		} else {
			lastErr = probeErr
		}

		wait := pingInterval - time.Since(probeStarted)
		if wait <= 0 || sent == count {
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
	if closeErr := pinger.Close(); closeErr != nil && lastErr == nil {
		lastErr = closeErr
	}

	if sent == 0 {
		return LatencyResult{
			Target: target,
			Error:  fmt.Sprintf("ping canceled before first probe: %v", ctx.Err()),
		}
	}

	lost := sent - uint32(len(samples))
	lossPct := 100 * float64(lost) / float64(sent)
	if len(samples) == 0 {
		errorText := fmt.Sprintf("no ping replies from %s (%.0f%% loss)", target, lossPct)
		if ctx.Err() != nil {
			errorText = fmt.Sprintf("ping stopped: %v", ctx.Err())
		} else if lastErr != nil {
			errorText = fmt.Sprintf("%s: %v", errorText, lastErr)
		}
		return LatencyResult{
			PacketLossPct:   lossPct,
			PacketLossValid: true,
			ProbesSent:      sent,
			Target:          target,
			Error:           errorText,
			SamplesMS:       []float64{},
		}
	}

	return LatencyResult{
		MinMS:           minFloat64(samples),
		AvgMS:           averageFloat64(samples),
		MaxMS:           maxFloat64(samples),
		JitterMS:        computeJitter(samples),
		PacketLossPct:   lossPct,
		PacketLossValid: true,
		ProbesSent:      sent,
		Target:          target,
		SamplesMS:       append([]float64{}, samples...),
	}
}

func resolvePingTarget(ctx context.Context, target string) (net.IP, error) {
	if parsed := net.ParseIP(target); parsed != nil {
		return parsed, nil
	}

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, target)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, errors.New("no addresses returned")
	}
	for _, addr := range addrs {
		if addr.IP.To4() != nil {
			return addr.IP, nil
		}
	}
	return addrs[0].IP, nil
}

type tcpPinger struct {
	addr net.IP
}

func (p *tcpPinger) Probe(ctx context.Context) (time.Duration, error) {
	timeout := pingProbeTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, max(time.Until(deadline), time.Millisecond))
	}

	var lastErr error
	for _, port := range []string{"443", "53"} {
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		started := time.Now()
		conn, err := (&net.Dialer{}).DialContext(
			probeCtx,
			"tcp",
			net.JoinHostPort(p.addr.String(), port),
		)
		elapsed := time.Since(started)
		cancel()
		if err == nil {
			closeErr := conn.Close()
			if closeErr != nil {
				return 0, fmt.Errorf("closing TCP fallback probe: %w", closeErr)
			}
			return elapsed, nil
		}
		if errors.Is(err, syscall.ECONNREFUSED) {
			return elapsed, nil
		}
		lastErr = err
	}
	return 0, fmt.Errorf("TCP fallback probe failed: %w", lastErr)
}

func (p *tcpPinger) Close() error {
	return nil
}

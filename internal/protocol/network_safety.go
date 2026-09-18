package protocol

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"time"
)

var (
	nonPublicPrefixes = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("::/128"), netip.MustParsePrefix("::1/128"),
		netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("64:ff9b:1::/48"),
		netip.MustParsePrefix("2001:db8::/32"),
		netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("fe80::/10"),
		netip.MustParsePrefix("ff00::/8"),
	}
)

// IsPublicIP rejects address classes a controller must never make the sensor
// contact. net.IP.IsGlobalUnicast includes private address space, so the
// explicit exclusions are security-significant.
func IsPublicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return false
	}
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func IsSafePublicHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return false
	}
	withoutZone := host
	if value, _, ok := strings.Cut(withoutZone, "%"); ok {
		withoutZone = value
	}
	if address, err := netip.ParseAddr(withoutZone); err == nil {
		return IsPublicIP(net.IP(address.AsSlice()))
	}
	return strings.Contains(host, ".")
}

// DialPublicContext resolves the destination immediately before dialing and
// connects only to a globally routable address. This closes the DNS-rebinding
// gap left by validation performed when controller config is received.
func DialPublicContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("public target must include a port")
	}
	if !IsSafePublicHost(host) {
		return nil, errors.New("private or local network target rejected")
	}
	addresses, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	candidates := interleaveFamilies(addresses)
	dialer := &net.Dialer{}
	err = nil
	for index, candidate := range candidates {
		// Each attempt gets a share of the remaining deadline so a
		// blackholed family (commonly IPv6) cannot consume the whole budget
		// and starve the fallback family of any attempt.
		attemptCtx, cancel := shareDeadline(ctx, len(candidates)-index)
		connection, dialErr := dialer.DialContext(attemptCtx, network, net.JoinHostPort(candidate.String(), port))
		cancel()
		if dialErr == nil {
			return connection, nil
		}
		err = dialErr
		if ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	return nil, errors.New("target did not resolve to a public address")
}

// interleaveFamilies alternates IPv4 and IPv6 candidates, filtered to public
// addresses, preserving resolver order within each family.
func interleaveFamilies(addresses []net.IP) []net.IP {
	var v4, v6 []net.IP
	for _, candidate := range addresses {
		if !IsPublicIP(candidate) {
			continue
		}
		if candidate.To4() != nil {
			v4 = append(v4, candidate)
		} else {
			v6 = append(v6, candidate)
		}
	}
	result := make([]net.IP, 0, len(v4)+len(v6))
	for index := 0; index < len(v4) || index < len(v6); index++ {
		if index < len(v6) {
			result = append(result, v6[index])
		}
		if index < len(v4) {
			result = append(result, v4[index])
		}
	}
	return result
}

// shareDeadline divides the context's remaining time evenly across the
// remaining attempts, mirroring net.Dialer's per-address deadline split, with
// a two-second floor so late candidates still get a usable attempt.
func shareDeadline(ctx context.Context, remainingAttempts int) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok || remainingAttempts <= 1 {
		return context.WithCancel(ctx)
	}
	share := max(time.Until(deadline)/time.Duration(remainingAttempts), 2*time.Second)
	return context.WithTimeout(ctx, share)
}

// Package prober runs bounded continuous and periodic network measurements.
package prober

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Taurine-Technology/axon-contracts/gen/go/diagnostics"
	"github.com/Taurine-Technology/axon-pulse/internal/aggregate"
	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
)

type (
	PingFunc func(context.Context, string, uint32) diagnostics.LatencyResult

	Engine struct {
		Ping PingFunc
		Dial func(context.Context, string) (time.Duration, error)
		Now  func() time.Time
	}

	pingTarget struct {
		label      string
		address    string
		tcpAddress string
		class      protocol.TargetClass
	}
)

const (
	maxLatencyTargets = 8
)

func New() *Engine {
	return &Engine{Ping: diagnostics.RunPing, Now: time.Now}
}

// Latency probes the configured targets concurrently. Each target still has
// an independent bounded timeout in the shared diagnostics engine.
func (e *Engine) Latency(ctx context.Context, config protocol.SensorConfig, controllerURL string) []aggregate.Sample {
	targets := resolveTargets(config.ProbeTargets, controllerURL)
	results := make([]aggregate.Sample, len(targets))
	var group sync.WaitGroup
	for index, target := range targets {
		group.Go(func() {
			address := target.address
			if target.class == protocol.TargetPublicAnchor || target.class == protocol.TargetCustom {
				resolved, err := resolvePublicAddress(ctx, address)
				if err != nil {
					results[index] = aggregate.Sample{Timestamp: e.now(), Target: target.label, TargetClass: target.class, ProbeMethod: protocol.ProbeICMP, Fallback: "none"}
					return
				}
				address = resolved
				target.tcpAddress = net.JoinHostPort(resolved, "443")
			}
			result := e.ping()(ctx, address, 1)
			sample := aggregate.Sample{Timestamp: e.now(), Target: target.label, TargetClass: target.class, ProbeMethod: protocol.ProbeICMP, Success: result.Error == "" && len(result.SamplesMS) > 0, Fallback: "none"}
			if len(result.SamplesMS) > 0 {
				sample.RTTMS = result.SamplesMS[0]
			} else if result.Error == "" {
				sample.RTTMS = result.AvgMS
			}
			if !sample.Success && target.tcpAddress != "" {
				publicOnly := target.class == protocol.TargetPublicAnchor || target.class == protocol.TargetCustom
				if elapsed, err := e.dial(publicOnly)(ctx, target.tcpAddress); err == nil {
					sample.Success = true
					sample.RTTMS = float64(elapsed.Microseconds()) / 1000
					sample.ProbeMethod = protocol.ProbeTCPConnect
					sample.Fallback = "tcp_connect"
				}
			}
			results[index] = sample
		})
	}
	group.Wait()
	return results
}

func (e *Engine) DNS(ctx context.Context, config protocol.SensorConfig) []protocol.DNSCheck {
	result := diagnostics.RunDNSCheck(ctx, diagnostics.DNSCheckOptions{
		Domains: config.DNSDomains, Resolvers: config.DNSResolvers, IncludeSystemResolver: true,
		QueryTimeoutMS: 2000, TotalBudgetMS: 12000, CheckHijack: true,
	})
	timestamp := e.now().Unix()
	checks := make([]protocol.DNSCheck, 0, len(result.Results))
	for _, row := range result.Results {
		status := row.Status
		if status == "NO_ANSWER" || status == "" {
			status = "ERROR"
		}
		checks = append(checks, protocol.DNSCheck{
			Timestamp: timestamp, Resolver: row.Resolver, Domain: row.Domain,
			ColdMS: float64(row.LatencyMS), WarmMS: float64(row.WarmLatencyMS),
			Status: status, Hijack: result.Hijack.Suspected, Error: truncate(row.Error, 512),
		})
	}
	return checks
}

func (e *Engine) HTTP(ctx context.Context, config protocol.SensorConfig, controllerURL string) []protocol.HTTPCheck {
	results := []diagnostics.HTTPResult{}
	captive := false
	if probeURL := controllerProbeURL(controllerURL); probeURL != "" {
		controllerResult := diagnostics.RunHTTPCheck(ctx, diagnostics.HTTPCheckOptions{
			Targets:            []diagnostics.HTTPTarget{{URL: probeURL, Method: "HEAD", ExpectStatus: 204}},
			PerTargetTimeoutMS: 5000, TotalBudgetMS: 5000, FollowRedirects: false,
		})
		results = append(results, controllerResult.Results...)
	}
	targets := make([]diagnostics.HTTPTarget, 0, len(config.HTTPTargets))
	for _, target := range config.HTTPTargets {
		targets = append(targets, diagnostics.HTTPTarget{URL: target, Method: "HEAD"})
	}
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok && len(targets) > 0 {
		transport := defaultTransport.Clone()
		transport.Proxy = nil
		transport.DialContext = protocol.DialPublicContext
		publicResult := diagnostics.RunHTTPCheck(ctx, diagnostics.HTTPCheckOptions{
			Targets: targets, PerTargetTimeoutMS: 5000, TotalBudgetMS: 15000,
			FollowRedirects: false, CaptiveProbe: true, Transport: transport,
		})
		results = append(results, publicResult.Results...)
		captive = publicResult.CaptivePortal.Suspected
		transport.CloseIdleConnections()
	}
	timestamp := e.now().Unix()
	checks := make([]protocol.HTTPCheck, 0, len(results))
	for _, row := range results {
		checks = append(checks, protocol.HTTPCheck{
			Timestamp: timestamp, Target: row.URL, DNSMS: float64(row.DNSMS),
			ConnectMS: float64(row.ConnectMS), TLSMS: float64(row.TLSMS),
			TTFBMS: float64(row.TTFBMS), TotalMS: float64(row.TotalMS),
			Status: int(row.HTTPStatus), TLSValid: row.TLSValid,
			Captive: captive, Error: truncate(row.Error, 512), TargetClass: protocol.TargetCustom, ProbeMethod: protocol.ProbeHTTPS,
		})
	}
	return checks
}

func resolveTargets(configured []string, controllerURL string) []pingTarget {
	results := make([]pingTarget, 0, len(configured))
	seen := make(map[string]bool)
	for _, configuredTarget := range configured {
		var target pingTarget
		switch {
		case configuredTarget == "gateway":
			gateway, err := DefaultGateway()
			if err != nil || gateway == "" {
				continue
			}
			target = pingTarget{label: "gateway", address: gateway, tcpAddress: net.JoinHostPort(gateway, "80"), class: protocol.TargetGateway}
		case configuredTarget == "controller":
			parsed, err := url.Parse(controllerURL)
			if err != nil || parsed.Hostname() == "" {
				continue
			}
			port := parsed.Port()
			if port == "" {
				port = "443"
			}
			target = pingTarget{label: "controller", address: parsed.Hostname(), tcpAddress: net.JoinHostPort(parsed.Hostname(), port), class: protocol.TargetController}
		case strings.HasPrefix(configuredTarget, "anchor:"):
			address := strings.TrimPrefix(configuredTarget, "anchor:")
			if address == "" {
				continue
			}
			target = pingTarget{label: configuredTarget, address: address, tcpAddress: defaultTCPAddress(address), class: protocol.TargetPublicAnchor}
		default:
			target = pingTarget{label: configuredTarget, address: configuredTarget, tcpAddress: defaultTCPAddress(configuredTarget), class: protocol.TargetCustom}
		}
		if !seen[target.label] {
			seen[target.label] = true
			results = append(results, target)
			if len(results) == maxLatencyTargets {
				break
			}
		}
	}
	return results
}

func controllerProbeURL(controllerURL string) string {
	parsed, err := url.Parse(controllerURL)
	if err != nil || parsed.Host == "" {
		return ""
	}
	parsed.Path = protocol.ProbePath
	parsed.RawPath, parsed.RawQuery, parsed.Fragment = "", "", ""
	return parsed.String()
}

func (e *Engine) ping() PingFunc {
	if e.Ping != nil {
		return e.Ping
	}
	return diagnostics.RunPing
}

func (e *Engine) dial(publicOnly bool) func(context.Context, string) (time.Duration, error) {
	if e.Dial != nil {
		return e.Dial
	}
	return func(ctx context.Context, address string) (time.Duration, error) {
		dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		started := time.Now()
		var connection net.Conn
		var err error
		if publicOnly {
			connection, err = protocol.DialPublicContext(dialCtx, "tcp", address)
		} else {
			connection, err = (&net.Dialer{}).DialContext(dialCtx, "tcp", address)
		}
		if err != nil {
			return 0, err
		}
		_ = connection.Close()
		return time.Since(started), nil
	}
}

func defaultTCPAddress(address string) string {
	return net.JoinHostPort(address, "443")
}

func resolvePublicAddress(ctx context.Context, host string) (string, error) {
	if parsed := net.ParseIP(host); parsed != nil {
		if !protocol.IsPublicIP(parsed) {
			return "", &net.AddrError{Err: "private or local target rejected", Addr: host}
		}
		return parsed.String(), nil
	}
	addresses, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return "", err
	}
	for _, address := range addresses {
		if protocol.IsPublicIP(address) {
			return address.String(), nil
		}
	}
	return "", &net.AddrError{Err: "target did not resolve publicly", Addr: host}
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func truncate(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	return value[:maximum]
}

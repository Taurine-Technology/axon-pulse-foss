package diagnostics

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"time"

	"github.com/Taurine-Technology/axon-contracts/gen/go/buildinfo"
)

const (
	defaultHTTPTargetTimeoutMS = 5000
	defaultHTTPBudgetMS        = 15000

	// Hard caps enforced regardless of what the command asks for. The body
	// window keeps a GET's timing meaningful (real content delivery) without
	// burning metered backhaul.
	maxHTTPTargets         = 10
	maxHTTPTargetTimeoutMS = 5000
	maxHTTPBudgetMS        = 20000
	maxHTTPBodyBytes       = 64 * 1024
	maxHTTPRedirects       = 5

	defaultCaptiveURL = "http://connectivitycheck.gstatic.com/generate_204"
	captiveTimeout    = 3 * time.Second
)

var (
	// builtinHTTPTargets keeps an empty command useful: the probe still returns
	// availability signal for globally-reachable anchors.
	builtinHTTPTargets = []HTTPTarget{
		{URL: "https://www.google.com", Method: http.MethodHead},
		{URL: "https://www.cloudflare.com", Method: http.MethodHead},
	}
)

type (
	// HTTPTarget is one URL to check.
	HTTPTarget struct {
		URL          string
		Method       string
		ExpectStatus uint32
	}
	// HTTPResult is one target's outcome with phase timings.
	HTTPResult struct {
		URL           string
		OK            bool
		HTTPStatus    uint32
		DNSMS         uint32
		ConnectMS     uint32
		TLSMS         uint32
		TTFBMS        uint32
		TotalMS       uint32
		BytesRead     uint64
		TLSValid      bool
		RedirectCount uint32
		Error         string
	}
	// CaptivePortalOutcome reports the generate-204 canary result.
	CaptivePortalOutcome struct {
		Attempted bool
		Suspected bool
		Detail    string
	}
	// HTTPCheckResult is the proto-agnostic HTTP check outcome.
	HTTPCheckResult struct {
		Results       []HTTPResult
		CaptivePortal CaptivePortalOutcome
		DurationMS    uint32
	}
	// HTTPCheckOptions configures RunHTTPCheck. Transport and CaptiveURL are
	// injectable for tests.
	HTTPCheckOptions struct {
		Targets            []HTTPTarget
		PerTargetTimeoutMS uint32
		TotalBudgetMS      uint32
		FollowRedirects    bool
		CaptiveProbe       bool

		Transport  http.RoundTripper
		CaptiveURL string
	}
	requestTiming struct {
		start     time.Time
		dnsMS     uint32
		connectMS uint32
		tlsMS     uint32
		ttfbMS    uint32
		totalMS   uint32
	}
)

// RunHTTPCheck measures availability and phase timings for each target,
// sequentially — parallel fetches on a constrained uplink would distort
// each other's timings. It enforces client-side hard caps and never
// returns nil slices.
func RunHTTPCheck(ctx context.Context, opts HTTPCheckOptions) HTTPCheckResult {
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()

	perTarget := time.Duration(clampUint32(opts.PerTargetTimeoutMS, defaultHTTPTargetTimeoutMS, maxHTTPTargetTimeoutMS)) * time.Millisecond
	budget := time.Duration(clampUint32(opts.TotalBudgetMS, defaultHTTPBudgetMS, maxHTTPBudgetMS)) * time.Millisecond

	ctx, cancel := context.WithDeadline(ctx, started.Add(budget))
	defer cancel()

	targets := opts.Targets
	if len(targets) == 0 {
		targets = builtinHTTPTargets
	}
	if len(targets) > maxHTTPTargets {
		targets = targets[:maxHTTPTargets]
	}

	result := HTTPCheckResult{Results: []HTTPResult{}}
	for _, target := range targets {
		remaining := time.Until(started.Add(budget))
		if remaining <= 0 || ctx.Err() != nil {
			result.Results = append(result.Results, HTTPResult{
				URL:   target.URL,
				Error: "skipped: total budget exhausted",
			})
			continue
		}
		timeout := min(remaining, perTarget)
		result.Results = append(result.Results, checkOneTarget(ctx, target, timeout, opts.FollowRedirects, opts.Transport))
	}

	if opts.CaptiveProbe {
		result.CaptivePortal = runCaptiveProbe(ctx, opts.CaptiveURL, opts.Transport)
	}

	result.DurationMS = uint32(time.Since(started).Milliseconds())
	return result
}

// checkOneTarget fetches one URL, following up to maxHTTPRedirects manually
// so the reported phases always describe the FINAL request (a fresh
// connection per hop — connection reuse would zero the phases we are here
// to measure).
func checkOneTarget(ctx context.Context, target HTTPTarget, timeout time.Duration, follow bool, transport http.RoundTripper) HTTPResult {
	out := HTTPResult{URL: target.URL}

	method := strings.ToUpper(strings.TrimSpace(target.Method))
	if method == "" {
		method = http.MethodHead
	}
	if method != http.MethodHead && method != http.MethodGet {
		out.Error = fmt.Sprintf("unsupported method %q", method)
		return out
	}

	targetCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	currentURL := target.URL
	for {
		resp, timing, err := timedRequest(targetCtx, method, currentURL, transport)
		if err != nil {
			out.Error = err.Error()
			out.TotalMS = timing.totalMS
			return out
		}

		if follow && isRedirect(resp.StatusCode) && out.RedirectCount < maxHTTPRedirects {
			location := resp.Header.Get("Location")
			drainAndClose(resp)
			if location == "" {
				out.Error = fmt.Sprintf("redirect %d without Location", resp.StatusCode)
				return out
			}
			next, err := resolveRedirect(currentURL, location)
			if err != nil {
				out.Error = fmt.Sprintf("bad redirect target %q: %v", location, err)
				return out
			}
			currentURL = next
			out.RedirectCount++
			continue
		}

		out.HTTPStatus = uint32(resp.StatusCode)
		out.DNSMS = timing.dnsMS
		out.ConnectMS = timing.connectMS
		out.TLSMS = timing.tlsMS
		out.TTFBMS = timing.ttfbMS

		if method == http.MethodGet {
			n, _ := io.CopyN(io.Discard, resp.Body, maxHTTPBodyBytes)
			out.BytesRead = uint64(n)
		}
		drainAndClose(resp)
		out.TotalMS = timing.sinceStartMS()

		// The default transport verifies certificates, so reaching here over
		// https means the chain validated; plain http is "valid" by contract.
		out.TLSValid = true

		if target.ExpectStatus != 0 {
			out.OK = out.HTTPStatus == target.ExpectStatus
			if !out.OK {
				out.Error = fmt.Sprintf("expected status %d, got %d", target.ExpectStatus, out.HTTPStatus)
			}
		} else {
			out.OK = resp.StatusCode >= 200 && resp.StatusCode < 400
			if !out.OK {
				out.Error = fmt.Sprintf("status %d", out.HTTPStatus)
			}
		}
		return out
	}
}

func (t *requestTiming) sinceStartMS() uint32 {
	return uint32(time.Since(t.start).Milliseconds())
}

// timedRequest performs one request on a fresh connection, capturing phase
// timings via httptrace.
func timedRequest(ctx context.Context, method, rawURL string, transport http.RoundTripper) (*http.Response, *requestTiming, error) {
	timing := &requestTiming{start: time.Now()}

	var dnsStart, connStart, tlsStart time.Time
	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone: func(httptrace.DNSDoneInfo) {
			if !dnsStart.IsZero() {
				timing.dnsMS = uint32(time.Since(dnsStart).Milliseconds())
			}
		},
		ConnectStart: func(string, string) { connStart = time.Now() },
		ConnectDone: func(_, _ string, err error) {
			if err == nil && !connStart.IsZero() {
				timing.connectMS = uint32(time.Since(connStart).Milliseconds())
			}
		},
		TLSHandshakeStart: func() { tlsStart = time.Now() },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err == nil && !tlsStart.IsZero() {
				timing.tlsMS = uint32(time.Since(tlsStart).Milliseconds())
			}
		},
		GotFirstResponseByte: func() {
			timing.ttfbMS = uint32(time.Since(timing.start).Milliseconds())
		},
	}

	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), method, rawURL, nil)
	if err != nil {
		return nil, timing, err
	}
	req.Header.Set("User-Agent", buildinfo.UserAgent("network-diagnostics"))

	rt := transport
	if rt == nil {
		rt = &http.Transport{
			Proxy:             http.ProxyFromEnvironment,
			DisableKeepAlives: true,
		}
	}
	client := &http.Client{
		Transport: rt,
		// Redirects are followed manually by the caller so phases always
		// describe one concrete request.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	timing.totalMS = timing.sinceStartMS()
	if err != nil {
		return nil, timing, err
	}
	return resp, timing, nil
}

// runCaptiveProbe fetches a known generate-204 endpoint over plain HTTP.
// Any 200-with-body or off-host redirect means something on-path is
// intercepting traffic (captive portal / walled garden).
func runCaptiveProbe(ctx context.Context, captiveURL string, transport http.RoundTripper) CaptivePortalOutcome {
	if captiveURL == "" {
		captiveURL = defaultCaptiveURL
	}
	probeCtx, cancel := context.WithTimeout(ctx, captiveTimeout)
	defer cancel()

	resp, _, err := timedRequest(probeCtx, http.MethodGet, captiveURL, transport)
	if err != nil {
		// Can't reach the canary at all — that is an availability problem,
		// not portal evidence.
		return CaptivePortalOutcome{Attempted: true, Detail: fmt.Sprintf("canary unreachable: %v", err)}
	}
	defer drainAndClose(resp)

	switch {
	case resp.StatusCode == http.StatusNoContent:
		n, _ := io.CopyN(io.Discard, resp.Body, 1)
		if n > 0 {
			return CaptivePortalOutcome{Attempted: true, Suspected: true, Detail: "204 carried a body"}
		}
		return CaptivePortalOutcome{Attempted: true, Detail: "clean 204"}
	case isRedirect(resp.StatusCode):
		location := resp.Header.Get("Location")
		if sameHost(captiveURL, location) {
			return CaptivePortalOutcome{Attempted: true, Detail: fmt.Sprintf("same-host redirect to %q", location)}
		}
		return CaptivePortalOutcome{
			Attempted: true,
			Suspected: true,
			Detail:    fmt.Sprintf("redirected to %q", location),
		}
	case resp.StatusCode == http.StatusOK:
		n, _ := io.CopyN(io.Discard, resp.Body, 1)
		if n > 0 {
			return CaptivePortalOutcome{Attempted: true, Suspected: true, Detail: "expected 204, got 200 with body"}
		}
		return CaptivePortalOutcome{Attempted: true, Suspected: true, Detail: "expected 204, got 200"}
	default:
		return CaptivePortalOutcome{
			Attempted: true,
			Suspected: true,
			Detail:    fmt.Sprintf("expected 204, got %d", resp.StatusCode),
		}
	}
}

func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

func resolveRedirect(base, location string) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	next, err := baseURL.Parse(location)
	if err != nil {
		return "", err
	}
	if next.Scheme != "http" && next.Scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q", next.Scheme)
	}
	return next.String(), nil
}

func sameHost(a, b string) bool {
	ua, err := url.Parse(a)
	if err != nil {
		return false
	}
	ub, err := url.Parse(b)
	if err != nil {
		return false
	}
	return ub.Host == "" || ua.Hostname() == ub.Hostname()
}

func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)
	_ = resp.Body.Close()
}

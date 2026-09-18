package diagnostics

import (
	"context"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestDNSWireBuildParse(t *testing.T) {
	t.Run("builds single question query", func(t *testing.T) {
		query, err := buildDNSQuery("example.com", 0x1234, dnsmessage.TypeA)
		if err != nil {
			t.Fatalf("buildDNSQuery: %v", err)
		}

		var p dnsmessage.Parser
		hdr, err := p.Start(query)
		if err != nil {
			t.Fatalf("parse query: %v", err)
		}
		if hdr.ID != 0x1234 || hdr.Response || !hdr.RecursionDesired {
			t.Fatalf("header = %+v", hdr)
		}
		q, err := p.Question()
		if err != nil {
			t.Fatalf("question: %v", err)
		}
		if q.Name.String() != "example.com." || q.Type != dnsmessage.TypeA || q.Class != dnsmessage.ClassINET {
			t.Fatalf("question = %+v", q)
		}
	})

	t.Run("parses compressed A and AAAA answers", func(t *testing.T) {
		response := compressedDNSResponse()
		rcode, ips, truncated, err := parseDNSAnswer(response, 0x1234)
		if err != nil {
			t.Fatalf("parseDNSAnswer: %v", err)
		}
		want := []string{"93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946"}
		if rcode != 0 || truncated || !slices.Equal(ips, want) {
			t.Fatalf("got rcode=%d ips=%v truncated=%v, want rcode=0 ips=%v truncated=false",
				rcode, ips, truncated, want)
		}
	})
}

func TestDNSStatusFromRCode(t *testing.T) {
	tests := []struct {
		name  string
		rcode int
		want  string
	}{
		{name: "success", rcode: 0, want: dnsStatusOK},
		{name: "servfail", rcode: 2, want: dnsStatusSERVFAIL},
		{name: "nxdomain", rcode: 3, want: dnsStatusNXDOMAIN},
		{name: "refused", rcode: 5, want: dnsStatusREFUSED},
		{name: "other", rcode: 9, want: dnsStatusERROR},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dnsStatusFromRCode(tt.rcode); got != tt.want {
				t.Fatalf("dnsStatusFromRCode(%d) = %q, want %q", tt.rcode, got, tt.want)
			}
		})
	}
}

func TestRunDNSCheckNormalizesCapsAndWarms(t *testing.T) {
	var callsMu sync.Mutex
	var resolverCalls []string
	var systemCalls []string

	result := RunDNSCheck(context.Background(), DNSCheckOptions{
		Domains: []string{
			" Example.COM ", "", "cloudflare.com", "wikipedia.org", "iana.org",
			"ietf.org", "golang.org", "github.com", "kernel.org", "gnu.org",
			"debian.org", "ubuntu.com", "freebsd.org", "openbsd.org", "netbsd.org",
			"example.net", "example.org", "example.edu", "example.io", "example.dev",
			"example.test",
			"over-cap.invalid",
		},
		Resolvers:             []string{"1.1.1.1", "8.8.8.8", "9.9.9.9", "208.67.222.222", "over-cap"},
		IncludeSystemResolver: true,
		QueryTimeoutMS:        25,
		TotalBudgetMS:         500,
		CheckHijack:           true,
		QueryResolver: func(ctx context.Context, resolver, domain string, timeout time.Duration) DNSLookupResult {
			_ = ctx
			_ = timeout
			callsMu.Lock()
			resolverCalls = append(resolverCalls, resolver+" "+domain)
			callsMu.Unlock()
			if len(domain) >= len("axon-canary-") && domain[:len("axon-canary-")] == "axon-canary-" {
				return DNSLookupResult{Status: dnsStatusNXDOMAIN}
			}
			return DNSLookupResult{
				Status:    dnsStatusOK,
				IPs:       []string{netip.MustParseAddr("203.0.113.10").String()},
				LatencyMS: 7,
			}
		},
		SystemLookup: func(ctx context.Context, domain string, timeout time.Duration) DNSLookupResult {
			_ = ctx
			_ = timeout
			callsMu.Lock()
			systemCalls = append(systemCalls, domain)
			callsMu.Unlock()
			if len(domain) >= len("axon-canary-") && domain[:len("axon-canary-")] == "axon-canary-" {
				return DNSLookupResult{Status: dnsStatusNXDOMAIN}
			}
			return DNSLookupResult{
				Status:    dnsStatusOK,
				IPs:       []string{netip.MustParseAddr("2001:db8::10").String()},
				LatencyMS: 5,
			}
		},
		ReadSystemResolvers: func() []string {
			return []string{"192.0.2.53"}
		},
	})

	const wantDomains = maxDNSDomains
	const wantLanes = maxDNSResolvers + 1
	if got := len(result.Results); got != wantDomains*wantLanes {
		t.Fatalf("results = %d, want %d", got, wantDomains*wantLanes)
	}
	if result.Results[0].Domain != "example.com" || result.Results[0].Resolver != "system" {
		t.Fatalf("first result = %+v", result.Results[0])
	}
	if result.Results[0].WarmLatencyMS == 0 {
		t.Fatalf("warm latency was not populated for OK cold result: %+v", result.Results[0])
	}
	if slices.ContainsFunc(result.Results, func(r DNSQueryResult) bool { return r.Domain == "over-cap.invalid" }) {
		t.Fatal("domain cap was not enforced")
	}
	if slices.ContainsFunc(result.Results, func(r DNSQueryResult) bool { return r.Resolver == "over-cap" }) {
		t.Fatal("resolver cap was not enforced")
	}
	if !slices.Equal(result.SystemResolvers, []string{"192.0.2.53"}) {
		t.Fatalf("system resolvers = %v", result.SystemResolvers)
	}
	if !result.Hijack.Attempted || result.Hijack.Suspected || result.Hijack.Detail == "" {
		t.Fatalf("hijack = %+v", result.Hijack)
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	if len(systemCalls) == 0 || len(resolverCalls) == 0 {
		t.Fatalf("systemCalls=%v resolverCalls=%v", systemCalls, resolverCalls)
	}
}

func compressedDNSResponse() []byte {
	v6 := netip.MustParseAddr("2606:2800:220:1:248:1893:25c8:1946").As16()
	return append([]byte{
		0x12, 0x34, // ID
		0x81, 0x80, // response, RD, RA, no error
		0x00, 0x01, // questions
		0x00, 0x02, // answers
		0x00, 0x00, // authority
		0x00, 0x00, // additional
		0x07, 'e', 'x', 'a', 'm', 'p', 'l', 'e',
		0x03, 'c', 'o', 'm',
		0x00,
		0x00, 0x01, // A
		0x00, 0x01, // IN
		0xc0, 0x0c, // compressed name pointer to question name
		0x00, 0x01, // A
		0x00, 0x01, // IN
		0x00, 0x00, 0x00, 0x3c, // TTL
		0x00, 0x04, // RDLENGTH
		93, 184, 216, 34,
		0xc0, 0x0c, // compressed name pointer to question name
		0x00, 0x1c, // AAAA
		0x00, 0x01, // IN
		0x00, 0x00, 0x00, 0x3c, // TTL
		0x00, 0x10, // RDLENGTH
	}, v6[:]...)
}

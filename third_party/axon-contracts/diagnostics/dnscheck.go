package diagnostics

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/idna"
)

const (
	defaultDNSQueryTimeoutMS = 2000
	defaultDNSTotalBudgetMS  = 12000

	maxDNSDomains      = 20
	maxDNSResolvers    = 4
	maxDNSIPsPerResult = 8
	dnsPoolWorkers     = 4

	dnsStatusOK       = "OK"
	dnsStatusNXDOMAIN = "NXDOMAIN"
	dnsStatusSERVFAIL = "SERVFAIL"
	dnsStatusREFUSED  = "REFUSED"
	dnsStatusTIMEOUT  = "TIMEOUT"
	dnsStatusNOANSWER = "NO_ANSWER"
	dnsStatusERROR    = "ERROR"

	resolvConfPath = "/etc/resolv.conf"
)

var (
	fallbackDNSDomains = []string{"google.com", "cloudflare.com", "wikipedia.org"}
)

type (
	// DNSLookupResult is the outcome of one cold or warm lookup operation.
	DNSLookupResult struct {
		Status    string
		IPs       []string
		LatencyMS uint32
		Error     string
	}
	// DNSQueryResult is one (domain, resolver) cold+warm DNS probe row.
	DNSQueryResult struct {
		Domain        string
		Resolver      string
		Status        string
		ResolvedIPs   []string
		LatencyMS     uint32
		WarmLatencyMS uint32
		Error         string
	}
	// DNSHijackResult reports the random-nonce NXDOMAIN canary outcome.
	DNSHijackResult struct {
		Attempted bool
		Suspected bool
		Detail    string
	}
	// DNSCheckResult is the proto-agnostic DNS diagnostic result.
	DNSCheckResult struct {
		Results         []DNSQueryResult
		SystemResolvers []string
		Hijack          DNSHijackResult
		DurationMS      uint32
	}
	// DNSCheckOptions configures RunDNSCheck.
	DNSCheckOptions struct {
		Domains               []string
		Resolvers             []string
		IncludeSystemResolver bool
		QueryTimeoutMS        uint32
		TotalBudgetMS         uint32
		CheckHijack           bool

		QueryResolver       func(context.Context, string, string, time.Duration) DNSLookupResult
		SystemLookup        func(context.Context, string, time.Duration) DNSLookupResult
		ReadSystemResolvers func() []string
	}
	dnsLane struct {
		label string
		query func(context.Context, string, time.Duration) DNSLookupResult
	}
	dnsTask struct {
		kind     string
		index    int
		label    string
		domain   string
		resolver string
		run      func(context.Context) dnsTaskResult
	}
	dnsTaskResult struct {
		kind    string
		index   int
		label   string
		row     DNSQueryResult
		outcome DNSLookupResult
	}
)

// RunDNSCheck runs the DNS diagnostic. It caps fan-out, enforces a total wall
// budget, and never returns nil slices for result collections.
func RunDNSCheck(ctx context.Context, opts DNSCheckOptions) DNSCheckResult {
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()
	queryTimeout := time.Duration(defaultDNSQueryTimeoutMS) * time.Millisecond
	if opts.QueryTimeoutMS != 0 {
		queryTimeout = time.Duration(opts.QueryTimeoutMS) * time.Millisecond
	}
	totalBudget := time.Duration(defaultDNSTotalBudgetMS) * time.Millisecond
	if opts.TotalBudgetMS != 0 {
		totalBudget = time.Duration(opts.TotalBudgetMS) * time.Millisecond
	}

	checkCtx, cancel := context.WithTimeout(ctx, totalBudget)
	defer cancel()

	domains := normalizeDNSDomains(opts.Domains)
	resolvers := normalizeDNSResolvers(opts.Resolvers)
	queryResolver := opts.QueryResolver
	if queryResolver == nil {
		queryResolver = queryResolverUDP
	}
	systemLookup := opts.SystemLookup
	if systemLookup == nil {
		systemLookup = lookupSystemResolver
	}
	readResolvers := opts.ReadSystemResolvers
	if readResolvers == nil {
		readResolvers = readSystemResolvers
	}

	lanes := make([]dnsLane, 0, 1+len(resolvers))
	if opts.IncludeSystemResolver {
		lanes = append(lanes, dnsLane{
			label: "system",
			query: func(ctx context.Context, domain string, timeout time.Duration) DNSLookupResult {
				return systemLookup(ctx, domain, timeout)
			},
		})
	}
	for _, resolver := range resolvers {
		resolverIP := resolver
		lanes = append(lanes, dnsLane{
			label: resolverIP,
			query: func(ctx context.Context, domain string, timeout time.Duration) DNSLookupResult {
				return queryResolver(ctx, resolverIP, domain, timeout)
			},
		})
	}

	results := make([]DNSQueryResult, 0, len(domains)*len(lanes))
	tasks := make([]dnsTask, 0, len(domains)*len(lanes)+len(lanes))
	for _, domain := range domains {
		for _, lane := range lanes {
			index := len(results)
			results = append(results, DNSQueryResult{
				Domain:      domain,
				Resolver:    lane.label,
				ResolvedIPs: []string{},
			})
			tasks = append(tasks, dnsTask{
				kind:     "query",
				index:    index,
				domain:   domain,
				resolver: lane.label,
				run: func(ctx context.Context) dnsTaskResult {
					return dnsTaskResult{
						kind:  "query",
						index: index,
						row:   probeDNS(ctx, domain, lane.label, lane.query, queryTimeout),
					}
				},
			})
		}
	}

	hijack := DNSHijackResult{}
	if opts.CheckHijack && len(lanes) > 0 {
		hijack.Attempted = true
		nonce := fmt.Sprintf("axon-canary-%012x.example.com", randomUint64()&0xffffffffffff)
		for _, lane := range lanes {
			tasks = append(tasks, dnsTask{
				kind:  "hijack",
				label: lane.label,
				run: func(ctx context.Context) dnsTaskResult {
					return dnsTaskResult{
						kind:    "hijack",
						label:   lane.label,
						outcome: lane.query(ctx, nonce, queryTimeout),
					}
				},
			})
		}
	}

	completed := make([]bool, len(results))
	rewriting := runDNSTasks(checkCtx, tasks, results, completed, totalBudget)

	if hijack.Attempted {
		if len(rewriting) > 0 {
			hijack.Suspected = true
			hijack.Detail = "nonce domain unexpectedly resolved via: " + strings.Join(rewriting, "; ")
		} else {
			hijack.Detail = "nonce domain correctly returned no answer"
		}
	}

	return DNSCheckResult{
		Results:         results,
		SystemResolvers: readResolvers(),
		Hijack:          hijack,
		DurationMS:      durationMS(time.Since(started)),
	}
}

func runDNSTasks(
	ctx context.Context,
	tasks []dnsTask,
	results []DNSQueryResult,
	completed []bool,
	totalBudget time.Duration,
) []string {
	if len(tasks) == 0 {
		return []string{}
	}

	taskCh := make(chan dnsTask, len(tasks))
	resultCh := make(chan dnsTaskResult, len(tasks))
	for range dnsPoolWorkers {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case task, ok := <-taskCh:
					if !ok {
						return
					}
					result := runDNSTaskSafely(ctx, task)
					select {
					case resultCh <- result:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	for _, task := range tasks {
		taskCh <- task
	}
	close(taskCh)

	rewriting := make([]string, 0)
	for range tasks {
		select {
		case result := <-resultCh:
			switch result.kind {
			case "query":
				if result.index >= 0 && result.index < len(results) {
					results[result.index] = result.row
					completed[result.index] = true
				}
			case "hijack":
				if result.outcome.Status == dnsStatusOK && len(result.outcome.IPs) > 0 {
					ips := result.outcome.IPs
					if len(ips) > 2 {
						ips = ips[:2]
					}
					rewriting = append(rewriting, result.label+" -> "+strings.Join(ips, ", "))
				}
			}
		case <-ctx.Done():
			for i := range results {
				if !completed[i] {
					results[i].Status = dnsStatusTIMEOUT
					results[i].ResolvedIPs = []string{}
					results[i].LatencyMS = durationMS(totalBudget)
					results[i].WarmLatencyMS = 0
					results[i].Error = "total budget exhausted before probe completed"
				}
			}
			return rewriting
		}
	}
	return rewriting
}

func runDNSTaskSafely(ctx context.Context, task dnsTask) (result dnsTaskResult) {
	result = dnsTaskResult{
		kind:  task.kind,
		index: task.index,
		label: task.label,
	}
	defer func() {
		if r := recover(); r != nil && task.kind == "query" {
			result.row = DNSQueryResult{
				Domain:      task.domain,
				Resolver:    task.resolver,
				Status:      dnsStatusERROR,
				ResolvedIPs: []string{},
				Error:       fmt.Sprintf("probe raised: %v", r),
			}
		}
	}()
	return task.run(ctx)
}

func normalizeDNSDomains(in []string) []string {
	out := make([]string, 0, min(len(in), maxDNSDomains))
	for _, domain := range in {
		domain = strings.ToLower(strings.TrimSpace(domain))
		if domain == "" {
			continue
		}
		out = append(out, domain)
		if len(out) == maxDNSDomains {
			break
		}
	}
	if len(out) == 0 {
		out = append(out, fallbackDNSDomains...)
	}
	return out
}

func normalizeDNSResolvers(in []string) []string {
	out := make([]string, 0, min(len(in), maxDNSResolvers))
	for _, resolver := range in {
		resolver = strings.TrimSpace(resolver)
		if resolver == "" {
			continue
		}
		out = append(out, resolver)
		if len(out) == maxDNSResolvers {
			break
		}
	}
	return out
}

func probeDNS(
	ctx context.Context,
	domain string,
	resolverLabel string,
	query func(context.Context, string, time.Duration) DNSLookupResult,
	timeout time.Duration,
) DNSQueryResult {
	cold := query(ctx, domain, timeout)
	row := DNSQueryResult{
		Domain:        domain,
		Resolver:      resolverLabel,
		Status:        cold.Status,
		ResolvedIPs:   capStrings(cold.IPs, maxDNSIPsPerResult),
		LatencyMS:     cold.LatencyMS,
		WarmLatencyMS: 0,
		Error:         cold.Error,
	}
	if row.ResolvedIPs == nil {
		row.ResolvedIPs = []string{}
	}
	if cold.Status != dnsStatusOK {
		return row
	}
	warm := query(ctx, domain, timeout)
	if warm.Status == dnsStatusOK {
		row.WarmLatencyMS = warm.LatencyMS
	}
	return row
}

func queryResolverUDP(ctx context.Context, resolver, domain string, timeout time.Duration) DNSLookupResult {
	started := time.Now()
	qid := uint16(randomUint64())
	query, err := buildDNSQuery(domain, qid, dnsmessage.TypeA)
	if err != nil {
		return DNSLookupResult{
			Status:    dnsStatusERROR,
			IPs:       []string{},
			LatencyMS: 0,
			Error:     "bad domain: " + err.Error(),
		}
	}

	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	dialCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "udp", net.JoinHostPort(resolver, "53"))
	if err != nil {
		if errors.Is(dialCtx.Err(), context.DeadlineExceeded) {
			return dnsTimeoutResult(started, timeout)
		}
		return DNSLookupResult{
			Status:    dnsStatusERROR,
			IPs:       []string{},
			LatencyMS: durationMS(time.Since(started)),
			Error:     err.Error(),
		}
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(deadline)

	if _, err := conn.Write(query); err != nil {
		if isTimeout(err) || errors.Is(dialCtx.Err(), context.DeadlineExceeded) {
			return dnsTimeoutResult(started, timeout)
		}
		return DNSLookupResult{
			Status:    dnsStatusERROR,
			IPs:       []string{},
			LatencyMS: durationMS(time.Since(started)),
			Error:     err.Error(),
		}
	}

	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if isTimeout(err) || errors.Is(dialCtx.Err(), context.DeadlineExceeded) {
				return dnsTimeoutResult(started, timeout)
			}
			return DNSLookupResult{
				Status:    dnsStatusERROR,
				IPs:       []string{},
				LatencyMS: durationMS(time.Since(started)),
				Error:     err.Error(),
			}
		}
		rcode, ips, _, err := parseDNSAnswer(buf[:n], qid)
		if err != nil {
			continue
		}
		latency := durationMS(time.Since(started))
		if rcode == 0 {
			if len(ips) == 0 {
				return DNSLookupResult{
					Status:    dnsStatusNOANSWER,
					IPs:       []string{},
					LatencyMS: latency,
					Error:     "NOERROR but no A/AAAA records",
				}
			}
			return DNSLookupResult{
				Status:    dnsStatusOK,
				IPs:       capStrings(ips, maxDNSIPsPerResult),
				LatencyMS: latency,
			}
		}
		status := dnsStatusFromRCode(rcode)
		errText := ""
		if status == dnsStatusERROR {
			errText = fmt.Sprintf("rcode=%d", rcode)
		}
		return DNSLookupResult{
			Status:    status,
			IPs:       []string{},
			LatencyMS: latency,
			Error:     errText,
		}
	}
}

func buildDNSQuery(domain string, qid uint16, qtype dnsmessage.Type) ([]byte, error) {
	name, err := dnsMessageName(domain)
	if err != nil {
		return nil, err
	}
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:               qid,
			RecursionDesired: true,
		},
		Questions: []dnsmessage.Question{
			{
				Name:  name,
				Type:  qtype,
				Class: dnsmessage.ClassINET,
			},
		},
	}
	return msg.Pack()
}

func dnsMessageName(domain string) (dnsmessage.Name, error) {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return dnsmessage.Name{}, fmt.Errorf("empty domain")
	}
	ascii, err := idna.Lookup.ToASCII(strings.TrimSuffix(domain, "."))
	if err != nil {
		return dnsmessage.Name{}, err
	}
	if ascii == "" {
		return dnsmessage.Name{}, fmt.Errorf("empty domain")
	}
	return dnsmessage.NewName(ascii + ".")
}

func parseDNSAnswer(data []byte, expectedQID uint16) (int, []string, bool, error) {
	var p dnsmessage.Parser
	hdr, err := p.Start(data)
	if err != nil {
		return 0, nil, false, err
	}
	if hdr.ID != expectedQID {
		return 0, nil, false, fmt.Errorf("qid mismatch")
	}
	if !hdr.Response {
		return 0, nil, false, fmt.Errorf("not a response")
	}
	if err := p.SkipAllQuestions(); err != nil {
		return 0, nil, false, err
	}

	ips := make([]string, 0)
	for {
		ah, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return 0, nil, false, err
		}
		switch ah.Type {
		case dnsmessage.TypeA:
			res, err := p.AResource()
			if err != nil {
				return 0, nil, false, err
			}
			ips = append(ips, netip.AddrFrom4(res.A).String())
		case dnsmessage.TypeAAAA:
			res, err := p.AAAAResource()
			if err != nil {
				return 0, nil, false, err
			}
			ips = append(ips, netip.AddrFrom16(res.AAAA).String())
		default:
			if err := p.SkipAnswer(); err != nil {
				return 0, nil, false, err
			}
		}
	}
	return int(hdr.RCode), ips, hdr.Truncated, nil
}

func dnsStatusFromRCode(rcode int) string {
	switch dnsmessage.RCode(rcode) {
	case dnsmessage.RCodeSuccess:
		return dnsStatusOK
	case dnsmessage.RCodeServerFailure:
		return dnsStatusSERVFAIL
	case dnsmessage.RCodeNameError:
		return dnsStatusNXDOMAIN
	case dnsmessage.RCodeRefused:
		return dnsStatusREFUSED
	default:
		return dnsStatusERROR
	}
}

func lookupSystemResolver(ctx context.Context, domain string, timeout time.Duration) DNSLookupResult {
	started := time.Now()
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ips, err := net.DefaultResolver.LookupHost(lookupCtx, domain)
	latency := durationMS(time.Since(started))
	if err != nil {
		if errors.Is(lookupCtx.Err(), context.DeadlineExceeded) {
			return dnsTimeoutResult(started, timeout)
		}
		var dnsErr *net.DNSError
		status := dnsStatusERROR
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			status = dnsStatusNXDOMAIN
		}
		return DNSLookupResult{
			Status:    status,
			IPs:       []string{},
			LatencyMS: latency,
			Error:     err.Error(),
		}
	}

	unique := make([]string, 0, min(len(ips), maxDNSIPsPerResult))
	seen := make(map[string]struct{}, len(ips))
	for _, ip := range ips {
		if _, ok := seen[ip]; ok {
			continue
		}
		seen[ip] = struct{}{}
		unique = append(unique, ip)
		if len(unique) == maxDNSIPsPerResult {
			break
		}
	}
	if len(unique) == 0 {
		return DNSLookupResult{
			Status:    dnsStatusNOANSWER,
			IPs:       []string{},
			LatencyMS: latency,
			Error:     "no addresses returned",
		}
	}
	return DNSLookupResult{
		Status:    dnsStatusOK,
		IPs:       unique,
		LatencyMS: latency,
	}
}

func readSystemResolvers() []string {
	return readSystemResolversFile(resolvConfPath)
}

func readSystemResolversFile(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return []string{}
	}
	defer func() { _ = f.Close() }() // read-only; close error is not meaningful

	resolvers := make([]string, 0)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "nameserver" {
			resolvers = append(resolvers, fields[1])
		}
	}
	return resolvers
}

func dnsTimeoutResult(started time.Time, timeout time.Duration) DNSLookupResult {
	return DNSLookupResult{
		Status:    dnsStatusTIMEOUT,
		IPs:       []string{},
		LatencyMS: durationMS(time.Since(started)),
		Error:     fmt.Sprintf("no response within %dms", timeout.Milliseconds()),
	}
}

func randomUint64() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return binary.BigEndian.Uint64(b[:])
	}
	return uint64(time.Now().UnixNano())
}

func capStrings(in []string, limit int) []string {
	if len(in) == 0 {
		return []string{}
	}
	if len(in) > limit {
		in = in[:limit]
	}
	return append([]string(nil), in...)
}

func durationMS(d time.Duration) uint32 {
	if d <= 0 {
		return 0
	}
	ms := d.Milliseconds()
	if ms > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(ms)
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

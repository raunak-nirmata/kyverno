package apicall

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/kyverno/kyverno/pkg/toggle"
	"k8s.io/klog/v2"
)

// This file implements SSRF protection for `context.apiCall.service` requests
// (GHSA-qr4g-8hrp-c4rw / GO-2026-5594 and GHSA-459x-q9hg-4gpq / GO-2025-3615).
//
// The guard is two-phase, mirroring the design used by github.com/kyverno/sdk for the
// CEL http library:
//
//  1. Pre-flight, in buildHTTPRequest: scheme check, hostname blocklist, allowlist.
//  2. Dial time, in the client's DialContext: every resolved IP is checked against the
//     blocked CIDRs and the connection is then made to that already-resolved IP.
//
// Phase 2 is the load-bearing one. Because it sits in the transport it also covers
// every redirect hop, and because it dials the IP it already validated it closes the
// DNS-rebinding window between check and connect.
//
// It is applied at the executor rather than at apiCall.Fetch() so that
// GlobalContextEntry — which calls NewExecutor directly in
// pkg/globalcontext/externalapi/entry.go and would otherwise bypass any check placed
// higher up — is covered too.

const dialTimeout = 30 * time.Second

// lookupHost resolves a hostname. It is a variable so that tests can force a specific
// answer order; resolution of a name like "localhost" is not deterministic across
// platforms, which would otherwise make the dial-fallback behaviour untestable.
var lookupHost = net.DefaultResolver.LookupHost

// serviceCallFilter is the parsed, validated form of the blocklist/allowlist flags.
type serviceCallFilter struct {
	blockedHosts map[string]struct{}
	blockedCIDRs []*net.IPNet
	allowed      []*url.URL
}

// newServiceCallFilter parses the current flag values. Malformed entries are a hard
// error so that a typo in a CIDR fails loudly instead of silently permitting traffic.
func newServiceCallFilter() (*serviceCallFilter, error) {
	f := &serviceCallFilter{blockedHosts: map[string]struct{}{}}

	for _, entry := range toggle.APICallBlocklist.Values() {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			_, ipNet, err := net.ParseCIDR(entry)
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR %q in --%s: %w", entry, toggle.APICallBlocklistFlagName, err)
			}
			f.blockedCIDRs = append(f.blockedCIDRs, ipNet)
			continue
		}
		f.blockedHosts[normalizeHost(entry)] = struct{}{}
	}

	for _, entry := range toggle.APICallAllowlist.Values() {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		u, err := url.Parse(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid URL %q in --%s: %w", entry, toggle.APICallAllowlistFlagName, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("entry %q in --%s must include a scheme and host", entry, toggle.APICallAllowlistFlagName)
		}
		f.allowed = append(f.allowed, u)
	}

	return f, nil
}

// enabled reports whether any filtering is configured. When everything is cleared the
// guard is a no-op, which is the documented opt-out.
func (f *serviceCallFilter) enabled() bool {
	return len(f.blockedHosts) > 0 || len(f.blockedCIDRs) > 0 || len(f.allowed) > 0
}

// validateURL is phase 1. It rejects non-HTTP schemes, hostnames on the blocklist, and
// anything outside the allowlist when one is configured.
//
// A hostname that merely resolves into a blocked range passes here and is caught at
// dial time instead; that is by design, since resolving during validation would leave a
// rebinding window.
func (f *serviceCallFilter) validateURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid service URL: %w", err)
	}
	// Errors below reach the user through the admission denial, so they name the redacted
	// URL rather than the raw one: a service URL may carry credentials in its userinfo.
	safeURL := u.Redacted()

	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("service URL %q is not permitted: scheme must be http or https", safeURL)
	}
	if u.Host == "" {
		return fmt.Errorf("service URL %q is not permitted: must include a host", safeURL)
	}

	host := u.Hostname()
	if _, blocked := f.blockedHosts[normalizeHost(host)]; blocked {
		return fmt.Errorf("service URL %q is blocked: hostname %q is on the --%s", safeURL, host, toggle.APICallBlocklistFlagName)
	}

	// A literal IP in the URL can be rejected up front with a better message than the
	// dial-time failure would give.
	if ip := net.ParseIP(host); ip != nil {
		if cidr := f.blockedCIDR(ip); cidr != nil {
			return fmt.Errorf("service URL %q is blocked: address %s is in blocked range %s", safeURL, ip, cidr)
		}
	}

	if len(f.allowed) > 0 && !f.matchesAllowlist(u) {
		return fmt.Errorf("service URL %q is not permitted: no matching entry in --%s", safeURL, toggle.APICallAllowlistFlagName)
	}

	return nil
}

// blockedCIDR returns the range containing ip, or nil.
//
// net.IPNet.Contains normalizes IPv4-mapped IPv6 addresses itself, so an IPv4 CIDR such
// as 169.254.0.0/16 correctly contains ::ffff:169.254.169.254. That is the bypass
// reported upstream as kyverno/kyverno#16419, and it is closed here by listing the
// range as a CIDR at all — a bare hostname entry would not have caught it.
//
// Two other IPv6 encodings embed an IPv4 address without being recognised by To4, so the
// embedded address is checked as well.
func (f *serviceCallFilter) blockedCIDR(ip net.IP) *net.IPNet {
	candidates := [2]net.IP{ip, embeddedIPv4(ip)}
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		for _, cidr := range f.blockedCIDRs {
			if cidr.Contains(candidate) {
				return cidr
			}
		}
	}
	return nil
}

// embeddedIPv4 extracts the IPv4 address carried by an IPv6 address in a form that
// net.IP.To4 does not recognise: IPv4-compatible (::a.b.c.d), the NAT64 well-known prefix
// (64:ff9b::a.b.c.d), and 6to4 (2002:aabb:ccdd::). Returns nil when there is no embedded
// address, or when To4 already handles it.
//
// Each of these is a distinct spelling of an address that an IPv4 CIDR in the blocklist is
// meant to cover; without normalization, 2002:a9fe:a9fe:: reaches the metadata service
// even though 169.254.0.0/16 is blocked.
func embeddedIPv4(ip net.IP) net.IP {
	if ip.To4() != nil || len(ip.To16()) != net.IPv6len {
		return nil
	}
	ip = ip.To16()

	isZero := func(b []byte) bool {
		for _, c := range b {
			if c != 0 {
				return false
			}
		}
		return true
	}

	// ::a.b.c.d — IPv4-compatible IPv6 (deprecated, but still routable in some stacks).
	if isZero(ip[:12]) {
		return net.IPv4(ip[12], ip[13], ip[14], ip[15])
	}
	// 64:ff9b::a.b.c.d — NAT64 well-known prefix.
	if ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b && isZero(ip[4:12]) {
		return net.IPv4(ip[12], ip[13], ip[14], ip[15])
	}
	// 2002:aabb:ccdd::/48 — 6to4, which carries the IPv4 address in bytes 2-5 rather than
	// at the end. 2002:7f00:1:: is 127.0.0.1 and 2002:a9fe:a9fe:: is 169.254.169.254.
	if ip[0] == 0x20 && ip[1] == 0x02 {
		return net.IPv4(ip[2], ip[3], ip[4], ip[5])
	}
	return nil
}

// validateHost applies the hostname blocklist and the CIDR blocklist to a host, resolving
// it when it is not already a literal address.
//
// This duplicates what dialContext does, and exists for the proxied case where
// dialContext only ever sees the proxy's address. See proxy.
func (f *serviceCallFilter) validateHost(ctx context.Context, host string) error {
	if _, blocked := f.blockedHosts[normalizeHost(host)]; blocked {
		return fmt.Errorf("hostname %q is on the --%s", host, toggle.APICallBlocklistFlagName)
	}
	if ip := net.ParseIP(host); ip != nil {
		if cidr := f.blockedCIDR(ip); cidr != nil {
			return fmt.Errorf("address %s is in blocked range %s", ip, cidr)
		}
		return nil
	}
	ips, err := lookupHost(ctx, host)
	if err != nil {
		// Only reachable on the proxy path. A cluster that egresses through a proxy often
		// cannot resolve external names itself — the proxy does that — so refusing every
		// unresolvable name would break calls that work today. But silently allowing them
		// leaves the CIDR check unenforced for exactly the hosts we cannot see.
		//
		// The allowlist settles it. When one is configured, validateURL has already
		// required this URL to match an entry an operator wrote by hand, so the
		// destination is explicitly sanctioned and an in-pod resolution failure says
		// nothing about it. Proceed.
		//
		// With no allowlist, an unresolvable name is unvetted by anything except the
		// hostname blocklist, and the CIDR half of the filter is the load-bearing half.
		// Refuse, and point the operator at the way to opt the host back in.
		if len(f.allowed) > 0 {
			klog.V(2).Infof("apiCall SSRF filter: proxied target %q is not resolvable in-pod; permitted by --%s", host, toggle.APICallAllowlistFlagName)
			return nil
		}
		return fmt.Errorf("%q cannot be resolved to verify it is not in a blocked range; add it to --%s to permit it", host, toggle.APICallAllowlistFlagName)
	}
	for _, s := range ips {
		ip := net.ParseIP(s)
		if ip == nil {
			continue
		}
		if cidr := f.blockedCIDR(ip); cidr != nil {
			// The resolved address is deliberately not named: echoing it back would turn
			// admission denials into an internal DNS resolution oracle.
			return fmt.Errorf("%s resolves into blocked range %s", host, cidr)
		}
	}
	return nil
}

// proxy wraps the transport's proxy resolver.
//
// When a proxy is in use, Go dials the PROXY, so Transport.DialContext is handed the
// proxy's address and never sees the real target — phase 2 would be bypassed entirely,
// including for hosts that do not resolve locally at all. Validating here, at the point
// where we learn a proxy will be used, restores the check.
//
// Note this leaves a rebinding window that the direct path does not have: the proxy
// resolves the target itself, so the address we validated is not necessarily the one it
// connects to. That is inherent to proxying and cannot be closed from this side.
func (f *serviceCallFilter) proxy(base func(*http.Request) (*url.URL, error)) func(*http.Request) (*url.URL, error) {
	if base == nil {
		return nil
	}
	return func(req *http.Request) (*url.URL, error) {
		proxyURL, err := base(req)
		if err != nil || proxyURL == nil {
			return proxyURL, err
		}
		if err := f.validateHost(req.Context(), req.URL.Hostname()); err != nil {
			return nil, fmt.Errorf("request to %q is blocked: %w", req.URL.Redacted(), err)
		}
		return proxyURL, nil
	}
}

// fallbackDelay is the RFC 6555 head start the preferred address family gets before the
// other one is tried in parallel. It matches net.Dialer's own default.
const fallbackDelay = 300 * time.Millisecond

// dialContext is phase 2: resolve, verify every answer, then dial the verified address.
func (f *serviceCallFilter) dialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	// No per-attempt Timeout. The deadline below covers resolution and every attempt
	// together; leaving one here as well would let a host with many answers extend the
	// total to dialTimeout per address.
	base := &net.Dialer{KeepAlive: 30 * time.Second}

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}

		// One deadline spanning resolution and all connection attempts, which is what the
		// stock dialer gives a caller. Without it, a name answering with many black-holed
		// addresses would hold an admission request for resolution plus dialTimeout per
		// address. A shorter deadline already on ctx still wins.
		ctx, cancel := context.WithTimeout(ctx, dialTimeout)
		defer cancel()

		if ip := net.ParseIP(host); ip != nil {
			if cidr := f.blockedCIDR(ip); cidr != nil {
				return nil, fmt.Errorf("connection to %s blocked: address %s is in blocked range %s", addr, ip, cidr)
			}
			return dialTCP(ctx, base, network, addr)
		}

		ips, err := lookupHost(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve %s: %w", host, err)
		}

		// Reject if ANY answer is blocked, not just the one we would have used. A host
		// with a mix of permitted and blocked records must not be reachable.
		targets := make([]net.IP, 0, len(ips))
		for _, s := range ips {
			ip := net.ParseIP(s)
			if ip == nil {
				continue
			}
			if cidr := f.blockedCIDR(ip); cidr != nil {
				// The resolved address is deliberately omitted: naming it would turn
				// admission denial messages into an internal DNS resolution oracle.
				return nil, fmt.Errorf("connection to %s blocked: %s resolves into blocked range %s", addr, host, cidr)
			}
			targets = append(targets, ip)
		}
		if len(targets) == 0 {
			return nil, fmt.Errorf("no usable address resolved for %s", host)
		}

		// Every dial below targets an address that was just validated rather than the
		// hostname, so a second resolution cannot substitute a different one (DNS
		// rebinding).
		//
		// Split by family and race the two groups the way net.Dialer does, rather than
		// walking one flat list. Trying every address of the preferred family first would
		// make a dual-stack Service whose AAAA is present but not listening wait out the
		// whole IPv6 side before reaching IPv4 — the stock dialer starts IPv4 after
		// fallbackDelay instead. lookupHost returns addresses in RFC 6724 preference
		// order, so the first one names the preferred family.
		preferIPv4 := targets[0].To4() != nil
		var primary, fallback []net.IP
		for _, ip := range targets {
			if (ip.To4() != nil) == preferIPv4 {
				primary = append(primary, ip)
			} else {
				fallback = append(fallback, ip)
			}
		}
		if len(fallback) == 0 {
			return dialSeries(ctx, base, network, port, primary)
		}
		return dialRace(ctx, base, network, port, primary, fallback)
	}
}

// dialTCP performs one connection attempt. It is a variable so tests can observe the
// context handed to each attempt, which is what proves the deadline below is shared
// rather than per-address; a test that relies on unreachable addresses instead passes
// vacuously on any host where those addresses fail fast.
var dialTCP = func(ctx context.Context, d *net.Dialer, network, addr string) (net.Conn, error) {
	return d.DialContext(ctx, network, addr)
}

// dialSeries tries each address in turn and returns the first connection that succeeds.
func dialSeries(ctx context.Context, d *net.Dialer, network, port string, ips []net.IP) (net.Conn, error) {
	var lastErr error
	for _, ip := range ips {
		conn, err := dialTCP(ctx, d, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no addresses to dial")
	}
	return nil, lastErr
}

type dialOutcome struct {
	conn      net.Conn
	err       error
	isPrimary bool
}

// dialRace runs the two address families concurrently, giving the preferred one a
// fallbackDelay head start, and returns the first connection to succeed (RFC 6555).
func dialRace(ctx context.Context, d *net.Dialer, network, port string, primary, fallback []net.IP) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	// Cancelling on return tears down the losing attempt. A connection already returned
	// is unaffected: net.Dialer does not tie a live conn to the dial context.
	defer cancel()

	// Buffered, so a loser that finishes after we have returned never blocks on send.
	outcomes := make(chan dialOutcome, 2)
	race := func(ips []net.IP, isPrimary bool, delay time.Duration) {
		go func() {
			if delay > 0 {
				timer := time.NewTimer(delay)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-ctx.Done():
					outcomes <- dialOutcome{err: ctx.Err(), isPrimary: isPrimary}
					return
				}
			}
			conn, err := dialSeries(ctx, d, network, port, ips)
			outcomes <- dialOutcome{conn: conn, err: err, isPrimary: isPrimary}
		}()
	}
	race(primary, true, 0)
	race(fallback, false, fallbackDelay)

	var firstErr error
	for i := 0; i < 2; i++ {
		out := <-outcomes
		if out.err == nil {
			// Drain the sibling so a connection that lands after this point is closed
			// rather than leaked.
			if remaining := 1 - i; remaining > 0 {
				go func(n int) {
					for j := 0; j < n; j++ {
						if late := <-outcomes; late.conn != nil {
							late.conn.Close()
						}
					}
				}(remaining)
			}
			return out.conn, nil
		}
		// Prefer the primary family's error: it is the one that describes the address the
		// caller would have reached without this guard.
		if firstErr == nil || out.isPrimary {
			firstErr = out.err
		}
	}
	return nil, firstErr
}

var (
	sharedTransportOnce sync.Once
	sharedTransport     *http.Transport
)

// serviceCallTransport returns the process-wide transport used for service calls that
// carry no CA bundle.
//
// It is built once. Cloning DefaultTransport per request would give every apiCall its own
// empty connection pool, so a policy on the admission path would open a fresh TCP
// connection each time and leave its reader/writer goroutines alive until IdleConnTimeout.
func serviceCallTransport() *http.Transport {
	sharedTransportOnce.Do(func() {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		installGuard(transport)
		sharedTransport = transport
	})
	return sharedTransport
}

// installGuard installs both phases of the SSRF guard on a transport.
//
// The filter is re-derived inside each closure rather than captured, so a shared transport
// still honours the current flag values. Deriving it costs well under a microsecond and
// happens only on a new dial, not per request served from the pool.
func installGuard(transport *http.Transport) {
	baseDial := transport.DialContext
	baseProxy := transport.Proxy

	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		filter, err := newServiceCallFilter()
		if err != nil {
			return nil, err
		}
		if !filter.enabled() {
			return baseDial(ctx, network, addr)
		}
		return filter.dialContext()(ctx, network, addr)
	}

	transport.Proxy = func(req *http.Request) (*url.URL, error) {
		filter, err := newServiceCallFilter()
		if err != nil {
			return nil, err
		}
		if !filter.enabled() {
			return baseProxy(req)
		}
		return filter.proxy(baseProxy)(req)
	}
}

func normalizeHost(h string) string {
	return strings.ToLower(strings.TrimRight(h, "."))
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch u.Scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// matchesAllowlist requires scheme, host and port to match exactly, and the path to
// match on a segment boundary so that an entry of /v1 does not permit /v10.
func (f *serviceCallFilter) matchesAllowlist(reqURL *url.URL) bool {
	reqHost := normalizeHost(reqURL.Hostname())
	reqPort := effectivePort(reqURL)

	for _, entry := range f.allowed {
		if reqURL.Scheme != entry.Scheme ||
			normalizeHost(entry.Hostname()) != reqHost ||
			effectivePort(entry) != reqPort {
			continue
		}
		entryPath := entry.Path
		if entryPath == "" || entryPath == "/" {
			return true
		}
		// Compare canonical paths. url.Parse leaves "/v1/../admin" — and the percent-encoded
		// "/v1/%2e%2e/admin", which it decodes to the same thing — intact in Path, so a raw
		// prefix test would accept either for an entry of /v1 while the server resolves them
		// to /admin. path.Clean collapses the traversal before the comparison.
		reqPath := path.Clean("/" + strings.TrimPrefix(reqURL.Path, "/"))
		cleanEntry := path.Clean("/" + strings.TrimPrefix(entryPath, "/"))
		if reqPath == cleanEntry {
			return true
		}
		// Segment boundary, so an entry of /v1 does not permit /v10. Clean has already
		// stripped any trailing slash from cleanEntry, so this is the only prefix rule
		// needed for both "/v1" and "/v1/" forms.
		if strings.HasPrefix(reqPath, cleanEntry) && len(reqPath) > len(cleanEntry) && reqPath[len(cleanEntry)] == '/' {
			return true
		}
	}
	return false
}

// maxRedirects mirrors the limit net/http applies when CheckRedirect is nil. Setting
// CheckRedirect replaces that default wholesale, so the limit has to be restated or a
// redirect loop would follow forever.
const maxRedirects = 10

// checkRedirect re-applies the pre-flight checks to each redirect target.
//
// Redirects are still followed. Refusing them outright would close the same gap, but it
// changes what a working policy does: an endpoint that legitimately redirects — http to
// https, a bare path to a versioned one, anything behind an ingress that normalises URLs —
// would start failing on upgrade. A security fix should not do that.
//
// Following them is safe because the dial guard sits in Transport.DialContext and so
// re-checks the address of every hop already. The only checks it cannot make are the ones
// that need the URL rather than the address: the hostname blocklist and the allowlist,
// both of which are otherwise pre-flight only. Running them here covers each hop and
// leaves legitimate redirects working.
func (f *serviceCallFilter) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if err := f.validateURL(req.URL.String()); err != nil {
		// Only the reason is returned, not validateURL's detail. Note this does not hide
		// the destination: net/http wraps whatever we return in a *url.Error carrying the
		// URL it was about to fetch, so the redirect target reaches the caller either way.
		// That is unchanged from before the guard existed -- a dial failure echoed the same
		// URL -- and a URL is not the response body, which is the channel that matters.
		// What the dial guard withholds, the resolved IP behind a name, stays withheld.
		klog.V(2).Infof("apiCall SSRF filter: refused redirect: %v", err)
		return fmt.Errorf("the target redirected to a blocked destination")
	}
	return nil
}

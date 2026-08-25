package apicall

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kyverno/kyverno/pkg/toggle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateURL_DefaultBlocklist_Blocked exercises validateURL against the built-in
// default blocklist, for URLs that must be rejected.
// useDefaultBlocklist selects the shipped default blocklist explicitly. TestMain installs
// a narrower baseline for this package, so Reset() alone does not give the defaults.
func useDefaultBlocklist(t *testing.T) {
	t.Helper()
	require.NoError(t, toggle.APICallBlocklist.Parse(strings.Join(toggle.DefaultAPICallBlocklist(), ",")))
}

func TestValidateURL_DefaultBlocklist_Blocked(t *testing.T) {
	useDefaultBlocklist(t)
	defer toggle.APICallBlocklist.Reset()

	urls := []string{
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"http://[::ffff:169.254.169.254]/",
		"http://169.254.169.253/",
		"http://metadata.google.internal/",
		"http://METADATA.GOOGLE.INTERNAL./",
		"file:///etc/passwd",
		"gopher://x/",
	}

	f, err := newServiceCallFilter()
	require.NoError(t, err)

	for _, u := range urls {
		t.Run(u, func(t *testing.T) {
			err := f.validateURL(u)
			assert.Error(t, err, "expected %q to be blocked", u)
		})
	}
}

// TestValidateURL_DefaultBlocklist_Allowed exercises validateURL against the built-in
// default blocklist, for URLs that must be permitted.
func TestValidateURL_DefaultBlocklist_Allowed(t *testing.T) {
	useDefaultBlocklist(t)
	defer toggle.APICallBlocklist.Reset()

	urls := []string{
		"http://my-svc.my-ns.svc.cluster.local/api",
		"http://10.0.0.5/",
		"http://192.168.1.10/",
		"https://api.example.com/v1/data",
	}

	f, err := newServiceCallFilter()
	require.NoError(t, err)

	for _, u := range urls {
		t.Run(u, func(t *testing.T) {
			err := f.validateURL(u)
			assert.NoError(t, err, "expected %q to be allowed", u)
		})
	}
}

// TestServiceCallFilter_OptOut verifies that clearing the blocklist via an explicit
// empty value disables the filter entirely.
func TestServiceCallFilter_OptOut(t *testing.T) {
	require.NoError(t, toggle.APICallBlocklist.Parse(""))
	defer toggle.APICallBlocklist.Reset()

	f, err := newServiceCallFilter()
	require.NoError(t, err)

	assert.False(t, f.enabled())
	assert.NoError(t, f.validateURL("http://169.254.169.254/"))
}

// TestServiceCallFilter_MalformedConfig verifies that a malformed CIDR entry in the
// blocklist is a hard error rather than silently permitting traffic.
func TestServiceCallFilter_MalformedConfig(t *testing.T) {
	require.NoError(t, toggle.APICallBlocklist.Parse("not-a-cidr/99"))
	defer toggle.APICallBlocklist.Reset()

	_, err := newServiceCallFilter()
	require.Error(t, err)
}

// TestAllowlist_SegmentBoundary verifies that allowlist path matching only matches on
// segment boundaries, and that host matching plus default-port equivalence works.
func TestAllowlist_SegmentBoundary(t *testing.T) {
	require.NoError(t, toggle.APICallAllowlist.Parse("https://h.example.com/v1"))
	defer toggle.APICallAllowlist.Reset()

	f, err := newServiceCallFilter()
	require.NoError(t, err)

	assert.Error(t, f.validateURL("https://h.example.com/v10/x"), "expected /v10/x not to match /v1 allowlist entry")
	assert.NoError(t, f.validateURL("https://h.example.com/v1"))
	assert.NoError(t, f.validateURL("https://h.example.com/v1/x"))
}

// TestAllowlist_DefaultPortEquivalence verifies that an allowlist entry without an
// explicit port matches a request that does specify the scheme's default port.
func TestAllowlist_DefaultPortEquivalence(t *testing.T) {
	require.NoError(t, toggle.APICallAllowlist.Parse("https://h.example.com"))
	defer toggle.APICallAllowlist.Reset()

	f, err := newServiceCallFilter()
	require.NoError(t, err)

	assert.NoError(t, f.validateURL("https://h.example.com:443/anything"))
}

// TestAllowlist_RejectsUnlistedHost verifies that a host not present in the allowlist
// is rejected.
func TestAllowlist_RejectsUnlistedHost(t *testing.T) {
	require.NoError(t, toggle.APICallAllowlist.Parse("https://h.example.com/v1"))
	defer toggle.APICallAllowlist.Reset()

	f, err := newServiceCallFilter()
	require.NoError(t, err)

	assert.Error(t, f.validateURL("https://other.example.com/v1"))
}

// TestDialContext_BlocksConfiguredRange proves phase 2 (the dial-time guard) actually
// fires: an http.Transport using filter.dialContext() as its DialContext must refuse to
// connect to a loopback test server when 127.0.0.0/8 is blocked.
func TestDialContext_BlocksConfiguredRange(t *testing.T) {
	require.NoError(t, toggle.APICallBlocklist.Parse("127.0.0.0/8"))
	defer toggle.APICallBlocklist.Reset()

	f, err := newServiceCallFilter()
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &http.Client{
		Transport: &http.Transport{DialContext: f.dialContext()},
	}

	_, err = client.Get(srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blocked range")
}

// TestDialContext_AllowsUnrelatedRange proves the dial-time guard does not over-block:
// the same loopback test server must succeed when only an unrelated range is blocked.
func TestDialContext_AllowsUnrelatedRange(t *testing.T) {
	require.NoError(t, toggle.APICallBlocklist.Parse("169.254.0.0/16"))
	defer toggle.APICallBlocklist.Reset()

	f, err := newServiceCallFilter()
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &http.Client{
		Transport: &http.Transport{DialContext: f.dialContext()},
	}

	resp, err := client.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// Test_SSRF_dialTriesEveryResolvedAddress guards against the guard pinning itself to the
// first resolved address.
//
// The stock dialer walks the resolved list until one address connects. An earlier version
// of dialContext validated every answer but then dialed only the first, so a host whose
// first record is unreachable became unreachable through the guard — most realistically a
// dual-stack Service with no AAAA listener.
//
// The resolver is stubbed rather than relying on a real name: the ordering of answers for
// "localhost" varies between platforms and between runs, so a test built on it passes
// against the pinned-first bug about as often as it catches it.
func Test_SSRF_dialTriesEveryResolvedAddress(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)

	// Nothing is listening on ::1, so the first answer must be skipped for the dial to
	// land on the v4-only listener.
	restore := lookupHost
	lookupHost = func(context.Context, string) ([]string, error) {
		return []string{"::1", "127.0.0.1"}, nil
	}
	defer func() { lookupHost = restore }()

	f, err := newServiceCallFilter()
	require.NoError(t, err)

	conn, err := f.dialContext()(context.Background(), "tcp", net.JoinHostPort("dual-stack.test", port))
	require.NoError(t, err, "dial must fall back past an unreachable first address")
	require.NoError(t, conn.Close())
}

// Test_SSRF_dialRejectsWhenAnyResolvedAddressIsBlocked pins the fail-closed half of the
// same loop: falling back past an unreachable address must not become falling back past a
// blocked one.
func Test_SSRF_dialRejectsWhenAnyResolvedAddressIsBlocked(t *testing.T) {
	useDefaultBlocklist(t)
	defer toggle.APICallBlocklist.Reset()

	restore := lookupHost
	lookupHost = func(context.Context, string) ([]string, error) {
		return []string{"10.96.0.1", "169.254.169.254"}, nil
	}
	defer func() { lookupHost = restore }()

	f, err := newServiceCallFilter()
	require.NoError(t, err)

	_, err = f.dialContext()(context.Background(), "tcp", "split-horizon.test:80")
	require.Error(t, err, "a host with any blocked answer must be refused")
	assert.Contains(t, err.Error(), "blocked range")
	assert.NotContains(t, err.Error(), "169.254.169.254", "the resolved address must not be echoed back")
}

// Test_SSRF_allowlistRejectsPathTraversal covers the case where a path-prefix allowlist
// entry is walked out of with "..". The backend, not this filter, decides what the path
// finally means, and servers routinely normalise it -- so an entry of /v1 must not admit a
// URL that resolves to /admin.
func Test_SSRF_allowlistRejectsPathTraversal(t *testing.T) {
	require.NoError(t, toggle.APICallAllowlist.Parse("https://api.example.com/v1"))
	defer toggle.APICallAllowlist.Reset()

	f, err := newServiceCallFilter()
	require.NoError(t, err)

	for _, tc := range []struct {
		name    string
		url     string
		allowed bool
	}{
		{"exact match", "https://api.example.com/v1", true},
		{"trailing slash", "https://api.example.com/v1/", true},
		{"deeper path", "https://api.example.com/v1/widgets/3", true},
		{"sibling prefix is not a segment match", "https://api.example.com/v10/secrets", false},
		{"traversal out of the prefix", "https://api.example.com/v1/../admin", false},
		{"percent-encoded traversal", "https://api.example.com/v1/%2e%2e/admin", false},
		{"double traversal", "https://api.example.com/v1/../../admin", false},
		{"traversal that lands back inside is fine", "https://api.example.com/v1/x/../y", true},
		{"other host", "https://evil.example.com/v1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := f.validateURL(tc.url)
			if tc.allowed {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

// Test_SSRF_dialFallsBackToOtherAddressFamily is the RFC 6555 behaviour the stock dialer
// provides and a naive serial loop over resolved addresses would lose: when the preferred
// family cannot be reached, the other one is tried after a short delay rather than after
// the full dial timeout.
func Test_SSRF_dialFallsBackToOtherAddressFamily(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	_, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)

	restore := lookupHost
	lookupHost = func(context.Context, string) ([]string, error) {
		return []string{"2001:db8::1", "127.0.0.1"}, nil
	}
	defer func() { lookupHost = restore }()

	// The IPv6 attempt is forced to hang rather than left to the host's routing. On a
	// machine with no IPv6 route the 2001:db8::/32 documentation range fails instantly,
	// and then a serial walk of both families reaches IPv4 just as fast as a race does --
	// so the test would pass against the very behaviour it exists to catch.
	hungReturned := make(chan struct{})
	restoreDial := dialTCP
	dialTCP = func(ctx context.Context, d *net.Dialer, network, addr string) (net.Conn, error) {
		if host, _, err := net.SplitHostPort(addr); err == nil && net.ParseIP(host).To4() == nil {
			defer close(hungReturned)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return d.DialContext(ctx, network, addr)
	}
	defer func() {
		// The losing attempt outlives the winning one, and it reads dialTCP. Restoring
		// before it returns is a data race.
		select {
		case <-hungReturned:
		case <-time.After(5 * time.Second):
		}
		dialTCP = restoreDial
	}()

	require.NoError(t, toggle.APICallBlocklist.Parse("169.254.0.0/16"))
	defer toggle.APICallBlocklist.Reset()

	f, err := newServiceCallFilter()
	require.NoError(t, err)

	started := time.Now()
	conn, err := f.dialContext()(context.Background(), "tcp", net.JoinHostPort("dual.example.test", port))
	require.NoError(t, err, "the reachable address family must still be dialed")
	defer conn.Close()

	// Generous, but well under dialTimeout: a serial walk would block on the hung IPv6
	// attempt for the whole shared deadline before ever reaching IPv4.
	assert.Less(t, time.Since(started), 5*time.Second)
}

// Test_SSRF_dialDeadlineIsSharedAcrossAttempts asserts that resolution and every
// connection attempt run under one deadline. Previously each address carried its own, so
// a name answering with many black-holed addresses could hold an admission request for
// dialTimeout per address.
//
// The assertion is on the context handed to each attempt, not on elapsed time. A timing
// test would need addresses that hang rather than fail, and on any host without an IPv6
// route the documentation range fails instantly -- such a test passes just as happily
// against the per-address-timeout code it is meant to catch.
func Test_SSRF_dialDeadlineIsSharedAcrossAttempts(t *testing.T) {
	restore := lookupHost
	lookupHost = func(context.Context, string) ([]string, error) {
		return []string{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4"}, nil
	}
	defer func() { lookupHost = restore }()

	// Same family throughout, so this exercises dialSeries rather than the race.
	var deadlines []time.Time
	restoreDial := dialTCP
	dialTCP = func(ctx context.Context, _ *net.Dialer, _, addr string) (net.Conn, error) {
		deadline, ok := ctx.Deadline()
		require.True(t, ok, "each attempt must run under a deadline, not an open-ended context")
		deadlines = append(deadlines, deadline)
		return nil, fmt.Errorf("refused %s", addr)
	}
	defer func() { dialTCP = restoreDial }()

	require.NoError(t, toggle.APICallBlocklist.Parse("169.254.0.0/16"))
	defer toggle.APICallBlocklist.Reset()

	f, err := newServiceCallFilter()
	require.NoError(t, err)

	before := time.Now()
	_, err = f.dialContext()(context.Background(), "tcp", "blackhole.example.test:80")
	require.Error(t, err)

	require.Len(t, deadlines, 4, "every resolved address must be attempted")
	for i, d := range deadlines {
		assert.WithinDuration(t, deadlines[0], d, time.Millisecond,
			"attempt %d must share the first attempt's deadline, not start a new one", i)
	}
	// One dialTimeout from the start of the dial, not one per address.
	assert.WithinDuration(t, before.Add(dialTimeout), deadlines[0], time.Second)
}

// Test_SSRF_dialDeadlineRespectsShorterParent checks the deadline above does not extend
// one the caller already set.
func Test_SSRF_dialDeadlineRespectsShorterParent(t *testing.T) {
	restore := lookupHost
	lookupHost = func(context.Context, string) ([]string, error) {
		return []string{"192.0.2.1"}, nil
	}
	defer func() { lookupHost = restore }()

	var got time.Time
	restoreDial := dialTCP
	dialTCP = func(ctx context.Context, _ *net.Dialer, _, _ string) (net.Conn, error) {
		got, _ = ctx.Deadline()
		return nil, fmt.Errorf("refused")
	}
	defer func() { dialTCP = restoreDial }()

	require.NoError(t, toggle.APICallBlocklist.Parse("169.254.0.0/16"))
	defer toggle.APICallBlocklist.Reset()

	f, err := newServiceCallFilter()
	require.NoError(t, err)

	parent := time.Now().Add(2 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), parent)
	defer cancel()

	_, err = f.dialContext()(ctx, "tcp", "blackhole.example.test:80")
	require.Error(t, err)
	assert.WithinDuration(t, parent, got, 50*time.Millisecond,
		"the caller's shorter deadline must win over dialTimeout")
}

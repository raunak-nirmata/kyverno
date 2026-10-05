package apicall

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/go-logr/logr"
	kyvernov1 "github.com/kyverno/kyverno/api/kyverno/v1"
	enginecontext "github.com/kyverno/kyverno/pkg/engine/context"
	"github.com/kyverno/kyverno/pkg/toggle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// These tests drive the full executor rather than the filter in isolation, so they prove
// the guard is actually reached on the code path an admission request takes.

const fakeIMDSCredentials = `{"AccessKeyId":"ASIAEXAMPLE","SecretAccessKey":"s3cr3t-do-not-leak"}`

func newExecutorForTest() *executor {
	return NewExecutor(logr.Discard(), "test", nil, NewAPICallConfiguration(10*1000*1000, 30*time.Second))
}

func serviceCall(url, method string) *kyvernov1.APICall {
	return &kyvernov1.APICall{
		Method:  kyvernov1.Method(method),
		Service: &kyvernov1.ServiceCall{URL: url},
	}
}

// Test_SSRF_blockedBeforeRequestIsMade is the regression test for the
// exfiltration channel described in GHSA-qr4g-8hrp-c4rw. Before the fix, a non-2xx
// response body from the target was embedded verbatim in the returned error, which
// propagates into the kubectl admission denial message. The request must now be refused
// before it is ever made, and the secret must not appear anywhere in the error.
func Test_SSRF_blockedBeforeRequestIsMade(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(fakeIMDSCredentials))
	}))
	defer server.Close()

	// Loopback is not in the shipped default (see defaultAPICallBlocklist), so it is
	// configured explicitly here. It stands in for 169.254.169.254, which a unit test
	// cannot dial; the code path exercised is identical.
	require.NoError(t, toggle.APICallBlocklist.Parse("127.0.0.0/8"))
	defer toggle.APICallBlocklist.Reset()

	_, err := newExecutorForTest().Execute(context.Background(), serviceCall(server.URL, "GET"))

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "s3cr3t-do-not-leak", "response body must not be reflected into the error")
	assert.NotContains(t, err.Error(), "ASIAEXAMPLE")
	assert.Contains(t, err.Error(), "blocked")
}

// Test_SSRF_metadataAddressBlocked covers the literal IMDS addresses, including the
// IPv4-mapped IPv6 form reported upstream as kyverno/kyverno#16419.
func Test_SSRF_metadataAddressBlocked(t *testing.T) {
	useDefaultBlocklist(t)
	defer toggle.APICallBlocklist.Reset()

	for _, url := range []string{
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"http://[::ffff:169.254.169.254]/latest/meta-data/",
		"http://metadata.google.internal/computeMetadata/v1/",
	} {
		t.Run(url, func(t *testing.T) {
			_, err := newExecutorForTest().Execute(context.Background(), serviceCall(url, "GET"))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "blocked")
		})
	}
}

// Test_SSRF_redirectIntoBlockedRangeRefused covers a permitted host redirecting into a
// blocked one. The hop is refused, but redirects as a mechanism still work -- see
// Test_SSRF_legitimateRedirectStillFollowed.
func Test_SSRF_redirectIntoBlockedRangeRefused(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer server.Close()

	// Baseline from TestMain permits loopback, so the initial request is allowed and the
	// redirect target is what must be refused.
	_, err := newExecutorForTest().Execute(context.Background(), serviceCall(server.URL, "GET"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "redirect")

	// What must not leak is the response BODY -- that is the exfiltration channel the
	// advisory describes. The hop is refused before the target is ever contacted, so
	// there is no body to leak.
	assert.NotContains(t, err.Error(), "s3cr3t-do-not-leak")

	// The redirect URL itself DOES appear, and cannot be suppressed from here: net/http
	// wraps any CheckRedirect error in a *url.Error carrying the URL it was about to
	// fetch. This is unchanged from before the guard existed -- a plain dial failure
	// echoed the same URL -- and it is a URL, not a response body. Asserted so the
	// distinction is deliberate rather than assumed.
	assert.Contains(t, err.Error(), "169.254.169.254")
}

// Test_SSRF_legitimateRedirectStillFollowed is the backward-compatibility canary for
// redirects.
//
// An earlier revision refused every redirect outright. That closes the same gap, but it
// breaks any service call whose endpoint legitimately redirects, which is a behaviour
// change on upgrade rather than a security fix. Redirects to permitted destinations must
// keep working.
func Test_SSRF_legitimateRedirectStillFollowed(t *testing.T) {
	var landed bool
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/data", func(w http.ResponseWriter, r *http.Request) {
		landed = true
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/data", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/v1/data", http.StatusFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	body, err := newExecutorForTest().Execute(context.Background(), serviceCall(server.URL+"/data", "GET"))

	require.NoError(t, err, "a redirect to a permitted destination must still be followed")
	assert.True(t, landed, "the redirect target must actually be reached")
	assert.Contains(t, string(body), `"ok":true`)
}

// Test_SSRF_redirectLoopStillBounded pins the hop limit. Setting CheckRedirect replaces
// net/http's default policy wholesale, so the limit has to be restated or a redirect loop
// would follow forever.
func Test_SSRF_redirectLoopStillBounded(t *testing.T) {
	var hops int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer server.Close()

	_, err := newExecutorForTest().Execute(context.Background(), serviceCall(server.URL, "GET"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "stopped after")
	assert.LessOrEqual(t, hops, maxRedirects+1, "the loop must be bounded")
}

// Test_SSRF_legitimateInClusterCallStillWorks is the backward-compatibility canary. The
// default blocklist must not break calls to ordinary in-cluster services, which is the
// documented primary use of service calls and what upstream's first attempt broke.
func Test_SSRF_legitimateInClusterCallStillWorks(t *testing.T) {
	useDefaultBlocklist(t)
	defer toggle.APICallBlocklist.Reset()

	f, err := newServiceCallFilter()
	require.NoError(t, err)

	for _, url := range []string{
		"http://my-svc.my-ns.svc.cluster.local/api",
		"http://10.96.0.10:8080/data",
		"http://172.20.1.5/v1",
		"http://192.168.4.4/health",
		"https://external-api.example.com/v2/policy",
	} {
		assert.NoError(t, f.validateURL(url), "in-cluster/external target must remain reachable: %s", url)
	}
}

// Test_SSRF_optOutRestoresPreviousBehaviour confirms the documented escape hatch works.
func Test_SSRF_optOutRestoresPreviousBehaviour(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	require.NoError(t, toggle.APICallBlocklist.Parse(""))
	defer toggle.APICallBlocklist.Reset()

	body, err := newExecutorForTest().Execute(context.Background(), serviceCall(server.URL, "GET"))
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(body), "ok"))
}

// Test_SSRF_proxyGuard covers the proxy bypass found in review.
//
// http.DefaultTransport carries ProxyFromEnvironment. When a proxy is selected, Go dials
// the PROXY, so Transport.DialContext receives the proxy's address and never sees the
// real target — the dial-time guard was completely inert, including for hosts that do
// not resolve locally at all. filter.proxy() re-applies the check at the point where the
// proxy is chosen.
//
// The wrapper is exercised directly rather than through HTTP_PROXY because net/http
// resolves the environment proxy once per process (sync.Once), so an env var set after
// any earlier request in the same test binary would be silently ignored and the test
// would pass without proving anything.
func Test_SSRF_proxyGuard(t *testing.T) {
	require.NoError(t, toggle.APICallBlocklist.Parse("169.254.0.0/16,metadata.google.internal"))
	defer toggle.APICallBlocklist.Reset()

	f, err := newServiceCallFilter()
	require.NoError(t, err)

	proxyURL, err := url.Parse("http://proxy.internal:3128")
	require.NoError(t, err)

	var baseCalled bool
	base := func(*http.Request) (*url.URL, error) {
		baseCalled = true
		return proxyURL, nil
	}
	wrapped := f.proxy(base)
	require.NotNil(t, wrapped)

	newReq := func(rawURL string) *http.Request {
		req, err := http.NewRequestWithContext(context.Background(), "GET", rawURL, nil)
		require.NoError(t, err)
		return req
	}

	t.Run("blocked target is refused even though the proxy address is fine", func(t *testing.T) {
		got, err := wrapped(newReq("http://169.254.169.254/latest/meta-data/"))
		require.Error(t, err)
		assert.Nil(t, got, "must not return a proxy for a blocked target")
		assert.Contains(t, err.Error(), "blocked")
	})

	t.Run("blocked hostname is refused", func(t *testing.T) {
		_, err := wrapped(newReq("http://metadata.google.internal/computeMetadata/v1/"))
		require.Error(t, err)
	})

	t.Run("unresolvable target is refused when no allowlist is configured", func(t *testing.T) {
		// The CIDR half of the filter is the half that closes the metadata vector, and it
		// cannot run on a name the pod cannot resolve. With nothing else vouching for the
		// destination, refuse rather than guess.
		got, err := wrapped(newReq("http://imds.attacker.invalid/latest/meta-data/"))
		require.Error(t, err)
		assert.Nil(t, got, "returning no proxy URL is what refuses the request")
		assert.Contains(t, err.Error(), toggle.APICallAllowlistFlagName,
			"the error must name the way to opt the host back in")
	})

	t.Run("unresolvable target is permitted when the allowlist vouches for it", func(t *testing.T) {
		// The escape hatch for proxied clusters. On egress-locked deployments the pod
		// often cannot resolve external names at all -- the proxy does -- so a blanket
		// refusal above would break every external service call. An allowlist entry is an
		// operator naming the destination by hand, which is the sanction the failed
		// resolution could not provide.
		require.NoError(t, toggle.APICallAllowlist.Parse("http://imds.attacker.invalid"))
		defer toggle.APICallAllowlist.Reset()

		allowFilter, err := newServiceCallFilter()
		require.NoError(t, err)

		baseCalled = false
		got, err := allowFilter.proxy(base)(newReq("http://imds.attacker.invalid/latest/meta-data/"))
		require.NoError(t, err)
		assert.Equal(t, proxyURL, got)
		assert.True(t, baseCalled)
	})

	t.Run("an allowlist does not rescue a target on the hostname blocklist", func(t *testing.T) {
		// The blocklist check runs before resolution is attempted, so the opt-in above
		// cannot be used to walk a blocked name back in.
		require.NoError(t, toggle.APICallAllowlist.Parse("http://metadata.google.internal"))
		defer toggle.APICallAllowlist.Reset()

		allowFilter, err := newServiceCallFilter()
		require.NoError(t, err)

		_, err = allowFilter.proxy(base)(newReq("http://metadata.google.internal/computeMetadata/v1/"))
		require.Error(t, err)
	})

	t.Run("unresolvable target on the blocklist is still refused", func(t *testing.T) {
		// The fail-open above must not swallow a hostname that is explicitly blocked --
		// that check happens before resolution is attempted.
		_, err := wrapped(newReq("http://metadata.google.internal/anything"))
		require.Error(t, err)
	})

	t.Run("permitted target is still proxied", func(t *testing.T) {
		baseCalled = false
		got, err := wrapped(newReq("http://203.0.113.10/api"))
		require.NoError(t, err)
		assert.Equal(t, proxyURL, got, "egress-proxy deployments must keep working")
		assert.True(t, baseCalled)
	})

	t.Run("nil base proxy stays nil", func(t *testing.T) {
		assert.Nil(t, f.proxy(nil))
	})
}

// Test_SSRF_embeddedIPv4Forms covers IPv6 encodings that carry an IPv4 address but are
// not recognised by net.IP.To4, so an IPv4 CIDR alone would miss them.
func Test_SSRF_embeddedIPv4Forms(t *testing.T) {
	useDefaultBlocklist(t)
	defer toggle.APICallBlocklist.Reset()

	f, err := newServiceCallFilter()
	require.NoError(t, err)

	for _, host := range []string{
		"::ffff:169.254.169.254",   // IPv4-mapped, already handled by To4
		"::169.254.169.254",        // IPv4-compatible
		"64:ff9b::169.254.169.254", // NAT64 well-known prefix
	} {
		t.Run(host, func(t *testing.T) {
			assert.Error(t, f.validateURL("http://["+host+"]/latest/meta-data/"))
		})
	}
}

// Test_SSRF_blockedErrorDoesNotLeakResolvedAddress guards the DNS oracle. A tenant must
// not be able to learn what an internal name resolves to from the denial message.
func Test_SSRF_blockedErrorDoesNotLeakResolvedAddress(t *testing.T) {
	require.NoError(t, toggle.APICallBlocklist.Parse("127.0.0.0/8"))
	defer toggle.APICallBlocklist.Reset()

	f, err := newServiceCallFilter()
	require.NoError(t, err)

	// Stub the resolver rather than leaning on "localhost". On an IPv6-only or otherwise
	// differently configured host it can answer with ::1 alone, which 127.0.0.0/8 does not
	// cover -- the assertion below would then pass vacuously by never blocking at all.
	restore := lookupHost
	lookupHost = func(context.Context, string) ([]string, error) {
		return []string{"127.0.0.1"}, nil
	}
	defer func() { lookupHost = restore }()

	err = f.validateHost(context.Background(), "internal.example.test")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "127.0.0.1", "must not reveal the resolved address")
	assert.Contains(t, err.Error(), "blocked range")

	// The dial path carries its own copy of the message and is the one an admission
	// request actually reaches, so assert on it too.
	_, err = f.dialContext()(context.Background(), "tcp", "internal.example.test:8200")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "127.0.0.1", "must not reveal the resolved address")
	assert.Contains(t, err.Error(), "blocked range")
}

// Test_SSRF_clientWiring asserts that buildHTTPClient actually installs every part of the
// guard, on both the plain and CA-bundle paths. Without this, the filter unit tests would
// still pass if the wiring in buildHTTPClient were removed.
func Test_SSRF_clientWiring(t *testing.T) {
	require.NoError(t, toggle.APICallBlocklist.Parse("169.254.0.0/16"))
	defer toggle.APICallBlocklist.Reset()

	caPEM := generateTestCA(t)

	for _, tc := range []struct {
		name    string
		service *kyvernov1.ServiceCall
	}{
		{"no CA bundle", &kyvernov1.ServiceCall{URL: "http://svc.ns.svc.cluster.local/"}},
		{"with CA bundle", &kyvernov1.ServiceCall{URL: "https://svc.ns.svc.cluster.local/", CABundle: caPEM}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := newExecutorForTest().buildHTTPClient(tc.service)
			require.NoError(t, err)

			assert.NotNil(t, client.CheckRedirect, "each redirect hop must be re-validated")
			// The guard must not have displaced the configured request deadline.
			assert.Equal(t, 30*time.Second, client.Timeout)

			// tracing.Transport wraps the *http.Transport, so reach through otelhttp.
			transport := extractHTTPTransport(t, client)
			require.NotNil(t, transport)
			assert.NotNil(t, transport.DialContext, "dial-time guard must be installed")
			require.NotNil(t, transport.Proxy, "proxy guard must be installed")

			// Transport.Proxy is the wrapped resolver. With no proxy configured in the
			// environment the wrapper short-circuits to nil, which is correct — the dial
			// guard covers the direct path. The refusal behaviour itself is covered by
			// Test_SSRF_proxyGuard, which drives the wrapper directly.

			// The process-wide default must never be mutated.
			defaultTransport := http.DefaultTransport.(*http.Transport)
			assert.NotSame(t, defaultTransport, transport, "must operate on a clone")
			assert.NotNil(t, defaultTransport.Proxy, "sanity: default still has its proxy")
			assert.NotEqual(t,
				reflect.ValueOf(transport.DialContext).Pointer(),
				reflect.ValueOf(defaultTransport.DialContext).Pointer(),
				"guarded dialer must not be the stdlib default")
		})
	}
}

// Test_SSRF_caBundleBranchBlocksAtDial closes the behavioural half of the caBundle
// question.
//
// Test_SSRF_clientWiring proves the guard is *installed* on that branch. This proves it
// actually refuses there. The distinction matters because the caBundle path is the one
// that builds its own transport rather than sharing the guarded one, so it was the most
// plausible place for a single missing line to open a bypass.
//
// The target resolves into a blocked range but is not itself blocklisted, so it passes
// pre-flight and the refusal can only have come from the dial guard.
func Test_SSRF_caBundleBranchBlocksAtDial(t *testing.T) {
	require.NoError(t, toggle.APICallBlocklist.Parse("10.0.0.0/8"))
	defer toggle.APICallBlocklist.Reset()

	restore := lookupHost
	lookupHost = func(context.Context, string) ([]string, error) {
		return []string{"10.1.2.3"}, nil
	}
	defer func() { lookupHost = restore }()

	svc := &kyvernov1.ServiceCall{URL: "https://benign.example/api", CABundle: generateTestCA(t)}

	// Sanity: pre-flight lets this through, so a pass here would be a real bypass rather
	// than an accident of ordering.
	f, err := newServiceCallFilter()
	require.NoError(t, err)
	require.NoError(t, f.validateURL(svc.URL), "target must pass pre-flight for this test to mean anything")

	client, err := newExecutorForTest().buildHTTPClient(svc)
	require.NoError(t, err)

	_, err = client.Get(svc.URL)
	require.Error(t, err, "the caBundle transport must enforce the dial guard")
	assert.Contains(t, err.Error(), "blocked")
	assert.NotContains(t, err.Error(), "10.1.2.3", "the resolved address must stay out of the error")
}

// extractHTTPTransport digs the *http.Transport out of the otelhttp wrapper that
// tracing.Transport applies, using reflection since the field is unexported.
func extractHTTPTransport(t *testing.T, client *http.Client) *http.Transport {
	t.Helper()
	if tr, ok := client.Transport.(*http.Transport); ok {
		return tr
	}
	v := reflect.ValueOf(client.Transport)
	for v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	for i := 0; i < v.NumField(); i++ {
		fv := v.Field(i)
		if !fv.CanInterface() {
			fv = reflect.NewAt(fv.Type(), unsafe.Pointer(fv.UnsafeAddr())).Elem()
		}
		if tr, ok := fv.Interface().(*http.Transport); ok {
			return tr
		}
		if rt, ok := fv.Interface().(http.RoundTripper); ok && rt != nil {
			if tr, ok := rt.(*http.Transport); ok {
				return tr
			}
		}
	}
	t.Fatalf("could not extract *http.Transport from %T", client.Transport)
	return nil
}

// generateTestCA returns a valid self-signed CA certificate in PEM form, so the CABundle
// branch of buildHTTPClient is genuinely exercised rather than skipped. That branch
// previously built its own transport and would have bypassed the guard entirely.
func generateTestCA(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kyverno-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// Test_SSRF_loopbackAndUnspecifiedBlockedByDefault pins what the shipped default does
// with addresses that reach listeners inside the Kyverno pod.
//
// A sidecar in the pod can hold credentials -- a Vault agent with auto-auth on
// 127.0.0.1:8200 is the case that matters -- and reading from it is the same credential
// theft this guard exists to stop.
//
// The unspecified addresses are blocked too. connect() to 0.0.0.0 or :: reaches those same
// listeners while sitting outside 127.0.0.0/8 and ::1/128, so leaving them out made the
// loopback entry bypassable by changing three characters.
func Test_SSRF_loopbackAndUnspecifiedBlockedByDefault(t *testing.T) {
	t.Run("loopback is blocked under the shipped default", func(t *testing.T) {
		useDefaultBlocklist(t)
		defer toggle.APICallBlocklist.Reset()

		f, err := newServiceCallFilter()
		require.NoError(t, err)
		for _, host := range []string{"127.0.0.1", "127.1.2.3", "[::1]", "[2002:7f00:1::]"} {
			assert.Error(t, f.validateURL("http://"+host+"/"),
				"%s must be blocked by default", host)
		}
	})

	t.Run("unspecified addresses are blocked under the shipped default", func(t *testing.T) {
		useDefaultBlocklist(t)
		defer toggle.APICallBlocklist.Reset()

		f, err := newServiceCallFilter()
		require.NoError(t, err)
		// Literal forms only. Spellings that need resolving, such as a bare "0",
		// are rejected by the dial-time guard instead of this pre-flight check.
		for _, host := range []string{"0.0.0.0", "[::]"} {
			assert.Error(t, f.validateURL("http://"+host+"/"),
				"%s reaches loopback and must be blocked by default", host)
		}
	})
}

// Test_SSRF_transportIsPooled guards against reintroducing a per-call transport, which
// gives every apiCall an empty connection pool and leaks goroutines until IdleConnTimeout.
func Test_SSRF_transportIsPooled(t *testing.T) {
	require.NoError(t, toggle.APICallBlocklist.Parse("169.254.0.0/16"))
	defer toggle.APICallBlocklist.Reset()

	var mu sync.Mutex
	var accepts int
	// Unstarted, so ConnState is set before the serve goroutine reads it. Assigning to
	// Config on an already-running httptest server is a data race.
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			mu.Lock()
			accepts++
			mu.Unlock()
		}
	}
	server.Start()
	defer server.Close()

	const calls = 20
	for i := 0; i < calls; i++ {
		_, err := newExecutorForTest().Execute(context.Background(), serviceCall(server.URL, "GET"))
		require.NoError(t, err)
	}

	mu.Lock()
	got := accepts
	mu.Unlock()
	assert.Less(t, got, calls, "connections must be reused across calls, got %d for %d calls", got, calls)
}

// Test_SSRF_addressFormMatrix is the closing test for the class of bypass that two review
// passes each found a member of: an address spelling that reaches a sensitive destination
// without literally matching a blocklist entry. It pins both the blocked and the allowed
// side, so a future change to the default list has to confront both.
func Test_SSRF_addressFormMatrix(t *testing.T) {
	useDefaultBlocklist(t)
	defer toggle.APICallBlocklist.Reset()

	f, err := newServiceCallFilter()
	require.NoError(t, err)

	blocked := []struct{ host, why string }{
		{"169.254.169.254", "IMDS literal"},
		{"[::ffff:169.254.169.254]", "IPv4-mapped IPv6"},
		{"[::169.254.169.254]", "IPv4-compatible IPv6"},
		{"[64:ff9b::169.254.169.254]", "NAT64"},
		{"[2002:a9fe:a9fe::]", "6to4 of 169.254.169.254"},
		{"[fd00:ec2::254]", "AWS IPv6 IMDS"},
		{"100.100.100.200", "Alibaba Cloud metadata"},
		{"[fe80::1]", "IPv6 link-local"},
		{"0.0.0.0", "unspecified IPv4: reaches a loopback listener"},
		{"[::]", "unspecified IPv6: reaches a loopback listener"},
		{"metadata.google.internal", "GCP metadata hostname"},
	}
	for _, tc := range blocked {
		t.Run("blocked/"+tc.host, func(t *testing.T) {
			assert.Error(t, f.validateURL("http://"+tc.host+"/"), "must be blocked: %s", tc.why)
		})
	}

	// The default list must not creep into ranges real clusters use.
	allowed := []struct{ host, why string }{
		{"10.96.0.1", "ClusterIP"},
		{"172.20.0.5", "RFC-1918"},
		{"192.168.1.1", "RFC-1918"},
		{"[fd00::1]", "IPv6 ULA, used for in-cluster IPv6"},
		{"[2001:db8::1]", "ordinary IPv6"},
		{"my-svc.my-ns.svc.cluster.local", "in-cluster DNS"},
		{"8.8.8.8", "ordinary public IPv4"},
		{"255.255.255.255", "broadcast: not an HTTP target"},
		{"224.0.0.1", "multicast: not an HTTP target"},
	}
	for _, tc := range allowed {
		t.Run("allowed/"+tc.host, func(t *testing.T) {
			assert.NoError(t, f.validateURL("http://"+tc.host+"/"), "must stay reachable: %s", tc.why)
		})
	}
}

// Test_SSRF_httpsServiceCallSucceedsThroughGuard exercises the documented primary use of
// service calls: an in-cluster HTTPS endpoint with a caBundle.
//
// This is the case most at risk from the guard. dialContext connects to the resolved IP
// rather than the hostname, so if TLS took its SNI and certificate verification from the
// dial address instead of the request URL, every in-cluster HTTPS service call would fail
// certificate validation. The target here is a HOSTNAME, so the resolve-then-dial-by-IP
// branch is genuinely taken — a literal 127.0.0.1 target would skip it.
func Test_SSRF_httpsServiceCallSucceedsThroughGuard(t *testing.T) {
	certPEM, keyPEM := generateTestServerCert(t)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"in-cluster":"ok"}`))
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	server.StartTLS()
	defer server.Close()

	// Loopback must stay permitted so the call can reach the test server; the guard is
	// still active for link-local.
	require.NoError(t, toggle.APICallBlocklist.Parse("169.254.0.0/16"))
	defer toggle.APICallBlocklist.Reset()

	port := strings.TrimPrefix(server.URL, "https://127.0.0.1:")
	call := &kyvernov1.APICall{
		Method: "GET",
		Service: &kyvernov1.ServiceCall{
			URL:      "https://localhost:" + port + "/api",
			CABundle: string(certPEM),
		},
	}

	body, err := newExecutorForTest().Execute(context.Background(), call)
	require.NoError(t, err, "in-cluster HTTPS service calls must keep working")
	assert.Contains(t, string(body), "in-cluster")
}

// generateTestServerCert returns a self-signed serving certificate valid for "localhost",
// usable both as the server's cert and as the caBundle that verifies it.
func generateTestServerCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM
}

// Test_SSRF_tenantControlledURL_GHSA_qr4g is the canonical reproduction of
// GHSA-qr4g-8hrp-c4rw, and the test that justifies this whole guard.
//
// It demonstrates the CVSS PR:L claim concretely: the attacker authors NO policy. An
// admin writes a ClusterPolicy whose context apiCall interpolates request data into
// service.url, and an ordinary tenant — holding only create on Pod in their own namespace
// — supplies the host by setting an annotation on the Pod they submit.
//
// variables.SubstituteAllInType marshals the entire APICall struct to untyped JSON and
// substitutes every string in it (pkg/engine/variables/vars.go), so there is no field
// allowlist keeping variables out of Service.URL. The admission request is added to the
// context at PolicyContext construction, before any context entry is evaluated, so
// request.object.* is fully in scope.
//
// The test asserts both halves: unguarded, the tenant's chosen host is contacted and its
// response is loaded back into the policy context (the exfiltration channel); guarded by
// the shipped defaults, the call is refused before the request is made.
func Test_SSRF_tenantControlledURL_GHSA_qr4g(t *testing.T) {
	var contactedHost, contactedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contactedHost, contactedPath = r.Host, r.URL.Path
		_, _ = w.Write([]byte(`{"stolen":"IMDS-STANDIN"}`))
	}))
	defer server.Close()
	// Stands in for 169.254.169.254, which cannot be dialed from a unit test.
	attackerChosenHost := strings.TrimPrefix(server.URL, "http://")

	// The only thing the attacker supplies: an ordinary Pod they are allowed to create.
	pod, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":        "evil",
			"namespace":   "tenant-ns",
			"annotations": map[string]any{"target": attackerChosenHost},
		},
	})
	require.NoError(t, err)

	jsonCtx := enginecontext.NewContext(jp)
	require.NoError(t, jsonCtx.AddRequest(admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: pod},
	}))

	// Admin-authored. The policy itself is not malicious.
	entry := kyvernov1.ContextEntry{
		Name: "external",
		APICall: &kyvernov1.ContextAPICall{
			APICall: kyvernov1.APICall{
				Method:  "GET",
				Service: &kyvernov1.ServiceCall{URL: "http://{{ request.object.metadata.annotations.target }}/exfil"},
			},
		},
	}

	t.Run("substitution really does place tenant data in service.url", func(t *testing.T) {
		// TestMain's baseline permits loopback, isolating the substitution question from
		// the guard. This is the pre-fix behaviour.
		call, err := New(logr.Discard(), jp, entry, jsonCtx, nil, apiConfig, "")
		require.NoError(t, err)
		data, err := call.FetchAndLoad(context.TODO())
		require.NoError(t, err)

		assert.Equal(t, attackerChosenHost, contactedHost, "the tenant chose the host that was contacted")
		assert.Equal(t, "/exfil", contactedPath)
		assert.Contains(t, string(data), "IMDS-STANDIN", "and the response is loaded back into the policy context")
	})

	t.Run("the shipped defaults refuse the real target", func(t *testing.T) {
		// Same policy, same tenant capability -- but with the annotation set to what an
		// attacker actually wants. This is the exact PoC from GHSA-qr4g-8hrp-c4rw.
		useDefaultBlocklist(t)
		defer toggle.APICallBlocklist.Reset()

		imdsPod, err := json.Marshal(map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]any{
				"name":      "evil",
				"namespace": "tenant-ns",
				"annotations": map[string]any{
					"target": "169.254.169.254/latest/meta-data/iam/security-credentials",
				},
			},
		})
		require.NoError(t, err)

		imdsCtx := enginecontext.NewContext(jp)
		require.NoError(t, imdsCtx.AddRequest(admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Object:    runtime.RawExtension{Raw: imdsPod},
		}))

		call, err := New(logr.Discard(), jp, entry, imdsCtx, nil, apiConfig, "")
		require.NoError(t, err)
		_, err = call.FetchAndLoad(context.TODO())

		require.Error(t, err)
		assert.Contains(t, err.Error(), "blocked")
		assert.Contains(t, err.Error(), "169.254.0.0/16")
	})

	// A namespaced Policy cannot make service calls at all, so the vector needs a
	// ClusterPolicy. Pinned so that mitigation is not silently lost.
	t.Run("namespaced Policy cannot reach the service-call path", func(t *testing.T) {
		call, err := New(logr.Discard(), jp, entry, jsonCtx, nil, apiConfig, "tenant-ns")
		require.NoError(t, err)
		_, err = call.FetchAndLoad(context.TODO())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "namespace segment")
	})
}

// Test_SSRF_allowlistEnforcedOnTheExecutorPath drives the allowlist through Execute.
//
// The allowlist is enforced only in the pre-flight phase -- the dial guard sees an
// address, not a URL, so it cannot apply it. Every other pre-flight rule has a dial-time
// counterpart that would keep a blocked request blocked if the pre-flight call were
// dropped; this one does not, so without this test removing that call would silently
// disable the allowlist while the rest of the suite stayed green.
func Test_SSRF_allowlistEnforcedOnTheExecutorPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	require.NoError(t, toggle.APICallAllowlist.Parse(server.URL+"/permitted"))
	defer toggle.APICallAllowlist.Reset()

	body, err := newExecutorForTest().Execute(context.Background(), serviceCall(server.URL+"/permitted/sub", "GET"))
	require.NoError(t, err, "a URL under an allowlisted prefix must still work")
	assert.JSONEq(t, `{"ok":true}`, string(body))

	_, err = newExecutorForTest().Execute(context.Background(), serviceCall(server.URL+"/elsewhere", "GET"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no matching entry")
}

// Test_SSRF_nonHTTPSchemeRefused covers the other pre-flight-only rule. url.Parse accepts
// these schemes and http.NewRequest would carry them to a transport, so they have to be
// refused before the request is built.
func Test_SSRF_nonHTTPSchemeRefused(t *testing.T) {
	for _, url := range []string{"file:///etc/passwd", "gopher://127.0.0.1:6379/_INFO", "ftp://internal.example.test/"} {
		t.Run(url, func(t *testing.T) {
			_, err := newExecutorForTest().Execute(context.Background(), serviceCall(url, "GET"))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "scheme must be http or https")
		})
	}
}

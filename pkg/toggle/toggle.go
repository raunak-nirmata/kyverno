package toggle

import (
	"os"
	"strconv"
	"strings"

	sdkhttp "github.com/kyverno/sdk/cel/libs/http"
)

// defaultAPICallBlocklist is the default set of hosts and CIDRs that
// context.apiCall service calls may not reach.
//
// The list stays minimal on purpose. It holds only destinations no legitimate service call
// needs, because upstream's first attempt at this filter defaulted to the full RFC-1918
// space and broke in-cluster service calls, which are the documented primary use case
// (kyverno/kyverno#15880 -> #15960 -> #15973). ClusterIP ranges, pod CIDRs, IPv6 ULA and
// ordinary public addresses all stay reachable exactly as before. That is also why this is
// a separate list from HTTPBlocklist below rather than a reuse of it: the CEL defaults from
// the SDK block 10.0.0.0/8, 172.16.0.0/12 and 192.168.0.0/16, which is where ClusterIPs live.
//
// Everything upstream blocks by default, this blocks too. Upstream's list is
// 169.254.169.254/32, 169.254.169.253/32, metadata.google.internal, 127.0.0.0/8 and
// ::1/128; the two metadata addresses fall inside 169.254.0.0/16 below. Staying a superset
// keeps a policy that works on n4k working on upstream, and means an operator who moves
// between them never gains reach by upgrading.
//
// Loopback is included for the same reason upstream includes it: a sidecar in the Kyverno
// pod can hold credentials. A Vault agent with auto-auth on 127.0.0.1:8200 is the case
// that matters, and reading from it is the same credential theft this guard exists to
// stop. It is also the one entry that can break a policy that works today -- a policy that
// deliberately calls a pod-local service -- so operators who need that can drop it with
// --apiCallBlocklist.
//
// The link-local ranges are listed as CIDRs, not merely as the well-known metadata
// hostnames. A bare hostname entry is matched as a string before the request is dialed, so
// on its own it is bypassed both by naming the address literally and by pointing any DNS
// name at it. The CIDR entries are what actually close the metadata vector, because they
// are enforced against every resolved IP at dial time.
var defaultAPICallBlocklist = []string{
	"169.254.0.0/16",    // IPv4 link-local: AWS/GCP/Azure IMDS and everything else there
	"fe80::/10",         // IPv6 link-local
	"fd00:ec2::254/128", // AWS IPv6 IMDS: a single address, not the fc00::/7 ULA range
	//                             it sits in, which must stay reachable for in-cluster IPv6
	"100.100.100.200/32",       // Alibaba Cloud metadata service
	"127.0.0.0/8",              // IPv4 loopback: sidecars in the Kyverno pod
	"::1/128",                  // IPv6 loopback
	"metadata.google.internal", // GCP metadata service hostname
	"metadata.internal",        // Oracle Cloud metadata service hostname
}

const (
	// protect managed resource
	ProtectManagedResourcesFlagName    = "protectManagedResources"
	ProtectManagedResourcesDescription = "Set the flag to 'true', to enable managed resources protection."
	protectManagedResourcesEnvVar      = "FLAG_PROTECT_MANAGED_RESOURCES"
	defaultProtectManagedResources     = false
	// force failure policy ignore
	ForceFailurePolicyIgnoreFlagName    = "forceFailurePolicyIgnore"
	ForceFailurePolicyIgnoreDescription = "Set the flag to 'true', to force set Failure Policy to 'ignore'."
	forceFailurePolicyIgnoreEnvVar      = "FLAG_FORCE_FAILURE_POLICY_IGNORE"
	defaultForceFailurePolicyIgnore     = false
	// enable deferred context loading
	EnableDeferredLoadingFlagName    = "enableDeferredLoading"
	EnableDeferredLoadingDescription = "enable deferred loading of context variables"
	enableDeferredLoadingEnvVar      = "FLAG_ENABLE_DEFERRED_LOADING"
	defaultEnableDeferredLoading     = true
	// generate validating admission policies
	GenerateValidatingAdmissionPolicyFlagName    = "generateValidatingAdmissionPolicy"
	GenerateValidatingAdmissionPolicyDescription = "Set the flag to 'false', to disable the generation of ValidatingAdmissionPolicies."
	generateValidatingAdmissionPolicyEnvVar      = "FLAG_GENERATE_VALIDATING_ADMISSION_POLICY"
	defaultGenerateValidatingAdmissionPolicy     = true
	// generate mutating admission policies
	GenerateMutatingAdmissionPolicyFlagName    = "generateMutatingAdmissionPolicy"
	GenerateMutatingAdmissionPolicyDescription = "Set the flag to 'true', to generate mutating admission policies."
	generateMutatingAdmissionPolicyEnvVar      = "FLAG_GENERATE_MUTATING_ADMISSION_POLICY"
	defaultGenerateMutatingAdmissionPolicy     = false
	// dump mutate patches
	DumpMutatePatchesFlagName    = "dumpPatches"
	DumpMutatePatchesDescription = "Set the flag to 'true', to dump mutate patches."
	dumpMutatePatchesEnvVar      = "FLAG_DUMP_PATCHES"
	defaultDumpMutatePatches     = false
	// select autogen
	AutogenV2FlagName    = "autogenV2"
	AutogenV2Description = "Set the flag to 'true', to enable autogen v2."
	autogenV2EnvVar      = "FLAG_AUTOGEN_V2"
	defaultAutogenV2     = false
	// enable http calls in namespaced policies (CVE-2026-4789)
	AllowHTTPInNamespacedPoliciesFlagName    = "allowHTTPInNamespacedPolicies"
	AllowHTTPInNamespacedPoliciesDescription = "Set to 'true' to enable CEL http.Get/Post in namespaced policies. Disabled by default due to SSRF risk (CVE-2026-4789); enable only when combined with restrictive network policies."
	allowHTTPInNamespacedPoliciesEnvVar      = "FLAG_ENABLE_HTTP_IN_NAMESPACED_POLICIES"
	defaultAllowHTTPInNamespacedPolicies     = false
	// http blocklist / allowlist flag names
	HTTPBlocklistFlagName    = "httpBlocklist"
	HTTPBlocklistDescription = "Comma-separated list of CIDR ranges or hostnames blocked from CEL http.Get/Post calls. Overrides the built-in defaults when set. Example: '10.0.0.0/8,metadata.google.internal'."
	httpBlocklistEnvVar      = "FLAG_HTTP_BLOCKLIST"
	HTTPAllowlistFlagName    = "httpAllowlist"
	HTTPAllowlistDescription = "Comma-separated list of URL prefixes (scheme+host[+path]) permitted in CEL http.Get/Post calls. When set, only matching URLs are allowed. Example: 'https://api.example.com,https://webhook.corp/v1/'."
	httpAllowlistEnvVar      = "FLAG_HTTP_ALLOWLIST"
	// api call SSRF blocklist / allowlist
	//
	// NOTE: these gate `context.apiCall.service` only. The CEL `http.Get` / `http.Post`
	// library is gated separately by --httpBlocklist / --httpAllowlist above. The two
	// lists are deliberately distinct: the CEL defaults block the RFC-1918 ranges that
	// in-cluster ClusterIPs live in, which apiCall service calls must keep reaching.
	APICallBlocklistFlagName    = "apiCallBlocklist"
	APICallBlocklistDescription = "Comma-separated list of CIDR ranges or hostnames that context.apiCall service calls may not reach. Overrides the built-in defaults when set; set to an empty value to disable the filter entirely. CEL http.Get/Post is gated separately by --httpBlocklist. Example: '169.254.0.0/16,metadata.google.internal'."
	apiCallBlocklistEnvVar      = "FLAG_APICALL_BLOCKLIST"
	APICallAllowlistFlagName    = "apiCallAllowlist"
	APICallAllowlistDescription = "Comma-separated list of URL prefixes (scheme+host[+path]) that context.apiCall service calls are restricted to. When set, only matching URLs are permitted. CEL http.Get/Post is gated separately by --httpAllowlist. Example: 'https://api.example.com,https://webhook.corp/v1/'."
	apiCallAllowlistEnvVar      = "FLAG_APICALL_ALLOWLIST"
)

var (
	ProtectManagedResources           = newToggle(defaultProtectManagedResources, protectManagedResourcesEnvVar)
	ForceFailurePolicyIgnore          = newToggle(defaultForceFailurePolicyIgnore, forceFailurePolicyIgnoreEnvVar)
	EnableDeferredLoading             = newToggle(defaultEnableDeferredLoading, enableDeferredLoadingEnvVar)
	GenerateValidatingAdmissionPolicy = newToggle(defaultGenerateValidatingAdmissionPolicy, generateValidatingAdmissionPolicyEnvVar)
	GenerateMutatingAdmissionPolicy   = newToggle(defaultGenerateMutatingAdmissionPolicy, generateMutatingAdmissionPolicyEnvVar)
	DumpMutatePatches                 = newToggle(defaultDumpMutatePatches, dumpMutatePatchesEnvVar)
	AutogenV2                         = newToggle(defaultAutogenV2, autogenV2EnvVar)
	AllowHTTPInNamespacedPolicies     = newToggle(defaultAllowHTTPInNamespacedPolicies, allowHTTPInNamespacedPoliciesEnvVar)
	HTTPBlocklist                     = newStringSliceFlag(append(append([]string{}, sdkhttp.DefaultBlockedCIDRs...), sdkhttp.DefaultBlockedHosts...), httpBlocklistEnvVar)
	HTTPAllowlist                     = newStringSliceFlag(nil, httpAllowlistEnvVar)
	APICallBlocklist                  = newStringSliceFlag(defaultAPICallBlocklist, apiCallBlocklistEnvVar)
	APICallAllowlist                  = newStringSliceFlag(nil, apiCallAllowlistEnvVar)
)

type ToggleFlag interface {
	Parse(string) error
}

type Toggle interface {
	ToggleFlag
	Enabled() bool
}

type toggle struct {
	value        *bool
	defaultValue bool
	envVar       string
}

func newToggle(defaultValue bool, envVar string) Toggle {
	return &toggle{
		defaultValue: defaultValue,
		envVar:       envVar,
	}
}

func (t *toggle) Parse(in string) error {
	if value, err := getBool(in); err != nil {
		return err
	} else {
		t.value = value
		return nil
	}
}

func (t *toggle) Enabled() bool {
	if t.value != nil {
		return *t.value
	}
	if value, err := getBool(os.Getenv(t.envVar)); err == nil && value != nil {
		return *value
	}
	return t.defaultValue
}

func getBool(in string) (*bool, error) {
	if in == "" {
		return nil, nil
	}
	value, err := strconv.ParseBool(in)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

// DefaultAPICallBlocklist returns a copy of the built-in apiCall blocklist. Exposed so
// that tests which need to exercise the shipped defaults can select them explicitly
// rather than depending on ambient flag state.
func DefaultAPICallBlocklist() []string {
	return cloneStringSlice(defaultAPICallBlocklist)
}

// StringSliceFlag is a flag that holds a comma-separated list of string values.
// It follows the same precedence as Toggle: CLI flag > environment variable > default.
type StringSliceFlag struct {
	value        []string
	hasValue     bool
	defaultValue []string
	envVar       string
}

func newStringSliceFlag(defaultValue []string, envVar string) *StringSliceFlag {
	return &StringSliceFlag{
		defaultValue: defaultValue,
		envVar:       envVar,
	}
}

// Parse is used as the callback for flagset.Func(...). The input is a comma-separated list of values.
func (f *StringSliceFlag) Parse(in string) error {
	f.value = splitAndTrim(in)
	f.hasValue = true
	return nil
}

// Reset clears any parsed flag value, returning the flag to its unset state so that
// env var and default precedence apply again. Intended for use in tests.
func (f *StringSliceFlag) Reset() {
	f.value = nil
	f.hasValue = false
}

// Values returns the current slice value following CLI > env var > default precedence.
// The returned slice is always a defensive copy so callers cannot mutate internal state.
func (f *StringSliceFlag) Values() []string {
	if f.hasValue {
		return cloneStringSlice(f.value)
	}
	if env, ok := os.LookupEnv(f.envVar); ok {
		return splitAndTrim(env)
	}
	return cloneStringSlice(f.defaultValue)
}

func cloneStringSlice(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func splitAndTrim(in string) []string {
	parts := strings.Split(in, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

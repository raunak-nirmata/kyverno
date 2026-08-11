package license

import (
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/nirmata/license-guardian/pkg/nctllicense"
)

func intPtr(n int) *int { return &n }

// signCommunity produces a community license with a throwaway key pair. The
// production signing key lives only in CI, so every loader test verifies the real
// code path against a key it generates itself.
func signCommunity(t *testing.T, mutate func(*nctllicense.Claims)) (string, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	claims := nctllicense.Claims{
		Customer:     "Nirmata N4K Community",
		NotBefore:    "2020-01-01T00:00:00Z",
		NotAfter:     nctllicense.CommunityNotAfter,
		LicenseType:  "community",
		Entitlements: []string{"n4k.policies"},
		MaxPolicies:  intPtr(10),
		MaxNodeUnits: intPtr(10),
		TrialDays:    intPtr(15),
	}
	if mutate != nil {
		mutate(&claims)
	}
	body, err := nctllicense.EncodeSignedFile(claims, priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(body), pub
}

func TestLoadCommunityHappyPath(t *testing.T) {
	t.Parallel()

	body, pub := signCommunity(t, nil)
	got, err := loadCommunity(body, pub, base)
	if err != nil {
		t.Fatalf("loadCommunity: %v", err)
	}
	if got.Origin != OriginCommunity {
		t.Errorf("Origin = %q, want %q", got.Origin, OriginCommunity)
	}
	if got.Commercial {
		t.Error("a community license must never report as commercial")
	}
	if got.Limits.Policies != Bound(10) {
		t.Errorf("Policies = %+v, want bound of 10", got.Limits.Policies)
	}
	if !got.Limits.NodeUnits.Bounded || got.Limits.NodeUnits.Limit != 10 {
		t.Errorf("NodeUnits = %v, want bound of 10", got.Limits.NodeUnits)
	}
	if got.Limits.Trial != days(15) {
		t.Errorf("Trial = %v, want 15 days", got.Limits.Trial)
	}
	// grace_days was not supplied, so the default applies and is recorded.
	if got.Limits.Grace != days(DefaultGraceDays) {
		t.Errorf("Grace = %v, want the %d-day default", got.Limits.Grace, DefaultGraceDays)
	}
	if len(got.Limits.UsedDefaults) != 1 || got.Limits.UsedDefaults[0] != "grace_days" {
		t.Errorf("UsedDefaults = %v, want exactly [grace_days]", got.Limits.UsedDefaults)
	}
}

// An empty constant is a build defect and must be reported as such — distinctly,
// so the operator-facing log can say "rebuild" rather than "check your license".
func TestLoadCommunityMissingIsABuildDefect(t *testing.T) {
	t.Parallel()

	_, pub := signCommunity(t, nil)
	_, err := loadCommunity("", pub, base)
	if !errors.Is(err, errCommunityLicenseMissing) {
		t.Fatalf("err = %v, want errCommunityLicenseMissing", err)
	}
}

func TestLoadCommunityRejectsTampering(t *testing.T) {
	t.Parallel()

	t.Run("wrong signing key", func(t *testing.T) {
		t.Parallel()
		body, _ := signCommunity(t, nil)
		otherPub, _, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := loadCommunity(body, otherPub, base); err == nil {
			t.Fatal("a license signed by a different key must not verify")
		}
	})

	t.Run("mutated claims payload", func(t *testing.T) {
		t.Parallel()
		body, pub := signCommunity(t, nil)
		// Flip a byte inside the base64 payload line.
		tampered := []byte(body)
		for i, b := range tampered {
			if b == '\n' {
				tampered[i+1] ^= 0x01
				break
			}
		}
		if _, err := loadCommunity(string(tampered), pub, base); err == nil {
			t.Fatal("a license with a mutated payload must not verify")
		}
	})

	t.Run("truncated file", func(t *testing.T) {
		t.Parallel()
		body, pub := signCommunity(t, nil)
		if _, err := loadCommunity(body[:len(body)/2], pub, base); err == nil {
			t.Fatal("a truncated license must not verify")
		}
	})
}

// The community sentinel means the upper bound is skipped, but not_before is
// still enforced — a badly skewed clock must be reported, not ignored.
func TestLoadCommunityHonoursNotBefore(t *testing.T) {
	t.Parallel()

	body, pub := signCommunity(t, nil)
	if _, err := loadCommunity(body, pub, time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("a clock before not_before must be reported")
	}
	// And the sentinel really does mean never expires.
	if _, err := loadCommunity(body, pub, time.Date(2090, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("the community sentinel must never expire, got %v", err)
	}
}

func TestResolveLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		claims        nctllicense.Claims
		wantPolicies  Cap
		wantNodeUnits Cap
		wantTrial     time.Duration
		wantDefaults  int
	}{
		{
			name:          "fully specified",
			claims:        nctllicense.Claims{MaxPolicies: intPtr(500), MaxNodeUnits: intPtr(200), TrialDays: intPtr(7), GraceDays: intPtr(14)},
			wantPolicies:  Bound(500),
			wantNodeUnits: Bound(200),
			wantTrial:     days(7),
			wantDefaults:  0,
		},
		{
			name:          "pre-extension license falls back and records it",
			claims:        nctllicense.Claims{},
			wantPolicies:  Unlimited(),
			wantNodeUnits: Unlimited(),
			wantTrial:     days(DefaultTrialDays),
			wantDefaults:  2,
		},
		{
			name:          "explicit zero caps are preserved, not treated as unlimited",
			claims:        nctllicense.Claims{MaxPolicies: intPtr(0), MaxNodeUnits: intPtr(0), TrialDays: intPtr(0), GraceDays: intPtr(0)},
			wantPolicies:  Bound(0),
			wantNodeUnits: Bound(0),
			wantTrial:     0,
			wantDefaults:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ResolveLimits(tt.claims)
			if got.Policies != tt.wantPolicies {
				t.Errorf("Policies = %+v, want %+v", got.Policies, tt.wantPolicies)
			}
			if got.NodeUnits != tt.wantNodeUnits {
				t.Errorf("NodeUnits = %+v, want %+v", got.NodeUnits, tt.wantNodeUnits)
			}
			if got.Trial != tt.wantTrial {
				t.Errorf("Trial = %v, want %v", got.Trial, tt.wantTrial)
			}
			if len(got.UsedDefaults) != tt.wantDefaults {
				t.Errorf("UsedDefaults = %v, want %d entries", got.UsedDefaults, tt.wantDefaults)
			}
		})
	}
}

// End-to-end: a signed community license drives the state machine to FreeTier
// with the caps the license actually carries. This is the seam where the signed
// artifact meets the evaluator, and it is the reason limits are not hardcoded.
func TestSignedLimitsDriveEvaluation(t *testing.T) {
	t.Parallel()

	body, pub := signCommunity(t, func(c *nctllicense.Claims) {
		c.MaxPolicies = intPtr(3)
		c.TrialDays = intPtr(2)
	})
	active, err := loadCommunity(body, pub, base)
	if err != nil {
		t.Fatal(err)
	}

	inTrial := Evaluate(Inputs{Now: base, TrialStart: base, Limits: active.Limits, PolicyCount: 4})
	if inTrial.State != FreeTier {
		t.Errorf("State = %q, want FreeTier", inTrial.State)
	}
	if !inTrial.OverPolicies {
		t.Error("4 policies against a signed cap of 3 must report as over limit")
	}

	afterTrial := Evaluate(Inputs{Now: base.Add(days(2)), TrialStart: base, Limits: active.Limits})
	if afterTrial.State != FreeExpired {
		t.Errorf("State = %q, want FreeExpired once the signed 2-day trial elapses", afterTrial.State)
	}
}

// A build with no trust root cannot verify anything, so it must report as
// unprovisioned rather than pretending to have checked a license.
func TestTrustRootMissingIsReportedDistinctly(t *testing.T) {
	t.Parallel()

	// Only meaningful while something is still missing. A fully provisioned build is
	// covered by TestLoadActiveWithEmbeddedLicense instead.
	if EmbeddedN4KLicensePublicKeyPEM != "" && EmbeddedN4KCommunityLicense != "" {
		t.Skip("build is fully provisioned; nothing to report as missing")
	}

	_, err := LoadActive(base)
	if err == nil {
		t.Fatal("an unprovisioned build must not report a usable license")
	}
	// Whichever half is absent, it must surface as one of the two "not provisioned"
	// sentinels rather than as a generic failure, because tick routes exactly those
	// two to the quiet path and everything else to a loud one.
	if !errors.Is(err, errTrustRootMissing) && !errors.Is(err, errCommunityLicenseMissing) {
		t.Errorf("err = %v, want either errTrustRootMissing or errCommunityLicenseMissing", err)
	}
}

// The N4K trust root must be N4K's own. Verifying against nctl's key would mean the
// two products could forge each other's licenses.
func TestN4KTrustRootIsNotTheNctlKey(t *testing.T) {
	t.Parallel()

	if EmbeddedN4KLicensePublicKeyPEM == "" {
		t.Skip("N4K public key not provisioned yet")
	}
	if EmbeddedN4KLicensePublicKeyPEM == nctllicense.EmbeddedNCTLLicensePublicKeyPEM {
		t.Fatal("the N4K public key is identical to nctl's; the products must have independent trust roots")
	}
	if _, err := n4kPublicKey(); err != nil {
		t.Errorf("committed N4K public key does not parse: %v", err)
	}
}

// TestLoadActiveWithEmbeddedLicense exercises the real production path: the license
// artifact actually committed in this build, verified against the public key
// actually committed in this build.
//
// It skips while the placeholder is empty. Once a license is embedded — by the
// generate-n4k-community-license workflow, or locally by
// hack/embed-n4k-license.sh — it becomes the check that the two halves match. That
// is the one thing no other test can cover, because every other test supplies its
// own throwaway key pair and so proves nothing about the committed pair.
func TestLoadActiveWithEmbeddedLicense(t *testing.T) {
	t.Parallel()

	if EmbeddedN4KCommunityLicense == "" {
		t.Skip("no license artifact embedded in this build yet")
	}

	active, err := LoadActive(base)
	if err != nil {
		t.Fatalf("the embedded license does not verify against the embedded public key: %v\n"+
			"the two were probably produced from different key pairs", err)
	}

	if active.Origin != OriginCommunity {
		t.Errorf("Origin = %q, want %q", active.Origin, OriginCommunity)
	}
	if active.Commercial {
		t.Error("the community license must not report as commercial")
	}
	if active.Claims.LicenseType != "community" {
		t.Errorf("license_type = %q, want community", active.Claims.LicenseType)
	}

	// The whole point of the artifact is that it carries capacity. If these are
	// unbounded, it was signed by a tool without the capacity flags and every
	// install would silently get an uncapped free tier.
	if !active.Limits.Policies.Bounded {
		t.Error("max_policies is absent — the free tier would be UNLIMITED")
	}
	if !active.Limits.NodeUnits.Bounded {
		t.Error("max_node_units is absent — the free tier would be UNLIMITED")
	}
	if active.Limits.Trial <= 0 {
		t.Errorf("Trial = %v, want a positive trial window", active.Limits.Trial)
	}

	// And it must actually drive the evaluator to a usable state.
	if snap := Evaluate(Inputs{Now: base, TrialStart: base, Limits: active.Limits}); snap.State != FreeTier {
		t.Errorf("State = %q, want FreeTier at the start of the trial", snap.State)
	}

	t.Logf("embedded license OK: customer=%q policies=%s nodeUnits=%s trial=%v",
		active.Claims.Customer, active.Limits.Policies, active.Limits.NodeUnits, active.Limits.Trial)
}

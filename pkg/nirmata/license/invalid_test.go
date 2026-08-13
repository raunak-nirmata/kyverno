package license

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A commercial license file that cannot be verified means the entitlement is
// unknown — not that it is the free-tier one.
//
// Inheriting the community caps here would tell a customer licensed for 200
// policies that they are over a limit of 10, fire limit events saying so, and
// once the grace window elapsed refuse their policy creates outright. A truncated
// Secret or a rotation mid-write is enough to trigger it, so the caps must be
// dropped even though the windows are still needed to run the grace ladder.
func TestInvalidLicenseDoesNotInheritCommunityCaps(t *testing.T) {
	t.Parallel()

	if EmbeddedN4KCommunityLicense == "" {
		t.Skip("no community license embedded in this build")
	}

	path := filepath.Join(t.TempDir(), "license")
	if err := os.WriteFile(path, []byte("this is not a signed license"), 0o600); err != nil {
		t.Fatal(err)
	}

	active, err := LoadActive(time.Now(), path)
	if err != nil {
		t.Fatalf("an unreadable license file must never fail the loader: %v", err)
	}
	if !active.Invalid {
		t.Fatal("Invalid must be true for a present but unverifiable file")
	}
	if active.Limits.Policies.Bounded {
		t.Errorf("Policies cap = %s, want unlimited: the entitlement is unknown, "+
			"so no cap may be reported or enforced", active.Limits.Policies)
	}
	if active.Limits.NodeUnits.Bounded {
		t.Errorf("NodeUnits cap = %s, want unlimited", active.Limits.NodeUnits)
	}
	if active.Limits.Grace == 0 {
		t.Error("the grace window must survive, or the Grace/Degraded ladder cannot run")
	}
}

// With no known cap, an invalid license reports non-compliance without also
// claiming a capacity overrun, and the gate has nothing to enforce.
func TestInvalidLicenseReportsNoOverrun(t *testing.T) {
	t.Parallel()

	now := time.Now()
	snap := Evaluate(Inputs{
		Now:     now,
		Invalid: true,
		Limits: Limits{
			Policies:  Unlimited(),
			NodeUnits: Unlimited(),
			Grace:     days(30),
		},
		PolicyCount:   200,
		NodeUnitCount: 500,
	})

	if snap.State != Grace {
		t.Errorf("State = %s, want Grace on first detection", snap.State)
	}
	if snap.OverPolicies || snap.OverNodeUnits {
		t.Error("an unknown entitlement must not be reported as a capacity overrun")
	}
	if snap.NearPolicies || snap.NearNodeUnits {
		t.Error("an unknown entitlement must not be reported as approaching a limit")
	}
}

func TestWarnRatioIsConfigurable(t *testing.T) {
	t.Parallel()

	cap50 := Bound(50)
	for _, tc := range []struct {
		name  string
		ratio float64
		used  int
		want  bool
	}{
		{"default warns at 90%", 0, 45, true},
		{"default quiet at 88%", 0, 44, false},
		{"override warns earlier", 0.5, 25, true},
		{"override quiet below its band", 0.5, 24, false},
		{"out of range falls back to default", 1.5, 44, false},
		{"negative falls back to default", -1, 45, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			limits := Limits{Policies: cap50, WarnRatio: tc.ratio}
			got := limits.Policies.Near(tc.used, limits.warnRatio())
			if got != tc.want {
				t.Errorf("Near(%d) with ratio %v = %v, want %v", tc.used, tc.ratio, got, tc.want)
			}
		})
	}
}

// Truncating the threshold puts small caps into the warning band far too early:
// a cap of 3 would warn from 2, which is 67% utilization.
func TestNearRoundsRatherThanTruncates(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		limit, used int
		want        bool
	}{
		{3, 2, false},
		{3, 3, true},
		{10, 9, true},
		{10, 8, false},
		{1, 1, true},
		{100, 90, true},
		{100, 89, false},
	} {
		if got := Bound(tc.limit).Near(tc.used, DefaultWarnRatio); got != tc.want {
			t.Errorf("Bound(%d).Near(%d) = %v, want %v", tc.limit, tc.used, got, tc.want)
		}
	}
}

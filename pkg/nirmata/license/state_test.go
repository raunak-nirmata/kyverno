package license

import (
	"testing"
	"time"
)

var (
	base    = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	windows = Limits{
		Policies:  Bound(10),
		NodeUnits: Bound(10),
		Trial:     days(15),
		Grace:     days(30),
		Warn:      days(30),
	}
)

func TestEvaluateFreeTier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		trialStart time.Time
		now        time.Time
		wantState  State
		wantDays   int
	}{
		{
			name:       "trial clock unreadable fails open to FreeTier",
			trialStart: time.Time{},
			now:        base,
			wantState:  FreeTier,
			wantDays:   0,
		},
		{
			name:       "first moment of the trial",
			trialStart: base,
			now:        base,
			wantState:  FreeTier,
			wantDays:   15,
		},
		{
			name:       "midway through the trial",
			trialStart: base,
			now:        base.Add(days(7)),
			wantState:  FreeTier,
			wantDays:   8,
		},
		{
			name:       "one second before trial end is still FreeTier",
			trialStart: base,
			now:        base.Add(days(15)).Add(-time.Second),
			wantState:  FreeTier,
			wantDays:   0,
		},
		{
			name:       "exactly at trial end is expired",
			trialStart: base,
			now:        base.Add(days(15)),
			wantState:  FreeExpired,
			wantDays:   0,
		},
		{
			name:       "long past trial end",
			trialStart: base,
			now:        base.Add(days(365)),
			wantState:  FreeExpired,
			wantDays:   0,
		},
		{
			name:       "trial start in the future does not read as expired",
			trialStart: base.Add(days(5)),
			now:        base,
			wantState:  FreeTier,
			wantDays:   20,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Evaluate(Inputs{
				Now:        tt.now,
				TrialStart: tt.trialStart,
				Limits:     windows,
			})
			if got.State != tt.wantState {
				t.Errorf("State = %q, want %q (reason: %s)", got.State, tt.wantState, got.Reason)
			}
			if got.DaysRemaining != tt.wantDays {
				t.Errorf("DaysRemaining = %d, want %d", got.DaysRemaining, tt.wantDays)
			}
			if got.Reason == "" {
				t.Error("Reason must always be populated")
			}
		})
	}
}

func TestEvaluateCommercialLadder(t *testing.T) {
	t.Parallel()

	expiry := base.Add(days(100))

	tests := []struct {
		name      string
		now       time.Time
		wantState State
	}{
		{"well inside the window", base, Licensed},
		{"one second before the warning window opens", expiry.Add(-days(30)).Add(-time.Second), Licensed},
		{"exactly at the warning boundary", expiry.Add(-days(30)), Warning},
		{"inside the warning window", expiry.Add(-days(1)), Warning},
		{"exactly at expiry is still Warning, not Grace", expiry, Warning},
		{"one second past expiry enters Grace", expiry.Add(time.Second), Grace},
		{"exactly at the end of grace is still Grace", expiry.Add(days(30)), Grace},
		{"one second past grace is Degraded", expiry.Add(days(30)).Add(time.Second), Degraded},
		{"long past grace", expiry.Add(days(400)), Degraded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Evaluate(Inputs{
				Now:        tt.now,
				Commercial: true,
				NotAfter:   expiry,
				Limits:     windows,
			})
			if got.State != tt.wantState {
				t.Errorf("State = %q, want %q (reason: %s)", got.State, tt.wantState, got.Reason)
			}
			if !got.Expiry.Equal(expiry) {
				t.Errorf("Expiry = %v, want %v", got.Expiry, expiry)
			}
		})
	}
}

// A commercial license must take precedence over the trial clock entirely: a
// licensed cluster whose ConfigMap says the trial ended long ago is Licensed.
func TestCommercialIgnoresTrialClock(t *testing.T) {
	t.Parallel()

	got := Evaluate(Inputs{
		Now:        base,
		Commercial: true,
		NotAfter:   base.Add(days(365)),
		TrialStart: base.Add(-days(999)),
		Limits:     windows,
	})
	if got.State != Licensed {
		t.Errorf("State = %q, want Licensed; an expired trial clock must not affect a licensed cluster", got.State)
	}
}

// Phase 1 contract: exceeding a cap is reported but must not change State.
// If this test starts failing, someone has given Phase 1 teeth by accident.
func TestOverLimitIsReportedButDoesNotChangeState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		policies      int
		nodeUnits     int
		wantOverPol   bool
		wantOverNodes bool
	}{
		{"within both caps", 5, 5, false, false},
		{"exactly at both caps", 10, 10, false, false},
		{"one policy over", 11, 5, true, false},
		{"one node unit over", 5, 11, false, true},
		{"far over both", 500, 400, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Evaluate(Inputs{
				Now:           base,
				TrialStart:    base,
				Limits:        windows,
				PolicyCount:   tt.policies,
				NodeUnitCount: tt.nodeUnits,
			})
			if got.OverPolicies != tt.wantOverPol {
				t.Errorf("OverPolicies = %v, want %v", got.OverPolicies, tt.wantOverPol)
			}
			if got.OverNodeUnits != tt.wantOverNodes {
				t.Errorf("OverNodeUnits = %v, want %v", got.OverNodeUnits, tt.wantOverNodes)
			}
			if got.State != FreeTier {
				t.Errorf("State = %q, want FreeTier; Phase 1 must not let capacity overrun change state", got.State)
			}
		})
	}
}

func TestUnlimitedCapIsNeverExceeded(t *testing.T) {
	t.Parallel()

	got := Evaluate(Inputs{
		Now:        base,
		TrialStart: base,
		Limits: Limits{
			Policies:  Unlimited(),
			NodeUnits: Unlimited(),
			Trial:     days(15),
		},
		PolicyCount:   100000,
		NodeUnitCount: 100000,
	})
	if got.OverPolicies || got.OverNodeUnits {
		t.Error("an unlimited cap must never report as exceeded")
	}
}

// A cap of zero is a real bound, distinct from unlimited. This is the pointer
// semantics from the SDK Claims surfacing at the top of the stack.
func TestZeroCapIsARealBound(t *testing.T) {
	t.Parallel()

	got := Evaluate(Inputs{
		Now:        base,
		TrialStart: base,
		Limits: Limits{
			Policies: Bound(0),
			Trial:    days(15),
		},
		PolicyCount: 1,
	})
	if !got.OverPolicies {
		t.Error("a cap of 0 with 1 policy in use must report as exceeded")
	}
}

func TestCapAccessors(t *testing.T) {
	t.Parallel()

	t.Run("unlimited", func(t *testing.T) {
		t.Parallel()
		c := Unlimited()
		if c.Exceeded(1 << 30) {
			t.Error("Exceeded should be false")
		}
		if c.String() != "unlimited" {
			t.Errorf("String = %q, want \"unlimited\"", c.String())
		}
		if c.MetricValue() != -1 {
			t.Errorf("MetricValue = %d, want -1", c.MetricValue())
		}
	})

	t.Run("bounded", func(t *testing.T) {
		t.Parallel()
		c := Bound(10)
		if c.Exceeded(10) {
			t.Error("at the limit is not exceeded")
		}
		if !c.Exceeded(11) {
			t.Error("one over the limit is exceeded")
		}
		if c.String() != "10" {
			t.Errorf("String = %q, want \"10\"", c.String())
		}
		if c.MetricValue() != 10 {
			t.Errorf("MetricValue = %d, want 10", c.MetricValue())
		}
	})
}

// The anti-escalation guard depends on this ordering, so pin it down.
func TestSeverityOrdering(t *testing.T) {
	t.Parallel()

	ordered := []State{Licensed, FreeTier, Warning, Grace, FreeExpired, Degraded}
	for i := 1; i < len(ordered); i++ {
		if severity(ordered[i-1]) >= severity(ordered[i]) {
			t.Errorf("severity(%s) should be less than severity(%s)", ordered[i-1], ordered[i])
		}
	}
	if severity(Unknown) != -1 {
		t.Error("Unknown must sort below every real state so the first evaluation always applies")
	}
	for _, s := range ordered {
		if severity(Unknown) >= severity(s) {
			t.Errorf("severity(Unknown) must be below severity(%s)", s)
		}
	}
}

func TestIsHealthy(t *testing.T) {
	t.Parallel()

	healthy := map[State]bool{Licensed: true, FreeTier: true}
	for _, s := range AllStates {
		if got := s.IsHealthy(); got != healthy[s] {
			t.Errorf("%s.IsHealthy() = %v, want %v", s, got, healthy[s])
		}
	}
}

// Totality: no combination of inputs may yield an empty state or reason.
func TestEvaluateIsTotal(t *testing.T) {
	t.Parallel()

	times := []time.Time{{}, base.Add(-days(400)), base, base.Add(days(400))}
	for _, commercial := range []bool{true, false} {
		for _, now := range times {
			for _, notAfter := range times {
				for _, trialStart := range times {
					got := Evaluate(Inputs{
						Now:        now,
						Commercial: commercial,
						NotAfter:   notAfter,
						TrialStart: trialStart,
						Limits:     windows,
					})
					if got.State == "" || got.State == Unknown {
						t.Fatalf("Evaluate returned %q for commercial=%v now=%v notAfter=%v trialStart=%v",
							got.State, commercial, now, notAfter, trialStart)
					}
					if got.Reason == "" {
						t.Fatalf("empty Reason for commercial=%v now=%v", commercial, now)
					}
					if got.DaysRemaining < 0 {
						t.Fatalf("DaysRemaining = %d, must never be negative", got.DaysRemaining)
					}
				}
			}
		}
	}
}

// A commercial license with no parseable expiry must not read as expired. Every
// arm of the commercial ladder compares against NotAfter, so a zero value would
// otherwise fall through to Degraded — failing closed on a valid license, the one
// outcome this package must never produce.
func TestCommercialWithoutExpiryIsLicensedNotDegraded(t *testing.T) {
	t.Parallel()

	got := Evaluate(Inputs{
		Now:        base,
		Commercial: true,
		NotAfter:   time.Time{},
		Limits:     windows,
	})
	if got.State != Licensed {
		t.Errorf("State = %q, want Licensed (reason: %s)", got.State, got.Reason)
	}
}

// Every state the evaluator can produce must be one the guard can order, or the
// anti-escalation comparison silently treats it as Unknown.
func TestEverySeverityIsDistinctAndOrdered(t *testing.T) {
	t.Parallel()

	seen := map[int]State{}
	for _, s := range AllStates {
		if s == Unknown {
			continue
		}
		v := severity(s)
		if v < 0 {
			t.Errorf("severity(%s) = %d; every real state needs an ordering", s, v)
		}
		if other, dup := seen[v]; dup {
			t.Errorf("severity(%s) == severity(%s) == %d; the guard cannot distinguish them", s, other, v)
		}
		seen[v] = s
	}
}

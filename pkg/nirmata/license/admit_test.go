package license

import (
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
)

// Not parallel: mutates the package globals current, policyCounter and inFlight,
// as does TestGraceAnchorSurvivesAClockReadFailure.
func TestAdmitPolicyCreate(t *testing.T) {
	store := func(snap Snapshot) {
		current.Store(&snap)
		resetInFlight()
		t.Cleanup(func() { current.Store(nil); resetInFlight() })
	}

	t.Run("non-create always allowed", func(t *testing.T) {
		store(Snapshot{
			State: FreeExpired,
			Inputs: Inputs{
				Limits:      Limits{Policies: Bound(10)},
				PolicyCount: 10,
			},
		})
		if err := AdmitPolicyCreate(admissionv1.Update); err != nil {
			t.Fatalf("UPDATE must be allowed, got %v", err)
		}
		if err := AdmitPolicyCreate(admissionv1.Delete); err != nil {
			t.Fatalf("DELETE must be allowed, got %v", err)
		}
	})

	t.Run("unknown fails open", func(t *testing.T) {
		current.Store(nil)
		if err := AdmitPolicyCreate(admissionv1.Create); err != nil {
			t.Fatalf("Unknown must fail open, got %v", err)
		}
	})

	t.Run("licensed allows over-cap create", func(t *testing.T) {
		store(Snapshot{
			State: Licensed,
			Inputs: Inputs{
				Limits:      Limits{Policies: Bound(10)},
				PolicyCount: 50,
			},
		})
		if err := AdmitPolicyCreate(admissionv1.Create); err != nil {
			t.Fatalf("Licensed must allow creates, got %v", err)
		}
	})

	t.Run("grace allows over-cap create", func(t *testing.T) {
		store(Snapshot{
			State: Grace,
			Inputs: Inputs{
				Limits:      Limits{Policies: Bound(10)},
				PolicyCount: 50,
			},
		})
		if err := AdmitPolicyCreate(admissionv1.Create); err != nil {
			t.Fatalf("Grace must preserve full function, got %v", err)
		}
	})

	t.Run("free expired at cap refuses", func(t *testing.T) {
		store(Snapshot{
			State: FreeExpired,
			Inputs: Inputs{
				Limits:      Limits{Policies: Bound(10)},
				PolicyCount: 10,
			},
		})
		err := AdmitPolicyCreate(admissionv1.Create)
		if err == nil {
			t.Fatal("FreeExpired at cap must refuse CREATE")
		}
		if !strings.Contains(err.Error(), "limit reached") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("degraded at cap refuses", func(t *testing.T) {
		store(Snapshot{
			State: Degraded,
			Inputs: Inputs{
				Limits:      Limits{Policies: Bound(10)},
				PolicyCount: 10,
			},
		})
		if err := AdmitPolicyCreate(admissionv1.Create); err == nil {
			t.Fatal("Degraded at cap must refuse CREATE")
		}
	})

	t.Run("free tier under cap allows", func(t *testing.T) {
		store(Snapshot{
			State: FreeTier,
			Inputs: Inputs{
				Limits:      Limits{Policies: Bound(10)},
				PolicyCount: 9,
			},
		})
		if err := AdmitPolicyCreate(admissionv1.Create); err != nil {
			t.Fatalf("FreeTier under cap must allow CREATE, got %v", err)
		}
	})

	t.Run("unlimited cap never refuses", func(t *testing.T) {
		store(Snapshot{
			State: Degraded,
			Inputs: Inputs{
				Limits:      Limits{Policies: Unlimited()},
				PolicyCount: 100000,
			},
		})
		if err := AdmitPolicyCreate(admissionv1.Create); err != nil {
			t.Fatalf("unlimited cap must allow CREATE, got %v", err)
		}
	})

	// The snapshot count is refreshed once per evaluation interval. Enforcing
	// against it alone lets a cluster at the cap admit unlimited creates until the
	// next tick, so a live counter must win when one is installed.
	counter := func(t *testing.T, n int, ok bool) {
		t.Helper()
		SetPolicyCounter(func() (int, bool) { return n, ok })
		t.Cleanup(func() { SetPolicyCounter(nil) })
	}

	t.Run("live count refuses even when the snapshot is stale and under cap", func(t *testing.T) {
		store(Snapshot{
			State: FreeTier,
			Inputs: Inputs{
				Limits:      Limits{Policies: Bound(10)},
				PolicyCount: 3, // stale: written at the last evaluation
			},
		})
		counter(t, 10, true)
		if err := AdmitPolicyCreate(admissionv1.Create); err == nil {
			t.Fatal("a live count at the cap must refuse, regardless of the stale snapshot count")
		}
	})

	t.Run("live count allows after a delete the snapshot has not seen", func(t *testing.T) {
		store(Snapshot{
			State: FreeExpired,
			Inputs: Inputs{
				Limits:      Limits{Policies: Bound(10)},
				PolicyCount: 10, // stale: two policies have since been deleted
			},
		})
		counter(t, 8, true)
		if err := AdmitPolicyCreate(admissionv1.Create); err != nil {
			t.Fatalf("a live count under the cap must allow CREATE, got %v", err)
		}
	})

	t.Run("unsynced live count falls back to the snapshot", func(t *testing.T) {
		store(Snapshot{
			State: FreeExpired,
			Inputs: Inputs{
				Limits:      Limits{Policies: Bound(10)},
				PolicyCount: 10,
			},
		})
		counter(t, 0, false) // caches not synced: zero would look like plenty of room
		if err := AdmitPolicyCreate(admissionv1.Create); err == nil {
			t.Fatal("an untrusted live count must fall back to the snapshot, not admit on zero")
		}
	})

	t.Run("error message reports the enforced count", func(t *testing.T) {
		store(Snapshot{
			State: FreeExpired,
			Inputs: Inputs{
				Limits:      Limits{Policies: Bound(10)},
				PolicyCount: 3,
			},
		})
		counter(t, 12, true)
		err := AdmitPolicyCreate(admissionv1.Create)
		if err == nil {
			t.Fatal("expected a refusal")
		}
		if !strings.Contains(err.Error(), "12 policies in use") {
			t.Errorf("message must quote the live count, got: %v", err)
		}
	})
}

func TestEnforcesPolicyCap(t *testing.T) {
	t.Parallel()

	want := map[State]bool{
		FreeTier:    true,
		FreeExpired: true,
		Degraded:    true,
		Licensed:    false,
		Warning:     false,
		Grace:       false,
		Unknown:     false,
	}
	for _, s := range AllStates {
		if got := s.EnforcesPolicyCap(); got != want[s] {
			t.Errorf("%s.EnforcesPolicyCap() = %v, want %v", s, got, want[s])
		}
	}
}

func TestIsValid(t *testing.T) {
	t.Parallel()

	want := map[State]bool{
		Licensed:    true,
		Warning:     true,
		FreeTier:    true,
		Grace:       false,
		FreeExpired: false,
		Degraded:    false,
		Unknown:     false,
	}
	for _, s := range AllStates {
		if got := s.IsValid(); got != want[s] {
			t.Errorf("%s.IsValid() = %v, want %v", s, got, want[s])
		}
	}
}

// A license whose window has not opened yet must not grant paid capacity.
func TestNotYetValidCommercialIsNonCompliant(t *testing.T) {
	now := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	in := Inputs{
		Now:        now,
		Commercial: true,
		NotBefore:  now.Add(30 * 24 * time.Hour),
		NotAfter:   now.Add(400 * 24 * time.Hour),
		Limits:     Limits{Policies: Bound(13), Grace: days(30)},
	}
	got := Evaluate(in)
	if got.State == Licensed || got.State == Warning {
		t.Fatalf("state = %s, want the non-compliance ladder: a license starting %s must not grant capacity today",
			got.State, in.NotBefore.Format(time.RFC3339))
	}
	if !strings.Contains(got.Reason, "not yet valid") {
		t.Errorf("reason = %q, want it to say the license is not yet valid", got.Reason)
	}
	// Already-started licenses must be unaffected.
	in.NotBefore = now.Add(-24 * time.Hour)
	if got := Evaluate(in); got.State != Licensed {
		t.Errorf("with not_before in the past, state = %s, want Licensed", got.State)
	}
	// An absent not_before means "in force since issue".
	in.NotBefore = time.Time{}
	if got := Evaluate(in); got.State != Licensed {
		t.Errorf("with no not_before, state = %s, want Licensed", got.State)
	}
}

// The gate must not admit a burst larger than the cap while the informer cache
// catches up: at 8 of 10, applying five policies may only yield two admits.
func TestGateCountsItsOwnUnobservedAdmits(t *testing.T) {
	current.Store(&Snapshot{
		State:  FreeTier,
		Inputs: Inputs{Limits: Limits{Policies: Bound(10)}, PolicyCount: 8},
	})
	t.Cleanup(func() { current.Store(nil); SetPolicyCounter(nil); resetInFlight() })

	resetInFlight()
	live := 8 // cache is frozen at 8 for the whole burst
	SetPolicyCounter(func() (int, bool) { return live, true })

	admitted := 0
	for i := 0; i < 5; i++ {
		if err := AdmitPolicyCreate(admissionv1.Create); err == nil {
			admitted++
		}
	}
	if admitted != 2 {
		t.Fatalf("admitted %d of 5 against a frozen count of 8/10, want exactly 2", admitted)
	}

	// Once the cache catches up, the tally resets and capacity is judged afresh.
	live = 10
	if err := AdmitPolicyCreate(admissionv1.Create); err == nil {
		t.Error("at a live count of 10/10 the gate must refuse")
	}
	live = 9
	if err := AdmitPolicyCreate(admissionv1.Create); err != nil {
		t.Errorf("at a live count of 9/10 the gate must admit, got %v", err)
	}
}

// TestGateAdmitsAfterDeleteFreesCapacity is the regression for a gate that
// wedged shut. Filling to the cap leaves the in-flight tally anchored to the
// live count it last admitted against; deleting a policy returns the live count
// to exactly that number, which the tally could not tell apart from a lagging
// cache. The phantom admit was then added on every request, so a cluster sitting
// at 9 of 10 refused every CREATE — for good, until the process restarted —
// while the evaluator reported it comfortably under its cap.
func TestGateAdmitsAfterDeleteFreesCapacity(t *testing.T) {
	current.Store(&Snapshot{
		State:  FreeTier,
		Inputs: Inputs{Limits: Limits{Policies: Bound(10)}, PolicyCount: 9},
	})
	now := time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)
	gateNow = func() time.Time { return now }
	t.Cleanup(func() {
		current.Store(nil)
		SetPolicyCounter(nil)
		resetInFlight()
		gateNow = time.Now
	})

	resetInFlight()
	live := 9
	SetPolicyCounter(func() (int, bool) { return live, true })

	// The 10th policy is created while the cache still shows 9.
	if err := AdmitPolicyCreate(admissionv1.Create); err != nil {
		t.Fatalf("10th create at a live count of 9/10 must admit, got %v", err)
	}

	// The cache catches up, then an operator deletes a policy to free capacity.
	// The live count lands back on 9 — the same number the tally is anchored to.
	live = 9

	// Inside the in-flight window the refusal is correct: this is exactly the
	// shape of a burst whose earlier admits have not landed yet.
	if err := AdmitPolicyCreate(admissionv1.Create); err == nil {
		t.Error("within the in-flight window an unobserved admit must still count")
	}

	// Past it, informer lag is no longer a credible explanation and the cluster
	// is under its cap, so the create must go through.
	now = now.Add(inFlightTTL + time.Second)
	if err := AdmitPolicyCreate(admissionv1.Create); err != nil {
		t.Fatalf("at 9/10 past the in-flight window the gate must admit, got %v", err)
	}
}

// TestGateTallyClearsWhenLiveCountDrops covers the other half of the anchor
// rule: a delete that moves the live count off the anchored number must retire
// the tally immediately, with no wait for the TTL.
func TestGateTallyClearsWhenLiveCountDrops(t *testing.T) {
	current.Store(&Snapshot{
		State:  FreeTier,
		Inputs: Inputs{Limits: Limits{Policies: Bound(10)}, PolicyCount: 9},
	})
	t.Cleanup(func() { current.Store(nil); SetPolicyCounter(nil); resetInFlight() })

	resetInFlight()
	live := 9
	SetPolicyCounter(func() (int, bool) { return live, true })

	if err := AdmitPolicyCreate(admissionv1.Create); err != nil {
		t.Fatalf("create at 9/10 must admit, got %v", err)
	}
	// Two policies deleted: the live count moves off the anchor.
	live = 7
	if err := AdmitPolicyCreate(admissionv1.Create); err != nil {
		t.Fatalf("create at 7/10 must admit, got %v", err)
	}
}

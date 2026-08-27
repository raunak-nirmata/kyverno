package license

import (
	"fmt"
	"sync/atomic"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
)

// PolicyCounter reports how many policies are installed right now.
//
// ok is false when the number cannot be trusted — informer caches not yet synced,
// or a lister failed — in which case the gate falls back to the last evaluated
// count rather than guessing.
type PolicyCounter func() (count int, ok bool)

// policyCounter is the live counter used by the admission gate, if one was
// installed. Package-level rather than a field because the gate is a free
// function called from the webhook handler, which has no license service handle.
var policyCounter atomic.Pointer[PolicyCounter]

// SetPolicyCounter installs the live policy counter used by AdmitPolicyCreate.
// Passing nil removes it, which returns the gate to the evaluated count.
func SetPolicyCounter(fn PolicyCounter) {
	if fn == nil {
		policyCounter.Store(nil)
		return
	}
	policyCounter.Store(&fn)
}

// inFlight counts CREATEs this process has admitted but not yet observed in the
// informer cache, alongside the observed count they were admitted against.
//
// The cache is eventually consistent, so read-then-admit overshoots on perfectly
// ordinary usage: `kubectl apply -f dir/` at 8 of 10 can admit five policies
// before the first lands, leaving 13. Counting what this process already waved
// through closes that window with no coordination and no API call on the
// admission path.
//
// Deliberately not an atomic quota. Replicas keep separate counters, so a burst
// spread across replicas can still overshoot briefly; the next evaluation reports
// the overrun and further CREATEs are refused. The gate is a capacity control,
// not a distributed semaphore — reserving capacity per request would add a new
// failure mode to a path whose first duty is to stay open.
var inFlight struct {
	admitted atomic.Int64
	observed atomic.Int64

	// lastAdmit is when the tally was last added to, as Unix nanoseconds. Zero
	// means the tally is empty. See inFlightTTL.
	lastAdmit atomic.Int64
}

// inFlightTTL bounds how long an unobserved admit keeps counting against
// capacity.
//
// The tally exists to cover informer lag, which is sub-second; a burst of
// creates from one `kubectl apply -f dir/` spans a couple of seconds at most.
// Past this window the tally is assumed stale and dropped.
//
// Without the bound the gate can wedge shut. A delete that returns the live
// count to exactly the value the tally is anchored to is indistinguishable, at
// this layer, from a cache that has not caught up — so the phantom admit is
// added forever and every CREATE is refused on a cluster that is provably under
// its cap, until the process restarts. Erring toward a brief overshoot rather
// than a permanent refusal is the same trade this gate makes everywhere else:
// it is a capacity control, not a distributed semaphore.
const inFlightTTL = 10 * time.Second

// gateNow is the gate's time source, injected so tests are deterministic.
var gateNow = time.Now

// resetInFlight clears the tally. Test-only.
func resetInFlight() {
	inFlight.admitted.Store(0)
	inFlight.observed.Store(0)
	inFlight.lastAdmit.Store(0)
}

// noteAdmitted records a CREATE the gate just allowed through, against the live
// count it was judged on. A move in that count — in either direction — means the
// tally's anchor is gone, so it restarts from the new observation.
func noteAdmitted(observed int) {
	if observed < 0 {
		return // no live counter; nothing meaningful to track against
	}
	if prev := inFlight.observed.Swap(int64(observed)); int64(observed) != prev {
		inFlight.admitted.Store(0)
	}
	inFlight.admitted.Add(1)
	inFlight.lastAdmit.Store(gateNow().UnixNano())
}

// gatePolicyCount is the count the gate enforces against: the live count plus
// this process's unobserved admits when a counter is installed, otherwise the
// count from the last evaluation. observed is the raw live count, or -1 when
// there is none, and is what noteAdmitted tracks catch-up against.
func gatePolicyCount(snap Snapshot) (count int, observed int) {
	if fn := policyCounter.Load(); fn != nil {
		if n, ok := (*fn)(); ok {
			pending := inFlight.admitted.Load()
			switch {
			case int64(n) != inFlight.observed.Load():
				// The tally is anchored to one observation of the live count. Any
				// move off that number retires the anchor: upward means the cache
				// caught up and those admits are inside n now, downward means a
				// delete landed. Either way the tally no longer describes n.
				inFlight.admitted.Store(0)
				pending = 0
			case inFlight.lastAdmit.Load() != 0 &&
				gateNow().UnixNano()-inFlight.lastAdmit.Load() > int64(inFlightTTL):
				inFlight.admitted.Store(0)
				pending = 0
			}
			return n + int(pending), n
		}
	}
	// No live counter: the evaluated count is already minutes old, so adding
	// in-flight admits to it would compound one staleness with another.
	return snap.Inputs.PolicyCount, -1
}

// AdmitPolicyCreate is the safe capacity gate for policy CREATE requests.
//
// Fail-open rules:
//   - Non-CREATE operations always succeed (existing policies are never disabled).
//   - Unknown state (evaluator not ready) always succeeds.
//   - Licensed / Warning / Grace always succeed (full function, including over-cap).
//   - An unbounded cap, or an unreadable licence whose entitlement is unknown,
//     always succeeds.
//   - FreeTier / FreeExpired / Degraded refuse CREATE when already at or over the
//     policy cap. Core workload admission is never gated here.
func AdmitPolicyCreate(operation admissionv1.Operation) error {
	if operation != admissionv1.Create {
		return nil
	}
	snap := Current()
	if snap.State == Unknown {
		return nil
	}
	if !snap.State.EnforcesPolicyCap() {
		return nil
	}
	limit := snap.Inputs.Limits.Policies
	if !limit.Bounded {
		return nil
	}
	count, observed := gatePolicyCount(snap)
	if !limit.AtLimit(count) {
		noteAdmitted(observed)
		return nil
	}
	// Capitalised and user-facing on purpose: this string is surfaced to whoever
	// ran kubectl, not logged as a Go error.
	return fmt.Errorf(
		"Nirmata license policy limit reached: %d policies in use, %s permitted "+
			"(state=%s). Obtain or renew a license to create additional policies; "+
			"existing policies continue to enforce",
		count, limit, snap.State,
	)
}

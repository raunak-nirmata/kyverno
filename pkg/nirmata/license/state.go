package license

import "time"

// State is the licensing posture reported for the cluster. In Phase 1 only Unknown,
// FreeTier and FreeExpired are reachable; the commercial states are implemented and
// tested so Phase 2 only has to supply a loader.
type State string

const (
	// Unknown is before the first evaluation completes. Never a problem.
	Unknown State = "Unknown"

	// FreeTier is no commercial license, inside the trial window.
	FreeTier State = "FreeTier"

	// FreeExpired is no commercial license, trial elapsed. Skips the grace window.
	FreeExpired State = "FreeExpired"

	// Licensed is a valid commercial license, comfortably inside its window.
	Licensed State = "Licensed"

	// Warning is a valid commercial license approaching expiry.
	Warning State = "Warning"

	// Grace is an expired commercial license still inside its grace window.
	Grace State = "Grace"

	// Degraded is a commercial license past both expiry and grace.
	Degraded State = "Degraded"
)

// AllStates lists every state, for one-hot metric reporting.
var AllStates = []State{Unknown, FreeTier, FreeExpired, Licensed, Warning, Grace, Degraded}

// severity orders states healthiest to worst, for the anti-escalation guard: on
// stale inputs a new state applies only if it is no worse. A transient API error
// must never move a cluster toward Degraded.
func severity(s State) int {
	switch s {
	case Licensed:
		return 0
	case FreeTier:
		return 1
	case Warning:
		return 2
	case Grace:
		return 3
	case FreeExpired:
		return 4
	case Degraded:
		return 5
	case Unknown:
		return -1
	default:
		return -1
	}
}

// IsHealthy reports whether the state needs no operator attention.
func (s State) IsHealthy() bool { return s == Licensed || s == FreeTier }

// Inputs is everything Evaluate needs — no clients, no clock, no logger.
type Inputs struct {
	// Now is injected so the state machine is deterministic.
	Now time.Time

	// Commercial is true when a valid commercial license was loaded. Phase 2 only.
	Commercial bool

	// NotAfter is the commercial license expiry. Zero when Commercial is false.
	NotAfter time.Time

	// TrialStart anchors the free-tier window. Zero means unreadable, which must
	// never be treated as expiry — see Evaluate.
	TrialStart time.Time

	Limits Limits

	PolicyCount   int
	NodeUnitCount int
}

// Snapshot is one evaluation's result, and the single source logs, events and
// metrics all derive from.
type Snapshot struct {
	State  State
	Reason string
	Inputs Inputs

	// OverPolicies and OverNodeUnits report capacity overrun. Deliberately not
	// inputs to State in Phase 1: overrun is reported but changes nothing.
	OverPolicies  bool
	OverNodeUnits bool

	// Expiry is when the current window ends. Zero when not applicable.
	Expiry time.Time

	// DaysRemaining is whole days until Expiry, floored at zero.
	DaysRemaining int

	EvaluatedAt time.Time
}

// Evaluate maps observed facts to a licensing state. Pure and total: every input
// combination yields exactly one state, and it never errors. Check order is the
// specification — the commercial ladder runs healthiest to worst, first match wins.
func Evaluate(in Inputs) Snapshot {
	s := Snapshot{Inputs: in, EvaluatedAt: in.Now}

	s.OverPolicies = in.Limits.Policies.Exceeded(in.PolicyCount)
	s.OverNodeUnits = in.Limits.NodeUnits.Exceeded(in.NodeUnitCount)

	switch {
	// Without this, a zero NotAfter falls through the ladder to Degraded — failing
	// closed on a license that is present and valid.
	case in.Commercial && in.NotAfter.IsZero():
		s.State = Licensed
		s.Reason = "commercial license valid; no expiry recorded"

	case in.Commercial:
		s.Expiry = in.NotAfter
		switch {
		case in.Now.Before(in.NotAfter.Add(-in.Limits.Warn)):
			s.State = Licensed
			s.Reason = "commercial license valid"
		case !in.Now.After(in.NotAfter):
			s.State = Warning
			s.Reason = "commercial license approaching expiry"
		case !in.Now.After(in.NotAfter.Add(in.Limits.Grace)):
			s.State = Grace
			s.Reason = "commercial license expired, within grace period"
		default:
			s.State = Degraded
			s.Reason = "commercial license expired and grace period elapsed"
		}

	// Fail-open keystone: an unreadable trial clock reports FreeTier, never
	// FreeExpired.
	case in.TrialStart.IsZero():
		s.State = FreeTier
		s.Reason = "no commercial license; trial start unknown, assuming trial is active"

	default:
		s.Expiry = in.TrialStart.Add(in.Limits.Trial)
		if in.Now.Before(s.Expiry) {
			s.State = FreeTier
			s.Reason = "no commercial license; within trial window"
		} else {
			s.State = FreeExpired
			s.Reason = "no commercial license; trial window elapsed"
		}
	}

	s.DaysRemaining = wholeDaysUntil(in.Now, s.Expiry)
	return s
}

// wholeDaysUntil returns the number of complete days between now and deadline,
// floored at zero. A zero deadline yields zero.
func wholeDaysUntil(now, deadline time.Time) int {
	if deadline.IsZero() {
		return 0
	}
	remaining := deadline.Sub(now)
	if remaining <= 0 {
		return 0
	}
	return int(remaining / (24 * time.Hour))
}

package license

import "time"

// State is the licensing posture reported for the cluster.
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

	// Warning is a valid commercial license approaching expiry or capacity.
	Warning State = "Warning"

	// Grace is a previously-valid commercial license that is now non-compliant,
	// still inside the grace window measured from first detection.
	Grace State = "Grace"

	// Degraded is commercial non-compliance past the grace window.
	Degraded State = "Degraded"
)

// AllStates lists every state, for one-hot metric reporting.
var AllStates = []State{Unknown, FreeTier, FreeExpired, Licensed, Warning, Grace, Degraded}

// DefaultWarnRatio is the utilization fraction (inclusive) at which a valid
// commercial license enters Warning for capacity. Over-cap is Grace/Degraded.
//
// A default, not a constant of the model: Options.WarnRatio and the
// --licenseWarnRatio flag override it, so the threshold is configurable like
// every other one. Claims carry no ratio field today.
const DefaultWarnRatio = 0.9

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

// EnforcesPolicyCap reports whether new over-cap policy creates must be refused.
func (s State) EnforcesPolicyCap() bool {
	return s == FreeTier || s == FreeExpired || s == Degraded
}

// IsValid reports whether the license posture is commercially/trial-valid for
// the license_valid metric (1 = healthy or warning free/paid; 0 otherwise).
func (s State) IsValid() bool {
	return s == Licensed || s == Warning || s == FreeTier
}

// Inputs is everything Evaluate needs — no clients, no clock, no logger.
type Inputs struct {
	// Now is injected so the state machine is deterministic.
	Now time.Time

	// Commercial is true when a verified commercial license was loaded. The
	// license may be past not_after; Evaluate decides Grace vs Licensed.
	Commercial bool

	// Invalid is true when a commercial license file was present but unusable.
	Invalid bool

	// NotAfter is the commercial license expiry. Zero when Commercial is false
	// or the license carries no real expiry.
	NotAfter time.Time

	// NotBefore is the start of the commercial validity window. Zero means the
	// license carries no start bound, i.e. it is already in force.
	NotBefore time.Time

	// TrialStart anchors the free-tier window. Zero means unreadable, which must
	// never be treated as expiry — see Evaluate.
	TrialStart time.Time

	// GraceStartedAt is first detection of paid non-compliance. Zero means this
	// evaluation is the first detection (Evaluate treats grace as starting Now).
	GraceStartedAt time.Time

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

	// OverPolicies and OverNodeUnits report capacity overrun.
	OverPolicies  bool
	OverNodeUnits bool

	// NearPolicies and NearNodeUnits report utilization in the warning band
	// [WarnRatio, 100%] of a bounded cap.
	NearPolicies  bool
	NearNodeUnits bool

	// Expiry is when the current window ends. Zero when not applicable.
	Expiry time.Time

	// DaysRemaining is whole days until Expiry, floored at zero.
	DaysRemaining int

	EvaluatedAt time.Time
}

// Evaluate maps observed facts to a licensing state. Pure and total: every input
// combination yields exactly one state, and it never errors.
func Evaluate(in Inputs) Snapshot {
	s := Snapshot{Inputs: in, EvaluatedAt: in.Now}

	ratio := in.Limits.warnRatio()
	s.OverPolicies = in.Limits.Policies.Exceeded(in.PolicyCount)
	s.OverNodeUnits = in.Limits.NodeUnits.Exceeded(in.NodeUnitCount)
	s.NearPolicies = in.Limits.Policies.Near(in.PolicyCount, ratio)
	s.NearNodeUnits = in.Limits.NodeUnits.Near(in.NodeUnitCount, ratio)

	switch {
	case in.Invalid:
		return finishCommercialNonCompliant(s, in, "commercial license invalid")

	// A correctly signed license whose window has not opened yet must not grant
	// paid capacity. Checked before the expiry branches so it applies whether or
	// not a real not_after is recorded, and routed through the same ladder as an
	// expired license rather than rejected outright — a misdated license should
	// degrade visibly over the grace window, not fail closed on day one.
	case in.Commercial && !in.NotBefore.IsZero() && in.Now.Before(in.NotBefore):
		return finishCommercialNonCompliant(s, in, "commercial license not yet valid")

	// Without this, a zero NotAfter falls through the ladder to Degraded — failing
	// closed on a license that is present and valid.
	case in.Commercial && in.NotAfter.IsZero():
		if s.OverPolicies || s.OverNodeUnits {
			return finishCommercialNonCompliant(s, in, "licensed capacity exceeded")
		}
		if s.NearPolicies || s.NearNodeUnits {
			s.State = Warning
			s.Reason = "licensed capacity approaching limit"
			s.DaysRemaining = 0
			return s
		}
		s.State = Licensed
		s.Reason = "commercial license valid; no expiry recorded"
		s.DaysRemaining = 0
		return s

	case in.Commercial:
		s.Expiry = in.NotAfter
		expired := in.Now.After(in.NotAfter)
		over := s.OverPolicies || s.OverNodeUnits
		switch {
		case expired || over:
			reason := "commercial license expired"
			if over && !expired {
				reason = "licensed capacity exceeded"
			} else if over && expired {
				reason = "commercial license expired and licensed capacity exceeded"
			}
			return finishCommercialNonCompliant(s, in, reason)
		case !in.Now.Before(in.NotAfter.Add(-in.Limits.Warn)) || s.NearPolicies || s.NearNodeUnits:
			s.State = Warning
			switch {
			case !in.Now.Before(in.NotAfter.Add(-in.Limits.Warn)) && (s.NearPolicies || s.NearNodeUnits):
				s.Reason = "commercial license approaching expiry and capacity limit"
			case s.NearPolicies || s.NearNodeUnits:
				s.Reason = "licensed capacity approaching limit"
			default:
				s.Reason = "commercial license approaching expiry"
			}
		default:
			s.State = Licensed
			s.Reason = "commercial license valid"
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

// finishCommercialNonCompliant applies the Grace/Degraded ladder measured from
// first detection (GraceStartedAt), not from NotAfter.
func finishCommercialNonCompliant(s Snapshot, in Inputs, reason string) Snapshot {
	start := in.GraceStartedAt
	if start.IsZero() {
		start = in.Now
	}
	s.Expiry = start.Add(in.Limits.Grace)
	if !in.Now.After(s.Expiry) {
		s.State = Grace
		s.Reason = reason + ", within grace period"
	} else {
		s.State = Degraded
		s.Reason = reason + ", grace period elapsed"
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

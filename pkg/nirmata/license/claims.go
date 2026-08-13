package license

import (
	"github.com/nirmata/license-guardian/pkg/nctllicense"
)

// ResolveLimits maps signed claims onto the bounds and windows the evaluator uses.
//
// The absent-versus-zero distinction is carried through verbatim: no max_policies
// means unlimited, max_policies=0 means none permitted.
//
// Windows fall back to package defaults for licenses issued before the capacity
// claims existed, recorded in Limits.UsedDefaults so the caller can say so.
func ResolveLimits(c nctllicense.Claims) Limits {
	l := Limits{
		Policies:  Unlimited(),
		NodeUnits: Unlimited(),
		Warn:      days(DefaultWarnDays),
		WarnRatio: DefaultWarnRatio,
	}

	if limit, bounded := c.PolicyCap(); bounded {
		l.Policies = Bound(limit)
	}
	if limit, bounded := c.NodeUnitCap(); bounded {
		l.NodeUnits = Bound(limit)
	}

	if d, set := c.TrialWindow(); set {
		l.Trial = days(d)
	} else {
		l.Trial = days(DefaultTrialDays)
		l.UsedDefaults = append(l.UsedDefaults, "trial_days")
	}

	if d, set := c.Grace(); set {
		l.Grace = days(d)
	} else {
		l.Grace = days(DefaultGraceDays)
		l.UsedDefaults = append(l.UsedDefaults, "grace_days")
	}

	return l
}

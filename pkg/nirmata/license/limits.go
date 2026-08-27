package license

import (
	"math"
	"strconv"
	"time"
)

// Fallback windows, used only when the active license does not carry its own. The
// real free-tier numbers live in the signed community license; falling back here is
// logged. See README.md.
const (
	DefaultTrialDays = 15
	DefaultGraceDays = 30

	// DefaultWarnDays is the pre-expiry warning window. Not carried in claims today.
	DefaultWarnDays = 30
)

// Cap is a capacity limit that may or may not be bounded. An omitted limit means
// unlimited; a limit of zero means none permitted. Pairing the bound with its
// presence flag makes that impossible to conflate at a call site.
type Cap struct {
	Limit   int
	Bounded bool
}

// Unlimited returns a Cap that imposes no bound.
func Unlimited() Cap { return Cap{} }

// Bound returns a Cap limited to n, where n may legitimately be 0.
func Bound(n int) Cap { return Cap{Limit: n, Bounded: true} }

// Exceeded reports whether used is over the limit. Always false when unbounded.
func (c Cap) Exceeded(used int) bool { return c.Bounded && used > c.Limit }

// AtLimit reports whether used is at or over the limit. Used by the CREATE gate
// so a cluster already at the cap cannot add another policy.
func (c Cap) AtLimit(used int) bool { return c.Bounded && used >= c.Limit }

// Near reports whether used is in the warning band [ratio*limit, limit].
// Always false when unbounded, when limit is zero, or when already over.
//
// The threshold is rounded, not truncated: truncation puts small caps into the
// warning band far too early — a cap of 3 at ratio 0.9 would warn from 2 (67%).
func (c Cap) Near(used int, ratio float64) bool {
	if !c.Bounded || c.Limit <= 0 || used > c.Limit {
		return false
	}
	threshold := int(math.Round(float64(c.Limit) * ratio))
	if threshold < 1 {
		threshold = 1
	}
	return used >= threshold
}

// String renders the cap for logs, as "unlimited" rather than a number so a log
// line cannot be misread as a limit of zero.
func (c Cap) String() string {
	if !c.Bounded {
		return "unlimited"
	}
	return strconv.Itoa(c.Limit)
}

// MetricValue renders the cap for a gauge. Unlimited is -1 rather than an absent
// series, so dashboards always have a value and can tell it apart from zero.
func (c Cap) MetricValue() int64 {
	if !c.Bounded {
		return -1
	}
	return int64(c.Limit)
}

// Limits is the full set of capacity bounds and windows in effect.
type Limits struct {
	Policies  Cap
	NodeUnits Cap

	// Trial is measured from the cluster's first-seen timestamp.
	Trial time.Duration

	// Grace is how long an expired commercial license keeps reporting normally.
	Grace time.Duration

	// Warn is how long before expiry a commercial license starts warning.
	Warn time.Duration

	// WarnRatio is the utilization fraction at which a bounded cap starts
	// warning. Zero means unset, in which case warnRatio() supplies the default
	// — Evaluate must stay total, so an unset ratio can never mean "never warn".
	WarnRatio float64

	// UsedDefaults names the windows that fell back to the constants above.
	// Non-empty means the active license should be re-issued.
	UsedDefaults []string
}

// warnRatio returns the capacity warning ratio in effect, falling back to
// DefaultWarnRatio when unset or out of the open interval (0, 1].
func (l Limits) warnRatio() float64 {
	if l.WarnRatio <= 0 || l.WarnRatio > 1 {
		return DefaultWarnRatio
	}
	return l.WarnRatio
}

// days converts a whole number of days to a Duration.
func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

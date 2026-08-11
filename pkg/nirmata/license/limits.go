package license

import (
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

	// UsedDefaults names the windows that fell back to the constants above.
	// Non-empty means the active license should be re-issued.
	UsedDefaults []string
}

// days converts a whole number of days to a Duration.
func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

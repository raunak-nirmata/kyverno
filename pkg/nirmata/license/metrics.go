package license

import (
	"context"

	"github.com/go-logr/logr"
	"github.com/kyverno/kyverno/pkg/metrics"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metric names. The kyverno_ prefix keeps existing scrape configs working; the
// nirmata_ segment marks these fork-only so an upstream rebase cannot collide.
const (
	metricState      = "kyverno_nirmata_license_state"
	metricInfo       = "kyverno_nirmata_license_info"
	metricValid      = "kyverno_nirmata_license_valid"
	metricPolicies   = "kyverno_nirmata_license_policies_used"
	metricPolicyCap  = "kyverno_nirmata_license_policies_allowed"
	metricNodeUnits  = "kyverno_nirmata_license_node_units_used"
	metricNodeCap    = "kyverno_nirmata_license_node_units_allowed"
	metricDaysLeft   = "kyverno_nirmata_license_days_remaining"
	metricOverLimit  = "kyverno_nirmata_license_over_limit"
	metricEvalErrors = "kyverno_nirmata_license_evaluation_errors"
)

// instruments holds the metric handles for one MeterProvider. The provider is
// retained to detect replacement: pkg/metrics rebuilds the global provider on a
// timer, and instruments bound to the old one silently stop recording.
type instruments struct {
	provider metric.MeterProvider

	state      metric.Int64Gauge
	info       metric.Int64Gauge
	valid      metric.Int64Gauge
	policies   metric.Int64Gauge
	policyCap  metric.Int64Gauge
	nodeUnits  metric.Int64Gauge
	nodeCap    metric.Int64Gauge
	daysLeft   metric.Int64Gauge
	overLimit  metric.Int64Gauge
	evalErrors metric.Int64Counter
}

// newInstruments builds gauges against the supplied provider.
//
// Synchronous rather than observable gauges: an observable one needs its
// registration re-tracked whenever the provider is replaced, and its callback would
// read state concurrently with the evaluator.
//
// Construction errors leave the handle nil; every record site tolerates that,
// because failing to publish a metric must not interrupt evaluation.
func newInstruments(provider metric.MeterProvider, logger logr.Logger) *instruments {
	meter := provider.Meter(metrics.MeterName)
	in := &instruments{provider: provider}

	gauge := func(name, desc string) metric.Int64Gauge {
		g, err := meter.Int64Gauge(name, metric.WithDescription(desc))
		if err != nil {
			logger.Error(err, "failed to create license metric", "name", name)
			return nil
		}
		return g
	}

	in.state = gauge(metricState, "One-hot indicator of the current N4K license state.")
	in.info = gauge(metricInfo, "Static labels describing the active N4K license. Always 1.")
	in.valid = gauge(metricValid, "1 when the N4K license posture is Licensed, Warning, or FreeTier; 0 otherwise.")
	in.policies = gauge(metricPolicies, "Number of Kyverno policies currently installed.")
	in.policyCap = gauge(metricPolicyCap, "Licensed policy limit; -1 when unlimited.")
	in.nodeUnits = gauge(metricNodeUnits, "Licensed node units currently consumed by the cluster.")
	in.nodeCap = gauge(metricNodeCap, "Licensed node unit limit; -1 when unlimited.")
	in.daysLeft = gauge(metricDaysLeft, "Whole days remaining in the current license or trial window.")
	in.overLimit = gauge(metricOverLimit, "1 when the named capacity limit is exceeded, 0 otherwise.")

	c, err := meter.Int64Counter(metricEvalErrors,
		metric.WithDescription("Count of failures encountered while evaluating the N4K license."))
	if err != nil {
		logger.Error(err, "failed to create license metric", "name", metricEvalErrors)
	} else {
		in.evalErrors = c
	}
	return in
}

// record publishes a snapshot. State is one-hot — 1 for the current state, 0 for
// the rest — so `sum by (state)` never shows two states after a transition.
func (in *instruments) record(ctx context.Context, snap Snapshot, active Active) {
	if in == nil {
		return
	}

	if in.state != nil {
		for _, s := range AllStates {
			value := int64(0)
			if s == snap.State {
				value = 1
			}
			in.state.Record(ctx, value, metric.WithAttributes(attribute.String("state", string(s))))
		}
	}

	if in.info != nil {
		in.info.Record(ctx, 1, metric.WithAttributes(
			attribute.String("customer", active.Claims.Customer),
			attribute.String("license_type", active.Claims.LicenseType),
			attribute.String("license_id", active.Claims.LicenseID),
			attribute.String("origin", string(active.Origin)),
			attribute.Bool("commercial", active.Commercial),
			attribute.Bool("invalid", active.Invalid),
		))
	}

	set(ctx, in.valid, boolToInt64(snap.State.IsValid()))
	set(ctx, in.policies, int64(snap.Inputs.PolicyCount))
	set(ctx, in.policyCap, snap.Inputs.Limits.Policies.MetricValue())
	set(ctx, in.nodeUnits, int64(snap.Inputs.NodeUnitCount))
	set(ctx, in.nodeCap, snap.Inputs.Limits.NodeUnits.MetricValue())
	set(ctx, in.daysLeft, int64(snap.DaysRemaining))

	if in.overLimit != nil {
		in.overLimit.Record(ctx, boolToInt64(snap.OverPolicies),
			metric.WithAttributes(attribute.String("resource", "policies")))
		in.overLimit.Record(ctx, boolToInt64(snap.OverNodeUnits),
			metric.WithAttributes(attribute.String("resource", "node_units")))
	}
}

// recordError increments the evaluation-error counter for a named stage.
func (in *instruments) recordError(ctx context.Context, stage string) {
	if in == nil || in.evalErrors == nil {
		return
	}
	in.evalErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", stage)))
}

func set(ctx context.Context, g metric.Int64Gauge, v int64) {
	if g == nil {
		return
	}
	g.Record(ctx, v)
}

func boolToInt64(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// currentProvider is indirected so tests can supply their own MeterProvider.
var currentProvider = func() metric.MeterProvider { return otel.GetMeterProvider() }

// sameProvider reports whether two MeterProviders are the same instance. Comparing
// interface values panics on uncomparable dynamic types; that cannot happen with
// today's providers, but the panic would recur every tick and disable metrics for a
// cosmetic reason. Reporting "not the same" just rebuilds, which is cheap.
func sameProvider(a, b metric.MeterProvider) (same bool) {
	if a == nil || b == nil {
		return false
	}
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

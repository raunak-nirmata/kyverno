package license

import (
	"context"
	"testing"

	"github.com/go-logr/logr/testr"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collect reads every metric currently recorded by the reader, flattened to
// name -> the last data point value seen, plus the attributes of each point.
func collect(t *testing.T, reader *sdkmetric.ManualReader) map[string][]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string][]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Gauge[int64]:
				for _, pt := range d.DataPoints {
					out[m.Name] = append(out[m.Name], pt.Value)
				}
			case metricdata.Sum[int64]:
				for _, pt := range d.DataPoints {
					out[m.Name] = append(out[m.Name], pt.Value)
				}
			}
		}
	}
	return out
}

func testProvider(t *testing.T) (*sdkmetric.MeterProvider, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	return sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)), reader
}

func TestRecordPublishesSnapshot(t *testing.T) {
	t.Parallel()

	provider, reader := testProvider(t)
	in := newInstruments(provider, testr.New(t))

	snap := Evaluate(Inputs{
		Now:           base.Add(days(1)),
		TrialStart:    base,
		Limits:        windows,
		PolicyCount:   11,
		NodeUnitCount: 4,
	})
	in.record(context.Background(), snap, Active{Origin: OriginCommunity})

	got := collect(t, reader)

	if v := got[metricPolicies]; len(v) != 1 || v[0] != 11 {
		t.Errorf("%s = %v, want [11]", metricPolicies, v)
	}
	if v := got[metricPolicyCap]; len(v) != 1 || v[0] != 10 {
		t.Errorf("%s = %v, want [10]", metricPolicyCap, v)
	}
	if v := got[metricNodeUnits]; len(v) != 1 || v[0] != 4 {
		t.Errorf("%s = %v, want [4]", metricNodeUnits, v)
	}
	if v := got[metricDaysLeft]; len(v) != 1 || v[0] != 14 {
		t.Errorf("%s = %v, want [14]", metricDaysLeft, v)
	}

	// State is one-hot: exactly one series at 1, the rest at 0, so a
	// `sum by (state)` can never show two states after a transition.
	states := got[metricState]
	if len(states) != len(AllStates) {
		t.Fatalf("%s has %d series, want one per state (%d)", metricState, len(states), len(AllStates))
	}
	ones := 0
	for _, v := range states {
		switch v {
		case 1:
			ones++
		case 0:
		default:
			t.Errorf("%s emitted %d, want only 0 or 1", metricState, v)
		}
	}
	if ones != 1 {
		t.Errorf("%s had %d series set to 1, want exactly 1", metricState, ones)
	}

	// Over-limit is reported for both dimensions: policies over, nodes not.
	over := got[metricOverLimit]
	if len(over) != 2 {
		t.Fatalf("%s has %d series, want 2 (policies and node_units)", metricOverLimit, len(over))
	}
	sum := over[0] + over[1]
	if sum != 1 {
		t.Errorf("%s series sum to %d, want 1 (policies over, node units within)", metricOverLimit, sum)
	}
}

// Unlimited must be reported as -1 rather than omitted, so dashboards always have
// a value and "unlimited" is distinguishable from a cap of zero.
func TestRecordReportsUnlimitedAsMinusOne(t *testing.T) {
	t.Parallel()

	provider, reader := testProvider(t)
	in := newInstruments(provider, testr.New(t))

	in.record(context.Background(), Evaluate(Inputs{
		Now:        base,
		TrialStart: base,
		Limits:     Limits{Policies: Unlimited(), NodeUnits: Unlimited(), Trial: days(15)},
	}), Active{})

	got := collect(t, reader)
	if v := got[metricPolicyCap]; len(v) != 1 || v[0] != -1 {
		t.Errorf("%s = %v, want [-1] for an unlimited cap", metricPolicyCap, v)
	}
	if v := got[metricNodeCap]; len(v) != 1 || v[0] != -1 {
		t.Errorf("%s = %v, want [-1] for an unlimited cap", metricNodeCap, v)
	}
}

// This is the reason instruments are rebuilt rather than cached forever:
// pkg/metrics shuts the global MeterProvider down and installs a new one on a
// timer, and instruments bound to the dead provider silently stop recording.
func TestEnsureInstrumentsRebuildsWhenProviderIsReplaced(t *testing.T) {
	first, firstReader := testProvider(t)
	second, secondReader := testProvider(t)

	active := first
	original := currentProvider
	currentProvider = func() metric.MeterProvider { return active }
	t.Cleanup(func() { currentProvider = original })

	s := &service{logger: testr.New(t)}

	s.ensureInstruments()
	built := s.metrics
	if built == nil {
		t.Fatal("instruments were not built")
	}

	// Same provider: must not rebuild, so gauge state is not thrown away.
	s.ensureInstruments()
	if s.metrics != built {
		t.Error("instruments were rebuilt even though the provider was unchanged")
	}

	snap := Evaluate(Inputs{Now: base, TrialStart: base, Limits: windows, PolicyCount: 3})
	s.metrics.record(context.Background(), snap, Active{})
	if v := collect(t, firstReader)[metricPolicies]; len(v) != 1 || v[0] != 3 {
		t.Fatalf("first provider recorded %v, want [3]", v)
	}

	// Provider swapped out from under us.
	active = second
	s.ensureInstruments()
	if s.metrics == built {
		t.Fatal("instruments must be rebuilt when the MeterProvider is replaced")
	}

	s.metrics.record(context.Background(), snap, Active{})
	if v := collect(t, secondReader)[metricPolicies]; len(v) != 1 || v[0] != 3 {
		t.Errorf("after the provider swap the new provider recorded %v, want [3]", v)
	}
}

func TestRecordErrorCountsByStage(t *testing.T) {
	t.Parallel()

	provider, reader := testProvider(t)
	in := newInstruments(provider, testr.New(t))

	in.recordError(context.Background(), "nodes")
	in.recordError(context.Background(), "nodes")
	in.recordError(context.Background(), "policies")

	got := collect(t, reader)[metricEvalErrors]
	if len(got) != 2 {
		t.Fatalf("%s has %d series, want 2 stages", metricEvalErrors, len(got))
	}
	total := got[0] + got[1]
	if total != 3 {
		t.Errorf("%s totals %d, want 3", metricEvalErrors, total)
	}
}

// Every record path must tolerate a nil receiver and nil handles, because failing
// to publish a metric must never interrupt or fail an evaluation.
func TestMetricsAreNilSafe(t *testing.T) {
	t.Parallel()

	var nilInstruments *instruments
	nilInstruments.record(context.Background(), Snapshot{}, Active{})
	nilInstruments.recordError(context.Background(), "nodes")

	empty := &instruments{}
	empty.record(context.Background(), Snapshot{}, Active{})
	empty.recordError(context.Background(), "nodes")
}

package license

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kyverno/kyverno/pkg/config"
)

const testDeploymentName = "kyverno-admission-controller"

// communityLoader returns a loader yielding a license with the given caps, so the
// service can be driven without the production signing key.
func communityLoader(policies, nodeUnits int, trial time.Duration) func(time.Time) (Active, error) {
	return func(time.Time) (Active, error) {
		return Active{
			Origin: OriginCommunity,
			Limits: Limits{
				Policies:  Bound(policies),
				NodeUnits: Bound(nodeUnits),
				Trial:     trial,
				Grace:     days(DefaultGraceDays),
				Warn:      days(DefaultWarnDays),
			},
		}, nil
	}
}

// leaseFor builds a Lease held by holder that is live for any evaluation time a
// test is likely to use. Tests that care about lease expiry use leaseAt instead.
func leaseFor(holder string) *coordinationv1.Lease {
	return leaseAt(holder, base, int32((10 * 365 * 24 * time.Hour).Seconds()))
}

func leaseAt(holder string, renewed time.Time, durationSeconds int32) *coordinationv1.Lease {
	rt := metav1.NewMicroTime(renewed)
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: leaderLeaseName, Namespace: testNamespace},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holder,
			RenewTime:            &rt,
			LeaseDurationSeconds: &durationSeconds,
		},
	}
}

func deployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testDeploymentName,
			Namespace: testNamespace,
			UID:       "dep-uid",
		},
	}
}

// newTestService builds a service wired to fakes. It does not call Start, so no
// goroutine runs and each tick can be driven explicitly.
func newTestService(t *testing.T, objs []runtime.Object, now time.Time) (*service, *fake.Clientset) {
	t.Helper()
	client := fake.NewSimpleClientset(objs...)
	s := &service{
		logger: testr.New(t),
		opts: Options{
			Component:      AuthoritativeComponent,
			KubeClient:     client,
			Namespace:      testNamespace,
			ConfigMapName:  testClockName,
			DeploymentName: testDeploymentName,
		},
		now:      func() time.Time { return now },
		load:     communityLoader(10, 10, days(15)),
		lastEmit: map[string]time.Time{},
	}
	return s, client
}

// Start must be inert for every component other than the admission controller,
// so the background, reports, cleanup and init controllers never evaluate.
func TestStartIsNoOpForNonAuthoritativeComponents(t *testing.T) {
	for _, component := range []string{
		"kyverno-background-controller",
		"kyverno-reports-controller",
		"kyverno-cleanup-controller",
		"kyverno-init-controller",
	} {
		t.Run(component, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			Start(context.Background(), testr.New(t), Options{
				Component:  component,
				KubeClient: client,
				Namespace:  testNamespace,
			})
			// Give any goroutine that should not exist a chance to misbehave.
			time.Sleep(50 * time.Millisecond)
			if got := client.Actions(); len(got) != 0 {
				t.Errorf("expected zero API calls for %s, got %v", component, got)
			}
		})
	}
}

func TestStartIsNoOpWithoutKubeClient(t *testing.T) {
	t.Parallel()
	// Must not panic and must return immediately.
	Start(context.Background(), testr.New(t), Options{Component: AuthoritativeComponent})
}

// Start must never block its caller: it is called from the shared startup path
// just before Setup returns.
func TestStartDoesNotBlock(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		Start(ctx, testr.New(t), Options{
			Component:      AuthoritativeComponent,
			KubeClient:     fake.NewSimpleClientset(),
			Namespace:      testNamespace,
			ConfigMapName:  testClockName,
			DeploymentName: testDeploymentName,
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start blocked; it must return immediately")
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	s, _ := newTestService(t, []runtime.Object{leaseFor("other"), deployment()}, base)
	s.opts.Interval = MinInterval

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		s.run(ctx)
		close(stopped)
	}()

	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after context cancellation")
	}
}

// The happy path: leader, clock provisioned, counts observed, state reported.
func TestTickReportsFreeTier(t *testing.T) {
	t.Parallel()

	s, client := newTestService(t, []runtime.Object{
		leaseFor(config.KyvernoPodName()),
		deployment(),
		clockCM(renderClock(clockState{FirstSeen: base, LastSeen: base})),
		node("n1", "64Gi"),
	}, base.Add(days(1)))

	s.tick(context.Background())

	got := s.snapshot.Load()
	if got == nil {
		t.Fatal("no snapshot stored")
	}
	if got.State != FreeTier {
		t.Errorf("State = %q, want FreeTier (reason: %s)", got.State, got.Reason)
	}
	if got.Inputs.NodeUnitCount != 2 {
		t.Errorf("NodeUnitCount = %d, want 2 for a 64Gi node", got.Inputs.NodeUnitCount)
	}
	// 15-day trial started at base, evaluated one day in.
	if got.DaysRemaining != 14 {
		t.Errorf("DaysRemaining = %d, want 14", got.DaysRemaining)
	}
	// The leader advances lastSeen.
	stored, err := client.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), testClockName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stored.Data[keyLastState] != string(FreeTier) {
		t.Errorf("persisted lastState = %q, want FreeTier", stored.Data[keyLastState])
	}
}

// A non-leader must observe and report without writing anything.
func TestTickNonLeaderDoesNotWrite(t *testing.T) {
	t.Parallel()

	s, client := newTestService(t, []runtime.Object{
		leaseFor("some-other-pod"),
		deployment(),
		clockCM(renderClock(clockState{FirstSeen: base, LastSeen: base})),
		node("n1", "64Gi"),
	}, base.Add(days(1)))

	s.tick(context.Background())

	if got := s.snapshot.Load(); got == nil || got.State != FreeTier {
		t.Error("a non-leader must still evaluate and report state")
	}
	for _, action := range client.Actions() {
		switch action.GetVerb() {
		case "create", "update", "patch", "delete":
			t.Errorf("non-leader performed a write: %s %s", action.GetVerb(), action.GetResource().Resource)
		}
	}
}

// The leader provisions the ConfigMap when the chart has not, so a non-Helm
// install still gets a trial anchor.
func TestTickCreatesClockWhenAbsent(t *testing.T) {
	t.Parallel()

	s, client := newTestService(t, []runtime.Object{
		leaseFor(config.KyvernoPodName()),
		deployment(),
	}, base)

	s.tick(context.Background())

	stored, err := client.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), testClockName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the leader should have provisioned the license state: %v", err)
	}
	if stored.Data[keyFirstSeen] == "" {
		t.Error("firstSeen should have been stamped")
	}
}

// Fail-open: a completely broken clock read must report FreeTier, never expiry.
func TestTickFailsOpenWhenClockUnreadable(t *testing.T) {
	t.Parallel()

	s, client := newTestService(t, []runtime.Object{
		leaseFor(config.KyvernoPodName()),
		deployment(),
	}, base)
	client.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("etcd unavailable"))
	})

	s.tick(context.Background())

	got := s.snapshot.Load()
	if got == nil {
		t.Fatal("a clock failure must still produce a snapshot")
	}
	if got.State != FreeTier {
		t.Errorf("State = %q, want FreeTier; an unreadable clock must never expire a cluster", got.State)
	}
}

// The anti-escalation guard: a stale tick must not move the reported state
// toward Degraded. Here the trial has genuinely elapsed, but node counting
// failed, so the worse state is withheld until inputs are trustworthy again.
func TestTickWithholdsEscalationOnStaleInputs(t *testing.T) {
	t.Parallel()

	s, client := newTestService(t, []runtime.Object{
		leaseFor(config.KyvernoPodName()),
		deployment(),
		clockCM(renderClock(clockState{FirstSeen: base, LastSeen: base})),
	}, base.Add(days(1)))

	// First tick establishes FreeTier from good inputs.
	s.tick(context.Background())
	if got := s.snapshot.Load(); got == nil || got.State != FreeTier {
		t.Fatalf("setup: want FreeTier, got %v", got)
	}

	// Now the trial has elapsed AND node counting breaks.
	s.now = func() time.Time { return base.Add(days(20)) }
	client.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("apiserver unavailable"))
	})

	s.tick(context.Background())

	if got := s.snapshot.Load(); got.State != FreeTier {
		t.Errorf("State = %q, want the previous FreeTier retained while inputs are stale", got.State)
	}

	// Once counting recovers, the real state is applied.
	client.ReactionChain = client.ReactionChain[1:]
	s.tick(context.Background())
	if got := s.snapshot.Load(); got.State != FreeExpired {
		t.Errorf("State = %q, want FreeExpired once inputs are trustworthy again", got.State)
	}
}

// An improvement is applied even on stale inputs — the guard only blocks
// worsening, so a recovered cluster is never held in a bad state.
func TestTickAppliesImprovementEvenWhenStale(t *testing.T) {
	t.Parallel()

	s, client := newTestService(t, []runtime.Object{
		leaseFor(config.KyvernoPodName()),
		deployment(),
		clockCM(renderClock(clockState{FirstSeen: base, LastSeen: base})),
	}, base.Add(days(20)))

	s.tick(context.Background())
	if got := s.snapshot.Load(); got.State != FreeExpired {
		t.Fatalf("setup: want FreeExpired, got %v", got)
	}

	// A commercial license valid for another year appears, but node counting is
	// still broken so this tick's inputs are stale.
	s.load = func(time.Time) (Active, error) {
		return Active{
			Origin:     OriginFile,
			Commercial: true,
			NotAfter:   base.Add(days(365)),
			Limits:     Limits{Policies: Unlimited(), NodeUnits: Unlimited(), Warn: days(30), Grace: days(30)},
		}, nil
	}
	s.now = func() time.Time { return base.Add(days(21)) }
	client.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("still down"))
	})

	s.tick(context.Background())
	// Licensed is healthier than FreeExpired, so it must be applied despite staleness.
	if got := s.snapshot.Load(); got.State != Licensed {
		t.Errorf("State = %q, want Licensed; an improvement must be applied even when some inputs are stale", got.State)
	}
}

// A panic inside evaluation must cost one tick, not the loop, and must leave the
// previously reported state intact.
func TestTickPanicRetainsPreviousState(t *testing.T) {
	t.Parallel()

	s, _ := newTestService(t, []runtime.Object{
		leaseFor(config.KyvernoPodName()),
		deployment(),
		clockCM(renderClock(clockState{FirstSeen: base, LastSeen: base})),
	}, base.Add(days(1)))

	s.tick(context.Background())
	before := s.snapshot.Load()
	if before == nil || before.State != FreeTier {
		t.Fatalf("setup: want FreeTier, got %v", before)
	}

	s.load = func(time.Time) (Active, error) { panic("boom") }
	s.tick(context.Background()) // must not propagate

	after := s.snapshot.Load()
	if after.State != before.State {
		t.Errorf("State = %q, want the previous %q retained after a panic", after.State, before.State)
	}
}

// A build without an embedded license must report nothing rather than invent
// limits, and must not panic.
func TestTickWithMissingLicenseReportsNothing(t *testing.T) {
	t.Parallel()

	s, _ := newTestService(t, []runtime.Object{leaseFor(config.KyvernoPodName()), deployment()}, base)
	s.load = func(time.Time) (Active, error) { return Active{}, errCommunityLicenseMissing }

	s.tick(context.Background())

	if got := s.snapshot.Load(); got != nil {
		t.Errorf("no snapshot should be stored when the license cannot be loaded, got %+v", got)
	}
}

func TestIsLeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		objs []runtime.Object
		want bool
	}{
		{"holds a live lease", []runtime.Object{leaseFor(config.KyvernoPodName())}, true},
		{"another pod holds it", []runtime.Object{leaseFor("other-pod")}, false},
		{"no lease at all", nil, false},
		{
			// A restarted or terminating pod still finds its own identity in a Lease
			// it no longer renews. It must not act as leader.
			name: "own identity but the lease has expired",
			objs: []runtime.Object{leaseAt(config.KyvernoPodName(), base.Add(-time.Hour), 15)},
			want: false,
		},
		{
			name: "lease with no renew time",
			objs: []runtime.Object{&coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: leaderLeaseName, Namespace: testNamespace},
				Spec:       coordinationv1.LeaseSpec{HolderIdentity: ptrTo(config.KyvernoPodName())},
			}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _ := newTestService(t, tt.objs, base)
			if got := s.isLeader(context.Background()); got != tt.want {
				t.Errorf("isLeader = %v, want %v", got, tt.want)
			}
		})
	}
}

func ptrTo[T any](v T) *T { return &v }

func TestIsLeaderWithNilHolder(t *testing.T) {
	t.Parallel()

	s, _ := newTestService(t, []runtime.Object{
		&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: leaderLeaseName, Namespace: testNamespace}},
	}, base)
	if s.isLeader(context.Background()) {
		t.Error("a Lease with no holder must not be read as leadership")
	}
}

// Regression test for pkg/event's silent drop-gates. License events are attached
// to the Deployment specifically so they survive; if someone re-points them at a
// policy object or reuses PolicyApplied, this fails.
func TestEventsReachTheDeployment(t *testing.T) {
	t.Parallel()

	ref, err := resolveDeploymentRef(context.Background(),
		fake.NewSimpleClientset(deployment()), testNamespace, testDeploymentName)
	if err != nil {
		t.Fatal(err)
	}

	if ref.Name == "" {
		t.Error("Regarding.Name must be set or the event controller drops the event")
	}
	if ref.APIVersion != "apps/v1" {
		t.Errorf("APIVersion = %q, want apps/v1", ref.APIVersion)
	}
	if ref.Namespace == "" {
		t.Error("a namespaced Regarding avoids the event being filed in the default namespace")
	}

	info := stateEvent(ref, FreeTier, FreeExpired, Snapshot{Reason: "trial elapsed"})
	if info.IsRegardingPolicy() {
		t.Error("license events must not regard a policy object; those are dropped unless generatePolicyEvents is on")
	}
	if info.Reason == "PolicyApplied" {
		t.Error("PolicyApplied is dropped unless generateSuccessEvents is on")
	}
	if info.Type != corev1.EventTypeWarning {
		t.Errorf("Type = %q, want Warning for a transition into FreeExpired", info.Type)
	}
	if info.Source != sourceLicense {
		t.Errorf("Source = %q, want %q", info.Source, sourceLicense)
	}

	healthy := stateEvent(ref, Unknown, FreeTier, Snapshot{Reason: "within trial"})
	if healthy.Type != corev1.EventTypeNormal {
		t.Errorf("Type = %q, want Normal for a healthy state", healthy.Type)
	}
}

func TestLimitEventMessageReflectsEnforcement(t *testing.T) {
	t.Parallel()

	enforced := limitEvent(corev1.ObjectReference{Name: "x"}, "policy", 11, Bound(10), FreeExpired)
	if enforced.Type != corev1.EventTypeWarning {
		t.Error("a capacity overrun should be a Warning")
	}
	if !strings.Contains(enforced.Message, "refused") {
		t.Errorf("FreeExpired message must mention refusal, got %q", enforced.Message)
	}
	if !strings.Contains(enforced.Message, "11") || !strings.Contains(enforced.Message, "10") {
		t.Errorf("the message must state used and permitted counts, got %q", enforced.Message)
	}

	grace := limitEvent(corev1.ObjectReference{Name: "x"}, "policy", 11, Bound(10), Grace)
	if !strings.Contains(grace.Message, "preserved") {
		t.Errorf("Grace message must mention preserved function, got %q", grace.Message)
	}
}

// Repeat reporting of a persistent overrun is throttled so the event stream is
// not flooded every interval.
func TestLimitEventsAreThrottled(t *testing.T) {
	t.Parallel()

	s, _ := newTestService(t, nil, base)
	if !s.shouldEmit("policies", base) {
		t.Fatal("the first report should be emitted")
	}
	if s.shouldEmit("policies", base.Add(time.Hour)) {
		t.Error("a repeat within the reminder interval should be suppressed")
	}
	if !s.shouldEmit("policies", base.Add(limitReminderInterval+time.Minute)) {
		t.Error("a repeat after the reminder interval should be emitted")
	}
}

func TestOptionsIntervalClamping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"unset uses the default", 0, DefaultInterval},
		{"too small is raised to the floor", time.Second, MinInterval},
		{"at the floor is kept", MinInterval, MinInterval},
		{"a larger value is respected", time.Hour, time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := (Options{Interval: tt.in}).interval(); got != tt.want {
				t.Errorf("interval() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCurrentBeforeFirstEvaluation(t *testing.T) {
	// Not parallel: reads the package-level snapshot.
	current.Store(nil)
	if got := Current(); got.State != Unknown {
		t.Errorf("Current().State = %q, want Unknown before the first evaluation", got.State)
	}
}

func TestGVRListDoesNotIncludeNonPolicyKinds(t *testing.T) {
	t.Parallel()

	for _, resource := range policyResources {
		if resource.Group != "kyverno.io" && resource.Group != "policies.kyverno.io" {
			t.Errorf("unexpected group %q in the policy count list", resource.Group)
		}
		for _, version := range resource.Versions {
			gvr := schema.GroupVersionResource{Group: resource.Group, Version: version, Resource: resource.Resource}
			if _, err := schema.ParseGroupVersion(gvr.GroupVersion().String()); err != nil {
				t.Errorf("invalid GroupVersion for %v: %v", gvr, err)
			}
		}
	}
}

// The chart ships the license-state ConfigMap with no data at all, so this is the
// default production path: the leader must stamp firstSeen into an existing but
// empty ConfigMap rather than only creating a missing one.
func TestTickStampsFirstSeenIntoChartCreatedEmptyConfigMap(t *testing.T) {
	t.Parallel()

	s, client := newTestService(t, []runtime.Object{
		leaseFor(config.KyvernoPodName()),
		deployment(),
		clockCM(nil), // exactly what the chart renders: metadata only, no data
	}, base)

	s.tick(context.Background())

	stored, err := client.CoreV1().ConfigMaps(testNamespace).Get(
		context.Background(), testClockName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stored.Data[keyFirstSeen] == "" {
		t.Fatal("the leader must stamp firstSeen into a chart-created empty ConfigMap")
	}
	if got := s.snapshot.Load(); got == nil || got.State != FreeTier {
		t.Errorf("State = %v, want FreeTier on a freshly stamped trial", got)
	}

	// A later tick must not restart the trial.
	firstSeen := stored.Data[keyFirstSeen]
	s.now = func() time.Time { return base.Add(days(3)) }
	s.tick(context.Background())

	stored, err = client.CoreV1().ConfigMaps(testNamespace).Get(
		context.Background(), testClockName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stored.Data[keyFirstSeen] != firstSeen {
		t.Errorf("firstSeen changed from %q to %q; the trial must not restart",
			firstSeen, stored.Data[keyFirstSeen])
	}
	if got := s.snapshot.Load(); got.DaysRemaining != 12 {
		t.Errorf("DaysRemaining = %d, want 12 three days into a 15-day trial", got.DaysRemaining)
	}
}

// Regression test for the fail-open trap where a pod that started with a wrong
// future clock poisoned lastSeen, and every later tick then clamped forward to
// that poisoned value — reporting FreeExpired forever with no recovery.
func TestTickRecoversFromPoisonedFutureClock(t *testing.T) {
	t.Parallel()

	poisoned := base.Add(days(365))
	s, client := newTestService(t, []runtime.Object{
		leaseFor(config.KyvernoPodName()),
		deployment(),
		clockCM(renderClock(clockState{FirstSeen: base, LastSeen: poisoned})),
	}, base.Add(days(1)))

	s.tick(context.Background())

	got := s.snapshot.Load()
	if got == nil {
		t.Fatal("no snapshot stored")
	}
	if got.State != FreeTier {
		t.Errorf("State = %q, want FreeTier; a future lastSeen must not pin the cluster to an expired trial", got.State)
	}
	if got.DaysRemaining != 14 {
		t.Errorf("DaysRemaining = %d, want 14 (one day into a 15-day trial)", got.DaysRemaining)
	}

	// The poisoned value must be replaced, so recovery is permanent rather than
	// re-poisoning itself on the next tick.
	stored, err := client.CoreV1().ConfigMaps(testNamespace).Get(
		context.Background(), testClockName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lastSeen, err := time.Parse(time.RFC3339, stored.Data[keyLastSeen])
	if err != nil {
		t.Fatal(err)
	}
	if !lastSeen.Equal(base.Add(days(1))) {
		t.Errorf("persisted lastSeen = %v, want it re-anchored to the current clock", lastSeen)
	}

	// Second tick confirms the fix is stable.
	s.tick(context.Background())
	if got := s.snapshot.Load(); got.State != FreeTier {
		t.Errorf("State = %q on the second tick, want FreeTier", got.State)
	}
}

// While a state change is withheld for stale inputs, everything that DID succeed
// must still be reported. Freezing every signal would hide a real capacity
// overrun for as long as an unrelated outage lasted.
func TestWithheldEscalationStillReportsFreshCounts(t *testing.T) {
	t.Parallel()

	s, client := newTestService(t, []runtime.Object{
		leaseFor(config.KyvernoPodName()),
		deployment(),
		clockCM(renderClock(clockState{FirstSeen: base, LastSeen: base})),
	}, base.Add(days(1)))

	s.tick(context.Background())
	if got := s.snapshot.Load(); got.State != FreeTier {
		t.Fatalf("setup: want FreeTier, got %v", got)
	}

	// Trial elapses (would escalate to FreeExpired) AND node counting breaks, but
	// the policy count succeeds and is over the cap.
	s.now = func() time.Time { return base.Add(days(20)) }
	s.opts.MetadataClient = nil // isolate: policy count comes from the seed below
	client.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("apiserver down"))
	})
	// Seed a fresh over-cap policy count through the loader's limits.
	s.load = communityLoader(1, 10, days(15))

	s.tick(context.Background())

	got := s.snapshot.Load()
	if got.State != FreeTier {
		t.Errorf("State = %q, want the previous FreeTier held while inputs are stale", got.State)
	}
	// A snapshot must still have been stored, so the next tick sees fresh context
	// rather than re-reading an ever-staler one.
	if !got.EvaluatedAt.Equal(base.Add(days(20))) {
		t.Errorf("EvaluatedAt = %v, want the current tick; the snapshot must be refreshed even when state is held",
			got.EvaluatedAt)
	}
	if got.Reason == "" {
		t.Error("Reason must explain that the state is being held")
	}
}

// Licensing cannot be turned off, so the cadence flag must not be usable as a
// soft kill switch: a very long interval is clamped rather than honoured.
func TestIntervalIsClampedAtBothEnds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"unset uses the default", 0, DefaultInterval},
		{"below the floor is raised", time.Second, MinInterval},
		{"at the floor is kept", MinInterval, MinInterval},
		{"in range is respected", 5 * time.Minute, 5 * time.Minute},
		{"at the ceiling is kept", MaxInterval, MaxInterval},
		{"above the ceiling is lowered", 365 * 24 * time.Hour, MaxInterval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := (Options{Interval: tt.in}).interval(); got != tt.want {
				t.Errorf("interval() = %v, want %v", got, tt.want)
			}
		})
	}
}

// A build with no license artifact is an expected interim state, not a defect. It
// must report no state, and must not be indistinguishable from a corrupt license.
func TestUnprovisionedBuildReportsNoStateWithoutErroring(t *testing.T) {
	t.Parallel()

	s, client := newTestService(t, []runtime.Object{
		leaseFor(config.KyvernoPodName()),
		deployment(),
	}, base)
	s.load = func(time.Time) (Active, error) { return Active{}, errCommunityLicenseMissing }

	s.tick(context.Background())

	if got := s.snapshot.Load(); got != nil {
		t.Errorf("no snapshot should be stored for an unprovisioned build, got %+v", got)
	}
	// It must also not have started writing cluster state on the strength of a
	// license it does not have.
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" || action.GetVerb() == "update" {
			t.Errorf("unprovisioned build performed a write: %s %s",
				action.GetVerb(), action.GetResource().Resource)
		}
	}
}

// A failed ConfigMap read must not restart an ongoing grace window.
//
// On that path the clockState is the zero value while Inputs.GraceStartedAt still
// carries the anchor forward from the previous snapshot. Sourcing the anchor from
// the clock instead would re-stamp it to now on every failed read, so a cluster
// with a flaky API server would sit in Grace forever and never reach Degraded.
// Not parallel: writes the package global current, as does TestAdmitPolicyCreate.
func TestGraceAnchorSurvivesAClockReadFailure(t *testing.T) {
	started := base.Add(-days(20))
	s, client := newTestService(t, []runtime.Object{leaseFor(config.KyvernoPodName())}, base)
	s.load = func(time.Time) (Active, error) {
		return Active{
			Origin:  OriginFile,
			Invalid: true,
			Limits:  Limits{Policies: Unlimited(), NodeUnits: Unlimited(), Grace: days(30)},
		}, nil
	}
	// Seed a snapshot that is already in Grace, anchored 20 days ago.
	s.store(&Snapshot{
		State:  Grace,
		Inputs: Inputs{GraceStartedAt: started},
	})
	// Every ConfigMap read fails, so readOrCreateClock returns a zero clockState.
	client.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("apiserver unreachable"))
	})
	client.PrependReactor("create", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("apiserver unreachable"))
	})

	s.tick(context.Background())

	got := Current()
	t.Cleanup(func() { current.Store(nil) })
	if !got.Inputs.GraceStartedAt.Equal(started) {
		t.Errorf("GraceStartedAt = %v, want the original anchor %v; a failed read re-stamped it",
			got.Inputs.GraceStartedAt, started)
	}
}

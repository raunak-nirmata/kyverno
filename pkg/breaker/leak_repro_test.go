package breaker

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8swatch "k8s.io/apimachinery/pkg/watch"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"
	apiregv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	aggregatorfake "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset/fake"
)

// This file used to contain hypothesis-confirmation tests for the CS-3797
// RetryWatcher leak (onReady firing on every reconnect instead of only on
// availability transitions, leaked RetryWatcher goroutines, and an orphaned
// counter on mid-init failure). Those hypotheses are now fixed; the tests
// below are the inverted regression assertions guarding against relapse.

// countRetryWatcherGoroutines returns the number of currently running goroutines
// whose stack trace mentions the (forked) RetryWatcher type, i.e. either its
// receive() loop or the inner goroutine it spawns to watch its stopChan.
func countRetryWatcherGoroutines(t *testing.T) int {
	t.Helper()
	buf := make([]byte, 4<<20)
	n := runtime.Stack(buf, true)
	blocks := strings.Split(string(buf[:n]), "\n\n")
	count := 0
	for _, b := range blocks {
		if strings.Contains(b, "RetryWatcher") {
			count++
		}
	}
	return count
}

// waitForGoroutineCount polls countRetryWatcherGoroutines until it reaches
// want or the timeout elapses, returning whatever the last observed count was.
func waitForGoroutineCount(t *testing.T, want int, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	got := countRetryWatcherGoroutines(t)
	for got != want && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		got = countRetryWatcherGoroutines(t)
	}
	return got
}

// availableAPIService returns a v1.reports.kyverno.io APIService object with
// Available=True, matching what watchAPIService looks for.
func availableAPIService(rv string) *apiregv1.APIService {
	return &apiregv1.APIService{
		ObjectMeta: metav1.ObjectMeta{
			Name:            reportsAPIServiceName,
			ResourceVersion: rv,
		},
		Status: apiregv1.APIServiceStatus{
			Conditions: []apiregv1.APIServiceCondition{
				{Type: apiregv1.Available, Status: apiregv1.ConditionTrue},
			},
		},
	}
}

// TestTransitionNotifier_OnlyFiresOnAvailabilityChange is the direct unit
// test for newTransitionNotifier: repeated notify() calls with the same
// availability value must only invoke the corresponding callback once; a
// flip in availability must fire again. This is the regression test for the
// root cause of CS-3797 (onReady firing on every reconnect/Modified event).
func TestTransitionNotifier_OnlyFiresOnAvailabilityChange(t *testing.T) {
	var onReadyCount, onNotReadyCount atomic.Int32
	onReady := func(context.Context) { onReadyCount.Add(1) }
	onNotReady := func() { onNotReadyCount.Add(1) }

	notify := newTransitionNotifier(context.Background(), onReady, onNotReady)

	notify(true)
	notify(true)
	notify(true)
	if got := onReadyCount.Load(); got != 1 {
		t.Fatalf("expected onReady to fire exactly once across repeated notify(true) calls, got %d", got)
	}
	if got := onNotReadyCount.Load(); got != 0 {
		t.Fatalf("expected onNotReady not to fire while availability stays true, got %d", got)
	}

	notify(false)
	notify(false)
	notify(false)
	if got := onNotReadyCount.Load(); got != 1 {
		t.Fatalf("expected onNotReady to fire exactly once across repeated notify(false) calls, got %d", got)
	}

	notify(true)
	if got := onReadyCount.Load(); got != 2 {
		t.Fatalf("expected onReady to fire again after a transition back to available, got %d", got)
	}
}

// TestWatchAPIService_OnReadyOnlyOnTransition exercises watchAPIService end
// to end against a fake aggregator client: the backing APIService stays
// Available=True across 3 reconnects (the watch connection drops and gets
// re-established each time), so onReady must fire exactly once, not once per
// reconnect. This directly regresses the kind repro finding (onReady 1->7 in
// 5 flaps).
func TestWatchAPIService_OnReadyOnlyOnTransition(t *testing.T) {
	client := aggregatorfake.NewSimpleClientset(availableAPIService("1"))

	// Every Watch() call gets a fresh fake watcher that we close almost
	// immediately, simulating a connection that keeps dropping and
	// reconnecting while the backing APIService stays healthy throughout.
	client.PrependWatchReactor("apiservices", func(action k8stesting.Action) (bool, k8swatch.Interface, error) {
		fw := k8swatch.NewFake()
		go func() {
			time.Sleep(5 * time.Millisecond)
			fw.Stop()
		}()
		return true, fw, nil
	})

	var onReadyCount, onNotReadyCount atomic.Int32
	onReady := func(context.Context) { onReadyCount.Add(1) }
	onNotReady := func() { onNotReadyCount.Add(1) }

	ctx := context.Background()
	logger := logr.Discard()
	notify := newTransitionNotifier(ctx, onReady, onNotReady)

	const iterations = 3
	for i := 0; i < iterations; i++ {
		if err := watchAPIService(ctx, logger, client, notify); err != nil {
			t.Fatalf("watchAPIService iteration %d returned unexpected error: %v", i, err)
		}
	}

	if got := onReadyCount.Load(); got != 1 {
		t.Fatalf("expected onReady to fire exactly once across %d reconnects while Available stayed True, got %d (onNotReady=%d)", iterations, got, onNotReadyCount.Load())
	}

	// Now flip availability and confirm the transition fires onNotReady.
	notify(false)
	if got := onNotReadyCount.Load(); got != 1 {
		t.Fatalf("expected onNotReady to fire once after flipping availability, got %d", got)
	}
}

// TestStartResourceCounter_StopsOnCtxCancel is the regression test for the
// RetryWatcher lifecycle fix: cancelling the ctx passed to
// StartAdmissionReportsCounter must stop every RetryWatcher it created,
// bringing the RetryWatcher-related goroutine count back to baseline.
func TestStartResourceCounter_StopsOnCtxCancel(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	metav1.AddMetaToScheme(scheme)
	client := metadatafake.NewSimpleMetadataClient(scheme)

	before := countRetryWatcherGoroutines(t)
	t.Logf("RetryWatcher-related goroutines before any counters started: %d", before)

	ctx, cancel := context.WithCancel(context.Background())

	const n = 5
	counters := make([]Counter, 0, n)
	for i := 0; i < n; i++ {
		c, err := StartAdmissionReportsCounter(ctx, client)
		if err != nil {
			t.Fatalf("StartAdmissionReportsCounter call %d failed: %v", i, err)
		}
		counters = append(counters, c)
	}

	// Give the spawned goroutines time to actually start running.
	time.Sleep(500 * time.Millisecond)

	after := countRetryWatcherGoroutines(t)
	t.Logf("RetryWatcher-related goroutines after %d StartAdmissionReportsCounter calls: %d (delta=%d)", n, after, after-before)
	if after <= before {
		t.Fatalf("expected RetryWatcher goroutines to increase after starting %d counters, before=%d after=%d", n, before, after)
	}

	cancel()

	got := waitForGoroutineCount(t, before, 5*time.Second)
	if got != before {
		t.Fatalf("expected RetryWatcher goroutines to return to baseline %d after ctx cancellation, got %d", before, got)
	}
	t.Logf("RetryWatcher-related goroutines returned to baseline (%d) after ctx cancellation", got)

	_ = counters
}

// TestStartAdmissionReportsCounter_NoOrphanOnSecondFailure is the regression
// test for the mid-init orphan fix: when the second StartResourceCounter call
// (clusterephemeralreports) fails, the first counter's (ephemeralreports)
// RetryWatcher must not be left running with no reference to stop it.
func TestStartAdmissionReportsCounter_NoOrphanOnSecondFailure(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	metav1.AddMetaToScheme(scheme)
	client := metadatafake.NewSimpleMetadataClient(scheme)

	// Make List() fail only for clusterephemeralreports; ephemeralreports
	// (called first, per StartAdmissionReportsCounter's implementation)
	// succeeds normally.
	client.PrependReactor("list", "clusterephemeralreports", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, fmt.Errorf("simulated API failure for clusterephemeralreports")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	before := countRetryWatcherGoroutines(t)
	t.Logf("RetryWatcher-related goroutines before call: %d", before)

	counter, err := StartAdmissionReportsCounter(ctx, client)
	if err == nil {
		t.Fatalf("expected StartAdmissionReportsCounter to fail when clusterephemeralreports List fails, got nil error and counter=%v", counter)
	}
	if counter != nil {
		t.Fatalf("expected nil Counter to be returned alongside the error, got %v", counter)
	}
	t.Logf("StartAdmissionReportsCounter returned expected error: %v", err)

	got := waitForGoroutineCount(t, before, 5*time.Second)
	if got != before {
		t.Fatalf("expected the first (ephemeralreports) counter's RetryWatcher to be stopped rather than orphaned: before=%d after=%d", before, got)
	}
	t.Logf("no orphaned RetryWatcher goroutines remain after the failed call (before=%d after=%d)", before, got)
}

// TestOnReadySessionSwap_DoesNotAccumulateWatchers simulates the controller
// mains' onReady pattern: each firing cancels the previous session's context
// before starting a new one. Firing onReady twice must not double the
// watcher goroutine count -- the first session's watchers must be torn down,
// not leaked alongside the second session's.
func TestOnReadySessionSwap_DoesNotAccumulateWatchers(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	metav1.AddMetaToScheme(scheme)
	client := metadatafake.NewSimpleMetadataClient(scheme)

	before := countRetryWatcherGoroutines(t)

	var sessionCancel context.CancelFunc
	startSession := func(ctx context.Context) Counter {
		if sessionCancel != nil {
			sessionCancel()
		}
		sctx, cancel := context.WithCancel(ctx)
		sessionCancel = cancel
		c, err := StartAdmissionReportsCounter(sctx, client)
		if err != nil {
			t.Fatalf("StartAdmissionReportsCounter failed: %v", err)
		}
		return c
	}

	ctx := context.Background()

	_ = startSession(ctx)
	time.Sleep(300 * time.Millisecond)
	afterFirst := countRetryWatcherGoroutines(t)
	t.Logf("RetryWatcher-related goroutines after first onReady: %d (delta=%d)", afterFirst, afterFirst-before)
	if afterFirst <= before {
		t.Fatalf("expected RetryWatcher goroutines to increase after the first onReady, before=%d after=%d", before, afterFirst)
	}

	// Second transition (e.g. flap + recover): fires onReady again, which
	// must cancel the first session before starting the second.
	_ = startSession(ctx)
	time.Sleep(300 * time.Millisecond)
	afterSecond := countRetryWatcherGoroutines(t)
	t.Logf("RetryWatcher-related goroutines after second onReady (session swap): %d (delta from first=%d)", afterSecond, afterSecond-afterFirst)
	if afterSecond > afterFirst {
		t.Fatalf("expected watcher goroutine count to stay flat across a session swap, not accumulate: afterFirst=%d afterSecond=%d", afterFirst, afterSecond)
	}

	sessionCancel()
	got := waitForGoroutineCount(t, before, 5*time.Second)
	if got != before {
		t.Fatalf("expected RetryWatcher goroutines to return to baseline %d after the final cancellation, got %d", before, got)
	}
}

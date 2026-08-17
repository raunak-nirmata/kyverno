package watch

import (
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8swatch "k8s.io/apimachinery/pkg/watch"
)

// fakeObj implements runtime.Object and resourceVersionGetter.
type fakeObj struct {
	rv string
}

// Satisfy runtime.Object.
func (f *fakeObj) GetObjectKind() schema.ObjectKind { return schema.EmptyObjectKind }
func (f *fakeObj) DeepCopyObject() runtime.Object   { return &fakeObj{rv: f.rv} }

// resourceVersionGetter method used by the watcher under test.
func (f *fakeObj) GetResourceVersion() string { return f.rv }

// fakeWatcherClient implements a minimal Watcher returning a sequence of watchers/errors.
type fakeWatcherClient struct {
	mu        sync.Mutex
	watchers  []k8swatch.Interface
	errs      []error
	calls     int
	opts      []metav1.ListOptions
	callTimes []time.Time
}

func (f *fakeWatcherClient) Watch(options metav1.ListOptions) (k8swatch.Interface, error) {
	f.mu.Lock()
	f.opts = append(f.opts, options)
	f.callTimes = append(f.callTimes, time.Now())
	idx := f.calls
	f.calls++
	f.mu.Unlock()
	if idx < len(f.errs) && f.errs[idx] != nil {
		return nil, f.errs[idx]
	}
	if idx < len(f.watchers) {
		return f.watchers[idx], nil
	}
	return k8swatch.NewRaceFreeFake(), nil
}

// times returns a snapshot of the recorded Watch() call timestamps.
func (f *fakeWatcherClient) times() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.callTimes...)
}

func TestNewRetryWatcherRejectsEmptyOrZeroRV(t *testing.T) {
	fc := &fakeWatcherClient{}
	if _, err := NewRetryWatcher("", fc); err == nil {
		t.Fatalf("expected error for empty initial RV, got nil")
	}
	if _, err := NewRetryWatcher("0", fc); err == nil {
		t.Fatalf("expected error for '0' initial RV, got nil")
	}
}

func TestRetryWatcherProcessesEventsAndUpdatesResourceVersion(t *testing.T) {
	fw := k8swatch.NewRaceFreeFake()
	fw.Add(&fakeObj{rv: "10"})
	fw.Modify(&fakeObj{rv: "11"})
	fw.Delete(&fakeObj{rv: "12"})
	fw.Action(k8swatch.Bookmark, &fakeObj{rv: "13"})

	rw, err := newRetryWatcher("9", &fakeWatcherClient{watchers: []k8swatch.Interface{fw}}, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("newRetryWatcher error: %v", err)
	}
	defer rw.Stop()

	got := make([]k8swatch.Event, 0, 3)
	timeout := time.After(2 * time.Second)
	for len(got) < 3 {
		select {
		case ev, ok := <-rw.ResultChan():
			if !ok {
				t.Fatalf("result channel closed prematurely")
			}
			got = append(got, ev)
		case <-timeout:
			t.Fatalf("timed out waiting for events, got %d", len(got))
		}
	}

	if got[0].Type != k8swatch.Added || got[1].Type != k8swatch.Modified || got[2].Type != k8swatch.Deleted {
		t.Fatalf("unexpected event types: %+v", got)
	}

	// Small delay to ensure Bookmark event is processed
	time.Sleep(100 * time.Millisecond)

	// Thread-safe read of lastResourceVersion
	rw.mu.RLock()
	actualRV := rw.lastResourceVersion
	rw.mu.RUnlock()

	if actualRV != "13" {
		t.Fatalf("expected lastResourceVersion=13, got=%s", actualRV)
	}
}

func TestRetryWatcherStopsOnGoneError(t *testing.T) {
	fw := k8swatch.NewRaceFreeFake()
	fw.Error(&metav1.Status{Code: http.StatusGone})
	rw, err := newRetryWatcher("100", &fakeWatcherClient{watchers: []k8swatch.Interface{fw}}, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("newRetryWatcher error: %v", err)
	}
	defer rw.Stop()

	select {
	case ev := <-rw.ResultChan():
		if ev.Type != k8swatch.Error {
			t.Fatalf("expected Error event, got %v", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for error event")
	}

	select {
	case <-rw.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for watcher to stop after 410 Gone")
	}
}

func TestRetryWatcherRetriesOnEOFThenSucceeds(t *testing.T) {
	fw := k8swatch.NewRaceFreeFake()
	fw.Add(&fakeObj{rv: "21"})
	// The first call to Watch() will return an error.
	// The second call will return the watcher `fw`.
	fc := &fakeWatcherClient{
		errs:     []error{io.EOF},
		watchers: []k8swatch.Interface{nil, fw},
	}

	rw, err := newRetryWatcher("20", fc, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("newRetryWatcher error: %v", err)
	}
	defer rw.Stop()

	select {
	case ev := <-rw.ResultChan():
		if ev.Type != k8swatch.Added {
			t.Fatalf("expected Added event after retry, got %v", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for event after EOF retry")
	}

	if len(fc.opts) < 2 || fc.opts[0].ResourceVersion != "20" {
		t.Fatalf("expected first watch to use RV=20, got opts=%+v", fc.opts)
	}
}

type noRVObj struct{ metav1.TypeMeta }

// Satisfy runtime.Object for noRVObj.
func (o *noRVObj) GetObjectKind() schema.ObjectKind { return schema.EmptyObjectKind }
func (o *noRVObj) DeepCopyObject() runtime.Object   { return &noRVObj{} }

func TestRetryWatcherErrorOnNonRVObjectStops(t *testing.T) {
	funcObj := &noRVObj{}

	fw := k8swatch.NewRaceFreeFake()
	fw.Add(funcObj)

	rw, err := newRetryWatcher("50", &fakeWatcherClient{watchers: []k8swatch.Interface{fw}}, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("newRetryWatcher error: %v", err)
	}
	defer rw.Stop()

	select {
	case ev := <-rw.ResultChan():
		if ev.Type != k8swatch.Error {
			t.Fatalf("expected Error event for non-RV object, got %v", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for error event")
	}

	select {
	case <-rw.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for watcher to stop after non-RV object")
	}
}

func TestRetryWatcherStop(t *testing.T) {
	rw, err := newRetryWatcher("77", &fakeWatcherClient{watchers: []k8swatch.Interface{k8swatch.NewRaceFreeFake()}}, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("newRetryWatcher error: %v", err)
	}
	rw.Stop()
	select {
	case <-rw.Done():
	case <-time.After(1 * time.Second):
		t.Fatalf("expected Done to be closed after Stop()")
	}
}

// TestRetryWatcherBackoffGrowsAndResetsOnSuccess exercises the exponential
// backoff added to receive(): consecutive rounds that fail to even connect
// must back off with growing delays, and a round that does connect (streams
// at least one event before dropping) must reset the backoff back down
// instead of letting it keep growing.
func TestRetryWatcherBackoffGrowsAndResetsOnSuccess(t *testing.T) {
	const minDelay = 30 * time.Millisecond

	fwConnected := k8swatch.NewRaceFreeFake()
	fwConnected.Add(&fakeObj{rv: "2"})

	fwAfterReset := k8swatch.NewRaceFreeFake()
	fwAfterReset.Add(&fakeObj{rv: "3"})

	fc := &fakeWatcherClient{
		errs:     []error{errors.New("boom-1"), errors.New("boom-2"), errors.New("boom-3")},
		watchers: []k8swatch.Interface{nil, nil, nil, fwConnected, fwAfterReset},
	}

	rw, err := newRetryWatcher("1", fc, minDelay)
	if err != nil {
		t.Fatalf("newRetryWatcher error: %v", err)
	}
	defer rw.Stop()

	// The first event only arrives after 3 failed connection attempts, each
	// waiting a progressively longer backoff.
	select {
	case ev := <-rw.ResultChan():
		if ev.Type != k8swatch.Added {
			t.Fatalf("expected Added event, got %v", ev.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for event after retries")
	}

	times := fc.times()
	if len(times) < 4 {
		t.Fatalf("expected at least 4 Watch() calls (3 failures + 1 success), got %d", len(times))
	}

	gap1 := times[1].Sub(times[0])
	gap2 := times[2].Sub(times[1])
	gap3 := times[3].Sub(times[2])
	t.Logf("backoff gaps before connecting: %v, %v, %v", gap1, gap2, gap3)

	if gap2 < gap1+minDelay/2 {
		t.Fatalf("expected backoff to grow between attempt 1 and 2: gap1=%v gap2=%v", gap1, gap2)
	}
	if gap3 < gap2+minDelay/2 {
		t.Fatalf("expected backoff to keep growing between attempt 2 and 3: gap2=%v gap3=%v", gap2, gap3)
	}

	// End the connected round; the reconnect that follows should use the
	// reset (~minDelay) backoff rather than continuing to grow from gap3.
	fwConnected.Stop()

	select {
	case ev := <-rw.ResultChan():
		if ev.Type != k8swatch.Added {
			t.Fatalf("expected Added event after reset, got %v", ev.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for event after backoff reset")
	}

	times = fc.times()
	if len(times) < 5 {
		t.Fatalf("expected a 5th Watch() call after the connected round ended, got %d", len(times))
	}
	gapAfterReset := times[4].Sub(times[3])
	t.Logf("gap after a connected round: %v (gap3 was %v)", gapAfterReset, gap3)
	if gapAfterReset >= gap3 {
		t.Fatalf("expected backoff to reset to ~minDelay after a connected round, not continue growing: gap3=%v gapAfterReset=%v", gap3, gapAfterReset)
	}
}

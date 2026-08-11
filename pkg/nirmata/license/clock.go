package license

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Keys in the license-state ConfigMap. Values are RFC3339 UTC strings.
const (
	keySchemaVersion = "schemaVersion"
	keyFirstSeen     = "firstSeen"
	keyLastSeen      = "lastSeen"
	keyGraceStarted  = "graceStartedAt"
	keyLastState     = "lastState"

	clockSchemaVersion = "1"

	// skewTolerance is how far the clock may move backwards before we stop trusting
	// it. Small steps are normal NTP corrections.
	skewTolerance = 5 * time.Minute

	// forwardTrustHorizon bounds how far ahead of now a stored lastSeen may be and
	// still be believed; beyond it, it is treated as corrupt. Without this, a pod
	// that starts with a wrong future clock poisons lastSeen permanently. See the
	// clock section of README.md for the trade this makes.
	forwardTrustHorizon = 24 * time.Hour
)

// errClockAbsent means the license-state ConfigMap does not exist yet.
var errClockAbsent = errors.New("license state configmap not found")

// clockState is the persisted trial and grace bookkeeping, held in a ConfigMap so
// it survives restarts and is shared across replicas. Deliberately unsigned;
// deleting it grants a fresh trial, which is an accepted risk (README.md).
type clockState struct {
	// FirstSeen anchors the trial window. Written once, never overwritten.
	FirstSeen time.Time

	// LastSeen is refreshed each leader tick, to detect a backwards clock.
	LastSeen time.Time

	// GraceStartedAt is first detection of non-compliance. Reserved for Phase 2.
	GraceStartedAt time.Time

	// LastState is the last reported state, so events fire only on transition.
	LastState State
}

// parseClock reads a clockState out of ConfigMap data. Unparseable timestamps yield
// a zero field plus an error, so the caller can log while using what was readable.
// A zero FirstSeen drives Evaluate to FreeTier, never FreeExpired: a corrupt
// ConfigMap must not present a cluster as out of compliance.
func parseClock(data map[string]string) (clockState, error) {
	var (
		out  clockState
		errs []error
	)
	parse := func(key string, dst *time.Time) {
		raw, ok := data[key]
		if !ok || raw == "" {
			return
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s=%q: %w", key, raw, err))
			return
		}
		*dst = t.UTC()
	}
	parse(keyFirstSeen, &out.FirstSeen)
	parse(keyLastSeen, &out.LastSeen)
	parse(keyGraceStarted, &out.GraceStartedAt)
	out.LastState = State(data[keyLastState])
	return out, errors.Join(errs...)
}

// renderClock serialises a clockState back to ConfigMap data.
func renderClock(s clockState) map[string]string {
	data := map[string]string{keySchemaVersion: clockSchemaVersion}
	put := func(key string, t time.Time) {
		if t.IsZero() {
			return
		}
		data[key] = t.UTC().Format(time.RFC3339)
	}
	put(keyFirstSeen, s.FirstSeen)
	put(keyLastSeen, s.LastSeen)
	put(keyGraceStarted, s.GraceStartedAt)
	if s.LastState != "" {
		data[keyLastState] = string(s.LastState)
	}
	return data
}

// skewVerdict describes how a stored lastSeen compared to the current clock.
type skewVerdict int

const (
	// clockTrusted means the current clock advanced normally.
	clockTrusted skewVerdict = iota

	// clockRolledBack means the clock is behind lastSeen by more than
	// skewTolerance but within the trust horizon. lastSeen is used instead, so the
	// trial neither resets nor advances.
	clockRolledBack

	// clockStoredUnusable means lastSeen is implausibly far ahead; it is discarded
	// in favour of the current clock. See forwardTrustHorizon.
	clockStoredUnusable
)

// effectiveNow decides which time to measure the trial against, returning that
// time and a verdict so the caller can log and know when to overwrite a poisoned
// lastSeen. A sustained rollback freezes the trial rather than expiring it; it does
// not prevent extension. See README.md.
func effectiveNow(now, lastSeen time.Time) (time.Time, skewVerdict) {
	if lastSeen.IsZero() {
		return now, clockTrusted
	}
	if now.After(lastSeen.Add(forwardTrustHorizon)) || !now.Before(lastSeen.Add(-skewTolerance)) {
		// Advanced normally, or so far ahead that nothing can be concluded; either
		// way the current clock is the best answer.
		return now, clockTrusted
	}
	if lastSeen.After(now.Add(forwardTrustHorizon)) {
		return now, clockStoredUnusable
	}
	return lastSeen, clockRolledBack
}

// readClock fetches the license-state ConfigMap.
// A missing ConfigMap is reported as errClockAbsent so callers can distinguish
// "not provisioned yet" from a genuine API failure.
func readClock(ctx context.Context, client kubernetes.Interface, namespace, name string) (clockState, *corev1.ConfigMap, error) {
	cm, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return clockState{}, nil, errClockAbsent
		}
		return clockState{}, nil, err
	}
	state, parseErr := parseClock(cm.Data)
	return state, cm, parseErr
}

// createClock provisions the license-state ConfigMap with the trial starting now.
// The chart normally ships it; this covers non-Helm installs and self-healing.
// AlreadyExists is resolved by re-reading the winner's value, not by retrying.
func createClock(ctx context.Context, client kubernetes.Interface, namespace, name string, now time.Time) (clockState, error) {
	want := clockState{FirstSeen: now, LastSeen: now}
	_, err := client.CoreV1().ConfigMaps(namespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       renderClock(want),
	}, metav1.CreateOptions{})
	if err == nil {
		return want, nil
	}
	if apierrors.IsAlreadyExists(err) {
		state, _, readErr := readClock(ctx, client, namespace, name)
		if readErr != nil {
			return clockState{}, readErr
		}
		return state, nil
	}
	return clockState{}, err
}

// writeClock persists updated bookkeeping. FirstSeen is write-once — including when
// unparseable, since a repair is indistinguishable from a trial reset. Leader only;
// conflicts are surfaced and retried on the next tick.
func writeClock(ctx context.Context, client kubernetes.Interface, cm *corev1.ConfigMap, want clockState) error {
	updated := cm.DeepCopy()
	if updated.Data == nil {
		updated.Data = map[string]string{}
	}

	next := renderClock(want)
	for k, v := range next {
		// Never rewrite an existing firstSeen, parseable or not.
		if k == keyFirstSeen {
			if existing, ok := updated.Data[keyFirstSeen]; ok && existing != "" {
				continue
			}
		}
		updated.Data[k] = v
	}

	_, err := client.CoreV1().ConfigMaps(updated.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
	return err
}

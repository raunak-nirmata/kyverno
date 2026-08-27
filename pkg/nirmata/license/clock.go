package license

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/kyverno/kyverno/pkg/config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Keys in the license-state ConfigMap. Values are RFC3339 UTC strings.
const (
	keySchemaVersion  = "schemaVersion"
	keyFirstSeen      = "firstSeen"
	keyLastSeen       = "lastSeen"
	keyGraceStarted   = "graceStartedAt"
	keyCompliantSince = "compliantSince"
	keyLastState      = "lastState"
	keyHMAC           = "hmac"

	clockSchemaVersion = "1"

	// skewTolerance is how far the clock may move backwards before we stop trusting
	// it. Small steps are normal NTP corrections.
	skewTolerance = 5 * time.Minute

	// forwardTrustHorizon bounds how far ahead of now a stored lastSeen may be and
	// still be believed; beyond it, it is treated as corrupt. Without this, a pod
	// that starts with a wrong future clock poisons lastSeen permanently. See the
	// clock section of README.md for the trade this makes.
	forwardTrustHorizon = 24 * time.Hour

	hmacKeyMaterialEnv = "N4K_LICENSE_STATE_HMAC_SEED"
)

// errClockAbsent means the license-state ConfigMap does not exist yet.
var errClockAbsent = errors.New("license state configmap not found")

// clockState is the persisted trial and grace bookkeeping, held in a ConfigMap so
// it survives restarts and is shared across replicas.
type clockState struct {
	// FirstSeen anchors the trial window. Written once, never overwritten.
	FirstSeen time.Time

	// LastSeen is refreshed each leader tick, to detect a backwards clock.
	LastSeen time.Time

	// GraceStartedAt is first detection of paid non-compliance.
	GraceStartedAt time.Time

	// CompliantSince is when a cluster holding a grace anchor last became
	// compliant again. Zero while non-compliant. See nextGraceState.
	CompliantSince time.Time

	// LastState is the last reported state, so events fire only on transition.
	LastState State

	// HMACValid and HMACPresent are parse results, not persisted fields:
	// renderClock ignores them and recomputes the MAC from the data it writes.
	// Only parseClock sets them meaningfully.
	//
	// HMACValid is true when the stored MAC matched, or when no MAC was expected
	// yet. Soft integrity: a mismatch is reported but never fails closed.
	HMACValid bool

	// HMACPresent is true when the ConfigMap carried an hmac field.
	HMACPresent bool
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
	parse(keyCompliantSince, &out.CompliantSince)
	out.LastState = State(data[keyLastState])

	if mac, ok := data[keyHMAC]; ok && mac != "" {
		out.HMACPresent = true
		out.HMACValid = hmac.Equal([]byte(mac), []byte(computeHMAC(data)))
	} else {
		// No MAC yet — normal for upgrades from Phase 1. Soft: treat as valid.
		out.HMACValid = true
	}
	return out, errors.Join(errs...)
}

// renderClock serialises a clockState back to ConfigMap data, including a soft HMAC.
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
	put(keyCompliantSince, s.CompliantSince)
	if s.LastState != "" {
		data[keyLastState] = string(s.LastState)
	}
	data[keyHMAC] = computeHMAC(data)
	return data
}

func computeHMAC(data map[string]string) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		if k == keyHMAC {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(data[k])
		b.WriteByte('\n')
	}
	mac := hmac.New(sha256.New, hmacKey())
	_, _ = mac.Write([]byte(b.String()))
	return hex.EncodeToString(mac.Sum(nil))
}

// hmacKey returns the checksum key. Prefers an explicit seed (tests, and installs
// that want a real secret); otherwise derives from the namespace so replicas agree
// without shipping a private key in the binary. A namespace-derived key is not
// secret — see computeHMAC for what that does and does not buy.
func hmacKey() []byte {
	if seed := os.Getenv(hmacKeyMaterialEnv); seed != "" {
		sum := sha256.Sum256([]byte(seed))
		return sum[:]
	}
	sum := sha256.Sum256([]byte("n4k-license-state|" + config.KyvernoNamespace()))
	return sum[:]
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

// Helm's object-adoption keys. Helm refuses to take over an existing object
// unless all three agree with the release it is installing.
const (
	helmManagedByLabel       = "app.kubernetes.io/managed-by"
	helmReleaseNameAnno      = "meta.helm.sh/release-name"
	helmReleaseNamespaceAnno = "meta.helm.sh/release-namespace"
	helmResourcePolicyAnno   = "helm.sh/resource-policy"
	helmManagedByValue       = "Helm"
)

// helmAdoption returns the labels and annotations that let Helm adopt an object
// we create into the release that owns this controller, copied from the
// controller's own Deployment.
//
// Without them a self-healed ConfigMap is orphaned from Helm's point of view,
// and the operator's next install or upgrade fails outright with "invalid
// ownership metadata" — a self-heal that breaks the next upgrade is not a heal.
// resource-policy: keep matches the chart, so the recreated ConfigMap also
// survives an uninstall and does not hand out a fresh trial window.
//
// Best-effort by design: any failure, or a Deployment that Helm does not manage,
// returns nils and the ConfigMap is created bare — correct for non-Helm installs.
func helmAdoption(ctx context.Context, client kubernetes.Interface, namespace, deploymentName string) (map[string]string, map[string]string) {
	if deploymentName == "" {
		return nil, nil
	}
	d, err := client.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		return nil, nil
	}
	if d.Labels[helmManagedByLabel] != helmManagedByValue {
		return nil, nil
	}
	release := d.Annotations[helmReleaseNameAnno]
	releaseNS := d.Annotations[helmReleaseNamespaceAnno]
	if release == "" || releaseNS == "" {
		return nil, nil
	}
	return map[string]string{
			helmManagedByLabel: helmManagedByValue,
		}, map[string]string{
			helmReleaseNameAnno:      release,
			helmReleaseNamespaceAnno: releaseNS,
			helmResourcePolicyAnno:   "keep",
		}
}

// createClock provisions the license-state ConfigMap with the trial starting now.
// The chart normally ships it; this covers non-Helm installs and self-healing.
// AlreadyExists is resolved by re-reading the winner's value, not by retrying.
//
// labels and annotations carry Helm's adoption keys when the install is
// Helm-managed; see helmAdoption. Both may be nil.
func createClock(ctx context.Context, client kubernetes.Interface, namespace, name string, now time.Time, labels, annotations map[string]string) (clockState, error) {
	want := clockState{FirstSeen: now, LastSeen: now}
	_, err := client.CoreV1().ConfigMaps(namespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Data: renderClock(want),
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
		if k == keyHMAC {
			continue
		}
		// Never rewrite an existing firstSeen, parseable or not.
		if k == keyFirstSeen {
			if existing, ok := updated.Data[keyFirstSeen]; ok && existing != "" {
				continue
			}
		}
		updated.Data[k] = v
	}
	// renderClock omits zero timestamps, so a cleared value needs an explicit
	// delete or the stale one would survive the merge above.
	if want.GraceStartedAt.IsZero() {
		delete(updated.Data, keyGraceStarted)
	}
	if want.CompliantSince.IsZero() {
		delete(updated.Data, keyCompliantSince)
	}
	updated.Data[keyHMAC] = computeHMAC(updated.Data)

	_, err := client.CoreV1().ConfigMaps(updated.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
	return err
}

const graceClearAfter = 24 * time.Hour

func nextGraceState(prev clockState, state State, now time.Time) (graceStartedAt, compliantSince time.Time) {
	switch state {
	case Grace, Degraded:
		if prev.GraceStartedAt.IsZero() {
			return now, time.Time{}
		}
		return prev.GraceStartedAt, time.Time{}
	default:
		if prev.GraceStartedAt.IsZero() {
			return time.Time{}, time.Time{}
		}
		since := prev.CompliantSince
		if since.IsZero() {
			since = now
		}
		if !now.Before(since.Add(graceClearAfter)) {
			return time.Time{}, time.Time{}
		}
		return prev.GraceStartedAt, since
	}
}

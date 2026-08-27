package license

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testNamespace = "kyverno"
	testClockName = "kyverno-license-state"
)

func TestParseClockRoundTrip(t *testing.T) {
	t.Parallel()

	want := clockState{
		FirstSeen:      base,
		LastSeen:       base.Add(time.Hour),
		GraceStartedAt: base.Add(2 * time.Hour),
		LastState:      FreeTier,
	}
	got, err := parseClock(renderClock(want))
	if err != nil {
		t.Fatal(err)
	}
	if !got.FirstSeen.Equal(want.FirstSeen) || !got.LastSeen.Equal(want.LastSeen) ||
		!got.GraceStartedAt.Equal(want.GraceStartedAt) || got.LastState != want.LastState {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestRenderClockOmitsZeroTimes(t *testing.T) {
	t.Parallel()

	data := renderClock(clockState{FirstSeen: base})
	if _, ok := data[keyGraceStarted]; ok {
		t.Error("a zero graceStartedAt must not be written as an empty or epoch value")
	}
	if data[keySchemaVersion] != clockSchemaVersion {
		t.Errorf("schemaVersion = %q, want %q", data[keySchemaVersion], clockSchemaVersion)
	}
}

// A corrupt timestamp must be reported AND must leave FirstSeen zero, because
// Evaluate reads a zero FirstSeen as "trial active" rather than "trial expired".
func TestParseClockCorruptFailsOpen(t *testing.T) {
	t.Parallel()

	got, err := parseClock(map[string]string{keyFirstSeen: "not-a-date"})
	if err == nil {
		t.Fatal("a corrupt firstSeen must be reported")
	}
	if !got.FirstSeen.IsZero() {
		t.Error("a corrupt firstSeen must leave the value zero so evaluation fails open")
	}
	snap := Evaluate(Inputs{Now: base, TrialStart: got.FirstSeen, Limits: windows})
	if snap.State != FreeTier {
		t.Errorf("State = %q, want FreeTier; a corrupt clock must never expire a cluster", snap.State)
	}
}

func TestParseClockEmptyIsNotAnError(t *testing.T) {
	t.Parallel()

	got, err := parseClock(nil)
	if err != nil {
		t.Fatalf("an empty ConfigMap is the normal chart-provisioned state, got %v", err)
	}
	if !got.FirstSeen.IsZero() {
		t.Error("FirstSeen should be zero for empty data")
	}
}

func TestEffectiveNow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		now         time.Time
		lastSeen    time.Time
		wantNow     time.Time
		wantVerdict skewVerdict
	}{
		{"no stored lastSeen", base, time.Time{}, base, clockTrusted},
		{"normal forward progress", base.Add(time.Hour), base, base.Add(time.Hour), clockTrusted},
		{"small backward step within tolerance", base.Add(-time.Minute), base, base.Add(-time.Minute), clockTrusted},
		{"rollback beyond tolerance uses the recorded time", base.Add(-time.Hour), base, base, clockRolledBack},
		{
			name:        "stored value implausibly far ahead is discarded",
			now:         base,
			lastSeen:    base.Add(365 * 24 * time.Hour),
			wantNow:     base,
			wantVerdict: clockStoredUnusable,
		},
		{
			name:        "large forward jump is trusted, not treated as a rollback",
			now:         base.Add(365 * 24 * time.Hour),
			lastSeen:    base,
			wantNow:     base.Add(365 * 24 * time.Hour),
			wantVerdict: clockTrusted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotNow, gotVerdict := effectiveNow(tt.now, tt.lastSeen)
			if !gotNow.Equal(tt.wantNow) {
				t.Errorf("now = %v, want %v", gotNow, tt.wantNow)
			}
			if gotVerdict != tt.wantVerdict {
				t.Errorf("verdict = %v, want %v", gotVerdict, tt.wantVerdict)
			}
		})
	}
}

// A rollback within the trust horizon holds the trial where it was rather than
// resetting it. Note this asserts freezing, NOT expiry: the clamp stops a
// rolled-back clock from handing back trial time, but because lastSeen only ever
// advances from the clamped value the trial does not continue to elapse either.
// That is the deliberate fail-open outcome.
func TestRollbackFreezesRatherThanResetsTrial(t *testing.T) {
	t.Parallel()

	trialStart := base
	lastSeen := base.Add(days(14))
	rolledBack := lastSeen.Add(-2 * time.Hour) // inside forwardTrustHorizon

	now, verdict := effectiveNow(rolledBack, lastSeen)
	if verdict != clockRolledBack {
		t.Fatalf("verdict = %v, want clockRolledBack", verdict)
	}
	// The production path feeds the clamped value straight into Evaluate.
	snap := Evaluate(Inputs{Now: now, TrialStart: trialStart, Limits: windows})
	if snap.State != FreeTier {
		t.Errorf("State = %q, want FreeTier", snap.State)
	}
	// Held at day 14 of a 15-day trial, not rewound to the rolled-back time.
	if snap.DaysRemaining != 1 {
		t.Errorf("DaysRemaining = %d, want 1 — the trial is held at day 14 of 15", snap.DaysRemaining)
	}
	if !snap.Expiry.Equal(trialStart.Add(days(15))) {
		t.Errorf("Expiry = %v, want the original trial end", snap.Expiry)
	}
}

// The documented cost of healing a poisoned clock: a rollback larger than
// forwardTrustHorizon is indistinguishable from a corrupt stored observation, so
// the trial re-anchors to the current clock instead of staying frozen.
//
// This is deliberate. The alternative — trusting an arbitrarily far-future stored
// value — pins a cluster whose clock was briefly wrong at startup to FreeExpired
// permanently, with no recovery but hand-editing the ConfigMap. Fail-open is a
// requirement here; resisting tampering is explicitly not.
func TestLargeRollbackReAnchorsRatherThanPinningExpired(t *testing.T) {
	t.Parallel()

	_, verdict := effectiveNow(base, base.Add(days(365)))
	if verdict != clockStoredUnusable {
		t.Fatalf("verdict = %v, want clockStoredUnusable", verdict)
	}
	now, _ := effectiveNow(base, base.Add(days(365)))
	if !now.Equal(base) {
		t.Errorf("now = %v, want the current clock %v", now, base)
	}
}

func clockCM(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testClockName, Namespace: testNamespace},
		Data:       data,
	}
}

func TestReadClockAbsent(t *testing.T) {
	t.Parallel()

	_, _, err := readClock(context.Background(), fake.NewSimpleClientset(), testNamespace, testClockName)
	if !errors.Is(err, errClockAbsent) {
		t.Fatalf("err = %v, want errClockAbsent", err)
	}
}

func TestCreateClockStampsFirstSeen(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	got, err := createClock(context.Background(), client, testNamespace, testClockName, base, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.FirstSeen.Equal(base) {
		t.Errorf("FirstSeen = %v, want %v", got.FirstSeen, base)
	}

	stored, _, err := readClock(context.Background(), client, testNamespace, testClockName)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.FirstSeen.Equal(base) {
		t.Errorf("persisted FirstSeen = %v, want %v", stored.FirstSeen, base)
	}
}

// During leader handover two replicas may both try to create. The loser must
// adopt the winner's timestamp rather than overwrite it or error out.
func TestCreateClockAdoptsExistingOnRace(t *testing.T) {
	t.Parallel()

	existing := base.Add(-days(10))
	client := fake.NewSimpleClientset(clockCM(renderClock(clockState{FirstSeen: existing, LastSeen: existing})))
	client.PrependReactor("create", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(
			schema.GroupResource{Resource: "configmaps"}, testClockName)
	})

	got, err := createClock(context.Background(), client, testNamespace, testClockName, base, nil, nil)
	if err != nil {
		t.Fatalf("an AlreadyExists race must be resolved by adopting the winner, got %v", err)
	}
	if !got.FirstSeen.Equal(existing) {
		t.Errorf("FirstSeen = %v, want the pre-existing %v", got.FirstSeen, existing)
	}
}

// The single most important persistence property: an established trial start is
// never rewritten, because doing so would silently restart a customer's trial.
func TestWriteClockNeverOverwritesFirstSeen(t *testing.T) {
	t.Parallel()

	original := base.Add(-days(10))
	client := fake.NewSimpleClientset(clockCM(renderClock(clockState{FirstSeen: original, LastSeen: original})))

	_, cm, err := readClock(context.Background(), client, testNamespace, testClockName)
	if err != nil {
		t.Fatal(err)
	}
	// A later tick tries to write a different firstSeen.
	err = writeClock(context.Background(), client, cm, clockState{
		FirstSeen: base,
		LastSeen:  base,
		LastState: FreeTier,
	})
	if err != nil {
		t.Fatal(err)
	}

	stored, _, err := readClock(context.Background(), client, testNamespace, testClockName)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.FirstSeen.Equal(original) {
		t.Errorf("FirstSeen = %v, want the original %v — the trial must not restart", stored.FirstSeen, original)
	}
	if !stored.LastSeen.Equal(base) {
		t.Errorf("LastSeen = %v, want it advanced to %v", stored.LastSeen, base)
	}
	if stored.LastState != FreeTier {
		t.Errorf("LastState = %q, want FreeTier", stored.LastState)
	}
}

// An unparseable stored firstSeen is left alone rather than repaired: a repair is
// indistinguishable from a reset, and resetting is the worse failure.
func TestWriteClockLeavesCorruptFirstSeenAlone(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(clockCM(map[string]string{keyFirstSeen: "garbage"}))
	_, cm, _ := readClock(context.Background(), client, testNamespace, testClockName)

	if err := writeClock(context.Background(), client, cm, clockState{FirstSeen: base, LastSeen: base}); err != nil {
		t.Fatal(err)
	}
	stored, err := client.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), testClockName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stored.Data[keyFirstSeen] != "garbage" {
		t.Errorf("firstSeen = %q, want it left untouched", stored.Data[keyFirstSeen])
	}
}

func TestWriteClockSurfacesConflict(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(clockCM(renderClock(clockState{FirstSeen: base})))
	_, cm, _ := readClock(context.Background(), client, testNamespace, testClockName)
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Resource: "configmaps"}, testClockName, errors.New("stale"))
	})

	err := writeClock(context.Background(), client, cm, clockState{LastSeen: base})
	if !apierrors.IsConflict(err) {
		t.Fatalf("err = %v, want a conflict so the next tick retries", err)
	}
}

func TestRenderClockIncludesHMAC(t *testing.T) {
	t.Setenv(hmacKeyMaterialEnv, "test-seed")
	data := renderClock(clockState{FirstSeen: base, LastSeen: base, LastState: FreeTier})
	if data[keyHMAC] == "" {
		t.Fatal("hmac must be present")
	}
	got, err := parseClock(data)
	if err != nil {
		t.Fatal(err)
	}
	if !got.HMACPresent || !got.HMACValid {
		t.Errorf("HMACPresent=%v HMACValid=%v, want both true", got.HMACPresent, got.HMACValid)
	}
}

func TestParseClockSoftHMACMismatch(t *testing.T) {
	t.Setenv(hmacKeyMaterialEnv, "test-seed")
	data := renderClock(clockState{FirstSeen: base, LastSeen: base})
	data[keyHMAC] = "deadbeef"
	got, err := parseClock(data)
	if err != nil {
		t.Fatal(err)
	}
	if !got.HMACPresent {
		t.Error("HMACPresent must be true")
	}
	if got.HMACValid {
		t.Error("tampered hmac must be invalid")
	}
	// Fail-open: timestamps remain usable.
	if !got.FirstSeen.Equal(base) {
		t.Errorf("FirstSeen = %v, want %v despite hmac mismatch", got.FirstSeen, base)
	}
}

func TestNextGraceState(t *testing.T) {
	t.Parallel()

	prev := base.Add(-days(5))

	t.Run("first detection stamps now", func(t *testing.T) {
		got, compliant := nextGraceState(clockState{}, Grace, base)
		if !got.Equal(base) {
			t.Errorf("grace anchor = %v, want %v", got, base)
		}
		if !compliant.IsZero() {
			t.Errorf("compliantSince = %v, want zero while non-compliant", compliant)
		}
	})

	t.Run("ongoing non-compliance keeps the original stamp", func(t *testing.T) {
		got, _ := nextGraceState(clockState{GraceStartedAt: prev}, Degraded, base)
		if !got.Equal(prev) {
			t.Errorf("grace anchor = %v, want the original %v", got, prev)
		}
	})

	// A cluster oscillating around its node-unit entitlement would otherwise
	// restart the grace window on every dip and never reach Degraded.
	t.Run("brief compliance holds the anchor", func(t *testing.T) {
		got, compliant := nextGraceState(clockState{GraceStartedAt: prev}, Licensed, base)
		if !got.Equal(prev) {
			t.Errorf("grace anchor = %v, want it held at %v until compliance is sustained", got, prev)
		}
		if !compliant.Equal(base) {
			t.Errorf("compliantSince = %v, want %v", compliant, base)
		}
	})

	t.Run("sustained compliance clears the anchor", func(t *testing.T) {
		state := clockState{GraceStartedAt: prev, CompliantSince: base.Add(-graceClearAfter)}
		got, compliant := nextGraceState(state, Licensed, base)
		if !got.IsZero() {
			t.Errorf("grace anchor = %v, want cleared after %v of compliance", got, graceClearAfter)
		}
		if !compliant.IsZero() {
			t.Errorf("compliantSince = %v, want cleared alongside the anchor", compliant)
		}
	})

	t.Run("compliance without an anchor stays empty", func(t *testing.T) {
		got, compliant := nextGraceState(clockState{}, FreeTier, base)
		if !got.IsZero() || !compliant.IsZero() {
			t.Errorf("got (%v, %v), want both zero", got, compliant)
		}
	})
}

func TestWriteClockClearsGraceStartedAt(t *testing.T) {
	t.Setenv(hmacKeyMaterialEnv, "test-seed")
	client := fake.NewSimpleClientset(clockCM(renderClock(clockState{
		FirstSeen:      base,
		LastSeen:       base,
		GraceStartedAt: base.Add(time.Hour),
		LastState:      Grace,
	})))
	_, cm, err := readClock(context.Background(), client, testNamespace, testClockName)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeClock(context.Background(), client, cm, clockState{
		FirstSeen: base,
		LastSeen:  base,
		LastState: Licensed,
	}); err != nil {
		t.Fatal(err)
	}
	stored, _, err := readClock(context.Background(), client, testNamespace, testClockName)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.GraceStartedAt.IsZero() {
		t.Errorf("GraceStartedAt = %v, want cleared", stored.GraceStartedAt)
	}
}

// helmDeployment is the admission controller Deployment as Helm installs it.
func helmDeployment(name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "Helm"},
			Annotations: map[string]string{
				"meta.helm.sh/release-name":      "kyverno",
				"meta.helm.sh/release-namespace": testNamespace,
			},
		},
	}
}

// TestCreateClockCarriesHelmOwnership is the regression for a self-heal that
// broke the next upgrade. When the license-state ConfigMap is deleted the leader
// recreates it, but it used to do so with no metadata at all — so Helm no longer
// recognised it as part of the release and the operator's next install or
// upgrade failed with "invalid ownership metadata", needing hand-repair before
// the chart would deploy again.
func TestCreateClockCarriesHelmOwnership(t *testing.T) {
	const deployName = "kyverno-admission-controller"
	client := fake.NewSimpleClientset(helmDeployment(deployName))
	ctx := context.Background()

	labels, annotations := helmAdoption(ctx, client, testNamespace, deployName)
	if _, err := createClock(ctx, client, testNamespace, testClockName, base, labels, annotations); err != nil {
		t.Fatalf("createClock: %v", err)
	}

	cm, err := client.CoreV1().ConfigMaps(testNamespace).Get(ctx, testClockName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := cm.Labels["app.kubernetes.io/managed-by"]; got != "Helm" {
		t.Errorf("managed-by label = %q, want Helm", got)
	}
	if got := cm.Annotations["meta.helm.sh/release-name"]; got != "kyverno" {
		t.Errorf("release-name annotation = %q, want kyverno", got)
	}
	if got := cm.Annotations["meta.helm.sh/release-namespace"]; got != testNamespace {
		t.Errorf("release-namespace annotation = %q, want %q", got, testNamespace)
	}
	// Matches the chart, so a recreated ConfigMap also survives an uninstall
	// rather than silently resetting the customer's trial on reinstall.
	if got := cm.Annotations["helm.sh/resource-policy"]; got != "keep" {
		t.Errorf("resource-policy annotation = %q, want keep", got)
	}
}

// TestHelmAdoptionSkipsNonHelmInstalls keeps the self-heal working where there
// is no Helm release to adopt into: a plain ConfigMap is the right result, and a
// missing or unmanaged Deployment must never fail the create.
func TestHelmAdoptionSkipsNonHelmInstalls(t *testing.T) {
	const deployName = "kyverno-admission-controller"
	ctx := context.Background()

	plain := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: deployName, Namespace: testNamespace},
	}
	for _, tc := range []struct {
		name   string
		client kubernetes.Interface
	}{
		{"no deployment", fake.NewSimpleClientset()},
		{"deployment not managed by helm", fake.NewSimpleClientset(plain)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			labels, annotations := helmAdoption(ctx, tc.client, testNamespace, deployName)
			if labels != nil || annotations != nil {
				t.Fatalf("want no adoption metadata, got labels=%v annotations=%v", labels, annotations)
			}
			if _, err := createClock(ctx, tc.client, testNamespace, testClockName, base, labels, annotations); err != nil {
				t.Fatalf("createClock must still succeed: %v", err)
			}
		})
	}
}

package license

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/kyverno/kyverno/pkg/config"
	"github.com/kyverno/kyverno/pkg/event"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// AuthoritativeComponent is the only component that evaluates licensing. Four
// controllers share this startup path; letting each evaluate would mean four
// writers of the ConfigMap and four event sources.
const AuthoritativeComponent = "kyverno-admission-controller"

// current holds the most recent snapshot for package-level access via Current().
// AdmitPolicyCreate reads it on the policy CREATE path.
var current atomic.Pointer[Snapshot]

// Current returns the most recently evaluated state. Unknown before the first
// evaluation completes, which means "not yet known", not "problem".
func Current() Snapshot {
	if s := current.Load(); s != nil {
		return *s
	}
	return Snapshot{State: Unknown}
}

// service owns one evaluation loop. No context field: it is threaded through
// parameters so its lifetime stays visible at each call site.
type service struct {
	logger logr.Logger
	opts   Options
	now    func() time.Time

	metrics *instruments
	events  event.Interface

	// load resolves the active license. A field so tests can inject a throwaway key.
	load func(time.Time) (Active, error)

	// snapshot is this service's own view, so the loop is testable in isolation.
	snapshot atomic.Pointer[Snapshot]

	// ref is the object events attach to, resolved lazily and cached.
	ref      corev1.ObjectReference
	refValid bool

	// lastEmit throttles repeat reporting of a persistent condition.
	lastEmit map[string]time.Time

	// loggedOnce keys errors reported once per distinct cause.
	loggedOnce sync.Map
}

// Start begins license evaluation. Never blocks, never errors, never panics.
//
// Licensing is always on; there is no flag to disable it. Still a no-op for the
// non-authoritative components and when no kube client was built.
func Start(ctx context.Context, logger logr.Logger, opts Options) {
	if opts.Component != AuthoritativeComponent {
		return
	}
	if opts.KubeClient == nil {
		logger.V(2).Info("licensing disabled: no kube client available")
		return
	}
	if opts.ConfigMapName == "" {
		opts.ConfigMapName = config.KyvernoLicenseStateConfigMapName()
	}
	if opts.DeploymentName == "" {
		opts.DeploymentName = config.KyvernoDeploymentName()
	}

	path := opts.LicensePath
	s := &service{
		logger: logger,
		opts:   opts,
		now:    opts.clock(),
		load: func(now time.Time) (Active, error) {
			return LoadActive(now, path)
		},
		lastEmit: map[string]time.Time{},
	}
	if opts.EventsClient != nil {
		gen := event.NewEventGenerator(opts.EventsClient, logger.WithName("events"), 64, opts.Config)
		s.events = gen
		go gen.Run(ctx, 1)
	}
	go s.run(ctx)
}

// run drives the evaluation loop until ctx is cancelled. The outer recover means a
// bug here can only disable licensing observability, never the admission controller.
func (s *service) run(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error(fmt.Errorf("panic: %v", r),
				"license evaluator stopped unexpectedly; licensing observability is disabled for this process")
		}
	}()

	interval := s.opts.interval()
	s.logger.V(2).Info("starting license evaluator", "interval", interval, "component", s.opts.Component)
	defer s.logger.V(4).Info("license evaluator stopped")

	// Start runs before informers and leader election exist. Evaluating instantly
	// would compete with cache sync for the same rate-limited clients and always run
	// as a non-leader, delaying the trial anchor by a full interval. Jittered so HA
	// replicas do not align.
	select {
	case <-ctx.Done():
		return
	case <-time.After(wait.Jitter(startupDelay, 1.0)):
	}

	// Runs the first tick immediately, then every interval.
	wait.UntilWithContext(ctx, s.tick, interval)
}

// tick performs one evaluation. The recover is per-tick, not only around the loop,
// so a panic costs one evaluation rather than all future ones.
func (s *service) tick(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error(fmt.Errorf("panic: %v", r),
				"license evaluation failed; retaining previously reported state")
			s.metrics.recordError(ctx, "panic")
		}
	}()

	s.ensureInstruments()

	// One clock reading per tick: loading and evaluating against different times
	// could report a state inconsistent with the license just validated.
	now := s.now()

	active, err := s.load(now)
	switch {
	case errors.Is(err, errCommunityLicenseMissing), errors.Is(err, errTrustRootMissing):
		// No license artifact, or no key to verify one. Unprovisioned is an expected
		// build state an operator cannot act on, so it is reported once at default
		// verbosity. Nothing is evaluated — inventing limits would not be honest.
		s.infoOnce("unprovisioned", err.Error(),
			"Nirmata licensing is not provisioned in this build; no license state will be reported")
		s.metrics.recordError(ctx, "unprovisioned")
		return
	case err != nil:
		// Present but unverifiable is a genuine defect, and should be loud.
		s.logOnce("load", err, "the embedded Nirmata license failed to verify; license state cannot be reported")
		s.metrics.recordError(ctx, "load")
		return
	}
	if active.Invalid {
		cause := "invalid"
		if active.FileError != nil {
			cause = active.FileError.Error()
		}
		s.logOnce("file", fmt.Errorf("%s", cause),
			"commercial license file is present but invalid; treating as non-compliant")
		s.metrics.recordError(ctx, "file")
	}
	if active.FileMissing {
		// Secret deleted or mount empty: keep serving on free tier. Cap gate may
		// refuse new over-cap policies; workload admission stays on.
		s.logOnce("filemissing", errors.New("commercial license file missing"),
			"commercial license file is missing or empty; falling back to free-tier community license",
			"path", s.opts.LicensePath)
		s.metrics.recordError(ctx, "file_missing")
	}
	if len(active.Limits.UsedDefaults) > 0 {
		// Not an error: a community license legitimately carries no grace_days, so the
		// common case is a correct license falling back to a default.
		s.infoOnce("defaults", strings.Join(active.Limits.UsedDefaults, ","),
			"some capacity windows are not set in the active license; using built-in defaults",
			"fields", active.Limits.UsedDefaults)
	}

	// Operator override for the capacity warning threshold. Applied to active, not
	// just to Inputs, so metrics and logs describe the same limits Evaluate used.
	if s.opts.WarnRatio > 0 {
		active.Limits.WarnRatio = s.opts.WarnRatio
	}

	prev := s.snapshot.Load()
	in := Inputs{
		Now:        now,
		Commercial: active.Commercial,
		Invalid:    active.Invalid,
		NotAfter:   active.NotAfter,
		NotBefore:  active.NotBefore,
		Limits:     active.Limits,
	}
	// Carry previous counts forward so a failed fetch cannot look like released
	// capacity.
	if prev != nil {
		in.PolicyCount = prev.Inputs.PolicyCount
		in.NodeUnitCount = prev.Inputs.NodeUnitCount
		in.TrialStart = prev.Inputs.TrialStart
		in.GraceStartedAt = prev.Inputs.GraceStartedAt
	}
	stale := false

	clock, cm, clockErr := s.readOrCreateClock(ctx, in.Now)
	switch {
	case clockErr != nil:
		s.logOnce("clock", clockErr, "unable to read license state; assuming the trial is active")
		s.metrics.recordError(ctx, "clock")
		stale = true
	default:
		in.TrialStart = clock.FirstSeen
		in.GraceStartedAt = clock.GraceStartedAt
		if clock.HMACPresent && !clock.HMACValid {
			s.logOnce("hmac", errors.New("license state hmac mismatch"),
				"license state ConfigMap HMAC does not match; continuing with readable timestamps (fail-open)")
			s.metrics.recordError(ctx, "hmac")
		}
		adjusted, verdict := effectiveNow(in.Now, clock.LastSeen)
		in.Now = adjusted
		switch verdict {
		case clockRolledBack:
			s.logOnce("skew", errors.New("clock moved backwards"),
				"system clock is behind the last recorded observation; measuring the trial against the recorded time instead",
				"now", now, "lastSeen", clock.LastSeen)
		case clockStoredUnusable:
			// Implausibly far in the future; discard rather than pin the cluster to an
			// expired trial permanently.
			s.logOnce("skewstored", errors.New("recorded observation is in the future"),
				"recorded license observation is implausibly far ahead of the system clock; discarding it and re-anchoring to the current time",
				"now", in.Now, "lastSeen", clock.LastSeen)
			clock.LastSeen = time.Time{}
		case clockTrusted:
		}
	}

	if s.opts.MetadataClient != nil {
		counts, err := countPolicies(ctx, s.opts.MetadataClient)
		if err != nil {
			s.logOnce("policies", err, "unable to count Kyverno policies; retaining the previous count")
			s.metrics.recordError(ctx, "policies")
			stale = true
		} else {
			in.PolicyCount = counts.Total
			s.logger.V(4).Info("counted Kyverno policies", "total", counts.Total, "byKind", counts.ByKind)
			if len(counts.Unavailable) > 0 {
				// Default verbosity: usually a chart upgrade missing the licensing RBAC.
				s.logOnce("unavailable", errors.New("policy kinds not listable"),
					"some Kyverno policy kinds could not be listed and were counted as zero; check the licensing RBAC rule",
					"kinds", counts.Unavailable)
			}
		}
	}

	// Leader-only: the number is identical across replicas, so every replica listing
	// every node would be pure waste.
	isLeader := s.isLeader(ctx)
	if isLeader {
		units, err := countNodeUnits(ctx, s.opts.KubeClient)
		if err != nil {
			s.logOnce("nodes", err, "unable to count licensed node units; retaining the previous count")
			s.metrics.recordError(ctx, "nodes")
			stale = true
		} else {
			in.NodeUnitCount = units
		}
	}

	next := Evaluate(in)

	priorCompliantSince := clock.CompliantSince
	graceStartedAt, compliantSince := nextGraceState(
		clockState{GraceStartedAt: in.GraceStartedAt, CompliantSince: clock.CompliantSince},
		next.State, next.Inputs.Now,
	)
	next.Inputs.GraceStartedAt = graceStartedAt
	clock.CompliantSince = compliantSince

	// Anti-escalation guard: on stale input, hold State at what we already knew so an
	// API outage cannot move a cluster toward Degraded. Improvements always apply.
	//
	// Only State is pinned — counts and overrun still publish, since those calls
	// succeeded. Discarding the whole evaluation would hide a real overrun for as
	// long as the outage lasted.
	if stale && prev != nil && severity(next.State) > severity(prev.State) {
		s.logger.V(2).Info("holding license state while inputs are incomplete",
			"holdingAt", prev.State, "wouldBe", next.State)
		next.State = prev.State
		next.Reason = "inputs incomplete; holding previously reported state"
		next.Inputs.GraceStartedAt = prev.Inputs.GraceStartedAt
		// Incomplete inputs are not evidence of compliance, so do not let them
		// start or advance the clear-down clock.
		clock.CompliantSince = priorCompliantSince
	}

	s.store(&next)
	s.metrics.record(ctx, next, active)
	s.report(ctx, prev, next, active, isLeader, cm, clock)
}

// store publishes a snapshot to both this service and the package-level accessor.
func (s *service) store(snap *Snapshot) {
	s.snapshot.Store(snap)
	current.Store(snap)
}

// ensureInstruments rebuilds metric handles when the global MeterProvider has been
// replaced. pkg/metrics rebuilds it on a timer, and instruments bound to a dead
// provider record into nothing.
func (s *service) ensureInstruments() {
	provider := currentProvider()
	if s.metrics != nil && sameProvider(s.metrics.provider, provider) {
		return
	}
	s.metrics = newInstruments(provider, s.logger)
}

// readOrCreateClock reads the license-state ConfigMap, provisioning it if absent.
// Leader-only creation, so replicas cannot stamp different start times.
func (s *service) readOrCreateClock(ctx context.Context, now time.Time) (clockState, *corev1.ConfigMap, error) {
	state, cm, err := readClock(ctx, s.opts.KubeClient, s.opts.Namespace, s.opts.ConfigMapName)
	if err == nil {
		// The chart ships this ConfigMap empty, so "no firstSeen" is normal on a fresh
		// install. Stamp it here, not in the post-evaluation write: otherwise the tick
		// that establishes the trial reports zero days remaining, which reads exactly
		// like expiry at the moment the trial begins.
		if state.FirstSeen.IsZero() && s.isLeader(ctx) {
			stamped := state
			stamped.FirstSeen = now
			stamped.LastSeen = now
			if writeErr := writeClock(ctx, s.opts.KubeClient, cm, stamped); writeErr != nil {
				// Leave it to the next tick rather than failing the evaluation.
				s.logger.V(3).Info("could not stamp the trial start; will retry on the next evaluation",
					"error", writeErr.Error())
				return state, cm, nil
			}
			s.logger.V(2).Info("started license trial", "configMap", s.opts.ConfigMapName, "firstSeen", now)
			// Re-read for a current resourceVersion, or the later write conflicts.
			if fresh, freshCM, readErr := readClock(ctx, s.opts.KubeClient, s.opts.Namespace, s.opts.ConfigMapName); readErr == nil {
				return fresh, freshCM, nil
			}
			return stamped, nil, nil
		}
		return state, cm, nil
	}
	if !errors.Is(err, errClockAbsent) {
		// A parse error still returns usable state; only report a hard failure.
		if cm != nil {
			s.logOnce("clockparse", err, "license state contains an unreadable timestamp; treating the trial as active")
			return state, cm, nil
		}
		return clockState{}, nil, err
	}
	if !s.isLeader(ctx) {
		// Not leader and nothing to read: zero TrialStart reports FreeTier.
		return clockState{}, nil, nil
	}
	created, createErr := createClock(ctx, s.opts.KubeClient, s.opts.Namespace, s.opts.ConfigMapName, now)
	if createErr != nil {
		return clockState{}, nil, createErr
	}
	s.logger.V(2).Info("provisioned license state", "configMap", s.opts.ConfigMapName, "firstSeen", created.FirstSeen)
	_, cm, _ = readClock(ctx, s.opts.KubeClient, s.opts.Namespace, s.opts.ConfigMapName)
	return created, cm, nil
}

// isLeader reports whether this pod holds the admission controller's leader Lease.
// Reads the existing Lease rather than running a second election, whose startWork
// contract would not compose with a fire-and-forget Start.
//
// A snapshot, not a guarantee: two pods can both believe they are leader during
// handover, so every write path is idempotent.
func (s *service) isLeader(ctx context.Context) bool {
	lease, err := s.opts.KubeClient.CoordinationV1().
		Leases(s.opts.Namespace).Get(ctx, leaderLeaseName, metav1.GetOptions{})
	if err != nil {
		// Fail closed for writes: if leadership cannot be established, do not write.
		return false
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != config.KyvernoPodName() {
		return false
	}
	// Name matching alone is not enough: a restarted or terminating pod still finds
	// its own identity in a Lease it no longer renews.
	return leaseIsLive(lease.Spec.RenewTime, lease.Spec.LeaseDurationSeconds, s.now())
}

// leaseIsLive reports whether a Lease was renewed recently enough to still be held.
// A Lease missing either field is not live: there is no evidence it is.
func leaseIsLive(renewed *metav1.MicroTime, durationSeconds *int32, now time.Time) bool {
	if renewed == nil || durationSeconds == nil {
		return false
	}
	expiry := renewed.Add(time.Duration(*durationSeconds) * time.Second)
	return now.Before(expiry)
}

// report logs the snapshot, persists bookkeeping, and emits events on transition.
func (s *service) report(
	ctx context.Context,
	prev *Snapshot,
	next Snapshot,
	active Active,
	isLeader bool,
	cm *corev1.ConfigMap,
	clock clockState,
) {
	from := Unknown
	if prev != nil {
		from = prev.State
	}
	changed := from != next.State

	fields := []any{
		"state", next.State,
		"reason", next.Reason,
		"customer", active.Claims.Customer,
		"origin", active.Origin,
		"commercial", active.Commercial,
		"policies", next.Inputs.PolicyCount,
		"policiesAllowed", next.Inputs.Limits.Policies,
		"nodeUnits", next.Inputs.NodeUnitCount,
		"nodeUnitsAllowed", next.Inputs.Limits.NodeUnits,
		"daysRemaining", next.DaysRemaining,
	}
	switch {
	case next.State == FreeExpired || next.State == Grace || next.State == Degraded:
		if changed || s.shouldEmit("state:"+string(next.State), next.Inputs.Now) {
			s.logger.Info("Nirmata license state", fields...)
		} else {
			s.logger.V(3).Info("Nirmata license evaluated", fields...)
		}
	case changed && !next.State.IsHealthy():
		s.logger.Info("Nirmata license state changed", fields...)
	// Before the general `changed` case, which would otherwise swallow it: a
	// commercial license coming into force is the one healthy transition an
	// operator wants confirmed at default verbosity.
	case changed && next.State == Licensed:
		s.logger.Info("Nirmata commercial license active", fields...)
	case changed:
		s.logger.V(2).Info("Nirmata license state changed", fields...)
	case next.State == FreeTier && s.shouldEmit("freetier", next.Inputs.Now):
		s.logger.Info("Nirmata free tier active", fields...)
	default:
		s.logger.V(3).Info("Nirmata license evaluated", fields...)
	}
	if next.OverPolicies || next.OverNodeUnits {
		msg := "Nirmata licensed capacity exceeded"
		if next.State.EnforcesPolicyCap() {
			msg += "; new over-cap policy creates will be refused"
		} else {
			msg += "; full function preserved during grace/warning"
		}
		s.logger.Info(msg,
			"policies", next.Inputs.PolicyCount, "policiesAllowed", next.Inputs.Limits.Policies,
			"nodeUnits", next.Inputs.NodeUnitCount, "nodeUnitsAllowed", next.Inputs.Limits.NodeUnits,
			"state", next.State)
	}

	if !isLeader {
		return
	}

	if cm != nil {
		want := clock
		want.FirstSeen = clock.FirstSeen
		if want.FirstSeen.IsZero() {
			want.FirstSeen = next.Inputs.Now
		}
		want.LastSeen = next.Inputs.Now
		want.LastState = next.State
		want.GraceStartedAt = next.Inputs.GraceStartedAt
		if err := writeClock(ctx, s.opts.KubeClient, cm, want); err != nil {
			s.logger.V(3).Info("could not persist license state; will retry on the next evaluation",
				"error", err.Error())
		}
	}

	s.emit(ctx, from, next, changed)
}

// emit sends events for transitions and capacity overrun. On change only, or a
// per-interval warning would bury the event stream and trip the queue limit; a
// persistent overrun re-reports daily so it does not vanish.
func (s *service) emit(ctx context.Context, from State, next Snapshot, changed bool) {
	if s.events == nil {
		return
	}
	ref, ok := s.deploymentRef(ctx)
	if !ok {
		return
	}

	var infos []event.Info
	if changed {
		infos = append(infos, stateEvent(ref, from, next.State, next))
	}
	if next.OverPolicies && s.shouldEmit("policies", next.Inputs.Now) {
		infos = append(infos, limitEvent(ref, "policy", next.Inputs.PolicyCount, next.Inputs.Limits.Policies, next.State))
	}
	if next.OverNodeUnits && s.shouldEmit("nodeUnits", next.Inputs.Now) {
		infos = append(infos, limitEvent(ref, "node unit", next.Inputs.NodeUnitCount, next.Inputs.Limits.NodeUnits, next.State))
	}
	if len(infos) > 0 {
		s.events.Add(infos...)
	}
}

// shouldEmit rate-limits a repeating condition to once per limitReminderInterval.
func (s *service) shouldEmit(key string, now time.Time) bool {
	if last, ok := s.lastEmit[key]; ok && now.Sub(last) < limitReminderInterval {
		return false
	}
	s.lastEmit[key] = now
	return true
}

// deploymentRef resolves and caches the object events attach to, retrying on later
// ticks since the Deployment may not be visible yet during startup.
func (s *service) deploymentRef(ctx context.Context) (corev1.ObjectReference, bool) {
	if s.refValid {
		return s.ref, true
	}
	ref, err := resolveDeploymentRef(ctx, s.opts.KubeClient, s.opts.Namespace, s.opts.DeploymentName)
	if err != nil {
		s.logOnce("ref", err, "unable to resolve the deployment to attach license events to; events are suppressed")
		return corev1.ObjectReference{}, false
	}
	s.ref, s.refValid = ref, true
	return ref, true
}

// infoOnce reports an expected condition once per cause at default verbosity, then
// at high verbosity. For states an operator can see but not act on.
func (s *service) infoOnce(key string, cause string, msg string, kv ...any) {
	if prior, seen := s.loggedOnce.Load(key); seen && prior == cause {
		s.logger.V(4).Info(msg, append(kv, "detail", cause)...)
		return
	}
	s.loggedOnce.Store(key, cause)
	s.logger.V(2).Info(msg, append(kv, "detail", cause)...)
}

// logOnce reports an error once per cause, then at high verbosity. A transient
// outage would otherwise log identically on every tick.
func (s *service) logOnce(key string, err error, msg string, kv ...any) {
	cause := err.Error()
	if prior, seen := s.loggedOnce.Load(key); seen && prior == cause {
		s.logger.V(4).Info(msg, append(kv, "error", cause)...)
		return
	}
	s.loggedOnce.Store(key, cause)
	s.logger.Error(err, msg, kv...)
}

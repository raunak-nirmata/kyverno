# N4K Licensing — Phase 2

License evaluation and **graduated enforcement** for Nirmata Enterprise Kyverno.

Phase 2 keeps the Phase 1 evaluation engine and adds commercial license loading,
persisted grace bookkeeping, and a **safe** capacity gate: refuse CREATE of a new
policy that would exceed the licensed policy cap when the cluster is in
`FreeTier`, `FreeExpired`, or `Degraded`. Core workload admission
(validate / mutate / generate) is never gated. Feature-tier gating via
`pkg/toggle` is deferred.

## Principles

- **Fail-open on the hot path, fail-loud everywhere else.** License problems never
  crash the admission controller and never disable already-enforced policies.
- **Grace clock starts at first detection** of paid non-compliance and is
  persisted in the leader-managed license-state ConfigMap (`graceStartedAt`). It is
  cleared only after `graceClearAfter` of sustained compliance, so a cluster
  flapping around its node-unit entitlement cannot restart the window indefinitely.
- **Checksum, not tamper protection,** on that ConfigMap. The `hmac` key is keyed
  from the namespace name unless `N4K_LICENSE_STATE_HMAC_SEED` is set, and the
  namespace is public — anyone who can edit the ConfigMap can recompute a matching
  value. It catches partial writes and hand edits, nothing more, and a mismatch is
  logged and counted while the timestamps stay usable. Restart-safety comes from
  `writeClock` never overwriting `firstSeen`, not from this. Deleting the
  ConfigMap can still reset a trial — accepted risk.
- **No kill switch.** Licensing cannot be disabled by flag or Helm value. The
  capacity gate is also excluded from `validationWebhooks`, so listing it in the
  disable-webhook ConfigMap does not delete it — the controller reconciles it back.
  See `TestLicenseWebhookIsNotOperatorDisableable`.

## States

| State | Condition | Behavior |
|---|---|---|
| FreeTier | No commercial license, inside trial | Full function up to policy cap; CREATE at/over cap refused |
| FreeExpired | No commercial license, trial elapsed | Skips paid grace; CREATE at/over cap refused; loud warnings |
| Licensed | Valid commercial, in window, under 90% capacity | Full function |
| Warning | Valid commercial; nearing `not_after` or 90–100% of cap | Full function; warnings |
| Grace | Paid non-compliance within grace from first detection | Full function preserved |
| Degraded | Paid non-compliance past grace | CREATE at/over policy cap refused |

## Layout

| File | Purpose |
|---|---|
| `state.go` | `State`, `Inputs`, `Snapshot`, pure `Evaluate` |
| `limits.go`, `claims.go` | Caps and claim → limit mapping |
| `loader.go` | File-first commercial load, community fallback |
| `admit.go` | `AdmitPolicyCreate` safe gate, live `PolicyCounter` hook |
| `policycount.go` | Counted kinds (`policyResources`) and the periodic count |
| `clock.go` | Trial/grace ConfigMap + integrity checksum |
| `metrics.go`, `events.go` | Reporting surfaces |
| `license.go` | `Start`, loop, anti-escalation, leader detection |

## The capacity gate

`AdmitPolicyCreate` is served by its own webhook,
`kyverno-license-validating-webhook-cfg` (`pkg/webhooks/license`), on CREATE only
and with `failurePolicy: Ignore`. It is deliberately separate from the policy
validating webhook: the cap counts kinds — `mutatingpolicies`,
`namespacedmutatingpolicies`, `namespaceddeletingpolicies` — whose content Kyverno
does not validate at admission, and a counted-but-ungated kind could be created
without limit while still consuming other kinds' capacity.

The gate enforces against a **live** count from the informer caches
(`license.SetPolicyCounter`, wired in `cmd/kyverno/licensecount.go`), not the
snapshot count, which is only as fresh as the evaluation interval. When the caches
have not synced the counter reports `ok=false` and the gate falls back to the last
evaluated count rather than admitting on a zero.

Those informers live on their own factory, excluded from the startup cache-sync
barriers in `main.go` (which `os.Exit(1)` on failure). An informer for an absent
CRD never syncs, so sharing the barrier would let a missing policy CRD stop
admission from starting — licensing must never be why admission is down.

The cap is a **capacity control, not an atomic quota.** Admission reads a count the
informer cache has yet to catch up with, so `AdmitPolicyCreate` also counts the
CREATEs this process admitted but has not yet observed (`inFlight`). That closes
the ordinary `kubectl apply -f dir/` burst, where five creates at 8 of 10 would
otherwise all be admitted before the first lands. It is not a distributed
semaphore: replicas count separately, so a burst spread across replicas can still
overshoot briefly, after which the next evaluation reports the overrun and further
CREATEs are refused. Reserving capacity per request would put coordination, and a
new failure mode, on a path whose first duty is to stay open.

## Validity window

The commercial path does not call `ClaimsValidNow`: an expired license must still
reach `Evaluate` so Grace and Degraded can run, rather than being rejected at load.
`not_before` is therefore carried separately (`Active.NotBefore`) and ruled on by
`Evaluate`, which treats a window that has not opened yet as paid non-compliance —
the same ladder as expiry. Without that, a correctly signed license dated a year
out would evaluate as `Licensed` and grant full capacity today.

`licensedPolicyRules` in `pkg/controllers/webhook/controller.go` and
`policyResources` here must describe the same kinds;
`TestLicenseWebhookCoversCountedKinds` fails if they drift.

## Commercial license

Nirmata issues a signed license file. The customer mounts it via Helm:

```yaml
license:
  create: true
  value: |
    nctl-license-v1
    ...
```

or points at an existing Secret with `license.existingSecret`. The chart mounts
it into the admission controller at `/var/run/kyverno/license/license` (**optional**
volume) and passes `--licensePath`. Offline verification uses N4K’s Ed25519 trust
root in `public_key.go` (not nctl’s).

If the Secret is deleted, pods keep running: the mount stays optional, the
evaluator sees a missing file, sets `FileMissing`, and falls back to the embedded
community license → **FreeTier** or **FreeExpired** from the trial ConfigMap.
Workload admission is unaffected; only the policy-CREATE cap gate applies again.

## Where the limits come from

Free-tier limits travel in the embedded Ed25519-signed community license.
Windows (`trial_days`, `grace_days`) and caps come from claims, with package
defaults when absent (`DefaultTrialDays`, `DefaultGraceDays`, `DefaultWarnDays`).
The 90% capacity warning band is `DefaultWarnRatio`.

## Provisioning and the signing key

Unchanged from Phase 1: see the trust-root section in git history / ops docs.
Order matters: ship the public key before signing licenses against it.

## Module access

This package imports `github.com/nirmata/license-guardian/pkg/nctllicense`.
CI needs the Nirmata internal App credentials; see Phase 1 notes in the PR that
introduced the dependency.

## Testing

```bash
go test ./pkg/nirmata/license/...
```

Important contracts:

- `TestAdmitPolicyCreate` — CREATE gate matrix; UPDATE/DELETE and Unknown fail open.
- `TestCommercialOverLimitEntersGrace` / `TestFreeTierOverLimitDoesNotChangeState`
- `TestNextGraceStartedAt` / soft HMAC mismatch fail-open
- Anti-escalation and clock skew tests from Phase 1 remain in force

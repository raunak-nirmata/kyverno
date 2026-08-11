# N4K Licensing — Phase 1

License evaluation and reporting for Nirmata Enterprise Kyverno.

**Phase 1 is observability only.** Nothing here refuses an admission request,
disables a policy, or stops a controller. It observes the cluster, resolves the
limits the active license grants, computes a state, and reports it through logs,
Kubernetes events and metrics. The point of this phase is to prove the evaluation
engine is harmless in production before it is given any teeth.

## Layout

| File | Purpose |
|---|---|
| `state.go` | `State`, `Inputs`, `Snapshot`, pure `Evaluate`. Read first — this is the semantics. No I/O, no clock, no clients. |
| `limits.go`, `claims.go` | `Cap` (bounded vs unlimited), `Limits`, and the mapping from signed claims. |
| `loader.go` | Resolves the active license. Phase 1 has one source. |
| `community_license.go`, `public_key.go` | The signed free-tier license (generated) and the trust root it verifies against. |
| `policycount.go`, `nodecount.go` | Policy count across 12 kinds; node units as `ceil(memGiB / 33)`. |
| `clock.go` | Trial/grace bookkeeping in a ConfigMap. |
| `metrics.go`, `events.go` | Reporting surfaces. |
| `license.go` | `Start`, the loop, the anti-escalation guard, leader detection. |

## Where the limits come from

Free-tier limits are not hardcoded. They travel in an embedded Ed25519-signed
community license, so changing the free tier is a re-issued license rather than a
code release. `n4k-community-config.yaml` in `nirmata/license-gen` is the source of
record; the `generate-n4k-community-license` workflow signs it and opens a PR
replacing `community_license.go`.

The community license carries a `2099` sentinel expiry and **never expires**. The
per-install 15-day window is anchored cluster-side in the license-state ConfigMap,
because a signed license has exactly one date and cannot express "fifteen days from
whenever this cluster was installed".

## There is no way to turn it off

Licensing has no flag, environment variable or Helm value to disable it. A
customer-settable kill switch would be a trivial bypass the moment a later phase
enforces anything, so it does not exist rather than existing and being trusted.

The only knob is cadence, clamped at both ends so it cannot be set long enough to
switch reporting off in practice:

```yaml
features:
  licensing:
    evaluationInterval: 5m   # optional; clamped to 1m..1h, defaults to 15m
```

Consequences:

- **There is no emergency lever.** If the evaluator misbehaves in a customer
  cluster, support cannot disable it without a patch release or chart rollback.
  Mitigated by construction, not configuration: the loop refuses nothing, recovers
  from panics at two levels, and every failure path is fail-open.
- **Node-read RBAC is granted to every install**, since node units come from node
  memory. Cluster-wide read of nodes only, in its own aggregated ClusterRole
  (`...:licensing`) so it is easy to audit.
- `Start` is a no-op for the non-authoritative components; only the admission
  controller evaluates.

## Provisioning and the signing key

N4K verifies against **its own** Ed25519 trust root, not nctl's, so the two products
cannot forge or invalidate each other's licenses and rotate independently. The
private half exists only as the `N4K_LICENSE_PRIVATE_KEY` secret in
`nirmata/license-gen`; the public half in `public_key.go` is not secret and ships in
every binary, which is what makes offline verification work.

Two rules that do not change:

- **Order.** The public key must be committed and shipped *before* the first license
  is signed with its private counterpart. A license signed against a key the binary
  does not carry fails verification, and reports as a *corrupt* artifact rather than
  a missing one — a confusing symptom for a simple cause.
- **Rotation invalidates every issued license.** Replacing the public key means
  re-issuing to every customer, and the new key must reach clusters via an upgrade
  *before* the new licenses do. Whoever holds the private half needs an offline
  backup: losing it means every shipped binary trusts a key nobody can sign with.

To generate a pair (somewhere the private half will not persist):

```bash
openssl genpkey -algorithm ed25519 -out n4k-license-private.pem
openssl pkey -in n4k-license-private.pem -pubout -out n4k-license-public.pem
gh secret set N4K_LICENSE_PRIVATE_KEY --repo nirmata/license-gen < n4k-license-private.pem
```

Then commit the public half (with PEM armour) and shred the private file.

## Unprovisioned versus corrupt

While `EmbeddedN4KCommunityLicense` is empty, `LoadActive` returns
`errCommunityLicenseMissing` and the evaluator reports nothing. It deliberately does
**not** fall back to hardcoded limits — a build missing its artifact should be
visible, not quietly indistinguishable from a provisioned one.

| Situation | Reported as |
|---|---|
| Constant empty — no artifact in this build | `V(2)` info, once per process, plus `..._evaluation_errors{stage="unprovisioned"}`. Expected while the pipeline is stood up; nothing an operator can act on. |
| Constant present but fails to verify | `Error`, once per cause, plus `..._evaluation_errors{stage="load"}`. A real defect. |

For local testing, `hack/embed-n4k-license.sh` embeds a locally signed license, and
`--restore` puts the placeholder back. Not a bypass — the license must still verify
against `public_key.go`. Always restore before committing: a dev-signed license on a
release branch would define the free tier with a key Nirmata does not control.

## Module access

This package imports the private module
`github.com/nirmata/license-guardian/pkg/nctllicense`, so CI needs a credential;
`secrets.GITHUB_TOKEN` is repository-scoped and insufficient.

`.github/actions/go/install` takes `nirmata-app-id`/`nirmata-app-private-key` inputs
and mints a short-lived token scoped to `license-guardian` (the `nirmata-internal`
App already has read access). `go/setup` threads them through, and callers pass
`secrets.NIRMATA_INTERNAL_APP_ID`/`_PRIVATE_KEY`. `reuse.yaml` does the same for the
FIPS image builds, exporting `NIRMATA_MODULE_TOKEN` so the Makefile can forward it to
BuildKit as a build secret — never a build arg, so it is not recoverable from a
published layer.

Secrets are unavailable to `pull_request` runs from a fork, which would therefore
fail at `go mod download`.

For local work against an unreleased SDK change, use a Go workspace (`go work init
. && go work use ../license-guardian`). `go.work` is gitignored and must never be
committed — an absolute local path breaks every CI job and container build.

## Clock handling, and what it does not promise

The trial is wall-clock based, anchored on `firstSeen`, so it is sensitive to a wrong
system clock. Two guards, both fail-open:

- **Backward skew** beyond `skewTolerance` measures against the stored `lastSeen`
  instead of the current clock. This *freezes* the trial where it had reached rather
  than expiring it: `lastSeen` only ever advances from the clamped value, so the two
  form a fixed point. The guard stops a rollback handing trial time back; it is not a
  claim that a trial cannot be extended.
- **A `lastSeen` implausibly far in the future** (beyond `forwardTrustHorizon`) is
  discarded and re-anchored. Without this the guards form a trap: a pod starting with
  a badly wrong future clock — resumed VM snapshot, dead RTC, container running before
  NTP settles — writes that time on its first tick, and every later tick clamps
  forward to it, reporting `FreeExpired` permanently.

That healing costs the ability to tell a very large deliberate rollback from a
corrupt stored value, so such a rollback restarts the trial. Intentional: fail-open
is a requirement and resisting tampering is not, so a misprovisioned cluster
recovering matters more than an operator being unable to game a capped free tier.

The ConfigMap is plain text and unsigned, so deleting it grants a fresh trial. An
accepted risk — the 10-policy cap is the real control, and signing would mean
shipping a key and owning its rotation.

## Testing

```bash
go test ./pkg/nirmata/license/...
```

The state machine is exhaustively table-tested, including a totality test. Everything
touching Kubernetes uses fake clientsets. Time is always injected — nothing calls
`time.Now()` outside `Options.clock()`.

Tests that encode decisions rather than mechanics:

- `TestOverLimitIsReportedButDoesNotChangeState` — the Phase 1 contract. If this
  fails, someone has given Phase 1 teeth by accident.
- `TestEventsReachTheDeployment` — `pkg/event` silently drops events with an empty
  `Regarding.Name`, events regarding policy objects, and `PolicyApplied`.
- `TestWriteClockNeverOverwritesFirstSeen` — rewriting it restarts a customer's trial.
- `TestTickWithholdsEscalationOnStaleInputs` and
  `TestWithheldEscalationStillReportsFreshCounts` — a failed call must not move a
  cluster toward Degraded, nor freeze the signals that did succeed.
- `TestTickRecoversFromPoisonedFutureClock` — the trap described above.
- `TestCommercialWithoutExpiryIsLicensedNotDegraded` — the one place the state
  machine could fail *closed*.
- `TestNodeUnits` — carries license-guardian's own expected values, so the two
  implementations cannot silently diverge.

## Notes for whoever extends this

- **Metrics use synchronous gauges, not observable gauges with callbacks.**
  `pkg/metrics` rebuilds the global `MeterProvider` on a timer when
  `metricsRefreshInterval` is set, re-registering only its own built-ins, so anything
  bound to a captured meter silently stops recording. `ensureInstruments` compares
  provider identity every tick for this reason.
- **Leadership is read from the existing `kyverno` Lease**, not a second election,
  and `RenewTime`/`LeaseDurationSeconds` are checked rather than just the holder name
  — a restarted or terminating pod still finds its own identity in a Lease it no
  longer renews. Even so it is a snapshot: two pods can briefly both believe they are
  leader, so every write path is idempotent.
- **The anti-escalation guard pins `State` only.** Counts, overrun and the trial
  anchor from a partially failed tick still publish, since those calls succeeded.
  Discarding the whole evaluation would let one failing input hide a real overrun for
  the duration of an unrelated outage.
- **`LoadActiveClaims` in the SDK is unsuitable for diagnostics.** It degrades to the
  community license on every failure mode and its sentinel errors are unexported, so
  it cannot report *why* a license was rejected. Phase 2's file loader should call
  `ParseAndVerify` and `ClaimsValidNow` directly.

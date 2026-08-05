# N4K Licensing — Architecture & Integration Design

**Status:** Draft
**Owner:** Ritesh Patel
**Last updated:** 2026-07-20
**Companion to:** [`licensing-requirements.md`](./licensing-requirements.md) (expands §7 "Architecture — sync-safe integration")
**Target release:** Enterprise Kyverno 1.18.x

---

## 1. Purpose & scope

This document is the engineering design for **how** N4K enforces licensing while remaining trivial to keep in
sync with upstream `kyverno/kyverno`. It expands §7 of the requirements doc into concrete packages, files, hook
points, and build wiring, grounded in the current codebase.

It covers: package layout, the startup hook, build-time gating, the license evaluation loop, feature-tier and
policy-cap enforcement, the state machine, observability, and — critically — a **merge-conflict budget** and
sync playbook. It does **not** re-argue product decisions (metric, grace, delivery) settled in the requirements
doc.

### The governing constraint

`main` = upstream + a thin Nirmata overlay, resynced from `kyverno/kyverno@main` **daily** via
`.github/workflows/sync-main-oss.yml` (`main-oss` mirror → auto-PR to `main`). **Merge cost is paid every day.**
Therefore the design is optimized to minimize one number:

> **The count of lines changed inside upstream-owned files on `main`.**

New files in Nirmata-owned paths cost **zero** merge risk (upstream never creates them). Lines edited inside
files that also exist upstream cost merge risk proportional to how often upstream touches those lines. The whole
design is a campaign to push all logic into new files and leave a **single, stable, rarely-adjacent line** in
one upstream file.

---

## 2. Design targets

| # | Target | Mechanism |
|---|--------|-----------|
| A | All license logic in Nirmata-owned packages | New `pkg/nirmata/license/` tree (§3) |
| B | Exactly **one** line touched in an upstream file | Single call in `cmd/internal/setup.go` (§4) |
| C | OSS mirror (`main-oss`) builds & runs with the gate **absent** | Build-tag stub/impl pair (§4, §5) |
| D | No new conditionals inside engine/webhook/controller files | Reuse `pkg/toggle`; new logic in own webhook (§6, §7) |
| E | Fail-open: license faults never crash or block admission | Background goroutine, panic-recover, no hot-path calls (§8) |

---

## 3. Package layout (all new, zero merge risk)

```
pkg/nirmata/license/                 # Nirmata-owned; upstream will never create this path
  gate.go            # Gate struct: orchestrates load → evaluate → enforce → report
  state.go           # State machine (FreeTier|Licensed|Warning|Grace|Degraded) + transitions
  loader.go          # Resolve + read license (file now; dial-home later) → nctllicense.Claims
  evaluate.go        # Compare claims vs cluster facts (node units, policy count, time)
  entitlements.go    # Feature-string → toggle mapping; applies entitlements
  policycap.go       # Policy-count cap: counter + enforcement decision
  nodecount.go       # Thin adapter over license-guardian node-unit counting
  graceclock.go      # Persist/read first-non-compliance timestamp (ConfigMap-backed)
  metrics.go         # Prometheus collectors (n4k_license_*)
  events.go          # Kubernetes event emission
  config.go          # License flags/env (path, dial-home) — Nirmata-owned config struct
  dialhome/          # Phase 3 only; interface + no-op until backend exists
    client.go

cmd/internal/
  license_hook_n4k.go    # //go:build n4k    → real startLicense(...) calling pkg/nirmata/license
  license_hook_stub.go   # //go:build !n4k   → startLicense(...) no-op

pkg/nirmata/license/webhook/    # Phase 2+; only if policy-cap uses admission (see §7.3)
  handler.go
```

- Dependency added to `go.mod`: `github.com/nirmata/license-guardian` (reuses `pkg/nctllicense`, node counting).
  `go.mod`/`go.sum` conflicts are mechanical and rare vs. logic-file conflicts.
- Everything under `pkg/nirmata/` and the two `license_hook*.go` files are **new files**. The only upstream file
  that changes is `cmd/internal/setup.go` (§4).

---

## 4. The single hook point

All five controller binaries route through one function (verified call sites):

```
cmd/kyverno/main.go:478                internal.Setup(appConfig, "kyverno-admission-controller", false)
cmd/background-controller/main.go:183  internal.Setup(appConfig, "kyverno-background-controller", false)
cmd/reports-controller/main.go:357     internal.Setup(appConfig, "kyverno-reports-controller", skipResourceFilters)
cmd/cleanup-controller/main.go:146     internal.Setup(appConfig, "kyverno-cleanup-controller", false)
cmd/kyverno-init/main.go:50            internal.Setup(appConfig, "kyverno-init-controller", false)
```

So the hook lives inside **`cmd/internal/setup.go` → `Setup()`**, which already builds the logger, kube client,
and config we need. We add **one line**, immediately before `Setup()` returns:

```go
// setup.go — the ONE upstream line we own on `main`
startLicense(ctx, logger, client, configuration, name)
```

`startLicense` is **not** defined in `setup.go`. It is defined in a build-tag pair so the same one line compiles
to either a no-op (OSS) or the real gate (N4K):

```go
// cmd/internal/license_hook_stub.go
//go:build !n4k

package internal

// startLicense is a no-op in open-source builds. The N4K build tag swaps in the real implementation.
func startLicense(ctx context.Context, logger logr.Logger, client kubeclient.UpstreamInterface,
    cfg config.Configuration, component string) {
}
```

```go
// cmd/internal/license_hook_n4k.go
//go:build n4k

package internal

import "github.com/kyverno/kyverno/pkg/nirmata/license"

func startLicense(ctx context.Context, logger logr.Logger, client kubeclient.UpstreamInterface,
    cfg config.Configuration, component string) {
    // Only the admission controller runs the authoritative gate + enforcement;
    // other components run a read-only status reporter. Component name decides.
    license.Start(ctx, logger, client, cfg, component)
}
```

**Why this shape:**
- The single line in `setup.go` is present in **every** build — OSS included — so `main` and `main-oss` differ
  by exactly that one line. It sits just before `return`, the least-churned region of the function.
- The `//go:build n4k` files are new files → **zero** conflict risk, ever.
- OSS builds (no `n4k` tag) compile the stub → the linker never even pulls in `pkg/nirmata/license` or
  `license-guardian`. `main-oss` stays a genuine clean mirror at the binary level.

> **Zero-touch alternative (resolved — D11):** we could avoid even the one line by giving
> each cmd `main.go` a blank import of a Nirmata package with an `init()` that registers a hook, then have a
> Nirmata-owned wrapper invoke it. That spreads edits across five upstream `main.go` files (worse), or requires
> a callable run-loop upstream doesn't expose. **Recommendation: accept the single `setup.go` line.** One stable
> line beats five, and beats fighting the framework.

---

## 5. Build-time gating (matches existing machinery)

The build system **already** threads a build-tag variable and ldflags through every image target:

```makefile
BUILD_TAGS := "fips"                                   # Makefile:100 (today)
LD_FLAGS   := "-s -w -X $(PACKAGE)/pkg/version.BuildVersion=$(VERSION)"   # Makefile:201
# builds pass: -tags "$(BUILD_TAGS)"  and  --build-arg BUILD_TAGS=$(BUILD_TAGS)
```

N4K release images simply add the tag:

```makefile
BUILD_TAGS := "fips n4k"          # N4K release builds only
```

- OSS / `main-oss` CI builds **without** `n4k` → stub → inert.
- N4K release CI (goreleaser / ko / docker) builds **with** `n4k` → real gate.
- The Makefile/goreleaser/ko config are Nirmata-overlay files already (they carry Nirmata release wiring), so
  toggling the tag there is not an upstream-file edit in the sensitive sense.

**ldflags use:** inject the license-public-key fingerprint and a build channel marker the same way `BuildVersion`
is injected, e.g. `-X .../pkg/nirmata/license.buildChannel=n4k-release`. Belt-and-suspenders with the build tag;
also lets us fail closed if someone builds with the tag but wrong key.

**Decision (D4):** build-tag is primary (compile-time exclusion, cleanest `main-oss` story);
ldflags is a secondary marker. Runtime-only gating is rejected — it would ship enforcement code in the OSS binary.

---

## 6. License evaluation loop

`license.Start()` spawns a single background goroutine (never blocks `Setup()`), mirroring how
`license-guardian` already runs perpetual reporting. Panic-recover wraps the loop so a bug can never take down
the controller (Target E).

```
Start(ctx, ...):
  go run():
    recover-guard {
      evaluateOnce()                      # immediate, at startup
      ticker := every refreshInterval (default 15m)
      for { select ctx.Done -> return; ticker -> evaluateOnce() } }

evaluateOnce():
  claims, source := loader.Load()          # file | dial-home | none
  facts          := gather()               # node units (leader only), policy count, now()
  newState       := state.Evaluate(claims, facts, graceClock)
  entitlements.Apply(newState, claims)     # flip toggles / policy cap (§7)
  metrics.Set(newState, facts, claims); events.Emit(transitions)
  if dialHomeEnabled { dialhome.Report(status) }   # Phase 3, best-effort
```

- **Only the admission controller** (`component == "kyverno-admission-controller"`) is the authoritative
  evaluator and the only one that flips enforcement. Other components run a **read-only reporter** (logs/metrics
  only) so we don't have four controllers fighting over toggles. (Requirements R7.)
- Node counting runs under the existing leader election (D6) to avoid every replica listing nodes.

---

## 7. Enforcement mechanisms

### 7.1 Feature tiers via `pkg/toggle` — **DEFERRED (post-v1, D1)**

> **Not built for 1.18.x.** v1 is capacity-only (D1); the only enforcement mechanisms that ship are §7.2
> (node/time) and §7.3 (policy cap). The design below is the intended approach for the later feature-tier phase
> and is documented now so the mechanism is agreed.

`pkg/toggle` exposes package-level `Toggle` vars whose value can be forced at runtime:

```go
func (t *toggle) Parse(in string) error   // sets t.value
func (t *toggle) Enabled() bool           // read at upstream call sites
```

For any enterprise feature that **already** has a toggle (e.g. `GenerateValidatingAdmissionPolicy`,
`GenerateMutatingAdmissionPolicy`, `AutogenV2`), entitlement enforcement is simply:

```go
// entitlements.go — Degraded state, feature not entitled
_ = toggle.GenerateMutatingAdmissionPolicy.Parse("false")
```

Upstream code keeps reading `.Enabled()` exactly as today — **no new conditionals in engine/webhook files**
(Target D). This is the whole reason to gate through toggles rather than sprinkling `if licensed`.

**For features that lack a toggle:** add the toggle **upstream-style** (contribute the pattern into
`pkg/toggle`, ideally upstreamable), rather than a Nirmata-only `if`. A toggle addition is a localized, low-churn
diff in one file that reads like upstream code — far cheaper than scattered conditionals. Which features need
new toggles is a later-phase task tied to the final feature catalog (D2).

### 7.2 Time & node-unit entitlement

Pure comparison in `evaluate.go`; no upstream surface. Over-entitlement feeds the state machine (§8), it does
not block anything inline.

### 7.3 Policy-count cap (the genuinely new enforcement)

No upstream equivalent exists, so this lives entirely in Nirmata code. Two enforcement points, both safe
(refusing a Kyverno policy never blocks a workload):

1. **Timer count (always):** `policycap.go` lists policy CRs on the refresh tick, compares to the effective cap
   (no-license default **10**; `max_policies`; omitted ⇒ unlimited), and feeds state + metrics
   (`n4k_license_policies_used` / `_allowed`). Over-cap → Warning → Grace → Degraded.
2. **Reject-on-create (Phase 2, D7):** a **Nirmata-owned validating webhook** on Kyverno policy kinds that
   rejects creation of a policy beyond the cap in Degraded state. This is a *new webhook registered from Nirmata
   code + chart* — it does **not** edit upstream policy-validation logic. Existing over-cap policies are never
   retroactively disabled; only new ones are refused once degraded.

**Resolved (D7):** the cap counts **all Kyverno policy kinds** — `ClusterPolicy`, `Policy`, and CEL-based
`ValidatingPolicy`/`MutatingPolicy`/`GeneratingPolicy`/`ImageValidatingPolicy`/… . Phase 1 = timer-only;
Phase 2 = timer + reject-on-create webhook.

---

## 8. State machine & grace clock

States and transitions are the requirements §6 table, implemented in `state.go`. Key implementation points:

- **Two independent windows:** the 15-day free-tier window (net-new, no license) and the 30-day paid-grace
  window (a valid license went bad). Existing-customer migration uses the longer 60–90 day window
  (requirements §13). All three are config constants, not hardcoded.
- **Grace/free clock must survive restarts** (requirements R6). `graceclock.go` persists the first-detection
  timestamp in a **leader-managed ConfigMap** in the kyverno namespace (not pod-local disk, not memory — matches
  the HA "shared state" discipline). The stored value is **HMAC-signed** with a build-injected key to resist
  casual editing (anti-tamper is best-effort, not DRM — requirements §3/§9).
- **Fail-open everywhere:** any error in load/evaluate/enforce logs loudly and leaves the *previous* state in
  place; it never escalates to Degraded on the strength of an internal error, and never returns an error that
  could bubble into the controller's health.

---

## 9. Observability

- **Metrics** (`metrics.go`, registered with the existing Prometheus registry): `n4k_license_valid`,
  `n4k_license_state{state}`, `n4k_license_expiry_seconds`, `n4k_license_node_units_{used,allowed}`,
  `n4k_license_policies_{used,allowed}`, `n4k_license_days_remaining`.
- **Events** (`events.go`): `Normal`/`Warning` Kubernetes events on the kyverno Deployment (or a dedicated
  license ConfigMap) on every state transition, so `kubectl get events` tells the story.
- **Logs** (logr, per AGENTS.md levels): L2 startup summary; L0/warnings for grace/degraded.
- All three are driven off the same `evaluateOnce()` result — single source of truth, no drift between surfaces.

---

## 10. Dial-home (Phase 3, interface-only now)

`dialhome/client.go` defines the interface and a no-op impl so Phases 1–2 compile and run without a backend.
When the central service exists: retrieve license by customer/tenant ID (requirements §11 Phase 3), cache the
last-good license locally (shared store, not pod disk), report status/usage, and honor **soft revocation**
(backend marks `license_id` revoked → enter Grace). Dial-home failure alone **never** degrades the product; the
cached/offline license stays authoritative. Identity = tenant ID (D9); the wire contract (endpoint, auth,
payload, revocation) remains OPEN (requirements §12).

---

## 11. Merge-conflict budget & sync playbook

This is the deliverable that makes the whole thing maintainable.

### 11.1 File inventory & conflict risk

| Path | Kind | On `main-oss`? | Merge-conflict risk |
|------|------|----------------|---------------------|
| `pkg/nirmata/license/**` | New Nirmata files | No | **None** (upstream never creates this path) |
| `cmd/internal/license_hook_stub.go` | New, `//go:build !n4k` | No | **None** (new file) |
| `cmd/internal/license_hook_n4k.go` | New, `//go:build n4k` | No | **None** (new file) |
| `pkg/nirmata/license/webhook/**` | New (Phase 2, optional) | No | **None** (new files) |
| `cmd/internal/setup.go` | **1 line added** | Yes (clean) | **Low** — single line before `return`; conflicts only if upstream rewrites the tail of `Setup()` |
| `go.mod` / `go.sum` | Dep added | Yes | **Low/mechanical** — resolved by `go mod tidy` |
| `Makefile` (BUILD_TAGS), goreleaser, ko, Helm chart | Nirmata overlay | Overlay files | **Low** — already Nirmata-managed surfaces |
| `pkg/toggle/*` (new toggles for un-toggled features) | Small upstream-style diffs | Yes | **Low** — localized, upstreamable-shaped |

**Total sensitive footprint on `main`: one line in `setup.go`** (+ mechanical go.mod + any new toggles). That is
the number we defend in review.

### 11.2 When the daily sync PR conflicts

1. Conflicts should occur **only** in `setup.go` (rare) or `go.mod` (mechanical). If a conflict appears anywhere
   under `pkg/nirmata/` something is wrong — investigate, don't blindly resolve.
2. `setup.go` conflict → re-apply the single `startLicense(...)` line at the new end-of-function location.
3. `go.mod`/`go.sum` → `go mod tidy`.
4. A new-toggle conflict → re-apply the small toggle diff.
5. Add a CI check on `main` that greps for the `startLicense(` call in `setup.go` and fails the build if it's
   missing — so a bad conflict resolution that drops the hook is caught immediately, not in production.

### 11.3 Guardrails

- **`main-oss` is never edited by hand** for licensing. If licensing code ever appears on `main-oss`, the mirror
  is polluted — revert.
- A CI job builds the **OSS profile** (no `n4k` tag) on every PR to prove the product still compiles and runs
  with the gate inert (Target C, requirements R3).

---

## 12. Testing strategy

- **Unit:** state-machine transitions (all window boundaries), `evaluate.go` (expired / over-units / over-policy
  / missing / valid), node-unit math parity with `license-guardian`, grace-clock persistence + HMAC tamper
  rejection, entitlement → toggle mapping.
- **Build-profile tests:** compile & run with and without `-tags n4k`; assert stub is a no-op and the OSS binary
  never links `license-guardian`.
- **Conformance (chainsaw):** free-tier expiry degrades new-policy creation but not workload admission; valid
  license unlocks; grace behavior; policy-cap reject-on-create (if shipped).
- **Fail-open fault injection:** corrupt license, unreadable path, panic in evaluate — assert the controller
  keeps admitting workloads and stays healthy.
- **HA:** two admission replicas — assert single leader counts nodes and owns the grace ConfigMap.

---

## 13. Phasing (implementation, maps to requirements §11)

| Phase | Build/ship |
|-------|-----------|
| **0 — Prereq** | `license-guardian` `Claims` extension; `pkg/nirmata/license` skeleton; hook + build-tag pair; OSS-profile CI. |
| **1 — Free tier** | Full state machine, policy-cap timer count, node counting for telemetry, all observability. No license needed. |
| **2 — BYOL** | File loader, offline validation, entitlement→toggle mapping, grace clock, optional reject-on-create webhook, Helm license Secret. |
| **3 — Dial home** | `dialhome` client, cached license in shared store, usage reporting, soft revocation. |

---

## 14. Engineering decisions

Resolved 2026-07-20 (cross-referenced to requirements §12 `D#`).

| # | Question | Resolution |
|---|----------|------------|
| E1 | Zero-touch vs one-line `setup.go` | **One line** in `Setup()` via the build-tag hook pair. (D11) |
| E2 | Which un-toggled features need new `pkg/toggle` entries | **None in v1** — feature-tier gating is deferred (D1). Revisit when tiers land; then add upstream-style toggles per the final catalog (D2). |
| E3 | Policy-cap CRD kinds & reject-on-create timing | Count **all Kyverno policy kinds** (`ClusterPolicy`, `Policy`, `ValidatingPolicy`, `MutatingPolicy`, `GeneratingPolicy`, `ImageValidatingPolicy`, …). **Phase 1** timer count; **Phase 2** Nirmata-owned reject-on-create webhook. (D7) |
| E4 | Grace-clock ConfigMap ownership under HA | **Leader owns** the ConfigMap (write); non-leader components read-only. Aligns with node-count leader (D6). |
| E5 | HMAC key for grace/migration marker | **Reuse the build-injected key** (ldflags, same as the build-channel marker). Rotating it invalidates stored markers → treated conservatively as "first detection now" (fail-loud, safe). Flagged OPEN in requirements §12 for sign-off. (D5) |
| E6 | CLI (`kubectl-kyverno`) scope | **Not licensed** — offline evaluation tool, not the enforcement surface. (D13) |

**Still OPEN:** the **dial-home wire contract** — the protocol covering both license *retrieval* and status/usage
*reporting* (endpoint/auth/payload/revocation) for Phase 3, cross-team with backend/NCH. Tenant-ID identity is
settled (D9); the protocol is not. See `dialhome/` (§10).

---

## Appendix — Verified code anchors

- `internal.Setup()` — `cmd/internal/setup.go`; single shared startup for all 5 binaries (call sites listed §4).
- `pkg/toggle` — `Toggle{Parse(string) error; Enabled() bool}`; package-level vars + `Toggles` interface in
  `context.go`. Forcing a value via `.Parse("false")` is the entitlement lever.
- Build machinery — `Makefile:100 BUILD_TAGS := "fips"`, threaded via `-tags "$(BUILD_TAGS)"` and
  `--build-arg BUILD_TAGS`; `Makefile:201 LD_FLAGS ... -X .../pkg/version.BuildVersion=$(VERSION)`.
- Upstream sync — `.github/workflows/sync-main-oss.yml` (daily `kyverno/kyverno@main` → `main-oss` → PR to `main`).
- SDK — `github.com/nirmata/license-guardian`: `pkg/nctllicense` (Ed25519 verify, `Claims`,
  `ValidateFileForFeature`), node-unit counting `ceil(mem_GiB/33)`.

# Nirmata Enterprise Kyverno (N4K) — Licensing Requirements

**Status:** Draft
**Owner:** Ritesh Patel
**Last updated:** 2026-07-20
**Related repos:** `nirmata/enterprise-kyverno` (this repo), `nirmata/license-guardian` (licensing SDK)

---

## 1. Background & problem statement

`nirmata/enterprise-kyverno` is **Nirmata's commercial distribution of Kyverno (N4K)**. It is a downstream
fork of the upstream open-source project `kyverno/kyverno`:

- The Go module path is still `github.com/kyverno/kyverno` (unchanged from upstream).
- Upstream `kyverno/kyverno@main` is pulled daily into a `main-oss` branch and auto-PR'd into `main` via
  `.github/workflows/sync-main-oss.yml`. **`main` = upstream + a thin Nirmata overlay.**
- Today the only Nirmata-specific overlay is licensing *text*: the Apache `LICENSE` file was removed and the
  README/chart NOTES now reference the [Nirmata Enterprise Software License Agreement](https://nirmata.com/eula/)
  (commit #664). There is **no runtime license enforcement of any kind.**

**Problem:** N4K is distributed as a proprietary, EULA-covered product but ships with zero technical controls.
Anyone who obtains the images can run it unlicensed and indefinitely. We need to gate the software on a valid
Nirmata license to prevent misuse — **without** making the daily upstream sync painful to maintain.

### The central tension

Every line we change *inside an upstream-owned file* is a potential merge conflict on **every** sync (daily).
Naive licensing (sprinkling `checkLicense()` calls through `pkg/engine`, webhook handlers, controllers) would
create permanent, recurring merge friction and slow our ability to track upstream security fixes — the primary
reason customers pay for N4K. **Sync-safety is a first-class requirement, co-equal with enforcement.**

---

## 2. Goals

1. **Gate N4K on a valid Nirmata license** so unlicensed/expired/out-of-entitlement use is detectable and,
   after a grace period, functionally degraded.
2. **Minimize upstream-sync friction** — license code lives in Nirmata-owned files/packages with the smallest
   possible number of touch-points inside upstream files (target: **one** hook, in the shared startup path).
3. **Meter on capacity** — licensed node units + a policy-count cap. The claims schema also carries a `features`
   set so per-feature entitlements can be added later without re-plumbing; **feature-tier gating is out of scope
   for v1 (1.18.x) — see D1.**
4. **Graduated enforcement** — warn during grace, degrade gracefully after; **never** hard-crash an admission
   controller sitting in the cluster's critical path.
5. **Reuse `license-guardian`** as the licensing SDK rather than reinventing signing/validation/metering.
6. **Support disconnected & connected deployments** — offline signed license file *and* optional phone-home to
   the Nirmata backend for real-time visibility.
7. **Give customers and Nirmata clear visibility** into license status via logs, Kubernetes events, Prometheus
   metrics, and (optionally) backend reporting.

## 3. Non-goals

- **Not** a DRM / anti-tamper / obfuscation system. A determined bad actor with source access can strip the
  gate; the goal is to deter *casual* and *accidental* misuse and to give Nirmata contractual + telemetry
  leverage, consistent with the EULA. (See §11 Security.)
- **Not** metering the open-source `kyverno/kyverno` — enforcement must be inert unless the N4K build/overlay is
  present, so `main-oss` stays a clean mirror.
- **Not** a per-request / per-admission hot-path check. Licensing is evaluated at startup + on a periodic timer,
  never inline in admission handling (correctness and latency risk).
- **Not** changing how policies are authored or evaluated.
- **AWS Marketplace metering is out of scope for v1** but the design must not preclude it (license-guardian
  already implements it for EKS; see §5.3).

---

## 4. What we license on

### 4.1 Capacity: Licensed Node Units

Reuse the node-unit metric already implemented in `license-guardian`:

```
Licensed Units (per node) = ceil(node_memory_GiB / 33)      # 32 GiB/unit + 1 GiB buffer
Cluster Units             = Σ Licensed Units over all Ready nodes with reported memory
```

| Node memory | Units |
|-------------|-------|
| ≤ 33 GiB    | 1     |
| 34–66 GiB   | 2     |
| 67–99 GiB   | 3     |

- The license carries a **maximum entitled node-unit count** (`max_node_units` claim).
- N4K counts current cluster units and compares against entitlement on a periodic timer.
- Nodes without reported memory capacity are skipped (matches SDK behavior).
- **Counting under HA (D6):** the **admission controller under leader election** is the single counter; other
  controllers/replicas never count nodes independently.

### 4.2 Capacity: Policy count (`max_policies`)

A second capacity dimension caps the number of Kyverno policies N4K will enforce. This doubles as the
**built-in free/PoC tier** — the product is usable out of the box for evaluation without any license, but both
scale-limited (policy count) **and time-limited (15 days)**, i.e. too small and too short for production.

| Situation | Policy cap |
|-----------|-----------|
| **No license present (free tier)** | **10 policies**, usable for **15 days** (see §6). Enough to test / PoC. |
| **License present, `max_policies` omitted** | **Unlimited.** |
| **License present, `max_policies = N`** | **N policies.** |

- The default (10 policies) and the free-tier window (15 days) are config constants, not hardcoded in a hot
  path, and apply whenever no valid license is loaded.
- Omitting `max_policies` in the license is the common enterprise case → unlimited. This keeps most licenses
  simple and lets us sell a flat "unlimited policies" entitlement without touching the number.
- **What counts as "a policy" (D7):** **all Kyverno policy kinds** — `ClusterPolicy`, `Policy`, and the
  CEL-based `ValidatingPolicy`, `MutatingPolicy`, `GeneratingPolicy`, `ImageValidatingPolicy`, etc. Counting
  every policy CR is consistent and hard to game (a customer can't sidestep the free-tier 10 by using CEL types).
- **Enforcement point (D7):** **Phase 1** — the counter runs on the periodic timer (consistent with node units)
  and feeds the graduated model in §6. **Phase 2** — additionally reject *creation* of a policy beyond the cap
  via a **Nirmata-owned validating webhook** (a safe gate: it refuses a new Kyverno policy, never a workload).
  Existing over-cap policies are never silently disabled without passing through Warning → Grace first.

### 4.3 Entitlements: Feature Tiers — **DEFERRED (post-v1)**

> **Not in 1.18.x.** v1 is **capacity-only** (node units + policy cap); see decision **D1**. The `features`
> claim is still carried in the schema and validated, but it does **not** gate behavior at launch. Feature-tier
> gating (and the SKU catalog, **D2**) ships in a later minor. The sketch below is retained for that future work.

When feature tiers land, the license's **feature set** (`features` claim) will gate optional/enterprise
capabilities, checked once at startup (and on refresh) and applied via the existing `pkg/toggle` mechanism so we
don't invent a parallel system.

Draft tier → feature mapping (future, to be finalized with product/sales):

| Tier          | Example entitlements (`features`)                                               |
|---------------|---------------------------------------------------------------------------------|
| **Base**      | Core admission (validate/mutate/generate), reporting                            |
| **Advanced**  | + Image verification (cosign/notary), CEL policies, cleanup policies            |
| **Enterprise**| + Priority support entitlement, extended retention, air-gap, SLA features       |

> Feature strings should be namespaced (e.g. `n4k.image-verify`, `n4k.cel`, `n4k.cleanup`) to coexist with the
> existing SDK features (`nctl.ai`, `nctl.agent`) in one signing authority.

### 4.4 Time-boxing

Every license carries `not_before` / `not_after` (RFC3339 UTC). Enforcement keys off `not_after` for the grace
window (§6).

---

## 5. License delivery & validation

Two delivery paths, both backed by `license-guardian`. **v1 ships offline-file as the source of truth;
phone-home is additive visibility, never a hard runtime dependency.**

### 5.1 Offline signed license file (nctl) — primary

- Nirmata issues an Ed25519-signed **nctl license file** (`license-guardian/pkg/nctllicense`), delivered to the
  customer and mounted into N4K pods via a Kubernetes `Secret` (preferred) or `ConfigMap`, e.g. at
  `/etc/nirmata/license`. Path configurable via flag/env.
- N4K verifies the signature **offline** against the public key embedded in the SDK
  (`pkg/nctllicense/embedded_public_key.go`) — no network needed. Air-gap friendly.
- Validation = signature valid **AND** `not_before ≤ now ≤ not_after` **AND** requested feature present
  (`ValidateFileForFeature`) **AND** cluster node units ≤ `max_node_units`.

**Claims schema (extends the current SDK `Claims`):**

```json
{
  "customer":       "acme-corp",
  "not_before":     "2026-01-01T00:00:00Z",
  "not_after":      "2027-01-01T00:00:00Z",
  "features":       ["n4k.base", "n4k.image-verify", "n4k.cel"],
  "max_node_units": 200,
  "max_policies":   500,                  // OMIT for unlimited; absent w/o license => default 10 (PoC)
  "license_id":     "uuid-...",          // for backend correlation / revocation
  "issued_to":      "acme-corp prod",    // human-readable, for UI/logs
  "grace_days":     30                    // optional per-license override of default grace
}
```

> `max_node_units`, `max_policies`, `license_id`, `issued_to`, `grace_days` are **new fields** to add to the
> SDK's `Claims` struct. Adding optional JSON fields is backward-compatible with existing `nctl.ai`/`nctl.agent`
> licenses. Note the deliberate asymmetry: an **absent `max_policies` in a valid license means unlimited**,
> whereas **no license at all means the 10-policy PoC default** (§4.2).

### 5.2 Phone-home to Nirmata backend — secondary (visibility)

- On a periodic timer (aligned with the SDK's reporting cadence — sample 15 min, report ~60 min), N4K reports
  license status + current node-unit usage to the Nirmata backend (NCH).
- Purpose: real-time fleet visibility, over-usage detection, expiry warnings, and revocation signaling.
- **Must degrade gracefully offline:** if the backend is unreachable, the offline file remains authoritative and
  N4K keeps running. Phone-home failure alone never degrades the product.
- **Identity resolved (D9):** tenant ID + per-tenant secret. **Still OPEN:** the wire contract — endpoint, auth
  handshake, payload schema, revocation semantics (cross-team with backend/NCH; see §12 OPEN).

### 5.3 AWS Marketplace metering — future

Not in v1. `license-guardian` already implements EKS detection + Marketplace `MeterUsage` reporting; the
detector/enforcer pattern lets us add it as an alternate enforcer later without touching N4K core.

---

## 6. Enforcement model (graduated / grace period)

The admission controller is in the cluster's critical path. A hard failure could block **all** workload
admission cluster-wide. Enforcement therefore escalates gradually and **never crashes the process on license
problems.**

| State | Condition | Behavior |
|-------|-----------|----------|
| **Free tier** | No license present, within the 15-day free window | Full function up to the 10-policy cap (§4.2). Startup + periodic info logs stating "free tier, N days remaining". Encourages obtaining a license. |
| **Free expired** | No license present, past the 15-day free window | Enters **Degraded** directly (skips the paid grace window). Over-cap policy creation refused; core admission stays ON. Loud "free trial expired — license required" warnings/events. |
| **Licensed** | Valid signature, in-window, within entitlement | Full function. Info log at startup: customer, expiry, units used/entitled. |
| **Warning** | Valid but nearing `not_after` (default ≤ 30 days) **or** node units / policy count 90–100% of entitlement | Full function. Periodic **warning** logs, Kubernetes `Warning` events, metric flips. Surfaced to backend. |
| **Grace** | A previously-valid **paid** license expired / went over node-unit or policy entitlement / became invalid, within grace window (default 30 days from first detection, or `grace_days`) | Full function preserved. **Loud** warnings + events on a timer. `n4k_license_valid=0`. Backend flagged non-compliant. |
| **Degraded** | Still non-compliant after grace window | **v1 (capacity-only, D1/D3):** refuse *new* policies over the cap (safe gate); never silently disable already-enforced policies. **Core admission validate/mutate/generate always stays ON** to avoid breaking clusters. *(Post-v1, when feature tiers land: also gate off non-entitled enterprise features via `pkg/toggle`.)* Persistent warnings. |

**Principles:**
- **Fail-open on the hot path, fail-loud everywhere else.** We never introduce an availability risk to the
  customer's cluster to enforce a commercial term.
- Grace clock starts at **first detection of non-compliance**, persisted in a **leader-managed, HMAC-signed
  ConfigMap** in the kyverno namespace (**D5**) so a pod restart can't silently reset it.
- **Degraded-mode scope (D3):** in v1, Degraded gates **only** the policy-count cap (refuse new over-cap
  policies). Core validate/mutate/generate is **never** gated off. Feature gating is deferred with feature tiers.
- All thresholds (free-tier days, grace days, warning window, migration days) are **config-driven**, not hardcoded.

---

## 7. Architecture — sync-safe integration

This is the make-or-break requirement. Design rules, strictest first. The full engineering design (packages,
hook code, build wiring, merge-conflict budget, testing) lives in the companion doc
[`licensing-architecture.md`](./licensing-architecture.md).

### 7.1 Isolation: all license logic in Nirmata-owned files

- New code lives in a **new, clearly-named package** that does not exist upstream, e.g.
  `pkg/nirmata/license/` (or `ext/nirmata/license/`). Upstream will never create a file at that path, so it can
  never conflict on sync.
- Depend on `github.com/nirmata/license-guardian` for signing/validation/metering (add to `go.mod`). `go.mod`
  conflicts are trivial, mechanical, and rare compared to conflicts inside `.go` logic files.

### 7.2 Single hook point in the shared startup path

- All N4K binaries (admission, background, reports, cleanup controllers) route through
  **`cmd/internal/setup.go` → `Setup()`**. That is the one place to initialize licensing.
- Target: **a single added line** in `Setup()` — `license.Init(ctx, logger, config)` — or, even better, register
  the gate via an `init()`/hook in a Nirmata file so `setup.go` needs **zero** edits (preferred; investigate
  feasibility). The fewer characters we touch in an upstream file, the cheaper every sync.
- Licensing runs as background goroutines (startup validation + periodic refresh/report + grace timer),
  identical in spirit to how `license-guardian`'s perpetual reporting already works.

### 7.3 Keep enforcement out of upstream logic files

- Feature gating uses the **existing `pkg/toggle`** flags. License logic *sets* toggles; upstream code *reads*
  them exactly as it does today. No new conditionals inside engine/webhook/controller files.
- Where a feature has no existing toggle, add the toggle **upstream-style** (contribute the pattern) rather than
  a Nirmata-only `if`, so the diff blends in and ideally could be upstreamed.

### 7.4 Build-level gating so `main-oss` stays clean

- The license gate must be **absent or inert** in the OSS mirror. Options to evaluate (§12):
  - **Go build tag** (e.g. `//go:build n4k`) — enforcement compiled only into N4K release images; `main-oss`
    builds without it. Clean separation, but adds a build dimension.
  - **ldflags injection** — like the existing `version`/`commit` vars, inject an N4K build marker; gate is a
    no-op unless set. Zero new files in hot paths.
  - **Runtime overlay only** — gate always compiled but inert without a license path configured. Simplest;
    weakest separation.
- **Recommendation:** build-tag or ldflags, matching how N4K release images are already produced, so the
  distinction is a build concern (CI/goreleaser/ko) not a source concern.

### 7.5 Charts / manifests

- License Secret mount, license path flag/env, and (optional) phone-home config are added to the N4K Helm chart
  overlay. Charts already carry Nirmata-specific NOTES/README changes and an auto-sync workflow, so this is an
  established overlay surface.

---

## 8. Observability & telemetry

- **Logs** (logr, per AGENTS.md levels): L2 startup summary (customer, tier, expiry, units used/entitled); L0/
  warnings for grace/degraded.
- **Kubernetes events:** `Normal`/`Warning` events on a well-known object (kyverno Deployment or a dedicated
  ConfigMap) so `kubectl get events` surfaces license state.
- **Prometheus metrics** (new, N4K-namespaced): e.g. `n4k_license_valid{customer,tier}`,
  `n4k_license_expiry_seconds`, `n4k_license_node_units_used`, `n4k_license_node_units_entitled`,
  `n4k_license_state{state=licensed|warning|grace|degraded}`.
- **Backend reporting** (phone-home): license_id, customer, node units used, state, timestamp.
- **License-status surfacing (D12):** v1 uses **metrics + events + logs**; a CRD status / dedicated
  `LicenseStatus` resource for GitOps/dashboards is deferred pending demand.

---

## 9. Security considerations

- **Signature trust:** Ed25519 public key embedded at build time in the SDK. Private key custody is Nirmata's
  (issuing service / CI secret `NCTL_LICENSE_PRIVATE_KEY`). Never ships in images.
- **Revocation:** offline files can't be revoked offline. Phone-home enables soft revocation (backend marks
  `license_id` revoked → N4K enters grace). Hard offline revocation relies on `not_after` expiry.
- **Anti-tamper is explicitly out of scope** (§3). Source-level bypass is possible for anyone with the code; the
  control is a deterrent + contractual/telemetry lever, not DRM.
- **Grace-clock integrity (D5):** the first-detection timestamp is persisted cluster-side in a leader-managed
  ConfigMap (not pod-local), **HMAC-signed** with the build-injected key to resist casual edits. Key rotation
  behavior is the one remaining OPEN item in §12.
- **Privacy:** phone-home payload must be minimal (no cluster workload data), documented, and disableable for
  air-gapped/privacy-sensitive customers (who then rely solely on the offline file).

---

## 10. Requirements summary (testable)

| # | Requirement | Priority |
|---|-------------|----------|
| R1 | N4K validates an offline nctl license (signature + window + features + node-unit entitlement) at startup. | P0 |
| R2 | Licensing integrates via ≤1 touch-point in an upstream file; all logic in a Nirmata-owned package. | P0 |
| R3 | The OSS mirror (`main-oss`) builds and runs with the gate absent/inert. | P0 |
| R4 | License problems never crash or block the admission hot path (fail-open). | P0 |
| R5 | Enforcement escalates Licensed → Warning → Grace → Degraded per §6, all thresholds config-driven. | P0 |
| R6 | Grace clock starts at first non-compliance and survives pod restart. | P1 |
| R7 | Node-unit usage counted cluster-wide by a single (leader-elected) counter. | P1 |
| R8 | Feature tiers gate optional features via existing `pkg/toggle`. | **Deferred (post-v1, D1)** |
| R8b | Policy-count cap enforced: no-license default 10 (PoC); `max_policies` in license sets the cap; omitted `max_policies` = unlimited; counts all Kyverno policy kinds (D7). | P0 |
| R9 | Prometheus metrics + K8s events + logs expose license state. | P1 |
| R10 | Optional phone-home reports status/usage and enables soft revocation; failure never degrades product. | P2 |
| R11 | License config (path, phone-home) exposed via N4K Helm chart overlay. | P1 |
| R12 | Claims schema extended (`max_node_units`, `max_policies`, `license_id`, `issued_to`, `grace_days`) backward-compatibly. | P0 |

---

## 11. Phased delivery

Licensing is introduced in **Enterprise Kyverno 1.18.x** (see §13 for migration of pre-1.18 customers). The
three phases below deliver increasing licensing capability; each is independently shippable and preserves the
sync-safety and fail-open guarantees.

> **Phase 0 — SDK prep (prerequisite, spans all phases):** Extend `license-guardian` `Claims` (R12: add
> `max_node_units`, `max_policies`, `license_id`, `issued_to`, `grace_days`); define N4K feature strings; stand
> up the Nirmata package + single `Setup()` hook + build-tag/ldflags gating; wire logs/events/metrics. Node-unit
> counting already exists in the SDK.

### Phase 1 — Free tier (no license)

- N4K runs with **no license**: full function up to **10 policies** for **15 days**, then Degraded (§4.2, §6).
- No customer action required; this is the default experience for anyone who installs 1.18.x.
- Delivers: free-tier/free-expired states, policy-count cap, node-unit counting for telemetry, all
  observability. (R3, R4, R5, R8b, R9)
- Goal: ship the enforcement *engine* and prove it never harms a cluster, before any license is ever issued.

### Phase 2 — BYOL (bring your own license, offline file)

- Nirmata issues a signed **nctl license file** and **shares it with the customer via email**.
- Customer **manually adds the license to the Helm chart** (as a Secret value / mounted file) at install/upgrade.
- N4K validates it offline (§5.1) and unlocks capacity per claims (node units, `max_policies`). Feature-tier
  gating is **not** part of v1 (D1) — the `features` claim is validated but not enforced yet.
- Delivers: offline validation, Licensed/Warning/Grace states, degraded-after-grace, Helm overlay for the
  license Secret. (R1, R2, R6, R7, R11, R12)
- This is the first **commercially enforceable** phase and the target for GA of licensing.

### Phase 3 — Dial home (auto license retrieval)

- N4K **retrieves its license automatically from a central Nirmata service** using a **tenant ID** (D9) — plus a
  provisioned per-tenant secret — set in the chart (instead of pasting the license file).
- Requires: a central licensing/entitlement service, a per-tenant identity/secret, periodic check-in for
  status + usage reporting, and **soft revocation** (backend marks `license_id`/tenant revoked → N4K enters
  grace).
- Must degrade gracefully offline: a previously-retrieved/cached license remains authoritative if the service is
  unreachable; dial-home failure alone never degrades the product (§5.2). (R10)
- Delivers real-time fleet visibility and removes the manual email/paste step from Phase 2.

### Future (post-1.18)

- AWS Marketplace metering enforcer (SDK already supports it for EKS); license-status CRD / dashboard surfacing.

---

## 12. Decisions

Resolved 2026-07-20. Items marked **DEFERRED** are intentionally out of scope for v1 (1.18.x) and revisited in a
later minor; **OPEN** items remain to be settled (owner noted).

| # | Topic | Decision |
|---|-------|----------|
| D1 | **v1 licensing scope** | **Capacity-only for 1.18.x** — enforce node units + policy-count cap. **No feature-tier gating in v1.** This is the pivotal scoping call: it removes the need to finalize the feature catalog/SKU map (D2) or add any feature-gating toggles for v1. Feature tiers are DEFERRED (§4.3). |
| D2 | Feature catalog & tier mapping | **DEFERRED** with feature-tier gating (D1). No `features`→SKU map ships in v1; the `features` claim is still carried and validated, just not used to gate behavior yet. |
| D3 | Degraded-mode scope | In v1, "Degraded" = **policy-cap enforcement only** (refuse *new* policies over cap) + loud warnings/events. **Core validate/mutate/generate always stays ON.** No feature gating (follows D1). |
| D4 | Build gating mechanism | **Build tag `n4k` (primary)** — compile-time exclusion so `main-oss` links no license code; **ldflags marker (secondary)** for build-channel + key fingerprint. Runtime-only rejected. (§7.4) |
| D5 | Grace/free-clock persistence | **Leader-managed ConfigMap** in the kyverno namespace, **HMAC-signed** with the build-injected key. Not pod-local, not memory. (§6, §9) |
| D6 | Node counting under HA | **Admission controller only, under the existing leader election.** Other components never count nodes. (§4.1, R7) |
| D7 | Policy counting & enforcement point | **Count all Kyverno policy kinds** — `ClusterPolicy`, `Policy`, and the CEL-based `ValidatingPolicy`/`MutatingPolicy`/`GeneratingPolicy`/`ImageValidatingPolicy`/etc. **Phase 1:** timer count feeding state. **Phase 2:** add a Nirmata-owned reject-on-create webhook. (§4.2) |
| D8 | Migration window | **90 days** for existing pre-1.18 installs. "Existing install" heuristic = pre-existing Kyverno policy CRs at first 1.18.x start (and/or a prior-version marker). Anchored by the same HMAC-signed marker as the grace clock to resist gaming. (§13) |
| D9 | Dial-home identity (Phase 3) | **Tenant ID** (+ a provisioned per-tenant secret), aligned with the NCH tenant model; one tenant may cover many clusters. (§11 Phase 3) |
| D10 | License distribution & renewal | **Phase 2 (BYOL):** Nirmata issues a signed file, delivers by **email**, customer adds it to the Helm chart. **Renewal:** reissue a new file (new `not_after`), same delivery. **Phase 3:** dial-home auto-retrieval by tenant ID removes the manual step. Self-service portal is a future enhancement. |
| D11 | Zero-touch `setup.go` | **Accept the single line** in `Setup()` (via the build-tag hook pair). Not worth the complexity of an init-hook to reach literal zero. (§7.2) |
| D12 | License-status surfacing | v1 = **metrics + Kubernetes events + logs**. A status CRD / dedicated resource is DEFERRED pending GitOps demand. (§8) |
| D13 | CLI (`kubectl-kyverno`) scope | **Not licensed.** It is an offline policy-evaluation tool, not the enforcement surface. |

**OPEN (owner):**
- **Phone-home contract** (§5.2) — endpoint, auth handshake, payload schema, revocation semantics. *Eng + backend/NCH; Phase 3.* Identity is settled (D9); the wire contract is not.
- **HMAC key rotation** (§9, D5) — rotating the build-injected key invalidates stored grace/migration markers (they fail HMAC → treated conservatively as "first detection now"). Confirm this fail-loud-but-safe behavior is acceptable, or provision a cluster-side key instead. *Eng.*

---

## 13. Migrating existing customers (pre-1.18.x → 1.18.x)

Licensing lands in **1.18.x**. Customers already running N4K (1.17.x and earlier) have **no license today** and
must not be broken by an upgrade. The graduated, fail-open model (§6) is the safety net; the following makes the
transition explicit and non-disruptive.

### 13.1 Principles

- **No upgrade to 1.18.x may reduce a running cluster's protection on day one.** An existing customer who
  upgrades before receiving a license must not immediately lose policy enforcement.
- Enforcement is **time-bounded and loud**, never a silent hard cutover.
- Communicate **before**, **during**, and **in-product**.

### 13.2 Mechanism — migration grace window

- On first start of 1.18.x, if **no license** is present but the cluster shows signs of an **existing
  install** (pre-existing Kyverno policy CRs at first 1.18.x start, and/or a prior-version marker), N4K enters a
  **90-day migration grace window** (**D8**; longer than the 15-day new-user free tier) — during which it runs at
  **full entitlement** (no 10-policy cap), emitting escalating warnings/events/metrics and (if dial-home is
  available) backend notifications to obtain a license.
- After the migration window, the cluster falls back to standard Degraded behavior (§6). Core admission
  validate/mutate/generate always stays ON.
- The window is anchored by the same **HMAC-signed marker** as the grace clock (**D5/D8**) to resist gaming via
  reinstall/restart.

### 13.3 Rollout sequence

1. **Pre-announce** (release notes, email, docs): licensing arrives in 1.18.x, what changes, how to get a
   license, and the migration-window length. Ship this ahead of the 1.18.0 tag.
2. **Issue licenses proactively** to all active/entitled customers **before** they upgrade, so the ideal path is
   "license already in place at upgrade time" (Phase 2 BYOL, or Phase 3 dial-home once available).
3. **Upgrade** to 1.18.x — migration grace window covers anyone not yet licensed.
4. **In-product nudges** during the window (logs/events/metrics; dial-home banners where available) with days
   remaining and remediation steps.
5. **Post-window**: standard enforcement. Support/CS dashboards (via phone-home) flag any still-unlicensed
   clusters for outreach *before* they degrade.

### 13.4 Compatibility guarantees

- 1.18.x with a valid license behaves identically to pre-1.18 (no functional regression from licensing).
- Downgrade path: a 1.18.x → 1.17.x rollback removes the gate entirely (pre-licensing code), so a botched
  migration is always recoverable by rolling back the chart.
- The free-tier 15-day rule (§4.2) applies to **net-new** installs; **existing** installs get the longer
  migration window instead — the two must be distinguished by the "existing install" heuristic above.

---

## Appendix A — Key repo facts this design relies on

- **Module unchanged:** `go.mod` is `github.com/kyverno/kyverno` (downstream fork, not renamed).
- **Upstream sync:** `.github/workflows/sync-main-oss.yml` pulls `kyverno/kyverno@main` → `main-oss` daily and
  auto-PRs to `main`. `main-oss` must remain a clean mirror.
- **Shared startup:** `cmd/internal/setup.go` `Setup()` is the common init path for all controller binaries.
- **Feature flags:** `pkg/toggle` is the existing mechanism for enabling/disabling behavior.
- **Version injection precedent:** `main.go` `version`/`commit`/`buildDate` are ldflags-injected — a proven
  pattern for build-time markers.
- **SDK reuse:** `nirmata/license-guardian` provides `pkg/nctllicense` (Ed25519 sign/verify, `Claims`,
  `ValidateFileForFeature`), node-unit counting (`ceil(mem_GiB/33)`), and a detector/enforcer pattern with an
  existing AWS Marketplace enforcer.

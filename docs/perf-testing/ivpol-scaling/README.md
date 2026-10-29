# ImageValidatingPolicy scaling harness

Measures what image verification costs on the admission path, and what makes
that cost grow. Sibling to [`../clusterpolicy-scaling`](../clusterpolicy-scaling),
reusing its cluster rig (KinD + KWOK + Prometheus) and its shuffled round-robin
ladder methodology.

ivpol is a different animal from the CEL validating and mutating engines. Those
evaluate expressions; this one fetches image manifests over the network and
verifies signatures. Registry round trips and crypto dominate, so the parameters
that matter are different, and so are the ways a run can silently measure
nothing.

## The question this answers

**Does verification cost scale with the number of images in a Pod, and how much
did that change between releases?**

PR #17313 changed the per-image manifest prefetch from concurrent to sequential,
to fix a map race in the SDK:

```diff
-	if err := ictx.AddImages(ctx, imgList, c.authOpts, c.nameOpts); err != nil {
+	// Prefetch image data through Get() one image at a time to avoid triggering
+	// racy concurrent map writes in the SDK AddImages() implementation.
+	for _, image := range imgList {
+		if _, err := imgCtx.Get(ctx, image, c.authOpts, c.nameOpts); err != nil {
```

`AddImages` runs an errgroup with 20 workers; `Get` fetches inline. So a Pod's
image-fetch cost went from roughly *the slowest image* to *the sum of all
images*. It is a correctness fix with a throughput price that has not been
quantified. Quantifying it is the point of the headline chart.

It lands in **v1.19.1** and not **v1.19.0**, with nothing else relevant to the
image path between them, which makes that pair a clean A/B.

### The 1.19.0 crash (chart 1)

Before #17313, `AddImages`'s unsynchronized map writes (SDK
`imagedataloader/context.go:43-66`) can trip the Go runtime's
`fatal error: concurrent map writes` detector. This is a genuine,
unconditional data race on any code path with two or more images per Pod, but
the detector only fires when two writes land in the same scheduler tick, so
whether a given run observes it depends on network timing, not on the image
count alone.

This harness's TLS-terminating registry changes that timing: pooled TLS
connections spread the two goroutines' completions wider than a plaintext
rig does, which narrows the window the detector needs. Confirmed empirically:
at the plaintext rig's original shape (2 images, +100 ms, 1 VU) the crash did
not reproduce on the TLS rig (0 restarts, 0% failures). A single follow-up
probe at a shape that maximizes simultaneous map writes (8 images, 0 ms
delay — more concurrent goroutines per request, no added spread from
injected latency) **did** reproduce it: 2 restarts, 72.4% admission failures,
`fatal error: concurrent map writes` in the container logs. So the race is
still live on the TLS rig; the window is merely narrower than on a plaintext
rig, not closed. The reliable, cheap reproduction nonetheless lives on a
separate, pinned commit rather than the current (TLS) rig — see
`results/crash-boundary/SOURCE.md` for the exact commit and command. Don't
expect the crash at every shape on the current rig; it is not one of this
rig's validity gates, and the rig deliberately has no plaintext or
non-pooled mode to force it back.

## What a run measures

| Sweep | Cells | The claim it supports |
|---|---|---|
| **images per pod** | {1,2,4,8,16} × {0, +25 ms, +100 ms} × {1.19.0, 1.19.1} | Verification cost scales with images per Pod, and 1.19.1 scales worse. The 0 ms lane is the control for 1.19+: with no injected latency, those versions should converge. 1.18.2 does not, because it pays a Sigstore CDN round trip per image that no delay setting removes - see below. |
| **cache state** | {off, warm, post-policy-edit} at a fixed shape | What the verification cache is worth, and that editing *any* policy throws it away. |
| **concurrency ramp** | VUs raised until the webhook times out | Admissions per second before it breaks — the literal "will it scale" number. |
| **release-over-release** | {1.18.2, 1.19.0, 1.19.1} at a fixed shape | How the feature matured. **Not attributable to any single change** — see below. |

### Why 1.18.2 cannot carry an attribution

Four things changed between 1.18.2 and 1.19.0 that touch this path: ivpol cache
reuse (#16694), intoto payload repopulation on cache hit (#17117), TUF access
serialization (#16663), and the absence of the serial prefetch (#17313, which
arrives later in 1.19.1). A 1.18→1.19 bar therefore says *something moved*, not
*which change moved it*.

That is still a legitimate slide — "here is the feature maturing across
releases" is what an operator deciding when to upgrade wants — as long as it is
labelled as release-over-release and never as a claim about one change. The
clean attribution lives in the 1.19.0 → 1.19.1 pair, where exactly one relevant
thing differs.

## Run it

Prerequisites: Docker, `kubectl`, `helm`, `cosign`, `python3`, and the `k6-xk6`
build from the sibling directory.

```sh
# 1. cluster rig (KinD + KWOK fake node + Prometheus) — the sibling's, unmodified
CLUSTER=ivpol-bench bash ../clusterpolicy-scaling/setup-mac.sh

# 2. registry: in-cluster, behind the latency proxy, 16 signed images
bash registry/setup-registry.sh

# 3. the sweeps
bash ladder-ivpol-run.sh      # images x latency, 1.19.0 vs 1.19.1   (~4.1 h)
bash ivpol-cache-run.sh       # cache off / warm / post-policy-edit  (~1.3 h)
bash ivpol-ramp-run.sh        # concurrency to saturation            (~0.8 h)

# or one version's matrix on its own
KYVERNO_VERSION=1.19.1 KYVERNO_CHART=3.9.1 bash ivpol-run.sh

python3 ivpol-report.py results/ladder --baseline 1.19.0

# or, for a version comparison table (shape-aware; refuses to average
# cells that differ in images, delay, load shape or image digest)
python3 version-table.py results/ladder
```

Add the release-over-release chart by widening the ladder's versions:

```sh
VERSIONS="1.18.2:3.8.2 1.19.0:3.9.0 1.19.1:3.9.1" IMAGES=8 bash ladder-ivpol-run.sh
```

### Versions with no published chart

A chart of `local` means the version has no chart on kyverno.github.io - release
candidates - so `fetch-chart.sh` extracts one from the repo at that git tag:

```sh
VERSIONS="1.19.1:3.9.1 1.19.2-rc.1:local" IMAGES=8 bash ladder-ivpol-run.sh
```

Two traps this handles, both of which would otherwise produce a chart
attributing numbers to the wrong version:

- **The chart at an RC tag carries the previous release's appVersion.** At
  `v1.19.2-rc.1` it still says `appVersion: v1.19.1`, so installing it as-is
  pulls the 1.19.1 image. The image tag is therefore overridden explicitly, and
  `KYVERNO_CHART_PATH` without `KYVERNO_IMAGE_TAG` is refused outright.
- **The chart's default registry cannot be probed.** `reg.kyverno.io` answers
  401 to an anonymous manifest request for every tag, so there is no way to
  confirm an RC image is there before starting a slot. `ghcr.io` can be checked
  and has both tags, so local-chart installs point there
  (`LOCAL_CHART_REGISTRY` overrides it).

After install, the running container image is read back and compared against
the version the slot claims. A mismatch aborts rather than producing numbers
filed under the wrong release.

**What 1.19.2-rc.1 is worth measuring for.** It carries the ivpol request-map
hoist (#17585 / #17872) plus three security fixes, one of which (#17996) rewrites
how layer reads are bounded and so also touches the registry read path. That
makes a 1.19.1 → 1.19.2-rc.1 delta release-over-release, not a clean attribution
of the hoist - the same caveat as 1.18.2. Expect little from the hoist here
regardless: it only pays when one engine call carries several policies, and
ivpol policies are almost always fine-grained (see below).

### Cells are timed, not counted

Every cell runs for `LOAD` (default `60s`) at fixed VUs, rather than for a fixed
number of iterations. A fixed count makes cell length depend on how expensive
the cell is: 400 iterations at +100 ms with 16 images runs about 32 s, but the
same 400 at 0 ms with one image finishes in under a second - far too short to
read CPU out of Prometheus, and that cheap lane is the control the whole chart
leans on. A duration gives every cell the same window, and makes throughput
(iterations completed) a second comparable metric. `LOAD=400` still works and
means a fixed count.

Smoke it first — one round, two cells, a few minutes:

```sh
ROUNDS=1 IMAGES="1 8" DELAYS="100" VUS=5 LOAD=20s bash ladder-ivpol-run.sh
```

**Kill criterion.** If the `img8-d100` cell shows no gap between 1.19.0 and
1.19.1 in that first smoke run — which is why the smoke uses the +100 ms lane,
where the effect should be largest — the premise is wrong and the rest of the matrix
is not worth running. Check that before committing hours to it.

## Keyless is two measurements, not one

"What does keyless cost" conflates two independent things, and the rig measures
them separately because only one of them can be measured offline.

**Signature format** is the registry question: a legacy `<digest>.sig` tag
against an OCI 1.1 bundle hung off the image as a referrer. This is fully
measurable with no Sigstore infrastructure, because Kyverno picks the format by
probing the registry rather than by looking at the attestor type
(`verifier.go:39-41`). Both repos are signed with the same key A, so
`MODE=key` against `MODE=bundle-key` isolates the format and nothing else.

It has to be a **key** attestor, not a certificate one. A certificate attestor
sets `opts.SigVerifier` only when the policy supplies the leaf `cert:`
(`opts.go:229-240`); with `certChain` alone it stays nil, and
`insecureIgnoreTlog` additionally takes `skipSigstoreInfra`, leaving
`opts.TrustedMaterial` nil too. cosign then refuses before touching the
registry: "a trusted root is required for identity-based verification"
(`verify.go:208-210`). So there is no offline cert-plus-bundle cell to be had,
and the cert-chain cell stays on the legacy format.

**Trust model** is the Sigstore question: a key or certificate against Fulcio
identities. This one cannot be made offline by configuration.
`skipSigstoreInfra` is `keyOrCert && ignoreTlog`, and `keyOrCert` requires
`att.Keyless == nil` (`opts.go:85-87`), so a keyless attestor always runs
`initTUFAndFetch`. That does two forced TUF refreshes per attestor per
verification — `forceUpdate=true` bypasses the on-disk cache — inside a single
process-wide mutex (`pkg/sigstoretuf/sigstoretuf.go`). Setting
`trustedRoot.value` swaps the material used for verification; it does not
prevent the fetch. Measuring it therefore needs an in-cluster TUF mirror, which
is why `policies/ivpol-cosign-keyless.yaml` refuses to render until one exists.

Keeping them apart is what lets the first half be reported now and the second
half be reported honestly as a code read until its rig is built.

| `MODE` | Policy | Images | Measures |
|---|---|---|---|
| `key` | key attestor, legacy `.sig` | `bench/signed` | the original baseline |
| `cert` | certificate attestor, legacy `.sig` | `bench/signed-cert` | local chain building, offline |
| `bundle-key` | key attestor, bundle | `bench/signed-bundle` | the bundle format's own cost, same key as `key` |
| `bundle-combined` / `bundle-split` | two roots, bundle | `bench/signed-bundle` | whether the cache defeat reproduces on bundles |
| `keyless` | keyless attestor, bundle | `bench/signed-bundle` | the TUF path (needs the mirror) |

`MODE` exists because the policy template, the image repository and the
expected request signature have to agree. Setting them separately is how a cell
ends up with a policy whose glob names one repository while the workload uses
another: nothing matches, every admission is a fast no-op, and the cell reports
excellent latency.

## The registry serves referrers, and that is a measurement decision

The backend is zot, not distribution. distribution 3.x has no Referrers API
route at all, so `/v2/<repo>/referrers/<digest>` returns a plain-text 404.

That 404 is survivable. go-containerregistry defaults
`referrersTagFallback` to true and nothing turns it off, so the client falls
back to the dist-spec 1.1 referrers tag, and cosign v3 writes that tag. Bundles
do work on distribution. The reason to run zot anyway is that the fallback
costs a 404 probe plus a tag fetch where a referrers-capable registry answers
in one request, and that extra round trip would otherwise be counted as part of
the code path's cost.

`REGISTRY_FLAVOR=distribution` brings the old backend back, which is how the
"what does a registry without the Referrers API cost you" cell is run. The
proxy counts `referrers` and `fallback_tag` separately, so the two are
distinguishable in any cell's `registry-stats.json`.

**The swap breaks comparability with everything measured before it, including
every number in the published findings.** Kyverno issues the bundle probe on
every uncached verification regardless of signature format, so on distribution
each one cost two delayed round trips (the 404, then the fallback tag) where
zot costs one. Every per-image request count therefore shifts with the backend,
on the legacy `.sig` path as much as on the bundle path. Each cell now records
`registry_flavor` in `meta.txt`, read from the running deployment rather than
from an environment variable; cells archived before the swap carry no such
field, and absence means distribution. Do not put a pre-swap and a post-swap
number in the same table without saying which is which.

See `registry/SOURCE.md` for the probes that establish all of this.

## Why the registry is in the cluster

Kyverno verifies images by making its own HTTP calls from the admission
controller pod. The usual kind host-side registry, reached through containerd
mirroring, serves the kubelet — not Kyverno — so the thing being measured would
never touch it. An in-cluster registry gives Kyverno a resolvable address and
puts the latency proxy on the path that actually matters.

Images are built `FROM scratch` with one distinct file each, so every tag has
its own digest and forces its own manifest fetch. They are never pulled: Pods
land on the KWOK fake node, which fakes the kubelet.

## The latency proxy

`registry/delay-proxy.py` sits between Kyverno and the registry and does two
jobs: it adds a fixed delay per HTTP request, and it counts requests by kind
(manifest, blob, tags).

It is in path for **every** cell, including the 0 ms baseline, so the topology
is identical across lanes and the request counters are always available. A
baseline that bypassed it would be a different system *and* would have no way to
prove verification happened.

The delay is applied per request, not per connection — verified against a
keep-alive client, where four sequential requests on one connection cost
4 × delay. That matters: Kyverno's registry client reuses connections, and a
per-connection delay would charge a serial loop the same as a parallel fetch,
erasing the effect being measured.

```sh
ACTION=stats bash registry/setup-registry.sh     # read counters
ACTION=delay DELAY_MS=100 bash registry/setup-registry.sh
```

## Two things 1.18.2 does that no other version does

Both come from reading the 1.18.2 source, corroborated by behaviour on this
rig. The CDN round trip is sampled per cell; the serialization is inferred from
the code and from a goroutine dump showing 33 verifications blocked on one
mutex. Both must be stated beside any 1.18 number.

**It contacts Sigstore on every verification.** `checkOptions` calls
`tuf.Initialize` unconditionally, with `forceUpdate=true`, so every image
verification fetches TUF metadata from `tuf-repo-cdn.sigstore.dev` - even for a
key attestor with the transparency log ignored. The registry proxy never sees
this traffic, so no request count includes it. Each cell therefore samples the
CDN round trip and records `sigstore_cdn_rtt_ms`, `sigstore_cdn_reachable` and
`tuf_per_verification`. On this rig a sample was about 0.4 s, so a 4-image Pod pays at least ~1.6 s of
CDN time (a TUF refresh is several requests, not one) that the registry counters do not show. Resist pairing that with a
1.19.x figure until both come from matched cells: the archived numbers were
taken at different load shapes, and 1.18.2's histogram mean is per webhook
invocation rather than per admission, so the two are not the same quantity.
`version-table.py` prints `rev/iter` and marks such a column `per-inv`. 1.19 added `skipSigstoreInfra`, which skips all of it for
key and certificate attestors that ignore the log.

**It serializes.** That same call holds a process-wide mutex for the whole of
`checkOptions`, so cosign verifications run one at a time per process. Offering
1.18.2 any arrival rate builds an unbounded queue and every request passes the
30 s webhook deadline, which is latency measuring queue depth rather than work.
Hence `CONCURRENCY=1`: a `constant-vus` k6 executor pinning one admission in
flight, applied to **every** version so the comparison is like-for-like. The
shape is recorded per cell as `load_shape`, and `version-table.py` refuses to
average cells that disagree on it. Replicas do help, since the mutex is
per-process, and keyless still serializes on 1.19.

## Validity gates

A cell that silently stops verifying looks exactly like a fast cell. Every slot
and cell is gated, and a failure aborts rather than producing a number:

| Gate | Catches |
|---|---|
| **Negative control** — an unsigned image must be rejected | A mis-scoped `matchImageReferences`, a policy that never compiled, a webhook that never registered. Without it, every latency number would be measuring an empty pipeline. |
| **Positive control** — a correctly signed image must be admitted | The opposite failure: a wrong key or unreachable registry makes *everything* fail fast, which passes the negative control and looks like a very quick cell. |
| **Manifest floor** — at least one manifest fetch per request | Verification short-circuiting. The prefetch runs per request in every cache state, so this holds for warm cells too. |
| **Manifest coverage** — warns below ~one fetch per image per request | Fewer images really being fetched than the cell claims, which would flatten the N axis for a reason that is not the engine. |
| **Warm-cache evidence** — signature blob fetches per second, warm-up window against measured window | A "warm" cell that is actually cold, which would misattribute the cache's effect. This **warns and records** rather than aborting: the install-time serving probe can itself warm the cache, so on the first cell after an install the rate legitimately does not fall. The numbers land in `warm-cache-evidence.txt`, so a warm claim can be checked rather than assumed. Earlier versions of this table described this as an assertion; no such code existed. |
| **Signature format** — no `.sig` tag fetches in a cell labelled bundle | A bundle cell silently measuring the legacy path. When no bundle is found Kyverno does not error, it falls back (`verifier.go:40`), so the cell keeps running under a label that no longer describes it. Counting *referrers lookups* cannot catch this: `GetBundles` probes on every uncached verification whatever the image carries, so a legacy cell probes too. The `.sig` tag fetch is the only bucket that reports an outcome. Warm and postedit cells are judged on their warm-up window, because a cache hit issues no verification traffic at all (`impl.go:157-166`). Only applies when `EXPECT_BUNDLE=1`. |
| **End-of-cell controls** — the positive and negative controls re-run after the measurement | A signing certificate expiring, or a policy being replaced, part-way through a sweep. Both turn every later cell into the failure path, which is faster than verification and reads as an excellent result. Skipped under `ALLOW_FAILURES=1`, where saturation is the point. |
| **Failure rate** — above 1% aborts the cell | A rejected or timed-out request still completes an iteration, and the failure path is *faster* than verification — so a broken or saturated cell reports better latency than a healthy one. `ALLOW_FAILURES=1` lifts this for the concurrency ramp, where saturation is the point. |
| **Verification coverage** — referrers probes must reach 95% of iterations x images | Admissions that completed without verifying. The manifest-coverage figure cannot see this: its denominator assumes about one manifest per image, so it reads a few hundred percent and passes regardless. Cache-off cells only. |
| **Legacy-format floor** — a cell labelled legacy must show `.sig` fetches near the same 95% floor | The converse of the bundle gate: a key cell that silently verified bundles instead. |
| **Restart check** — recorded per cell, flagged by the report | A pod restart mid-cell empties the webhook Service; with `failurePolicy: Fail` those requests fail and the latency is meaningless. |
| **Even rounds** — the report *warns* when round counts differ | A version measured in two rounds against one measured in three is not a comparison. This is a warning in `ivpol-report.py`, not a refusal; read it before trusting an uneven table. |

## Things to know before reading a chart

**The cache is consulted *after* the fetch.** The per-image manifest prefetch
(`pkg/image/verification/evaluator/policy.go:180`) runs before the verification
cache is checked (`pkg/cel/libs/imageverify/impl.go:168`), and `ImageContext` is
rebuilt per admission request with a per-instance memo map. So the images-per-pod
curve is measurable in **any** cache state. Cache state is an orthogonal
variable, not a precondition.

**These policies are fine-grained, on purpose.** Setting `timeoutSeconds` makes
Kyverno give a policy its own webhook endpoint
(`pkg/controllers/webhook/validating.go:35`), so each engine call carries one
policy. That is what real ivpol policies look like — 40 of the 55 in the
conformance suite set it — so it is the honest default, even though it means a
policy-count sweep would measure very little. That sweep is deliberately out of
scope.

**Absolute numbers are host-specific.** Compare within a run. The ladder exists
so that comparisons *between versions* survive thermal drift; it does nothing
for comparisons between machines.

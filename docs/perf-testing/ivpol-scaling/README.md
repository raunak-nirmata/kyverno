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

## What a run measures

| Sweep | Cells | The claim it supports |
|---|---|---|
| **images per pod** | {1,2,4,8,16} × {0, +25 ms, +100 ms} × {1.19.0, 1.19.1} | Verification cost scales with images per Pod, and 1.19.1 scales worse. The 0 ms lane is the control: with no latency to serialise, the versions should converge. |
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
```

Add the release-over-release chart by widening the ladder's versions:

```sh
VERSIONS="1.18.2:3.8.2 1.19.0:3.9.0 1.19.1:3.9.1" IMAGES=8 bash ladder-ivpol-run.sh
```

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

## Validity gates

A cell that silently stops verifying looks exactly like a fast cell. Every slot
and cell is gated, and a failure aborts rather than producing a number:

| Gate | Catches |
|---|---|
| **Negative control** — an unsigned image must be rejected | A mis-scoped `matchImageReferences`, a policy that never compiled, a webhook that never registered. Without it, every latency number would be measuring an empty pipeline. |
| **Positive control** — a correctly signed image must be admitted | The opposite failure: a wrong key or unreachable registry makes *everything* fail fast, which passes the negative control and looks like a very quick cell. |
| **Manifest floor** — at least one manifest fetch per request | Verification short-circuiting. The prefetch runs per request in every cache state, so this holds for warm cells too. |
| **Manifest coverage** — warns below ~one fetch per image per request | Fewer images really being fetched than the cell claims, which would flatten the N axis for a reason that is not the engine. |
| **Warm-cache proof** — signature blob fetches must fall after warming | A "warm" cell that is actually cold, which would misattribute the cache's effect. |
| **Failure rate** — above 1% aborts the cell | A rejected or timed-out request still completes an iteration, and the failure path is *faster* than verification — so a broken or saturated cell reports better latency than a healthy one. `ALLOW_FAILURES=1` lifts this for the concurrency ramp, where saturation is the point. |
| **Restart check** — recorded per cell, flagged by the report | A pod restart mid-cell empties the webhook Service; with `failurePolicy: Fail` those requests fail and the latency is meaningless. |
| **Even rounds** — the report refuses to compare unequal round counts | A version measured in two rounds against one measured in three is not a comparison. |

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

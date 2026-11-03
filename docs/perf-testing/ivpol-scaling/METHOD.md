# How the ivpol benchmark was run

The six things a reader needs in order to judge the numbers or reproduce them.
Values below are the ones used for the published 1.18.2 / 1.19.1 / main results;
each is an environment variable, so a different run can change it.

## Cluster

A throwaway KinD cluster, rebuilt from scratch for every version so nothing
carries over between them.

| | |
|---|---|
| Kubernetes | `kindest/node:v1.31.6` |
| Real nodes | 1 control plane + 3 workers (three so the admission controller's replicas can spread) |
| Fake nodes | KWOK nodes advertising 32 CPU / 256 GiB / 1000 pods |
| Host | MacBook, Apple silicon, 12 cores, Docker Desktop |
| Kyverno | installed by Helm — chart 3.8.2 for 1.18.2, 3.9.1 for 1.19.1 and main |
| Kyverno settings | chart defaults, except the cache and reports flags set per cell |

**Why KWOK matters.** KWOK fakes the kubelet, so a Pod is admitted and scheduled
but its image is never pulled. Without it the node would pull every benchmark
image and the registry traffic we count would be a mix of Kyverno's work and
the kubelet's. With it, every request the registry sees came from Kyverno.

**What this is not.** One laptop, one admission-controller replica under load,
no etcd pressure, no competing controllers. Absolute milliseconds are this
machine's; the version-to-version ratios and the registry call counts are what
travel.

## Policies

One `ImageValidatingPolicy` at a time. Nothing else is installed — no
ClusterPolicies, no other admission webhooks.

| Setting | Value | Why |
|---|---|---|
| Attestor | one cosign public key | the cheapest trust model, so the number is a floor not a worst case |
| `failurePolicy` | `Fail` | an error must surface as a rejected Pod, never a silent allow |
| `validationActions` | `[Deny]` | same reason |
| `webhookConfiguration.timeoutSeconds` | `30` | pinned, not left to the chart default, or versions would be compared on different timeouts |
| `required` | `true` | with it off, 1.18.2 silently skipped verification for ~15% of admissions in a measured cell |
| `mutateDigest` / `verifyDigest` | `false` for the main sweeps, `true` for the default-config cells | see Cache state |
| `ctlog.insecureIgnoreTlog` | `true` | the images are signed offline with a local key, so there is no transparency-log entry to find; leaving it on would put a public service on the admission path |

1.18.2 runs a textually different policy file (`-118` variants) because 1.18
rejects a `ctlog` block that has no `url` and no inline log keys. The added
fields are inert — same key, same images, same CEL expression.

## Image mix

| | |
|---|---|
| Built | `FROM scratch`, one distinct file per tag, so each has its own digest |
| Pushed per repo | `IMAGE_COUNT` tags, `:v1`…`:vN` (4 for the published runs) |
| **Actually exercised** | **`:v1` only**, because each Pod carries `IMAGES_PER_POD=1` container and k6 names images `:v1`…`:v{IMAGES_PER_POD}` |
| Controls | `:unsigned-control` (no signature) and, on cert repos, `:wrong-ca-control` (valid signature, untrusted root) |
| Repos | `bench/signed` (key), `bench/signed-cert`, `bench/signed-bundle`, plus `-b` variants for two-root cells |

**This is the biggest caveat in the whole rig.** The measured workload is *one
image, admitted thousands of times*. That is deliberate — it isolates
per-verification cost from registry-side variance — but it makes the cache look
as good as it possibly can. A cluster admitting thousands of distinct images
would see a far lower hit rate and more registry traffic per admission. Read the
cache numbers as "the cache works and here is its shape", not as a hit rate you
should expect.

The images are also single-arch and essentially empty, so nothing here measures
manifest-list handling or layer size effects.

## Cache state

`CACHE_MODE` takes three values, and the harness asserts the resulting flag is
actually on the running Deployment before it measures anything — a mode that
silently failed to apply would otherwise look like a fast cell.

| Mode | Flag | Meaning |
|---|---|---|
| `off` | `--imageVerifyCacheEnabled=false` | every admission does full verification |
| `warm` | `--imageVerifyCacheEnabled=true` | steady state, after a discarded warm-up burst |
| `postedit` | `true`, policy touched mid-run | measures what editing a policy costs the cache |

Cache size and TTL are chart defaults unless overridden; the default-config
cells set `imageVerifyCacheMaxSize=10485760` explicitly.

Two facts worth knowing before comparing cache numbers across versions:

- **1.18.2 has an image verify cache, but ImageValidatingPolicy never consults
  it** — only the legacy ClusterPolicy image rules do. So its warm and cold cells
  are the same work.
- **On main, a cached verdict is only stored for a digest-resolved reference.**
  With `mutateDigest: false` the image stays a tag and nothing caches. The
  published default-config cells therefore use `mutateDigest: true`, which is the
  CRD default.

## Offered load

k6 drives the load, creating Pods through the Kubernetes API.

| | |
|---|---|
| Executor | `constant-vus` — a fixed number of admissions in flight |
| Concurrency | 1 for the latency and cache cells; ramped 1 → 2 → 4 → 8 → 16 → 32 for capacity |
| Duration | 120 s or 180 s per measured cell |
| Warm-up | 10 s, discarded, before every measured window |
| Registry delay | 25 ms injected per request by the proxy |
| Rounds | 2–3 per version, version order rotated each round |

**Closed loop, not open.** `constant-vus` holds N requests in flight and waits
for each to finish, so a slow controller is never sent more than it can take.
The alternative, a fixed arrival rate, would queue against a saturated webhook
and report the queue rather than the engine — that is how a benchmark ends up
measuring its own backlog.

**Why concurrency 1 for the headline cells.** It isolates per-admission cost.
It is also why the throughput row at concurrency 1 is arithmetically
`1 ÷ latency` rather than an independent measurement — the capacity ramp is the
independent one.

## Errors and timeouts

A failed admission completes faster than a real one, so a cell containing
failures reports a better number than the truth. The harness treats that as the
primary threat.

| Guard | Rule |
|---|---|
| Failure rate | a cell above **1%** aborts and is not reported |
| Timeout | webhook `timeoutSeconds: 30`; a request that exceeds it is rejected, because `failurePolicy: Fail` |
| Restarts | pod restarts are counted per cell against that cell's own baseline; any restart invalidates it |
| Negative control | an unsigned image must be **denied** before and after every cell |
| Positive control | a correctly signed image must be **admitted** — catches a policy that rejects everything and looks fast |
| Serving probe | short discarded bursts until the controller both succeeds and demonstrably reaches the registry, before measurement starts |
| Verification coverage | on cache-off cells, ≥95% of admissions must have produced registry traffic, or the cell aborts |

k6 counts a failure as any exception from the Pod create call — a webhook
denial, a timeout, or an API error. It logs up to five distinct messages per
cell, because a failing cell otherwise produces thousands of identical lines.

`ALLOW_FAILURES=1` relaxes the 1% abort, and is set **only** for the concurrency
ramp, where driving the failure rate up is the measurement. The step below the
threshold is the reported answer; the steps above it are not published.

**What a failure meant in practice.** Two cells were excluded from the published
results and are kept with their evidence rather than deleted: a 1.18.2 cell
where the controller exited mid-run and left 95% of admissions unserved, and the
first main warm cells, which were measured in a configuration that disabled
their own cache.

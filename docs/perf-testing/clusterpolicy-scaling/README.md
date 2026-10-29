# ClusterPolicy scaling / regression harness

Measures legacy Kyverno `ClusterPolicy` admission latency and resource use across
Kyverno versions, on a single-machine KinD + KWOK + Prometheus rig. Reproduces the
methodology of the "Reproducing Regression (ClusterPolicy 1.13.6 vs 1.18.2)"
benchmark: 17 PSS ClusterPolicies, both ADMIT (compliant pod) and DENY (busybox
fails restricted PSS) paths, cells = {1,3} replicas × {100/1000, 200/5000,
500/10000} VUs/iterations, capturing k6 `iteration_duration` avg/max and Prometheus
max CPU / max memory per cell.

## Files

| File | Purpose |
|------|---------|
| `setup-mac.sh` | Build/teardown the KinD cluster (KWOK fake node, Prometheus, metrics-server). `CLUSTER=name`, `ACTION=delete`. |
| `scaling-run.sh` | One version's matrix: install Kyverno, apply the 17 policies, run every cell, capture per-cell latency + CPU/mem. `SKIP_INSTALL=1` reuses an existing install (the deny pass). |
| `ladder-run.sh` | **Multi-version shuffled round-robin.** Each round runs every version once in a fresh random order; fresh cluster per slot; 20-min cooldown between slots. |
| `ladder-report.py` | Aggregates the ladder: per (version, cell) geomean across rounds, reported relative to the `1.13.6` baseline, admit and deny separately. |
| `kyverno-scaling-values.yaml` | Kyverno control-plane config (1 replica base, side controllers off, no CPU/mem limits, relaxed probes, serviceMonitor on). |
| `script-admit.js` | Admit workload (restricted-PSS-compliant pod). Deny workload is `../../../scripts/k6/script.js` (violating busybox). |
| `policies/pss-clusterpolicies.yaml` | The pinned 17 PSS ClusterPolicies. |
| `k6-xk6` | k6 built with xk6-kubernetes (rebuild: `xk6 build --with github.com/grafana/xk6-kubernetes --output ./k6-xk6`). |

## The version ladder

Four released tags, all on the same k8s node image (`kindest/node:v1.31.6`) so the
Kyverno version is the only variable:

| Tag | Chart | Role |
|-----|-------|------|
| v1.13.6 | 3.3.9 | pre-regression baseline |
| v1.16.4 | 3.6.4 | just before the `copyContext` per-Checkpoint deep copy |
| v1.17.0 | 3.7.0 | `copyContext` deep copy (#14600) lands here — the suspected step |
| v1.19.1 | 3.9.1 | latest release — does the regression persist? |

Expectation: 1.17.0 / 1.19.1 sit on the same regressed plateau above 1.13.6, with
the step at 1.16.4 → 1.17.0. If 1.19.1 is materially better than the 1.17 plateau,
that's a surprise worth flagging. This ladder does **not** include the candidate
lazy-materialization fix (unreleased, branch
`perf/17507-lazy-checkpoint-materialization`) — that's a separate patched-build vs
v1.19.1 A/B to run later.

## Run it

```sh
# full ladder: 8 rounds × 4 versions × (admit + deny), 20-min cooldown between slots
bash ladder-run.sh

# reproducible / tuned
ROUNDS=10 SEED=42 COOLDOWN=1200 bash ladder-run.sh

# quick smoke (one cell, 1 replica, 2 rounds, short cooldown)
MATRIX="200:5000" REPLICAS="1" ROUNDS=2 COOLDOWN=60 bash ladder-run.sh

# aggregate
python3 ladder-report.py results/ladder --baseline 1.13.6
```

Flags: `VERSIONS` (app:chart list), `ROUNDS`, `SEED` (per-round shuffle =
`shuffle(SEED+round)`), `COOLDOWN` (per-slot, seconds), `KIND_IMAGE`, `REPLICAS`,
`MATRIX`, `RESULTS_ROOT`.

### Correction basis (changed from the old flow)

The previous two-version report took the geometric mean of a forward-order and
reverse-order ratio to cancel the second-run thermal penalty. The ladder replaces
that with a **shuffled round-robin**: no version is systematically first or last,
and `ladder-report.py` takes the geometric mean of each version's latency across
all rounds, reported relative to `1.13.6`. Admit is the headline path (the credible
one — all cells agreed); deny is corroboration.

### Caveats

- Single machine: magnitude is host-specific; the shape/delta across versions is
  what's meaningful. Cooldown + shuffle handle thermal/position drift.
- The ladder switches the current kube-context per slot (fresh cluster) — don't run
  other kubectl work against this machine while it runs.
- This directory was reconstructed from the working session after the earlier
  untracked copy was removed by a `git clean`. Commit it (or keep the backup) so it
  survives future cleans.

## Differential alloc pprof for 1.16.4 → 1.17.0 (`alloc-diff.sh`)

Isolates the `copyContext` deep-copy regression at the *allocation* level, not just
latency. The profiling block is identical across charts 3.6.4 / 3.7.0 / 3.9.1
(`admissionController.profiling.{enabled=false,port=6060}`), so one flag enables it
everywhere. For each version, `alloc-diff.sh` does: fresh cluster → install Kyverno
with pprof on → apply the 17 policies → drive a FIXED request count of the admit
load (both versions run the same count on a fresh install, so the cumulative heaps
are comparable) → capture heap → teardown; then diffs `alloc_objects`.

```sh
bash alloc-diff.sh                                   # 1.16.4 vs 1.17.0, 20000 admits
ALLOC_REQUESTS=40000 bash alloc-diff.sh              # more allocations per side
PAIR="1.13.6:3.3.9 1.17.0:3.7.0" bash alloc-diff.sh  # any base->comparison pair
```

Output: `results/alloc-diff/diff_1.16.4_to_1.17.0.txt`. The frame to watch grow is
`runtime.DeepCopyJSONValue` under
`github.com/kyverno/kyverno/pkg/engine/context.(*context).copyContext` (called from
`Checkpoint`) — the mechanism the ladder's latency step corroborates. The script
also prints any `copyContext` / `DeepCopyJSONValue` / `Checkpoint` lines from the
diff directly.

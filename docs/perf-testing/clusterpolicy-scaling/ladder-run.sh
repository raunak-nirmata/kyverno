#!/usr/bin/env bash
# Multi-version, shuffled round-robin ClusterPolicy regression ladder.
#
# Replaces the old two-version forward/reverse pairwise flow with an N-version
# round-robin: each ROUND runs every version once, in a fresh random order, and we
# do many rounds. Shuffling the position within each round (rather than a fixed
# forward+reverse pair) is what cancels ordering/thermal-position bias now; the
# aggregation (ladder-report.py) takes the geometric mean of each version's latency
# across all rounds and reports it relative to the v1.13.6 baseline.
#
# Every version slot gets its OWN fresh cluster (build -> deploy -> run -> teardown),
# same as before, so no version inherits another's warm caches or residue. After
# each slot the machine sleeps COOLDOWN seconds to thermally settle at the source,
# complementing the statistical correction.
#
# Methodology is unchanged from the doc's "Reproducing Regression" section: the same
# 17 PSS ClusterPolicies, KWOK fake nodes, both ADMIT (compliant pod) and DENY
# (busybox fails restricted PSS) paths, the same cells (1 and 3 replicas x
# 100/1000, 200/5000, 500/10000), capturing k6 iteration_duration avg+max and
# Prometheus max CPU / max memory per cell.
#
# Prereqs: host kind/kubectl/helm/jq/python3, and the xk6 build at ./k6-xk6
#   xk6 build --with github.com/grafana/xk6-kubernetes@v0.9.0 --output ./k6-xk6
#
# Usage:
#   bash ladder-run.sh                      # 8 rounds, 4 versions, 20-min cooldown
#   ROUNDS=10 SEED=42 bash ladder-run.sh    # reproducible 10-round run
#   MATRIX="200:5000" REPLICAS="1" ROUNDS=2 COOLDOWN=60 bash ladder-run.sh   # quick smoke
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
KYVERNO_REPO="$(cd "$HERE/../../.." && pwd)"

# app:chart pairs. Charts verified against `helm search repo kyverno/kyverno`.
#   1.13.6 pre-regression baseline | 1.16.4 just before copyContext deep-copy
#   1.17.0 copyContext deep-copy (#14600) lands | 1.19.1 latest release
VERSIONS="${VERSIONS:-1.13.6:3.3.9 1.16.4:3.6.4 1.17.0:3.7.0 1.19.1:3.9.1}"
BASELINE="${BASELINE:-1.13.6}"           # every version is reported relative to this
ROUNDS="${ROUNDS:-8}"
SEED="${SEED:-1337}"                      # per-round permutation = shuffle(SEED+round); reproducible
COOLDOWN="${COOLDOWN:-1200}"             # seconds to sleep after each slot's teardown (thermal settle)
KIND_IMAGE="${KIND_IMAGE:-kindest/node:v1.31.6}"   # SAME k8s for every version, so Kyverno version is the only variable
REPLICAS="${REPLICAS:-1 3}"
MATRIX="${MATRIX:-100:1000 200:5000 500:10000}"
CELL_COOLDOWN="${CELL_COOLDOWN:-120}"    # between cells within a matrix (passed to scaling-run.sh)
RESULTS_ROOT="${RESULTS_ROOT:-$HERE/results/ladder}"
DENY_SCRIPT="${DENY_SCRIPT:-$KYVERNO_REPO/scripts/k6/script.js}"   # busybox, no securityContext -> denied
ADMIT_SCRIPT="${ADMIT_SCRIPT:-$HERE/script-admit.js}"
K6="${K6:-$HERE/k6-xk6}"

[[ -x "$K6" ]] || { echo "FATAL: missing xk6 build: $K6"; echo "  build it: xk6 build --with github.com/grafana/xk6-kubernetes@v0.9.0 --output $K6"; exit 1; }
command -v python3 >/dev/null || { echo "FATAL: python3 required for the seeded shuffle"; exit 1; }

# shellcheck disable=SC2206
read -ra VER_ARR <<< "$VERSIONS"
NVER=${#VER_ARR[@]}
TOTAL_SLOTS=$(( ROUNDS * NVER ))
mkdir -p "$RESULTS_ROOT"
LOG="$RESULTS_ROOT/ladder.log"

echo "=== ClusterPolicy regression ladder ===" | tee -a "$LOG"
echo "versions : $VERSIONS" | tee -a "$LOG"
echo "rounds   : $ROUNDS   seed: $SEED   cooldown: ${COOLDOWN}s   k8s: $KIND_IMAGE" | tee -a "$LOG"
echo "matrix   : replicas={$REPLICAS} cells={$MATRIX}   paths: admit + deny" | tee -a "$LOG"
echo "baseline : $BASELINE (all versions reported relative to it)" | tee -a "$LOG"
echo "results  : $RESULTS_ROOT" | tee -a "$LOG"
echo | tee -a "$LOG"

# Optional map of local builds: "name:worktree:tag ...". A version whose app name
# appears here installs from its in-repo chart + local ko images (see scaling-run.sh
# LOCAL_WT/LOCAL_TAG) instead of the released helm chart. Used for the fix A/B where
# both "parent" (fix OFF) and "fix" (fix ON) are local builds off the same 1.20-dev base.
LOCAL_BUILDS="${LOCAL_BUILDS:-}"

# Echo "worktree tag" for a local-build app name, or nothing for a released version.
local_build_for() {
  local want="$1" spec name rest
  for spec in $LOCAL_BUILDS; do
    name="${spec%%:*}"; rest="${spec#*:}"
    if [[ "$name" == "$want" ]]; then echo "${rest%%:*} ${rest##*:}"; return; fi
  done
}

# One version slot: fresh cluster -> admit matrix -> deny matrix -> teardown.
run_slot() {
  local round="$1" app="$2" chart="$3"
  local cluster="ladder-$(echo "$app" | tr '.' '-')-r${round}"
  local base="$RESULTS_ROOT/round${round}/${app}"

  # Resume: if this slot already has complete admit AND deny results, skip it.
  # A partial slot (killed mid-matrix) is wiped and redone cleanly. This makes the
  # ladder restartable — if a run is interrupted, just launch it again and it
  # continues where it left off (same seed -> same per-round order).
  local expected have_a have_d
  expected=$(( $(echo $REPLICAS | wc -w) * $(echo $MATRIX | wc -w) ))
  # `|| true` so an empty glob (ls exits non-zero, e.g. a slot with no deny cells
  # yet) doesn't fail the pipeline under set -o pipefail and kill the whole run.
  have_a=$(ls "$base"/admit/*/k6-summary.json 2>/dev/null | wc -l | tr -d ' ' || true)
  have_d=$(ls "$base"/deny/*/k6-summary.json 2>/dev/null | wc -l | tr -d ' ' || true)
  SLOT_SKIPPED=0
  if [[ "$have_a" -ge "$expected" && "$have_d" -ge "$expected" ]]; then
    echo ">>> round $round | $app — already complete ($have_a admit / $have_d deny cells), SKIP" | tee -a "$LOG"
    SLOT_SKIPPED=1   # no cooldown after a no-op skip (only real slots cool down)
    return
  fi
  rm -rf "$base/admit" "$base/deny" 2>/dev/null || true

  mkdir -p "$base"
  echo ">>> round $round | version $app (chart $chart) | cluster $cluster | $(date -u +%H:%M:%SZ)" | tee -a "$LOG"

  echo "  [build] fresh cluster" | tee -a "$LOG"
  CLUSTER="$cluster" KIND_IMAGE="$KIND_IMAGE" bash "$HERE/setup-mac.sh" >>"$LOG" 2>&1

  # Local build? Resolve worktree+tag so scaling-run.sh installs the in-repo chart.
  local lb lwt ltag
  lb="$(local_build_for "$app")"; lwt="${lb%% *}"; ltag="${lb##* }"
  [[ -n "$lb" ]] && echo "  [local build] $app -> ${ltag:0:12} ($lwt)" | tee -a "$LOG"

  echo "  [admit] matrix" | tee -a "$LOG"
  KYVERNO_VERSION="$app" KYVERNO_CHART="$chart" \
    LOCAL_WT="$lwt" LOCAL_TAG="$ltag" \
    SCRIPT_JS="$ADMIT_SCRIPT" READY_KIND=cpol POLICY_MODE=validate \
    REPLICAS="$REPLICAS" MATRIX="$MATRIX" COOLDOWN="$CELL_COOLDOWN" K6="$K6" \
    RESULTS_DIR="$base/admit" \
    bash "$HERE/scaling-run.sh" >>"$LOG" 2>&1 || echo "  WARN: admit matrix returned non-zero (see log)" | tee -a "$LOG"

  echo "  [deny] matrix (reuse install)" | tee -a "$LOG"
  SKIP_INSTALL=1 KYVERNO_VERSION="$app" KYVERNO_CHART="$chart" \
    SCRIPT_JS="$DENY_SCRIPT" READY_KIND=cpol POLICY_MODE=validate \
    REPLICAS="$REPLICAS" MATRIX="$MATRIX" COOLDOWN="$CELL_COOLDOWN" K6="$K6" \
    RESULTS_DIR="$base/deny" \
    bash "$HERE/scaling-run.sh" >>"$LOG" 2>&1 || echo "  WARN: deny matrix returned non-zero (see log)" | tee -a "$LOG"

  echo "  [teardown]" | tee -a "$LOG"
  CLUSTER="$cluster" ACTION=delete bash "$HERE/setup-mac.sh" >>"$LOG" 2>&1 || true
  { echo "app=$app"; echo "chart=$chart"; echo "round=$round"; echo "k8s=$KIND_IMAGE"; echo "cluster=$cluster"; } > "$base/slot-meta.txt"
}

slot=0
for (( r=1; r<=ROUNDS; r++ )); do
  # fresh random permutation of the versions for this round (reproducible via SEED)
  perm=$(python3 -c "
import random,sys
r,s=int(sys.argv[1]),int(sys.argv[2]); items=sys.argv[3:]
rnd=random.Random(s+r); rnd.shuffle(items); print(' '.join(items))" "$r" "$SEED" "${VER_ARR[@]}")
  echo "== round $r/$ROUNDS  order: $perm ==" | tee -a "$LOG"
  for vc in $perm; do
    slot=$(( slot + 1 ))
    app="${vc%%:*}"; chart="${vc##*:}"
    run_slot "$r" "$app" "$chart"
    if (( slot < TOTAL_SLOTS )) && [[ "${SLOT_SKIPPED:-0}" != "1" ]]; then
      echo "  [cooldown] ${COOLDOWN}s ($(date -u +%H:%M:%SZ)) — slot $slot/$TOTAL_SLOTS done" | tee -a "$LOG"
      sleep "$COOLDOWN"
    fi
  done
done

echo | tee -a "$LOG"
echo "=== ladder complete ($TOTAL_SLOTS slots). Aggregate: ===" | tee -a "$LOG"
echo "python3 $HERE/ladder-report.py $RESULTS_ROOT" | tee -a "$LOG"

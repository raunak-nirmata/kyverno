#!/usr/bin/env bash
# Multi-version, shuffled round-robin ivpol scaling ladder.
#
# Same methodology as the ClusterPolicy ladder next door: every version slot gets
# its own fresh cluster, each round runs every version once in a fresh seeded
# random order, and the machine sleeps COOLDOWN between slots to settle. No
# version is systematically first or last, so a thermal or cache-warming trend
# in the host cannot be read as a difference between versions.
#
# It duplicates ~40 lines of that shuffle/resume loop rather than importing it.
# The slot bodies differ enough - an admit+deny matrix over replicas x VUs there,
# an images x latency matrix with a signed-image registry here - that threading
# both through one script would make both harder to follow. Accepted duplication,
# not a pattern to repeat.
#
# The registry is rebuilt per slot because the cluster is. That is ~16 scratch
# image builds and signatures, well under a minute, and it keeps each slot
# genuinely independent.
#
# Usage:
#   bash ladder-ivpol-run.sh
#   ROUNDS=1 IMAGES="1 8" DELAYS="25" bash ladder-ivpol-run.sh     # smoke
#   VERSIONS="1.18.2:3.8.2 1.19.0:3.9.0 1.19.1:3.9.1" bash ladder-ivpol-run.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SIBLING="$HERE/../clusterpolicy-scaling"
KYVERNO_REPO="$(cd "$HERE/../../.." && pwd)"

# The two versions that isolate the serial-prefetch change (#17313): it is in
# 1.19.1 and not in 1.19.0, and nothing else relevant to the image path differs
# between them. 1.18.2 is a separate, explicitly non-attributable comparison -
# see README - and is added by overriding VERSIONS.
# Entries are app:chart. A chart of "local" means there is no published chart
# for that version - release candidates - and fetch-chart.sh extracts one from
# the matching git tag, with the image tag overridden explicitly.
VERSIONS="${VERSIONS:-1.19.0:3.9.0 1.19.1:3.9.1}"
BASELINE="${BASELINE:-1.19.0}"
ROUNDS="${ROUNDS:-3}"
SEED="${SEED:-1337}"
COOLDOWN="${COOLDOWN:-300}"
KIND_IMAGE="${KIND_IMAGE:-kindest/node:v1.31.6}"
IMAGES="${IMAGES:-1 2 4 8 16}"
DELAYS="${DELAYS:-0 25 100}"
CACHE_MODE="${CACHE_MODE:-off}"
VUS="${VUS:-20}"
# Duration per cell, so every cell in the matrix measures over the same
# window regardless of how expensive it is. See ivpol-run.sh.
LOAD="${LOAD:-60s}"
CELL_COOLDOWN="${CELL_COOLDOWN:-60}"
IMAGE_COUNT="${IMAGE_COUNT:-16}"
RESULTS_ROOT="${RESULTS_ROOT:-$HERE/results/ladder}"
K6="${K6:-$SIBLING/k6-xk6}"

[[ -x "$K6" ]] || { echo "FATAL: missing xk6 build: $K6"; exit 1; }
command -v python3 >/dev/null || { echo "FATAL: python3 required for the seeded shuffle"; exit 1; }
command -v cosign  >/dev/null || { echo "FATAL: cosign required to sign benchmark images"; exit 1; }

# shellcheck disable=SC2206
read -ra VER_ARR <<< "$VERSIONS"
NVER=${#VER_ARR[@]}
TOTAL_SLOTS=$(( ROUNDS * NVER ))
mkdir -p "$RESULTS_ROOT"
LOG="$RESULTS_ROOT/ladder.log"

expected_cells() {
  local n=0
  for _ in $DELAYS; do for _ in $IMAGES; do n=$(( n + 1 )); done; done
  echo "$n"
}
EXPECTED_CELLS="$(expected_cells)"

run_slot() {
  local round="$1" app="$2" chart="$3"
  local base="$RESULTS_ROOT/round${round}/${app}"
  local cluster="ivpol-${app//./-}-r${round}"
  SLOT_SKIPPED=0

  # Resume: a slot with a full set of cells is skipped; a partial one (killed
  # mid-matrix) is wiped and redone, so an interrupted ladder never leaves a
  # half-measured version in the aggregate.
  local have
  have=$(ls -d "$base"/img*-d*/meta.txt 2>/dev/null | wc -l | tr -d ' ' || true)
  if [[ "$have" == "$EXPECTED_CELLS" ]]; then
    echo "  [skip] round $round $app — $have/$EXPECTED_CELLS cells already present" | tee -a "$LOG"
    SLOT_SKIPPED=1
    return
  fi
  [[ "$have" != "0" ]] && { echo "  [redo] round $round $app — partial ($have/$EXPECTED_CELLS), wiping" | tee -a "$LOG"; rm -rf "$base"; }
  mkdir -p "$base"

  echo "  [slot] round $round $app (chart $chart) cluster=$cluster" | tee -a "$LOG"
  CLUSTER="$cluster" KIND_IMAGE="$KIND_IMAGE" bash "$SIBLING/setup-mac.sh" >>"$LOG" 2>&1

  # Registry first: the policy's public key and the signed images must exist
  # before Kyverno's controls run, or the positive control fails and the slot
  # aborts for a reason that has nothing to do with the version.
  IMAGE_COUNT="$IMAGE_COUNT" KIND_CLUSTER="$cluster" bash "$HERE/registry/setup-registry.sh" >>"$LOG" 2>&1

  # A version with no published chart is installed from the repo at its tag.
  local chart_path="" image_tag="" image_registry=""
  if [[ "$chart" == "local" ]]; then
    chart_path="$(bash "$HERE/fetch-chart.sh" "v$app")" \
      || { echo "FATAL: could not fetch chart for v$app" | tee -a "$LOG"; exit 1; }
    image_tag="v$app"
    image_registry="${LOCAL_CHART_REGISTRY:-ghcr.io}"
    echo "    local chart: $chart_path (image $image_registry/kyverno/kyverno:v$app)" | tee -a "$LOG"
  fi

  KYVERNO_VERSION="$app" KYVERNO_CHART="$chart" \
  KYVERNO_CHART_PATH="$chart_path" KYVERNO_IMAGE_TAG="$image_tag" \
  KYVERNO_IMAGE_REGISTRY="$image_registry" \
  IMAGES="$IMAGES" DELAYS="$DELAYS" CACHE_MODE="$CACHE_MODE" \
  VUS="$VUS" LOAD="$LOAD" CELL_COOLDOWN="$CELL_COOLDOWN" \
  RESULTS_DIR="$base" \
    bash "$HERE/ivpol-run.sh" >>"$LOG" 2>&1

  CLUSTER="$cluster" ACTION=delete bash "$SIBLING/setup-mac.sh" >>"$LOG" 2>&1 || true
  { echo "app=$app"; echo "chart=$chart"; echo "round=$round"
    echo "k8s=$KIND_IMAGE"; echo "cluster=$cluster"
    echo "cache_mode=$CACHE_MODE"; echo "image_count=$IMAGE_COUNT"
  } > "$base/slot-meta.txt"
}

echo "=== ivpol ladder: $NVER versions x $ROUNDS rounds = $TOTAL_SLOTS slots ===" | tee -a "$LOG"
echo "    cells/slot: $EXPECTED_CELLS   images: $IMAGES   delays: $DELAYS   cache: $CACHE_MODE" | tee -a "$LOG"

slot=0
for r in $(seq 1 "$ROUNDS"); do
  # Seeded per-round permutation, so the order is random across rounds but
  # reproducible across reruns of the same SEED.
  order=$(python3 -c "import random,sys
r=int(sys.argv[1]); s=int(sys.argv[2]); items=sys.argv[3:]
rnd=random.Random(s+r); rnd.shuffle(items); print(' '.join(items))" "$r" "$SEED" "${VER_ARR[@]}")
  echo "[round $r] order: $order" | tee -a "$LOG"
  for pair in $order; do
    slot=$(( slot + 1 ))
    run_slot "$r" "${pair%%:*}" "${pair##*:}"
    if (( slot < TOTAL_SLOTS )) && [[ "${SLOT_SKIPPED:-0}" != "1" ]]; then
      echo "  [cooldown] ${COOLDOWN}s ($(date -u +%H:%M:%SZ)) — slot $slot/$TOTAL_SLOTS done" | tee -a "$LOG"
      sleep "$COOLDOWN"
    fi
  done
done

echo "=== ladder complete ($TOTAL_SLOTS slots) ===" | tee -a "$LOG"
echo "aggregate: python3 $HERE/ivpol-report.py $RESULTS_ROOT --baseline $BASELINE" | tee -a "$LOG"

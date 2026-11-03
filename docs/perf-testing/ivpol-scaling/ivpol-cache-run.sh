#!/usr/bin/env bash
# Cache-state sweep: what the image verification cache is worth, and what it
# costs you to throw it away.
#
# Three states at one fixed pod shape:
#
#   off       cache disabled entirely — the floor, every request verifies
#   warm      cache enabled and primed — the best case steady state
#   postedit  cache enabled, primed, then the policy is annotated
#
# The third is the point. The cache key includes the policy's resourceVersion
# (pkg/image/verification/cache/client.go, generateKey), so *any* edit to a
# policy invalidates every entry belonging to it. A routine annotation - not a
# change to what the policy does - silently puts a busy cluster back on the cold
# path. Operators do not expect that, and it is the kind of thing that turns a
# quiet Tuesday into an incident.
#
# Each state is a separate Kyverno install, because the cache is a controller
# flag. ivpol-run.sh does the install, the gates and the archiving; this script
# only decides which states to visit and in what order.
#
# Usage:
#   bash ivpol-cache-run.sh
#   VERSIONS="1.19.1:3.9.1" ROUNDS=1 bash ivpol-cache-run.sh      # smoke
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SIBLING="$HERE/../clusterpolicy-scaling"

VERSIONS="${VERSIONS:-1.19.0:3.9.0 1.19.1:3.9.1}"
MODES="${MODES:-off warm postedit}"
ROUNDS="${ROUNDS:-2}"
SEED="${SEED:-1337}"
COOLDOWN="${COOLDOWN:-300}"
KIND_IMAGE="${KIND_IMAGE:-kindest/node:v1.31.6}"
# Fixed shape: enough images that the cache has real work to save, and enough
# latency that saving it is visible.
IMAGES="${IMAGES:-8}"
DELAYS="${DELAYS:-25}"
VUS="${VUS:-20}"
LOAD="${LOAD:-60s}"
CELL_COOLDOWN="${CELL_COOLDOWN:-60}"
IMAGE_COUNT="${IMAGE_COUNT:-16}"
RESULTS_ROOT="${RESULTS_ROOT:-$HERE/results/cache}"

read -ra VER_ARR <<< "$VERSIONS"
mkdir -p "$RESULTS_ROOT"
LOG="$RESULTS_ROOT/cache.log"

echo "=== cache sweep: ${#VER_ARR[@]} versions x $ROUNDS rounds x $(echo "$MODES" | wc -w | tr -d ' ') modes ===" | tee -a "$LOG"

slot=0
for r in $(seq 1 "$ROUNDS"); do
  order=$(python3 -c "import random,sys
r=int(sys.argv[1]); s=int(sys.argv[2]); items=sys.argv[3:]
rnd=random.Random(s+r); rnd.shuffle(items); print(' '.join(items))" "$r" "$SEED" "${VER_ARR[@]}")
  echo "[round $r] order: $order" | tee -a "$LOG"
  for pair in $order; do
    app="${pair%%:*}"; chart="${pair##*:}"
    base="$RESULTS_ROOT/round${r}/${app}"
    cluster="ivpolc-${app//./-}-r${r}"
    slot=$(( slot + 1 ))

    if [[ -f "$base/.complete" ]]; then
      echo "  [skip] round $r $app already complete" | tee -a "$LOG"
      continue
    fi
    rm -rf "$base"; mkdir -p "$base"

    echo "  [slot] round $r $app cluster=$cluster" | tee -a "$LOG"
    CLUSTER="$cluster" KIND_IMAGE="$KIND_IMAGE" bash "$SIBLING/setup-mac.sh" >>"$LOG" 2>&1
    IMAGE_COUNT="$IMAGE_COUNT" KIND_CLUSTER="$cluster" bash "$HERE/registry/setup-registry.sh" >>"$LOG" 2>&1

    for mode in $MODES; do
      echo "    [mode] $mode" | tee -a "$LOG"
      # No SKIP_INSTALL: each mode needs its own install, because enabling or
      # disabling the cache is a controller flag, and a warm cell needs a
      # controller that started with the cache on.
      KYVERNO_VERSION="$app" KYVERNO_CHART="$chart" \
      IMAGES="$IMAGES" DELAYS="$DELAYS" CACHE_MODE="$mode" \
      VUS="$VUS" LOAD="$LOAD" CELL_COOLDOWN="$CELL_COOLDOWN" \
      RESULTS_DIR="$base" \
        bash "$HERE/ivpol-run.sh" >>"$LOG" 2>&1
    done

    touch "$base/.complete"
    { echo "app=$app"; echo "chart=$chart"; echo "round=$r"
      echo "modes=$MODES"; echo "images=$IMAGES"; echo "delay_ms=$DELAYS"
    } > "$base/slot-meta.txt"

    CLUSTER="$cluster" ACTION=delete bash "$SIBLING/setup-mac.sh" >>"$LOG" 2>&1 || true
    echo "  [cooldown] ${COOLDOWN}s" | tee -a "$LOG"
    sleep "$COOLDOWN"
  done
done

echo "=== cache sweep complete ===" | tee -a "$LOG"
echo "aggregate: python3 $HERE/ivpol-report.py $RESULTS_ROOT --baseline ${VER_ARR[0]%%:*}" | tee -a "$LOG"

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
# The registry is rebuilt per slot because the cluster is. With the defaults
# below that is roughly 100 scratch image builds and signatures - IMAGE_COUNT
# images per signed-image set (key, cert-chain, bundle, keyless, ...), not
# IMAGE_COUNT total - well under a minute, and it keeps each slot genuinely
# independent.
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
# Whether MODES was handed to us at all, captured before the default below
# fills it in - this is what lets a bare run keep writing cells straight into
# the slot directory the way every archived ladder result already does,
# instead of nesting them one level under a "key" subdirectory nobody asked for.
MODES_WAS_SET="${MODES+1}"
VERSIONS="${VERSIONS:-1.19.0:3.9.0 1.19.1:3.9.1}"
BASELINE="${BASELINE:-1.19.0}"
ROUNDS="${ROUNDS:-3}"
SEED="${SEED:-1337}"
COOLDOWN="${COOLDOWN:-300}"
KIND_IMAGE="${KIND_IMAGE:-kindest/node:v1.31.6}"
IMAGES="${IMAGES:-1 2 4 8 16}"
DELAYS="${DELAYS:-0 25 100}"
CACHE_MODE="${CACHE_MODE:-off}"
# Signature formats to run per slot, space-separated. Each one is a separate
# sub-sweep - its own cells, its own meta.txt mode= field - because a version
# difference that only shows up in the bundle path is the one the plain
# key-only sweep has always been blind to.
MODES="${MODES:-key}"
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
read -ra MODE_ARR <<< "$MODES"
NVER=${#VER_ARR[@]}
NMODES=${#MODE_ARR[@]}
TOTAL_SLOTS=$(( ROUNDS * NVER ))
mkdir -p "$RESULTS_ROOT"
LOG="$RESULTS_ROOT/ladder.log"

expected_cells() {
  local n=0
  for _ in $DELAYS; do for _ in $IMAGES; do n=$(( n + 1 )); done; done
  echo "$n"
}
EXPECTED_CELLS="$(expected_cells)"
# Per-mode cell count, times the number of modes this slot has to cover - the
# banner's job is to say how many cells the whole slot is, not just one mode's.
EXPECTED_CELLS_TOTAL=$(( EXPECTED_CELLS * NMODES ))

run_slot() {
  local round="$1" app="$2" chart="$3" imageref="${4:-}"
  local base="$RESULTS_ROOT/round${round}/${app}"
  local cluster="ivpol-${app//./-}-r${round}"
  SLOT_SKIPPED=0

  # Legacy layout (MODES unset): cells live straight under $base, exactly as
  # before. Multi-mode layout: each mode gets its own subdirectory under
  # $base, so a "bundle-key" cell and a "key" cell with the same img/delay
  # never collide on the same meta.txt.
  local -a mode_dirs=()
  if [[ -z "$MODES_WAS_SET" ]]; then
    mode_dirs=("$base")
  else
    local m
    # ${arr[@]+"${arr[@]}"}: bash 3.2 under set -u errors iterating an empty
    # array otherwise (MODES="" would hit this).
    for m in ${MODE_ARR[@]+"${MODE_ARR[@]}"}; do mode_dirs+=("$base/$m"); done
  fi

  # Resume is per mode-directory: a slot where "key" is done but "bundle-key"
  # is not re-runs only the missing mode, rather than wiping a mode that
  # already has a full, valid set of cells.
  local all_skip=1 any_present=0 d have
  for d in ${mode_dirs[@]+"${mode_dirs[@]}"}; do
    have=$(ls -d "$d"/img*-d*/meta.txt 2>/dev/null | wc -l | tr -d ' ' || true)
    [[ "$have" != "0" ]] && any_present=1
    [[ "$have" != "$EXPECTED_CELLS" ]] && all_skip=0
  done
  if [[ "$all_skip" == "1" ]]; then
    echo "  [skip] round $round $app — all modes already present" | tee -a "$LOG"
    SLOT_SKIPPED=1
    return
  fi

  echo "  [slot] round $round $app (chart $chart) cluster=$cluster" | tee -a "$LOG"
  CLUSTER="$cluster" KIND_IMAGE="$KIND_IMAGE" bash "$SIBLING/setup-mac.sh" >>"$LOG" 2>&1

  # Registry first: the policy's public key and the signed images must exist
  # before Kyverno's controls run, or the positive control fails and the slot
  # aborts for a reason that has nothing to do with the version.
  IMAGE_COUNT="$IMAGE_COUNT" KIND_CLUSTER="$cluster" bash "$HERE/registry/setup-registry.sh" >>"$LOG" 2>&1

  # A version with no published chart is installed from the repo at its tag.
  # An explicit imageref (3rd VERSIONS field, e.g. for "main", which has no
  # "vmain" git tag for fetch-chart.sh to find) takes the opposite path: the
  # published chart is used as-is and only the image is overridden, so
  # fetch-chart.sh is never called at all.
  local chart_path="" image_tag="" image_registry="" slot_extra_helm_args=""
  if [[ -n "$imageref" ]]; then
    # imageref is a full reference, e.g. ghcr.io/kyverno/kyverno@sha256:<hex>
    # or ghcr.io/kyverno/kyverno:main. Split off the registry host (before the
    # first "/") and pass the remainder through as the tag: the chart's image
    # helper only ever appends ":$tag" after "$registry/$repository", so a
    # remainder of "kyverno/kyverno@sha256:<hex>" renders as
    # "ghcr.io/kyverno/kyverno:kyverno/kyverno@sha256:<hex>" unless repository
    # is also overridden below when it differs from the chart's default.
    image_registry="${imageref%%/*}"
    local image_rest="${imageref#*/}"
    # image_rest is "<repo-path><separator><tag-or-digest>", e.g.
    # "kyverno/kyverno@sha256:<hex>" or "kyverno/kyverno:main". The chart's
    # default repository is kyverno/kyverno; anything else needs its own
    # override, since KYVERNO_IMAGE_REGISTRY/TAG alone can't express a
    # different repository path.
    local image_repo
    if [[ "$image_rest" == *@* ]]; then
      # Digest form. Packed into the tag field rather than kept as a separate
      # digest field: the helper always renders "repository:$tag", and
      # Kubernetes accepts "name:tag@digest" as a valid reference (the digest
      # wins resolution, the tag is cosmetic), so a tag value of
      # "pinned@sha256:<hex>" produces a correctly digest-pinned image through
      # the existing registry/tag override alone, with no chart change.
      image_repo="${image_rest%%@*}"
      image_tag="pinned@${image_rest#*@}"
    else
      # Plain tag form. Only one tag fits in "name:tag" - unlike the digest
      # case, there is no room for a cosmetic prefix here.
      image_repo="${image_rest%%:*}"
      image_tag="${image_rest#*:}"
    fi
    # Only kyverno/kyverno (the chart default) is exercised by this harness in
    # practice; anything else is threaded through EXTRA_HELM_ARGS, the
    # existing per-run chart-override knob, rather than inventing a new one.
    slot_extra_helm_args=""
    [[ "$image_repo" != "kyverno/kyverno" ]] \
      && slot_extra_helm_args="--set admissionController.container.image.repository=$image_repo"
    echo "    explicit image: $imageref (registry=$image_registry tag=$image_tag)" | tee -a "$LOG"
  elif [[ "$chart" == "local" ]]; then
    chart_path="$(bash "$HERE/fetch-chart.sh" "v$app")" \
      || { echo "FATAL: could not fetch chart for v$app" | tee -a "$LOG"; exit 1; }
    image_tag="v$app"
    image_registry="${LOCAL_CHART_REGISTRY:-ghcr.io}"
    echo "    local chart: $chart_path (image $image_registry/kyverno/kyverno:v$app)" | tee -a "$LOG"
  fi

  local i mode mdir
  for i in "${!mode_dirs[@]}"; do
    mdir="${mode_dirs[$i]}"
    mode=""
    [[ -n "$MODES_WAS_SET" ]] && mode="${MODE_ARR[$i]}"

    have=$(ls -d "$mdir"/img*-d*/meta.txt 2>/dev/null | wc -l | tr -d ' ' || true)
    if [[ "$have" == "$EXPECTED_CELLS" ]]; then
      echo "    [skip mode] ${mode:-default} — $have/$EXPECTED_CELLS cells already present" | tee -a "$LOG"
      continue
    fi
    [[ "$have" != "0" ]] && { echo "    [redo mode] ${mode:-default} — partial ($have/$EXPECTED_CELLS), wiping" | tee -a "$LOG"; rm -rf "$mdir"; }
    mkdir -p "$mdir"

    # MODE is only set on the child when the operator opted into MODES: an
    # unset MODES must honour POLICY_TEMPLATE/REPO exactly as ivpol-run.sh
    # did before this flag existed, not silently default them to "key".
    MODE="$mode" \
    KYVERNO_VERSION="$app" KYVERNO_CHART="$chart" \
    KYVERNO_CHART_PATH="$chart_path" KYVERNO_IMAGE_TAG="$image_tag" \
    KYVERNO_IMAGE_REGISTRY="$image_registry" \
    EXTRA_HELM_ARGS="${EXTRA_HELM_ARGS:-} $slot_extra_helm_args" \
    IMAGES="$IMAGES" DELAYS="$DELAYS" CACHE_MODE="$CACHE_MODE" \
    VUS="$VUS" LOAD="$LOAD" CELL_COOLDOWN="$CELL_COOLDOWN" \
    RESULTS_DIR="$mdir" \
      bash "$HERE/ivpol-run.sh" >>"$LOG" 2>&1
  done

  CLUSTER="$cluster" ACTION=delete bash "$SIBLING/setup-mac.sh" >>"$LOG" 2>&1 || true
  # Resolved image and imageID digest, read back from whichever mode ran last
  # (every mode in a slot installs the same image) - this is what makes a
  # tag-based or digest-pinned slot (main, or an RC) reproducible after the
  # tag has moved on, which is the whole point of accepting an imageref here.
  local resolved_image="" resolved_image_id=""
  if [[ ${#mode_dirs[@]} -gt 0 ]]; then
    # Bash 3.2 has no negative array indices.
    local last_mdir="${mode_dirs[$(( ${#mode_dirs[@]} - 1 ))]}"
    resolved_image=$(cat "$last_mdir/running-image.txt" 2>/dev/null)
    resolved_image_id=$(cat "$last_mdir/running-image-id.txt" 2>/dev/null)
  fi
  { echo "app=$app"; echo "chart=$chart"; echo "round=$round"
    echo "k8s=$KIND_IMAGE"; echo "cluster=$cluster"
    echo "cache_mode=$CACHE_MODE"; echo "image_count=$IMAGE_COUNT"
    echo "modes=${MODES}"
    echo "imageref=${imageref}"
    echo "resolved_image=${resolved_image}"
    echo "resolved_image_id=${resolved_image_id}"
  } > "$base/slot-meta.txt"
}

echo "=== ivpol ladder: $NVER versions x $ROUNDS rounds = $TOTAL_SLOTS slots ===" | tee -a "$LOG"
echo "    cells/slot: $EXPECTED_CELLS_TOTAL ($EXPECTED_CELLS x $NMODES modes)   images: $IMAGES   delays: $DELAYS   cache: $CACHE_MODE   modes: $MODES" | tee -a "$LOG"

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
    # Entries are app:chart or app:chart:imageref. Parsed by peeling off the
    # first colon-delimited field at a time, not by splitting on every colon,
    # since imageref itself routinely contains one (a port, or sha256:<hex>
    # in a digest) - plain "${pair##*:}" would have cut a 3-field entry's
    # chart wrong and truncated its imageref at the last colon.
    entry_app="${pair%%:*}"
    entry_rest="${pair#*:}"
    if [[ "$entry_rest" == *:* ]]; then
      entry_chart="${entry_rest%%:*}"
      entry_imageref="${entry_rest#*:}"
    else
      entry_chart="$entry_rest"
      entry_imageref=""
    fi
    run_slot "$r" "$entry_app" "$entry_chart" "$entry_imageref"
    if (( slot < TOTAL_SLOTS )) && [[ "${SLOT_SKIPPED:-0}" != "1" ]]; then
      echo "  [cooldown] ${COOLDOWN}s ($(date -u +%H:%M:%SZ)) — slot $slot/$TOTAL_SLOTS done" | tee -a "$LOG"
      sleep "$COOLDOWN"
    fi
  done
done

echo "=== ladder complete ($TOTAL_SLOTS slots) ===" | tee -a "$LOG"
echo "aggregate: python3 $HERE/ivpol-report.py $RESULTS_ROOT --baseline $BASELINE" | tee -a "$LOG"

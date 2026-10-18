#!/usr/bin/env bash
# Concurrency ramp: how many admissions per second before image verification
# stops keeping up. The literal "will it scale" number.
#
# Raises VUs a step at a time against a fixed pod shape until requests start
# failing, then stops. The failure mode is the policy's
# webhookConfiguration.timeoutSeconds - a request that waits too long on
# registry round trips and crypto is rejected outright, because failurePolicy is
# Fail. That is why timeoutSeconds is pinned in the policy rather than left to
# the chart default: an unpinned value would move the cliff between versions and
# the chart would be comparing defaults, not engines.
#
# ALLOW_FAILURES=1 is set deliberately. Every other sweep aborts a cell whose
# failure rate exceeds 1%, because there a failure means the measurement is
# wrong. Here, driving the failure rate up *is* the measurement - but the cell
# before the threshold is the answer, not the cells after it.
#
# Usage:
#   bash ivpol-ramp-run.sh
#   STEPS="50 100 200" ROUNDS=1 bash ivpol-ramp-run.sh     # smoke
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SIBLING="$HERE/../clusterpolicy-scaling"

# One version: this is a capacity question, not a comparison. 1.19.1 because
# that is what someone deploying today would run.
KYVERNO_VERSION="${KYVERNO_VERSION:-1.19.1}"
KYVERNO_CHART="${KYVERNO_CHART:-3.9.1}"
STEPS="${STEPS:-50 100 200 400 800}"
ROUNDS="${ROUNDS:-2}"
COOLDOWN="${COOLDOWN:-300}"
KIND_IMAGE="${KIND_IMAGE:-kindest/node:v1.31.6}"
IMAGES="${IMAGES:-8}"
DELAYS="${DELAYS:-25}"
# Warm, because a production cluster at steady state is warm. A cold ramp would
# measure the registry's capacity as much as Kyverno's.
CACHE_MODE="${CACHE_MODE:-warm}"
LOAD="${LOAD:-60s}"
CELL_COOLDOWN="${CELL_COOLDOWN:-60}"
IMAGE_COUNT="${IMAGE_COUNT:-16}"
# Stop once more than this fraction of requests fail. The last step below it is
# the reportable capacity.
FAIL_THRESHOLD="${FAIL_THRESHOLD:-0.01}"
RESULTS_ROOT="${RESULTS_ROOT:-$HERE/results/ramp}"

mkdir -p "$RESULTS_ROOT"
LOG="$RESULTS_ROOT/ramp.log"

echo "=== concurrency ramp: $KYVERNO_VERSION, steps [$STEPS], $ROUNDS round(s) ===" | tee -a "$LOG"

for r in $(seq 1 "$ROUNDS"); do
  base="$RESULTS_ROOT/round${r}"
  cluster="ivpolr-r${r}"
  if [[ -f "$base/.complete" ]]; then
    echo "[skip] round $r already complete" | tee -a "$LOG"
    continue
  fi
  rm -rf "$base"; mkdir -p "$base"

  echo "[round $r] cluster=$cluster" | tee -a "$LOG"
  CLUSTER="$cluster" KIND_IMAGE="$KIND_IMAGE" bash "$SIBLING/setup-mac.sh" >>"$LOG" 2>&1
  IMAGE_COUNT="$IMAGE_COUNT" KIND_CLUSTER="$cluster" bash "$HERE/registry/setup-registry.sh" >>"$LOG" 2>&1

  saturated=""
  crashed=""
  first=1
  for vus in $STEPS; do
    echo "  [step] ${vus} VUs" | tee -a "$LOG"
    step_dir="$base/vus${vus}"
    mkdir -p "$step_dir"

    # Install once, on the first step. Later steps reuse it: reinstalling would
    # reset the cache the ramp is deliberately running warm, and would add a
    # cold-start cost to every step that has nothing to do with capacity.
    skip=""
    [[ "$first" == "1" ]] || skip=1
    first=0

    KYVERNO_VERSION="$KYVERNO_VERSION" KYVERNO_CHART="$KYVERNO_CHART" \
    IMAGES="$IMAGES" DELAYS="$DELAYS" CACHE_MODE="$CACHE_MODE" \
    VUS="$vus" LOAD="$LOAD" CELL_COOLDOWN="$CELL_COOLDOWN" \
    SKIP_INSTALL="$skip" ALLOW_FAILURES=1 \
    RESULTS_DIR="$step_dir" \
      bash "$HERE/ivpol-run.sh" >>"$LOG" 2>&1

    # The cell archives its own failure rate and restart count; read both back.
    # restarts.txt is this step's own delta since its t0 (ivpol-run.sh samples
    # it per cell now), so no cross-step bookkeeping is needed here any more —
    # a nonzero value already means this step, not an earlier one, crashed it.
    rate_file=$(find "$step_dir" -name admission-failed-rate.txt | head -1)
    rate=$(cat "$rate_file" 2>/dev/null || echo "-1")
    iters=$(cat "$(find "$step_dir" -name iterations.txt | head -1)" 2>/dev/null || echo 0)
    restarts=$(cat "$(find "$step_dir" -name restarts.txt | head -1)" 2>/dev/null || echo 0)
    secs="${LOAD%s}"
    # Throughput is the number the talk wants: admissions per second sustained.
    rps=$(python3 -c "print(f'{$iters / max(1,$secs):.1f}')" 2>/dev/null || echo "")
    echo "    vus=$vus iterations=$iters rps=$rps failure_rate=$rate restarts=$restarts" | tee -a "$LOG"
    { echo "vus=$vus"; echo "iterations=$iters"; echo "rps=$rps"
      echo "failure_rate=$rate"; echo "restarts=$restarts"; } > "$step_dir/ramp-step.txt"

    # A crash and a saturation both show up as a wall of failed requests, and
    # they mean opposite things: one is a bug, the other is a capacity limit.
    # Restarts tell them apart, so check that first and name it in the result.
    if (( restarts > 0 )); then
      echo "  [CRASHED] admission controller restarted at ${vus} VUs (restarts this step: ${restarts})" | tee -a "$LOG"
      echo "            this is a crash, not saturation — see the pod's previous logs" | tee -a "$LOG"
      kubectl -n kyverno logs deploy/kyverno-admission-controller --previous --tail=40 2>/dev/null \
        | sed 's/\x1b\[[0-9;]*m//g' | grep -iE "fatal error|panic|concurrent map" | head -3 \
        | tee -a "$LOG" | sed 's/^/            /'
      kubectl -n kyverno logs deploy/kyverno-admission-controller --previous --tail=120 2>/dev/null \
        > "$step_dir/crash-log.txt" 2>&1 || true
      crashed="$vus"
      break
    fi

    if [[ "$rate" != "-1" ]] && python3 -c "import sys; sys.exit(0 if float('$rate') > float('$FAIL_THRESHOLD') else 1)"; then
      echo "  [saturated] failure rate $rate exceeds $FAIL_THRESHOLD at ${vus} VUs — stopping" | tee -a "$LOG"
      saturated="$vus"
      break
    fi
  done

  { echo "version=$KYVERNO_VERSION"; echo "chart=$KYVERNO_CHART"
    echo "images=$IMAGES"; echo "delay_ms=$DELAYS"; echo "cache_mode=$CACHE_MODE"
    echo "steps=$STEPS"; echo "saturated_at_vus=${saturated:-none}"
    echo "crashed_at_vus=${crashed:-none}"
    echo "fail_threshold=$FAIL_THRESHOLD"
  } > "$base/round-meta.txt"
  touch "$base/.complete"

  if [[ -z "$saturated" && -z "$crashed" ]]; then
    echo "  [note] neither saturated nor crashed within [$STEPS] — the ceiling is above the top step" | tee -a "$LOG"
  fi

  CLUSTER="$cluster" ACTION=delete bash "$SIBLING/setup-mac.sh" >>"$LOG" 2>&1 || true
  (( r < ROUNDS )) && { echo "[cooldown] ${COOLDOWN}s" | tee -a "$LOG"; sleep "$COOLDOWN"; }
done

echo "=== ramp complete ===" | tee -a "$LOG"
grep -h "^saturated_at_vus=" "$RESULTS_ROOT"/round*/round-meta.txt 2>/dev/null | tee -a "$LOG"

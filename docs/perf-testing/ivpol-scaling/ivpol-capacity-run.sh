#!/usr/bin/env bash
# Capacity campaign: fills every row of the version-comparison table in one run.
#
# Two phases per version, because one phase cannot answer both questions:
#
#   RAMP  short steps at rising concurrency until failures or a crash. Finds
#         the ceiling. ALLOW_FAILURES=1, because driving the failure rate up is
#         the measurement here - the step *below* the threshold is the answer.
#   HOLD  one long cell at the last good concurrency, with every gate switched
#         back on. This is the cell that gets reported: sustained ops/sec, CPU
#         and memory per replica, and latency under load.
#
# The split exists because metrics-server refreshes roughly every 15s. A 60s
# ramp step yields about four distinct CPU values, which is too thin to quote
# as "CPU per replica". The hold cell runs long enough to mean something.
#
# WHY WARM. A cold ramp would put every verification through the TLS delay
# proxy, which is one asyncio process on one core. At high concurrency that
# proxy saturates before Kyverno does, and the ramp would measure Python's TLS
# throughput instead of the admission controller's. Warm keeps registry traffic
# near zero at steady state, so the ceiling found here is Kyverno's own.
# Kyverno 1.18.x has no ivpol cache, so its "warm" cells are really cold - but
# it saturates at single-digit concurrency anyway, far below where the proxy
# would matter. Proxy CPU is sampled per step regardless and flagged if it
# approaches a core, so this assumption is checked rather than trusted.
#
# Usage:
#   bash ivpol-capacity-run.sh
#   STEPS="1 2" ROUNDS=1 RAMP_LOAD=30s HOLD_LOAD=60s bash ivpol-capacity-run.sh   # smoke
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SIBLING="$HERE/../clusterpolicy-scaling"

VERSIONS="${VERSIONS:-1.18.2:3.8.2 1.19.1:3.9.1 main:3.9.1:ghcr.io/kyverno/kyverno:latest}"
# Concurrency, not arrival rate. The k6 script's constant-vus executor holds
# this many admissions in flight; a constant-arrival-rate run would queue
# against a saturated webhook and report the queue rather than the engine.
STEPS="${STEPS:-1 2 4 8 16 32}"
ROUNDS="${ROUNDS:-2}"
MODE="${MODE:-key}"
IMAGES="${IMAGES:-1}"
DELAYS="${DELAYS:-25}"
CACHE_MODE="${CACHE_MODE:-warm}"
RAMP_LOAD="${RAMP_LOAD:-60s}"
HOLD_LOAD="${HOLD_LOAD:-180s}"
WARMUP="${WARMUP:-10s}"
IMAGE_COUNT="${IMAGE_COUNT:-4}"
KIND_IMAGE="${KIND_IMAGE:-kindest/node:v1.31.6}"
FAIL_THRESHOLD="${FAIL_THRESHOLD:-0.01}"
COOLDOWN="${COOLDOWN:-180}"
CELL_COOLDOWN="${CELL_COOLDOWN:-30}"
# Before the HOLD only: Go returns freed pages to the OS over minutes, so a
# HOLD started straight after a saturating step reports that step's memory.
HOLD_COOLDOWN="${HOLD_COOLDOWN:-150}"
RESULTS_ROOT="${RESULTS_ROOT:-$HERE/results/capacity}"

mkdir -p "$RESULTS_ROOT"
LOG="$RESULTS_ROOT/capacity.log"

read -ra VER_ARR <<< "$VERSIONS"
echo "=== capacity campaign: ${#VER_ARR[@]} version(s), steps [$STEPS], $ROUNDS round(s), cache=$CACHE_MODE ===" | tee -a "$LOG"

# Peak CPU of the registry-namespace pods over a step. If the delay proxy is
# near a full core the ceiling found is the rig's, not Kyverno's, and the step
# is not reportable. Sampled by the caller around each k6 run.
sample_proxy_cpu() {
  local out="$1"
  # The subshell must redirect ALL its fds: backgrounded inside $(...) it
  # inherits the substitution's pipe, which then never closes and the caller
  # hangs forever waiting for it.
  ( while true; do
      kubectl top pod -n bench-registry --no-headers 2>/dev/null \
        | awk '{gsub("m","",$2); print $2}' >> "$out"
      sleep 3
    done ) >/dev/null 2>&1 &
  echo $!
}

proxy_peak() {
  local f="$1"
  [[ -s "$f" ]] || { echo ""; return; }
  sort -n "$f" | tail -1
}

# Split a full image reference into the three knobs the chart actually has.
# Same logic as ladder-ivpol-run.sh: the chart's helper only ever renders
# "$registry/$repository:$tag", so a digest has to ride inside the tag field as
# "pinned@sha256:<hex>" - Kubernetes resolves on the digest and ignores the
# cosmetic tag. A repository other than the chart default needs EXTRA_HELM_ARGS.
resolve_image() {
  IMAGE_REGISTRY=""; IMAGE_TAG=""; SLOT_HELM_ARGS=""
  [[ -z "$IMAGEREF" ]] && return 0
  IMAGE_REGISTRY="${IMAGEREF%%/*}"
  local rest="${IMAGEREF#*/}" repo
  if [[ "$rest" == *@* ]]; then
    repo="${rest%%@*}"; IMAGE_TAG="pinned@${rest#*@}"
  else
    repo="${rest%%:*}"; IMAGE_TAG="${rest#*:}"
  fi
  [[ "$repo" != "kyverno/kyverno" ]] \
    && SLOT_HELM_ARGS="--set admissionController.container.image.repository=$repo"
  echo "    explicit image: $IMAGEREF (registry=$IMAGE_REGISTRY tag=$IMAGE_TAG)" | tee -a "$LOG"
}

# One cell. Echoes "rate restarts iters cov proxypeak" for the caller to parse.
run_cell() {
  local dir="$1" conc="$2" load="$3" skip="$4" allow="$5"
  mkdir -p "$dir"
  local pcsv="$dir/proxy-top.csv" ppid
  : > "$pcsv"
  ppid=$(sample_proxy_cpu "$pcsv")

  env MODE="$MODE" IMAGES="$IMAGES" DELAYS="$DELAYS" CACHE_MODE="$CACHE_MODE" \
      CONCURRENCY="$conc" LOAD="$load" WARMUP="$WARMUP" \
      KYVERNO_VERSION="$APP" KYVERNO_CHART="$CHART" \
      KYVERNO_IMAGE_TAG="$IMAGE_TAG" KYVERNO_IMAGE_REGISTRY="$IMAGE_REGISTRY" \
      EXTRA_HELM_ARGS="${EXTRA_HELM_ARGS:-} $SLOT_HELM_ARGS" \
      KNOWN_POOLING_LIMIT=1 CELL_COOLDOWN="$CELL_COOLDOWN" \
      ${skip:+SKIP_INSTALL=1} ${allow:+ALLOW_FAILURES=1} \
      RESULTS_DIR="$dir" \
      bash "$HERE/ivpol-run.sh" >>"$LOG" 2>&1
  local rc=$?

  kill "$ppid" 2>/dev/null; wait "$ppid" 2>/dev/null

  local rate iters restarts cov
  rate=$(cat "$(find "$dir" -name admission-failed-rate.txt 2>/dev/null | head -1)" 2>/dev/null || echo "-1")
  iters=$(cat "$(find "$dir" -name iterations.txt 2>/dev/null | head -1)" 2>/dev/null || echo 0)
  # Exact cell path, not `find`: pre-cell/restarts.txt is cumulative and would
  # attribute an earlier crash to this step.
  restarts=$(cat "$(find "$dir" -name restarts.txt -not -path "*/pre-cell/*" 2>/dev/null | head -1)" 2>/dev/null || echo 0)
  cov=$(cat "$(find "$dir" -name verification-coverage-pct.txt 2>/dev/null | head -1)" 2>/dev/null || echo "")
  # Comma-delimited, not whitespace: a warm cell writes no coverage value, and
  # `read` collapses adjacent empty whitespace fields, shifting every field after
  # it. Same trap as kubectl-top parsing in ivpol-run.sh.
  local krate
  krate=$(python3 -c '
import json,sys
try: print(f"{json.load(open(sys.argv[1]))[\"metrics\"][\"iterations\"][\"rate\"]:.2f}")
except Exception: print("")' "$(find "$dir" -name k6-summary.json 2>/dev/null | head -1)" 2>/dev/null || echo "")
  echo "${rate:--1},${restarts:-0},${iters:-0},${cov:-},$(proxy_peak "$pcsv"),$rc,${krate:-}"
}

for r in $(seq 1 "$ROUNDS"); do
  # Reshuffled each round, so a version is never always first on a warm laptop.
  # Rotate by round so a version is never always first on a warm laptop.
  # srand() seeding was deterministic and gave the same order every round.
  order=""
  n=${#VER_ARR[@]}
  for ((i=0;i<n;i++)); do order="$order ${VER_ARR[$(( (i + r - 1) % n ))]}"; done
  echo "[round $r] order: $(echo $order)" | tee -a "$LOG"

  for pair in $order; do
    APP="${pair%%:*}"
    rest="${pair#*:}"
    if [[ "$rest" == *:* ]]; then CHART="${rest%%:*}"; IMAGEREF="${rest#*:}"
    else CHART="$rest"; IMAGEREF=""; fi
    resolve_image

    base="$RESULTS_ROOT/round${r}/${APP}"
    if [[ -f "$base/.complete" ]]; then echo "[skip] round $r $APP done" | tee -a "$LOG"; continue; fi
    rm -rf "$base"; mkdir -p "$base"
    cluster="ivpolc-r${r}"

    echo "[round $r] $APP (chart $CHART)${IMAGEREF:+ image $IMAGEREF} cluster=$cluster" | tee -a "$LOG"
    CLUSTER="$cluster" KIND_IMAGE="$KIND_IMAGE" bash "$SIBLING/setup-mac.sh" >>"$LOG" 2>&1
    IMAGE_COUNT="$IMAGE_COUNT" KIND_CLUSTER="$cluster" bash "$HERE/registry/setup-registry.sh" >>"$LOG" 2>&1

    last_good="" saturated="" crashed="" first=1
    for c in $STEPS; do
      echo "  [ramp] concurrency $c" | tee -a "$LOG"
      skip=""; [[ "$first" == "1" ]] || skip=1; first=0
      IFS=, read -r rate restarts iters cov ppeak rc krate <<< "$(run_cell "$base/c${c}" "$c" "$RAMP_LOAD" "$skip" 1)"
      secs="${RAMP_LOAD%s}"
      rps="${krate:-}"
      [[ -z "$rps" ]] && rps=$(python3 -c "print(f'{$iters/max(1,$secs):.1f}')" 2>/dev/null || echo "")
      echo "    c=$c iters=$iters rps=$rps fail=$rate restarts=$restarts cov=${cov:-n/a} proxy_peak=${ppeak:-?}m" | tee -a "$LOG"
      { echo "concurrency=$c"; echo "iterations=$iters"; echo "rps=$rps"
        echo "failure_rate=$rate"; echo "restarts=$restarts"
        echo "coverage_pct=$cov"; echo "proxy_cpu_peak_millicores=$ppeak"
      } > "$base/c${c}/ramp-step.txt"

      if [[ -n "$ppeak" ]] && (( ppeak > 800 )); then
        echo "  [WARN] delay proxy at ${ppeak}m - approaching one core. The ceiling from here" | tee -a "$LOG"
        echo "         may be the rig's, not Kyverno's. Do not report steps above this." | tee -a "$LOG"
      fi

      if (( restarts > 0 )); then
        echo "  [CRASHED] controller restarted at concurrency $c - a bug, not a ceiling" | tee -a "$LOG"
        kubectl -n kyverno logs deploy/kyverno-admission-controller --previous --tail=120 \
          > "$base/c${c}/crash-log.txt" 2>&1 || true
        crashed="$c"; break
      fi
      if [[ "$rate" != "-1" ]] && python3 -c "import sys;sys.exit(0 if float('$rate')>float('$FAIL_THRESHOLD') else 1)"; then
        echo "  [saturated] failure rate $rate exceeds $FAIL_THRESHOLD at concurrency $c" | tee -a "$LOG"
        saturated="$c"; break
      fi
      last_good="$c"
    done

    # The reportable cell: long enough for metrics-server to produce real CPU
    # numbers, and with the gates back on so it has to prove it verified.
    hold_rps="" hold_rate="" hold_cov=""
    if [[ -n "$last_good" ]]; then
      echo "  [hold] settling ${HOLD_COOLDOWN}s so the previous step's memory is released" | tee -a "$LOG"
      sleep "$HOLD_COOLDOWN"
      echo "  [hold] concurrency $last_good for $HOLD_LOAD, gates on" | tee -a "$LOG"
      IFS=, read -r hold_rate hrestarts hiters hold_cov hppeak hrc hkrate <<< "$(run_cell "$base/hold-c${last_good}" "$last_good" "$HOLD_LOAD" 1 "")"
      hsecs="${HOLD_LOAD%s}"
      hold_rps="${hkrate:-}"
      [[ -z "$hold_rps" ]] && hold_rps=$(python3 -c "print(f'{$hiters/max(1,$hsecs):.1f}')" 2>/dev/null || echo "")
      echo "    hold c=$last_good iters=$hiters rps=$hold_rps fail=$hold_rate cov=${hold_cov:-n/a} rc=$hrc" | tee -a "$LOG"
      if [[ "$hrc" != "0" ]]; then
        echo "  [WARN] hold cell failed its own gates (rc=$hrc) - not reportable, see the log" | tee -a "$LOG"
      fi
    else
      echo "  [note] no step passed - this version saturates below the lowest step ($(echo $STEPS | cut -d' ' -f1))" | tee -a "$LOG"
    fi

    { echo "app=$APP"; echo "chart=$CHART"; echo "imageref=$IMAGEREF"
      echo "mode=$MODE"; echo "images=$IMAGES"; echo "delay_ms=$DELAYS"
      echo "cache_mode=$CACHE_MODE"; echo "steps=$STEPS"
      echo "ramp_load=$RAMP_LOAD"; echo "hold_load=$HOLD_LOAD"
      echo "last_good_concurrency=${last_good:-none}"
      echo "saturated_at=${saturated:-none}"; echo "crashed_at=${crashed:-none}"
      echo "hold_rps=$hold_rps"; echo "hold_failure_rate=$hold_rate"
      echo "hold_coverage_pct=$hold_cov"
      echo "fail_threshold=$FAIL_THRESHOLD"
    } > "$base/version-meta.txt"
    touch "$base/.complete"

    CLUSTER="$cluster" ACTION=delete bash "$SIBLING/setup-mac.sh" >>"$LOG" 2>&1 || true
    echo "  [cooldown] ${COOLDOWN}s" | tee -a "$LOG"; sleep "$COOLDOWN"
  done
done

echo "=== capacity campaign complete ===" | tee -a "$LOG"

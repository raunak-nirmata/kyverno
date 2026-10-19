#!/usr/bin/env bash
# Matrix driver for the ivpol scaling benchmark (one Kyverno version).
#
# Cells are images-per-pod x injected-registry-latency, at a fixed cache mode.
# Per cell: k6 iteration_duration stats, Prometheus CPU/memory over the exact
# cell window, Kyverno's own admission-latency metric, and the registry request
# counters that prove verification actually happened.
#
# Prereqs: bash ../clusterpolicy-scaling/setup-mac.sh (cluster, KWOK, Prometheus)
#          bash registry/setup-registry.sh            (registry, images, keys)
#
# Usage:
#   bash ivpol-run.sh
#   IMAGES="1 4" DELAYS="25" bash ivpol-run.sh                  # smoke
#   CACHE_MODE=warm bash ivpol-run.sh
#   KYVERNO_VERSION=1.19.0 KYVERNO_CHART=3.9.0 bash ivpol-run.sh
#   REPORTS=off bash ivpol-run.sh                                # evidence-retention cell
#   POLICY_TEMPLATE=policies/ivpol-cosign-key-combined.yaml bash ivpol-run.sh   # multi-attestor cells
#
# ladder-ivpol-run.sh drives this once per (round, version) slot.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SIBLING="$HERE/../clusterpolicy-scaling"
KYVERNO_REPO="$(cd "$HERE/../../.." && pwd)"
K6="${K6:-$SIBLING/k6-xk6}"
SCRIPT_JS="${SCRIPT_JS:-$HERE/script-ivpol-admit.js}"
RES_REPORT="${RES_REPORT:-$KYVERNO_REPO/scripts/kyverno-pods-resources-report.sh}"
POLICY_TEMPLATE="${POLICY_TEMPLATE:-$HERE/policies/ivpol-cosign-key.yaml}"
VALUES="${VALUES:-$HERE/ivpol-scaling-values.yaml}"
KEY_DIR="${KEY_DIR:-$HERE/registry/keys}"

KYVERNO_VERSION="${KYVERNO_VERSION:-1.19.1}"
KYVERNO_CHART="${KYVERNO_CHART:-3.9.1}"
# For versions with no published chart (release candidates). Set to a chart
# directory - fetch-chart.sh extracts one from a git tag - and KYVERNO_CHART is
# ignored. KYVERNO_IMAGE_TAG is then REQUIRED, because a chart taken from an RC
# tag still carries the previous release's appVersion and would otherwise
# install the wrong image.
KYVERNO_CHART_PATH="${KYVERNO_CHART_PATH:-}"
KYVERNO_IMAGE_TAG="${KYVERNO_IMAGE_TAG:-}"
# The chart defaults to reg.kyverno.io. That host answers 401 to an anonymous
# manifest request for every tag, so it cannot be probed ahead of time to
# confirm a release candidate is actually there. ghcr.io can be, and was: both
# v1.19.1 and v1.19.2-rc.1 return 200. Point RC installs at ghcr so a missing
# image fails at `helm install` rather than halfway through a slot.
KYVERNO_IMAGE_REGISTRY="${KYVERNO_IMAGE_REGISTRY:-}"
# The headline sweep. Powers of two so the shape of the curve is legible on a
# slide; 16 is where a serial fetch loop should be unmistakable.
IMAGES="${IMAGES:-1 2 4 8 16}"
# 0 is in the list on purpose. It is the control that says how much of the
# difference between versions is the fetch pattern rather than anything else:
# with no latency to serialise, the versions should converge.
DELAYS="${DELAYS:-0 25 100}"
# pkg/image/verification/evaluator/eval.go's DisabledImageVerifyCache() is the
# CLI apply/test path, not this one. The real admission path is
# pkg/cel/policies/ivpol/engine/engine.go:513 (HandleValidating), which builds
# its compiler with the configurable e.ivCache - only HandleMutating (:240,
# the MutateDigest path, unused here) hardcodes the cache off. So
# --imageVerifyCacheEnabled does change admission behaviour on this rig.
# Earlier archived counters (7251 fetches for 904 requests x 8 images, no
# hits) were from a run against that flag's default, not proof the flag is
# inert - re-verify with assert_cache_mode before trusting a "no effect" claim.
CACHE_MODE="${CACHE_MODE:-off}"
# Evidence-retention toggle. features.admissionReports.enabled is consumed by
# the chart's `with`-on-map helper (templates/_helpers.tpl), which tests the
# map's presence, not the boolean inside it — unlike extraArgs' truthiness
# test, so plain --set is safe here and false does not get silently dropped.
# See assert_reports_mode, which proves this rather than assuming it.
REPORTS="${REPORTS:-on}"
VUS="${VUS:-20}"
# Duration, not an iteration count. A fixed count makes cell length depend on how
# expensive the cell is: 400 iterations at +100ms with 16 images runs ~32s, but
# the same 400 at 0ms with 1 image finishes in under a second - far too short to
# read CPU out of Prometheus, and that cheap lane is the control the whole chart
# leans on. A duration gives every cell the same window and makes throughput
# (iterations completed) a second, comparable metric.
# An integer is still accepted and means a fixed iteration count.
LOAD="${LOAD:-60s}"
CELL_COOLDOWN="${CELL_COOLDOWN:-60}"
# Fixed arrival rate, not a cap derived from a connection budget. With pooled
# TLS connections the port budget no longer binds, so the rate is a
# measurement choice. Default matches SERVING_RPS and keeps the slowest cell
# (16 images, +100ms, ~8.6s) at ~90 in flight, under MAX_VUS=200.
TARGET_RPS="${TARGET_RPS:-10}"
SKIP_INSTALL="${SKIP_INSTALL:-}"
RESULTS_DIR="${RESULTS_DIR:-$HERE/results/ivpol}"
REGISTRY_HOST="${REGISTRY_HOST:-registry.bench-registry.svc.cluster.local:5000}"
REPO="${REPO:-bench/signed}"
TREND_STATS="avg,min,med,max,p(90),p(95),p(99)"
PROM_LOCAL="http://localhost:9090"
TEST_NS="${TEST_NS:-testns}"

export KYVERNO_NAMESPACE=kyverno
mkdir -p "$RESULTS_DIR"

[[ -x "$K6" ]] || { echo "FATAL: missing k6 build: $K6"; exit 1; }
[[ -f "$KEY_DIR/cosign.pub" ]] || { echo "FATAL: no public key at $KEY_DIR/cosign.pub — run registry/setup-registry.sh"; exit 1; }
[[ -f "$KEY_DIR/ca-bundle.pem" ]] || { echo "FATAL: no CA bundle at $KEY_DIR/ca-bundle.pem — run registry/setup-registry.sh"; exit 1; }

helm repo add kyverno https://kyverno.github.io/kyverno/ >/dev/null 2>&1 || true
helm repo update kyverno >/dev/null 2>&1 || true

# Prometheus is a cross-check, not the measurement: a missing value is worth a
# blank column, never a dead run. Without the `|| true` a failed curl fails the
# pipeline (set -o pipefail), which fails the assignment, which set -e turns
# into a silent exit just before the cell writes its metadata.
promq() {
  { curl -sf --get "$PROM_LOCAL/api/v1/query" --data-urlencode "query=$1" 2>/dev/null \
    | python3 -c 'import json,sys
try:
    d=json.load(sys.stdin)["data"]["result"]
    print(d[0]["value"][1] if d else "")
except Exception:
    print("")' 2>/dev/null; } || true
}

# The sibling harness port-forwards Prometheus for the whole run; this one needs
# the same, and previously just assumed localhost:9090 was already listening.
PROM_PF_PID=""
start_prom_pf() {
  pkill -f "port-forward svc/kube-prometheus-stack-prometheus" 2>/dev/null || true
  sleep 1
  kubectl -n monitoring port-forward svc/kube-prometheus-stack-prometheus 9090:9090 \
    >/tmp/ivpol-prom-pf.log 2>&1 &
  PROM_PF_PID=$!
  disown "$PROM_PF_PID" 2>/dev/null || true
  for _ in $(seq 1 60); do
    curl -sf "$PROM_LOCAL/-/ready" >/dev/null 2>&1 && { echo "Prometheus reachable"; return 0; }
    sleep 1
  done
  echo "WARN: Prometheus not reachable on $PROM_LOCAL; resource columns will be blank"
}
trap 'kill $PROM_PF_PID 2>/dev/null || true' EXIT

registry_ctl() { ACTION="$1" DELAY_MS="${2:-0}" bash "$HERE/registry/setup-registry.sh"; }

# ---------------------------------------------------------------- policy

# Maps a placeholder token found in a policy template to the file supplying
# its value. Bash 3.2 (macOS) has no associative arrays, hence a case
# statement rather than a dict. Add an entry here whenever a new policy
# template introduces a new *_PLACEHOLDER token.
placeholder_source() {
  case "$1" in
    PUBLIC_KEY_PLACEHOLDER) echo "$KEY_DIR/cosign.pub" ;;
    KEY_A_PLACEHOLDER)      echo "$KEY_DIR/cosign.pub" ;;
    KEY_B_PLACEHOLDER)      echo "$KEY_DIR/cosign-b.pub" ;;
    KEY_C_PLACEHOLDER)      echo "$KEY_DIR/cosign-c.pub" ;;
    CERT_CHAIN_PLACEHOLDER) echo "$KEY_DIR/codesign-ca.crt" ;;
    *) return 1 ;;
  esac
}

render_policy() {
  # Every key or cert chain is indented into its YAML block scalar with
  # python rather than sed, because the value is multi-line and must keep its
  # exact bytes: a mangled key fails verification, and a cell that fails
  # verification is measuring the error path at full speed.
  #
  # Generalized from a single hardcoded PUBLIC_KEY_PLACEHOLDER: a multi-attestor
  # template can carry several tokens (KEY_A_PLACEHOLDER, KEY_B_PLACEHOLDER,
  # CERT_CHAIN_PLACEHOLDER, ...), and each is resolved and validated
  # independently rather than assuming attestors[0] is the only one that matters.
  local out="$1"
  local tokens
  tokens=$(grep -o '[A-Z_]*_PLACEHOLDER' "$POLICY_TEMPLATE" | sort -u)
  if [[ -z "$tokens" ]]; then
    echo "FATAL: $POLICY_TEMPLATE carries no _PLACEHOLDER token to render."
    exit 1
  fi
  local args=() token src
  for token in $tokens; do
    src=$(placeholder_source "$token") || {
      echo "FATAL: no source file mapped for placeholder $token in $POLICY_TEMPLATE."
      echo "       Add it to placeholder_source() in ivpol-run.sh."
      exit 1
    }
    [[ -f "$src" ]] || { echo "FATAL: placeholder $token maps to $src, which does not exist — run registry/setup-registry.sh."; exit 1; }
    args+=("$token" "$src")
  done
  python3 - "$POLICY_TEMPLATE" "$out" "${args[@]}" <<'PY'
import sys, pathlib
template, out = sys.argv[1], sys.argv[2]
pairs = sys.argv[3:]
text = pathlib.Path(template).read_text()
lines = text.split("\n")
rendered = text
for i in range(0, len(pairs), 2):
    token, valuefile = pairs[i], pairs[i + 1]
    # Exactly one occurrence, not at least one. A second one - in a comment
    # describing the placeholder, for instance - would also be substituted,
    # splicing a multi-line value into a comment and producing a manifest
    # that does not parse.
    occurrences = text.count(token)
    if occurrences != 1:
        sys.exit(f"policy template has {occurrences} {token} tokens, expected exactly 1")
    # Indent is derived from the block-scalar line above the placeholder
    # ("key: |" / "value: |"), not a single hardcoded width: a key nested
    # under cosign.key.data sits at a different depth than one nested under
    # cosign.certificate.certChain.value, and a fixed indent that happens to
    # work for one breaks the YAML block scalar for the other (the content
    # must indent deeper than its parent key, not just differently from it).
    placeholder_line = next(ln for ln in lines if ln.strip() == token)
    parent_line = lines[lines.index(placeholder_line) - 1]
    parent_indent = len(parent_line) - len(parent_line.lstrip(" "))
    pad = " " * (parent_indent + 2)
    value = pathlib.Path(valuefile).read_text().rstrip("\n")
    indented = "\n".join(pad + line for line in value.splitlines())
    rendered = rendered.replace(token, indented)
pathlib.Path(out).write_text(rendered)
# Parse what will actually be applied. A policy that does not round-trip is a
# cell that fails for a reason unrelated to the thing being measured.
try:
    import yaml
except ImportError:
    pass
else:
    doc = yaml.safe_load(rendered)
    for att in doc["spec"]["attestors"]:
        cosign = att.get("cosign", {})
        if "key" in cosign:
            got = cosign["key"]["data"].strip()
            if not (got.startswith("-----BEGIN PUBLIC KEY-----") and got.endswith("-----END PUBLIC KEY-----")):
                sys.exit(f"rendered policy attestor {att.get('name')} does not carry an intact public key")
        elif "certificate" in cosign:
            chain = cosign["certificate"].get("certChain", {}).get("value", "").strip()
            if not (chain.startswith("-----BEGIN CERTIFICATE-----") and chain.endswith("-----END CERTIFICATE-----")):
                sys.exit(f"rendered policy attestor {att.get('name')} does not carry an intact certificate chain")
PY
}

assert_policy_matches_template() {
  # Prove the live policy is the one this cell asked for. The controls cannot:
  # a positive and a negative control behave identically under every policy in
  # this matrix, so a stale policy passes them and the cell silently measures
  # the previous cell's configuration.
  local want_attestors live_attestors live_names
  want_attestors=$(python3 -c "
import sys,yaml
d=yaml.safe_load(open(sys.argv[1]))
print(len(d['spec'].get('attestors',[])))" "$RENDERED_POLICY_PATH" 2>/dev/null || echo "?")
  live_names=$(kubectl get ivpol --no-headers -o custom-columns=NAME:.metadata.name 2>/dev/null | tr '\n' ' ')
  live_attestors=$(kubectl get ivpol "$IVPOL_NAME" -o json 2>/dev/null \
    | python3 -c "
import json,sys
try: print(len(json.load(sys.stdin)['spec'].get('attestors',[])))
except Exception: print('?')" 2>/dev/null || echo "?")
  echo "policy check: $IVPOL_NAME live with $live_attestors attestor(s); cluster has: $live_names"
  if [[ "$want_attestors" != "?" && "$live_attestors" != "$want_attestors" ]]; then
    echo "FATAL: $IVPOL_NAME has $live_attestors attestor(s), template asked for $want_attestors."
    echo "       The cell would measure a policy it did not request."
    exit 1
  fi
  local extra
  extra=$(kubectl get ivpol --no-headers -o custom-columns=NAME:.metadata.name 2>/dev/null | grep -v "^${IVPOL_NAME}$" || true)
  if [[ -n "$extra" ]]; then
    echo "FATAL: policies other than $IVPOL_NAME are live: $(echo $extra | tr '\n' ' ')"
    echo "       Admission would be evaluated by all of them, not just this cell's."
    exit 1
  fi
}

apply_policy() {
  # Rendered filename carries the template's basename, not a fixed name, so
  # different policy variants in the same RESULTS_DIR don't overwrite each
  # other's rendered output or its shasum in meta.txt.
  local base; base="$(basename "$POLICY_TEMPLATE" .yaml)"
  RENDERED_POLICY_PATH="$RESULTS_DIR/ivpol-rendered-$base.yaml"
  render_policy "$RENDERED_POLICY_PATH"
  kubectl apply -f "$RENDERED_POLICY_PATH" >/dev/null
  # Read the actual metadata.name back rather than hardcoding
  # ivpol-bench-cosign-key: every template now has its own distinct name.
  IVPOL_NAME=$(awk '/^metadata:/{f=1; next} f && /^  name:/{print $2; exit}' "$RENDERED_POLICY_PATH")
  [[ -n "$IVPOL_NAME" ]] || { echo "FATAL: could not read metadata.name from $RENDERED_POLICY_PATH"; exit 1; }
  kubectl wait --for=condition=Ready "ivpol/$IVPOL_NAME" --timeout=120s >/dev/null 2>&1 \
    || echo "WARN: ivpol did not report Ready; controls below decide whether it is actually enforcing"
  echo "applied $IVPOL_NAME ($(kubectl get ivpol --no-headers 2>/dev/null | wc -l | tr -d ' ') ImageValidatingPolicy total)"
}

# ---------------------------------------------------------------- gates

pod_json() {
  # $1 = image ref, $2 = pod name
  printf '{"apiVersion":"v1","kind":"Pod","metadata":{"name":"%s","namespace":"%s"},"spec":{"containers":[{"name":"c","image":"%s","command":["sh","-c","sleep 1"]}]}}' \
    "$2" "$TEST_NS" "$1"
}

get_restart_count() {
  # Cumulative restartCount summed across admission-controller containers.
  kubectl -n kyverno get pods -l app.kubernetes.io/component=admission-controller \
    -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{"\n"}{end}' 2>/dev/null \
    | awk '{s+=$1} END {print s+0}'
}

capture_crash_evidence() {
  # $1 = destination dir. Writes the previous container's log, its termination
  # reason/exit code/finishedAt, and a grepped signature. Caller decides
  # whether it belongs to a cell or predates one.
  local outdir="$1"
  mkdir -p "$outdir" 2>/dev/null || true
  kubectl -n kyverno logs deploy/kyverno-admission-controller --previous --tail=200 \
    > "$outdir/crash-previous.log" 2>&1 || true
  kubectl -n kyverno get pods -l app.kubernetes.io/component=admission-controller \
    -o jsonpath='{range .items[*]}{.status.containerStatuses[0].lastState.terminated.reason}{" exit="}{.status.containerStatuses[0].lastState.terminated.exitCode}{" finishedAt="}{.status.containerStatuses[0].lastState.terminated.finishedAt}{"\n"}{end}' \
    > "$outdir/crash-reason.txt" 2>&1 || true
  sed 's/\x1b\[[0-9;]*m//g' "$outdir/crash-previous.log" 2>/dev/null \
    | grep -iE "fatal error|panic:|concurrent map" | head -3 > "$outdir/crash-signature.txt" || true
}

verify_ivpol_active() {
  # Negative control: an image the policy matches but which carries no valid
  # signature MUST be rejected. If it is admitted, ivpol is not on the admission
  # path and every latency number in this slot would be measuring an empty
  # pipeline. This is the gate that catches a mis-scoped matchImageReferences,
  # a policy that failed to compile, or a webhook that never registered.
  # VERIFY_REPO/VERIFY_TAG default to REPO/v1, the pinned single-key recipe's
  # own repo — unchanged behaviour there. Multi-attestor cells set VERIFY_REPO
  # to whichever repo the active policy is scoped to (e.g. bench/signed-cert,
  # bench/signed-all), so this control actually exercises the policy under test
  # instead of silently re-checking the single-key repo regardless of cell.
  local verify_repo="${VERIFY_REPO:-$REPO}"
  local verify_tag="${VERIFY_TAG:-v1}"
  local unsigned="$REGISTRY_HOST/$verify_repo:unsigned-control"
  if pod_json "$unsigned" "ivpol-neg-control" | kubectl create --dry-run=server -f - >/tmp/ivpol-neg.out 2>&1; then
    echo "FATAL: negative control was ADMITTED — an unsigned image passed ivpol."
    sed 's/^/  negctl: /' /tmp/ivpol-neg.out
    exit 1
  fi
  echo "negative control OK: unsigned image rejected"

  # Positive control: a correctly signed image MUST be admitted. Without this,
  # a policy that rejects everything - wrong key, unreachable registry - would
  # pass the negative control and look like a working, very fast cell.
  local signed="$REGISTRY_HOST/$verify_repo:$verify_tag"
  if ! pod_json "$signed" "ivpol-pos-control" | kubectl create --dry-run=server -f - >/tmp/ivpol-pos.out 2>&1; then
    echo "FATAL: positive control was REJECTED — a correctly signed image failed ivpol."
    echo "       Every cell would be measuring the rejection path, not verification."
    sed 's/^/  posctl: /' /tmp/ivpol-pos.out
    exit 1
  fi
  echo "positive control OK: signed image admitted"
}

wait_until_serving() {
  # The controls above prove the policy is wired up, but they are single
  # requests. A freshly installed controller can satisfy them and still fail
  # most of a concurrent workload for minutes afterwards - observed on
  # 1.19.2-rc.1, which failed 94% of a 60s cell and then ran clean with zero
  # errors when the same load was repeated eleven minutes later. A fixed sleep
  # is a guess at a number that varies by version and by host; this measures the
  # thing it is waiting for.
  #
  # Runs short bursts, discards them, and proceeds once one comes back below the
  # failure threshold. Everything here is thrown away - it exists to find the
  # point where measurement can begin, not to measure.
  local images="$1"
  local budget="${SERVING_BUDGET:-12}"
  local attempt rate
  for attempt in $(seq 1 "$budget"); do
    IMAGES_PER_POD="$images" REGISTRY_HOST="$REGISTRY_HOST" REPO="$REPO" TEST_NAMESPACE="$TEST_NS" \
      TARGET_RPS="${SERVING_RPS:-10}" DURATION=10s \
      "$K6" run "$SCRIPT_JS" --quiet \
      --summary-export /tmp/ivpol-serving.json >/dev/null 2>&1 || true
    rate=$(python3 -c '
import json
try:
    m = json.load(open("/tmp/ivpol-serving.json"))["metrics"].get("admission_failed", {})
    print(m.get("value", m.get("rate", 1)))
except Exception:
    print(1)' 2>/dev/null)
    kubectl delete pods -n "$TEST_NS" --all --ignore-not-found --wait=false >/dev/null 2>&1 || true
    if python3 -c "import sys; sys.exit(0 if float('$rate') <= 0.01 else 1)" 2>/dev/null; then
      echo "serving check passed after ${attempt} burst(s) (failure rate $rate)"
      return 0
    fi
    echo "  not serving yet (attempt $attempt/$budget, failure rate $rate) — waiting"
    sleep 15
  done
  # ALLOW_FAILURES means the failure path is what this cell measures - the
  # crash-boundary and two-version sweeps, where a controller that never
  # stabilises is the result rather than a broken rig. Aborting here would
  # discard it, so record the shortfall and let the cell run.
  if [[ "${ALLOW_FAILURES:-}" == "1" ]]; then
    echo "WARN: never reached a serving state after $budget bursts (last rate $rate)."
    echo "      ALLOW_FAILURES is set, so this is recorded as a crash marker, not an abort."
    echo "crash-marker" > "${RESULTS_DIR:-/tmp}/never-served.txt" 2>/dev/null || true
    # This crash happened during the serving probe, before any cell ran, so its
    # evidence goes in its own subdirectory rather than being left for the
    # first cell's assert_cell_valid to pick up and misattribute.
    local precell="${RESULTS_DIR:-/tmp}/pre-cell"
    mkdir -p "$precell" 2>/dev/null || true
    get_restart_count > "$precell/restarts.txt" 2>/dev/null || true
    capture_crash_evidence "$precell"
    return 0
  fi
  echo "FATAL: still failing after $budget bursts; the controller never reached a"
  echo "       state where it could serve the workload. Measuring now would record"
  echo "       the failure path, not verification."
  exit 1
}

assert_cell_valid() {
  # $1 = cell dir, $2 = images per pod, $3 = restarts sampled at cell t0,
  # $4 = cell t0 as a unix epoch.
  #
  # Three things have to be true for a cell to be a measurement of image
  # verification rather than a number that merely looks like one.
  local dir="$1" images="$2" restarts_t0="$3" cell_t0_epoch="$4"

  # (a) The load actually ran. With duration-based cells the iteration count is
  #     an output, not an input, so it has to be read back rather than assumed.
  local iters failed dropped
  read -r iters failed dropped <<<"$(python3 - "$dir/k6-summary.json" <<'PY2'
import json, sys
try:
    m = json.load(open(sys.argv[1]))["metrics"]
except Exception:
    print("0 0 0"); raise SystemExit
iters = int(m.get("iterations", {}).get("count", 0))
# k6 reports a Rate metric as {"passes": n, "fails": n, "value": r}, where
# value is the proportion of true samples - here, the share of admissions that
# failed. There is no "rate" key; reading one made this gate silently inert.
failed_metric = m.get("admission_failed", {})
rate = failed_metric.get("value", failed_metric.get("rate"))
dropped = int(m.get("dropped_iterations", {}).get("count", 0))
# A summary with no such metric reports nothing rather than a false zero.
print(f"{iters} {-1 if rate is None else rate} {dropped}")
PY2
)"
  echo "iterations=$iters admission_failed_rate=$failed dropped_iterations=$dropped"
  echo "$iters" > "$dir/iterations.txt"
  echo "$failed" > "$dir/admission-failed-rate.txt"
  # A fixed TARGET_RPS is now an input, not a derived cap, so VUs running out
  # before it's satisfied is worth flagging — it means the offered load was
  # lower than TARGET_RPS asked for. WARN, not FATAL: the admissions that did
  # happen are still a valid measurement.
  if (( iters > 0 )) && python3 -c "import sys; sys.exit(0 if $dropped > 0.05 * $iters else 1)" 2>/dev/null; then
    echo "WARN: $dropped dropped iterations against $iters completed — offered load fell short"
    echo "      of TARGET_RPS=$TARGET_RPS. Raise PREALLOC_VUS/MAX_VUS if this persists."
  fi

  # Record the restart count here, before any gate below can exit. A cell that
  # fails its gate is precisely the cell where "did the engine crash or did it
  # merely saturate" matters, and writing this only on the success path meant
  # the answer was missing exactly when it was needed.
  #
  # restarts.txt is cumulative since the pod started, so a crash in an earlier
  # cell of the same slot stays on the counter forever and every later cell
  # inherits it. The delta since this cell's own t0 is what actually isolates
  # this cell's crash signal; that is what gates and downstream reports read.
  local restarts_t1 restarts_delta
  restarts_t1=$(get_restart_count)
  restarts_delta=$(( restarts_t1 - restarts_t0 ))
  # A negative delta means the pod object itself was replaced (eviction,
  # reschedule) and restartCount reset; the delta can't be trusted then.
  (( restarts_delta < 0 )) && restarts_delta=0
  echo "$restarts_t1" > "$dir/restarts-cumulative.txt"
  echo "$restarts_delta" > "$dir/restarts.txt"
  echo "admission controller restarts: cumulative=$restarts_t1 this-cell-delta=$restarts_delta"
  # A restart count without a cause cannot tell a data race from an OOM kill,
  # and the cluster is deleted at the end of the slot, so capture it now.
  # Captured whenever the cumulative count is nonzero, even if the delta is
  # zero, because lastState still carries the most recent termination; the
  # finishedAt check below decides whether it belongs to this cell.
  if (( restarts_t1 > 0 )); then
    capture_crash_evidence "$dir"
    local finished_at finished_epoch
    finished_at=$(grep -o 'finishedAt=[^ ]*' "$dir/crash-reason.txt" 2>/dev/null | head -1 | cut -d= -f2)
    finished_epoch=$(python3 -c '
import calendar, sys
from datetime import datetime
try:
    print(calendar.timegm(datetime.strptime(sys.argv[1], "%Y-%m-%dT%H:%M:%SZ").timetuple()))
except Exception:
    print(0)
' "$finished_at" 2>/dev/null)
    finished_epoch="${finished_epoch:-0}"
    if [[ -n "$finished_at" ]] && (( finished_epoch > 0 )) && (( finished_epoch < cell_t0_epoch )); then
      echo "  crash predates this cell (finishedAt=$finished_at, cell started $(( cell_t0_epoch - finished_epoch ))s later) — labeling evidence predates-cell, not attributing it here"
      for f in crash-previous.log crash-reason.txt crash-signature.txt; do
        [[ -f "$dir/$f" ]] && mv "$dir/$f" "$dir/predates-cell.$f"
      done
    fi
    # Pick the file before reading it: a failed redirect reports through the
    # shell, which the command's own 2>/dev/null cannot suppress.
    local reason_file="$dir/crash-reason.txt"
    [[ -f "$reason_file" ]] || reason_file="$dir/predates-cell.crash-reason.txt"
    [[ -f "$reason_file" ]] && echo "  crash reason: $(tr '\n' ' ' < "$reason_file")"
    local sig_file="$dir/crash-signature.txt"
    [[ -f "$dir/predates-cell.crash-signature.txt" ]] && sig_file="$dir/predates-cell.crash-signature.txt"
    [[ -s "$sig_file" ]] && sed 's/^/  /' "$sig_file"
  fi
  if (( iters < 1 )); then
    echo "FATAL: the cell completed no iterations — nothing was measured."
    exit 1
  fi

  # (c) Verification really reached the registry. Captured before the gates
  #     below can exit, for the same reason as the restart count: a cell that
  #     fails is the one whose connection counters explain why.
  local stats manifests total
  stats="$(registry_ctl stats 2>/dev/null | tail -1)"
  echo "$stats" > "$dir/registry-stats.json"
  manifests=$(printf '%s' "$stats" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("manifest",0))
except Exception: print(0)' 2>/dev/null)
  total=$(printf '%s' "$stats" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("total",0))
except Exception: print(0)' 2>/dev/null)
  echo "registry requests: total=$total manifests=$manifests"

  # (b) Admission mostly succeeded. A failed request still completes an
  #     iteration, and the failure path is usually *faster* than verification,
  #     so a saturated or broken cell reports a better latency than a healthy
  #     one. Outside the concurrency ramp - which is defined by driving this up
  #     on purpose - a cell with real failures is not comparable.
  if [[ "$failed" != "-1" ]] && [[ "${ALLOW_FAILURES:-}" != "1" ]]; then
    python3 -c "import sys; sys.exit(0 if float('$failed') <= 0.01 else 1)" || {
      echo "FATAL: admission failure rate ${failed} exceeds 1%."
      echo "       Latency here is the failure path, not verification. Set ALLOW_FAILURES=1"
      echo "       only for the concurrency ramp, where saturation is the point."
      exit 1
    }
  fi


  # Hard floor: at least one manifest fetch per admission request. Below this,
  # verification short-circuited for most requests. The prefetch runs per
  # request in every cache state, so this holds for warm cells too.
  if (( manifests < iters )); then
    # ALLOW_FAILURES means failure is what this cell is measuring - the crash
    # boundary sweep, where the engine is expected to die. A dead engine fetches
    # nothing, so a starved manifest count is the symptom, not a misconfigured
    # policy, and aborting here would throw away the very result being sought.
    if [[ "${ALLOW_FAILURES:-}" == "1" ]]; then
      echo "WARN: only $manifests manifest fetches for $iters requests."
      echo "      Expected while the engine is failing; ALLOW_FAILURES is set, continuing."
    else
      echo "FATAL: only $manifests manifest fetches for $iters requests."
      echo "       Verification short-circuited — check matchImageReferences and the registry address."
      exit 1
    fi
  fi
  # Soft expectation: one fetch per image per request. A large shortfall means
  # fewer images were really fetched than the cell claims to be testing, which
  # would flatten the N axis for a reason that is not the engine.
  local expected=$(( iters * images ))
  local pct=$(( manifests * 100 / (expected > 0 ? expected : 1) ))
  echo "manifest fetches: $manifests of ~$expected expected (${pct}%)"
  if (( pct < 60 )); then
    echo "WARN: manifest fetches well below one-per-image-per-request; the N axis may be compressed"
  fi
  echo "$pct" > "$dir/manifest-coverage-pct.txt"

  # (e) The proxy terminates TLS now, so the two things that used to be a
  #     connection-count cap become output assertions instead.
  local tls_failed conns rate_per_s ratio
  tls_failed=$(printf '%s' "$stats" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("tls_handshake_failed",0))
except Exception: print(0)' 2>/dev/null)
  conns=$(printf '%s' "$stats" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("connections",0))
except Exception: print(0)' 2>/dev/null)
  rate_per_s=$(python3 -c "print(int($conns / max(1,$window)))" 2>/dev/null || echo 0)
  ratio=$(python3 -c "print(round($conns / max(1,$total), 3))" 2>/dev/null || echo 0)
  echo "registry connections: $conns, requests: $total, ratio: $ratio (${rate_per_s}/s), tls_handshake_failed=$tls_failed" \
    | tee "$dir/connection-rate.txt" >/dev/null
  echo "registry connections: $conns, requests: $total, ratio: $ratio (${rate_per_s}/s), tls_handshake_failed=$tls_failed"

  # Something reached the registry over plain HTTP. With name.Insecure gone and
  # the proxy TLS-only, this should never happen.
  if (( tls_failed > 0 )); then
    if [[ "${ALLOW_FAILURES:-}" == "1" ]]; then
      echo "WARN: $tls_failed TLS handshake failures — something reached the registry over"
      echo "      plain HTTP. Expected while the engine is failing; ALLOW_FAILURES is set."
    else
      echo "FATAL: $tls_failed TLS handshake failures — something reached the registry over plain HTTP."
      echo "       Set ALLOW_FAILURES=1 only for the crash-boundary sweep, where the engine"
      echo "       dying mid-handshake is expected."
      exit 1
    fi
  fi

  # Pooling check. With TLS the port budget no longer binds, so a healthy cell
  # should hold roughly its peak concurrency in connections (tens) against
  # thousands of requests; the failed-https churn this replaced measured
  # 1-2 connections per request.
  if (( total > 0 )) && (( conns * 4 > total )); then
    if [[ "${ALLOW_FAILURES:-}" == "1" ]]; then
      echo "WARN: $conns connections against $total requests exceeds the 1-per-4 budget."
      echo "      Expected while the engine is failing; ALLOW_FAILURES is set, continuing."
    else
      # Escalated, not widened. The SDK transport sets MaxIdleConns=100 but
      # never MaxIdleConnsPerHost, so Go's default of 2 binds: every image
      # comes from one registry host, and concurrency above 2 churns
      # connections. Measured 0.067/request at ~1 in flight against
      # 0.268 at ~74, same request count. Set KNOWN_POOLING_LIMIT=1 to record
      # a cell under that known cause; it still fails if the churn did harm.
      if [[ "${KNOWN_POOLING_LIMIT:-}" == "1" ]] && (( tls_failed == 0 )); then
        echo "WARN: $conns connections against $total requests exceeds the 1-per-4 budget."
        echo "      Known cause: the SDK transport leaves MaxIdleConnsPerHost at Go's"
        echo "      default of 2, so concurrency above 2 cannot pool. No handshake"
        echo "      failures, so the cell measured Kyverno, not the rig's ceiling."
        echo "known-pooling-limit" > "$dir/connection-churn.txt"
      else
        echo "FATAL: $conns connections against $total requests exceeds the 1-per-4 budget."
        echo "       Connections are not pooling; this would measure the rig, not Kyverno."
        echo "       A healthy cell that trips this is a finding about Kyverno's pooling,"
        echo "       not a reason to loosen the gate — escalate rather than widen it."
        exit 1
      fi
    fi
  fi
}

# ---------------------------------------------------------------- install

assert_cache_mode() {
  # Prove the cache flag reached the container, rather than trusting that
  # passing it meant anything.
  #
  # The Kyverno chart renders extraArgs with a truthiness test, so an entry
  # whose value is boolean false is dropped entirely and the flag silently falls
  # back to its default - which for imageVerifyCacheEnabled is true. Every cell
  # labelled "cache off" was therefore running with the cache on, and nothing
  # said so. --set-string keeps the value a string so it renders; this check is
  # what makes the next such slip impossible to miss.
  local want="$1"
  local args
  args=$(kubectl -n kyverno get deploy kyverno-admission-controller \
    -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null)
  if [[ "$args" != *"--imageVerifyCacheEnabled=$want"* ]]; then
    echo "FATAL: cache mode not applied. Expected --imageVerifyCacheEnabled=$want"
    echo "       in the running container's args, and it is not there."
    echo "       CACHE_MODE=$CACHE_MODE would be a label on a cell that does not match it."
    exit 1
  fi
  echo "cache flag verified: --imageVerifyCacheEnabled=$want"
}

assert_reports_mode() {
  # Mirrors assert_cache_mode: prove the reports flag reached the container
  # rather than trusting that passing it meant anything. This harness has
  # twice been fooled by a Helm flag that was silently dropped — once already
  # for imageVerifyCacheEnabled (see assert_cache_mode). admissionReports
  # is set via the chart's `with`-on-map helper, not extraArgs, so it is not
  # exposed to the same truthiness-drop bug, but "not exposed to that bug" is
  # a claim worth checking, not trusting.
  local want="$1"
  local args
  args=$(kubectl -n kyverno get deploy kyverno-admission-controller \
    -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null)
  if [[ "$args" != *"--admissionReports=$want"* ]]; then
    echo "FATAL: reports mode not applied. Expected --admissionReports=$want"
    echo "       in the running container's args, and it is not there."
    echo "       REPORTS=$REPORTS would be a label on a cell that does not match it."
    exit 1
  fi
  echo "reports flag verified: --admissionReports=$want"
}

assert_registry_trust() {
  # Prove TLS trust actually landed, rather than trusting that --set-file
  # meant anything. Three checks, each catching a distinct way this could
  # silently fail. The end-to-end proof is the positive control below: a
  # signed image is admitted only if Kyverno completed a verified TLS
  # handshake and fetched the signature over it.
  local args
  args=$(kubectl -n kyverno get deploy kyverno-admission-controller \
    -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null)
  if [[ "$args" == *"--allowInsecureRegistry=true"* ]]; then
    echo "FATAL: --allowInsecureRegistry=true is present in the running container's args."
    echo "       A values file or override re-added it; the run would be back on the"
    echo "       failed-https dial this change exists to remove."
    exit 1
  fi

  local mount_ok
  mount_ok=$(kubectl -n kyverno get deploy kyverno-admission-controller -o json 2>/dev/null | python3 -c '
import json, sys
try:
    mounts = json.load(sys.stdin)["spec"]["template"]["spec"]["containers"][0]["volumeMounts"]
except Exception:
    mounts = []
print("yes" if any(m.get("name") == "ca-certificates"
                    and m.get("mountPath") == "/etc/ssl/certs/ca-certificates.crt"
                    for m in mounts) else "no")
')
  if [[ "$mount_ok" != "yes" ]]; then
    echo "FATAL: no ca-certificates volumeMount at /etc/ssl/certs/ca-certificates.crt."
    echo "       --set-file may have read an empty ca-bundle.pem, which the chart's"
    echo "       'if .data' silently drops; every verification would then fail with"
    echo "       x509: certificate signed by unknown authority."
    exit 1
  fi

  local cm_data cm_ok
  cm_data=$(kubectl -n kyverno get configmap kyverno-admission-controller-ca-certificates \
    -o jsonpath='{.data.ca-certificates}' 2>/dev/null)
  cm_ok=$(python3 -c '
import sys
cm, ca_path = sys.argv[1], sys.argv[2]
ca = open(ca_path).read().strip()
print("yes" if ca in cm else "no")
' "$cm_data" "$KEY_DIR/ca.crt")
  if [[ "$cm_ok" != "yes" ]]; then
    echo "FATAL: the ca-certificates ConfigMap does not contain our CA's PEM body."
    echo "       Kyverno would be trusting a bundle that does not include the registry's CA."
    exit 1
  fi

  echo "registry trust verified: --allowInsecureRegistry not true, CA mount present, CM carries our CA"
}

assert_running_image() {
  # Prove the pod is running the version this slot claims. The chart's
  # appVersion is not trustworthy for a release candidate - at v1.19.2-rc.1 it
  # still says v1.19.1 - so the only honest check is what the container is
  # actually running.
  local want="${KYVERNO_IMAGE_TAG:-v$KYVERNO_VERSION}"
  local running
  running=$(kubectl -n kyverno get deploy kyverno-admission-controller \
    -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)
  echo "  running image: $running"
  if [[ "$running" != *"$want"* ]]; then
    echo "FATAL: admission controller is running '$running' but this slot claims '$want'."
    echo "       Every number would be attributed to the wrong version."
    exit 1
  fi
  echo "$running" > "$RESULTS_DIR/running-image.txt"
}

apply_kyverno_crds() {
  # Rendered out-of-band and applied directly, not tracked by the Helm release
  # (see setup_kyverno: crds.install=false). --server-side avoids the
  # last-applied-configuration annotation client-side apply would add, which
  # would double some of these CRDs (clusterpolicies/policies render past
  # 1.4MB each) past etcd's request size limit.
  local tmp
  tmp="$(mktemp)"
  helm template kyverno "$@" -n kyverno -f "$VALUES" \
    > "$tmp.full" 2>"$tmp.err" \
    || { echo "FATAL: helm template failed while extracting CRDs for apply."
         sed 's/^/  helm: /' "$tmp.err"; exit 1; }
  python3 -c '
import re, sys
docs = open(sys.argv[1]).read().split("\n---\n")
crds = [d for d in docs if re.search(r"^kind:\s*CustomResourceDefinition", d, re.M)]
sys.stdout.write("\n---\n".join(crds))
' "$tmp.full" > "$tmp.crds"
  local count
  count=$(grep -c '^kind: CustomResourceDefinition' "$tmp.crds" || true)
  if [[ "$count" -lt 1 ]]; then
    echo "FATAL: extracted zero CRDs from the chart's rendered manifest."
    echo "       The chart's CRD layout may have changed; this needs a look, not a retry."
    exit 1
  fi
  echo "applying $count CRDs out-of-band (kept out of the Helm release record)"
  kubectl apply --server-side --force-conflicts -f "$tmp.crds" >/dev/null
  rm -f "$tmp" "$tmp.full" "$tmp.err" "$tmp.crds"
}

setup_kyverno() {
  # Before the SKIP_INSTALL return: the ramp reuses one install across steps, and
  # each step still needs Prometheus reachable to fill its resource columns.
  start_prom_pf
  # SKIP_INSTALL reuses the controller, but the policy is a per-cell input, not
  # an install property: a sweep that varies POLICY_TEMPLATE across cells would
  # otherwise measure the first cell's policy every time.
  if [[ -n "$SKIP_INSTALL" ]]; then
    echo "SKIP_INSTALL set — reusing existing install, reapplying this cell's policy"
    kubectl create ns "$TEST_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
    apply_policy
    assert_policy_matches_template
    verify_ivpol_active
    wait_until_serving "$(echo $IMAGES | awk '{print $1}')"
    return 0
  fi
  echo "==> installing Kyverno $KYVERNO_VERSION (chart $KYVERNO_CHART), cache mode $CACHE_MODE, reports $REPORTS"
  helm uninstall kyverno -n kyverno >/dev/null 2>&1 || true
  kubectl delete ns kyverno --ignore-not-found --wait=true >/dev/null 2>&1 || true

  # Cache mode is an install-time property because it is a controller flag.
  # "off" removes the cache as a variable so the images-per-pod curve is about
  # the fetch pattern alone; it is not the shipped default, which is on.
  #
  # The admission path honours --imageVerifyCacheEnabled (ivpol engine.go:513,
  # fed from main.go's ImageVerifyCacheClient). Only the CLI dry-run and the
  # MutateDigest paths hardcode the cache off, and neither is measured here.
  local cache_enabled
  case "$CACHE_MODE" in
    off)            cache_enabled=false ;;
    warm|postedit)  cache_enabled=true  ;;
    *)              echo "FATAL: CACHE_MODE=$CACHE_MODE is not one of off, warm, postedit."
                    exit 1 ;;
  esac

  local reports_enabled
  case "$REPORTS" in
    on)  reports_enabled=true ;;
    off) reports_enabled=false ;;
    *)   echo "FATAL: REPORTS=$REPORTS is not one of on, off"; exit 1 ;;
  esac

  local chart_args=()
  if [[ -n "$KYVERNO_CHART_PATH" ]]; then
    [[ -f "$KYVERNO_CHART_PATH/Chart.yaml" ]] \
      || { echo "FATAL: no chart at $KYVERNO_CHART_PATH"; exit 1; }
    [[ -n "$KYVERNO_IMAGE_TAG" ]] \
      || { echo "FATAL: KYVERNO_CHART_PATH set without KYVERNO_IMAGE_TAG."
           echo "       A chart taken from an RC tag still declares the previous release's"
           echo "       appVersion, so this would install the wrong image and the run would"
           echo "       measure a version it did not claim to."; exit 1; }
    chart_args=("$KYVERNO_CHART_PATH")
    echo "  chart: local $KYVERNO_CHART_PATH"
  else
    chart_args=(kyverno/kyverno --version "$KYVERNO_CHART")
  fi
  local image_args=()
  if [[ -n "$KYVERNO_IMAGE_TAG" ]]; then
    image_args+=(--set "admissionController.container.image.tag=$KYVERNO_IMAGE_TAG")
    echo "  image tag override: $KYVERNO_IMAGE_TAG"
  fi
  if [[ -n "$KYVERNO_IMAGE_REGISTRY" ]]; then
    image_args+=(--set "admissionController.container.image.registry=$KYVERNO_IMAGE_REGISTRY")
    echo "  image registry override: $KYVERNO_IMAGE_REGISTRY"
  fi

  apply_kyverno_crds "${chart_args[@]}"

  # crds.install=false here, CRDs applied separately above: the CRD templates
  # alone render past 5MB for this chart, and with the CA bundle on top of that
  # in the tracked release, the gzipped, base64'd release Secret clears the
  # Kubernetes 1MiB object limit. The CRDs are identical either way; only
  # whether Helm tracks them in its release record changes.
  #
  # ${arr[@]+"${arr[@]}"} rather than "${arr[@]}": macOS ships bash 3.2, where
  # expanding an empty array under `set -u` is an unbound-variable error. The
  # override arrays are empty on the normal published-chart path, which is the
  # one the main ladder uses.
  helm install kyverno "${chart_args[@]}" \
    -n kyverno --create-namespace \
    -f "$VALUES" \
    ${image_args[@]+"${image_args[@]}"} \
    --set-string "admissionController.container.extraArgs.imageVerifyCacheEnabled=$cache_enabled" \
    --set "features.admissionReports.enabled=$reports_enabled" \
    --set-file "admissionController.caCertificates.data=$KEY_DIR/ca-bundle.pem" \
    --set crds.install=false \
    --wait --timeout 10m >/dev/null
  kubectl -n kyverno rollout status deploy/kyverno-admission-controller --timeout=300s
  assert_running_image
  assert_cache_mode "$cache_enabled"
  assert_reports_mode "$reports_enabled"
  assert_registry_trust
  kubectl create ns "$TEST_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  apply_policy
  assert_policy_matches_template
  verify_ivpol_active
  # Cheapest shape, just to find the point where it is serving at all.
  wait_until_serving "$(echo $IMAGES | awk '{print $1}')"
}


touch_policy() {
  # postedit: bump an annotation so the policy's resourceVersion changes. The
  # verification cache keys on it (pkg/image/verification/cache/client.go
  # generateKey), so this invalidates every entry for the policy without
  # changing what the policy does. That is the production gotcha the cache
  # chart exists to show: a routine edit silently returns you to the cold path.
  #
  # Uses $IVPOL_NAME, set by apply_policy from the rendered policy's actual
  # metadata.name, rather than the single hardcoded ivpol-bench-cosign-key —
  # every template now has its own name.
  [[ -n "${IVPOL_NAME:-}" ]] || { echo "FATAL: touch_policy called before apply_policy set IVPOL_NAME."; exit 1; }
  kubectl annotate ivpol "$IVPOL_NAME" \
    "bench.kyverno.io/touched-at=$(date -u +%s)" --overwrite >/dev/null
  echo "policy $IVPOL_NAME annotated — resourceVersion bumped, cache entries invalidated"
}

# ---------------------------------------------------------------- cells

run_cell() {
  local images="$1" delay="$2"
  local cell="img${images}-d${delay}-${CACHE_MODE}"

  # Fixed arrival rate (TARGET_RPS). With pooled TLS connections the pod's
  # ephemeral-port budget no longer binds, so there is no connection-cost
  # formula to fit here; gate (e) below checks the result instead of a cap
  # trying to prevent it.
  local rps="$TARGET_RPS"
  echo "arrival rate: ${rps} req/s"
  local dir="$RESULTS_DIR/$cell"; mkdir -p "$dir"
  echo
  echo "==================== cell $cell ===================="

  # Sampled before anything this cell does — including the discarded warm-up,
  # which runs the same shape against the same registry condition — so a
  # restart caused by an earlier cell or the pre-cell serving burst is never
  # credited to this one.
  local cell_t0_epoch restarts_t0
  cell_t0_epoch=$(date +%s)
  restarts_t0=$(get_restart_count)

  registry_ctl delay "$delay" >/dev/null

  # Discarded warm-up, before anything is measured. Changing the delay restarts
  # the proxy pod, which kills the keep-alive connections Kyverno holds to the
  # old pod's IP; requests issued into that window fail while they are
  # re-established. Measured straight after a restart, the first cell of a slot
  # showed 0.15% failures on one version and 93% on another - the same artifact,
  # differing only by timing, and the second one aborted a six-slot run.
  # A short burst absorbs it, and its results are thrown away.
  echo "warm-up (discarded): ${WARMUP:-10s}"
  IMAGES_PER_POD="$images" REGISTRY_HOST="$REGISTRY_HOST" REPO="$REPO" TEST_NAMESPACE="$TEST_NS" \
    TARGET_RPS="$rps" DURATION="${WARMUP:-10s}" \
    "$K6" run "$SCRIPT_JS" --quiet >/dev/null 2>&1 || true
  kubectl delete pods -n "$TEST_NS" --all --ignore-not-found --wait=false >/dev/null 2>&1 || true

  [[ "$CACHE_MODE" == "postedit" ]] && touch_policy
  registry_ctl reset >/dev/null

  local topcsv="$dir/kubectl-top.csv"; echo "timestamp,pod,cpu,memory" > "$topcsv"
  ( while true; do ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
      kubectl top pod -n kyverno --no-headers 2>/dev/null | awk -v ts="$ts" '{print ts","$1","$2","$3}' >> "$topcsv"
      sleep 2; done ) & local SPID=$!

  # The script's own constant-arrival-rate scenario does the pacing, driven by
  # TARGET_RPS and DURATION below. No --vus/--duration here: those would define a
  # second scenario and k6 refuses the combination.
  local load_flags=()
  local t0 t1
  t0=$(date +%s)
  IMAGES_PER_POD="$images" REGISTRY_HOST="$REGISTRY_HOST" REPO="$REPO" TEST_NAMESPACE="$TEST_NS" \
  TARGET_RPS="$rps" DURATION="$LOAD" PREALLOC_VUS="${PREALLOC_VUS:-50}" \
  KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}" "$K6" run "$SCRIPT_JS" \
    ${load_flags[@]+"${load_flags[@]}"} \
    --summary-trend-stats "$TREND_STATS" \
    --summary-export "$dir/k6-summary.json" 2>&1 | tee "$dir/k6-output.log"
  t1=$(date +%s)
  kill "$SPID" 2>/dev/null || true
  local window=$(( t1 - t0 )); (( window < 30 )) && window=30

  assert_cell_valid "$dir" "$images" "$restarts_t0" "$cell_t0_epoch"

  ( cd "$dir" && KYVERNO_PODS_RANGE="$window" KYVERNO_PODS_STEP=5 \
      bash "$RES_REPORT" "$PROM_LOCAL" ) >/dev/null 2>&1 || echo "WARN: resource report failed"

  # Kyverno's own admission-review duration is the headline, not k6's
  # iteration_duration. Measured against the archived cells they differ by about
  # 2x - 148ms vs 340ms at eight images - because an iteration also carries the
  # apiserver, etcd, and a fresh Kubernetes client built per iteration in the
  # workload. Both are recorded; only this one answers "what does verification
  # cost".
  #
  # The window is the cell's own duration rather than a fixed [5m], which would
  # reach back over the warm-up and the serving probe and average them in.
  local win_range="${window}s"
  local adm_avg adm_p99
  adm_avg=$(promq "sum(rate(kyverno_admission_review_duration_seconds_sum[$win_range]))/sum(rate(kyverno_admission_review_duration_seconds_count[$win_range]))")
  adm_p99=$(promq "histogram_quantile(0.99, sum(rate(kyverno_admission_review_duration_seconds_bucket[$win_range])) by (le))")

  {
    echo "cell=$cell"
    echo "images_per_pod=$images"
    echo "registry_delay_ms=$delay"
    echo "cache_mode=$CACHE_MODE"
    echo "reports_mode=$REPORTS"
    echo "policy_template=$(basename "$POLICY_TEMPLATE")"
    echo "verify_repo=${VERIFY_REPO:-$REPO}"
    echo "kyverno_version=$KYVERNO_VERSION"
    echo "kyverno_chart=${KYVERNO_CHART_PATH:-$KYVERNO_CHART}"
    echo "kyverno_image=$(cat "$RESULTS_DIR/running-image.txt" 2>/dev/null)"
    echo "vus=$VUS"; echo "load=$LOAD"
    echo "window_sec=$window"
    echo "kyverno_admission_review_duration_avg_sec=${adm_avg:-}"
    echo "kyverno_admission_review_duration_p99_sec=${adm_p99:-}"
    # Delta since this cell's t0 (restarts.txt), not a fresh cumulative snapshot —
    # a cumulative count here would carry every earlier cell's crash forever.
    echo "pod_restarts=$(cat "$dir/restarts.txt" 2>/dev/null || echo "")"
    echo "pod_restarts_cumulative=$(cat "$dir/restarts-cumulative.txt" 2>/dev/null || echo "")"
    echo "ivpol_present=$(kubectl get ivpol --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    shasum -a 256 "$POLICY_TEMPLATE"
    # The actual applied bytes, not just the unrendered template — this is
    # what the cluster enforced, keys/cert chain included.
    [[ -n "${RENDERED_POLICY_PATH:-}" && -f "$RENDERED_POLICY_PATH" ]] && shasum -a 256 "$RENDERED_POLICY_PATH"
  } > "$dir/meta.txt"
  echo "archived -> $dir"

  kubectl delete pods -n "$TEST_NS" --all --ignore-not-found --wait=false >/dev/null 2>&1 || true
  echo "cooldown ${CELL_COOLDOWN}s ..."; sleep "$CELL_COOLDOWN"
}

setup_kyverno
for delay in $DELAYS; do
  for images in $IMAGES; do
    run_cell "$images" "$delay"
  done
done
echo
echo "=== ivpol run complete. Cells under $RESULTS_DIR/ ==="

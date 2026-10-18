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
# The ivpol evaluator constructs its compiler with
# imageverifycache.DisabledImageVerifyCache() unconditionally
# (pkg/image/verification/evaluator/eval.go:46, identical at v1.19.0 and
# v1.19.1), so the verification cache is never consulted on this path and the
# --imageVerifyCacheEnabled flag changes nothing here. Confirmed from the
# archived counters: 7251 signature blob fetches for 904 requests x 8 images =
# 7232, one fetch per image, no hits.
#
# The setting is kept only so an existing invocation does not break, and is
# asserted to be "off" because any other value would imply a measurement the
# code cannot produce.
CACHE_MODE="${CACHE_MODE:-off}"
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

render_policy() {
  # The public key is indented into the YAML block scalar. Done with python
  # rather than sed because the key is multi-line and must keep its exact bytes:
  # a mangled key fails verification, and a cell that fails verification is
  # measuring the error path at full speed.
  local out="$1"
  python3 - "$POLICY_TEMPLATE" "$KEY_DIR/cosign.pub" "$out" <<'PY'
import sys, pathlib
template, keyfile, out = sys.argv[1:4]
key = pathlib.Path(keyfile).read_text().rstrip("\n")
indented = "\n".join("            " + line for line in key.splitlines())
text = pathlib.Path(template).read_text()
# Exactly one occurrence, not at least one. A second one - in a comment
# describing the placeholder, for instance - would also be substituted, splicing
# a multi-line key into a comment and producing a manifest that does not parse.
occurrences = text.count("PUBLIC_KEY_PLACEHOLDER")
if occurrences != 1:
    sys.exit(f"policy template has {occurrences} PUBLIC_KEY_PLACEHOLDER tokens, expected exactly 1")
rendered = text.replace("PUBLIC_KEY_PLACEHOLDER", indented)
pathlib.Path(out).write_text(rendered)
# Parse what will actually be applied. A policy that does not round-trip is a
# cell that fails for a reason unrelated to the thing being measured.
try:
    import yaml
except ImportError:
    pass
else:
    doc = yaml.safe_load(rendered)
    got = doc["spec"]["attestors"][0]["cosign"]["key"]["data"].strip()
    if not (got.startswith("-----BEGIN PUBLIC KEY-----") and got.endswith("-----END PUBLIC KEY-----")):
        sys.exit("rendered policy does not carry an intact public key")
PY
}

apply_policy() {
  local rendered="$RESULTS_DIR/ivpol-rendered.yaml"
  render_policy "$rendered"
  kubectl apply -f "$rendered" >/dev/null
  kubectl wait --for=condition=Ready ivpol/ivpol-bench-cosign-key --timeout=120s >/dev/null 2>&1 \
    || echo "WARN: ivpol did not report Ready; controls below decide whether it is actually enforcing"
  echo "applied $(kubectl get ivpol --no-headers 2>/dev/null | wc -l | tr -d ' ') ImageValidatingPolicy"
}

# ---------------------------------------------------------------- gates

pod_json() {
  # $1 = image ref, $2 = pod name
  printf '{"apiVersion":"v1","kind":"Pod","metadata":{"name":"%s","namespace":"%s"},"spec":{"containers":[{"name":"c","image":"%s","command":["sh","-c","sleep 1"]}]}}' \
    "$2" "$TEST_NS" "$1"
}

verify_ivpol_active() {
  # Negative control: an image the policy matches but which carries no valid
  # signature MUST be rejected. If it is admitted, ivpol is not on the admission
  # path and every latency number in this slot would be measuring an empty
  # pipeline. This is the gate that catches a mis-scoped matchImageReferences,
  # a policy that failed to compile, or a webhook that never registered.
  local unsigned="$REGISTRY_HOST/$REPO:unsigned-control"
  if pod_json "$unsigned" "ivpol-neg-control" | kubectl create --dry-run=server -f - >/tmp/ivpol-neg.out 2>&1; then
    echo "FATAL: negative control was ADMITTED — an unsigned image passed ivpol."
    sed 's/^/  negctl: /' /tmp/ivpol-neg.out
    exit 1
  fi
  echo "negative control OK: unsigned image rejected"

  # Positive control: a correctly signed image MUST be admitted. Without this,
  # a policy that rejects everything - wrong key, unreachable registry - would
  # pass the negative control and look like a working, very fast cell.
  local signed="$REGISTRY_HOST/$REPO:v1"
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
    return 0
  fi
  echo "FATAL: still failing after $budget bursts; the controller never reached a"
  echo "       state where it could serve the workload. Measuring now would record"
  echo "       the failure path, not verification."
  exit 1
}

assert_cell_valid() {
  # $1 = cell dir, $2 = images per pod
  #
  # Three things have to be true for a cell to be a measurement of image
  # verification rather than a number that merely looks like one.
  local dir="$1" images="$2"

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
  local restarts
  restarts=$(kubectl -n kyverno get pods -l app.kubernetes.io/component=admission-controller \
    -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{"\n"}{end}' 2>/dev/null \
    | awk '{s+=$1} END {print s+0}')
  echo "$restarts" > "$dir/restarts.txt"
  echo "admission controller restarts so far: $restarts"
  # A restart count without a cause cannot tell a data race from an OOM kill,
  # and the cluster is deleted at the end of the slot, so capture it now.
  if (( restarts > 0 )); then
    kubectl -n kyverno logs deploy/kyverno-admission-controller --previous --tail=200 \
      > "$dir/crash-previous.log" 2>&1 || true
    kubectl -n kyverno get pods -l app.kubernetes.io/component=admission-controller \
      -o jsonpath='{range .items[*]}{.status.containerStatuses[0].lastState.terminated.reason}{" exit="}{.status.containerStatuses[0].lastState.terminated.exitCode}{"\n"}{end}' \
      > "$dir/crash-reason.txt" 2>&1 || true
    sed 's/\x1b\[[0-9;]*m//g' "$dir/crash-previous.log" 2>/dev/null \
      | grep -iE "fatal error|panic:|concurrent map" | head -3 > "$dir/crash-signature.txt" || true
    echo "  crash reason: $(tr '\n' ' ' < "$dir/crash-reason.txt" 2>/dev/null)"
    [[ -s "$dir/crash-signature.txt" ]] && sed 's/^/  /' "$dir/crash-signature.txt"
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
      echo "FATAL: $conns connections against $total requests exceeds the 1-per-4 budget."
      echo "       Connections are not pooling; this would measure the rig, not Kyverno."
      echo "       A healthy cell that trips this is a finding about Kyverno's pooling,"
      echo "       not a reason to loosen the gate — escalate rather than widen it."
      exit 1
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
  [[ -n "$SKIP_INSTALL" ]] && { echo "SKIP_INSTALL set — reusing existing install"; return 0; }
  echo "==> installing Kyverno $KYVERNO_VERSION (chart $KYVERNO_CHART), cache mode $CACHE_MODE"
  helm uninstall kyverno -n kyverno >/dev/null 2>&1 || true
  kubectl delete ns kyverno --ignore-not-found --wait=true >/dev/null 2>&1 || true

  # Cache mode is an install-time property because it is a controller flag.
  # "off" is the headline sweep's state: it removes the cache as a variable so
  # the images-per-pod curve is about the fetch pattern alone.
  if [[ "$CACHE_MODE" != "off" ]]; then
    echo "FATAL: CACHE_MODE=$CACHE_MODE, but the ivpol path disables the verification"
    echo "       cache in code (evaluator/eval.go:46). A warm or post-edit cell would"
    echo "       be labelled as something the engine cannot do."
    exit 1
  fi
  local cache_enabled=false

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
    --set-file "admissionController.caCertificates.data=$KEY_DIR/ca-bundle.pem" \
    --set crds.install=false \
    --wait --timeout 10m >/dev/null
  kubectl -n kyverno rollout status deploy/kyverno-admission-controller --timeout=300s
  assert_running_image
  assert_cache_mode "$cache_enabled"
  assert_registry_trust
  kubectl create ns "$TEST_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  apply_policy
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
  kubectl annotate ivpol ivpol-bench-cosign-key \
    "bench.kyverno.io/touched-at=$(date -u +%s)" --overwrite >/dev/null
  echo "policy annotated — resourceVersion bumped, cache entries invalidated"
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

  assert_cell_valid "$dir" "$images"

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
    echo "kyverno_version=$KYVERNO_VERSION"
    echo "kyverno_chart=${KYVERNO_CHART_PATH:-$KYVERNO_CHART}"
    echo "kyverno_image=$(cat "$RESULTS_DIR/running-image.txt" 2>/dev/null)"
    echo "vus=$VUS"; echo "load=$LOAD"
    echo "window_sec=$window"
    echo "kyverno_admission_review_duration_avg_sec=${adm_avg:-}"
    echo "kyverno_admission_review_duration_p99_sec=${adm_p99:-}"
    echo "pod_restarts=$(kubectl -n kyverno get pods -l app.kubernetes.io/component=admission-controller -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{" "}{end}' 2>/dev/null)"
    echo "ivpol_present=$(kubectl get ivpol --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    shasum -a 256 "$POLICY_TEMPLATE"
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

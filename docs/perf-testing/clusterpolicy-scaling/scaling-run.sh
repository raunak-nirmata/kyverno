#!/usr/bin/env bash
# Matrix driver for the Kyverno ClusterPolicy scaling reproduction (one version).
#
# Reproduces the "ClusterPolicy (pre-v1.14)" table from
# https://kyverno.io/docs/installation/scaling/ : 17 PSS ClusterPolicies,
# Validate / Enforce / Pods, across replicas {1,3} x VUs/Iterations
# {100/1000, 200/5000, 500/10000}. Runs the unchanged kyverno xk6 workload
# (kubernetes.create into testns on the KWOK node) and captures, per cell: k6
# iteration_duration stats, Prometheus CPU/mem over the exact cell window, a
# kubectl-top cross-check, Kyverno's own admission-latency metric, and etcd size.
#
# Prereq: bash setup-mac.sh (cluster + KWOK + Prometheus + metrics-server).
#
# Usage:
#   bash scaling-run.sh
#   REPLICAS="1" MATRIX="100:1000" bash scaling-run.sh              # smoke one cell
#   SKIP_INSTALL=1 SCRIPT_JS=.../script.js bash scaling-run.sh      # reuse an existing install (deny pass)
#
# ladder-run.sh drives this once per (round, version) slot: first the admit pass
# (installs Kyverno for KYVERNO_VERSION/CHART), then the deny pass with
# SKIP_INSTALL=1 to reuse that same install.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
KYVERNO_REPO="$(cd "$HERE/../../.." && pwd)"        # .../kyverno
SCRIPT_JS="${SCRIPT_JS:-$HERE/script-admit.js}"
RES_REPORT="${RES_REPORT:-$KYVERNO_REPO/scripts/kyverno-pods-resources-report.sh}"
ETCD_REPORT="${ETCD_REPORT:-$KYVERNO_REPO/scripts/etcd-storage-report.sh}"
K6="${K6:-$HERE/k6-xk6}"
POLICY_FILE="${POLICY_FILE:-$HERE/policies/pss-clusterpolicies.yaml}"
VALUES="${VALUES:-$HERE/kyverno-scaling-values.yaml}"
# Policy kind to count/wait on: cpol (ClusterPolicy) for the legacy runs.
READY_KIND="${READY_KIND:-cpol}"
# How verify_policies_active() proves the policies are on the admission path:
#   validate -> a PSS-violating Pod MUST be rejected (deny/validate scenarios)
POLICY_MODE="${POLICY_MODE:-validate}"

KYVERNO_VERSION="${KYVERNO_VERSION:-1.13.6}"
KYVERNO_CHART="${KYVERNO_CHART:-3.3.9}"             # chart for app 1.13.6
REPLICAS="${REPLICAS:-1 3}"
MATRIX="${MATRIX:-100:1000 200:5000 500:10000}"    # vus:iterations
COOLDOWN="${COOLDOWN:-120}"                         # seconds between cells; > the 1m rate window so the prior CPU peak fully decays
# When set, skip the Kyverno install + policy apply and reuse whatever is already
# on the cluster (used for the deny pass, which runs right after the admit pass on
# the same slot). The negative control still runs so we never measure an empty pipeline.
SKIP_INSTALL="${SKIP_INSTALL:-}"
TREND_STATS="avg,min,med,max,p(90),p(95),p(99)"
RESULTS_DIR="${RESULTS_DIR:-$HERE/results/scaling}"
PROM_LOCAL="http://localhost:9090"

export KYVERNO_NAMESPACE=kyverno
mkdir -p "$RESULTS_DIR"

[[ -x "$K6" ]] || { echo "missing k6 build: $K6 (run: xk6 build --with github.com/grafana/xk6-kubernetes@v0.9.0 --output $K6)"; exit 1; }
echo "pinned policy sha256:"; shasum -a 256 $POLICY_FILE

helm repo add kyverno https://kyverno.github.io/kyverno/ >/dev/null 2>&1 || true
helm repo update kyverno >/dev/null 2>&1 || true

# --- Prometheus port-forward (kept alive for the whole run) ---
PF_PID=""
start_prom_pf() {
  pkill -f "port-forward svc/kube-prometheus-stack" 2>/dev/null || true; sleep 1
  kubectl -n monitoring rollout status statefulset/prometheus-kube-prometheus-stack-prometheus --timeout=180s >/dev/null 2>&1 || true
  kubectl -n monitoring port-forward svc/kube-prometheus-stack-prometheus 9090:9090 >/tmp/prom-pf.log 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 60); do
    curl -sf "$PROM_LOCAL/-/ready" >/dev/null 2>&1 && { echo "Prometheus port-forward ready (pid $PF_PID)"; return; }
    sleep 1
  done
  echo "WARN: Prometheus not reachable on $PROM_LOCAL; resource reports may be empty"
}
cleanup() { [[ -n "$PF_PID" ]] && kill "$PF_PID" 2>/dev/null || true; pkill -f "port-forward svc/kube-prometheus-stack" 2>/dev/null || true; }
trap cleanup EXIT
start_prom_pf

# Prometheus instant query helper -> prints the scalar value (or empty). `|| true`
# so a transient Prometheus outage never fails the pipeline under set -o pipefail.
promq() { { curl -sS --max-time 15 "$PROM_LOCAL/api/v1/query" --data-urlencode "query=$1" 2>/dev/null | jq -r '.data.result[0].value[1] // empty' 2>/dev/null; } || true; }

verify_policies_active() {
  # Negative control: a Pod that VIOLATES restricted PSS MUST be rejected by the
  # admission webhook (server-side dry-run runs the full chain without persisting).
  # If it is ADMITTED, the policies are not on the admission path and every latency
  # number would be measuring an empty pipeline. Abort instead.
  local present="${1:-0}"
  local violating='{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pss-neg-control","namespace":"testns"},"spec":{"containers":[{"name":"c","image":"busybox","command":["sh","-c","sleep 1"]}]}}'
  if echo "$violating" | kubectl create --dry-run=server -f - >/tmp/negctl.out 2>&1; then
    echo "FATAL: negative control was ADMITTED — a PSS-violating Pod passed admission."
    echo "       Policies ($READY_KIND, present=$present) are NOT enforcing on the admission path."
    sed 's/^/  negctl: /' /tmp/negctl.out
    exit 1
  fi
  echo "negative control OK: a PSS-violating Pod is rejected ($READY_KIND present=$present, policies active)"
}

setup_kyverno() {
  if [[ -n "$SKIP_INSTALL" ]]; then
    echo "--- SKIP_INSTALL set: reusing existing Kyverno install + policies ---"
    local present
    present=$(kubectl get "$READY_KIND" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    echo "$READY_KIND present: $present"
    verify_policies_active "$present"
    return
  fi
  # Install Kyverno once with --no-hooks (the chart's clean-reports post-hook Job
  # hangs on kind and is irrelevant to a benchmark), then apply the 17 policies.
  # The replica change between legs is done with kubectl scale, not helm upgrade.
  helm uninstall kyverno -n kyverno --no-hooks >/dev/null 2>&1 || true
  if [[ -n "${LOCAL_WT:-}" && -n "${LOCAL_TAG:-}" ]]; then
    # Local build: install the IN-REPO chart with local ko images. 1.20-dev hard-blocks
    # legacy ClusterPolicy create, so open the escape hatch (--blockLegacyPolicyAPIs=false).
    local ky=github.com/kyverno/kyverno/cmd/kyverno init=github.com/kyverno/kyverno/cmd/kyverno-init
    echo "--- installing LOCAL build $KYVERNO_VERSION (tag ${LOCAL_TAG:0:12}, in-repo chart), replicas=1 ---"
    kind load docker-image "ko.local/$ky:$LOCAL_TAG" "ko.local/$init:$LOCAL_TAG" --name "$(kubectl config current-context | sed 's/^kind-//')" >/dev/null 2>&1 || true
    helm dependency build "$LOCAL_WT/charts/kyverno" >/dev/null 2>&1 || helm dependency update "$LOCAL_WT/charts/kyverno" >/dev/null 2>&1
    helm install kyverno "$LOCAL_WT/charts/kyverno" \
      --namespace kyverno --create-namespace --no-hooks \
      -f "$VALUES" --set admissionController.replicas=1 \
      --set features.blockLegacyPolicyAPIs.enabled=false \
      --set admissionController.container.image.registry=ko.local \
      --set admissionController.container.image.repository=$ky \
      --set admissionController.container.image.tag="$LOCAL_TAG" \
      --set admissionController.container.image.pullPolicy=IfNotPresent \
      --set admissionController.initContainer.image.registry=ko.local \
      --set admissionController.initContainer.image.repository=$init \
      --set admissionController.initContainer.image.tag="$LOCAL_TAG" \
      --set admissionController.initContainer.image.pullPolicy=IfNotPresent \
      --wait --timeout 10m
  else
    echo "--- installing Kyverno $KYVERNO_VERSION (chart $KYVERNO_CHART), replicas=1 ---"
    helm install kyverno kyverno/kyverno --version "$KYVERNO_CHART" \
      --namespace kyverno --create-namespace --no-hooks \
      -f "$VALUES" --set admissionController.replicas=1 \
      --wait --timeout 10m
  fi
  kubectl -n kyverno rollout status deployment/kyverno-admission-controller --timeout=5m
  echo "--- applying policies ($POLICY_FILE) ---"
  # Retry: for a few seconds after install Kyverno's policy webhook exists but the
  # pod isn't serving it yet, so an apply can hit "connection refused".
  for pf in $POLICY_FILE; do
    for attempt in $(seq 1 20); do
      if kubectl apply -f "$pf" 2>/tmp/apply.err; then break; fi
      if [[ $attempt -lt 20 ]] && grep -qiE 'connection refused|failed calling webhook|no endpoints' /tmp/apply.err; then
        echo "  policy webhook not ready (attempt $attempt/20), retrying in 5s..."; sleep 5; continue
      fi
      echo "FATAL: could not apply $pf"; sed 's/^/  /' /tmp/apply.err; exit 1
    done
  done
  kubectl wait --for=condition=Ready --timeout=180s "$READY_KIND" --all >/dev/null 2>&1 || \
    echo "note: '$READY_KIND' exposes no Ready condition to wait on; relying on the negative control"
  local present
  present=$(kubectl get "$READY_KIND" --no-headers 2>/dev/null | wc -l | tr -d ' ')
  echo "$READY_KIND present: $present"
  verify_policies_active "$present"
}

set_replicas() {
  local reps="$1"
  echo "--- scaling admission controller to $reps replica(s) ---"
  kubectl -n kyverno scale deployment/kyverno-admission-controller --replicas="$reps"
  kubectl -n kyverno rollout status deployment/kyverno-admission-controller --timeout=5m || \
    echo "WARN: rollout to $reps did not fully settle in 5m; continuing"
  local got
  got=$(kubectl -n kyverno get deploy kyverno-admission-controller -o jsonpath='{.status.readyReplicas}')
  echo "admission-controller readyReplicas=$got (expected $reps)"
  # Let the fresh replicas warm up and let the rate(...[1m]) window fill entirely
  # with post-scale samples before measuring.
  sleep 120
}

run_cell() {
  local reps="$1" vus="$2" iters="$3"
  local cell="r${reps}-${vus}x${iters}"
  local dir="$RESULTS_DIR/$cell"; mkdir -p "$dir"
  echo
  echo "==================== cell $cell ===================="

  # kubectl-top sampler (2s) as a rate-free cross-check for CPU/mem max
  local topcsv="$dir/kubectl-top.csv"; echo "timestamp,pod,cpu,memory" > "$topcsv"
  ( while true; do ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
      kubectl top pod -n kyverno --no-headers 2>/dev/null | awk -v ts="$ts" '{print ts","$1","$2","$3}' >> "$topcsv"
      sleep 2; done ) & local SPID=$!

  local load_flags
  if [[ "$iters" =~ ^[0-9]+s$ ]]; then
    load_flags=(--vus "$vus" --duration "$iters")
  else
    load_flags=(--vus "$vus" --iterations "$iters")
  fi
  local t0 t1
  t0=$(date +%s)
  KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}" "$K6" run "$SCRIPT_JS" \
    "${load_flags[@]}" \
    --summary-trend-stats "$TREND_STATS" \
    --summary-export "$dir/k6-summary.json" 2>&1 | tee "$dir/k6-output.log"
  t1=$(date +%s)
  kill "$SPID" 2>/dev/null || true
  local window=$(( t1 - t0 )); (( window < 30 )) && window=30
  echo "cell wall-clock window: ${window}s"

  # Prometheus CPU/mem over the EXACT cell window (no cross-cell contamination)
  ( cd "$dir" && KYVERNO_PODS_RANGE="$window" KYVERNO_PODS_STEP=5 \
      bash "$RES_REPORT" "$PROM_LOCAL" ) || echo "WARN: resource report failed"
  ( cd "$dir" && bash "$ETCD_REPORT" "$PROM_LOCAL" ) || echo "WARN: etcd report failed"

  local adm_avg adm_p99 adm_reqs
  adm_avg=$(promq 'sum(rate(kyverno_admission_review_duration_seconds_sum[5m]))/sum(rate(kyverno_admission_review_duration_seconds_count[5m]))')
  adm_p99=$(promq 'histogram_quantile(0.99, sum(rate(kyverno_admission_review_duration_seconds_bucket[5m])) by (le))')
  adm_reqs=$(promq 'sum(kyverno_admission_requests_total)')

  {
    echo "cell=$cell"; echo "replicas=$reps"
    echo "ready_replicas=$(kubectl -n kyverno get deploy kyverno-admission-controller -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
    echo "pod_restarts=$(kubectl -n kyverno get pods -l app.kubernetes.io/component=admission-controller -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{" "}{end}' 2>/dev/null)"
    echo "vus=$vus"; echo "iterations=$iters"
    echo "kyverno_version=$KYVERNO_VERSION"; echo "kyverno_chart=$KYVERNO_CHART"
    echo "window_sec=$window"
    echo "kyverno_admission_review_duration_avg_sec=${adm_avg:-}"
    echo "kyverno_admission_review_duration_p99_sec=${adm_p99:-}"
    echo "kyverno_admission_requests_total=${adm_reqs:-}"
    shasum -a 256 $POLICY_FILE
    echo "policy_kind=$READY_KIND"
    echo "policies_present=$(kubectl get "$READY_KIND" --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    echo "pods_persisted=$(kubectl get pods -n testns --no-headers 2>/dev/null | wc -l | tr -d ' ')"
  } > "$dir/meta.txt"
  echo "archived -> $dir"

  # Clean testns between cells so persisted pods (admit path) do not accumulate.
  kubectl delete pods -n testns --all --ignore-not-found --wait=false >/dev/null 2>&1 || true

  echo "cooldown ${COOLDOWN}s ..."; sleep "$COOLDOWN"
}

setup_kyverno
for reps in $REPLICAS; do
  set_replicas "$reps"
  for pair in $MATRIX; do
    vus="${pair%%:*}"; iters="${pair##*:}"
    run_cell "$reps" "$vus" "$iters"
  done
done

echo
echo "=== scaling run complete. Cells under $RESULTS_DIR/ ==="

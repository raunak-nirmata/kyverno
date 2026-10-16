#!/usr/bin/env bash
# Benchmark registry for the ivpol arm: an in-cluster registry behind a latency
# proxy, holding N distinct cosign-signed images.
#
# Why in-cluster rather than the usual kind host-side registry: Kyverno verifies
# images by making its own HTTP calls from the admission controller pod. A
# host-side registry reached through containerd mirroring would serve the
# kubelet, not Kyverno, so the thing being measured would never touch it. Putting
# the registry in the cluster gives Kyverno a resolvable address
# (registry.bench-registry.svc.cluster.local:5000) and puts the proxy on the path
# that actually matters.
#
# Images are built FROM scratch with one distinct file each, so every tag has its
# own digest and forces its own manifest fetch. They are never pulled: pods land
# on the KWOK fake node, which fakes the kubelet. Tiny is the only requirement.
#
# Usage:
#   bash setup-registry.sh                 # deploy + build/sign/push IMAGE_COUNT images
#   IMAGE_COUNT=16 bash setup-registry.sh
#   ACTION=delay DELAY_MS=100 bash setup-registry.sh   # change injected latency
#   ACTION=stats bash setup-registry.sh               # read the request counters
#   ACTION=reset bash setup-registry.sh               # zero the counters
#   ACTION=delete bash setup-registry.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
NS="${NS:-bench-registry}"
IMAGE_COUNT="${IMAGE_COUNT:-16}"
DELAY_MS="${DELAY_MS:-0}"
ACTION="${ACTION:-install}"
REPO="${REPO:-bench/signed}"
# What Kyverno resolves. Must match the policy's matchImageReferences glob.
IN_CLUSTER_HOST="${IN_CLUSTER_HOST:-registry.${NS}.svc.cluster.local:5000}"
# Host-side port-forward used only to push and sign. The registry host differs
# from the in-cluster one, but the repository path is identical, and cosign
# stores a signature in the same repository as its subject - so a signature
# pushed through the forward is the one Kyverno finds in cluster.
PUSH_PORT="${PUSH_PORT:-5999}"
KEY_DIR="${KEY_DIR:-$HERE/keys}"
export COSIGN_PASSWORD="${COSIGN_PASSWORD:-}"

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing required command: $1"; exit 1; }; }

stats_url() { echo "http://127.0.0.1:${STATS_PORT:-5998}/stats"; }

case "$ACTION" in
  delete)
    kubectl delete ns "$NS" --ignore-not-found --wait=false
    echo "deleted namespace $NS (keys kept in $KEY_DIR)"
    exit 0
    ;;
  delay)
    kubectl -n "$NS" set env deploy/registry-proxy "DELAY_MS=$DELAY_MS" >/dev/null
    kubectl -n "$NS" rollout status deploy/registry-proxy --timeout=120s
    echo "registry delay set to ${DELAY_MS}ms"
    exit 0
    ;;
  stats|reset)
    # Short-lived forward: the caller wants one reading, not a session.
    kubectl -n "$NS" port-forward svc/registry 5998:5001 >/tmp/regstats-pf.log 2>&1 &
    pf=$!
    trap 'kill $pf 2>/dev/null || true' EXIT
    for _ in $(seq 1 40); do curl -sf "$(stats_url)" >/dev/null 2>&1 && break; sleep 0.25; done
    if [[ "$ACTION" == "reset" ]]; then
      curl -sf -X POST "http://127.0.0.1:5998/reset" || { echo "FATAL: could not reset registry counters"; exit 1; }
      echo
    else
      curl -sf "$(stats_url)" || { echo "FATAL: could not read registry counters"; exit 1; }
      echo
    fi
    exit 0
    ;;
esac

need kubectl
need docker
need cosign

echo "==> namespace $NS"
kubectl create ns "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

echo "==> delay proxy source -> ConfigMap"
kubectl -n "$NS" create configmap registry-proxy-src \
  --from-file=delay-proxy.py="$HERE/delay-proxy.py" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

echo "==> registry + proxy"
kubectl -n "$NS" apply -f - <<YAML >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata:
  name: registry-backend
spec:
  replicas: 1
  selector: { matchLabels: { app: registry-backend } }
  template:
    metadata:
      labels: { app: registry-backend }
    spec:
      # The registry is part of the measurement rig, not the workload under
      # test. Keep it off the KWOK fake node, which cannot actually run it.
      nodeSelector: { kubernetes.io/os: linux }
      tolerations:
        - key: node-role.kubernetes.io/control-plane
          operator: Exists
          effect: NoSchedule
      containers:
        - name: registry
          image: registry:2
          ports: [ { containerPort: 5000 } ]
          env:
            - { name: REGISTRY_STORAGE_DELETE_ENABLED, value: "true" }
          resources:
            requests: { cpu: 100m, memory: 128Mi }
---
apiVersion: v1
kind: Service
metadata:
  name: registry-backend
spec:
  selector: { app: registry-backend }
  ports: [ { name: http, port: 5000, targetPort: 5000 } ]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: registry-proxy
spec:
  replicas: 1
  selector: { matchLabels: { app: registry-proxy } }
  template:
    metadata:
      labels: { app: registry-proxy }
    spec:
      nodeSelector: { kubernetes.io/os: linux }
      tolerations:
        - key: node-role.kubernetes.io/control-plane
          operator: Exists
          effect: NoSchedule
      containers:
        - name: proxy
          image: python:3.12-alpine
          command: ["python3", "/src/delay-proxy.py"]
          env:
            - { name: UPSTREAM_HOST, value: registry-backend }
            - { name: UPSTREAM_PORT, value: "5000" }
            - { name: LISTEN_PORT, value: "5000" }
            - { name: STATS_PORT, value: "5001" }
            - { name: DELAY_MS, value: "$DELAY_MS" }
          ports:
            - { containerPort: 5000 }
            - { containerPort: 5001 }
          volumeMounts: [ { name: src, mountPath: /src } ]
          resources:
            requests: { cpu: 100m, memory: 128Mi }
      volumes:
        - name: src
          configMap: { name: registry-proxy-src }
---
apiVersion: v1
kind: Service
metadata:
  name: registry
spec:
  selector: { app: registry-proxy }
  ports:
    - { name: http, port: 5000, targetPort: 5000 }
    - { name: stats, port: 5001, targetPort: 5001 }
YAML

kubectl -n "$NS" rollout status deploy/registry-backend --timeout=180s
kubectl -n "$NS" rollout status deploy/registry-proxy --timeout=180s

echo "==> cosign key pair"
mkdir -p "$KEY_DIR"
if [[ ! -f "$KEY_DIR/cosign.key" ]]; then
  ( cd "$KEY_DIR" && cosign generate-key-pair >/dev/null )
  echo "generated $KEY_DIR/cosign.pub"
else
  echo "reusing $KEY_DIR/cosign.pub"
fi

echo "==> port-forward for push (localhost:$PUSH_PORT)"
kubectl -n "$NS" port-forward svc/registry "$PUSH_PORT:5000" >/tmp/regpush-pf.log 2>&1 &
PF=$!
trap 'kill $PF 2>/dev/null || true' EXIT
for _ in $(seq 1 60); do
  curl -sf "http://127.0.0.1:$PUSH_PORT/v2/" >/dev/null 2>&1 && break
  sleep 0.5
done
curl -sf "http://127.0.0.1:$PUSH_PORT/v2/" >/dev/null 2>&1 \
  || { echo "FATAL: registry not reachable through port-forward on $PUSH_PORT"; exit 1; }

echo "==> build, push, sign $IMAGE_COUNT images"
BUILD="$(mktemp -d)"
trap 'kill $PF 2>/dev/null || true; rm -rf "$BUILD"' EXIT
for i in $(seq 1 "$IMAGE_COUNT"); do
  tag="v$i"
  # One distinct byte-string per image so every tag resolves to its own digest
  # and forces its own manifest fetch. Identical content would collapse them.
  printf 'ivpol-benchmark image %s\n' "$i" > "$BUILD/marker"
  cat > "$BUILD/Dockerfile" <<EOF
FROM scratch
COPY marker /marker
EOF
  docker build -q -t "127.0.0.1:$PUSH_PORT/$REPO:$tag" "$BUILD" >/dev/null
  docker push -q "127.0.0.1:$PUSH_PORT/$REPO:$tag" >/dev/null
  cosign sign --yes --tlog-upload=false \
    --key "$KEY_DIR/cosign.key" \
    "127.0.0.1:$PUSH_PORT/$REPO:$tag" >/dev/null 2>&1
  printf '  signed %s:%s\r' "$REPO" "$tag"
done
echo
echo "==> verifying one image through the same path Kyverno will use"
cosign verify --key "$KEY_DIR/cosign.pub" --insecure-ignore-tlog=true \
  "127.0.0.1:$PUSH_PORT/$REPO:v1" >/dev/null 2>&1 \
  || { echo "FATAL: signature does not verify; every cell would measure a failing policy"; exit 1; }

echo
echo "registry ready"
echo "  in-cluster:  $IN_CLUSTER_HOST/$REPO:v{1..$IMAGE_COUNT}"
echo "  public key:  $KEY_DIR/cosign.pub"
echo "  delay:       ${DELAY_MS}ms  (change: ACTION=delay DELAY_MS=25 bash $0)"

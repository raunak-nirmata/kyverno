#!/usr/bin/env bash
# Provision a single-machine ClusterPolicy scaling rig on a macOS host.
#
# This is the Mac-host adaptation of docs/perf-testing/provision/akamai-setup-cluster.sh:
# it keeps the parts that matter for the measurement (KinD + KWOK fake node +
# Prometheus + testns) and drops the amd64 tool downloads, using the host's own
# kind/kubectl/helm instead. Everything runs on the host through Docker Desktop;
# no separate VM is involved.
#
# The docs scaling page ran the k6 loader and the cluster on one machine, so the
# loader->apiserver path is loopback and loader/Kyverno share CPU. This rig is
# also single-machine, so it reproduces that shape; only core count/speed differ.
#
# Usage:
#   bash setup-mac.sh                 # create the cluster
#   ACTION=delete bash setup-mac.sh   # tear it down
#   CLUSTER=foo bash setup-mac.sh     # a named cluster (the ladder uses one per version slot)
set -euo pipefail

CLUSTER="${CLUSTER:-cpol-scaling}"
KIND_IMAGE="${KIND_IMAGE:-kindest/node:v1.31.6}"
KWOK_VERSION="${KWOK_VERSION:-v0.7.0}"
METRICS_SERVER_URL="${METRICS_SERVER_URL:-https://github.com/kubernetes-sigs/metrics-server/releases/download/v0.7.2/components.yaml}"
ACTION="${ACTION:-create}"

if [[ "$ACTION" == "delete" ]]; then
  kind delete cluster --name "$CLUSTER"
  exit 0
fi

echo "==> Creating KinD cluster '$CLUSTER' ($KIND_IMAGE) ..."
# 1 control-plane + 3 workers: the KWOK node is tainted, so the 3 real Kyverno
# admission replicas need 3 schedulable real nodes to spread across.
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
cat <<EOF | kind create cluster --name "$CLUSTER" --image "$KIND_IMAGE" --wait 5m --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
- role: worker
- role: worker
- role: worker
EOF

echo "==> Installing Prometheus stack (no grafana/alertmanager to fit host RAM) ..."
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
# Retried, because this is a network call inside a script run under `set -e`,
# and a ladder builds one cluster per version slot per round. A single transient
# failure here killed an entire multi-slot run at the first slot; the repo was
# reachable again seconds later. Three tries with a short backoff turns a lost
# run into a lost few seconds.
repo_updated=0
for attempt in 1 2 3; do
  if helm repo update prometheus-community >/dev/null 2>&1; then
    repo_updated=1
    break
  fi
  echo "  helm repo update failed (attempt $attempt/3); retrying in $((attempt * 5))s"
  sleep $((attempt * 5))
done
if [[ "$repo_updated" != "1" ]]; then
  echo "FATAL: could not refresh the prometheus-community chart repo after 3 attempts."
  echo "       Check network access to https://prometheus-community.github.io/helm-charts"
  exit 1
fi
helm upgrade --install --wait --timeout 15m \
  --namespace monitoring --create-namespace \
  kube-prometheus-stack prometheus-community/kube-prometheus-stack \
  --set prometheus.prometheusSpec.enableRemoteWriteReceiver=true \
  --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
  --set kubeEtcd.service.targetPort=2382 \
  --set grafana.enabled=false \
  --set alertmanager.enabled=false

echo "==> Installing metrics-server (kubectl top cross-check for CPU/mem max) ..."
kubectl apply -f "$METRICS_SERVER_URL"
kubectl -n kube-system patch deployment metrics-server --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]' || true
kubectl -n kube-system rollout status deployment/metrics-server --timeout=120s || \
  echo "WARN: metrics-server not ready; kubectl-top cross-check may be skipped"

echo "==> Installing KWOK $KWOK_VERSION ..."
kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/kwok.yaml"
kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/stage-fast.yaml"
kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/metrics-usage.yaml"

echo "==> Creating KWOK fake node (32 cpu / 256Gi / 1000 pods) ..."
kubectl create -f - <<EOF 2>/dev/null || echo "(kwok node already exists)"
apiVersion: v1
kind: Node
metadata:
  annotations:
    node.alpha.kubernetes.io/ttl: "0"
    kwok.x-k8s.io/node: fake
  labels:
    beta.kubernetes.io/arch: amd64
    beta.kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    kubernetes.io/os: linux
    kubernetes.io/role: agent
    node-role.kubernetes.io/agent: ""
    type: kwok
  generateName: kwok-node-
spec:
  taints:
  - effect: NoSchedule
    key: kwok.x-k8s.io/node
    value: fake
status:
  allocatable:
    cpu: "32"
    memory: 256Gi
    pods: "1000"
  capacity:
    cpu: "32"
    memory: 256Gi
    pods: "1000"
  nodeInfo:
    architecture: amd64
    bootID: ""
    containerRuntimeVersion: ""
    kernelVersion: ""
    kubeProxyVersion: fake
    kubeletVersion: fake
    machineID: ""
    operatingSystem: linux
    osImage: ""
    systemUUID: ""
  phase: Running
EOF

echo "==> Creating test namespace ..."
kubectl create ns testns 2>/dev/null || true

echo ""
echo "==> Setup complete. Cluster '$CLUSTER' is the current kube-context."
echo "    Prometheus: kubectl -n monitoring port-forward svc/kube-prometheus-stack-prometheus 9090:9090"
echo "    Next: bash scaling-run.sh   (or bash ladder-run.sh for the multi-version ladder)"

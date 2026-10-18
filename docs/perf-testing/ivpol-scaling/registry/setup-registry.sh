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
# distribution 3.x. Tried first as a fix for the signature-format problem
# described under COSIGN_BIN, on the theory that its Referrers API would let
# Kyverno find cosign v3's bundles - it did not, the referrers endpoint 404s
# here either way, and signing with cosign v2 is what actually fixed it. Kept at
# 3 only because it is the current release; 2 works identically for this
# benchmark.
REGISTRY_IMAGE="${REGISTRY_IMAGE:-registry:3}"
PROXY_IMAGE="${PROXY_IMAGE:-python:3.12-alpine}"
# Optional: the kind cluster to side-load the rig's images into. Every slot in a
# ladder is a fresh cluster, so without this each one pulls the same two images
# from Docker Hub again - slow, and enough repetitions to meet a rate limit
# partway through a run.
KIND_CLUSTER="${KIND_CLUSTER:-}"
# A fresh cluster pulling both images can take minutes on a cold cache. The old
# 180s was tuned against a warm one and timed out a slot that was working.
# Generous, because the rig images are pulled inside the cluster when the
# preload below cannot work. On Docker Desktop's containerd image store
# `kind load` fails with "content digest ... not found" for both
# docker-image and image-archive, so the in-cluster pull is the normal path
# here, not the fallback.
ROLLOUT_TIMEOUT="${ROLLOUT_TIMEOUT:-900s}"
export COSIGN_PASSWORD="${COSIGN_PASSWORD:-}"
# cosign v2, not v3. v3 writes signatures only in the OCI 1.1 bundle form, and
# Kyverno decides which form to read by calling cosign.GetBundles, which needs
# the registry's Referrers API - distribution does not serve it here, so
# GetBundles finds nothing and Kyverno falls back to the legacy <digest>.sig tag
# that v3 never wrote. v2 writes that tag, which the fallback path reads.
# Install: go install github.com/sigstore/cosign/v2/cmd/cosign@v2.4.3
COSIGN_BIN="${COSIGN_BIN:-$HOME/go/bin/cosign}"

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
    # Same generous timeout as the initial rollout. A hardcoded 120s here killed
    # a run mid-matrix: changing the delay restarts the proxy pod, and on a host
    # under memory pressure that took longer than two minutes. Losing ten
    # measured cells to a pod that was merely slow to come back is not a trade
    # worth making.
    kubectl -n "$NS" rollout status deploy/registry-proxy --timeout="$ROLLOUT_TIMEOUT"
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
need crane
need openssl
[[ -x "$COSIGN_BIN" ]] || { echo "missing cosign at $COSIGN_BIN"; echo "  install: go install github.com/sigstore/cosign/v2/cmd/cosign@v2.4.3"; exit 1; }
# Fail now, with an explanation, rather than after signing 16 images that
# Kyverno will not be able to read.
cosign_major=$("$COSIGN_BIN" version 2>/dev/null | awk '/GitVersion/{gsub(/v/,"",$2); split($2,a,"."); print a[1]}')
if [[ "${cosign_major:-0}" -ge 3 ]]; then
  echo "FATAL: $COSIGN_BIN is cosign v${cosign_major}, which writes signatures only in the"
  echo "       OCI 1.1 bundle form. Kyverno reads those only when the registry serves the"
  echo "       Referrers API, which this one does not, so every verification would fail"
  echo "       with 'no signatures found'. Install v2 and point COSIGN_BIN at it:"
  echo "         go install github.com/sigstore/cosign/v2/cmd/cosign@v2.4.3"
  exit 1
fi

echo "==> namespace $NS"
kubectl create ns "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

echo "==> delay proxy source -> ConfigMap"
kubectl -n "$NS" create configmap registry-proxy-src \
  --from-file=delay-proxy.py="$HERE/delay-proxy.py" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

echo "==> TLS CA"
mkdir -p "$KEY_DIR"
if [[ ! -f "$KEY_DIR/ca.key" ]]; then
  openssl ecparam -name prime256v1 -genkey -noout -out "$KEY_DIR/ca.key"
  openssl req -x509 -new -key "$KEY_DIR/ca.key" -out "$KEY_DIR/ca.crt" \
    -days 3650 -subj "/CN=ivpol-bench-ca"
  echo "generated $KEY_DIR/ca.crt"
else
  echo "reusing $KEY_DIR/ca.crt"
fi

echo "==> TLS server certificate for $NS"
# Regenerated every install: the SAN list depends on $NS, which can change
# between runs. A temp config file, not -addext, so LibreSSL (macOS default)
# works even when Homebrew OpenSSL isn't first on PATH.
SERVER_CNF="$(mktemp)"
cat > "$SERVER_CNF" <<EOF
[req]
distinguished_name = dn
req_extensions = ext
[dn]
[ext]
subjectAltName = DNS:registry.${NS}.svc.cluster.local,DNS:registry.${NS}.svc,DNS:registry.${NS},DNS:registry,DNS:localhost,IP:127.0.0.1
EOF
openssl ecparam -name prime256v1 -genkey -noout -out "$KEY_DIR/server.key"
openssl req -new -key "$KEY_DIR/server.key" -out "$KEY_DIR/server.csr" \
  -subj "/CN=registry.${NS}.svc.cluster.local" -config "$SERVER_CNF" -extensions ext
openssl x509 -req -in "$KEY_DIR/server.csr" -CA "$KEY_DIR/ca.crt" -CAkey "$KEY_DIR/ca.key" \
  -CAcreateserial -out "$KEY_DIR/server.crt" -days 3650 -extfile "$SERVER_CNF" -extensions ext
rm -f "$SERVER_CNF" "$KEY_DIR/server.csr" "$KEY_DIR/ca.srl"
echo "generated $KEY_DIR/server.crt (SANs for namespace $NS)"

echo "==> registry-tls Secret"
# ca.crt is carried in the Secret too, so assert_registry_tls (in the proxy pod)
# and the CA-bundle step below can both read it without a separate ConfigMap.
kubectl -n "$NS" create secret generic registry-tls \
  --from-file=tls.crt="$KEY_DIR/server.crt" \
  --from-file=tls.key="$KEY_DIR/server.key" \
  --from-file=ca.crt="$KEY_DIR/ca.crt" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# Pull once on the host and side-load, so repeated slots reuse the local copy.
if [[ -n "$KIND_CLUSTER" ]] && command -v kind >/dev/null 2>&1 && command -v docker >/dev/null 2>&1; then
  echo "==> preloading rig images into kind cluster $KIND_CLUSTER"
  for img in "$REGISTRY_IMAGE" "$PROXY_IMAGE"; do
    docker image inspect "$img" >/dev/null 2>&1 || docker pull -q "$img" >/dev/null 2>&1 || true
    if out=$(kind load docker-image "$img" --name "$KIND_CLUSTER" 2>&1); then
      echo "  loaded $img"
    else
      # Report the reason rather than swallowing it: a silent "could not
      # preload" cost a diagnosis cycle when the in-cluster pull then timed out.
      echo "  could not preload $img (will pull in cluster): $(printf '%s' "$out" | tail -1 | cut -c1-120)"
    fi
  done
fi

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
          image: $REGISTRY_IMAGE
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
          image: $PROXY_IMAGE
          command: ["python3", "/src/delay-proxy.py"]
          env:
            - { name: UPSTREAM_HOST, value: registry-backend }
            - { name: UPSTREAM_PORT, value: "5000" }
            - { name: LISTEN_PORT, value: "5000" }
            - { name: STATS_PORT, value: "5001" }
            - { name: DELAY_MS, value: "$DELAY_MS" }
            - { name: TLS_CERT, value: /tls/tls.crt }
            - { name: TLS_KEY, value: /tls/tls.key }
          ports:
            - { containerPort: 5000 }
            - { containerPort: 5001 }
          volumeMounts:
            - { name: src, mountPath: /src }
            - { name: tls, mountPath: /tls, readOnly: true }
          resources:
            requests: { cpu: 100m, memory: 128Mi }
      volumes:
        - name: src
          configMap: { name: registry-proxy-src }
        - name: tls
          secret: { secretName: registry-tls }
---
apiVersion: v1
kind: Service
metadata:
  name: registry
spec:
  selector: { app: registry-proxy }
  ports:
    - { name: https, port: 5000, targetPort: 5000 }
    - { name: stats, port: 5001, targetPort: 5001 }
YAML

kubectl -n "$NS" rollout status deploy/registry-backend --timeout="$ROLLOUT_TIMEOUT"
kubectl -n "$NS" rollout status deploy/registry-proxy --timeout="$ROLLOUT_TIMEOUT"

echo "==> proving the registry is reachable over TLS, and only over TLS"
REGISTRY_FQDN="registry.${NS}.svc.cluster.local"
# Runs from inside the proxy pod: proves DNS, the CA chain, the SAN, and
# forwarding to the backend all in one place, with the python already there.
# Retried briefly: "rollout status" returning success does not guarantee the
# Service's Endpoints have converged on the new pod yet, and a probe that
# lands on a terminating old pod mid-rollout isn't testing anything real.
tls_probe_ok=0
for _ in $(seq 1 10); do
  if kubectl -n "$NS" exec deploy/registry-proxy -- python3 -c "
import socket, ssl, sys
host = '$REGISTRY_FQDN'
ctx = ssl.create_default_context(cafile='/tls/ca.crt')
with socket.create_connection((host, 5000), timeout=10) as sock:
    with ctx.wrap_socket(sock, server_hostname=host) as ssock:
        ssock.sendall(b'GET /v2/ HTTP/1.1\r\nHost: ' + host.encode() + b'\r\n\r\n')
        resp = ssock.recv(4096)
        if not resp.startswith(b'HTTP/1.1'):
            sys.exit('no HTTP status line over TLS: ' + repr(resp[:80]))
" > /tmp/registry-tls-probe.out 2>&1; then
    tls_probe_ok=1
    break
  fi
  sleep 2
done
if [[ "$tls_probe_ok" != "1" ]]; then
  echo "FATAL: TLS probe against $REGISTRY_FQDN:5000 failed — CA, SAN, hostname, or"
  echo "       forwarding is wrong, and Kyverno's own dial would fail the same way."
  sed 's/^/  tlsprobe: /' /tmp/registry-tls-probe.out
  exit 1
fi
echo "TLS probe OK: $REGISTRY_FQDN:5000 verified with the private CA"

if kubectl -n "$NS" exec deploy/registry-proxy -- python3 -c "
import socket, sys
host = '$REGISTRY_FQDN'
with socket.create_connection((host, 5000), timeout=10) as sock:
    sock.settimeout(5)
    sock.sendall(b'GET /v2/ HTTP/1.1\r\nHost: ' + host.encode() + b'\r\n\r\n')
    try:
        data = sock.recv(4096)
    except (socket.timeout, ConnectionResetError, OSError):
        data = b''
    if data.startswith(b'HTTP/1.1'):
        sys.exit('plaintext GET got an HTTP response: ' + repr(data[:80]))
" > /tmp/registry-plain-probe.out 2>&1; then
  echo "plaintext rejected OK: a non-TLS request to $REGISTRY_FQDN:5000 got no HTTP response"
else
  echo "FATAL: a plaintext request to $REGISTRY_FQDN:5000 got an HTTP response — the proxy"
  echo "       is accepting unencrypted traffic, which means removing allowInsecureRegistry"
  echo "       would not actually stop plaintext use."
  sed 's/^/  plainprobe: /' /tmp/registry-plain-probe.out
  exit 1
fi

echo "==> CA bundle for Kyverno"
# admissionController.caCertificates.data *replaces* the image's bundle, so
# passing only our CA would strand the keyless policy and anything reaching a
# public registry. FATAL rather than a silent CA-only fallback.
if ! kubectl -n "$NS" exec deploy/registry-proxy -- cat /etc/ssl/certs/ca-certificates.crt \
    > /tmp/public-ca-bundle.pem 2>/tmp/public-ca-bundle.err; then
  echo "FATAL: could not read the proxy image's public CA bundle to concatenate with ours."
  sed 's/^/  cabundle: /' /tmp/public-ca-bundle.err
  exit 1
fi
cat "$KEY_DIR/ca.crt" /tmp/public-ca-bundle.pem > "$KEY_DIR/ca-bundle.pem"
echo "wrote $KEY_DIR/ca-bundle.pem (our CA + the proxy image's public bundle)"

echo "==> cosign key pair"
mkdir -p "$KEY_DIR"
if [[ ! -f "$KEY_DIR/cosign.key" ]]; then
  ( cd "$KEY_DIR" && "$COSIGN_BIN" generate-key-pair >/dev/null )
  echo "generated $KEY_DIR/cosign.pub"
else
  echo "reusing $KEY_DIR/cosign.pub"
fi

echo "==> port-forward for push (localhost:$PUSH_PORT)"
# Forwards to the backend, not the proxy: pushes neither need TLS nor should
# count against the registry-request counters a cell gates on.
kubectl -n "$NS" port-forward svc/registry-backend "$PUSH_PORT:5000" >/tmp/regpush-pf.log 2>&1 &
PF=$!
# Detach from job control so the shell does not print "Terminated" when the trap
# kills it. That line reads like a failure and would camouflage a real one.
disown "$PF" 2>/dev/null || true
trap 'kill $PF 2>/dev/null || true' EXIT
for _ in $(seq 1 60); do
  curl -sf "http://127.0.0.1:$PUSH_PORT/v2/" >/dev/null 2>&1 && break
  sleep 0.5
done
curl -sf "http://127.0.0.1:$PUSH_PORT/v2/" >/dev/null 2>&1 \
  || { echo "FATAL: registry not reachable through port-forward on $PUSH_PORT"; exit 1; }

echo "==> build, push, sign $IMAGE_COUNT images"
# crane, not docker. On Docker Desktop the daemon runs inside its own VM, so a
# `docker push` to 127.0.0.1:$PUSH_PORT resolves to the VM's loopback and never
# reaches the kubectl port-forward listening on the host's. crane runs in this
# process, on the host, and speaks plain HTTP with --insecure.
BUILD="$(mktemp -d)"
trap 'kill $PF 2>/dev/null || true; rm -rf "$BUILD"' EXIT
for i in $(seq 1 "$IMAGE_COUNT"); do
  tag="v$i"
  # One distinct byte-string per image so every tag resolves to its own digest
  # and forces its own manifest fetch. Identical content would collapse them.
  printf 'ivpol-benchmark image %s\n' "$i" > "$BUILD/marker"
  ( cd "$BUILD" && tar cf layer.tar marker )
  crane append -f "$BUILD/layer.tar" -t "127.0.0.1:$PUSH_PORT/$REPO:$tag" --insecure >/dev/null 2>&1 \
    || { echo "FATAL: crane could not push $REPO:$tag"; exit 1; }
  # --use-signing-config=false is required with cosign v3: --tlog-upload=false
  # alone is refused, because a default signing config naming a transparency log
  # is in play. There is no Rekor here by design.
  "$COSIGN_BIN" sign --yes --tlog-upload=false \
    --allow-insecure-registry \
    --key "$KEY_DIR/cosign.key" \
    "127.0.0.1:$PUSH_PORT/$REPO:$tag" >/dev/null 2>&1 \
    || { echo "FATAL: cosign could not sign $REPO:$tag"; exit 1; }
  printf '  signed %s:%s\r' "$REPO" "$tag"
done
echo
# An image that exists but carries no signature. Without it the negative control
# would point at a tag that is simply missing, and the registry's
# MANIFEST_UNKNOWN would reject the Pod whether or not verification works at
# all - a gate that passes on a completely broken policy.
printf 'ivpol-benchmark unsigned control\n' > "$BUILD/marker"
( cd "$BUILD" && tar cf layer.tar marker )
crane append -f "$BUILD/layer.tar" -t "127.0.0.1:$PUSH_PORT/$REPO:unsigned-control" --insecure >/dev/null 2>&1 \
  || { echo "FATAL: could not push the unsigned control image"; exit 1; }
echo "  pushed $REPO:unsigned-control (deliberately unsigned)"

echo "==> verifying the signature exists and verifies"
# Not "the same path Kyverno will use" anymore: this goes straight to the
# backend, plain HTTP. The TLS path is proven by the probes above, and the
# end-to-end proof is the positive control in ivpol-run.sh.
"$COSIGN_BIN" verify --key "$KEY_DIR/cosign.pub" --insecure-ignore-tlog=true \
  --allow-insecure-registry \
  "127.0.0.1:$PUSH_PORT/$REPO:v1" >/dev/null 2>&1 \
  || { echo "FATAL: signature does not verify; every cell would measure a failing policy"; exit 1; }

echo
echo "registry ready"
echo "  in-cluster:  $IN_CLUSTER_HOST/$REPO:v{1..$IMAGE_COUNT}  (TLS, private CA)"
echo "  public key:  $KEY_DIR/cosign.pub"
echo "  CA bundle:   $KEY_DIR/ca-bundle.pem  (for admissionController.caCertificates.data)"
echo "  delay:       ${DELAY_MS}ms  (change: ACTION=delay DELAY_MS=25 bash $0)"

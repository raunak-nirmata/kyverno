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
#   MODE=bundle-key bash ivpol-run.sh                           # bundle-format cell
#
# MODE sets the policy template, the image repository and whether the bundle
# walk is expected, as one choice, because those three have to agree: a policy
# whose glob names one repo while the workload uses another matches nothing,
# and a cell that matches nothing reports excellent latency. Known modes:
#   key cert bundle-key bundle-combined bundle-split keyless
# Setting POLICY_TEMPLATE and REPO directly still works and still wins for
# anything MODE does not name.
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
# with no latency to serialise, 1.19+ should converge. 1.18.2 will not: it pays
# a Sigstore CDN round trip per image that no delay setting removes.
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

# MODE binds the three things that must agree for a cell to measure what its
# label says: the policy template, the repository the images come from, and
# whether the bundle (OCI referrers) walk is expected in the request counts.
#
# They used to be set independently, and nothing checked them: a POLICY_TEMPLATE
# whose matchImageReferences glob names one repo, with REPO pointing at another,
# produces a cell where the policy never matches and every admission is a fast
# no-op. That passes the failure-rate gate and looks like excellent performance.
# Setting MODE makes the combination a single choice. Setting the variables
# directly still works, for the ad-hoc sweeps and the committed drivers.
MODE="${MODE:-}"
case "$MODE" in
  "") ;;  # nothing set: honour POLICY_TEMPLATE / REPO as given
  key)              POLICY_TEMPLATE="$HERE/policies/ivpol-cosign-key.yaml";            REPO="bench/signed";             EXPECT_BUNDLE=0 ;;
  cert)             POLICY_TEMPLATE="$HERE/policies/ivpol-cosign-certchain.yaml";      REPO="bench/signed-cert";        EXPECT_BUNDLE=0 ;;
  bundle-key)       POLICY_TEMPLATE="$HERE/policies/ivpol-cosign-bundle-key.yaml";     REPO="bench/signed-bundle";      EXPECT_BUNDLE=1 ;;
  # bundle-attest is deliberately absent: verifyAttestationSignatures needs the
  # images to carry attestations, and setup-registry.sh does not produce any
  # yet. Adding the mode without the image set would give a cell that matches
  # nothing and reports excellent latency.
  bundle-combined)  POLICY_TEMPLATE="$HERE/policies/ivpol-cosign-bundle-tworoot-combined.yaml"; REPO="bench/signed-bundle"; EXPECT_BUNDLE=1 ;;
  bundle-split)     POLICY_TEMPLATE="$HERE/policies/ivpol-cosign-bundle-tworoot-split.yaml";    REPO="bench/signed-bundle"; EXPECT_BUNDLE=1 ;;
  keyless)
    # Not runnable yet, and the failure would otherwise be obscure: the
    # bundle images are signed by the local code-signing CA, whose leaves
    # carry no OIDC issuer or subject extension, so they cannot satisfy the
    # identities a keyless attestor matches on. Both the TUF mirror and a
    # Fulcio-equivalent image set have to exist first.
    if [[ ! -f "$KEY_DIR/trusted_root.json" ]]; then
      echo "FATAL: MODE=keyless needs the in-cluster TUF mirror and a Fulcio-signed image"
      echo "       set, neither of which exists yet. Missing $KEY_DIR/trusted_root.json."
      echo "       Keyless cannot be made offline by policy alone: skipSigstoreInfra is"
      echo "       false for every keyless attestor (opts.go:85-87), so TUF is contacted"
      echo "       on every check. Until the mirror is up, measure the signature format"
      echo "       with MODE=bundle-key and report the TUF cost as a code read."
      exit 1
    fi
    POLICY_TEMPLATE="$HERE/policies/ivpol-cosign-keyless.yaml"; REPO="bench/signed-keyless"; EXPECT_BUNDLE=1 ;;
  *) echo "FATAL: unknown MODE '$MODE'. Known: key cert bundle-key bundle-combined bundle-split keyless"; exit 1 ;;
esac
# Whether to require referrers traffic in the gate. Derived from MODE above;
# settable directly for a hand-built cell that does not use MODE.
EXPECT_BUNDLE="${EXPECT_BUNDLE:-0}"

# Kyverno 1.18 needs its own copy of some policies. Not a preference: on 1.18
# getRekor() runs for every attestor and rejects a ctlog block that names no
# URL, so the policy every other version accepts fails there outright - a
# correctly signed image is denied. The -118 variants add an inert URL and
# inline the two public keys that would otherwise be fetched from Sigstore.
# See policies/ivpol-cosign-key-118.yaml for the full trace.
#
# Swapped here rather than by asking the operator to remember, because the
# failure it prevents looks like a broken rig, not like a version difference.
# A ladder sweeping one MODES list across three versions stays correct.
case "$KYVERNO_VERSION" in
  1.18|1.18.*)
    _base118="${POLICY_TEMPLATE%.yaml}-118.yaml"
    if [[ -f "$_base118" ]]; then
      echo "note: Kyverno $KYVERNO_VERSION needs the 1.18 policy variant; using $(basename "$_base118")"
      echo "      (1.18 has no offline fast path - it requires ctlog.url and inline log keys)"
      POLICY_TEMPLATE="$_base118"
    else
      echo "WARN: Kyverno $KYVERNO_VERSION usually needs a -118 policy variant, and none exists"
      echo "      for $(basename "$POLICY_TEMPLATE"). If the positive control fails with a"
      echo "      signed image, that is why."
    fi
    ;;
esac
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
  # $2, if given, anchors the query's evaluation instant via Prometheus's
  # `time=` parameter instead of the implicit "now". A range vector inside the
  # query (e.g. [64s]) then ends exactly at that instant rather than at
  # whatever wall-clock moment the curl happens to run, which matters when the
  # query is issued after other work has elapsed since the window it should
  # describe actually closed.
  local query="$1" at="${2:-}"
  local -a args=(--data-urlencode "query=$query")
  [[ -n "$at" ]] && args+=(--data-urlencode "time=$at")
  { curl -sf --get "$PROM_LOCAL/api/v1/query" "${args[@]}" 2>/dev/null \
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
# Which registry backend actually served this cell, read from the cluster
# rather than from an environment variable. REGISTRY_FLAVOR is setup-registry's
# variable and is not normally exported into a run, so trusting it here would
# record "unset" for every cell, or worse, record the value the operator meant
# rather than the one that is running.
registry_flavor() {
  local img
  img=$(kubectl -n "${REGISTRY_NS:-bench-registry}" get deploy registry-backend \
          -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null) || true
  case "$img" in
    *zot*)      echo "zot" ;;
    *registry*) echo "distribution" ;;
    "")         echo "unknown" ;;
    *)          echo "$img" ;;
  esac
}

# 1.18.2's checkOptions calls tuf.Initialize unconditionally with
# forceUpdate=true (opts.go:46), under a global mutex, so every image
# verification on that version round-trips to the Sigstore TUF CDN - traffic
# the benchmark registry proxy never sees and that this harness would
# otherwise have no record of at all. ~0.4s per fetch measured from inside the
# cluster accounts for most of the gap between 1.18.2 and 1.19.1 at identical
# registry call counts. This is real 1.18 behavior, not something to route
# around; the point is to make it visible and attributable rather than to
# eliminate it.
#
# Sets SIGSTORE_CDN_RTT_MS and SIGSTORE_CDN_REACHABLE for the caller to read.
# Non-fatal by construction: a failed sample must never abort a cell over a
# number that exists purely for attribution, and egress to a public CDN is
# not something this harness controls.
sample_sigstore_cdn() {
  SIGSTORE_CDN_RTT_MS=""
  SIGSTORE_CDN_REACHABLE="no"
  local ns="${REGISTRY_NS:-bench-registry}"
  local out
  out=$(kubectl -n "$ns" exec deploy/registry-proxy -- python3 -c '
import time, urllib.request
rtts = []
for _ in range(3):
    t0 = time.time()
    try:
        urllib.request.urlopen("https://tuf-repo-cdn.sigstore.dev/", timeout=5).read(1)
        rtts.append((time.time() - t0) * 1000)
    except Exception:
        pass
if rtts:
    rtts.sort()
    print(f"{rtts[len(rtts)//2]:.1f} yes")
else:
    print("NA no")
' 2>/dev/null) || out=""
  if [[ -n "$out" ]]; then
    local rtt reach
    read -r rtt reach <<< "$out"
    [[ "$rtt" != "NA" ]] && SIGSTORE_CDN_RTT_MS="$rtt"
    [[ -n "$reach" ]] && SIGSTORE_CDN_REACHABLE="$reach"
  fi
}

placeholder_source() {
  case "$1" in
    PUBLIC_KEY_PLACEHOLDER) echo "$KEY_DIR/cosign.pub" ;;
    KEY_A_PLACEHOLDER)      echo "$KEY_DIR/cosign.pub" ;;
    KEY_B_PLACEHOLDER)      echo "$KEY_DIR/cosign-b.pub" ;;
    KEY_C_PLACEHOLDER)      echo "$KEY_DIR/cosign-c.pub" ;;
    CERT_CHAIN_PLACEHOLDER) echo "$KEY_DIR/codesign-ca.crt" ;;
    # The bundle cells reuse KEY_A/KEY_B above: their images are key-signed,
    # because a certificate attestor cannot verify a bundle offline at all
    # (see policies/ivpol-cosign-bundle-key.yaml for the code trace).
    #
    # The 1.18 variants inline a Rekor and a CT-log public key so that version's
    # getRekor() takes its local-parse branch instead of fetching from Sigstore.
    # Both are the benchmark's own cosign public key: nothing verifies against
    # them, they only have to parse. Two distinct tokens because the renderer
    # asserts each appears exactly once.
    REKOR_PUBKEY_PLACEHOLDER) echo "$KEY_DIR/cosign.pub" ;;
    CTLOG_PUBKEY_PLACEHOLDER) echo "$KEY_DIR/cosign.pub" ;;
    # Keyless cells only. TUF material for the in-cluster mirror; absent until
    # the mirror is stood up, and a keyless template referencing them FATALs
    # here rather than silently reaching Sigstore's public CDN.
    TRUSTED_ROOT_PLACEHOLDER) echo "$KEY_DIR/trusted_root.json" ;;
    TUF_ROOT_PLACEHOLDER)     echo "$KEY_DIR/tuf-root.json" ;;
    FULCIO_ROOTS_PLACEHOLDER) echo "$KEY_DIR/fulcio-ca.crt" ;;
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
        elif "keyless" in cosign:
            # A keyless attestor can never take the offline fast path:
            # skipSigstoreInfra is `keyOrCert && ignoreTlog` and keyOrCert
            # requires Keyless == nil (opts.go:85-87). So initTUFAndFetch runs
            # on every check, and without an explicit mirror it reaches
            # Sigstore's public TUF CDN - which would put someone else's
            # network inside the measurement and make the cell unreportable.
            # Fail at render, where it is obvious, not at run.
            if not cosign["keyless"].get("identities"):
                sys.exit(f"rendered policy attestor {att.get('name')} is keyless with no identities")
            mirror = cosign.get("tuf", {}).get("mirror", "")
            if not mirror:
                sys.exit(f"keyless attestor {att.get('name')} has no tuf.mirror; it would fetch from the public Sigstore CDN")
            if "sigstore.dev" in mirror:
                sys.exit(f"keyless attestor {att.get('name')} points tuf.mirror at {mirror}; benchmark cells must use the in-cluster mirror")
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
  # Read the actual metadata.name back rather than hardcoding
  # ivpol-bench-cosign-key: every template now has its own distinct name.
  IVPOL_NAME=$(awk '/^metadata:/{f=1; next} f && /^  name:/{print $2; exit}' "$RENDERED_POLICY_PATH")
  # Drop every other policy first. Applying alongside a previous cell's policy
  # leaves both enforcing, so admission pays for both and the cell measures a
  # workload no one configured.
  local stale
  stale=$(kubectl get ivpol --no-headers -o custom-columns=NAME:.metadata.name 2>/dev/null \
    | grep -v "^${IVPOL_NAME}$" || true)
  if [[ -n "$stale" ]]; then
    echo "removing policies from a previous cell: $(echo $stale | tr '\n' ' ')"
    # shellcheck disable=SC2086
    kubectl delete ivpol $stale --ignore-not-found >/dev/null 2>&1 || true
  fi
  # Retried: right after install, Kyverno's own CA bundle can take a few
  # seconds to propagate to the apiserver's webhook config, and the apply
  # fails with "x509: certificate signed by unknown authority" against
  # validate-policy.kyverno.svc. That is a propagation race, not a bad
  # policy, so it is worth a few attempts before this aborts the cell.
  local apply_tries=5 apply_delay=3 attempt apply_out apply_rc
  for (( attempt=1; attempt<=apply_tries; attempt++ )); do
    if apply_out=$(kubectl apply -f "$RENDERED_POLICY_PATH" 2>&1); then
      apply_rc=0
      break
    fi
    apply_rc=$?
    echo "WARN: policy apply failed (attempt $attempt/$apply_tries): $apply_out"
    (( attempt < apply_tries )) && sleep "$apply_delay"
  done
  if [[ "$apply_rc" != "0" ]]; then
    echo "FATAL: policy apply failed after $apply_tries attempts: $apply_out"
    exit 1
  fi
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
  # A low failure rate is necessary but not sufficient, and that gap produced
  # three bogus cells before this check existed (69%, 85%, 85% coverage, each
  # the first cell after a fresh install). A webhook that has not finished
  # registering does not reject anything - it is simply not consulted, so the
  # admissions succeed, the failure rate is zero, and the probe passes while
  # verification is not happening. Those admissions are also faster than real
  # ones, so the bias is toward a better-looking number.
  #
  # So the probe also requires evidence that verification actually ran: the
  # registry must see roughly one referrers probe per image per admission, the
  # same denominator assert_cell_valid uses later.
  local images="$1"
  local budget="${SERVING_BUDGET:-12}"
  local attempt rate probe_iters probe_refs probe_cov probe_mans
  # Registry traffic proves interception only when the cache cannot serve the
  # probe. A warm cache verifies from memory, so coverage reads 0% on a
  # perfectly healthy controller - that FATALed every 1.19+ warm cell while
  # 1.18 (no ivpol cache) passed. Where coverage is not a valid signal the
  # controls are: an unsigned image must be denied, cached or not.
  local cov_meaningful=1
  [[ "${CACHE_MODE:-off}" != "off" ]] && cov_meaningful=0
  for attempt in $(seq 1 "$budget"); do
    registry_ctl reset >/dev/null 2>&1 || true
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
    probe_iters=$(python3 -c '
import json
try:
    m = json.load(open("/tmp/ivpol-serving.json"))["metrics"].get("iterations", {})
    print(int(m.get("count", 0)))
except Exception:
    print(0)' 2>/dev/null)
    probe_refs=$(registry_ctl stats 2>/dev/null | tail -1 | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("referrers",0))
except Exception: print(0)' 2>/dev/null)
    probe_mans=$(registry_ctl stats 2>/dev/null | tail -1 | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("manifest",0))
except Exception: print(0)' 2>/dev/null)
    probe_cov=$(python3 -c "
exp = max($probe_iters * $images, 1)
print(int($probe_refs * 100 / exp))" 2>/dev/null || echo 0)
    kubectl delete pods -n "$TEST_NS" --all --ignore-not-found --wait=false >/dev/null 2>&1 || true
    if python3 -c "import sys; sys.exit(0 if float('$rate') <= 0.01 else 1)" 2>/dev/null; then
      if (( cov_meaningful == 0 )); then
        # Two independent proofs, because neither alone covers the warm case.
        # The controls prove the policy is on the admission path at all; the
        # manifest floor proves each probe admission did registry work, since
        # Kyverno still resolves tag to digest on a cache hit. Floor is one per
        # admission against 2-4 observed, so it trips on a dead webhook only.
        if (( probe_mans < probe_iters )); then
          echo "  not intercepting yet (attempt $attempt/$budget: $probe_mans manifest fetches for"
          echo "  $probe_iters admissions - even a cache hit resolves the digest) — waiting"
          sleep 15
          continue
        fi
        # FATALs on its own if the webhook is not on the admission path.
        verify_ivpol_active
        echo "serving check passed after ${attempt} burst(s) (failure rate $rate; cache mode"
        echo "  ${CACHE_MODE}, so interception is proven by the controls plus $probe_mans manifest"
        echo "  fetches for $probe_iters admissions, not by referrers traffic)"
        return 0
      fi
      if (( probe_cov >= 95 )); then
        echo "serving check passed after ${attempt} burst(s) (failure rate $rate, verification coverage ${probe_cov}%)"
        return 0
      fi
      echo "  not verifying yet (attempt $attempt/$budget: failures $rate but only ${probe_cov}% of"
      echo "  admissions reached the registry - the webhook is not intercepting yet) — waiting"
      sleep 15
      continue
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
  local stats manifests total referrers fallback blobs sig_tag
  stats="$(registry_ctl stats 2>/dev/null | tail -1)"
  echo "$stats" > "$dir/registry-stats.json"
  manifests=$(printf '%s' "$stats" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("manifest",0))
except Exception: print(0)' 2>/dev/null)
  total=$(printf '%s' "$stats" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("total",0))
except Exception: print(0)' 2>/dev/null)
  referrers=$(printf '%s' "$stats" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("referrers",0))
except Exception: print(0)' 2>/dev/null)
  fallback=$(printf '%s' "$stats" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("fallback_tag",0))
except Exception: print(0)' 2>/dev/null)
  blobs=$(printf '%s' "$stats" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("blob",0))
except Exception: print(0)' 2>/dev/null)
  sig_tag=$(printf '%s' "$stats" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("sig_tag",0))
except Exception: print(0)' 2>/dev/null)
  echo "registry requests: total=$total manifests=$manifests referrers=$referrers fallback_tag=$fallback sig_tag=$sig_tag blobs=$blobs"
  echo "$referrers" > "$dir/referrers.txt"
  echo "$fallback"  > "$dir/fallback-tag.txt"
  echo "$blobs"     > "$dir/blobs.txt"
  echo "$sig_tag"   > "$dir/sig-tag.txt"

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
      # Say which half broke rather than leaving the next person to guess.
      # Zero registry traffic has two very different causes needing different
      # fixes: either the webhook never ran for these Pods (policy matched
      # nothing, or the webhook was not registered for the namespace), or it
      # ran and the engine did no registry work. Kyverno's own admission
      # counter separates them, and it costs one query.
      local adm_seen
      adm_seen=$(promq "sum(increase(kyverno_admission_review_duration_seconds_count[${window:-60}s]))")
      if [[ -z "$adm_seen" ]]; then
        echo "       Kyverno's admission counter was unreadable, so which half broke is unknown."
      elif python3 -c "import sys; sys.exit(0 if float('$adm_seen') < 1 else 1)" 2>/dev/null; then
        echo "       Kyverno saw ~0 admission reviews in this window: the webhook never ran for"
        echo "       these Pods. Look at matchImageReferences and the webhook registration,"
        echo "       not at the registry."
      else
        echo "       Kyverno saw ~$adm_seen admission reviews: the webhook did run and the engine"
        echo "       did no registry work. Look at the engine path and the cache, not the match."
      fi
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
  # The bundle path fetches a manifest per referrer on top of the per-image
  # prefetch, so this figure runs well above 100% there and the 60% floor is
  # not the signal it is on the legacy path. Say so rather than leaving a
  # reader to conclude the cell fetched twice what it should have.
  if [[ "$EXPECT_BUNDLE" == "1" && "$pct" -gt 120 ]]; then
    echo "NOTE: above one manifest per image is expected on the bundle path; each referrer"
    echo "      costs its own manifest fetch on top of the prefetch."
  fi
  echo "$pct" > "$dir/manifest-coverage-pct.txt"

  # Verification coverage: did every admission actually verify every image?
  #
  # The manifest coverage figure above cannot answer this. Its denominator
  # assumes about one manifest fetch per image, but the real paths fetch
  # several, so it reports a few hundred percent and passes whatever happens.
  # It read 275% on a cell that verified only 69% of its images.
  #
  # The referrers probe is the honest denominator. Kyverno issues exactly one
  # per image per attestor on every uncached verification, whatever the
  # signature format, so with the cache off the count should be at least
  # iterations x images. A shortfall means admissions completed without
  # verifying, which is the failure this whole harness exists to catch: those
  # admissions are fast, they do not fail, and nothing else notices.
  #
  # Only meaningful with the cache off; a warm cell legitimately probes little.
  if [[ "$CACHE_MODE" == "off" && "$iters" -gt 0 && "$images" -gt 0 ]]; then
    local vexp=$(( iters * images ))
    local vpct=$(( referrers * 100 / (vexp > 0 ? vexp : 1) ))
    echo "verification coverage: $referrers probes of ~$vexp expected (${vpct}%)"
    echo "$vpct" > "$dir/verification-coverage-pct.txt"
    # 95, not 80: the converse legacy-format gate below already FATALs under
    # 95% of expected .sig fetches, and there is no reason a bundle cell may
    # ship having verified four fifths of its images when a legacy one may not.
    if (( vpct < 95 )) && [[ "${ALLOW_FAILURES:-}" != "1" ]]; then
      echo "FATAL: only ${vpct}% of admissions verified their images (floor 95%). The rest were admitted"
      echo "       without verification, which is faster than the real path and therefore"
      echo "       reads as a better result. Latency from this cell is not comparable."
      exit 1
    fi
  fi

  # Which signature format did this cell actually verify?
  #
  # Counting referrers lookups does not answer that. GetBundles runs on every
  # uncached verification whatever the image carries (verifier.go:39), so a
  # legacy .sig cell probes for referrers too and any "referrers >= iters"
  # floor passes trivially. Confirmed against the archived cells: a cold
  # 16-image .sig cell shows exactly one unclassified probe per image.
  #
  # The .sig tag fetch is the outcome. Only the legacy path fetches it, so
  # under a bundle label it must be absent. That is the real failure this
  # guards: when no bundle is found Kyverno does not error, it falls back
  # (verifier.go:40, bundleDetected false), and the cell goes on measuring the
  # legacy format under a bundle name.
  #
  # Warm and postedit cells cannot be judged on the measured window at all: the
  # cache is consulted before VerifyImageSignature (impl.go:157-166), so a
  # cache hit issues no verification traffic of any kind. The archived warm
  # cells show zero probes, against one per image cold. For those the evidence
  # is the cell's own warm-up window, which ran cold.
  if [[ "$EXPECT_BUNDLE" == "1" ]]; then
    local judge_sig="$sig_tag" judge_ref=$(( referrers + fallback )) judge_src="measured window"
    if [[ "$CACHE_MODE" != "off" ]]; then
      judge_sig=$(printf '%s' "${warm_stats_json:-}" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("sig_tag",0))
except Exception: print(0)' 2>/dev/null || echo 0)
      judge_ref=$(printf '%s' "${warm_stats_json:-}" | python3 -c 'import json,sys
try: d=json.load(sys.stdin); print(d.get("referrers",0)+d.get("fallback_tag",0))
except Exception: print(0)' 2>/dev/null || echo 0)
      judge_src="warm-up window"
    fi
    echo "format evidence ($judge_src): sig_tag=$judge_sig referrers+fallback=$judge_ref"
    # Proportional, not absolute. A real fallback is wholesale - every image
    # that lacks a bundle fetches its .sig tag, so sig_tag tracks referrers
    # one for one. A handful against thousands is a control image or a stray,
    # not a format change, and failing on it would abort good cells. 5% is far
    # below any real fallback and far above any plausible stray.
    local sig_pct=$(( judge_sig * 100 / (judge_ref > 0 ? judge_ref : 1) ))
    if (( sig_pct >= 5 )); then
      echo "FATAL: $judge_sig legacy .sig tag fetches against $judge_ref referrers lookups"
      echo "       (${sig_pct}%) in a cell labelled bundle format. Kyverno found no bundle and"
      echo "       fell back to the legacy path, so this cell is measuring .sig while its"
      echo "       label, directory and chart all say bundle. Check the images were signed"
      echo "       with cosign v3 in oci-1-1 mode."
      exit 1
    elif (( judge_sig > 0 )); then
      echo "NOTE: $judge_sig stray .sig fetches (${sig_pct}%), consistent with a control image"
      echo "      that carries no signature. Not a format fallback."
    fi
    if (( judge_ref == 0 )); then
      if [[ "$CACHE_MODE" == "off" ]] && [[ "${ALLOW_FAILURES:-}" != "1" ]]; then
        echo "FATAL: no referrers lookup at all with the cache off, so nothing verified a"
        echo "       bundle here. Either the policy matched no image, or verification never ran."
        exit 1
      fi
      # Warm cells legitimately reach zero. When several cells share an install,
      # the cache is already full before the later cell's warm-up runs, so even
      # the warm-up issues no probe - which is the cache working, not a broken
      # cell. The format evidence for those comes from the cold cell on the
      # same images. Failing here aborted a good cell that showed exactly the
      # prefetch-only floor the cache is supposed to produce.
      echo "NOTE: no referrers lookup in the $judge_src: the cache was already warm before"
      echo "      this cell began. Format evidence comes from the cache-off cell on these"
      echo "      same images, not from here."
    fi
    if (( referrers == 0 && fallback > 0 )); then
      echo "NOTE: every lookup used the dist-spec fallback tag, so this backend serves no"
      echo "      Referrers API. That is one extra round trip per attestor per image."
    fi
  else
    # Converse of the bundle check above. A legacy cell that silently verified
    # bundles instead would look identical to a healthy legacy cell on every
    # other gate in this function - manifest coverage and referrers-based
    # verification coverage both pass either way, since GetBundles always
    # probes for a bundle regardless of format (verifier.go:39-40). The .sig
    # tag fetch is the only signal that the legacy path actually ran, so a
    # cell labelled legacy must show it near the expected floor.
    #
    # Only meaningful with the cache off, for the same reason as the bundle
    # side: a cache hit issues no verification traffic at all.
    if [[ "$CACHE_MODE" == "off" && "$iters" -gt 0 && "$images" -gt 0 ]]; then
      local legacy_exp=$(( iters * images ))
      local legacy_floor
      legacy_floor=$(python3 -c "print(int($legacy_exp * 0.95))")
      echo "format evidence (measured window): sig_tag=$sig_tag of ~$legacy_exp expected"
      if (( sig_tag < legacy_floor )); then
        echo "FATAL: only $sig_tag legacy .sig tag fetches against ~$legacy_exp expected"
        echo "       (floor $legacy_floor) in a cell labelled legacy format. Either"
        echo "       verification silently used bundles instead, or it did not run at all -"
        echo "       either way this cell is not measuring what its mode claims."
        exit 1
      fi
    fi
  fi

  # Warm-cache evidence. The README has claimed a "warm-cache proof" gate since
  # the beginning and none existed, so every warm number rested on the Helm flag
  # having been set rather than on the cache having been populated. This
  # compares signature blob fetches per second in the measured window against
  # the same rate during the cell's own discarded warm-up, which runs at the
  # same rps against the same images.
  #
  # It warns rather than aborting, deliberately. The serving probe that runs at
  # install time can itself populate the cache, so on the first cell after an
  # install the warm-up may already be warm and the rate will not fall. That is
  # a measurement artefact, not a broken cell, and aborting on it would throw
  # away good runs. The numbers are recorded either way, so a warm claim can be
  # checked after the fact instead of taken on faith.
  if [[ "$CACHE_MODE" != "off" ]]; then
    local warm_secs="${WARMUP:-10s}"; warm_secs="${warm_secs%s}"
    local warm_rate meas_rate
    warm_rate=$(python3 -c "print(f'{${warm_blobs:-0}/max(${warm_secs},1):.2f}')" 2>/dev/null || echo 0)
    meas_rate=$(python3 -c "print(f'{${blobs}/max(${window:-1},1):.2f}')" 2>/dev/null || echo 0)
    echo "blob fetch rate: warm-up ${warm_rate}/s, measured ${meas_rate}/s"
    {
      printf 'warmup_blobs_per_s=%s\nmeasured_blobs_per_s=%s\n' "$warm_rate" "$meas_rate"
      # Always written, including the zero case. The zero case is not an
      # absent measurement, it is a specific and reportable state: the cache
      # was already populated before this cell's warm-up ran, so the warm-up
      # fetched nothing and the comparison below carries no information.
      if [[ "${warm_blobs:-0}" == "0" ]]; then
        printf 'verdict=no-evidence\nreason=warm-up fetched no signature blobs; cache was already warm before this cell\n'
      elif python3 -c "import sys; sys.exit(0 if float('$meas_rate') < float('$warm_rate') else 1)" 2>/dev/null; then
        printf 'verdict=fell\n'
      else
        printf 'verdict=did-not-fall\n'
      fi
    } > "$dir/warm-cache-evidence.txt"
    if [[ "${warm_blobs:-0}" == "0" ]]; then
      echo "NOTE: the warm-up fetched no signature blobs, so this cell has no warm-cache"
      echo "      evidence of its own. Expected on the first cell after an install, where"
      echo "      the serving probe already warmed the cache."
    elif ! python3 -c "import sys; sys.exit(0 if float('$meas_rate') < float('$warm_rate') else 1)" 2>/dev/null; then
      echo "WARN: signature blob fetches did not fall after warming (${warm_rate}/s -> ${meas_rate}/s)."
      echo "      Check warm-cache-evidence.txt before reporting this cell as warm."
    fi
  fi

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

  # The requested image string above says what was asked for; imageID is what
  # the container runtime actually resolved and pulled - the only thing that
  # makes a tag-based or digest-pinned "main" cell reproducible after the tag
  # has moved on. Best-effort: a pod not yet reporting status leaves this
  # empty rather than aborting a cell over a recording detail.
  local image_id
  image_id=$(kubectl -n kyverno get pods -l app.kubernetes.io/component=admission-controller \
    -o jsonpath='{.items[0].status.containerStatuses[0].imageID}' 2>/dev/null)
  echo "  running imageID: ${image_id:-<unavailable>}"
  echo "$image_id" > "$RESULTS_DIR/running-image-id.txt"
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
  # Per-run chart overrides, for knobs that are not worth their own variable.
  # Space-separated, e.g. EXTRA_HELM_ARGS="--set admissionController.profiling.enabled=true"
  local extra_helm=()
  [[ -n "${EXTRA_HELM_ARGS:-}" ]] && read -ra extra_helm <<< "$EXTRA_HELM_ARGS"
  helm install kyverno "${chart_args[@]}" \
    -n kyverno --create-namespace \
    -f "$VALUES" \
    ${image_args[@]+"${image_args[@]}"} \
    --set-string "admissionController.container.extraArgs.imageVerifyCacheEnabled=$cache_enabled" \
    --set "features.admissionReports.enabled=$reports_enabled" \
    --set-file "admissionController.caCertificates.data=$KEY_DIR/ca-bundle.pem" \
    --set crds.install=false \
    ${extra_helm[@]+"${extra_helm[@]}"} \
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

  # Re-prove verification after the delay change, not just before it.
  #
  # `registry_ctl delay` sets an env var on the proxy Deployment, which rolls
  # the pod. Kyverno's keep-alive connections to the old pod IP die with it,
  # and until they are re-established some admissions complete without reaching
  # the registry at all - they succeed, they are faster than real ones, and
  # nothing downstream notices. That is how a first cell came in at 85%
  # coverage having passed the install-time probe at 100% minutes earlier.
  #
  # The warm-up below was supposed to absorb this, and does at the rates the
  # rig was built for; at CONCURRENCY=1 ten seconds is about ten iterations,
  # which is not enough. Rather than guess a longer warm-up, re-run the probe:
  # it already gates on verification coverage and costs one 10s burst.
  wait_until_serving "$images"

  # Discarded warm-up, before anything is measured. Changing the delay restarts
  # the proxy pod, which kills the keep-alive connections Kyverno holds to the
  # old pod's IP; requests issued into that window fail while they are
  # re-established. Measured straight after a restart, the first cell of a slot
  # showed 0.15% failures on one version and 93% on another - the same artifact,
  # differing only by timing, and the second one aborted a six-slot run.
  # A short burst absorbs it, and its results are thrown away.
  # Zero the counters before the warm-up, not only after it. The warm-up
  # snapshot below is what a warm cell's signature-format check is judged on,
  # and without this reset that snapshot also contains the install-time serving
  # probe and the controls. The negative control deliberately uses an image
  # with no signature at all, so Kyverno correctly probes for a bundle, finds
  # none and falls back to the legacy .sig tag - which the format check then
  # misreads as the cell having fallen back. Measured: 3 such fetches against
  # 5,644 referrers lookups, enough to fail every warm bundle cell.
  registry_ctl reset >/dev/null

  echo "warm-up (discarded): ${WARMUP:-10s}"
  IMAGES_PER_POD="$images" REGISTRY_HOST="$REGISTRY_HOST" REPO="$REPO" TEST_NAMESPACE="$TEST_NS" \
    TARGET_RPS="$rps" DURATION="${WARMUP:-10s}" \
    "$K6" run "$SCRIPT_JS" --quiet >/dev/null 2>&1 || true
  kubectl delete pods -n "$TEST_NS" --all --ignore-not-found --wait=false >/dev/null 2>&1 || true

  # Snapshot the warm-up's own registry traffic before the counters are zeroed.
  # This is what makes a warm cell provable rather than merely labelled: the
  # README has claimed a "warm-cache proof" gate since the beginning and no such
  # assertion existed, so every warm number rested on the flag having been set
  # rather than on the cache having been populated. The signature blob fetches
  # the warm-up pays are the ones a warm measured window should not repeat.
  # warm_stats_json is read by assert_cell_valid, which judges a warm cell's
  # signature format on this window rather than the measured one: a cache hit
  # issues no verification traffic at all, so the measured window of a warm
  # cell is silent about which format was verified.
  local warm_blobs=0 warm_stats_json=""
  if [[ "$CACHE_MODE" != "off" ]]; then
    warm_stats_json="$(registry_ctl stats 2>/dev/null | tail -1)"
    echo "$warm_stats_json" > "$dir/registry-stats-warmup.json"
    warm_blobs=$(printf '%s' "$warm_stats_json" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("blob",0))
except Exception: print(0)' 2>/dev/null)
    echo "warm-up registry blobs: $warm_blobs"
  fi

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

  # Queried immediately when the load finishes, anchored at t1 (the cell's own
  # end), and before anything else this function does. This used to run after
  # assert_cell_valid, the end-of-cell verify_ivpol_active probe and the
  # resource report below, all of which issue their own admission requests.
  # With an unanchored "now" query, every one of those seconds elapsed before
  # the query ran, so the [window] range slid forward to include those
  # end-of-cell control admissions while the cell's own early seconds fell out
  # the back - the opposite of what "the window is the cell's own duration"
  # was supposed to mean.
  #
  # Kyverno's own admission-review duration is the headline, not k6's
  # iteration_duration. Measured against the archived cells they differ by about
  # 2x - 148ms vs 340ms at eight images - because an iteration also carries the
  # apiserver, etcd, and a fresh Kubernetes client built per iteration in the
  # workload. Both are recorded; only this one answers "what does verification
  # cost".
  local win_range="${window}s"
  local adm_avg adm_p99
  adm_avg=$(promq "sum(rate(kyverno_admission_review_duration_seconds_sum[$win_range]))/sum(rate(kyverno_admission_review_duration_seconds_count[$win_range]))" "$t1")
  local adm_count
  adm_count=$(promq "sum(increase(kyverno_admission_review_duration_seconds_count[${win_range}]))" "$t1")
  adm_p99=$(promq "histogram_quantile(0.99, sum(rate(kyverno_admission_review_duration_seconds_bucket[$win_range])) by (le))" "$t1")
  # adm_p99 lands on Prometheus's default histogram bucket boundaries far more
  # often than a real percentile would (2.485, 4.975, 9.95 recur across
  # unrelated cells, by construction of le-bucket interpolation) - it is
  # bucket-quantised, not a measured percentile. Recorded anyway as its own
  # labeled field; k6's p99 does not have this artifact and is preferred when
  # the two disagree (see version-table.py).

  assert_cell_valid "$dir" "$images" "$restarts_t0" "$cell_t0_epoch"

  # A p99 computed over fewer than 100 iterations is, at best, the maximum of
  # a handful of samples, not a percentile - a concurrency-1 cell recently
  # completed only 7. iterations.txt is written by assert_cell_valid above.
  local cell_iters
  cell_iters=$(cat "$dir/iterations.txt" 2>/dev/null || echo 0)
  if [[ "$cell_iters" -lt 100 ]]; then
    echo "WARN: only $cell_iters iterations this cell; p99 figures are not meaningful percentiles below ~100 samples."
  fi

  # Sampled once per cell, non-fatal - see sample_sigstore_cdn's comment. Sets
  # SIGSTORE_CDN_RTT_MS / SIGSTORE_CDN_REACHABLE, read into meta.txt below.
  sample_sigstore_cdn
  echo "sigstore CDN: rtt=${SIGSTORE_CDN_RTT_MS:-<unreachable>}ms reachable=$SIGSTORE_CDN_REACHABLE"

  # Re-run the controls after the measurement, not only at install. A signing
  # certificate that falls out of its validity window mid-sweep, or a policy
  # another process replaced, turns every later cell into the failure path -
  # which is faster than verification and therefore reads as a very good
  # result. Checking at the end of each cell bounds that to one cell.
  #
  # Not under ALLOW_FAILURES: that is the concurrency ramp, where the engine is
  # saturated on purpose and the positive control would be rejected by the very
  # timeout the cell exists to provoke. Aborting there would throw away the
  # result being sought.
  if [[ "${ALLOW_FAILURES:-}" != "1" ]]; then
    verify_ivpol_active
  fi

  ( cd "$dir" && KYVERNO_PODS_RANGE="$window" KYVERNO_PODS_STEP=5 \
      bash "$RES_REPORT" "$PROM_LOCAL" ) >/dev/null 2>&1 || echo "WARN: resource report failed"

  # kubectl-top.csv is raw samples, unreadable at a glance; summarize here so
  # meta.txt carries the number anyone actually wants. A metrics-server that
  # was not ready yet leaves the file header-only - that is a missing
  # cross-check, not a failed cell, so it prints empty fields rather than exit.
  local cpu_peak="" cpu_mean="" mem_peak="" mem_mean="" cpu_samples=0
  # Comma-delimited, not whitespace: a missing field has to survive as an
  # empty string, and `read` collapses whitespace-adjacent empty fields.
  IFS=, read -r cpu_peak cpu_mean mem_peak mem_mean cpu_samples <<< "$(python3 -c '
import csv, sys
cpu, mem = [], []
with open(sys.argv[1]) as f:
    for row in csv.DictReader(f):
        try:
            cpu.append(float(row["cpu"].rstrip("m")))
            mem.append(float(row["memory"].rstrip("Mi")))
        except (KeyError, ValueError, TypeError, AttributeError):
            continue
if not cpu:
    print(",,,,0")
else:
    print(f"{max(cpu):.0f},{sum(cpu)/len(cpu):.0f},{max(mem):.0f},{sum(mem)/len(mem):.0f},{len(cpu)}")
' "$topcsv" 2>/dev/null || echo ",,,,0")"

  {
    echo "cell=$cell"
    echo "images_per_pod=$images"
    echo "registry_delay_ms=$delay"
    echo "cache_mode=$CACHE_MODE"
    echo "reports_mode=$REPORTS"
    echo "mode=${MODE:-unset}"
    echo "expect_bundle=$EXPECT_BUNDLE"
    echo "policy_template=$(basename "$POLICY_TEMPLATE")"
    echo "verify_repo=${VERIFY_REPO:-$REPO}"
    # Which backend served this cell. Not cosmetic: distribution has no
    # Referrers API, so the bundle probe Kyverno issues on every uncached
    # verification costs it two delayed round trips (404, then the fallback
    # tag) where zot costs one. Every per-image request count therefore shifts
    # with the backend, and a cell measured on one is not comparable with a
    # cell measured on the other. Cells archived before the zot swap carry no
    # such field; absence means distribution.
    echo "registry_flavor=$(registry_flavor)"
    echo "registry_referrers=$(cat "$dir/referrers.txt" 2>/dev/null || echo 0)"
    echo "registry_fallback_tag=$(cat "$dir/fallback-tag.txt" 2>/dev/null || echo 0)"
    echo "registry_sig_tag=$(cat "$dir/sig-tag.txt" 2>/dev/null || echo 0)"
    echo "registry_blobs=$(cat "$dir/blobs.txt" 2>/dev/null || echo 0)"
    echo "kyverno_version=$KYVERNO_VERSION"
    echo "kyverno_chart=${KYVERNO_CHART_PATH:-$KYVERNO_CHART}"
    echo "kyverno_image=$(cat "$RESULTS_DIR/running-image.txt" 2>/dev/null)"
    echo "kyverno_image_id=$(cat "$RESULTS_DIR/running-image-id.txt" 2>/dev/null)"
    echo "vus=$VUS"; echo "load=$LOAD"
    # Recorded explicitly rather than left to ambient environment: meta.txt
    # used to record vus=20 (the default) even on a CONCURRENCY-driven cell,
    # since VUS is never unset just because CONCURRENCY is. Empty when unset,
    # same convention as every other optional field here.
    echo "concurrency=${CONCURRENCY:-}"
    # Which load shape produced this cell. A concurrency-capped cell and a
    # rate-paced cell are different experiments and must not be averaged.
    if [[ -n "${CONCURRENCY:-}" ]]; then echo "load_shape=concurrency=$CONCURRENCY"
    else echo "load_shape=rps=$rps"; fi
    echo "window_sec=$window"
    echo "kyverno_admission_review_count=${adm_count:-}"
    echo "kyverno_admission_review_duration_avg_sec=${adm_avg:-}"
    echo "kyverno_admission_review_duration_p99_sec=${adm_p99:-}"
    # See the comment above the promq calls: this lands on Prometheus's
    # default bucket boundaries far more often than a real percentile would.
    echo "kyverno_admission_review_duration_p99_bucket_quantised=yes"
    echo "iterations=$cell_iters"
    # Sigstore CDN visibility (fix for 1.18.2's unconditional tuf.Initialize,
    # opts.go:46): empty/no on a failed or skipped sample, never fatal.
    echo "sigstore_cdn_rtt_ms=${SIGSTORE_CDN_RTT_MS:-}"
    echo "sigstore_cdn_reachable=${SIGSTORE_CDN_REACHABLE:-no}"
    # Derived from the version, not sampled: every image verification on
    # 1.18.x pays this unconditionally (checkOptions, opts.go:46); 1.19+
    # short-circuits it for non-keyless attestors (skipSigstoreInfra).
    case "$KYVERNO_VERSION" in
      1.18.*) echo "tuf_per_verification=yes" ;;
      *)      echo "tuf_per_verification=no" ;;
    esac
    echo "cpu_millicores_peak=$cpu_peak"
    echo "cpu_millicores_mean=$cpu_mean"
    echo "memory_mib_peak=$mem_peak"
    echo "memory_mib_mean=$mem_mean"
    echo "cpu_samples=$cpu_samples"
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

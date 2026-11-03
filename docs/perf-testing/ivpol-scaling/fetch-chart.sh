#!/usr/bin/env bash
# Extract the Kyverno Helm chart from a git tag, for versions with no published
# chart (release candidates, mostly).
#
# Why this exists: v1.19.2-rc.1 has an image on ghcr but no chart on
# kyverno.github.io, so `helm install kyverno/kyverno --version ...` cannot
# reach it. The chart in the repo at that tag can.
#
# The trap this guards against: at v1.19.2-rc.1 the chart still declares
# `version: 3.9.1` and `appVersion: v1.19.1` — it was not bumped for the RC. So
# installing it as-is pulls the *1.19.1* image while the operator believes they
# are measuring the RC. The image tag must be overridden explicitly, and
# ivpol-run.sh asserts afterwards that the running pod carries the tag that was
# asked for.
#
# Usage:
#   bash fetch-chart.sh v1.19.2-rc.1              # prints the chart path
#   CHART_CACHE=/tmp/charts bash fetch-chart.sh v1.19.2-rc.1
set -euo pipefail

TAG="${1:?usage: fetch-chart.sh GIT_TAG}"
HERE="$(cd "$(dirname "$0")" && pwd)"
KYVERNO_REPO="${KYVERNO_REPO:-$(cd "$HERE/../../.." && pwd)}"
CHART_CACHE="${CHART_CACHE:-$HERE/.charts}"
DEST="$CHART_CACHE/$TAG"

command -v helm >/dev/null || { echo "FATAL: helm required" >&2; exit 1; }

if [[ -f "$DEST/charts/kyverno/Chart.yaml" ]]; then
  echo "$DEST/charts/kyverno"
  exit 0
fi

git -C "$KYVERNO_REPO" rev-parse "$TAG" >/dev/null 2>&1 \
  || { echo "FATAL: tag $TAG not found in $KYVERNO_REPO (try: git fetch upstream --tags)" >&2; exit 1; }

mkdir -p "$DEST"
# archive rather than checkout: leaves the working tree and HEAD alone, which
# matters because a ladder may be mid-run in another worktree of this repo.
git -C "$KYVERNO_REPO" archive "$TAG" charts/ | tar x -C "$DEST"

# The chart has remote dependencies (kyverno-api, openreports) that must be
# fetched before it can install from a directory.
helm dependency build "$DEST/charts/kyverno" >/dev/null 2>&1 \
  || helm dependency update "$DEST/charts/kyverno" >/dev/null 2>&1 \
  || { echo "FATAL: could not resolve chart dependencies for $TAG" >&2; exit 1; }

echo "$DEST/charts/kyverno"

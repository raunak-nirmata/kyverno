#!/usr/bin/env bash
set -euo pipefail

GO_VERSION=$(grep '^go ' go.mod | awk '{print $2}')
echo "🔍 go.mod version: $GO_VERSION"
echo

# find all Dockerfile.fips files in repo
FILES=$(find . -name "Dockerfile.fips")

MISMATCH=0

for file in $FILES; do
  echo "📂 Checking $file ..."
  # extract all Go versions used in this Dockerfile
  versions=$(grep -Eo 'golang:[0-9]+\.[0-9]+(\.[0-9]+)?' "$file" | sed 's/.*golang://')

  if [ -z "$versions" ]; then
    echo "  ⚠️  No golang image found in $file"
    continue
  fi

  echo "  Found versions: $versions"

  for v in $versions; do
    if [ "$v" != "$GO_VERSION" ]; then
      echo "  ❌ Mismatch: go.mod = $GO_VERSION, but $file uses $v"
      MISMATCH=1
    else
      echo "  ✅ Match: $v"
    fi
  done
  echo
done

if [ $MISMATCH -eq 1 ]; then
  echo "❌ One or more Docker.fips files are out of sync with go.mod"
  exit 1
fi

echo "✅ All Docker.fips files are consistent with go.mod"

# check Go version in .github/workflows/reuse.yaml
REUSE_YAML=".github/workflows/reuse.yaml"

if [ -f "$REUSE_YAML" ]; then
  # extract the go-version from the setup-go step
  SETUP_GO_VERSION=$(grep -Eo 'go-version: *~?[0-9]+\.[0-9]+(\.[0-9]+)?' "$REUSE_YAML" | sed -E 's/.*go-version: *~?([0-9]+\.[0-9]+(\.[0-9]+)?)/\1/')
  echo "🔍 $REUSE_YAML setup-go version: go-version: $SETUP_GO_VERSION"
  if [ -z "$SETUP_GO_VERSION" ]; then
    echo "⚠️  No go-version found in setup-go step of $REUSE_YAML"
  else
    if [ "$SETUP_GO_VERSION" != "$GO_VERSION" ]; then
      echo "❌ Mismatch: go.mod = $GO_VERSION, but $REUSE_YAML uses go-version: $SETUP_GO_VERSION"
      exit 1
    else
      echo "✅ $REUSE_YAML uses compatible Go version $SETUP_GO_VERSION (matches go.mod $GO_VERSION)"
    fi
  fi
else
  echo "⚠️  $REUSE_YAML not found"
fi

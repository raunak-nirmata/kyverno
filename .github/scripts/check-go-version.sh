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

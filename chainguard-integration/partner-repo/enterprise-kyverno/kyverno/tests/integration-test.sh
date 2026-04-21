#!/usr/bin/env bash
# Integration smoke test for the kyverno admission controller binary.
#
# Environment variables:
#   BINARY_PATH  Full path to the binary under test.
#                Defaults to ./kyverno (relative to this script's directory).
#
# Required tools: bash, file
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BINARY="${BINARY_PATH:-${SCRIPT_DIR}/../kyverno}"

echo "==> Test: kyverno admission controller"
echo "    Binary: $BINARY"

# 1. Binary must exist and be executable
echo "--> Checking binary exists and is executable..."
if [[ ! -f "$BINARY" ]]; then
  echo "FAIL: binary not found at $BINARY"
  exit 1
fi
if [[ ! -x "$BINARY" ]]; then
  echo "FAIL: binary is not executable: $BINARY"
  exit 1
fi

# 2. Binary must be a Linux ELF (cross-compiled for the container)
echo "--> Checking binary is a Linux ELF..."
FILE_OUTPUT="$(file "$BINARY")"
echo "    $FILE_OUTPUT"
if ! echo "$FILE_OUTPUT" | grep -qi "ELF"; then
  echo "FAIL: binary does not appear to be an ELF executable"
  exit 1
fi

# 3. Binary must start without crashing (exit 139 = SIGSEGV)
echo "--> Running: kyverno version"
"$BINARY" version 2>&1 || EXIT_CODE=$?
EXIT_CODE="${EXIT_CODE:-0}"
if [[ "$EXIT_CODE" -eq 139 ]]; then
  echo "FAIL: binary crashed with SIGSEGV (exit 139)"
  exit 1
fi
echo "    exit code: $EXIT_CODE (acceptable)"

echo "PASS: kyverno admission controller smoke test passed"

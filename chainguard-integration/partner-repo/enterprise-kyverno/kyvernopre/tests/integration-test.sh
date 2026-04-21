#!/usr/bin/env bash
# Integration smoke test for the kyvernopre init container binary.
#
# Environment variables:
#   BINARY_PATH  Full path to the binary under test.
#                Defaults to ./kyvernopre (relative to this script's directory).
#
# Required tools: bash, file
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BINARY="${BINARY_PATH:-${SCRIPT_DIR}/../kyvernopre}"

echo "==> Test: kyvernopre init container"
echo "    Binary: $BINARY"

echo "--> Checking binary exists and is executable..."
if [[ ! -f "$BINARY" ]]; then
  echo "FAIL: binary not found at $BINARY"
  exit 1
fi
if [[ ! -x "$BINARY" ]]; then
  echo "FAIL: binary is not executable: $BINARY"
  exit 1
fi

echo "--> Checking binary is a Linux ELF..."
FILE_OUTPUT="$(file "$BINARY")"
echo "    $FILE_OUTPUT"
if ! echo "$FILE_OUTPUT" | grep -qi "ELF"; then
  echo "FAIL: binary does not appear to be an ELF executable"
  exit 1
fi

# kyvernopre is an init container — it runs and exits. We check --help does not crash.
echo "--> Running: kyvernopre --help"
EXIT_CODE=0
"$BINARY" --help 2>&1 || EXIT_CODE=$?
if [[ "$EXIT_CODE" -eq 139 ]]; then
  echo "FAIL: binary crashed with SIGSEGV (exit 139)"
  exit 1
fi
echo "    exit code: $EXIT_CODE (acceptable)"

echo "PASS: kyvernopre init container smoke test passed"

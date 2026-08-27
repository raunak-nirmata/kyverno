#!/usr/bin/env bash
# Embed a signed N4K license into pkg/nirmata/license/community_license.go for local
# cluster testing.
#
# Production licenses come from the generate-n4k-community-license workflow in
# nirmata/license-gen. This is not a bypass: the license must still verify against
# the public key in public_key.go, so one signed with any other key is rejected at
# runtime exactly as in production.
#
#   hack/embed-n4k-license.sh path/to/n4k-community.lic   # embed
#   hack/embed-n4k-license.sh --restore                   # put the placeholder back
#
# Always --restore before committing. A dev-signed license on a release branch would
# silently define the free tier using a key Nirmata does not control.

set -euo pipefail

TARGET="pkg/nirmata/license/community_license.go"

if [[ ! -f "$TARGET" ]]; then
	echo "error: $TARGET not found — run this from the repository root" >&2
	exit 1
fi

# restore rewrites the committed placeholder. It prefers git, but falls back to a
# literal write because community_license.go may not be committed yet — in which case
# `git checkout --` would restore the embedded licence from the index, not the
# placeholder.
restore() {
	if git cat-file -e "HEAD:$TARGET" 2>/dev/null; then
		git checkout HEAD -- "$TARGET"
		echo "Restored $TARGET from HEAD."
		return
	fi
	cat > "$TARGET" <<'GOEOF'
package license

// EmbeddedN4KCommunityLicense is the signed N4K community license, defining the
// free tier: policy cap, node-unit cap and trial window.
//
// This placeholder is empty on purpose. It is filled by the
// generate-n4k-community-license workflow in nirmata/license-gen, which signs the
// claims and opens a PR. While empty, LoadActive returns
// errCommunityLicenseMissing and the evaluator reports nothing.
//
// hack/embed-n4k-license.sh writes a locally signed license here for testing; it
// must still verify against public_key.go. Restore this placeholder before
// committing. See README.md.
const EmbeddedN4KCommunityLicense = ``
GOEOF
	gofmt -w "$TARGET"
	echo "Rewrote $TARGET with the empty placeholder."
}

if [[ "${1:-}" == "--restore" ]]; then
	restore
	exit 0
fi

LIC="${1:-}"
if [[ -z "$LIC" ]]; then
	echo "usage: $0 <path-to-.lic> | --restore" >&2
	exit 2
fi
if [[ ! -f "$LIC" ]]; then
	echo "error: $LIC does not exist" >&2
	exit 1
fi

# Check the file shape first, so a malformed licence fails here rather than at
# runtime with a confusing "failed to verify".
LINES=$(grep -c . "$LIC" || true)
if [[ "$LINES" -ne 3 ]]; then
	echo "error: expected 3 non-empty lines in $LIC, found $LINES" >&2
	echo "       a license is: header / base64(claims) / base64(signature)" >&2
	exit 1
fi
if [[ "$(sed -n 1p "$LIC")" != "nctl-license-v1" ]]; then
	echo "error: $LIC does not start with the nctl-license-v1 header" >&2
	exit 1
fi

echo "--- claims being embedded ---"
sed -n 2p "$LIC" | base64 -d | python3 -m json.tool

LICENSE_BODY=$(cat "$LIC")
GENERATED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)

cat > "$TARGET" <<GOEOF
package license

// EmbeddedN4KCommunityLicense is the signed N4K community license.
//
// # LOCALLY EMBEDDED — DO NOT COMMIT
//
// Written by hack/embed-n4k-license.sh at ${GENERATED_AT}, not by the
// generate-n4k-community-license workflow. Almost certainly signed with a
// development key, and must not reach a release branch.
//
// Restore the placeholder with:
//	hack/embed-n4k-license.sh --restore
const EmbeddedN4KCommunityLicense = \`${LICENSE_BODY}\`
GOEOF

gofmt -w "$TARGET"

echo
echo "Wrote $TARGET. Verifying it loads against the committed public key..."

# The real proof: a signature that does not match public_key.go fails here rather
# than in a cluster.
if go test ./pkg/nirmata/license/... -run TestLoadActiveWithEmbeddedLicense -count=1 >/dev/null 2>&1; then
	echo "  OK — LoadActive accepted the embedded license."
else
	echo "  FAILED — the .lic was signed with a key that does not match"
	echo "  EmbeddedN4KLicensePublicKeyPEM in pkg/nirmata/license/public_key.go."
fi

echo
echo "Remember: $0 --restore before committing anything."

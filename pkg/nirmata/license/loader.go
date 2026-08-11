package license

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"github.com/nirmata/license-guardian/pkg/nctllicense"
)

// Origin records which license supplied the active claims.
type Origin string

const (
	// OriginCommunity is the embedded signed license defining the free tier.
	OriginCommunity Origin = "community"

	// OriginFile is an operator-supplied commercial license. Phase 2; never
	// produced today.
	OriginFile Origin = "file"
)

// errCommunityLicenseMissing means this build has no community license embedded.
// A build state, not a customer misconfiguration.
var errCommunityLicenseMissing = errors.New(
	"no community license embedded in this build; " +
		"run the generate-n4k-community-license workflow in nirmata/license-gen and merge the resulting PR")

// Active is the license in force, along with where it came from.
type Active struct {
	Claims nctllicense.Claims
	Origin Origin
	Limits Limits

	// NotAfter is the parsed expiry, zero for a community license (which carries
	// the never-expires sentinel). Parsed once per load, not per use.
	NotAfter time.Time

	// Commercial is true only for a verified, in-window commercial license.
	Commercial bool
}

// LoadActive resolves the license in force at now. Phase 1 has one source, the
// embedded community license; Phase 2 will try a file first, which is why the return
// type already carries Origin and Commercial.
//
// An error means the community license is unusable. Callers must report nothing and
// keep running — never degrade the cluster.
func LoadActive(now time.Time) (Active, error) {
	// Verified against N4K's own trust root, not nctl's. See public_key.go.
	pub, err := n4kPublicKey()
	if err != nil {
		return Active{}, err
	}
	return loadCommunity(EmbeddedN4KCommunityLicense, pub, now)
}

// loadCommunity parses and verifies a community license body. The public key is a
// parameter so tests can use a throwaway pair rather than the CI signing key.
func loadCommunity(body string, pub ed25519.PublicKey, now time.Time) (Active, error) {
	if body == "" {
		return Active{}, errCommunityLicenseMissing
	}
	claims, err := nctllicense.ParseAndVerify([]byte(body), pub)
	if err != nil {
		return Active{}, fmt.Errorf("community license: %w", err)
	}
	// Sentinel expiry means this checks only not_before — still needed, or a clock
	// skewed before it would read as a valid window.
	if err := nctllicense.ClaimsValidNow(claims, now); err != nil {
		return Active{}, fmt.Errorf("community license: %w", err)
	}
	return Active{
		Claims:     claims,
		Origin:     OriginCommunity,
		Limits:     ResolveLimits(claims),
		NotAfter:   parseNotAfter(claims),
		Commercial: false,
	}, nil
}

// parseNotAfter returns the license expiry as a time, or the zero time when the
// license carries the community sentinel and therefore has no real expiry.
func parseNotAfter(c nctllicense.Claims) time.Time {
	if c.NotAfter == "" || c.NotAfter == nctllicense.CommunityNotAfter {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, c.NotAfter)
	if err != nil {
		// Unreachable: ClaimsValidNow already rejected an unparseable not_after.
		return time.Time{}
	}
	return t.UTC()
}

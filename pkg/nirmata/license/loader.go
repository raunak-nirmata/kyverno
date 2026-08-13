package license

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/nirmata/license-guardian/pkg/nctllicense"
)

// Origin records which license supplied the active claims.
type Origin string

const (
	// OriginCommunity is the embedded signed license defining the free tier.
	OriginCommunity Origin = "community"

	// OriginFile is an operator-supplied commercial license.
	OriginFile Origin = "file"
)

// DefaultLicensePath is where the Helm chart mounts the commercial license Secret.
const DefaultLicensePath = "/var/run/kyverno/license/license"

// errCommunityLicenseMissing means this build has no community license embedded.
// A build state, not a customer misconfiguration.
var errCommunityLicenseMissing = errors.New(
	"no community license embedded in this build; " +
		"run the generate-n4k-community-license workflow in nirmata/license-gen and merge the resulting PR")

// errLicenseFileInvalid means an on-disk license was present but could not be
// verified. Distinct from a missing file so the evaluator can enter the paid
// non-compliance ladder (Grace/Degraded) rather than the free-tier path.
var errLicenseFileInvalid = errors.New("commercial license file is present but invalid")

// Active is the license in force, along with where it came from.
type Active struct {
	Claims nctllicense.Claims
	Origin Origin
	Limits Limits

	// NotAfter is the parsed expiry, zero for a community license (which carries
	// the never-expires sentinel). Parsed once per load, not per use.
	NotAfter time.Time

	// NotBefore is the parsed start of the validity window, zero when absent or
	// unparseable. Carried separately from NotAfter because the commercial path
	// deliberately skips ClaimsValidNow: without it, a correctly signed license
	// whose window has not opened yet would grant full paid capacity today.
	NotBefore time.Time

	// Commercial is true when a verified commercial license was loaded from disk.
	// The license may be past not_after; Evaluate decides Grace vs Licensed.
	Commercial bool

	// Invalid is true when a license file was present but failed signature or
	// parse checks. Evaluate treats this as paid non-compliance.
	Invalid bool

	// FileMissing is true when LicensePath was configured but the file was
	// absent or empty (e.g. Secret deleted). Caller falls back to community /
	// FreeTier|FreeExpired — fail-open, no outage.
	FileMissing bool

	// FileError is the underlying reason when Invalid is true. Empty otherwise.
	FileError error
}

// LoadActive resolves the license in force at now. Prefers an operator-supplied
// file at path when set; otherwise uses the embedded community license.
//
// An error means the community license is unusable (and no usable commercial
// file was loaded). Callers must report nothing and keep running — never degrade
// the cluster.
func LoadActive(now time.Time, path string) (Active, error) {
	pub, err := n4kPublicKey()
	if err != nil {
		return Active{}, err
	}
	if path != "" {
		if active, ok, fileErr := loadFile(path, pub, now); ok {
			return active, nil
		} else if fileErr != nil {
			community, cerr := loadCommunity(EmbeddedN4KCommunityLicense, pub, now)
			if cerr != nil {
				return Active{}, cerr
			}
			community.Invalid = true
			community.FileError = fileErr
			community.Origin = OriginFile
			community.Limits.Policies = Unlimited()
			community.Limits.NodeUnits = Unlimited()
			return community, nil
		}
		// Path configured but file gone (Secret deleted / optional mount empty).
		// Fall back to free tier — never fail the process.
		community, cerr := loadCommunity(EmbeddedN4KCommunityLicense, pub, now)
		if cerr != nil {
			return Active{}, cerr
		}
		community.FileMissing = true
		return community, nil
	}
	return loadCommunity(EmbeddedN4KCommunityLicense, pub, now)
}

// loadFile tries to load a commercial license from path.
// ok is true when a verified commercial license was produced.
// When the file is absent or empty, ok is false and err is nil (caller falls
// through to community / free tier — fail-open if the Secret was deleted).
// When the file is present but bad, ok is false and err explains why.
func loadFile(path string, pub ed25519.PublicKey, now time.Time) (Active, bool, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Active{}, false, nil
		}
		// Optional Secret mounts can leave a directory where the file should be,
		// or return other transient read errors. Treat those as missing so the
		// admission controller stays up and free-tier evaluation continues.
		if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
			return Active{}, false, nil
		}
		return Active{}, false, fmt.Errorf("%w: read: %v", errLicenseFileInvalid, err)
	}
	if len(body) == 0 {
		// Empty file after Secret deletion / rotation — fail-open to free tier.
		return Active{}, false, nil
	}

	claims, err := nctllicense.ParseAndVerify(body, pub)
	if err != nil {
		return Active{}, false, fmt.Errorf("%w: %v", errLicenseFileInvalid, err)
	}

	// Community artifact mistakenly mounted as a "commercial" file: treat as
	// community rather than unlocking paid posture from the free-tier blob.
	if claims.LicenseType == "community" || claims.NotAfter == nctllicense.CommunityNotAfter {
		active, err := loadCommunity(string(body), pub, now)
		if err != nil {
			return Active{}, false, err
		}
		active.Origin = OriginFile
		return active, true, nil
	}

	// Signature OK. Do not require ClaimsValidNow here: an expired commercial
	// license must still feed Evaluate so Grace/Degraded can run, rather than
	// being rejected at load. The same applies to a window that has not opened
	// yet, so not_before is carried through for Evaluate to rule on — it is
	// non-compliant paid posture, not a valid license.
	return Active{
		Claims:     claims,
		Origin:     OriginFile,
		Limits:     ResolveLimits(claims),
		NotAfter:   parseNotAfter(claims),
		NotBefore:  parseNotBefore(claims),
		Commercial: true,
	}, true, nil
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
// parseNotBefore is the start of the validity window, zero when absent.
//
// Unlike not_after there is no sentinel: an absent not_before means "valid from
// issue", which is the common case and must not be mistaken for a future window.
// An unparseable value returns zero rather than an error, so a malformed claim
// fails open to "already started" instead of locking a paying customer out.
func parseNotBefore(c nctllicense.Claims) time.Time {
	if c.NotBefore == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, c.NotBefore)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func parseNotAfter(c nctllicense.Claims) time.Time {
	if c.NotAfter == "" || c.NotAfter == nctllicense.CommunityNotAfter {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, c.NotAfter)
	if err != nil {
		// Unreachable: ParseAndVerify already produced a Claims; callers that
		// also ran ClaimsValidNow already rejected an unparseable not_after.
		return time.Time{}
	}
	return t.UTC()
}

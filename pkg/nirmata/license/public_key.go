package license

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"github.com/nirmata/license-guardian/pkg/nctllicense"
)

// EmbeddedN4KLicensePublicKeyPEM is the trust root for N4K licenses: a license
// signed by any other key is rejected.
//
// Deliberately not the nctl key, so the two products have independent trust roots.
// The private half exists only as the N4K_LICENSE_PRIVATE_KEY secret in
// nirmata/license-gen. This half is not secret — it ships in every binary, which is
// what makes offline verification work.
//
// Provisioning order and rotation cost are covered in README.md; both matter.
const EmbeddedN4KLicensePublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAETWAn1MNjYObFJ9+I1UFrUq9i5AeKIEwc2OSMiemSLg=
-----END PUBLIC KEY-----
`

// errTrustRootMissing means this build carries no public key, so there is nothing
// to verify against. Treated as unprovisioned, not as a defect.
var errTrustRootMissing = errors.New(
	"no N4K license public key embedded in this build; " +
		"generate the N4K key pair and commit the public half to pkg/nirmata/license/public_key.go")

// n4kPublicKey returns the trust root for N4K licenses.
func n4kPublicKey() (ed25519.PublicKey, error) {
	if EmbeddedN4KLicensePublicKeyPEM == "" {
		return nil, errTrustRootMissing
	}
	// SDK parser, so the encoding matches what the signing tool produces.
	pub, err := nctllicense.PublicKeyFromPEM([]byte(EmbeddedN4KLicensePublicKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("embedded N4K license public key: %w", err)
	}
	return pub, nil
}

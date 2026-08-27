package license

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The Helm chart writes the license into a Secret with `stringData` and a `|`
// block scalar, which appends a trailing newline. loadFile passes the bytes to
// ParseAndVerify untrimmed, so if the parser is strict about surrounding
// whitespace then every license installed through the chart fails to load.
//
// This pins the contract between charts/kyverno/templates/config/licenseSecret.yaml
// and loadFile. If it ever fails, either the loader must trim or the chart must
// stop adding the newline — silently falling back to free tier is the worst
// outcome, because it looks like the customer simply has no license.
func TestLicenseFileToleratesSurroundingWhitespace(t *testing.T) {
	t.Parallel()

	if EmbeddedN4KCommunityLicense == "" {
		t.Skip("no community license embedded in this build")
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{"exact bytes", EmbeddedN4KCommunityLicense},
		{"trailing newline (what the chart produces)", EmbeddedN4KCommunityLicense + "\n"},
		{"trailing CRLF", EmbeddedN4KCommunityLicense + "\r\n"},
		{"trailing blank line", EmbeddedN4KCommunityLicense + "\n\n"},
		{"leading newline", "\n" + EmbeddedN4KCommunityLicense},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "license")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			active, err := LoadActive(time.Now(), path)
			if err != nil {
				t.Fatalf("LoadActive failed: %v", err)
			}
			if active.Invalid {
				t.Errorf("license rejected as invalid with %s: %v", tc.name, active.FileError)
			}
			if active.FileMissing {
				t.Errorf("license read as missing with %s", tc.name)
			}
		})
	}
}

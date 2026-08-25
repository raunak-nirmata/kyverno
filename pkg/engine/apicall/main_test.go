package apicall

import (
	"os"
	"testing"
)

// TestMain installs a metadata-only apiCall blocklist for this package.
//
// Most tests here drive real httptest.NewServer instances, which bind loopback, so the
// baseline must not block it regardless of what the shipped default does. Pinning the
// baseline here also keeps these tests independent of changes to that default. The
// override is applied through the
// environment variable rather than through Parse() so that a test calling
// toggle.APICallBlocklist.Reset() falls back to this baseline instead of to the
// production default — otherwise the first test to Reset() would break every loopback
// test that ran after it.
//
// Tests that need to exercise the shipped defaults select them explicitly via
// toggle.DefaultAPICallBlocklist().
func TestMain(m *testing.M) {
	os.Setenv("FLAG_APICALL_BLOCKLIST", "169.254.169.254,metadata.google.internal")
	code := m.Run()
	os.Unsetenv("FLAG_APICALL_BLOCKLIST")
	os.Exit(code)
}

package mockgraph

import (
	"os"
	"testing"
)

// Live reports whether the test suites should run against a real OneDrive
// account (ONEDRIVER_TEST_LIVE=1) instead of the mock. The mock is the default.
func Live() bool {
	return os.Getenv("ONEDRIVER_TEST_LIVE") == "1"
}

// SkipUnlessLive skips a test that can only be meaningful against the real
// Microsoft Graph service.
func SkipUnlessLive(t testing.TB, reason string) {
	t.Helper()
	if !Live() {
		t.Skip("requires a live OneDrive account (ONEDRIVER_TEST_LIVE=1): " + reason)
	}
}

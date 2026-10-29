package testutils

import (
	"runtime"
	"strings"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kuberecord"
)

// SkipWithIssue is the default way to skip a test. A JIRA issue is required to track
// work that will be required to re-enable the test.
func SkipWithIssue(t *testing.T, jiraIssue string, reason ...string) {
	t.Helper()
	if len(reason) > 0 {
		t.Skipf("Skipped test with JIRA: %s; reason: %s", jiraIssue, strings.Join(reason, " ")) // nolint:testskip
	} else {
		t.Skipf("Skipped test with JIRA: %s", jiraIssue) // nolint:testskip
	}
}

func SkipIfKuberecord(t *testing.T) {
	t.Helper()
	if kuberecord.IsEnabled() {
		t.Skip("Skipped test because kuberecord is enabled")
	}
}

// MarkFlakyTest causes a test to be skipped if it is known to be flaky. If no JIRA
// already exists for the test, create one, then note its key (eg "CC-1234") here.
func MarkFlakyTest(t *testing.T, jiraIssue string, reason ...string) {
	t.Helper()
	reason = append([]string{"flaky"}, reason...)
	SkipWithIssue(t, jiraIssue, reason...)
}

// MarkIntegrationTest marks a test as an integration test, and causes it not to run
// in the unit test suite (and thus not on PR builds). Will fail your test if called
// from a test that does not begin with "TestIntegration" (which is required for a
// test to run in the integration suite)
func MarkIntegrationTest(t *testing.T) {
	t.Helper()

	// Make sure the test actually is an integration test
	pc, _, _, ok := runtime.Caller(1)
	if !ok {
		t.Fatal("unable to get caller in call to testutils.MarkIntegrationTest")
	}
	caller := runtime.FuncForPC(pc)
	if caller == nil {
		t.Fatal("unable to get caller details in call to testutils.MarkIntegrationTest")
	}
	name := caller.Name() // is something like "github.com/cockroachlabs/cockroach-enterprise-operator/foo/bar_test.TestFunc"
	parts := strings.Split(name, ".")
	funcName := parts[len(parts)-1]
	if !strings.HasPrefix(funcName, "TestIntegration") {
		t.Fatalf("testutils.MarkIntegrationTest used in non-integration test '%s'", funcName)
	}

	if !IntegrationTestsEnabled {
		t.Skip("Skipped integration test running in non-integration build") // nolint:testskip
	}
}

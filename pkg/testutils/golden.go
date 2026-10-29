package testutils

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime"
)

// update controls whether golden assertions will fail on a mismatch or
// write the new value.
var update = flag.Bool("update-golden", false, "update all golden files")

// AssertGolden matches the given actual value with an expected value
// previously written to a file.
//
// To write a new golden file or update an existing one, pass the
// `-update` flag at the end of your test command. The filename depends
// on the test name, so you will need to update the file when that
// changes.
func AssertGolden(t *testing.T, label string, actual []byte) {
	t.Helper()
	if !matchesGolden(t, label, actual) {
		t.Fatalf(
			"%s does not match. to update, run your test again with `-update`",
			GoldenPath(t, label),
		)
	}
}

// AssertGoldenJSON matches the given actual value with an expected value
// previously written to a file, when both actual and expected are treated as
// JSON objects.
//
// To write a new golden file or update an existing one, pass the
// `-update` flag at the end of your test command. The filename depends
// on the test name, so you will need to update the file when that
// changes.
func AssertGoldenJSON(t *testing.T, label string, actual []byte) {
	t.Helper()
	if !matchesGoldenJSON(t, label, actual) {
		t.Fatalf(
			"%s does not match. to update, run your test again with `-update`",
			GoldenPath(t, label),
		)
	}
}

// AssertGoldenManifestsIndentedJSON wraps AssertGoldenJSON and asserts that a
// series of serialized manifest objects matches an expected value previously
// written to a file.
//
// To write a new golden file or update an existing one, pass the
// `-update` flag at the end of your test command. The filename depends
// on the test name, so you will need to update the file when that
// changes.
func AssertGoldenManifestsIndentedJSON(t *testing.T, label string, o ...runtime.Object) {
	actual, err := json.MarshalIndent(o, "", "\t")
	assert.NoError(t, err)

	AssertGoldenJSON(t, label, actual)
}

func shouldUpdateGoldenFiles(t *testing.T) bool {
	if *update {
		return true
	}
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		return true
	}
	return false
}

func loadGolden(t *testing.T, label string, actual []byte) []byte {
	t.Helper()
	path := GoldenPath(t, label)

	if shouldUpdateGoldenFiles(t) {
		if err := os.MkdirAll("testdata", 0755); err != nil {
			t.Fatalf("updating golden: %s", err)
		}
		if err := os.WriteFile(path, actual, 0644); err != nil {
			t.Fatalf("writing golden: %s", err)
		}
		t.Logf("updated %s:\n%s", path, actual)
	}

	expected, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("reading golden: %s\ntry running your test again with `-update`", err)
	}
	return expected
}

// matchesGolden reads the contents of the relevant file and compares
// its contents with actual.
func matchesGolden(t *testing.T, label string, actual []byte) bool {
	t.Helper()
	expected := loadGolden(t, label, actual)
	return assert.Equal(t, string(expected), string(actual))
}

// matchesGoldenJSON reads the contents of the relevant file and compares
// its contents with actual using JSONEq (which checks that two JSON
// strings are equivalent).
func matchesGoldenJSON(t *testing.T, label string, actual []byte) bool {
	t.Helper()
	expected := loadGolden(t, label, actual)
	return assert.JSONEq(t, string(expected), string(actual))
}

// GoldenPath constructs a file path from the given test and label.
func GoldenPath(t *testing.T, label string) string {
	return filepath.Join(
		"testdata",
		fmt.Sprintf(
			"%s.%s.golden",
			strings.ReplaceAll(t.Name(), string(filepath.Separator), "_"),
			// Bazel builds don't like testdata files with spaces in the names.
			strings.ReplaceAll(label, " ", "-"),
		),
	)
}

// GoldenMatcher is a gomock.Matcher that is similar to AssertGolden.
type GoldenMatcher struct {
	t         *testing.T
	label     string
	getActual func(any) []byte
}

// AssertGoldenString matches the given actual string with an expected value
// previously written to a file.
//
// To write a new golden file or update an existing one, pass the
// `-update-golden` flag at the end of your test command. The filename depends
// on the test name, so you will need to update the file when that
// changes.
func AssertGoldenString(t *testing.T, label string, actual []byte) {
	t.Helper()
	AssertGolden(t, label, actual)
}

// MatchGolden constructs a gomock.Matcher that checks against the
// golden file implied by the current test and the given label.
//
// getActual will be called with values received by the mock in
// question, and its return value will be compared with the relevant
// golden file's contents.
//
// Example:
//
//	type Widget interface {
//	  Scribble(string) error
//	}
//	ctrl := gomock.NewController(t)
//	widget := NewMockWidget(ctrl)
//	widget.EXPECT().Scribble(MatchGolden(t, "Scribble's first argument",
//	  func(actual interface{}) []byte {
//	    return []byte(actual.(string))
//	  },
//	).Return(nil)
//
// The update behavior is the same as that in AssertGolden.
func MatchGolden(t *testing.T, label string, getActual func(any) []byte) *GoldenMatcher {
	return &GoldenMatcher{t, label, getActual}
}

func (m *GoldenMatcher) String() string {
	return GoldenPath(m.t, m.label)
}

func (m *GoldenMatcher) Matches(x any) bool {
	actual := m.getActual(x)
	return matchesGolden(m.t, m.label, actual)
}

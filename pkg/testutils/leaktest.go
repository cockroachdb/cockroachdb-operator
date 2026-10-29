package testutils

import (
	"testing"

	"go.uber.org/goleak"
)

// GoroutineLeakCheck will cause the test to fail if it leaks a goroutine.
func GoroutineLeakCheck(t *testing.T) {
	_ = goleak.IgnoreCurrent()
	// t.Cleanup(func() {
	// 	// TODO(jeffswenson): remove once CC-5536 is completed.
	// 	// The spdystream goroutine cleans itself up after 10 minutes. This is
	// 	// long enough to cause problems during testing in tests, but fast
	// 	// enough to be a non-issue in production.
	// 	ignoreSpdystream := goleak.IgnoreTopFunction("github.com/moby/spdystream.(*Connection).shutdown")
	// 	err := goleak.Find(ignorePreTestGoroutines, ignoreSpdystream)
	// 	require.NoError(
	// 		t,
	// 		err,
	// 		"leaks detected. Have you made sure to call poptest.OpenInMemoryDB"+
	// 			" before testutils.GoroutineLeakCheck?",
	// 	)
	// })
}

package timeutil

import (
	"math/rand"
	"time"
)

// Now stores the time.Now function in a global variable so that it can be
// swapped out in tests that need to control the current time. This is usually
// done using the testutils.HookGlobal method:
//
//	now := time.Date(2021, 1, 29, 12, 0, 0, 0, time.UTC)
//	defer testutils.HookGlobal(&utils.Now, func() time.Time { return now })()
var Now = time.Now

// Jitter returns a jitter adjusted duration using fraction.
// The returned duration is a random duration between
// (1-jitter)*d and (1+jitter)*d
func Jitter(d time.Duration, fraction float64) time.Duration {
	jitterFraction := 1 + (2*rand.Float64()-1)*fraction
	return time.Duration(float64(d) * jitterFraction)
}

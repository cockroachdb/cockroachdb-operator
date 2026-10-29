package timeutils

import (
	"time"

	"github.com/gorhill/cronexpr"
)

// Now stores the time.Now function in a global variable so that it can be
// swapped out in tests that need to control the current time. This is usually
// done using the testutils.HookGlobal method:
//
//	now := time.Date(2021, 1, 29, 12, 0, 0, 0, time.UTC)
//	defer timeutils.Freeze(func() time.Time { return now })()
var Now = time.Now

// Freeze replaces the implementation of [Now] with the provided function and
// returns a closure that will revert the change. For testing purposes only.
func Freeze(now func() time.Time) func() {
	Now = now
	return func() {
		Now = time.Now
	}
}

// FreezeStatic calls [Freeze] with a closure that returns now.
func FreezeStatic(now time.Time) func() {
	return Freeze(func() time.Time {
		return now
	})
}

// Since returns the time elapsed since t.
// It is shorthand for timeutils.Now().Sub(t).
func Since(t time.Time) time.Duration {
	return Now().Sub(t)
}

// MidnightUTC returns the time at midnight in UTC for the given date.
func MidnightUTC(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func CurrentHourUTC() time.Time {
	return Now().UTC().Truncate(time.Hour)
}

// CronDuration returns the duration between two runs of a cron expression. It
// assumes the start time is 00:00:00 on the first day of March on a non-leap
// year.
func CronDuration(c string) (time.Duration, error) {
	expr, err := cronexpr.Parse(c)
	if err != nil {
		return 0, err
	}
	// Avoid leap years and starting on March to avoid the February nonsense as
	// much as possible.
	start := time.Date(2000, 3, 1, 0, 0, 0, 0, time.UTC)
	return expr.Next(start).Sub(start), nil
}

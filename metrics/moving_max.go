package metrics

import (
	"container/list"
	"time"

	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
)

// movingMax implements a sliding windows max value over a period of time.
// It does that with a list, that contains all significant values in the
// given window. It is sorted in descending order with the highest value first
// and the lowest value last. Adding a new element is implemented as first removing
// all elements from the tail that are lower than the new element, then appending
// the new element.
type movingMax struct {
	list     *list.List
	duration time.Duration
	now      func() time.Time // To help with testing.
}

// elem stores the details for each sample - value and timestamp of the sample.
type elem struct {
	val float64
	at  time.Time
}

// newMovingMax constructs a new sliding windows max value over the given duration.
func newMovingMax(duration time.Duration) *movingMax {
	return &movingMax{
		list:     list.New(),
		duration: duration,
		now:      timeutil.Now,
	}
}

// add a sample value to the sequence. Its timestamp is Now().
func (mm *movingMax) add(val float64) {
	mm.trim()
	for {
		if mm.list.Back() == nil ||
			mm.list.Back().Value.(elem).val > val {
			mm.list.PushBack(elem{val: val, at: mm.now()})
			break
		}
		mm.list.Remove(mm.list.Back())
	}
}

// trim removes any samples that are beyond the duration look back window.
// These should be the largest values so the remaining head will be the new max.
func (mm *movingMax) trim() {
	trimTime := mm.now().Add(-mm.duration)
	for {
		if mm.list.Front() == nil ||
			mm.list.Front().Value.(elem).at.After(trimTime) {
			break
		}
		mm.list.Remove(mm.list.Front())
	}
}

// max returns the max value over the duration look back window interval.
func (mm *movingMax) max() float64 {
	mm.trim()
	if mm.list.Front() != nil {
		return mm.list.Front().Value.(elem).val
	}
	return 0
}

// sinceMax returns the duration from the time the max was recorded to now.
func (mm *movingMax) sinceMax() time.Duration {
	mm.trim()
	if mm.list.Front() != nil {
		return mm.now().Sub(mm.list.Front().Value.(elem).at)
	}
	return 0
}

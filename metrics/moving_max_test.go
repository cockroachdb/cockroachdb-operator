package metrics

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func Test_movingMax(t *testing.T) {
	type s struct {
		// Value that was added to the series.
		v float64
		// The offset in ms when add was called.
		t int64
	}

	// All tests bellow assume 10ms sliding window.
	tests := []struct {
		name        string
		samples     []s
		maxTime     int64
		expectedMax int64
	}{
		{name: "empty",
			samples:     []s{},
			maxTime:     20,
			expectedMax: 0},
		{name: "1 in (range)",
			samples:     []s{{1, 1}},
			maxTime:     2,
			expectedMax: 1},
		{name: "1 out",
			samples:     []s{{1, 1}},
			maxTime:     20,
			expectedMax: 0},
		{name: "4 in",
			samples:     []s{{1, 0}, {10, 1}, {7, 2}, {5, 3}},
			maxTime:     4,
			expectedMax: 10},
		{name: "1 out, 3 in",
			samples:     []s{{1, 0}, {10, 1}, {7, 2}, {5, 3}},
			maxTime:     10,
			expectedMax: 10},
		{name: "2 out, 2 in",
			samples:     []s{{1, 0}, {10, 1}, {7, 2}, {5, 3}},
			maxTime:     11,
			expectedMax: 7},
		{name: "3 out, 1 in",
			samples:     []s{{1, 0}, {10, 1}, {7, 2}, {5, 3}},
			maxTime:     12,
			expectedMax: 5},
		{name: "4 out, no in",
			samples:     []s{{1, 0}, {10, 1}, {7, 2}, {5, 3}},
			maxTime:     13,
			expectedMax: 0},
		{name: "5 in",
			samples:     []s{{1, 0}, {10, 1}, {7, 2}, {5, 3}, {20, 4}},
			maxTime:     5,
			expectedMax: 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mm := newMovingMax(10 * time.Millisecond)
			var nowTime int64
			mm.now = func() time.Time { return time.UnixMilli(nowTime) }
			for _, e := range tt.samples {
				nowTime = e.t
				mm.add(e.v)
			}
			nowTime = tt.maxTime
			assert.EqualValues(t, tt.expectedMax, mm.max())
		})
	}
}

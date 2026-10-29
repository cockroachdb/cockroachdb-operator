package metrics

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_movingAvg(t *testing.T) {
	// All tests bellow assume 10ms sliding window.
	tests := []struct {
		name          string
		values        []float64
		slidingWindow int
		expectedAvg   float64
	}{
		{name: "empty",
			values: []float64{}, slidingWindow: 20, expectedAvg: 0},
		{name: "single value",
			values: []float64{100}, slidingWindow: 20, expectedAvg: 5},
		{name: "all values",
			values: []float64{3, 4, 5}, slidingWindow: 3, expectedAvg: 4},
		{name: "removing oldest",
			values: []float64{3, 4, 5, 6}, slidingWindow: 3, expectedAvg: 5},
		{name: "removing oldest 2",
			values: []float64{3, 4, 5, 6, 7}, slidingWindow: 3, expectedAvg: 6},
		{name: "removing oldest 3",
			values: []float64{3, 4, 5, 6, 7, 8}, slidingWindow: 3, expectedAvg: 7},
		{name: "removing oldest 4",
			values: []float64{3, 4, 5, 6, 7, 8, 9}, slidingWindow: 3, expectedAvg: 8},
		{name: "truncating",
			values: []float64{3, 4, 5, 5}, slidingWindow: 3, expectedAvg: 4.66},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mm := newMovingAvg(tt.slidingWindow)
			for _, e := range tt.values {
				mm.add(e)
			}
			assert.EqualValues(t, tt.expectedAvg, mm.avg())
		})
	}
}

func Test_movingAvg_fillRatio(t *testing.T) {
	tests := []struct {
		name     string
		samples  int
		expected float64
	}{
		{name: "empty window", samples: 0, expected: 0},
		{name: "partially filled", samples: 50, expected: 0.5},
		{name: "fully filled", samples: 100, expected: 1.0},
		{name: "overflow capped", samples: 150, expected: 1.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ma := newMovingAvg(100)
			for range tt.samples {
				ma.add(1.0)
			}
			assert.Equal(t, tt.expected, ma.fillRatio())
		})
	}
}

func Test_movingAvg_sampleCount_caps_at_window(t *testing.T) {
	ma := newMovingAvg(10)
	for range 25 {
		ma.add(1.0)
	}
	assert.Equal(t, 10, ma.sampleCount)
}

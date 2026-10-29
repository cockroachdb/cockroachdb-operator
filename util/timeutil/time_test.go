package timeutil

import (
	"testing"
	"time"
)

func TestJitter(t *testing.T) {
	tests := []struct {
		name     string
		d        time.Duration
		fraction float64
		smallest time.Duration
		biggest  time.Duration
	}{
		{
			name:     "simple",
			d:        time.Minute,
			fraction: 0.1,
			smallest: 54 * time.Second,
			biggest:  66 * time.Second,
		},
		{
			name:     "half",
			d:        time.Minute,
			fraction: 0.5,
			smallest: 30 * time.Second,
			biggest:  90 * time.Second,
		},
		{
			name:     "exaggerated",
			d:        time.Minute,
			fraction: 1,
			smallest: 0,
			biggest:  2 * time.Minute,
		},
		{
			name:     "no jitter",
			d:        time.Minute,
			fraction: 0,
			smallest: time.Minute,
			biggest:  time.Minute + time.Nanosecond,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := 1; i < 1000; i++ {
				if got := Jitter(tt.d, tt.fraction); tt.smallest > got || got >= tt.biggest {
					t.Errorf("Jitter() = %v, want in [%v, %v)", got, tt.smallest, tt.biggest)
				}
			}
		})
	}
}

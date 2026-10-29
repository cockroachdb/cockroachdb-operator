package sliceutils

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMultiSetsEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b []string
		want bool
	}{
		{
			name: "both empty",
			a:    nil,
			b:    nil,
			want: true,
		},
		{
			name: "same elements same order",
			a:    []string{"a", "b", "c"},
			b:    []string{"a", "b", "c"},
			want: true,
		},
		{
			name: "same elements different order",
			a:    []string{"c", "a", "b"},
			b:    []string{"b", "c", "a"},
			want: true,
		},
		{
			name: "different lengths",
			a:    []string{"a", "b"},
			b:    []string{"a", "b", "c"},
			want: false,
		},
		{
			name: "same length different elements",
			a:    []string{"a", "b", "c"},
			b:    []string{"a", "b", "d"},
			want: false,
		},
		{
			name: "one nil one empty",
			a:    nil,
			b:    []string{},
			want: true,
		},
		{
			name: "different duplicate counts",
			a:    []string{"a", "a", "b"},
			b:    []string{"a", "b", "b"},
			want: false,
		},
		{
			name: "b subset of a unique elements",
			a:    []string{"a", "b", "c"},
			b:    []string{"a", "b", "a"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MultiSetsEqual(tt.a, tt.b))
		})
	}
}

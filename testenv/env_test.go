package testenv

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRandomizedZones(t *testing.T) {
	t.Run("property based test", func(t *testing.T) {
		rand.Seed(0) //nolint:staticcheck // ensure deterministic test failures
		for range 100 {
			region := randomizedZones(
				rand.Intn,
				1, // minNumNodes
				8, // maxNumNodes
				3, // minNumAZs
				5) // maxNumAZs

			numZones := len(region)
			require.GreaterOrEqual(t, numZones, 3) // min zones respected
			require.LessOrEqual(t, numZones, 5)    // max zones respected

			numNodes := 0
			for _, nodesInZone := range region {
				numNodes += nodesInZone
			}
			require.GreaterOrEqual(t, numNodes, 1) // min nodes respected
			require.LessOrEqual(t, numZones, 8)    // max nodes respected
		}
	})

	for i, tc := range []struct {
		intn        func(int) int
		minNumNodes int
		maxNumNodes int
		minNumAZs   int
		maxNumAZs   int
		want        Zones
	}{
		{
			intn:        func(x int) int { return 0 },
			minNumNodes: 4,
			maxNumNodes: 7,
			minNumAZs:   3,
			maxNumAZs:   4,
			want: Zones{
				"zone-0": 2,
				"zone-1": 1,
				"zone-2": 1,
			},
		},
		{
			intn:        func(x int) int { return x - 1 },
			minNumNodes: 4,
			maxNumNodes: 7,
			minNumAZs:   3,
			maxNumAZs:   4,
			want: Zones{
				"zone-0": 2,
				"zone-1": 2,
				"zone-2": 2,
				"zone-3": 1,
			},
		},
		{
			intn:        func(x int) int { return 1 },
			minNumNodes: 5,
			maxNumNodes: 10,
			minNumAZs:   2,
			maxNumAZs:   4,
			want: Zones{
				"zone-0": 2,
				"zone-1": 2,
				"zone-2": 2,
			},
		},
		{
			intn:        func(x int) int { return rand.Intn(x) },
			minNumNodes: 8,
			maxNumNodes: 8,
			minNumAZs:   3,
			maxNumAZs:   3,
			want: Zones{
				"zone-0": 3,
				"zone-1": 3,
				"zone-2": 2,
			},
		},
	} {
		t.Run(fmt.Sprintf("case %d", i), func(t *testing.T) {
			got := randomizedZones(tc.intn, tc.minNumNodes, tc.maxNumNodes, tc.minNumAZs, tc.maxNumAZs)
			require.Equal(t, tc.want, got)
		})
	}
}

package testenv

import (
	"fmt"
	"log"
	"math/rand"
)

// SingleZoneConfig specifies a single K8s node in a single zone.
var SingleZoneConfig = EnvConfig{
	Name:  "single-zone-test",
	Reuse: true,
	Regions: Regions{
		"us-east-1": Zones{
			"us-east-1a": 1,
		},
	},
}

// MultiZoneConfig specifies K8s nodes in three zones in a region.
var MultiZoneConfig = EnvConfig{
	Name:  "multi-zone-test",
	Reuse: true,
	Regions: Regions{
		"us-east-1": Zones{
			"us-east-1a": 1,
			"us-east-1b": 1,
			"us-east-1c": 1,
		},
	},
}

// RandomizedMultiZoneConfig returns an EnvConfig for a single region k8s
// cluster with a randomized number of AZs and k8s nodes.  minNumNodes,
// maxNumNodes, etc. are inclusive bounds. k3d cluster reuse is turned off
// since this can't be reused in other tests.
func RandomizedMultiZoneConfig(minNumNodes, maxNumNodes, minNumAZs, maxNumAZs int) EnvConfig {
	envConfig := EnvConfig{
		Name: "randomized-multi-zone-test",
		// Since randomized, cannot reuse today.
		Reuse: false,
		Regions: Regions{
			"region": randomizedZones(rand.Intn, minNumNodes, maxNumNodes, minNumAZs, maxNumAZs),
		},
	}
	return envConfig
}

// EnvConfig specifies the desired name and starting geographic layout of a test
// environment. The test environment will manage one K8s cluster per geographic
// region.
type EnvConfig struct {
	// Name is used as the basis of the K8s cluster name(s) created for this
	// topology. If multiple tests use the same name, then they will share the
	// same K8s cluster(s).
	Name string

	// Reuse specifies whether the environment's K8s cluster(s) should be torn
	// down at the end of the test, or instead kept around for reuse in future
	// tests.
	Reuse bool

	// Regions maps each region name to the zone configuration for that region.
	Regions Regions

	// DefaultRegion specifies which of the regions contains the K8s cluster
	// returned by the DefaultCluster method. If empty, then Env will pick an
	// arbitrary default region.
	DefaultRegion string
}

// Regions maps from region name to the set of availability zones in that
// region.
type Regions map[string]Zones

// Zones maps from zone name to the number of K8s nodes in that zone.
type Zones map[string]int

func randomizedZones(
	randIntn func(int) int, minNumNodes, maxNumNodes, minNumAZs, maxNumAZs int,
) Zones {
	region := Zones{}

	nameZone := func(i int) string {
		return fmt.Sprintf("zone-%d", i)
	}

	wantNodes := randIntn(maxNumNodes-minNumNodes+1) + minNumNodes
	wantAZs := randIntn(maxNumAZs-minNumAZs+1) + minNumAZs

	// Init wantAZs zones.
	for i := range wantAZs {
		region[nameZone(i)] = 0
	}

	// Evenly distribute wantNodes k8s nodes throughout the zones inited above.
	left, i := wantNodes, 0
	for left > 0 {
		if _, ok := region[nameZone(i%wantAZs)]; !ok {
			log.Panicf("all zones should be inited in above loop: %v", region)
		}
		region[nameZone(i%wantAZs)]++
		i++
		left--
	}

	return region
}

package infrastructure

import (
	"context"
	"sort"

	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/constants"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type DistributionState struct {
	DesiredScale uint
	CurrentScale uint
}

// GetNumNodesPerZone uses a round-robin distribution style to
// determine the number of CRDB pods that should be deployed per zone.
func GetNumNodesPerZone(numCRDBPods int, numZones int) []int {
	sizes := make([]int, numZones)
	for i := range numZones {
		sizes[i] = numCRDBPods / numZones
		if i < numCRDBPods%numZones {
			sizes[i]++
		}
	}
	return sizes
}

// NodeDistribution computes the expected distribution of machines across a given set of availability zones
func NodeDistribution(zones []AvailabilityZone, scale uint) map[AvailabilityZone]int {
	ret := make(map[AvailabilityZone]int, len(zones))
	for i, numNodes := range GetNumNodesPerZone(int(scale), len(zones)) {
		ret[zones[i]] = numNodes
	}

	return ret
}

// BuildNodeDistribution computes the expected distribution of machines across a given set of availability zones
// taking into account the inability to move disks across zones.
func BuildNodeDistribution(
	ctx context.Context, vpg VMPoolGroup, scale uint,
) (map[AvailabilityZone]DistributionState, error) {
	zones, err := vpg.AvailabilityZones(ctx)
	if err != nil {
		return nil, err
	}

	zoneStatuses := map[AvailabilityZone]State{}
	for _, zone := range zones {
		state, err := vpg.State(ctx, zone)
		if err != nil {
			return nil, err
		}

		zoneStatuses[zone] = state
	}

	// Order zones largest to smallest and then alphabetically so distribution will always be even
	// and stable across invocations. We can't rely solely on alphabetically as this would cause the below
	// configuration to move the extra machine from C to A.
	// | A | B | C |
	// | 1 | 1 | 2 |
	// With a single statefulset distributed across any number of zones, "reshuffling" would
	// result in an unscheduable cluster as disks cannot be moved across zones.
	sort.Slice(zones, func(i, j int) bool {
		if zoneStatuses[zones[i]].DesiredNodes == zoneStatuses[zones[j]].DesiredNodes {
			return zones[i] < zones[j]
		}

		return zoneStatuses[zones[i]].DesiredNodes > zoneStatuses[zones[j]].DesiredNodes
	})

	distribution := make(map[AvailabilityZone]DistributionState, len(zones))

	for zone, desiredScale := range NodeDistribution(zones, scale) {
		distribution[zone] = DistributionState{
			DesiredScale: uint(desiredScale),
			CurrentScale: uint(zoneStatuses[zone].DesiredNodes),
		}
	}

	return distribution, nil
}

type VMPoolResizer struct {
	Namespace   string
	VMPoolGroup VMPoolGroup
	// The client for communication with kubernetes API server.
	ClientSet kubernetes.Interface
}

func (g *VMPoolResizer) UnoccupiedNodes(ctx context.Context) ([]*corev1.Node, error) {
	pods, err := g.ClientSet.CoreV1().Pods(g.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + manifests.CockroachDB,
	})
	if err != nil {
		return nil, err
	}

	nodes, err := g.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		// Manged instances don't return the master node in get nodes queries
		// however test instance do. Better safe than sorry.
		LabelSelector: "node-role.kubernetes.io/master != true",
	})
	if err != nil {
		return nil, err
	}

	// Filter out all but the nodes whose purpose is or was to host a CRDB pod.
	// TODO(logston): Repave all nodes with correct labels so that CRDB nodes
	// can be filtered in, rather than non-CRDB nodes filtered out.
	var crdbNodes []*corev1.Node = nil
	for i, node := range nodes.Items {
		isCrdbNode := true
		for labelKey, labelValue := range node.Labels {
			if labelKey == constants.CRLAppLabelKey && labelValue != constants.CRLAppLabelValue {
				// A non-CRDB node has been found. Skip it as we only want
				// mutate nodes in pool related to CRDB.
				isCrdbNode = false
				break
			}
		}

		if isCrdbNode {
			crdbNodes = append(crdbNodes, &nodes.Items[i])
		}
	}

	occupied := map[string]bool{}
	for _, pod := range pods.Items {
		occupied[pod.Spec.NodeName] = true
	}

	var unoccupied []*corev1.Node = nil
	for i, node := range crdbNodes {
		if !occupied[node.Name] {
			unoccupied = append(unoccupied, crdbNodes[i])
		}
	}

	return unoccupied, nil
}

func (g *VMPoolResizer) CurrentNodes(ctx context.Context) (uint, error) {
	zones, err := g.VMPoolGroup.AvailabilityZones(ctx)
	if err != nil {
		return 0, err
	}

	var currentNodes uint
	for _, zone := range zones {
		state, err := g.VMPoolGroup.State(ctx, zone)
		if err != nil {
			return 0, err
		}
		currentNodes += uint(state.DesiredNodes)
	}
	return currentNodes, nil
}

func (g *VMPoolResizer) Resize(
	ctx context.Context, replicas uint, defunctVMs ...*VM,
) error {
	// Remove all defunctVMs. This may be used as a way to specify which machines should be removed
	// or just to recycle VMs if replicas is not changing.
	for _, vm := range defunctVMs {
		logging.Infof(ctx, "removing VM %s in zone %s", vm.Name, vm.AvailabilityZone)
		if err := g.VMPoolGroup.RemoveNode(ctx, vm); err != nil {
			return err
		}
	}

	distribution, err := BuildNodeDistribution(ctx, g.VMPoolGroup, replicas)
	if err != nil {
		return err
	}

	for zone, scale := range distribution {
		ctx := logging.WithLogger(ctx, logging.L(ctx).With(zap.String("zone", string(zone))))

		logging.Infof(ctx, "have: %d  want: %d", scale.CurrentScale, scale.DesiredScale)
		if scale.CurrentScale == scale.DesiredScale {
			logging.Infof(ctx, "skipping zone %s because scale is unchanged", zone)
			continue
		}

		if err := g.VMPoolGroup.Resize(ctx, zone, int(scale.DesiredScale)); err != nil {
			return err
		}
	}

	return g.VMPoolGroup.WaitUntilReady(ctx)
}

package betaclusterctrl

import (
	"context"
	"fmt"
	"math"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/constants"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type nodeStatus struct {
	k8sNode     corev1.Node
	schedulable bool
}

// NodePool tracks free and used k8s nodes.
type NodePool struct {
	nodeStatuses []nodeStatus
	k8sToCrdb    map[string]string
	crdbToK8s    map[string]string
	zoneCounts   map[string]int
}

// GetPlacement determines the k8s node placement for the passed in list of
// crdb nodes. An empty node pool is returned if the explicit placement feature
// is not enabled.
func GetPlacement(
	ctx context.Context,
	ctl *kube.Ctl,
	cluster *crdbv1beta1.CrdbCluster,
	nodeList []crdbv1beta1.CrdbNode,
) (*NodePool, error) {
	if !cluster.Spec.FeatureEnabled(crdbv1beta1.ClusterExplicitNodePlacement) {
		return &NodePool{}, nil
	}

	var k8sNodeList corev1.NodeList
	if err := ctl.List(ctx, &kube.ListOptions{
		LabelSelector: kube.ParseLabelOrDie(fmt.Sprintf("%s=%s", constants.CRLAppLabelKey, constants.CRLAppLabelValue)),
	}, &k8sNodeList); err != nil {
		return nil, errors.Wrap(err, "listing k8s nodes")
	}

	nodeList, err := backfillNodeName(ctx, ctl, nodeList)
	if err != nil {
		return nil, errors.Wrap(err, "backfilling NodeName for already scheduled pods")
	}

	pool, unplaceable := placeNodes(k8sNodeList.Items, nodeList)

	if len(unplaceable) != 0 {
		logging.Errorf(ctx, "unable to place nodes %+v", unplaceable)
	}

	return pool, nil
}

// NewNodePool constructs a NodePool that is backed by the provided list of
// nodes.
func NewNodePool(nodeList []corev1.Node) *NodePool {
	k8sToCrdb := map[string]string{}
	zoneCounts := map[string]int{}
	nodes := make([]nodeStatus, 0, len(nodeList))
	for _, node := range nodeList {
		schedulable := !node.Spec.Unschedulable
		nodes = append(nodes, nodeStatus{
			k8sNode:     node,
			schedulable: schedulable,
		})
		k8sToCrdb[node.Name] = ""
		// Only seed zone counts for schedulable nodes. Cordoned nodes with
		// no existing pods would otherwise contribute a 0 count that skews
		// isZoneFull and blocks placement on zones that have capacity.
		if schedulable {
			zoneCounts[getZone(node)] = 0
		}
	}
	return &NodePool{
		nodeStatuses: nodes,
		k8sToCrdb:    k8sToCrdb,
		crdbToK8s:    map[string]string{},
		zoneCounts:   zoneCounts,
	}
}

// GetPlacement returns which k8s node the crdb node is supposed to be placed
// on.
func (n *NodePool) GetPlacement(crdbNode string) (string, bool) {
	node, found := n.crdbToK8s[crdbNode]
	return node, found
}

// GetFreeNode returns an available k8s node. It will only return nodes in
// zones that can be used without introducing skew into the number of crdb
// nodes per k8s zone.
func (n *NodePool) GetFreeNode() (string, bool) {
	for _, node := range n.nodeStatuses {
		if n.isAvailable(node.k8sNode.Name) && node.schedulable && !n.isZoneFull(getZone(node.k8sNode)) {
			return node.k8sNode.Name, true
		}
	}
	return "", false
}

// GetFreeNodeInZone returns an available k8s node in the zone. This does not
// care about zone skew, so it should only be used to schedule crdb pods that
// are already fixed to a specific zone.
func (n *NodePool) GetFreeNodeInZone(zoneName string) (string, bool) {
	for _, node := range n.nodeStatuses {
		if getZone(node.k8sNode) == zoneName && node.schedulable && n.isAvailable(node.k8sNode.Name) {
			return node.k8sNode.Name, true
		}
	}
	return "", false
}

// ClaimNode marks the k8s node as being used by the specific crdbNode.
func (n *NodePool) ClaimNode(k8sNodeName string, crdbNode string) bool {
	if !n.isAvailable(k8sNodeName) {
		return false
	}

	// Adjust zone counts
	node, _ := n.getNode(k8sNodeName)
	n.zoneCounts[getZone(node)] += 1

	// Record the assignment
	n.crdbToK8s[crdbNode] = k8sNodeName
	n.k8sToCrdb[k8sNodeName] = crdbNode

	return true
}

func (n *NodePool) getNode(k8sNode string) (corev1.Node, bool) {
	for _, node := range n.nodeStatuses {
		if node.k8sNode.Name == k8sNode {
			return node.k8sNode, true
		}
	}
	return corev1.Node{}, false
}

func (n *NodePool) isAvailable(k8sNode string) bool {
	crdbNode, exists := n.k8sToCrdb[k8sNode]
	return exists && crdbNode == ""
}

func (n *NodePool) isNodeSchedulable(k8sNode string) bool {
	for _, nodeStatus := range n.nodeStatuses {
		if nodeStatus.k8sNode.Name == k8sNode {
			return nodeStatus.schedulable
		}
	}
	return false
}

func (n *NodePool) isZoneFull(zoneName string) bool {
	minZone := math.MaxInt
	for _, zoneCount := range n.zoneCounts {
		if zoneCount < minZone {
			minZone = zoneCount
		}
	}
	return minZone < n.zoneCounts[zoneName]
}

func getZone(node corev1.Node) string {
	return node.Labels[corev1.LabelTopologyZone]
}

func backfillNodeName(
	ctx context.Context, ctl *kube.Ctl, nodeList []crdbv1beta1.CrdbNode,
) ([]crdbv1beta1.CrdbNode, error) {
	// TODO(jeffswenson): delete the backfill logic when the feature flag is
	// deleted.
	_ = crdbv1beta1.ClusterExplicitNodePlacement

	newList := make([]crdbv1beta1.CrdbNode, len(nodeList))
	for i, node := range nodeList {
		if node.Spec.NodeName == "" {
			var pod corev1.Pod
			err := ctl.Get(ctx, node.Name, &pod)
			if err != nil && !kubeerrors.IsNotFound(err) {
				return nil, errors.Wrapf(err, "fetching pod for node '%s'", node.Name)
			}
			node.Spec.NodeName = pod.Spec.NodeName
		}
		newList[i] = node
	}

	return newList, nil
}

// placeNodes is the core node placement algorithm for the operator.
//
// There are four constraints on crdb node placement:
//  1. Each k8s node has at most one crdb node placed on it.
//  2. Crdb nodes may not move between zones. Once a crdb node is scheduled, a
//     disk is created in its birth zone and the disk can't be moved between zones.
//  3. Crdb nodes should be moved as infrequently as possible. Each time a node is moved,
//     it needs to restart, which has some impact on the customer cluster.
//  4. The number of crdb nodes in each zone should be balanced in order to
//     minimize the amount of capacity lost when a zone fails.
//
// A greedy algorithm is used to assign crdb nodes to k8s nodes. It places crdb
// nodes with the most specific constraints first. The exact order is:
//
//  1. Crdb nodes with k8s node assignments keep their assignments.
//  2. Crdb nodes with a known zone (i.e. an assigned persistent volume) are placed.
//  3. Crdb nodes with no zone are placed. A zoneless CRDB node does not yet
//     have a disk, so the node it is placed on will determine the zone the disk is
//     created in. New crdb nodes are placed in the k8s zone with the fewest crdb
//     nodes.
func placeNodes(
	k8sNodes []corev1.Node, crdbNodes []crdbv1beta1.CrdbNode,
) (pool *NodePool, unschedulable []string) {
	pool = NewNodePool(k8sNodes)

	// Update the NodePool placement state for every crdb node with a valid
	// placement. For CrdbNodes that explicitly don't have running pods,
	// avoid placing them on unschedulable nodes to allow rescheduling.
	for _, crdbNode := range crdbNodes {
		if crdbNode.Spec.NodeName != "" {
			// Check if this CrdbNode explicitly doesn't have a running pod
			hasNoRunningPod := crdbNode.Status.HasCondition(crdbv1beta1.PodRunning, metav1.ConditionFalse)

			if hasNoRunningPod && !pool.isNodeSchedulable(crdbNode.Spec.NodeName) {
				// If the assigned node is unschedulable, don't claim it so
				// the CrdbNode can be reassigned to a schedulable node
				continue
			}
			pool.ClaimNode(crdbNode.Spec.NodeName, crdbNode.Name)
		}
	}

	// Place nodes that have an assigned locality.
	for _, crdbNode := range crdbNodes {
		if _, ok := pool.GetPlacement(crdbNode.Name); ok {
			// Skip nodes that are already placed
			continue
		}
		topologyValue := crdbNode.Status.GetTopologyValue()
		if topologyValue == "" {
			// Skip nodes that do not have an assigned locality. We need to
			// place the nodes with the most specific requirements first.
			continue
		}
		k8sNode, ok := pool.GetFreeNodeInZone(topologyValue)
		if ok {
			pool.ClaimNode(k8sNode, crdbNode.Name)
		}
	}

	// Place nodes with no assigned locality.
	for _, crdbNode := range crdbNodes {
		if _, ok := pool.GetPlacement(crdbNode.Name); ok {
			// Skip nodes that are already placed
			continue
		}
		if 0 < len(crdbNode.Status.TopologyValues) {
			// This node has locality constraints even if its first value is temporarily
			// empty. If it is still unplaced, the value is missing or its zone is full,
			// so placing it as zoneless could move it to a different zone.
			continue
		}
		k8sNode, ok := pool.GetFreeNode()
		if ok {
			pool.ClaimNode(k8sNode, crdbNode.Name)
		}
	}

	// Check to see if any crdb nodes are unschedulable.
	for _, crdbNode := range crdbNodes {
		if _, ok := pool.GetPlacement(crdbNode.Name); !ok {
			unschedulable = append(unschedulable, crdbNode.Name)
		}
	}

	return pool, unschedulable
}

package betaclusterctrl

import (
	"container/heap"
	"context"
	"math"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Scale reconciles the total number of CrdbNodes.
//   - If new nodes need to be provisioned, they will all be created within
//     this reconciliation iteration.
//   - If existing nodes need to be removed, they will be decommissioned up to
//     the max concurrent decommission limit per reconciliation iteration.
func (r *Reconciler) Scale(
	ctx context.Context,
	cluster *crdbv1beta1.CrdbCluster,
	nodes crdbv1beta1.CrdbNodeList,
	nodePool *NodePool,
) (didChange bool, requeueAfter time.Duration, err error) {
	if cluster.Spec.Mode != nil && *cluster.Spec.Mode == crdbv1beta1.Disabled {
		logging.Warnf(ctx, "Reconciliation mode is disabled; not scaling. CrdbCluster: %s", cluster.Name)
		return false, 0, nil
	}

	scaler := scaler{
		cluster: cluster,
		ctl:     r.Ctl.WithNamespace(cluster.Namespace),
		nodes:   nodes,
	}

	// Determine the number of "active" nodes. Active in this case means nodes in the
	// list that are not in the process of decommissioning and not explicitly excluded.
	thisRegion := cluster.Region()
	if thisRegion == nil {
		return false, 0, errors.Newf("cluster %s has no region matching Status.Region %q in Spec.Regions; "+
			"check that CLOUD_REGION or node topology labels are configured correctly", cluster.Name, cluster.Status.Region)
	}
	desiredNodes := int(thisRegion.Nodes)
	activeNodes := []crdbv1beta1.CrdbNode{}
	decommissioningNodes := []crdbv1beta1.CrdbNode{}
	for i := range nodes.Items {
		// If the node is already being deleted, don't count it against
		// max concurrent decommissioning and don't treat it as active.
		// Otherwise a stuck terminating node can block further scale-down.
		if !nodes.Items[i].DeletionTimestamp.IsZero() {
			continue
		}
		// Check if crdbnode is not decommissioning and not explicitly excluded via the annotation
		// "crdb.cockroachlabs.com/excluded" set to "true".
		if !nodes.Items[i].IsDecommissioning() && !nodes.Items[i].IsExcluded() {
			activeNodes = append(activeNodes, nodes.Items[i])
		} else if nodes.Items[i].IsDecommissioning() {
			decommissioningNodes = append(decommissioningNodes, nodes.Items[i])
		}
	}

	if desiredNodes == len(activeNodes) {
		return false, 0, nil
	}

	if desiredNodes > len(activeNodes) {
		logging.Infof(ctx, "Scaling up to %d from %d nodes", desiredNodes, len(activeNodes))
		return true, 0, scaler.scaleUp(ctx, desiredNodes-len(activeNodes), nodePool)
	}

	maxDecomLimit := getMaxConcurrentDecommissioning(cluster)
	decomLimit := max(0, maxDecomLimit-len(decommissioningNodes))
	toRemove := min(len(activeNodes)-desiredNodes, decomLimit)

	if toRemove == 0 {
		// We're not at our desired scale yet, but we've reached the max number of decommissioning nodes.
		// Requeue and check again later.
		return true, 0, nil
	}

	// Our intention is to have the best of both worlds if the cluster has under-replicated ranges i.e;
	//  1) To not block the rest of the reconcile ops (PVC resize, owner refs, cluster settings, rolling update) and
	//  2) Block the scale down op (to hold off on decommission).

	// So we return didChange=false with a requeueAfter so the reconciler doesn't
	// short-circuit and let the reconciliation run and still requeue after a delay
	// to check for under-replicated ranges again (in the next reconcile loop).
	if blocked := r.underReplicatedBlocks(ctx, cluster, nodes); blocked {
		return false, rollingRestartDelay(cluster), nil
	}

	logging.Infof(
		ctx, "Scaling down from %d to %d nodes. This iteration will remove %d nodes",
		len(activeNodes), desiredNodes, toRemove)
	return true, 0, scaler.scaleDown(ctx, activeNodes, toRemove)
}

type scaler struct {
	cluster *crdbv1beta1.CrdbCluster
	ctl     *kube.Ctl
	nodes   crdbv1beta1.CrdbNodeList
}

func (s *scaler) scaleUp(ctx context.Context, n int, nodePool *NodePool) error {
	for range n {
		if err := s.createNode(ctx, nodePool); err != nil {
			return err
		}
	}

	return nil
}

func (s *scaler) createNode(ctx context.Context, nodePool *NodePool) error {
	k8sNodeName, ok := nodePool.GetFreeNode()

	// When explicit node placement is enabled, we must have an
	// available k8s node before creating a CrdbNode. This prevents
	// creating CrdbNodes with empty spec.nodeName, which can lead to
	// race conditions where pods get scheduled by k8s before the
	// operator can assign a placement.
	if s.cluster.Spec.FeatureEnabled(crdbv1beta1.ClusterExplicitNodePlacement) && !ok {
		return errors.New("no available k8s node for new CrdbNode; waiting for infrastructure")
	}

	node := manifests.BetaCrdbClusterNode(s.cluster, s.cluster.Region(), k8sNodeName)

	// When the CrdbNodes are created by the clusterctrl, add in a finalizer
	// for performing cleaning up. This allows the finalizer to easily be
	// removed for manual operations.
	controllerutil.AddFinalizer(node, manifests.CrdbNodeFinalizerName)

	if err := ctrl.SetControllerReference(s.cluster, node, s.ctl.Scheme()); err != nil {
		return errors.Wrap(err, "failed to set controller reference")
	}

	if err := s.ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, node); err != nil {
		return errors.Wrap(err, "creating crdbnode")
	}

	nodePool.ClaimNode(k8sNodeName, node.Name)

	return nil
}

func (s *scaler) scaleDown(
	ctx context.Context, activeNodes []crdbv1beta1.CrdbNode, toRemove int,
) error {
	var nonRunningNodes []crdbv1beta1.CrdbNode
	var markedDecommissioningNodes []crdbv1beta1.CrdbNode
	var healthyNodes []crdbv1beta1.CrdbNode

	/*
			Note: The activeNodes will never include nodes that are marked for exclusion or already decommissioning.

			1.  Avoiding pending pods entirely from the scale down process can result in running nodes being scaled down.
				This would cause the pending pods to be rescheduled on available k8s nodes, leading to significant rebalancing.

			2. 	Allowing pending pods to be included in the decommissioning heap algorithm could lead to a mix of running
				and pending pods being selected for scale-down. As a pending pod cannot be decommissioned (no NodeID available), the operation
				would be stuck indefinitely.

			 To prevent these issues, this approach prioritizes non-running nodes (pending, failed, terminating and unknown state pods)
		     and removes them from being processed from the heap algorithm.
			 It skips the decommissioning operation for these pods and directly marks them with pending decommission annotation,
			 allowing the cluster controller to clean them up.
	*/

	// Excluding pending, decommissioning, and updating nodes
	// from the topology heap algorithm.
	for _, node := range activeNodes {
		// Skip nodes that are currently updating - they temporarily have PodRunning=False
		// but will become running again, so they shouldn't be treated as pending nodes
		if node.Status.HasCondition(crdbv1beta1.PodNeedsUpdate, metav1.ConditionTrue) {
			continue
		}

		if node.Status.HasCondition(crdbv1beta1.PodRunning, metav1.ConditionFalse) {
			nonRunningNodes = append(nonRunningNodes, node)
			continue
		}
		if node.IsMarkedForDecommission() {
			markedDecommissioningNodes = append(markedDecommissioningNodes, node)
			continue
		}
		healthyNodes = append(healthyNodes, node)
	}

	// First, handle any nodes that are already marked for decommissioning.
	for i := range markedDecommissioningNodes {
		if toRemove == 0 {
			break
		}

		node := &markedDecommissioningNodes[i]
		if node.Status.NodeID == nil {
			return errors.Newf("node %s has no node id", node.Name)
		}

		markedDecommissioningNodes[i].Decommission(*node.Status.NodeID)
		if err := s.ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, node); err != nil {
			return errors.Wrapf(err, "starting decommissioning process: %s", node.Name)
		}

		toRemove -= 1
	}

	// Next, handle any non-running nodes.
	// These nodes cannot be decommissioned (no NodeID available), so we
	// mark them with pending decommissioned annotation.
	// In the next reconciliation loop, we verify if the node is still in a non-running state,
	// and proceed accordingly for cleanup.
	for i := range nonRunningNodes {
		if toRemove == 0 {
			break
		}
		node := &nonRunningNodes[i]
		node.MarkForPendingDecommission()
		if err := s.ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, node); err != nil {
			return errors.Wrapf(err, "marking pending node for decommission: %s", node.Name)
		}
		toRemove -= 1
	}

	// Finally, if we still need to decommission more nodes, use the
	// topology-aware heap algorithm to select nodes from the healthy nodes.
	// getDecomIndexes returns indices into healthyNodes, not activeNodes.
	for _, i := range getDecomIndexes(healthyNodes, toRemove) {
		node := &healthyNodes[i]
		if node.Status.NodeID == nil {
			return errors.Newf("node %s has no node id", node.Name)
		}

		healthyNodes[i].Decommission(*node.Status.NodeID)
		if err := s.ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, node); err != nil {
			return errors.Wrapf(err, "starting decommissioning process: %s", healthyNodes[i].Name)
		}
	}
	return nil
}

func getMaxConcurrentDecommissioning(cluster *crdbv1beta1.CrdbCluster) int {
	if cluster.Spec.FeatureEnabled(crdbv1beta1.ClusterDrainAfterDecommission) {
		return math.MaxInt
	}
	return 1
}

func getDecomIndexes(activeNodes []crdbv1beta1.CrdbNode, n int) []int {
	if n == 0 || len(activeNodes) == 0 {
		return nil
	}
	// Clamp n to the number of available nodes to avoid popping from an
	// empty heap if filtering left fewer healthy nodes than expected.
	n = min(n, len(activeNodes))

	topoMap := make(map[string][]int)
	for i := range activeNodes {
		// We currently use only the first topology value for each node. If topology
		// spread constraints are not provided or none of their keys exist on the K8s
		// Node, the key will be an empty string. This groups all nodes without a
		// topology value together, and the heap picks one from that group.
		// TODO(nishanth): Modify the heap implementation to consider all topology
		// values if decommissioning needs to balance across multiple dimensions.
		key := activeNodes[i].Status.GetTopologyValue()
		topoMap[key] = append(topoMap[key], i)
	}

	// We use a max-heap to store the topologies, where the largest topology
	// is on the top. For every iteration, we mark the last node of the top of
	// the heap to be decommissioned. This way, we keep the sizes of
	// each zone in (better) balance.
	var decomIndexes []int
	th := &topologyHeap{}
	for tk := range topoMap {
		th.Push(&topology{value: tk, indices: topoMap[tk]})
	}
	heap.Init(th)
	left := n
	for range left {
		t := heap.Pop(th).(*topology)
		tLen := len(t.indices)
		if tLen > 0 {
			last := tLen - 1
			index := t.indices[last]
			decomIndexes = append(decomIndexes, index)
			t.indices = t.indices[:last]
		}

		// Push it back if there are still nodes left in this topology.
		if len(t.indices) > 0 {
			heap.Push(th, t)
		}
	}
	return decomIndexes
}

type topology struct {
	value   string
	indices []int
}

type topologyHeap []*topology

// Len implements heap.Interface.
func (th topologyHeap) Len() int {
	return len(th)
}

// Less implements heap.Interface.
func (th topologyHeap) Less(i, j int) bool {
	li, lj := len(th[i].indices), len(th[j].indices)
	if li == lj {
		// If two topologies are equal in size,
		// we put the lexicographically larger topology
		// on top.
		return strings.Compare(th[i].value, th[j].value) > 0
	}
	// We want larger topologies to be on the top of the heap.
	return li > lj
}

// Swap implements heap.Interface.
func (th topologyHeap) Swap(i, j int) {
	th[i], th[j] = th[j], th[i]
}

// Push implements heap.Interface.
func (th *topologyHeap) Push(x any) {
	// Push and Pop use pointer receivers because they modify the slice's length,
	// not just its contents.
	*th = append(*th, x.(*topology))
}

// Pop implements heap.Interface.
func (th *topologyHeap) Pop() any {
	old := *th
	n := len(old)
	item := old[n-1]
	old[n-1] = nil // avoid memory leak
	*th = old[0 : n-1]
	return item
}

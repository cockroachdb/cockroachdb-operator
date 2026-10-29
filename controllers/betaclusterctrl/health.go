package betaclusterctrl

import (
	"context"
	"fmt"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// defaultRollingRestartDelay matches the webhook default for
// spec.rollingRestartDelay. It's a fallback for objects that haven't been
// defaulted, like those built in tests.
const defaultRollingRestartDelay = time.Minute

// underReplicatedBlocks reports whether a disruptive operation (rolling restart
// or scale-down) should be held off because the cluster has under-replicated
// ranges. Restarting another node while ranges are already under-replicated can
// push them to unavailable.
//
//   - Bypass: setting ClusterSkipUnderReplicatedRangesCheck on the CR skips the
//     check entirely, so a force upgrade can proceed on a cluster that's stuck.
//
//   - Single-node clusters: the check is skipped because the default replication
//     factor cannot be satisfied and every disruption necessarily makes ranges
//     unavailable.
//
//   - Uninitialized clusters: the check is skipped because cluster-wide range
//     status is not available until CockroachDB has been initialized. This also
//     allows updates that repair a cluster which failed during bootstrap.
//
//   - No ready pod: the check is skipped without blocking so the requested
//     operation can continue when there is no safe pod to query.
//
//   - Fail-closed: a check failure blocks the operation and sets CheckError on
//     the condition. The cluster is most at risk when the check is most likely
//     to fail, so proceeding on error would remove protection when it matters
//     most. The skip flag is the escape hatch.
//
//   - Scope: the check returns a cluster-wide count (all stores, all regions).
//     Ranges can span regions, so a restart here can still affect cross-region
//     under-replicated ranges.
func (r *Reconciler) underReplicatedBlocks(
	ctx context.Context, cluster *crdbv1beta1.CrdbCluster, nodes crdbv1beta1.CrdbNodeList,
) bool {
	if cluster.Spec.FeatureEnabled(crdbv1beta1.ClusterSkipUnderReplicatedRangesCheck) {
		// Record that the check was bypassed so the condition doesn't sit stale at a
		// value from before the flag was set. Once the flag is removed, the next
		// disruptive operation runs the real query and overwrites this.
		cluster.Status.SetConditionWithMessage(
			crdbv1beta1.ClusterRangesUnderReplicated, metav1.ConditionFalse, "CheckSkipped",
			"under-replicated ranges check skipped via the skip-under-replicated-ranges-check feature flag",
		)
		return false
	}

	if cluster.Spec.Nodes() == 1 {
		cluster.Status.SetConditionWithMessage(
			crdbv1beta1.ClusterRangesUnderReplicated, metav1.ConditionFalse, "CheckSkipped",
			"under-replicated ranges check skipped for single-node cluster",
		)
		return false
	}

	if !cluster.IsInitialized() {
		return false
	}

	// The under-replicated count is cluster-wide, so we can query any ready
	// pod. If none are ready, skip the check without blocking the requested
	// operation so reconciliation can continue.
	readyNodes := nodes.WithCondition(crdbv1beta1.PodReady, metav1.ConditionTrue)
	if len(readyNodes) == 0 {
		return false
	}
	node := readyNodes[0]

	ctl := r.Ctl.WithNamespace(cluster.Namespace)
	expiringCtx, cancelFunc := context.WithTimeout(ctx, 30*time.Second)
	defer cancelFunc()
	count, err := r.NodeExecutor(node, ctl).UnderReplicatedRanges(expiringCtx)
	if err != nil {
		cluster.Status.SetConditionWithMessage(
			crdbv1beta1.ClusterRangesUnderReplicated, metav1.ConditionTrue, "CheckError",
			fmt.Sprintf("under-replicated ranges check failed: %v", err),
		)
		logging.Errorf(ctx, "under-replicated ranges check failed for CrdbCluster/%s, blocking disruptive operations until the check succeeds. Set the skip-under-replicated-ranges-check feature flag to override: %v", cluster.Name, err)
		return true
	}

	// ClusterRangesUnderReplicated is True while ranges are under-replicated and
	// False once they clear.
	if count > 0 {
		cluster.Status.SetConditionWithMessage(
			crdbv1beta1.ClusterRangesUnderReplicated, metav1.ConditionTrue, "UnderReplicated",
			fmt.Sprintf("%d range(s) are under-replicated", count),
		)
		logging.Warnf(ctx, "CrdbCluster/%s has %d under-replicated range(s); holding off disruptive operations. Set the skip-under-replicated-ranges-check feature flag to override.", cluster.Name, count)
		return true
	}

	cluster.Status.SetConditionWithMessage(
		crdbv1beta1.ClusterRangesUnderReplicated, metav1.ConditionFalse, "AllReplicated",
		"no under-replicated ranges",
	)
	return false
}

// nodeNeedsRoll reports whether a node still has to be rolled to the current
// revision. A node is up to date when it carries the current revision and sits on
// its assigned k8s node. RollingUpdate's rolling loop uses this to decide which
// node to update next, and rollPending uses it to gate the under-replicated query,
// so both stay in sync from one definition.
func nodeNeedsRoll(
	cluster *crdbv1beta1.CrdbCluster, node *crdbv1beta1.CrdbNode, nodePool *NodePool,
) bool {
	revision := node.Annotations[manifests.BetaCrdbClusterNodeRevisionAnnotationKey]
	k8sNodeName, _ := nodePool.GetPlacement(node.Name)
	return revision != cluster.Status.CurrentRevision || node.Spec.NodeName != k8sNodeName
}

// rollPending reports whether RollingUpdate still has a node to roll, so we only
// run the under-replicated query when a new eviction is actually imminent. We
// intentionally exclude PodNeedsUpdate here because a node mid-update is already
// being handled by the PodNeedsUpdate wait further down in RollingUpdate, which
// returns RequeueOnDependent without starting a new eviction.
func rollPending(
	cluster *crdbv1beta1.CrdbCluster, nodes crdbv1beta1.CrdbNodeList, nodePool *NodePool,
) bool {
	for i := range nodes.Items {
		if nodeNeedsRoll(cluster, &nodes.Items[i], nodePool) {
			return true
		}
	}
	return false
}

// rollingRestartDelay is the delay we requeue the under-replicated gate on, so it
// re-checks at the same pace the rolling restart would evict pods. We fall back
// to the default when the field is unset or non-positive. A zero value is a valid
// eviction-pacing setting (evict with no delay), but using it as a requeue delay
// would mean RequeueAfter(0), which never requeues and stalls the gate until an
// unrelated watch event.
func rollingRestartDelay(cluster *crdbv1beta1.CrdbCluster) time.Duration {
	if cluster.Spec.RollingRestartDelay != nil && cluster.Spec.RollingRestartDelay.Duration > 0 {
		return cluster.Spec.RollingRestartDelay.Duration
	}
	return defaultRollingRestartDelay
}

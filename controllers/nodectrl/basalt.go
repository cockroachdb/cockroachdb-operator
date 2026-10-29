package nodectrl

import (
	"context"
	"slices"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TODO(prod-orch): add an integration test for the basalt node decommission and
// cleanup flow. It should cover the store-id snapshot to status, the
// pod-delete-before-cleanup ordering, kill-once, and finalizer gating on
// cleanup. Tracked in CC-36680.

// snapshotBasaltStoreIDs records the CRDB store IDs for a Basalt-backed node
// while it still serves SQL. cleanupBasaltStores later uses them to remove the
// range-shared s<store-id> directories that live outside the node directory. By
// cleanup time, the node has been decommissioned and is no longer present in
// crdb_internal.kv_store_status, so the mapping cannot be recovered then.
//
// ensurePod calls this only when it fetches NodeID after a readiness transition.
// Ordinary reconciles therefore do not repeat the SQL query. A Pod that becomes
// unready clears NodeID, so the next ready transition fetches both values again.
// Existing IDs are merged with the result because a degraded multi-store node
// may return only a subset. Any ID never observed requires manual or sweeper
// cleanup.
func snapshotBasaltStoreIDs(
	ctx context.Context,
	node *crdbv1beta1.CrdbNode,
	exec kubecrdbnode.NodeExecutor,
	nodeID int64,
) error {
	if !node.Spec.DataStore.IsBasalt() {
		return nil
	}
	storeIDs, err := exec.StoreIDs(ctx, nodeID)
	if err != nil {
		return errors.Wrap(err, "querying store ids")
	}
	storeIDSet := make(map[int64]struct{}, len(node.Status.StoreIDs)+len(storeIDs))
	for _, id := range node.Status.StoreIDs {
		storeIDSet[id] = struct{}{}
	}
	for _, id := range storeIDs {
		storeIDSet[id] = struct{}{}
	}
	storeIDs = storeIDs[:0]
	for id := range storeIDSet {
		storeIDs = append(storeIDs, id)
	}
	slices.Sort(storeIDs)

	node.Status.StoreIDs = storeIDs
	expectedStoreCount := node.Spec.DataStore.Basalt.GetStoreCount()
	if int32(len(storeIDs)) < expectedStoreCount {
		logging.Warnf(ctx, "Snapshotted %d of %d expected basalt store ids %v for CrdbNode %s/%s. Partial results are accepted for NodeID %d, and missing directories may require manual or sweeper reclamation",
			len(storeIDs), expectedStoreCount, storeIDs, node.Namespace, node.Name, nodeID)
	} else {
		logging.Infof(ctx, "Snapshotted all %d basalt store ids %v for CrdbNode %s/%s",
			expectedStoreCount, storeIDs, node.Namespace, node.Name)
	}
	return nil
}

// cleanupBasaltStores drives directory cleanup for a Basalt-backed node being
// deleted. It runs a one-shot Job that recursively removes the node's store-local
// subtree and each range-shared store directory recorded in status. Reconciliation
// reads Job status on every pass, so the operation survives operator restarts.
//
// The caller invokes this only after the CockroachDB Pod is gone. The CrdbNode
// finalizer waits for cleanup so a later same-named Pod cannot inherit an
// orphaned directory.
func (r *Reconciler) cleanupBasaltStores(
	ctx context.Context, node *crdbv1beta1.CrdbNode,
) (cleaned bool, err error) {
	jobName := manifests.BasaltStoreCleanupJobName(node.Name)
	ctl := r.Ctl.WithNamespace(node.Namespace)

	var job batchv1.Job
	if getErr := ctl.Get(ctx, jobName, &job); getErr != nil {
		if !kubeerrors.IsNotFound(getErr) {
			return false, errors.Wrapf(getErr, "getting basalt cleanup job %s", jobName)
		}
		// The Job intentionally has no owner reference. Foreground node deletion
		// would otherwise remove it before its Pod can finish, and each reconcile
		// would create another Job. The node finalizer waits for the result, and the
		// Job TTL handles cleanup afterward.
		spec := node.Spec.DataStore.Basalt
		cleanupJob := manifests.BuildBasaltCleanupJob(node, spec, node.Labels, node.Annotations)
		if createErr := ctl.Create(ctx, cleanupJob); createErr != nil {
			return false, errors.Wrapf(createErr, "creating basalt cleanup job %s", jobName)
		}
		logging.Infof(ctx, "Created basalt store cleanup job %s for CrdbNode %s", jobName, node.Name)
		return false, nil
	}

	jobResourceUID := job.Annotations[manifests.BasaltLifecycleResourceUIDAnnotation]
	if jobResourceUID != string(node.UID) {
		// An ownerless Job can outlive its CrdbNode name. The UID prevents a stale
		// result from releasing the finalizer of a later same-named node.
		//
		// Foreground deletion leaves the Job visible with a deletion timestamp until
		// its Pod is garbage-collected. Requeue without re-issuing the delete during
		// that window so we don't make a redundant API call each pass.
		if !job.DeletionTimestamp.IsZero() {
			return false, nil
		}
		logging.Warnf(ctx, "deleting stale basalt store cleanup Job %s/%s for CrdbNode %s/%s: Job resource UID %q does not match current UID %q",
			job.Namespace, job.Name, node.Namespace, node.Name, jobResourceUID, node.UID)
		deletePolicy := metav1.DeletePropagationForeground
		if delErr := ctl.DeleteWithOptions(ctx, kube.DeleteOptions{
			GracePeriodSeconds: new(int64(0)),
			PropagationPolicy:  deletePolicy,
		}, &job); delErr != nil && !kubeerrors.IsNotFound(delErr) {
			return false, errors.Wrapf(delErr, "deleting stale basalt cleanup job %s", jobName)
		}
		return false, nil
	}

	switch {
	case job.Status.Succeeded > 0:
		// Leave the succeeded Job in place. Deleting it before the CrdbNode
		// finalizer update lands could make a concurrent reconcile see no Job and
		// create it again. TTLSecondsAfterFinished reclaims it afterward.
		logging.Infof(ctx, "Basalt store cleanup completed for CrdbNode %s", node.Name)
		return true, nil

	case manifests.BasaltJobFailed(&job):
		// Once the Job exhausts its retries, keeping the finalizer would block
		// scale-down and rolling updates indefinitely. Let deletion continue and
		// leave any remaining directories for manual or sweeper cleanup.
		logging.Errorf(ctx, "basalt store cleanup job %s for CrdbNode %s/%s and tenant %q failed after retries. Deletion will continue, but manual or sweeper reclamation is required. Store ids: %v. Pod logs: %s",
			jobName, node.Namespace, node.Name, node.Spec.DataStore.Basalt.TenantID,
			node.Status.StoreIDs, ctl.FetchJobPodLogs(ctx, &job))
		return true, nil

	default:
		return false, nil
	}
}

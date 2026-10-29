package nodectrl

import (
	"context"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/clusterutil"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/evictionutil"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// +kubebuilder:rbac:groups=crdb.cockroachlabs.com,resources=crdbnodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=crdb.cockroachlabs.com,resources=crdbnodes/status,verbs=get;update;patch

// Reconciler is an in development controller for the CrdbNode CRD. It is
// leveraged by the betaclusterctrl and controls an individual Pod running
// CockroachDB. CrdbNode is responsible for making requested changes to the
// underlying Pod. The CrdbNode is NOT responsible for any coordination
// decisions, sans using evictions to ensure PDB compliance when updating its
// Pod. For the original design, see docs/design/crdbnode-operator.md.
type Reconciler struct {
	Ctl            *kube.Ctl
	EvictionHelper *evictionutil.Helper

	// runningDrains tracks which nodes have the "cockroach drain" command
	// running.
	runningDrains inflight[string]

	// Exposed for testing purposes only.
	NodeExecutor func(node *crdbv1beta1.CrdbNode, exec kubecrdbnode.Executor) kubecrdbnode.NodeExecutor
}

var _ controllers.Reconciler = &Reconciler{}

const (
	retentionTimeStamp = "crdb.cockroachlabs.com/retention-timestamp"
	retentionReason    = "crdb.cockroachlabs.com/retention-reason"
	retentionPolicy    = "crdb.cockroachlabs.com/retention-policy"

	// lastRestartLoggedAnnotation is written on the pod after logging a crash restart.
	// Stores the FinishedAt timestamp of the last logged termination to deduplicate across reconciles.
	lastRestartLoggedAnnotation = "crdb.cockroachlabs.com/last-restart-logged"

	// lastEvictionLoggedAnnotation holds the UID of the last evicted pod whose eviction was
	// logged. It deduplicates across reconciles that fire while the pod is still in Evicted
	// state. Storing the UID (rather than the reused pod name) lets a replacement pod that is
	// evicted again be logged as a distinct event.
	lastEvictionLoggedAnnotation = "crdb.cockroachlabs.com/last-eviction-logged"
)

func NewReconciler(ctl *kube.Ctl, evictor *evictionutil.Helper) *Reconciler {
	return &Reconciler{
		Ctl:            ctl,
		EvictionHelper: evictor,
		NodeExecutor:   kubecrdbnode.NewNodeExecutor,
	}

}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctl := r.Ctl.WithNamespace(req.Namespace)

	// Get the current state of the node.
	var node crdbv1beta1.CrdbNode
	if err := ctl.Get(ctx, req.Name, &node); err != nil {
		// If resource cannot be found, it's been deleted, so don't requeue.
		if kubeerrors.IsNotFound(err) {
			return controllers.NoRequeue()
		}
		logging.Errorf(ctx, "failed to retrieve CrdbNode resource: %v", err)
		return controllers.RequeueIfError(err)
	}

	// If this CrdbNode is in the process of being deleted, don't perform any
	// additional reconciliation.
	if !node.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&node, manifests.CrdbNodeFinalizerName) {
			logging.Infof(ctx, "finalizer %q not present, skipping CrdbNode cleanup", manifests.CrdbNodeFinalizerName)
			return controllers.NoRequeue()
		}
		// NB: Currently, the crdbcluster controller will delete CrdbNode's
		// with Foreground deletion which ensures that Pod is deleted before
		// the CrdbNode is deleted.
		// It's unclear if this is desirable long term.
		return r.cleanupNodeResources(ctx, &node)
	}

	result, err := r.upsertPVC(ctx, &node)
	if err != nil || !result.IsZero() {
		if err != nil {
			logging.Errorf(ctx, "%s upsertPVC failed: %v", node.Name, err)
		}
		logging.Infof(ctx, "%s upsertPVC requeueing", node.Name)
		return result, err
	}

	pod, err := r.getOrCreatePod(ctx, &node)
	if err != nil {
		logging.Error(ctx, err.Error())
		return controllers.RequeueIfError(err)
	}

	if pod == nil {
		logging.Infof(ctx, "waiting for pod creation for CrdbNode %s", node.Name)
		return controllers.RequeueImmediately()
	}

	// Reconcile the Pod of this CrdbNode and update .Status with any
	// information from the Pod.
	result, err = r.ensurePod(ctx, &node, pod)
	if err != nil {
		logging.Errorf(ctx, "%s ensurePod failed: %v", node.Name, err)
		return result, err
	}

	// We are not decommissioning the node if it's marked for pending decommission
	// and transitions to running. This is the same behavior as cluster reconciler,
	// where we requeue by removing annotation. Here we handle only marked decommissioning nodes.
	if node.IsDecommissioning() {
		// Emit a gauge metric indicating that the node is decommissioning. The
		// metric will be reset to 0 when the operator deletes the node.
		nodeDecommissioningMetric(&node).Set(1)

		// Start/continue the decommissioning process only if this CrdbNode's Pod
		// is running. If it's not running, the Pod's Status changes will
		// eventually re-trigger reconciliation and ensure decommissioning runs.
		//
		// TODO: change the flow of control such that UpdateStatus runs in the
		// case that we requeue in this decommission block.
		if node.Status.HasCondition(crdbv1beta1.PodRunning, metav1.ConditionTrue) {
			if err := r.Decommission(ctx, &node); err != nil {
				// If we're still decommissioning, requeue after giving the node some
				// time to shed connections and replicas.
				if errors.Is(err, ErrNodeStillDecommissioning) || errors.Is(err, ErrNodeStillDraining) {
					return controllers.RequeueAfter(10*time.Second, nil)
				}

				logging.Errorf(ctx, "got error attempting to decommission node %s: %v", node.Name, err)
				return controllers.RequeueIfError(err)
			}
		}
	} else {
		// We need to explicitly set the decommissioning metric to 0, in case
		// the node was previously being decommissioned, but has now been
		// recommissioned.
		nodeDecommissioningMetric(&node).Set(0)
	}

	if result.IsZero() {
		result, err = r.monitorPVCFilesystemResize(ctx, &node)
		if err != nil {
			logging.Errorf(ctx, "%s monitorPVCFilesystemResize failed: %v", node.Name, err)
			return result, err
		}
	}

	// Setting ObservedGeneration indicates that a node has successfully
	// reconciled without error or requeue. This means the pod is up to date
	// and no filesystem resize is pending.
	//
	// If we advanced it earlier, the cluster controller would think this
	// CrdbNode is done and move on to update the next one. The eviction map
	// and PDB prevent simultaneous pod evictions, but we still want only one
	// CrdbNode spec update in flight at a time to catch bad specs early.
	if result.IsZero() {
		node.Status.ObservedGeneration = node.Generation
	}
	if err := ctl.UpdateStatus(ctx, &node); err != nil {
		logging.Errorf(ctx, "failed to update status for CrdbNode/%s: %v", node.Name, err)
		return controllers.RequeueIfError(err)
	}
	return result, nil
}

func (r *Reconciler) getOrCreatePod(
	ctx context.Context, node *crdbv1beta1.CrdbNode,
) (*corev1.Pod, error) {
	ctl := r.Ctl.WithNamespace(node.Namespace)
	pod := &corev1.Pod{}
	if err := ctl.Get(ctx, node.Name, pod); err != nil {
		if !kubeerrors.IsNotFound(err) {
			return nil, err
		}

		logging.Infof(ctx, "pod not found for CrdbNode %q, creating it", node.Name)

		// If the node has conditions, we need to clear them out. This tells the
		// clusterctrl that the node does not have a running pod.
		if len(node.Status.Conditions) > 0 {
			logging.Infof(ctx, "clearing conditions %v for CrdbNode %q", node.Status.Conditions, node.Name)
			node.Status.Conditions = FilterPodConditions(node.Status.Conditions)
			if err := ctl.UpdateStatus(ctx, node); err != nil {
				return nil, errors.Wrapf(err, "failed to update status for CrdbNode/%s: %v", node.Name, err)
			}
		}

		_, err := r.createPod(ctx, ctl, node)
		return nil, err
	}
	return pod, nil
}

// SetTestMode implements controllers.Reconciler.
func (r *Reconciler) SetTestMode(inTestMode bool) {
	// Noop
}

func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&crdbv1beta1.CrdbNode{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}

// cleanupNodeResources handles pod and PVC cleanup for a CrdbNode being deleted.
// It kills the cockroach process, deletes the pod, then either deletes or retains
// the PVCs depending on the node's PersistentVolumeClaimRetentionPolicy. Once
// both the pod and PVCs are fully handled, the finalizer on the CrdbNode is removed.
func (r *Reconciler) cleanupNodeResources(
	ctx context.Context, node *crdbv1beta1.CrdbNode,
) (reconcile.Result, error) {
	var pod corev1.Pod

	pod.Name = node.Name
	pod.Namespace = node.Namespace

	podDeleted := false
	pvcDeleted := false
	pvcRetained := false

	ctl := r.Ctl.WithNamespace(node.Namespace)

	err := ctl.Get(ctx, pod.Name, &pod)
	if err == nil {
		// The node has already drained and decommissioned, or its cluster is being
		// deleted. Kill CockroachDB before waiting for Pod deletion because
		// decommissioned nodes can hang during graceful termination. Kill is
		// idempotent when reconciliation races container teardown.
		// https://www.cockroachlabs.com/docs/stable/node-shutdown?filters=decommission#step-5-terminate-the-process-on-decommissioned-nodes
		// https://github.com/cockroachdb/cockroach/issues/111620
		exec := r.NodeExecutor(node, ctl)
		if err := exec.Kill(ctx); err != nil {
			return controllers.RequeueIfError(err)
		}

		if pod.DeletionTimestamp.IsZero() {
			// Begin Pod deletion after killing CockroachDB. The Pod must disappear
			// before the CrdbNode finalizer is removed. With background cascading
			// deletion, waiting for the CrdbNode to disappear first would deadlock:
			// the Pod waits for its owner, while the owner waits for the Pod.
			if err := r.Ctl.DeleteWithOptions(
				ctx,
				kube.DeleteOptions{NoWait: true, GracePeriodSeconds: new(int64(30))},
				&pod,
			); err != nil && !kubeerrors.IsNotFound(err) {
				return controllers.RequeueIfError(err)
			}
		} else {
			logging.Infof(ctx, "Waiting for Pod %s/%s deletion to finish", pod.Namespace, pod.Name)
		}
	} else if kubeerrors.IsNotFound(err) {
		podDeleted = true
	} else {
		return controllers.RequeueIfError(err)
	}
	if r.shouldRetainPVCs(node) {
		// Since Retention policy is Retain, annotate PVCs instead of deleting them.
		if err := r.addRetentionMetadata(ctx, node); err != nil {
			logging.Warnf(ctx, "Failed to add retention metadata for node %s: %v", node.Name, err)
		}
		pvcRetained = true
		logging.Infof(ctx, "Retaining PVCs for node %s due to retention policy", node.Name)
	} else {
		if err := r.deletePVC(ctx, node); err != nil {
			return controllers.RequeueIfError(err)
		}
		pvcDeleted = true
	}

	// PVC is not watched by the CrdbNode controller. Manual requeueing after a
	// set amount of time (10s) is necessary for reconciliation upon deletion or
	// retention of the PVC.
	if !pvcRetained && !pvcDeleted {
		return controllers.RequeueAfter(10*time.Second, nil)
	}

	// The CrdbNode controller watches pods, so the pod deletion event
	// will re-trigger reconciliation. Requeue as a safety net in case the watch
	// event arrives before this reconcile fully unwinds.
	if !podDeleted {
		return controllers.RequeueAfter(1*time.Second, nil)
	}

	// Basalt-backed nodes reclaim their store directories once the Pod is gone, so
	// the recursive removal runs against data nothing is writing to. The CrdbNode
	// finalizer waits on it, otherwise a same-named replacement could inherit an
	// orphaned directory.
	//
	// We skip the per-node cleanup Job since a terminating (DeletionTimestamp is set)
	// or missing owning cluster indicates cluster deletion. Basalt-backed nodes have
	// no PVC, so their cleanup is complete once CRDB stops and its owning Pod is
	// deleted. The control plane is responsible for removing the Basalt namespace
	// and tenant.
	if node.Spec.DataStore.IsBasalt() {
		clusterDeleting, clusterLookupErr := clusterutil.IsClusterForNodeBeingDeleted(ctx, ctl, node)
		if clusterLookupErr != nil {
			// Prefer duplicate idempotent cleanup over wedging this node on a lookup error.
			logging.Errorf(ctx, "failed to determine whether the owning cluster is being deleted for CrdbNode %s/%s. Proceeding with per-node basalt cleanup: %+v",
				node.Namespace, node.Name, clusterLookupErr)
		}
		if !clusterDeleting {
			cleaned, err := r.cleanupBasaltStores(ctx, node)
			if err != nil {
				return controllers.RequeueIfError(err)
			}
			if !cleaned {
				return controllers.RequeueAfter(10*time.Second, nil)
			}
		}
	}

	// This gauge could have been set during decommission.
	// Reset it now that the node is being deleted.
	nodeDecommissioningMetric(node).Set(0)

	// Pod and PVCs are both handled (deleted or retained). Remove the finalizer.
	controllerutil.RemoveFinalizer(node, manifests.CrdbNodeFinalizerName)

	// After migrating to v1beta1, some CrdbNodes can get stuck in Terminating
	// with only the Kubernetes-managed foregroundDeletion finalizer remaining, even
	// after the Pod and PVC are deleted. This appears to be a timing/GC issue where
	// the garbage collector doesn't promptly clear the finalizer despite no remaining
	// dependents. As a workaround, we explicitly strip foregroundDeletion here to
	// unblock deletion. This is safe because we've already verified Pod/PVC cleanup.
	if controllerutil.ContainsFinalizer(node, metav1.FinalizerDeleteDependents) {
		controllerutil.RemoveFinalizer(node, metav1.FinalizerDeleteDependents)
		logging.Infof(ctx, "removing foregroundDeletion finalizer from CrdbNode %s as a workaround for stuck deletion", node.Name)
	}

	return controllers.RequeueIfError(r.Ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, node))
}

// addRetentionMetadata adds retention metadata to the PVCs for the given CrdbNode.
func (r *Reconciler) addRetentionMetadata(ctx context.Context, node *crdbv1beta1.CrdbNode) error {
	ctl := r.Ctl.WithNamespace(node.Namespace)
	pvcNames := r.getPVCNamesForNode(node)
	for _, pvcName := range pvcNames {
		var pvc corev1.PersistentVolumeClaim
		if err := ctl.Get(ctx, pvcName, &pvc); err != nil {
			if kubeerrors.IsNotFound(err) {
				continue
			}
			return err
		}

		// Add retention metadata
		if pvc.Labels == nil {
			pvc.Labels = make(map[string]string)
		}
		if pvc.Annotations == nil {
			pvc.Annotations = make(map[string]string)
		}

		pvc.Annotations[retentionTimeStamp] = timeutil.Now().Format(time.RFC3339)
		pvc.Annotations[retentionReason] = "node-deletion"
		pvc.Labels[retentionPolicy] = string(node.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted)
		// Clear ownerReferences so K8s GC does not delete the retained PVC when
		// the CrdbNode is removed. The ownerRef is set for topology visibility
		// but must not survive past the point where we commit to retaining the PVC.
		pvc.OwnerReferences = nil

		if err := ctl.Update(ctx, &pvc); err != nil {
			return err
		}
	}

	return nil
}

// getPVCNamesForNode returns the names of the PVCs for the given CrdbNode.
func (r *Reconciler) getPVCNamesForNode(node *crdbv1beta1.CrdbNode) []string {
	var pvcNames []string

	// Basalt and a data VolumeClaimTemplate are mutually exclusive (enforced by
	// the webhook). Basalt nodes have no data PVC but may own a scratch PVC
	// (scratch-<node>); data nodes own the data PVC and, when enabled, a WAL PVC.
	if b := node.Spec.DataStore.Basalt; b != nil && b.ScratchVolumeClaimTemplate != nil {
		pvcNames = append(pvcNames, manifests.CrdbNodeScratchPVCName(node.Name, b.ScratchVolumeClaimTemplate))
	} else if vct := node.Spec.DataStore.VolumeClaimTemplate; vct != nil {
		pvcNames = append(pvcNames, manifests.CrdbNodeStoragePVCName(node.Name, vct))
		if node.Spec.WALFailoverSpec != nil {
			pvcNames = append(pvcNames, manifests.CrdbNodeWalStoragePVCName(node.Spec.WALFailoverSpec.Name, node.Name, vct))
		}
	}

	// A dedicated logs PVC applies to both datastore kinds.
	if node.Spec.LogsStore != nil && node.Spec.LogsStore.VolumeClaimTemplate != nil {
		pvcNames = append(pvcNames, manifests.CrdbNodeLogsStoragePVCName(node.Name, node.Spec.LogsStore.VolumeClaimTemplate))
	}

	return pvcNames
}

func nodeDecommissioningMetric(node *crdbv1beta1.CrdbNode) prometheus.Gauge {
	return metrics.NodeDecommissioning.With(prometheus.Labels{
		"node": node.Name,
	})
}

// FilterPodConditions removes Pod conditions from the given slice of conditions.
func FilterPodConditions(conditions []crdbv1beta1.NodeCondition) []crdbv1beta1.NodeCondition {
	var newConditions []crdbv1beta1.NodeCondition
	for _, c := range conditions {
		switch c.Type {
		case crdbv1beta1.PodRunning, crdbv1beta1.PodNeedsUpdate, crdbv1beta1.PodReady:
		default:
			newConditions = append(newConditions, c)
		}
	}
	return newConditions
}

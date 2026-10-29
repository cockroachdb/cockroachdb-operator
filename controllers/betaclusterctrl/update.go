package betaclusterctrl

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/version"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// validationRejectedImageAnnotation records the spec image rejected by version validation.
// The guard in RollingUpdate uses it to block re-validation until the user changes the image.
const validationRejectedImageAnnotation = "crdb.cockroachlabs.com/validation-rejected-image"

// RollingUpdate applies changes detected to nodes.
// It returns a positive requeue delay if we need to wait that long before
// calling this function again. The caller is responsible to call
// `RequeueAfter` with this return value. A non-nil error explaining the reason
// will also be returned in this case.
func (r *Reconciler) RollingUpdate(
	ctx context.Context,
	cluster *crdbv1beta1.CrdbCluster,
	nodes crdbv1beta1.CrdbNodeList,
	nodePool *NodePool,
) (ctrl.Result, error) {
	if !cluster.IsManagedMode() {
		logging.Warnf(
			ctx,
			"Reconciliation mode is %q; not updating existing nodes. CrdbCluster/%s",
			*cluster.Spec.Mode,
			cluster.Name,
		)

		r.EvictionHelper.AllowNextEvictionFor(cluster)

		// Clusters with a disabled reconciliation mode shouldn't perform rolling updates.
		return controllers.NoRequeue()
	}

	ctl := r.Ctl.WithNamespace(cluster.Namespace)

	// Block re-validation for an image that already failed upgrade path checks.
	// Without this guard, watch events drive an infinite create-job / fail / clean
	// up cycle. To unblock: change spec.image or remove the annotation manually.
	if cluster.Status.HasAction(crdbv1beta1.ValidateVersion, crdbv1beta1.Failed) &&
		cluster.Annotations[validationRejectedImageAnnotation] == cluster.Spec.Template.Spec.Image {
		logging.Warnf(ctx, "Version validation previously failed for image %s on cluster %s, rolling update blocked until image changes", cluster.Spec.Template.Spec.Image, cluster.Name)
		return controllers.NoRequeue()
	}

	needsValidation, currentVersion := requiresUpgradeValidation(cluster)
	if needsValidation {
		versionCheckerJob := r.VersionCheckerJob(ctx, ctl, fmt.Sprintf("%s-%s", versionCheckerJobNamePrefix, cluster.Name))
		validationResp := versionCheckerJob.validateClusterUpgrade(cluster, currentVersion)

		switch validationResp.Result {
		case ValidationPending:
			// Job is still running, set condition and requeue to check again later
			cluster.Status.SetAction(crdbv1beta1.ValidateVersion, crdbv1beta1.Starting)
			logging.Infof(ctx, "Version validation job is running for cluster %s, will requeue", cluster.Name)
			return controllers.RequeueImmediately()

		case ValidationFailed:
			logging.Errorf(ctx, "Version validation failed for cluster %s: %v", cluster.Name, validationResp.Error)
			cluster.Status.SetAction(crdbv1beta1.ValidateVersion, crdbv1beta1.Failed)
			// Persist status before touching the annotation. Both UpdateStatus and
			// the metadata patch below return the server object and reset in-memory
			// status, so the status write has to land first.
			if err := ctl.UpdateStatus(ctx, cluster); err != nil {
				logging.Warnf(ctx, "Failed to persist validation failed status for cluster %s: %v", cluster.Name, err)
			}
			base := cluster.DeepCopy()
			if cluster.Annotations == nil {
				cluster.Annotations = make(map[string]string)
			}
			cluster.Annotations[validationRejectedImageAnnotation] = cluster.Spec.Template.Spec.Image
			// Patch only metadata.annotations. A full object Update would make the
			// operator co-own the whole spec, so the CrdbCluster's server-side
			// applier would conflict on its next apply over fields like the pod
			// template containers.
			if err := ctl.Patch(ctx, cluster, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				logging.Warnf(ctx, "Failed to persist rejected image annotation for cluster %s: %v", cluster.Name, err)
			}
			return controllers.NoRequeue()

		case ValidationSucceeded:
			// Only mark the upgrade as starting once validation passes. Setting it
			// before validation means a failed validation writes Upgrade Starting to
			// the status permanently, which blocks any quiescence check from clearing.
			logging.Infof(ctx, "Version validation succeeded for cluster %s, detected version: %s", cluster.Name, validationResp.Version)
			cluster.Status.SetAction(crdbv1beta1.Upgrade, crdbv1beta1.Starting)
			cluster.Status.SetAction(crdbv1beta1.ValidateVersion, crdbv1beta1.Finished)
			// Clear the rejected-image annotation from a previous failure. Leaving it is not
			// a correctness problem (the guard requires ValidateVersion:Failed), but it's cleaner.
			// Status must be persisted first: both UpdateStatus and the metadata patch return the
			// server object and reset in-memory status, which would discard Upgrade:Starting and
			// ValidateVersion:Finished if the status write did not land first.
			if _, hadAnnotation := cluster.Annotations[validationRejectedImageAnnotation]; hadAnnotation {
				if err := ctl.UpdateStatus(ctx, cluster); err != nil {
					logging.Warnf(ctx, "Failed to persist validation succeeded status for cluster %s: %v", cluster.Name, err)
				}
				base := cluster.DeepCopy()
				delete(cluster.Annotations, validationRejectedImageAnnotation)
				// Patch only metadata.annotations so the operator does not take SSA
				// ownership of the externally-applied CrdbCluster spec.
				if err := ctl.Patch(ctx, cluster, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
					logging.Warnf(ctx, "Failed to clear rejected image annotation for cluster %s: %v", cluster.Name, err)
				}
			}
		}
	}

	// TODO: log CrdbCluster spec drift for fields outside the CrdbNode template
	// (regions, clusterSettings, disruptionBudget, etc.) by storing a "last
	// reconciled spec" annotation and diffing on generation change.
	cluster.Status.CurrentRevision = manifests.GetClusterRevision(cluster)

	// If there are any CrdbNodes that don't have a running pod, apply the
	// latest spec immediately. This happens before any readiness checks or
	// validation because we never expect a CrdbNode to not have a running pod.
	if err := r.applyLatestForUnscheduledPods(ctx, cluster, nodes, nodePool); err != nil {
		return controllers.RequeueIfError(err)
	}

	// If there is a node that is unready for unknown reasons, don't do
	// any reconciliation.
	ok := r.canProceedWithUpdate(ctx, cluster, nodes, nodePool)
	if !ok {
		return controllers.RequeueOnDependent()
	}

	// We've now vetted that we can safely allow another eviction to be
	// performed.
	r.EvictionHelper.AllowNextEvictionFor(cluster)

	// If any CrdbNodes are running on an unknown revision, update them to the
	// current revision immediately allowing the PDB and eviction helper to
	// perform any pacing.
	staleNode, err := r.applyKnownRevision(ctx, cluster, nodes, nodePool)
	if err != nil {
		return controllers.RequeueIfError(err)
	}

	if staleNode != nil {
		logging.Infof(
			ctx,
			"Waiting for known revision %s to propagate to node CrdbNode/%s",
			cluster.Status.CurrentRevision,
			staleNode.Name,
		)
		return controllers.RequeueOnDependent()
	}

	// Don't update a node that already has a pending update on a known revision.
	// Wait for the update to be applied by the nodectrl.
	for _, node := range nodes.Items {
		// Nodectrl clears PodNeedsUpdate for decommissioning nodes (see
		// ensurePod), but a stale True set before the decommission mark
		// can still be sitting on the CrdbNode. Skipping those here keeps
		// one stuck condition from blocking the whole cluster's rolling
		// update.
		if node.IsDecommissioning() || node.IsExcluded() {
			continue
		}
		if node.Status.HasCondition(crdbv1beta1.PodNeedsUpdate, metav1.ConditionTrue) {
			logging.Infof(ctx,
				"CrdbNode/%s's Pod needs to be updated. Waiting for Pod update before proceeding with revision %q",
				node.Name,
				cluster.Status.CurrentRevision,
			)
			return controllers.RequeueOnDependent()
		}
	}

	// Before rolling the next node, hold off if the cluster has under-replicated
	// ranges. Taking down another node now could push those ranges to unavailable.
	// This gate sits after applyKnownRevision and the PodNeedsUpdate wait so
	// unknown-revision recovery and in-flight updates proceed without being held
	// off. It only fires right before a new eviction would start. Single-node
	// clusters bypass the gate because no rolling update can preserve range
	// availability.
	if rollPending(cluster, nodes, nodePool) {
		if r.underReplicatedBlocks(ctx, cluster, nodes) {
			r.EvictionHelper.DisableEvictionsFor(cluster)
			return controllers.RequeueAfter(rollingRestartDelay(cluster), nil)
		}
	} else if cluster.Status.HasCondition(crdbv1beta1.ClusterRangesUnderReplicated, metav1.ConditionTrue) {
		// Nodes are done rolling, so refresh the condition. It's only written during
		// a roll, so it would otherwise sit stale in status. We ignore the result and
		// finish regardless. Blocking on under-replicated ranges would wedge the
		// upgrade, since they can persist for reasons the roll can't fix (a dead node,
		// or a replication factor above the node count).
		r.underReplicatedBlocks(ctx, cluster, nodes)
	}

	// Finally, update all nodes from the previous revision one at a time to
	// ensure that we don't rush a bad spec.
	for i := range nodes.Items {
		node := &nodes.Items[i]

		if !nodeNeedsRoll(cluster, node, nodePool) {
			continue
		}

		// nodeNeedsRoll already read these, but we re-read here for the log line and
		// the spec update below. They're cheap map lookups, not worth threading out.
		revision := node.Annotations[manifests.BetaCrdbClusterNodeRevisionAnnotationKey]
		k8sNodeName, _ := nodePool.GetPlacement(node.Name)

		existingPVCSize, err := r.getExistingPVCSize(ctx, node)
		if err != nil {
			return controllers.RequeueIfError(err)
		}

		logging.Infof(
			ctx,
			"updating CrdbNode/%s from previous revision %q to revision %q",
			node.Name,
			revision,
			cluster.Status.CurrentRevision,
		)
		r.updateBetaCrdbClusterNodeWithSpecDiff(ctx, cluster, node, existingPVCSize, k8sNodeName)

		if err := r.Ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, node); err != nil {
			return controllers.RequeueIfError(err)
		}

		return controllers.RequeueOnDependent()
	}

	logging.Infof(ctx, "all CrdbNodes up to date with revision %q", cluster.Status.CurrentRevision)
	cluster.Status.PreviousRevision = cluster.Status.CurrentRevision
	if cluster.Status.HasAction(crdbv1beta1.Upgrade, crdbv1beta1.Starting) {
		cluster.Status.SetAction(crdbv1beta1.Upgrade, crdbv1beta1.Finished)
		jobName := fmt.Sprintf("%s-%s", versionCheckerJobNamePrefix, cluster.Name)
		if err := r.VersionCheckerJob(ctx, ctl, jobName).cleanupVersionCheckerJob(); err != nil {
			logging.Errorf(ctx, "Failed to clean up version checker job %s: %v", jobName, err)
		}
	}

	return controllers.NoRequeue()
}

// If a CrdbNode does not have a corresponding pod, apply latest spec immediately.
// A CrdbNode without a pod usually indicates a scheduling issue so we want to
// rectify immediately.
func (r *Reconciler) applyLatestForUnscheduledPods(
	ctx context.Context,
	cluster *crdbv1beta1.CrdbCluster,
	nodes crdbv1beta1.CrdbNodeList,
	nodePool *NodePool,
) error {
	needsApply := []kube.Object{}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if !node.Status.HasCondition(crdbv1beta1.PodRunning, metav1.ConditionTrue) {
			k8sNodeName, _ := nodePool.GetPlacement(node.Name)
			existingPVCSize, err := r.getExistingPVCSize(ctx, node)
			if err != nil {
				return err
			}

			logging.Infof(ctx,
				"updating CrdbNode/%s spec immediately due to unscheduled pod.",
				node.Name,
			)
			r.updateBetaCrdbClusterNodeWithSpecDiff(ctx, cluster, node, existingPVCSize, k8sNodeName)
			needsApply = append(needsApply, node)
		}
	}

	if err := r.Ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, needsApply...); err != nil {
		return err
	}

	return nil
}

func (r *Reconciler) canProceedWithUpdate(
	ctx context.Context,
	cluster *crdbv1beta1.CrdbCluster,
	nodes crdbv1beta1.CrdbNodeList,
	nodePool *NodePool,
) bool {
	for i := range nodes.Items {
		node := &nodes.Items[i]

		// Wait for the node controller to finish reconciling before proceeding.
		// ObservedGeneration only advances when the node controller completes
		// without requeue (result.IsZero).
		//
		// If the node already has PodNeedsUpdate, the node controller has seen
		// the spec change but couldn't evict the pod, likely because the
		// in-memory eviction map is empty after an operator restart. Blocking
		// here would deadlock because AllowNextEvictionFor runs after this
		// check, and the node controller can't advance ObservedGeneration
		// without evicting first. An operator restart doesn't help either
		// because it wipes the eviction map again.
		if node.Status.ObservedGeneration < node.Generation &&
			!node.Status.HasCondition(crdbv1beta1.PodNeedsUpdate, metav1.ConditionTrue) {
			logging.Warnf(
				ctx,
				"CrdbNode/%s .ObservedGeneration (%d) is behind .Generation (%d). Waiting for reconciliation",
				node.Name,
				node.Status.ObservedGeneration,
				node.Generation,
			)
			return false
		}

		// Skip all CrdbNodes that are on an out of date revision. Any previous
		// revision could be unstable, "bad", or contain a bug that leads to
		// unreadiness. We don't want to let those cases prevent us from
		// rolling out a new update. Our PDB will prevent us from taking down
		// more than our given allowance of Pods.
		// In theory, CrdbNodes are on the previous revision could be
		// considered stable as the revision would have been completely rolled
		// out but there's always the chance of a bug that surfaced after the
		// rollout.
		revision := node.Annotations[manifests.BetaCrdbClusterNodeRevisionAnnotationKey]
		if revision != cluster.Status.CurrentRevision {
			continue
		}

		// If the k8s node of a pod has changed, that implies the old node
		// doesn't exist or is unschedulable.
		// TODO(jeffswenson): until the backfill is complete, this will also
		// skip the readiness check while the backfill is occuring.
		k8sNodeName, _ := nodePool.GetPlacement(node.Name)
		if node.Spec.NodeName != k8sNodeName {
			continue
		}

		// At this point we know that the CrdbNode and its .Status field are up
		// to date. If the nodectrl thinks the Pod needs to be updated, we are
		// not going to let the Pod's unreadiness prevent a rolling update from
		// occurring as there may be an issue with currently running Pod.
		// The combination of the PDB and eviction helper will ensure that any
		// updates are paced appropriately.
		if node.Status.HasCondition(crdbv1beta1.PodNeedsUpdate, metav1.ConditionTrue) {
			continue
		}

		// Finally, we have an up to date CrdbNode and Pod. If that Pod is
		// unready, we want to pause the rollout until it becomes ready. This
		// Pod's unreadiness may be indicative of a bad Spec. The one expection
		// to this is if the k8s node has changed. If the k8s node changed,
		// that implies the old node does not exist anymore, which is expected
		// to be unready.
		if !node.Status.HasCondition(crdbv1beta1.PodReady, metav1.ConditionTrue) {
			logging.Warnf(
				ctx,
				"CrdbNode/%s's Pod is not ready. Waiting for Pod readiness before proceeding with revision %q",
				node.Name,
				cluster.Status.CurrentRevision,
			)
			r.EvictionHelper.DisableEvictionsFor(cluster)
			return false
		}
	}

	return true
}

// Update all nodes on unknown revisions to the current revision immediately
// allowing the PDB and eviction helper to perform any pacing. We only track
// the current and previous revisions. All other revisions are "unknown" and
// could be unreliable.
func (r *Reconciler) applyKnownRevision(
	ctx context.Context,
	cluster *crdbv1beta1.CrdbCluster,
	nodes crdbv1beta1.CrdbNodeList,
	nodePool *NodePool,
) (*crdbv1beta1.CrdbNode, error) {
	var staleNode *crdbv1beta1.CrdbNode
	for i := range nodes.Items {
		node := &nodes.Items[i]
		revision := node.Annotations[manifests.BetaCrdbClusterNodeRevisionAnnotationKey]

		if revision == cluster.Status.CurrentRevision || revision == cluster.Status.PreviousRevision {
			continue
		}

		existingPVCSize, err := r.getExistingPVCSize(ctx, node)
		if err != nil {
			return nil, err
		}

		logging.Infof(
			ctx,
			"updating CrdbNode/%s from unknown revision %q to revision %q",
			node.Name,
			revision,
			cluster.Status.CurrentRevision,
		)

		k8sNodeName, _ := nodePool.GetPlacement(node.Name)

		r.updateBetaCrdbClusterNodeWithSpecDiff(ctx, cluster, node, existingPVCSize, k8sNodeName)

		if err := r.Ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, node); err != nil {
			return nil, err
		}

		staleNode = node
	}

	return staleNode, nil
}

func (r *Reconciler) updateBetaCrdbClusterNodeWithSpecDiff(
	ctx context.Context,
	cluster *crdbv1beta1.CrdbCluster,
	node *crdbv1beta1.CrdbNode,
	existingPVCSize *resource.Quantity,
	k8sNodeName string,
) {
	// If the node is already on the current cluster revision, skip the DeepCopy
	// and diff work since there's nothing new to log.
	previousRevision := node.Annotations[manifests.BetaCrdbClusterNodeRevisionAnnotationKey]
	if previousRevision == cluster.Status.CurrentRevision {
		manifests.UpdateBetaCrdbClusterNode(cluster, node, existingPVCSize, k8sNodeName)
		return
	}

	previousSpec := node.Spec.DeepCopy()
	manifests.UpdateBetaCrdbClusterNode(cluster, node, existingPVCSize, k8sNodeName)
	currentRevision := node.Annotations[manifests.BetaCrdbClusterNodeRevisionAnnotationKey]
	if previousRevision == currentRevision {
		return
	}

	if previousRevision == "" {
		logging.Infof(ctx, "CrdbNode/%s initial spec populated with revision %q", node.Name, currentRevision)
		return
	}

	logCrdbNodeSpecDiff(
		ctx,
		&r.Emit,
		node,
		previousRevision,
		currentRevision,
		previousSpec,
		&node.Spec,
	)
}

func logCrdbNodeSpecDiff(
	ctx context.Context,
	emit *kube.EventEmitter,
	node *crdbv1beta1.CrdbNode,
	previousRevision, currentRevision string,
	previousSpec, currentSpec *crdbv1beta1.CrdbNodeSpec,
) {
	changes := changedJSONPaths("spec", previousSpec, currentSpec)
	if len(changes) == 0 {
		return
	}

	summary := formatChanges(changes)
	logging.Infof(
		ctx,
		"CrdbNode/%s spec drift detected: previous revision %q, desired revision %q, changes: %s",
		node.Name,
		previousRevision,
		currentRevision,
		summary,
	)
	emit.Infof(
		node,
		"CrdbNodeSpecDrift",
		"Spec drift detected: previous revision %q, desired revision %q",
		previousRevision,
		currentRevision,
	)

	// Write the spec change summary to the CrdbNode annotation so the restart classifier
	// in ensurePod can read it when it evicts the pod. The caller's ApplyWithOptions persists
	// it, but persistence is not guaranteed until then. A later re-apply of the same node
	// without a fresh spec diff drops the annotation from the desired object, and the 3-way
	// merge patch then deletes it from the server. If that happens before ensurePod consumes
	// the annotation, the RollingUpdate event for this spec change is not logged.
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}
	node.Annotations[manifests.LastSpecChangeAnnotationKey] = summary
}

type specChange struct {
	path     string
	previous any
	current  any
}

func (c specChange) String() string {
	if c.previous == nil {
		return fmt.Sprintf("%s (added: %v)", c.path, c.current)
	}
	if c.current == nil {
		return fmt.Sprintf("%s (removed: %v)", c.path, c.previous)
	}
	return fmt.Sprintf("%s (%v → %v)", c.path, c.previous, c.current)
}

func formatChanges(changes []specChange) string {
	parts := make([]string, len(changes))
	for i, c := range changes {
		parts[i] = c.String()
	}
	return strings.Join(parts, ", ")
}

// changedJSONPaths compares two structs by JSON-round-tripping them into
// generic maps and recursively walking the result. This avoids writing
// per-field comparison logic and works for any spec shape.
func changedJSONPaths(prefix string, previous, current any) []specChange {
	var previousValue, currentValue any
	raw, err := json.Marshal(previous)
	if err != nil {
		return []specChange{{path: prefix, previous: previous, current: current}}
	}
	if err := json.Unmarshal(raw, &previousValue); err != nil {
		return []specChange{{path: prefix, previous: previous, current: current}}
	}
	raw, err = json.Marshal(current)
	if err != nil {
		return []specChange{{path: prefix, previous: previous, current: current}}
	}
	if err := json.Unmarshal(raw, &currentValue); err != nil {
		return []specChange{{path: prefix, previous: previous, current: current}}
	}

	changes := collectChangedJSONPaths(prefix, previousValue, currentValue)
	sort.Slice(changes, func(i, j int) bool { return changes[i].path < changes[j].path })
	return changes
}

func collectChangedJSONPaths(prefix string, previous, current any) []specChange {
	if reflect.DeepEqual(previous, current) {
		return nil
	}

	previousMap, previousIsMap := previous.(map[string]any)
	currentMap, currentIsMap := current.(map[string]any)
	if previousIsMap && currentIsMap {
		keys := make(map[string]struct{}, len(previousMap)+len(currentMap))
		for key := range previousMap {
			keys[key] = struct{}{}
		}
		for key := range currentMap {
			keys[key] = struct{}{}
		}

		changes := make([]specChange, 0, len(keys))
		for key := range keys {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			changes = append(changes, collectChangedJSONPaths(path, previousMap[key], currentMap[key])...)
		}
		return changes
	}

	// Arrays are compared by index position, so an insertion at the start
	// will report every subsequent element as changed. If the output looks
	// noisy, check the actual spec diff rather than relying on this summary.
	previousArr, previousIsArr := previous.([]any)
	currentArr, currentIsArr := current.([]any)
	if previousIsArr && currentIsArr {
		maxLen := max(len(previousArr), len(currentArr))
		changes := make([]specChange, 0, maxLen)
		for i := range maxLen {
			path := fmt.Sprintf("%s[%d]", prefix, i)
			var prev, curr any
			if i < len(previousArr) {
				prev = previousArr[i]
			}
			if i < len(currentArr) {
				curr = currentArr[i]
			}
			changes = append(changes, collectChangedJSONPaths(path, prev, curr)...)
		}
		return changes
	}

	return []specChange{{path: prefix, previous: previous, current: current}}
}

// handlePVCResize handles resizing the PVCs if the desired size on the cluster
// is larger than their current size. This operation does not require a rolling
// restart and does not require the cluster to be healthy to proceed, so it is
// performed outside of the `RollingUpdate` logic.
func (r *Reconciler) handlePVCResize(
	ctx context.Context, cluster *crdbv1beta1.CrdbCluster, nodes crdbv1beta1.CrdbNodeList,
) (ctrl.Result, error) {
	if !cluster.IsManagedMode() {
		logging.Warnf(
			ctx,
			"Reconciliation mode is %q; not handling PVC resize. CrdbCluster/%s",
			*cluster.Spec.Mode,
			cluster.Name,
		)

		// Clusters with a disabled reconciliation mode shouldn't handle PVC
		// resize.
		return controllers.NoRequeue()
	}

	clusterVolumeClaimTemplate := cluster.Spec.Template.Spec.DataStore.VolumeClaimTemplate
	if clusterVolumeClaimTemplate == nil {
		return controllers.NoRequeue()
	}
	clusterPVCSize := clusterVolumeClaimTemplate.Spec.Resources.Requests.Storage()

	// Check if expanding volume on any node. If we are, the update must go
	// through. The volume expansion may be blocking one or more pods becoming
	// healthy.
	for i := range nodes.Items {
		node := &nodes.Items[i]

		nodeVolumeClaimTemplate := node.Spec.DataStore.VolumeClaimTemplate
		if nodeVolumeClaimTemplate == nil {
			continue
		}
		nodePVCSize := nodeVolumeClaimTemplate.Spec.Resources.Requests.Storage()

		// If the volume needs to be resized, we want the update to go through
		// even if the pod isn't ready. The pod's volume could be out of space,
		// and the pod won't become healthy until the disks have been resized.
		if nodePVCSize.Cmp(*clusterPVCSize) < 0 {
			nodeVolumeClaimTemplate.Spec.Resources.Requests[corev1.ResourceStorage] = *clusterPVCSize
			if err := r.Ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, node); err != nil {
				return controllers.RequeueIfError(err)
			}
		}
	}

	return controllers.NoRequeue()
}

func (r *Reconciler) getExistingPVCSize(
	ctx context.Context, node *crdbv1beta1.CrdbNode,
) (*resource.Quantity, error) {
	if node.Spec.DataStore.VolumeClaimTemplate == nil {
		return nil, nil
	}
	pvc := node.Spec.DataStore.VolumeClaimTemplate
	pvcName := manifests.CrdbNodeStoragePVCName(node.Name, pvc)

	var existingPVC corev1.PersistentVolumeClaim
	err := r.Ctl.WithNamespace(node.Namespace).Get(ctx, pvcName, &existingPVC)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, errors.Wrapf(err, "failed to get PVC for node %s, PVC: %s", node.Name, pvc.Name)
	}
	return existingPVC.Spec.Resources.Requests.Storage(), nil
}

func requiresUpgradeValidation(cluster *crdbv1beta1.CrdbCluster) (bool, version.Version) {
	if cluster.Status.Image == "" || cluster.Status.Version == "" {
		return false, version.Version{}
	}

	if cluster.Spec.Template.Spec.Image == cluster.Status.Image {
		return false, version.Version{}
	}

	// Validation already completed for this upgrade cycle. The rolling update
	// is in progress and will continue until all nodes reach the new revision,
	// at which point Upgrade transitions from Starting to Finished and
	// Status.Image is updated to match Spec.Image.
	if cluster.Status.HasAction(crdbv1beta1.ValidateVersion, crdbv1beta1.Finished) &&
		cluster.Status.HasAction(crdbv1beta1.Upgrade, crdbv1beta1.Starting) {
		return false, version.Version{}
	}

	currentVersion, err := version.Parse(cluster.Status.Version)
	if err != nil {
		return false, version.Version{}
	}

	return true, currentVersion
}

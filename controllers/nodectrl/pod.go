package nodectrl

import (
	"context"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/restartclass"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// logRestart logs a pod restart event. It builds the common fields shared across all restart paths
// (cluster_name, node_name, restart_reason, crdb_node_id) and appends any path-specific fields passed
// by the caller. Annotation writes are handled by each call site since they differ per restart type.
func logRestart(ctx context.Context, pod *corev1.Pod, node *crdbv1beta1.CrdbNode, reason restartclass.RestartReason, extraFields ...zap.Field) {
	fields := []zap.Field{
		zap.String("cluster_name", pod.Labels["cluster"]),
		zap.String("node_name", pod.Name),
		zap.String("restart_reason", string(reason)),
	}
	// The node ID is assigned by CockroachDB after the pod joins the cluster, which can lag
	// behind pod readiness. A nil check is necessary even here.
	if node.Status.NodeID != nil {
		fields = append(fields, zap.Int64("crdb_node_id", *node.Status.NodeID))
	}
	fields = append(fields, extraFields...)
	logging.Info(ctx, "pod restart detected", fields...)
}

// ensurePod reconciles the underlying Pod of this CrdbNode and updates .Status
// to reflect that of the Pod.
func (r *Reconciler) ensurePod(
	ctx context.Context, node *crdbv1beta1.CrdbNode, pod *corev1.Pod,
) (ctrl.Result, error) {

	// Memory evictions are checked first before restartclass.ClassifyRestart() because evicted pods have no LastTerminationState.
	// When a pod is evicted, pod.Status.Reason is "Evicted" and phase is PodFailed. The pod will be deleted shortly
	// after and the new pod will have no eviction state, so we must catch before it disappears.
	if pod.Status.Reason == restartclass.PodReasonEvicted {
		// Only log once per eviction event. The CrdbNode annotation stores the evicted pod's UID
		// as the dedup guard, since the evicted pod is deleted during this reconcile and we cannot
		// annotate it directly.
		if node.Annotations[lastEvictionLoggedAnnotation] != string(pod.UID) {
			logRestart(ctx, pod, node, restartclass.RestartReasonNodePressureEviction,
				zap.String("message", pod.Status.Message),
				zap.String("evicted_pod_uid", string(pod.UID)),
			)
			if node.Annotations == nil {
				node.Annotations = make(map[string]string)
			}
			node.Annotations[lastEvictionLoggedAnnotation] = string(pod.UID)
			if err := r.Ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, node); err != nil {
				logging.Warnf(ctx, "failed to write eviction annotation: %v", err)
			}
		}
	} else {
		// Clear the eviction annotation on every non-evicted reconcile so a future eviction
		// is not silently skipped due to a stale annotation from a previous eviction.
		if _, exists := node.Annotations[lastEvictionLoggedAnnotation]; exists {
			delete(node.Annotations, lastEvictionLoggedAnnotation)
			if err := r.Ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, node); err != nil {
				logging.Warnf(ctx, "failed to clear eviction annotation: %v", err)
			}
		}
	}

	reason, containerStatus := restartclass.ClassifyRestart(pod)

	// Only log after the pod is fully Ready. The restart cycle has completed and the
	// kubelet-written fields (restart_count, exit_code, FinishedAt) are stable.
	// IsPodReady returns nil when Ready.
	if kube.IsPodReady(pod) == nil {

		// The condition passes when the cockroachdb container crashed and restarted, meaning restartclass.ClassifyRestart
		// found the container and populated its last termination state. For example, an OOMKill returns
		// (OOMKilled, &containerStatus) where containerStatus holds the exit code and timestamps.
		if reason != restartclass.RestartReasonNoLastTerminationState && reason != restartclass.RestartReasonCockroachContainerNotFound {
			currentFinishedAt := containerStatus.LastTerminationState.Terminated.FinishedAt.String() // timestamp written by kubelet when the pod crashed

			// lastLogged is empty on the first crash, holds the previous crash's FinishedAt on subsequent
			// reconciles of the same crash, and holds an old timestamp when a new crash happens. Comparing
			// against currentFinishedAt handles all three: log on first crash, skip on duplicate reconciles,
			// and log again on a new crash.
			lastLogged := pod.Annotations[lastRestartLoggedAnnotation]

			if lastLogged != currentFinishedAt {
				terminated := containerStatus.LastTerminationState.Terminated
				logFields := []zap.Field{
					zap.Int32("exit_code", terminated.ExitCode),
					zap.Int32("restart_count", containerStatus.RestartCount),
				}
				// StartedAt is zero when the container was killed before it finished
				// starting. Subtracting a zero StartedAt yields a nonsensical duration,
				// so only emit the field when StartedAt is set.
				if !terminated.StartedAt.IsZero() {
					logFields = append(logFields, zap.Float64("previous_run_duration_seconds",
						terminated.FinishedAt.Sub(terminated.StartedAt.Time).Seconds()))
				}
				logRestart(ctx, pod, node, reason, logFields...)

				// Initialize the annotations map if nil before writing.
				if pod.Annotations == nil {
					pod.Annotations = make(map[string]string)
				}
				pod.Annotations[lastRestartLoggedAnnotation] = currentFinishedAt

				// If the annotation write fails, the next reconcile will log the event again.
				if err := r.Ctl.Update(ctx, pod); err != nil {
					logging.Warnf(ctx, "failed to update restart annotation, next reconcile may log again: %v", err)
				}

			}

		}
	}

	// Propagate Pod information into the EvictionHelper. This should happen
	// before calling CanEvict to prevent unnecessary reconciles.
	r.EvictionHelper.UpdateStateFrom(ctx, node, pod.Status)

	expectedRef, err := kube.OwnerReferenceFor(r.Ctl.Scheme(), node)
	if err != nil {
		return controllers.RequeueIfError(errors.Wrap(err, "failed to generate owner reference"))
	}
	existing := metav1.GetControllerOf(pod)
	if existing == nil || existing.APIVersion != expectedRef.APIVersion {
		if err = ctrl.SetControllerReference(node, pod, r.Ctl.Scheme()); err != nil {
			return controllers.RequeueIfError(errors.Wrap(err, "failed to set owner ref"))
		}
		if err = r.Ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, pod); err != nil {
			return controllers.RequeueIfError(errors.Wrap(err, "failed to apply pod owner ref update"))
		}
	}

	// If the pod is not running, we should delete it immediately instead of waiting
	// to evict, so a new pod can be created ASAP.
	if r.shouldDelete(ctx, pod) {
		if err := r.Ctl.DeleteWithOptions(ctx, kube.DeleteOptions{NoWait: true}, pod); err != nil {
			logging.Errorf(ctx, "failed to delete Pod for CrdbNode %q: %v", node.Name, err)
			return controllers.RequeueIfError(err)
		}
	}

	// Recreating a decommissioning pod for a spec change is a footgun.
	// If CRDB severs the node right as a fresh pod comes up, the
	// replacement ends up hosting a process the peers have already
	// rejected, and any cluster-wide status checks against it would
	// hang. For decommissioning or excluded nodes the drift path stays
	// off. The pod is going away with the CrdbNode either way.
	//
	// Only the drift path is guarded here. A pod that crashes or goes
	// missing still runs through shouldDelete and getOrCreatePod normally.
	// The "permanently removed" tolerance in kubecrdbnode/exec.go is what
	// keeps the state machine unstuck if the replacement pod ever hits a
	// severed membership.
	if node.IsDecommissioning() || node.IsExcluded() {
		node.Status.SetCondition(crdbv1beta1.PodNeedsUpdate, metav1.ConditionFalse)
	} else if pod.Name == "" || r.podNeedsUpdate(ctx, node, pod) {
		if !node.Status.HasCondition(crdbv1beta1.PodNeedsUpdate, metav1.ConditionTrue) && pod.Status.Phase != corev1.PodFailed {
			logPodSpecDrift(ctx, node, pod)
		}
		node.Status.SetCondition(crdbv1beta1.PodNeedsUpdate, metav1.ConditionTrue)
		logging.Debugf(ctx, "pod needs update, new pod:\n%s\n\nnew node:\n%v", pod.String(), node)
	} else {
		node.Status.SetCondition(crdbv1beta1.PodNeedsUpdate, metav1.ConditionFalse)
		r.maybeRestampLegacyRevision(ctx, node, pod)
	}

	// If our Pod if out of date and needs to be updated, initialize the
	// eviction.
	if node.Status.HasCondition(crdbv1beta1.PodNeedsUpdate, metav1.ConditionTrue) {
		if ok, wait := r.EvictionHelper.CanEvict(ctx, node); !ok {
			logging.Infof(ctx, "not enough time has passed since last eviction, wait for another %v", wait)
			return controllers.RequeueAfter(
				wait,
				nil,
			)
		}
		logging.Infof(ctx, "evicting Pod/%s in %s: deployed revision %q does not match desired revision %q",
			pod.Name, pod.Namespace, manifests.GetNodePodRevision(pod), manifests.GetNodeHashRevisionNew(node))
		if err := r.Ctl.EvictPod(ctx, pod, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{
				UID:             &pod.UID,
				ResourceVersion: &pod.ResourceVersion,
			},
		}); err != nil {
			// PDB blocks the eviction when not enough healthy pods remain. The Evict API
			// returns 429 TooManyRequests in this case. Wait and retry rather than failing —
			// this is the normal backpressure path during rolling updates.
			if apierrors.IsTooManyRequests(err) {
				logging.Warnf(ctx, "eviction of pod %q blocked by PodDisruptionBudget, will retry", pod.Name)
				return controllers.RequeueAfter(30*time.Second, nil)
			}
			return controllers.RequeueIfError(err)
		}

		// Log RollingUpdate here, after EvictPod succeeds. This is the moment
		// the operator actually initiates the rolling restart of this pod, so
		// it's the most accurate point to record the event.
		if specChange, ok := node.Annotations[manifests.LastSpecChangeAnnotationKey]; ok {
			logRestart(ctx, pod, node, restartclass.RestartReasonRollingUpdate,
				zap.String("spec_changes", specChange),
				zap.String("evicted_pod_uid", string(pod.UID)),
				zap.String("deployed_revision", manifests.GetNodePodRevision(pod)),
				zap.String("desired_revision", manifests.GetNodeHashRevisionNew(node)),
			)
			delete(node.Annotations, manifests.LastSpecChangeAnnotationKey)
			if err := r.Ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, node); err != nil {
				logging.Warnf(ctx, "failed to clear spec change annotation: %v", err)
			}
		}

		// TODO(chrisseto): This is a bit of hack. We should probably do
		// something within EvictPod to make it a bit nicer to user. setting
		// DeletionTimestamp will cause podToNodeStatus to set the Pod as not
		// running which avoids errors around the decommissioning process
		// attempting to run on a recently evicted Pod.
		now := metav1.Now()
		pod.DeletionTimestamp = &now
	}

	// Once the Pod has a K8s Node assignment, we can fill TopologyValues from
	// that Node's labels. The Pod does not need to be Running because
	// spec.nodeName is enough to find the Node.
	if pod.Spec.NodeName != "" {
		var k8sNode corev1.Node
		if err := r.Ctl.Get(ctx, pod.Spec.NodeName, &k8sNode); err != nil {
			return controllers.RequeueIfError(errors.Wrapf(err, "get k8s node %q", k8sNode.Name))
		}
		// We read constraints from the persisted Pod instead of the CrdbNode's
		// PodTemplate because the Pod contains the effective constraints after
		// compatibility and precedence rules are applied. The cluster controller
		// later uses these values for explicit node placement and topology-aware
		// decommissioning.
		node.Status.TopologyValues = nil
		if len(pod.Spec.TopologySpreadConstraints) > 0 {
			node.Status.TopologyValues = make([]string, len(pod.Spec.TopologySpreadConstraints))
		}
		for i, constraint := range pod.Spec.TopologySpreadConstraints {
			value := k8sNode.Labels[constraint.TopologyKey]
			if value == "" {
				// A missing value should not block other CrdbNode status updates because
				// K8s owns topology constraint enforcement. We keep the empty value rather
				// than dropping it because decommissioning and explicit node placement use
				// the first entry. Otherwise, a later constraint could be mistaken for it.
				//
				// Since Node label changes do not trigger reconciliation, this empty value
				// remains until the next CrdbNode reconciliation updates TopologyValues.
				logging.Warnf(ctx, "topology key %q from Pod %q is missing or empty on K8s Node %q",
					constraint.TopologyKey, pod.Name, k8sNode.Name)
				continue
			}
			node.Status.TopologyValues[i] = value
		}
	}

	// Update node's status with information from the Pod. The top level
	// reconciler is responsible for persisting the status update.
	podToNodeStatus(node, pod)

	// Set NodeID only while the Pod is ready. The top-level reconciler normally
	// persists this status. The Basalt branch below writes it early together with
	// StoreIDs because decommissioning can return before the final status update.
	if node.Status.NodeID == nil && node.Status.HasCondition(crdbv1beta1.PodReady, metav1.ConditionTrue) {
		exec := r.NodeExecutor(node, r.Ctl.WithNamespace(node.Namespace))
		nodeID, err := exec.NodeID(ctx)
		if err != nil {
			return controllers.RequeueIfError(err)
		}
		node.Status.NodeID = &nodeID
		// Snapshot Basalt store IDs while this Pod is healthy and serving SQL. By
		// cleanup time the node has been decommissioned and removed from
		// kv_store_status, so the mapping is no longer available. Keep the query
		// next to the NodeID lookup rather than the decommission path: draining SQL
		// there races the query and can reject it with "server is not accepting
		// clients". Because NodeID is fetched only on a readiness transition,
		// ordinary reconciles do not repeat either query.
		if err := snapshotBasaltStoreIDs(ctx, node, exec, nodeID); err != nil {
			logging.Warnf(ctx, "failed to snapshot basalt store ids for CrdbNode %s/%s. Recorded ids %v may be incomplete and range-shared directories may require manual or sweeper reclamation: %+v",
				node.Namespace, node.Name, node.Status.StoreIDs, err)
		}
		if node.Spec.DataStore.IsBasalt() {
			// Decommissioning can return before the reconciler's normal status write.
			// Persist NodeID and StoreIDs together here so a draining node does not
			// repeat both SQL queries on every reconcile. A failed status update is
			// retried because neither value is durable yet.
			ctl := r.Ctl.WithNamespace(node.Namespace)
			if err := ctl.UpdateStatus(ctx, node); err != nil {
				return controllers.RequeueIfError(errors.Wrap(err, "persisting basalt node and store ids"))
			}
		}
	} else if node.Status.NodeID != nil && !node.Status.HasCondition(crdbv1beta1.PodReady, metav1.ConditionTrue) {
		node.Status.NodeID = nil
	}

	return controllers.NoRequeue()
}

func (r *Reconciler) shouldDelete(ctx context.Context, pod *corev1.Pod) bool {
	switch pod.Status.Phase {
	case corev1.PodFailed, corev1.PodSucceeded, corev1.PodUnknown:
		// Unknown shouldn't happen. According to the docs, it hasn't been used since 2015.
		{
			logging.Warnf(
				ctx,
				"Pod %q is in %q due to %q: %q",
				pod.Name,
				pod.Status.Phase,
				pod.Status.Reason,
				pod.Status.Message,
			)
			return true
		}
	case corev1.PodRunning, corev1.PodPending:
		{
			return false
		}
	default:
		{
			logging.Warnf(
				ctx,
				"Unexpected pod phase. Pod %q is in %q due to %q: %q",
				pod.Name,
				pod.Status.Phase,
				pod.Status.Reason,
				pod.Status.Message,
			)
			return false
		}
	}
}

func (r *Reconciler) createPod(
	ctx context.Context, ctl *kube.Ctl, node *crdbv1beta1.CrdbNode,
) (*corev1.Pod, error) {
	// TODO(jeffswenson): require NodeName once the feature is fully rolled
	// out.
	var k8sNode *corev1.Node
	if node.Spec.NodeName != "" {
		k8sNode = &corev1.Node{}
		if err := ctl.Get(ctx, node.Spec.NodeName, k8sNode); err != nil {
			return nil, err
		}
	}

	pod := manifests.CrdbNodePod(node, k8sNode)
	if err := ctrl.SetControllerReference(node, pod, r.Ctl.Scheme()); err != nil {
		return nil, errors.Wrap(err, "failed to set owner ref")
	}
	if err := ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, pod); err != nil {
		logging.Errorf(ctx, "failed to create Pod for CrdbNode %q: %v", node.Name, err)
		return nil, err
	}

	return pod, nil
}

// podNeedsUpdate determines if the Pod for this CrdbNode needs to be update to
// a new spec.
func (r *Reconciler) podNeedsUpdate(
	ctx context.Context, node *crdbv1beta1.CrdbNode, pod *corev1.Pod,
) bool {
	// If the Pod is already in the process of being terminated, don't evict it
	// a second time.
	if !pod.DeletionTimestamp.IsZero() {
		return false
	}

	// If the Pod has been evicted Kubernetes will mark it as failed and leave
	// it around. We'll have to recreate it, evicted Pods just dangle in the
	// API.
	if pod.Status.Phase == corev1.PodFailed {
		// Log out warning when we encounter
		logging.Warnf(ctx, "Pod %q failed due to %q: %q", pod.Name, pod.Status.Reason, pod.Status.Message)
		return true
	}

	// We compare first by generation, then by revision.
	// We cannot simply compare by revision due to the following race condition
	// during controller leadership transfer from A to B:
	// - B sees a new version of node, and successfully created a new pod.
	// - A might be looking at an old version of node due to network latency, and
	//   find that the new pod has a different revision, and then evict the
	//   pod.
	deployedGeneration := manifests.GetNodePodGeneration(pod)
	if deployedGeneration > node.Generation {
		metrics.NodePodEvictCheckErrorCounter.WithLabelValues("generation_skew").Inc()
		logging.Warnf(ctx,
			"deployed generation %d is greater than node's generation %d for node %q in %q",
			deployedGeneration, node.Generation, node.Name, node.Namespace,
		)
		return false
	}
	deployedRevision := manifests.GetNodePodRevision(pod)

	// New pods carry the new hash. We also accept the old hash, which included
	// the WAL disk size, so upgrading the operator doesn't restart pods that
	// were created before WAL size was excluded. Drop the old branch once all
	// pods carry the new hash.
	desiredRevision := manifests.GetNodeHashRevisionNew(node)
	backwardCompatibleRevision := manifests.GetNodeHashRevisionOld(node)

	return (deployedRevision != desiredRevision) && (deployedRevision != backwardCompatibleRevision)
}

// maybeRestampLegacyRevision checks if a pod's hash revision is equal
// to the old legacy one, as opposed to the new updated one, and updates
// the revision if so. Occasionally, we update the hash revision so that
// a change to a particular field doesn't cause an unnecessary rolling
// restart, and these updates are done via the old and new hash revision
// functions. When we update the hash revision calculated by the
// operator, we also want to update the revision on the pod to keep it
// up-to-date without causing that pod to restart. So this function
// checks if we are in that scenario, and rewrites the hash revision
// annotation if so.
func (r *Reconciler) maybeRestampLegacyRevision(
	ctx context.Context, node *crdbv1beta1.CrdbNode, pod *corev1.Pod,
) {
	if pod.Name == "" || !pod.DeletionTimestamp.IsZero() {
		return
	}

	deployedRevision := manifests.GetNodePodRevision(pod)
	desiredRevision := manifests.GetNodeHashRevisionNew(node)
	if deployedRevision == desiredRevision {
		// If the annotation on the pod already matches the new revision,
		// there's nothing to do.
		return
	} else if deployedRevision != manifests.GetNodeHashRevisionOld(node) {
		// We may reach this branch if the race condition described in the
		// podNeedsUpdate function is hit. In this scenario, if the hash
		// matches neither the old nor the new hash, then something
		// meaningful has actually changed in the pod and we cannot write a
		// new hash. A later reconciliation run will update the pod
		// properly once the pod race condition is resolved.
		return
	}

	base := pod.DeepCopy()
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[manifests.NodeRevisionAnnotationKey] = desiredRevision
	patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
	if err := r.Ctl.Patch(ctx, pod, patch); err != nil {
		logging.Warnf(ctx, "failed to re-stamp legacy pod revision for %q, next reconcile will retry: %v", pod.Name, err)
	}
}

func logPodSpecDrift(ctx context.Context, node *crdbv1beta1.CrdbNode, pod *corev1.Pod) {
	desiredRevision := manifests.GetNodeHashRevisionNew(node)

	if pod.Name == "" {
		logging.Infof(
			ctx,
			"Pod for CrdbNode/%s is missing; desired generation %d, desired revision %q",
			node.Name,
			node.Generation,
			desiredRevision,
		)
		return
	}

	logging.Infof(
		ctx,
		"Pod/%s for CrdbNode/%s spec drift detected: deployed generation %d, desired generation %d, deployed revision %q, desired revision %q",
		pod.Name,
		node.Name,
		manifests.GetNodePodGeneration(pod),
		node.Generation,
		manifests.GetNodePodRevision(pod),
		desiredRevision,
	)
}

// podToNodeStatus updates the status of a CrdbNode based on a Pod's status.
// It only updates in-memory status structure. The caller still needs to
// call ctl.UpdateStatus for persistence.
// This is useful because some code depends on updated status of the node.
func podToNodeStatus(node *crdbv1beta1.CrdbNode, pod *corev1.Pod) {
	if !pod.DeletionTimestamp.IsZero() {
		node.Status.SetCondition(crdbv1beta1.PodRunning, metav1.ConditionFalse)
		node.Status.SetConditionWithTimestamp(crdbv1beta1.PodReady, metav1.ConditionFalse, time.Now())
		return
	}
	if pod.Status.Phase == corev1.PodRunning {
		node.Status.SetCondition(crdbv1beta1.PodRunning, metav1.ConditionTrue)
	} else {
		node.Status.SetCondition(crdbv1beta1.PodRunning, metav1.ConditionFalse)
	}
	readyFound := false
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			readyFound = true
			node.Status.SetConditionWithTimestamp(
				crdbv1beta1.PodReady, metav1.ConditionStatus(cond.Status), cond.LastTransitionTime.Time,
			)
		}
	}
	if !readyFound {
		node.Status.SetConditionWithTimestamp(crdbv1beta1.PodReady, metav1.ConditionFalse, time.Now())
	}
}

package migrationctrl

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/utils/sliceutils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
)

// handleRollback handles rollback of an in-progress migration.
// Triggered when the migration label is removed.
func (r *Reconciler) handleRollback(
	ctx context.Context, ctl *kube.Ctl, discovery *DiscoveryResult, cluster *crdbv1beta1.CrdbCluster,
) (ctrl.Result, error) {
	migration := cluster.Status.Migration

	// If rollback is already complete, validate and clear status
	if migration != nil && migration.Phase == PhaseRollbackComplete {
		return r.validateAndClearRollback(ctx, ctl, discovery, cluster)
	}

	// Cannot rollback completed migration
	if migration != nil && migration.Phase == PhaseComplete {
		logging.Info(ctx, "Cannot rollback completed migration")
		return controllers.NoRequeue()
	}

	// Even if migration status is nil or incomplete, check for orphaned CrdbNodes
	// Handles cases where cert migration created CrdbNodes but they're stuck/failing
	hasOrphanedNodes, err := r.checkForOrphanedCrdbNodes(ctx, ctl, cluster.Name)
	if err != nil {
		return controllers.RequeueAfter(5*time.Second, errors.Wrap(err, "failed to check for orphaned nodes"))
	}

	if migration == nil && !hasOrphanedNodes {
		// No migration in progress and no orphaned nodes - nothing to rollback
		logging.Info(ctx, "No migration to rollback")
		return controllers.NoRequeue()
	}

	// Initialize migration status if nil (handles cases where CrdbNodes exist but status is missing)
	if migration == nil {
		logging.Warn(ctx, "Migration status is nil but CrdbNodes exist - initializing for rollback")
		if err := persistMigrationStatus(ctx, ctl, cluster, func(m *crdbv1beta1.MigrationStatus) {
			m.Message = "Rolling back orphaned migration"
		}, PhaseRollbackInProgress, discovery.MigrationType); err != nil {
			return controllers.RequeueAfter(5*time.Second, errors.Wrap(err, "failed to persist rollback init status"))
		}

		return controllers.RequeueImmediately()
	}

	logging.Infof(ctx, "Starting rollback from phase %s", migration.Phase)

	// If the STS was already deleted during finalization, automated rollback is not possible.
	// The STS must be manually recreated.
	if migration.Phase == PhaseFinalization && discovery.StatefulSet == nil {
		logging.Errorf(ctx, "Cannot rollback from Finalization, StatefulSet already deleted")
		failedMsg := "Automated rollback not possible: StatefulSet already deleted during finalization. " +
			"Manual recovery required: " +
			"1) Delete all CrdbNodes (kubectl delete crdbnode -l crdb.cockroachlabs.com/cluster=<name>), " +
			"2) Recreate StatefulSet, " +
			"3) Scale up pods and verify cluster health, " +
			"4) Remove migration label."
		if err := persistMigrationStatus(ctx, ctl, cluster, func(m *crdbv1beta1.MigrationStatus) {
			m.Phase = PhaseFailed
			m.Message = failedMsg
		}, PhaseFailed, migration.Type); err != nil {
			return controllers.RequeueAfter(5*time.Second, errors.Wrap(err, "failed to persist PhaseFailed status"))
		}
		return controllers.NoRequeue()
	}

	// Set phase to RollbackInProgress if not already set. The event records the phase
	// we're rolling back from so the user knows where the migration was interrupted.
	if migration.Phase != PhaseRollbackInProgress {
		previousPhase := migration.Phase
		migratedCount := len(migration.MigratedNodes)
		if err := persistMigrationStatus(ctx, ctl, cluster, func(m *crdbv1beta1.MigrationStatus) {
			m.Phase = PhaseRollbackInProgress
			m.Message = fmt.Sprintf("Rolling back migration from %s (%d nodes migrated)", previousPhase, migratedCount)
		}, PhaseRollbackInProgress, migration.Type); err != nil {
			return controllers.RequeueAfter(5*time.Second, errors.Wrap(err, "failed to persist RollbackInProgress"))
		}

		r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "RollbackStarted",
			"Rolling back migration from phase %s (%d/%d nodes migrated)", previousPhase, migratedCount, migration.TotalPods)
		return controllers.RequeueImmediately()
	}

	sts := discovery.StatefulSet
	if sts == nil {
		// StatefulSet is gone; delete orphaned CrdbNodes and the cluster, then exit.
		deleteOrphanedClusterResources(ctx, ctl, cluster)
		return controllers.NoRequeue()
	}

	// Record rollback metric
	RollbacksTotal.WithLabelValues(sts.Name, sts.Namespace, "manual").Inc()

	// Sync MigratedNodes in status with actual CrdbNodes before proceeding.
	// Status might show 1 migrated when 2 exist if reconciliation lags.
	if requeued := syncMigratedNodesStatus(ctx, ctl, cluster, migration); requeued {
		return controllers.RequeueImmediately()
	}

	// Process one CrdbNode at a time, driven by the STS replica count. The STS's
	// next pod is at index = currentReplicas, so we drain and delete the CrdbNode
	// at that index, then scale up by one. Each cycle verifies the previous STS pod
	// is ready before touching the next CrdbNode.
	if len(migration.MigratedNodes) > 0 {
		return r.rollbackNextNode(ctx, ctl, sts, cluster, migration)
	}

	// All CrdbNodes rolled back. Verify STS is at full replica count and all pods
	// are ready before running cleanup.
	if migration.TotalPods > 0 {
		if err := ctl.Get(ctx, sts.Name, sts); err != nil {
			return controllers.RequeueAfter(5*time.Second, errors.Wrap(err, "re-fetch STS for final check"))
		}
		currentReplicas := int32(0)
		if sts.Spec.Replicas != nil {
			currentReplicas = *sts.Spec.Replicas
		}
		if currentReplicas < migration.TotalPods {
			logging.Infof(ctx, "Final STS scale-up: %d -> %d", currentReplicas, migration.TotalPods)
			sts.Spec.Replicas = &migration.TotalPods
			if err := ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, sts); err != nil {
				return controllers.RequeueIfError(errors.Wrap(err, "final STS scale-up"))
			}
			return controllers.RequeueAfter(5*time.Second, nil)
		}
		if sts.Status.ReadyReplicas < migration.TotalPods {
			logging.Infof(ctx, "Waiting for final STS readiness (%d/%d)", sts.Status.ReadyReplicas, migration.TotalPods)
			migration.Message = fmt.Sprintf("Rollback: waiting for STS pods (%d/%d ready)", sts.Status.ReadyReplicas, migration.TotalPods)
			if err := ctl.UpdateStatus(ctx, cluster); err != nil && !apierrors.IsConflict(err) {
				logging.Warnf(ctx, "Status update during rollback wait: %v", err)
			}
			return controllers.RequeueAfter(5*time.Second, nil)
		}
	}
	logging.Info(ctx, "All CrdbNodes rolled back, STS fully ready")

	cleanupMigrationArtifacts(ctx, ctl, sts, migration, discovery)

	// cleanupMigrationArtifacts may have updated the v1alpha1 CrdbCluster
	// (removeMigrationLabel), which bumps the v1beta1 resourceVersion via the
	// conversion webhook. Re-fetch so subsequent writes use a fresh version.
	migration, err = refetchClusterMigration(ctx, ctl, cluster, PhaseRollbackInProgress, discovery.MigrationType)
	if err != nil {
		return controllers.RequeueAfter(5*time.Second, errors.Wrap(err, "re-fetch after cleanup"))
	}

	if migration.Type == MigrationTypeHelm {
		// For Helm migration, the CrdbCluster was created by this controller so delete it now.
		logging.Infof(ctx, "Deleting migration-created CrdbCluster for Helm rollback")
		if err := ctl.Delete(ctx, cluster); err != nil && !apierrors.IsNotFound(err) {
			logging.Warnf(ctx, "Failed to delete CrdbCluster (non-fatal): %v", err)
		}
		r.Recorder.Event(cluster, corev1.EventTypeWarning, "RollbackComplete", "Helm migration rolled back")
		return controllers.NoRequeue()
	}

	// Operator migration: keep CrdbCluster, set Mode=Disabled so v1beta1 controller
	// minimizes its actions until the public operator resumes.
	if cluster.Spec.Mode == nil || *cluster.Spec.Mode != crdbv1beta1.Disabled {
		logging.Infof(ctx, "Setting cluster mode to Disabled after rollback")
		mode := crdbv1beta1.Disabled
		cluster.Spec.Mode = &mode
		if err := ctl.Update(ctx, cluster); err != nil {
			logging.Warnf(ctx, "Failed to set mode to Disabled (non-fatal): %v", err)
		}
		// ctl.Update replaces the in-memory cluster object, stale-ifying the
		// migration pointer. Re-derive it so the phase write below lands correctly.
		migration, err = refetchClusterMigration(ctx, ctl, cluster, PhaseRollbackInProgress, discovery.MigrationType)
		if err != nil {
			return controllers.RequeueAfter(5*time.Second, errors.Wrap(err, "re-fetch after mode update"))
		}
	}

	// Transition to RollbackComplete phase for validation. persistMigrationStatus
	// retries on conflict so the phase survives concurrent betaclusterctrl writes.
	migrationType := migration.Type
	if err = persistMigrationStatus(ctx, ctl, cluster, func(m *crdbv1beta1.MigrationStatus) {
		m.Phase = PhaseRollbackComplete
		m.Message = "Rollback complete - awaiting validation"
	}, PhaseRollbackInProgress, migrationType); err != nil {
		return controllers.RequeueAfter(5*time.Second, errors.Wrap(err, "failed to persist RollbackComplete"))
	}

	logging.Info(ctx, "Rollback operations complete - transitioning to validation phase")
	r.Recorder.Event(cluster, corev1.EventTypeWarning, "RollbackComplete", "Migration rolled back successfully")
	return controllers.RequeueAfter(5*time.Second, nil)
}

// deleteOrphanedClusterResources deletes all CrdbNodes for the cluster followed by the
// CrdbCluster itself. Used when the StatefulSet is gone and there is nothing left to rollback
// to. All migration-created resources are removed.
func deleteOrphanedClusterResources(
	ctx context.Context, ctl *kube.Ctl, cluster *crdbv1beta1.CrdbCluster,
) {
	logging.Warn(ctx, "No StatefulSet found for rollback")
	clusterNodes, err := listClusterCrdbNodes(ctx, ctl, cluster.Name)
	if err != nil {
		logging.Warnf(ctx, "Failed to list CrdbNodes for cleanup: %v", err)
	}
	for i := range clusterNodes {
		if err := ctl.Delete(ctx, &clusterNodes[i]); err != nil && !apierrors.IsNotFound(err) {
			logging.Warnf(ctx, "Failed to delete CrdbNode %s: %v", clusterNodes[i].Name, err)
		}
	}
	if err := ctl.Delete(ctx, cluster); err != nil && !apierrors.IsNotFound(err) {
		logging.Warnf(ctx, "Failed to delete v1beta1 CrdbCluster during rollback: %v", err)
	}
}

// syncMigratedNodesStatus reconciles the MigratedNodes list in status with the actual CrdbNodes
// found by label. When there is a mismatch it updates status and returns true so the caller can
// requeue immediately and continue rollback with the corrected list.
func syncMigratedNodesStatus(
	ctx context.Context,
	ctl *kube.Ctl,
	cluster *crdbv1beta1.CrdbCluster,
	migration *crdbv1beta1.MigrationStatus,
) bool {
	actualMigratedNodes, err := discoverActualMigratedNodes(ctx, ctl, cluster.Name)
	if err != nil {
		logging.Warnf(ctx, "Failed to discover actual migrated nodes (non-fatal): %v", err)
		return false
	}
	if sliceutils.MultiSetsEqual(actualMigratedNodes, migration.MigratedNodes) {
		return false
	}
	logging.Warnf(ctx, "Status mismatch: MigratedNodes shows %v but found %v actual CrdbNodes - updating status",
		migration.MigratedNodes, actualMigratedNodes)
	if err := persistMigrationStatus(ctx, ctl, cluster, func(m *crdbv1beta1.MigrationStatus) {
		m.MigratedNodes = actualMigratedNodes
		m.Message = fmt.Sprintf("Rolling back - synced status (%d nodes to delete)", len(actualMigratedNodes))
	}, PhaseRollbackInProgress, migration.Type); err != nil {
		logging.Warnf(ctx, "Failed to sync status before rollback (non-fatal): %v", err)
		return false
	}
	return true
}

// cleanupMigrationArtifacts removes all side effects created by the migration controller
// during rollback. All steps are non-fatal. Failures are logged and execution continues
// to clean up as many resources as possible.
func cleanupMigrationArtifacts(
	ctx context.Context,
	ctl *kube.Ctl,
	sts *appsv1.StatefulSet,
	migration *crdbv1beta1.MigrationStatus,
	discovery *DiscoveryResult,
) {
	if err := removeMigrationLabelsFromPods(ctx, ctl, sts.Name, migration.TotalPods); err != nil {
		logging.Warnf(ctx, "Failed to remove migration labels from pods (non-fatal): %v", err)
	}
	if err := deleteMigrationCertResources(ctx, ctl, sts.Name, migration.Type); err != nil {
		logging.Warnf(ctx, "Failed to delete migration cert resources (non-fatal): %v", err)
	}
	switch migration.Type {
	case MigrationTypeOperator:
		// Undo the key rename applied during cert migration (logs.yaml → logging.yaml).
		// Use the actual ConfigMap name from the v1alpha1 CR when available. The forward
		// path uses V1Alpha1Cluster.Spec.LogConfigMap, so rollback must use the same name
		// to find the right ConfigMap. Fall back to name-guessing only when the v1alpha1
		// CR is unavailable (e.g. it was deleted before rollback).
		logConfigMapName := ""
		if discovery.V1Alpha1Cluster != nil {
			logConfigMapName = discovery.V1Alpha1Cluster.Spec.LogConfigMap
		}
		if err := reverseConfigMapKeyMove(ctx, ctl, sts.Name, logConfigMapName); err != nil {
			logging.Warnf(ctx, "Failed to reverse ConfigMap key migration (non-fatal): %v", err)
		}
	case MigrationTypeHelm:
		// Delete the ConfigMap we created from the original Secret.
		if err := deleteHelmLogConfigMap(ctx, ctl, sts); err != nil {
			logging.Warnf(ctx, "Failed to delete Helm log-config ConfigMap (non-fatal): %v", err)
		}
	}
	if err := deleteAllPodDisruptionBudgets(ctx, ctl, sts.Name); err != nil {
		logging.Warnf(ctx, "PDB deletion failed (non-fatal): %v", err)
	}
	// Restore original service selectors so the v1alpha1 operator can adopt the service.
	if err := restoreServiceSelectors(ctx, ctl, sts.Name, migration.Type); err != nil {
		logging.Warnf(ctx, "Failed to restore service selectors during rollback: %v", err)
	}
	// Restore the original port configuration that was saved before migration modified it.
	if err := restoreServicePorts(ctx, ctl, sts.Name); err != nil {
		logging.Warnf(ctx, "Failed to restore service ports during rollback: %v", err)
	}
	deleteMigrationClusterRBAC(ctx, ctl, sts.Name)
	if err := removeMigrationLabel(ctx, ctl, discovery); err != nil {
		logging.Warnf(ctx, "Failed to remove migration label after rollback (non-fatal): %v", err)
	}
}

// rollbackWait persists a status message and requeues after 5 seconds.
// Used in validateAndClearRollback while waiting for cluster state to converge.
func rollbackWait(
	ctx context.Context, ctl *kube.Ctl, cluster *crdbv1beta1.CrdbCluster, message string,
) (ctrl.Result, error) {
	cluster.Status.Migration.Message = message
	if err := ctl.UpdateStatus(ctx, cluster); err != nil && !apierrors.IsConflict(err) {
		logging.Warnf(ctx, "Failed to update status (non-fatal): %v", err)
	}
	return controllers.RequeueAfter(5*time.Second, nil)
}

// rollbackNextNode processes one CrdbNode per invocation. It uses the current STS
// replica count to determine which pod index to restore next. STS uses OrderedReady
// policy, so scaling from N to N+1 creates pod at index N. The full per-node cycle is:
//
//  1. Verify all current STS pods are ready before touching the next node.
//
//  2. Check SQL health (under-replicated ranges and live node count) so we don't
//     compound data risk across successive rollback steps.
//
//  3. Find the CrdbNode at index = currentReplicas.
//
//  4. Drain and delete it. Drain failures are non-fatal since CRDB self-drains on SIGTERM.
//
//  5. Wait for the pod to be gone.
//
//  6. Scale the STS up by one and wait for the new pod to be ready.
//
//  7. Remove the node from MigratedNodes and persist progress.
//
// Each step requeues, so only one step runs per reconcile. This ensures at most one
// node is down at any time during rollback.
func (r *Reconciler) rollbackNextNode(
	ctx context.Context,
	ctl *kube.Ctl,
	sts *appsv1.StatefulSet,
	cluster *crdbv1beta1.CrdbCluster,
	migration *crdbv1beta1.MigrationStatus,
) (ctrl.Result, error) {
	// Re-fetch STS for current replica count and status.
	if err := ctl.Get(ctx, sts.Name, sts); err != nil {
		return controllers.RequeueAfter(5*time.Second, errors.Wrap(err, "re-fetch STS"))
	}
	currentReplicas := int32(0)
	if sts.Spec.Replicas != nil {
		currentReplicas = *sts.Spec.Replicas
	}
	remaining := len(migration.MigratedNodes)

	// 1. Wait for all current STS pods to be ready before touching the next CrdbNode.
	// This verifies the previous scale-up completed successfully.
	if currentReplicas > 0 && sts.Status.ReadyReplicas < currentReplicas {
		migration.Message = fmt.Sprintf("Rollback: waiting for STS pods to be ready (%d/%d)", sts.Status.ReadyReplicas, currentReplicas)
		return controllers.RequeueAfter(5*time.Second, nil)
	}

	// The next pod the STS will create is at index currentReplicas.
	targetIdx := currentReplicas
	targetName := fmt.Sprintf("%s-%d", sts.Name, targetIdx)

	// 2. Check whether the target CrdbNode exists. If it does, we need to drain
	// and delete it before scaling the STS up. If it doesn't, the STS was scaled
	// down during forward migration but the CrdbNode was never created (e.g. the
	// operator was stopped in the gap between scale-down and node creation). In
	// that case we skip the health check and go straight to scaling the STS up,
	// since restoring a missing node can only improve cluster health.
	var targetNode crdbv1beta1.CrdbNode
	targetExists := false
	if err := ctl.Get(ctx, targetName, &targetNode); err == nil {
		targetExists = true
	} else if !apierrors.IsNotFound(err) {
		return controllers.RequeueIfError(err)
	}

	if targetExists {
		// Verify SQL-level cluster health before removing this node. Under-replicated
		// ranges from a previous rollback step must resolve first, otherwise successive
		// deletions compound the replication deficit.
		expectedLive := currentReplicas + int32(remaining)
		healthy, healthErr := r.checkRollbackHealth(ctx, ctl, migration, expectedLive)
		if healthErr != nil {
			// A Get failure means the migrated nodes list is stale. Requeue so
			// syncMigratedNodesStatus can correct it before we touch any nodes.
			return controllers.RequeueAfter(5*time.Second, errors.Wrap(healthErr, "rollback health check failed, requeuing to sync state"))
		}
		if !healthy {
			migration.Message = fmt.Sprintf("Rollback: waiting for cluster health before next step (%d nodes remaining)", remaining)
			return controllers.RequeueAfter(10*time.Second, nil)
		}

		logging.Infof(ctx, "Rolling back node %s (index %d, %d remaining)", targetName, targetIdx, remaining)

		// 3. Drain and delete the CrdbNode. Draining transfers range leases and
		// disconnects SQL clients before the pod goes away, keeping the disruption
		// window short. Drain failures are non-fatal since deleting the CrdbNode
		// triggers pod deletion and CRDB's SIGTERM handler drains as fallback.
		if err := drainCrdbNodeForRollback(ctx, ctl, &targetNode); err != nil {
			logging.Warnf(ctx, "Pre-drain of %s failed (proceeding with delete): %v", targetName, err)
		} else {
			logging.Infof(ctx, "Drained %s before rollback delete", targetName)
		}

		logging.Infof(ctx, "Deleting CrdbNode %s for rollback", targetName)
		if err := ctl.Delete(ctx, &targetNode); err != nil && !apierrors.IsNotFound(err) {
			return controllers.RequeueIfError(errors.Wrapf(err, "delete CrdbNode %s", targetName))
		}
		migration.Message = fmt.Sprintf("Rollback: draining and deleting CrdbNode %s (%d remaining)", targetName, remaining)
		return controllers.RequeueAfter(5*time.Second, nil)
	}

	// Target CrdbNode doesn't exist. This is either because we already deleted it
	// on a previous reconcile, or because forward migration scaled down the STS
	// without creating the CrdbNode. Either way, proceed to scaling the STS up.
	logging.Infof(ctx, "Rolling back index %d (no CrdbNode found, restoring STS pod %s)", targetIdx, targetName)

	// 4. Wait for the CrdbNode's pod to be fully removed. The STS controller
	// cannot create a pod with the same name while the old one still exists.
	var pod corev1.Pod
	if err := ctl.Get(ctx, targetName, &pod); err == nil {
		logging.Infof(ctx, "Waiting for pod %s to be deleted", targetName)
		migration.Message = fmt.Sprintf("Rollback: waiting for pod %s deletion (%d remaining)", targetName, remaining)
		return controllers.RequeueAfter(5*time.Second, nil)
	} else if !apierrors.IsNotFound(err) {
		return controllers.RequeueIfError(err)
	}

	// 5. Scale STS up by one to restore this pod.
	desiredReplicas := currentReplicas + 1
	logging.Infof(ctx, "Scaling STS from %d to %d to restore pod %s", currentReplicas, desiredReplicas, targetName)
	sts.Spec.Replicas = &desiredReplicas
	if err := ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, sts); err != nil {
		return controllers.RequeueIfError(errors.Wrapf(err, "scale STS for %s", targetName))
	}

	// 6. Remove the node from MigratedNodes and persist progress. We persist after
	// the scale-up so that if it fails, the node stays in the list and gets retried.
	updatedNodes := make([]string, 0, remaining-1)
	for _, n := range migration.MigratedNodes {
		if n != targetName {
			updatedNodes = append(updatedNodes, n)
		}
	}

	progressMsg := fmt.Sprintf("Rollback: restored pod %s (%d nodes remaining)", targetName, len(updatedNodes))
	if err := persistMigrationStatus(ctx, ctl, cluster, func(m *crdbv1beta1.MigrationStatus) {
		m.MigratedNodes = updatedNodes
		m.CurrentPodIndex = targetIdx
		m.Message = progressMsg
	}, PhaseRollbackInProgress, migration.Type); err != nil {
		return controllers.RequeueAfter(5*time.Second, errors.Wrap(err, "persist rollback progress"))
	}

	logging.Infof(ctx, "Rolled back node %s, %d remaining", targetName, len(updatedNodes))
	r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "NodeRolledBack",
		"Restored pod %s to StatefulSet (%d remaining)", targetName, len(updatedNodes))

	// Requeue to wait for the new STS pod to be ready (step 1 on next iteration)
	// before processing the next CrdbNode.
	return controllers.RequeueAfter(5*time.Second, nil)
}

// checkRollbackHealth verifies SQL-level cluster health before proceeding with the
// next rollback step. It validates all migrated nodes still exist, then execs into
// one of them to check under-replicated ranges and live node count.
func (r *Reconciler) checkRollbackHealth(
	ctx context.Context,
	ctl *kube.Ctl,
	migration *crdbv1beta1.MigrationStatus,
	expectedLiveNodes int32,
) (bool, error) {
	// Verify the migrated nodes list is still accurate before proceeding. If a
	// Get fails, the list is stale and we return an error so the reconciler
	// requeues and syncs state before doing anything else.
	if len(migration.MigratedNodes) == 0 {
		return true, nil
	}
	var node crdbv1beta1.CrdbNode
	for _, name := range migration.MigratedNodes {
		if err := ctl.Get(ctx, name, &node); err != nil {
			return false, errors.Wrapf(err, "failed to verify CrdbNode %s, migrated nodes list may be stale", name)
		}
	}

	healthy, reason, err := r.checkNodeHealth(ctx, ctl, &node, expectedLiveNodes)
	if err != nil {
		return false, err
	}
	if !healthy {
		logging.Infof(ctx, "Rollback health check not passing yet (%s), waiting before next step", reason)
	}
	return healthy, nil
}

// drainCrdbNodeForRollback drains a CrdbNode's pod before deleting it during rollback.
func drainCrdbNodeForRollback(
	ctx context.Context, ctl *kube.Ctl, node *crdbv1beta1.CrdbNode,
) error {
	return drainLocalNode(ctx, ctl, node.Name, manifests.CRDBContainerName, node.Spec.GetGRPCPort(), node.Spec.TLSEnabled)
}

// removeMigrationLabelsFromPods removes the cluster owner label and svc label that
// were added to StatefulSet pods during migration.
func removeMigrationLabelsFromPods(
	ctx context.Context, ctl *kube.Ctl, clusterName string, replicas int32,
) error {
	logging.Infof(ctx, "Removing migration labels from pods for cluster %s", clusterName)

	for i := range replicas {
		podName := fmt.Sprintf("%s-%d", clusterName, i)
		pod := &corev1.Pod{}

		err := ctl.Get(ctx, podName, pod)
		if err != nil {
			if apierrors.IsNotFound(err) {
				logging.Warnf(ctx, "Pod %s not found, skipping label removal", podName)
				continue
			}
			return errors.Wrapf(err, "failed to get pod %s", podName)
		}

		// Remove the labels we added during migration
		labelsToRemove := []string{
			"crdb.cockroachlabs.com/cluster",
			"svc",
		}

		modified := false
		for _, label := range labelsToRemove {
			if _, exists := pod.Labels[label]; exists {
				delete(pod.Labels, label)
				modified = true
			}
		}

		if modified {
			if err := ctl.Update(ctx, pod); err != nil {
				return errors.Wrapf(err, "failed to update pod %s", podName)
			}
			logging.Infof(ctx, "Removed migration labels from pod %s", podName)
		}
	}

	return nil
}

// checkForOrphanedCrdbNodes checks if there are any CrdbNodes for this cluster.
// This is used during rollback to detect orphaned nodes even if migration status is incomplete.
func (r *Reconciler) checkForOrphanedCrdbNodes(
	ctx context.Context, ctl *kube.Ctl, clusterName string,
) (bool, error) {
	nodes, err := listClusterCrdbNodes(ctx, ctl, clusterName)
	if err != nil {
		return false, errors.Wrap(err, "failed to list CrdbNodes")
	}
	if len(nodes) > 0 {
		logging.Infof(ctx, "Found %d orphaned CrdbNode(s) for cluster %s", len(nodes), clusterName)
	}
	return len(nodes) > 0, nil
}

// discoverActualMigratedNodes discovers all CrdbNodes for this cluster by label.
// Returns the actual list of migrated node names, which may differ from status.
// Used before rollback to sync status with reality.
func discoverActualMigratedNodes(
	ctx context.Context, ctl *kube.Ctl, clusterName string,
) ([]string, error) {
	nodes, err := listClusterCrdbNodes(ctx, ctl, clusterName)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list CrdbNodes")
	}
	names := make([]string, len(nodes))
	for i, node := range nodes {
		names[i] = node.Name
	}
	return names, nil
}

// tryDeleteMigrationResource deletes a K8s object by name if it exists.
// It returns an error only if the delete call fails with a reason other than NotFound.
func tryDeleteMigrationResource(
	ctx context.Context, ctl *kube.Ctl, name, kind string, obj kube.Object,
) error {
	obj.SetName(name)
	if err := ctl.Delete(ctx, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return errors.Wrapf(err, "failed to delete %s %s", kind, name)
	}
	logging.Infof(ctx, "Deleted %s %s", kind, name)
	return nil
}

// deleteMigrationCertResources deletes certificate-related resources created during migration.
// Always deletes the CA ConfigMap ({name}-ca-crt). For operator migrations only, also deletes
// the {name}-node-secret and {name}-client-secret. These are newly created secrets, not the
// original Helm secrets, which must be preserved.
func deleteMigrationCertResources(
	ctx context.Context, ctl *kube.Ctl, clusterName, migrationType string,
) error {
	logging.Infof(ctx, "Deleting migration-created cert resources for cluster %s", clusterName)

	if err := tryDeleteMigrationResource(ctx, ctl, clusterName+suffixCAConfigMap, "CA ConfigMap", &corev1.ConfigMap{}); err != nil {
		return err
	}

	// for operator migrations only, delete the secrets created during cert migration
	if migrationType == MigrationTypeOperator {
		if err := tryDeleteMigrationResource(ctx, ctl, clusterName+suffixNodeSecret, "node secret", &corev1.Secret{}); err != nil {
			return err
		}
		if err := tryDeleteMigrationResource(ctx, ctl, clusterName+suffixClientSecret, "client secret", &corev1.Secret{}); err != nil {
			return err
		}
	}

	return nil
}

// deleteMigrationClusterRBAC deletes the cluster-scoped ClusterRole and ClusterRoleBinding
// created by the migration controller. These use namespace-qualified names to prevent collision.
func deleteMigrationClusterRBAC(ctx context.Context, ctl *kube.Ctl, clusterName string) {
	crName := fmt.Sprintf("%s-%s", ctl.Namespace(), clusterName)

	if err := tryDeleteMigrationResource(ctx, ctl, crName, "ClusterRole", &rbacv1.ClusterRole{}); err != nil {
		logging.Warnf(ctx, "Failed to delete ClusterRole %s: %v", crName, err)
	}
	if err := tryDeleteMigrationResource(ctx, ctl, crName, "ClusterRoleBinding", &rbacv1.ClusterRoleBinding{}); err != nil {
		logging.Warnf(ctx, "Failed to delete ClusterRoleBinding %s: %v", crName, err)
	}
	// Namespace-scoped Role and RoleBinding use the cluster name directly (not namespace-qualified).
	if err := tryDeleteMigrationResource(ctx, ctl, clusterName, "Role", &rbacv1.Role{}); err != nil {
		logging.Warnf(ctx, "Failed to delete Role %s: %v", clusterName, err)
	}
	if err := tryDeleteMigrationResource(ctx, ctl, clusterName, "RoleBinding", &rbacv1.RoleBinding{}); err != nil {
		logging.Warnf(ctx, "Failed to delete RoleBinding %s: %v", clusterName, err)
	}
}

// removeSkipReconcileLabel removes the skip-reconcile label from the v1alpha1 CrdbCluster.
// This must be the last step in operator migration rollback. Once removed, the public operator
// resumes reconciliation and recreates its own RBAC, PDB, and services.
func removeSkipReconcileLabel(
	ctx context.Context, ctl *kube.Ctl, discovery *DiscoveryResult,
) error {
	if discovery.V1Alpha1Cluster == nil {
		return nil
	}
	if discovery.V1Alpha1Cluster.Labels == nil {
		return nil
	}
	if _, ok := discovery.V1Alpha1Cluster.Labels[LabelSkipReconcile]; !ok {
		return nil
	}
	delete(discovery.V1Alpha1Cluster.Labels, LabelSkipReconcile)
	logging.Infof(ctx, "Removing skip-reconcile label from v1alpha1 CrdbCluster %s", discovery.Name)
	return ctl.Update(ctx, discovery.V1Alpha1Cluster)
}

// validateAndClearRollback validates that rollback is complete before clearing migration status.
// Prevents premature status clearing and ensures clean handoff to v1alpha1 operator.
func (r *Reconciler) validateAndClearRollback(
	ctx context.Context, ctl *kube.Ctl, discovery *DiscoveryResult, cluster *crdbv1beta1.CrdbCluster,
) (ctrl.Result, error) {
	migration := cluster.Status.Migration
	logging.Infof(ctx, "Validating rollback completion for cluster %s", cluster.Name)

	// Already validated in a previous reconcile. Avoid re-running all checks.
	if strings.HasPrefix(migration.Message, "Rollback validated") {
		logging.Infof(ctx, "Rollback already validated for cluster %s", cluster.Name)
		return controllers.NoRequeue()
	}

	// 1. Verify no CrdbNodes exist.
	clusterNodes, err := listClusterCrdbNodes(ctx, ctl, cluster.Name)
	if err != nil {
		return controllers.RequeueAfter(5*time.Second, errors.Wrap(err, "failed to list CrdbNodes during validation"))
	}
	if len(clusterNodes) > 0 {
		logging.Warnf(ctx, "Rollback validation: %d CrdbNode(s) still exist, waiting for deletion", len(clusterNodes))
		return rollbackWait(ctx, ctl, cluster, fmt.Sprintf("Waiting for %d CrdbNode(s) to be deleted", len(clusterNodes)))
	}

	// 2. Verify StatefulSet exists and replicas match original.
	sts := &appsv1.StatefulSet{}
	if err := ctl.Get(ctx, cluster.Name, sts); err != nil {
		if apierrors.IsNotFound(err) {
			logging.Warn(ctx, "Rollback validation: StatefulSet not found")
			return rollbackWait(ctx, ctl, cluster, "Waiting for StatefulSet to be created by v1alpha1 operator")
		}
		return controllers.RequeueAfter(5*time.Second, errors.Wrap(err, "failed to get StatefulSet during validation"))
	}

	expectedReplicas := migration.TotalPods
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != expectedReplicas {
		currentReplicas := int32(0)
		if sts.Spec.Replicas != nil {
			currentReplicas = *sts.Spec.Replicas
		}
		logging.Warnf(ctx, "Rollback validation: StatefulSet replicas (%d) don't match original (%d)", currentReplicas, expectedReplicas)
		return rollbackWait(ctx, ctl, cluster, fmt.Sprintf("Waiting for StatefulSet replicas to match original (%d)", expectedReplicas))
	}

	// 3. Verify all pods are ready.
	if sts.Status.ReadyReplicas != expectedReplicas {
		logging.Warnf(ctx, "Rollback validation: Only %d/%d pods are ready", sts.Status.ReadyReplicas, expectedReplicas)
		return rollbackWait(ctx, ctl, cluster, fmt.Sprintf("Waiting for all %d pods to be ready (%d ready)", expectedReplicas, sts.Status.ReadyReplicas))
	}

	// 4. Verify service ownership. The headless service should never carry v1beta1 ownerRefs
	// because the v1beta1 controller only creates the JoinService during migration. This check
	// catches unexpected state that could block the v1alpha1 operator from adopting the service.
	svc := &corev1.Service{}
	if err := ctl.Get(ctx, cluster.Name, svc); err != nil {
		logging.Warnf(ctx, "Rollback validation: Failed to get service: %v", err)
		return rollbackWait(ctx, ctl, cluster, "Waiting for service to be available")
	}
	for _, ref := range svc.OwnerReferences {
		if ref.APIVersion == "crdb.cockroachlabs.com/v1beta1" {
			logging.Errorf(ctx, "Unexpected v1beta1 ownerReference found on service - this should not happen during migration")
			return rollbackWait(ctx, ctl, cluster, "Service has unexpected v1beta1 ownerReference")
		}
	}

	// All validations passed. Hand control back to the public operator before we
	// clear status so a failed label update stays retryable.
	if migration.Type == MigrationTypeOperator {
		if err := removeSkipReconcileLabel(ctx, ctl, discovery); err != nil {
			logging.Warnf(ctx, "Failed to remove skip-reconcile label: %v", err)
			return rollbackWait(ctx, ctl, cluster, "Waiting to hand control back to the public operator")
		}
		// removeSkipReconcileLabel updates the v1alpha1 CrdbCluster, which bumps the
		// v1beta1 resourceVersion via the conversion webhook. Re-fetch so the status
		// clear below uses a fresh resourceVersion.
		if _, err := refetchClusterMigration(ctx, ctl, cluster, PhaseRollbackComplete, migration.Type); err != nil {
			return controllers.RequeueAfter(5*time.Second, errors.Wrap(err, "re-fetch after label removal"))
		}
	}

	// All validations passed. Keep migration status as a permanent audit trail
	// instead of clearing it. The "Rollback validated" prefix is used as an
	// idempotency guard at the top of this function to skip re-validation.
	logging.Info(ctx, "Rollback validation complete - all checks passed")
	logging.Infof(ctx, "  No CrdbNodes exist")
	logging.Infof(ctx, "  StatefulSet replicas restored to %d", expectedReplicas)
	logging.Infof(ctx, "  All %d pods are ready", expectedReplicas)
	logging.Infof(ctx, "  Service adopted by v1alpha1 operator")

	if err := persistMigrationStatus(ctx, ctl, cluster, func(m *crdbv1beta1.MigrationStatus) {
		m.Phase = PhaseRollbackComplete
		m.Message = fmt.Sprintf("Rollback validated - cluster fully restored to v1alpha1 control (%d pods ready)", expectedReplicas)
	}, PhaseRollbackComplete, migration.Type); err != nil {
		return controllers.RequeueAfter(5*time.Second, errors.Wrap(err, "failed to persist rollback validated status"))
	}

	logging.Info(ctx, "Rollback validated - migration status preserved as audit trail")
	r.Recorder.Event(cluster, corev1.EventTypeNormal, "RollbackValidated", "Rollback validated - cluster fully restored")

	return controllers.NoRequeue()
}

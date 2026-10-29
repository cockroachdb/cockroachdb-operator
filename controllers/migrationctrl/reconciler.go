package migrationctrl

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1alpha1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1alpha1"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

type Reconciler struct {
	Ctl        *kube.Ctl
	Recorder   record.EventRecorder
	inTestMode bool
	// CloudRegion is the operator's own region, detected from the K8s node topology
	// label or the CLOUD_REGION env var. Used as the default region when the STS
	// has no --locality flag or regionCode annotation.
	CloudRegion string
	// v1alpha1InStorage controls whether SetupWithManager registers a watch on
	// v1alpha1 CrdbCluster objects. Only operator migrations need this watch.
	// Helm migrations trigger entirely through the StatefulSet watch.
	v1alpha1InStorage bool
	// Exposed for testing purposes only.
	NodeExecutor func(node *crdbv1beta1.CrdbNode, exec kubecrdbnode.Executor) kubecrdbnode.NodeExecutor
}

func NewReconciler(
	ctl *kube.Ctl, recorder record.EventRecorder, cloudRegion string, v1alpha1InStorage bool,
) *Reconciler {
	return &Reconciler{
		Ctl:               ctl,
		Recorder:          recorder,
		CloudRegion:       cloudRegion,
		v1alpha1InStorage: v1alpha1InStorage,
		NodeExecutor:      kubecrdbnode.NewNodeExecutor,
	}
}

var _ controllers.Reconciler = &Reconciler{}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctl := r.Ctl.WithNamespace(req.Namespace)

	// Discover what type of migration target we have.
	discoveryResult, err := r.discoverMigrationResources(ctx, ctl, req.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// No StatefulSet or v1alpha1 CrdbCluster found. Check if a v1beta1 CrdbCluster
			// already exists, which means migration was already completed or the cluster was
			// created directly with the CockroachDB Operator.
			var existing crdbv1beta1.CrdbCluster
			if getErr := ctl.Get(ctx, req.Name, &existing); getErr == nil {
				// Retry any handoff missing Mode, PhaseComplete, or the terminal label.
				if IsMigrationFullyComplete(&existing) {
					logging.Infof(ctx, "Migration already completed for cluster %s, nothing to do", req.Name)
					return controllers.NoRequeue()
				}
				// The source is gone, so finish from the stored v1beta1 state.
				if existing.Status.Migration != nil {
					return r.completeMigration(ctx, ctl, nil, &existing)
				}
				logging.Warnf(ctx, "No StatefulSet or v1alpha1 CrdbCluster found for %s but v1beta1 CrdbCluster exists. "+
					"Cluster may have been created directly with the CockroachDB Operator, or migration already completed. "+
					"Cannot migrate or rollback.", req.Name)
			}
			return controllers.NoRequeue()
		}
		return controllers.RequeueIfError(err)
	}

	labelValue, labelExists := getMigrationLabel(discoveryResult)
	if !labelExists {
		return r.handleNoLabel(ctx, ctl, discoveryResult)
	}

	// A source label is terminal only after the v1beta1 handoff is complete.
	if labelValue == MigrateComplete {
		if discoveryResult.StatefulSet == nil {
			var existing crdbv1beta1.CrdbCluster
			if getErr := ctl.Get(ctx, req.Name, &existing); getErr != nil {
				if !apierrors.IsNotFound(getErr) {
					return controllers.RequeueIfError(getErr)
				}
			} else if existing.Status.Migration != nil && !IsMigrationFullyComplete(&existing) {
				return r.completeMigration(ctx, ctl, discoveryResult, &existing)
			}
		}
		return controllers.NoRequeue()
	}

	// Other terminal label values set by the controller.
	if labelValue == MigrateFailed || labelValue == MigrateStopped {
		return controllers.NoRequeue()
	}

	if labelValue != MigrateStart && labelValue != MigrateInProgress && labelValue != MigrateRollback && labelValue != MigrateStop {
		logging.Warnf(ctx, "invalid migration label value: %s", labelValue)
		return controllers.NoRequeue()
	}

	// Rollback request (user sets label to rollback)
	if labelValue == MigrateRollback {
		logging.Infof(ctx, "Rollback requested via label")
		return r.handleNoLabel(ctx, ctl, discoveryResult)
	}

	if labelValue == MigrateStop {
		return r.handleStopRequest(ctx, ctl, discoveryResult)
	}

	// Update label to in-progress if needed (resume from stopped or starting fresh).
	// Skipped when the STS is already gone because finalization handles completion directly.
	if labelValue == MigrateStart && discoveryResult.StatefulSet != nil {
		if err = updateLabelValue(ctx, ctl, discoveryResult, MigrateInProgress); err != nil {
			return controllers.RequeueIfError(err)
		}
	}

	// Get or create the v1beta1 CrdbCluster (our migration target)
	cluster, err := r.getOrCreateCluster(ctx, ctl, discoveryResult)
	if err != nil {
		return controllers.RequeueIfError(err)
	}

	// Completion persists Mode, status, and the terminal label in separate requests.
	// Re-enter it after a restart or conflict until all three writes have landed.
	if cluster.Spec.Mode != nil && *cluster.Spec.Mode == crdbv1beta1.MutableOnly {
		if IsMigrationFullyComplete(cluster) {
			logging.Infof(ctx, "Migration already completed for cluster %s", req.Name)
			return controllers.NoRequeue()
		}
		logging.Infof(ctx, "Mode=MutableOnly set but migration not fully complete for %s, finishing", req.Name)
		return r.completeMigration(ctx, ctl, discoveryResult, cluster)
	}

	// Initialize migration status if needed. TotalPods and CurrentPodIndex are set
	// by handleInitPhase after StatefulSet validation, not here. Setting TotalPods
	// here would cause Init's guard to skip CurrentPodIndex initialization.
	if cluster.Status.Migration == nil {
		now := metav1.Now()
		cluster.Status.Migration = &crdbv1beta1.MigrationStatus{
			Phase:     PhaseInit,
			Type:      discoveryResult.MigrationType,
			StartTime: &now,
		}
	}

	migration := cluster.Status.Migration

	// Reconstruct TotalPods from the source of truth if it was lost. The status
	// sub-resource can be overwritten by other controllers between reconciles, so
	// TotalPods may be 0 even though Init already ran. OriginalReplicas is always
	// reliable because it comes from v1alpha1 Spec.Nodes (operator) or
	// existingCrdbNodes+STSReplicas (Helm), not from persisted status.
	if migration.TotalPods == 0 && migration.Phase != PhaseInit && migration.Phase != "" {
		migration.TotalPods = discoveryResult.OriginalReplicas
		logging.Warnf(ctx, "Reconstructed TotalPods=%d from discovery (was 0)", migration.TotalPods)
	}

	// Reconstruct MigratedNodes from actual CrdbNodes if the status was wiped. The
	// CrdbNodes themselves are never deleted during forward migration, so listing them
	// is always accurate.
	if migration.Phase != PhaseInit && migration.Phase != "" {
		actualNodes, listErr := listClusterCrdbNodes(ctx, ctl, cluster.Name)
		if listErr == nil && len(actualNodes) > len(migration.MigratedNodes) {
			migration.MigratedNodes = make([]string, 0, len(actualNodes))
			for _, node := range actualNodes {
				migration.MigratedNodes = append(migration.MigratedNodes, node.Name)
			}
			logging.Warnf(ctx, "Reconstructed MigratedNodes from actual CrdbNodes: %d", len(migration.MigratedNodes))
		}
	}

	// A user might set the label back to "start" while phase is Failed. The label was
	// already updated to "in-progress" above, so if we let the switch handle PhaseFailed
	// the label and phase drift apart. Catch it here and re-route to the failed handler.
	if migration.Phase == PhaseFailed {
		return r.handlePhaseFailed(ctx, ctl, discoveryResult, migration, req.Namespace)
	}

	if migration.Phase == PhaseStopped {
		resumeFromStopped(ctx, migration, discoveryResult)
	}

	logging.Infof(ctx, "Reconciling %s: phase=%s, migrated=%d/%d (current index=%d)",
		req.Name, migration.Phase, len(migration.MigratedNodes), migration.TotalPods, migration.CurrentPodIndex)

	r.updateMetrics(req.Name, req.Namespace, migration)

	var result ctrl.Result
	switch migration.Phase {
	case PhaseInit, "":
		result, err = r.handleInitPhase(ctx, ctl, discoveryResult, cluster)
	case PhaseCertMigration:
		result, err = r.handleCertMigrationPhase(ctx, ctl, discoveryResult, cluster)
	case PhasePodMigration:
		result, err = r.handlePodMigrationPhase(ctx, ctl, discoveryResult, cluster)
	case PhaseFinalization:
		result, err = r.handleFinalizationPhase(ctx, ctl, discoveryResult, cluster)
	// PhaseComplete in the switch handles re-entrant reconciles where a previous run
	// already recorded Complete. The post-switch check handles the case where a phase
	// handler sets PhaseComplete and returns without error.
	case PhaseComplete:
		// PhaseComplete without the other markers is a partial handoff.
		return r.completeMigration(ctx, ctl, discoveryResult, cluster)
	case PhaseFailed:
		// Migration stopped due to an unrecoverable error. Do not reconcile further.
		// User must remove the migration label to trigger rollback, or fix the issue and
		// re-add the label to retry.
		return r.handlePhaseFailed(ctx, ctl, discoveryResult, migration, req.Namespace)
	default:
		err = errors.Errorf("unknown phase: %s", migration.Phase)
	}

	// Phase handlers like finalizeHelm replace *cluster in-place, which detaches
	// the migration pointer from the live object. Re-derive it so the status
	// writes below land on the current cluster.Status.Migration.
	migration = cluster.Status.Migration
	if migration == nil {
		// should not happen, every phase handler initializes migration
		return result, err
	}

	if err != nil {
		migration.LastError = err.Error()
		logging.Errorf(ctx, "Migration step failed: %v", err)
		MigrationErrorsTotal.WithLabelValues(req.Name, req.Namespace, migration.Phase, "error").Inc()
		result, _ = controllers.RequeueAfter(30*time.Second, nil)
	} else {
		migration.LastError = ""
	}

	// Capture the fields we want to persist so the mutator can re-apply them on
	// retry. persistMigrationStatus re-fetches the cluster on conflict and
	// re-initializes migration if another controller (e.g. the public operator)
	// wiped it between our fetch and update.
	phase := migration.Phase
	lastErr := migration.LastError
	msg := migration.Message
	totalPods := migration.TotalPods
	migratedNodes := migration.MigratedNodes
	currentPodIdx := migration.CurrentPodIndex
	migrationType := migration.Type
	if err = persistMigrationStatus(ctx, ctl, cluster, func(m *crdbv1beta1.MigrationStatus) {
		m.Phase = phase
		m.LastError = lastErr
		m.Message = msg
		m.TotalPods = totalPods
		m.MigratedNodes = migratedNodes
		m.CurrentPodIndex = currentPodIdx
		m.LastUpdateTime = &metav1.Time{Time: time.Now()}
	}, phase, migrationType); err != nil {
		logging.Errorf(ctx, "Failed to persist migration status: %v", err)
		return controllers.RequeueAfter(5*time.Second, errors.Wrap(err, "failed to persist migration status"))
	}

	// Only Helm keeps its trigger label on the source after completion.
	if migration.Phase == PhaseComplete && migration.Type == MigrationTypeHelm {
		logging.Info(ctx, "Updating migration label to 'complete'")
		if err := updateLabelValue(ctx, ctl, discoveryResult, MigrateComplete); err != nil {
			logging.Warnf(ctx, "Failed to update label to complete (non-fatal): %v", err)
		}
	}

	return result, nil
}

// IsMigrationFullyComplete reports whether all three handoff writes have landed.
func IsMigrationFullyComplete(cluster *crdbv1beta1.CrdbCluster) bool {
	return cluster.IsManagedMode() &&
		cluster.Status.Migration != nil && cluster.Status.Migration.Phase == PhaseComplete &&
		cluster.Labels[LabelMigrate] == MigrateComplete
}

// needsMigrationCompletionRecovery identifies handoffs that migrationctrl must
// resume from the persisted v1beta1 state. This supplies a durable trigger after
// the source StatefulSet is deleted, including when the operator restarts.
func needsMigrationCompletionRecovery(cluster *crdbv1beta1.CrdbCluster) bool {
	migration := cluster.Status.Migration
	if migration == nil {
		return false
	}
	if migration.Phase != PhaseFinalization && migration.Phase != PhaseComplete {
		return false
	}
	return !IsMigrationFullyComplete(cluster)
}

func migrationHandoffMarkersChanged(oldCluster, newCluster *crdbv1beta1.CrdbCluster) bool {
	oldPhase, newPhase := "", ""
	if oldCluster.Status.Migration != nil {
		oldPhase = oldCluster.Status.Migration.Phase
	}
	if newCluster.Status.Migration != nil {
		newPhase = newCluster.Status.Migration.Phase
	}
	return oldCluster.IsManagedMode() != newCluster.IsManagedMode() ||
		oldPhase != newPhase ||
		oldCluster.Labels[LabelMigrate] != newCluster.Labels[LabelMigrate]
}

// migrationResourceKind returns the Kubernetes resource kind string for a given migration type,
// used to construct user-facing kubectl commands in log messages.
func migrationResourceKind(migrationType string) string {
	if migrationType == MigrationTypeOperator {
		return "crdbcluster"
	}
	return "statefulset"
}

// handlePhaseFailed logs the terminal failure state, updates the label to "failed", and stops
// reconciliation. The user must remove the label to trigger rollback or retry migration.
func (r *Reconciler) handlePhaseFailed(
	ctx context.Context,
	ctl *kube.Ctl,
	discoveryResult *DiscoveryResult,
	migration *crdbv1beta1.MigrationStatus,
	namespace string,
) (ctrl.Result, error) {
	logging.Errorf(ctx, "Migration stopped in failed state: %s", migration.Message)
	if migration.LastError != "" {
		logging.Errorf(ctx, "Last error: %s", migration.LastError)
	}
	kind := migrationResourceKind(discoveryResult.MigrationType)
	logging.Infof(ctx, "To rollback: kubectl label %s/%s crdb.io/migrate- -n %s", kind, discoveryResult.Name, namespace)
	if err := updateLabelValue(ctx, ctl, discoveryResult, MigrateFailed); err != nil {
		logging.Warnf(ctx, "Failed to update label to failed: %v", err)
	}
	return controllers.NoRequeue()
}

// handleNoLabel handles the case when the migration label is removed.
// This triggers rollback if migration is in progress or is a no-op if complete.
func (r *Reconciler) handleNoLabel(
	ctx context.Context, ctl *kube.Ctl, discovery *DiscoveryResult,
) (ctrl.Result, error) {
	// Check if the v1beta1 cluster exists and has migration status
	var cluster crdbv1beta1.CrdbCluster
	if err := ctl.Get(ctx, discovery.Name, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return controllers.NoRequeue()
		}
		return controllers.RequeueIfError(err)
	}

	if cluster.Status.Migration == nil {
		// Status may have been lost due to a concurrent controller overwriting the
		// status sub-resource. If CrdbNodes exist, we still need to roll them back.
		// handleRollback checks for orphaned CrdbNodes and initializes status if needed.
		logging.Warnf(ctx, "Migration status is nil for cluster %s, checking for orphaned CrdbNodes", discovery.Name)
		return r.handleRollback(ctx, ctl, discovery, &cluster)
	}

	phase := cluster.Status.Migration.Phase
	if phase == PhaseComplete {
		// Migration successfully completed, nothing to do.
		return controllers.NoRequeue()
	}

	// Check if rollback is in progress or completed
	if phase == PhaseRollbackInProgress || phase == PhaseRollbackComplete {
		// Rollback already in progress, continue rollback handling.
		logging.Infof(ctx, "Continuing rollback from phase: %s", phase)
		return r.handleRollback(ctx, ctl, discovery, &cluster)
	}

	// Migration in progress, triggering rollback.
	logging.Infof(ctx, "Label removed, initiating rollback from phase: %s", phase)
	return r.handleRollback(ctx, ctl, discovery, &cluster)
}

// getOrCreateCluster gets the existing v1beta1 CrdbCluster or creates a minimal one for Helm
// migrations. For operator migrations, the CrdbCluster must already exist via the conversion
// webhook. For Helm migrations, a minimal CrdbCluster is created with Mode=Disabled so that
// migration status can be persisted under CrdbCluster.Status.Migration. The full spec is
// populated during Finalization.
func (r *Reconciler) getOrCreateCluster(
	ctx context.Context, ctl *kube.Ctl, discovery *DiscoveryResult,
) (*crdbv1beta1.CrdbCluster, error) {
	var cluster crdbv1beta1.CrdbCluster
	if err := ctl.Get(ctx, discovery.Name, &cluster); err == nil {
		return &cluster, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}

	// For operator migrations, CrdbCluster must exist via conversion webhook.
	if discovery.MigrationType == MigrationTypeOperator {
		return nil, errors.Errorf("CrdbCluster %s not found, expected via conversion webhook for operator migration", discovery.Name)
	}

	// Helm migration: create a webhook-valid CrdbCluster with Mode=Disabled for status
	// tracking. The validating webhook still runs on create, so the object must carry
	// a real Regions/DataStore spec even though no controller will reconcile it yet.
	if discovery.StatefulSet == nil {
		return nil, errors.New("StatefulSet is required to create Helm migration CrdbCluster")
	}
	stsCtx, err := extractContextFromSTS(discovery.StatefulSet)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse StatefulSet for Helm migration CrdbCluster")
	}

	// Helm charts store log config in a Secret, but the operator expects a ConfigMap.
	// The webhook validates the ConfigMap exists on CrdbCluster create, so we convert
	// it before the Create call below.
	if err := convertLogConfigSecretToConfigMap(ctx, ctl, stsCtx.LoggingConfigMap, discovery.Namespace); err != nil {
		return nil, errors.Wrap(err, "failed to convert log config Secret to ConfigMap")
	}

	mode := crdbv1beta1.Disabled
	cluster = crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      discovery.Name,
			Namespace: discovery.Namespace,
			Labels: map[string]string{
				LabelMigrationStatus: MigrateInProgress,
			},
		},
		Spec: generateV1Beta1ClusterSpec(ctx, discovery.StatefulSet, stsCtx, discovery.OriginalReplicas, r.CloudRegion),
	}
	cluster.Spec.Mode = &mode
	if err := ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, &cluster); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// race: another reconcile created it
			if err := ctl.Get(ctx, discovery.Name, &cluster); err != nil {
				return nil, err
			}
			return &cluster, nil
		}
		return nil, errors.Wrap(err, "failed to create CrdbCluster for Helm migration")
	}
	logging.Infof(ctx, "Created minimal CrdbCluster (Mode=Disabled) for Helm migration status tracking")
	return &cluster, nil
}

// handleStopRequest pauses migration when the user sets the label to MigrateStop.
// It transitions the migration status to PhaseStopped, persists the status sub-resource
// before returning, and updates the label to MigrateStopped so users see a clear terminal state.
// Stopping during rollback is supported. The user can resume rollback by setting the
// label back to "rollback", which re-enters handleRollback from PhaseStopped.
func (r *Reconciler) handleStopRequest(
	ctx context.Context, ctl *kube.Ctl, discoveryResult *DiscoveryResult,
) (ctrl.Result, error) {
	logging.Infof(ctx, "Stop requested via label - pausing migration")

	cluster, err := r.getOrCreateCluster(ctx, ctl, discoveryResult)
	if err != nil {
		return controllers.RequeueIfError(err)
	}

	// Migration status may be nil if another controller wiped it between reconciles.
	// Reconstruct from actual CrdbNodes and discovery's OriginalReplicas.
	if cluster.Status.Migration == nil {
		logging.Warnf(ctx, "Migration status is nil during stop request, reconstructing from cluster state")

		var migratedNodes []string
		actualNodes, listErr := listClusterCrdbNodes(ctx, ctl, cluster.Name)
		if listErr != nil {
			logging.Warnf(ctx, "Failed to list CrdbNodes for status reconstruction: %v", listErr)
		} else {
			for _, node := range actualNodes {
				migratedNodes = append(migratedNodes, node.Name)
			}
		}

		cluster.Status.Migration = &crdbv1beta1.MigrationStatus{
			Phase:           PhaseStopped,
			Type:            discoveryResult.MigrationType,
			TotalPods:       discoveryResult.OriginalReplicas,
			CurrentPodIndex: highestUnmigratedPodIndex(discoveryResult.Name, discoveryResult.OriginalReplicas, migratedNodes),
			MigratedNodes:   migratedNodes,
			Message: fmt.Sprintf("Migration paused by user (status reconstructed, %d/%d nodes found). "+
				"Set label to 'start' to resume, or remove label to rollback.",
				len(migratedNodes), discoveryResult.OriginalReplicas),
		}
	} else if cluster.Status.Migration.Phase != PhaseStopped {
		previousPhase := cluster.Status.Migration.Phase
		// Use OriginalReplicas if TotalPods was wiped.
		totalPods := cluster.Status.Migration.TotalPods
		if totalPods == 0 {
			totalPods = discoveryResult.OriginalReplicas
		}
		cluster.Status.Migration.TotalPods = totalPods

		// If we're stopping during pod migration and the STS was scaled down
		// without the CrdbNode being created, the cluster is one node short.
		// Scale the STS back up so the cluster stays healthy during the pause.
		// We check the actual CrdbNode resource, not the status field, because
		// the CrdbNode might have been created but MigratedNodes not yet updated.
		if previousPhase == PhasePodMigration {
			if gapErr := r.restoreGapNodeOnStop(ctx, ctl, discoveryResult, cluster.Status.Migration); gapErr != nil {
				return controllers.RequeueAfter(5*time.Second, errors.Wrap(gapErr, "gap node restoration failed, retrying before persisting stop"))
			}
		}

		// During rollback the gap is the inverse: rollbackNextNode deletes a
		// CrdbNode and then requeues to scale the STS up. If a stop arrives
		// between delete and scale-up, the cluster is one node short.
		if previousPhase == PhaseRollbackInProgress {
			if gapErr := r.restoreRollbackGapOnStop(ctx, ctl, discoveryResult, cluster.Status.Migration); gapErr != nil {
				return controllers.RequeueAfter(5*time.Second, errors.Wrap(gapErr, "rollback gap restoration failed, retrying before persisting stop"))
			}
		}

		cluster.Status.Migration.Phase = PhaseStopped
		migratedCount := len(cluster.Status.Migration.MigratedNodes)

		// rollback resumes via the "rollback" label, forward migration via "start"
		resumeHint := "Set label to 'start' to resume, or remove label to rollback."
		if previousPhase == PhaseRollbackInProgress {
			resumeHint = "Set label to 'rollback' to resume rollback, or 'start' to resume forward migration."
		}
		cluster.Status.Migration.Message = fmt.Sprintf("Migration paused by user (was in %s, %d/%d pods migrated). %s",
			previousPhase, migratedCount, totalPods, resumeHint)
		logging.Infof(ctx, "Migration paused from phase %s (%d/%d pods migrated)", previousPhase, migratedCount, totalPods)
	}

	// Persist PhaseStopped before returning. Without this, an operator restart loses the phase
	// because NoRequeue() exits before the end-of-Reconcile status update runs.
	// Capture the state we want to persist so the mutator can re-apply it on retry.
	stoppedMsg := cluster.Status.Migration.Message
	stoppedTotal := cluster.Status.Migration.TotalPods
	stoppedMigrated := cluster.Status.Migration.MigratedNodes
	stoppedPodIndex := cluster.Status.Migration.CurrentPodIndex
	if err = persistMigrationStatus(ctx, ctl, cluster, func(m *crdbv1beta1.MigrationStatus) {
		m.Phase = PhaseStopped
		m.Message = stoppedMsg
		m.TotalPods = stoppedTotal
		m.MigratedNodes = stoppedMigrated
		m.CurrentPodIndex = stoppedPodIndex
	}, PhaseStopped, discoveryResult.MigrationType); err != nil {
		return controllers.RequeueAfter(5*time.Second, errors.Wrap(err, "failed to persist PhaseStopped status"))
	}

	if err = updateLabelValue(ctx, ctl, discoveryResult, MigrateStopped); err != nil {
		logging.Warnf(ctx, "Failed to update label to stopped: %v", err)
	}
	return controllers.NoRequeue()
}

// restoreGapNodeOnStop detects and fixes the gap between STS scale-down and
// CrdbNode creation during a user-initiated stop. If the STS was scaled down
// for the current target but the CrdbNode was never created, the cluster is
// running with one fewer node than expected. Scaling the STS back up restores
// the pod so the cluster stays healthy during the pause. Returns an error if
// the gap was detected but the STS could not be restored, so the caller can
// requeue instead of persisting PhaseStopped with a degraded cluster.
func (r *Reconciler) restoreGapNodeOnStop(
	ctx context.Context,
	ctl *kube.Ctl,
	discovery *DiscoveryResult,
	migration *crdbv1beta1.MigrationStatus,
) error {
	if discovery.StatefulSet == nil || migration.CurrentPodIndex < 0 || migration.CurrentPodIndex >= migration.TotalPods {
		return nil
	}
	sts := discovery.StatefulSet
	targetName := fmt.Sprintf("%s-%d", sts.Name, migration.CurrentPodIndex)

	var targetNode crdbv1beta1.CrdbNode
	if err := ctl.Get(ctx, targetName, &targetNode); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "check CrdbNode %s during stop", targetName)
	}

	// CrdbNode doesn't exist. Re-fetch STS for current replica count.
	if err := ctl.Get(ctx, sts.Name, sts); err != nil {
		return errors.Wrap(err, "re-fetch STS for gap detection")
	}
	currentReplicas := int32(0)
	if sts.Spec.Replicas != nil {
		currentReplicas = *sts.Spec.Replicas
	}

	// If the STS was already scaled down past the target index, the pod at
	// that index is gone and no CrdbNode replaced it. Scale up to at least
	// CurrentPodIndex+1 so the target pod is restored even if replicas
	// dropped further than expected.
	if currentReplicas <= migration.CurrentPodIndex {
		restored := max(currentReplicas+1, migration.CurrentPodIndex+1)
		logging.Infof(ctx, "Scaling STS from %d to %d to restore pod %s during stop (CrdbNode was never created)",
			currentReplicas, restored, targetName)
		sts.Spec.Replicas = &restored
		if err := ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, sts); err != nil {
			return errors.Wrapf(err, "restore STS pod %s during stop", targetName)
		}
	}
	return nil
}

// restoreRollbackGapOnStop handles the inverse of restoreGapNodeOnStop. During
// rollback, rollbackNextNode deletes a CrdbNode and requeues to scale the STS
// up on the next reconcile. If a stop arrives between the delete and the
// scale-up, the cluster is one node short. This function detects that gap,
// scales the STS up, and idempotently removes stale entries from MigratedNodes.
//
// The idempotent cleanup is important for crash recovery. If the operator
// crashes after scaling the STS but before persisting PhaseStopped, the next
// reconcile sees the already-scaled STS and won't detect a gap to fix. Without
// the cleanup pass, stale entries whose CrdbNodes were deleted would persist
// in MigratedNodes indefinitely.
func (r *Reconciler) restoreRollbackGapOnStop(
	ctx context.Context,
	ctl *kube.Ctl,
	discovery *DiscoveryResult,
	migration *crdbv1beta1.MigrationStatus,
) error {
	if discovery.StatefulSet == nil || len(migration.MigratedNodes) == 0 {
		return nil
	}
	sts := discovery.StatefulSet

	if err := ctl.Get(ctx, sts.Name, sts); err != nil {
		return errors.Wrap(err, "re-fetch STS for rollback gap detection")
	}
	currentReplicas := int32(0)
	if sts.Spec.Replicas != nil {
		currentReplicas = *sts.Spec.Replicas
	}

	// If the node at currentReplicas (the next STS index) is in MigratedNodes
	// but its CrdbNode was already deleted, no pod exists at that index. Scale
	// the STS up to restore it.
	targetName := fmt.Sprintf("%s-%d", sts.Name, currentReplicas)
	if slices.Contains(migration.MigratedNodes, targetName) {
		var targetNode crdbv1beta1.CrdbNode
		if err := ctl.Get(ctx, targetName, &targetNode); err == nil {
			// CrdbNode still exists, no gap to fix
		} else if !apierrors.IsNotFound(err) {
			return errors.Wrapf(err, "check CrdbNode %s during rollback stop", targetName)
		} else {
			restored := currentReplicas + 1
			logging.Infof(ctx, "Scaling STS from %d to %d to restore pod %s during rollback stop (CrdbNode was deleted but STS not yet scaled up)",
				currentReplicas, restored, targetName)
			sts.Spec.Replicas = &restored
			if err := ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, sts); err != nil {
				return errors.Wrapf(err, "restore STS pod %s during rollback stop", targetName)
			}
			currentReplicas = restored
		}
	}

	// Even when no gap was detected above, stale MigratedNodes entries may
	// remain from a previous run that scaled the STS but crashed before
	// persisting the cleaned list. Remove any entry whose ordinal is below
	// currentReplicas (STS-managed) and whose CrdbNode no longer exists.
	prefix := sts.Name + "-"
	cleaned := make([]string, 0, len(migration.MigratedNodes))
	for _, name := range migration.MigratedNodes {
		if !strings.HasPrefix(name, prefix) {
			cleaned = append(cleaned, name)
			continue
		}
		ordinal, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
		if err != nil {
			cleaned = append(cleaned, name)
			continue
		}
		if int32(ordinal) >= currentReplicas {
			cleaned = append(cleaned, name)
			continue
		}
		// ordinal is below currentReplicas so the STS covers this index,
		// but only remove if the CrdbNode was actually deleted
		var node crdbv1beta1.CrdbNode
		if getErr := ctl.Get(ctx, name, &node); getErr == nil {
			cleaned = append(cleaned, name)
		} else if apierrors.IsNotFound(getErr) {
			logging.Infof(ctx, "Removing stale MigratedNodes entry %s (ordinal %d < currentReplicas %d, CrdbNode deleted)",
				name, ordinal, currentReplicas)
		} else {
			return errors.Wrapf(getErr, "check CrdbNode %s during rollback stop cleanup", name)
		}
	}
	migration.MigratedNodes = cleaned
	return nil
}

// resumeFromStopped advances the migration phase after a user-initiated pause.
// The target phase is determined by how far Init got and how many nodes have been migrated.
// TotalPods is reconstructed from discovery if it was lost, so this function never
// falls back to PhaseInit due to a status wipe.
func resumeFromStopped(
	ctx context.Context, migration *crdbv1beta1.MigrationStatus, discovery *DiscoveryResult,
) {
	logging.Infof(ctx, "Resuming migration from stopped state")

	// Reconstruct TotalPods from discovery if it was lost between stop and resume.
	if migration.TotalPods == 0 {
		migration.TotalPods = discovery.OriginalReplicas
		logging.Warnf(ctx, "Reconstructed TotalPods=%d from discovery during resume", migration.TotalPods)
	}

	switch {
	case migration.TotalPods == 0:
		// OriginalReplicas was also 0, which means no STS and no v1alpha1 spec.
		// Re-enter Init to establish prerequisites.
		migration.Phase = PhaseInit
		migration.Message = "Resuming from init (could not determine pod count)"
	case len(migration.MigratedNodes) == 0:
		migration.Phase = PhaseCertMigration
		// No nodes migrated yet, so migration will start from the highest index
		// once cert migration completes. Set CurrentPodIndex now so it's correct
		// when PhasePodMigration begins (handleCertMigrationPhase does not set it).
		migration.CurrentPodIndex = migration.TotalPods - 1
		migration.Message = "Resuming from certificate migration"
	case len(migration.MigratedNodes) < int(migration.TotalPods):
		migration.Phase = PhasePodMigration
		migration.CurrentPodIndex = highestUnmigratedPodIndex(discovery.Name, migration.TotalPods, migration.MigratedNodes)
		migration.Message = fmt.Sprintf("Resuming pod migration (%d/%d complete)", len(migration.MigratedNodes), migration.TotalPods)
	default:
		migration.Phase = PhaseFinalization
		migration.Message = "Resuming finalization"
	}
	logging.Infof(ctx, "Resumed to phase: %s", migration.Phase)
}

func (r *Reconciler) SetTestMode(inTestMode bool) {
	r.inTestMode = inTestMode
}

func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	migrationLabelChanged := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			val, ok := e.Object.GetLabels()[LabelMigrate]
			return ok && (val == MigrateStart || val == MigrateInProgress || val == MigrateRollback || val == MigrateStop)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldVal, oldOk := e.ObjectOld.GetLabels()[LabelMigrate]
			newVal, newOk := e.ObjectNew.GetLabels()[LabelMigrate]

			// Trigger on label added, removed, or changed
			if !oldOk && newOk && (newVal == MigrateStart || newVal == MigrateRollback || newVal == MigrateStop) {
				return true
			}
			if oldOk && !newOk {
				return true // label removed, potential rollback
			}
			if oldOk && newOk && oldVal != newVal {
				return true
			}
			return false
		},
		// Reconcile deletes for any CockroachDB StatefulSet, not just those carrying the
		// migration label. For operator migrations the label lives on the v1alpha1
		// CrdbCluster, so the STS itself won't have it. The reconcile logic guards
		// against false positives: completeMigration requires an existing MigrationStatus,
		// and the PhaseComplete early return at the top of Reconcile prevents re-processing.
		DeleteFunc: func(e event.DeleteEvent) bool {
			labels := e.Object.GetLabels()
			if _, ok := labels[LabelMigrate]; ok {
				return true
			}
			// operator-managed STS won't have the migration label but we still
			// need a reconcile when it's deleted to complete the migration
			return labels["app.kubernetes.io/name"] == "cockroachdb"
		},
	}
	completionRecovery := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			cluster, ok := e.Object.(*crdbv1beta1.CrdbCluster)
			return ok && needsMigrationCompletionRecovery(cluster)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldCluster, oldOK := e.ObjectOld.(*crdbv1beta1.CrdbCluster)
			newCluster, newOK := e.ObjectNew.(*crdbv1beta1.CrdbCluster)
			return oldOK && newOK && needsMigrationCompletionRecovery(newCluster) &&
				migrationHandoffMarkersChanged(oldCluster, newCluster)
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
	enqueueByName := handler.EnqueueRequestsFromMapFunc(func(obj client.Object) []reconcile.Request {
		return []reconcile.Request{
			{NamespacedName: types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}},
		}
	})

	// controller-runtime only allows a single primary object via For(...).
	// We reconcile based on the migration label which can live on either a
	// StatefulSet (Helm migration) or a v1alpha1 CrdbCluster (operator migration).
	//
	// We pick StatefulSet as the primary object. The v1alpha1 watch is only added
	// when v1alpha1 is in storedVersions and served by the API server. If v1alpha1
	// is not served, the watch would fail to sync and the manager would crash on
	// startup after the cache sync timeout. The v1beta1 watch is always available,
	// but its predicate only admits incomplete completion handoffs.
	b := ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.StatefulSet{}, builder.WithPredicates(migrationLabelChanged)).
		Watches(
			&source.Kind{Type: &crdbv1beta1.CrdbCluster{}},
			enqueueByName,
			builder.WithPredicates(completionRecovery),
		)

	if r.v1alpha1InStorage {
		b = b.Watches(
			&source.Kind{Type: &crdbv1alpha1.CrdbCluster{}},
			enqueueByName,
			builder.WithPredicates(migrationLabelChanged),
		)
	}

	return b.Complete(r)
}

// updateMetrics updates Prometheus metrics for the migration.
func (r *Reconciler) updateMetrics(name, namespace string, migration *crdbv1beta1.MigrationStatus) {
	if migration == nil {
		return
	}

	phase := migration.Phase
	if phase == "" {
		phase = PhaseInit
	}

	// Set phase gauge (includes all possible phases)
	for _, p := range []string{PhaseInit, PhaseCertMigration, PhasePodMigration, PhaseFinalization, PhaseComplete, PhaseFailed, PhaseStopped, PhaseRollbackInProgress, PhaseRollbackComplete} {
		var value float64
		if p == phase {
			value = 1
		}
		MigrationPhaseGauge.WithLabelValues(name, namespace, p).Set(value)
	}

	// Set progress gauges
	MigrationPodsTotal.WithLabelValues(name, namespace).Set(float64(migration.TotalPods))
	MigrationPodsMigrated.WithLabelValues(name, namespace).Set(float64(len(migration.MigratedNodes)))

	// Only record duration when migration reaches a terminal phase. Observing on
	// every reconcile would flood the histogram with intermediate samples.
	if migration.StartTime != nil && (phase == PhaseComplete || phase == PhaseFailed || phase == PhaseRollbackComplete) {
		duration := time.Since(migration.StartTime.Time).Seconds()
		MigrationDurationSeconds.WithLabelValues(name, namespace, phase).Observe(duration)
	}
}

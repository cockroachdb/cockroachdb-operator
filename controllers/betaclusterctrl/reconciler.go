package betaclusterctrl

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1alpha1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1alpha1"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/certs"
	opmetrics "github.com/cockroachlabs/cockroach-enterprise-operator/controllers/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/migrationctrl"
	"github.com/cockroachlabs/cockroach-enterprise-operator/crypto"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/crdbcerts"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubenet"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/evictionutil"
	"github.com/cockroachlabs/version"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
)

// +kubebuilder:rbac:groups=crdb.cockroachlabs.com,resources=crdbclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=crdb.cockroachlabs.com,resources=crdbclusters/status,verbs=get;update;patch

// Reconciler is the primary controller for CrdbCluster resources. It manages
// the full lifecycle: deploying Kubernetes resources (services, PDB, priority
// classes), initializing the CockroachDB cluster, performing rolling updates,
// scaling, PVC resizing, and applying cluster settings.
//
// During an active migration (managed by migrationctrl), this controller defers
// most operations and only deploys the join service. It takes full ownership only
// after migrationctrl has persisted Mode=MutableOnly, PhaseComplete, and the
// terminal migrate label.
//
// This controller was originally created separately from the legacy clusterctrl
// to isolate multi-tenancy work from the production controller. It has since
// become the primary reconciler for all CrdbCluster resources. For the original
// design, see docs/design/crdbnode-operator.md.
type Reconciler struct {
	Ctl *kube.Ctl

	Env            *cloud.Environment
	EvictionHelper *evictionutil.Helper
	Emit           kube.EventEmitter

	// Exposed for testing purposes only.
	NodeExecutor func(node *crdbv1beta1.CrdbNode, exec kubecrdbnode.Executor) kubecrdbnode.NodeExecutor

	// Exposed for testing purposes only.
	VersionCheckerJob func(ctx context.Context, ctl *kube.Ctl, jobName string) VersionCheckerJob

	// inTestMode means that the reconciler is being run in testing mode, where
	// it runs in the same process as unit tests. In this mode, copyist and/or
	// kuberecord might be active.
	//
	// NOTE: Try not to modify the behavior of the reconciler based on this
	// flag; only use inTestMode when there's no other way of using the same
	// code paths for both testing and production.
	inTestMode bool
}

var _ controllers.Reconciler = &Reconciler{}

func NewReconciler(
	env *cloud.Environment, ctl *kube.Ctl, evictor *evictionutil.Helper,
) *Reconciler {
	return &Reconciler{
		Env:               env,
		Ctl:               ctl,
		EvictionHelper:    evictor,
		NodeExecutor:      kubecrdbnode.NewNodeExecutor,
		VersionCheckerJob: NewVersionCheckerJob,
	}
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctl := r.Ctl.WithNamespace(req.Namespace)

	// Get the current state of the cluster CRD.
	var cluster crdbv1beta1.CrdbCluster
	if err := ctl.Get(ctx, req.Name, &cluster); err != nil {
		// If resource cannot be found, it's been deleted, so don't requeue.
		if kubeerrors.IsNotFound(err) {
			return controllers.NoRequeue()
		}
		logging.Errorf(ctx, "failed to retrieve CrdbCluster resource: %+v", err)
		return controllers.RequeueIfError(err)
	}
	return r.ReconcileCluster(ctx, ctl, &cluster)
}

func (r *Reconciler) deploy(
	ctx context.Context,
	ctl *kube.Ctl,
	cluster *crdbv1beta1.CrdbCluster,
	objects []kube.Object,
	includePDB bool,
) error {

	// Only apply the PDB when not in an active migration phase. During migration, the migration
	// controller manages the PDB. Patching it here every 10 seconds races with the K8s disruption
	// controller's status updates. The disruption controller's conditional writes lose to our
	// patches, leaving stale currentHealthy counts that block pod evictions.
	thisRegion := cluster.Region()
	if includePDB && thisRegion != nil {
		budget := 1
		if cluster.Spec.DisruptionBudget != nil {
			budget = *cluster.Spec.DisruptionBudget
		}
		pdb := manifests.CrdbClusterOperatorPDB(cluster, int(thisRegion.Nodes), budget)
		objects = append(objects, pdb)
	}

	ownerRef, err := kube.OwnerReferenceFor(ctl.Scheme(), cluster)
	if err != nil {
		return errors.WithStack(err)
	}

	tv := true
	ownerRef.Controller = &tv

	// PriorityClass is cluster-scoped and shared across every CrdbCluster,
	// unlike the per-namespace PDB. Multiple CrdbClusters would race to claim
	// ownership and flap the owner ref on every reconcile. Skip unconditionally
	// so single-cluster installs stay consistent with multi-cluster.
	for _, obj := range objects {
		if _, isPriorityClass := obj.(*schedulingv1.PriorityClass); isPriorityClass {
			obj.SetOwnerReferences(nil)
			continue
		}
		obj.SetOwnerReferences([]metav1.OwnerReference{ownerRef})
	}

	if err = ctl.ApplyWithOptions(
		ctx,
		kube.ApplyOptions{
			NoWait: true,
		},
		objects...,
	); err != nil {
		return errors.Wrap(err, "failed to deploy cluster objects")
	}

	// Clean up legacy PDB if it exists to avoid the "multiple PDBs" error.
	// We do this after applying the new PDB to ensure the cluster is never left unprotected.
	if thisRegion != nil && cluster.Name != "cockroachdb" {
		legacyPDBName := "cockroachdb-pdb"
		legacyPDB := &policyv1.PodDisruptionBudget{}
		err = ctl.Get(ctx, legacyPDBName, legacyPDB)
		if err == nil {
			// It exists, delete it
			logging.Info(ctx, "Deleting legacy PDB cockroachdb-pdb to allow migration to scoped PDB")
			if err = ctl.Delete(ctx, legacyPDB); err != nil {
				return errors.Wrap(err, "failed to delete legacy PDB")
			}
		} else if !kubeerrors.IsNotFound(err) {
			return errors.Wrap(err, "failed to check for legacy PDB")
		}
	}

	return nil
}

func (r *Reconciler) ReconcileCluster(
	ctx context.Context, ctl *kube.Ctl, cluster *crdbv1beta1.CrdbCluster,
) (ctrl.Result, error) {
	// Seed Status.Region from the operator env so c.Region() and c.Domain()
	// resolve correctly for every code path in this method. The self-signed cert
	// generator below needs the right domain on the first reconcile because the
	// cert is never regenerated on domain changes (see certs/self_signed_certs.go).
	// updateStatus later in this method sets the same value again.
	cluster.Status.Region = r.Env.CloudRegion

	// Mode lands before status and the terminal label. Normal reconciliation must wait
	// for all three or it can race migrationctrl's remaining writes.
	handoffComplete := MigrationHandoffComplete(cluster)
	deployJoinServiceOnly := false
	if cluster.Status.Migration != nil && !handoffComplete {
		phase := cluster.Status.Migration.Phase
		switch phase {
		case migrationctrl.PhaseInit, migrationctrl.PhaseCertMigration, migrationctrl.PhasePodMigration, migrationctrl.PhaseFinalization:
			logging.Infof(ctx, "During migration phase %s: will create join service only", phase)
			deployJoinServiceOnly = true
		case migrationctrl.PhaseRollbackInProgress, migrationctrl.PhaseRollbackComplete:
			logging.Infof(ctx, "Skipping v1beta1 deployment during rollback phase: %s", phase)
		default:
			// migrationctrl still owns every phase until the terminal label lands
		}
	}

	// When mode is Disabled and no migration has started, the v1alpha1 operator owns the
	// cluster completely. Skip all reconcile work — the migration controller requeues us
	// when migration begins by updating Status.Migration.
	if cluster.Spec.Mode != nil && *cluster.Spec.Mode == crdbv1beta1.Disabled && cluster.Status.Migration == nil {
		return controllers.NoRequeue()
	}

	// The conversion webhook injects migration annotations during active migration.
	// Once migration completes, strip them so they do not cause SSA field ownership
	// conflicts on Helm upgrade. This block must stay before updateStatus because
	// UpdateWithOptions resets any in-memory status changes.
	if handoffComplete {
		if cleaned := cleanMigrationAnnotations(cluster); cleaned {
			logging.Infof(ctx, "Removing stale migration annotations from cluster %s", cluster.Name)
			// UpdateWithOptions refreshes cluster with the API response, including the
			// new ResourceVersion, so the rest of reconciliation works with a fresh object.
			if err := ctl.UpdateWithOptions(ctx, kube.UpdateOptions{
				NoWait:                  true,
				NoLastAppliedAnnotation: true,
			}, cluster); err != nil {
				return controllers.RequeueIfError(errors.Wrap(err, "failed to remove migration annotations"))
			}
		}
	}

	// Return before node, annotation, or status writes can race the handoff.
	// migrationctrl ensures the enterprise PDB before setting Mode=MutableOnly.
	if cluster.Status.Migration != nil && !handoffComplete {
		if deployJoinServiceOnly {
			if err := r.deploy(ctx, ctl, cluster, []kube.Object{manifests.JoinService(cluster)}, false); err != nil {
				return controllers.RequeueIfError(err)
			}
		}
		return controllers.NoRequeue()
	}

	// Normal path: deploy the full set of resources including NodeService, PDB,
	// and PriorityClasses.
	deployObjects := append([]kube.Object{
		manifests.NodeService(cluster),
		manifests.JoinService(cluster),
	},
		manifests.CrdbClusterPriorityClasses()...,
	)

	if cluster.Spec.Template.Spec.Certificates.SelfSigned {
		joinNamespaces := make([]string, len(cluster.Spec.Regions))
		for i, region := range cluster.Spec.Regions {
			joinNamespaces[i] = manifests.RegionJoinNamespace(cluster.Namespace, region)
		}
		certGenerator := certs.NewSelfSignedCertGenerator(ctl, cluster.Namespace, cluster.Domain(), cluster.Name).
			WithOwner(cluster).
			WithJoinNamespaces(joinNamespaces)
		if err := certGenerator.GenerateCrdbCerts(ctx); err != nil {
			return controllers.RequeueIfError(errors.Wrap(err, "failed to generate CockroachDB certificates"))
		}
	}

	if err := r.deploy(ctx, ctl, cluster, deployObjects, true); err != nil {
		return controllers.RequeueIfError(err)
	}

	if cluster.Spec.IsClusterDisrupted {
		opmetrics.IsClusterDisrupted.Set(1)
	} else {
		opmetrics.IsClusterDisrupted.Set(0)
	}

	// Report currently enabled/disabled features for better telemetry
	// on rollouts.
	reportFeatureMetrics(cluster)

	var nodeList crdbv1beta1.CrdbNodeList
	if err := ctl.List(ctx, &kube.ListOptions{
		Namespace:     cluster.Namespace,
		LabelSelector: nodeLabelSelectorForCluster(cluster),
	}, &nodeList); err != nil {
		logging.Errorf(ctx, "failed to list CrdbNodes: %+v", err)
		return controllers.RequeueIfError(err)
	}

	for i := range nodeList.Items {
		node := &nodeList.Items[i]

		// Skip nodes that are currently updating to avoid race conditions
		if node.Status.HasCondition(crdbv1beta1.PodNeedsUpdate, metav1.ConditionTrue) {
			continue
		}

		// Handle pending decommission nodes
		// These are the non-running nodes that are prioritized during scaling down.
		if node.IsMarkedForPendingDecommission() {
			return r.handlePendingDecommissionNode(ctx, node)
		}

		// Handle fully decommissioned nodes
		if node.Status.Decommission == crdbv1beta1.Decommissioned {
			return r.handleDecommissionedNode(ctx, node)
		}
	}

	// Initial update status.
	// Some of the subsequent operations depend on `cluster.Status`.
	r.updateStatus(cluster, nodeList)

	placement, err := GetPlacement(ctx, ctl, cluster, nodeList.Items)
	if err != nil {
		logging.Errorf(ctx, "failed to calculate node placement %+v", err)
		return controllers.RequeueIfError(err)
	}

	// Reconcile the number of CrdbNodes within this Kubernetes cluster.
	// NB: Scale handles the initial creation of CrdbNodes.
	// Note: Scale() already checks IsManagedMode() internally and skips if Disabled

	// scaleHoldOff is set when scale-down is blocked because of
	// under-replicated ranges. We use this at the end to ensure a
	// delayed requeue so the scale-down retries.
	var scaleHoldOff time.Duration
	if didChange, requeueAfter, err := r.Scale(ctx, cluster, nodeList, placement); err != nil {
		logging.Errorf(ctx, "failed to scale CrdbCluster %s: %+v", cluster.Name, err)
		return controllers.RequeueIfError(err)
	} else if requeueAfter > 0 {
		scaleHoldOff = requeueAfter
	} else if didChange && cluster.IsInitialized() {
		// Scale changed the CrdbNodes, so the node list captured at the start of
		// this reconcile is now stale. Requeue before using that snapshot again.
		// The initial scale-up continues below because cluster initialization needs
		// the first Pod to come online in this reconcile.
		if updateErr := ctl.UpdateStatus(ctx, cluster); updateErr != nil {
			logging.Errorf(ctx, "Failed to persist status after Scale for CrdbCluster %q: %v", cluster.Name, updateErr)
		}
		return controllers.RequeueImmediately()
	}

	if result, err := r.ensureOwnerReference(ctx, cluster, nodeList); err != nil {
		logging.Errorf(ctx, "failed to ensure owner references for CrdbCluster %q: %+v", cluster.Name, err)
		return result, err
	}

	if result, err := r.handlePVCResize(ctx, cluster, nodeList); err != nil {
		logging.Errorf(ctx, "failed to handle PVC resize for CrdbCluster %q: %+v", cluster.Name, err)
		return result, err
	}

	// After .Scale has reconciled our total number of CrdbNodes, perform any
	// rolling updates to reconcile the state of all existing CrdbNodes.
	if result, err := r.RollingUpdate(ctx, cluster, nodeList, placement); err != nil {
		logging.Errorf(ctx, "failed to update CrdbNodes of CrdbCluster %q: %+v", cluster.Name, err)
		return result, err
	} else if !result.IsZero() {
		// TODO(orchestration): Ideally we wouldn't short circuit here and
		// instead run the rest of the controller to completion. This is a bit
		// difficult to achieve right now.
		//
		// During upgradeValidation if the validation job is still pending we requeue immediately,
		// So we need to make sure the status is persisted and reflected on the next reconcile
		if updateErr := ctl.UpdateStatus(ctx, cluster); updateErr != nil {
			logging.Errorf(ctx, "Failed to persist status after RollingUpdate for CrdbCluster %q: %v", cluster.Name, updateErr)
		}
		return result, nil
	}

	if len(nodeList.Items) == 0 {
		logging.Infof(ctx, "No nodes available yet. CrdbCluster: %v", cluster.Name)
		// No need to requeue here, we'll be notified when a node has been created.
		return controllers.NoRequeue()
	}

	// Initialize the cluster _before_ attempting to write any settings.
	if !cluster.IsInitialized() && cluster.Spec.Mode != nil && *cluster.Spec.Mode == crdbv1beta1.MutableOnly {
		initNode := initializationNode(cluster, nodeList)
		if initNode == nil {
			logging.Warn(ctx, "No Running CrdbNodes available to initialize the cluster")
			return controllers.RequeueOnDependent()
		}
		if pinnedNodeName := cluster.Status.InitializationNode; pinnedNodeName != "" && pinnedNodeName != initNode.Name {
			logging.Warnf(ctx, "Pinned initialization node CrdbNode/%s no longer exists; selecting replacement CrdbNode/%s",
				pinnedNodeName, initNode.Name)
		}
		if !initNode.Status.HasCondition(crdbv1beta1.PodRunning, metav1.ConditionTrue) {
			// We stick with the pinned node here instead of grabbing a healthy one,
			// since for all we know it already bootstrapped. initializationNode
			// spells out why that matters.
			if err := r.recordInitializationNodeUnavailable(ctx, ctl, cluster, initNode.Name); err != nil {
				logging.Errorf(ctx, "Failed to persist unavailable initialization node for CrdbCluster %v: %+v",
					cluster.Name, err)
				return controllers.RequeueIfError(err)
			}
			return controllers.RequeueOnDependent()
		}
		// This lands the pin on the API server before init runs, so a retry, restart,
		// or leader handoff from this point keeps bootstrapping this same node.
		if err := pinInitializationNode(ctx, ctl, cluster, initNode); err != nil {
			logging.Errorf(ctx, "Failed to pin initialization node CrdbNode/%s for CrdbCluster %v: %+v",
				initNode.Name, cluster.Name, err)
			return controllers.RequeueIfError(err)
		}

		// TODO(pseudomuto): We may need to figure out a way to determine whether or
		// not a cluster has already been initialized. We should be able to reconstruct
		// the Status from the existing state. In an ideal world, we'd be able to ask
		// CRDB whether or not the cluster is initialized and take action accordingly.
		if err := r.Initialize(ctx, initNode, cluster.Spec.Regions, ctl); err != nil {
			if statusErr := r.recordInitializationFailure(ctx, ctl, cluster, initNode.Name, err); statusErr != nil {
				logging.Errorf(ctx, "Failed to persist initialization failure for CrdbCluster %v: %+v",
					cluster.Name, statusErr)
				err = errors.CombineErrors(err, statusErr)
				return controllers.RequeueIfError(err)
			}
			// TODO(prod-orch): We could add a dedicated failure metric if alerting needs the exact
			// retry count. Returning an error here would increment controller-runtime's
			// reconcile error metric, but it would also replace this fixed delay with
			// the runtime's rate limiter.
			return controllers.RequeueAfter(clusterInitializationRetryDelay, nil)
		}

		cluster.Status.SetCondition(crdbv1beta1.ClusterInitialized, metav1.ConditionTrue)
		// The pin has no more value once we're initialized, and the CrdbNode it names
		// might get scaled down or decommissioned later.
		cluster.Status.InitializationNode = ""
		logging.Infof(ctx, "Successfully initialized CrdbCluster/%s on CrdbNode/%s", cluster.Name, initNode.Name)
	}

	// Ensure cluster settings are as they should be.
	if err := r.ensureClusterSettings(ctx, cluster); err != nil {
		logging.Errorf(ctx, "Failed to set cluster settings for CrdbCluster %v: %+v", cluster.Name, err)
		return controllers.RequeueIfError(err)
	}

	// Execute post-init SQL exactly once after the cluster is initialized.
	// The error is intentionally not logged here because pgx driver errors
	// can echo tokens from the failing SQL, which may be secret-sourced.
	// See the PostInitSQLApplied condition's Message for the user-visible
	// error (with the same caveat documented in ensurePostInitSQL).
	if err := r.ensurePostInitSQL(ctx, cluster); err != nil {
		logging.Warnf(ctx, "Post-init SQL failed for CrdbCluster %s; see status for details", cluster.Name)
		return controllers.RequeueIfError(err)
	}

	// Detect and update the current CockroachDB image and version if possible
	// This happens during initial cluster setup and after upgrades of cockroachdb
	if cluster.IsInitialized() {
		isUpgradeFinished := cluster.Status.HasAction(crdbv1beta1.Upgrade, crdbv1beta1.Finished)
		// Detect and update the CockroachDB version and image in the cluster status if needed:
		// - On initial cluster initialization (when status.Version is empty), always detect and set the version and image.
		// - After a rolling upgrade, if the upgrade action is marked as finished and the image in the status does not match
		//   the spec, detect and update the version and image. This ensures detectCurrentVersion is only called once per
		//   upgrade, since after updating, status.Image and spec.Image will match.
		// Updating Status.Image to Spec.Image when validation has failed would make
		// requiresUpgradeValidation return false on the next reconcile, allowing the
		// rolling update to proceed with the rejected image.
		validationNotFailed := !cluster.Status.HasAction(crdbv1beta1.ValidateVersion, crdbv1beta1.Failed)
		if cluster.Status.Version == "" || (isUpgradeFinished && validationNotFailed && cluster.Status.Image != cluster.Spec.Template.Spec.Image) {
			if currentVersion, err := r.detectCurrentVersion(ctx, cluster, nodeList); err != nil {
				logging.Warnf(ctx, "Failed to detect current CockroachDB version: %v", err)
			} else {
				logging.Infof(ctx, "Detected current CockroachDB version: %s", currentVersion)
				cluster.Status.Image = cluster.Spec.Template.Spec.Image
				cluster.Status.Version = currentVersion
			}
		}
	}

	r.updateStatus(cluster, nodeList)
	if err := ctl.UpdateStatus(ctx, cluster); err != nil {
		logging.Errorf(ctx, "Failed to set ObservedGeneration on status for CrdbCluster %v: %v", cluster.Name, err)
		return controllers.RequeueIfError(err)
	}

	logging.Infof(ctx, "Reconciliation complete for generation: %d", cluster.Status.ObservedGeneration)

	// scaleHoldOff > 0 hints that there are under-replicated ranges and, we should requeue
	// after a delay to allow the cluster to recover before attempting a scale-down again.

	// We defer this to the end of loop to let other ops run.
	if scaleHoldOff > 0 {
		return controllers.RequeueAfter(scaleHoldOff, nil)
	}
	return controllers.NoRequeue()
}

// ensureOwnerReference ensures that all nodes are owned by the cluster.
// Before potentially updating the CrdbNodes, ensure that this CrdbCluster is
// marked as its owner. Without ownership, Kubernetes will not send
// watch events to clusterctrl as expected.
// We take ownership in this loop as it will correct any clusters
// migrated from StatefulSets by first creating CrdbNodes and then
// CrdbClusters.
func (r *Reconciler) ensureOwnerReference(
	ctx context.Context, cluster *crdbv1beta1.CrdbCluster, nodes crdbv1beta1.CrdbNodeList,
) (ctrl.Result, error) {
	expectedRef, err := kube.OwnerReferenceFor(r.Ctl.Scheme(), cluster)
	if err != nil {
		return controllers.RequeueIfError(errors.Wrap(err, "failed to generate owner reference"))
	}

	for i := range nodes.Items {
		node := &nodes.Items[i]
		existing := metav1.GetControllerOf(node)

		if existing == nil || existing.APIVersion != expectedRef.APIVersion {
			if err := ctrl.SetControllerReference(cluster, node, r.Ctl.Scheme()); err != nil {
				return controllers.RequeueIfError(errors.Wrap(err, "failed to set controller for node"))
			}
			if err := r.Ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, node); err != nil {
				return controllers.RequeueIfError(errors.Wrap(err, "failed apply owner reference"))
			}
		}
	}
	return controllers.NoRequeue()
}

// detectCurrentVersion detects the current CockroachDB version from running nodes
func (r *Reconciler) detectCurrentVersion(
	ctx context.Context, cluster *crdbv1beta1.CrdbCluster, nodes crdbv1beta1.CrdbNodeList,
) (string, error) {
	// Find a running node to execute the version command
	runningNodes := nodes.WithCondition(crdbv1beta1.PodRunning, metav1.ConditionTrue)
	if len(runningNodes) == 0 {
		return "", errors.New("no running nodes available to detect version")
	}

	// Use the first running node to detect version
	node := runningNodes[0]
	ctl := r.Ctl.WithNamespace(cluster.Namespace)

	// Execute cockroach version --build-tag command using the executor directly
	crdbVersion, err := r.NodeExecutor(node, ctl).Version(ctx)
	if err != nil {
		return "", errors.Wrapf(err, "failed to execute version command on node %s", node.Name)
	}

	crdbVersion = strings.TrimSpace(crdbVersion)

	ver, err := version.Parse(crdbVersion)
	if err != nil {
		return "", errors.Wrapf(err, "failed to parse version: %s", crdbVersion)
	}

	return ver.String(), nil
}

// MigrationHandoffComplete reports whether migrationctrl has transferred ownership.
func MigrationHandoffComplete(cluster *crdbv1beta1.CrdbCluster) bool {
	return migrationctrl.IsMigrationFullyComplete(cluster)
}

// cleanMigrationAnnotations removes operator-injected migration annotations
// from a cluster that has completed migration. User-applied annotations
// (regionCode, cloudProvider, migration-regions) are left alone because Helm
// or GitOps might still declare them. Returns true if any were removed.
func cleanMigrationAnnotations(cluster *crdbv1beta1.CrdbCluster) bool {
	cleaned := false
	for _, key := range crdbv1alpha1.OperatorInjectedMigrationAnnotationKeys() {
		if _, ok := cluster.Annotations[key]; ok {
			delete(cluster.Annotations, key)
			cleaned = true
		}
	}
	return cleaned
}

// updateStatus simply updates cluster.Status in place based on the nodes
// and the cluster.
// The changes are not persisted; the caller is responsible for doing so.
func (r *Reconciler) updateStatus(
	cluster *crdbv1beta1.CrdbCluster, nodes crdbv1beta1.CrdbNodeList,
) {
	cluster.Status.Region = r.Env.CloudRegion
	cluster.Status.Provider = string(r.Env.CloudProvider)
	cluster.Status.ReconciledByBetaController = true

	// ObjectMeta.Generation monotonically increases with changes to Spec.
	// Setting ObservedGeneration equal to the current Generation indicates
	// that we've successfully reconciled the latest changes to this CrdbCluster
	// resource. Thus, it's important that this is ONLY set at the END of reconciliation.
	cluster.Status.ObservedGeneration = cluster.ObjectMeta.Generation

	// Extract the region that this reconciler is responsible for. If there's
	// no region, there's nothing to be done.
	thisRegion := cluster.Region()
	if thisRegion == nil {
		return
	}

	reconciled := true
	if len(nodes.Items) != int(thisRegion.Nodes) {
		reconciled = false
	}

	//  We should only initialize in one of the regions. Otherwise, if there are
	//  any connectivity issues between regions, the two regions will be initialized
	//  as independent clusters.
	// TODO(ye): we mark other regions as initialized immediately to prevent further
	//  initializations for now. This is just a temporary measure. We should
	//  try to detect, potentially by execing some command periodically to determine
	//  if the first region is initialized.
	if thisRegion != &cluster.Spec.Regions[0] {
		cluster.Status.SetCondition(crdbv1beta1.ClusterInitialized, metav1.ConditionTrue)
	}

	readyNodes := 0
	revision := manifests.GetClusterRevision(cluster)
	for i := range nodes.Items {
		// If any nodes are in the process of being deleted, wait for them to
		// be fully deleted first by setting reconciled to false. Deletions
		// occur with foreground deletion, which causes us to wait for the
		// underlying CRDB Pod to be fully terminated before proceeding.
		if !nodes.Items[i].DeletionTimestamp.IsZero() {
			reconciled = false
		}

		if nodes.Items[i].Annotations[manifests.BetaCrdbClusterNodeRevisionAnnotationKey] != revision {
			reconciled = false
		}

		// Below, we count ready nodes.
		if kube.IsReady(&nodes.Items[i]) != nil {
			continue
		}
		readyNodes++
	}

	cluster.Status.Reconciled = reconciled
	cluster.Status.ReadyNodes = int32(readyNodes)
}

// SetTestMode implements controllers.Reconciler.
func (r *Reconciler) SetTestMode(inTestMode bool) {
	r.inTestMode = inTestMode
}

// SetupWithManager configures this reconciler with the supplied manager.
func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	r.Emit = kube.EventEmitter{EventRecorder: mgr.GetEventRecorderFor("betaclusterctrl")}

	return ctrl.NewControllerManagedBy(mgr).
		For(&crdbv1beta1.CrdbCluster{}).
		Owns(&crdbv1beta1.CrdbNode{}).
		Owns(&corev1.Service{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}

func (r *Reconciler) ensureClusterSettings(ctx context.Context, c *crdbv1beta1.CrdbCluster) error {
	if c.Spec.Mode == nil || *c.Spec.Mode != crdbv1beta1.MutableOnly {
		return nil
	}
	// Cluster settings require an authenticated SQL connection over TLS and
	// are not supported for insecure clusters.
	if !c.Spec.TLSEnabled {
		logging.Warnf(ctx, "Skipping cluster settings for %s: insecure clusters do not support authenticated SQL connections", c.Name)
		return nil
	}
	settings := &ClusterSettings{Cluster: c}
	if settings.IsStale() {
		ctl := r.Ctl.WithNamespace(c.Namespace)
		cfg, err := tlsConfigForCluster(ctl, c)
		if err != nil {
			return err
		}

		opts := kubenet.Options{
			Selector:  nodeLabelSelectorForCluster(c),
			TLSConfig: cfg,
			Port:      int(c.Spec.Template.Spec.GetSQLPort()),
		}

		if err := settings.Apply(ctx, ctl, opts); err != nil {
			return err
		}

		// Store the settings on the status
		c.Status.Settings = c.Spec.ClusterSettings
	}

	return nil
}

// ensurePostInitSQL executes the user-provided post-init SQL statements
// exactly once. On failure it sets the PostInitSQLApplied condition to
// False with the error in Reason/Message and returns the error so
// controller-runtime requeues with backoff. A spec edit triggers an
// immediate reconcile via watch event; all statements replay from the
// beginning.
func (r *Reconciler) ensurePostInitSQL(ctx context.Context, c *crdbv1beta1.CrdbCluster) error {
	if c.Spec.PostInitSQL == nil {
		return nil
	}
	if c.Status.HasCondition(crdbv1beta1.ClusterPostInitSQLApplied, metav1.ConditionTrue) {
		return nil
	}
	if !c.IsInitialized() {
		return nil
	}
	if c.Spec.Mode == nil || *c.Spec.Mode != crdbv1beta1.MutableOnly {
		return nil
	}
	if !c.Spec.TLSEnabled {
		logging.Warnf(ctx, "Skipping post-init SQL for %s: insecure clusters do not support authenticated SQL connections", c.Name)
		return nil
	}

	ctl := r.Ctl.WithNamespace(c.Namespace)
	cfg, err := tlsConfigForCluster(ctl, c)
	if err != nil {
		return err
	}

	pis := &PostInitSQL{
		Spec:             c.Spec.PostInitSQL,
		ResolveConfigMap: configMapResolver(ctx, ctl),
		ResolveSecret:    secretResolver(ctx, ctl),
	}

	opts := kubenet.Options{
		Selector:             nodeLabelSelectorForCluster(c),
		TLSConfig:            cfg,
		Port:                 int(c.Spec.Template.Spec.GetSQLPort()),
		PreferSimpleProtocol: true, // required for multi-statement Exec on configmap/secret blobs
	}

	if err := pis.Apply(ctx, ctl, opts); err != nil {
		// Persist the failure so kubectl describe surfaces the cause.
		// Apply's wrap adds only the source label, not the SQL body. The
		// underlying driver error can still echo offending tokens from the
		// failing statement, so secret-derived SQL with malformed values can
		// surface partial content in the condition's Message. Treat Message
		// as not suitable for storing secrets and document this in the user
		// guide.
		c.Status.SetConditionWithMessage(crdbv1beta1.ClusterPostInitSQLApplied, metav1.ConditionFalse, "Failed", err.Error())
		if updateErr := ctl.UpdateStatus(ctx, c); updateErr != nil {
			logging.Errorf(ctx, "Failed to persist PostInitSQL failure status: %v", updateErr)
		}
		// If we ever want crashloop-on-failure semantics for maximum
		// cross-cluster visibility (at the cost of taking the whole
		// operator down on one bad spec), replace the return below
		// with: klog.Fatalf("PostInitSQL failed for %s/%s: %+v",
		// c.Namespace, c.Name, err). The status update above already
		// runs before any such crash, so the failure remains visible
		// after the pod restarts.
		return errors.Wrap(err, "post-init SQL failed")
	}

	c.Status.SetConditionWithMessage(crdbv1beta1.ClusterPostInitSQLApplied, metav1.ConditionTrue, "", "")
	return nil
}

// configMapResolver returns a function that fetches a key from a ConfigMap
// in the same namespace as the CrdbCluster being reconciled.
func configMapResolver(
	ctx context.Context, ctl *kube.Ctl,
) func(*corev1.ConfigMapKeySelector) (string, error) {
	return func(ref *corev1.ConfigMapKeySelector) (string, error) {
		var cm corev1.ConfigMap
		if err := ctl.Get(ctx, ref.Name, &cm); err != nil {
			return "", errors.Wrapf(err, "fetching configmap %s", ref.Name)
		}
		body, ok := cm.Data[ref.Key]
		if !ok {
			return "", errors.Newf("configmap %s has no key %q", ref.Name, ref.Key)
		}
		return body, nil
	}
}

// secretResolver returns a function that fetches a key from a Secret in
// the same namespace as the CrdbCluster being reconciled.
func secretResolver(
	ctx context.Context, ctl *kube.Ctl,
) func(*corev1.SecretKeySelector) (string, error) {
	return func(ref *corev1.SecretKeySelector) (string, error) {
		var sec corev1.Secret
		if err := ctl.Get(ctx, ref.Name, &sec); err != nil {
			return "", errors.Wrapf(err, "fetching secret %s", ref.Name)
		}
		raw, ok := sec.Data[ref.Key]
		if !ok {
			return "", errors.Newf("secret %s has no key %q", ref.Name, ref.Key)
		}
		return string(raw), nil
	}
}

func nodeLabelSelectorForCluster(cluster *crdbv1beta1.CrdbCluster) labels.Selector {
	return kube.ParseLabelOrDie(fmt.Sprintf("%s=%s", manifests.BetaCrdbClusterNodeOwnerLabelKey,
		cluster.Name))
}

// handlePendingDecommissionNode handles a node marked for pending decommission.
// It either deletes the node immediately (if not running) or transitions it to normal decommission flow.
func (r *Reconciler) handlePendingDecommissionNode(
	ctx context.Context, node *crdbv1beta1.CrdbNode,
) (ctrl.Result, error) {
	if !node.DeletionTimestamp.IsZero() {
		// Deletion is already in progress, so another Delete would only churn the
		// API server. RequeueOnDependent returns a zero result. The CrdbNode watch
		// wakes this controller when finalizer cleanup changes or deletes the node.
		logging.Infof(ctx, "Waiting for CrdbNode %s cleanup to finish", node.Name)
		return controllers.RequeueOnDependent()
	}

	// Verify if this node is still not running. If it's not running,
	// we can immediately delete it with a resource version check.
	if node.Status.HasCondition(crdbv1beta1.PodRunning, metav1.ConditionFalse) {
		return r.deletePendingNode(ctx, node)
	}

	// Node has progressed to running, so we need to clear the
	// pending decommission annotation.
	return r.removePendingDecommissionAnnotation(ctx, node)
}

// deletePendingNode safely deletes a non-running node with optimistic concurrency control.
func (r *Reconciler) deletePendingNode(
	ctx context.Context, node *crdbv1beta1.CrdbNode,
) (ctrl.Result, error) {
	logging.Infof(ctx, "Deleting non-running node with pending decommission: %s", node.Name)

	// If this operation fails, it means the resource has been modified
	if err := r.Ctl.DeleteWithOptions(ctx, kube.DeleteOptions{
		NoWait:            true,
		PropagationPolicy: metav1.DeletePropagationForeground,
		Preconditions: &metav1.Preconditions{
			UID:             &node.UID,
			ResourceVersion: &node.ResourceVersion,
		},
	}, node); err != nil {
		if kubeerrors.IsNotFound(err) {
			logging.Infof(ctx, "CrdbNode %s already deleted, skipping", node.Name)
			return controllers.RequeueImmediately()
		}
		return controllers.RequeueIfError(errors.Wrapf(err,
			"Failed to delete CrdbNode (fetched resource version %s was out of date): %s", node.ResourceVersion, node.Name))
	}

	return controllers.RequeueOnDependent()
}

// removePendingDecommissionAnnotation clears pending decommission annotation for a node.
// We do not mark this node as decommissioning here; instead, we requeue immediately.
// This scenario occurs when a pod marked for pending decommission transitions from non-running to running,
// typically due to a scale down issued concurrently with a scale up or update.
// Therefore, we prioritize decommissioning other non-running pods first if any exist.
func (r *Reconciler) removePendingDecommissionAnnotation(
	ctx context.Context, node *crdbv1beta1.CrdbNode,
) (ctrl.Result, error) {
	if node.Status.NodeID == nil {
		return controllers.RequeueImmediately()
	}

	// Clear the pending decommission annotation
	delete(node.Annotations, crdbv1beta1.CrdbNodeStartPendingDecommissionAnnotation)

	if err := r.Ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, node); err != nil {
		return controllers.RequeueIfError(errors.Wrapf(err,
			"Failed to update node: %s", node.Name))
	}

	return controllers.RequeueImmediately()
}

// handleDecommissionedNode handles deletion of fully decommissioned nodes.
func (r *Reconciler) handleDecommissionedNode(
	ctx context.Context, node *crdbv1beta1.CrdbNode,
) (ctrl.Result, error) {
	if !node.DeletionTimestamp.IsZero() {
		// Deletion is already in progress, so another Delete would only churn the
		// API server. RequeueOnDependent returns a zero result. The CrdbNode watch
		// wakes this controller when finalizer cleanup changes or deletes the node.
		logging.Infof(ctx, "Waiting for CrdbNode %s cleanup to finish", node.Name)
		return controllers.RequeueOnDependent()
	}

	logging.Infof(ctx, "Deleting fully decommissioned node: %s", node.Name)

	// PropagationPolicy ensures that the underlying Pod will be
	// deleted BEFORE the CrdbNode is deleted.
	// TODO(chrisseto): This is likely better handled with a finalizer
	// set on the CrdbNode.
	if err := r.Ctl.DeleteWithOptions(
		ctx,
		kube.DeleteOptions{NoWait: true, PropagationPolicy: metav1.DeletePropagationForeground},
		node,
	); err != nil && !kubeerrors.IsNotFound(err) {
		return controllers.RequeueIfError(err)
	}

	// The CrdbNode watch re-triggers reconciliation when finalizer cleanup
	// changes or deletes the node.
	return controllers.RequeueOnDependent()
}

func tlsConfigForCluster(ctl *kube.Ctl, c *crdbv1beta1.CrdbCluster) (*tls.Config, error) {
	clusterCerts := c.Spec.Template.Spec.Certificates

	// If the cluster uses self-signed certs, load the CA and root client cert
	// that GenerateCrdbCerts deposited into the well-known secrets.
	if clusterCerts.SelfSigned {
		ca, bundle, err := crdbcerts.GetBundle(ctl, manifests.SelfSignedCAConfigMapName(c.Name), manifests.SelfSignedRootClientSecretName(c.Name))
		if err != nil {
			return nil, errors.Wrap(err, "fetching self-signed cert bundle")
		}
		// Use the join service FQDN for server name verification, consistent
		// with the external certs path. The join service always exists and its
		// FQDN is included as a SAN on the self-signed node cert.
		serverName := manifests.JoinFQDN(c.Name, c.Namespace, c.Domain())
		cfg, err := crypto.TLSConfigFromBundle(ca, bundle, serverName)
		return cfg, errors.Wrap(err, "creating TLS config from self-signed bundle")
	}

	certNames := clusterCerts.ExternalCertificates

	// If the cluster is using operator-managed certs, load the standard root
	// client bundle from the K8s secrets store.
	if certNames == nil {
		ca, bundle, err := crdbcerts.GetRootClientCertBundle(ctl)
		if err != nil {
			return nil, errors.Wrap(err, "fetching root client cert bundle")
		}
		cfg, err := crypto.TLSConfigFromBundle(ca, bundle, fmt.Sprintf("%s.%s", c.Name, c.Namespace))
		return cfg, errors.Wrap(err, "creating TLS config from bundle")
	}

	caConfigMapName := certNames.NodeCAConfigMapName
	if caConfigMapName == "" {
		// When using a single CA, CAConfigMapName is set instead of NodeCAConfigMapName.
		caConfigMapName = certNames.CAConfigMapName
	}
	if caConfigMapName == "" {
		return nil, errors.New("missing CA config map name for external certificates (set caConfigMapName or nodeCaConfigMapName)")
	}
	if certNames.RootSQLClientSecretName == "" {
		return nil, errors.New("missing root SQL client secret name for external certificates (set rootSqlClientSecretName)")
	}

	ca, bundle, err := crdbcerts.GetBundle(
		ctl,
		caConfigMapName,
		certNames.RootSQLClientSecretName,
	)
	if err != nil {
		return nil, errors.Wrap(err, "fetching cert bundle")
	}

	// Use the join service FQDN for server name verification. The join service
	// only routes to Ready pods, and its DNS name is included on node
	// certificates (operator-managed and typically for external certs as well),
	// whereas "<cluster>.<namespace>" is not.
	serverName := manifests.JoinFQDN(c.Name, c.Namespace, c.Domain())
	cfg, err := crypto.TLSConfigFromBundle(ca, bundle, serverName)
	return cfg, errors.Wrap(err, "creating TLS config from bundle")
}

func reportFeatureMetrics(cluster *crdbv1beta1.CrdbCluster) {
	for _, feature := range crdbv1beta1.AllClusterFeatures {
		enabled := 0
		if cluster.Spec.FeatureEnabled(feature) {
			enabled = 1
		}
		opmetrics.ClusterFeatureEnabled.WithLabelValues(string(feature)).Set(float64(enabled))
	}
}

package migrationctrl

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1alpha1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1alpha1"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/migrationutil"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/utils/sliceutils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

// podMigrationState holds the per-pod state shared between verifyNodeHealthAndProceed
// and createNodeAndScaleDown. Grouping these avoids a long argument list.
type podMigrationState struct {
	cluster        *crdbv1beta1.CrdbCluster
	migration      *crdbv1beta1.MigrationStatus
	stsCtx         *migrationutil.ParsedMigrationContext
	targetNodeName string
	targetIdx      int32
}

// handleInitPhase validates prerequisites and prepares for migration.
func (r *Reconciler) handleInitPhase(
	ctx context.Context, ctl *kube.Ctl, discovery *DiscoveryResult, cluster *crdbv1beta1.CrdbCluster,
) (ctrl.Result, error) {
	migration := cluster.Status.Migration

	logging.Infof(ctx, "Starting init for %s migration", migration.Type)
	r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "MigrationStarted", "Starting %s migration", migration.Type)

	// Validate we have a StatefulSet
	if discovery.StatefulSet == nil {
		logging.Errorf(ctx, "No StatefulSet found for migration")
		return controllers.RequeueIfError(errors.New("no StatefulSet found"))
	}

	sts := discovery.StatefulSet
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas == 0 {
		logging.Errorf(ctx, "StatefulSet %s has no replicas configured", sts.Name)
		return controllers.RequeueIfError(errors.New("statefulset has no replicas"))
	}

	// If CrdbNodes already exist, a previous migration was interrupted before status was persisted.
	// Auto-recover by syncing MigratedNodes from actual CrdbNodes and jumping to the
	// appropriate phase rather than blocking the user.
	recovered, err := r.autoRecoverInterruptedMigration(ctx, ctl, migration, cluster, discovery)
	if err != nil {
		logging.Errorf(ctx, "Failed to list existing CrdbNodes: %v", err)
		return controllers.RequeueIfError(err)
	}
	if recovered {
		return controllers.RequeueImmediately()
	}

	stsCtx, err := extractContextFromSTS(sts)
	if err != nil {
		logging.Errorf(ctx, "Failed to extract context from StatefulSet: %v", err)
		return controllers.RequeueIfError(errors.Wrap(err, "failed to extract context"))
	}

	// Log if insecure cluster detected. The TLSEnabled value from the parser is
	// preserved as-is and flows through to CrdbNode.Spec.TLSEnabled and
	// CrdbCluster.Spec.TLSEnabled. Insecure clusters skip cert generation.
	if !stsCtx.TLSEnabled {
		logging.Warnf(ctx, "Insecure cluster detected, will migrate without cert migration")
		r.Recorder.Event(cluster, corev1.EventTypeWarning, "InsecureClusterMigration",
			"Insecure cluster detected. Cert migration will be skipped.")
	}

	if slices.ContainsFunc(sts.Spec.VolumeClaimTemplates, func(v corev1.PersistentVolumeClaim) bool {
		return v.Name == manifests.CockroachLogsDirVolumeName || v.Name == "logs-dir"
	}) {
		logging.Infof(ctx, "Detected logsdir VolumeClaimTemplate; will migrate to CrdbNode LogsStore")
	}

	// TotalPods comes from OriginalReplicas, which is the immutable source of truth.
	// For operator migrations that's v1alpha1 Spec.Nodes; for Helm it's
	// existingNodes+STSReplicas. Using *sts.Spec.Replicas directly is wrong when
	// auto-recovery re-enters Init after a partial migration.
	if migration.TotalPods == 0 {
		migration.TotalPods = discovery.OriginalReplicas
		migration.CurrentPodIndex = migration.TotalPods - 1
	}

	if migration.MigratedNodes == nil {
		migration.MigratedNodes = []string{}
	}

	// Sync region annotations into spec.Regions before proceeding. If the user
	// annotated the CrdbCluster with regionCode via v1beta1 kubectl, conversion
	// never ran and spec.Regions still has stale defaults.
	if _, err := syncRegionAnnotations(ctx, ctl, cluster); err != nil {
		return controllers.RequeueIfError(errors.Wrap(err, "failed to sync region annotations"))
	}

	migration.Message = fmt.Sprintf("Init complete: %d pods, image=%s", migration.TotalPods, stsCtx.Image)

	migration.Phase = PhaseCertMigration

	logging.Infof(ctx, "Init complete: %d pods, tls=%v", migration.TotalPods, stsCtx.TLSEnabled)
	return controllers.RequeueImmediately()
}

// handleCertMigrationPhase migrates certificates and prepares cluster resources.
func (r *Reconciler) handleCertMigrationPhase(
	ctx context.Context, ctl *kube.Ctl, discovery *DiscoveryResult, cluster *crdbv1beta1.CrdbCluster,
) (ctrl.Result, error) {
	migration := cluster.Status.Migration
	sts := discovery.StatefulSet

	// Validate Init phase completed: TotalPods must be set.
	if migration.TotalPods == 0 {
		logging.Errorf(ctx, "Invalid migration state: TotalPods=0 - Init phase may not have completed")
		return controllers.RequeueIfError(errors.New("invalid state: TotalPods is 0, Init phase may not have completed"))
	}

	logging.Infof(ctx, "Starting cert migration")

	stsCtx, err := buildPhaseContext(ctx, ctl, sts, migration.Type)
	if err != nil {
		logging.Errorf(ctx, "Failed to extract context from StatefulSet: %v", err)
		return controllers.RequeueIfError(err)
	}
	if stsCtx.VirtualClusterSpec != nil {
		logging.Infof(ctx, "PCR detected: mode=%s", stsCtx.VirtualClusterSpec.Mode)
	}

	// Detect Ingress configuration and store it in stsCtx for later annotation in finalization.
	detectAndApplyIngress(ctx, ctl, discovery, stsCtx)

	// For public operator migrations, migrate ConfigMap key format
	// Use the v1alpha1 CR's LogConfigMap field directly (not parsed from STS)
	if migration.Type == MigrationTypeOperator && discovery.V1Alpha1Cluster != nil {
		if discovery.V1Alpha1Cluster.Spec.LogConfigMap != "" {
			if err := moveConfigMapKey(ctx, ctl, discovery.V1Alpha1Cluster.Spec.LogConfigMap); err != nil {
				logging.Warnf(ctx, "Failed to migrate ConfigMap key (non-fatal): %v", err)
			}
			stsCtx.LoggingConfigMap = discovery.V1Alpha1Cluster.Spec.LogConfigMap
		}
	}

	// For operator migrations, the v1alpha1 spec may have custom secret names that
	// override the defaults. These take priority over names parsed from STS volumes.
	// When custom secrets are provided, the user manages their own certs and the
	// controller cannot regenerate them (the CA private key is not available).
	var externalCerts bool
	if migration.Type == MigrationTypeOperator && discovery.V1Alpha1Cluster != nil {
		if discovery.V1Alpha1Cluster.Spec.NodeTLSSecret != "" {
			stsCtx.SourceNodeSecretName = discovery.V1Alpha1Cluster.Spec.NodeTLSSecret
			externalCerts = true
		}
		if discovery.V1Alpha1Cluster.Spec.ClientTLSSecret != "" {
			stsCtx.SourceClientSecretName = discovery.V1Alpha1Cluster.Spec.ClientTLSSecret
			externalCerts = true
		}
	}

	// Certificate migration is only needed for TLS-enabled clusters. Insecure clusters
	// use --insecure and have no certs to detect, migrate, or regenerate.
	if stsCtx.TLSEnabled {
		if err = performCertMigration(ctx, ctl, sts, stsCtx, migration.Type, externalCerts); err != nil {
			return controllers.RequeueIfError(err)
		}
	} else {
		logging.Infof(ctx, "Insecure cluster, skipping certificate migration")
	}

	// ClusterRole and ClusterRoleBinding are cluster-scoped, so their names still use
	// the cluster name for uniqueness. The bindings themselves should point at the same
	// ServiceAccount the migrated pods run as.
	if err = ensureRBAC(ctx, ctl, sts.Namespace, sts.Name, stsCtx.ServiceAccountName, migration.Type); err != nil {
		return controllers.RequeueIfError(errors.Wrap(err, "RBAC setup failed"))
	}

	// Update public service to have separate GRPC and SQL ports for Helm migrations.
	// The resolved ports from stsCtx handle non-default port clusters correctly.
	if migration.Type == MigrationTypeHelm {
		if err = updatePublicServicePorts(ctx, ctl, sts.Name, stsCtx.GRPCPort, stsCtx.SQLPort); err != nil {
			logging.Warnf(ctx, "Failed to update public service ports (non-fatal): %v", err)
		}
	}

	if err = ensurePriorityClass(ctx, ctl); err != nil {
		return controllers.RequeueIfError(errors.Wrap(err, "PriorityClass setup failed"))
	}

	if sts.Spec.Replicas != nil && *sts.Spec.Replicas > 0 {
		if err = labelExistingPods(ctx, ctl, sts.Name, *sts.Spec.Replicas); err != nil {
			logging.Warnf(ctx, "Pod labeling failed (non-fatal): %v", err)
		}
	}

	migration.Phase = PhasePodMigration
	migration.Message = "Certs, RBAC, PriorityClass ready"

	logging.Infof(ctx, "Cert migration complete")
	r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "CertMigrationComplete", "Certificate and RBAC setup complete")

	return controllers.RequeueImmediately()
}

// autoRecoverInterruptedMigration detects that a previous migration was interrupted by checking
// for existing CrdbNodes. When found, it syncs MigratedNodes from actual state and advances
// the phase to Finalization (all migrated) or CertMigration (partial) to resume safely.
// Returns true if recovery was triggered and the caller should requeue immediately.
func (r *Reconciler) autoRecoverInterruptedMigration(
	ctx context.Context,
	ctl *kube.Ctl,
	migration *crdbv1beta1.MigrationStatus,
	cluster *crdbv1beta1.CrdbCluster,
	discovery *DiscoveryResult,
) (bool, error) {
	existingNodes, err := listClusterCrdbNodes(ctx, ctl, cluster.Name)
	if err != nil {
		return false, errors.Wrap(err, "failed to list existing CrdbNodes")
	}
	if len(existingNodes) == 0 {
		return false, nil
	}

	logging.Warnf(ctx, "Found %d existing CrdbNodes for cluster %s, auto-recovering (originalReplicas=%d)",
		len(existingNodes), cluster.Name, discovery.OriginalReplicas)
	r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "MigrationAutoRecovery",
		"Auto-recovering interrupted migration: found %d/%d CrdbNodes", len(existingNodes), discovery.OriginalReplicas)

	// TotalPods comes from OriginalReplicas which is always the correct original count.
	migration.TotalPods = discovery.OriginalReplicas
	migration.MigratedNodes = make([]string, 0, len(existingNodes))
	for _, node := range existingNodes {
		migration.MigratedNodes = append(migration.MigratedNodes, node.Name)
	}

	if int32(len(existingNodes)) >= migration.TotalPods {
		logging.Infof(ctx, "All %d pods found as CrdbNodes, resuming at finalization", migration.TotalPods)
		migration.Phase = PhaseFinalization
		migration.Message = fmt.Sprintf("Auto-recovered: all %d pods migrated, resuming finalization", migration.TotalPods)
	} else {
		nextIdx := highestUnmigratedPodIndex(discovery.Name, migration.TotalPods, migration.MigratedNodes)
		migration.CurrentPodIndex = nextIdx
		// Re-enter at CertMigration to verify cert prerequisites before resuming pod migration.
		migration.Phase = PhaseCertMigration
		migration.Message = fmt.Sprintf("Auto-recovered: %d/%d pods migrated, re-verifying cert prerequisites",
			len(existingNodes), migration.TotalPods)
		logging.Warnf(ctx, "Auto-recovered partial migration: %d/%d pods, resuming at CertMigration (nextPodIndex=%d)",
			len(existingNodes), migration.TotalPods, nextIdx)
	}
	return true, nil
}

// detectAndApplyIngress detects Ingress configuration for the cluster and stores it in stsCtx.
// For Helm migrations it reads an Ingress resource; for operator migrations it converts the
// v1alpha1 CR Ingress field. Errors are non-fatal.
func detectAndApplyIngress(
	ctx context.Context,
	ctl *kube.Ctl,
	discovery *DiscoveryResult,
	stsCtx *migrationutil.ParsedMigrationContext,
) {
	switch discovery.MigrationType {
	case MigrationTypeHelm:
		ingressConfig, err := detectIngressFromHelm(ctx, ctl, discovery.Name)
		if err != nil {
			logging.Warnf(ctx, "Ingress detection failed (non-fatal): %v", err)
			return
		}
		if ingressConfig != nil {
			logging.Info(ctx, "Detected Ingress configuration from Helm chart")
			stsCtx.IngressConfig = ingressConfig
		}
	case MigrationTypeOperator:
		if discovery.V1Alpha1Cluster != nil && discovery.V1Alpha1Cluster.Spec.Ingress != nil {
			logging.Info(ctx, "Using Ingress configuration from v1alpha1 CrdbCluster")
			stsCtx.IngressConfig = convertV1Alpha1IngressToAnnotation(discovery.V1Alpha1Cluster.Spec.Ingress)
		}
	}
}

// buildPhaseContext creates a ParsedMigrationContext for the current phase by parsing the
// STS, resolving cert-manager secret names, and re-detecting PCR config. Each phase runs
// in a separate reconcile, so the context from previous phases is not available.
func buildPhaseContext(
	ctx context.Context, ctl *kube.Ctl, sts *appsv1.StatefulSet, migrationType string,
) (*migrationutil.ParsedMigrationContext, error) {
	stsCtx, err := extractContextFromSTS(sts)
	if err != nil {
		return nil, errors.Wrap(err, "failed to extract context")
	}
	if stsCtx.TLSEnabled {
		resolveCertSecretNames(ctx, ctl, sts.Name, stsCtx)
	}
	if migrationType == MigrationTypeHelm {
		pcrSpec, pcrErr := detectPCRFromInitJob(ctx, ctl, sts.Name)
		if pcrErr != nil {
			logging.Warnf(ctx, "PCR detection failed (non-fatal): %v", pcrErr)
		} else if pcrSpec != nil {
			stsCtx.VirtualClusterSpec = pcrSpec
		}
	}
	return stsCtx, nil
}

func applySourceCertDefaults(
	stsCtx *migrationutil.ParsedMigrationContext, clusterName, migrationType string,
) {
	switch migrationType {
	case MigrationTypeOperator:
		// The public operator has no "ca-certs" volume, so ParseStatefulSet never
		// overwrites SourceCASecretName from its Helm-style default ({name}-ca-secret).
		// The v1alpha1 spec also has no CA secret field. Always set the operator-style
		// name here so loadCASecretForMigration finds the right secret ({name}-ca).
		stsCtx.SourceCASecretName = clusterName + suffixOperatorCA
		if stsCtx.SourceNodeSecretName == "" {
			stsCtx.SourceNodeSecretName = clusterName + suffixOperatorNode
		}
		if stsCtx.SourceClientSecretName == "" {
			stsCtx.SourceClientSecretName = clusterName + suffixOperatorClient
		}
	default: // MigrationTypeHelm
		if stsCtx.SourceCASecretName == "" {
			stsCtx.SourceCASecretName = clusterName + suffixCASecret
		}
		if stsCtx.SourceNodeSecretName == "" {
			stsCtx.SourceNodeSecretName = clusterName + suffixNodeSecret
		}
		if stsCtx.SourceClientSecretName == "" {
			stsCtx.SourceClientSecretName = clusterName + suffixClientSecret
		}
	}

	stsCtx.CASecretName = stsCtx.SourceCASecretName
	migrationutil.NormalizeRuntimeTLSConfig(stsCtx, clusterName)
}

// performCertMigration handles all certificate setup for TLS-enabled clusters. It detects
// the cert management method (cert-manager, self-signer, or external), sets secret name
// fields on stsCtx, and runs the appropriate cert creation and regeneration steps.
//
// When externalCerts is true the user provided custom NodeTLSSecret or ClientTLSSecret in
// the v1alpha1 spec. The CA private key is not available in that case, so cert regeneration
// is skipped. CrdbNode pods mount the user's existing secrets directly. The user must
// include join service DNS SANs in their certificates before starting the migration.
func performCertMigration(
	ctx context.Context,
	ctl *kube.Ctl,
	sts *appsv1.StatefulSet,
	stsCtx *migrationutil.ParsedMigrationContext,
	migrationType string,
	externalCerts bool,
) error {
	usingCertManager, err := detectCertManagerCertificates(ctx, ctl, sts.Name)
	if err != nil {
		logging.Warnf(ctx, "Cert-manager detection failed (non-fatal): %v", err)
	}

	stsCtx.CAConfigMap = sts.Name + suffixCAConfigMap
	applySourceCertDefaults(stsCtx, sts.Name, migrationType)

	// Resolve actual secret names from cert-manager Certificate CRs when present;
	// self-signer defaults set above remain unchanged if no CR is found.
	resolveCertSecretNames(ctx, ctl, sts.Name, stsCtx)

	if usingCertManager {
		logging.Infof(ctx, "Detected cert-manager managed certificates")
		if err = ensureCertManagerCertificateSecrets(ctx, ctl, sts.Name); err != nil {
			logging.Warnf(ctx, "Cert-manager Secrets verification: %v", err)
		}
		if err = warnAboutCertManagerResources(ctx, ctl, sts.Name); err != nil {
			logging.Warnf(ctx, "Failed to check cert-manager resources (non-fatal): %v", err)
		}
	} else if externalCerts {
		logging.Infof(ctx, "Using externally managed certificates (custom NodeTLSSecret/ClientTLSSecret)")
		domain := stsCtx.Domain
		if domain == "" {
			domain = defaultClusterDomain
		}
		logging.Warnf(ctx, "Cert regeneration skipped: CA private key is not available for external certs. "+
			"Ensure your certificates include join service DNS SANs: %s-join, %s-join.%s, %s-join.%s.svc.%s",
			sts.Name, sts.Name, sts.Namespace, sts.Name, sts.Namespace, domain)
	} else {
		logging.Infof(ctx, "Using self-signer certificates")
	}

	logging.Infof(ctx, "Starting certificate migration - CA: %s, Node: %s, Client: %s",
		stsCtx.CAConfigMap, stsCtx.NodeSecretName, stsCtx.ClientSecretName)
	if err = ensureCerts(ctx, ctl, sts, stsCtx); err != nil {
		return errors.Wrap(err, "cert migration failed")
	}
	logging.Infof(ctx, "Certificate migration completed successfully")

	// Cert regeneration is only needed for self-signed certs managed by the operator or
	// Helm's self-signer. Cert-manager handles SAN updates via Certificate CR patches.
	// External certs (operator custom secrets or Helm provided mode) skip regeneration
	// because the CA private key is not available.
	switch {
	case usingCertManager:
		if err := updateCertManagerNodeCertificate(ctx, ctl, sts.Name, sts.Namespace, stsCtx.Domain); err != nil {
			logging.Warnf(ctx, "Failed to update cert-manager Certificate CR (non-fatal): %v", err)
		}
	case externalCerts:
		// already logged above
	default:
		// Probe for the CA private key before attempting regeneration. Helm provided mode
		// and other user-managed cert setups won't have ca.key in any secret.
		_, _, caErr := loadCASecretForMigration(ctx, ctl, sts.Name, stsCtx.SourceNodeSecretName, stsCtx.SourceCASecretName)
		if caErr != nil {
			d := stsCtx.Domain
			if d == "" {
				d = defaultClusterDomain
			}
			logging.Warnf(ctx, "CA private key not available, skipping cert regeneration: %v", caErr)
			logging.Warnf(ctx, "Ensure your certificates include join service DNS SANs: "+
				"%s-join, %s-join.%s, %s-join.%s.svc.%s",
				sts.Name, sts.Name, sts.Namespace, sts.Name, sts.Namespace, d)
			return nil
		}

		logging.Infof(ctx, "Regenerating self-signed node certificates with join service DNS SANs")
		if err = regenerateNodeCertificatesForMigration(ctx, ctl, sts.Name, sts.Namespace, stsCtx.SourceNodeSecretName, stsCtx.SourceCASecretName, stsCtx.Domain); err != nil {
			return errors.Wrap(err, "failed to regenerate node certificates")
		}
		logging.Infof(ctx, "Node certificate regeneration completed successfully")

		logging.Infof(ctx, "Regenerating self-signed client certificates")
		if err = regenerateClientCertificatesForMigration(ctx, ctl, sts.Name, sts.Namespace, stsCtx.SourceNodeSecretName, stsCtx.SourceCASecretName); err != nil {
			return errors.Wrap(err, "failed to regenerate client certificates")
		}
		logging.Infof(ctx, "Client certificate regeneration completed successfully")
	}
	return nil
}

// finalizeHelmSpec updates the minimal CrdbCluster created for status tracking with the
// full spec derived from the StatefulSet. It does not set Mode=MutableOnly. That is
// done by completeMigration after the STS is deleted. It replaces *cluster in-place so
// the caller must refresh any migration pointer derived from the old value after this call.
func finalizeHelmSpec(
	ctx context.Context,
	ctl *kube.Ctl,
	sts *appsv1.StatefulSet,
	stsCtx *migrationutil.ParsedMigrationContext,
	migration *crdbv1beta1.MigrationStatus,
	cluster *crdbv1beta1.CrdbCluster,
	cloudRegion string,
) error {
	// Re-fetch to get a fresh resourceVersion after any intermediate updates.
	var existingCluster crdbv1beta1.CrdbCluster
	if err := ctl.Get(ctx, sts.Name, &existingCluster); err != nil {
		return errors.Wrap(err, "failed to get CrdbCluster for Helm finalization")
	}

	spec := generateV1Beta1ClusterSpec(ctx, sts, stsCtx, migration.TotalPods, cloudRegion)
	// Preserve Mode from the existing spec. generateV1Beta1ClusterSpec doesn't
	// set Mode, so overwriting the full spec would clear Mode=Disabled to nil.
	// Without this, betaclusterctrl falls through all its migration guards and
	// attempts cockroach init on an already-running cluster.
	spec.Mode = existingCluster.Spec.Mode
	existingCluster.Spec = spec

	if existingCluster.Labels == nil {
		existingCluster.Labels = make(map[string]string)
	}
	existingCluster.Labels[LabelMigrationStatus] = MigrateInProgress

	// Preserve Ingress config in an annotation since v1beta1 has no Ingress spec field.
	if stsCtx.IngressConfig != nil {
		if ingressBytes, err := json.Marshal(stsCtx.IngressConfig); err == nil {
			if existingCluster.Annotations == nil {
				existingCluster.Annotations = make(map[string]string)
			}
			existingCluster.Annotations[crdbv1alpha1.BetaCrdbClusterIngressAnnotationKey] = string(ingressBytes)
			logging.Info(ctx, "Preserved Ingress configuration in cluster annotations")
		} else {
			logging.Warnf(ctx, "Failed to marshal ingress config (non-fatal): %v", err)
		}
	}

	if err := ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, &existingCluster); err != nil {
		return errors.Wrap(err, "failed to update CrdbCluster spec")
	}
	// Replace *cluster so subsequent status updates use the persisted object.
	// The Update response may not include status (subresource split), so
	// re-fetch to get a consistent view with both spec and status.
	*cluster = existingCluster
	if _, err := refetchClusterMigration(ctx, ctl, cluster, PhaseFinalization, MigrationTypeHelm); err != nil {
		return errors.Wrap(err, "re-fetch after Helm spec update")
	}
	logging.Infof(ctx, "CrdbCluster updated with full spec (Mode pending STS deletion)")
	return nil
}

// handlePodMigrationPhase migrates pods one at a time, highest index first.
func (r *Reconciler) handlePodMigrationPhase(
	ctx context.Context, ctl *kube.Ctl, discovery *DiscoveryResult, cluster *crdbv1beta1.CrdbCluster,
) (ctrl.Result, error) {
	migration := cluster.Status.Migration
	sts := discovery.StatefulSet

	// Sync the MigratedNodes list with actual CrdbNodes before proceeding. This catches
	// state drift from operator restarts or failed status updates between CrdbNode creation
	// and status persistence (e.g. a node was created but never tracked in MigratedNodes).
	// We compare sets, not just counts, because a delete+create can leave the count unchanged
	// while the names differ.
	actualNodes, err := listClusterCrdbNodes(ctx, ctl, cluster.Name)
	if err != nil {
		return controllers.RequeueIfError(errors.Wrap(err, "failed to list CrdbNodes for drift sync"))
	}
	actualNames := make([]string, 0, len(actualNodes))
	for _, node := range actualNodes {
		actualNames = append(actualNames, node.Name)
	}
	if !sliceutils.MultiSetsEqual(actualNames, migration.MigratedNodes) {
		logging.Warnf(ctx, "MigratedNodes drift detected: status=%v, actual=%v, syncing",
			migration.MigratedNodes, actualNames)
		migration.MigratedNodes = actualNames
		migration.CurrentPodIndex = highestUnmigratedPodIndex(discovery.Name, migration.TotalPods, migration.MigratedNodes)
	}

	stsCtx, err := buildPhaseContext(ctx, ctl, sts, migration.Type)
	if err != nil {
		return controllers.RequeueIfError(err)
	}

	// Validate cert migration completed by checking that the node secret exists.
	// Skipped for insecure clusters which have no cert secrets.
	if stsCtx.TLSEnabled {
		nodeSecret := &corev1.Secret{}
		if err := ctl.Get(ctx, stsCtx.NodeSecretName, nodeSecret); err != nil {
			if apierrors.IsNotFound(err) {
				return controllers.RequeueIfError(errors.Errorf(
					"node secret %s not found, cert migration may not have completed", stsCtx.NodeSecretName,
				))
			}
			return controllers.RequeueIfError(errors.Wrap(err, "failed to verify node secret"))
		}
	}

	// Migration completion check: All pods are migrated when we've successfully created and
	// health-checked TotalPods number of nodes. We track this via MigratedNodes slice.
	if len(migration.MigratedNodes) >= int(migration.TotalPods) {
		logging.Infof(ctx, "All %d pods migrated (%d nodes healthy), moving to finalization",
			migration.TotalPods, len(migration.MigratedNodes))
		migration.Phase = PhaseFinalization
		migration.Message = fmt.Sprintf("All %d pods migrated successfully", migration.TotalPods)
		r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "AllPodsMigrated",
			"All %d pods migrated successfully", migration.TotalPods)
		return controllers.RequeueImmediately()
	}

	// Sanity check that the index is still in bounds. If someone manually scaled
	// the StatefulSet during migration, we could end up with an invalid index.
	if migration.CurrentPodIndex < 0 || migration.CurrentPodIndex >= migration.TotalPods {
		return controllers.RequeueIfError(errors.Errorf("migration index out of bounds (currentPodIndex=%d, totalPods=%d), cluster may have been modified during migration",
			migration.CurrentPodIndex, migration.TotalPods))
	}

	// Migrate one pod at a time (highest index first), using CurrentPodIndex as the target.
	state := podMigrationState{
		cluster:        cluster,
		migration:      migration,
		stsCtx:         stsCtx,
		targetNodeName: fmt.Sprintf("%s-%d", sts.Name, migration.CurrentPodIndex),
		targetIdx:      migration.CurrentPodIndex,
	}

	logging.Infof(ctx, "Migrating pod %s (index %d, progress: %d/%d)",
		state.targetNodeName, state.targetIdx, len(migration.MigratedNodes), migration.TotalPods)

	// Check if CrdbNode already exists (important for resume scenarios after operator restart)
	var node crdbv1beta1.CrdbNode
	if err = ctl.Get(ctx, state.targetNodeName, &node); err == nil {
		// Node exists - verify it's healthy before proceeding to next pod
		return r.verifyNodeHealthAndProceed(ctx, ctl, sts, &node, state)
	}

	// Node doesn't exist yet. Create it by scaling down the STS and then creating a CrdbNode.
	return r.createNodeAndScaleDown(ctx, ctl, sts, discovery.V1Alpha1Cluster, state)
}

// verifyNodeHealthAndProceed verifies a node is healthy and advances to the next migration step.
func (r *Reconciler) verifyNodeHealthAndProceed(
	ctx context.Context,
	ctl *kube.Ctl,
	sts *appsv1.StatefulSet,
	node *crdbv1beta1.CrdbNode,
	state podMigrationState,
) (ctrl.Result, error) {
	migration := state.migration
	targetNodeName := state.targetNodeName
	targetIdx := state.targetIdx

	if err := r.performComprehensiveHealthCheck(ctx, ctl, node, sts.Name, migration.TotalPods); err != nil {
		// Check if we've been stuck on this node for too long (10 minutes).
		// Prevents infinite waiting on nodes that will never become healthy.
		if migration.NodeMigrationStartTime != nil {
			stuckDuration := time.Since(migration.NodeMigrationStartTime.Time)
			if stuckDuration > 10*time.Minute {
				// Log pod status for debugging.
				var pod corev1.Pod
				if podErr := ctl.Get(ctx, targetNodeName, &pod); podErr == nil {
					logging.Debugf(ctx, "Pod %s status after %v: Phase=%s", targetNodeName, stuckDuration, pod.Status.Phase)
					for _, cs := range pod.Status.ContainerStatuses {
						logging.Debugf(ctx, "Container %s: Ready=%v, State=%+v", cs.Name, cs.Ready, cs.State)
					}
					for _, ics := range pod.Status.InitContainerStatuses {
						logging.Debugf(ctx, "InitContainer %s: Ready=%v, State=%+v", ics.Name, ics.Ready, ics.State)
					}
					for _, cond := range pod.Status.Conditions {
						if cond.Status != corev1.ConditionTrue {
							logging.Debugf(ctx, "Condition %s=%s: %s", cond.Type, cond.Status, cond.Message)
						}
					}
				}

				logging.Errorf(ctx, "Node %s not healthy after %v, pausing migration", targetNodeName, stuckDuration)
				stoppedMsg := fmt.Sprintf("Node %s not healthy after %v. Set label to 'start' to resume or remove it to rollback.",
					targetNodeName, stuckDuration)
				stoppedErrMsg := fmt.Sprintf("health check timeout on %s", targetNodeName)
				// Persist PhaseStopped before returning. Without this an operator restart
				// loses the stopped phase, and the health check loop restarts from scratch.
				if err := persistMigrationStatus(ctx, ctl, state.cluster, func(m *crdbv1beta1.MigrationStatus) {
					m.Phase = PhaseStopped
					m.Message = stoppedMsg
					m.LastError = stoppedErrMsg
				}, PhasePodMigration, migration.Type); err != nil {
					return controllers.RequeueIfError(errors.Wrap(err, "failed to persist PhaseStopped status"))
				}
				return controllers.NoRequeue()
			}
		}

		logging.Infof(ctx, "Health check pending for %s: %v", targetNodeName, err)
		migration.Message = fmt.Sprintf("Waiting for %s health", targetNodeName)
		return controllers.RequeueAfter(10*time.Second, nil)
	}

	logging.Infof(ctx, "Node %s healthy", targetNodeName)

	if migration.MigratedNodes == nil {
		migration.MigratedNodes = []string{}
	}
	if !slices.Contains(migration.MigratedNodes, targetNodeName) {
		migration.MigratedNodes = append(migration.MigratedNodes, targetNodeName)
	}

	migration.NodeMigrationStartTime = nil
	// Move to the next pod index. StatefulSet scale-down for the next pod happens in createNodeAndScaleDown.
	migration.Message = fmt.Sprintf("Node %s healthy, proceeding to next pod", targetNodeName)
	migration.CurrentPodIndex = targetIdx - 1
	return controllers.RequeueImmediately()
}

// drainSTSPodForMigration issues cockroach node drain on the target StatefulSet pod
// before migration scales it down.
//
// During migration, old STS pods and new CrdbNode pods are members of the same CRDB
// cluster, so data is always available. The risk is connections. When K8s scales down
// the STS, it removes the pod from the endpoint slice before sending SIGTERM. New
// connections stop routing here, but existing TCP connections stay open until SIGTERM
// arrives. CRDB's SIGTERM handler then starts draining, but it only completes if
// terminationGracePeriodSeconds on the source StatefulSet is long enough. We do not
// control that value.
//
// cockroach node drain marks the node as draining in gossip and:
//   - Transfers range leases to other nodes so reads and writes continue without
//     waiting for SIGTERM-triggered drain to finish.
//   - Signals connected SQL clients to reconnect elsewhere. CRDB drivers retry
//     transient connection failures automatically, keeping the disruption window short.
//   - Lets the pod terminate quickly once SIGTERM arrives since CRDB is already drained.
//
// Failures are non-fatal. The pod still self-drains when SIGTERM arrives.
func drainSTSPodForMigration(
	ctx context.Context, ctl *kube.Ctl, podName string, stsCtx *migrationutil.ParsedMigrationContext,
) error {
	grpcPort := stsCtx.GRPCPort
	if grpcPort == 0 {
		grpcPort = migrationutil.DefaultGRPCPort
	}
	return drainLocalNode(ctx, ctl, podName, migrationutil.CrdbContainerName, grpcPort, stsCtx.TLSEnabled)
}

// createNodeAndScaleDown scales down the StatefulSet to delete the target pod,
// waits for the pod to be gone (to avoid controller ownership conflicts),
// then creates the CrdbNode.
func (r *Reconciler) createNodeAndScaleDown(
	ctx context.Context,
	ctl *kube.Ctl,
	sts *appsv1.StatefulSet,
	v1alpha1Cluster *crdbv1alpha1.CrdbCluster,
	state podMigrationState,
) (ctrl.Result, error) {
	migration := state.migration
	targetNodeName := state.targetNodeName
	targetIdx := state.targetIdx

	// Scale down the StatefulSet to delete the target pod if it hasn't been removed yet.
	currentReplicas := int32(0)
	if sts.Spec.Replicas != nil {
		currentReplicas = *sts.Spec.Replicas
	}
	if currentReplicas > targetIdx {
		// Drain the target pod before scaling the StatefulSet down. See drainSTSPodForMigration
		// for the full rationale. Failures are non-fatal: the pod self-drains via SIGTERM.
		if err := drainSTSPodForMigration(ctx, ctl, targetNodeName, state.stsCtx); err != nil {
			logging.Warnf(ctx, "Pre-drain of %s failed (proceeding with scale-down): %v", targetNodeName, err)
		} else {
			logging.Infof(ctx, "Drained %s before scale-down", targetNodeName)
		}

		newReplicas := targetIdx
		sts.Spec.Replicas = &newReplicas
		logging.Infof(ctx, "Scaling down StatefulSet to %d to delete pod %s", newReplicas, targetNodeName)
		migration.Message = fmt.Sprintf("Scaling down StatefulSet to delete %s", targetNodeName)
		// We use NoWait here because the StatefulSet won't report as ready
		// until pods finish terminating. The next requeue checks for that.
		if err := ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, sts); err != nil {
			return controllers.RequeueIfError(errors.Wrap(err, "scale down failed"))
		}
		return controllers.RequeueAfter(5*time.Second, nil)
	}

	// Wait for the pod to be deleted. The existing pod is owned by the StatefulSet
	// controller; nodectrl cannot set ownerRef until it's gone.
	var existingPod corev1.Pod
	if err := ctl.Get(ctx, targetNodeName, &existingPod); err == nil {
		logging.Infof(ctx, "Waiting for pod %s to be deleted before creating CrdbNode", targetNodeName)
		migration.Message = fmt.Sprintf("Waiting for pod %s deletion", targetNodeName)
		return controllers.RequeueAfter(5*time.Second, nil)
	} else if !apierrors.IsNotFound(err) {
		return controllers.RequeueIfError(err)
	}

	// Create the CrdbNode. For operator migrations, v1alpha1Cluster fields take precedence.
	spec := buildNodeSpec(sts, "", state.stsCtx, v1alpha1Cluster)
	newNode := &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      targetNodeName,
			Namespace: sts.Namespace,
			Labels: map[string]string{
				manifests.BetaCrdbClusterNodeOwnerLabelKey: sts.Name,
				"app": "cockroachdb",
			},
			Annotations: map[string]string{
				LabelMigrationStatus: "true",
			},
		},
		Spec: spec,
	}

	logging.Infof(ctx, "Creating CrdbNode %s", targetNodeName)
	if err := ctl.Create(ctx, newNode); err != nil {
		return controllers.RequeueIfError(errors.Wrap(err, "create CrdbNode failed"))
	}

	r.Recorder.Eventf(state.cluster, corev1.EventTypeNormal, "PodMigrationStarted",
		"Created CrdbNode %s (%d/%d)", targetNodeName, len(migration.MigratedNodes)+1, migration.TotalPods)

	// Track when we began this node's migration so we can detect stuck health checks.
	now := metav1.Now()
	migration.NodeMigrationStartTime = &now
	migration.Phase = PhasePodMigration
	migration.Message = fmt.Sprintf("Created %s, waiting for health check", targetNodeName)

	return controllers.RequeueImmediately()
}

// handleFinalizationPhase performs finalization work and waits for the user to
// delete the StatefulSet. Once the StatefulSet is gone, completeMigration
// persists Mode=MutableOnly, PhaseComplete, and the terminal label in order.
func (r *Reconciler) handleFinalizationPhase(
	ctx context.Context, ctl *kube.Ctl, discovery *DiscoveryResult, cluster *crdbv1beta1.CrdbCluster,
) (ctrl.Result, error) {
	migration := cluster.Status.Migration
	sts := discovery.StatefulSet

	// The StatefulSet is gone. Continue the ordered, idempotent ownership handoff.
	if sts == nil {
		return r.completeMigration(ctx, ctl, discovery, cluster)
	}

	// If finalization work already completed, just wait for STS deletion. The message
	// is set at the end of this function and persisted by the caller's UpdateStatus.
	// This avoids re-running all idempotent finalization steps and emitting duplicate
	// events every 30 seconds while waiting for the user to delete the STS.
	if strings.HasPrefix(migration.Message, "Finalization complete") {
		logging.Infof(ctx, "Finalization already done, waiting for StatefulSet %s deletion", sts.Name)
		return controllers.RequeueAfter(30*time.Second, nil)
	}

	logging.Infof(ctx, "Starting finalization (%s migration)", migration.Type)

	// Validate all CrdbNodes actually exist before proceeding. This catches cases
	// where status was manually modified or nodes were deleted during migration.
	actualNodes, err := listClusterCrdbNodes(ctx, ctl, cluster.Name)
	if err != nil {
		return controllers.RequeueIfError(errors.Wrap(err, "failed to list CrdbNodes for validation"))
	}
	if len(actualNodes) != int(migration.TotalPods) {
		return controllers.RequeueIfError(errors.Errorf("CrdbNode count mismatch: expected %d, found %d, cannot finalize",
			migration.TotalPods, len(actualNodes)))
	}

	stsCtx, err := buildPhaseContext(ctx, ctl, sts, migration.Type)
	if err != nil {
		return controllers.RequeueIfError(err)
	}

	// Re-detect Ingress for Helm migrations. Ingress was detected in CertMigration
	// and stored on a local stsCtx that is discarded between phases. Finalization needs
	// it to annotate the CrdbCluster.
	if migration.Type == MigrationTypeHelm {
		detectAndApplyIngress(ctx, ctl, discovery, stsCtx)
	}

	// Delete the old PDB. deletePodDisruptionBudget handles both naming conventions
	// (Helm: {name}-budget, Operator: {name}). The CockroachDB Operator PDB ({name}-pdb)
	// is applied immediately below to avoid a protection gap.
	if err = deletePodDisruptionBudget(ctx, ctl, sts.Name); err != nil {
		logging.Warnf(ctx, "PDB deletion failed (non-fatal): %v", err)
	}

	// The enterprise operator uses LocalityMappings (not LocalityLabels) to derive
	// CRDB locality tiers from K8s node topology labels. After migration completes,
	// the user should configure LocalityMappings on the CrdbCluster spec to match
	// their topology requirements.
	if len(stsCtx.LocalityLabels) > 0 {
		logging.Infof(ctx, "Source cluster uses locality labels: %v", stsCtx.LocalityLabels)
	}

	// For Helm migrations, update the CrdbCluster spec now (without Mode=MutableOnly).
	// Mode is set later in completeMigration after the STS is deleted.
	if migration.Type == MigrationTypeHelm {
		if err = finalizeHelmSpec(ctx, ctl, sts, stsCtx, migration, cluster, r.CloudRegion); err != nil {
			return controllers.RequeueIfError(err)
		}
		// finalizeHelmSpec replaces *cluster in-place; refresh migration pointer.
		migration = cluster.Status.Migration
	}

	// Apply the CockroachDB Operator PDB so pods are protected while waiting for
	// the user to delete the STS.
	if err = ensureEnterprisePDB(ctx, ctl, cluster, migration.TotalPods); err != nil {
		return controllers.RequeueIfError(err)
	}

	// Mark the cluster as initialized before the user deletes the STS. Once
	// Mode=MutableOnly is set in completeMigration, betaclusterctrl starts
	// reconciling and will try cockroach init if this condition is missing.
	// Setting it here avoids a cache race where betaclusterctrl sees the Mode
	// change but the ClusterInitialized status update hasn't synced yet.
	if !cluster.IsInitialized() {
		cluster.Status.SetCondition(crdbv1beta1.ClusterInitialized, metav1.ConditionTrue)
		if err := ctl.UpdateStatus(ctx, cluster); err != nil {
			if apierrors.IsConflict(err) {
				return controllers.RequeueImmediately()
			}
			return controllers.RequeueIfError(errors.Wrap(err, "failed to set ClusterInitialized condition"))
		}
	}

	// All finalization work is done. Wait for the user to delete the STS. The
	// delete watch event triggers a reconcile that re-enters this function with
	// sts=nil, which calls completeMigration. RequeueAfter is a safety net in
	// case the informer misses the delete event.
	migration.Message = "Finalization complete. Delete the StatefulSet to mark migration complete."
	r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "FinalizationComplete",
		"PDB applied. Delete StatefulSet %s to complete migration.", sts.Name)
	logging.Infof(ctx, "StatefulSet %s left intact; user must delete it to complete migration", sts.Name)

	return controllers.RequeueAfter(30*time.Second, nil)
}

// completeMigration hands cluster ownership to betaclusterctrl with three ordered writes.
// It first persists Mode=MutableOnly, then PhaseComplete, and finally the terminal label.
// Each partial state can re-enter this function, while the final label stops further retries.
func (r *Reconciler) completeMigration(
	ctx context.Context, ctl *kube.Ctl, discovery *DiscoveryResult, cluster *crdbv1beta1.CrdbCluster,
) (ctrl.Result, error) {
	migration := cluster.Status.Migration
	if migration == nil {
		logging.Errorf(ctx, "completeMigration called with nil migration status")
		return controllers.RequeueIfError(errors.New("migration status is nil, cannot complete"))
	}
	migrationType := migration.Type
	logging.Info(ctx, "StatefulSet deleted, completing migration")

	// ClusterInitialized was already set during finalization to avoid a cache
	// race with betaclusterctrl. Double-check here as a safety net. If this
	// update conflicts, requeue so the condition is persisted before Mode is
	// set. Without this, betaclusterctrl may see Mode=MutableOnly without
	// the initialized condition and attempt cockroach init on an already-running
	// cluster.
	if !cluster.IsInitialized() {
		cluster.Status.SetCondition(crdbv1beta1.ClusterInitialized, metav1.ConditionTrue)
		if err := ctl.UpdateStatus(ctx, cluster); err != nil {
			if apierrors.IsConflict(err) {
				return controllers.RequeueImmediately()
			}
			return controllers.RequeueIfError(errors.Wrap(err, "failed to set ClusterInitialized condition"))
		}
	}

	// Sync region annotations into spec.Regions before setting Mode. Once Mode is
	// MutableOnly, the validating webhook enforces region validity. If spec.Regions
	// still has stale defaults from conversion, the mode update would be rejected.
	if _, err := syncRegionAnnotations(ctx, ctl, cluster); err != nil {
		return controllers.RequeueIfError(errors.Wrap(err, "failed to sync region annotations"))
	}

	// Finalization normally creates this PDB before the StatefulSet is deleted.
	// Ensure it again so an early deletion or a partial handoff cannot leave the
	// cluster unprotected after Mode becomes MutableOnly.
	if err := ensureEnterprisePDB(ctx, ctl, cluster, migration.TotalPods); err != nil {
		return controllers.RequeueAfter(5*time.Second,
			errors.Wrap(err, "failed to ensure PDB before migration handoff"))
	}

	// Public-operator objects retain v1alpha1 field ownership after conversion. Clear it
	// once so an SSA manager can adopt the v1beta1 object. Checking the stored API version
	// also repairs partial handoffs from older binaries.
	if migrationType == MigrationTypeOperator && hasV1Alpha1ManagedFields(cluster) {
		if err := ctl.ResetManagedFields(ctx, cluster); err != nil {
			return controllers.RequeueAfter(5*time.Second,
				errors.Wrap(err, "failed to reset managed fields before operator migration handoff"))
		}
	}

	// Set Mode before recording completion. If validation rejects the spec, the phase
	// stays at Finalization and the next reconcile retries this transition.
	if cluster.Spec.Mode == nil || *cluster.Spec.Mode != crdbv1beta1.MutableOnly {
		mode := crdbv1beta1.MutableOnly
		cluster.Spec.Mode = &mode
		if cluster.Annotations == nil {
			cluster.Annotations = make(map[string]string)
		}
		cluster.Annotations[crdbv1alpha1.BetaCrdbClusterModeAnnotationKey] = string(mode)
		// NoWait because ctl.Update blocks waiting for IsReady, which may not
		// succeed until betaclusterctrl completes its first full reconcile.
		if err := ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, cluster); err != nil {
			if apierrors.IsConflict(err) {
				return controllers.RequeueImmediately()
			}
			return controllers.RequeueIfError(errors.Wrap(err, "failed to set Mode=MutableOnly"))
		}
	}

	// UpdateWithOptions refreshes cluster from the response. Check that conversion did
	// not drop Mode before recording PhaseComplete.
	if cluster.Spec.Mode == nil || *cluster.Spec.Mode != crdbv1beta1.MutableOnly {
		return controllers.RequeueAfter(5*time.Second,
			errors.New("mode did not persist as MutableOnly after update; retrying completion"))
	}

	// Mode is durable now. Status conflicts are retried inside persistMigrationStatus.
	now := metav1.Now()
	if err := persistMigrationStatus(ctx, ctl, cluster, func(m *crdbv1beta1.MigrationStatus) {
		m.Phase = PhaseComplete
		m.Message = "Migration complete."
		m.LastUpdateTime = &now
	}, PhaseFinalization, migrationType); err != nil {
		return controllers.RequeueAfter(5*time.Second, errors.Wrap(err, "failed to persist PhaseComplete"))
	}

	// Write the terminal label last. The protected-cluster guard rejects the old
	// v1alpha1 source now that its StatefulSet is gone.
	if cluster.Labels == nil {
		cluster.Labels = make(map[string]string)
	}
	if cluster.Labels[LabelMigrate] != MigrateComplete || cluster.Labels[LabelMigrationStatus] != MigrateComplete {
		cluster.Labels[LabelMigrationStatus] = MigrateComplete
		cluster.Labels[LabelMigrate] = MigrateComplete
		if err := ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, cluster); err != nil {
			if apierrors.IsConflict(err) {
				return controllers.RequeueImmediately()
			}
			return controllers.RequeueIfError(errors.Wrap(err, "failed to set completion label"))
		}
	}

	// Helm keeps the trigger label on the StatefulSet. Operator migration already
	// wrote it to the v1beta1 object above.
	if migrationType == MigrationTypeHelm && discovery != nil {
		if err := updateLabelValue(ctx, ctl, discovery, MigrateComplete); err != nil {
			logging.Warnf(ctx, "Failed to update source label to complete: %v", err)
		}
	}

	// Clean up the saved-ports annotation now that migration is done.
	removeOriginalPortsAnnotation(ctx, ctl, cluster.Name)

	logging.Infof(ctx, "Migration complete for cluster %s", cluster.Name)
	r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "MigrationComplete", "Migration to v1beta1 complete")
	return controllers.NoRequeue()
}

func ensureEnterprisePDB(
	ctx context.Context, ctl *kube.Ctl, cluster *crdbv1beta1.CrdbCluster, nodes int32,
) error {
	budget := 1
	if cluster.Spec.DisruptionBudget != nil {
		budget = *cluster.Spec.DisruptionBudget
	}
	pdb := manifests.CrdbClusterOperatorPDB(cluster, int(nodes), budget)
	if err := ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, pdb); err != nil {
		return errors.Wrap(err, "failed to apply CockroachDB Operator PDB")
	}
	return nil
}

func hasV1Alpha1ManagedFields(cluster *crdbv1beta1.CrdbCluster) bool {
	for _, entry := range cluster.ManagedFields {
		if entry.APIVersion == crdbv1alpha1.SchemeGroupVersion.String() {
			return true
		}
	}
	return false
}

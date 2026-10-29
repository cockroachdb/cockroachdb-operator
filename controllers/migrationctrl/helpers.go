package migrationctrl

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/cockroachdb/errors"
	crdbv1alpha1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1alpha1"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/migrationutil"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// nodeSpecFields holds the mutable fields used to construct a CrdbNodeSpec. It is
// populated from the parsed STS context and optionally overridden from a v1alpha1 CR.
type nodeSpecFields struct {
	grpcPort               int32
	sqlPort                int32
	httpPort               int32
	loggingConfigMap       string
	image                  string
	terminationGracePeriod *int64
}

// applyV1Alpha1Overrides updates fields from the v1alpha1 CR spec when the corresponding
// values are present. Port fields and image are preferred from the CR because the STS
// may reflect older or defaulted values. TerminationGracePeriodSecs is always taken from
// the CR since the v1alpha1 type requires it (it is never nil).
func applyV1Alpha1Overrides(cluster *crdbv1alpha1.CrdbCluster, fields *nodeSpecFields) {
	if cluster.Spec.GRPCPort != nil {
		fields.grpcPort = *cluster.Spec.GRPCPort
	}
	if cluster.Spec.SQLPort != nil {
		fields.sqlPort = *cluster.Spec.SQLPort
	}
	if cluster.Spec.HTTPPort != nil {
		fields.httpPort = *cluster.Spec.HTTPPort
	}
	if cluster.Spec.Image != nil && cluster.Spec.Image.Name != "" {
		fields.image = cluster.Spec.Image.Name
	}
	if cluster.Spec.LogConfigMap != "" {
		fields.loggingConfigMap = cluster.Spec.LogConfigMap
	}
	fields.terminationGracePeriod = &cluster.Spec.TerminationGracePeriodSecs
}

// buildNodeSpec creates a CrdbNodeSpec based on the STS pod template and parsed context.
// For operator migrations, v1alpha1Cluster is non-nil and its fields take precedence
// over the STS-parsed values. Pass nil for Helm migrations.
func buildNodeSpec(
	sts *appsv1.StatefulSet,
	nodeName string,
	pc *migrationutil.ParsedMigrationContext,
	v1alpha1Cluster *crdbv1alpha1.CrdbCluster,
) crdbv1beta1.CrdbNodeSpec {
	pvcName := pc.PVCTemplateName
	if pvcName == "" {
		pvcName = "datadir"
	}

	// Start with values from the parsed STS context.
	fields := nodeSpecFields{
		grpcPort:               pc.GRPCPort,
		sqlPort:                pc.SQLPort,
		httpPort:               pc.HTTPPort,
		loggingConfigMap:       pc.LoggingConfigMap,
		image:                  pc.Image,
		terminationGracePeriod: sts.Spec.Template.Spec.TerminationGracePeriodSeconds,
	}

	// Override with v1alpha1 CR values for public operator migrations.
	if v1alpha1Cluster != nil {
		applyV1Alpha1Overrides(v1alpha1Cluster, &fields)
	}

	serviceAccountName := pc.ServiceAccountName
	if serviceAccountName == "" {
		serviceAccountName = sts.Spec.Template.Spec.ServiceAccountName
	}

	// Find the data VolumeClaimTemplate by name. Both Helm and the public operator place the
	// data volume first. Matching by name is safer than assuming index 0.
	var dataStore crdbv1beta1.DataStore
	for i := range sts.Spec.VolumeClaimTemplates {
		if sts.Spec.VolumeClaimTemplates[i].Name == pvcName {
			dataStore = crdbv1beta1.DataStore{
				VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{Name: pvcName},
					Spec:       sts.Spec.VolumeClaimTemplates[i].Spec,
				},
			}
			break
		}
	}

	// Find the CRDB container by name rather than assuming index 0. The Helm chart names
	// it "db", so we search for that. If not found, fall back to the first container.
	var containerResources corev1.ResourceRequirements
	var imagePullPolicy corev1.PullPolicy
	for i := range sts.Spec.Template.Spec.Containers {
		if sts.Spec.Template.Spec.Containers[i].Name == migrationutil.CrdbContainerName {
			containerResources = sts.Spec.Template.Spec.Containers[i].Resources
			imagePullPolicy = sts.Spec.Template.Spec.Containers[i].ImagePullPolicy
			break
		}
	}
	if containerResources.Limits == nil && containerResources.Requests == nil && len(sts.Spec.Template.Spec.Containers) > 0 {
		containerResources = sts.Spec.Template.Spec.Containers[0].Resources
		if imagePullPolicy == "" {
			imagePullPolicy = sts.Spec.Template.Spec.Containers[0].ImagePullPolicy
		}
	}

	// Build the CrdbNodeSpec from the parsed STS context and optional v1alpha1 overrides.
	spec := crdbv1beta1.CrdbNodeSpec{
		NodeName: nodeName,
		PodTemplate: &crdbv1beta1.PodTemplateSpec{
			Metadata: crdbv1beta1.PodMeta{
				Labels:      sts.Spec.Template.Labels,
				Annotations: sts.Spec.Template.Annotations,
			},
			Spec: corev1.PodSpec{
				ImagePullSecrets: sts.Spec.Template.Spec.ImagePullSecrets,
				Containers: []corev1.Container{
					{
						Image:           fields.image,
						Name:            manifests.CRDBContainerName,
						Resources:       containerResources,
						Env:             pc.EnvVars, // already includes HOST_IP and GODEBUG from parser
						ImagePullPolicy: imagePullPolicy,
					},
				},
				ServiceAccountName:            serviceAccountName,
				Affinity:                      sts.Spec.Template.Spec.Affinity,
				NodeSelector:                  sts.Spec.Template.Spec.NodeSelector,
				PriorityClassName:             sts.Spec.Template.Spec.PriorityClassName,
				Tolerations:                   sts.Spec.Template.Spec.Tolerations,
				TerminationGracePeriodSeconds: fields.terminationGracePeriod,
				TopologySpreadConstraints:     sts.Spec.Template.Spec.TopologySpreadConstraints,
			},
		},
		StartFlags:           pc.StartFlags,
		DataStore:            dataStore,
		TLSEnabled:           pc.TLSEnabled,
		Domain:               pc.Domain, // empty means cluster.local (default); non-empty for custom-domain clusters
		LoggingConfigMapName: fields.loggingConfigMap,
		Image:                fields.image,
		GRPCPort:             &fields.grpcPort,
		SQLPort:              &fields.sqlPort,
		HTTPPort:             &fields.httpPort,
		PersistentVolumeClaimRetentionPolicy: &crdbv1beta1.CrdbNodePersistentVolumeClaimRetentionPolicy{
			WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		},
	}

	// Only configure ExternalCertificates when TLS is enabled. Insecure clusters
	// use --insecure and don't need cert volumes, secrets, or the cert-reloader sidecar.
	if pc.TLSEnabled {
		spec.Certificates = migrationutil.BuildExternalCertificates(pc)
	}

	// Add LogsStore if a dedicated logsdir/logs-dir VolumeClaimTemplate is present on the STS.
	// The mount path is read from the container's VolumeMount for accuracy; if absent, the
	// operator's default (/cockroach/cockroach-data/logs) is used.
	for _, vct := range sts.Spec.VolumeClaimTemplates {
		if vct.Name == "logsdir" || vct.Name == "logs-dir" {
			mountPath := ""
			for _, c := range sts.Spec.Template.Spec.Containers {
				for _, vm := range c.VolumeMounts {
					if vm.Name == vct.Name {
						mountPath = vm.MountPath
						break
					}
				}
				if mountPath != "" {
					break
				}
			}
			spec.LogsStore = &crdbv1beta1.CrdbLogsStoreSpec{
				VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{Name: vct.Name},
					Spec:       vct.Spec,
				},
				MountPath: mountPath,
			}
			break
		}
	}

	// InitContainers, volumes, mounts, and security context handling differs
	// between Helm and operator migrations.
	//
	// Helm deployments may use a custom UID, so we preserve their SecurityContext
	// and backfill FSGroup if missing. The v1alpha1 API does not support init
	// containers or custom volumes, so only Helm StatefulSets will have them.
	//
	// For operator migrations the enterprise operator's default SecurityContext
	// with UID/GID/FSGroup 1000 is correct. The public operator runs pods as root
	// (uid 0), so the data directory is owned by root. We add an init container to
	// chown it to 1000:1000 before the main container starts. Without this, platforms
	// that don't fully support fsGroupChangePolicy OnRootMismatch will fail with
	// permission denied on the data directory.
	if v1alpha1Cluster == nil {
		if pc.PodSecurityContext != nil {
			spec.PodTemplate.Spec.SecurityContext = pc.PodSecurityContext
			if spec.PodTemplate.Spec.SecurityContext.FSGroup == nil {
				gid := int64(1000)
				spec.PodTemplate.Spec.SecurityContext.FSGroup = &gid
			}
		}
		spec.PodTemplate.Spec.InitContainers = pc.InitContainers
		spec.PodTemplate.Spec.Volumes = pc.CustomVolumes
		// Containers[0] is the cockroachdb container initialized above in this function's
		// struct literal; it is always present and always the only container at this point.
		spec.PodTemplate.Spec.Containers[0].VolumeMounts = pc.CustomVolumeMounts
	} else {
		// The public operator runs as root (uid 0). The enterprise operator switches
		// to uid 1000. An init container chowns the data directory so the non-root
		// cockroach process can access files created under the previous ownership.
		rootUID := int64(0)
		spec.PodTemplate.Spec.InitContainers = []corev1.Container{
			{
				Name:    "chown-data-dir",
				Image:   fields.image,
				Command: []string{"chown", "-R", "1000:1000", manifests.CockroachDataDirMountAbsPath},
				VolumeMounts: []corev1.VolumeMount{
					{Name: manifests.CockroachDataDirVolumeName, MountPath: manifests.CockroachDataDirMountAbsPath},
				},
				SecurityContext: &corev1.SecurityContext{
					RunAsUser: &rootUID,
				},
			},
		}
	}

	// Add WAL failover spec if detected
	if pc.WALFailoverSpec != nil {
		spec.WALFailoverSpec = pc.WALFailoverSpec
	}

	// Add Physical Cluster Replication (PCR) spec if detected
	if pc.VirtualClusterSpec != nil {
		spec.VirtualCluster = pc.VirtualClusterSpec
	}

	// LocalityLabels are the tier keys from the --locality flag (e.g. ["region", "zone"]).
	// This field is deprecated in favor of LocalityMappings. Clusters with custom node
	// labels need to update LocalityMappings on the CrdbCluster after migration.
	spec.LocalityLabels = pc.LocalityLabels

	return spec
}

// refetchClusterMigration re-fetches the cluster from the API server and returns
// the refreshed migration status pointer. If the stored status is nil after the
// re-fetch, it initializes one with the given phase and type so the caller can
// continue without a nil check.
//
// Call this after any operation that updates a v1alpha1 resource (which bumps the
// v1beta1 resourceVersion via the conversion webhook) or after ctl.Update which
// replaces the in-memory cluster object and stale-ifies any previously derived
// migration pointer.
func refetchClusterMigration(
	ctx context.Context,
	ctl *kube.Ctl,
	cluster *crdbv1beta1.CrdbCluster,
	fallbackPhase, fallbackType string,
) (*crdbv1beta1.MigrationStatus, error) {
	if err := ctl.Get(ctx, cluster.Name, cluster); err != nil {
		return nil, errors.Wrap(err, "failed to re-fetch cluster")
	}
	if cluster.Status.Migration == nil {
		cluster.Status.Migration = &crdbv1beta1.MigrationStatus{
			Phase: fallbackPhase,
			Type:  fallbackType,
		}
	}
	return cluster.Status.Migration, nil
}

// persistMigrationStatus retries UpdateStatus up to maxRetries times, re-fetching
// the cluster on conflict and re-applying the status mutator before each retry.
// This is critical because other controllers (betaclusterctrl) can overwrite the
// entire status sub-resource between our fetch and our update, and a simple
// requeue loses the in-memory migration state.
func persistMigrationStatus(
	ctx context.Context,
	ctl *kube.Ctl,
	cluster *crdbv1beta1.CrdbCluster,
	mutate func(migration *crdbv1beta1.MigrationStatus),
	fallbackPhase, fallbackType string,
) error {
	const maxRetries = 3
	for attempt := range maxRetries {
		if attempt > 0 {
			// Re-fetch to get a fresh resourceVersion and the latest status.
			migration, err := refetchClusterMigration(ctx, ctl, cluster, fallbackPhase, fallbackType)
			if err != nil {
				return err
			}
			mutate(migration)
		} else {
			if cluster.Status.Migration == nil {
				cluster.Status.Migration = &crdbv1beta1.MigrationStatus{
					Phase: fallbackPhase,
					Type:  fallbackType,
				}
			}
			mutate(cluster.Status.Migration)
		}

		err := ctl.UpdateStatus(ctx, cluster)
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return errors.Wrap(err, "failed to persist migration status")
		}
		logging.Warnf(ctx, "Status update conflict (attempt %d/%d), retrying", attempt+1, maxRetries)
	}
	return errors.New("failed to persist migration status after retries")
}

// extractContextFromSTS parses the StatefulSet command to extract flags, ports, and other migration context.
func extractContextFromSTS(sts *appsv1.StatefulSet) (*migrationutil.ParsedMigrationContext, error) {
	// Delegate to the shared parser so conversion and migration stay in sync.
	ctx, err := migrationutil.ParseStatefulSet(sts, sts.Name)
	if err != nil {
		return nil, err
	}

	// Default HTTP port if not detected.
	if ctx.HTTPPort == 0 {
		ctx.HTTPPort = migrationutil.DefaultHTTPPort
	}
	// Default SQL port if not detected (--port / --sql-addr were absent in the STS args).
	// Without this a zero SQLPort becomes &int32(0) in CrdbNodeSpec, and health.go then
	// calls "cockroach sql --port=0" because the pointer is non-nil.
	if ctx.SQLPort == 0 {
		ctx.SQLPort = migrationutil.DefaultSQLPort
	}

	return ctx, nil
}

// generateV1Beta1ClusterSpec creates the spec for the final cluster object.
// For multi-region clusters, set the crdb.cockroachlabs.com/migration-regions annotation on the
// source StatefulSet to a JSON-encoded []CrdbClusterRegion before starting migration.
// Without it, only the local region (from --locality) is used.
// originalReplicas is used as a fallback when STS.Spec.Replicas is nil (e.g. during scale-down).
func generateV1Beta1ClusterSpec(
	ctx context.Context,
	sts *appsv1.StatefulSet,
	stsCtx *migrationutil.ParsedMigrationContext,
	originalReplicas int32,
	cloudRegion string,
) crdbv1beta1.CrdbClusterSpec {
	replicas := originalReplicas
	if sts.Spec.Replicas != nil {
		replicas = *sts.Spec.Replicas
	}
	if replicas == 0 {
		replicas = originalReplicas
	}

	nodeSpec := buildNodeSpec(sts, "", stsCtx, nil)

	// Check for user-provided multi-region topology annotation first. If present and
	// valid, use it verbatim since it defines the full cluster topology.
	var regions []crdbv1beta1.CrdbClusterRegion
	if regionsJSON, ok := sts.Annotations[AnnotationMigrationRegions]; ok && regionsJSON != "" {
		if err := json.Unmarshal([]byte(regionsJSON), &regions); err != nil || len(regions) == 0 {
			logging.Warnf(ctx, "Failed to parse %s annotation, falling back to single region: %v",
				AnnotationMigrationRegions, err)
			regions = nil
		}
	}

	// For single-region clusters, resolve the region code from annotations, the locality
	// flag, or the operator's own cloud region. The webhook requires that the operator's
	// CloudRegion is present in the cluster's Regions, so using it as the fallback
	// guarantees the CrdbCluster passes validation.
	if len(regions) == 0 {
		regionCode := ""
		// Check the regionCode annotation (same pattern as v1alpha1 conversion).
		if code, ok := sts.Annotations[crdbv1alpha1.BetaCrdbClusterNodeRegionCodeAnnotationKey]; ok && code != "" {
			regionCode = code
		}
		// Fall back to --locality=region=<value> from the STS args.
		if regionCode == "" {
			regionCode = stsCtx.Region
		}
		// Fall back to the operator's own cloud region.
		if regionCode == "" {
			regionCode = cloudRegion
		}
		if regionCode == "" {
			regionCode = migrationutil.DefaultRegion
		}
		regions = []crdbv1beta1.CrdbClusterRegion{
			{Code: regionCode, Nodes: replicas, Namespace: sts.Namespace},
		}
	}

	return crdbv1beta1.CrdbClusterSpec{
		Template: crdbv1beta1.CrdbNodeTemplate{
			Spec: nodeSpec,
		},
		Regions:    regions,
		TLSEnabled: stsCtx.TLSEnabled,
	}
}

// moveConfigMapKey renames the logging config key from "logging.yaml" to "logs.yaml".
// The public operator uses "logging.yaml" while the CockroachDB Operator expects "logs.yaml".
func moveConfigMapKey(ctx context.Context, ctl *kube.Ctl, configMapName string) error {
	logging.Infof(ctx, "Checking ConfigMap %s for key migration", configMapName)

	cm := &corev1.ConfigMap{}
	if err := ctl.Get(ctx, configMapName, cm); err != nil {
		if apierrors.IsNotFound(err) {
			logging.Infof(ctx, "ConfigMap %s not found, skipping key migration", configMapName)
			return nil
		}
		return errors.Wrap(err, "failed to get ConfigMap")
	}

	// Check if migration is needed
	needsUpdate := false
	if data, ok := cm.Data["logging.yaml"]; ok {
		// public operator uses "logging.yaml", CockroachDB Operator uses "logs.yaml"
		cm.Data["logs.yaml"] = data
		delete(cm.Data, "logging.yaml")
		needsUpdate = true
		logging.Infof(ctx, "Migrating ConfigMap key: logging.yaml → logs.yaml")
	}

	if needsUpdate {
		if err := ctl.Update(ctx, cm); err != nil {
			return errors.Wrap(err, "failed to update ConfigMap")
		}
		logging.Infof(ctx, "ConfigMap %s key successfully migrated", configMapName)
	} else {
		logging.Infof(ctx, "ConfigMap %s already uses correct key format", configMapName)
	}

	return nil
}

// reverseConfigMapKeyMove renames the logging config key from "logs.yaml" back to "logging.yaml"
// during operator migration rollback. This undoes the moveConfigMapKey operation.
// logConfigMapName is the actual ConfigMap name from the v1alpha1 CR. When non-empty it is tried
// first. When empty (v1alpha1 CR unavailable), common naming patterns are tried as a fallback.
func reverseConfigMapKeyMove(
	ctx context.Context, ctl *kube.Ctl, clusterName, logConfigMapName string,
) error {
	candidates := []string{}
	if logConfigMapName != "" {
		candidates = append(candidates, logConfigMapName)
	}
	// Fall back to common naming patterns if the actual name wasn't provided or doesn't match.
	candidates = append(candidates, clusterName, fmt.Sprintf("%s-log-config", clusterName))

	for _, cmName := range candidates {
		cm := &corev1.ConfigMap{}
		if err := ctl.Get(ctx, cmName, cm); err != nil {
			continue
		}

		if data, ok := cm.Data["logs.yaml"]; ok {
			if _, hasOld := cm.Data["logging.yaml"]; !hasOld {
				cm.Data["logging.yaml"] = data
				delete(cm.Data, "logs.yaml")
				if err := ctl.Update(ctx, cm); err != nil {
					return errors.Wrapf(err, "failed to reverse ConfigMap key in %s", cmName)
				}
				logging.Infof(ctx, "Reversed ConfigMap key: logs.yaml -> logging.yaml in %s", cmName)
				return nil
			}
		}
	}

	return nil
}

// deleteHelmLogConfigMap deletes the log-config ConfigMap created by convertLogConfigSecretToConfigMap
// during Helm migration. If both a Secret and ConfigMap exist with the same name, the ConfigMap
// was created by us and should be removed to restore the original Secret as the sole source.
func deleteHelmLogConfigMap(ctx context.Context, ctl *kube.Ctl, sts *appsv1.StatefulSet) error {
	stsCtx, err := extractContextFromSTS(sts)
	if err != nil {
		// Can't determine the config name so nothing to clean up.
		return nil
	}
	if stsCtx.LoggingConfigMap == "" {
		return nil
	}

	// Only delete if both the original Secret and our created ConfigMap exist.
	// If only the ConfigMap exists, it might be the original (operator migration) and
	// should not be touched.
	secret := &corev1.Secret{}
	if err := ctl.Get(ctx, stsCtx.LoggingConfigMap, secret); err != nil {
		return nil // no original Secret found, leave ConfigMap alone
	}

	cm := &corev1.ConfigMap{}
	if err := ctl.Get(ctx, stsCtx.LoggingConfigMap, cm); err != nil {
		return nil // no ConfigMap to delete
	}

	if err := ctl.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "failed to delete migration log-config ConfigMap %s", stsCtx.LoggingConfigMap)
	}
	logging.Infof(ctx, "Deleted migration-created log-config ConfigMap %s (restoring original Secret)", stsCtx.LoggingConfigMap)
	return nil
}

// convertLogConfigSecretToConfigMap converts a log-config Secret to ConfigMap if needed.
// Helm charts use Secrets for log-config while the CockroachDB Operator expects ConfigMaps.
func convertLogConfigSecretToConfigMap(
	ctx context.Context, c *kube.Ctl, secretName, namespace string,
) error {
	if secretName == "" {
		return nil
	}

	// Check if ConfigMap already exists
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, secretName, cm); err == nil {
		logging.Infof(ctx, "Log config ConfigMap %s already exists", secretName)
		return nil
	}

	// Try to get the Secret
	secret := &corev1.Secret{}
	if err := c.Get(ctx, secretName, secret); err != nil {
		if apierrors.IsNotFound(err) {
			// Neither Secret nor ConfigMap exists - no log config to migrate
			logging.Info(ctx, "No log config Secret or ConfigMap found, skipping conversion")
			return nil
		}
		return errors.Wrapf(err, "failed to get log-config secret %s", secretName)
	}

	// Convert Secret data to ConfigMap data
	// Helm key: "log-config.yaml" → CockroachDB Operator key: "logs.yaml"
	configMapData := make(map[string]string)
	for key, value := range secret.Data {
		if key == "log-config.yaml" {
			configMapData["logs.yaml"] = string(value)
			logging.Infof(ctx, "Converting log-config.yaml → logs.yaml")
		} else {
			configMapData[key] = string(value)
		}
	}

	// Create ConfigMap with the same name as the Secret
	newCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		Data: configMapData,
	}

	if err := c.Create(ctx, newCM); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Race condition - another reconcile created it
			logging.Info(ctx, "ConfigMap was created by another reconcile, continuing")
			return nil
		}
		return errors.Wrap(err, "failed to create log-config ConfigMap")
	}

	logging.Infof(ctx, "Successfully converted log-config Secret %s to ConfigMap", secretName)
	return nil
}

// convertV1Alpha1IngressToAnnotation converts v1alpha1 Ingress config to annotation format.
// This preserves the Ingress configuration when migrating from the public operator.
func convertV1Alpha1IngressToAnnotation(ingress *crdbv1alpha1.IngressConfig) any {
	result := map[string]any{}

	if ingress.UI != nil {
		result["ui"] = map[string]any{
			"ingressClassName": ingress.UI.IngressClassName,
			"annotations":      ingress.UI.Annotations,
			"host":             ingress.UI.Host,
			"tls":              ingress.UI.TLS,
		}
	}

	if ingress.SQL != nil {
		result["sql"] = map[string]any{
			"ingressClassName": ingress.SQL.IngressClassName,
			"annotations":      ingress.SQL.Annotations,
			"host":             ingress.SQL.Host,
			"tls":              ingress.SQL.TLS,
		}
	}

	return result
}

// detectIngressFromHelm checks if an Ingress resource exists for the cluster
// and extracts configuration to preserve it. The v1beta1 API has no Ingress spec,
// so the config is stored as a JSON annotation on the CrdbCluster.
func detectIngressFromHelm(ctx context.Context, c *kube.Ctl, clusterName string) (any, error) {
	// Try to find an Ingress resource with a cluster name
	ingressName := fmt.Sprintf("%s-ingress", clusterName)
	ingress := &networkingv1.Ingress{}

	if err := c.Get(ctx, ingressName, ingress); err != nil {
		if apierrors.IsNotFound(err) {
			// No ingress found
			return nil, nil
		}
		return nil, errors.Wrap(err, "failed to get ingress")
	}

	// Found an ingress. Store the raw config rather than an IngressConfig struct to avoid
	// importing v1alpha1 types in the migration controller.
	ingressConfig := map[string]any{
		"ui": map[string]any{
			"ingressClassName": ingress.Spec.IngressClassName,
			"annotations":      ingress.Annotations,
		},
	}

	// Extract host from rules
	if len(ingress.Spec.Rules) > 0 && ingress.Spec.Rules[0].Host != "" {
		if ui, ok := ingressConfig["ui"].(map[string]any); ok {
			ui["host"] = ingress.Spec.Rules[0].Host
		}
	}

	// Extract TLS config
	if len(ingress.Spec.TLS) > 0 {
		if ui, ok := ingressConfig["ui"].(map[string]any); ok {
			ui["tls"] = ingress.Spec.TLS
		}
	}

	logging.Infof(ctx, "Detected Ingress configuration from Helm chart")
	return ingressConfig, nil
}

// detectPCRFromInitJob checks if Physical Cluster Replication is configured
// by inspecting the init job for --virtualized flags.
func detectPCRFromInitJob(
	ctx context.Context, c *kube.Ctl, stsName string,
) (*crdbv1beta1.CrdbVirtualClusterSpec, error) {
	jobName := fmt.Sprintf("%s-init", stsName)
	job := &batchv1.Job{}

	if err := c.Get(ctx, jobName, job); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, errors.Wrap(err, "failed to get init job")
	}

	// Helm init job uses a bash script with embedded flags.
	// The --virtualized flags may appear in either Args or Command.
	for _, container := range job.Spec.Template.Spec.Containers {
		if spec := detectPCRModeFromArgs(append(container.Args, container.Command...)); spec != nil {
			logging.Infof(ctx, "PCR detected: %s mode", spec.Mode)
			return spec, nil
		}
	}

	return nil, nil
}

// detectPCRModeFromArgs scans a list of args/command tokens for --virtualized flags
// and returns the corresponding VirtualClusterSpec, or nil if not found.
// --virtualized-empty (standby) is checked before --virtualized (primary) because the
// primary flag is a prefix of the standby flag.
func detectPCRModeFromArgs(args []string) *crdbv1beta1.CrdbVirtualClusterSpec {
	for _, arg := range args {
		if strings.Contains(arg, "--virtualized-empty") {
			return &crdbv1beta1.CrdbVirtualClusterSpec{Mode: crdbv1beta1.VirtualClusterModeStandby}
		}
		if strings.Contains(arg, "--virtualized") {
			return &crdbv1beta1.CrdbVirtualClusterSpec{Mode: crdbv1beta1.VirtualClusterModePrimary}
		}
	}
	return nil
}

// ensureCerts creates the CA ConfigMap needed by the CockroachDB Operator.
// The node and client secrets are regenerated later because the migrated pods need
// the join-service DNS names added to those certs.
func ensureCerts(
	ctx context.Context,
	c *kube.Ctl,
	sts *appsv1.StatefulSet,
	stsCtx *migrationutil.ParsedMigrationContext,
) error {
	destPrefix := sts.Name

	// Create CA ConfigMap from existing secrets
	caCMName := destPrefix + suffixCAConfigMap
	caCM := &corev1.ConfigMap{}
	if err := c.Get(ctx, caCMName, caCM); apierrors.IsNotFound(err) {
		// Try to find CA in node secret or ca secret
		caData, err := getSecretData(ctx, c, stsCtx.SourceNodeSecretName, "ca.crt")
		if err != nil {
			// Try CA secret
			caData, err = getSecretData(ctx, c, stsCtx.SourceCASecretName, "ca.crt")
			if err != nil {
				return errors.Wrapf(err, "failed to find ca.crt in node or ca secrets")
			}
		}

		newCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: caCMName, Namespace: sts.Namespace},
			Data:       map[string]string{"ca.crt": string(caData)},
		}
		if err := c.Create(ctx, newCM); err != nil {
			return err
		}
		logging.Infof(ctx, "Created CA ConfigMap: %s", caCMName)
	}

	// Node and client secrets will be created by regenerate*CertificatesForMigration with the
	// correct DNS SANs. Copying them here is wasted work since regeneration overwrites them.
	return nil
}

// getSecret fetches a Secret by name from the controller's namespace.
func getSecret(ctx context.Context, c *kube.Ctl, name string) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	if err := c.Get(ctx, name, s); err != nil {
		return nil, err
	}
	return s, nil
}

// getSecretData returns a single key's value from the named Secret.
func getSecretData(ctx context.Context, c *kube.Ctl, name, key string) ([]byte, error) {
	s, err := getSecret(ctx, c, name)
	if err != nil {
		return nil, err
	}
	if v, ok := s.Data[key]; ok {
		return v, nil
	}
	return nil, errors.Errorf("key %s not found in secret %s", key, name)
}

// highestUnmigratedPodIndex returns the index of the highest-ordinal pod that
// has not been migrated yet. Migration processes pods from highest to lowest,
// so this is the correct starting point when resuming or recovering. Returns
// -1 when all pods are migrated.
func highestUnmigratedPodIndex(stsName string, totalPods int32, migratedNodes []string) int32 {
	migratedSet := make(map[string]bool, len(migratedNodes))
	for _, name := range migratedNodes {
		migratedSet[name] = true
	}
	for i := totalPods - 1; i >= 0; i-- {
		if !migratedSet[fmt.Sprintf("%s-%d", stsName, i)] {
			return i
		}
	}
	return -1
}

// listClusterCrdbNodes returns all CrdbNodes belonging to a cluster.
func listClusterCrdbNodes(
	ctx context.Context, ctl *kube.Ctl, clusterName string,
) ([]crdbv1beta1.CrdbNode, error) {
	nodes := &crdbv1beta1.CrdbNodeList{}
	if err := ctl.List(ctx, &kube.ListOptions{
		Namespace:     ctl.Namespace(),
		LabelSelector: kube.ParseLabelOrDie(fmt.Sprintf("%s=%s", manifests.BetaCrdbClusterNodeOwnerLabelKey, clusterName)),
	}, nodes); err != nil {
		return nil, err
	}
	return nodes.Items, nil
}

// ensurePriorityClass creates the CockroachDB priority class if it doesn't already exist.
func ensurePriorityClass(ctx context.Context, ctl *kube.Ctl) error {
	logging.Infof(ctx, "Ensuring PriorityClass %s", manifests.CRDBCritical)

	pc := &schedulingv1.PriorityClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: manifests.CRDBCritical,
			Labels: map[string]string{
				"app": "cockroachdb",
			},
		},
		Value:         manifests.CriticalPriority,
		GlobalDefault: false,
		Description:   "Priority class for CockroachDB pods",
	}

	if err := ctl.Create(ctx, pc); err != nil {
		if apierrors.IsAlreadyExists(err) {
			logging.Infof(ctx, "PriorityClass %s already exists", manifests.CRDBCritical)
			return nil
		}
		return errors.Wrap(err, "failed to create PriorityClass")
	}

	logging.Infof(ctx, "Created PriorityClass %s", manifests.CRDBCritical)
	return nil
}

// labelExistingPods labels existing StatefulSet pods with cluster labels for service selector
// matching. The operator's headless service uses the cluster owner label to find pods.
func labelExistingPods(
	ctx context.Context, ctl *kube.Ctl, clusterName string, replicas int32,
) error {
	logging.Infof(ctx, "Labeling existing pods for cluster %s", clusterName)

	for i := range replicas {
		podName := fmt.Sprintf("%s-%d", clusterName, i)
		pod := &corev1.Pod{}

		err := ctl.Get(ctx, podName, pod)
		if err != nil {
			if apierrors.IsNotFound(err) {
				logging.Warnf(ctx, "Pod %s not found, skipping labeling", podName)
				continue
			}
			return errors.Wrapf(err, "failed to get pod %s", podName)
		}

		// Add cluster label if not present
		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}

		needsUpdate := false
		if pod.Labels[manifests.BetaCrdbClusterNodeOwnerLabelKey] != clusterName {
			pod.Labels[manifests.BetaCrdbClusterNodeOwnerLabelKey] = clusterName
			needsUpdate = true
		}
		// Needed for backwards compatibility with headless discovery service selectors.
		// The legacy StatefulSet headless Service uses selector `svc=cockroachdb`.
		if pod.Labels["svc"] != "cockroachdb" {
			pod.Labels["svc"] = "cockroachdb"
			needsUpdate = true
		}

		if needsUpdate {
			if err := ctl.Update(ctx, pod); err != nil {
				return errors.Wrapf(err, "failed to update pod %s labels", podName)
			}
			logging.Infof(ctx, "Labeled pod %s with cluster label", podName)
		} else {
			logging.Infof(ctx, "Pod %s already has cluster label", podName)
		}
	}

	logging.Infof(ctx, "Finished labeling existing pods for cluster %s", clusterName)
	return nil
}

// syncRegionAnnotations ensures spec.Regions reflects the region annotations on the
// CrdbCluster. Region configuration reaches spec.Regions via ConvertTo (v1alpha1 writes),
// but kubectl uses v1beta1 by default after the enterprise operator sets v1beta1 as the
// storage version. In that case conversion never runs and spec.Regions retains stale
// defaults from the initial conversion. This helper bridges that gap. Three cases:
//
//  1. User set the full regions JSON annotation directly (multi-region or automation).
//     Use it as the base for spec.Regions.
//
//  2. User set regionCode/cloudProvider annotations. Override spec.Regions[0] with the
//     explicit values. If a regions JSON base was also set, individual annotations win
//     for the first region.
//
//  3. Neither annotation is present. No change.
func syncRegionAnnotations(
	ctx context.Context, ctl *kube.Ctl, cluster *crdbv1beta1.CrdbCluster,
) (bool, error) {
	anns := cluster.Annotations
	if anns == nil {
		return false, nil
	}

	regionCode, hasRegion := anns[crdbv1alpha1.BetaCrdbClusterNodeRegionCodeAnnotationKey]
	cloudProvider, hasProvider := anns[crdbv1alpha1.BetaCrdbClusterNodeCloudProviderAnnotationKey]

	// Check if the full regions JSON annotation was set directly.
	var regionsFromJSON []crdbv1beta1.CrdbClusterRegion
	if regionsJSON, ok := anns[crdbv1alpha1.BetaCrdbClusterRegionsAnnotationKey]; ok && regionsJSON != "" {
		if err := json.Unmarshal([]byte(regionsJSON), &regionsFromJSON); err != nil {
			logging.Warnf(ctx, "Failed to unmarshal regions annotation: %v", err)
		}
	}

	if !hasRegion && !hasProvider && len(regionsFromJSON) == 0 {
		return false, nil
	}

	updated := false

	// If a full regions JSON annotation exists and differs from spec, adopt it.
	if len(regionsFromJSON) > 0 && !slices.Equal(regionsFromJSON, cluster.Spec.Regions) {
		cluster.Spec.Regions = regionsFromJSON
		updated = true
	}

	// Individual annotations override the first region, matching ConvertTo behavior.
	if len(cluster.Spec.Regions) > 0 {
		if hasRegion && cluster.Spec.Regions[0].Code != regionCode {
			cluster.Spec.Regions[0].Code = regionCode
			updated = true
		}
		if hasProvider && cluster.Spec.Regions[0].CloudProvider != cloudProvider {
			cluster.Spec.Regions[0].CloudProvider = cloudProvider
			updated = true
		}
	}

	if !updated {
		return false, nil
	}

	logging.Infof(ctx, "Synced region annotations to spec: region=%s, provider=%s",
		cluster.Spec.Regions[0].Code, cluster.Spec.Regions[0].CloudProvider)

	if err := ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, cluster); err != nil {
		return false, errors.Wrap(err, "failed to sync region annotations to spec")
	}
	return true, nil
}

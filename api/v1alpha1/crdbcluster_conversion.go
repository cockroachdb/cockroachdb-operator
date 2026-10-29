package v1alpha1

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/migrationutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/conversion"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	BetaCrdbClusterNodeCloudProviderAnnotationKey = "crdb.cockroachlabs.com/cloudProvider"
	BetaCrdbClusterNodeRegionCodeAnnotationKey    = "crdb.cockroachlabs.com/regionCode"
	BetaCrdbClusterModeAnnotationKey              = "crdb.cockroachlabs.com/mode"
	BetaCrdbClusterSettingsAnnotationKey          = "crdb.cockroachlabs.com/clusterSettings"
	BetaCrdbClusterRestartDelayAnnotationKey      = "crdb.cockroachlabs.com/rollingRestartDelay"
	BetaCrdbClusterIngressAnnotationKey           = "crdb.cockroachlabs.com/ingress"
	BetaCrdbClusterFeaturesAnnotationKey          = "crdb.cockroachlabs.com/features"
	BetaCrdbClusterTenantHostAnnotationKey        = "crdb.cockroachlabs.com/tenantHost"
	BetaCrdbClusterRegionsAnnotationKey           = "crdb.cockroachlabs.com/regions"

	// BetaCrdbClusterV1Alpha1FieldsAnnotationKey holds a JSON-encoded v1alpha1PreservedFields struct.
	// It captures v1alpha1 fields that have no direct v1beta1 equivalent so they survive a
	// round-trip through the hub and can be restored if migration is rolled back.
	BetaCrdbClusterV1Alpha1FieldsAnnotationKey = "crdb.cockroachlabs.com/v1alpha1-fields"

	// BetaCrdbClusterHubSpecAnnotationKey holds a JSON-encoded v1beta1.CrdbClusterSpec.
	// Older operator versions wrote it during ConvertFrom as a round-trip fallback, but
	// nothing reads it anymore. The key stays in the cleanup lists so betaclusterctrl
	// strips it from clusters that still carry it after upgrading.
	BetaCrdbClusterHubSpecAnnotationKey = "crdb.cockroachlabs.com/v1beta1-spec"

	// BetaCrdbClusterMigrationStatusAnnotationKey holds a JSON-encoded v1beta1.MigrationStatus.
	// v1alpha1 has no status.migration field, so conversion must carry it explicitly or any
	// v1alpha1 metadata update can wipe the stored migration progress.
	BetaCrdbClusterMigrationStatusAnnotationKey = "crdb.cockroachlabs.com/migration-status"

	// clusterStatusAnnotationKey carries the v1alpha1 ClusterStatus string through the hub.
	// It intentionally lacks the crdb.cockroachlabs.com/ prefix for backward compatibility.
	clusterStatusAnnotationKey = "clusterStatus"

	// crdbContainerName is the enterprise operator's container name for CockroachDB pods.
	// The Helm chart and public operator use "db" in the STS, but the enterprise operator
	// creates pods with "cockroachdb". Kept local to avoid a circular import with manifests.
	crdbContainerName = "cockroachdb"
)

// operatorInjectedAnnotationKeys are annotations the conversion webhook or
// migration controller inject automatically. These are safe to strip after
// migration because no external tool (Helm, GitOps) manages them.
var operatorInjectedAnnotationKeys = []string{
	BetaCrdbClusterModeAnnotationKey,
	BetaCrdbClusterRestartDelayAnnotationKey,
	BetaCrdbClusterSettingsAnnotationKey,
	BetaCrdbClusterFeaturesAnnotationKey,
	BetaCrdbClusterTenantHostAnnotationKey,
	BetaCrdbClusterRegionsAnnotationKey,
	BetaCrdbClusterHubSpecAnnotationKey,
	BetaCrdbClusterV1Alpha1FieldsAnnotationKey,
	BetaCrdbClusterMigrationStatusAnnotationKey,
	BetaCrdbClusterIngressAnnotationKey,
	clusterStatusAnnotationKey,
}

// userAppliedMigrationAnnotationKeys are annotations the user applies before
// starting migration. cleanMigrationAnnotations leaves them alone because Helm
// or GitOps might still declare them, and stripping would cause a reconcile loop.
var userAppliedMigrationAnnotationKeys = []string{
	BetaCrdbClusterNodeRegionCodeAnnotationKey,
	BetaCrdbClusterNodeCloudProviderAnnotationKey,
	// Defined in migrationctrl/types.go as AnnotationMigrationRegions but
	// duplicated here to avoid a circular import (api/ cannot import controllers/).
	"crdb.cockroachlabs.com/migration-regions",
}

// migrationAnnotationKeys is the union of operator-injected and user-applied
// migration annotations. Used by convertNonMigrationClusterFromHub to strip
// stale annotations from the v1alpha1 representation.
var migrationAnnotationKeys = slices.Concat(operatorInjectedAnnotationKeys, userAppliedMigrationAnnotationKeys)

// OperatorInjectedMigrationAnnotationKeys returns the operator-injected keys for
// cleanup. Use this rather than MigrationAnnotationKeys so user-applied
// annotations are left in place.
func OperatorInjectedMigrationAnnotationKeys() []string {
	return slices.Clone(operatorInjectedAnnotationKeys)
}

// MigrationAnnotationKeys returns all migration annotation keys for detection.
func MigrationAnnotationKeys() []string {
	return slices.Clone(migrationAnnotationKeys)
}

// operatorInjectedEnvVars is the set of env var names injected by ConvertTo into the
// PodTemplate. ConvertFrom filters them out so they do not accumulate in PodEnvVariables
// across round-trips (the v1alpha1 operator re-adds its own generated vars on top).
var operatorInjectedEnvVars = map[string]bool{
	"HOST_IP": true,
	"GODEBUG": true,
}

// v1alpha1PreservedFields holds v1alpha1 spec fields that have no direct v1beta1 equivalent.
// It is serialised as JSON into BetaCrdbClusterV1Alpha1FieldsAnnotationKey during ConvertTo
// so that ConvertFrom can restore the original values on rollback without data loss.
type v1alpha1PreservedFields struct {
	// NodeTLSSecret is the user-provided node TLS secret name. The enterprise operator
	// creates a new secret under a different name during migration; the original must be
	// preserved so the public operator can find it again on rollback.
	NodeTLSSecret string `json:"nodeTLSSecret,omitempty"`
	// ClientTLSSecret is the user-provided root client TLS secret name (same rationale).
	ClientTLSSecret string `json:"clientTLSSecret,omitempty"`
	// MinAvailable is the PDB min-available value; v1beta1 only models max-unavailable via DisruptionBudget.
	MinAvailable *int32 `json:"minAvailable,omitempty"`
	// AutomountServiceAccountToken has no v1beta1 field.
	AutomountServiceAccountToken bool `json:"automountServiceAccountToken,omitempty"`
}

var (
	// conversionClient is the cache-backed client for the common conversion path
	// (parsing the running StatefulSet). Set from mgr.GetClient(). Because ConvertTo
	// runs on every read of a v1alpha1-stored object as v1beta1, keeping this on the
	// cache avoids a direct API call per conversion.
	conversionClient client.Client

	// conversionUncachedReader reads straight from the API server. It is used only
	// for skip-labeled (protected) clusters, where a stale cached StatefulSet could
	// hide a just-deleted STS and let a clobbering v1alpha1 write slip past the
	// ConvertTo rejection. Reserving it for that path keeps the high-volume ordinary
	// conversions on the cache.
	conversionUncachedReader client.Reader

	// conversionLog is the logger for conversion operations
	conversionLog = logf.Log.WithName("conversion")
)

// SetConversionClient sets the cache-backed client used for the common conversion
// path. Pass mgr.GetClient(). Call during operator startup before any conversions.
func SetConversionClient(c client.Client) {
	conversionClient = c
}

// SetConversionUncachedReader sets the uncached reader used only for protected
// (skip-labeled) clusters. Pass mgr.GetAPIReader().
func SetConversionUncachedReader(c client.Reader) {
	conversionUncachedReader = c
}

// fetchStatefulSet retrieves the StatefulSet for the cluster if it exists.
// Protected (skip-labeled) clusters read uncached so a just-deleted StatefulSet
// can't linger in the cache and defeat the protected-write rejection. Everything
// else reads through the cache. It falls back to the cached client when no
// uncached reader is configured.
func (src *CrdbCluster) fetchStatefulSet(ctx context.Context, uncached bool) (*appsv1.StatefulSet, error) {
	var reader client.Reader = conversionClient
	if uncached && conversionUncachedReader != nil {
		reader = conversionUncachedReader
	}
	if reader == nil {
		conversionLog.Info("conversion reader is nil, skipping StatefulSet fetch")
		return nil, nil
	}

	// Webhooks need to be fast, so add a timeout to avoid blocking kubectl commands
	// if the API server is slow or hung. We'd rather fail fast and use defaults.
	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	sts := &appsv1.StatefulSet{}
	err := reader.Get(timeoutCtx, types.NamespacedName{
		Name:      src.Name,
		Namespace: src.Namespace,
	}, sts)

	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, nil
		}
		conversionLog.Error(err, "failed to fetch StatefulSet for migration", "name", src.Name, "namespace", src.Namespace)
		return nil, err
	}

	return sts, nil
}

const (
	// These values mirror migrationctrl. The api package cannot import controllers.
	crdbMigrateLabel       = "crdb.io/migrate"
	crdbMigrateComplete    = "complete"
	migrationPhaseComplete = "Complete"
)

// migrationHandoffComplete controls when conversion can use the minimal projection.
// Keep this aligned with migrationctrl.IsMigrationFullyComplete.
func migrationHandoffComplete(src *v1beta1.CrdbCluster) bool {
	return src.IsManagedMode() &&
		src.Status.Migration != nil && src.Status.Migration.Phase == migrationPhaseComplete &&
		src.Labels[crdbMigrateLabel] == crdbMigrateComplete
}

// isNonMigrationCluster reports whether ConvertFrom can use the minimal projection.
//
// Disabled clusters stay on the full path because skip-reconcile can be applied before
// migration status exists. That window still needs the region annotations.
//
// A migration also stays on the full path until Mode, status, and the terminal label have
// landed. Fresh clusters and completed migrations can omit the compatibility annotations
// that cause SSA ownership conflicts.
func isNonMigrationCluster(src *v1beta1.CrdbCluster) bool {
	if src.Spec.Mode != nil && *src.Spec.Mode == v1beta1.Disabled {
		return false
	}
	if src.Status.Migration != nil && !migrationHandoffComplete(src) {
		return false
	}
	return true
}

func skipsPublicOperatorReconciliation(labels map[string]string) bool {
	return labels[CrdbOperatorMigrationLabel] == "true"
}

// ConvertTo converts this v1alpha1 CrdbCluster to the Hub version (v1beta1)
func (src *CrdbCluster) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.CrdbCluster)
	ctx := context.Background()

	dst.ObjectMeta = src.ObjectMeta
	// Normalize so annotation writes below never need repeated nil checks.
	if dst.Annotations == nil {
		dst.Annotations = make(map[string]string)
	}

	// nil-map reads are safe in Go; this alias eliminates repeated nil guards below.
	anns := src.Annotations

	// v1alpha1 has no region in its spec, so region and cloud provider come from annotations
	// that the user applies before migration. We need to know whether these annotations are
	// actually present or just falling back to defaults, because the region override logic
	// below must only apply when the user explicitly set a value. Without this distinction,
	// installing the enterprise operator would bake DefaultRegion into every cluster's cached
	// regions, and the user couldn't correct it later.
	regionCode := migrationutil.DefaultRegion
	cloudProvider := migrationutil.DefaultCloudProvider
	regionAnnotationSet := false
	cloudProviderAnnotationSet := false
	if region, ok := anns[BetaCrdbClusterNodeRegionCodeAnnotationKey]; ok {
		regionCode = region
		regionAnnotationSet = true
	}
	if provider, ok := anns[BetaCrdbClusterNodeCloudProviderAnnotationKey]; ok {
		cloudProvider = provider
		cloudProviderAnnotationSet = true
	}

	// Fetch the StatefulSet to get runtime configuration. Protected (skip-labeled)
	// clusters read uncached so a stale cache can't hide a just-deleted STS.
	protected := skipsPublicOperatorReconciliation(src.Labels)
	sts, err := src.fetchStatefulSet(ctx, protected)
	if err != nil {
		// Fail closed for a skip-labeled cluster so we never convert from defaults and
		// overwrite its stored spec. An ordinary cluster has nothing to protect, so
		// fall back to defaults and let a transient fetch error convert successfully.
		if protected {
			return errors.Wrap(err, "failed to fetch StatefulSet for protected CrdbCluster conversion")
		}
		conversionLog.Error(err, "failed to fetch StatefulSet, converting with defaults")
	}

	// Ordinary v1alpha1 clusters continue through compatibility conversion even when
	// their StatefulSet does not exist yet.
	//
	// Once a skip-labeled cluster loses its StatefulSet, reject v1alpha1 writes so a
	// lossy projection cannot replace the stored v1beta1 spec. The public operator's
	// admission webhooks must use Exact matching while both operators are installed.
	if sts == nil && protected {
		return errors.Newf(
			"cannot convert protected v1alpha1 CrdbCluster %s/%s without its migration StatefulSet",
			src.Namespace,
			src.Name,
		)
	}

	// Parse the running StatefulSet so the converted spec matches what is actually deployed.
	// The conversion webhook is read-only. Missing Secrets or ConfigMaps are fine here
	// because the migration controller creates the runtime copies later.
	parsedCtx, err := migrationutil.ParseStatefulSet(sts, src.Name)
	if err != nil {
		conversionLog.Error(err, "failed to parse StatefulSet config, using defaults")
		parsedCtx = &migrationutil.ParsedMigrationContext{
			GRPCPort:           migrationutil.DefaultGRPCPort,
			ServiceAccountName: src.Name,
		}
	}
	if parsedCtx.ServiceAccountName == "" {
		parsedCtx.ServiceAccountName = src.Name
	}
	migrationutil.NormalizeRuntimeTLSConfig(parsedCtx, src.Name)

	// Apply overrides from v1alpha1 spec. TLSEnabled is always taken from the spec
	// because the STS parser defaults to true, which would incorrectly enable certs for insecure clusters.
	parsedCtx.TLSEnabled = src.Spec.TLSEnabled
	if src.Spec.PriorityClassName != "" {
		parsedCtx.PriorityClassName = src.Spec.PriorityClassName
	}
	if src.Spec.LogConfigMap != "" {
		parsedCtx.LoggingConfigMap = src.Spec.LogConfigMap
	}

	// Convert mode - check if we have a preserved mode from a previous v1beta1 round-trip.
	// If not, default to Disabled for initial migration from v1alpha1.
	mode := v1beta1.Disabled // Default to Disabled during migration
	if modeStr, ok := anns[BetaCrdbClusterModeAnnotationKey]; ok {
		mode = v1beta1.ReconciliationMode(modeStr)
	}

	// Restore rollingRestartDelay from annotation or use default
	rollingRestartDelay := metav1.Duration{Duration: 1 * time.Minute}
	if delayStr, ok := anns[BetaCrdbClusterRestartDelayAnnotationKey]; ok {
		if d, err := time.ParseDuration(delayStr); err == nil {
			rollingRestartDelay = metav1.Duration{Duration: d}
		}
	}

	// Restore ClusterSettings from annotation if present
	var clusterSettings map[string]string
	if settingsJSON, ok := anns[BetaCrdbClusterSettingsAnnotationKey]; ok {
		if err := json.Unmarshal([]byte(settingsJSON), &clusterSettings); err != nil {
			conversionLog.Error(err, "failed to unmarshal cluster settings from annotation")
		}
	}

	// Restore Features from annotation if present
	var features []v1beta1.ClusterFeature
	if featuresJSON, ok := anns[BetaCrdbClusterFeaturesAnnotationKey]; ok {
		if err := json.Unmarshal([]byte(featuresJSON), &features); err != nil {
			conversionLog.Error(err, "failed to unmarshal features from annotation")
		}
	}

	// Restore TenantHost from annotation if present
	var tenantHost *v1beta1.TenantHost
	if tenantHostJSON, ok := anns[BetaCrdbClusterTenantHostAnnotationKey]; ok {
		tenantHost = &v1beta1.TenantHost{}
		if err := json.Unmarshal([]byte(tenantHostJSON), tenantHost); err != nil {
			conversionLog.Error(err, "failed to unmarshal tenant host from annotation")
			tenantHost = nil
		}
	}

	// Restore full Regions from annotation if present (for round-trip preservation)
	var regions []v1beta1.CrdbClusterRegion
	if regionsJSON, ok := anns[BetaCrdbClusterRegionsAnnotationKey]; ok {
		if err := json.Unmarshal([]byte(regionsJSON), &regions); err != nil {
			conversionLog.Error(err, "failed to unmarshal regions from annotation")
		}
	}

	// Build the CrdbNodeSpec template. ExternalCertificates are only set for TLS clusters;
	// insecure clusters use --insecure and have no cert secrets to reference.
	nodeSpec := v1beta1.CrdbNodeSpec{
		ServiceAccountName:   parsedCtx.ServiceAccountName,
		LoggingConfigMapName: parsedCtx.LoggingConfigMap,
		LocalityLabels:       parsedCtx.LocalityLabels,
		StartFlags:           parsedCtx.StartFlags,
		WALFailoverSpec:      parsedCtx.WALFailoverSpec,
		Image:                "", // will be set below
	}
	if parsedCtx.TLSEnabled {
		nodeSpec.Certificates = migrationutil.BuildExternalCertificates(parsedCtx)
	}

	// Ports: spec fields take priority over STS-derived values. The spec is explicit user
	// intent (set via kubectl or helm). Falling back to parsedCtx ensures that clusters
	// with custom ports in their STS (but no explicit spec field) are migrated correctly.
	if src.Spec.GRPCPort != nil {
		nodeSpec.GRPCPort = src.Spec.GRPCPort
	} else if parsedCtx.GRPCPort > 0 {
		nodeSpec.GRPCPort = &parsedCtx.GRPCPort
	}

	if src.Spec.SQLPort != nil {
		nodeSpec.SQLPort = src.Spec.SQLPort
	} else if parsedCtx.SQLPort > 0 {
		nodeSpec.SQLPort = &parsedCtx.SQLPort
	}

	if src.Spec.HTTPPort != nil {
		nodeSpec.HTTPPort = src.Spec.HTTPPort
	} else if parsedCtx.HTTPPort > 0 {
		nodeSpec.HTTPPort = &parsedCtx.HTTPPort
	}

	// Convert spec
	dst.Spec = v1beta1.CrdbClusterSpec{
		Mode:                &mode,
		RollingRestartDelay: &rollingRestartDelay,
		ClusterSettings:     clusterSettings,
		TLSEnabled:          parsedCtx.TLSEnabled,
		Features:            features,
		TenantHost:          tenantHost,
		Template: v1beta1.CrdbNodeTemplate{
			Spec: nodeSpec,
		},
	}

	// Preserve Ingress config in annotation. v1beta1 has no Ingress spec field, so the
	// config is stored as JSON and restored on ConvertFrom for round-trip fidelity. The
	// enterprise operator does not create or manage Ingress resources. Users must manage
	// them separately after migration. The annotation is preserved so rollback can restore
	// the v1alpha1 Ingress field without data loss.
	if src.Spec.Ingress != nil {
		ingressBytes, err := json.Marshal(src.Spec.Ingress)
		if err == nil {
			dst.Annotations[BetaCrdbClusterIngressAnnotationKey] = string(ingressBytes)
		} else {
			conversionLog.Error(err, "failed to marshal ingress config for annotation")
		}
	}

	// Preserve v1alpha1-only fields as a single JSON annotation so ConvertFrom can restore
	// them on rollback. Fields with a direct v1beta1 mapping (MaxUnavailable→DisruptionBudget,
	// TerminationGracePeriodSecs→PodTemplate, PullPolicy/PullSecret→container) are handled
	// by direct field assignment below and do not need annotation storage.
	preserved := v1alpha1PreservedFields{
		NodeTLSSecret:                src.Spec.NodeTLSSecret,
		ClientTLSSecret:              src.Spec.ClientTLSSecret,
		MinAvailable:                 src.Spec.MinAvailable,
		AutomountServiceAccountToken: src.Spec.AutomountServiceAccountToken,
	}
	if preservedBytes, err := json.Marshal(preserved); err == nil {
		dst.Annotations[BetaCrdbClusterV1Alpha1FieldsAnnotationKey] = string(preservedBytes)
	} else {
		conversionLog.Error(err, "failed to marshal v1alpha1 preserved fields")
	}
	// MaxUnavailable maps directly to DisruptionBudget (max nodes offline during rolling restart).
	if src.Spec.MaxUnavailable != nil {
		budget := int(*src.Spec.MaxUnavailable)
		dst.Spec.DisruptionBudget = &budget
	}

	// Convert nodes to regions. Cached regions (from BetaCrdbClusterRegionsAnnotationKey) preserve
	// v1beta1-only fields like Domain and EncryptionAtRest across round-trips. Three cases:
	//
	//  1. User explicitly set regionCode/cloudProvider annotations after the initial conversion
	//     baked in defaults. The explicit annotations override the cached region so the user's
	//     correction takes effect on the next v1alpha1 write.
	//
	//  2. Normal round-trip where ConvertFrom already synced the annotations from the stored
	//     regions. The values match, so the override is a no-op.
	//
	//  3. User (or automation) set the regions JSON annotation directly without setting
	//     regionCode/cloudProvider. The annotations are absent, so we leave the cached regions
	//     untouched. This covers multi-region setups where the regions array has multiple entries.
	if len(regions) > 0 {
		if regionAnnotationSet {
			regions[0].Code = regionCode
		}
		if cloudProviderAnnotationSet {
			regions[0].CloudProvider = cloudProvider
		}
		dst.Spec.Regions = regions
	} else if src.Spec.Nodes != 0 {
		// Initial migration: construct single region from v1alpha1 fields
		dst.Spec.Regions = []v1beta1.CrdbClusterRegion{
			{
				Code:          regionCode,
				Nodes:         src.Spec.Nodes,
				CloudProvider: cloudProvider,
				Namespace:     src.Namespace,
			},
		}
	}

	// Initialize the flags map if not already set from StatefulSet
	if dst.Spec.Template.Spec.StartFlags == nil {
		dst.Spec.Template.Spec.StartFlags = &v1beta1.Flags{}
	}

	// Ensure slices are initialized (not nil) so they marshal to [] instead of null
	// This is important because the JSON tags lack omitempty
	if dst.Spec.Template.Spec.StartFlags.Upsert == nil {
		dst.Spec.Template.Spec.StartFlags.Upsert = []string{}
	}
	if dst.Spec.Template.Spec.StartFlags.Omit == nil {
		dst.Spec.Template.Spec.StartFlags.Omit = []string{}
	}

	// Build a set of existing flags so addFlag can dedup in O(1) instead of O(n) per call.
	existingFlags := make(map[string]bool, len(dst.Spec.Template.Spec.StartFlags.Upsert))
	for _, f := range dst.Spec.Template.Spec.StartFlags.Upsert {
		existingFlags[f] = true
	}
	addFlag := func(flag string) {
		if existingFlags[flag] {
			return
		}
		existingFlags[flag] = true
		dst.Spec.Template.Spec.StartFlags.Upsert = append(dst.Spec.Template.Spec.StartFlags.Upsert, flag)
	}

	// Convert cache and memory settings (these override parsed flags)
	if src.Spec.Cache != "" {
		addFlag("--cache=" + src.Spec.Cache)
	}
	if src.Spec.MaxSQLMemory != "" {
		addFlag("--max-sql-memory=" + src.Spec.MaxSQLMemory)
	}

	// Convert additional args. Skip operator-managed flags (addresses, certs, join, logging)
	// even if they appear in AdditionalArgs from a pre-fix round-trip. They are regenerated
	// from first-class fields each reconcile and must not override those values.
	for _, arg := range src.Spec.AdditionalArgs {
		if !migrationutil.IsOperatorManagedFlag(arg) {
			addFlag(arg)
		}
	}

	// Set image - prefer StatefulSet's CRDB container, fallback to spec.
	crdbContainer := migrationutil.FindCRDBContainer(sts)
	if crdbContainer != nil {
		dst.Spec.Template.Spec.Image = crdbContainer.Image
	} else if src.Spec.Image != nil {
		dst.Spec.Template.Spec.Image = src.Spec.Image.Name
	}

	// Build PodTemplate from StatefulSet (this is the preferred way)
	if crdbContainer != nil {
		dst.Spec.Template.Spec.PodTemplate = &v1beta1.PodTemplateSpec{
			Metadata: v1beta1.PodMeta{
				Labels:      sts.Spec.Template.Labels,
				Annotations: sts.Spec.Template.Annotations,
			},
			Spec: corev1.PodSpec{
				ImagePullSecrets: parsedCtx.ImagePullSecrets,
				// v1alpha1 cannot express custom init containers or volumes. Those only
				// matter for Helm StatefulSets, and the migration controller handles them.
				ServiceAccountName:            parsedCtx.ServiceAccountName,
				PriorityClassName:             parsedCtx.PriorityClassName,
				Affinity:                      sts.Spec.Template.Spec.Affinity,
				NodeSelector:                  sts.Spec.Template.Spec.NodeSelector,
				Tolerations:                   sts.Spec.Template.Spec.Tolerations,
				TerminationGracePeriodSeconds: parsedCtx.TerminationGracePeriodSeconds,
				TopologySpreadConstraints:     sts.Spec.Template.Spec.TopologySpreadConstraints,
			},
		}

		// Add the main container with environment variables.
		// Use the user's explicitly set PodEnvVariables as the base, not parsedCtx.EnvVars
		// (the full STS container env). The STS env includes vars the v1alpha1 operator
		// auto-generates (COCKROACH_CHANNEL, POD_NAME, GOMAXPROCS, MEMORY_LIMIT_MIB); if
		// those end up in PodEnvVariables after a round-trip, the v1alpha1 operator
		// re-adds them on top and the STS ends up with duplicates.
		mainContainer := *crdbContainer
		envVars := deduplicateEnvVars(src.Spec.PodEnvVariables, []corev1.EnvVar{
			{
				Name: "HOST_IP",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						APIVersion: "v1",
						FieldPath:  "status.hostIP",
					},
				},
			},
			{
				Name:  "GODEBUG",
				Value: "disablethp=1",
			},
		})

		mainContainerSpec := corev1.Container{
			Name:      crdbContainerName,
			Image:     mainContainer.Image,
			Resources: mainContainer.Resources,
			Env:       envVars,
		}
		// Map PullPolicyName from v1alpha1 spec into the container's ImagePullPolicy.
		if src.Spec.Image != nil && src.Spec.Image.PullPolicyName != nil {
			mainContainerSpec.ImagePullPolicy = *src.Spec.Image.PullPolicyName
		}
		dst.Spec.Template.Spec.PodTemplate.Spec.Containers = []corev1.Container{mainContainerSpec}

		// Prefer STS-derived ImagePullSecrets (runtime truth); fall back to spec's PullSecret.
		if len(dst.Spec.Template.Spec.PodTemplate.Spec.ImagePullSecrets) == 0 &&
			src.Spec.Image != nil && src.Spec.Image.PullSecret != nil && *src.Spec.Image.PullSecret != "" {
			dst.Spec.Template.Spec.PodTemplate.Spec.ImagePullSecrets = []corev1.LocalObjectReference{
				{Name: *src.Spec.Image.PullSecret},
			}
		}
	} else {
		// Fallback: Construct PodTemplate from Spec fields to avoid using deprecated v1beta1 fields
		fallbackContainer := corev1.Container{
			Name:      crdbContainerName,
			Env:       src.Spec.PodEnvVariables,
			Resources: src.Spec.Resources,
		}
		if src.Spec.Image != nil && src.Spec.Image.PullPolicyName != nil {
			fallbackContainer.ImagePullPolicy = *src.Spec.Image.PullPolicyName
		}

		fallbackPodSpec := corev1.PodSpec{
			Affinity:                  src.Spec.Affinity,
			TopologySpreadConstraints: src.Spec.TopologySpreadConstraints,
			Tolerations:               src.Spec.Tolerations,
			NodeSelector:              src.Spec.NodeSelector,
			ServiceAccountName:        parsedCtx.ServiceAccountName,
			PriorityClassName:         src.Spec.PriorityClassName,
			Containers:                []corev1.Container{fallbackContainer},
		}
		if src.Spec.Image != nil && src.Spec.Image.PullSecret != nil && *src.Spec.Image.PullSecret != "" {
			fallbackPodSpec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: *src.Spec.Image.PullSecret}}
		}
		if src.Spec.TerminationGracePeriodSecs != 0 {
			grace := src.Spec.TerminationGracePeriodSecs
			fallbackPodSpec.TerminationGracePeriodSeconds = &grace
		}

		dst.Spec.Template.Spec.PodTemplate = &v1beta1.PodTemplateSpec{
			Metadata: v1beta1.PodMeta{
				Labels:      src.Spec.AdditionalLabels,
				Annotations: src.Spec.AdditionalAnnotations,
			},
			Spec: fallbackPodSpec,
		}
	}

	// Convert data store. We use the PVC name captured from the StatefulSet (or default
	// "datadir") to ensure we bind to the existing PVCs.
	//
	// NB: v1alpha1 VolumeClaim.PersistentVolumeSource.ClaimName allows users to mount an
	// existing PVC by name instead of using a VolumeClaimTemplate. This "existing claim"
	// semantic is not modeled in v1beta1 DataStore, which only supports VolumeClaimTemplate
	// or VolumeSource. Clusters using this pattern are rare and must manually adjust their
	// storage configuration after migration.
	pvcName := parsedCtx.PVCTemplateName
	if pvcName == "" {
		pvcName = migrationutil.DefaultPVCName
	}

	if src.Spec.DataStore.HostPath != nil || src.Spec.DataStore.VolumeClaim != nil {
		dst.Spec.Template.Spec.DataStore = v1beta1.DataStore{}
		if src.Spec.DataStore.HostPath != nil {
			dst.Spec.Template.Spec.DataStore.VolumeSource = &corev1.VolumeSource{
				HostPath: src.Spec.DataStore.HostPath,
			}
		}
		if src.Spec.DataStore.VolumeClaim != nil {
			dst.Spec.Template.Spec.DataStore.VolumeClaimTemplate = &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: pvcName,
				},
				Spec: src.Spec.DataStore.VolumeClaim.PersistentVolumeClaimSpec,
			}
		}
	} else if sts != nil && len(sts.Spec.VolumeClaimTemplates) > 0 {
		// Use PVC template from StatefulSet to ensure exact match
		dst.Spec.Template.Spec.DataStore = v1beta1.DataStore{
			VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: pvcName,
				},
				Spec: sts.Spec.VolumeClaimTemplates[0].Spec,
			},
		}
	}

	convertStatusToHub(dst, src, regionCode, cloudProvider)

	return nil
}

func convertStatusToHub(
	dst *v1beta1.CrdbCluster, src *CrdbCluster, regionCode, cloudProvider string,
) {
	dst.Status = v1beta1.CrdbClusterStatus{
		Version:  src.Status.Version,
		Image:    src.Status.CrdbContainerImage,
		Region:   regionCode,
		Provider: cloudProvider,
	}
	dst.Annotations[clusterStatusAnnotationKey] = src.Status.ClusterStatus

	if len(src.Status.Conditions) > 0 {
		dst.Status.Conditions = make([]v1beta1.ClusterCondition, len(src.Status.Conditions))
		for i, condition := range src.Status.Conditions {
			dst.Status.Conditions[i] = v1beta1.ClusterCondition{
				Type:               v1beta1.ClusterConditionType(condition.Type),
				Status:             condition.Status,
				LastTransitionTime: condition.LastTransitionTime,
			}
		}
	}

	if migrationJSON, ok := src.Annotations[BetaCrdbClusterMigrationStatusAnnotationKey]; ok && migrationJSON != "" {
		var migration v1beta1.MigrationStatus
		if err := json.Unmarshal([]byte(migrationJSON), &migration); err != nil {
			conversionLog.Error(err, "failed to unmarshal migration status annotation")
		} else {
			dst.Status.Migration = &migration
		}
	}
}

// convertNonMigrationClusterFromHub produces a read-only v1alpha1 projection of
// an enterprise-owned v1beta1 cluster, such as a fresh install or completed
// migration. It exposes enough status for the public operator to observe the
// skip-reconcile label without first issuing an initialization status write, and
// injects no migration annotations, which prevents SSA field ownership conflicts
// on Helm upgrades.
func convertNonMigrationClusterFromHub(dst *CrdbCluster, src *v1beta1.CrdbCluster) {
	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()
	// Strip any stale migration annotations that betaclusterctrl has not yet
	// cleaned up. These annotations are not part of the read-only projection.
	for _, key := range migrationAnnotationKeys {
		delete(dst.Annotations, key)
	}
	dst.Spec = CrdbClusterSpec{
		TLSEnabled: src.Spec.TLSEnabled,
	}
	if len(src.Spec.Regions) > 0 {
		dst.Spec.Nodes = src.Spec.Regions[0].Nodes
	}
	if src.Spec.Template.Spec.Image != "" {
		dst.Spec.Image = &PodImage{Name: src.Spec.Template.Spec.Image}
	}
	dst.Status = CrdbClusterStatus{
		// A non-empty status prevents the public operator from writing its
		// initialization status before it observes the skip-reconcile label.
		ClusterStatus:      ActionStatus(Finished).String(),
		Version:            src.Status.Version,
		CrdbContainerImage: src.Status.Image,
	}
}

// ConvertFrom converts from the Hub version (v1beta1) to this v1alpha1 CrdbCluster
func (dst *CrdbCluster) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.CrdbCluster)

	// Non-migration clusters do not need the annotation-heavy conversion path.
	// Injecting those annotations causes SSA field ownership conflicts on helm
	// upgrade. Return a minimal v1alpha1 representation instead.
	if isNonMigrationCluster(src) {
		convertNonMigrationClusterFromHub(dst, src)
		return nil
	}

	// Deep-copy so annotation writes below do not mutate src's shared label and
	// annotation maps. The non-migration path does the same.
	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()

	// Convert spec
	dst.Spec = CrdbClusterSpec{
		TLSEnabled: src.Spec.TLSEnabled,
	}

	if dst.Annotations == nil {
		dst.Annotations = make(map[string]string)
	}

	// Preserve v1beta1-only fields in annotations for round-trip conversion.
	// If an API client reads a v1beta1 object as v1alpha1 and writes it back,
	// these values keep Helm-driven updates from getting dropped.

	// Preserve Mode
	if src.Spec.Mode != nil {
		dst.Annotations[BetaCrdbClusterModeAnnotationKey] = string(*src.Spec.Mode)
	}

	// Preserve RollingRestartDelay
	if src.Spec.RollingRestartDelay != nil {
		dst.Annotations[BetaCrdbClusterRestartDelayAnnotationKey] = src.Spec.RollingRestartDelay.Duration.String()
	}

	// Preserve ClusterSettings
	if len(src.Spec.ClusterSettings) > 0 {
		if settingsBytes, err := json.Marshal(src.Spec.ClusterSettings); err == nil {
			dst.Annotations[BetaCrdbClusterSettingsAnnotationKey] = string(settingsBytes)
		}
	}

	// Preserve Features
	if len(src.Spec.Features) > 0 {
		if featuresBytes, err := json.Marshal(src.Spec.Features); err == nil {
			dst.Annotations[BetaCrdbClusterFeaturesAnnotationKey] = string(featuresBytes)
		}
	}

	// Preserve TenantHost
	if src.Spec.TenantHost != nil {
		if tenantHostBytes, err := json.Marshal(src.Spec.TenantHost); err == nil {
			dst.Annotations[BetaCrdbClusterTenantHostAnnotationKey] = string(tenantHostBytes)
		}
	}

	// Preserve full Regions for a round-trip (includes all region-specific fields like Domain, EncryptionAtRest)
	if len(src.Spec.Regions) > 0 {
		if regionsBytes, err := json.Marshal(src.Spec.Regions); err == nil {
			dst.Annotations[BetaCrdbClusterRegionsAnnotationKey] = string(regionsBytes)
		}
	}

	// Convert nodes from regions for v1alpha1 spec display
	if len(src.Spec.Regions) > 0 {
		dst.Spec.Nodes = src.Spec.Regions[0].Nodes
		// These annotations are kept for initial migration compatibility
		dst.Annotations[BetaCrdbClusterNodeRegionCodeAnnotationKey] = src.Spec.Regions[0].Code
		if src.Spec.Regions[0].CloudProvider != "" {
			dst.Annotations[BetaCrdbClusterNodeCloudProviderAnnotationKey] = src.Spec.Regions[0].CloudProvider
		}
	} else {
		dst.Spec.Nodes = int32(src.Spec.Nodes())
	}

	// Restore Ingress from annotation
	if ingressJSON, ok := src.Annotations[BetaCrdbClusterIngressAnnotationKey]; ok {
		var ingress IngressConfig
		if err := json.Unmarshal([]byte(ingressJSON), &ingress); err == nil {
			dst.Spec.Ingress = &ingress
		} else {
			conversionLog.Error(err, "failed to unmarshal ingress annotation")
		}
	}

	// Convert flags back to individual fields
	if src.Spec.Template.Spec.StartFlags != nil {
		// Use a map to track seen flags to avoid duplication
		seenFlags := make(map[string]bool)

		for _, flag := range src.Spec.Template.Spec.StartFlags.Upsert {
			if strings.HasPrefix(flag, "--cache") {
				parts := strings.SplitN(flag, "=", 2)
				if len(parts) == 2 {
					dst.Spec.Cache = parts[1]
				}
			} else if strings.HasPrefix(flag, "--max-sql-memory") {
				parts := strings.SplitN(flag, "=", 2)
				if len(parts) == 2 {
					dst.Spec.MaxSQLMemory = parts[1]
				}
			} else if !migrationutil.IsOperatorManagedFlag(flag) {
				// Only preserve flags the user explicitly set. Operator-managed flags
				// (addresses, certs, join, logging) are regenerated each reconcile from
				// first-class fields; putting them in AdditionalArgs causes them to
				// accumulate and override those first-class values on round-trips.
				if !seenFlags[flag] {
					dst.Spec.AdditionalArgs = append(dst.Spec.AdditionalArgs, flag)
					seenFlags[flag] = true
				}
			}
		}
	}

	// Convert PriorityClass and LogConfigMap
	if src.Spec.Template.Spec.PodTemplate != nil {
		dst.Spec.PriorityClassName = src.Spec.Template.Spec.PodTemplate.Spec.PriorityClassName
	}
	dst.Spec.LogConfigMap = src.Spec.Template.Spec.LoggingConfigMapName

	// Convert image and restore PullPolicyName / PullSecret from the PodTemplate.
	if src.Spec.Template.Spec.Image != "" {
		img := &PodImage{Name: src.Spec.Template.Spec.Image}
		if src.Spec.Template.Spec.PodTemplate != nil {
			if len(src.Spec.Template.Spec.PodTemplate.Spec.Containers) > 0 {
				policy := src.Spec.Template.Spec.PodTemplate.Spec.Containers[0].ImagePullPolicy
				if policy != "" {
					img.PullPolicyName = &policy
				}
			}
			if len(src.Spec.Template.Spec.PodTemplate.Spec.ImagePullSecrets) > 0 {
				name := src.Spec.Template.Spec.PodTemplate.Spec.ImagePullSecrets[0].Name
				if name != "" {
					img.PullSecret = &name
				}
			}
		}
		dst.Spec.Image = img
	}

	// Convert resources and pod settings from PodTemplate if available
	if src.Spec.Template.Spec.PodTemplate != nil {
		dst.Spec.AdditionalLabels = src.Spec.Template.Spec.PodTemplate.Metadata.Labels
		dst.Spec.AdditionalAnnotations = src.Spec.Template.Spec.PodTemplate.Metadata.Annotations

		dst.Spec.Affinity = src.Spec.Template.Spec.PodTemplate.Spec.Affinity
		dst.Spec.TopologySpreadConstraints = src.Spec.Template.Spec.PodTemplate.Spec.TopologySpreadConstraints
		dst.Spec.Tolerations = src.Spec.Template.Spec.PodTemplate.Spec.Tolerations
		dst.Spec.NodeSelector = src.Spec.Template.Spec.PodTemplate.Spec.NodeSelector

		if len(src.Spec.Template.Spec.PodTemplate.Spec.Containers) > 0 {
			// The first container is cockroachdb because ConvertTo always places it at index 0.
			// Filter out env vars the operator injects internally (HOST_IP, GODEBUG) so they
			// don't appear in PodEnvVariables on the next read. The v1alpha1 operator would
			// see them as user-specified and add duplicates alongside its own auto-generated vars.
			crdbContainer := src.Spec.Template.Spec.PodTemplate.Spec.Containers[0]
			dst.Spec.PodEnvVariables = filterOperatorEnvVars(crdbContainer.Env)
			dst.Spec.Resources = crdbContainer.Resources
		}
	} else {
		// Fallback for backward compatibility
		dst.Spec.Resources = src.Spec.Template.Spec.ResourceRequirements
		dst.Spec.AdditionalLabels = src.Spec.Template.Spec.PodLabels
		dst.Spec.AdditionalAnnotations = src.Spec.Template.Spec.PodAnnotations
		dst.Spec.PodEnvVariables = src.Spec.Template.Spec.Env
		dst.Spec.Affinity = src.Spec.Template.Spec.Affinity
		dst.Spec.TopologySpreadConstraints = src.Spec.Template.Spec.TopologySpreadConstraints
		dst.Spec.Tolerations = src.Spec.Template.Spec.Tolerations
		dst.Spec.NodeSelector = src.Spec.Template.Spec.NodeSelector
	}

	// Convert data store from v1beta1 back to v1alpha1
	if src.Spec.Template.Spec.DataStore.VolumeClaimTemplate != nil {
		dst.Spec.DataStore = Volume{
			VolumeClaim: &VolumeClaim{
				PersistentVolumeClaimSpec: src.Spec.Template.Spec.DataStore.VolumeClaimTemplate.Spec,
			},
		}
	} else if src.Spec.Template.Spec.DataStore.VolumeSource != nil {
		dst.Spec.DataStore = Volume{
			HostPath: src.Spec.Template.Spec.DataStore.VolumeSource.HostPath,
		}
	}

	// Convert ports
	if src.Spec.Template.Spec.GRPCPort != nil {
		dst.Spec.GRPCPort = src.Spec.Template.Spec.GRPCPort
	}
	if src.Spec.Template.Spec.HTTPPort != nil {
		dst.Spec.HTTPPort = src.Spec.Template.Spec.HTTPPort
	}
	if src.Spec.Template.Spec.SQLPort != nil {
		dst.Spec.SQLPort = src.Spec.Template.Spec.SQLPort
	}

	// Restore TerminationGracePeriodSecs from PodTemplate.
	if src.Spec.Template.Spec.PodTemplate != nil &&
		src.Spec.Template.Spec.PodTemplate.Spec.TerminationGracePeriodSeconds != nil {
		dst.Spec.TerminationGracePeriodSecs = *src.Spec.Template.Spec.PodTemplate.Spec.TerminationGracePeriodSeconds
	}

	// MaxUnavailable maps from DisruptionBudget.
	if src.Spec.DisruptionBudget != nil {
		n := int32(*src.Spec.DisruptionBudget)
		dst.Spec.MaxUnavailable = &n
	}

	// Restore v1alpha1-only fields from the preserved-fields annotation.
	if fieldsJSON, ok := src.Annotations[BetaCrdbClusterV1Alpha1FieldsAnnotationKey]; ok && fieldsJSON != "" {
		var preserved v1alpha1PreservedFields
		if err := json.Unmarshal([]byte(fieldsJSON), &preserved); err == nil {
			dst.Spec.NodeTLSSecret = preserved.NodeTLSSecret
			dst.Spec.ClientTLSSecret = preserved.ClientTLSSecret
			dst.Spec.MinAvailable = preserved.MinAvailable
			dst.Spec.AutomountServiceAccountToken = preserved.AutomountServiceAccountToken
		} else {
			conversionLog.Error(err, "failed to unmarshal v1alpha1 preserved fields annotation")
		}
	}

	// Convert status
	dst.Status = CrdbClusterStatus{
		Version:            src.Status.Version,
		CrdbContainerImage: src.Status.Image,
	}
	dst.Status.ClusterStatus = src.Annotations[clusterStatusAnnotationKey]
	if src.Status.Migration != nil {
		if migrationBytes, err := json.Marshal(src.Status.Migration); err == nil {
			dst.Annotations[BetaCrdbClusterMigrationStatusAnnotationKey] = string(migrationBytes)
		} else {
			conversionLog.Error(err, "failed to marshal migration status annotation")
		}
	} else {
		delete(dst.Annotations, BetaCrdbClusterMigrationStatusAnnotationKey)
	}

	// Convert conditions
	if len(src.Status.Conditions) > 0 {
		dst.Status.Conditions = make([]ClusterCondition, len(src.Status.Conditions))
		for i, condition := range src.Status.Conditions {
			dst.Status.Conditions[i] = ClusterCondition{
				Type:               ClusterConditionType(condition.Type),
				Status:             condition.Status,
				LastTransitionTime: condition.LastTransitionTime,
			}
		}
	}

	return nil
}

// filterOperatorEnvVars removes env vars that the enterprise operator injects internally
// so they are not written back into v1alpha1 PodEnvVariables. Keeping them there would
// cause the v1alpha1 operator to re-add them alongside its own auto-generated vars.
func filterOperatorEnvVars(envVars []corev1.EnvVar) []corev1.EnvVar {
	result := make([]corev1.EnvVar, 0, len(envVars))
	for _, env := range envVars {
		if !operatorInjectedEnvVars[env.Name] {
			result = append(result, env)
		}
	}
	return result
}

// deduplicateEnvVars merges existing and required env vars, ensuring no duplicates.
// Required vars override existing ones with the same name.
// The result preserves the order of existing vars (first occurrence wins for duplicates
// within existing), then appends any new required vars not already present.
func deduplicateEnvVars(existing []corev1.EnvVar, required []corev1.EnvVar) []corev1.EnvVar {
	// Build a map of required vars for quick lookup.
	reqMap := make(map[string]corev1.EnvVar, len(required))
	for _, env := range required {
		reqMap[env.Name] = env
	}

	// Track which required vars have been merged. The seen map skips duplicate names
	// within existing so they do not produce duplicate output entries.
	merged := make(map[string]bool, len(required))
	seen := make(map[string]bool)

	// Iterate existing in order; override with required if present, skip seen duplicates.
	result := make([]corev1.EnvVar, 0, len(existing)+len(required))
	for _, env := range existing {
		if seen[env.Name] {
			continue
		}
		seen[env.Name] = true
		if req, ok := reqMap[env.Name]; ok {
			result = append(result, req)
			merged[env.Name] = true
		} else {
			result = append(result, env)
		}
	}

	// Append required vars that weren't already in existing (preserving required order).
	for _, env := range required {
		if !merged[env.Name] {
			result = append(result, env)
		}
	}

	return result
}

package v1beta1

import (
	"slices"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReconciliationMode describes the modus operandi of the CrdbCluster
// controller when reconciling CrdbNodes.
type ReconciliationMode string

// This file defines a CrdbCluster custom resource definition (CRD) object.
// NOTE: json tags are required.  Any new fields you add must have json tags for
// the fields to be serialized.
const (
	// CrdbClusterKind is the CrdbCluster CRD kind string.
	CrdbClusterKind = "CrdbCluster"

	// LocalPathStorageClass refers to the Rancher local-path-provisioner that
	// creates persistent volumes that utilize the local storage in each node.
	// See https://github.com/rancher/local-path-provisioner.
	// This is used for testing only.
	LocalPathStorageClass = "local-path"

	// MutableOnly ReconciliationMode reconciles mutable fields of all
	// CrdbNodes. New CrdbNodes will be created and CrdbNodes will be
	// decommissioned. MutableOnly is the default ReconciliationMode if one is
	// not specified.
	MutableOnly ReconciliationMode = "MutableOnly"

	// CreateOnly ReconciliationMode disables reconciliation for existing
	// CrdbNodes. New CrbdNodes nodes will be created inline with
	// CrdbNodeTemplate. CrdbNodes will not be decommissioned.
	CreateOnly ReconciliationMode = "CreateOnly"

	// Disabled ReconciliationMode disables reconciliation of CrdbNodes.
	// Changes to the CrdbNodeTemplate will not be propagated, new CrdbNodes
	// will not be created, and CrdbNodes will not be decommissioned.
	Disabled ReconciliationMode = "Disabled"
)

// CrdbNodeTemplate is the template from which CrdbNodes will be created or
// reconciled towards.
type CrdbNodeTemplate struct {
	// ObjectMeta is a set of metadata that will be propagated to CrdbNodes.
	// Only Labels and Annotations will be respected.
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec is used as the template to construct or reconcile the Spec of
	// CrdbNodes, depending on the ReconciliationMode of the CrdbCluster.
	// +kubebuilder:validation:Required
	Spec CrdbNodeSpec `json:"spec,omitempty"`
}

// PostInitSQL specifies SQL statements to execute once after the cluster has
// been initialized. Statements run in the order: secretRef contents, then
// configMapRef contents, then inline. Execution is all-or-nothing per
// reconcile. Any failure flips the PostInitSQLApplied condition to False
// with the error in Reason/Message. Users are responsible for writing
// idempotent SQL (IF NOT EXISTS, ON CONFLICT DO NOTHING) because all
// statements replay from the beginning after a fix. Wrap statements in
// BEGIN/COMMIT if atomicity across multiple statements is required.
type PostInitSQL struct {
	// Inline is an ordered list of SQL statements to execute. Each entry
	// is sent as a single Exec; with the simple-query protocol enabled,
	// an entry may itself contain multiple semicolon-separated statements.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	Inline []string `json:"inline,omitempty"`

	// ConfigMapRef references a ConfigMap key whose value is a SQL script.
	// The whole value is sent to CockroachDB as a single Exec; the server
	// parses the multi-statement string, so semicolons inside string
	// literals and comments are handled correctly.
	// +kubebuilder:validation:Optional
	ConfigMapRef *corev1.ConfigMapKeySelector `json:"configMapRef,omitempty"`

	// SecretRef references a Secret key whose value is a SQL script. Use
	// this for sensitive content like license keys.
	// +kubebuilder:validation:Optional
	SecretRef *corev1.SecretKeySelector `json:"secretRef,omitempty"`
}

// CrdbClusterSpec defines the desired state of CrdbCluster.
// NOTE: Run "make" to regenerate code after modifying this file.
// TODO(chrisseto): Add backward compatibility for all betaclusterctrl fields
// by installing a mutating webhook.
type CrdbClusterSpec struct {
	// Mode sets the modus operandi of the CrdbCluster controller when
	// reconciling CrdbNodes.
	// NOTE: Mode is only respected by the betaclusterctrl and is therefore
	// optional.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=MutableOnly
	// +kubebuilder:validation:Enum=MutableOnly;CreateOnly;Disabled
	Mode *ReconciliationMode `json:"mode,omitempty"`

	// Template is the object that describes the CrdbNodes that will be created
	// and reconciled by the CrdbCluster controller, depending on the
	// ReconciliationMode.
	// NOTE: Template is only respected by the betaclusterctrl and is therefore
	// optional.
	// +kubebuilder:validation:Optional
	Template CrdbNodeTemplate `json:"template"`

	// ClusterSettings is a set of CockroachDB CLUSTER SETTINGS that will be
	// set by the CrdbCluster controller via executing SET CLUSTER SETTING.
	// NOTE: ClusterSettings is only respected by the betaclusterctrl and is
	// therefore optional.
	// +kubebuilder:validation:Optional
	ClusterSettings map[string]string `json:"clusterSettings,omitempty"`

	// PostInitSQL specifies SQL statements to run once after cluster
	// initialization. See PostInitSQL for execution semantics.
	// +kubebuilder:validation:Optional
	PostInitSQL *PostInitSQL `json:"postInitSQL,omitempty"`

	// Regions specifies the regions in which this cluster is deployed, along
	// with information about how each region is configured.
	// +kubebuilder:validation:Required
	Regions []CrdbClusterRegion `json:"regions"`

	// TLSEnabled indicates whether the cluster is running in secure mode.
	// Note(alyshan): The operator currently signs the certificates using
	// k8s default signer, which is not an intended use of the Certificates
	// API. See https://github.com/cockroachdb/cockroach-operator/issues/291.
	// +kubebuilder:validation:Optional
	TLSEnabled bool `json:"tlsEnabled,omitempty"`

	// TenantHost, if defined, indicates that the CRDB cluster operates as a
	// host for logical tenant clusters. Note that TLSEnabled must be set to
	// true as the SQL Proxy does not support insecure clusters. See comment for
	// TenantHost for more details.
	// +kubebuilder:validation:Optional
	TenantHost *TenantHost `json:"tenantHost,omitempty"`

	// Features specifies the enabled ClusterFeatures for this cluster.
	// +kubebuilder:validation:Optional
	Features []ClusterFeature `json:"features,omitempty"`

	// RollingRestartDelay is the delay between node restarts during a rolling
	// update. Defaults to 1 minute.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default="1m"
	RollingRestartDelay *metav1.Duration `json:"rollingRestartDelay,omitempty"`

	// IsClusterDisrupted specifies if this cluster is intentionally crippled
	// or not. Functionality is handled by Intrusion and this value is
	// informative more than anything.
	// +kubebuilder:validation:Optional
	IsClusterDisrupted bool `json:"isClusterDisrupted,omitempty"`

	// DisruptionBudget specifies the max number of nodes that can be brought
	// offline during rolling restarts. Defaults to 1.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	DisruptionBudget *int `json:"disruptionBudget,omitempty"`
}

// IsTenantHost returns true if the CRDB cluster operates as a host for logical
// tenant clusters.
func (s *CrdbClusterSpec) IsTenantHost() bool {
	return s.TenantHost != nil
}

// Nodes returns the total number of CRDB nodes across all regions in the
// cluster.
func (s *CrdbClusterSpec) Nodes() int {
	cnt := 0
	for i := range s.Regions {
		cnt += int(s.Regions[i].Nodes)
	}
	return cnt
}

// Region returns the CrdbClusterRegion that matches the given name.
func (s *CrdbClusterSpec) Region(regionName string) *CrdbClusterRegion {
	for i := range s.Regions {
		if s.Regions[i].Code == regionName {
			return &s.Regions[i]
		}
	}
	return nil
}

// NodesInRegion returns the total number of CRDB nodes in a particular region
func (s *CrdbClusterSpec) NodesInRegion(regionName string) int32 {
	region := s.Region(regionName)
	if region != nil {
		return region.Nodes
	}
	return 0
}

// FeatureEnabled returns true if the supplied feature is enabled.
func (s *CrdbClusterSpec) FeatureEnabled(f ClusterFeature) bool {
	return slices.Contains(s.Features, f)
}

// CrdbClusterRegion describes a region in which CRDB cluster nodes operate. It
// is used to generate the --join flag passed to each CrdbNode within the
// cluster.
type CrdbClusterRegion struct {
	// Code corresponds to the cloud provider's identifier of this region (e.g.
	// "us-east-1" for AWS, "us-east1" for GCP). This value is used to detect
	// which CrdbClusterRegion will be reconciled and must match the
	// "topology.kubernetes.io/region" label on Kubernetes Nodes in this
	// cluster.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength:=1
	Code string `json:"code"`

	// Nodes is the number of CRDB nodes that are in the region.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum:=0
	Nodes int32 `json:"nodes"`

	// CloudProvider sets the cloud provider for this region. When set, this value
	// is used to prefix the locality flag for all nodes in the region.
	// +kubebuilder:validation:Optional
	CloudProvider string `json:"cloudProvider,omitempty"`

	// Namespace is the name of the Kubernetes namespace that this
	// CrdbClusterRegion is deployed within. It is used to compute the --join
	// flag for this region. Defaults to the .Code of this region and then the
	// Namespace of this CrdbCluster, if not provided.
	// +kubebuilder:validation:Optional
	Namespace string `json:"namespace,omitempty"`

	// Domain is the domain of the CrdbClusterRegion.
	// Other regions need to reach this region by connecting to
	// <cluster-name>.<namespace>.svc.<domain>.
	// It defaults an empty string, but this will not work
	// in a multi-region setup, where CrdbCluster objects are potentially
	// in different namespaces.
	// It will also not work if the k8s cluster has a custom domain.
	Domain string `json:"domain,omitempty"`

	// EncryptionAtRest contains all secret names and keys for EAR encryption.
	// +kubebuilder:validation:Optional
	EncryptionAtRest *EncryptionAtRest `json:"encryptionAtRest,omitempty"`
}

// EncryptionAtRest contains secrets for managing Encryption at Rest.
type EncryptionAtRest struct {
	// KeySecretName is the name of the k8s secret containing the (new)
	// store key. If nil, this will be interpreted as "plain" i.e.
	// unencrypted.
	// +kubebuilder:validation:Optional
	KeySecretName *string `json:"keySecretName,omitempty"`

	// Platform is the cloud platform whose KMS is used to gate the
	// new Customer-Managed Encryption Key (CMEK). This string value can
	// be mapped to CMEKKeyType with the CMEKKeyType_value map.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=UNKNOWN_KEY_TYPE;AWS_KMS;GCP_CLOUD_KMS;AZURE_KEY_VAULT
	Platform EncryptionPlatform `json:"platform"`

	// CMEKCredentialsSecretName is the name of the k8s secret containing
	// our credentials that are needed to authenticate into the customer's
	// KMS. This value is required if Platform is non-zero.
	// +kubebuilder:validation:Optional
	CMEKCredentialsSecretName *string `json:"cmekCredentialsSecretName,omitempty"`

	// OldKeySecretName is the name of the k8s secret containing the old
	// store key. If nil, this will be interpreted as "plain" i.e. unencrypted.
	// +kubebuilder:validation:Optional
	OldKeySecretName *string `json:"oldKeySecretName,omitempty"`

	// OldPlatform is the cloud platform whose KMS was used to encrypt the
	// old store key. Only needed when the old key uses a different KMS
	// provider than the current Platform (e.g. switching from AWS to GCP)
	// or when disabling EAR entirely (KeySecretName is nil). If unset,
	// defaults to Platform.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Enum=UNKNOWN_KEY_TYPE;AWS_KMS;GCP_CLOUD_KMS;AZURE_KEY_VAULT
	OldPlatform EncryptionPlatform `json:"oldPlatform,omitempty"`

	// OldCMEKCredentialsSecretName is the name of the k8s secret containing
	// credentials for the old KMS. Only needed when switching cloud KMS
	// providers or disabling EAR. If unset, defaults to
	// CMEKCredentialsSecretName.
	// +kubebuilder:validation:Optional
	OldCMEKCredentialsSecretName *string `json:"oldCmekCredentialsSecretName,omitempty"`
}

type EncryptionPlatform string

const (
	EncryptionPlatformUnknown       = EncryptionPlatform("UNKNOWN_KEY_TYPE")
	EncryptionPlatformAwsKms        = EncryptionPlatform("AWS_KMS")
	EncryptionPlatformGcpCloudKms   = EncryptionPlatform("GCP_CLOUD_KMS")
	EncryptionPlatformAzureKeyVault = EncryptionPlatform("AZURE_KEY_VAULT")
)

// ResolvedOldPlatform returns the KMS platform for the old store key. If
// OldPlatform is explicitly set, it is used. Otherwise it falls back to
// Platform so that same-provider key rotations work without extra fields.
func (e *EncryptionAtRest) ResolvedOldPlatform() EncryptionPlatform {
	if e.OldPlatform != "" {
		return e.OldPlatform
	}
	return e.Platform
}

// ResolvedOldCMEKCredentialsSecretName returns the credentials secret for the
// old KMS. Falls back to CMEKCredentialsSecretName when not explicitly set.
func (e *EncryptionAtRest) ResolvedOldCMEKCredentialsSecretName() *string {
	if e.OldCMEKCredentialsSecretName != nil {
		return e.OldCMEKCredentialsSecretName
	}
	return e.CMEKCredentialsSecretName
}

func ParseEncryptionPlatform(name string) EncryptionPlatform {
	switch name {
	case string(EncryptionPlatformAwsKms):
		return EncryptionPlatformAwsKms
	case string(EncryptionPlatformGcpCloudKms):
		return EncryptionPlatformGcpCloudKms
	case string(EncryptionPlatformAzureKeyVault):
		return EncryptionPlatformAzureKeyVault
	default:
		return EncryptionPlatformUnknown
	}
}

func (p EncryptionPlatform) String() string {
	return string(p)
}

// TenantPool defines the desired runtime attributes of the Pods that
// tenants will be assigned (stamped) into.
type TenantPool struct {
	// Name is the unique handle to this TenantPool definition. Tenants will
	// use this to select the pool they desire to be scheduled in.
	// +kubebuilder:validation:Pattern=`^[a-z]+[a-z0-9-]{1,241}$`
	Name string `json:"name"`

	// AllowInternetAccess indicated whether or not tenants in this pool should
	// be able to connect to the internet. Enabling internet access enables
	// tenants to make use of feature such as BACKUP and RESTORE.
	// +kubebuilder:validation:Optional
	AllowInternetAccess bool `json:"allowInternetAccess,omitempty"`

	// HostReplicaMultiplier is the number of pods in a region's tenantpool as a
	// proportion of the number of CRDB nodes in that region. This value
	// represents the average warm pool pods per Kubernetes node since the
	// scheduler will distribute the pods when scheduling the tenantpool
	// deployment.
	//
	// For any region, the number of tenantpool pods, with a minimum of three,
	// can be computed as follows:
	//
	//     ceil(max(3, HostReplicaMultiplier * Nodes))
	//
	// For example, if HostReplicaMultiplier = 0.33, and there are 12 CRDB nodes
	// in the region, then we have ceil(max(3, 3.96)) = 4 tenantpool pods. This
	// currently assumes that the multiplier is the same across all regions.
	// +kubebuilder:validation:Optional
	HostReplicaMultiplier *float32 `json:"hostReplicaMultiplier,omitempty"`

	// Resources is the requests and limits of various resource types for this
	// set of tenant pods. Limits are optional. Requests will be defaulted to
	// the below values if not specified.
	// CPU: 50m, Memory: 750M, EphemeralStorage: 250Mi.
	// +kubebuilder:validation:Required
	Resources corev1.ResourceRequirements `json:"resources"`

	// PriorityClassName is the name of the k8s priority class for this set of
	// tenant pods.
	PriorityClassName string `json:"priorityClassName,omitempty"`

	// MaxPods is the default maximum number of SQL tenant pods that can be
	// stamped and running for tenants using this pool. This value is only used
	// when the CrdbTenant.MaxPods field is not set for a particular tenant.
	// If MaxPods is nil, then no max limit will be enforced.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Optional
	MaxPods *int32 `json:"maxPods,omitempty"`

	// CockroachImage is the location and name of the CockroachDB Docker image
	// to deploy (e.g. cockroachdb/cockroach:v20.1.1). If provided, CockroachImage
	// will take precedence over host cluster's CockroachImage.
	// +kubebuilder:validation:Optional
	CockroachImage string `json:"image,omitempty"`

	// LoggingConfigMapName is the name of a ConfigMap containing logging
	// configuration for CRDB SQL pods. If not specified, INFO logs will be
	// written to stderr for the health and dev channels.
	// +kubebuilder:validation:Optional
	LoggingConfigMapName string `json:"loggingConfigMapName,omitempty"`
}

// TenantHost describes the desired state of a CRDB cluster that operates as a
// host for logical tenant clusters. In addition to the set of resources
// installed by the CrdbCluster, a tenant host cluster will install a SQL Proxy
// deployment, a tenant pool deployment, and other resources.
type TenantHost struct {
	// SqlProxyImage is the location and name of the SQL Proxy Docker image to
	// deploy. The SQL Proxy functions as a reverse proxy for the CRDB tenant
	// host cluster. It is responsible for routing incoming tenant connections
	// requests to one of the SQL pods owned by that tenant. This field defaults
	// to a recent build of the SQL Proxy from the "us.gcr.io/cockroach-cloud-images"
	// container registry.
	// +kubebuilder:validation:Optional
	SqlProxyImage string `json:"sqlProxyImage,omitempty"`

	// SqlProxyReplicas controls the replicas of the SQL Proxy Deployment
	// created by the operator. In most cases, the default should be relied on.
	// This field exists as an escape hatch for scaling up the SQL Proxy in
	// production and for deploying only one instance in test cases.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Optional
	SqlProxyReplicas *int32 `json:"sqlProxyReplicas,omitempty"`

	// Certificates controls the TLS configuration for TenantHosts.
	// NOTE: Certificates is only respected by the betaclusterctrl and is
	// therefore optional for now.
	// +kubebuilder:validation:Optional
	Certificates *TenantCertificates `json:"certificates,omitempty"`

	// TenantDirImage is the location and name of the tenantdir Docker image to
	// deploy. The tenantdir maintains an up-to-date mapping between each active
	// tenant and the list of SQL pods currently assigned to that tenant. It
	// runs as a sidecar container to the SQL Proxy, which calls it as part of
	// routing incoming connections to the right SQL pod. This field defaults to
	// a recent build of tenantdir from the "us.gcr.io/cockroach-cloud-images"
	// container registry.
	// +kubebuilder:validation:Optional
	TenantDirImage string `json:"tenantDirImage,omitempty"`

	// TenantOtelCollectorImage is the location and name of the OpenTelemetry
	// Collector Docker image to deploy for *SQL pods*. This field defaults to
	// the `otelCollectorDefaultImage` field defined in tenant_pool.go.
	//
	// NOTE: This was introduced to help with testing without the need of
	// building a new operator image (e.g. before rolling out a new version of
	// the collector to tenants).
	//
	// TODO(jaylim-crl): Consider moving this to the TenantPool struct to allow
	// for a per-tenantpool collector image. Currently, we do not do this for
	// several reasons:
	//   1. There is ongoing work to migrate existing tenant pools, which were
	//      previously defined as high/low-trust, to CRDB-version-specific pools.
	//   2. We lack the RPC/admin-cli infrastructure necessary to modify tenant
	//      pools.
	//   3. There is no compelling need at present to customize the collector
	//      image for each tenant pool.
	//
	// +kubebuilder:validation:Optional
	TenantOtelCollectorImage string `json:"tenantOtelCollectorImage,omitempty"`

	// TenantPools is a collection of TenantPool definitions that individual
	// tenants may be assigned to. Pools may have varying resource limits or
	// requests and may be used implement different "tiers" of tenants.
	// Defaults to a single restrictive pool named "default".
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=10
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:default={{"name":"default","resources":{"limits":{"cpu":"333m","memory":"1500M","ephemeral-storage":"380Mi"}}}}
	TenantPools []TenantPool `json:"tenantPools,omitempty"`
}

// TenantPool finds a TenantPool by its .Name. If no matching
// TenantPool is found, nil is returned.
func (s *TenantHost) TenantPool(name string) (pool *TenantPool, ok bool) {
	for _, pool := range s.TenantPools {
		if pool.Name == name {
			return &pool, true
		}
	}
	return nil, false
}

// TenantCertificates controls the TLS configuration of the various services used
// by TenantHosts. These secrets are expected to exist already and are not
// controlled by the operator.
// NOTE: Currently these values are only supported by the betaclusterctrl
// controller.
type TenantCertificates struct {
	// SQLProxySecretName is the name of a Kubernetes TLS secret that will be used
	// for the SQLProxy service.
	// +kubebuilder:validation:Required
	SQLProxySecretName string `json:"sqlProxySecretName,omitempty"`

	// SQLProxyTenantCASecretName is the name of a Kubernetes secret containing a
	// ca.crt entry that is used to secure Proxy -> Tenant communication.
	// +kubebuilder:validation:Required
	SQLProxyTenantCASecretName string `json:"sqlProxyTenantCASecretName,omitempty"`

	// SQLStarterCASecretName is the name of a Kubernetes secret containing a ca.crt
	// entry that will be used as the CA for the SQL starter.
	// +kubebuilder:validation:Required
	SQLStarterCASecretName string `json:"sqlStarterCASecretName,omitempty"`

	// SQLStarterSecretName is the name of a Kubernetes TLS secret that will be used
	// by the SQL starter.
	// +kubebuilder:validation:Required
	SQLStarterSecretName string `json:"sqlStarterSecretName,omitempty"`
}

// CrdbClusterStatus defines the observed state of CrdbCluster.
// NOTE: Run "make" to regenerate code after modifying this file
type CrdbClusterStatus struct {
	// ObservedGeneration is the value of the ObjectMeta.Generation last
	// reconciled by the controller.
	// Note(alyshan): ObjectMeta.Generation uses int64, so we match the type.
	ObservedGeneration int64 `json:"observedGeneration"`

	// Actions are the set of operations taken on this cluster.
	Actions []ClusterAction `json:"actions,omitempty"`

	// Conditions are the set of current status indicators for the cluster.
	Conditions []ClusterCondition `json:"conditions,omitempty"`

	// InitializationNode is the CrdbNode selected to bootstrap this cluster.
	// The operator retains it across retries and clears it when initialization
	// succeeds.
	InitializationNode string `json:"initializationNode,omitempty"`

	// Settings contains the cluster settings for the CRDB cluster.
	Settings map[string]string `json:"settings,omitempty"`

	// ReadyNodes is the number of nodes that are ready in this region.
	ReadyNodes int32 `json:"readyNodes,omitempty"`

	// Reconciled indicates whether the spec of ObservedGeneration is reconciled.
	Reconciled bool `json:"reconciled,omitempty"`

	// ReconciledByBetaController is true if the cluster is reconciled by
	// the beta cluster controller.
	ReconciledByBetaController bool `json:"reconciledByBetaController,omitempty"`

	// Provider is the name of the cloud provider that this object's k8s server is in.
	Provider string `json:"provider,omitempty"`

	// Region is the name of the region that this crdbcluster object's k8s server is in.
	// This is useful for consumers to determine if this region's crdb pods
	// are ready, etc..
	Region string `json:"region,omitempty"`

	// CurrentRevision is the fingerprint of the revision this CrdbCluster
	// believes to be current.
	CurrentRevision string `json:"currentRevision,omitempty"`

	// PreviousRevision is the fingerprint of the last revision that was
	// successfully rolled out to this CrdbCluster's CrdbNodes.
	PreviousRevision string `json:"previousRevision,omitempty"`

	// Image is the CockroachDB image currently running in this cluster.
	// +kubebuilder:validation:Optional
	Image string `json:"image,omitempty"`

	// Version is the version of CockroachDB currently running in this cluster.
	// This is populated by specifying the version where version is the output of executing
	// `cockroach version` command on running pods.
	// +kubebuilder:validation:Optional
	Version string `json:"version,omitempty"`

	// Migration tracks the state of v1alpha1 -> v1beta1 migration.
	// This field is populated when a StatefulSet or v1alpha1 CrdbCluster is being
	// migrated to v1beta1 CrdbCluster + CrdbNode resources.
	// +kubebuilder:validation:Optional
	Migration *MigrationStatus `json:"migration,omitempty"`
}

// MigrationStatus tracks the state of a v1alpha1 to v1beta1 migration.
type MigrationStatus struct {
	// Phase is the current migration phase.
	Phase string `json:"phase"`

	// Type indicates whether this is a "helm" or "operator" migration.
	// +kubebuilder:validation:Optional
	Type string `json:"type,omitempty"`

	// StartTime is when the migration started.
	// +kubebuilder:validation:Optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// LastUpdateTime is when the status was last updated.
	// +kubebuilder:validation:Optional
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`

	// NodeMigrationStartTime tracks when we started migrating the current node.
	// We need this separate from LastUpdateTime since that gets updated on every
	// reconcile loop, which would break the timeout check. This only gets set when
	// we start a new node migration and cleared when it completes.
	// +kubebuilder:validation:Optional
	NodeMigrationStartTime *metav1.Time `json:"nodeMigrationStartTime,omitempty"`

	// CurrentPodIndex is the index of the pod currently being migrated.
	// +kubebuilder:validation:Optional
	CurrentPodIndex int32 `json:"currentPodIndex,omitempty"`

	// TotalPods is the total number of pods to migrate. Also used during rollback
	// to restore the StatefulSet to its pre-migration replica count.
	// +kubebuilder:validation:Optional
	TotalPods int32 `json:"totalPods,omitempty"`

	// MigratedNodes tracks which CrdbNodes have been created.
	// +kubebuilder:validation:Optional
	MigratedNodes []string `json:"migratedNodes,omitempty"`

	// Message contains a human-readable status message.
	// +kubebuilder:validation:Optional
	Message string `json:"message,omitempty"`

	// LastError contains the last error encountered.
	// +kubebuilder:validation:Optional
	LastError string `json:"lastError,omitempty"`
}

// SetAction adds a new action to the cluster actions list if no matching
// action exists, or else overwrites the existing action.
func (s *CrdbClusterStatus) SetAction(typ ActionType, st ActionStatus) {
	// Use timeutil.Now to get current time, since it can be hooked in tests.
	action := ClusterAction{
		Type:               typ,
		Status:             st,
		LastTransitionTime: metav1.NewTime(timeutil.Now()),
	}
	for i := range s.Actions {
		if s.Actions[i].Type == typ {
			s.Actions[i] = action
			return
		}
	}
	s.Actions = append(s.Actions, action)
}

// HasAction returns true if an action of the given type exists in the
// action list and has the given status value.
func (s *CrdbClusterStatus) HasAction(typ ActionType, st ActionStatus) bool {
	for i := range s.Actions {
		if s.Actions[i].Type == typ {
			return s.Actions[i].Status == st
		}
	}
	return false
}

// ActionPresent returns true if an action of the given type exists in the
// action list.
func (s *CrdbClusterStatus) ActionPresent(typ ActionType) bool {
	for i := range s.Actions {
		if s.Actions[i].Type == typ {
			return true
		}
	}
	return false
}

// GetCondition returns the condition status, last transition time
// of the given type, and "true" if the given condition type exists.
// Otherwise, it returns empty values for the former two and "false".
func (s *CrdbClusterStatus) GetCondition(
	typ ClusterConditionType,
) (st metav1.ConditionStatus, ts time.Time, exists bool) {
	if cond, ok := s.FindCondition(typ); ok {
		return cond.Status, cond.LastTransitionTime.Time, true
	}
	return
}

// FindCondition returns the full condition of the given type and whether it
// exists. Unlike GetCondition it exposes Reason and Message, not just Status.
func (s *CrdbClusterStatus) FindCondition(typ ClusterConditionType) (ClusterCondition, bool) {
	for i := range s.Conditions {
		if s.Conditions[i].Type == typ {
			return s.Conditions[i], true
		}
	}
	return ClusterCondition{}, false
}

// HasCondition returns true if a condition of the given type exists in the
// condition list and has the given status value.
func (s *CrdbClusterStatus) HasCondition(typ ClusterConditionType, st metav1.ConditionStatus) bool {
	cond, _, ok := s.GetCondition(typ)
	return ok && cond == st
}

// SetConditionWithTimestamp adds a new condition to the cluster condition list
// if no matching condition exists yet, or else overwrites
// the existing condition if the existing timestamp is before or equal
// to the input timestamp.
func (s *CrdbClusterStatus) SetConditionWithTimestamp(
	typ ClusterConditionType, status metav1.ConditionStatus, ts time.Time,
) {
	s.setConditionWithTimestampAndMessage(typ, status, "", "", ts)
}

// SetCondition adds a new condition to the cluster condition list if no matching
// condition exists yet, or else overwrites the existing condition.
func (s *CrdbClusterStatus) SetCondition(typ ClusterConditionType, status metav1.ConditionStatus) {
	// If the condition already exists with the same status, don't touch it.
	// This keeps LastTransitionTime stable unless the status actually changes.
	curStatus, _, exists := s.GetCondition(typ)
	if exists && curStatus == status {
		return
	}
	// If Status gets changed or condition didn’t exist, update the condition with the latest time.
	// Use timeutil.Now to get current time, since it can be hooked in tests.
	s.SetConditionWithTimestamp(typ, status, timeutil.Now())
}

// SetConditionWithMessage is like SetCondition but also records a short Reason
// token and a human-readable Message. LastTransitionTime is reset only when
// Status actually changes; Reason/Message updates without a status change
// refresh the fields in place.
func (s *CrdbClusterStatus) SetConditionWithMessage(
	typ ClusterConditionType, status metav1.ConditionStatus, reason, message string,
) {
	ts := timeutil.Now()
	for i := range s.Conditions {
		if s.Conditions[i].Type == typ {
			if s.Conditions[i].Status != status {
				s.Conditions[i].LastTransitionTime = metav1.NewTime(ts)
			}
			s.Conditions[i].Status = status
			s.Conditions[i].Reason = reason
			s.Conditions[i].Message = message
			return
		}
	}
	s.setConditionWithTimestampAndMessage(typ, status, reason, message, ts)
}

// setConditionWithTimestampAndMessage is the inner helper shared by
// SetConditionWithTimestamp and SetConditionWithMessage.
func (s *CrdbClusterStatus) setConditionWithTimestampAndMessage(
	typ ClusterConditionType, status metav1.ConditionStatus, reason, message string, ts time.Time,
) {
	cond := ClusterCondition{
		Type:               typ,
		Status:             status,
		LastTransitionTime: metav1.NewTime(ts),
		Reason:             reason,
		Message:            message,
	}
	for i := range s.Conditions {
		if s.Conditions[i].Type == typ {
			if !s.Conditions[i].LastTransitionTime.After(ts) {
				s.Conditions[i] = cond
			}
			return
		}
	}
	s.Conditions = append(s.Conditions, cond)
}

// ClusterCondition describes the current state of some aspect of the cluster's
// status. The operator will add these to the Conditions list as it completes
// work.
// NOTE: Conditions should always be able to be reconstructed by the operator
// based on observations it makes about the state of the cluster; these
// conditions only exist to speed up reconciliation and to provide additional
// observability to the user.
type ClusterCondition struct {
	// Type is the kind of this condition.
	// +kubebuilder:validation:Required
	Type ClusterConditionType `json:"type"`
	// Status is the current state of the condition: True, False or Unknown.
	// +kubebuilder:validation:Required
	Status metav1.ConditionStatus `json:"status"`
	// LastTransitionTime is the time at which the condition was last updated.
	// +kubebuilder:validation:Required
	LastTransitionTime metav1.Time `json:"lastTransitionTime"`
	// Reason is a short, CamelCase token categorizing the cause of the
	// current status. Optional and unset for most conditions.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message is a human-readable explanation of the current status,
	// typically used to carry an error string when Status is False.
	// Optional and unset for most conditions.
	// +optional
	Message string `json:"message,omitempty"`
}

// ClusterAction describes an operation performed by the operator on the
// CrdbCluster.
type ClusterAction struct {
	// Type is the kind of this action.
	// +kubebuilder:validation:Required
	Type ActionType `json:"type"`
	// Status is the current state of the action: Starting, Failed, Finished or Unknown.
	// +kubebuilder:validation:Required
	Status ActionStatus `json:"status"`
	// LastTransitionTime is the time at which the condition was last updated.
	// +kubebuilder:validation:Required
	LastTransitionTime metav1.Time `json:"lastTransitionTime"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:webhook:conversion

// CrdbCluster is the Schema for the crdbclusters API.
// NOTE: Don't add new fields to this struct. Instead, add fields describing
// the desired state of the CrdbCluster to CrdbClusterSpec, and fields
// describing the observed state of the CrdbCluster to CrdbClusterStatus.
type CrdbCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CrdbClusterSpec   `json:"spec,omitempty"`
	Status CrdbClusterStatus `json:"status,omitempty"`
}

// IsInitialized returns true when the cluster has been marked as initialized.
func (cluster *CrdbCluster) IsInitialized() bool {
	return cluster.Status.HasCondition(ClusterInitialized, metav1.ConditionTrue)
}

// IsManagedMode reports whether or not this CrdbCluster's .Mode field is a
// "managed" mode. Managed modes are Modes in which all CrdbNodes will be
// "managed" by the clusterctrl (reconciled).
func (cluster *CrdbCluster) IsManagedMode() bool {
	if cluster.Spec.Mode == nil {
		return false
	}
	// TODO When we add support for an "All" mode, we'll need to update this function.
	return *cluster.Spec.Mode == MutableOnly
}

// Region returns the region that the cluster object is reconciled in.
// This depends on the cluster.Status that is populated by the crdbcluster reconciler.
func (cluster *CrdbCluster) Region() *CrdbClusterRegion {
	return cluster.Spec.Region(cluster.Status.Region)
}

// Domain returns the DNS domain to use when building FQDNs for this cluster.
// It prefers the current region's Domain and falls back to
// spec.template.spec.domain when the region has no domain override. Returns
// "" when neither is set.
func (cluster *CrdbCluster) Domain() string {
	if region := cluster.Region(); region != nil && region.Domain != "" {
		return region.Domain
	}
	return cluster.Spec.Template.Spec.Domain
}

// IsReady returns nil once all nodes in the CRDB cluster are ready
// to serve traffic.
func (cluster *CrdbCluster) IsReady() error {
	if cluster.Generation > cluster.Status.ObservedGeneration {
		return errors.Newf("observed CrdbCluster generation %d is lower "+
			"than expected generation %d", cluster.Status.ObservedGeneration, cluster.Generation)
	}
	if !cluster.Status.HasCondition(ClusterInitialized, metav1.ConditionTrue) {
		// Reason and Message are recorded together, so finding a reason means we
		// can surface the full initialization failure.
		if condition, ok := cluster.Status.FindCondition(ClusterInitialized); ok && condition.Reason != "" {
			return errors.Newf("cluster is not initialized: %s: %s", condition.Reason, condition.Message)
		}
		return errors.Newf("cluster is not initialized")
	}
	if !cluster.Status.ReconciledByBetaController {
		return nil
	}

	thisRegion := cluster.Region()
	if thisRegion == nil {
		return nil
	}
	if cluster.Spec.Mode != nil && *cluster.Spec.Mode == Disabled {
		return nil
	}
	if cluster.Status.ReadyNodes < thisRegion.Nodes {
		return errors.Newf("only %d out of %d CrdbNodes are Ready", cluster.Status.ReadyNodes, thisRegion.Nodes)
	}
	return nil
}

// IsQuiescent returns nil if the provided CrdbCluster has no
// pending actions.
func (cluster *CrdbCluster) IsQuiescent() error {
	for _, action := range cluster.Status.Actions {
		if action.Type == Upgrade && action.Status != Finished {
			return errors.Newf("cluster has a pending %s action with status %s", action.Type, action.Status)
		}
	}
	if !cluster.Status.ReconciledByBetaController {
		return nil
	}
	if cluster.Status.ObservedGeneration != cluster.Generation {
		return errors.Newf("observed cluster generation %d is behind %d",
			cluster.Status.ObservedGeneration, cluster.Generation)
	}
	if cluster.Spec.Mode != nil && *cluster.Spec.Mode == Disabled {
		return nil
	}
	// TODO(ye): add support for CreationOnly mode.
	//  Likely, we will need to add a "nodeCount" field to the status struct,
	//  and compare that with Spec.Regions[x].count.
	if !cluster.Status.Reconciled {
		return errors.Newf("cluster is not fully reconciled")
	}
	return nil
}

// +kubebuilder:object:root=true

// CrdbClusterList contains a list of CrdbCluster.
type CrdbClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CrdbCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CrdbCluster{}, &CrdbClusterList{})
}

type CrdbWalFailoverStatus string

const (
	WalEnable  CrdbWalFailoverStatus = "enable"
	WalDisable CrdbWalFailoverStatus = "disable"
	WalNotSet  CrdbWalFailoverStatus = ""
)

type CrdbWalFailoverSpec struct {
	Name             string                `json:"name,omitempty"`
	Size             string                `json:"size"`
	StorageClassName string                `json:"storageClassName,omitempty"`
	Status           CrdbWalFailoverStatus `json:"status"`
	Path             string                `json:"path,omitempty"`
}

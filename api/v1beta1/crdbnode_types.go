package v1beta1

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/securitycontext"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
	"github.com/cockroachlabs/version"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
)

type NodeConditionType string

// NodeDecommissionState enumerates the possible decommissioning states that a
// node can be in.
//
// There are three states to decommissioning:
// 1. CRDB is actively transferring range replicas to other nodes.
// 2. CRDB is finished transferring range replicas, but is awaiting a finalization step.
// 3. The finalization step has occurred.
type NodeDecommissionState string

// VirtualClusterMode enumerates the possible virtual cluster modes that a
// node can be in.
type VirtualClusterMode string

const (
	// CrdbNodeKind is the CrdbNode CRD kind string.
	CrdbNodeKind = "CrdbNode"

	// CrdbNodeDecommissionAnnotation is an annotation indicating that the node
	// should be prioritized when picking a node to decommission.
	CrdbNodeDecommissionAnnotation = "crdb.cockroachlabs.com/decommission"

	// K8sNodeStartDecommissionAnnotation is an annotation applied to a k8s node
	// indicating that crdb nodes scheduled to the k8s node should be
	// decommissioned.
	K8sNodeStartDecommissionAnnotation = "crdb.cockroachlabs.com/decommission"

	CrdbNodeStartDecommissionAnnotation = "internal.crdb.cockroachlabs.com/decommission"

	// CrdbNodeStartPendingDecommissionAnnotation is an annotation where the CRDB node is in
	// a non-running state and prioritized for decommissioning during scale down operation.
	CrdbNodeStartPendingDecommissionAnnotation = "internal.crdb.cockroachlabs.com/pending-decommission"

	CrdbNodeSuspendAnnotation = "crdb.cockroachlabs.com/suspend"
	CrdbNodeExcludeAnnotation = "crdb.cockroachlabs.com/excluded"

	// PodReady is a duplicate of Kubernetes' Pod's Ready Condition but
	// reported from the CrdbNode controller. If true, it implies that
	// PodRunning is also true.
	PodReady NodeConditionType = "PodReady"

	// PodReady is a duplicate of Kubernetes' Pod's Running Condition but
	// reported from the CrdbNode controller.
	PodRunning NodeConditionType = "PodRunning"

	// PodNeedsUpdate indicates that the nodectrl has determined that the
	// underlying Pod of this CrdbNode should be updated to a new PodSpec.
	PodNeedsUpdate NodeConditionType = "PodNeedsUpdate"

	// Default ports.
	DefaultCockroachGRPCPort = 26258
	DefaultCockroachHTTPPort = 8080
	DefaultCockroachSQLPort  = 26257

	// VirtualClusterModeNone is the VirtualClusterMode where the virtual cluster is disabled.
	VirtualClusterModeNone VirtualClusterMode = "disabled"
	// VirtualClusterModePrimary is the VirtualClusterMode where the virtual cluster is primary.
	VirtualClusterModePrimary VirtualClusterMode = "primary"
	// VirtualClusterModeStandby is the VirtualClusterMode where the virtual cluster is standby.
	VirtualClusterModeStandby VirtualClusterMode = "standby"
)

// Decommissioning related constants.
const (
	// Draining indicates the node has started to drain SQL connections and
	// leases.
	Draining NodeDecommissionState = "draining"

	// Drained indicates the node has successfully drained SQL connections
	// and leases.
	Drained NodeDecommissionState = "drained"

	// TransferringReplicas is the NodeDecommissionState where the CRDB node is
	// still transferring replicas to other nodes.
	TransferringReplicas NodeDecommissionState = "transferringReplicas"

	// ZeroReplicas is the NodeDecommissionState where the CRDB node is finished
	// transferring replicas to other nodes, but is awaiting a finalization
	// step.
	ZeroReplicas NodeDecommissionState = "zeroReplicas"

	// Decommissioned is the NodeDecommissionState where the CRDB node is fully
	// decommissioned.
	Decommissioned NodeDecommissionState = "decommissioned"
)

// CrdbVirtualClusterSpec defines the configuration to set up the virtual cluster.
type CrdbVirtualClusterSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:default=disabled
	// +kubebuilder:validation:Enum=disabled;primary;standby
	Mode VirtualClusterMode `json:"mode,omitempty"`
}

// CrdbNodeSpec defines the desired state of an individual
// CockroachDB node process.
type CrdbNodeSpec struct {
	// PodAnnotations are the annotations that should be applied to the
	// underlying CRDB pod.
	// Deprecated: use `PodTemplate` instead.
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`

	// PodLabels are the labels that should be applied to the underlying CRDB
	// pod.
	// Deprecated: use `PodTemplate` instead.
	PodLabels map[string]string `json:"podLabels,omitempty"`

	// Image is the location and name of the CockroachDB container image to
	// deploy (e.g. cockroachdb/cockroach:v20.1.1).
	// +kubebuilder:validation:Required
	Image string `json:"image,omitempty"`

	// InitContainerImage is the location and name of the CockroachDB init
	// container to deploy. Defaults to [manifests.defaultInitContainerImage].
	// +kubebuilder:validation:Optional
	InitContainerImage string `json:"initContainerImage,omitempty"`

	// Env is a list of environment variables to set in the container.
	// +kubebuilder:validation:Optional
	// Deprecated: use `PodTemplate` instead.
	Env []corev1.EnvVar `json:"env,omitempty"`

	// DataStore specifies the disk configuration for this CrdbNode.
	// +kubebuilder:validation:Required
	DataStore DataStore `json:"dataStore,omitempty"`

	// LogsStore specifies an optional dedicated disk configuration for CockroachDB logs.
	// If not specified, logs will be stored in a subdirectory of DataStore (/cockroach/cockroach-data/logs).
	// If specified, logs will be stored on a separate volume mounted at the path specified by
	// LogsStore.MountPath (or the default path /cockroach/cockroach-data/logs if not set).
	// +kubebuilder:validation:Optional
	LogsStore *CrdbLogsStoreSpec `json:"logsStore,omitempty"`

	// Certificates controls the TLS configuration of this CrdbNode.
	// +kubebuilder:validation:Optional
	Certificates Certificates `json:"certificates,omitempty"`

	// ResourceRequirements is the resource requirements for the main
	// crdb container.
	// +kubebuilder:validation:Optional
	// Deprecated: use `PodTemplate` instead.
	ResourceRequirements corev1.ResourceRequirements `json:"resourceRequirements,omitempty"`

	// ConditionalResourceRequirements describes resource requirements  that are
	// conditionally determined based on the node's capacity.
	// +kubebuilder:validation:Optional
	ConditionalResourceRequirements *ConditionalResourceRequirements `json:"conditionalResources,omitempty"`

	// ServiceAccountName is the name of a service account to use for
	// running pods.
	// +kubebuilder:validation:Optional
	// Deprecated: use `PodTemplate` instead.
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// SideCars will be run in the same pod as the crdb process.
	// This can be useful for running containers for fluentbit logging.
	// +kubebuilder:validation:Optional
	//  Deprecated: use `PodTemplate` instead.
	SideCars CrdbNodeSideCars `json:"sideCars,omitempty"`

	// TopologySpreadConstraints will be used to construct the crdb pod's topology spread
	// constraints.
	// Note: any label selectors will be discarded; the operator will instead set
	// the selector to match all pods belonging to the same cluster.
	// +kubebuilder:validation:Optional
	// Deprecated: use `PodTemplate` instead.
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// Join will be used for setting the join flag.
	// Defaults to <cluster-name>.<ns>.svc.cluster.local.
	// +kubebuilder:validation:Optional
	Join string `json:"join,omitempty"`

	// Domain is the cluster domain of the Kubernetes cluster this CrdbNode is
	// in. It is used to construct the address advertised by this CrdbNode.
	// Defaults to `cluster.local`.
	// +kubebuilder:validation:Optional
	Domain string `json:"domain,omitempty"`

	// LoggingConfigMapName is the name of a ConfigMap containing logging
	// configuration for CRDB nodes. If not specified, INFO logs will be
	// written to stderr for the health and dev channels.
	// +kubebuilder:validation:Optional
	LoggingConfigMapName string `json:"loggingConfigMapName,omitempty"`

	// LoggingConfigVars specifies a list of environment variable names
	// that will be expanded if present in the body of the
	// logging configuration.
	// Corresponding ENV values should be set via the `Env` field or `PodTemplate`.
	// +kubebuilder:validation:Optional
	LoggingConfigVars []string `json:"loggingConfigVars,omitempty"`

	// Flags specify the flags that will be used for starting the cluster.
	// Deprecated: use `StartFlags` instead.
	// +kubebuilder:validation:Optional
	Flags map[string]string `json:"flags,omitempty"`

	// StartFlags specify the flags that will be used for starting the cluster.
	// Any flag defined in the StartFlags will take precedence over the first-class
	// fields responsible for setting the same flags.
	// +kubebuilder:validation:Optional
	StartFlags *Flags `json:"startFlags,omitempty"`

	// EncryptionAtRest contains all the information that will be needed
	// for EAR setup at CrdbNode pod initialization. This value will be updated
	// given an update to the corresponding CrdbClusterRegion's EncryptionAtRest
	// attribute.
	// TODO(orchestration): currently this field is populated internally
	// through the CrdbClusterRegion struct. Given that both CrdbClusterRegion
	// and CrdbNode template can change this field, precedence needs to be
	// determined on which source of information to use.
	// +kubebuilder:validation:Optional
	EncryptionAtRest *EncryptionAtRest `json:"encryptionAtRest,omitempty"`

	// NoCloudPrefixedLocalities indicates whether this node's locality flags
	// are prefixed with its cloud provider's short code. See
	// IntrusionCrdbCluster.NoCloudPrefixedLocalities for more info. It is part
	// of CrdbNodeSpec as opposed to CrdbClusterSpec since the init container
	// is spawned from CrdbNodeSpec. This field is expected to be removed once
	// all clusters are using a value of `false`.
	NoCloudPrefixedLocalities bool `json:"noCloudPrefixedLocalities,omitempty"`

	// LocalityLabels specifies a list of kubernetes node labels
	// to read and add to the CockroachDB locality string.
	// Deprecated: use `LocalityMappings` instead.
	// +kubebuilder:validation:Optional
	LocalityLabels []string `json:"localityLabels,omitempty"`

	// LocalityMappings specifies a list of mappings from k8s node labels to crdb
	// locality labels. In order to set the `--locality` flag for the `cockroach
	// start` command, we may need information that's available at runtime, but
	// not available when the crdb pod is scheduled. We use an init continer to
	// look up node labels at runtime, then write the contents of the
	// `--locality` flag to disk. Then we `cat` the locality file as an argument
	// to `cockroach start`. By default, map the standard
	// "topology.kubernetes.io/region" and "topology.kubernetes.io/zone" node
	// labels to "region" and "zone", respectively. For details, see
	// https://www.cockroachlabs.com/docs/stable/cockroach-start#locality.
	// +kubebuilder:validation:Optional
	LocalityMappings []LocalityMapping `json:"localityMappings,omitempty"`

	// TerminationGracePeriodSeconds determines the time available to CRDB for graceful drain.
	// It defaults to 5m.
	// See documentation for [manifests.CRDBTerminationGracePeriodSeconds].
	// +kubebuilder:validation:Optional
	// Deprecated: use `PodTemplate` instead.
	TerminationGracePeriod *metav1.Duration `json:"terminationGracePeriod,omitempty"`

	// NodeName is the name of the kubernetes corev1.Node the CrdbNode is
	// placed on. If "", then the CrdbNode is placed on a node by the
	// kubernetes scheduler.
	NodeName string `json:"nodeName,omitempty"`

	// WALFailover indicates whether we are attaching a new PVC to the node to be used
	// for WAL writes while the data dir disk encounters a stall or increased latency
	WALFailoverSpec *CrdbWalFailoverSpec `json:"walFailoverSpec,omitempty"`

	// Tolerations is the set of tolerations to apply to a node.
	// https://kubernetes.io/docs/concepts/scheduling-eviction/taint-and-toleration/
	// Deprecated: use `PodTemplate` instead.
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// NodeSelector is the set of nodeSelector labels to apply to a node.
	// https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#nodeselector
	// Deprecated: use `PodTemplate` instead.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Affinity is the affinity policy to apply to a node.
	// https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#affinity-and-anti-affinity
	// Deprecated: use `PodTemplate` instead.
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// GRPCPort is the port used for the grpc service. This controls the
	// `--listen-addr` flag to `cockroach start`. If not set, default to 26258.
	// +kubebuilder:validation:Optional
	GRPCPort *int32 `json:"grpcPort,omitempty"`

	// HTTPPort is the port used for the http service. This controls the
	// `--http-addr` flag to `cockroach start`. If not set, default to 8080.
	// +kubebuilder:validation:Optional
	HTTPPort *int32 `json:"httpPort,omitempty"`

	// SQLPort is the port used for the sql service. This controls the
	// `--sql-addr` flag to `cockroach start`. If not set, default to 26257.
	// +kubebuilder:validation:Optional
	SQLPort *int32 `json:"sqlPort,omitempty"`

	// DropChownContainer drops the chown initContainer from the crdb pod. We
	// needed this initContainer during the rollout of the securityContext
	// feature previously, but now that this rollout is complete, we can drop the
	// container.
	// TODO(jmcarp): Drop this flag after it's fully rolled out.
	// +kubebuilder:validation:Optional
	DropChownContainer bool `json:"dropChownContainer,omitempty"`

	// PodTemplate is an optional pod specification that overrides the default pod specification configured by the
	// operator. If specified, PodTemplate is merged with the default pod specification, with settings in
	// PodTemplate taking precedence. This can be used to add or update containers, volumes, and
	// other settings of the cockroachdb pod. List fields are generally merged by name;
	// for details of merging behavior, see the PodTemplateBuilder type.
	// +kubebuilder:validation:Optional
	PodTemplate *PodTemplateSpec `json:"podTemplate,omitempty"`

	// ReadinessProbe specifies the readiness probe configuration for the CRDB container.
	// If not specified, uses default values (InitialDelaySeconds: 10, PeriodSeconds: 10, etc.)
	// +kubebuilder:validation:Optional
	// Deprecated: use `PodTemplate` instead.
	ReadinessProbe *corev1.Probe `json:"readinessProbe,omitempty"`

	// TLSEnabled indicates whether the node is running in secure mode. This field
	// is populated from CrdbCluster.Spec.TLSEnabled and controls whether cert volumes,
	// CERTS_DIR env vars, and the cert-reloader sidecar are included in the pod.
	// +kubebuilder:validation:Optional
	TLSEnabled bool `json:"tlsEnabled,omitempty"`

	// PersistentVolumeClaimRetentionPolicy controls the retention of PVCs when the CrdbNode is deleted.
	// If not specified, defaults to "Delete" for backward compatibility.
	// +kubebuilder:validation:Optional
	PersistentVolumeClaimRetentionPolicy *CrdbNodePersistentVolumeClaimRetentionPolicy `json:"persistentVolumeClaimRetentionPolicy,omitempty"`

	// VirtualCluster specifies the configuration to set up the virtual cluster.
	// You can read more about virtual clusters here: https://www.cockroachlabs.com/docs/stable/set-up-physical-cluster-replication
	// +kubebuilder:validation:Optional
	VirtualCluster *CrdbVirtualClusterSpec `json:"virtualCluster,omitempty"`
}

// PodTemplateSpec is a structure allowing the user to set a template for Pod
// generation.
type PodTemplateSpec struct {
	Metadata PodMeta        `json:"metadata,omitempty"`
	Spec     corev1.PodSpec `json:"spec,omitempty"`
}

type PodMeta struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// CrdbNodePersistentVolumeClaimRetentionPolicy describes the policy for PVCs created from CrdbNode.
type CrdbNodePersistentVolumeClaimRetentionPolicy struct {
	// WhenDeleted specifies what happens to PVCs when the CrdbNode is deleted.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=Delete
	// +kubebuilder:validation:Enum=Retain;Delete
	WhenDeleted appsv1.PersistentVolumeClaimRetentionPolicyType `json:"whenDeleted,omitempty"`
}

// Flags enable upserting and omitting default flags
// that are passed to the cockroach start command.
type Flags struct {
	// Upsert defines a set of flags that are given higher precedence
	// in the start command.
	// +kubebuilder:validation:Optional
	Upsert []string `json:"upsert,omitempty"`
	// Omit defines a set of flags which will be omitted
	// from the start command.
	// +kubebuilder:validation:Optional
	Omit []string `json:"omit,omitempty"`
}

// ConditionalResourceRequirements describes compute resource requirements for
// the crdb node that are determined based on the k8s node's capacity.
//
// The largest valid quantity is used for each resource. If there are no valid
// capacity requirements for a node's capacity, the traditional resource
// requirements are used.
type ConditionalResourceRequirements struct {
	// Limits contains conditional resource limits.
	Limits map[corev1.ResourceName][]ConditionalQuantity `json:"limits,omitempty"`
	// Requests contains conditional resource requests.
	Requests map[corev1.ResourceName][]ConditionalQuantity `json:"requests,omitempty"`
}

// ConditionalQuantity is a (capacity, quantity) pair. A ConditionalQuantity is
// valid if RequiredCapacity is less than or equal to the node's actual
// capacity.
type ConditionalQuantity struct {
	// RequiredCapacity is the amount of capacity the node must have for the
	// conditional quantity to be valid.
	RequiredCapacity resource.Quantity `json:"requiredCapacity"`
	// Quantity is the amount of resources that should be configured if the
	// conditional quantity is valid.
	Quantity resource.Quantity `json:"quantity"`
}

// CrdbNodeSideCars will be run in the same pod as the crdb process.
// This can be useful for running containers for fluentbit logging.
type CrdbNodeSideCars struct {
	// InitContainers will be run as init containers for the crdb pod.
	// This is mostly used for setting up sidecars, like FluentBit.
	// +kubebuilder:validation:Optional
	InitContainers []corev1.Container `json:"initContainers,omitempty"`

	// Containers will be run in the same pod as the the crdb container.
	// This is mostly used for running sidecars, like FluentBit.
	// +kubebuilder:validation:Optional
	Containers []corev1.Container `json:"containers,omitempty"`

	// Volumes will be requested in addition to the crdb volumes.
	// This is mostly used by the additional containers.
	// +kubebuilder:validation:Optional
	Volumes []corev1.Volume `json:"volumes,omitempty"`
}

// LocalityMapping represents a mapping between a k8s node label and a crdb
// locality label.
type LocalityMapping struct {
	NodeLabel     string `json:"nodeLabel,omitempty"`
	LocalityLabel string `json:"localityLabel,omitempty"`
	Prefix        string `json:"prefix,omitempty"`
}

// DataStore is the specification for the disk configuration of a CrdbNode.
// Exactly one of VolumeSource, VolumeClaimTemplate, or Basalt must be specified.
type DataStore struct {
	// VolumeSource specifies a pre-existing VolumeSource to be mounted to this
	// CrdbNode.
	// +kubebuilder:validation:Optional
	VolumeSource *corev1.VolumeSource `json:"volumeSource,omitempty"`
	// VolumeClaimTemplate specifies a that a new PersistentVolumeClaim should
	// be created for this CrdbNode.
	// +kubebuilder:validation:Optional
	VolumeClaimTemplate *corev1.PersistentVolumeClaim `json:"volumeClaimTemplate,omitempty"`
	// Basalt configures this CrdbNode to store data in a Basalt cluster instead
	// of a local volume. When set, VolumeSource and VolumeClaimTemplate must be
	// empty. The operator generates the basalt start flags and derives the
	// namespace. The control plane owns the tenant itself, including creating
	// it before the cluster is applied and deleting it afterwards.
	// +kubebuilder:validation:Optional
	Basalt *BasaltStoreSpec `json:"basalt,omitempty"`
}

// BasaltStoreSpec configures Basalt-backed storage for a CrdbNode. Basalt
// separates storage from compute, so a basalt-backed node holds no persistent
// local disk; all data is read and written over the network to the basalt
// controllers.
type BasaltStoreSpec struct {
	// ControllerAddrs are the basalt controller addresses (host:port) the node
	// connects to for storage. The control plane provides these.
	// +kubebuilder:validation:MinItems=1
	ControllerAddrs []string `json:"controllerAddrs"`
	// TenantID is the globally-unique, stable identity of this cluster's basalt
	// tenant. It must be unique across every cluster sharing the same basalt
	// infrastructure, since the tenant and its derived directory namespace are
	// the unit of data isolation. The control plane sets this to the CC cluster
	// id. It is never derived from the display name, which is only unique within
	// a namespace and would let two clusters collide. The operator derives the
	// basalt directory namespace deterministically from this value, so a unique
	// TenantID guarantees a unique namespace with no separate field to
	// misconfigure.
	// +kubebuilder:validation:Required
	TenantID string `json:"tenantID"`
	// DisableRSEngine disables the per-range RSEngine. Defaults to false
	// (RSEngine enabled). Set to true only if the basalt version requires it.
	// +kubebuilder:validation:Optional
	DisableRSEngine *bool `json:"disableRSEngine,omitempty"`
	// Insecure disables mTLS on the connection to the basalt controllers and
	// blob servers. A spec with neither this nor TLS runs insecure. Setting TLS
	// is what turns mTLS on, so this field only asserts the intent. True
	// alongside a TLS block, or false without one, is rejected as
	// self-contradictory.
	//
	// Only run insecure when the basalt controllers and blob servers are
	// themselves running insecure. They take either --insecure or --certs-dir,
	// so a secure basalt cluster refuses a plaintext client.
	// +kubebuilder:validation:Optional
	Insecure *bool `json:"insecure,omitempty"`
	// TLS references the credentials the node presents to basalt, and setting it
	// is what turns mTLS on. The control plane provisions and rotates both
	// objects.
	// +kubebuilder:validation:Optional
	TLS *BasaltTLSSpec `json:"tls,omitempty"`
	// CLIImage overrides the basalt-cli image used by the store cleanup Jobs.
	// When empty a built-in default is used.
	// +kubebuilder:validation:Optional
	CLIImage string `json:"cliImage,omitempty"`
	// StoreCount is the number of basalt stores per node. Each store maps to a
	// unique directory under the basalt namespace with its own Pebble instance.
	// More stores improve write throughput. Defaults to 1. Immutable after
	// cluster creation.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	StoreCount *int32 `json:"storeCount,omitempty"`
	// ScratchVolumeClaimTemplate, when set, backs the node's local scratch
	// directory (/cockroach/cockroach-data) with a dedicated PersistentVolumeClaim
	// instead of an ephemeral EmptyDir, so temp/spill and dumps land on a bounded
	// disk off the Kubernetes node's boot disk. When nil, an EmptyDir is used.
	// +kubebuilder:validation:Optional
	ScratchVolumeClaimTemplate *corev1.PersistentVolumeClaim `json:"scratchVolumeClaimTemplate,omitempty"`
}

// BasaltTLSSpec references the credentials a node uses for mTLS to the basalt
// controllers and blob servers. Both objects must live in the cluster's
// namespace. The operator only projects them into the pod. It never creates,
// signs, or rotates them.
type BasaltTLSSpec struct {
	// ClientSecretName names a kubernetes.io/tls Secret holding the tenant leaf
	// certificate under tls.crt and its key under tls.key. Basalt authenticates
	// the tenant from this certificate, which has to carry both OU=BasaltTenant
	// and a tenant URI SAN.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ClientSecretName string `json:"clientSecretName"`
	// CAConfigMapName names a ConfigMap holding the basalt cluster CA under
	// ca.crt. It is the trust anchor for the basalt controller and blob server
	// certificates, and a different PKI from the CockroachDB CA.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	CAConfigMapName string `json:"caConfigMapName"`
}

// IsBasalt reports whether this DataStore is backed by basalt.
func (d DataStore) IsBasalt() bool { return d.Basalt != nil }

// IsSecureBasalt reports whether this DataStore is backed by basalt and has the
// credentials to reach it over mTLS.
func (d DataStore) IsSecureBasalt() bool { return d.Basalt != nil && d.Basalt.UsesTLS() }

// GetStoreCount returns the number of basalt stores per node, defaulting to 1.
func (b *BasaltStoreSpec) GetStoreCount() int32 {
	if b.StoreCount == nil {
		return 1
	}
	return *b.StoreCount
}

// UsesTLS reports whether the spec carries credentials to project into the pod,
// which is the same question as whether the connection is secure. The webhook
// rejects an Insecure value that disagrees with this, so pod construction can
// trust it and never has to guard a nil TLS.
func (b *BasaltStoreSpec) UsesTLS() bool { return b.TLS != nil }

// CrdbLogsStoreSpec specifies the dedicated disk configuration for CockroachDB logs.
// Exactly one of VolumeSource or VolumeClaimTemplate must be specified.
type CrdbLogsStoreSpec struct {
	// VolumeSource specifies a pre-existing VolumeSource to be mounted for logs storage.
	// +kubebuilder:validation:Optional
	VolumeSource *corev1.VolumeSource `json:"volumeSource,omitempty"`
	// VolumeClaimTemplate specifies that a new PersistentVolumeClaim should
	// be created for logs storage.
	// +kubebuilder:validation:Optional
	VolumeClaimTemplate *corev1.PersistentVolumeClaim `json:"volumeClaimTemplate,omitempty"`
	// MountPath specifies the mount path for the dedicated logs volume.
	// This should match the file-defaults.dir value in the CockroachDB logging configuration
	// so that the dedicated volume is mounted where CockroachDB writes logs.
	// If not specified, defaults to /cockroach/cockroach-data/logs.
	// +kubebuilder:validation:Optional
	MountPath string `json:"mountPath,omitempty"`
}

// Certificates controls the TLS configuration of the various services run by
// CockroachDB. When TLS is enabled for the cluster, exactly one of SelfSigned
// or ExternalCertificates must be specified. When TLS is disabled, neither
// should be set. See https://www.cockroachlabs.com/docs/stable/authentication.html
// for more details.
type Certificates struct {
	// SelfSigned enables automatic generation of self-signed TLS certificates
	// for this CockroachDB cluster. When true, the operator generates and manages
	// the CA, node, and root client certificates automatically.
	// Mutually exclusive with ExternalCertificates.
	// +kubebuilder:validation:Optional
	SelfSigned bool `json:"selfSigned,omitempty"`

	// ExternalCertificates allows end users to assume full control over the
	// certificates used by CockroachDB.
	ExternalCertificates *ExternalCertificates `json:"externalCertificates,omitempty"`
}

// ExternalCertificates is a certificate configuration that mounts the
// pre-existing and non-operator controlled Kubernetes Secrets into CrdbNode
// Pods for use as TLS on the various services run by CockroachDB.
//
// See https://www.cockroachlabs.com/docs/stable/authentication.html
type ExternalCertificates struct {
	// CAConfigMapName is the name of a Kubernetes ConfigMap containing a ca.crt
	// entry that was used to sign other external certificates. This is used to
	// validate the node and client certificates.
	// https://www.cockroachlabs.com/docs/stable/authentication.html#client-authentication
	// +kubebuilder:validation:Optional
	CAConfigMapName string `json:"caConfigMapName,omitempty"`

	// NodeCAConfigMapName is the name of a Kubernetes ConfigMap containing a
	// ca.crt entry that will be used as the CA for node authentication. Only set
	// if using split CA certificates, which is not recommended:
	// https://www.cockroachlabs.com/docs/stable/authentication.html#using-split-ca-certificates.
	// Exactly one of CAConfigMapName and NodeCAConfigMapName must be set.
	// +kubebuilder:validation:Optional
	NodeCAConfigMapName string `json:"nodeCaConfigMapName,omitempty"`

	// ClientCAConfigMapName is the name of a Kubernetes ConfigMap containing a
	// ca.crt entry that will be used as the CA for client authentication. This
	// is used to validate the client certificates. Only set if using split
	// CA certificates, which is not recommended:
	// https://www.cockroachlabs.com/docs/stable/authentication.html#using-split-ca-certificates.
	// https://www.cockroachlabs.com/docs/stable/authentication.html#client-authentication
	// +kubebuilder:validation:Optional
	ClientCAConfigMapName string `json:"clientCaConfigMapName,omitempty"`

	// HTTPSecretName is the name of a Kubernetes TLS Secret that will be used
	// for the HTTP service.
	// +kubebuilder:validation:Optional
	HTTPSecretName string `json:"httpSecretName,omitempty"`

	// NodeClientSecretName is the name of a Kubernetes TLS secret holding client
	// certificates used when establishing connections to other nodes in the cluster
	// (e.g. joining an existing cluster).
	//
	// The certificate must be signed with the CA identified by CAConfigMapName,
	// or ClientCASecretName if using split CA certificates.
	// +kubebuilder:validation:Optional
	NodeClientSecretName string `json:"nodeClientSecretName,omitempty"`

	// NodeSecretName is the name of a Kubernetes TLS Secret that will be used
	// when receiving incoming connections from other nodes for RPC and SQL calls.
	//
	// The certificate must be signed with the CA identified by CAConfigMapName,
	// or NodeCASecretName if using split CA certificates.
	// +kubebuilder:validation:Required
	NodeSecretName string `json:"nodeSecretName,omitempty"`

	// RootSQLClientSecretName is the name of a Kubernetes TLS secret holding SQL
	// client certificates for the root SQL user. It allows the operator to perform
	// various administrative actions (e.g. set cluster settings).
	//
	// The certificate must be signed with the CA identified by CAConfigMapName,
	// or ClientCASecretName if using split CA certificates.
	// +kubebuilder:validation:Required
	RootSQLClientSecretName string `json:"rootSqlClientSecretName,omitempty"`
}

func (extCerts *ExternalCertificates) Validate() error {
	hasSingleCACert := extCerts.CAConfigMapName != ""
	hasSplitCACert := extCerts.NodeCAConfigMapName != "" && extCerts.ClientCAConfigMapName != ""
	if hasSingleCACert && hasSplitCACert {
		return errors.New("must define caConfigMapName or both nodeCaConfigMapName and clientCaConfigMapName; got both")
	}
	if !hasSingleCACert && !hasSplitCACert {
		return errors.New("must define caConfigMapName or both nodeCaConfigMapName and clientCaConfigMapName; got neither")
	}
	if (extCerts.NodeCAConfigMapName != "" && extCerts.ClientCAConfigMapName == "") ||
		(extCerts.NodeCAConfigMapName == "" && extCerts.ClientCAConfigMapName != "") {
		return errors.New("must define both nodeCaConfigMapName and clientCaConfigMapName or neither")
	}
	return nil
}

// CrdbNodeStatus defines the observed state of an individual
// CockroachDB node process.
type CrdbNodeStatus struct {
	// ObservedGeneration is the value of the ObjectMeta.Generation last
	// reconciled by the controller.
	ObservedGeneration int64 `json:"observedGeneration"`

	// Conditions are the set of current status indicators for the node.
	Conditions []NodeCondition `json:"conditions,omitempty"`

	// NodeID is the numerical ID of this node according to the CRDB cluster
	NodeID *int64 `json:"nodeID,omitempty"`

	// TODO(prod-orch): basalt#650 (BAS-540) may let the operator reserve
	// node/store ids up front, removing the need to query kv_store_status.

	// StoreIDs are the CRDB store IDs owned by this node, captured from
	// crdb_internal.kv_store_status while the node is healthy. They name the
	// range-shared Basalt directories (s<store-id>) that live outside the node
	// directory, so the operator needs them to reclaim that space when the node
	// is decommissioned. Store IDs are retained across Pod readiness transitions
	// and merged with later snapshots. Empty for non-Basalt nodes.
	StoreIDs []int64 `json:"storeIDs,omitempty"`

	// Decommission indicates the current decommissioning state (if any).
	Decommission NodeDecommissionState `json:"decommission,omitempty"`

	// TopologyValues contains the K8s Node label values for the Pod's effective
	// topology spread constraints, in constraint order. We currently use only the
	// first value for decommissioning. When explicit node placement is enabled,
	// placement also treats it as the K8s zone.
	TopologyValues []string `json:"topologyValues,omitempty"`
}

// CopyAndFilterInternalNodeAnnotations seperates internal annotations.
// The returned values are copies of the input map.
func CopyAndFilterInternalNodeAnnotations(
	annotations map[string]string,
) (nonInternalAnnotations map[string]string, internalAnnotations map[string]string) {
	nonInternalAnnotations = map[string]string{}
	internalAnnotations = map[string]string{}
	for k, v := range annotations {
		if isInternalNodeAnnotation(k) {
			internalAnnotations[k] = v
			continue
		}
		nonInternalAnnotations[k] = v
	}
	return nonInternalAnnotations, internalAnnotations
}

func isInternalNodeAnnotation(key string) bool {
	return key == CrdbNodeDecommissionAnnotation ||
		key == CrdbNodeStartDecommissionAnnotation ||
		key == CrdbNodeStartPendingDecommissionAnnotation
}

// GetGRPCPort returns the port used to serve grpc connections, defaulting to
// 26258 if not specified. Write default logic explicitly via an accessor
// rather than using kubedefault annotations so that test code that doesn't
// receive defaults via kube-apiserver sees the correct port.
func (s *CrdbNodeSpec) GetGRPCPort() int32 {
	if s.GRPCPort != nil {
		return *s.GRPCPort
	}
	return DefaultCockroachGRPCPort
}

// GetHTTPPort returns the port used to serve http connections, defaulting to
// 8080 if not specified.
func (s *CrdbNodeSpec) GetHTTPPort() int32 {
	if s.HTTPPort != nil {
		return *s.HTTPPort
	}
	return DefaultCockroachHTTPPort
}

// GetSQLPort returns the port used to serve grpc connections, defaulting to
// 26257 if not specified.
func (s *CrdbNodeSpec) GetSQLPort() int32 {
	if s.SQLPort != nil {
		return *s.SQLPort
	}
	return DefaultCockroachSQLPort
}

// GetTopologyValue returns the value for the first topology constraint, or an
// empty string if that value is missing.
func (s *CrdbNodeStatus) GetTopologyValue() string {
	if len(s.TopologyValues) > 0 {
		return s.TopologyValues[0]
	}
	return ""
}

// GetCondition returns the condition status, last transition time
// of the given type, and "true" if the given condition type exists.
// Otherwise, it returns empty values for the former two and "false".
func (s *CrdbNodeStatus) GetCondition(
	typ NodeConditionType,
) (st metav1.ConditionStatus, ts time.Time, exists bool) {
	for i := range s.Conditions {
		if s.Conditions[i].Type == typ {
			return s.Conditions[i].Status, s.Conditions[i].LastTransitionTime.Time, true
		}
	}
	exists = false
	return
}

// HasCondition returns true if a condition of the given type exists in the
// condition list and has the given status value.
func (s *CrdbNodeStatus) HasCondition(typ NodeConditionType, st metav1.ConditionStatus) bool {
	cond, _, ok := s.GetCondition(typ)
	return ok && cond == st
}

// SetConditionWithTimestamp adds a new condition to the cluster condition list
// if no matching condition exists yet.
// Otherwise, it overwrites the existing condition,
// even if `ts` is earlier than existing timestamp.
// This is different than how it is implemented for CrdbCluster, because
// we the `ts` here sometimes are not under our control.
// E.g., this will be called for setting `PodReady` with a `ts` set by
// the pod controller, where `ts` can be from a timestamp from the past.
// This behavior should generally be harmless; the controller should not
// rely on these timestamps to be non-decreasing anyways.
func (s *CrdbNodeStatus) SetConditionWithTimestamp(
	typ NodeConditionType, status metav1.ConditionStatus, ts time.Time,
) {
	cond := NodeCondition{
		Type:               typ,
		Status:             status,
		LastTransitionTime: metav1.NewTime(ts),
	}
	for i := range s.Conditions {
		if s.Conditions[i].Type == typ {
			s.Conditions[i] = cond
			return
		}
	}
	s.Conditions = append(s.Conditions, cond)
}

// SetCondition adds a new condition to the cluster condition list if no matching
// condition exists yet, or else overwrites the existing condition.
func (s *CrdbNodeStatus) SetCondition(typ NodeConditionType, status metav1.ConditionStatus) {
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

type NodeCondition struct {
	// Type is the kind of this condition.
	// +kubebuilder:validation:Required
	Type NodeConditionType `json:"type"`
	// Status is the current state of the condition: True, False or Unknown.
	// +kubebuilder:validation:Required
	Status metav1.ConditionStatus `json:"status"`
	// LastTransitionTime is the time at which the condition was last updated.
	// +kubebuilder:validation:Required
	LastTransitionTime metav1.Time `json:"lastTransitionTime"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion

// CrdbNode is the Schema for the crdbnode API.
// NOTE: Don't add new fields to this struct. Instead, add fields describing
// the desired state of the CrdbNode to CrdbNodeSpec, and fields describing the
// observed state of the CrdbNode to CrdbNodeStatus.
type CrdbNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CrdbNodeSpec   `json:"spec,omitempty"`
	Status CrdbNodeStatus `json:"status,omitempty"`
}

// MarkForDecommission annotates a node such that it will be given priority
// when scaling down the cluster.
func (node *CrdbNode) MarkForDecommission() {
	node.setAnnotation(CrdbNodeDecommissionAnnotation, "true")
}

// MarkForPendingDecommission annotates a non-running node such that it will be given
// priority when scaling down the cluster.
func (node *CrdbNode) MarkForPendingDecommission() {
	node.setAnnotation(CrdbNodeStartPendingDecommissionAnnotation, "true")
}

// Decommission sets the appropriate annotation used to signal that the
// decommissioning process should be started.
func (node *CrdbNode) Decommission(nodeID int64) {
	node.setAnnotation(CrdbNodeStartDecommissionAnnotation, fmt.Sprintf("%d", nodeID))
}

// IsMarkedForDecommission returns true when this node has been annotated
// with the crdb.cockroachlabs.com/decommission annotation. Marked nodes are
// given priority when scaling down the cluster.
func (node *CrdbNode) IsMarkedForDecommission() bool {
	return node.getAnnotation(CrdbNodeDecommissionAnnotation)
}

// IsMarkedForPendingDecommission returns true when this node has been annotated
// with the internal.crdb.cockroachlabs.com/pending-decommission annotation. Marked nodes
// are given priority when scaling down the cluster.
func (node *CrdbNode) IsMarkedForPendingDecommission() bool {
	return node.getAnnotation(CrdbNodeStartPendingDecommissionAnnotation)
}

// IsDecommissioning returns true when the decommission annotation is set on
// the node indicating the process has started.
func (node *CrdbNode) IsDecommissioning() bool {
	return node.getAnnotation(CrdbNodeStartDecommissionAnnotation)
}

// IsExcluded returns true when the CRDB node is excluded from the cluster.
func (node *CrdbNode) IsExcluded() bool {
	return node.getAnnotation(CrdbNodeExcludeAnnotation)
}

func (node *CrdbNode) getAnnotation(key string) bool {
	if node.Annotations == nil {
		return false
	}
	_, ok := node.Annotations[key]
	return ok
}

func (node *CrdbNode) setAnnotation(key string, value string) {
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}
	node.Annotations[key] = value
}

// DecommissionNodeID returns the node id of the CRDB node. This value is only
// set if node is decommissioning.
func (node *CrdbNode) DecommissionNodeID() (uint, error) {
	v, ok := node.Annotations[CrdbNodeStartDecommissionAnnotation]
	if !ok {
		return 0, errors.Newf("annotation %s not set", CrdbNodeStartDecommissionAnnotation)
	}

	id, err := strconv.Atoi(v)
	if err != nil {
		return 0, errors.Wrapf(err, "parsing annotation %s value", CrdbNodeStartDecommissionAnnotation)
	}

	return uint(id), nil
}

func (node *CrdbNode) CrdbVersion() (version.Version, error) {
	parts := strings.Split(node.Spec.Image, ":")
	if len(parts) != 2 {
		return version.Version{}, errors.Newf("unexpected image format '%s'", node.Spec.Image)
	}

	v, err := version.Parse(parts[len(parts)-1])
	if err != nil {
		return version.Version{}, errors.Wrapf(err, "could not parse crdbnode image %s", node.Spec.Image)
	}

	return v, nil
}

// PodSecurityContext returns the security context used to run pods for this node.
func (node *CrdbNode) PodSecurityContext() *corev1.PodSecurityContext {
	psc := securitycontext.PodRunAsNonRoot()
	// runAsNonRoot is set to false because some containers may need to run as
	// root, for example to control kernel parameters. No matter what this value
	// is, the main CockroachDB container will not run as UserID, not as root.
	psc.RunAsNonRoot = pointer.Bool(false)
	return psc
}

// ContainerSecurityContext returns the security context for all containers in
// the pod. This ensures we're dropped all unnecessary capabilities, removed
// privileged access, and ensured the root filesystem is read-only.
func (node *CrdbNode) ContainerSecurityContext() *corev1.SecurityContext {
	return securitycontext.ContainerRunAsNonRoot()
}

// IsReady returns nil once the underlying Pod running CockroachDB is ready.
func (node *CrdbNode) IsReady() error {
	if node.Generation > node.Status.ObservedGeneration {
		return errors.Newf(
			"observed %T generation %d is lower than expected generation %d",
			node,
			node.Status.ObservedGeneration,
			node.Generation,
		)
	}

	if node.Status.HasCondition(PodNeedsUpdate, metav1.ConditionTrue) {
		return errors.Newf(
			"%T's pod has not been updated yet",
			node,
		)
	}
	if !node.Status.HasCondition(PodRunning, metav1.ConditionTrue) {
		return errors.Newf(
			"%T's pod is not running yet",
			node,
		)
	}
	if !node.Status.HasCondition(PodReady, metav1.ConditionTrue) {
		return errors.Newf(
			"%T's pod is not ready yet",
			node,
		)
	}
	return nil
}

// +kubebuilder:object:root=true

// CrdbNodeList contains a list of CrdbCluster.
type CrdbNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CrdbNode `json:"items"`
}

// WithCondition returns a sub-slice of .Items where the provided condition is
// equal to the provided status.
func (l *CrdbNodeList) WithCondition(
	condition NodeConditionType, status metav1.ConditionStatus,
) []*CrdbNode {
	x := 0
	filtered := make([]*CrdbNode, len(l.Items))
	for i := range l.Items {
		node := &l.Items[i]

		if node.Status.HasCondition(condition, status) {
			filtered[x] = node
			x++
		}
	}
	return filtered[:x]
}

func init() {
	SchemeBuilder.Register(&CrdbNode{}, &CrdbNodeList{})
}

package configtypes

import (
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/kube/kubeconfig"
	"google.golang.org/protobuf/proto"
)

// Config defines options that represent the CC cluster as a whole, rather
// than any one particular region/K8s cluster.
type Config struct {
	// Cluster has details about the CC cluster this config is for.
	Cluster Cluster
	// CustomNetwork determines if computing resources are created in the default
	// subnetwork or a custom subnetwork.
	CustomNetwork bool
	// BackupsRegion is the region in which to store database backups.
	BackupsRegion string
	// Regions contains the configurations for each region in the footprint.
	Regions []RegionConfig

	// DesiredState is a vendor-specific protobuf describing the desired state
	// for the Pulumi stack. (e.g pulumipb.GCPProject or pulumipb.AWSCluster).
	DesiredState proto.Message `json:"-" yaml:"-"`
}

// Cluster defines global details about the CC cluster.
type Cluster struct {
	ID                     string
	Name                   string
	FriendlyID             string
	ShortID                string
	CloudProviderAccountID string
}

// RegionConfig defines a region for a cluster. This is a one to one mapping to K8s
// cluster belonging to a Cluster object.
type RegionConfig struct {
	kubeconfig.RestConfigProvider

	// Cluster has details about the CC cluster this region belongs to.
	Cluster Cluster
	// DNSRecords represents the DNS records that should be available for this
	// region.
	DNSRecords map[DNSKind]string
	// Region is the name of the cloud provider region. The Namespace,Name pair
	// defines a subspace within the Namespace. Resources belonging to the
	// RegionalFootprint will live in this subspace.
	Region string
	// NodeCount specifies the number of VMs in this region.
	NodeCount int
	// MachineType is the desired virtual machine type for the VMs in this
	// region
	MachineType string
	// KubernetesMasterVersion specifies the desired Kubernetes Master version
	// for the Kubernetes cluster in this region.
	KubernetesMasterVersion string
	// KubernetesCNIPlugin specifies the CNI plugin to run in the Kubernetes
	// cluster. See pulumipb for supported values.
	KubernetesCNIPlugin string
	// KubernetesClusterID is an integer that uniquely identifies the
	// Kubernetes cluster amongst the other Kubernetes clusters backing
	// the same CRDB cluster.
	KubernetesClusterID int
	// MultiregionNetworkPolicies if true results in the configuration of infra
	// such that NetworkPolicy resources can be enforced across kubernetes
	// clusters. Though this flag is configured at the Region level, we do not
	// expect this value to vary across regions. Field only valid if
	// KubernetesCNIPlugin=CILIUM.
	MultiregionNetworkPolicies bool

	// DesiredState is a vendor-specific protobuf describing the desired state
	// for the Pulumi stack. (e.g pulumipb.GCPRegion or pulumipb.AWSRegion).
	DesiredState proto.Message `json:"-" yaml:"-"`

	// Output is a set of read-only fields that are set by the cloud provider.
	Output RegionOutput
}

// RegionOutput is a set of regional read-only fields that are set by the cloud
// provider.
type RegionOutput struct {
	KubernetesConfig
	// AvailabilityZones are the availability zones the Kubernetes cluster
	// spans.
	AvailabilityZones []string

	// AzureK8sManagedIdentityClientID is the ID of the AKS cluster's default
	// managed identity. All workloads in the AKS cluster can authenticate as
	// this MI.
	//
	// TODO(CC-25472) Once workload identity is introduced for AKS clusters, we
	// can remove this output in favor of each workload's individual identity.
	AzureK8sManagedIdentityClientID string
}

// KubernetesConfig contains values useful for connecting to Kubernetes.
// These fields are populated from cloud-provider generated values.
type KubernetesConfig struct {
	// CertificateAuthority is the cluster's root Certificate Authority (CA),
	// and is used by the Kubernetes API client to validate the Kubernetes
	// API server's certificate.
	CertificateAuthority []byte
	// ServerAddress is the Kubernetes API server address to which
	// the Kubernetes clients will connect.
	ServerAddress string
}

// DNSKind is an enumeration of the kinds of DNS records that can be created.
type DNSKind string

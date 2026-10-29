package cloudinfra

import (
	"context"

	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/configtypes"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/infrastructure"
)

// Cloud defines an interface for working with cluster-wide cloud resources.
type Cloud interface {
	// Get retrieves all of the global and regional cloud resources for this
	// CC cluster.
	Get(context.Context) (*CloudResources, error)

	// Create creates all of the global and regional cloud resources for this CC
	// cluster. It will stand up K8s clusters in each region and ensure they are
	// all peered together.
	Create(context.Context) (*CloudResources, error)

	// Delete deletes the CC cluster by removing all of the global/regional cloud
	// resources.
	Delete(context.Context) error

	// KubernetesForRegion returns a Kubernetes object configured with the supplied
	// region. An error is returned when the region is not found.
	KubernetesForRegion(string) (Kubernetes, error)

	// Update updates cloud resources for the CC cluster and all regions.
	Update(context.Context) error

	// UpdateBase only updates the base stack
	UpdateBase(ctx context.Context) error

	// UpdateDNS updates the global DNS records for a CC cluster, excluding any
	// regions supplied after the context.
	UpdateDNS(context.Context, ...string) error

	// UpdateLogging updates logging resources for each region in the cluster.
	UpdateLogging(context.Context) error

	// UpdateInSitu updates In-Situ resources in the base and regional stacks of the cluster.
	UpdateInSitu(context.Context) error

	// UpdateInternalPeerings updates internal peering resources for the CC
	// cluster. Note that peering resources could either be global, or regional,
	// depending on the cloud provider.
	UpdateInternalPeerings(context.Context) error

	// WithConfig returns a new Cloud instance with the specified Config.
	WithConfig(*configtypes.Config) Cloud
}

// Kubernetes defines an interface for working with region-specific (single K8s
// cluster) cloud resources.
type Kubernetes interface {
	// Create creates all of the cloud resources for this cluster. It will also
	// update global DNS and ensure this cluster is peered with all of the other
	// regions in the CC cluster.
	Create(context.Context) (*CloudResources, error)

	// Delete deletes all cloud resources for this cluster.
	Delete(context.Context) error

	// UpdateDNS updates the regional DNS records for this cluster.
	UpdateDNS(context.Context) error

	// UpdateMasterVersion updates the K8s master version.
	UpdateMasterVersion(context.Context) error

	// UpdateServiceQuota issues a request to the cloud provider to update a
	// specific service quota. This method just issues the request and returns
	// the request id before request resolution. See WaitForServiceQuotaUpdate
	// for options to poll for request resolution. This method is NOT
	// idempotent, the caller must take measures to ensure idempotency higher
	// up in the stack.
	UpdateServiceQuota(context.Context, *UpdateServiceQuotaRequest) (requestId string, err error)

	// WaitForServiceQuotaUpdate waits for a specific service quota request to
	// be resolved. If the request is approved, no error is returned. Any other
	// request resolution state results in error. Request resolution can take
	// many hours, even days.
	WaitForServiceQuotaUpdate(context.Context, *WaitForServiceQuotaUpdateRequest) error

	// // GetComponentReader returns the cloud specific implementation of the
	// // ComponentReader interface, which operates at the scope of a single
	// // K8s cluster.
	// GetComponentReader() ComponentReader

	// // InstallCilium installs the desired Cilium cluster config to the cluster.
	// // It applies the manifest file of version to the cluster. e.g. gke-cilium-v1.12.4-20241209.yaml.
	// InstallCilium(context.Context, string, *kube.Ctl, crypto.CertificateSigner) error
}

type UpdateServiceQuotaRequest struct {
	Service string
	Quota   string
	Value   float64
}

type WaitForServiceQuotaUpdateRequest struct {
	Service   string
	Quota     string
	RequestId string
}

type UpdateVMPoolGroupOutput struct {
	VMGroupIdentifier string
	VMPoolReplaced    bool
}

// VMPoolGroupUpdater defines an interface for updating VM pool groups.
type VMPoolGroupUpdater interface {
	// UpdateVMPoolGroup ensures the current node group state matches the
	// desired state. Node group state includes machine type, VM image, OAuth
	// Scopes, among other fields. UpdateVMPoolGroup returns the identifier of
	// either the existing group or a newly created group.
	UpdateVMPoolGroup(
		context.Context,
		infrastructure.VMPoolGroupOptions,
		map[infrastructure.AvailabilityZone]int,
	) (*UpdateVMPoolGroupOutput, error)
}

// CloudResources contains details about the deployment. This includes details
// about each K8s cluster that has been created.
type CloudResources struct {
	AWSInstanceRole              string
	AzureClusterIdentityAppObjID string
	KubeResources                map[string]KubernetesResources
}

// KubernetesResources contains details about the K8s deployment.
type KubernetesResources struct {
	APIServer                       string
	CA                              string
	Zones                           []string
	AWSVpcID                        string
	AzureK8sManagedIdentityClientID string
	AzureVnetID                     string
}

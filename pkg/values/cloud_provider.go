package values

import "github.com/cockroachlabs/cockroach-enterprise-operator/pkg/constants"

// CloudProvider identifies the public cloud provider that is hosting a K8s
// cluster.
type CloudProvider string

// Supported cloud providers. These must be sync'd with corresponding
// cloud.Provider consts in the operator.
const (
	// NB: the below Provider constants should match the constants defined in:
	// operators/cockroach-operator/cloud.Provider

	CloudProviderAWS   = CloudProvider(constants.AWS)
	CloudProviderAzure = CloudProvider(constants.Azure)
	CloudProviderGCP   = CloudProvider(constants.GCP)
	CloudProviderK3D   = CloudProvider(constants.K3D)
	CloudProviderFake  = CloudProvider(constants.Fake)
	CloudProviderNil   = CloudProvider("")

	// CloudProviderTest is only used for internal testing.
	CloudProviderTest = CloudProvider("test")
)

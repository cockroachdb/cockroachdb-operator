package cloud

import (
	"strings"

	"github.com/cockroachdb/errors"
)

// Provider identifies the public cloud provider that is hosting a K8s cluster.
type Provider string

const (
	// NB: the below Provider constants should match the constants defined in:
	// pkg/values.CloudProvider. The CloudProvider constants used to reference
	// these constants and its better if they don't drift.

	// Unknown indicates that the hosting cloud provider is not known.
	Unknown = Provider("")

	// AWS is the Amazon Web Services cloud.
	AWS = Provider("aws")

	// GCP is the Google cloud.
	GCP = Provider("gcp")

	// Azure is Microsoft's cloud.
	Azure = Provider("azure")

	// K3D is the Rancher Labs K3D distribution, hosted locally for testing.
	K3D = Provider("k3d")
)

// ParseProvider returns an error if the given name is not a known cloud
// provider name.
func ParseProvider(name string) (Provider, error) {
	switch strings.ToLower(name) {
	case string(AWS):
		return AWS, nil
	case string(GCP):
		return GCP, nil
	case string(K3D):
		return K3D, nil
	case string(Azure):
		return Azure, nil
	default:
		return Unknown, errors.Errorf("unknown CloudProvider: %s", name)
	}
}

func (p Provider) String() string {
	return string(p)
}

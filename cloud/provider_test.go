package cloud

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProvider(t *testing.T) {
	// This is silly test to ensure that we don't change the Provider constants
	// since the pkg/values.CloudProvider constants used to use these constants
	// and I'm not sure what the impact of them drifting would be.
	require.Equal(t, AWS, Provider("aws"))
	require.Equal(t, GCP, Provider("gcp"))
	require.Equal(t, Azure, Provider("azure"))
	require.Equal(t, K3D, Provider("k3d"))
}

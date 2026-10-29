package manifests

import (
	"encoding/json"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/stretchr/testify/require"
)

func TestTenantNetworkPolicy(t *testing.T) {
	testCases := []struct {
		Name    string
		Options TenantNetworkPolicyOptions
	}{
		{
			Name: "defaults",
		},
		{
			Name: "cilium",
			Options: TenantNetworkPolicyOptions{
				NetworkPlugin: cloud.Cilium,
			},
		},
	}

	for _, tc := range testCases {
		mani := TenantNetworkPolicy(tc.Options)

		actual, err := json.MarshalIndent(mani, "", "\t")
		require.NoError(t, err)

		testutils.AssertGoldenJSON(t, tc.Name, actual)
	}
}

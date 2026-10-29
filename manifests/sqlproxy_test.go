package manifests

import (
	"encoding/json"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/denylist"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSQLProxy(t *testing.T) {
	testCases := []struct {
		Name    string
		Options SQLProxyOptions
	}{
		{
			Name: "custom name",
			Options: SQLProxyOptions{
				Name: "mysqlproxy",
			},
		},
		{
			Name: "custom number of replicas",
			Options: SQLProxyOptions{
				Replicas: 5,
			},
		},
		{
			Name: "custom sqlproxy image",
			Options: SQLProxyOptions{
				Image: "cockroach/sqlproxy:v42",
			},
		},
		{
			Name: "custom tenantdir image",
			Options: SQLProxyOptions{
				TenantDirImage: "cockroach/tenantdir:v1",
			},
		},
		{
			Name: "custom namespace",
			Options: SQLProxyOptions{
				Namespace: "europe-west1",
			},
		},
		{
			Name: "with k8s API server IPs",
			Options: SQLProxyOptions{
				K8sAPIServerIPs: []string{"1.2.3.4", "5.6.7.8"},
			},
		},
		{
			Name: "with test mode",
			Options: SQLProxyOptions{
				InTestMode: true,
			},
		},
		{
			Name: "with disable connection rebalancing",
			Options: SQLProxyOptions{
				DisableConnectionRebalancing: true,
			},
		},
		{
			Name: "with cilium",
			Options: SQLProxyOptions{
				NetworkPlugin: cloud.Cilium,
			},
		},
		{
			Name: "with proxy protocol listen addr",
			Options: SQLProxyOptions{
				ProxyProtocolListenAddrEnabled: true,
			},
		},
		{
			Name: "with apply config maps",
			Options: SQLProxyOptions{
				ApplyConfigMaps: true,
			},
		},
	}
	for _, tc := range testCases {
		opts := tc.Options
		opts.Image = "us-docker.pkg.dev/cockroach-cloud-images/cockroachdb/cockroach:v22.1.5"
		mani := SQLProxy(opts)

		actual, err := json.MarshalIndent(mani, "", "\t")
		assert.NoError(t, err)

		testutils.AssertGoldenJSON(t, tc.Name, actual)
	}
}

func TestSqlProxyRBAC(t *testing.T) {
	testCases := []struct {
		name      string
		namespace string
		appName   string
	}{
		{
			name:      "namespace",
			namespace: "my-ns",
			appName:   "foobar",
		},
	}
	for _, tc := range testCases {
		mani := SqlProxyRBAC(tc.namespace, tc.appName)

		actual, err := json.MarshalIndent(mani, "", "\t")
		assert.NoError(t, err)

		testutils.AssertGoldenJSON(t, tc.name, actual)
	}
}

func TestDefaultSqlProxyDenylistContent(t *testing.T) {
	// Make sure that the default sqlproxy denylist is valid.
	file, err := denylist.Deserialize([]byte(defaultSqlProxyDenylistContent))
	require.NoError(t, err)
	require.Equal(t, int64(0), file.Seq)
	require.Empty(t, file.Denylist)
}

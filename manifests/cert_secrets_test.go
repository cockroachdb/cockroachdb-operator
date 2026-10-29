package manifests

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/crypto"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestCrdbClusterCertSecrets(t *testing.T) {
	type golden struct {
		Secret *corev1.Secret
		Config *crypto.SecretConfig
	}
	proxyCertConfig, err := SqlProxyCertConfig("3p3.some-region-1.some.root")
	require.NoError(t, err)
	moltAICertConfig, err := MoltAICertConfig("molt-ai.3p3.some.root")
	require.NoError(t, err)
	testCases := []struct {
		Name   string
		Config *crypto.SecretConfig
	}{
		{"operator", CockroachOperatorCertConfig()},
		{"node", nodeCertConfig("namespace", "cluster.other", "cluster.1x.other", []string{"extra.dns.name1", "extra.dns.name2"})},
		{"client", rootClientCertConfig()},
		{"sqlstarter", SqlStarterCertConfig()},
		{"sqlproxy", proxyCertConfig},
		{"molt-ai", moltAICertConfig},
		{"ui-certs", UICertConfig(crypto.TestBundle)},
		{"tenant", TenantCertConfig(1337, "namespace", "cluster.other")},
		{"tenantdir", TenantDirCertConfig()},
	}
	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			secret, err := tc.Config.ToK8SSecret(context.Background(), crypto.DeterministicFakeSignerForTesting(t))
			require.NoError(t, err)

			actual, err := json.MarshalIndent(golden{secret, tc.Config}, "", "\t")
			assert.NoError(t, err)

			testutils.AssertGoldenJSON(t, tc.Name, actual)
		})
	}
}

func TestMoltAICertConfigEmptyDNSName(t *testing.T) {
	_, err := MoltAICertConfig("")
	require.Error(t, err)
	require.Contains(t, err.Error(), "non-blank dns name")
}

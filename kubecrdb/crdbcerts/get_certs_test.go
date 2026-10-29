package crdbcerts_test

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/crypto"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/crdbcerts"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/stretchr/testify/require"
)

func TestGetCrdbCertBundles(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("default")

	signer, err := crypto.FakeSigner()
	require.NoError(t, err)
	externalBundle, err := signer.Sign(context.Background(),
		crypto.CertificateSigningRequest{CommonName: "from-lets-encrypt"},
	)
	require.NoError(t, err)
	expectedCA, err := signer.Certificate(ctx)
	require.NoError(t, err)

	require.NoError(t, crdbcerts.CreateCrdbClusterCerts(ctx, signer, ctl, crdbcerts.CrdbClusterCertOpts{
		IsHost:            true,
		IsCustomerAccount: false,
		ExternalCert:      externalBundle,
		ServerlessDNS:     "3p3.gcp-us-central-1.crdb.io",
	}))

	t.Run("node", func(t *testing.T) {
		ca, cert, err := crdbcerts.GetNodeCertBundle(ctl)
		require.NoError(t, err)
		requireCommonName(t, cert, "node")
		require.Equal(t, expectedCA, ca)
	})
	t.Run("root-client", func(t *testing.T) {
		ca, cert, err := crdbcerts.GetRootClientCertBundle(ctl)
		require.NoError(t, err)
		requireCommonName(t, cert, "root")
		require.Equal(t, expectedCA, ca)
	})
}

func TestGetCertBundles(t *testing.T) {
	namespace := "default"

	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace(namespace)

	signer, err := crypto.FakeSigner()
	require.NoError(t, err)
	expectedCA, err := signer.Certificate(ctx)
	require.NoError(t, err)

	// Install all the secrets into an in-memory store using the fake Ctl.
	require.NoError(t, crdbcerts.CreateCrdbClusterCerts(ctx, signer, ctl, crdbcerts.CrdbClusterCertOpts{
		IsHost:            true,
		IsCustomerAccount: false,
		ExternalCert:      crypto.TestBundle,
		ServerlessDNS:     "3p3.gcp-us-central-1.crdb.io",
	}))

	require.NoError(t, crdbcerts.CreateTenantCerts(ctx, signer, ctl, crdbv1beta1.TenantID(10), "cluster.remote"))

	secret, err := manifests.CockroachOperatorCertConfig().ToK8SSecret(ctx, signer)
	require.NoError(t, err)
	require.NoError(t, ctl.Apply(ctx, secret))

	t.Run("ui", func(t *testing.T) {
		cert, err := crdbcerts.GetUICertBundle(ctl)
		require.NoError(t, err)
		require.Contains(t, string(cert.PrivateKey), "BEGIN PRIVATE KEY")
	})
	t.Run("sqlproxy", func(t *testing.T) {
		cert, err := crdbcerts.GetSqlProxyCertBundle(ctl)
		require.NoError(t, err)
		requireCommonName(t, cert, "")
		requireDNSNames(t, cert, []string{
			"*.gcp-us-central-1.crdb.io",
			"*.crdb.io",
			"*.3p3.gcp-us-central-1.crdb.io",
			"*.internal-3p3.gcp-us-central-1.crdb.io",
			"*.3p3.crdb.io"})
	})
	t.Run("operator", func(t *testing.T) {
		ca, cert, err := crdbcerts.GetOperatorCertBundle(ctl)
		require.NoError(t, err)
		requireCommonName(t, cert, "cockroach-webhook-service.default.svc")
		require.Equal(t, expectedCA, ca)
	})
	t.Run("sqlstarter", func(t *testing.T) {
		ca, cert, err := crdbcerts.GetSqlStarterCertBundle(ctl)
		require.NoError(t, err)
		requireCommonName(t, cert, "sqlstarter")
		require.Equal(t, expectedCA, ca)
	})
	t.Run("tenant-client", func(t *testing.T) {
		ca, cert, err := crdbcerts.GetTenantClientCertBundle(ctl, 10)
		require.NoError(t, err)
		requireCommonName(t, cert, "10")
		require.Equal(t, expectedCA, ca)
	})
	t.Run("tenantdir", func(t *testing.T) {
		ca, cert, err := crdbcerts.GetTenantDirCertBundle(ctl)
		require.NoError(t, err)
		requireCommonName(t, cert, "tenantdir")
		require.Equal(t, expectedCA, ca)
	})
}

func requireCommonName(t *testing.T, bundle *crypto.Bundle, name string) {
	der, _ := pem.Decode(bundle.Certificate)
	require.NotNil(t, der)

	cert, err := x509.ParseCertificate(der.Bytes)
	require.NoError(t, err)

	require.Equal(t, cert.Subject.CommonName, name)
}

func requireDNSNames(t *testing.T, bundle *crypto.Bundle, names []string) {
	der, _ := pem.Decode(bundle.Certificate)
	require.NotNil(t, der)

	cert, err := x509.ParseCertificate(der.Bytes)
	require.NoError(t, err)

	require.ElementsMatch(t, cert.DNSNames, names)
}

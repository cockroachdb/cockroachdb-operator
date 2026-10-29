package crdbcerts

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"testing"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/crypto"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

type failSigner struct {
	signer crypto.CertificateSigner
}

// Certificate implements crypto.CertificateSigner
func (f *failSigner) Certificate(ctx context.Context) ([]byte, error) {
	return f.signer.Certificate(ctx)
}

// Sign implements crypto.CertificateSigner
func (f *failSigner) Sign(
	ctx context.Context, csr crypto.CertificateSigningRequest,
) (*crypto.Bundle, error) {
	return nil, errors.AssertionFailedf("failSigner.Sign was called with %v", csr)
}

var _ crypto.CertificateSigner = &failSigner{}

func TestCreateCrdbClusterCerts(t *testing.T) {
	ctx := context.Background()

	signer, err := crypto.FakeSigner()
	require.NoError(t, err)

	t.Run("dedicated cluster", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{})
		ctl = ctl.WithNamespace("us-east1")
		createTestCrdbClusterCerts(ctx, t, ctl, false /* isHostCluster */, nil /* customCA */, signer)
	})

	t.Run("host cluster", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{})
		ctl = ctl.WithNamespace("us-east1")
		createTestCrdbClusterCerts(ctx, t, ctl, true /* isHostCluster */, nil /* customCA */, signer)
	})

	t.Run("retries skip pre-existing secrets", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{})
		ctl = ctl.WithNamespace("us-east1")
		createTestCrdbClusterCerts(ctx, t, ctl, true /* isHostCluster */, nil /* customCA */, signer)
		createTestCrdbClusterCerts(ctx, t, ctl, true /* isHostCluster */, nil /* customCA */, &failSigner{signer})
	})

	t.Run("custom CA", func(t *testing.T) {
		// openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -x509 -nodes -days 3650
		customCA := []byte(`-----BEGIN CERTIFICATE-----
MIICKzCCAdGgAwIBAgIUNYLCTz5rW9+OhjhuinPXY69XbTQwCgYIKoZIzj0EAwIw
azELMAkGA1UEBhMCQ0ExCzAJBgNVBAgMAk9OMQ8wDQYDVQQHDAZPdHRhd2ExEjAQ
BgNVBAoMCUNvY2tyb2FjaDEWMBQGA1UECwwNT3JjaGVzdHJhdGlvbjESMBAGA1UE
AwwJQ3VzdG9tIENBMB4XDTIzMDIwOTE4MzkwMVoXDTMyMTEwODE4MzkwMVowazEL
MAkGA1UEBhMCQ0ExCzAJBgNVBAgMAk9OMQ8wDQYDVQQHDAZPdHRhd2ExEjAQBgNV
BAoMCUNvY2tyb2FjaDEWMBQGA1UECwwNT3JjaGVzdHJhdGlvbjESMBAGA1UEAwwJ
Q3VzdG9tIENBMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE4rs5fVb8egkkttqP
dPymlzaoG4+uS4BLI8gBbPUfIcWrCpYmrFbUAufydt974MNAPM+QDNi92vihNOW+
zReW1KNTMFEwHQYDVR0OBBYEFDWb2fqDFAJxUrE8pO6liy5jtGckMB8GA1UdIwQY
MBaAFDWb2fqDFAJxUrE8pO6liy5jtGckMA8GA1UdEwEB/wQFMAMBAf8wCgYIKoZI
zj0EAwIDSAAwRQIgASwJKyHWSfSUtD2/ok49yqJ/SGoFXTm4uD8QWvd2cvACIQDR
SgarRERCNKgRhm/yg95FDkxTNSR8PGIOX6bpAyGPLg==
-----END CERTIFICATE-----`)

		ctl := kubetest.NewFakeCtl(kubetest.Options{})
		ctl = ctl.WithNamespace("us-east1")
		createTestCrdbClusterCerts(ctx, t, ctl, false /* isHostCluster */, customCA, signer)
	})
}

func requireIsCAConfigMap(t *testing.T, ctl *kube.Ctl, name string) {
	var cm corev1.ConfigMap
	require.NoError(t, ctl.Get(context.Background(), name, &cm))
	require.Contains(t, cm.Data, "ca.crt")
}

func requireIsTLSSecret(t *testing.T, ctl *kube.Ctl, secretName string) {
	var s corev1.Secret
	require.NoError(t, ctl.Get(context.Background(), secretName, &s))
	require.Equal(t, corev1.SecretTypeTLS, s.Type)
	require.Contains(t, s.Data, "tls.crt")
	require.Contains(t, s.Data, "tls.key")
}

func TestCreateTenantCerts(t *testing.T) {
	const tenantID = crdbv1beta1.TenantID(10)
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("namespace")
	createTestTenantCerts(ctx, t, ctl, tenantID)
}

func parseCertificate(t *testing.T, bundle *crypto.Bundle) *x509.Certificate {
	require.NotNil(t, bundle)

	der, rest := pem.Decode(bundle.Certificate)
	require.Empty(t, rest)
	require.Equal(t, der.Type, "CERTIFICATE")

	x509Cert, err := x509.ParseCertificate(der.Bytes)
	require.NoError(t, err)
	return x509Cert
}

func createTestCrdbClusterCerts(
	ctx context.Context,
	t *testing.T,
	ctl *kube.Ctl,
	isHostCluster bool,
	customCA []byte,
	signer crypto.CertificateSigner,
) {

	// Typically the external certs would be signed by let's encrypt.
	externalSigner, err := crypto.FakeSigner()
	require.NoError(t, err)
	externalCertBundle, err := externalSigner.Sign(ctx, crypto.CertificateSigningRequest{
		CommonName: "external-cert-bundle",
	})
	require.NoError(t, err)

	requireExternalCert := func(cert *x509.Certificate) {
		require.NotNil(t, cert)
		require.Equal(t, "external-cert-bundle", cert.Subject.CommonName)
	}

	require.NoError(t, CreateCrdbClusterCerts(
		ctx, signer, ctl, CrdbClusterCertOpts{
			IsHost:                           true,
			IsCustomerAccount:                false,
			AltNodeNames:                     []string{"altName1", "altName2"},
			DeprecatedAlternateClusterDomain: "cluster.remote",
			AlternateClusterDomain:           "cluster.3p3.remote",
			AdditionalSQLClientCA:            customCA,
			ExternalCert:                     externalCertBundle,
			ServerlessDNS:                    "3p3.gcp-us-central1.crdb.io",
		},
	))

	expectedCA, err := signer.Certificate(ctx)
	require.NoError(t, err)

	getSecret := func(secretName string) map[string]*x509.Certificate {
		var secret corev1.Secret
		require.NoError(t, ctl.Get(ctx, secretName, &secret))
		ca, certs := crypto.FromSecretData(secret.Data)
		require.Equal(t, expectedCA, ca)

		result := map[string]*x509.Certificate{}
		for name, cert := range certs {
			result[name] = parseCertificate(t, cert)
		}

		return result
	}

	nodeCerts := getSecret("node-certs")
	nodeCert := nodeCerts["node"]
	require.NotNil(t, nodeCert)
	require.Equal(t, "node", nodeCert.Subject.CommonName)
	require.Equal(t, []net.IP{[]byte{127, 0, 0, 1}}, nodeCert.IPAddresses)
	require.Equal(t, []string{
		"altName1",
		"altName2",
		"cockroachdb-internal",
		"cockroachdb-internal.us-east1",
		"cockroachdb-internal.us-east1.svc.cluster.local",
		"cockroachdb-internal.us-east1.svc.cluster.remote",
		"cockroachdb-internal.us-east1.svc.cluster.3p3.remote",
		"cockroachdb-join.us-east1.svc.cluster.local",
		"cockroachdb-join.us-east1.svc.cluster.remote",
		"cockroachdb-join.us-east1.svc.cluster.3p3.remote",
		"*.cockroachdb",
		"*.cockroachdb.us-east1",
		"*.cockroachdb.us-east1.svc.cluster.local",
		"*.cockroachdb.us-east1.svc.cluster.remote",
		"*.cockroachdb.us-east1.svc.cluster.3p3.remote",
		"localhost",
	}, nodeCert.DNSNames)

	require.Contains(t, nodeCerts, "client.root")
	require.Equal(t, "root", nodeCerts["client.root"].Subject.CommonName)
	require.Len(t, nodeCerts, 2)

	clientCerts := getSecret("client-certs")
	require.Contains(t, clientCerts, "client.root")
	require.Equal(t, "root", clientCerts["client.root"].Subject.CommonName)
	require.Len(t, clientCerts, 1)

	uiCerts := getSecret("ui-certs")
	requireExternalCert(uiCerts["ui"])
	require.Len(t, uiCerts, 1)

	// Assert that we've created all the CRD config maps and secrets we expect to have created.
	requireIsCAConfigMap(t, ctl, manifests.CockroachClientCAConfigMapName)
	requireIsCAConfigMap(t, ctl, manifests.CockroachNodeCAConfigMapName)
	requireIsTLSSecret(t, ctl, manifests.CockroachHTTPSecretName)
	requireIsTLSSecret(t, ctl, manifests.CockroachNodeClientSecretName)
	requireIsTLSSecret(t, ctl, manifests.CockroachNodeSecretName)
	requireIsTLSSecret(t, ctl, manifests.CockroachRootSQLClientSecretName)

	if isHostCluster {
		sqlProxyCerts := getSecret("sql-proxy-external-certs")
		require.Equal(t, "", sqlProxyCerts["sql-proxy-external"].Subject.CommonName)
		require.ElementsMatch(t, []string{
			"*.gcp-us-central1.crdb.io",
			"*.crdb.io",
			"*.3p3.gcp-us-central1.crdb.io",
			"*.internal-3p3.gcp-us-central1.crdb.io",
			"*.3p3.crdb.io",
		}, sqlProxyCerts["sql-proxy-external"].DNSNames)
		require.Len(t, sqlProxyCerts, 1)

		sqlStarterCerts := getSecret("sqlstarter-certs")
		require.Contains(t, sqlStarterCerts, "sqlstarter")
		require.Equal(t, sqlStarterCerts["sqlstarter"].Subject.CommonName, "sqlstarter")
		require.Len(t, sqlStarterCerts, 1)

		tenantdirCerts := getSecret("tenantdir-certs")
		require.Contains(t, tenantdirCerts, "tls")
		uri, err := url.Parse("crdb://service/tenantdir")
		require.NoError(t, err)
		require.Equal(t, tenantdirCerts["tls"].URIs, []*url.URL{uri})
	}

	if len(customCA) != 0 {
		var cm corev1.ConfigMap
		require.NoError(t, ctl.Get(ctx, manifests.CockroachClientCAConfigMapName, &cm))
		require.Contains(t, cm.Data["ca.crt"], string(customCA))

		// should not be added to the node CA
		require.NoError(t, ctl.Get(ctx, manifests.CockroachNodeCAConfigMapName, &cm))
		require.NotContains(t, cm.Data["ca.crt"], string(customCA))
	}
}

func createTestTenantCerts(
	ctx context.Context, t *testing.T, ctl *kube.Ctl, tenantID crdbv1beta1.TenantID,
) {
	signer, err := crypto.FakeSigner()
	require.NoError(t, err)

	require.NoError(t, CreateTenantCerts(ctx, signer, ctl, tenantID, "cluster.3p3.remote"))

	var secret corev1.Secret
	require.NoError(t, ctl.Get(ctx, fmt.Sprintf("tenant-client-%d-certs", tenantID), &secret))
	ca, certs := crypto.FromSecretData(secret.Data)

	expectedCA, err := signer.Certificate(ctx)
	require.NoError(t, err)
	require.Equal(t, ca, expectedCA)

	require.Len(t, certs, 3)

	tenantCert := parseCertificate(t, certs["client-tenant.10"])
	require.Equal(t, "10", tenantCert.Subject.CommonName)
	require.Equal(t, []string{"Tenants"}, tenantCert.Subject.OrganizationalUnit)
	require.Equal(t, []string{
		fmt.Sprintf("tenant-%d", tenantID),
		"localhost",
		fmt.Sprintf("*.%s.pod.cluster.local", ctl.Namespace()),
		fmt.Sprintf("*.%s.pod.cluster.3p3.remote", ctl.Namespace()),
	}, tenantCert.DNSNames)
	require.Equal(t, []net.IP{
		[]byte{127, 0, 0, 1},
		[]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1},
		[]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	}, tenantCert.IPAddresses)

	tenantSigningCert := parseCertificate(t, certs["tenant-signing.10"])
	require.Equal(t, "tenant-10", tenantSigningCert.Subject.CommonName)
}

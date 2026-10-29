package kubecrdbtest

import (
	"context"
	"fmt"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/crypto"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/crdbcerts"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/stretchr/testify/require"
)

// InstallOperatorCerts installs certificates used during testing of the
// operator. This is called each time a new K8s cluster is created (not once for
// each namespace like InstallClusterCerts). The kube.Ctl should be globally
// scoped.
// NEVER USE THESE CERTIFICATES IN A PRODUCTION ENVIRONMENT!
func InstallOperatorCerts(t *testing.T, ctl *kube.Ctl) {
	ctx := context.Background()

	// Use the deterministic signer so that kuberecord tests always get
	// identical certificates during playback.
	signer := crypto.DeterministicFakeSignerForTesting(t)

	secret, err := manifests.CockroachOperatorCertConfig().ToK8SSecret(ctx, signer)
	require.NoError(t, err)

	require.NoError(t, ctl.Apply(ctx, secret))
}

// InstallClusterCerts installs certificates used during testing of dedicated
// and tenant clusters. This is called each time a new K8s namespace is created
// for a CrdbCluster. The kube.Ctl instance should be scoped to that namespace.
// NEVER USE THESE CERTIFICATES IN A PRODUCTION ENVIRONMENT!
func InstallClusterCerts(t *testing.T, ctl *kube.Ctl, isHostCluster bool) {
	ctx := context.Background()

	// Use the deterministic signer so that kuberecord tests always get identical
	// certificates during playback.
	signer := crypto.DeterministicFakeSignerForTesting(t)

	// Generate external certificate for the SQLProxy and/or DBConsole that
	// resembles the ones we use in staging and production.
	namespace := ctl.Namespace()
	dnsName := fmt.Sprintf("*.%s.crdb.io", namespace)
	extCertBundle, err := signer.Sign(ctx, crypto.CertificateSigningRequest{
		Role:       "server",
		CommonName: dnsName,
		// Adding "cockroachdb-sqlproxy-external" allows integration tests to
		// connect to the sql proxy through the external service. In staging and
		// production environments, our components connect using the sqlproxy dns
		// that is created per cluster.
		DNSNames: []string{"*.crdb.io", dnsName, "cockroachdb-sqlproxy-external", "localhost"},
	})
	require.NoError(t, err)

	err = crdbcerts.CreateCrdbClusterCerts(ctx, signer, ctl, crdbcerts.CrdbClusterCertOpts{
		ExternalCert:      extCertBundle,
		IsHost:            isHostCluster,
		IsCustomerAccount: false,
		ServerlessDNS:     fmt.Sprintf("k8s.%s.crdb.io", namespace),
		// NB: DeprecatedAlternateClusterDomain is not currently used in K3d
		// tests but is used in all other code paths. Rather than build in
		// gating to respect the empty string, we use a dummy value.
		DeprecatedAlternateClusterDomain: "not.used",
		AlternateClusterDomain:           "not.used",
	})
	require.NoError(t, err)
}

// InstallTenantCerts installs certificates used during testing of tenant
// clusters. This is called each time a new CrdbTenant is created in a K8s
// namespace. The kube.Ctl instance should be scoped to that namespace.
// NEVER USE THESE CERTIFICATES IN A PRODUCTION ENVIRONMENT!
func InstallTenantCerts(t *testing.T, ctl *kube.Ctl, tenantID crdbv1beta1.TenantID) {
	// Use the deterministic signer so that kuberecord tests always get identical
	// certificates during playback.
	ctx := context.Background()
	signer := crypto.DeterministicFakeSignerForTesting(t)
	// NB: alternateClusterDomain is not currently used in K3d tests but is
	// used in all other code paths. Rather than build in gating to respect
	// the empty string, we use a dummy value.
	require.NoError(t, crdbcerts.CreateTenantCerts(ctx, signer, ctl, tenantID, "not.used"))
}

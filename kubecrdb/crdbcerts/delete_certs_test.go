package crdbcerts

import (
	"context"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/stretchr/testify/require"
)

func TestDeleteTenantCerts(t *testing.T) {
	// Create certificates and then delete them.
	const tenantID = crdbv1beta1.TenantID(10)
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("namespace")
	createTestTenantCerts(ctx, t, ctl, tenantID)

	require.NoError(t, DeleteTenantCerts(ctx, ctl, tenantID))

	// Ensure secrets are gone.
	_, _, err := GetTenantClientCertBundle(ctl, tenantID)
	require.True(t, kubeerrors.IsNotFound(err))
}

package v1beta1_test

import (
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/stretchr/testify/require"
)

func TestMakeCrdbClusterReady(t *testing.T) {
	var cluster crdbv1beta1.CrdbCluster

	require.Error(t, cluster.IsReady())
	crdbv1beta1.MakeCrdbClusterReady(&cluster)
	require.NoError(t, cluster.IsReady())
}

func TestMakeCrdbClusterQuiescent(t *testing.T) {
	var cluster crdbv1beta1.CrdbCluster

	crdbv1beta1.MakeCrdbClusterQuiescent(&cluster)
	require.NoError(t, cluster.IsQuiescent())
}

func TestMakeCrdbNodeReady(t *testing.T) {
	var node crdbv1beta1.CrdbNode
	require.Error(t, node.IsReady())
	crdbv1beta1.MakeCrdbNodeReady(&node)
	require.NoError(t, node.IsReady())
}

func TestMakeCrdbTenantReady(t *testing.T) {
	var tenant crdbv1beta1.CrdbTenant
	require.Error(t, tenant.IsReady())
	crdbv1beta1.MakeCrdbTenantReady(&tenant)
	require.NoError(t, tenant.IsReady())
}

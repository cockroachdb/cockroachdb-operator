package opserver_test

import (
	"context"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/operatorpb"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/testenv"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestIntegrationScaleTenant(t *testing.T) {
	testutils.MarkIntegrationTest(t)

	// Set up a test cluster with two tenants.
	ctx := context.Background()
	env := testenv.New(t, testenv.SingleZoneConfig)
	cluster := testenv.CreateCrdbCluster(t, env, "test-cluster", &crdbv1beta1.CrdbClusterSpec{
		TLSEnabled: true,
		TenantHost: &crdbv1beta1.TenantHost{
			TenantPools: []crdbv1beta1.TenantPool{
				{
					Name:    "v25-1-2",
					MaxPods: new(int32(5)),
				},
			},
		},
	})
	ctl := env.DefaultCluster().NewCtl(cluster.Namespace)

	tenantScaleUp := testenv.CreateCrdbTenant(t, ctl, "scale-up", &crdbv1beta1.CrdbTenantSpec{
		TenantID:   5,
		TenantPool: new("v25-1-2"),
	})
	tenantIdle := testenv.CreateCrdbTenant(t, ctl, "idle", &crdbv1beta1.CrdbTenantSpec{
		TenantID:   6,
		TenantPool: new("v25-1-2"),
	})

	// Ensure both tenants start with 0 pods
	tenantScaleUp.Spec.Pods = 0
	tenantIdle.Spec.Pods = 0
	require.NoError(t, ctl.Apply(ctx, tenantScaleUp, tenantIdle))

	// Use the RPC to cold start one of the tenants
	opClient := testenv.DialOperatorServer(t, ctl)
	req := &operatorpb.ScaleTenantRequest{
		Tenant: &operatorpb.ResourceID{
			Name:      tenantScaleUp.GetName(),
			Namespace: cluster.Namespace,
		},
	}
	_, err := opClient.ScaleTenant(ctx, req)
	require.NoError(t, err)

	// Verify the tenant has a bound pod.
	var pods corev1.PodList
	listOptions := &kube.ListOptions{LabelSelector: kube.ParseLabelOrDie("tenant-id=5")}
	require.NoError(t, ctl.List(ctx, listOptions, &pods))
	require.Len(t, pods.Items, 1)

	// Only the scaled tenant should have a bound pod. This ensures the RPC
	// modified the correct tenant and the scale rpc triggered the scale up.
	require.NoError(t, ctl.Get(ctx, tenantScaleUp.GetName(), tenantScaleUp))
	require.Equal(t, tenantScaleUp.Spec.Pods, int32(1))
	require.Equal(t, tenantScaleUp.Status.Pods, int32(1))
	require.NoError(t, ctl.Get(ctx, tenantIdle.GetName(), tenantIdle))
	require.Equal(t, tenantIdle.Spec.Pods, int32(0))

	// Ensure QPS limit is set above the default.
	start := time.Now()
	count := 0
	// Test maximum QPS over 3 sec
	for time.Since(start) < 3*time.Second {
		_, err = opClient.ScaleTenant(ctx, req)
		require.NoError(t, err)
		count++
	}
	t.Logf("ScaleTenant calls/sec measured: %d", count/3)
	// ScaleTenant will do at least 2 k8s calls. So the 3 sec max ScaleTenant
	// call/sec with default client go limits (20 QPS and 30 Burst) will
	// be 15 calls/sec (as the 3s max total query count is 30+3x20=90)
	// So ensure seeing at least 20 call/sec - the typical number is usually >90
	require.GreaterOrEqual(t, count, 60)
}

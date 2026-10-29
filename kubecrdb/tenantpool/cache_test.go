package tenantpool_test

import (
	"context"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/tenantpool"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTenantPoolCache(t *testing.T) {
	cluster1 := crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "cluster"},
		Spec: crdbv1beta1.CrdbClusterSpec{
			TenantHost: &crdbv1beta1.TenantHost{
				TenantPools: []crdbv1beta1.TenantPool{
					{Name: "free", HostReplicaMultiplier: new(float32(1))},
					{Name: "paid", HostReplicaMultiplier: new(float32(3))},
				},
			},
		},
	}

	cluster2 := crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns2", Name: "cluster"},
		Spec: crdbv1beta1.CrdbClusterSpec{
			TenantHost: &crdbv1beta1.TenantHost{
				TenantPools: []crdbv1beta1.TenantPool{
					{Name: "free", HostReplicaMultiplier: new(float32(2))},
					{Name: "low-trust", HostReplicaMultiplier: new(float32(3))},
					{Name: "high-trust", HostReplicaMultiplier: new(float32(5))},
				},
			},
		},
	}

	cluster3 := cluster2
	cluster3.Namespace = "ns3"
	cluster3.Name = "another-cluster"

	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{}, &cluster1, &cluster2, &cluster3)
	var cache tenantpool.Cache

	t.Run("require namespaced Ctl", func(t *testing.T) {
		_, err := cache.Get(ctx, ctl, "unk")
		require.EqualError(t, err, "Get received an unnamespaced *kube.Ctl")
	})

	t.Run("host cluster not found in namespace", func(t *testing.T) {
		_, err := cache.Get(ctx, ctl.WithNamespace("unk"), "free")
		require.EqualError(t, err, "could not find host in namespace unk")
	})

	t.Run("pool not found in namespace", func(t *testing.T) {
		_, err := cache.Get(ctx, ctl.WithNamespace("ns1"), "unk")
		require.EqualError(t, err, "could not find tenant pool unk in namespace ns1")
	})

	t.Run("get pools in namespace 1", func(t *testing.T) {
		pool, err := cache.Get(ctx, ctl.WithNamespace("ns1"), "free")
		require.NoError(t, err)
		require.Equal(t, &crdbv1beta1.TenantPool{Name: "free", HostReplicaMultiplier: new(float32(1))}, pool)

		pool, err = cache.Get(ctx, ctl.WithNamespace("ns1"), "paid")
		require.NoError(t, err)
		require.Equal(t, &crdbv1beta1.TenantPool{Name: "paid", HostReplicaMultiplier: new(float32(3))}, pool)

		// Fetch free pool again.
		pool, err = cache.Get(ctx, ctl.WithNamespace("ns1"), "free")
		require.NoError(t, err)
		require.Equal(t, &crdbv1beta1.TenantPool{Name: "free", HostReplicaMultiplier: new(float32(1))}, pool)
	})

	t.Run("get pool with same name in different namespace", func(t *testing.T) {
		pool, err := cache.Get(ctx, ctl.WithNamespace("ns2"), "free")
		require.NoError(t, err)
		require.Equal(t, &crdbv1beta1.TenantPool{Name: "free", HostReplicaMultiplier: new(float32(2))}, pool)
	})

	t.Run("get 3rd pool in slice", func(t *testing.T) {
		pool, err := cache.Get(ctx, ctl.WithNamespace("ns2"), "high-trust")
		require.NoError(t, err)
		require.Equal(t, &crdbv1beta1.TenantPool{Name: "high-trust", HostReplicaMultiplier: new(float32(5))}, pool)
	})

	t.Run("invalidate pool updated on server", func(t *testing.T) {
		start := timeutil.Now()
		ctl = ctl.WithNamespace("ns3")
		pool, err := cache.Get(ctx, ctl, "low-trust")
		require.NoError(t, err)
		require.Equal(t, &crdbv1beta1.TenantPool{Name: "low-trust", HostReplicaMultiplier: new(float32(3))}, pool)

		// Update the cluster 3 pools.
		cluster3.Spec.TenantHost = &crdbv1beta1.TenantHost{
			TenantPools: []crdbv1beta1.TenantPool{
				{Name: "low-trust", HostReplicaMultiplier: new(float32(4))},
			},
		}
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, &cluster3))

		// Refresh every 100ms.
		cache.RefreshInterval = 100 * time.Millisecond
		defer func() {
			cache.RefreshInterval = 0
		}()

		require.Eventually(t, func() bool {
			pool, err := cache.Get(ctx, ctl, "low-trust")
			require.NoError(t, err)
			return *pool.HostReplicaMultiplier == 4
		}, time.Second, 10*time.Millisecond)

		require.Greater(t, timeutil.Now().Sub(start), cache.RefreshInterval)

		// Ensure that other pools in the namespace are gone.
		_, err = cache.Get(ctx, ctl, "high-trust")
		require.EqualError(t, err, "could not find tenant pool high-trust in namespace ns3")
	})

	// Leave this test case last, since it deletes a host cluster.
	t.Run("ensure deleted pool is still accessible", func(t *testing.T) {
		// Ensure pool is in cache.
		ctl = ctl.WithNamespace("ns3")
		before, err := cache.Get(ctx, ctl, "low-trust")
		require.NoError(t, err)

		// Now delete its host cluster.
		ctl = ctl.WithNamespace("ns3")
		require.NoError(t, ctl.DeleteWithOptions(ctx, kube.DeleteOptions{NoWait: true}, &cluster3))

		// Pool should still be in cache.
		after, err := cache.Get(ctx, ctl, "low-trust")
		require.NoError(t, err)
		require.Equal(t, before, after)

		// Force refresh of the pool and ensure namespace is gone.
		cache.RefreshInterval = time.Millisecond
		defer func() {
			cache.RefreshInterval = 0
		}()

		require.Eventually(t, func() bool {
			_, err := cache.Get(ctx, ctl, "low-trust")
			return err != nil && err.Error() == "could not find host in namespace ns3"
		}, time.Second, 10*time.Millisecond)
	})
}

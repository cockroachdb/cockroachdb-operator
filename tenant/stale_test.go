package tenant

import (
	"context"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/testmanifest"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestLabelStalePoolPods(t *testing.T) {
	ctx := context.Background()
	namespace := "test-ns"

	factory := testmanifest.NewFactory(namespace)
	ctl := kubetest.NewFakeCtl(kubetest.Options{})

	createTenant := func(cluster *crdbv1beta1.CrdbCluster, pool string) *crdbv1beta1.CrdbTenant {
		tenant := factory.CreateTenant(cluster)
		tenant.Spec.TenantPool = &pool
		return tenant
	}

	createStampedPod := func(tenant *crdbv1beta1.CrdbTenant, pool string) *corev1.Pod {
		pod := factory.CreateStampedPod(tenant)
		pod.ObjectMeta.Labels[manifests.TenantPoolNameLabel] = pool
		return pod
	}

	listStampedPods := func(tenant *crdbv1beta1.CrdbTenant) *corev1.PodList {
		options := &kube.ListOptions{
			LabelSelector: manifests.MakeStampedPodSelector(tenant.Spec.TenantID),
		}
		list := &corev1.PodList{}
		require.NoError(t, ctl.List(ctx, options, list))
		return list
	}

	t.Run("NoPods", func(t *testing.T) {
		cluster := factory.CreateCluster()
		crdbTenant := createTenant(cluster, "current-pool")
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true},
			cluster, crdbTenant))
		require.NoError(t, labelStalePoolPods(ctx, ctl, listStampedPods(crdbTenant).Items, crdbTenant))
	})

	t.Run("PodsForCorrectPool", func(t *testing.T) {
		cluster := factory.CreateCluster()
		crdbTenant := createTenant(cluster, "current-pool")
		pod1 := createStampedPod(crdbTenant, "current-pool")
		pod2 := createStampedPod(crdbTenant, "current-pool")

		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true},
			cluster, crdbTenant, pod1, pod2))
		require.NoError(t, labelStalePoolPods(ctx, ctl, listStampedPods(crdbTenant).Items, crdbTenant))

		ignored := &corev1.Pod{}
		require.NoError(t, ctl.Get(ctx, pod1.Name, ignored))
		require.False(t, manifests.IsTenantPodStale(ignored))
		require.NoError(t, ctl.Get(ctx, pod2.Name, ignored))
		require.False(t, manifests.IsTenantPodStale(ignored))
	})

	t.Run("PodsForIncorrectPool", func(t *testing.T) {
		cluster := factory.CreateCluster()
		crdbTenant := createTenant(cluster, "current-pool")
		pod1 := createStampedPod(crdbTenant, "wrong-pool")
		pod2 := createStampedPod(crdbTenant, "wrong-pool")

		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true},
			cluster, crdbTenant, pod1, pod2))
		require.NoError(t, labelStalePoolPods(ctx, ctl, listStampedPods(crdbTenant).Items, crdbTenant))

		ignored := &corev1.Pod{}
		require.NoError(t, ctl.Get(ctx, pod1.Name, ignored))
		require.True(t, manifests.IsTenantPodStale(ignored))
		require.NoError(t, ctl.Get(ctx, pod2.Name, ignored))
		require.True(t, manifests.IsTenantPodStale(ignored))
	})

	t.Run("PodsFromMixedPool", func(t *testing.T) {
		cluster := factory.CreateCluster()
		crdbTenant := createTenant(cluster, "current-pool")
		pod1 := createStampedPod(crdbTenant, "wrong-pool")
		pod2 := createStampedPod(crdbTenant, "wrong-pool")
		pod3 := createStampedPod(crdbTenant, "current-pool")

		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true},
			cluster, crdbTenant, pod1, pod2, pod3))
		require.NoError(t, labelStalePoolPods(ctx, ctl, listStampedPods(crdbTenant).Items, crdbTenant))

		ignored := &corev1.Pod{}
		require.NoError(t, ctl.Get(ctx, pod1.Name, ignored))
		require.True(t, manifests.IsTenantPodStale(ignored))
		require.NoError(t, ctl.Get(ctx, pod2.Name, ignored))
		require.True(t, manifests.IsTenantPodStale(ignored))
		require.NoError(t, ctl.Get(ctx, pod3.Name, ignored))
		require.False(t, manifests.IsTenantPodStale(ignored))
	})

	t.Run("PodsFromDifferentTenant", func(t *testing.T) {
		cluster := factory.CreateCluster()
		crdbTenant := createTenant(cluster, "current-pool")
		tenant2 := createTenant(cluster, "current-pool")
		tenant2Pod1 := createStampedPod(tenant2, "wrong-pool")
		tenant2Pod2 := createStampedPod(tenant2, "wrong-pool")
		tenant1Pod1 := createStampedPod(crdbTenant, "current-pool")

		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true},
			cluster, crdbTenant, tenant2, tenant1Pod1, tenant2Pod2, tenant2Pod1))
		require.NoError(t, labelStalePoolPods(ctx, ctl, listStampedPods(crdbTenant).Items, crdbTenant))

		ignored := &corev1.Pod{}
		require.NoError(t, ctl.Get(ctx, tenant2Pod1.Name, ignored))
		require.False(t, manifests.IsTenantPodStale(ignored))
		require.NoError(t, ctl.Get(ctx, tenant2Pod2.Name, ignored))
		require.False(t, manifests.IsTenantPodStale(ignored))
		require.NoError(t, ctl.Get(ctx, tenant1Pod1.Name, ignored))
		require.False(t, manifests.IsTenantPodStale(ignored))
	})
}

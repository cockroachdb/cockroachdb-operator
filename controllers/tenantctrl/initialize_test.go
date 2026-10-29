package tenantctrl

import (
	"context"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSetOwningClusterTo(t *testing.T) {
	ctx := context.Background()
	namespace := "test-ns"
	opts := kube.Options{BeforeMutation: kubetest.MakeReady}

	makeCluster := func() *crdbv1beta1.CrdbCluster {
		return &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cockroachdb",
				Namespace: namespace,
				UID:       "cluster-uid-123",
			},
		}
	}

	makeTenant := func() *crdbv1beta1.CrdbTenant {
		return &crdbv1beta1.CrdbTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "tenant-1",
				Namespace: namespace,
			},
			Spec: crdbv1beta1.CrdbTenantSpec{
				TenantID: 1,
			},
		}
	}

	newCtl := func(objects ...kube.Object) *kube.Ctl {
		return kubetest.NewFakeCtl(kubetest.Options{Options: opts}, objects...).WithNamespace(namespace)
	}

	t.Run("adds ownerRef when none exists", func(t *testing.T) {
		cluster := makeCluster()
		tenant := makeTenant()
		ctl := newCtl(cluster, tenant)

		require.NoError(t, SetOwningClusterTo(ctx, ctl, tenant, cluster))

		var updated crdbv1beta1.CrdbTenant
		require.NoError(t, ctl.Get(ctx, tenant.Name, &updated))
		require.Len(t, updated.OwnerReferences, 1)
		require.Equal(t, crdbv1beta1.GroupVersion.String(), updated.OwnerReferences[0].APIVersion)
		require.Equal(t, cluster.UID, updated.OwnerReferences[0].UID)
	})

	t.Run("no-op when ownerRef already current", func(t *testing.T) {
		cluster := makeCluster()
		tenant := makeTenant()
		ctl := newCtl(cluster, tenant)

		require.NoError(t, SetOwningClusterTo(ctx, ctl, tenant, cluster))

		var after crdbv1beta1.CrdbTenant
		require.NoError(t, ctl.Get(ctx, tenant.Name, &after))
		rv := after.ResourceVersion

		require.NoError(t, SetOwningClusterTo(ctx, ctl, &after, cluster))
		require.Equal(t, rv, after.ResourceVersion)
	})

	t.Run("updates stale APIVersion on existing ownerRef", func(t *testing.T) {
		cluster := makeCluster()
		tenant := makeTenant()
		tenant.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion:         "crdb.cockroachlabs.com/v1alpha1",
				Kind:               "CrdbCluster",
				Name:               cluster.Name,
				UID:                cluster.UID,
				BlockOwnerDeletion: new(true),
			},
		}

		ctl := newCtl(cluster, tenant)

		require.NoError(t, SetOwningClusterTo(ctx, ctl, tenant, cluster))

		var updated crdbv1beta1.CrdbTenant
		require.NoError(t, ctl.Get(ctx, tenant.Name, &updated))
		require.Len(t, updated.OwnerReferences, 1)
		require.Equal(t, crdbv1beta1.GroupVersion.String(), updated.OwnerReferences[0].APIVersion)
		require.Equal(t, cluster.UID, updated.OwnerReferences[0].UID)
	})
}

func TestEnsureOwnerRef(t *testing.T) {
	ctx := context.Background()
	namespace := "test-ns"
	opts := kube.Options{BeforeMutation: kubetest.MakeReady}

	makeCluster := func() *crdbv1beta1.CrdbCluster {
		return &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cockroachdb",
				Namespace: namespace,
				UID:       "cluster-uid-123",
			},
		}
	}

	makeTenant := func() *crdbv1beta1.CrdbTenant {
		return &crdbv1beta1.CrdbTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "tenant-1",
				Namespace: namespace,
			},
			Spec: crdbv1beta1.CrdbTenantSpec{
				TenantID: 1,
			},
		}
	}

	newCtl := func(objects ...kube.Object) *kube.Ctl {
		return kubetest.NewFakeCtl(kubetest.Options{Options: opts}, objects...).WithNamespace(namespace)
	}

	t.Run("no-op when no cluster exists", func(t *testing.T) {
		tenant := makeTenant()
		ctl := newCtl(tenant)
		handler := requestHandler{ctx: ctx, ctl: ctl, tenant: tenant}

		require.NoError(t, handler.ensureOwnerRef())

		var after crdbv1beta1.CrdbTenant
		require.NoError(t, ctl.Get(ctx, tenant.Name, &after))
		require.Empty(t, after.OwnerReferences)
	})

	t.Run("sets ownerRef when cluster exists", func(t *testing.T) {
		cluster := makeCluster()
		tenant := makeTenant()
		ctl := newCtl(cluster, tenant)
		handler := requestHandler{ctx: ctx, ctl: ctl, tenant: tenant}

		require.NoError(t, handler.ensureOwnerRef())

		var updated crdbv1beta1.CrdbTenant
		require.NoError(t, ctl.Get(ctx, tenant.Name, &updated))
		require.Len(t, updated.OwnerReferences, 1)
		require.Equal(t, crdbv1beta1.GroupVersion.String(), updated.OwnerReferences[0].APIVersion)
	})
}

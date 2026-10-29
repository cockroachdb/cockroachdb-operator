package tenantctrl

import (
	"context"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDelete(t *testing.T) {
	ctx := context.Background()
	namespace := "test-ns"

	const tenantID = crdbv1beta1.TenantID(20)
	tenant := crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:              tenantID.Tag(),
			Namespace:         namespace,
			DeletionTimestamp: &metav1.Time{Time: time.Now()},
		},
		Spec: crdbv1beta1.CrdbTenantSpec{
			TenantID: tenantID,
		},
	}

	opts := kube.Options{BeforeMutation: kubetest.MakeReady}
	cnpCrd := apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ciliumnetworkpolicies.cilium.io",
			Namespace: namespace,
		},
	}
	ccnpCrd := apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ciliumclusterwidenetworkpolicies.cilium.io",
			Namespace: namespace,
		},
	}

	ctl := kubetest.NewFakeCtl(kubetest.Options{Options: opts}, &cnpCrd, &ccnpCrd).WithNamespace(namespace)
	h := requestHandler{ctx: ctx, ctl: ctl, tenant: &tenant}
	require.NoError(t, h.Deploy())
	require.NoError(t, h.Delete())

	policy := manifests.TenantNetworkPolicy(manifests.TenantNetworkPolicyOptions{
		TenantID:      h.tenant.Spec.TenantID,
		NetworkPlugin: cloud.Cilium,
	})

	err := ctl.Get(ctx, policy.GetName(), policy)
	require.True(t, kubeerrors.IsNotFound(err))
}

// TestDeleteTerminatesStampedPods verifies that Delete removes all pods
// stamped with the tenant's ID before returning, and does not touch pods
// stamped with a different tenant's ID. This is what prevents the CrashLoop
// window between crdb_internal.destroy_tenant() and the operator finalizer
// clearing (which is what fires the "tenant containers are restarting"
// monitor).
func TestDeleteTerminatesStampedPods(t *testing.T) {
	ctx := context.Background()
	namespace := "test-ns"

	const tenantID = crdbv1beta1.TenantID(20)
	const otherTenantID = crdbv1beta1.TenantID(21)

	tenant := crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:              tenantID.Tag(),
			Namespace:         namespace,
			DeletionTimestamp: &metav1.Time{Time: time.Now()},
		},
		Spec: crdbv1beta1.CrdbTenantSpec{
			TenantID: tenantID,
		},
	}

	// Two pods stamped for our tenant — both should be deleted.
	stampedPod1 := makeStampedPod(namespace, "stamped-1", tenantID)
	stampedPod2 := makeStampedPod(namespace, "stamped-2", tenantID)
	// One pod stamped for a different tenant — must NOT be deleted.
	otherTenantPod := makeStampedPod(namespace, "other-tenant", otherTenantID)

	opts := kube.Options{BeforeMutation: kubetest.MakeReady}
	cnpCrd := apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "ciliumnetworkpolicies.cilium.io", Namespace: namespace},
	}
	ccnpCrd := apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "ciliumclusterwidenetworkpolicies.cilium.io", Namespace: namespace},
	}

	ctl := kubetest.NewFakeCtl(
		kubetest.Options{Options: opts},
		&cnpCrd, &ccnpCrd, stampedPod1, stampedPod2, otherTenantPod,
	).WithNamespace(namespace)

	h := requestHandler{ctx: ctx, ctl: ctl, tenant: &tenant}
	require.NoError(t, h.Deploy())
	require.NoError(t, h.Delete())

	// Both pods stamped for this tenant should be gone.
	var got corev1.Pod
	require.True(t, kubeerrors.IsNotFound(ctl.Get(ctx, stampedPod1.Name, &got)),
		"expected pod %q to be deleted", stampedPod1.Name)
	require.True(t, kubeerrors.IsNotFound(ctl.Get(ctx, stampedPod2.Name, &got)),
		"expected pod %q to be deleted", stampedPod2.Name)

	// The other tenant's pod must still exist.
	require.NoError(t, ctl.Get(ctx, otherTenantPod.Name, &got),
		"pod %q for a different tenant should not have been deleted", otherTenantPod.Name)
}

func makeStampedPod(namespace, name string, id crdbv1beta1.TenantID) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app":                   manifests.TenantPoolAppName,
				manifests.TenantIDLabel: id.String(),
			},
		},
	}
}

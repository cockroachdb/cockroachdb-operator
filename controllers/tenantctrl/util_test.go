package tenantctrl

import (
	"context"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestUpdateFilterPredicates(t *testing.T) {
	testCases := []struct {
		Object          kube.Object
		ShouldReconcile bool
	}{
		{&crdbv1beta1.CrdbTenant{}, true},
		{&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{{Kind: crdbv1beta1.CrdbTenantKind}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodFailed},
		}, true},
		{&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{{Kind: "notCrdbTenant"}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodFailed},
		}, false},
		{&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{{Kind: crdbv1beta1.CrdbTenantKind}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}, false},
		{&corev1.Pod{}, false},
	}

	updateFunc := filterPredicates(context.Background()).UpdateFunc
	for _, tc := range testCases {
		require.Equal(t, tc.ShouldReconcile, updateFunc(event.UpdateEvent{ObjectOld: tc.Object, ObjectNew: tc.Object}))
	}

	// Test case for reconciling on a tenant pod being marked as stale.
	podNotStale := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{{Kind: crdbv1beta1.CrdbTenantKind}},
		},
	}
	podStale := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{{Kind: crdbv1beta1.CrdbTenantKind}},
			Labels:          map[string]string{},
		},
	}
	manifests.MarkTenantPodAsStale(podStale)
	require.Equal(t, true, updateFunc(event.UpdateEvent{ObjectOld: podNotStale, ObjectNew: podStale}))

	podReady := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{{Kind: crdbv1beta1.CrdbTenantKind}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: manifests.CockroachDB, Ready: true},
			},
		},
	}
	podNotReady := podReady.DeepCopy()
	podNotReady.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: manifests.CockroachDB, Ready: false},
	}

	require.True(t, updateFunc(event.UpdateEvent{ObjectOld: podReady, ObjectNew: podNotReady}))
}

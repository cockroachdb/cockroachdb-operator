package manager

import (
	"context"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode/nodemock"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestOOMKiller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Inject the pods and CrdbNode resources expected by the test.
	ctl := kubetest.NewFakeCtl(kubetest.Options{})
	opts := kube.ApplyOptions{NoWait: true}
	require.NoError(t, ctl.ApplyWithOptions(ctx, opts,
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "namespace-1",
			},
		},
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "namespace-2",
			},
		},
	))

	require.NoError(t, ctl.WithNamespace("namespace-1").ApplyWithOptions(ctx, opts,
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "running-pod",
				Namespace: "namespace-1",
				Labels:    map[string]string{"app": "cockroachdb"},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pending-pod",
				Namespace: "namespace-1",
				Labels:    map[string]string{"app": "cockroachdb"},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "evicted-node",
				Namespace: "namespace-1",
				Labels:    map[string]string{"app": "cockroachdb"},
			},
			Status: corev1.PodStatus{
				Phase:   corev1.PodFailed,
				Reason:  "Evicted",
				Message: "The node was low on resource: memory. Threshold quantity: 100Mi, available: -116Ki. Container stress was using 120613632Ki, request is 0, has larger consumption of memory. ",
			},
		},
		&crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "evicted-node",
				Namespace: "namespace-1",
				Labels:    map[string]string{"app": "cockroachdb"},
			},
		},
	))

	require.NoError(t, ctl.WithNamespace("namespace-2").ApplyWithOptions(ctx, opts,
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "evicted-node",
				Namespace: "namespace-2",
				Labels:    map[string]string{"app": "cockroachdb"},
			},
			Status: corev1.PodStatus{
				Phase:   corev1.PodFailed,
				Reason:  "Evicted",
				Message: "The node was low on resource: memory. Threshold quantity: 100Mi, available: -116Ki. Container stress was using 120613632Ki, request is 0, has larger consumption of memory. ",
			},
		},
		&crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "evicted-node",
				Namespace: "namespace-2",
				Labels:    map[string]string{"app": "cockroachdb"},
			},
		},
	))

	mockCtrl := gomock.NewController(t)
	exec := nodemock.NewMockNodeExecutor(mockCtrl)

	var callCount int
	exec.EXPECT().Kill(gomock.Any()).DoAndReturn(func(_ context.Context) error {
		callCount++
		if callCount == 2 {
			cancel()
		}
		return nil
	}).Times(2)

	k := OOMKiller{
		ctl: ctl,
		nodeExecutor: func(node *crdbv1beta1.CrdbNode, _ kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
			require.Equal(t, "evicted-node", node.Name)
			return exec
		},
	}

	k.Start(ctx)
}

package k8snodectrl

import (
	"context"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode/nodemock"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNodeWithoutDecommissionAnnotation(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, crdbv1beta1.AddToScheme(scheme))

	// Create k8s node without decommission annotation
	k8sNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
	}

	ctl := kubetest.NewFakeCtl(kubetest.Options{})
	fakeAPIReader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(k8sNode).
		Build()

	reconciler := &Reconciler{
		Ctl:       ctl,
		APIReader: fakeAPIReader,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-node"}}
	result, err := reconciler.Reconcile(ctx, req)

	require.NoError(t, err)
	// No requeue since no annotation
	assert.True(t, result.IsZero())
}

func TestNodeWithDecommissionAnnotationNoCrdbNodes(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, crdbv1beta1.AddToScheme(scheme))

	// Create k8s node with decommission annotation but no CrdbNodes
	k8sNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
			Annotations: map[string]string{
				crdbv1beta1.K8sNodeStartDecommissionAnnotation: "true",
			},
		},
	}

	ctl := kubetest.NewFakeCtl(kubetest.Options{})
	fakeAPIReader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(k8sNode).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(obj client.Object) []string {
			pod := obj.(*corev1.Pod)
			return []string{pod.Spec.NodeName}
		}).
		Build()

	reconciler := &Reconciler{
		Ctl:       ctl,
		APIReader: fakeAPIReader,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-node"}}
	result, err := reconciler.Reconcile(ctx, req)

	require.NoError(t, err)
	// No requeue since no CrdbNodes
	assert.True(t, result.IsZero())
}

func TestDecommissionRunningNode(t *testing.T) {
	ctx := context.Background()
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, crdbv1beta1.AddToScheme(scheme))

	k8sNodeName := "test-node"
	namespace := "test-namespace"
	nodeName := "test-crdbnode"

	ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace(namespace)

	// Create k8s node with decommission annotation
	k8sNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: k8sNodeName,
			Annotations: map[string]string{
				crdbv1beta1.K8sNodeStartDecommissionAnnotation: "true",
			},
		},
	}

	// Create pod scheduled on the k8s node with proper OwnerReference
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodeName,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "crdb.cockroachlabs.com/v1beta1",
					Kind:       "CrdbNode",
					Name:       nodeName,
					UID:        "test-uid",
					Controller: &[]bool{true}[0],
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: k8sNodeName,
		},
	}

	// Create running CrdbNode
	crdbNode := &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodeName,
			Namespace: namespace,
		},
		Status: crdbv1beta1.CrdbNodeStatus{
			Conditions: []crdbv1beta1.NodeCondition{
				{
					Type:   crdbv1beta1.PodRunning,
					Status: metav1.ConditionTrue,
				},
			},
		},
	}

	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, crdbNode))

	fakeAPIReader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(k8sNode, pod).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(obj client.Object) []string {
			p := obj.(*corev1.Pod)
			return []string{p.Spec.NodeName}
		}).
		Build()

	// Mock NodeExecutor
	mockExec := nodemock.NewMockNodeExecutor(mockCtrl)
	mockExec.EXPECT().NodeID(ctx).Return(int64(1), nil)

	reconciler := &Reconciler{
		Ctl:       ctl,
		APIReader: fakeAPIReader,
		NodeExecutor: func(node *crdbv1beta1.CrdbNode, exec kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
			return mockExec
		},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: k8sNodeName}}
	result, err := reconciler.Reconcile(ctx, req)

	require.NoError(t, err)
	// Should requeue to process next node
	assert.Equal(t, time.Second*10, result.RequeueAfter)

	// Verify the CrdbNode was updated with decommission annotation
	var updatedNode crdbv1beta1.CrdbNode
	err = ctl.Get(ctx, nodeName, &updatedNode)
	require.NoError(t, err)
	assert.True(t, updatedNode.IsDecommissioning())
}

func TestWaitForDecommissioningNodes(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, crdbv1beta1.AddToScheme(scheme))

	k8sNodeName := "test-node"
	namespace := "test-namespace"

	ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace(namespace)

	// Create k8s node with decommission annotation
	k8sNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: k8sNodeName,
			Annotations: map[string]string{
				crdbv1beta1.K8sNodeStartDecommissionAnnotation: "true",
			},
		},
	}

	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node1",
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "crdb.cockroachlabs.com/v1beta1",
					Kind:       "CrdbNode",
					Name:       "node1",
					UID:        "test-uid-1",
					Controller: &[]bool{true}[0],
				},
			},
		},
		Spec: corev1.PodSpec{NodeName: k8sNodeName},
	}

	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node2",
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "crdb.cockroachlabs.com/v1beta1",
					Kind:       "CrdbNode",
					Name:       "node2",
					UID:        "test-uid-2",
					Controller: &[]bool{true}[0],
				},
			},
		},
		Spec: corev1.PodSpec{NodeName: k8sNodeName},
	}

	// First node is already decommissioning
	decommissioningNode := &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node1",
			Namespace: namespace,
			Annotations: map[string]string{
				crdbv1beta1.CrdbNodeStartDecommissionAnnotation: "1",
			},
		},
	}

	// Second node is ready to be decommissioned
	candidateNode := &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node2", Namespace: namespace},
	}

	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, decommissioningNode, candidateNode))

	fakeAPIReader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(k8sNode, pod1, pod2).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(obj client.Object) []string {
			pod := obj.(*corev1.Pod)
			return []string{pod.Spec.NodeName}
		}).
		Build()

	reconciler := &Reconciler{
		Ctl:       ctl,
		APIReader: fakeAPIReader,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: k8sNodeName}}
	result, err := reconciler.Reconcile(ctx, req)

	require.NoError(t, err)
	// Should wait for decommissioning node
	assert.Equal(t, time.Second*30, result.RequeueAfter)
}

func TestIgnorePodsWithoutOwnerReference(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, crdbv1beta1.AddToScheme(scheme))

	k8sNodeName := "test-node"
	namespace := "test-namespace"

	// Create k8s node with decommission annotation
	k8sNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: k8sNodeName,
			Annotations: map[string]string{
				crdbv1beta1.K8sNodeStartDecommissionAnnotation: "true",
			},
		},
	}

	// Create a pod without OwnerReference, this should be ignored
	podWithoutOwner := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "some-random-pod",
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			NodeName: k8sNodeName,
		},
	}

	// Create a pod with the wrong OwnerReference kind, this should be ignored
	podWithWrongOwner := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "deployment-pod",
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "some-deployment",
					UID:        "wrong-uid",
					Controller: &[]bool{true}[0],
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: k8sNodeName,
		},
	}

	ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace(namespace)
	fakeAPIReader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(k8sNode, podWithoutOwner, podWithWrongOwner).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(obj client.Object) []string {
			pod := obj.(*corev1.Pod)
			return []string{pod.Spec.NodeName}
		}).
		Build()

	reconciler := &Reconciler{
		Ctl:       ctl,
		APIReader: fakeAPIReader,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: k8sNodeName}}
	result, err := reconciler.Reconcile(ctx, req)

	require.NoError(t, err)
	// No requeue since no CrdbNodes found
	assert.True(t, result.IsZero())
}

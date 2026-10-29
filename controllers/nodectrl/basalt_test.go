package nodectrl

import (
	"context"
	"testing"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode/nodemock"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/evictionutil"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func basaltTestReconciler(t *testing.T, storeIDs []int64) (*Reconciler, *kube.Ctl) {
	t.Helper()
	mockCtrl := gomock.NewController(t)
	t.Cleanup(mockCtrl.Finish)
	ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("default")
	r := &Reconciler{
		Ctl:            ctl,
		EvictionHelper: &evictionutil.Helper{},
		NodeExecutor: func(_ *crdbv1beta1.CrdbNode, _ kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
			m := nodemock.NewMockNodeExecutor(mockCtrl)
			m.EXPECT().Kill(gomock.Any()).AnyTimes()
			m.EXPECT().StoreIDs(gomock.Any(), gomock.Any()).Return(storeIDs, nil).AnyTimes()
			return m
		},
	}
	return r, ctl
}

func newBasaltNode(name string) *crdbv1beta1.CrdbNode {
	return &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: crdbv1beta1.CrdbNodeSpec{
			DataStore: crdbv1beta1.DataStore{
				Basalt: &crdbv1beta1.BasaltStoreSpec{
					ControllerAddrs: []string{"10.0.0.2:26256"},
					TenantID:        "5e0c10f6-5bc7-5be1-bbb7-78cec71d8570",
				},
			},
		},
	}
}

func TestSnapshotBasaltStoreIDs(t *testing.T) {
	ctx := context.Background()
	mockCtrl := gomock.NewController(t)
	t.Cleanup(mockCtrl.Finish)

	t.Run("merges store ids", func(t *testing.T) {
		executor := nodemock.NewMockNodeExecutor(mockCtrl)
		executor.EXPECT().StoreIDs(gomock.Any(), int64(7)).Return([]int64{6, 5, 6}, nil)
		node := newBasaltNode("cockroachdb")
		node.Status.StoreIDs = []int64{4, 5}

		require.NoError(t, snapshotBasaltStoreIDs(ctx, node, executor, 7))
		require.Equal(t, []int64{4, 5, 6}, node.Status.StoreIDs)
	})

	t.Run("preserves store ids on query failure", func(t *testing.T) {
		executor := nodemock.NewMockNodeExecutor(mockCtrl)
		executor.EXPECT().StoreIDs(gomock.Any(), int64(7)).Return(nil, errors.New("sql unavailable"))
		node := newBasaltNode("cockroachdb")
		node.Status.StoreIDs = []int64{5}

		require.ErrorContains(t, snapshotBasaltStoreIDs(ctx, node, executor, 7), "sql unavailable")
		require.Equal(t, []int64{5}, node.Status.StoreIDs)
	})

	t.Run("no-op for non-basalt nodes", func(t *testing.T) {
		executor := nodemock.NewMockNodeExecutor(mockCtrl)
		node := &crdbv1beta1.CrdbNode{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "default"}}

		require.NoError(t, snapshotBasaltStoreIDs(ctx, node, executor, 7))
		require.Nil(t, node.Status.StoreIDs)
	})
}

// TestCleanupNodeResources_BasaltNodeDecommission drives the node deletion path
// for a single-node decommission: the pod is killed and deleted, then a cleanup
// Job reclaims the dirs, and only once it succeeds is the finalizer removed.
func TestCleanupNodeResources_BasaltNodeDecommission(t *testing.T) {
	ctx := context.Background()
	r, ctl := basaltTestReconciler(t, nil)

	// Owning cluster that is not being deleted, so the node does its own cleanup.
	cluster := &crdbv1beta1.CrdbCluster{ObjectMeta: metav1.ObjectMeta{Name: "cockroachdb", Namespace: "default"}}
	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, cluster))

	node := newBasaltNode("cockroachdb-xyz")
	node.Finalizers = []string{manifests.CrdbNodeFinalizerName}
	node.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: crdbv1beta1.GroupVersion.String(),
		Kind:       crdbv1beta1.CrdbClusterKind,
		Name:       "cockroachdb",
	}}
	node.Status.StoreIDs = []int64{5}
	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, node))

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: "default"}}
	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, pod))

	cleanupJobName := manifests.BasaltStoreCleanupJobName(node.Name)

	// Pod is killed and deleted first, before any cleanup Job exists.
	res, err := r.cleanupNodeResources(ctx, node)
	require.NoError(t, err)
	require.NotZero(t, res.RequeueAfter)
	kubetest.RequireNotExists(t, ctl, node.Name, &corev1.Pod{})
	kubetest.RequireNotExists(t, ctl, cleanupJobName, &batchv1.Job{})

	// With the pod gone, the cleanup Job is created and the finalizer is held.
	_, err = r.cleanupNodeResources(ctx, node)
	require.NoError(t, err)
	kubetest.RequireExists(t, ctl, cleanupJobName, &batchv1.Job{})
	require.Contains(t, node.Finalizers, manifests.CrdbNodeFinalizerName)

	// Simulate the cleanup Job succeeding.
	var job batchv1.Job
	require.NoError(t, ctl.Get(ctx, cleanupJobName, &job))
	job.Status.Succeeded = 1
	require.NoError(t, ctl.UpdateStatus(ctx, &job))

	// Cleanup done, so the finalizer is removed.
	_, err = r.cleanupNodeResources(ctx, node)
	require.NoError(t, err)
	require.NotContains(t, node.Finalizers, manifests.CrdbNodeFinalizerName)
}

// TestCleanupNodeResources_BasaltClusterDeleting confirms a node whose owning
// cluster is being deleted skips its own per-node cleanup Job, since the
// control plane's tenant delete reclaims the whole namespace, while still
// killing and deleting its pod and dropping its finalizer.
func TestCleanupNodeResources_BasaltClusterDeleting(t *testing.T) {
	ctx := context.Background()
	r, ctl := basaltTestReconciler(t, nil)

	// The owning cluster is already terminating, which is what steers the node
	// away from its own cleanup Job.
	now := metav1.Now()
	cluster := &crdbv1beta1.CrdbCluster{ObjectMeta: metav1.ObjectMeta{
		Name:              "cockroachdb",
		Namespace:         "default",
		DeletionTimestamp: &now,
	}}
	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, cluster))

	node := newBasaltNode("cockroachdb-xyz")
	node.Finalizers = []string{manifests.CrdbNodeFinalizerName}
	node.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: crdbv1beta1.GroupVersion.String(),
		Kind:       crdbv1beta1.CrdbClusterKind,
		Name:       "cockroachdb",
	}}
	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, node))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: "default"}}
	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, pod))

	// Drive cleanup to completion.
	for range 5 {
		res, err := r.cleanupNodeResources(ctx, node)
		require.NoError(t, err)
		if res.RequeueAfter == 0 && !res.Requeue {
			break
		}
	}

	// The pod is gone, the finalizer is dropped, and no per-node cleanup Job was
	// ever created on this path.
	kubetest.RequireNotExists(t, ctl, node.Name, &corev1.Pod{})
	kubetest.RequireNotExists(t, ctl, manifests.BasaltStoreCleanupJobName(node.Name), &batchv1.Job{})
	require.NotContains(t, node.Finalizers, manifests.CrdbNodeFinalizerName)
}

// TestCleanupNodeResources_BasaltCleanupGivesUp confirms a terminally failed
// cleanup Job does not wedge node deletion: the operator gives up, drops the
// finalizer, and leaves reclamation to an external sweeper or operator.
func TestCleanupNodeResources_BasaltCleanupGivesUp(t *testing.T) {
	ctx := context.Background()
	r, ctl := basaltTestReconciler(t, nil)

	cluster := &crdbv1beta1.CrdbCluster{ObjectMeta: metav1.ObjectMeta{Name: "cockroachdb", Namespace: "default"}}
	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, cluster))

	node := newBasaltNode("cockroachdb-xyz")
	node.Finalizers = []string{manifests.CrdbNodeFinalizerName}
	node.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: crdbv1beta1.GroupVersion.String(),
		Kind:       crdbv1beta1.CrdbClusterKind,
		Name:       "cockroachdb",
	}}
	node.Status.StoreIDs = []int64{5}
	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, node))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: "default"}}
	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, pod))

	cleanupJobName := manifests.BasaltStoreCleanupJobName(node.Name)

	// First pass deletes the pod, second creates the cleanup Job.
	_, err := r.cleanupNodeResources(ctx, node)
	require.NoError(t, err)
	_, err = r.cleanupNodeResources(ctx, node)
	require.NoError(t, err)
	kubetest.RequireExists(t, ctl, cleanupJobName, &batchv1.Job{})

	// Mark the cleanup Job as terminally failed.
	var job batchv1.Job
	require.NoError(t, ctl.Get(ctx, cleanupJobName, &job))
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	require.NoError(t, ctl.UpdateStatus(ctx, &job))

	// The operator gives up rather than retrying forever, and removes the finalizer.
	_, err = r.cleanupNodeResources(ctx, node)
	require.NoError(t, err)
	require.NotContains(t, node.Finalizers, manifests.CrdbNodeFinalizerName)
}

func TestCleanupBasaltStores_RejectsStaleJob(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("default")
	r := &Reconciler{Ctl: ctl}
	node := newBasaltNode("cockroachdb-reused")
	node.UID = "current-node-uid"

	jobName := manifests.BasaltStoreCleanupJobName(node.Name)
	staleJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: node.Namespace,
			Annotations: map[string]string{
				manifests.BasaltLifecycleResourceUIDAnnotation: "previous-node-uid",
			},
		},
		Status: batchv1.JobStatus{Succeeded: 1},
	}
	require.NoError(t, ctl.Create(ctx, staleJob))

	cleaned, err := r.cleanupBasaltStores(ctx, node)
	require.NoError(t, err)
	require.False(t, cleaned)
	kubetest.RequireNotExists(t, ctl, jobName, &batchv1.Job{})

	// The next pass creates a Job tied to the current node rather than trusting
	// the stale success from the previous same-named node.
	cleaned, err = r.cleanupBasaltStores(ctx, node)
	require.NoError(t, err)
	require.False(t, cleaned)
	var currentJob batchv1.Job
	require.NoError(t, ctl.Get(ctx, jobName, &currentJob))
	require.Equal(t, string(node.UID),
		currentJob.Annotations[manifests.BasaltLifecycleResourceUIDAnnotation])
}

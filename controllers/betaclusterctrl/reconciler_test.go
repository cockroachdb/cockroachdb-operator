package betaclusterctrl

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/migrationctrl"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode/nodemock"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/evictionutil"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/record"
)

func TestNodeLabelSelectorForCluster(t *testing.T) {
	sel := nodeLabelSelectorForCluster(&crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "crdb-test"},
	})
	require.True(t,
		sel.Matches(labels.Set(map[string]string{manifests.BetaCrdbClusterNodeOwnerLabelKey: "crdb-test"})))
	require.False(t,
		sel.Matches(labels.Set(map[string]string{manifests.BetaCrdbClusterNodeOwnerLabelKey: "crdb-test1"})))
	require.False(t, sel.Matches(labels.Set{}))
}

func TestInitializationNode(t *testing.T) {
	nodeWithRunningStatus := func(name string, status metav1.ConditionStatus) crdbv1beta1.CrdbNode {
		return crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: crdbv1beta1.CrdbNodeStatus{Conditions: []crdbv1beta1.NodeCondition{
				{Type: crdbv1beta1.PodRunning, Status: status},
			}},
		}
	}
	nodes := crdbv1beta1.CrdbNodeList{Items: []crdbv1beta1.CrdbNode{
		nodeWithRunningStatus("node-c", metav1.ConditionTrue),
		nodeWithRunningStatus("node-a", metav1.ConditionFalse),
		nodeWithRunningStatus("node-b", metav1.ConditionTrue),
	}}

	cluster := &crdbv1beta1.CrdbCluster{}
	node := initializationNode(cluster, nodes)
	require.NotNil(t, node)
	require.Equal(t, "node-b", node.Name, "an unpinned cluster should choose a stable running node")

	node = initializationNode(cluster, crdbv1beta1.CrdbNodeList{Items: []crdbv1beta1.CrdbNode{
		nodeWithRunningStatus("node-a", metav1.ConditionFalse),
	}})
	require.Nil(t, node)

	cluster.Status.InitializationNode = "node-c"
	node = initializationNode(cluster, nodes)
	require.NotNil(t, node)
	require.Equal(t, "node-c", node.Name, "a retry should use the pinned node")

	cluster.Status.InitializationNode = "node-a"
	node = initializationNode(cluster, nodes)
	require.NotNil(t, node)
	require.Equal(t, "node-a", node.Name, "a pinned node should be retained while it is not running")

	cluster.Status.InitializationNode = "missing"
	node = initializationNode(cluster, nodes)
	require.NotNil(t, node)
	require.Equal(t, "node-b", node.Name, "a deleted pin should be replaced with a stable running node")
}

func TestPinInitializationNode(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("test")
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "test", Generation: 1},
	}
	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, cluster))
	require.NoError(t, ctl.Get(ctx, cluster.Name, cluster))
	resourceVersion := cluster.ResourceVersion
	cluster.Status.ObservedGeneration = cluster.Generation
	cluster.Status.Region = "us-east1"
	cluster.Status.Provider = "k3d"
	cluster.Status.ReconciledByBetaController = true
	cluster.Status.Reconciled = true
	cluster.Status.ReadyNodes = 3

	require.NoError(t, pinInitializationNode(ctx, ctl, cluster, &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-b"},
	}))
	require.Equal(t, "node-b", cluster.Status.InitializationNode)
	require.NotEqual(t, resourceVersion, cluster.ResourceVersion, "pinning must refresh the reconcile object's resource version")
	require.Equal(t, "us-east1", cluster.Status.Region)
	require.Equal(t, "k3d", cluster.Status.Provider)
	require.True(t, cluster.Status.ReconciledByBetaController)
	require.True(t, cluster.Status.Reconciled)
	require.Equal(t, int32(3), cluster.Status.ReadyNodes)

	var stored crdbv1beta1.CrdbCluster
	require.NoError(t, ctl.Get(ctx, cluster.Name, &stored))
	require.Equal(t, "node-b", stored.Status.InitializationNode)
	require.Equal(t, cluster.Generation, stored.Status.ObservedGeneration,
		"the first status write must include the observed generation")

	cluster.Status.ReadyNodes = 1
	require.NoError(t, ctl.UpdateStatus(ctx, cluster), "the outer reconciler must be able to update status after pinning")

	require.NoError(t, ctl.Get(ctx, cluster.Name, &stored))
	require.Equal(t, "node-b", stored.Status.InitializationNode)
	require.Equal(t, int32(1), stored.Status.ReadyNodes)

	repinned := stored.ResourceVersion
	require.NoError(t, pinInitializationNode(ctx, ctl, &stored, &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-b"},
	}))
	require.Equal(t, repinned, stored.ResourceVersion, "re-pinning the same node must not issue a patch")

	require.NoError(t, pinInitializationNode(ctx, ctl, &stored, &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-c"},
	}))
	require.NoError(t, ctl.Get(ctx, cluster.Name, &stored))
	require.Equal(t, "node-c", stored.Status.InitializationNode, "a deleted pin must be replaceable")
}

func TestRecordInitializationStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		// record stands in for one reconcile that could not initialize the cluster.
		record          func(*Reconciler, context.Context, *kube.Ctl, *crdbv1beta1.CrdbCluster) error
		wantReason      string
		wantMessage     string
		wantRepeatEvent bool
	}{
		{
			name: "timeout",
			record: func(r *Reconciler, ctx context.Context, ctl *kube.Ctl, c *crdbv1beta1.CrdbCluster) error {
				return r.recordInitializationFailure(ctx, ctl, c, "node-b", context.DeadlineExceeded)
			},
			wantReason:  "InitializationTimedOut",
			wantMessage: "cockroach init on CrdbNode/node-b exceeded 5m0s; see operator logs",
			// Every attempt we actually got to make is worth reporting.
			wantRepeatEvent: true,
		},
		{
			name: "other failure",
			record: func(r *Reconciler, ctx context.Context, ctl *kube.Ctl, c *crdbv1beta1.CrdbCluster) error {
				return r.recordInitializationFailure(ctx, ctl, c, "node-b", errors.New("boom"))
			},
			wantReason:      "InitializationFailed",
			wantMessage:     "cockroach init on CrdbNode/node-b failed; see operator logs",
			wantRepeatEvent: true,
		},
		{
			name: "node unavailable",
			record: func(r *Reconciler, ctx context.Context, ctl *kube.Ctl, c *crdbv1beta1.CrdbCluster) error {
				return r.recordInitializationNodeUnavailable(ctx, ctl, c, "node-b")
			},
			wantReason:  "InitializationNodeUnavailable",
			wantMessage: "CrdbNode/node-b was selected to initialize the cluster but is not Running",
			// We requeue through this path on every pass while we wait, so it must
			// not keep announcing the same stall.
			wantRepeatEvent: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("test")
			cluster := &crdbv1beta1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "test"},
			}
			require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, cluster))
			require.NoError(t, ctl.Get(ctx, cluster.Name, cluster))

			recorder := record.NewFakeRecorder(2)
			reconciler := &Reconciler{Emit: kube.EventEmitter{EventRecorder: recorder}}
			require.NoError(t, tc.record(reconciler, ctx, ctl, cluster))

			select {
			case event := <-recorder.Events:
				require.Contains(t, event, "Warning "+tc.wantReason)
				require.Contains(t, event, tc.wantMessage)
			default:
				t.Fatal("a failed initialization did not emit a warning event")
			}

			var stored crdbv1beta1.CrdbCluster
			require.NoError(t, ctl.Get(ctx, cluster.Name, &stored))
			condition, ok := stored.Status.FindCondition(crdbv1beta1.ClusterInitialized)
			require.True(t, ok, "Initialized condition was not persisted")
			require.Equal(t, metav1.ConditionFalse, condition.Status)
			require.Equal(t, tc.wantReason, condition.Reason)
			require.Equal(t, tc.wantMessage, condition.Message)
			require.EqualError(t, stored.IsReady(),
				fmt.Sprintf("cluster is not initialized: %s: %s", tc.wantReason, tc.wantMessage),
				"the reason a cluster never initialized must reach readiness checks")

			require.NoError(t, tc.record(reconciler, ctx, ctl, cluster))
			select {
			case event := <-recorder.Events:
				require.True(t, tc.wantRepeatEvent, "reconciling again emitted a duplicate event: %s", event)
			default:
				require.False(t, tc.wantRepeatEvent, "reconciling again dropped the event")
			}
		})
	}
}

func TestUpdateStatus(t *testing.T) {
	provider := "gcp"
	region := "us-east1"
	cluster := &crdbv1beta1.CrdbCluster{
		Spec: crdbv1beta1.CrdbClusterSpec{Regions: []crdbv1beta1.CrdbClusterRegion{
			{
				CloudProvider: provider,
				Code:          region,
				Nodes:         2,
			},
			{
				CloudProvider: "aws",
				Code:          "another-region",
				Nodes:         100,
			},
		}},
	}
	reconciler := &Reconciler{
		Env: &cloud.Environment{
			CloudProvider: cloud.Provider(provider),
			CloudRegion:   region,
		},
	}

	// update status to populate the region code and provider so that we can get an actual revision.
	reconciler.updateStatus(cluster, crdbv1beta1.CrdbNodeList{})
	revision := manifests.GetClusterRevision(cluster)
	testCases := []struct {
		testName   string
		cluster    *crdbv1beta1.CrdbCluster
		nodes      crdbv1beta1.CrdbNodeList
		readyNodes int32
		reconciled bool
	}{
		{
			testName:   "no nodes",
			cluster:    cluster.DeepCopy(),
			nodes:      crdbv1beta1.CrdbNodeList{},
			readyNodes: 0,
			reconciled: false,
		},
		{
			testName: "one node is out of date",
			cluster:  cluster.DeepCopy(),
			nodes: crdbv1beta1.CrdbNodeList{
				Items: []crdbv1beta1.CrdbNode{
					{
						Status: crdbv1beta1.CrdbNodeStatus{
							NodeID: new(int64(1)),
							Conditions: []crdbv1beta1.NodeCondition{
								{
									Type:   crdbv1beta1.PodReady,
									Status: metav1.ConditionTrue,
								},
								{
									Type:   crdbv1beta1.PodRunning,
									Status: metav1.ConditionTrue,
								},
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Annotations: map[string]string{
								manifests.BetaCrdbClusterNodeRevisionAnnotationKey: revision,
							},
						},
						Status: crdbv1beta1.CrdbNodeStatus{
							NodeID: new(int64(2)),
							Conditions: []crdbv1beta1.NodeCondition{
								{
									Type:   crdbv1beta1.PodReady,
									Status: metav1.ConditionTrue,
								},
								{
									Type:   crdbv1beta1.PodRunning,
									Status: metav1.ConditionTrue,
								},
							},
						},
					},
				},
			},
			readyNodes: 2,
			reconciled: false,
		},
		{
			testName: "all nodes are healthy",
			cluster:  cluster.DeepCopy(),
			nodes: crdbv1beta1.CrdbNodeList{
				Items: []crdbv1beta1.CrdbNode{
					{
						ObjectMeta: metav1.ObjectMeta{
							Annotations: map[string]string{
								manifests.BetaCrdbClusterNodeRevisionAnnotationKey: revision,
							},
						},
						Status: crdbv1beta1.CrdbNodeStatus{
							NodeID: new(int64(1)),
							Conditions: []crdbv1beta1.NodeCondition{
								{
									Type:   crdbv1beta1.PodReady,
									Status: metav1.ConditionTrue,
								},
								{
									Type:   crdbv1beta1.PodRunning,
									Status: metav1.ConditionTrue,
								},
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Annotations: map[string]string{
								manifests.BetaCrdbClusterNodeRevisionAnnotationKey: revision,
							},
						},
						Status: crdbv1beta1.CrdbNodeStatus{
							NodeID: new(int64(2)),
							Conditions: []crdbv1beta1.NodeCondition{
								{
									Type:   crdbv1beta1.PodReady,
									Status: metav1.ConditionTrue,
								},
								{
									Type:   crdbv1beta1.PodRunning,
									Status: metav1.ConditionTrue,
								},
							},
						},
					},
				},
			},
			readyNodes: 2,
			reconciled: true,
		},
		{
			testName: "one node is not healthy",
			cluster:  cluster.DeepCopy(),
			nodes: crdbv1beta1.CrdbNodeList{
				Items: []crdbv1beta1.CrdbNode{
					{
						ObjectMeta: metav1.ObjectMeta{
							Annotations: map[string]string{
								manifests.BetaCrdbClusterNodeRevisionAnnotationKey: revision,
							},
						},
						Status: crdbv1beta1.CrdbNodeStatus{
							Conditions: []crdbv1beta1.NodeCondition{
								// Pod is not ready and not running.
								{
									Type:   crdbv1beta1.PodReady,
									Status: metav1.ConditionFalse,
								},
								{
									Type:   crdbv1beta1.PodRunning,
									Status: metav1.ConditionFalse,
								},
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Annotations: map[string]string{
								manifests.BetaCrdbClusterNodeRevisionAnnotationKey: revision,
							},
						},
						Status: crdbv1beta1.CrdbNodeStatus{
							NodeID: new(int64(2)),
							Conditions: []crdbv1beta1.NodeCondition{
								{
									Type:   crdbv1beta1.PodReady,
									Status: metav1.ConditionTrue,
								},
								{
									Type:   crdbv1beta1.PodRunning,
									Status: metav1.ConditionTrue,
								},
							},
						},
					},
				},
			},
			readyNodes: 1,
			reconciled: true,
		},
	}

	for i := range testCases {
		t.Run(testCases[i].testName, func(t *testing.T) {

			reconciler.updateStatus(testCases[i].cluster, testCases[i].nodes)
			readyNodes := testCases[i].cluster.Status.ReadyNodes
			require.Equal(t, testCases[i].readyNodes, readyNodes, "healthy status is wrong")

			reconciled := testCases[i].cluster.Status.Reconciled
			require.Equal(t, testCases[i].reconciled, reconciled, "reconciled status is wrong")

			isBetaController := testCases[i].cluster.Status.ReconciledByBetaController
			require.True(t, isBetaController)
		})
	}
}

func TestReconcilerHandlesPendingDecommissionAnnotation(t *testing.T) {
	ctx := context.Background()
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("test")

	mutableOnly := crdbv1beta1.MutableOnly
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "test",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east1", Nodes: 1, CloudProvider: "k3d"},
			},
			Mode:                &mutableOnly,
			RollingRestartDelay: &metav1.Duration{Duration: 10 * time.Second},
		},
	}

	// Test case 1: Node with pending decommission that is still not running
	t.Run("pending decommission node not running gets deleted immediately", func(t *testing.T) {
		node := &crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-node",
				Namespace: "test",
				Annotations: map[string]string{
					crdbv1beta1.CrdbNodeStartPendingDecommissionAnnotation: "true",
				},
				Labels: map[string]string{
					manifests.BetaCrdbClusterNodeOwnerLabelKey: cluster.Name,
				},
			},
			Status: crdbv1beta1.CrdbNodeStatus{
				Conditions: []crdbv1beta1.NodeCondition{
					{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionFalse},
				},
			},
		}

		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{
			NoWait: true,
		}, cluster))
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{
			NoWait: true,
		}, node))

		mockNodeExec := nodemock.NewMockNodeExecutor(mockCtrl)
		mockNodeExec.EXPECT().QueryRegionStatus(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		mockNodeExec.EXPECT().Version(gomock.Any()).Return("v23.1.0", nil).AnyTimes()
		mockNodeExec.EXPECT().UnderReplicatedRanges(gomock.Any()).Return(int64(0), nil).AnyTimes()
		r := &Reconciler{
			Ctl: ctl,
			Env: &cloud.Environment{},
			NodeExecutor: func(node *crdbv1beta1.CrdbNode, ctl kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
				return mockNodeExec
			},
		}
		result, err := r.ReconcileCluster(ctx, ctl, cluster)

		require.NoError(t, err)
		require.True(t, result.IsZero()) // wait for the CrdbNode deletion event

		// Verify node was deleted (non-running nodes with pending decommission are deleted immediately)
		var updatedNode crdbv1beta1.CrdbNode
		err = ctl.Get(ctx, node.Name, &updatedNode)
		require.Error(t, err)
		require.True(t, kubeerrors.IsNotFound(err), "Node should be deleted")
	})

	// Test case 2: Node with pending decommission that becomes running
	t.Run("pending decommission node becomes running gets annotation removed", func(t *testing.T) {
		node := &crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-node-2",
				Namespace: "test",
				Annotations: map[string]string{
					crdbv1beta1.CrdbNodeStartPendingDecommissionAnnotation: "true",
				},
				Labels: map[string]string{
					manifests.BetaCrdbClusterNodeOwnerLabelKey: cluster.Name,
				},
			},
			Status: crdbv1beta1.CrdbNodeStatus{
				NodeID: new(int64(2)),
				Conditions: []crdbv1beta1.NodeCondition{
					{Type: crdbv1beta1.PodReady, Status: metav1.ConditionTrue}, // Now running and ready
					{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue},
				},
			},
		}

		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{
			NoWait: true,
		}, cluster, node))

		// Mock NodeExecutor
		mockNodeExec := nodemock.NewMockNodeExecutor(mockCtrl)
		mockNodeExec.EXPECT().UnderReplicatedRanges(gomock.Any()).Return(int64(0), nil).AnyTimes()

		nodeExecFunc := func(node *crdbv1beta1.CrdbNode, exec kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
			return mockNodeExec
		}

		r := &Reconciler{
			Ctl:          ctl,
			Env:          &cloud.Environment{},
			NodeExecutor: nodeExecFunc,
		}

		_, err := r.ReconcileCluster(ctx, ctl, cluster)
		require.NoError(t, err)

		// Verify the pending decommission annotation was cleared
		var updatedNode crdbv1beta1.CrdbNode
		require.NoError(t, ctl.Get(ctx, node.Name, &updatedNode))

		require.False(t, updatedNode.IsMarkedForPendingDecommission(), "Pending decommission annotation should be cleared")
	})

	// Test case 3: Node that is updating should be skipped
	t.Run("updating node with pending decommission is skipped", func(t *testing.T) {
		node := &crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-node-3",
				Namespace: "test",
				Annotations: map[string]string{
					crdbv1beta1.CrdbNodeStartPendingDecommissionAnnotation: "true",
				},
				Labels: map[string]string{
					manifests.BetaCrdbClusterNodeOwnerLabelKey: cluster.Name,
				},
			},
			Status: crdbv1beta1.CrdbNodeStatus{
				Conditions: []crdbv1beta1.NodeCondition{
					{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionFalse},
					{Type: crdbv1beta1.PodNeedsUpdate, Status: metav1.ConditionTrue}, // Currently updating
				},
			},
		}

		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{
			NoWait: true,
		}, cluster, node))

		mockNodeExec2 := nodemock.NewMockNodeExecutor(mockCtrl)
		mockNodeExec2.EXPECT().QueryRegionStatus(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		mockNodeExec2.EXPECT().Version(gomock.Any()).Return("v23.1.0", nil).AnyTimes()
		mockNodeExec2.EXPECT().UnderReplicatedRanges(gomock.Any()).Return(int64(0), nil).AnyTimes()
		r := &Reconciler{
			Ctl:            ctl,
			EvictionHelper: &evictionutil.Helper{},
			Env: &cloud.Environment{
				CloudProvider: cloud.Provider("k3d"),
				CloudRegion:   "us-east1",
			},
			NodeExecutor: func(node *crdbv1beta1.CrdbNode, ctl kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
				return mockNodeExec2
			},
		}
		_, err := r.ReconcileCluster(ctx, ctl, cluster)

		require.NoError(t, err)

		// Verify node was not touched - still has pending decommission annotation
		var updatedNode crdbv1beta1.CrdbNode
		require.NoError(t, ctl.Get(ctx, node.Name, &updatedNode))
		require.True(t, updatedNode.IsMarkedForPendingDecommission(), "Pending decommission annotation should remain")
		require.Equal(t, crdbv1beta1.NodeDecommissionState(""), updatedNode.Status.Decommission, "Status should not be updated")
	})

	// Test case 4: Node with decommissioned status
	t.Run("decommissioned node gets deleted", func(t *testing.T) {
		node := &crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-node-4",
				Namespace: "test",
				Labels: map[string]string{
					manifests.BetaCrdbClusterNodeOwnerLabelKey: cluster.Name,
				},
			},
			Status: crdbv1beta1.CrdbNodeStatus{
				NodeID:       new(int64(4)),
				Decommission: crdbv1beta1.Decommissioned,
				Conditions: []crdbv1beta1.NodeCondition{
					{Type: crdbv1beta1.PodReady, Status: metav1.ConditionTrue}, // Now running and ready
					{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue},
				},
			},
		}

		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{
			NoWait: true,
		}, cluster, node))

		mockNodeExec := nodemock.NewMockNodeExecutor(mockCtrl)
		mockNodeExec.EXPECT().QueryRegionStatus(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		mockNodeExec.EXPECT().Version(gomock.Any()).Return("v23.1.0", nil).AnyTimes()
		mockNodeExec.EXPECT().UnderReplicatedRanges(gomock.Any()).Return(int64(0), nil).AnyTimes()
		r := &Reconciler{
			Ctl: ctl,
			Env: &cloud.Environment{},
			NodeExecutor: func(node *crdbv1beta1.CrdbNode, ctl kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
				return mockNodeExec
			},
		}

		_, err := r.ReconcileCluster(ctx, ctl, cluster)
		require.NoError(t, err)

		// Verify the decommissioned node is deleted
		var nodeAfter crdbv1beta1.CrdbNode
		err = ctl.Get(ctx, node.Name, &nodeAfter)
		require.Error(t, err)
		require.True(t, kubeerrors.IsNotFound(err), "Original decommissioned node should be deleted")
	})
}

func TestDetectCurrentVersion(t *testing.T) {
	ctx := context.Background()

	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "test-ns",
		},
	}

	testCases := []struct {
		name            string
		nodes           crdbv1beta1.CrdbNodeList
		mockSetup       func(*nodemock.MockNodeExecutor)
		expectedError   string
		expectedVersion string
	}{
		{
			name: "successful version detection",
			nodes: crdbv1beta1.CrdbNodeList{
				Items: []crdbv1beta1.CrdbNode{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
						Status: crdbv1beta1.CrdbNodeStatus{
							Conditions: []crdbv1beta1.NodeCondition{
								{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue},
							},
						},
					},
				},
			},
			mockSetup: func(mockExecutor *nodemock.MockNodeExecutor) {
				mockExecutor.EXPECT().Version(ctx).Return("v23.1.0", nil)
			},
			expectedVersion: "v23.1.0",
		},
		{
			name: "version with whitespace trimmed",
			nodes: crdbv1beta1.CrdbNodeList{
				Items: []crdbv1beta1.CrdbNode{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
						Status: crdbv1beta1.CrdbNodeStatus{
							Conditions: []crdbv1beta1.NodeCondition{
								{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue},
							},
						},
					},
				},
			},
			mockSetup: func(mockExecutor *nodemock.MockNodeExecutor) {
				mockExecutor.EXPECT().Version(ctx).Return("  v23.1.12  \n", nil)
			},
			expectedVersion: "v23.1.12",
		},
		{
			name: "version command execution error",
			nodes: crdbv1beta1.CrdbNodeList{
				Items: []crdbv1beta1.CrdbNode{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
						Status: crdbv1beta1.CrdbNodeStatus{
							Conditions: []crdbv1beta1.NodeCondition{
								{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue},
							},
						},
					},
				},
			},
			mockSetup: func(mockExecutor *nodemock.MockNodeExecutor) {
				mockExecutor.EXPECT().Version(ctx).Return("", errors.New("command execution failed"))
			},
			expectedError: "failed to execute version command on node node-1",
		},
		{
			name: "invalid version format",
			nodes: crdbv1beta1.CrdbNodeList{
				Items: []crdbv1beta1.CrdbNode{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
						Status: crdbv1beta1.CrdbNodeStatus{
							Conditions: []crdbv1beta1.NodeCondition{
								{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue},
							},
						},
					},
				},
			},
			mockSetup: func(mockExecutor *nodemock.MockNodeExecutor) {
				mockExecutor.EXPECT().Version(ctx).Return("invalid-version", nil)
			},
			expectedError: "failed to parse version: invalid-version",
		},
		{
			name: "empty version output",
			nodes: crdbv1beta1.CrdbNodeList{
				Items: []crdbv1beta1.CrdbNode{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
						Status: crdbv1beta1.CrdbNodeStatus{
							Conditions: []crdbv1beta1.NodeCondition{
								{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue},
							},
						},
					},
				},
			},
			mockSetup: func(mockExecutor *nodemock.MockNodeExecutor) {
				mockExecutor.EXPECT().Version(ctx).Return("", nil)
			},
			expectedError: "failed to parse version: ",
		},
		{
			name: "no running nodes available",
			nodes: crdbv1beta1.CrdbNodeList{
				Items: []crdbv1beta1.CrdbNode{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
						Status: crdbv1beta1.CrdbNodeStatus{
							Conditions: []crdbv1beta1.NodeCondition{
								{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionFalse},
							},
						},
					},
				},
			},
			mockSetup:     func(mockExecutor *nodemock.MockNodeExecutor) {}, // No expectations set
			expectedError: "no running nodes available to detect version",
		},
		{
			name:          "empty node list",
			nodes:         crdbv1beta1.CrdbNodeList{},
			mockSetup:     func(mockExecutor *nodemock.MockNodeExecutor) {}, // No expectations set
			expectedError: "no running nodes available to detect version",
		},
		{
			name: "multiple nodes - uses first running node",
			nodes: crdbv1beta1.CrdbNodeList{
				Items: []crdbv1beta1.CrdbNode{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
						Status: crdbv1beta1.CrdbNodeStatus{
							Conditions: []crdbv1beta1.NodeCondition{
								{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionFalse},
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
						Status: crdbv1beta1.CrdbNodeStatus{
							Conditions: []crdbv1beta1.NodeCondition{
								{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue},
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{Name: "node-3"},
						Status: crdbv1beta1.CrdbNodeStatus{
							Conditions: []crdbv1beta1.NodeCondition{
								{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue},
							},
						},
					},
				},
			},
			mockSetup: func(mockExecutor *nodemock.MockNodeExecutor) {
				mockExecutor.EXPECT().Version(ctx).Return("v23.2.0", nil)
			},
			expectedVersion: "v23.2.0",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create mock controller and mock node executor
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockExecutor := nodemock.NewMockNodeExecutor(ctrl)
			tc.mockSetup(mockExecutor)

			// Create reconciler with mocked NodeExecutor
			r := &Reconciler{
				Ctl: kubetest.NewFakeCtl(kubetest.Options{}),
				NodeExecutor: func(node *crdbv1beta1.CrdbNode, ctl kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
					return mockExecutor
				},
			}

			// Test the function
			version, err := r.detectCurrentVersion(ctx, cluster, tc.nodes)

			// Verify results
			if tc.expectedError != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.expectedError)
				require.Empty(t, version)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.expectedVersion, version)
			}
		})
	}
}

func TestReconcilerScaleDownHoldOffWiresRequeueAfter(t *testing.T) {
	ctx := context.Background()
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("test")

	mutableOnly := crdbv1beta1.MutableOnly
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "test",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east1", Nodes: 2, CloudProvider: "k3d"},
			},
			Mode:                &mutableOnly,
			RollingRestartDelay: &metav1.Duration{Duration: 30 * time.Second},
		},
		Status: crdbv1beta1.CrdbClusterStatus{
			Conditions: []crdbv1beta1.ClusterCondition{
				{Type: crdbv1beta1.ClusterInitialized, Status: metav1.ConditionTrue},
			},
			Region:   "us-east1",
			Provider: "k3d",
		},
	}

	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, cluster))

	// Create 3 running nodes so a scale-down to 2 is pending.
	for i := range 3 {
		nodeID := int64(i + 1)
		node := &crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("test-node-%d", i),
				Namespace: "test",
				Labels:    map[string]string{manifests.BetaCrdbClusterNodeOwnerLabelKey: cluster.Name},
			},
			Status: crdbv1beta1.CrdbNodeStatus{
				NodeID: &nodeID,
				Conditions: []crdbv1beta1.NodeCondition{
					{Type: crdbv1beta1.PodReady, Status: metav1.ConditionTrue},
					{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue},
				},
			},
		}
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, node))
	}

	mockNodeExec := nodemock.NewMockNodeExecutor(mockCtrl)
	mockNodeExec.EXPECT().QueryRegionStatus(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockNodeExec.EXPECT().Version(gomock.Any()).Return("v23.1.0", nil).AnyTimes()
	// Report under-replicated ranges so Scale holds off the decommission.
	mockNodeExec.EXPECT().UnderReplicatedRanges(gomock.Any()).Return(int64(5), nil).AnyTimes()

	r := &Reconciler{
		Ctl:            ctl,
		EvictionHelper: &evictionutil.Helper{},
		Env: &cloud.Environment{
			CloudProvider: "k3d",
			CloudRegion:   "us-east1",
		},
		NodeExecutor: func(*crdbv1beta1.CrdbNode, kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
			return mockNodeExec
		},
	}

	result, err := r.ReconcileCluster(ctx, ctl, cluster)
	require.NoError(t, err)

	// The reconciler should return a delayed requeue (from scaleHoldOff), not an
	// immediate requeue. This is the wiring we're testing — Scale returns
	// requeueAfter, the reconciler saves it, lets the rest of the reconcile run,
	// and applies it at the end.
	require.False(t, result.Requeue, "should not RequeueImmediately")
	require.Equal(t, 30*time.Second, result.RequeueAfter, "should RequeueAfter the rolling restart delay")

	// No node should be decommissioning.
	var nodes crdbv1beta1.CrdbNodeList
	require.NoError(t, ctl.List(ctx, &kube.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{
			manifests.BetaCrdbClusterNodeOwnerLabelKey: cluster.Name,
		}),
	}, &nodes))
	require.Len(t, nodes.Items, 3)
	for _, node := range nodes.Items {
		require.False(t, node.IsDecommissioning(), "node %s should not be decommissioning while under-replicated", node.Name)
	}

	// The condition should be persisted (UpdateStatus ran at end of reconcile).
	var updatedCluster crdbv1beta1.CrdbCluster
	require.NoError(t, ctl.Get(ctx, cluster.Name, &updatedCluster))
	require.True(t, updatedCluster.Status.HasCondition(crdbv1beta1.ClusterRangesUnderReplicated, metav1.ConditionTrue))
}

// TestReconcileClusterInitialization drives full reconciles through the bootstrap
// paths, checking the init block is reachable and that each path returns the
// requeue the retry loop needs.
func TestReconcileClusterInitialization(t *testing.T) {
	const namespace = "test"
	const nodeName = "test-cluster-0"

	uninitializedCluster := func(t *testing.T, ctx context.Context) (*kube.Ctl, *crdbv1beta1.CrdbCluster) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace(namespace)
		mutableOnly := crdbv1beta1.MutableOnly
		cluster := &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: namespace},
			Spec: crdbv1beta1.CrdbClusterSpec{
				// The region namespace matches the cluster's, so Initialize skips the
				// cross-region probe and gets as far as cockroach init.
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{Code: "us-east1", Namespace: namespace, Nodes: 1, CloudProvider: "k3d"},
				},
				Mode:                &mutableOnly,
				RollingRestartDelay: &metav1.Duration{Duration: 10 * time.Second},
			},
		}
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, cluster))
		require.NoError(t, ctl.Get(ctx, cluster.Name, cluster))
		return ctl, cluster
	}

	// The node is on the cluster's current revision so RollingUpdate leaves it
	// alone and the reconcile carries on to the init block.
	createNode := func(
		t *testing.T,
		ctx context.Context,
		ctl *kube.Ctl,
		cluster *crdbv1beta1.CrdbCluster,
		running metav1.ConditionStatus,
	) {
		node := upToDateNodeFor(cluster, nodeName)
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, node))
		require.NoError(t, ctl.Get(ctx, node.Name, node))

		node.Status.ObservedGeneration = node.Generation
		node.Status.SetCondition(crdbv1beta1.PodRunning, running)
		// Nothing is Ready before the cluster is bootstrapped.
		node.Status.SetCondition(crdbv1beta1.PodReady, metav1.ConditionFalse)
		require.NoError(t, ctl.UpdateStatus(ctx, node))
	}

	newReconciler := func(ctl *kube.Ctl, exec kubecrdbnode.NodeExecutor) (*Reconciler, *record.FakeRecorder) {
		recorder := record.NewFakeRecorder(10)
		return &Reconciler{
			Ctl:            ctl,
			EvictionHelper: &evictionutil.Helper{},
			Emit:           kube.EventEmitter{EventRecorder: recorder},
			Env:            &cloud.Environment{CloudProvider: "k3d", CloudRegion: "us-east1"},
			NodeExecutor: func(*crdbv1beta1.CrdbNode, kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
				return exec
			},
		}, recorder
	}

	newExecutor := func(mockCtrl *gomock.Controller) *nodemock.MockNodeExecutor {
		exec := nodemock.NewMockNodeExecutor(mockCtrl)
		exec.EXPECT().UnderReplicatedRanges(gomock.Any()).Return(int64(0), nil).AnyTimes()
		exec.EXPECT().Version(gomock.Any()).Return("v24.1.0", nil).AnyTimes()
		return exec
	}

	t.Run("pins a node before initializing and clears the pin on success", func(t *testing.T) {
		ctx := context.Background()
		mockCtrl := gomock.NewController(t)
		defer mockCtrl.Finish()

		ctl, cluster := uninitializedCluster(t, ctx)
		createNode(t, ctx, ctl, cluster, metav1.ConditionTrue)

		exec := newExecutor(mockCtrl)
		exec.EXPECT().Init(gomock.Any()).Return("", "", nil).Times(1)
		r, _ := newReconciler(ctl, exec)

		result, err := r.ReconcileCluster(ctx, ctl, cluster)
		require.NoError(t, err)
		require.True(t, result.IsZero())

		var initialized crdbv1beta1.CrdbCluster
		require.NoError(t, ctl.Get(ctx, cluster.Name, &initialized))
		require.True(t, initialized.IsInitialized())
		require.Empty(t, initialized.Status.InitializationNode, "a bootstrapped cluster must not keep the pin")
	})

	t.Run("waits on a pinned node that stops running", func(t *testing.T) {
		ctx := context.Background()
		mockCtrl := gomock.NewController(t)
		defer mockCtrl.Finish()

		ctl, cluster := uninitializedCluster(t, ctx)
		createNode(t, ctx, ctl, cluster, metav1.ConditionFalse)
		cluster.Status.InitializationNode = nodeName
		require.NoError(t, ctl.UpdateStatus(ctx, cluster))

		exec := newExecutor(mockCtrl)
		// We wait on the pin rather than bootstrapping somewhere else.
		exec.EXPECT().Init(gomock.Any()).Times(0)
		r, recorder := newReconciler(ctl, exec)

		result, err := r.ReconcileCluster(ctx, ctl, cluster)
		require.NoError(t, err)
		require.True(t, result.IsZero(), "waiting on the pinned node requeues on the dependent, not on a timer")

		var stalled crdbv1beta1.CrdbCluster
		require.NoError(t, ctl.Get(ctx, cluster.Name, &stalled))
		require.Equal(t, nodeName, stalled.Status.InitializationNode)
		condition, ok := stalled.Status.FindCondition(crdbv1beta1.ClusterInitialized)
		require.True(t, ok)
		require.Equal(t, metav1.ConditionFalse, condition.Status)
		require.Equal(t, initializationNodeUnavailableReason, condition.Reason)

		select {
		case event := <-recorder.Events:
			require.Contains(t, event, initializationNodeUnavailableReason)
		default:
			t.Fatal("a stalled initialization did not emit a warning event")
		}
	})

	t.Run("requeues after a fixed delay when init fails", func(t *testing.T) {
		ctx := context.Background()
		mockCtrl := gomock.NewController(t)
		defer mockCtrl.Finish()

		ctl, cluster := uninitializedCluster(t, ctx)
		createNode(t, ctx, ctl, cluster, metav1.ConditionTrue)

		exec := newExecutor(mockCtrl)
		exec.EXPECT().Init(gomock.Any()).Return("", "", errors.New("boom")).Times(1)
		r, _ := newReconciler(ctl, exec)

		result, err := r.ReconcileCluster(ctx, ctl, cluster)
		// A failed init must not surface as a reconcile error. That would swap this
		// fixed delay for controller-runtime's rate limiter.
		require.NoError(t, err)
		require.Equal(t, clusterInitializationRetryDelay, result.RequeueAfter)

		var failed crdbv1beta1.CrdbCluster
		require.NoError(t, ctl.Get(ctx, cluster.Name, &failed))
		// The pin is written and read back from the API server, so the node that may
		// have already bootstrapped is the one the next attempt uses.
		require.Equal(t, nodeName, failed.Status.InitializationNode, "the pin must survive a failed attempt")
		require.Equal(t, "us-east1", failed.Status.Region)
		require.Equal(t, "k3d", failed.Status.Provider)
		require.True(t, failed.Status.ReconciledByBetaController)
		require.True(t, failed.Status.Reconciled)
		condition, ok := failed.Status.FindCondition(crdbv1beta1.ClusterInitialized)
		require.True(t, ok)
		require.Equal(t, metav1.ConditionFalse, condition.Status)
		require.Equal(t, initializationFailedReason, condition.Reason)
	})
}

func TestEnsureOwnerReference(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("default")

	r := NewReconciler(nil, ctl, &evictionutil.Helper{})

	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
			UID:       "cluster-uid",
		},
	}

	// Create a node with an incorrect API version in the owner ref
	node := crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-node",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					// Incorrect version
					APIVersion: "crdb.cockroachlabs.com/v1beta1",
					Kind:       "CrdbCluster",
					Name:       "test-cluster",
					UID:        "cluster-uid",
					Controller: func() *bool { b := true; return &b }(),
				},
			},
		},
	}

	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, cluster))
	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, &node))

	nodes := crdbv1beta1.CrdbNodeList{Items: []crdbv1beta1.CrdbNode{node}}
	_, err := r.ensureOwnerReference(ctx, cluster, nodes)
	require.NoError(t, err)

	var updatedNode crdbv1beta1.CrdbNode
	require.NoError(t, ctl.Get(ctx, node.Name, &updatedNode))

	controllerRef := metav1.GetControllerOf(&updatedNode)
	require.NotNil(t, controllerRef)
	expectedAPIVersion := crdbv1beta1.GroupVersion.String()
	require.Equal(t, expectedAPIVersion, controllerRef.APIVersion)
}

// TestDeployDoesNotOwnPriorityClass asserts deploy() clears owner refs on
// cluster-scoped PriorityClasses while still stamping the other objects it
// applies.
func TestDeployDoesNotOwnPriorityClass(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Mapper: &kubetest.FakeRESTMapper{}},
	}).WithNamespace("test-ns")
	// A ctl with no bound namespace so we can address cluster-scoped objects.
	rootCtl := ctl.WithNamespace("")

	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "test-ns",
			UID:       "cluster-uid",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east1", Nodes: 1, CloudProvider: "k3d"},
			},
		},
		Status: crdbv1beta1.CrdbClusterStatus{Region: "us-east1"},
	}

	// Seed the operator-managed PriorityClasses with a stale owner ref from
	// a different CrdbCluster (what a pre-fix operator would have left).
	// Reuse manifests.CrdbClusterPriorityClasses so owner metadata is the
	// only deliberate difference from the operator's own shape.
	staleOwner := metav1.OwnerReference{
		APIVersion: crdbv1beta1.GroupVersion.String(),
		Kind:       crdbv1beta1.CrdbClusterKind,
		Name:       "some-other-cluster",
		UID:        "other-cluster-uid",
		Controller: new(true),
	}
	for _, pc := range manifests.CrdbClusterPriorityClasses() {
		pc.SetOwnerReferences([]metav1.OwnerReference{staleOwner})
		require.NoError(t, rootCtl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, pc))
	}

	r := &Reconciler{Ctl: ctl}

	deployObjects := append([]kube.Object{
		manifests.NodeService(cluster),
		manifests.JoinService(cluster),
	}, manifests.CrdbClusterPriorityClasses()...)

	// includePDB=true so deploy() also appends the CrdbClusterOperatorPDB.
	require.NoError(t, r.deploy(ctx, ctl, cluster, deployObjects, true))

	for _, name := range []string{manifests.CRDBHigh, manifests.CRDBCritical} {
		var pc schedulingv1.PriorityClass
		require.NoError(t, rootCtl.Get(ctx, name, &pc))
		require.Empty(t, pc.OwnerReferences,
			"cluster-scoped PriorityClass %q must not carry ownerReferences to namespaced kinds", name)
	}

	// Namespaced resources still get the CrdbCluster as controller-owner.
	assertControllerOwner := func(name string, obj kube.Object, kind string) {
		t.Helper()
		require.NoError(t, ctl.Get(ctx, name, obj))
		owner := metav1.GetControllerOf(obj)
		require.NotNil(t, owner, "%s should have a controller-owner", kind)
		require.Equal(t, cluster.Name, owner.Name)
		require.Equal(t, cluster.UID, owner.UID)
	}
	assertControllerOwner(manifests.NodeService(cluster).Name, &corev1.Service{}, "Service")
	assertControllerOwner(cluster.Name+"-pdb", &policyv1.PodDisruptionBudget{}, "PodDisruptionBudget")
}

func TestHandleDecommissionedNodeDoesNotBusyLoopWhileDeleting(t *testing.T) {
	ctx := context.Background()
	deleteCalls := 0
	node := &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cockroachdb-decommissioned",
			Namespace:  "default",
			Finalizers: []string{manifests.CrdbNodeFinalizerName},
		},
	}
	ctl := kubetest.NewFakeCtl(kubetest.Options{Options: kube.Options{
		BeforeMutation: func(object kube.Object) error {
			if _, ok := object.(*crdbv1beta1.CrdbNode); ok {
				deleteCalls++
			}
			return nil
		},
	}}, node).WithNamespace(node.Namespace)
	r := &Reconciler{Ctl: ctl}

	result, err := r.handleDecommissionedNode(ctx, node)
	require.NoError(t, err)
	require.True(t, result.IsZero())
	require.Equal(t, 1, deleteCalls)

	now := metav1.Now()
	deletingNode := node.DeepCopy()
	deletingNode.DeletionTimestamp = &now

	result, err = r.handleDecommissionedNode(ctx, deletingNode)
	require.NoError(t, err)
	require.True(t, result.IsZero())
	require.Equal(t, 1, deleteCalls, "a terminating CrdbNode must not be deleted repeatedly")

	deletingNode.Annotations = map[string]string{
		crdbv1beta1.CrdbNodeStartPendingDecommissionAnnotation: "true",
	}
	deletingNode.Status.SetCondition(crdbv1beta1.PodRunning, metav1.ConditionFalse)
	result, err = r.handlePendingDecommissionNode(ctx, deletingNode)
	require.NoError(t, err)
	require.True(t, result.IsZero())
	require.Equal(t, 1, deleteCalls, "a terminating pending-decommission CrdbNode must not be deleted repeatedly")
}

func TestTlsConfigForCluster(t *testing.T) {
	ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("default")

	t.Run("missing NodeCAConfigMapName and CAConfigMapName", func(t *testing.T) {
		cluster := &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec: crdbv1beta1.CrdbClusterSpec{
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						Certificates: crdbv1beta1.Certificates{
							ExternalCertificates: &crdbv1beta1.ExternalCertificates{
								RootSQLClientSecretName: "root-client",
							},
						},
					},
				},
			},
		}
		_, err := tlsConfigForCluster(ctl, cluster)
		require.Error(t, err)
		require.Contains(t, err.Error(), "missing CA config map name")
	})

	t.Run("missing RootSQLClientSecretName", func(t *testing.T) {
		cluster := &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec: crdbv1beta1.CrdbClusterSpec{
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						Certificates: crdbv1beta1.Certificates{
							ExternalCertificates: &crdbv1beta1.ExternalCertificates{
								NodeCAConfigMapName: "node-ca",
							},
						},
					},
				},
			},
		}
		_, err := tlsConfigForCluster(ctl, cluster)
		require.Error(t, err)
		require.Contains(t, err.Error(), "missing root SQL client secret name")
	})

	t.Run("CAConfigMapName fallback", func(t *testing.T) {
		cluster := &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec: crdbv1beta1.CrdbClusterSpec{
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						Certificates: crdbv1beta1.Certificates{
							ExternalCertificates: &crdbv1beta1.ExternalCertificates{
								CAConfigMapName:         "fallback-ca",
								RootSQLClientSecretName: "root-client",
							},
						},
					},
				},
			},
		}
		// The error will be about fetching the bundle (secret not found),
		// not about missing CA config map name, proving the fallback worked.
		_, err := tlsConfigForCluster(ctl, cluster)
		require.Error(t, err)
		require.Contains(t, err.Error(), "fetching cert bundle")
	})
}

func TestCleanMigrationAnnotations(t *testing.T) {
	t.Run("removes operator-injected annotations", func(t *testing.T) {
		cluster := &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"crdb.cockroachlabs.com/mode":              "Disabled",
					"crdb.cockroachlabs.com/v1beta1-spec":      "{}",
					"crdb.cockroachlabs.com/v1alpha1-fields":   "{}",
					"crdb.cockroachlabs.com/migration-status":  "{}",
					"crdb.cockroachlabs.com/regions":           "[]",
					"crdb.cockroachlabs.com/clusterSettings":   "{}",
					"clusterStatus":                            "Running",
					"meta.helm.sh/release-name":                "cockroachdb",
					"crdb.cockroachlabs.com/regionCode":        "us-east1",
					"crdb.cockroachlabs.com/cloudProvider":     "gcp",
					"crdb.cockroachlabs.com/migration-regions": "[{}]",
				},
			},
		}

		cleaned := cleanMigrationAnnotations(cluster)
		require.True(t, cleaned)

		// Operator-injected annotations are removed.
		require.NotContains(t, cluster.Annotations, "crdb.cockroachlabs.com/mode")
		require.NotContains(t, cluster.Annotations, "crdb.cockroachlabs.com/v1beta1-spec")
		require.NotContains(t, cluster.Annotations, "crdb.cockroachlabs.com/v1alpha1-fields")
		require.NotContains(t, cluster.Annotations, "crdb.cockroachlabs.com/migration-status")
		require.NotContains(t, cluster.Annotations, "clusterStatus")

		// User-applied annotations are preserved.
		require.Contains(t, cluster.Annotations, "crdb.cockroachlabs.com/regionCode")
		require.Contains(t, cluster.Annotations, "crdb.cockroachlabs.com/cloudProvider")
		require.Contains(t, cluster.Annotations, "crdb.cockroachlabs.com/migration-regions")

		// Non-migration annotations are untouched.
		require.Equal(t, "cockroachdb", cluster.Annotations["meta.helm.sh/release-name"])
	})

	t.Run("returns false when no annotations present", func(t *testing.T) {
		cluster := &crdbv1beta1.CrdbCluster{}
		require.False(t, cleanMigrationAnnotations(cluster))
	})

	t.Run("returns false when only non-migration annotations present", func(t *testing.T) {
		cluster := &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"meta.helm.sh/release-name": "cockroachdb",
				},
			},
		}
		require.False(t, cleanMigrationAnnotations(cluster))
	})

	t.Run("idempotent on second call", func(t *testing.T) {
		cluster := &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"crdb.cockroachlabs.com/mode": "Disabled",
				},
			},
		}
		require.True(t, cleanMigrationAnnotations(cluster))
		require.False(t, cleanMigrationAnnotations(cluster))
	})
}

func TestMigrationHandoffComplete(t *testing.T) {
	mode := func(m crdbv1beta1.ReconciliationMode) *crdbv1beta1.ReconciliationMode { return &m }
	migration := func(phase string) *crdbv1beta1.MigrationStatus {
		return &crdbv1beta1.MigrationStatus{Phase: phase}
	}
	completeLabels := map[string]string{
		migrationctrl.LabelMigrate:         migrationctrl.MigrateComplete,
		migrationctrl.LabelMigrationStatus: migrationctrl.MigrateComplete,
	}

	tests := []struct {
		name   string
		mode   *crdbv1beta1.ReconciliationMode
		status *crdbv1beta1.MigrationStatus
		labels map[string]string
		want   bool
	}{
		{name: "all handoff markers complete", mode: mode(crdbv1beta1.MutableOnly), status: migration(migrationctrl.PhaseComplete), labels: completeLabels, want: true},
		{name: "migrate label still in progress", mode: mode(crdbv1beta1.MutableOnly), status: migration(migrationctrl.PhaseComplete), labels: map[string]string{migrationctrl.LabelMigrate: migrationctrl.MigrateInProgress}, want: false},
		{name: "internal status label is not required", mode: mode(crdbv1beta1.MutableOnly), status: migration(migrationctrl.PhaseComplete), labels: map[string]string{migrationctrl.LabelMigrate: migrationctrl.MigrateComplete}, want: true},
		{name: "finalization crash window", mode: mode(crdbv1beta1.MutableOnly), status: migration(migrationctrl.PhaseFinalization), labels: completeLabels, want: false},
		{name: "no migration status", mode: mode(crdbv1beta1.MutableOnly), status: nil, labels: completeLabels, want: false},
		{name: "Disabled with completion markers", mode: mode(crdbv1beta1.Disabled), status: migration(migrationctrl.PhaseComplete), labels: completeLabels, want: false},
		{name: "nil mode", mode: nil, status: migration(migrationctrl.PhaseComplete), labels: completeLabels, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := &crdbv1beta1.CrdbCluster{}
			cluster.Spec.Mode = tt.mode
			cluster.Status.Migration = tt.status
			cluster.Labels = tt.labels
			require.Equal(t, tt.want, MigrationHandoffComplete(cluster))
		})
	}
}

func TestReconcileClusterWaitsForMigrationHandoff(t *testing.T) {
	mode := crdbv1beta1.MutableOnly
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "migration-test",
			Namespace: "default",
			Labels: map[string]string{
				migrationctrl.LabelMigrate:         migrationctrl.MigrateInProgress,
				migrationctrl.LabelMigrationStatus: migrationctrl.MigrateInProgress,
			},
			Annotations: map[string]string{
				"crdb.cockroachlabs.com/mode": string(mode),
			},
		},
		Spec: crdbv1beta1.CrdbClusterSpec{Mode: &mode},
		Status: crdbv1beta1.CrdbClusterStatus{
			Migration: &crdbv1beta1.MigrationStatus{Phase: migrationctrl.PhaseComplete},
		},
	}
	reconciler := &Reconciler{
		Env: &cloud.Environment{CloudRegion: "us-east1"},
	}
	ctl := kubetest.NewFakeCtl(kubetest.Options{}, cluster.DeepCopy()).WithNamespace(cluster.Namespace)

	result, err := reconciler.ReconcileCluster(context.Background(), ctl, cluster)
	require.NoError(t, err)
	require.False(t, result.Requeue)
	require.Zero(t, result.RequeueAfter)
	require.Contains(t, cluster.Annotations, "crdb.cockroachlabs.com/mode",
		"migration annotations must remain until the terminal migrate label lands")
	require.Zero(t, cluster.Status.ObservedGeneration,
		"betaclusterctrl must not write status during the completion handoff")
}

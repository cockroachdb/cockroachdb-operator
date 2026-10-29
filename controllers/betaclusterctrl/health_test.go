package betaclusterctrl

import (
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode/nodemock"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	gomock "github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// stubNodeExecutor reports a fixed under-replicated range count and leaves every
// other NodeExecutor method unimplemented. The embedded nil interface panics if
// anything else is called, which keeps tests honest about what they exercise.
type stubNodeExecutor struct {
	kubecrdbnode.NodeExecutor
	count int64
}

func (s stubNodeExecutor) UnderReplicatedRanges(context.Context) (int64, error) {
	return s.count, nil
}

// StubNodeExecutorFactory returns a NodeExecutor factory for tests that drive
// RollingUpdate or Scale and so now pass through the under-replicated gate. A
// count of 0 keeps the cluster healthy so the gate is a no-op.
func StubNodeExecutorFactory(
	count int64,
) func(*crdbv1beta1.CrdbNode, kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
	return func(*crdbv1beta1.CrdbNode, kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
		return stubNodeExecutor{count: count}
	}
}

func readyNode(name string) crdbv1beta1.CrdbNode {
	return crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test"},
		Status: crdbv1beta1.CrdbNodeStatus{
			Conditions: []crdbv1beta1.NodeCondition{
				{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue},
				{Type: crdbv1beta1.PodReady, Status: metav1.ConditionTrue},
			},
		},
	}
}

func runningNode(name string) crdbv1beta1.CrdbNode {
	return crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test"},
		Status: crdbv1beta1.CrdbNodeStatus{
			Conditions: []crdbv1beta1.NodeCondition{
				{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue},
				{Type: crdbv1beta1.PodReady, Status: metav1.ConditionFalse},
			},
		},
	}
}

func TestUnderReplicatedBlocks(t *testing.T) {
	ctx := context.Background()
	withReadyNode := crdbv1beta1.CrdbNodeList{Items: []crdbv1beta1.CrdbNode{readyNode("n1")}}
	withRunningNode := crdbv1beta1.CrdbNodeList{Items: []crdbv1beta1.CrdbNode{runningNode("n1")}}

	for _, tc := range []struct {
		name string
		// featureSet enables the skip feature flag on the cluster.
		featureSet    bool
		singleNode    bool
		uninitialized bool
		nodes         crdbv1beta1.CrdbNodeList
		// mockSetup records the expected executor call, or is nil when the check
		// should never query.
		mockSetup     func(*nodemock.MockNodeExecutor)
		wantBlocked   bool
		checkCond     bool
		wantCondition metav1.ConditionStatus
		wantReason    string
	}{
		{
			name:          "feature flag bypasses the check and records CheckSkipped",
			featureSet:    true,
			nodes:         withReadyNode,
			checkCond:     true,
			wantCondition: metav1.ConditionFalse,
			wantReason:    "CheckSkipped",
		},
		{
			name:          "uninitialized cluster skips the check",
			uninitialized: true,
			nodes:         withReadyNode,
		},
		{
			name:  "running but unready node does not block",
			nodes: withRunningNode,
		},
		{
			name:          "single-node cluster bypasses the check and records CheckSkipped",
			singleNode:    true,
			nodes:         withReadyNode,
			checkCond:     true,
			wantCondition: metav1.ConditionFalse,
			wantReason:    "CheckSkipped",
		},
		{
			name:  "under-replicated ranges block and set UnderReplicated",
			nodes: withReadyNode,
			mockSetup: func(m *nodemock.MockNodeExecutor) {
				m.EXPECT().UnderReplicatedRanges(gomock.Any()).Return(int64(4), nil)
			},
			wantBlocked:   true,
			checkCond:     true,
			wantCondition: metav1.ConditionTrue,
			wantReason:    "UnderReplicated",
		},
		{
			name:  "zero under-replicated ranges clear with AllReplicated",
			nodes: withReadyNode,
			mockSetup: func(m *nodemock.MockNodeExecutor) {
				m.EXPECT().UnderReplicatedRanges(gomock.Any()).Return(int64(0), nil)
			},
			checkCond:     true,
			wantCondition: metav1.ConditionFalse,
			wantReason:    "AllReplicated",
		},
		{
			name:  "query error blocks and sets CheckError",
			nodes: withReadyNode,
			mockSetup: func(m *nodemock.MockNodeExecutor) {
				m.EXPECT().UnderReplicatedRanges(gomock.Any()).Return(int64(0), errors.New("boom"))
			},
			wantBlocked:   true,
			checkCond:     true,
			wantCondition: metav1.ConditionTrue,
			wantReason:    "CheckError",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockExec := nodemock.NewMockNodeExecutor(ctrl)
			if tc.mockSetup != nil {
				tc.mockSetup(mockExec)
			}

			cluster := &crdbv1beta1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "test"},
				Spec: crdbv1beta1.CrdbClusterSpec{
					Regions: []crdbv1beta1.CrdbClusterRegion{{Nodes: 3}},
				},
			}
			if !tc.uninitialized {
				cluster.Status.SetCondition(crdbv1beta1.ClusterInitialized, metav1.ConditionTrue)
			}
			if tc.featureSet {
				cluster.Spec.Features = []crdbv1beta1.ClusterFeature{crdbv1beta1.ClusterSkipUnderReplicatedRangesCheck}
			}
			if tc.singleNode {
				cluster.Spec.Regions[0].Nodes = 1
			}

			r := &Reconciler{
				Ctl: kubetest.NewFakeCtl(kubetest.Options{}),
				NodeExecutor: func(*crdbv1beta1.CrdbNode, kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
					return mockExec
				},
			}

			blocked := r.underReplicatedBlocks(ctx, cluster, tc.nodes)
			require.Equal(t, tc.wantBlocked, blocked)
			if tc.checkCond {
				require.True(t, cluster.Status.HasCondition(crdbv1beta1.ClusterRangesUnderReplicated, tc.wantCondition))
				if tc.wantReason != "" {
					cond, ok := cluster.Status.FindCondition(crdbv1beta1.ClusterRangesUnderReplicated)
					require.True(t, ok)
					require.Equal(t, tc.wantReason, cond.Reason)
				}
			}
		})
	}
}

// TestUnderReplicatedConditionReasonOverwrite guards against the stale-reason bug:
// after the skip flag records CheckSkipped, removing the flag and running a real
// check must overwrite the reason even though the status stays False.
func TestUnderReplicatedConditionReasonOverwrite(t *testing.T) {
	ctx := context.Background()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockExec := nodemock.NewMockNodeExecutor(ctrl)
	mockExec.EXPECT().UnderReplicatedRanges(gomock.Any()).Return(int64(0), nil)

	cluster := &crdbv1beta1.CrdbCluster{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "test"}}
	cluster.Status.SetCondition(crdbv1beta1.ClusterInitialized, metav1.ConditionTrue)
	r := &Reconciler{
		Ctl: kubetest.NewFakeCtl(kubetest.Options{}),
		NodeExecutor: func(*crdbv1beta1.CrdbNode, kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
			return mockExec
		},
	}
	nodes := crdbv1beta1.CrdbNodeList{Items: []crdbv1beta1.CrdbNode{readyNode("n1")}}

	// First pass with the skip flag set records CheckSkipped, status False.
	cluster.Spec.Features = []crdbv1beta1.ClusterFeature{crdbv1beta1.ClusterSkipUnderReplicatedRangesCheck}
	blocked := r.underReplicatedBlocks(ctx, cluster, nodes)
	require.False(t, blocked)
	cond, ok := cluster.Status.FindCondition(crdbv1beta1.ClusterRangesUnderReplicated)
	require.True(t, ok)
	require.Equal(t, "CheckSkipped", cond.Reason)

	// Removing the flag and running the real check keeps the status False but must
	// replace the reason, not leave the stale CheckSkipped note.
	cluster.Spec.Features = nil
	blocked = r.underReplicatedBlocks(ctx, cluster, nodes)
	require.False(t, blocked)
	cond, ok = cluster.Status.FindCondition(crdbv1beta1.ClusterRangesUnderReplicated)
	require.True(t, ok)
	require.Equal(t, "AllReplicated", cond.Reason)
}

func TestRollingRestartDelay(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay *metav1.Duration
		want  time.Duration
	}{
		{name: "unset falls back to default", delay: nil, want: defaultRollingRestartDelay},
		{name: "zero falls back to default", delay: &metav1.Duration{Duration: 0}, want: defaultRollingRestartDelay},
		{name: "negative falls back to default", delay: &metav1.Duration{Duration: -5 * time.Second}, want: defaultRollingRestartDelay},
		{name: "positive value is used", delay: &metav1.Duration{Duration: 90 * time.Second}, want: 90 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := &crdbv1beta1.CrdbCluster{Spec: crdbv1beta1.CrdbClusterSpec{RollingRestartDelay: tc.delay}}
			require.Equal(t, tc.want, rollingRestartDelay(cluster))
		})
	}
}

func TestRollPending(t *testing.T) {
	cluster := &crdbv1beta1.CrdbCluster{}
	cluster.Status.CurrentRevision = "rev1"

	onRevision := crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "n1",
			Annotations: map[string]string{manifests.BetaCrdbClusterNodeRevisionAnnotationKey: "rev1"},
		},
	}

	t.Run("no pending work", func(t *testing.T) {
		nodes := crdbv1beta1.CrdbNodeList{Items: []crdbv1beta1.CrdbNode{*onRevision.DeepCopy()}}
		require.False(t, rollPending(cluster, nodes, &NodePool{}))
	})

	t.Run("node on a different revision is pending", func(t *testing.T) {
		stale := *onRevision.DeepCopy()
		stale.Annotations[manifests.BetaCrdbClusterNodeRevisionAnnotationKey] = "rev0"
		nodes := crdbv1beta1.CrdbNodeList{Items: []crdbv1beta1.CrdbNode{stale}}
		require.True(t, rollPending(cluster, nodes, &NodePool{}))
	})

	t.Run("node with PodNeedsUpdate on current revision is not pending", func(t *testing.T) {
		// PodNeedsUpdate means a pod update is already in flight. The
		// PodNeedsUpdate wait in RollingUpdate handles it without a new eviction,
		// so rollPending should not trigger the SQL query for this case.
		needsUpdate := *onRevision.DeepCopy()
		needsUpdate.Status.Conditions = []crdbv1beta1.NodeCondition{
			{Type: crdbv1beta1.PodNeedsUpdate, Status: metav1.ConditionTrue},
		}
		nodes := crdbv1beta1.CrdbNodeList{Items: []crdbv1beta1.CrdbNode{needsUpdate}}
		require.False(t, rollPending(cluster, nodes, &NodePool{}))
	})
}

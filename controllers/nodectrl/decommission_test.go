package nodectrl_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	. "github.com/cockroachlabs/cockroach-enterprise-operator/controllers/nodectrl"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode/nodemock"
	"github.com/cockroachlabs/version"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Test base decommission logic flow.
func TestDecommissionLegacy(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	ctx := context.Background()
	exec := nodemock.NewMockNodeExecutor(mockCtrl)
	node := &crdbv1beta1.CrdbNode{ObjectMeta: metav1.ObjectMeta{
		Name:        "test-node",
		Namespace:   "test",
		Annotations: map[string]string{},
	}}

	node.Spec.Image = "us-docker.pkg.dev/cockroach-cloud-images/cockroachdb/cockroach:v21.2.4"
	nodeID := uint(1)

	r := NewReconciler(kubetest.NewFakeCtl(kubetest.Options{}, node), nil)
	r.NodeExecutor = func(node *crdbv1beta1.CrdbNode, _ kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
		return exec
	}

	t.Run("error fetching node id", func(t *testing.T) {
		delete(node.Annotations, crdbv1beta1.CrdbNodeStartDecommissionAnnotation)
		err := r.Decommission(ctx, node)
		require.ErrorContains(t, err, fmt.Sprintf("annotation %s not set", crdbv1beta1.CrdbNodeStartDecommissionAnnotation))

		node.Annotations[crdbv1beta1.CrdbNodeStartDecommissionAnnotation] = "start"
		err = r.Decommission(ctx, node)
		require.ErrorContains(t, err, "parsing annotation")
	})

	node.Annotations[crdbv1beta1.CrdbNodeStartDecommissionAnnotation] = fmt.Sprintf("%d", nodeID)
	t.Run("starts decommissioning when not yet started", func(t *testing.T) {
		// error decommissioning node
		exec.EXPECT().Decommission(ctx, nodeID).Return(errors.New("boom"))
		err := r.Decommission(ctx, node)
		require.EqualError(t, errors.Cause(err), "boom")
		require.Empty(t, node.Status.Decommission)

		// successful decommission call
		exec.EXPECT().Decommission(ctx, nodeID).Return(nil)
		err = r.Decommission(ctx, node)
		require.NoError(t, err)
		require.Equal(t, crdbv1beta1.TransferringReplicas, node.Status.Decommission)
	})

	t.Run("polls replicas while decommissioning", func(t *testing.T) {
		node.Status.Decommission = crdbv1beta1.TransferringReplicas

		// error fetching replicas
		exec.EXPECT().ReplicasDeprecated(ctx, nodeID).Return(uint64(0), errors.New("boom"))
		err := r.Decommission(ctx, node)
		require.EqualError(t, errors.Cause(err), "boom")
		require.Equal(t, crdbv1beta1.TransferringReplicas, node.Status.Decommission)

		// replicas still > 0
		exec.EXPECT().ReplicasDeprecated(ctx, nodeID).Return(uint64(1), nil)
		err = r.Decommission(ctx, node)
		require.True(t, errors.Is(err, ErrNodeStillDecommissioning))
		require.Equal(t, crdbv1beta1.TransferringReplicas, node.Status.Decommission)

		// replicas == 0, finished decommissioning
		exec.EXPECT().ReplicasDeprecated(ctx, nodeID).Return(uint64(0), nil)
		err = r.Decommission(ctx, node)
		require.NoError(t, err)
		require.Equal(t, crdbv1beta1.ZeroReplicas, node.Status.Decommission)
	})

	t.Run("finalizes decommission on the node at the end", func(t *testing.T) {
		node.Status.Decommission = crdbv1beta1.ZeroReplicas

		// error finalizing
		exec.EXPECT().FinalizeDecommissionDeprecated(ctx, nodeID).Return(errors.New("boom"))
		err := r.Decommission(ctx, node)
		require.EqualError(t, errors.Cause(err), "boom")
		require.Equal(t, crdbv1beta1.ZeroReplicas, node.Status.Decommission)

		// successful finalization
		exec.EXPECT().FinalizeDecommissionDeprecated(ctx, nodeID).Return(nil)
		err = r.Decommission(ctx, node)
		require.NoError(t, err)
		require.Equal(t, crdbv1beta1.Decommissioned, node.Status.Decommission)

		// no-op when already finalized
		err = r.Decommission(ctx, node)
		require.NoError(t, err)
	})
}

// Test base decommission logic flow.
func TestDecommission(t *testing.T) {
	testCases := []struct {
		description string
		features    []crdbv1beta1.ClusterFeature
	}{
		{
			description: "draining not skipped",
			features:    []crdbv1beta1.ClusterFeature{},
		},
		{
			description: "draining skipped",
			features: []crdbv1beta1.ClusterFeature{
				crdbv1beta1.ClusterDrainAfterDecommission,
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			mockCtrl := gomock.NewController(t)
			ctx := context.Background()
			exec := nodemock.NewMockNodeExecutor(mockCtrl)

			cluster := &crdbv1beta1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cluster",
					Namespace: "test",
				},
				Spec: crdbv1beta1.CrdbClusterSpec{
					Features: testCase.features,
				},
			}

			// This version contains the virtual cluster draining fix
			image := "us-docker.pkg.dev/cockroach-cloud-images/cockroachdb/cockroach:v25.2.8"
			node := &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-node-1",
					Namespace:   "test",
					Annotations: map[string]string{},
					OwnerReferences: []metav1.OwnerReference{
						{Name: "cluster", Kind: crdbv1beta1.CrdbClusterKind},
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image: image,
				},
				Status: crdbv1beta1.CrdbNodeStatus{
					Conditions: []crdbv1beta1.NodeCondition{
						{Type: crdbv1beta1.PodReady, Status: metav1.ConditionTrue},
						{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue},
					},
				},
			}

			otherNode := node.DeepCopy()
			otherNode.Name = "test-node-2"

			nodeID := uint(1)
			r := NewReconciler(kubetest.NewFakeCtl(kubetest.Options{}, cluster, node, otherNode), nil)

			r.NodeExecutor = func(node *crdbv1beta1.CrdbNode, _ kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
				return exec
			}

			t.Run("error fetching node id", func(t *testing.T) {
				delete(node.Annotations, crdbv1beta1.CrdbNodeStartDecommissionAnnotation)
				err := r.Decommission(ctx, node)
				require.ErrorContains(t, err, fmt.Sprintf("annotation %s not set", crdbv1beta1.CrdbNodeStartDecommissionAnnotation))

				node.Annotations[crdbv1beta1.CrdbNodeStartDecommissionAnnotation] = "start"
				err = r.Decommission(ctx, node)
				require.ErrorContains(t, err, "parsing annotation")
			})

			node.Annotations[crdbv1beta1.CrdbNodeStartDecommissionAnnotation] = fmt.Sprintf("%d", nodeID)

			// issueDrain checks IsFullyDecommissioned before starting
			// drain. For the normal decom path the node is not yet
			// fully decommissioned.
			exec.EXPECT().IsFullyDecommissioned(ctx, nodeID).Return(false, nil).AnyTimes()

			t.Run("starts draining", func(t *testing.T) {
				require.NoError(t, r.Decommission(ctx, node))
				if slices.Contains(testCase.features, crdbv1beta1.ClusterDrainAfterDecommission) {
					require.Equal(t, crdbv1beta1.Drained, node.Status.Decommission)
				} else {
					require.Equal(t, crdbv1beta1.Draining, node.Status.Decommission)
				}
			})

			t.Run("draining in progress", func(t *testing.T) {
				if slices.Contains(testCase.features, crdbv1beta1.ClusterDrainAfterDecommission) {
					// Draining is not expected to happen with this feature set
					exec.EXPECT().Drain(gomock.Any(), nodeID).Times(0)
				} else {
					// Drain succeeds. The update happens in a go routine so we must poll
					// until completion.
					exec.EXPECT().Drain(gomock.Any(), nodeID).Return("", nil).MinTimes(1)
					require.ErrorContains(t, r.Decommission(ctx, node), "node still draining")
					require.Eventually(t, func() bool {
						_ = r.Decommission(ctx, node)
						return node.Status.Decommission == crdbv1beta1.Drained
					}, time.Second, 50*time.Millisecond)
				}
			})

			t.Run("starts decommissioning when not yet started", func(t *testing.T) {
				// error decommissioning node
				exec.EXPECT().Decommission(ctx, nodeID).Return(errors.New("boom"))
				err := r.Decommission(ctx, node)
				require.EqualError(t, errors.Cause(err), "boom")
				require.Equal(t, crdbv1beta1.Drained, node.Status.Decommission)

				// successful decommission call
				exec.EXPECT().Decommission(ctx, nodeID).Return(nil)
				err = r.Decommission(ctx, node)
				require.NoError(t, err)
				require.Equal(t, crdbv1beta1.TransferringReplicas, node.Status.Decommission)
			})

			t.Run("polls replicas while decommissioning", func(t *testing.T) {
				node.Status.Decommission = crdbv1beta1.TransferringReplicas

				// error getting isDecommissioning
				exec.EXPECT().IsDecommissioning(ctx, nodeID).Return(false, errors.New("boom"))
				err := r.Decommission(ctx, node)
				require.EqualError(t, errors.Cause(err), "boom")
				require.Equal(t, crdbv1beta1.TransferringReplicas, node.Status.Decommission)

				// CRDB isn't decommissioning
				exec.EXPECT().IsDecommissioning(ctx, nodeID).Return(false, nil)
				err = r.Decommission(ctx, node)
				require.NoError(t, err)
				require.Equal(t, crdbv1beta1.Drained, node.Status.Decommission)

				// Reset decomission status
				node.Status.Decommission = crdbv1beta1.TransferringReplicas

				// error fetching replicas
				exec.EXPECT().IsDecommissioning(ctx, nodeID).Return(true, nil)
				exec.EXPECT().Replicas(ctx, nodeID).Return(uint64(0), errors.New("boom"))
				err = r.Decommission(ctx, node)
				require.EqualError(t, errors.Cause(err), "boom")
				require.Equal(t, crdbv1beta1.TransferringReplicas, node.Status.Decommission)

				// replicas still > 0
				exec.EXPECT().IsDecommissioning(ctx, nodeID).Return(true, nil)
				exec.EXPECT().Replicas(ctx, nodeID).Return(uint64(1), nil)
				err = r.Decommission(ctx, node)
				require.True(t, errors.Is(err, ErrNodeStillDecommissioning))
				require.Equal(t, crdbv1beta1.TransferringReplicas, node.Status.Decommission)

				// replicas == 0, finished decommissioning
				exec.EXPECT().IsDecommissioning(ctx, nodeID).Return(true, nil)
				exec.EXPECT().Replicas(ctx, nodeID).Return(uint64(0), nil)
				err = r.Decommission(ctx, node)
				require.NoError(t, err)
				require.Equal(t, crdbv1beta1.ZeroReplicas, node.Status.Decommission)
			})

			t.Run("if draining was skipped, we drain right before finalization", func(t *testing.T) {
				if slices.Contains(testCase.features, crdbv1beta1.ClusterDrainAfterDecommission) {
					exec.EXPECT().Drain(gomock.Any(), nodeID).Return("", nil).MinTimes(1)
					require.ErrorContains(t, r.Decommission(ctx, node), "node still draining")
				} else {
					// Draining is not expected to happen with this feature set
					exec.EXPECT().Drain(gomock.Any(), nodeID).Times(0)
				}
			})

			t.Run("finalizes decommission on the node at the end", func(t *testing.T) {
				node.Status.Decommission = crdbv1beta1.ZeroReplicas

				// error finalizing
				exec.EXPECT().FinalizeDecommission(ctx, nodeID).Return(errors.New("boom"))
				require.Eventually(t, func() bool {
					err := r.Decommission(ctx, node)
					if err == nil {
						return false
					}
					errorsMatch := strings.Contains(err.Error(), "boom")
					expectedStatus := node.Status.Decommission == crdbv1beta1.ZeroReplicas
					return errorsMatch && expectedStatus
				}, time.Second, 50*time.Millisecond, "error case")

				// successful finalization
				exec.EXPECT().FinalizeDecommission(ctx, nodeID).Return(nil)
				require.Eventually(t, func() bool {
					err := r.Decommission(ctx, node)
					errorIsNil := err == nil
					expectedStatus := node.Status.Decommission == crdbv1beta1.Decommissioned
					return errorIsNil && expectedStatus
				}, time.Second, 50*time.Millisecond, "success case")

				// no-op when already finalized
				err := r.Decommission(ctx, node)
				require.NoError(t, err)
			})
		})
	}
}

// TestDrainSkipsFullyDecommissionedNode verifies that issueDrain treats
// a fully-decommissioned node as already drained, so the state machine
// can advance without hanging on a drain against a severed process.
func TestDrainSkipsFullyDecommissionedNode(t *testing.T) {
	for _, startState := range []crdbv1beta1.NodeDecommissionState{
		crdbv1beta1.Draining,
		crdbv1beta1.ZeroReplicas,
	} {
		t.Run(string(startState), func(t *testing.T) {
			mockCtrl := gomock.NewController(t)
			ctx := context.Background()
			exec := nodemock.NewMockNodeExecutor(mockCtrl)

			cluster := &crdbv1beta1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cluster",
					Namespace: "test",
				},
				Spec: crdbv1beta1.CrdbClusterSpec{
					Features: []crdbv1beta1.ClusterFeature{
						crdbv1beta1.ClusterDrainAfterDecommission,
					},
				},
			}

			image := "us-docker.pkg.dev/cockroach-cloud-images/cockroachdb/cockroach:v25.2.8"
			node := &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-node",
					Namespace: "test",
					Annotations: map[string]string{
						crdbv1beta1.CrdbNodeStartDecommissionAnnotation: "5",
					},
					OwnerReferences: []metav1.OwnerReference{
						{Name: "cluster", Kind: crdbv1beta1.CrdbClusterKind},
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{Image: image},
				Status: crdbv1beta1.CrdbNodeStatus{
					Decommission: startState,
				},
			}

			r := NewReconciler(kubetest.NewFakeCtl(kubetest.Options{}, cluster, node), nil)
			r.NodeExecutor = func(node *crdbv1beta1.CrdbNode, _ kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
				return exec
			}

			// issueDrain checks IsFullyDecommissioned before starting drain.
			// Return true so it skips the drain and treats it as drained.
			exec.EXPECT().IsFullyDecommissioned(ctx, uint(5)).Return(true, nil)

			// For Draining: after skipping drain, state advances to Drained.
			// For ZeroReplicas: after skipping drain, FinalizeDecommission
			// runs, and it calls getDecommissionDetails which also handles
			// the severed node via the guard.
			if startState == crdbv1beta1.ZeroReplicas {
				exec.EXPECT().FinalizeDecommission(ctx, uint(5)).Return(nil)
			}

			err := r.Decommission(ctx, node)
			require.NoError(t, err)

			switch startState {
			case crdbv1beta1.Draining:
				require.Equal(t, crdbv1beta1.Drained, node.Status.Decommission)
			case crdbv1beta1.ZeroReplicas:
				require.Equal(t, crdbv1beta1.Decommissioned, node.Status.Decommission)
			}
		})
	}
}

func TestContainsVirtualClusterDrainFix(t *testing.T) {
	testCases := []struct {
		name     string
		version  string
		expected bool
	}{
		{name: "24.3.21 not supported", version: "v24.3.21", expected: false},
		{name: "24.3.22 supported", version: "v24.3.22", expected: true},
		{name: "24.3.23 supported", version: "v24.3.23", expected: true},

		{name: "25.2.7 not supported", version: "v25.2.7", expected: false},
		{name: "25.2.8 supported", version: "v25.2.8", expected: true},
		{name: "25.2.9 supported", version: "v25.2.9", expected: true},

		{name: "25.3.3 not supported", version: "v25.3.3", expected: false},
		{name: "25.3.4 supported", version: "v25.3.4", expected: true},
		{name: "25.3.5 supported", version: "v25.3.5", expected: true},

		{name: "25.4.0 not supported", version: "v25.4.0", expected: false},
		{name: "25.4.1 supported", version: "v25.4.1", expected: true},
		{name: "25.4.2 supported", version: "v25.4.2", expected: true},

		{name: "24.2.0 not supported", version: "v24.2.0", expected: false},
		{name: "25.1.0 not supported", version: "v25.1.0", expected: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			v := version.MustParse(tc.version)
			result := ContainsVirtualClusterDrainFix(v)
			require.Equal(t, tc.expected, result, "Expected %v for version %s", tc.expected, tc.version)
		})
	}
}

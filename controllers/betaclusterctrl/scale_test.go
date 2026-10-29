package betaclusterctrl_test

import (
	"context"
	"fmt"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	. "github.com/cockroachlabs/cockroach-enterprise-operator/controllers/betaclusterctrl"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestScalerScale(t *testing.T) {
	ctx := context.Background()
	mutableOnly := crdbv1beta1.MutableOnly

	mockctrl := gomock.NewController(t)
	defer mockctrl.Finish()

	// a ready cluster with 3 nodes
	cluster := crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "crdb-test",
			Namespace: "test",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Mode: &mutableOnly,
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east1", Nodes: 3, CloudProvider: "k3d"},
			},
		},
		Status: crdbv1beta1.CrdbClusterStatus{
			Conditions: []crdbv1beta1.ClusterCondition{
				{Type: crdbv1beta1.ClusterInitialized, Status: metav1.ConditionTrue},
			},
			Provider: "k3d",
			Region:   "us-east1",
		},
	}

	listNodes := func(r *Reconciler) crdbv1beta1.CrdbNodeList {
		selector := kube.ParseLabelOrDie(fmt.Sprintf(
			"%s=%s",
			manifests.BetaCrdbClusterNodeOwnerLabelKey,
			cluster.Name,
		))

		ctl := r.Ctl.WithNamespace(cluster.Namespace)

		var nodeList crdbv1beta1.CrdbNodeList
		require.NoError(t, ctl.List(ctx, &kube.ListOptions{LabelSelector: selector}, &nodeList))

		// Additionally ensure that all CrdbNodes are created with a finalizer
		for _, node := range nodeList.Items {
			require.Contains(t, node.Finalizers, manifests.CrdbNodeFinalizerName)
		}

		return nodeList
	}

	makeReconciler := func(cluster *crdbv1beta1.CrdbCluster, n int) *Reconciler {
		r := &Reconciler{
			Ctl:          kubetest.NewFakeCtl(kubetest.Options{}),
			NodeExecutor: StubNodeExecutorFactory(0),
		}
		cluster.Spec.Regions[0].Nodes = int32(n)

		initialNodes := make([]kube.Object, n)
		for i := range n {
			node := manifests.BetaCrdbClusterNode(cluster, cluster.Region(), "")
			nodeID := int64(i + 1)

			node.Finalizers = append(node.Finalizers, manifests.CrdbNodeFinalizerName)
			node.Annotations = map[string]string{
				manifests.BetaCrdbClusterNodeOwnerLabelKey:         cluster.Name,
				manifests.BetaCrdbClusterNodeRevisionAnnotationKey: "outdated",
			}
			node.Name = fmt.Sprintf("%s%d", node.GenerateName, nodeID)
			node.Status.SetCondition(crdbv1beta1.PodReady, metav1.ConditionTrue)
			node.Status.SetCondition(crdbv1beta1.PodRunning, metav1.ConditionTrue)
			node.Status.NodeID = &nodeID
			initialNodes[i] = node
		}

		require.NoError(t, r.Ctl.WithNamespace(cluster.Namespace).Apply(ctx, initialNodes...))
		require.Len(t, listNodes(r).Items, n)
		return r
	}

	t.Run("scales up the current region", func(t *testing.T) {
		cluster := cluster.DeepCopy()
		// pre-existing 3-node cluster
		r := makeReconciler(cluster, 3)

		cluster.Spec.Regions[0].Nodes = 4
		_, _, err := r.Scale(ctx, cluster, listNodes(r), &NodePool{})
		require.NoError(t, err)
		require.Len(t, listNodes(r).Items, 4)

		// Make sure encryption at rest field is not set.
		for _, node := range listNodes(r).Items {
			require.Nil(t, node.Spec.EncryptionAtRest)
		}
	})

	t.Run("scales up assigns an available k8s node", func(t *testing.T) {
		cluster := cluster.DeepCopy()
		// pre-existing 3-node cluster
		r := makeReconciler(cluster, 3)

		pool := NewNodePool([]corev1.Node{{
			ObjectMeta: metav1.ObjectMeta{
				Name: "k8s-node",
				Labels: map[string]string{
					"topology.kubernetes.io/zone": "zone-a",
				},
			},
		}})

		cluster.Spec.Regions[0].Nodes = 4
		_, _, err := r.Scale(ctx, cluster, listNodes(r), pool)
		require.NoError(t, err)
		require.Len(t, listNodes(r).Items, 4)

		// Make sure encryption at rest field is not set.
		assignedK8sNode := 0
		for _, node := range listNodes(r).Items {
			if node.Spec.NodeName == "k8s-node" {
				assignedK8sNode += 1
			}
			require.Nil(t, node.Spec.EncryptionAtRest)
		}
		require.Equal(t, assignedK8sNode, 1, "expected exactly one crdb node to be assigned to 'k8s-node'")
	})

	t.Run("scale up fails when no k8s node available with explicit placement", func(t *testing.T) {
		cluster := cluster.DeepCopy()
		cluster.Spec.Features = []crdbv1beta1.ClusterFeature{crdbv1beta1.ClusterExplicitNodePlacement}

		r := makeReconciler(cluster, 3)

		pool := NewNodePool([]corev1.Node{})

		cluster.Spec.Regions[0].Nodes = 4
		_, _, err := r.Scale(ctx, cluster, listNodes(r), pool)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no available k8s node")
		require.Len(t, listNodes(r).Items, 3)
	})

	t.Run("scales up the current region with cmek enabled", func(t *testing.T) {
		secret := "secret"
		anotherSecret := "anotherSecret"
		oldSecret := "oldSecret"

		cluster.Spec.Regions[0] = crdbv1beta1.CrdbClusterRegion{
			Code: "us-east1", Nodes: 3, CloudProvider: "k3d", EncryptionAtRest: &crdbv1beta1.EncryptionAtRest{
				KeySecretName:             &secret,
				Platform:                  "GCP_CLOUD_KMS",
				CMEKCredentialsSecretName: &anotherSecret,
				OldKeySecretName:          &oldSecret,
			},
		}

		r := makeReconciler(&cluster, 3)

		cluster.Spec.Regions[0].Nodes = 4
		_, _, err := r.Scale(ctx, &cluster, listNodes(r), &NodePool{})
		require.NoError(t, err)
		require.Len(t, listNodes(r).Items, 4)
		for _, node := range listNodes(r).Items {
			require.NotNil(t, node.Spec.EncryptionAtRest)
			require.Equal(t, secret, *node.Spec.EncryptionAtRest.KeySecretName)
			require.Equal(t, crdbv1beta1.EncryptionPlatformGcpCloudKms, node.Spec.EncryptionAtRest.Platform)
			require.Equal(t, anotherSecret, *node.Spec.EncryptionAtRest.CMEKCredentialsSecretName)
			require.Equal(t, oldSecret, *node.Spec.EncryptionAtRest.OldKeySecretName)
		}
	})

	t.Run("scales up when not initialized", func(t *testing.T) {
		cluster := cluster.DeepCopy()
		cluster.Status.SetCondition(crdbv1beta1.ClusterInitialized, metav1.ConditionFalse)

		r := makeReconciler(cluster, 0)

		cluster.Spec.Regions[0].Nodes = 3

		changed, _, err := r.Scale(ctx, cluster, listNodes(r), &NodePool{})
		require.NoError(t, err)
		require.True(t, changed)

		require.Len(t, listNodes(r).Items, 3)
	})

	t.Run("no scaling when not initialized but a node exists", func(t *testing.T) {
		cluster := cluster.DeepCopy()
		cluster.Status.SetCondition(crdbv1beta1.ClusterInitialized, metav1.ConditionFalse)

		// Single node that is still starting up
		r := makeReconciler(cluster, 1)
		require.Len(t, listNodes(r).Items, 1)
	})

	t.Run("scales down the current region", func(t *testing.T) {
		cluster := cluster.DeepCopy()

		// pre-existing 4-node cluster
		r := makeReconciler(cluster, 4)
		nodes := listNodes(r)
		require.Len(t, nodes.Items, 4)

		cluster.Spec.Regions[0].Nodes = 3
		_, _, err := r.Scale(ctx, cluster, nodes, &NodePool{})
		require.NoError(t, err)
		require.Len(t, listNodes(r).Items, 4) // decommissioning node still there

		decomCount := 0
		for _, node := range listNodes(r).Items {
			if node.IsDecommissioning() {
				decomCount++
				nodeID, err := node.DecommissionNodeID()
				require.NoError(t, err)
				require.Equal(t, uint(4), nodeID)
			}
		}
		require.Equal(t, 1, decomCount)
	})

	t.Run("under-replicated ranges hold off scale-down", func(t *testing.T) {
		cluster := cluster.DeepCopy()

		// pre-existing 4-node cluster reporting under-replicated ranges
		r := makeReconciler(cluster, 4)
		r.NodeExecutor = StubNodeExecutorFactory(5)
		nodes := listNodes(r)
		require.Len(t, nodes.Items, 4)

		cluster.Spec.Regions[0].Nodes = 3
		didChange, requeueAfter, err := r.Scale(ctx, cluster, nodes, &NodePool{})
		require.NoError(t, err)

		// Scale skips the decommission and returns a delayed requeue so the
		// reconciler doesn't short-circuit but still retries later. No node
		// should be decommissioned while under-replicated.
		require.False(t, didChange)
		require.Positive(t, requeueAfter)
		for _, node := range listNodes(r).Items {
			require.False(t, node.IsDecommissioning(), "no node should be decommissioning while under-replicated")
		}
		require.True(t, cluster.Status.HasCondition(crdbv1beta1.ClusterRangesUnderReplicated, metav1.ConditionTrue))
	})

	t.Run("scale down node limit", func(t *testing.T) {
		testCases := []struct {
			description        string
			features           []crdbv1beta1.ClusterFeature
			startSize          int
			endSize            int
			expectedDecomCount int
		}{
			{
				description:        "one at a time without feature flag",
				features:           []crdbv1beta1.ClusterFeature{},
				startSize:          6,
				endSize:            3,
				expectedDecomCount: 1,
			},
			{
				description:        "all at once with feature flag",
				features:           []crdbv1beta1.ClusterFeature{crdbv1beta1.ClusterDrainAfterDecommission},
				startSize:          6,
				endSize:            3,
				expectedDecomCount: 3,
			},
		}

		for _, testCase := range testCases {
			t.Run(testCase.description, func(t *testing.T) {
				cluster := cluster.DeepCopy()
				cluster.Spec.Features = testCase.features

				// pre-existing 6-node cluster
				r := makeReconciler(cluster, testCase.startSize)

				nodes := listNodes(r)
				require.Len(t, nodes.Items, testCase.startSize)

				cluster.Spec.Regions[0].Nodes = int32(testCase.endSize)
				_, _, err := r.Scale(ctx, cluster, nodes, &NodePool{})
				require.NoError(t, err)
				require.Len(t, listNodes(r).Items, testCase.startSize)

				decomCount := 0
				for _, node := range listNodes(r).Items {
					if node.IsDecommissioning() {
						decomCount++
						nodeID, err := node.DecommissionNodeID()
						require.NoError(t, err)
						require.Equal(t, int(*node.Status.NodeID), int(nodeID))
					}
				}
				require.Equal(t, testCase.expectedDecomCount, decomCount)
			})
		}
	})

	t.Run("scale down prioritizes marked nodes", func(t *testing.T) {
		cluster := cluster.DeepCopy()

		// pre-existing 4-node cluster
		r := makeReconciler(cluster, 4)
		nodes := listNodes(r)
		require.Len(t, nodes.Items, 4)

		decomNode := &nodes.Items[3]
		decomNode.MarkForDecommission()

		cluster.Spec.Regions[0].Nodes = 3
		_, _, err := r.Scale(ctx, cluster, nodes, &NodePool{})
		require.NoError(t, err)
		require.Len(t, listNodes(r).Items, 4) // decommissioning node still there

		decomCount := 0
		for _, node := range listNodes(r).Items {
			if node.IsDecommissioning() {
				require.Equal(t, decomNode.Name, node.Name)
				decomCount++
			}
		}
		require.Equal(t, 1, decomCount)
	})

	t.Run("excludes decommissioning nodes from desired count", func(t *testing.T) {
		cluster := cluster.DeepCopy()

		// pre-existing 4-node cluster
		r := makeReconciler(cluster, 4)

		// scale multiple times
		cluster.Spec.Regions[0].Nodes = 3

		for i := range 5 {
			changed, _, err := r.Scale(ctx, cluster, listNodes(r), &NodePool{})
			require.NoError(t, err)
			// Only expect a change on the first iteration.
			require.Equal(t, i == 0, changed)
		}

		require.Len(t, listNodes(r).Items, 4) // decommissioning node still there

		decomCount := 0
		for _, node := range listNodes(r).Items {
			if node.IsDecommissioning() {
				decomCount++
			}
		}
		require.Equal(t, 1, decomCount)
	})

	t.Run("scale down skips nodes which are updating", func(t *testing.T) {
		cluster := cluster.DeepCopy()

		// pre-existing 4-node cluster
		r := makeReconciler(cluster, 4)
		nodes := listNodes(r)
		require.Len(t, nodes.Items, 4)

		decomNode := &nodes.Items[3]
		decomNode.Status.SetCondition(crdbv1beta1.PodNeedsUpdate, metav1.ConditionTrue)

		cluster.Spec.Regions[0].Nodes = 3
		_, _, err := r.Scale(ctx, cluster, nodes, &NodePool{})
		require.NoError(t, err)
		require.Len(t, listNodes(r).Items, 4) // decommissioning node still there

		decomCount := 0
		for _, node := range listNodes(r).Items {
			if node.IsDecommissioning() {
				require.NotEqual(t, decomNode.Name, node.Name)
				decomCount++
			}
		}
		require.Equal(t, 1, decomCount)
	})

	t.Run("scale down prioritizes pending and decommission marked nodes", func(t *testing.T) {
		cluster := cluster.DeepCopy()
		cluster.Spec.Features = append(cluster.Spec.Features, crdbv1beta1.ClusterDrainAfterDecommission)

		// pre-existing 4-node cluster
		r := makeReconciler(cluster, 4)
		nodes := listNodes(r)
		require.Len(t, nodes.Items, 4)

		decomNode1 := &nodes.Items[3]
		decomNode1.Status.SetCondition(crdbv1beta1.PodRunning, metav1.ConditionFalse)

		decomNode2 := &nodes.Items[2]
		decomNode2.MarkForDecommission()

		cluster.Spec.Regions[0].Nodes = 2
		_, _, err := r.Scale(ctx, cluster, nodes, &NodePool{})
		require.NoError(t, err)
		require.Len(t, listNodes(r).Items, 4) // decommissioning node still there

		decomCount := 0
		for _, node := range listNodes(r).Items {
			if node.Name == decomNode1.Name {
				require.True(t, node.IsMarkedForPendingDecommission())
				decomCount++
			}
			if node.Name == decomNode2.Name {
				decomCount++
				require.True(t, node.IsDecommissioning())
			}
		}
		require.Equal(t, 2, decomCount)
	})

	t.Run("scale down skips terminating nodes", func(t *testing.T) {
		cluster := cluster.DeepCopy()

		// pre-existing 4-node cluster
		r := makeReconciler(cluster, 4)
		nodes := listNodes(r)
		require.Len(t, nodes.Items, 4)

		// Simulate a terminating node with DeletionTimestamp set and decommission annotation.
		// This node should not count as active or decommissioning.
		terminatingNode := &nodes.Items[3]
		terminatingNode.Annotations[crdbv1beta1.CrdbNodeStartDecommissionAnnotation] = "true"
		now := metav1.Now()
		terminatingNode.DeletionTimestamp = &now
		require.NoError(t, r.Ctl.WithNamespace(cluster.Namespace).UpdateWithOptions(
			ctx, kube.UpdateOptions{NoWait: true}, terminatingNode))

		// Scale to 3 from 4, but one node is already terminating so effective active = 3.
		// No new decommissions should be started.
		cluster.Spec.Regions[0].Nodes = 3
		nodes = listNodes(r)
		changed, _, err := r.Scale(ctx, cluster, nodes, &NodePool{})
		require.NoError(t, err)
		require.False(t, changed)
	})

}

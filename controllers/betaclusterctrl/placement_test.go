package betaclusterctrl

import (
	"context"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/utils/sliceutils"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TODO(jeffswenson): create an integration test that creates a cluster, rolls
// the feature flag back, then rolls the feature flag forward.
func makeK8s(name string, zone string, unschedulable bool) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"topology.kubernetes.io/zone": zone,
			},
		},
		Spec: corev1.NodeSpec{
			Unschedulable: unschedulable,
		},
	}
}

func makeCrdb(name string, k8sNode string, zone string) crdbv1beta1.CrdbNode {
	var topologyValues []string
	if zone != "" {
		topologyValues = []string{zone}
	}
	return crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: crdbv1beta1.CrdbNodeSpec{
			NodeName: k8sNode,
		},
		Status: crdbv1beta1.CrdbNodeStatus{
			TopologyValues: topologyValues,
		},
	}
}

// makeCrdbNotRunning creates a CrdbNode with PodRunning=False, used to test
// rescheduling behavior when a pod is not running on a cordoned node.
func makeCrdbNotRunning(name string, k8sNode string, zone string) crdbv1beta1.CrdbNode {
	node := makeCrdb(name, k8sNode, zone)
	node.Status.SetCondition(crdbv1beta1.PodRunning, metav1.ConditionFalse)
	return node
}

func TestPlaceNodes(t *testing.T) {
	type testCase struct {
		name             string
		crdb             []crdbv1beta1.CrdbNode
		k8s              []corev1.Node
		k8sNodeStatusMap []nodeStatus
		crdbToK8s        map[string]string
		zoneCounts       map[string]int
		unplaceable      []string
	}

	tests := []testCase{
		{
			name: "single node cluster scale up",
			crdb: []crdbv1beta1.CrdbNode{
				makeCrdb("crdb-1", "", "zone-a"),
			},
			k8s: []corev1.Node{
				makeK8s("node-1", "zone-a", false),
			},
			k8sNodeStatusMap: []nodeStatus{
				{k8sNode: makeK8s("node-1", "zone-a", false), schedulable: true},
			},
			crdbToK8s: map[string]string{
				"crdb-1": "node-1",
			},
			zoneCounts: map[string]int{
				"zone-a": 1,
			},
		},
		{
			name: "single node cluster placed",
			crdb: []crdbv1beta1.CrdbNode{
				makeCrdb("crdb-1", "node-1", "zone-a"),
			},
			k8s: []corev1.Node{
				makeK8s("node-1", "zone-a", false),
			},
			k8sNodeStatusMap: []nodeStatus{
				{k8sNode: makeK8s("node-1", "zone-a", false), schedulable: true},
			},
			crdbToK8s: map[string]string{
				"crdb-1": "node-1",
			},
			zoneCounts: map[string]int{
				"zone-a": 1,
			},
		},
		{
			name: "three node cluster empty",
			crdb: []crdbv1beta1.CrdbNode{},
			k8s: []corev1.Node{
				makeK8s("node-1", "zone-a", false),
				makeK8s("node-2", "zone-b", false),
				makeK8s("node-3", "zone-c", false),
			},
			k8sNodeStatusMap: []nodeStatus{
				{k8sNode: makeK8s("node-1", "zone-a", false), schedulable: true},
				{k8sNode: makeK8s("node-2", "zone-b", false), schedulable: true},
				{k8sNode: makeK8s("node-3", "zone-c", false), schedulable: true},
			},
			crdbToK8s: map[string]string{},
			zoneCounts: map[string]int{
				"zone-a": 0,
				"zone-b": 0,
				"zone-c": 0,
			},
		},
		{
			name: "three node cluster scale up",
			crdb: []crdbv1beta1.CrdbNode{
				makeCrdb("crdb-1", "", "zone-a"),
				makeCrdb("crdb-3", "", "zone-c"),
				makeCrdb("crdb-2", "", "zone-b"),
			},
			k8s: []corev1.Node{
				makeK8s("node-1", "zone-a", false),
				makeK8s("node-2", "zone-b", false),
				makeK8s("node-3", "zone-c", false),
			},
			k8sNodeStatusMap: []nodeStatus{
				{k8sNode: makeK8s("node-1", "zone-a", false), schedulable: true},
				{k8sNode: makeK8s("node-2", "zone-b", false), schedulable: true},
				{k8sNode: makeK8s("node-3", "zone-c", false), schedulable: true},
			},
			crdbToK8s: map[string]string{
				"crdb-1": "node-1",
				"crdb-2": "node-2",
				"crdb-3": "node-3",
			},
			zoneCounts: map[string]int{
				"zone-a": 1,
				"zone-b": 1,
				"zone-c": 1,
			},
		},
		{
			name: "old node does not exist",
			crdb: []crdbv1beta1.CrdbNode{
				makeCrdb("crdb-3", "", "zone-c"),
				makeCrdb("crdb-1", "zone-a-dead", "zone-a"),
				makeCrdb("crdb-2", "zone-missing-dead", ""),
			},
			k8s: []corev1.Node{
				makeK8s("node-1", "zone-a", false),
				makeK8s("node-2", "zone-b", false),
				makeK8s("node-3", "zone-c", false),
			},
			k8sNodeStatusMap: []nodeStatus{
				{k8sNode: makeK8s("node-1", "zone-a", false), schedulable: true},
				{k8sNode: makeK8s("node-2", "zone-b", false), schedulable: true},
				{k8sNode: makeK8s("node-3", "zone-c", false), schedulable: true},
			},
			crdbToK8s: map[string]string{
				"crdb-1": "node-1",
				"crdb-2": "node-2",
				"crdb-3": "node-3",
			},
			zoneCounts: map[string]int{
				"zone-a": 1,
				"zone-b": 1,
				"zone-c": 1,
			},
		},
		{
			name: "three node zone mixed state",
			crdb: []crdbv1beta1.CrdbNode{
				makeCrdb("crdb-1", "node-2", "zone-a"),
				makeCrdb("crdb-2", "", ""),
				makeCrdb("crdb-3", "", "zone-a"),
			},
			k8s: []corev1.Node{
				makeK8s("node-1", "zone-a", false),
				makeK8s("node-2", "zone-a", false),
				makeK8s("node-3", "zone-a", false),
				makeK8s("node-4", "zone-a", false),
			},
			k8sNodeStatusMap: []nodeStatus{
				{k8sNode: makeK8s("node-1", "zone-a", false), schedulable: true},
				{k8sNode: makeK8s("node-2", "zone-a", false), schedulable: true},
				{k8sNode: makeK8s("node-3", "zone-a", false), schedulable: true},
				{k8sNode: makeK8s("node-4", "zone-a", false), schedulable: true},
			},
			crdbToK8s: map[string]string{
				"crdb-1": "node-2",
				"crdb-2": "node-3",
				"crdb-3": "node-1",
			},
			zoneCounts: map[string]int{
				"zone-a": 3,
			},
		},
		{
			name: "unplaceable nodes",
			crdb: []crdbv1beta1.CrdbNode{
				makeCrdb("crdb-1", "node-1", "zone-wrong"),
				makeCrdb("crdb-2", "", "zone-wrong"),
			},
			k8s: []corev1.Node{
				makeK8s("node-1", "zone-a", false),
				makeK8s("node-2", "zone-b", false),
			},
			k8sNodeStatusMap: []nodeStatus{
				{k8sNode: makeK8s("node-1", "zone-a", false), schedulable: true},
				{k8sNode: makeK8s("node-2", "zone-b", false), schedulable: true},
			},
			crdbToK8s: map[string]string{
				"crdb-1": "node-1",
			},
			zoneCounts: map[string]int{
				"zone-a": 1,
				"zone-b": 0,
			},
			unplaceable: []string{"crdb-2"},
		},
		{
			name: "empty first topology value is not used as a zone",
			crdb: []crdbv1beta1.CrdbNode{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "crdb-1"},
					Spec:       crdbv1beta1.CrdbNodeSpec{NodeName: "missing-node"},
					Status: crdbv1beta1.CrdbNodeStatus{
						TopologyValues: []string{"", "us-west-2"},
						Conditions: []crdbv1beta1.NodeCondition{{
							Type:   crdbv1beta1.PodRunning,
							Status: metav1.ConditionFalse,
						}},
					},
				},
			},
			k8s: []corev1.Node{
				makeK8s("node-without-zone", "", false),
			},
			k8sNodeStatusMap: []nodeStatus{
				{k8sNode: makeK8s("node-without-zone", "", false), schedulable: true},
			},
			crdbToK8s: map[string]string{},
			zoneCounts: map[string]int{
				"": 0,
			},
			unplaceable: []string{"crdb-1"},
		},
		{
			name: "overloaded zone",
			crdb: []crdbv1beta1.CrdbNode{
				makeCrdb("crdb-1", "", "zone-a"),
				makeCrdb("crdb-2", "", "zone-a"),
				makeCrdb("crdb-3", "", "zone-a"),
				makeCrdb("crdb-4", "", "zone-a"),
			},
			k8s: []corev1.Node{
				makeK8s("node-1", "zone-a", false),
				makeK8s("node-2", "zone-a", false),
				makeK8s("node-3", "zone-a", false),
			},
			k8sNodeStatusMap: []nodeStatus{
				{k8sNode: makeK8s("node-1", "zone-a", false), schedulable: true},
				{k8sNode: makeK8s("node-2", "zone-a", false), schedulable: true},
				{k8sNode: makeK8s("node-3", "zone-a", false), schedulable: true},
			},
			crdbToK8s: map[string]string{
				"crdb-1": "node-1",
				"crdb-2": "node-2",
				"crdb-3": "node-3",
			},
			zoneCounts: map[string]int{
				"zone-a": 3,
			},
			unplaceable: []string{"crdb-4"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pool, unschedulable := placeNodes(tc.k8s, tc.crdb)

			require.Equal(t, tc.k8sNodeStatusMap, pool.nodeStatuses)
			require.Equal(t, tc.crdbToK8s, pool.crdbToK8s)
			require.Equal(t, tc.unplaceable, unschedulable, "%+v", pool)

			nodes := map[string]string{}
			for _, node := range tc.k8s {
				nodes[node.Name] = ""
			}
			for crdb, k8s := range tc.crdbToK8s {
				nodes[k8s] = crdb
			}
			require.Equal(t, nodes, pool.k8sToCrdb)
		})
	}
}

func TestEmptyNodePool(t *testing.T) {
	empty := &NodePool{}

	node, ok := empty.GetFreeNode()
	require.Empty(t, node)
	require.False(t, ok)

	node, ok = empty.GetPlacement("some-crdb-node")
	require.Empty(t, node)
	require.False(t, ok)
}

func TestGetPlacement(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{Options: kube.Options{BeforeMutation: kubetest.MakeReady}})

	for k8sNode, zone := range map[string]string{
		"node-a": "zone-a",
		"node-b": "zone-b",
		"node-c": "zone-c",
	} {
		require.NoError(t, ctl.Apply(ctx, &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: k8sNode,
				Labels: map[string]string{
					"topology.kubernetes.io/zone": zone,
					"crdb.io/app":                 "crdb",
				},
			},
		}))
	}
	require.NoError(t, ctl.Apply(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "other-app-node",
			Labels: map[string]string{
				"topology.kubernetes.io/zone": "zone-d",
				"crdb.io/app":                 "other-app",
			},
		},
	}))

	var crdbNodes []crdbv1beta1.CrdbNode
	for crdbNode, topologyValues := range map[string][]string{
		"crdb-a": {"zone-a", "ignored-secondary-value"},
		"crdb-b": {"zone-b"},
		"crdb-c": nil,
		"crdb-d": {"zone-d"},
	} {
		crdbNodes = append(crdbNodes, crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name: crdbNode,
			},
			Status: crdbv1beta1.CrdbNodeStatus{
				TopologyValues: topologyValues,
			},
		})
	}

	var cluster crdbv1beta1.CrdbCluster

	// Get placement with the feature flag off
	pool, err := GetPlacement(ctx, ctl, &cluster, crdbNodes)
	require.NoError(t, err)
	require.Equal(t, &NodePool{}, pool)

	cluster.Spec.Features = []crdbv1beta1.ClusterFeature{crdbv1beta1.ClusterExplicitNodePlacement}
	pool, err = GetPlacement(ctx, ctl, &cluster, crdbNodes)
	require.NoError(t, err)
	require.NotEqual(t, &NodePool{}, pool)

	expectPlacement := func(crdb string, k8s string) {
		node, ok := pool.GetPlacement(crdb)
		if k8s == "" {
			require.False(t, ok, "crdb %s was placed but shouldn't have been", crdb)
		} else {
			require.Equal(t, k8s, node)
			require.True(t, ok)
		}
	}

	expectPlacement("crdb-a", "node-a")
	expectPlacement("crdb-b", "node-b")
	expectPlacement("crdb-c", "node-c")
	// other-app, the only node in zone-d, is not a valid node for CRDB.
	expectPlacement("crdb-d", "")
}

func TestBackfillNodeName(t *testing.T) {
	type placement struct {
		name    string
		k8sNode string
	}

	type testCase struct {
		name       string
		pods       []placement
		nodes      []placement
		nodesAfter []placement
	}

	tests := []testCase{
		{
			name: "empty",
		},
		{
			name: "all-nodes-already-placed",
			nodes: []placement{
				{name: "crdb-1", k8sNode: "k8s-a"},
				{name: "crdb-2", k8sNode: "k8s-b"},
				{name: "crdb-3", k8sNode: "k8s-c"},
			},
			nodesAfter: []placement{
				{name: "crdb-1", k8sNode: "k8s-a"},
				{name: "crdb-2", k8sNode: "k8s-b"},
				{name: "crdb-3", k8sNode: "k8s-c"},
			},
			// Since all of the nodes have assigned pods, we don't define the
			// pods because there is no need to look them up.
		},
		{
			name: "expected-backfill",
			nodes: []placement{
				{name: "crdb-1"},
				{name: "crdb-2"},
				{name: "crdb-3"},
			},
			pods: []placement{
				{name: "crdb-1", k8sNode: "k8s-a"},
				{name: "crdb-2", k8sNode: "k8s-b"},
				{name: "crdb-3", k8sNode: "k8s-c"},
			},
			nodesAfter: []placement{
				{name: "crdb-1", k8sNode: "k8s-a"},
				{name: "crdb-2", k8sNode: "k8s-b"},
				{name: "crdb-3", k8sNode: "k8s-c"},
			},
		},
		{
			name: "missing-pod",
			nodes: []placement{
				{name: "crdb-1"},
				{name: "crdb-2"},
				{name: "crdb-3"},
			},
			pods: []placement{
				{name: "crdb-1", k8sNode: "k8s-a"},
				// {name: "crdb-2", k8sNode: "k8s-b"},
				{name: "crdb-3", k8sNode: "k8s-c"},
			},
			nodesAfter: []placement{
				{name: "crdb-1", k8sNode: "k8s-a"},
				{name: "crdb-2"},
				{name: "crdb-3", k8sNode: "k8s-c"},
			},
		},
		{
			name: "partial-backfill",
			nodes: []placement{
				{name: "crdb-1"},
				{name: "crdb-2", k8sNode: "k8s-b"},
				{name: "crdb-3"},
			},
			pods: []placement{
				{name: "crdb-1", k8sNode: "k8s-a"},
				// Since the node is already placed, the pod does not need to
				// exist. We comment it out to verify the Get is skipped.
				// {name: "crdb-2", k8sNode: "k8s-b"},
				{name: "crdb-3", k8sNode: "k8s-c"},
			},
			nodesAfter: []placement{
				{name: "crdb-1", k8sNode: "k8s-a"},
				{name: "crdb-2", k8sNode: "k8s-b"},
				{name: "crdb-3", k8sNode: "k8s-c"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctl := kubetest.NewFakeCtl(kubetest.Options{Options: kube.Options{BeforeMutation: kubetest.MakeReady}})

			// Insert the pods into the fake ctl
			pods := sliceutils.Map(tc.pods, func(p placement) kube.Object {
				return &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: p.name,
					},
					Spec: corev1.PodSpec{
						NodeName: p.k8sNode,
					},
				}
			})
			require.NoError(t, ctl.Apply(context.Background(), pods...))

			// Create the initiale node state
			placementToNode := func(p placement) crdbv1beta1.CrdbNode {
				return crdbv1beta1.CrdbNode{
					ObjectMeta: metav1.ObjectMeta{
						Name: p.name,
					},
					Spec: crdbv1beta1.CrdbNodeSpec{
						NodeName: p.k8sNode,
					},
				}
			}
			nodes := sliceutils.Map(tc.nodes, placementToNode)

			// Create the backfill list
			nodesAfter, err := backfillNodeName(context.Background(), ctl, nodes)
			require.NoError(t, err)

			// Ensure the backfill nodes have the right node name
			expectedNodesAfter := sliceutils.Map(tc.nodesAfter, placementToNode)
			require.Equal(t, expectedNodesAfter, nodesAfter)
		})
	}
}

func requirePlacement(t *testing.T, pool *NodePool, crdbNode string, k8sNode string) {
	foundNode, exists := pool.GetPlacement(crdbNode)
	require.Equal(t, k8sNode, foundNode)
	require.Equal(t, exists, k8sNode != "", "crdbNode=%s", crdbNode)
}

func TestNodePoolClaimNode(t *testing.T) {
	pool := NewNodePool([]corev1.Node{
		makeK8s("node-a-1", "zone-a", false),
		makeK8s("node-a-2", "zone-a", false),
	})

	// It's possible to claim nodes that exist
	require.True(t, pool.ClaimNode("node-a-1", "crdb-1"))
	require.True(t, pool.ClaimNode("node-a-2", "crdb-2"))

	// It is not possible to claim nodes that are already claimed.
	require.False(t, pool.ClaimNode("node-a-1", "crdb-3"))

	// It is not possible to claim nodes that do not exist
	require.False(t, pool.ClaimNode("node-does-not-exist", "crdb-4"))

	requirePlacement(t, pool, "crdb-1", "node-a-1")
	requirePlacement(t, pool, "crdb-2", "node-a-2")
	requirePlacement(t, pool, "crdb-3", "")
	requirePlacement(t, pool, "crdb-4", "")
}

func TestNodePoolClaimZone(t *testing.T) {
	pool := NewNodePool([]corev1.Node{
		makeK8s("node-a-1", "zone-a", false),
		makeK8s("node-a-2", "zone-a", false),
		makeK8s("node-b-1", "zone-b", false),
	})

	expectClaim := func(zone string, crdb string, expectedK8s string) {
		actualNode, ok := pool.GetFreeNodeInZone(zone)
		require.Equal(t, expectedK8s, actualNode)
		require.Equal(t, expectedK8s != "", ok)
		pool.ClaimNode(actualNode, crdb)
	}

	expectClaim("zone-a", "crdb-1", "node-a-1")
	expectClaim("zone-a", "crdb-2", "node-a-2")
	// zone-1 should be stocked out at this point.
	expectClaim("zone-a", "crdb-3", "")

	requirePlacement(t, pool, "crdb-1", "node-a-1")
	requirePlacement(t, pool, "crdb-2", "node-a-2")
	requirePlacement(t, pool, "crdb-3", "")
}

func TestNodePoolClaimAny(t *testing.T) {
	pool := NewNodePool([]corev1.Node{
		makeK8s("node-a-1", "zone-a", false),
		makeK8s("node-a-2", "zone-a", false),
		makeK8s("node-a-3", "zone-a", false),
		makeK8s("node-b-1", "zone-b", false),
		makeK8s("node-b-2", "zone-b", false),
		makeK8s("node-b-3", "zone-b", false),
		makeK8s("node-c-1", "zone-c", false),
		makeK8s("node-c-2", "zone-c", false),
	})

	require.True(t, pool.ClaimNode("node-a-1", "crdb-1"))
	require.True(t, pool.ClaimNode("node-a-2", "crdb-2"))
	require.True(t, pool.ClaimNode("node-a-3", "crdb-3"))
	require.True(t, pool.ClaimNode("node-b-1", "crdb-4"))

	expectClaim := func(crdb string, expectedK8s string) {
		actualNode, ok := pool.GetFreeNode()
		require.Equal(t, expectedK8s, actualNode)
		require.Equal(t, expectedK8s != "", ok)
		pool.ClaimNode(actualNode, crdb)
	}

	expectClaim("crdb-5", "node-c-1")
	expectClaim("crdb-6", "node-b-2")
	expectClaim("crdb-7", "node-c-2")
	expectClaim("crdb-8", "node-b-3")

	// This claim fails because zone-c is exhausted and allocating an
	// additional node in zone-a or zone-b would lead to imbalance.
	expectClaim("crdb-9", "")
}

func TestNodePoolIsAvailable(t *testing.T) {
	pool := NewNodePool([]corev1.Node{
		makeK8s("node-a-1", "zone-a", false),
	})

	// A node that exists with no claim is available.
	require.True(t, pool.isAvailable("node-a-1"))

	// If a node does not exist, it is not available.
	require.False(t, pool.isAvailable("node-a-2"))

	// Claiming a node makes it unavailable.
	pool.ClaimNode("node-a-1", "crdb-1")
	require.False(t, pool.isAvailable("node-a-1"))
}

func TestNodePoolIsZoneFull(t *testing.T) {
	pool := NewNodePool([]corev1.Node{
		makeK8s("node-a-1", "zone-a", false),
		makeK8s("node-a-2", "zone-a", false),
		makeK8s("node-a-3", "zone-a", false),
		makeK8s("node-b-1", "zone-b", false),
		makeK8s("node-b-2", "zone-b", false),
		makeK8s("node-c-1", "zone-c", false),
		makeK8s("node-c-2", "zone-c", false),
	})

	require.False(t, pool.isZoneFull("zone-a"))
	require.False(t, pool.isZoneFull("zone-b"))
	require.False(t, pool.isZoneFull("zone-c"))

	pool.ClaimNode("node-a-1", "crdb-1")

	// zone-a is full because allocating an additional node in the zone would
	// make it imbalanced
	require.True(t, pool.isZoneFull("zone-a"))
	require.False(t, pool.isZoneFull("zone-b"))
	require.False(t, pool.isZoneFull("zone-c"))

	pool.ClaimNode("node-b-1", "crdb-2")

	// zone-a and zone-b are full because allocating an additional node in the
	// zone would make it imbalanced
	require.True(t, pool.isZoneFull("zone-a"))
	require.True(t, pool.isZoneFull("zone-b"))
	require.False(t, pool.isZoneFull("zone-c"))

	pool.ClaimNode("node-c-1", "crdb-3")

	// now that all three zones are balanced, none of them are full.
	require.False(t, pool.isZoneFull("zone-a"))
	require.False(t, pool.isZoneFull("zone-b"))
	require.False(t, pool.isZoneFull("zone-c"))
}

func TestNodeCordoning(t *testing.T) {
	type testCase struct {
		name                  string
		k8sNodes              []corev1.Node
		crdbNodes             []crdbv1beta1.CrdbNode
		expectedSchedulable   map[string]bool   // k8s node -> schedulable status
		expectedPlacements    map[string]string // crdb -> k8s
		expectedUnschedulable []string
		description           string
	}

	tests := []testCase{
		{
			name: "basic unschedulable node",
			k8sNodes: []corev1.Node{
				makeK8s("node-a", "zone-a", false),
				makeK8s("node-b", "zone-b", false),
				makeK8s("node-c", "zone-c", true), // cordoned
			},
			crdbNodes: []crdbv1beta1.CrdbNode{
				makeCrdb("crdb-1", "", "zone-a"),
				makeCrdb("crdb-2", "", "zone-b"),
				makeCrdb("crdb-3", "", "zone-c"),
			},
			expectedSchedulable: map[string]bool{
				"node-a": true,
				"node-b": true,
				"node-c": false,
			},
			expectedPlacements: map[string]string{
				"crdb-1": "node-a",
				"crdb-2": "node-b",
			},
			expectedUnschedulable: []string{"crdb-3"},
			description:           "CRDB node wanting cordoned zone should be unschedulable",
		},
		{
			name: "existing placement preserved on cordoned node",
			k8sNodes: []corev1.Node{
				makeK8s("node-1", "zone-a", true), // cordoned but has existing pod
			},
			crdbNodes: []crdbv1beta1.CrdbNode{
				makeCrdb("crdb-1", "node-1", "zone-a"), // existing assignment
				makeCrdb("crdb-2", "", "zone-a"),       // new pod
			},
			expectedSchedulable: map[string]bool{
				"node-1": false,
			},
			expectedPlacements: map[string]string{
				"crdb-1": "node-1", // existing pod stays
			},
			expectedUnschedulable: []string{"crdb-2"}, // new pod can't schedule
			description:           "Existing pods should stay on cordoned nodes",
		},
		{
			name: "insufficient capacity due to cordoning",
			k8sNodes: []corev1.Node{
				makeK8s("node-a", "zone-a", false),
				makeK8s("node-b", "zone-b", true), // cordoned
				makeK8s("node-c", "zone-c", true), // cordoned
			},
			crdbNodes: []crdbv1beta1.CrdbNode{
				makeCrdb("crdb-1", "node-b", "zone-b"), // existing on cordoned
				makeCrdb("crdb-2", "node-c", "zone-c"), // existing on cordoned
				makeCrdb("crdb-3", "", ""),             // new pod
				makeCrdb("crdb-4", "", ""),             // new pod
			},
			expectedSchedulable: map[string]bool{
				"node-a": true,
				"node-b": false,
				"node-c": false,
			},
			expectedPlacements: map[string]string{
				"crdb-1": "node-b", // existing pods stay
				"crdb-2": "node-c", // existing pods stay
				"crdb-3": "node-a", // only one schedulable node
			},
			expectedUnschedulable: []string{"crdb-4"}, // insufficient capacity
			description:           "Should handle capacity constraints with cordoned nodes",
		},
		{
			name: "node addition resolves unschedulable pods",
			k8sNodes: []corev1.Node{
				makeK8s("node-1", "zone-a", true), // cordoned with existing pod
				makeK8s("node-2", "zone-b", false),
				makeK8s("node-3", "zone-c", false),
				makeK8s("node-4", "zone-a", false), // new node in the same zone as cordoned
			},
			crdbNodes: []crdbv1beta1.CrdbNode{
				makeCrdb("crdb-1", "node-1", "zone-a"), // existing on cordoned
				makeCrdb("crdb-2", "node-2", "zone-b"), // existing
				makeCrdb("crdb-3", "node-3", "zone-c"), // existing
				makeCrdb("crdb-4", "", ""),
			},
			expectedSchedulable: map[string]bool{
				"node-1": false, // cordoned
				"node-2": true,
				"node-3": true,
				"node-4": true, // new node
			},
			expectedPlacements: map[string]string{
				"crdb-1": "node-1", // stays on cordoned
				"crdb-2": "node-2",
				"crdb-3": "node-3",
				"crdb-4": "node-4", // gets new node
			},
			expectedUnschedulable: []string{},
			description:           "Adding nodes should allow previously unschedulable pods to be placed",
		},
		{
			name: "uncordoning allows new scheduling",
			k8sNodes: []corev1.Node{
				makeK8s("node-a", "zone-a", false),
				makeK8s("node-b", "zone-b", false),
				makeK8s("node-c", "zone-c", false),
			},
			crdbNodes: []crdbv1beta1.CrdbNode{
				makeCrdb("crdb-1", "", "zone-a"),
				makeCrdb("crdb-2", "", "zone-b"),
				makeCrdb("crdb-3", "", "zone-c"),
			},
			expectedSchedulable: map[string]bool{
				"node-a": true,
				"node-b": true,
				"node-c": true,
			},
			expectedPlacements: map[string]string{
				"crdb-1": "node-a",
				"crdb-2": "node-b",
				"crdb-3": "node-c",
			},
			expectedUnschedulable: []string{},
			description:           "Uncordoning should allow normal scheduling to resume",
		},
		{
			name: "correct handling of insufficient nodes for scale up",
			k8sNodes: []corev1.Node{
				makeK8s("node-a", "zone-a", false),
				makeK8s("node-b", "zone-b", false),
				makeK8s("node-c", "zone-c", true), // cordoned - no existing pods
			},
			crdbNodes: []crdbv1beta1.CrdbNode{
				makeCrdb("crdb-1", "node-a", "zone-a"), // existing
				makeCrdb("crdb-2", "node-b", "zone-b"), // existing
				makeCrdb("crdb-3", "", ""),             // new pod - should be unschedulable
				makeCrdb("crdb-4", "", ""),             // new pod - should be unschedulable
			},
			expectedSchedulable: map[string]bool{
				"node-a": true,
				"node-b": true,
				"node-c": false, // cordoned
			},
			expectedPlacements: map[string]string{
				"crdb-1": "node-a",
				"crdb-2": "node-b",
			},
			expectedUnschedulable: []string{"crdb-3", "crdb-4"}, // insufficient schedulable capacity
			description:           "Scale up should be blocked when insufficient schedulable nodes exist",
		},
		{
			name: "New Node should be scheduled on the schedulable node in the same zone",
			k8sNodes: []corev1.Node{
				makeK8s("node-a", "zone-a", false),
				makeK8s("node-b", "zone-b", false),
				makeK8s("node-c", "zone-c", true), // cordoned - no existing pods
				makeK8s("node-d", "zone-c", false),
			},
			crdbNodes: []crdbv1beta1.CrdbNode{
				makeCrdb("crdb-1", "node-a", "zone-a"), // existing
				makeCrdb("crdb-2", "node-b", "zone-b"), // existing
				makeCrdb("crdb-3", "", ""),             // new pod
			},
			expectedSchedulable: map[string]bool{
				"node-a": true,
				"node-b": true,
				"node-c": false, // cordoned
				"node-d": true,
			},
			expectedPlacements: map[string]string{
				"crdb-1": "node-a",
				"crdb-2": "node-b",
				"crdb-3": "node-d",
			},
			expectedUnschedulable: []string{},
			description:           "New nodes should be scheduled on the uncordoned node in the same zone",
		},
		{
			name: "non-running pod on cordoned node gets reassigned",
			k8sNodes: []corev1.Node{
				makeK8s("node-a", "zone-a", false),
				makeK8s("node-b", "zone-b", true),  // cordoned
				makeK8s("node-c", "zone-b", false), // schedulable replacement in same zone
			},
			crdbNodes: []crdbv1beta1.CrdbNode{
				makeCrdb("crdb-1", "node-a", "zone-a"),
				// crdb-2 was on the cordoned node but its pod is no longer running.
				// It should be reassigned to node-c in the same zone.
				makeCrdbNotRunning("crdb-2", "node-b", "zone-b"),
			},
			expectedSchedulable: map[string]bool{
				"node-a": true,
				"node-b": false,
				"node-c": true,
			},
			expectedPlacements: map[string]string{
				"crdb-1": "node-a",
				"crdb-2": "node-c",
			},
			expectedUnschedulable: []string{},
			description:           "A non-running pod on a cordoned node should be reassigned to a schedulable node",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pool, unschedulable := placeNodes(tc.k8sNodes, tc.crdbNodes)

			// Verify node schedulable status
			for _, nodeStatus := range pool.nodeStatuses {
				expectedSchedulable, exists := tc.expectedSchedulable[nodeStatus.k8sNode.Name]
				require.True(t, exists, "test case should specify schedulable status for %s", nodeStatus.k8sNode.Name)
				require.Equal(t, expectedSchedulable, nodeStatus.schedulable,
					"node %s schedulable status", nodeStatus.k8sNode.Name)
			}

			// Verify expected placements
			for crdbName, expectedK8s := range tc.expectedPlacements {
				placement, exists := pool.GetPlacement(crdbName)
				require.True(t, exists, "CRDB node %s should be placed", crdbName)
				require.Equal(t, expectedK8s, placement, "CRDB node %s placement", crdbName)
			}

			// Verify unschedulable nodes
			require.ElementsMatch(t, tc.expectedUnschedulable, unschedulable,
				"unschedulable nodes mismatch. %s", tc.description)

			// Verify unschedulable nodes don't have placements
			for _, crdbName := range tc.expectedUnschedulable {
				_, exists := pool.GetPlacement(crdbName)
				require.False(t, exists, "Unschedulable CRDB node %s should not have placement", crdbName)
			}
		})
	}
}

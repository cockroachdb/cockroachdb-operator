package betaclusterctrl

import (
	"container/heap"
	"fmt"
	"math/rand"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTopologyHeap(t *testing.T) {
	h := &topologyHeap{}
	const maxSize = 100

	// Setup the heap to contain topologies with sizes
	// from 1 to maxSize.
	for l := range rand.Perm(maxSize) {
		topo := &topology{indices: []int{}}
		for i := range l + 1 {
			topo.indices = append(topo.indices, i)
		}
		h.Push(topo)
	}
	heap.Init(h)

	require.Equal(t, maxSize, h.Len())
	for i := range maxSize {
		topo := heap.Pop(h).(*topology)
		require.Equal(t, maxSize-i, len(topo.indices))

		topo.indices = nil
		// Now topo should be at the bottom of the heap.
		heap.Push(h, topo)
		require.Equal(t, maxSize, h.Len())
	}

	// Now bottom of the heap should all have empty indices.
	require.Equal(t, maxSize, h.Len())
	for range maxSize {
		topo := heap.Pop(h).(*topology)
		require.Nil(t, topo.indices)
	}
}

func TestTopologyHeapLexicographicalOrdering(t *testing.T) {
	h := &topologyHeap{}
	const maxSize = 100

	for l := range rand.Perm(maxSize) {
		topo := &topology{
			indices: []int{},
			value:   fmt.Sprintf("%03d", l),
		}
		heap.Push(h, topo)
	}

	prevTop := heap.Pop(h).(*topology)
	for i := 1; i < maxSize; i++ {
		top := heap.Pop(h).(*topology)
		require.Greater(t, prevTop.value, top.value)
		prevTop = top
	}
}

func TestGetDecomIndexes(t *testing.T) {
	// The second values group these nodes differently. Keeping the selections
	// below based on the first values proves that decommissioning ignores them.
	nodes := []crdbv1beta1.CrdbNode{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cockroachdb-0",
			},
			Status: crdbv1beta1.CrdbNodeStatus{
				TopologyValues: []string{"a", "x"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cockroachdb-1",
			},
			Status: crdbv1beta1.CrdbNodeStatus{
				TopologyValues: []string{"b", "x"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cockroachdb-2",
			},
			Status: crdbv1beta1.CrdbNodeStatus{
				TopologyValues: []string{"c", "x"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cockroachdb-3",
			},
			Status: crdbv1beta1.CrdbNodeStatus{
				TopologyValues: []string{"a", "y"},
			},
		},
	}
	indexes := getDecomIndexes(nodes, 1)
	require.Len(t, indexes, 1)
	require.Equal(t, "cockroachdb-3", nodes[indexes[0]].Name)

	indexes = getDecomIndexes(nodes, 2)
	require.Len(t, indexes, 2)
	require.Equal(t, "cockroachdb-3", nodes[indexes[0]].Name)
	require.Equal(t, "cockroachdb-2", nodes[indexes[1]].Name)

	indexes = getDecomIndexes(nodes, 3)
	require.Len(t, indexes, 3)
	require.Equal(t, "cockroachdb-3", nodes[indexes[0]].Name)
	require.Equal(t, "cockroachdb-2", nodes[indexes[1]].Name)
	require.Equal(t, "cockroachdb-1", nodes[indexes[2]].Name)

	// These nodes have no topology values, so they all should be
	// under same group.
	newNodes := []crdbv1beta1.CrdbNode{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cockroachdb-4",
			},
			Status: crdbv1beta1.CrdbNodeStatus{},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cockroachdb-5",
			},
			Status: crdbv1beta1.CrdbNodeStatus{},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cockroachdb-6",
			},
			Status: crdbv1beta1.CrdbNodeStatus{},
		},
	}
	nodes = append(nodes, newNodes...)
	indexes = getDecomIndexes(nodes, 3)
	require.Len(t, indexes, 3)
	require.Equal(t, "cockroachdb-6", nodes[indexes[0]].Name)
	require.Equal(t, "cockroachdb-3", nodes[indexes[1]].Name)
	require.Equal(t, "cockroachdb-5", nodes[indexes[2]].Name)
}

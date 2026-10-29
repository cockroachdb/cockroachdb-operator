package metrics

import (
	"context"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/stretchr/testify/require"
	"sync"
	"testing"
)

// helper that seeds the manager with an initial map
func seededManager(t *testing.T) *NodeManager {
	t.Helper()
	// ctl == nil → monitor exits fast.
	m := NewNodeMetricsManager(context.Background(), nil)

	m.updateNodeMetrics(NodeMetricsMap{
		"n1": {CpuIRate: 50},
		"n2": {CpuIRate: 25},
	})
	return m
}

func TestGetNodeMetrics(t *testing.T) {
	testutils.GoroutineLeakCheck(t)
	m := seededManager(t)

	got := m.GetNodeMetrics()
	require.Equal(t, 50.0, got["n1"].CpuIRate)
	require.Equal(t, 25.0, got["n2"].CpuIRate)

	got["n1"] = NodeMetrics{CpuIRate: 99}
	require.Equal(t, 50.0, m.GetNodeMetrics()["n1"].CpuIRate)
}

func TestConcurrentRequests(t *testing.T) {
	testutils.GoroutineLeakCheck(t)
	m := seededManager(t)

	var wg sync.WaitGroup
	requests := 10
	wg.Add(2 * requests)

	for range requests {
		go func() {
			defer wg.Done()
			for i := 1; i <= 100; i++ {
				m.updateNodeMetrics(NodeMetricsMap{
					"n1": {CpuIRate: float64(i % 100)},
				})
			}
		}()

		go func() {
			defer wg.Done()
			for i := 1; i <= 100; i++ {
				_ = m.GetNodeMetrics()
			}
		}()
	}

	wg.Wait()
}

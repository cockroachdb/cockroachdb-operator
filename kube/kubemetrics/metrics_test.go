package kubemetrics

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/metrics"
)

func TestMetrics(t *testing.T) {
	collectors := Metrics()
	require.Contains(t, collectors, requestLatency)
	require.Contains(t, collectors, throttleLatency)
	require.IsType(t, &latencyMetric{}, metrics.RequestLatency)
	require.IsType(t, &latencyMetric{}, metrics.RateLimiterLatency)
}

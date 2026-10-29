package testutils

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// RequireMetricEqual checks if a prometheus.Collector storing exactly
// one Metric is equal to count. See testutil.ToFloat64 for more.
func RequireMetricEqual(t *testing.T, metric prometheus.Collector, count int) {
	t.Helper()
	require.Equal(t, count, int(testutil.ToFloat64(metric)))
}

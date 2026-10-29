package autoscaler

import (
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func TestDesiredPods(t *testing.T) {
	if testing.Verbose() {
		logging.SetGlobalLogger(zaptest.NewLogger(t))
	}

	testCases := []struct {
		desc       string
		maxPods    *int32
		metrics    *metrics.TenantMetrics
		podsAfter  int32
		podsBefore int32
	}{
		{
			desc: "initial value",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:       0.5,
				MaxCpu5m:       1,
				DeltaQueries2m: true,
				JobsRunning:    0,
				ConnsOpen:      1,
				Uptime:         50,
				FillRatio:      1.0,
			},
			podsBefore: 0,
			podsAfter:  1,
		},
		{
			desc: "another tenant",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:       2,
				MaxCpu5m:       5,
				DeltaQueries2m: true,
				JobsRunning:    0,
				ConnsOpen:      2,
				Uptime:         100,
				FillRatio:      1.0,
			},
			podsBefore: 0,
			podsAfter:  2,
		},
		{
			desc: "change < 10%",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:       0.52,
				MaxCpu5m:       1,
				DeltaQueries2m: true,
				JobsRunning:    0,
				ConnsOpen:      1,
				Uptime:         50,
				FillRatio:      1.0,
			},
			podsBefore: 1,
			podsAfter:  1,
		},
		{
			desc: "change < 10%",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:       1.09,
				MaxCpu5m:       1,
				DeltaQueries2m: true,
				JobsRunning:    0,
				ConnsOpen:      2,
				Uptime:         50,
				FillRatio:      1.0,
			},
			podsBefore: 1,
			podsAfter:  1,
		},
		{
			desc: "change > 10%",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:       1.11,
				MaxCpu5m:       2,
				DeltaQueries2m: true,
				JobsRunning:    0,
				ConnsOpen:      1,
				Uptime:         100,
				FillRatio:      1.0,
			},
			podsBefore: 1,
			podsAfter:  2,
		},
		{
			desc: "change < 10% downscale",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:       1.90,
				MaxCpu5m:       2,
				DeltaQueries2m: true,
				JobsRunning:    0,
				ConnsOpen:      1,
				Uptime:         100,
				FillRatio:      1.0,
			},
			podsBefore: 2,
			podsAfter:  2,
		},
		{
			desc: "max cpu spike",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:       1.5,
				MaxCpu5m:       24.5,
				DeltaQueries2m: true,
				JobsRunning:    1,
				ConnsOpen:      10,
				Uptime:         1000,
				FillRatio:      1.0,
			},
			podsBefore: 2,
			podsAfter:  9,
		},
		{
			desc:    "limit to max pods",
			maxPods: new(int32(8)),
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:       30,
				MaxCpu5m:       50,
				DeltaQueries2m: true,
				JobsRunning:    0,
				ConnsOpen:      100,
				Uptime:         600,
				FillRatio:      1.0,
			},
			podsBefore: 9,
			podsAfter:  8,
		},
		{
			desc:    "increase max pods limit",
			maxPods: new(int32(9)),
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:       30,
				MaxCpu5m:       50,
				DeltaQueries2m: true,
				JobsRunning:    0,
				ConnsOpen:      100,
				Uptime:         600,
				FillRatio:      1.0,
			},
			podsBefore: 8,
			podsAfter:  9,
		},
		{
			desc: "suspend",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:        0.0199,
				MaxCpu5m:        0.0199,
				DeltaQueries2m:  false,
				DeltaNewConns2m: false,
				JobsRunning:     0,
				ConnsOpen:       0,
				Uptime:          360,
				FillRatio:       1.0,
			},
			podsBefore: 1,
			podsAfter:  0,
		},
		{
			desc: "suspend when suspended",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:        0,
				MaxCpu5m:        0,
				DeltaQueries2m:  false,
				DeltaNewConns2m: false,
				JobsRunning:     0,
				ConnsOpen:       0,
				Uptime:          361,
				FillRatio:       1.0,
			},
			podsBefore: 0,
			podsAfter:  0,
		},
		{
			desc: "suspend when suspended cc-25182",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:        0,
				MaxCpu5m:        0,
				DeltaQueries2m:  false,
				DeltaNewConns2m: false,
				JobsRunning:     -1,
				ConnsOpen:       0,
				Uptime:          361,
				FillRatio:       1.0,
			},
			podsBefore: 0,
			podsAfter:  0,
		},
		{
			desc: "do not suspend if there are new connections",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:        0.0199,
				MaxCpu5m:        0.0199,
				DeltaQueries2m:  false,
				DeltaNewConns2m: true,
				JobsRunning:     0,
				ConnsOpen:       0,
				Uptime:          360,
				FillRatio:       1.0,
			},
			podsBefore: 1,
			podsAfter:  1,
		},
		{
			desc: "resume",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:       0.01,
				MaxCpu5m:       0.01,
				DeltaQueries2m: true,
				JobsRunning:    0,
				ConnsOpen:      0,
				Uptime:         0,
				FillRatio:      1.0,
			},
			podsBefore: 0,
			podsAfter:  1,
		},
		{
			desc: "do not suspend because pod just started",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:       0,
				MaxCpu5m:       0,
				DeltaQueries2m: false,
				JobsRunning:    0,
				ConnsOpen:      0,
				Uptime:         0,
				FillRatio:      1.0,
			},
			podsBefore: 1,
			podsAfter:  1,
		},
		{
			desc: "do not suspend because there was one query",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:       0.0199,
				MaxCpu5m:       0.0199,
				DeltaQueries2m: true,
				JobsRunning:    0,
				ConnsOpen:      0,
				Uptime:         360,
				FillRatio:      1.0,
			},
			podsBefore: 1,
			podsAfter:  1,
		},
		{
			desc: "do not suspend because there is a running job",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:       0.0199,
				MaxCpu5m:       0.0199,
				DeltaQueries2m: false,
				JobsRunning:    1,
				ConnsOpen:      0,
				Uptime:         360,
				FillRatio:      1.0,
			},
			podsBefore: 1,
			podsAfter:  1,
		},
		{
			desc: "do not suspend because there is an open transaction",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:       0.0199,
				MaxCpu5m:       0.0199,
				DeltaQueries2m: false,
				JobsRunning:    0,
				ConnsOpen:      1,
				Uptime:         360,
				FillRatio:      1.0,
			},
			podsBefore: 1,
			podsAfter:  1,
		},
		{
			desc: "do not suspend because no pod has been running for 6 minutes",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:       0.0199,
				MaxCpu5m:       0.0199,
				DeltaQueries2m: false,
				JobsRunning:    0,
				ConnsOpen:      0,
				Uptime:         359,
				FillRatio:      1.0,
			},
			podsBefore: 1,
			podsAfter:  1,
		},
		{
			desc: "another tenant, no change",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:       2,
				MaxCpu5m:       6,
				DeltaQueries2m: true,
				JobsRunning:    0,
				ConnsOpen:      0,
				Uptime:         10,
				FillRatio:      1.0,
			},
			podsBefore: 2,
			podsAfter:  2,
		},
		{
			desc: "third tenant, already suspended",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:        0.5,
				MaxCpu5m:        1,
				DeltaQueries2m:  false,
				DeltaNewConns2m: false,
				JobsRunning:     0,
				ConnsOpen:       0,
				Uptime:          360,
				FillRatio:       1.0,
			},
			podsBefore: 0,
			podsAfter:  0,
		},
		{
			desc: "block suspend when metrics not ready",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:        0,
				MaxCpu5m:        0,
				DeltaQueries2m:  false,
				DeltaNewConns2m: false,
				JobsRunning:     0,
				ConnsOpen:       0,
				Uptime:          360,
				FillRatio:       0.5,
			},
			podsBefore: 3,
			podsAfter:  3,
		},
		{
			desc: "block scale-down when metrics not ready",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:       0.5,
				MaxCpu5m:       1,
				DeltaQueries2m: true,
				JobsRunning:    0,
				ConnsOpen:      1,
				Uptime:         600,
				FillRatio:      0.5,
			},
			podsBefore: 5,
			podsAfter:  5,
		},
		{
			desc: "allow scale-up when metrics not ready",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:       3,
				MaxCpu5m:       10,
				DeltaQueries2m: true,
				JobsRunning:    0,
				ConnsOpen:      5,
				Uptime:         600,
				FillRatio:      0.5,
			},
			podsBefore: 1,
			podsAfter:  4,
		},
		{
			desc: "allow scale-down when metrics ready",
			metrics: &metrics.TenantMetrics{
				AvgCpu5m:       0.5,
				MaxCpu5m:       1,
				DeltaQueries2m: true,
				JobsRunning:    0,
				ConnsOpen:      1,
				Uptime:         600,
				FillRatio:      1.0,
			},
			podsBefore: 5,
			podsAfter:  1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			podsAfter := calculateClampedPods(tc.podsBefore, tc.maxPods, tc.metrics)
			require.Equal(t, tc.podsAfter, podsAfter)
		})
	}
}

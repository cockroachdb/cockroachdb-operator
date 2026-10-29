package metrics

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/stretchr/testify/require"
)

func lowerIntervalsForTest() func() {
	// lower the intervals so the test runs faster
	p, d := metricPollInterval, deltaNewConnOrQueryWindow
	m, a := movingMaxSlidingWindow, movingAvgSlidingWindow
	metricPollInterval = 100 * time.Millisecond
	deltaNewConnOrQueryWindow = 20 * metricPollInterval
	movingMaxSlidingWindow = 20 * metricPollInterval
	movingAvgSlidingWindow = 20 * metricPollInterval
	return func() {
		// Restore after the test is over
		metricPollInterval, deltaNewConnOrQueryWindow = p, d
		movingMaxSlidingWindow, movingAvgSlidingWindow = m, a
	}
}

func TestTenantMetricManager(t *testing.T) {
	testutils.GoroutineLeakCheck(t)

	defer lowerIntervalsForTest()()

	pod1, pod1Addr := NewMockMetricSource(t)
	defer func() { require.NoError(t, pod1.Shutdown()) }()
	pod2, pod2Addr := NewMockMetricSource(t)
	defer func() { require.NoError(t, pod2.Shutdown()) }()

	ctx := t.Context()

	mgr := NewTenantManager(ctx)

	// Add a pod with the mock metric source above.
	key := TenantKey{"namespace", v1beta1.TenantID(5)}
	mgr.addPodToMonitor("pod1", key, pod1Addr)

	// Wait until tracker has the metrics.
	pod1.JobsRunningNonIdle.Store(28)
	pod1.SqlQueryCount.Store(2)
	pod1.SqlConns.Store(5)
	pod1.ReportLoadPct.Store(50)
	var metrics *TenantMetrics
	require.Eventually(t, func() bool {
		metrics = mgr.Get(key)
		return metrics != nil &&
			math.Abs(metrics.MaxCpu5m*100-float64(pod1.ReportLoadPct.Load())) < 1
	}, 3*metricPollInterval, metricPollInterval/10)
	require.EqualValues(t, pod1.JobsRunningNonIdle.Load(), metrics.JobsRunning)
	require.EqualValues(t, pod1.SqlConns.Load(), metrics.ConnsOpen)
	require.EqualValues(t, true, metrics.DeltaQueries2m)
	require.EqualValues(t, false, metrics.DeltaNewConns2m)

	// Increase the reported CPU load and ensure that the tracker sees it.
	pod1.JobsRunningNonIdle.Store(14)
	pod1.SqlQueryCount.Store(3)
	pod1.SqlConns.Store(1)
	pod1.ReportLoadPct.Store(75)
	pod1.SqlNewConns.Store(1)
	require.Eventually(t, func() bool {
		metrics = mgr.Get(key)
		return metrics != nil &&
			math.Abs(metrics.MaxCpu5m*100-float64(pod1.ReportLoadPct.Load())) < 1
	}, 3*metricPollInterval, metricPollInterval/10)
	require.EqualValues(t, pod1.JobsRunningNonIdle.Load(), metrics.JobsRunning)
	require.EqualValues(t, pod1.SqlConns.Load(), metrics.ConnsOpen)
	require.EqualValues(t, true, metrics.DeltaQueries2m)
	require.EqualValues(t, true, metrics.DeltaNewConns2m)

	// add a second pod
	pod2.JobsRunningNonIdle.Store(1)
	pod2.SqlQueryCount.Store(10)
	pod2.SqlConns.Store(20)
	pod2.SqlNewConns.Store(20)
	pod2.ReportLoadPct.Store(25)
	mgr.addPodToMonitor("pod2", key, pod2Addr)

	// Check for the load to get to pod1 + pod2 load
	require.Eventually(t, func() bool {
		metrics = mgr.Get(key)
		return metrics != nil &&
			math.Abs(metrics.MaxCpu5m*100-float64(pod1.ReportLoadPct.Load()+pod2.ReportLoadPct.Load())) < 1
	}, 3*metricPollInterval, metricPollInterval/10)
	require.EqualValues(t, pod1.JobsRunningNonIdle.Load()+pod2.JobsRunningNonIdle.Load(), metrics.JobsRunning)
	require.EqualValues(t, pod1.SqlConns.Load()+pod2.SqlConns.Load(), metrics.ConnsOpen)
	require.EqualValues(t, true, metrics.DeltaQueries2m)
	require.EqualValues(t, true, metrics.DeltaNewConns2m)

	// Wait until the query metric within last 2m (5s in the test) reports false
	time.Sleep(deltaNewConnOrQueryWindow + 3*metricPollInterval)

	metrics = mgr.Get(key)
	require.InEpsilon(t, pod1.ReportLoadPct.Load()+pod2.ReportLoadPct.Load(), metrics.MaxCpu5m*100, 0.01)
	require.InEpsilon(t, pod1.ReportLoadPct.Load()+pod2.ReportLoadPct.Load(), metrics.AvgCpu5m*100, 0.01)
	require.EqualValues(t, pod1.JobsRunningNonIdle.Load()+pod2.JobsRunningNonIdle.Load(), metrics.JobsRunning)
	require.EqualValues(t, pod1.SqlConns.Load()+pod2.SqlConns.Load(), metrics.ConnsOpen)
	require.EqualValues(t, false, metrics.DeltaQueries2m)
	require.EqualValues(t, false, metrics.DeltaNewConns2m)

	oldLoad := pod1.ReportLoadPct.Load() + pod2.ReportLoadPct.Load()
	pod2.ReportLoadPct.Store(400)
	newLoad := pod1.ReportLoadPct.Load() + pod2.ReportLoadPct.Load()
	pod2.SqlQueryCount.Store(11)
	pod2.SqlNewConns.Store(21)
	time.Sleep(deltaNewConnOrQueryWindow / 4)

	metrics = mgr.Get(key)
	require.InEpsilon(t, newLoad, metrics.MaxCpu5m*100, 1)
	// 3/4 of the time at 75+25 pct and 1/4 of the time at 75+400
	act := float64(oldLoad*3/4 + newLoad*1/4)
	exp := metrics.AvgCpu5m * 100
	require.InEpsilonf(t, act, exp, 0.15,
		"%f vs %f -> eps %f\n", act, exp, math.Abs(exp-act)/math.Abs(exp))
	require.EqualValues(t, true, metrics.DeltaQueries2m)
	require.EqualValues(t, true, metrics.DeltaNewConns2m)

	// delete pods, wait for the metrics to be cleaned up
	mgr.deletePodFromMonitor("pod1")
	mgr.deletePodFromMonitor("pod2")
	require.Eventually(t, func() bool {
		return mgr.Get(key) == nil
	}, 3*metricPollInterval, metricPollInterval/10)

	// add the pod again, ensure the metrics are reported
	mgr.addPodToMonitor("pod1", key, pod1Addr)
	require.Eventually(t, func() bool {
		metrics = mgr.Get(key)
		return metrics != nil &&
			math.Abs(metrics.MaxCpu5m*100-float64(pod1.ReportLoadPct.Load())) < 1
	}, 3*metricPollInterval, metricPollInterval/10)
	require.EqualValues(t, pod1.JobsRunningNonIdle.Load(), metrics.JobsRunning)

	// simulate a process restart that zeroes accumulating CPU usage
	pod1.Reset()
	time.Sleep(2 * metricPollInterval)
	require.Eventually(t, func() bool {
		metrics = mgr.Get(key)
		return metrics != nil &&
			math.Abs(metrics.MaxCpu5m*100-float64(pod1.ReportLoadPct.Load())) < 1
	}, 3*metricPollInterval, metricPollInterval/10)

	// Ending the test here will cause a context cancellation, that should
	// result in terminating all go routines and will appease the leak test
	// detector
}

func TestTenantMetricManagerCallback(t *testing.T) {
	testutils.GoroutineLeakCheck(t)

	defer lowerIntervalsForTest()()

	key := TenantKey{"namespace", v1beta1.TenantID(5)}
	pod1, pod1Addr := NewMockMetricSource(t)
	defer func() { defer pod1.Shutdown() }()

	ctx := t.Context()

	var latestMetrics *TenantMetrics
	var latestMetricsUpdateCount atomic.Uint64
	var latestMetricsMu sync.Mutex
	mgr := NewTenantManager(ctx,
		WithUpdateFunc(func(ctx context.Context, metrics *TenantMetrics) {
			latestMetricsMu.Lock()
			defer latestMetricsMu.Unlock()
			latestMetricsUpdateCount.Add(1)
			latestMetrics = metrics
		}))

	pod1.ReportLoadPct.Store(10)
	pod1.SqlQueryCount.Store(20)
	pod1.JobsRunningNonIdle.Store(28)
	pod1.SqlConns.Store(71)
	pod1.SqlNewConns.Store(71)
	mgr.addPodToMonitor("pod1", key, pod1Addr)

	// Wait for enough updates to make the Avg be half of the CPU load
	require.Eventually(t, func() bool {
		latestMetricsMu.Lock()
		defer latestMetricsMu.Unlock()
		if latestMetrics != nil && latestMetrics.AvgCpu5m*100 >= 5 {
			require.InEpsilon(t, 10, latestMetrics.MaxCpu5m*100, 0.01)
			// Aty least 10 cycles needed to get to half CPU load given the
			// moving window for the test is set to 20
			require.LessOrEqual(t, uint64(10), latestMetricsUpdateCount.Load())
			require.True(t, latestMetrics.DeltaQueries2m)
			require.True(t, latestMetrics.DeltaNewConns2m)
			require.EqualValues(t, 28, latestMetrics.JobsRunning)
			require.EqualValues(t, 71, latestMetrics.ConnsOpen)
			return true
		}
		return false
	}, movingAvgSlidingWindow, metricPollInterval)

	mgr.deletePodFromMonitor("pod1")

	time.Sleep(metricPollInterval)
	// Updates will not happen anymore
	updCount := latestMetricsUpdateCount.Load()
	time.Sleep(3 * metricPollInterval)
	require.Equal(t, updCount, latestMetricsUpdateCount.Load())
}

func TestTenantMetricManagerBadPod(t *testing.T) {
	testutils.GoroutineLeakCheck(t)

	defer lowerIntervalsForTest()()

	pod1, pod1Addr := NewMockMetricSource(t)
	defer func() { defer pod1.Shutdown() }()

	pod2, pod2Addr := NewMockMetricSource(t)

	ctx := t.Context()

	mgr := NewTenantManager(ctx)

	// Add a pod with the mock metric source above.
	key := TenantKey{"namespace", v1beta1.TenantID(5)}
	mgr.addPodToMonitor("pod1", key, pod1Addr)

	// Wait until tracker has the metrics.
	pod1.JobsRunningNonIdle.Store(28)
	pod1.SqlQueryCount.Store(2)
	pod1.SqlConns.Store(5)
	pod1.SqlNewConns.Store(5)
	pod1.ReportLoadPct.Store(50)
	var metrics *TenantMetrics
	require.Eventually(t, func() bool {
		metrics = mgr.Get(key)
		return metrics != nil &&
			math.Abs(metrics.MaxCpu5m*100-float64(pod1.ReportLoadPct.Load())) < 1
	}, 3*metricPollInterval, metricPollInterval/10)
	require.EqualValues(t, pod1.JobsRunningNonIdle.Load(), metrics.JobsRunning)
	require.EqualValues(t, pod1.SqlConns.Load(), metrics.ConnsOpen)
	require.EqualValues(t, true, metrics.DeltaQueries2m)
	require.EqualValues(t, true, metrics.DeltaNewConns2m)

	// add a second pod
	pod2.JobsRunningNonIdle.Store(1)
	pod2.SqlQueryCount.Store(10)
	pod2.SqlConns.Store(20)
	pod2.SqlNewConns.Store(20)
	pod2.ReportLoadPct.Store(25)
	mgr.addPodToMonitor("pod2", key, pod2Addr)

	// Check for the load to get to pod1 + pod2 load
	require.Eventually(t, func() bool {
		metrics = mgr.Get(key)
		return metrics != nil &&
			math.Abs(metrics.MaxCpu5m*100-float64(pod1.ReportLoadPct.Load()+pod2.ReportLoadPct.Load())) < 1
	}, 3*metricPollInterval, metricPollInterval/10)
	require.EqualValues(t, pod1.JobsRunningNonIdle.Load()+pod2.JobsRunningNonIdle.Load(), metrics.JobsRunning)
	require.EqualValues(t, pod1.SqlConns.Load()+pod2.SqlConns.Load(), metrics.ConnsOpen)
	require.EqualValues(t, true, metrics.DeltaQueries2m)
	require.EqualValues(t, true, metrics.DeltaNewConns2m)

	// Create a third pod and close immediately before adding. This should cause
	// errors when fetching metrics but shouldn't affect the rest of the metrics
	// obtained from the other pods.
	pod3, pod3Addr := NewMockMetricSource(t)
	require.NoError(t, pod3.Shutdown())

	mgr.addPodToMonitor("pod3", key, pod3Addr)

	time.Sleep(3 * metricPollInterval)

	metrics = mgr.Get(key)
	require.InEpsilon(t, float64(pod1.ReportLoadPct.Load()+pod2.ReportLoadPct.Load()), metrics.MaxCpu5m*100, 0.01)
	require.EqualValues(t, pod1.JobsRunningNonIdle.Load()+pod2.JobsRunningNonIdle.Load(), metrics.JobsRunning)
	require.EqualValues(t, pod1.SqlConns.Load()+pod2.SqlConns.Load(), metrics.ConnsOpen)
	require.EqualValues(t, true, metrics.DeltaQueries2m)
	require.EqualValues(t, true, metrics.DeltaNewConns2m)

	// Close the pod 2 endpoint without removing it and make sure the metrics
	// change to reflect pod1
	require.NoError(t, pod2.Shutdown())
	time.Sleep(3 * metricPollInterval)
	metrics = mgr.Get(key)
	require.InEpsilon(t, float64(pod1.ReportLoadPct.Load()+pod2.ReportLoadPct.Load()), metrics.MaxCpu5m*100, 0.01)
	require.EqualValues(t, pod1.JobsRunningNonIdle.Load(), metrics.JobsRunning)
	require.EqualValues(t, pod1.SqlConns.Load(), metrics.ConnsOpen)
	require.EqualValues(t, true, metrics.DeltaQueries2m)
	require.EqualValues(t, true, metrics.DeltaNewConns2m)
}

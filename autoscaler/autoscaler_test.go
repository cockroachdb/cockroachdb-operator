package autoscaler

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	opmetrics "github.com/cockroachlabs/cockroach-enterprise-operator/controllers/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbtest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAutoscale(t *testing.T) {
	if testing.Verbose() {
		logging.SetGlobalLogger(zaptest.NewLogger(t))
	}

	testutils.GoroutineLeakCheck(t)

	// Create three tenants in a fake Ctl store.
	var initObjs []kube.Object
	for _, tenantID := range []crdbv1beta1.TenantID{10, 20, 30, 40, 50} {
		ns := fmt.Sprintf("ns-%d", tenantID)
		tenant := &crdbv1beta1.CrdbTenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tenantID.Tag(),
				Namespace: ns,
			},
			Spec: crdbv1beta1.CrdbTenantSpec{
				TenantID: tenantID,
				Pods:     5,
			},
		}

		// Set MaxPods on tenant 40 and TenantPool on tenant 50.
		if tenantID == 40 {
			tenant.Spec.MaxPods = new(int32(6))
		}
		if tenantID == 50 {
			tenant.Spec.TenantPool = new("some-pool")
		}

		initObjs = append(initObjs, tenant)
	}

	// Create tenant pool for tenant 50 in fake Ctl store.
	initObjs = append(initObjs, &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns-50", Name: "cluster-1"},
		Spec: crdbv1beta1.CrdbClusterSpec{
			TenantHost: &crdbv1beta1.TenantHost{
				TenantPools: []crdbv1beta1.TenantPool{
					{Name: "some-pool", MaxPods: new(int32(4))},
				},
			},
		},
	})

	t10 := metrics.TenantKey{Namespace: "ns-10", TenantID: 10}
	t20 := metrics.TenantKey{Namespace: "ns-20", TenantID: 20}
	t30 := metrics.TenantKey{Namespace: "ns-30", TenantID: 30}
	t40 := metrics.TenantKey{Namespace: "ns-40", TenantID: 40}
	t50 := metrics.TenantKey{Namespace: "ns-50", TenantID: 50}
	testCases := []struct {
		desc       string
		batch      []*metrics.TenantMetrics
		maxPods    *int32
		tenantPool string
		willUpdate []bool
		updates    []int
	}{
		{
			// 10: Average determines the number of pods.
			// 20: Max determines the number of pods.
			desc: "avg-max",
			batch: []*metrics.TenantMetrics{
				{
					Key:            t10,
					AvgCpu5m:       0.5,
					MaxCpu5m:       1,
					DeltaQueries2m: true,
					JobsRunning:    0,
					ConnsOpen:      1,
					Uptime:         200,
					FillRatio:      1.0,
				},
				{
					Key:            t20,
					AvgCpu5m:       0.5,
					MaxCpu5m:       6,
					DeltaQueries2m: true,
					JobsRunning:    0,
					ConnsOpen:      2,
					Uptime:         50,
					FillRatio:      1.0,
				},
			},
			willUpdate: []bool{true, true},
			updates:    []int{1, 2},
		},
		{
			// 10: More than 10% change, but number of pods doesn't change.
			// 20: Change is less than or equal to 10%.
			desc: "no updates",
			batch: []*metrics.TenantMetrics{
				{
					Key:            t10,
					AvgCpu5m:       1,
					MaxCpu5m:       2,
					DeltaQueries2m: false,
					JobsRunning:    0,
					ConnsOpen:      0,
					Uptime:         50,
				},
				{
					Key:            t20,
					AvgCpu5m:       0.5,
					MaxCpu5m:       5.4,
					DeltaQueries2m: false,
					JobsRunning:    0,
					ConnsOpen:      2,
					Uptime:         360,
				},
			},
			willUpdate: []bool{false, false},
		},
		{
			// Same as previous case, but in other direction.
			// 10: More than 10% change, but number of pods doesn't change.
			// 20: Change is less than or equal to 10%.
			desc: "no updates again",
			batch: []*metrics.TenantMetrics{
				{
					Key:            t10,
					AvgCpu5m:       0.1,
					MaxCpu5m:       0.1,
					DeltaQueries2m: false,
					JobsRunning:    0,
					ConnsOpen:      0,
					Uptime:         50,
				},
				{
					Key:            t20,
					AvgCpu5m:       0.5,
					MaxCpu5m:       6.6,
					DeltaQueries2m: false,
					JobsRunning:    0,
					ConnsOpen:      2,
					Uptime:         360,
				},
			},
			willUpdate: []bool{false, false},
		},
		{
			// 10: More than 10% change due to avg, with number of pods changing.
			// 20: More than 10% change due to max, with number of pods changing.
			desc: "avg-max scaling",
			batch: []*metrics.TenantMetrics{
				{
					Key:            t10,
					AvgCpu5m:       1.1,
					MaxCpu5m:       1.1,
					DeltaQueries2m: true,
					JobsRunning:    0,
					ConnsOpen:      0,
					Uptime:         100,
				},
				{
					Key:            t20,
					AvgCpu5m:       0.5,
					MaxCpu5m:       6.7,
					DeltaQueries2m: false,
					JobsRunning:    0,
					ConnsOpen:      1,
					Uptime:         200,
				},
			},
			willUpdate: []bool{true, true},
			updates:    []int{2, 3},
		},
		{
			// 10: Suspend tenant.
			// 20: Can't suspend tenant due to non-zero query count.
			// 30: Can't suspend tenant due to running job.
			// 40: Can't suspend tenant due to long-running transaction.
			// 50: Can't suspend tenant due to pods not existing 6m ago.
			desc: "suspend",
			batch: []*metrics.TenantMetrics{
				{
					Key:            t10,
					AvgCpu5m:       0.01,
					MaxCpu5m:       0.03,
					DeltaQueries2m: false,
					JobsRunning:    0,
					ConnsOpen:      0,
					Uptime:         360,
					FillRatio:      1.0,
				},
				{
					Key:            t20,
					AvgCpu5m:       0.01,
					MaxCpu5m:       0.03,
					DeltaQueries2m: true,
					JobsRunning:    0,
					ConnsOpen:      0,
					Uptime:         360,
					FillRatio:      1.0,
				},
				{
					Key:            t30,
					AvgCpu5m:       0.02,
					MaxCpu5m:       0.5,
					DeltaQueries2m: false,
					JobsRunning:    1,
					ConnsOpen:      0,
					Uptime:         360,
					FillRatio:      1.0,
				},
				{
					Key:            t40,
					AvgCpu5m:       0.02,
					MaxCpu5m:       0.5,
					DeltaQueries2m: false,
					JobsRunning:    0,
					ConnsOpen:      1,
					Uptime:         360,
					FillRatio:      1.0,
				},
				{
					Key:            t50,
					AvgCpu5m:       0.02,
					MaxCpu5m:       0.5,
					DeltaQueries2m: false,
					JobsRunning:    0,
					ConnsOpen:      0,
					Uptime:         359,
					FillRatio:      1.0,
				},
			},
			willUpdate: []bool{true, true, true, true, true},
			updates:    []int{0, 1, 1, 1, 1},
		},
		{
			// 10: Resume tenant.
			desc: "resume",
			batch: []*metrics.TenantMetrics{
				{
					Key:            t10,
					AvgCpu5m:       0,
					MaxCpu5m:       0,
					DeltaQueries2m: false,
					JobsRunning:    0,
					ConnsOpen:      0,
					Uptime:         10,
				},
			},
			willUpdate: []bool{true},
			updates:    []int{1},
		},
		{
			// 40: Has CrdbTenant.MaxPods=6.
			// 50: Has CrdbTenant.TenantPool.MaxPods=4.
			desc: "max pods",
			batch: []*metrics.TenantMetrics{
				{
					Key:            t40,
					AvgCpu5m:       5.5,
					MaxCpu5m:       25.2,
					DeltaQueries2m: true,
					JobsRunning:    10,
					ConnsOpen:      10,
					Uptime:         1000,
				},
				{
					Key:            t50,
					AvgCpu5m:       10,
					MaxCpu5m:       10,
					DeltaQueries2m: true,
					JobsRunning:    0,
					ConnsOpen:      5,
					Uptime:         50,
				},
			},
			willUpdate: []bool{true, true},
			updates:    []int{6, 4},
		},
		{
			// 10: Scale up significantly.
			// Other tenants no longer have metrics.
			desc: "scale-up",
			batch: []*metrics.TenantMetrics{
				{
					Key:            t10,
					AvgCpu5m:       5.5,
					MaxCpu5m:       25.2,
					DeltaQueries2m: true,
					JobsRunning:    10,
					ConnsOpen:      10,
					Uptime:         1000,
				},
			},
			willUpdate: []bool{true},
			updates:    []int{9},
		},
	}

	// Set up the fake watcher to send modify events after we use ctl.
	// This way the tenantCache always uses GetResource to retrieve objects.
	// Otherwise, the autoscaler keeps getting stale copies of the tenant.
	// And, in "no updates" cases it incorrectly make a call to ctl.Apply.
	fakeWatcher := watch.NewFake()
	mapper := kubecrdbtest.NewRESTMapper()
	opts := kube.Options{Mapper: mapper, AfterMutation: func(object kube.Object) error {
		fakeWatcher.Modify(object)
		return nil
	}}
	ctl := kubetest.NewFakeCtl(kubetest.Options{Options: opts}, initObjs...)

	// Run the autoscaler until it scales the tenants to the desired pod count.
	ctx := t.Context()
	eventRecorder := record.NewFakeRecorder(100)
	scaler := New(ctx, Options{
		Ctl: ctl,
		Emitter: kube.EventEmitter{
			EventRecorder: eventRecorder,
		},
	})
	mgr := &metrics.TestTenantManager{
		TenantMetricsMap: metrics.TenantMetricsMap{},
		UpdateFunc: func(ctx context.Context, metrics *metrics.TenantMetrics) {
			_ = scaler.updateDesiredPods(ctx, metrics)
		},
	}
	scaler.metricsManager = mgr
	// Override GetResource to fetch from the fake ctl.
	scaler.tenantCache.GetResource = func(
		ctx context.Context, key client.ObjectKey, options *metav1.ListOptions,
	) (kube.Object, error) {
		var tenant crdbv1beta1.CrdbTenant
		err := ctl.WithNamespace(key.Namespace).Get(ctx, key.Name, &tenant)
		return &tenant, err
	}

	// Override WatchResources to return a fake watcher.
	scaler.tenantCache.WatchResources = func(
		ctx context.Context, options *metav1.ListOptions,
	) (watch.Interface, error) {
		return fakeWatcher, nil
	}

	go func() {
		err := scaler.Start(ctx)
		if err != nil {
			t.Errorf("error received from Autoscaler.Start: %v", err)
			return
		}
	}()

	// Get starting values of metrics (they might not be zero if other tests ran
	// before this, or if this test is being run many times using count=N).
	tenantsScaledSuccessCounter := opmetrics.TenantsScaledTotal.With(opmetrics.SuccessLabel)
	tenantsScaledTotal := testutil.ToFloat64(tenantsScaledSuccessCounter)

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			tenantsScaledTotal += float64(len(tc.updates))
			mgr.UpdateMetrics(ctx, tc.batch)
			for i, willUpdate := range tc.willUpdate {
				if !willUpdate {
					continue
				}
				tenantID := tc.batch[i].Key.TenantID
				pods := tc.updates[i]
				ns := fmt.Sprintf("ns-%d", tenantID)
				var tenant crdbv1beta1.CrdbTenant
				require.NoError(t, ctl.WithNamespace(ns).Get(ctx, tenantID.Tag(), &tenant))
				require.Equal(t, int32(pods), tenant.Spec.Pods)
				event := <-eventRecorder.Events
				require.True(t, strings.HasPrefix(event, "Normal Scale"))
				require.Contains(t, event, fmt.Sprintf("->%d ", pods))
				metrics := tc.batch[i]
				if metrics.MaxCpu5m > 0 {
					require.Contains(t, event, fmt.Sprintf("->%d MaxCpu5m ", pods))
				}
			}
		})
	}

	// Do another round of metrics, but make the autoscaler error when getting the tenant.
	// Then test for the TenantsScaledTotal{"result": "error"} metric.

	tenantsScaledErrorCounter := opmetrics.TenantsScaledTotal.With(opmetrics.ErrorLabel)
	tenantsScaledErrors := testutil.ToFloat64(tenantsScaledErrorCounter) + 1

	scaler.tenantCache.GetResource = func(
		ctx context.Context, key client.ObjectKey, options *metav1.ListOptions,
	) (kube.Object, error) {
		return nil, errors.New("foobar")
	}
	m := &metrics.TenantMetrics{
		Key:            metrics.TenantKey{Namespace: "abc"},
		AvgCpu5m:       0.5,
		MaxCpu5m:       1,
		DeltaQueries2m: true,
		JobsRunning:    0,
		ConnsOpen:      1,
		Uptime:         200,
	}
	scaler.updateDesiredPods(ctx, m)
	require.Equal(t, tenantsScaledErrors, testutil.ToFloat64(tenantsScaledErrorCounter))
	// The success metric should remain the same.
	testutils.RequireMetricEqual(t, opmetrics.TenantsScaledTotal.With(opmetrics.SuccessLabel), int(tenantsScaledTotal))

	// Regress bug where autoscaler was setting Tenant.MaxPods when
	// TenantPool.MaxPods is defined.
	var tenant crdbv1beta1.CrdbTenant
	require.NoError(t, ctl.WithNamespace("ns-50").Get(ctx, "tenant-50", &tenant))
	require.Nil(t, tenant.Spec.MaxPods)
}

func TestSetMinimumScale(t *testing.T) {
	type tc struct {
		specBefore int32
		maxPods    *int32
		request    int32
		specAfter  int32
		expectErr  bool
	}
	tests := []tc{
		{
			specBefore: 0,
			request:    1,
			specAfter:  1,
		},
		{
			specBefore: 0,
			request:    100,
			specAfter:  100,
		},
		{
			specBefore: 0,
			request:    20,
			maxPods:    new(int32(20)),
			specAfter:  20,
		},
		{
			specBefore: 0,
			request:    10,
			maxPods:    new(int32(0)),
			specAfter:  0,
			expectErr:  true,
		},
		{
			specBefore: 10,
			request:    5,
			specAfter:  10,
		},
	}
	for _, tc := range tests {
		crdbTenant := &crdbv1beta1.CrdbTenant{}
		crdbTenant.Spec.Pods = tc.specBefore
		crdbTenant.Spec.MaxPods = tc.maxPods
		err := SetMinimumScale(crdbTenant, tc.request)
		if tc.expectErr {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
		require.Equal(t, crdbTenant.Spec.Pods, tc.specAfter)
	}
}

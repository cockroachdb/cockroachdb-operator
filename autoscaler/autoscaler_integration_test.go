package autoscaler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubenet"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/testenv"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// TestIntegrationAutoscale runs a real instance of the operator +
// Prometheus in a K3D cluster, and ensures auto-scaling works end-to-end.
func TestIntegrationAutoscale(t *testing.T) {
	// This test cannot be easily recorded because Prometheus records timestamps
	// along with metrics and those become part of requests/responses, which is
	// not idempotent.
	testutils.MarkIntegrationTest(t)

	ctx := context.Background()

	env := testenv.New(t, testenv.SingleZoneConfig)
	crdbCluster := testenv.CreateCrdbCluster(
		t,
		env,
		"test-cluster",
		&crdbv1beta1.CrdbClusterSpec{
			TLSEnabled: true,
			TenantHost: &crdbv1beta1.TenantHost{
				TenantPools: []crdbv1beta1.TenantPool{
					{Name: "prod", MaxPods: new(int32(5))},
					{Name: "test", MaxPods: new(int32(3))},
				},
			},
		})
	ctl := env.DefaultCluster().Ctl.WithNamespace(crdbCluster.Namespace)

	t.Run("auto-scale down", func(t *testing.T) {
		// Create tenant with three pods.
		const tenantID = crdbv1beta1.TenantID(10)
		_ = testenv.CreateCrdbTenant(t, ctl, "auto-scale", &crdbv1beta1.CrdbTenantSpec{
			TenantID:   tenantID,
			Pods:       3,
			TenantPool: new("prod"),
		})

		db := testenv.OpenKubeSql(t, ctl, kubenet.Options{TenantID: tenantID})
		defer db.Close()

		require.Eventually(t, func() bool {
			// Run a query against the tenant so that there is traffic.
			_, _ = db.Exec("SELECT 1")

			// Wait until the autoscaler has scaled down to 1 pod.
			var tenant crdbv1beta1.CrdbTenant
			require.NoError(t, ctl.Get(ctx, tenantID.Tag(), &tenant))
			return tenant.Spec.Pods == 1
		}, time.Minute*6, time.Millisecond*100)
	})

	t.Run("autoscaler metrics", func(t *testing.T) {
		// Create tenant with one pod.
		const tenantID = crdbv1beta1.TenantID(20)
		tenant := testenv.CreateCrdbTenant(t, ctl, "auto-scale", &crdbv1beta1.CrdbTenantSpec{
			TenantID:   tenantID,
			Pods:       1,
			MaxPods:    new(int32(1)),
			TenantPool: new("test"),
		})
		require.NoError(t, ctl.Apply(ctx, tenant))

		// Run a query.
		db := testenv.OpenKubeSql(t, ctl, kubenet.Options{TenantID: tenantID})
		defer db.Close()

		// Start a transaction.
		_, err := db.Exec("BEGIN TRANSACTION")
		require.NoError(t, err)
		_, err = db.Exec("SELECT 1")
		require.NoError(t, err)

		// Put some load on the CPUs.
		ctx, cancel := context.WithCancel(ctx)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				default:
					_, err = db.Exec("select crypt(uuid_v4()::text,gen_salt('bf',10))")
					require.NoError(t, err)
				}
			}
		}()
		defer cancel()

		// Get ending metrics and ensure they've changed as expected. This
		// takes time, since we have to wait for the new metrics to be generated
		// and the autoscaler to query them.
		metricsReq := getTenantMetrics(t, ctl, tenant)
		tenantKey := metrics.TenantKey{Namespace: tenant.Namespace, TenantID: tenantID}
		require.Eventually(t, func() bool {
			m, pods := metricsReq(t, tenantKey)
			if m == nil {
				return false
			}

			logging.Infof(ctx, "metrics: %v, pod count: %d", m, pods)
			if m.AvgCpu5m < 0.1 {
				return false
			}
			if m.MaxCpu5m < 0.5 {
				return false
			}
			if m.ConnsOpen != 1 {
				return false
			}
			if pods == 0 {
				return false
			}
			return true
		}, 2*time.Minute, 10*time.Second)
	})
}

func getTenantMetrics(
	t *testing.T, ctl *kube.Ctl, tenant *crdbv1beta1.CrdbTenant,
) func(t *testing.T, key metrics.TenantKey) (*metrics.TenantMetrics, int) {
	// Get an operator pod.
	ctx := context.Background()
	var pods corev1.PodList
	selector := kube.ParseLabelOrDie(fmt.Sprintf("app=%s", manifests.OperatorAppLabel))
	ctl = ctl.WithNamespace(manifests.OperatorNamespace)
	require.NoError(t, ctl.List(ctx, &kube.ListOptions{LabelSelector: selector}, &pods))
	require.Greater(t, len(pods.Items), 0)

	// Tunnel into an operator pod using the pod dialer.
	dialer, err := kube.NewPodDialer(ctl.RestConfig(), manifests.OperatorNamespace)
	require.NoError(t, err)

	// Construct HTTP client to scrape metrics.
	client := http.Client{
		Transport: &http.Transport{
			DialContext: dialer.DialContext,
			// Rather than reusing TCP connections, we will close them once the
			// HTTP request is done to prevent accidental leakage of goroutines.
			DisableKeepAlives: true,
		},
	}
	return func(t *testing.T, key metrics.TenantKey) (*metrics.TenantMetrics, int) {
		resp, err := client.Get(fmt.Sprintf("http://%s:8080/metrics", pods.Items[0].GetName()))
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, 200, resp.StatusCode)

		var parser expfmt.TextParser
		metricFamiliesByName, err := parser.TextToMetricFamilies(resp.Body)
		require.NoError(t, err)
		samples := model.Vector{}
		opts := &expfmt.DecodeOptions{}
		for _, mf := range metricFamiliesByName {
			mfs, err := expfmt.ExtractSamples(opts, mf)
			require.NoError(t, err)
			samples = append(samples, mfs...)
		}
		m := &metrics.TenantMetrics{}
		pods := 0
		for _, sample := range samples {
			if string(sample.Metric["tenant_id"]) == key.TenantID.String() &&
				string(sample.Metric["namespace"]) == key.Namespace {
				switch sample.Metric["__name__"] {
				case "autoscaler_cpu_avg":
					m.AvgCpu5m = float64(sample.Value)
				case "autoscaler_cpu_max":
					m.MaxCpu5m = float64(sample.Value)
				case "autoscaler_conns":
					m.ConnsOpen = float64(sample.Value)
				case "autoscaler_jobs":
					m.JobsRunning = float64(sample.Value)
				case "autoscaler_pods":
					pods = int(sample.Value)
				}
			}
		}
		return m, pods
	}
}

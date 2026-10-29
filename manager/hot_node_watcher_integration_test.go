package manager_test

import (
	"context"
	"errors"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manager"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/tenant"
	"github.com/cockroachlabs/cockroach-enterprise-operator/testenv"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// TestIntegrationHotNodeWatcher validates the hot node watcher
// end-to-end against a live k3d cluster. It creates a host cluster with the
// hot-node-auto-remediation feature enabled, stamps a tenant pod, then
// injects fake node metrics to simulate a hot node. The test verifies that
// the correct pod is marked stale.
func TestIntegrationHotNodeWatcher(t *testing.T) {
	testutils.MarkIntegrationTest(t)

	ctx := context.Background()

	t.Setenv("HOT_NODE_WATCH_INTERVAL", "1")

	env := testenv.New(t, testenv.MultiZoneConfig)
	cluster := testenv.CreateCrdbCluster(t, env, "hot-node-test", &crdbv1beta1.CrdbClusterSpec{
		TLSEnabled: true,
		TenantHost: &crdbv1beta1.TenantHost{
			TenantPools: []crdbv1beta1.TenantPool{
				{Name: "default", MaxPods: new(int32(3))},
			},
		},
		Features: []crdbv1beta1.ClusterFeature{
			crdbv1beta1.ClusterHotNodeAutoRemediation,
		},
	})

	ctl := env.DefaultCluster().Ctl.WithNamespace(cluster.Namespace)

	const tenantID = crdbv1beta1.TenantID(10)
	testenv.CreateCrdbTenant(t, ctl, "hot-node-test", &crdbv1beta1.CrdbTenantSpec{
		TenantID:         tenantID,
		Pods:             1,
		DisableAutoscale: true,
	})

	var stampedPods corev1.PodList
	require.Eventually(t, func() bool {
		if err := ctl.List(ctx, &kube.ListOptions{
			LabelSelector: manifests.MakeStampedPodsSelector(),
		}, &stampedPods); err != nil {
			return false
		}
		for _, pod := range stampedPods.Items {
			if pod.Status.Phase == corev1.PodRunning {
				return true
			}
		}
		return false
	}, 2*time.Minute, 5*time.Second, "timed out waiting for a stamped pod to be running")

	require.Len(t, stampedPods.Items, 1, "expected exactly one stamped pod")
	hotNodeName := stampedPods.Items[0].Spec.NodeName
	require.NotEmpty(t, hotNodeName, "stamped pod should be scheduled on a node")

	var k8sNodes corev1.NodeList
	require.NoError(t, env.DefaultCluster().Ctl.List(ctx, &kube.ListOptions{}, &k8sNodes))
	require.GreaterOrEqual(t, len(k8sNodes.Items), 3, "need at least 3 k8s nodes")

	// One hot node, all others cold.
	nodeMetrics := metrics.NodeMetricsMap{}
	for _, node := range k8sNodes.Items {
		if node.Name == hotNodeName {
			nodeMetrics[node.Name] = metrics.NodeMetrics{CpuAvg5m: 85.0, Schedulable: true}
		} else {
			nodeMetrics[node.Name] = metrics.NodeMetrics{CpuAvg5m: 40.0, Schedulable: true}
		}
	}
	tenantKey := metrics.TenantKey{Namespace: cluster.Namespace, TenantID: tenantID}
	podMetrics := metrics.PodMetricsMap{}
	for _, pod := range stampedPods.Items {
		// need nonzero CPU usage to pass hot pod selection
		podMetrics[pod.Name] = metrics.PodMetrics{SqlCpuAvg5m: 0.1}
	}
	tenantMetrics := metrics.TenantMetricsMap{
		tenantKey: &metrics.TenantMetrics{
			Key:  tenantKey,
			Pods: podMetrics,
		},
	}

	nmm := &metrics.TestNodeManager{NodeMetricsMap: nodeMetrics}
	tmm := &metrics.TestTenantManager{TenantMetricsMap: tenantMetrics}

	w := manager.NewHotNodeWatcher(env.DefaultCluster().Ctl, nmm, tmm, tenant.NewLockTable())

	watcherCtx := t.Context()
	go func() {
		if err := w.Start(watcherCtx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("error received from HotNodeWatcher.Start: %v", err)
		}
	}()

	require.Eventually(t, func() bool {
		var pods corev1.PodList
		if err := ctl.List(ctx, &kube.ListOptions{
			LabelSelector: manifests.MakeStaleStampedPodsSelector(),
		}, &pods); err != nil {
			return false
		}
		return len(pods.Items) > 0
	}, 30*time.Second, time.Second, "timed out waiting for watcher to mark a pod as stale")

	// the pod that was marked stale should be the one on the hot node
	var stalePods corev1.PodList
	require.NoError(t, ctl.List(ctx, &kube.ListOptions{
		LabelSelector: manifests.MakeStaleStampedPodsSelector(),
	}, &stalePods))
	require.Len(t, stalePods.Items, 1)
	require.Equal(t, hotNodeName, stalePods.Items[0].Spec.NodeName)
}

package manager

import (
	"context"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/tenant"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const testNamespace = "test-ns"

// makeStampedPod returns a stamped tenantpool pod on the given node.
func makeStampedPod(name, nodeName string, tenantID crdbv1beta1.TenantID) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels: map[string]string{
				"app":                         manifests.TenantPoolAppName,
				manifests.TenantIDLabel:       tenantID.String(),
				manifests.TenantPodStateLabel: string(manifests.TenantPodRunningState),
			},
			Annotations: map[string]string{},
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
}

// setupWatcher creates a HotNodeWatcher backed by a fake K8s client. It
// creates a single namespace with one CrdbCluster, and while it's technically
// possible for there to be multiple namespaces with multiple CrdbCluster resources,
// we've assumed that there will only be one in the underlying code.
func setupWatcher(
	t *testing.T,
	ctx context.Context,
	featureEnabled bool,
	nodeMetrics metrics.NodeMetricsMap,
	tenantMetrics metrics.TenantMetricsMap,
	pods ...kube.Object,
) (*HotNodeWatcher, *kube.Ctl) {
	t.Helper()

	ctl := kubetest.NewFakeCtl(kubetest.Options{})
	opts := kube.ApplyOptions{NoWait: true}

	require.NoError(t, ctl.ApplyWithOptions(ctx, opts,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}},
	))

	var features []crdbv1beta1.ClusterFeature
	if featureEnabled {
		features = []crdbv1beta1.ClusterFeature{crdbv1beta1.ClusterHotNodeAutoRemediation}
	}
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-1",
			Namespace: testNamespace,
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Features: features,
			Regions:  []crdbv1beta1.CrdbClusterRegion{{Code: testNamespace, Nodes: 3}},
		},
	}
	require.NoError(t, ctl.WithNamespace(testNamespace).ApplyWithOptions(ctx, opts, cluster))

	if len(pods) > 0 {
		require.NoError(t, ctl.WithNamespace(testNamespace).ApplyWithOptions(ctx, opts, pods...))
	}

	nmm := &metrics.TestNodeManager{NodeMetricsMap: nodeMetrics}
	var tmm metrics.TenantMetricsReader
	if tenantMetrics != nil {
		tmm = &metrics.TestTenantManager{TenantMetricsMap: tenantMetrics}
	} else {
		tmm = &metrics.TestTenantManager{TenantMetricsMap: metrics.TenantMetricsMap{}}
	}

	w := NewHotNodeWatcher(ctl, nmm, tmm, tenant.NewLockTable())
	return w, ctl
}

// getPod fetches a pod from the fake ctl and returns it.
func getPod(t *testing.T, ctx context.Context, ctl *kube.Ctl, name string) *corev1.Pod {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, ctl.WithNamespace(testNamespace).Get(ctx, name, &pod))
	return &pod
}

func TestFindHottestNode(t *testing.T) {
	ctx := context.Background()
	w := &HotNodeWatcher{}

	t.Run("returns hottest node when hot and cold nodes exist", func(t *testing.T) {
		nodeMetrics := metrics.NodeMetricsMap{
			"hot-node":  {CpuAvg5m: 90.0, Schedulable: true},
			"warm-node": {CpuAvg5m: 60.0, Schedulable: true},
			"cool-node": {CpuAvg5m: 40.0, Schedulable: true},
		}
		node, ok := w.findHottestNode(ctx, nodeMetrics)
		require.True(t, ok)
		require.Equal(t, "hot-node", node)
	})

	t.Run("picks the hotter of two hot nodes", func(t *testing.T) {
		nodeMetrics := metrics.NodeMetricsMap{
			"hot-node-a": {CpuAvg5m: 85.0, Schedulable: true},
			"hot-node-b": {CpuAvg5m: 95.0, Schedulable: true},
			"cool-node":  {CpuAvg5m: 30.0, Schedulable: true},
		}
		node, ok := w.findHottestNode(ctx, nodeMetrics)
		require.True(t, ok)
		require.Equal(t, "hot-node-b", node)
	})

	t.Run("returns false when no node exceeds hot threshold", func(t *testing.T) {
		nodeMetrics := metrics.NodeMetricsMap{
			"node-a": {CpuAvg5m: 70.0, Schedulable: true},
			"node-b": {CpuAvg5m: 40.0, Schedulable: true},
		}
		_, ok := w.findHottestNode(ctx, nodeMetrics)
		require.False(t, ok)
	})

	t.Run("returns false when hot node exists but no cold node", func(t *testing.T) {
		nodeMetrics := metrics.NodeMetricsMap{
			"hot-node":  {CpuAvg5m: 85.0, Schedulable: true},
			"warm-node": {CpuAvg5m: 60.0, Schedulable: true},
		}
		_, ok := w.findHottestNode(ctx, nodeMetrics)
		require.False(t, ok)
	})

	t.Run("returns false on empty metrics", func(t *testing.T) {
		_, ok := w.findHottestNode(ctx, metrics.NodeMetricsMap{})
		require.False(t, ok)
	})

	t.Run("node exactly at hot threshold is considered hot", func(t *testing.T) {
		nodeMetrics := metrics.NodeMetricsMap{
			"boundary-node": {CpuAvg5m: 80.0, Schedulable: true},
			"cool-node":     {CpuAvg5m: 30.0, Schedulable: true},
		}
		node, ok := w.findHottestNode(ctx, nodeMetrics)
		require.True(t, ok)
		require.Equal(t, "boundary-node", node)
	})

	t.Run("skips nodes whose moving average is not ready", func(t *testing.T) {
		nodeMetrics := metrics.NodeMetricsMap{
			"hot-node":     {CpuAvg5m: 90.0, MetricsNotReady: true, Schedulable: true},
			"cool-node":    {CpuAvg5m: 30.0, Schedulable: true},
			"another-cool": {CpuAvg5m: 35.0, Schedulable: true},
		}
		_, ok := w.findHottestNode(ctx, nodeMetrics)
		require.False(t, ok)
	})

	t.Run("skips hot nodes that are not schedulable", func(t *testing.T) {
		nodeMetrics := metrics.NodeMetricsMap{
			"hot-node":  {CpuAvg5m: 90.0},
			"cool-node": {CpuAvg5m: 30.0, Schedulable: true},
		}
		_, ok := w.findHottestNode(ctx, nodeMetrics)
		require.False(t, ok)
	})

	t.Run("unschedulable node does not count as a cold landing target", func(t *testing.T) {
		nodeMetrics := metrics.NodeMetricsMap{
			"hot-node":  {CpuAvg5m: 90.0, Schedulable: true},
			"cool-node": {CpuAvg5m: 30.0},
		}
		_, ok := w.findHottestNode(ctx, nodeMetrics)
		require.False(t, ok)
	})

	t.Run("node exactly at cold threshold is not considered cold", func(t *testing.T) {
		nodeMetrics := metrics.NodeMetricsMap{
			"hot-node":      {CpuAvg5m: 85.0, Schedulable: true},
			"boundary-node": {CpuAvg5m: 55.0, Schedulable: true},
		}
		_, ok := w.findHottestNode(ctx, nodeMetrics)
		require.False(t, ok)
	})
}

func TestFindHottestPod(t *testing.T) {
	ctx := context.Background()
	tenantID := crdbv1beta1.TenantID(10)
	tenantKey := metrics.TenantKey{Namespace: testNamespace, TenantID: tenantID}

	t.Run("picks pod with highest CPU", func(t *testing.T) {
		w, _ := setupWatcher(t, ctx, true,
			metrics.NodeMetricsMap{"hot-node": {CpuAvg5m: 85.0}, "cool-node": {CpuAvg5m: 40.0}},
			metrics.TenantMetricsMap{
				tenantKey: &metrics.TenantMetrics{
					Key: tenantKey,
					Pods: metrics.PodMetricsMap{
						"pod-low":  {SqlCpuAvg5m: 0.1},
						"pod-high": {SqlCpuAvg5m: 0.7},
					},
				},
			},
			makeStampedPod("pod-low", "hot-node", tenantID),
			makeStampedPod("pod-high", "hot-node", tenantID),
		)
		ctl := w.ctl.WithNamespace(testNamespace)
		pod, _ := w.findHottestPod(ctx, ctl, "hot-node", testNamespace)
		require.NotNil(t, pod)
		require.Equal(t, "pod-high", pod.Name)
	})

	t.Run("includes draining pods", func(t *testing.T) {
		drainingPod := makeStampedPod("draining-pod", "hot-node", tenantID)
		drainingPod.Labels[manifests.TenantPodStateLabel] = string(manifests.TenantPodDrainingState)

		w, _ := setupWatcher(t, ctx, true,
			metrics.NodeMetricsMap{"hot-node": {CpuAvg5m: 85.0}, "cool-node": {CpuAvg5m: 40.0}},
			metrics.TenantMetricsMap{
				tenantKey: &metrics.TenantMetrics{
					Key: tenantKey,
					Pods: metrics.PodMetricsMap{
						"draining-pod": {SqlCpuAvg5m: 0.9},
						"running-pod":  {SqlCpuAvg5m: 0.1},
					},
				},
			},
			drainingPod,
			makeStampedPod("running-pod", "hot-node", tenantID),
		)
		ctl := w.ctl.WithNamespace(testNamespace)
		pod, _ := w.findHottestPod(ctx, ctl, "hot-node", testNamespace)
		require.NotNil(t, pod)
		require.Equal(t, "draining-pod", pod.Name)
	})

	t.Run("skips pods with empty tenant ID", func(t *testing.T) {
		emptyTenantPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "empty-tenant-pod",
				Namespace: testNamespace,
				Labels: map[string]string{
					"app":                         manifests.TenantPoolAppName,
					manifests.TenantIDLabel:       "",
					manifests.TenantPodStateLabel: string(manifests.TenantPodRunningState),
				},
				Annotations: map[string]string{},
			},
			Spec:   corev1.PodSpec{NodeName: "hot-node"},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}

		w, _ := setupWatcher(t, ctx, true,
			metrics.NodeMetricsMap{"hot-node": {CpuAvg5m: 85.0}, "cool-node": {CpuAvg5m: 40.0}},
			nil,
			emptyTenantPod,
		)
		ctl := w.ctl.WithNamespace(testNamespace)
		pod, _ := w.findHottestPod(ctx, ctl, "hot-node", testNamespace)
		require.Nil(t, pod)
	})

	t.Run("skips deleting pods", func(t *testing.T) {
		now := metav1.Now()
		deletingPod := makeStampedPod("deleting-pod", "hot-node", tenantID)
		deletingPod.DeletionTimestamp = &now

		w, _ := setupWatcher(t, ctx, true,
			metrics.NodeMetricsMap{"hot-node": {CpuAvg5m: 85.0}, "cool-node": {CpuAvg5m: 40.0}},
			metrics.TenantMetricsMap{
				tenantKey: &metrics.TenantMetrics{
					Key:  tenantKey,
					Pods: metrics.PodMetricsMap{"deleting-pod": {SqlCpuAvg5m: 0.9}},
				},
			},
			deletingPod,
		)
		ctl := w.ctl.WithNamespace(testNamespace)
		pod, _ := w.findHottestPod(ctx, ctl, "hot-node", testNamespace)
		require.Nil(t, pod)
	})

	t.Run("returns nil when no pods on node", func(t *testing.T) {
		w, _ := setupWatcher(t, ctx, true,
			metrics.NodeMetricsMap{"hot-node": {CpuAvg5m: 85.0}, "cool-node": {CpuAvg5m: 40.0}},
			nil,
		)
		ctl := w.ctl.WithNamespace(testNamespace)
		pod, _ := w.findHottestPod(ctx, ctl, "hot-node", testNamespace)
		require.Nil(t, pod)
	})

	t.Run("returns nil when tenant metrics unavailable", func(t *testing.T) {
		w, _ := setupWatcher(t, ctx, true,
			metrics.NodeMetricsMap{"hot-node": {CpuAvg5m: 85.0}, "cool-node": {CpuAvg5m: 40.0}},
			metrics.TenantMetricsMap{},
			makeStampedPod("pod-1", "hot-node", tenantID),
		)
		ctl := w.ctl.WithNamespace(testNamespace)
		pod, _ := w.findHottestPod(ctx, ctl, "hot-node", testNamespace)
		require.Nil(t, pod)
	})

	t.Run("returns nil when pod metrics unavailable", func(t *testing.T) {
		w, _ := setupWatcher(t, ctx, true,
			metrics.NodeMetricsMap{"hot-node": {CpuAvg5m: 85.0}, "cool-node": {CpuAvg5m: 40.0}},
			metrics.TenantMetricsMap{
				tenantKey: &metrics.TenantMetrics{
					Key:  tenantKey,
					Pods: metrics.PodMetricsMap{},
				},
			},
			makeStampedPod("pod-1", "hot-node", tenantID),
		)
		ctl := w.ctl.WithNamespace(testNamespace)
		pod, _ := w.findHottestPod(ctx, ctl, "hot-node", testNamespace)
		require.Nil(t, pod)
	})
}

func TestRemediateHotNodes_MarksHighestCPUPodStale(t *testing.T) {
	ctx := context.Background()
	tenantID := crdbv1beta1.TenantID(10)
	tenantKey := metrics.TenantKey{Namespace: testNamespace, TenantID: tenantID}

	nodeMetrics := metrics.NodeMetricsMap{
		"hot-node":  {CpuAvg5m: 85.0, Schedulable: true},
		"cool-node": {CpuAvg5m: 40.0, Schedulable: true},
		"warm-node": {CpuAvg5m: 60.0, Schedulable: true},
	}

	tenantMetrics := metrics.TenantMetricsMap{
		tenantKey: &metrics.TenantMetrics{
			Key: tenantKey,
			Pods: metrics.PodMetricsMap{
				"pod-low":  {SqlCpuAvg5m: 0.1},
				"pod-high": {SqlCpuAvg5m: 0.7},
			},
		},
	}

	w, ctl := setupWatcher(t, ctx, true, nodeMetrics, tenantMetrics,
		makeStampedPod("pod-low", "hot-node", tenantID),
		makeStampedPod("pod-high", "hot-node", tenantID),
	)

	w.remediateHotNodes(ctx)

	podHigh := getPod(t, ctx, ctl, "pod-high")
	require.True(t, manifests.IsTenantPodStale(podHigh), "pod-high should be stale")

	podLow := getPod(t, ctx, ctl, "pod-low")
	require.False(t, manifests.IsTenantPodStale(podLow), "pod-low should not be stale")
}

func TestRemediateHotNodes_SkipsWhenStalePodExists(t *testing.T) {
	ctx := context.Background()
	tenantID := crdbv1beta1.TenantID(10)

	nodeMetrics := metrics.NodeMetricsMap{
		"hot-node":  {CpuAvg5m: 85.0, Schedulable: true},
		"cool-node": {CpuAvg5m: 40.0, Schedulable: true},
		"warm-node": {CpuAvg5m: 60.0, Schedulable: true},
	}

	alreadyStalePod := makeStampedPod("stale-pod", "hot-node", tenantID)
	manifests.MarkTenantPodAsStale(alreadyStalePod)

	freshPod := makeStampedPod("fresh-pod", "hot-node", tenantID)

	w, ctl := setupWatcher(t, ctx, true, nodeMetrics, nil,
		alreadyStalePod,
		freshPod,
	)

	w.remediateHotNodes(ctx)

	pod := getPod(t, ctx, ctl, "fresh-pod")
	require.False(t, manifests.IsTenantPodStale(pod), "fresh pod should not be stale when another stale pod exists")
}

func TestRemediateHotNodes_SkipsWhenFeatureDisabled(t *testing.T) {
	ctx := context.Background()
	tenantID := crdbv1beta1.TenantID(10)

	nodeMetrics := metrics.NodeMetricsMap{
		"hot-node":  {CpuAvg5m: 85.0, Schedulable: true},
		"cool-node": {CpuAvg5m: 40.0, Schedulable: true},
		"warm-node": {CpuAvg5m: 60.0, Schedulable: true},
	}

	w, ctl := setupWatcher(t, ctx, false, nodeMetrics, nil,
		makeStampedPod("pod-1", "hot-node", tenantID),
	)

	w.remediateHotNodes(ctx)

	pod := getPod(t, ctx, ctl, "pod-1")
	require.False(t, manifests.IsTenantPodStale(pod), "no pod should be stale when feature is disabled")
}

func TestRemediateHotNodes_SkipsWhenBelowMinSchedulableNodes(t *testing.T) {
	ctx := context.Background()
	tenantID := crdbv1beta1.TenantID(10)
	tenantKey := metrics.TenantKey{Namespace: testNamespace, TenantID: tenantID}

	// Override minSchedulableNodeCount to 5 so that 3 schedulable nodes is insufficient.
	defer testutils.HookGlobal(&minSchedulableNodeCount, 5)()

	nodeMetrics := metrics.NodeMetricsMap{
		"hot-node":  {CpuAvg5m: 85.0, Schedulable: true},
		"cool-node": {CpuAvg5m: 40.0, Schedulable: true},
		"warm-node": {CpuAvg5m: 60.0, Schedulable: true},
	}

	tenantMetrics := metrics.TenantMetricsMap{
		tenantKey: &metrics.TenantMetrics{
			Key: tenantKey,
			Pods: metrics.PodMetricsMap{
				"pod-1": {SqlCpuAvg5m: 0.5},
			},
		},
	}

	w, ctl := setupWatcher(t, ctx, true, nodeMetrics, tenantMetrics,
		makeStampedPod("pod-1", "hot-node", tenantID),
	)

	w.remediateHotNodes(ctx)

	pod := getPod(t, ctx, ctl, "pod-1")
	require.False(t, manifests.IsTenantPodStale(pod), "no pod should be stale when schedulable node count is below minimum")
}

func TestRemediateHotNodes_DryRunDoesNotMarkStale(t *testing.T) {
	ctx := context.Background()
	tenantID := crdbv1beta1.TenantID(10)
	tenantKey := metrics.TenantKey{Namespace: testNamespace, TenantID: tenantID}

	defer testutils.HookGlobal(&isDryRun, true)()

	nodeMetrics := metrics.NodeMetricsMap{
		"hot-node":  {CpuAvg5m: 85.0, Schedulable: true},
		"cool-node": {CpuAvg5m: 40.0, Schedulable: true},
		"warm-node": {CpuAvg5m: 60.0, Schedulable: true},
	}

	tenantMetrics := metrics.TenantMetricsMap{
		tenantKey: &metrics.TenantMetrics{
			Key: tenantKey,
			Pods: metrics.PodMetricsMap{
				"pod-1": {SqlCpuAvg5m: 0.5},
			},
		},
	}

	w, ctl := setupWatcher(t, ctx, true, nodeMetrics, tenantMetrics,
		makeStampedPod("pod-1", "hot-node", tenantID),
	)

	w.remediateHotNodes(ctx)

	pod := getPod(t, ctx, ctl, "pod-1")
	require.False(t, manifests.IsTenantPodStale(pod), "dry run should not actually mark pod stale")
	require.Len(t, w.remediationTimestamps, 0, "dry run should not record a remediation")
}

func TestRemediateHotNodes_RateLimiting(t *testing.T) {
	ctx := context.Background()
	tenantID := crdbv1beta1.TenantID(10)
	tenantKey := metrics.TenantKey{Namespace: testNamespace, TenantID: tenantID}

	defer testutils.HookGlobal(&maxRemediationsPerHour, 2)()

	nodeMetrics := metrics.NodeMetricsMap{
		"hot-node":  {CpuAvg5m: 85.0, Schedulable: true},
		"cool-node": {CpuAvg5m: 40.0, Schedulable: true},
		"warm-node": {CpuAvg5m: 60.0, Schedulable: true},
	}

	tenantMetrics := metrics.TenantMetricsMap{
		tenantKey: &metrics.TenantMetrics{
			Key: tenantKey,
			Pods: metrics.PodMetricsMap{
				"pod-1": {SqlCpuAvg5m: 0.5},
			},
		},
	}

	w, ctl := setupWatcher(t, ctx, true, nodeMetrics, tenantMetrics,
		makeStampedPod("pod-1", "hot-node", tenantID),
	)

	// first remediation should succeed
	w.remediateHotNodes(ctx)
	pod := getPod(t, ctx, ctl, "pod-1")
	require.True(t, manifests.IsTenantPodStale(pod), "first remediation should mark pod stale")
	require.Len(t, w.remediationTimestamps, 1)

	// reset the pod so we can test a second remediation
	delete(pod.Labels, manifests.TenantPodStaleLabel)
	require.NoError(t, ctl.WithNamespace(testNamespace).ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, pod))

	// second remediation should also succeed (limit is 2)
	w.remediateHotNodes(ctx)
	pod = getPod(t, ctx, ctl, "pod-1")
	require.True(t, manifests.IsTenantPodStale(pod), "second remediation should mark pod stale")
	require.Len(t, w.remediationTimestamps, 2)

	// reset the pod again
	delete(pod.Labels, manifests.TenantPodStaleLabel)
	require.NoError(t, ctl.WithNamespace(testNamespace).ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, pod))

	// third remediation should be rate limited
	w.remediateHotNodes(ctx)
	pod = getPod(t, ctx, ctl, "pod-1")
	require.False(t, manifests.IsTenantPodStale(pod), "third remediation should be rate limited")

	// simulate the oldest entry aging out past the one-hour window
	w.remediationTimestamps[0] = time.Now().Add(-61 * time.Minute)

	// now remediation should succeed again since the oldest entry expired
	w.remediateHotNodes(ctx)
	pod = getPod(t, ctx, ctl, "pod-1")
	require.True(t, manifests.IsTenantPodStale(pod), "remediation should succeed after oldest entry expires")
}

func TestRemediateHotNodes_SkipsWhenTenantLocked(t *testing.T) {
	ctx := context.Background()
	tenantID := crdbv1beta1.TenantID(10)
	tenantKey := metrics.TenantKey{Namespace: testNamespace, TenantID: tenantID}

	nodeMetrics := metrics.NodeMetricsMap{
		"hot-node":  {CpuAvg5m: 85.0, Schedulable: true},
		"cool-node": {CpuAvg5m: 40.0, Schedulable: true},
		"warm-node": {CpuAvg5m: 60.0, Schedulable: true},
	}

	tenantMetrics := metrics.TenantMetricsMap{
		tenantKey: &metrics.TenantMetrics{
			Key: tenantKey,
			Pods: metrics.PodMetricsMap{
				"pod-1": {SqlCpuAvg5m: 0.5},
			},
		},
	}

	w, ctl := setupWatcher(t, ctx, true, nodeMetrics, tenantMetrics,
		makeStampedPod("pod-1", "hot-node", tenantID),
	)

	// Acquire the lock before the watcher runs, simulating a concurrent reconcile.
	release, locked := w.scaleLockTable.TryLock(tenantID.Tag())
	require.True(t, locked, "should be able to acquire lock")

	w.remediateHotNodes(ctx)

	pod := getPod(t, ctx, ctl, "pod-1")
	require.False(t, manifests.IsTenantPodStale(pod), "pod should not be stale when tenant is locked")

	// Release the lock and verify remediation succeeds on the next attempt.
	release()

	w.remediateHotNodes(ctx)

	pod = getPod(t, ctx, ctl, "pod-1")
	require.True(t, manifests.IsTenantPodStale(pod), "pod should be stale after lock is released")
}

package manager

import (
	"context"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	opmetrics "github.com/cockroachlabs/cockroach-enterprise-operator/controllers/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/tenant"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/envutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	hotNodeThresholdEnv        = "HOT_NODE_THRESHOLD"
	coldNodeThresholdEnv       = "COLD_NODE_THRESHOLD"
	hotNodeWatchIntervalEnv    = "HOT_NODE_WATCH_INTERVAL"
	minSchedulableNodeCountEnv = "HOT_NODE_MIN_SCHEDULABLE_NODE_COUNT"
	hotNodeDryRunEnv           = "HOT_NODE_DRY_RUN"
	maxRemediationsPerHourEnv  = "HOT_NODE_MAX_REMEDIATIONS_PER_HOUR"
)

var (
	// Nodes with CPU utilization percentage above hotNodeThreshold are considered hot
	// and will be targeted for remediation.
	hotNodeThreshold = 80

	// Nodes with CPU utilization percentage below coldNodeThreshold are considered cold.
	// We want at least one cold node available so that the evicted
	// pod can land somewhere. Decided on 55% as one pod could potentially
	// use up to 25% of a node's cores (node has 16 cores).
	coldNodeThreshold = 55

	// How often to check for hot nodes and remediate.
	hotNodeWatchInterval = 60 * time.Second

	// Skip remediation entirely if the cluster doesn't have at least this
	// number of schedulable nodes reporting metrics.
	minSchedulableNodeCount = 3

	// Enabling isDryRun prevents remediation from actually being applied, and also prevents metrics from being recorded.
	// Instead, it logs the attempt and notes that it was in dry run mode.
	isDryRun = false

	// The limit of how many pods the watcher can mark stale in a rolling one-hour window.
	// 0 or negative means unlimited.
	maxRemediationsPerHour = 4
)

// loadHotNodeConfig reads environment variables and overrides defaults.
// Default values are kept on parse failure.
func loadHotNodeConfig(ctx context.Context) {
	envutil.LoadInt(ctx, hotNodeThresholdEnv, &hotNodeThreshold)
	envutil.LoadInt(ctx, coldNodeThresholdEnv, &coldNodeThreshold)
	envutil.LoadInt(ctx, minSchedulableNodeCountEnv, &minSchedulableNodeCount)
	envutil.LoadInt(ctx, maxRemediationsPerHourEnv, &maxRemediationsPerHour)
	envutil.LoadDurationSeconds(ctx, hotNodeWatchIntervalEnv, &hotNodeWatchInterval)
	envutil.LoadBool(ctx, hotNodeDryRunEnv, &isDryRun)
}

// HotNodeWatcher checks k8s nodes for their CPU util and marks the pod with the highest
// CPU usage on the node with the highest CPU usage as stale for eviction.
type HotNodeWatcher struct {
	ctl                 *kube.Ctl
	nodeMetricsManager  metrics.NodeMetricsReader
	tenantMetricsReader metrics.TenantMetricsReader
	scaleLockTable      *tenant.LockTable

	// remediationTimestamps tracks when each remediation occurred for rate limiting.
	remediationTimestamps []time.Time
}

// A compiler check to ensure HotNodeWatcher implements manager.LeaderElectionRunnable.
// https://go.dev/doc/faq#guarantee_satisfies_interface
var _ manager.LeaderElectionRunnable = &HotNodeWatcher{}

// NewHotNodeWatcher constructs a new HotNodeWatcher, which when called with Start(),
// polls k8s nodes for CPU utilization. Nodes that exceed hotNodeThreshold are considered hot.
// The hottest node is chosen and the hottest tenantpool pod on that node is chosen for eviction.
func NewHotNodeWatcher(ctl *kube.Ctl, nmm metrics.NodeMetricsReader, tmr metrics.TenantMetricsReader, scaleLockTable *tenant.LockTable) *HotNodeWatcher {
	return &HotNodeWatcher{
		ctl:                 ctl,
		nodeMetricsManager:  nmm,
		tenantMetricsReader: tmr,
		scaleLockTable:      scaleLockTable,
	}
}

// Start polls for hot nodes every hotNodeWatchInterval seconds and marks eligible tenant pods as stale.
func (w *HotNodeWatcher) Start(ctx context.Context) error {
	loadHotNodeConfig(ctx)

	ticker := time.NewTicker(hotNodeWatchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			w.remediateHotNodes(ctx)
		}
	}
}

// NeedLeaderElection returns true always, since we only allow leader to remediate.
func (w *HotNodeWatcher) NeedLeaderElection() bool {
	return true
}

func (w *HotNodeWatcher) remediateHotNodes(ctx context.Context) {
	// crdbcluster resources are namespaced, and it's in there that we
	// enable or disable this remediation feature flag.
	enabledNamespace := w.hotNodeEnabledNamespace(ctx)
	if enabledNamespace == "" {
		logging.Debugf(ctx, "hot-node watcher: no namespaces with the hot-node-auto-remediation feature flag enabled found")
		return
	}

	nodeMetrics := w.nodeMetricsManager.GetNodeMetrics()
	if schedulableNodes := nodeMetrics.CountSchedulable(); schedulableNodes < minSchedulableNodeCount {
		logging.Infof(ctx, "hot-node watcher: only %d schedulable nodes when we need %d minimum", schedulableNodes, minSchedulableNodeCount)
		return
	}

	hottestNode, hotNodeExists := w.findHottestNode(ctx, nodeMetrics)
	if !hotNodeExists {
		return
	}

	// No remediation occurs if there are any existing stale pods.
	ctl := w.ctl.WithNamespace(enabledNamespace)
	var stalePods corev1.PodList
	if err := ctl.List(ctx, &kube.ListOptions{
		LabelSelector: manifests.MakeStaleStampedPodsSelector(),
	}, &stalePods); err != nil {
		logging.Warnf(ctx, "hot-node watcher: failed to list stale pods in namespace %s, aborting remediation: %v", enabledNamespace, err)
		return
	}
	if len(stalePods.Items) > 0 {
		logging.Infof(ctx, "hot-node watcher: at least one stale pod (%s) already exists in namespace %s, skipping remediation until no more stale pods exist", stalePods.Items[0].Name, enabledNamespace)
		return
	}

	bestPod, bestTenantID := w.findHottestPod(ctx, ctl, hottestNode, enabledNamespace)
	if bestPod == nil {
		logging.Warnf(ctx, "hot-node watcher: no eligible pods found on hot node %s", hottestNode)
		return
	}

	if w.isRateLimited(ctx) {
		return
	}

	tenantTag := bestTenantID.Tag()
	release, locked := w.scaleLockTable.TryLock(tenantTag)
	if !locked {
		logging.Infof(ctx, "hot-node watcher: tenant %s is locked, skipping remediation", tenantTag)
		return
	}
	defer release()

	if isDryRun {
		logging.Infof(ctx, "hot-node watcher: dry-run marked pod %s on hot node %s as stale",
			bestPod.Name, hottestNode)
	} else {
		manifests.MarkTenantPodAsStale(bestPod)
		if err := ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, bestPod); err != nil {
			logging.Warnf(ctx, "hot-node watcher: failed to mark pod %s stale: %v", bestPod.Name, err)
			return
		}
		opmetrics.HotNodeRemediationTotal.Inc()
		w.recordRemediation()
		logging.Infof(ctx, "hot-node watcher: marked pod %s on hot node %s as stale",
			bestPod.Name, hottestNode)
	}
}

// findHottestPod returns the stamped pod with the highest CPU usage on the
// given node and its tenant ID, or nil if no eligible pod was found.
func (w *HotNodeWatcher) findHottestPod(
	ctx context.Context, ctl *kube.Ctl, nodeName, namespace string,
) (*corev1.Pod, crdbv1beta1.TenantID) {
	var pods corev1.PodList
	if err := ctl.List(ctx, &kube.ListOptions{
		LabelSelector: manifests.MakeStampedPodsSelector(),
		FieldSelector: fields.SelectorFromSet(fields.Set{"spec.nodeName": nodeName}),
	}, &pods); err != nil {
		logging.Warnf(ctx, "hot-node watcher: failed to list pods on node %s in namespace %s, aborting remediation: %v", nodeName, namespace, err)
		return nil, 0
	}

	var bestPod *corev1.Pod
	var bestTenantID crdbv1beta1.TenantID
	var highestCPURate float64
	for i := range pods.Items {
		pod := &pods.Items[i]

		tenantID := pod.Labels[manifests.TenantIDLabel]
		// These would be system pods, warm pods, etc.
		if tenantID == "" {
			continue
		}

		state, _ := manifests.GetTenantPodState(pod)
		// These would be non-stale deleting pods so they're unlikely to be contributing to the CPU load.
		// But we do allow draining pods to be marked as stale if they are contributing the most CPU load.
		if state == manifests.TenantPodDeletingState {
			continue
		}

		parsedTenantID, err := crdbv1beta1.ParseTenantID(tenantID)
		if err != nil {
			logging.Warnf(ctx, "hot-node watcher: unable to parse tenantID (%s): %v", tenantID, err)
			continue
		}
		key := metrics.TenantKey{Namespace: namespace, TenantID: parsedTenantID}
		tenantMetrics := w.tenantMetricsReader.Get(key)
		if tenantMetrics == nil {
			logging.Infof(ctx, "hot-node watcher: no tenant metrics for tenant %d, skipping pod", parsedTenantID)
			continue
		}
		podMetrics, ok := tenantMetrics.Pods[pod.Name]
		if !ok {
			logging.Infof(ctx, "hot-node watcher: no pod metrics for pod %s, skipping pod", pod.Name)
			continue
		}
		if podMetrics.SqlCpuAvg5m > highestCPURate {
			bestPod = pod
			bestTenantID = parsedTenantID
			highestCPURate = podMetrics.SqlCpuAvg5m
		}
	}
	return bestPod, bestTenantID
}

// isRateLimited returns true if the watcher has hit the hourly remediation
// limit. Returns false when rate limiting is disabled (maxRemediationsPerHour <= 0).
func (w *HotNodeWatcher) isRateLimited(ctx context.Context) bool {
	if maxRemediationsPerHour <= 0 {
		return false
	}
	if len(w.remediationTimestamps) < maxRemediationsPerHour {
		return false
	}
	// The slice is at capacity, so check if the oldest entry has aged out.
	oldest := w.remediationTimestamps[0]
	if time.Since(oldest) >= time.Hour {
		return false
	}
	logging.Warnf(ctx, "hot-node watcher: rate limited, %d remediations in the last hour (max %d)",
		len(w.remediationTimestamps), maxRemediationsPerHour)
	return true
}

// recordRemediation appends the current time and drops any entries older than
// one hour from the front of the slice.
func (w *HotNodeWatcher) recordRemediation() {
	now := time.Now()
	cutoff := now.Add(-time.Hour)
	start := 0
	for start < len(w.remediationTimestamps) && !w.remediationTimestamps[start].After(cutoff) {
		start++
	}
	w.remediationTimestamps = append(w.remediationTimestamps[start:], now)
}

// findHottestNode returns the k8s node with the highest CPU util above hotNodeThreshold,
// provided at least one cold node (CPU util less than coldNodeThreshold) exists for the
// evicted pod to land on. Returns false if both conditions aren't satisfied.
func (w *HotNodeWatcher) findHottestNode(ctx context.Context, nodeMetrics metrics.NodeMetricsMap) (string, bool) {
	var hottestNode string
	var hottestCPU float64
	hasCold := false
	for nodeName, nodeMetric := range nodeMetrics {
		if nodeMetric.MetricsNotReady || !nodeMetric.Schedulable {
			continue
		}
		nodeCPUAvg5m := nodeMetric.CpuAvg5m
		if nodeCPUAvg5m >= float64(hotNodeThreshold) && nodeCPUAvg5m > hottestCPU {
			hottestNode = nodeName
			hottestCPU = nodeCPUAvg5m
		} else if nodeCPUAvg5m < float64(coldNodeThreshold) {
			hasCold = true
		}
	}
	if hottestNode == "" {
		logging.Infof(ctx, "hot-node watcher: no hot nodes found meeting the hot threshold %d%%", hotNodeThreshold)
		return "", false
	}
	if !hasCold {
		logging.Warnf(ctx, "hot-node watcher: found hot node %s (%.1f%%) but unable to find any node below %d%% CPU. Skipping remediation",
			hottestNode, hottestCPU, coldNodeThreshold)
		return "", false
	}
	logging.Infof(ctx, "hot-node watcher: selecting node %s to remediate due to high CPU (%.1f%%)", hottestNode, hottestCPU)
	return hottestNode, true
}

// hotNodeEnabledNamespace returns the first namespace that contains a CrdbCluster
// with the hot-node auto-remediation feature enabled. While it's possible to have
// multiple namespaces with crdbclusters, we assume just one to simplify logic.
func (w *HotNodeWatcher) hotNodeEnabledNamespace(ctx context.Context) string {
	var namespaces corev1.NamespaceList
	if err := w.ctl.List(ctx, &kube.ListOptions{}, &namespaces); err != nil {
		logging.Warnf(ctx, "hot-node watcher: failed to list namespaces: %v", err)
		return ""
	}

	for _, ns := range namespaces.Items {
		ctl := w.ctl.WithNamespace(ns.Name)
		var clusters crdbv1beta1.CrdbClusterList
		if err := ctl.List(ctx, &kube.ListOptions{}, &clusters); err != nil {
			logging.Warnf(ctx, "hot-node watcher: failed to list CrdbClusters in namespace %s: %v", ns.Name, err)
			continue
		}

		for i := range clusters.Items {
			if clusters.Items[i].Spec.FeatureEnabled(crdbv1beta1.ClusterHotNodeAutoRemediation) {
				return ns.Name
			}
		}
	}
	return ""
}

package metrics

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metrics "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"go.uber.org/zap"
)

// NodeMetricPollInterval The interval between subsequent current node metric pulls.
var nodeMetricPollInterval = 3 * time.Second

// nodeMovingAvgSlidingWindow is the interval length by which we calculate the moving average.
var nodeMovingAvgSlidingWindow = 5 * time.Minute

// NodeMetricsMap defines a map from node name to the set of metrics associated with that node
type NodeMetricsMap map[string]NodeMetrics

// CountSchedulable returns the number of nodes that are schedulable in the
// node metrics map.
func (m NodeMetricsMap) CountSchedulable() int {
	count := 0
	for _, nm := range m {
		if nm.Schedulable {
			count++
		}
	}
	return count
}

// NodeManager manages metrics for all nodes that are
// running in the region.
// NOTE: All public methods on the NodeManager are thread-safe.
type NodeManager struct {
	ctx      context.Context
	ctl      *kube.Ctl
	nodeAvgs map[string]*movingAvg
	mu       struct {
		sync.Mutex
		nodeMetrics map[string]NodeMetrics
	}
}

// NodeMetricsReader is used by various clients that only need read only
// access to current node metrics.
type NodeMetricsReader interface {
	GetNodeMetrics() NodeMetricsMap
}

// NodeMetrics records the metrics collected for an individual node.
type NodeMetrics struct {
	// CpuIRate is the current rate of CPU usage on this node, as of the time
	// of the last sample.
	CpuIRate float64

	// CpuAvg5m is the average CPU usage on this node over the previous
	// 5 minute window. If this is not full (e.g. when operator restarts),
	// then this metric is inaccurate. See MetricsNotReady below as well.
	CpuAvg5m float64

	// MetricsNotReady is true if the fill ratio of the underlying movingAvg is less than 1.
	MetricsNotReady bool

	// Schedulable is true if the node is ready, uncordoned, and free of
	// NoSchedule and NoExecute taints.
	Schedulable bool
}

// NewNodeMetricsManager constructs a new node metrics manager instance.
func NewNodeMetricsManager(ctx context.Context, ctl *kube.Ctl) *NodeManager {
	tm := &NodeManager{
		ctx:      ctx,
		ctl:      ctl,
		nodeAvgs: make(map[string]*movingAvg),
	}
	tm.mu.nodeMetrics = NodeMetricsMap{}

	// Start monitoring goroutine which continuously monitors the node metrics
	// This will be invoked only once when autoscaler gets instantiated.
	go tm.monitorNodes()
	return tm
}

// GetNodeMetrics implements NodeMetricsManager,
// and returns a copy of the node metrics map.
func (m *NodeManager) GetNodeMetrics() NodeMetricsMap {
	m.mu.Lock()
	defer m.mu.Unlock()
	nodeMetrics := make(NodeMetricsMap, len(m.mu.nodeMetrics))
	maps.Copy(nodeMetrics, m.mu.nodeMetrics)
	return nodeMetrics
}

// monitorNodes periodically polls node metrics and updates the NodeMetrics map,
// we don't need any locking mechanism here as there won't be concurrent updates happening
// Map reads are concurrent-safe in go by default.
func (m *NodeManager) monitorNodes() {
	if m.ctl == nil {
		logging.Error(m.ctx, "kubernetes client not initialized")
		return
	}

	ticker := time.NewTicker(nodeMetricPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.fetchLatestNodeMetrics()
		}
	}
}

func (m *NodeManager) fetchLatestNodeMetrics() {
	newMetrics := make(NodeMetricsMap)
	defer m.updateNodeMetrics(newMetrics)
	// Get metrics for all nodes.
	// If this fails, instead of maintaining the lastUpdatedTimeStamp,
	// we can just return an empty metrics map that suggests using the default stamping method.
	// If we want to configure some interval to consider metrics as stale,
	// we can introduce lastUpdatedTimeStamp.
	nodeMetrics, err := m.fetchK8sNodeMetrics(m.ctx)
	if err != nil {
		logging.Error(m.ctx, "failed to get node metrics", zap.Error(err))
		return
	}

	// Get a list of nodes.
	// If this fails, we can just return an empty metrics map that suggests using the default stamping method.
	nodes, err := m.getNodes(m.ctx)
	if err != nil {
		logging.Error(m.ctx, "failed to get nodes", zap.Error(err))
		return
	}

	logging.Debug(m.ctx, "fetched node metrics", zap.Any("nodeMetrics", nodeMetrics))
	// Update the node metrics map.
	for _, nodeMetric := range nodeMetrics.Items {
		nodeName := nodeMetric.Name
		if node, found := nodes[nodeName]; found {
			if cpu, exists := nodeMetric.Usage["cpu"]; exists {
				// Get the allocatable CPU for the node.
				cpuAllocatable := node.Status.Allocatable.Cpu()

				var cpuPercent float64
				if cpuAllocatable.MilliValue() > 0 {
					cpuPercent = (float64(cpu.MilliValue()) / float64(cpuAllocatable.MilliValue())) * 100
				}
				avg, ok := m.nodeAvgs[nodeName]
				if !ok {
					avg = newMovingAvg(int(nodeMovingAvgSlidingWindow / nodeMetricPollInterval))
					m.nodeAvgs[nodeName] = avg
				}
				avg.add(cpuPercent)
				newMetrics[nodeName] = NodeMetrics{
					CpuIRate:        cpuPercent,
					CpuAvg5m:        avg.avg(),
					MetricsNotReady: avg.fillRatio() < 1.0,
					Schedulable:     kube.IsNodeSchedulable(&node) == nil,
				}
			}
		}
	}

	// Prune averages for nodes that stopped reporting.
	for name := range m.nodeAvgs {
		if _, ok := nodes[name]; !ok {
			delete(m.nodeAvgs, name)
		}
	}
}

func (m *NodeManager) updateNodeMetrics(metrics NodeMetricsMap) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mu.nodeMetrics = metrics
}

// fetchK8sNodeMetrics returns the latest metrics for all nodes in the cluster.
func (m *NodeManager) fetchK8sNodeMetrics(ctx context.Context) (*metricsv1beta1.NodeMetricsList, error) {
	// Create a metrics client.
	config := m.ctl.RestConfig()

	metricsClient, err := metrics.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create metrics client: %w", err)
	}

	// Get node metrics.
	return metricsClient.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
}

// getNodes returns a list of k8s nodes in the cluster.
func (m *NodeManager) getNodes(ctx context.Context) (map[string]corev1.Node, error) {
	nodes := corev1.NodeList{}
	if err := m.ctl.List(ctx, &kube.ListOptions{}, &nodes); err != nil {
		return nil, fmt.Errorf("failed to get nodes: %v", err)
	}

	nodesMap := make(map[string]corev1.Node)
	for _, node := range nodes.Items {
		nodesMap[node.Name] = node
	}
	return nodesMap, nil
}

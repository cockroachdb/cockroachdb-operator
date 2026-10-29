package metrics

var _ NodeMetricsReader = &TestNodeManager{}

type TestNodeManager struct {
	NodeMetricsMap
}

func (t *TestNodeManager) GetNodeMetrics() NodeMetricsMap {
	return t.NodeMetricsMap
}

func (t *TestNodeManager) UpdateNodeMetrics(nodeMetrics NodeMetricsMap) {
	t.NodeMetricsMap = nodeMetrics
}

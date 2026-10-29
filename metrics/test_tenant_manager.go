package metrics

import "context"

var _ TenantMetricsReader = &TestTenantManager{}

type TestTenantManager struct {
	TenantMetricsMap
	UpdateFunc func(ctx context.Context, metrics *TenantMetrics)
}

func (m *TestTenantManager) Get(key TenantKey) *TenantMetrics {
	return m.TenantMetricsMap[key]
}

func (m *TestTenantManager) UpdateMetrics(ctx context.Context, tenantMetrics []*TenantMetrics) {
	m.TenantMetricsMap = TenantMetricsMap{}
	for _, metric := range tenantMetrics {
		m.TenantMetricsMap[metric.Key] = metric
		if m.UpdateFunc != nil {
			m.UpdateFunc(ctx, metric)
		}
	}
}

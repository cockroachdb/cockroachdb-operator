package tenantctrl

import "github.com/cockroachlabs/cockroach-enterprise-operator/tenant"

// Scale ensures that the number of available pods for the given tenant match
// the desired number of pods. It does this by stamping new pods from the tenant
// pool or by marking extra pods as DRAINING, in preparation for termination.
func (h *requestHandler) Scale() error {
	s := tenant.TenantPodScaler{
		Ctl:                h.ctl,
		MetricManager:      h.reconciler.MetricsManager,
		NodeMetricManager:  h.reconciler.NodeMetricsManager,
		Rng:                h.reconciler.Rng,
		Emit:               h.emit,
		PodSyncer:          h.reconciler.TenantPodSyncer,
		NodeLocalityReader: h.reconciler.NodeLocalityReader,
	}
	scaleTenantFn := s.ScaleTenantWithMetrics
	if h.testingKnobs.scaleTenant != nil {
		scaleTenantFn = h.testingKnobs.scaleTenant
	}
	shouldRequeue, requeueDuration, err := scaleTenantFn(h.ctx, h.tenant)
	if err != nil {
		return err
	}
	if shouldRequeue {
		// The scaler may have requested requeue-ing reconciliation.
		h.RequeueAfter(requeueDuration)
	}
	return nil
}

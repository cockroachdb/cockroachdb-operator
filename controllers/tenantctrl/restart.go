package tenantctrl

import (
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/tenant"
	corev1 "k8s.io/api/core/v1"
)

// RecordRestarts lists the tenant's stamped pods and hands them to
// tenant.RecordPodRestarts, which classifies fresh restart events and emits a
// structured log per event. Dedup is handled inside RecordPodRestarts via
// per-pod and per-tenant annotations, so this method is safe to call on every
// reconcile without causing duplicate logs.
func (h *requestHandler) RecordRestarts() error {
	var podList corev1.PodList
	if err := h.ctl.List(h.ctx, &kube.ListOptions{
		LabelSelector: manifests.MakeStampedPodSelector(h.tenant.Spec.TenantID),
	}, &podList); err != nil {
		return err
	}
	tenant.RecordPodRestarts(h.ctx, h.ctl, h.tenant, podList.Items)
	return nil
}

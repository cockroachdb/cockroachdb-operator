package tenant

import (
	"context"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// labelStalePoolPods looks for pods bound to a tenant from the wrong pool. If any
// are found they are labelled as stale. This occurs when the TenantPool field on
// the CrdbTenant was changed.
func labelStalePoolPods(
	ctx context.Context, ctl *kube.Ctl, pods []corev1.Pod, tenant *crdbv1beta1.CrdbTenant,
) error {
	selector := manifests.MakeStalePoolSelector(tenant.Spec.TenantID, *tenant.Spec.TenantPool)

	for _, pod := range pods {
		// Pod has already been labelled as stale.
		if manifests.IsTenantPodStale(&pod) {
			continue
		}

		// Pod is from wrong pool, label as stale.
		if selector.Matches(labels.Set(pod.Labels)) {
			logging.Infof(ctx, "found stale pool pod %v for %v, labelling as stale", pod.Name, tenant.Name)
			manifests.MarkTenantPodAsStale(&pod)
			if err := ctl.Apply(ctx, &pod); err != nil {
				return err
			}
		}
	}

	return nil
}

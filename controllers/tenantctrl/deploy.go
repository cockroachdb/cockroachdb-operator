package tenantctrl

import (
	"fmt"

	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Deploy ensures the existence and "up to date"-ness of tenant related
// resources (IE NetworkPolicys). All resources created by this method will have
// their OwnerReference set to the provided tenant. Deploy is idempotent and
// should run during every Reconcile loop.
func (h *requestHandler) Deploy() error {
	networkPlugin, err := cloud.GetNetworkPluginForCluster(h.ctx, h.ctl)
	if err != nil {
		return err
	}

	policy := manifests.TenantNetworkPolicy(manifests.TenantNetworkPolicyOptions{
		TenantID:      h.tenant.Spec.TenantID,
		NetworkPlugin: networkPlugin,
	})

	ref, err := kube.OwnerReferenceFor(h.ctl.Scheme(), h.tenant)
	if err != nil {
		return err
	}
	ref.Controller = new(true)
	tenantOwnerRef := []metav1.OwnerReference{ref}

	// For all tenant resources, set their OwnerReference to be the tenant.
	// When the tenant CRD is deleted, Kubernetes will automatically garbage
	// collect all owned resourecs.
	// See: https://kubernetes.io/docs/concepts/workloads/controllers/garbage-collection/
	objects := kube.SetOwnerReferences(tenantOwnerRef, policy)

	return h.apply(fmt.Sprintf("resources for tenant %d", h.tenant.Spec.TenantID), objects...)
}

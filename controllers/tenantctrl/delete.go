package tenantctrl

import (
	_ "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/stringutil"
	corev1 "k8s.io/api/core/v1"
)

// Delete deletes the given tenant from its host cluster, if that has been
// requested.
func (h *requestHandler) Delete() error {
	// If the tenant has not been deleted, nothing to do.
	if h.tenant.DeletionTimestamp.IsZero() {
		return nil
	}

	// Terminate any pods stamped with this tenant's ID before touching the
	// host keyspace. Otherwise the cockroachdb container in each stamped pod
	// would keep exiting on its next attempt to reach the destroyed tenant
	// and enter CrashLoopBackOff for the entire window until this reconcile
	// pass finishes and the K8s garbage collector removes the pod via
	// ownerReference cascade.
	//
	// Best-effort: failures are logged and swallowed so that a flaky pod API
	// cannot stall keyspace destruction and leak a tenant. In the worst case
	// we fall back to today's behavior (brief crashloop, then GC reclaims via
	// ownerReference cascade) — strictly better than a stuck finalizer.
	h.terminateStampedPods()

	tenantID := h.tenant.Spec.TenantID
	// Only delete the tenant's keyspace if its host cluster exists and is not
	// in the process of being deleted.
	cluster, err := getOwningCluster(h.ctx, h.ctl, h.tenant)
	if err != nil {
		return err
	}

	if cluster != nil && cluster.DeletionTimestamp.IsZero() {
		// Destroy the tenant's keyspace in the host cluster.
		const stmt = `
			SELECT
				CASE
					WHEN EXISTS(SELECT * FROM system.tenants WHERE id = $1 AND active)
					THEN crdb_internal.destroy_tenant($1)
				END`
		err := h.execSqlOnHost(stmt, tenantID)
		if err != nil {
			return errors.Wrapf(err, "failed to delete tenant %d", tenantID)
		}
	}

	// We must explicitly delete the cilium cluster wide network policy during
	// tenant deletion. If cluster-scoped dependent specifies a namespaced kind
	// as an owner, it is treated as having an unresolvable owner reference,
	// and is not able to be garbage collected.
	// https://kubernetes.io/docs/concepts/architecture/garbage-collection/#:~:text=A%20namespaced%20owner%20must%20exist,all%20owners%20are%20verified%20absent.
	networkPlugin, err := cloud.GetNetworkPluginForCluster(h.ctx, h.ctl)
	if err != nil {
		return err
	}

	if networkPlugin == cloud.Cilium {
		policy := manifests.TenantNetworkPolicy(manifests.TenantNetworkPolicyOptions{
			TenantID:      h.tenant.Spec.TenantID,
			NetworkPlugin: networkPlugin,
		})
		if err := h.ctl.Apply(h.ctx, kube.Tombstone(policy)...); err != nil {
			return err
		}
	}

	// Remove the finalizer so that K8s can delete the tenant from its store.
	// NOTE: This must be the final step of deletion, or else a crash during
	// deletion can orphan resources.
	if stringutil.StringSetRemove(&h.tenant.Finalizers, TenantFinalizerName) {
		if err := h.ctl.ApplyWithOptions(h.ctx, kube.ApplyOptions{NoWait: true}, h.tenant); err != nil {
			return errors.Wrapf(err, "failed to remove finalizer on tenant %d", h.tenant.Spec.TenantID)
		}
	}

	logging.Infof(h.ctx, "deleted tenant %d", h.tenant.Spec.TenantID)
	return nil
}

// terminateStampedPods best-effort deletes every pod stamped with this
// tenant's ID. Termination is fire-and-forget: SQL-only tenant pods exit
// immediately on SIGTERM (see the comment on ForceDrainTimeout in
// tenant/scale.go), and no draining is worthwhile because the tenant keyspace
// is about to be destroyed. Missing pods are ignored so this is safe to
// re-run on retries.
//
// Errors are intentionally logged and swallowed rather than returned. See
// Delete() for why: propagating errors here could stall keyspace destruction
// on a flaky pod API and leak the tenant.
func (h *requestHandler) terminateStampedPods() {
	tenantID := h.tenant.Spec.TenantID
	options := kube.ListOptions{
		LabelSelector: manifests.MakeStampedPodSelector(tenantID),
	}
	var pods corev1.PodList
	if err := h.ctl.List(h.ctx, &options, &pods); err != nil {
		logging.Warnf(h.ctx, "failed to list stamped pods for tenant %d: %v", tenantID, err)
		return
	}
	if len(pods.Items) > 0 {
		logging.Infof(h.ctx, "terminating %d stamped pods for tenant %d before destroy",
			len(pods.Items), tenantID)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		err := h.ctl.DeleteWithOptions(h.ctx, kube.DeleteOptions{NoWait: true}, pod)
		if err != nil && !kubeerrors.IsNotFound(err) {
			logging.Warnf(h.ctx, "failed to delete pod %s for tenant %d: %v", pod.Name, tenantID, err)
		}
	}
}

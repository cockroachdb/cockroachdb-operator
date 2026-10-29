package tenantctrl

import (
	"context"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/stringutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Initialize initializes the given tenant in its host cluster, if that has not
// yet been done.
func (h *requestHandler) Initialize() error {
	// If the tenant already has the Initialized condition, then no-op.
	if h.tenant.Status.HasCondition(crdbv1beta1.TenantInitialized, metav1.ConditionTrue) {
		return nil
	}

	// Ensure that Labels is not nil for later assignments.
	if h.tenant.Labels == nil {
		h.tenant.Labels = map[string]string{}
	}

	// Start by adding a finalizer to the CrdbTenant metadata. This will prevent
	// the tenant from being deleted from the K8s metadata store until the
	// operator has a chance to cleanup the tenant keyspace on the host cluster.
	// NOTE: This must be done before doing anything else, as otherwise a crash
	// during initialization can orphan resources.
	//
	// TODO(andyk): Using finalizers is not idempotent, because applying the
	// tenant CRD once will add the finalizer, but then applying the same CRD
	// again will delete that finalizer. StrategicMergePatch would fix this, but
	// CRDs use the regular MergePatch. Another idea is to use a mutating
	// webhook, which can add back any removed finalizers.
	//
	// TODO(andyk): There is a race condition when initialization has started,
	// but the tenant is deleted. The below Apply will fail with "not found".
	// The fix for this is to add the finalizer in a mutating webhook.
	if stringutil.StringSetAdd(&h.tenant.Finalizers, TenantFinalizerName) {
		if err := h.ctl.ApplyWithOptions(h.ctx, kube.ApplyOptions{NoWait: true}, h.tenant); err != nil {
			return errors.Wrapf(err, "adding finalizer to tenant %d", h.tenant.Spec.TenantID)
		}
	}

	// Allocate the tenant's keyspace in the host cluster.
	const stmt = `
	SELECT
		CASE
			WHEN NOT EXISTS(SELECT * FROM system.tenants WHERE id = $1)
			THEN crdb_internal.create_tenant($1::INT)
		END
`
	err := h.execSqlOnHost(stmt, h.tenant.Spec.TenantID)
	if err != nil {
		return errors.Wrapf(err, "creating keyspace for tenant %d", h.tenant.Spec.TenantID)
	}

	// Add the Initialized condition so that this reconciliation check can be
	// skipped next time.
	// NOTE: This must be the final step of initialization, or else these steps
	// will not be idempotent.
	h.tenant.Status.SetCondition(crdbv1beta1.TenantInitialized, metav1.ConditionTrue)
	if err := h.ctl.UpdateStatus(h.ctx, h.tenant); err != nil {
		return errors.Wrapf(err, "setting initialized condition on tenant %d", h.tenant.Spec.TenantID)
	}

	logging.Infof(h.ctx, "initialized tenant %d", h.tenant.Spec.TenantID)
	return nil
}

// ensureOwnerRef sets or updates the ownerRef on the tenant to point to the
// CrdbCluster in the same namespace.
// TODO(andyk): This should really be added in a mutating webhook so that
// there is no race condition where the OwnerReferences can't be set because
// the owning cluster was deleted after the validating webhook ran but before
// reconciliation happened.
func (h *requestHandler) ensureOwnerRef() error {
	cluster, err := getOwningCluster(h.ctx, h.ctl, h.tenant)
	if err != nil {
		return errors.Wrapf(err, "looking up owning cluster in namespace %s", h.tenant.Namespace)
	}
	// The cluster can be nil if it was deleted while the tenant still exists.
	// This happens briefly before K8s GC propagates the deletion to tenants
	// via the ownerRef. Skip the update and let GC handle cleanup.
	if cluster == nil {
		return nil
	}
	return SetOwningClusterTo(h.ctx, h.ctl, h.tenant, cluster)
}

// SetOwningClusterTo sets the given tenant's OwnerReferences field to the
// provided cluster and saves the change in the K8s store. This ensures that the
// tenant will be deleted when the cluster is deleted.
func SetOwningClusterTo(
	ctx context.Context,
	ctl *kube.Ctl,
	tenant *crdbv1beta1.CrdbTenant,
	cluster *crdbv1beta1.CrdbCluster,
) error {
	// Add the owning cluster to the OwnerReferences list if it doesn't exist
	// there yet.
	ownerRef, err := kube.OwnerReferenceFor(ctl.Scheme(), cluster)
	if err != nil {
		return errors.Wrapf(err, "creating owner reference for CRDB cluster %s", cluster.Name)
	}
	ownerRef.BlockOwnerDeletion = new(true)

	for i := range tenant.OwnerReferences {
		if tenant.OwnerReferences[i].UID == ownerRef.UID {
			if tenant.OwnerReferences[i].APIVersion == ownerRef.APIVersion {
				return nil
			}
			tenant.OwnerReferences[i] = ownerRef
			if err := ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, tenant); err != nil {
				return errors.Wrapf(err, "updating owner reference APIVersion on tenant %d", tenant.Spec.TenantID)
			}
			return nil
		}
	}
	tenant.OwnerReferences = append(tenant.OwnerReferences, ownerRef)

	if err := ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, tenant); err != nil {
		return errors.Wrapf(err, "adding owner references on tenant %d", tenant.Spec.TenantID)
	}

	return nil
}

// getOwningCluster returns a reference to the CrdbCluster that owns the given
// tenant. If no reference can be found, getOwningCluster returns nil.
func getOwningCluster(
	ctx context.Context, ctl *kube.Ctl, tenant *crdbv1beta1.CrdbTenant,
) (*crdbv1beta1.CrdbCluster, error) {
	// Get the owning cluster.
	var clusters crdbv1beta1.CrdbClusterList
	options := &kube.ListOptions{Namespace: tenant.Namespace}
	if err := ctl.List(ctx, options, &clusters); err != nil {
		return nil, errors.Wrapf(err, "listing CRDB clusters in namespace %s", tenant.Namespace)
	}
	if len(clusters.Items) == 0 {
		return nil, nil
	}
	if len(clusters.Items) > 1 {
		return nil, errors.AssertionFailedf(
			"namespace %s cannot contain more than one CRDB cluster", tenant.Namespace)
	}
	return &clusters.Items[0], nil
}

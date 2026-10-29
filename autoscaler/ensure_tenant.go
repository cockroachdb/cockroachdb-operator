package autoscaler

import (
	"context"
	"fmt"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
)

const (
	// ZeroMaxPodsReason is an annotation that can be used to provide a reason
	// when setting maxPods to 0.
	ZeroMaxPodsReason = "zero-max-pods-reason"
)

// EnsureTenantResumed ensures that .Spec.Pods > 1 and .Status.Pods > 0 for the
// given tenant. After calling EnsureTenantResumed, at least one stamped tenant
// pod will be available.
//
// Returns ErrTenantDisabled if the tenant has MaxPods == 0.
func EnsureTenantResumed(ctx context.Context, ctl *kube.Ctl, tenantID crdbv1beta1.TenantID) error {
	startTime := time.Now()
	defer func() {
		metrics.EnsureTenantResumedTime.Observe(time.Since(startTime).Seconds())
	}()
	var tenant crdbv1beta1.CrdbTenant
	return kube.RetryConflicts(func() error {
		if err := ctl.Get(ctx, tenantID.Tag(), &tenant); err != nil {
			return err
		}
		// If both Spec.Pods and Status.Pods are >= 1, short circuit. Having a
		// requested pods and running pods indicate that there will be pods
		// that may be connected to that are not currently being drained.
		if tenant.Spec.Pods >= 1 && tenant.Status.Pods >= 1 {
			return nil
		}
		if err := SetMinimumScale(&tenant, 1); err != nil {
			return err
		}
		// Use update here rather than .Apply as we don't want to accidentally
		// overwrite any changes. We only want to ensure that
		// tenant.Status.Pods > 0 when this method returns.
		return ctl.Update(ctx, &tenant)
	})
}

// SetMinimumScale sets the number of pods requested for the tenant. If MaxPods
// == 0 SetMinimumScale returns ErrTenantDsiabled.
//
// Spec.Pods is set equal to max(Spec.MaxPods, Spec.Pods, scale)
func SetMinimumScale(tenant *crdbv1beta1.CrdbTenant, scale int32) error {
	if tenant.Spec.MaxPods != nil && *tenant.Spec.MaxPods == 0 {
		return &ErrTenantDisabled{Reason: tenant.Annotations[ZeroMaxPodsReason]}
	}
	if tenant.Spec.MaxPods != nil && *tenant.Spec.MaxPods < scale {
		scale = *tenant.Spec.MaxPods
	}
	if tenant.Spec.Pods < scale {
		tenant.Spec.Pods = scale
	}
	return nil
}

// ErrTenantDisabled is returned by EnsureTenantResumed if the tenant is
// disabled and may not be scaled.
type ErrTenantDisabled struct {
	Reason string
}

// Error implements the error interface.
func (e *ErrTenantDisabled) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("cluster unavailable: %s", e.Reason)
	}
	return "cluster unavailable"
}

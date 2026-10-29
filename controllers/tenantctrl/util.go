package tenantctrl

import (
	"context"
	"encoding/json"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

func filterPredicates(ctx context.Context) *predicate.Funcs {
	// Trigger reconciliation in these cases:
	//   1. For all CrdbTenant object events.
	//   2. For Pod object deletion (we don't add a DeleteFunc predicate so all such events
	//      trigger reconciliation).
	//   3. For tenant pod evictions.
	//   4. For tenant pods being marked as stale.
	var pred predicate.Funcs
	pred.CreateFunc = func(evt event.CreateEvent) bool {
		res := isTenantObj(evt.Object)
		if res {
			logging.Infof(ctx, "reconciling tenant due to creation event on %s", evt.Object.GetName())
		}
		return res
	}
	pred.UpdateFunc = func(evt event.UpdateEvent) bool {
		res := doesUpdateTriggerReconciliation(ctx, evt)
		if res {
			logging.Infof(ctx, "reconciling tenant due to update event on %s", evt.ObjectOld.GetName())
		}
		return res
	}
	pred.GenericFunc = func(evt event.GenericEvent) bool {
		res := isTenantObj(evt.Object)
		if res {
			logging.Infof(ctx, "reconciling tenant due to generic event on %s", evt.Object.GetName())
		}
		return res
	}
	return &pred
}

func isTenantObj(obj runtime.Object) bool {
	_, ok := obj.(*crdbv1beta1.CrdbTenant)
	return ok
}

// doesUpdateTriggerReconciliation returns true if the UpdateEvent is a tenant pod eviction,
// a CrdbTenant update, or a tenant pod being marked as stale.
func doesUpdateTriggerReconciliation(ctx context.Context, evt event.UpdateEvent) bool {
	pod, isPod := evt.ObjectNew.(*corev1.Pod)
	isTenant := isTenantObj(evt.ObjectNew)
	var isTenantOwnedPod bool
	var podMarkedAsStale bool
	var cockroachReadinessChanged bool
	var isFailedTenantOwnedPod bool

	if isPod {
		for _, ownerReferences := range pod.OwnerReferences {
			if ownerReferences.Kind == crdbv1beta1.CrdbTenantKind {
				isTenantOwnedPod = true
				break
			}
		}

		if isTenantOwnedPod {
			oldPod, _ := evt.ObjectOld.(*corev1.Pod)
			if !manifests.IsTenantPodStale(oldPod) && manifests.IsTenantPodStale(pod) {
				podMarkedAsStale = true
			}
			cockroachReadinessChanged = manifests.IsCockroachReady(oldPod) != manifests.IsCockroachReady(pod)
			isFailedTenantOwnedPod = pod.Status.Phase == corev1.PodFailed
		}
	} else if isTenant {
		oldJSON, _ := json.Marshal(evt.ObjectOld)
		newJSON, _ := json.Marshal(evt.ObjectNew)
		logging.Infof(ctx, "Changes found for tenant [%s]. Old: %s\n New: %s\n", evt.ObjectNew.GetName(), oldJSON, newJSON)
		return true
	}
	return isTenant || isFailedTenantOwnedPod || podMarkedAsStale || cockroachReadinessChanged
}

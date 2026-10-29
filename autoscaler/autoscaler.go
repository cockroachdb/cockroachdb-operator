package autoscaler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	opmetrics "github.com/cockroachlabs/cockroach-enterprise-operator/controllers/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/tenantpool"
	"github.com/cockroachlabs/cockroach-enterprise-operator/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// tenantCacheTTL governs the amount of time that tenant metadata will stay in
// the autoscaler cache before it is evicted.
// NOTE: Each time a tenant's metadata is fetched from the cache, its expiration
// time is reset.
const tenantCacheTTL = 5 * time.Minute

// EventSource defines the name of the event source used when emitting events.
// This will be shown in the "From" field when using kubectl describe.
const EventSource = "crdbtenant-autoscaler"

// Autoscaler automatically determines the right number of SQL pods that each
// tenant should have assigned to it, and then sets the Spec.Pods field of each
// tenant to that value. The CrdbTenant reconciler will then either stamp new
// pods or mark existing pods as DRAINING, depending on whether the tenant is
// scaling up or down, respectively.
//
// The autoscaler determines the right number of SQL pods by monitoring the
// CPU usage and SQL query count metrics produced by running SQL pods. Those
// metrics are scraped by directly connecting to each running tenant SQL pod
// periodically (by default every 3 seconds). Once the metrics for a tenant are
// collected and aggregated, it then calculates how many pods to assign to that
// tenant based on these formulas:
//
//   - At least one pod for average CPU usage in last 5 minutes
//   - At least one pod for 1/3rd peak CPU usage in last 5 minutes
//
// In addition, if average CPU usage falls below 2% and no queries have been run
// against the tenant in the last 5 minutes, then the autoscaler will suspend
// the tenant (i.e. set Spec.Pods = 0).
type Autoscaler struct {
	ctl                *kube.Ctl
	metricsManager     metrics.TenantMetricsReader
	nodeMetricsManager metrics.NodeMetricsReader
	tenantPoolCache    tenantpool.Cache
	tenantCache        *kube.Cache
	emit               kube.EventEmitter
}

var _ manager.LeaderElectionRunnable = &Autoscaler{}

// Options defines a set of options to use when constructing a new instance.
type Options struct {
	Ctl     *kube.Ctl
	Emitter kube.EventEmitter
}

// New constructs a new autoscaler. Call Run to start it.
func New(ctx context.Context, opts Options) *Autoscaler {
	a := &Autoscaler{
		ctl:  opts.Ctl,
		emit: opts.Emitter,
	}
	a.metricsManager = metrics.NewTenantManager(ctx,
		metrics.WithUpdateFunc(func(ctx context.Context, metrics *metrics.TenantMetrics) {
			_ = a.updateDesiredPods(ctx, metrics)
		}))
	a.nodeMetricsManager = metrics.NewNodeMetricsManager(ctx, opts.Ctl)
	a.tenantCache = kube.NewCache(a.ctl, &crdbv1beta1.CrdbTenant{})
	a.tenantCache.TTL = tenantCacheTTL

	return a
}

// NodeMetricsManager returns the manager for node metrics used for
// stamping pods. This method and all methods on the manager are thread-safe.
func (a *Autoscaler) NodeMetricsManager() metrics.NodeMetricsReader {
	return a.nodeMetricsManager
}

// MetricsManager returns the manager for all tenant and pod metrics used for
// scaling. This method and all methods on the manager are thread-safe.
func (a *Autoscaler) MetricsManager() metrics.TenantMetricsReader {
	return a.metricsManager
}

// Start starts the autoscaler, which will run until the provided context is
// closed. Every X seconds, the autoscaler calls Prometheus to get updated
// metrics and calculate how to scale each tenant accordingly.
//
// NOTE: Typically, Start is called on a background goroutine, since it runs
// forever, or until cancellation.  Start implements manager.Runnable.
func (a *Autoscaler) Start(ctx context.Context) error {
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		logging.Info(ctx, "running tenant cache")
		return a.tenantCache.Run(groupCtx)
	})

	group.Go(func() error {
		mgr, ok := a.metricsManager.(metrics.TenantMetricsManager)
		if !ok {
			// Tests use [metrics.TestTenantManager] which doesn't
			// implement AddPod and DeletePod and doesn't need an
			// informer running
			return nil
		}
		logging.Debug(ctx, "running new metric monitor")
		cs, err := kubernetes.NewForConfig(a.ctl.RestConfig())
		if err != nil {
			logging.Errorf(ctx, "metrics monitor error: %+v", err)
			return err
		}
		informer, err := NewInformer(ctx, cs, a.ctl.Namespace(), handlerFuncs{
			AddFunc:    mgr.AddPod,
			DeleteFunc: mgr.DeletePod,
		})
		if err != nil {
			logging.Errorf(ctx, "metrics monitor error: %+v", err)
			return err
		}
		informer.Run(ctx)
		return nil
	})

	return group.Wait()
}

// NeedLeaderElection implements manager.LeaderElectionRunnable to tell the
// k8s manager that this runnable cannot be run prior to becoming the leader.
// Otherwise, non-leader operator pods start an autoscaler which is undesirable.
func (a *Autoscaler) NeedLeaderElection() bool {
	return true
}

// updateDesiredPods will check and update the number of pods per
// tenant/namespace as well as the metrics that track the successful or failed
// update attempts.
func (a *Autoscaler) updateDesiredPods(ctx context.Context, metrics *metrics.TenantMetrics) bool {
	updated, err := a.updateDesiredPodsNoMetrics(ctx, metrics)
	if err != nil {
		logging.Errorf(ctx,
			"error updating desired pods for tenant %v: %v",
			metrics.Key, err)
		opmetrics.TenantsScaledTotal.With(opmetrics.ErrorLabel).Inc()
		return false
	}
	// We successfully updated the Spec.DesiredPods of a tenant.
	if updated {
		opmetrics.TenantsScaledTotal.With(opmetrics.SuccessLabel).Inc()
	}
	return true
}

func (a *Autoscaler) updateDesiredPodsNoMetrics(
	ctx context.Context, m *metrics.TenantMetrics,
) (bool, error) {
	ctl := a.ctl.WithNamespace(m.Key.ObjectKey().Namespace)

	scaled := false
	var cachedTenant *crdbv1beta1.CrdbTenant
	err := kube.RetryConflicts(func() error {
		// If the cache value is stale, trying to update the tenant will fail
		// with a conflict, so retries should mark the tenant as dirty to
		// retrieve a fresh object from the API server.
		if cachedTenant != nil {
			a.tenantCache.MarkAsDirty(cachedTenant)
		}

		obj, err := a.tenantCache.Get(ctx, m.Key.ObjectKey())
		if err != nil {
			return errors.Wrapf(err, "getting cached tenant %v", m.Key)
		}
		cachedTenant = obj.(*crdbv1beta1.CrdbTenant)

		maxPods := cachedTenant.Spec.MaxPods
		if maxPods == nil && cachedTenant.Spec.TenantPool != nil {
			// Fall back on the pool's max pods if the tenant does not have a max
			// pods.
			pool, err := a.tenantPoolCache.Get(ctx, ctl, *cachedTenant.Spec.TenantPool)
			if err != nil {
				return errors.Wrapf(err, "getting %v's pool %v", m.Key, *cachedTenant.Spec.TenantPool)
			}
			maxPods = pool.MaxPods
		}

		requestedPods := calculateClampedPods(cachedTenant.Spec.Pods, maxPods, m)
		if requestedPods == cachedTenant.Spec.Pods {
			return nil
		}

		if cachedTenant.Spec.DisableAutoscale {
			logging.Infof(ctx, "auto-scale is disabled for tenant %d, skip scale to %d pods",
				cachedTenant.Spec.TenantID, cachedTenant.Spec.Pods)
			return nil
		}

		// A shallow copy of the tenant for modification.
		modifiedTenant := *cachedTenant
		modifiedTenant.Spec.Pods = requestedPods

		// No need to block until the reconciler actually scales the pods.
		if err := ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, &modifiedTenant); err != nil {
			return errors.Wrap(err, "updating the tenant pod count")
		}

		scaled = true
		recordEvent(ctx, a.emit, &modifiedTenant, cachedTenant.Spec.Pods, requestedPods, maxPods, m, nil)
		return nil
	})
	if err != nil {
		return false, err
	}

	return scaled, nil
}

// recordEvent will create a new event object for the given tenant. Also logs
// a copy of the event.
// Here is an example:
//
//	kubectl get events -A --field-selector involvedObject.kind=CrdbTenant
//	NAMESPACE                    LAST SEEN   TYPE     REASON            OBJECT                 MESSAGE
//
// test-integration-autoscale 8m8s Normal ScaleUp crdbtenant/tenant-10 1->3 MaxCpu5m 762% AvgCpu5m 28% MaxPods 5 DeltaQ2m 181575 Jobs 0 Cons 302 Up 1m50s
// test-integration-autoscale 4m58s Normal ScaleUp crdbtenant/tenant-10 3->4 MaxCpu5m 762% AvgCpu5m 314% MaxPods 5 DeltaQ2m 1072143 Jobs 0 Cons 302 Up 5m0s
// test-integration-autoscale 3m58s Normal ScaleUpToMax crdbtenant/tenant-10 4->5 MaxCpu5m 762% AvgCpu5m 404% MaxPods 5 DeltaQ2m 1072091 Jobs 0 Cons 302 Up 6m0s
// test-integration-autoscale 88s Normal ScaleDown crdbtenant/tenant-10 5->4 MaxCpu5m 771% AvgCpu5m 349% MaxPods 5 DeltaQ2m 198494 Jobs 0 Cons 0 Up 8m30s
// test-integration-autoscale 58s Normal ScaleDownToZero crdbtenant/tenant-10 4->0 MaxCpu5m 771% AvgCpu5m 304% MaxPods 5 DeltaQ2m 0 Jobs 0 Cons 0 Up 9m0s
func recordEvent(
	ctx context.Context,
	emit kube.EventEmitter,
	tenant *crdbv1beta1.CrdbTenant,
	podsBefore int32,
	podsAfter int32,
	maxPods *int32,
	metrics *metrics.TenantMetrics,
	err error,
) {
	reason := ""
	if err != nil {
		reason = "FailedTo"
	}
	if podsBefore > podsAfter {
		reason += "ScaleDown"
	} else {
		reason += "ScaleUp"
	}
	if podsAfter == 0 {
		reason += "ToZero"
	}
	if maxPods != nil && podsAfter == *maxPods {
		reason += "ToMax"
	}
	msg := fmt.Sprintf("%d->%d", podsBefore, podsAfter)
	if metrics.MaxCpu5m > 0 {
		msg += fmt.Sprintf(" MaxCpu5m %.0f%%", metrics.MaxCpu5m*100)
	}
	if metrics.AvgCpu5m > 0 {
		msg += fmt.Sprintf(" AvgCpu5m %.0f%%", metrics.AvgCpu5m*100)
	}
	if maxPods != nil {
		msg += fmt.Sprintf(" MaxPods %d", *maxPods)
	}
	msg += fmt.Sprintf(" DeltaQ2m %t Jobs %.0f Cons %.0f Up %s",
		metrics.DeltaQueries2m, metrics.JobsRunning, metrics.ConnsOpen, time.Duration(metrics.Uptime),
	)
	pods := []string{}
	for _, podMetrics := range metrics.Pods {
		pods = append(pods, fmt.Sprintf("%.0f,%.0f%%", podMetrics.SqlConns, podMetrics.SqlCpuIRate*100))
	}
	msg += fmt.Sprintf(" Pods [%s]", strings.Join(pods, "|"))
	if err != nil {
		logging.Warnf(ctx, "%s %s %s: %v", metrics.ID(), reason, msg, err)
		emit.Eventf(tenant, corev1.EventTypeWarning, reason, "%s: %v", msg, err)
	} else {
		logging.Infof(ctx, "%s %s %s", metrics.ID(), reason, msg)
		emit.Event(tenant, corev1.EventTypeNormal, reason, msg)
	}
}

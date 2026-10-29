package tenant

import (
	"context"
	"encoding/base32"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	opmetrics "github.com/cockroachlabs/cockroach-enterprise-operator/controllers/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/restartclass"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/envutil"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ForceDrainTimeout is the time period after which a draining pod will be
// terminated, after attempts to gracefully terminate have failed (due to
// continuous activity over the time period).
//
// NOTE: This is independent of CRDB's draining logic, which is invoked when the
// pod is actually terminated (and cannot be cancelled, unlike the operator's
// DRAINING state). K8s will terminate by first sending a SIGTERM and then 30
// seconds later a SIGKILL. However, at this time, SQL-only pods do not respect
// query_wait and so immediately shut down upon receiving SIGTERM. See
// https://github.com/cockroachdb/cockroach/issues/67783.
//
// NOTE: Ideally, we'd be *moving* connections from pod to pod rather than
// cutting them. But that'd still be "draining" the connections and we'd still
// need to force terminate pods that can't be moved within the 10 minute window
// (e.g. due to a long-running transaction or usage of session temp tables).
const ForceDrainTimeout = 10 * time.Minute

const (
	// DrainingRequeueAfter is the amount of time to wait between shutting down
	// draining pods.
	DrainingRequeueAfter = 30 * time.Second
	// PendingPodRequeueAfter is the amount of time to wait before
	// checking if pods in the pending phase have booted
	// successfully. This timeout is similar to the retry duration in
	// tenantdir if a warm pod cannot be found.
	PendingPodRequeueAfter = 500 * time.Millisecond
	// LowCapacityRequeueAfter is the amount of time to wait between attempting
	// to roll over stale pods again when the tenantpool has a low capacity.
	// Generally this means that there are too many attempts trying to stamp
	// pods from the tenantpool. Pick an arbitrary 15 seconds.
	LowCapacityRequeueAfter = 15 * time.Second
)

// lowCapacityErr corresponds to an error that indicates that the operation did
// not succeed because the number of unstamped tenantpool pods was below a
// certain threshold (i.e. low capacity).
var lowCapacityErr = errors.New("low capacity in tenant pool")

// minDrainPeriod is the amount of time that a SQL pod needs to be in the
// DRAINING state before it could be terminated.
const minDrainPeriod = 1 * time.Minute

// nodeCPUUsageStampingThresholdEnvVar overrides nodeCPUUsageStampingThreshold at startup.
const nodeCPUUsageStampingThresholdEnvVar = "TENANT_NODE_CPU_USAGE_STAMPING_THRESHOLD"

// nodeCPUUsageStampingMinFilterPercentageEnvVar overrides nodeCPUUsageStampingMinFilterPercentage at startup.
const nodeCPUUsageStampingMinFilterPercentageEnvVar = "TENANT_NODE_CPU_USAGE_STAMPING_MIN_FILTER_PERCENTAGE"

// nodeCPUUsageStampingThreshold is the node CPU util percentage at or above
// which a node's warm pods are excluded from stamping. If filtering leaves
// too few warm pods (see nodeCPUUsageStampingMinFilterPercentage), we ignore
// the filter and use all warm pods. Overridable via
// nodeCPUUsageStampingThresholdEnvVar.
var nodeCPUUsageStampingThreshold = 70

// nodeCPUUsageStampingMinFilterPercentage is the minimum percentage of warm pool pods
// that must remain after we finish filtering. If fewer than this percentage remain,
// we don't use the filter and use the full pool of warm pods. Overridable via
// nodeCPUUsageStampingMinFilterPercentageEnvVar (TENANT_NODE_CPU_USAGE_STAMPING_MIN_FILTER_PERCENTAGE).
// This should not be set too low since multiple warm pods can be scheduled onto the same node,
// and too low a percentage could then cause hotspotting.
var nodeCPUUsageStampingMinFilterPercentage = 50

// LoadTenantEnvVarConfig reads env var overrides and updates defaults.
func LoadTenantEnvVarConfig(ctx context.Context) {
	envutil.LoadInt(ctx, nodeCPUUsageStampingThresholdEnvVar, &nodeCPUUsageStampingThreshold)
	envutil.LoadInt(ctx, nodeCPUUsageStampingMinFilterPercentageEnvVar, &nodeCPUUsageStampingMinFilterPercentage)
}

// TenantPodScaler is a helper struct used by ScaleTenant.
type TenantPodScaler struct {
	Ctl                *kube.Ctl
	MetricManager      metrics.TenantMetricsReader
	NodeMetricManager  metrics.NodeMetricsReader
	Rng                rand.Source
	Emit               *kube.EventEmitter
	PodSyncer          TenantPodSyncer
	NodeLocalityReader cloud.NodeLocalityReaderFunc
}

type plan struct {
	// newPods is how many pods need to be stamped from the warm pool.
	newPods int
	// drainPods are the pods that should be set to DRAINING state.
	drainPods []corev1.Pod
	// undrainPods are DRAINING pods that should be set to RUNNING state.
	undrainPods []corev1.Pod
	// terminatePods are DRAINING pods that should be terminated.
	terminatePods []corev1.Pod
	// requeue indicates whether the operator needs to requeue reconciliation of
	// the tenant. This is true when there exists a draining or stale pod after
	// scaling.
	requeue bool
}

func (p plan) isScaleUp() bool {
	return p.newPods > 0 || len(p.undrainPods) > 0
}

func (p plan) isScaleDown() bool {
	return len(p.drainPods) > 0 || len(p.terminatePods) > 0
}

func (s *TenantPodScaler) ScaleTenantWithMetrics(
	ctx context.Context, tenant *crdbv1beta1.CrdbTenant,
) (bool, time.Duration, error) {
	opmetrics.TenantScaleAttemptCount.Inc()
	scalePlan, requeueDuration, err := s.scaleTenant(ctx, tenant)
	if err != nil {
		opmetrics.TenantScaleFailureCount.WithLabelValues(
			strconv.FormatBool(scalePlan.isScaleUp()),
			strconv.FormatBool(scalePlan.isScaleDown()),
		).Inc()
		return false, 0, err
	}
	return scalePlan.requeue, requeueDuration, nil
}

// scaleTenant ensures that the number of running pods for the given tenant
// matches the desired number of pods, as specified in tenant.Spec.Pods. If
// there are too many RUNNING pods, then scaleTenant will put excess pods into
// the DRAINING state, in preparation for termination. If there are not enough
// RUNNING pods, then scaleTenant will convert existing (non-stale) DRAINING pods to RUNNING
// pods, or else will stamp new pods from the tenant pool. Any DRAINING pods
// with zero remaining connections will be terminated. If there are draining
// pods after scaleTenant completes, scaleTenant will return draining = true.
func (s *TenantPodScaler) scaleTenant(
	ctx context.Context, tenant *crdbv1beta1.CrdbTenant,
) (plan, time.Duration, error) {
	cluster, err := s.getClusterForTenant(ctx, tenant)
	if err != nil {
		return plan{}, 0, err
	}

	specPods, statusPods := int(tenant.Spec.Pods), int(tenant.Status.Pods)
	tenantID := tenant.Spec.TenantID

	// List all pods that have been stamped with this tenant's ID.
	// See listStampedPods for details on how the list is sorted, and it's
	// implications on scaling.
	allStampedPods, err := s.listStampedPods(ctx, tenant)
	if err != nil {
		return plan{}, 0, err
	}

	// Collect metrics on the number of stamped pods for this tenant
	s.collectMetrics(allStampedPods, tenant)

	// Filter the list of stamped pods to only running, stamped pods.
	// Additionally, delete any phase=pending adhoc pods that are over the
	// allowed time limit.
	stampedPods := []corev1.Pod{}
	for _, pod := range allStampedPods {
		if pod.Status.Phase == corev1.PodRunning {
			stampedPods = append(stampedPods, pod)
		} else if pod.Status.Phase == corev1.PodPending && manifests.IsAdhocPod(&pod) {
			maxPendingAdhocPodAge, err := manifests.MaxTenantAdhocPendingPodAge(cluster)
			if err != nil {
				return plan{}, 0, err
			}

			if time.Since(pod.CreationTimestamp.Time) > maxPendingAdhocPodAge {
				// This pod has been booting for too long. Delete it and attempt scaling up again
				logging.Warnf(ctx,
					"deleting adhoc pod %s for tenant %s because it did not start time in time",
					pod.Name, tenant.Name)
				opmetrics.TenantAdhocPodFailedToStartTotal.Inc()
				if err := s.Ctl.DeleteWithOptions(ctx, kube.DeleteOptions{NoWait: true}, &pod); err != nil {
					return plan{}, 0, err
				}
			} else {
				// We need to check these pending pods again and attempt to start them
				logging.Infof(
					ctx,
					"found a pending pod %s for tenant %s, will requeue and check again soon",
					pod.Name, tenant.Name)
				opmetrics.TenantAdhocPodPendingRequeueTotal.Inc()
				return plan{requeue: true}, PendingPodRequeueAfter, nil
			}
		}
	}

	// Check for any pending pod that was created by a previous reconciliation
	// that failed midway through, having never been marked as RUNNING.
	pendingPods := getPendingPodsFromList(stampedPods)
	for _, pendingPod := range pendingPods {
		if err := s.startPendingPod(ctx, pendingPod, tenant); err != nil {
			return plan{}, 0, err
		}
	}

	// TODO(alyshan): The general solution described in CC-8011 would identify these
	// stale pool pods. But for now we identify and label them as stale here.
	if err := labelStalePoolPods(ctx, s.Ctl, stampedPods, tenant); err != nil {
		return plan{}, 0, err
	}

	scalePlan, err := s.planScale(ctx, tenant, cluster, stampedPods)
	if err != nil {
		return plan{}, 0, err
	}

	logging.Infof(ctx, "scaling %v with plan: %v current pods, %v pods to stamp, %v pods to undrain, %v pods to drain, %v pods to terminate",
		tenant.Name, tenant.Status.Pods, scalePlan.newPods, len(scalePlan.undrainPods), len(scalePlan.drainPods), len(scalePlan.terminatePods))

	// isStaleRollOver is the single case where we stamp a new pod and drain a stale
	// one at the same time. A scale-down also drains stale pods but terminates them
	// without replacement, so this distinguishes a rolling update from a scale-down.
	isStaleRollOver := scalePlan.newPods == 1 && len(scalePlan.drainPods) == 1

	// Pods that need to be converted from DRAINING to RUNNING.
	for _, pod := range scalePlan.undrainPods {
		err := s.setPodState(ctx, &pod, manifests.TenantPodRunningState)
		if err != nil {
			return plan{}, 0, err
		}
	}

	// Pods that need to be added for this tenant.
	var lastStampedPod *corev1.Pod
	for range scalePlan.newPods {
		startTime := time.Now()

		var pendingPod *corev1.Pod
		var err error
		if isStaleRollOver {
			// Attempt to create an adhoc createdPod for stale rollovers
			if pendingPod, err = s.createAdhocTenantPod(ctx, cluster, tenant); err != nil {
				// Fallback to stamping a pod in the warm pool.
				pendingPod, err = s.stampTenantPod(ctx, cluster, tenant, isStaleRollOver)
			}
		} else {
			pendingPod, err = s.stampTenantPod(ctx, cluster, tenant, isStaleRollOver)
		}
		if err != nil && errors.Is(err, lowCapacityErr) {
			return plan{requeue: true}, LowCapacityRequeueAfter, nil
		} else if err != nil {
			return plan{}, 0, err
		} else if manifests.IsAdhocPod(pendingPod) {
			// We created an ad-hoc pod that won't be in the running
			// phase yet. Re-queue and don't try to start the pod.
			return plan{requeue: true}, PendingPodRequeueAfter, nil
		} else {
			// Now start the SQL process running on the pod.
			if err = s.startPendingPod(ctx, pendingPod, tenant); err != nil {
				return plan{}, 0, err
			}
			opmetrics.PodStampAndStartTime.Observe(time.Since(startTime).Seconds())
		}
		lastStampedPod = pendingPod
	}

	// For stale rollovers, wait for the replacement pod's CockroachDB process
	// to become ready before draining the stale pod. This prevents a gap where
	// neither the old nor new pod is serving the tenant's SQL traffic.
	if lastStampedPod != nil && isStaleRollOver {
		if err := s.waitForCockroachReady(ctx, lastStampedPod); err != nil {
			logging.Warnf(ctx, "replacement pod %s did not become ready in time: %v; proceeding with drain", lastStampedPod.Name, err)
		}
	}

	// Pods that need to be put into DRAINING state.
	for _, pod := range scalePlan.drainPods {
		if err := s.setPodState(ctx, &pod, manifests.TenantPodDrainingState); err != nil {
			return plan{}, 0, err
		}
		// A stale rollover replaces one pod with a fresh one, so we log it as a rolling
		// update for parity with nodectrl. A scale-down also drains stale pods but
		// terminates them without replacement, so those must not be logged as rolling
		// updates. isStaleRollOver is true only for the replace case.
		if isStaleRollOver {
			logTenantPodRestart(ctx, &pod, tenant, restartclass.RestartReasonRollingUpdate)
		}
	}

	// Pods that need to be terminated.
	for _, pod := range scalePlan.terminatePods {
		// If the pod hasn't been put into the DRAINING state yet, set it now
		// so that the proxy doesn't route connections to the pod while it goes
		// into the Terminating phase. This is best-effort only since there
		// might be delays before the proxy's watcher detects the changes to

		state, _ := manifests.GetTenantPodState(&pod)
		if state != manifests.TenantPodDrainingState {
			if err := s.setPodState(ctx, &pod, manifests.TenantPodDrainingState); err != nil {
				return plan{}, 0, err
			}
		}

		// No need to wait for this to complete, so do it asynchronously.
		logging.Infof(ctx, "terminating pod %s for tenant %d", pod.Name, tenant.Spec.TenantID)
		if err := s.Ctl.DeleteWithOptions(ctx, kube.DeleteOptions{NoWait: true}, &pod); err != nil {
			return plan{}, 0, err
		}
	}

	if statusPods != specPods {
		// Update the tenant status to reflect the number of pods now running.
		tenant.Status.Pods = int32(specPods)
		if err := s.Ctl.UpdateStatus(ctx, tenant); err != nil {
			return plan{}, 0, errors.Wrapf(err, "failed to set Status.Pods on tenant %d", tenantID)
		}

		if statusPods == 1 && specPods == 0 && !scalePlan.requeue {
			logging.Infof(ctx, "suspended tenant %d", tenantID)
		} else if statusPods == 0 && specPods > 0 {
			logging.Infof(ctx, "resumed tenant %d", tenantID)
		} else if specPods > statusPods {
			logging.Infof(ctx, "started %d pods for tenant %d", specPods-statusPods, tenantID)
		} else {
			logging.Infof(ctx, "draining %d pods for tenant %d", statusPods-specPods, tenantID)
		}

		// For now, we only emit events if were scaling up from 0 or down to 0.
		// This is to keep the volume low for now. Overtime, we may want to
		// observe more changes, or perhaps a sampling of them.
		if specPods == 0 || statusPods == 0 {
			s.Emit.Infof(tenant, "Scaled", "Scaled pods from %d to %d", statusPods, specPods)
		}
	}

	// scalePlan.requeue is only true if there are draining pods
	return scalePlan, DrainingRequeueAfter, nil
}

func (s *TenantPodScaler) planScale(
	ctx context.Context,
	tenant *crdbv1beta1.CrdbTenant,
	cluster *crdbv1beta1.CrdbCluster,
	stampedPods []corev1.Pod,
) (scalePlan plan, err error) {
	// If MaxPods is 0, all pods can be terminated right away. There's no need
	// to gracefully drain since connections have no pods to be transferred to,
	// and no new pods can be spun up.
	if tenant.Spec.MaxPods != nil && *tenant.Spec.MaxPods == 0 {
		// tenant.Spec.Pods must be 0, since it is clamped to MaxPods.
		if tenant.Spec.Pods == 0 {
			// Terminate everything.
			scalePlan.terminatePods = stampedPods
			return scalePlan, nil
		}
		// This should not happen unless there's a bug in the scaler code.
		// Fallthrough and log an error.
		logging.Errorf(ctx, "Spec.Pods must be 0 for %v, but found %d, skipping fast scale down for MaxPods=0",
			tenant.Name, tenant.Spec.Pods)
	}

	allRunning, staleRunning, draining, staleDraining := partitionPods(stampedPods)
	running, err := s.removeUnready(ctx, tenant, cluster, allRunning)
	if err != nil {
		return plan{}, err
	}

	scale := int(tenant.Spec.Pods) - (len(running) + len(staleRunning))
	staleRunningExists := len(staleRunning) > 0

	if scale > 0 {
		// We need more RUNNING pods.
		scalePlan.newPods = scale
	} else if scale == 0 {
		// Don't need to scale up or down.
		// Check if a stale running pod can be marked as draining.
		// We only convert stale running to stale draining when there aren't
		// any existing stale draining pods. This enables a "rolling upgrade"
		// of stale pods.
		if len(staleRunning) > 0 && len(staleDraining) == 0 {
			// Ask for a new pod to replace the stale running pod.
			// Ask to drain the stale running pod.
			scalePlan.newPods = 1
			scalePlan.drainPods = append(scalePlan.drainPods, staleRunning[0])
			logging.Infof(ctx, "rolling over stale pod %v for %v", staleRunning[0].Name, tenant.Name)
			if len(staleRunning) == 1 {
				staleRunningExists = false
			}
		}
	} else {
		// scale < 0 so make it a positive int.
		numToDrain := -1 * scale
		// We need less RUNNING pods.
		// In this case we are okay with marking multiple stale running pods as draining
		// because it is a scale down of the tenant. So we aren't replacing the stale pods
		// with new ones like in the case above.
		runningPods := append(running, staleRunning...)
		// Prefer draining pods that are stale, and pods with lower CPU consumption (take
		// from end of list).
		scalePlan.drainPods = runningPods[len(runningPods)-numToDrain:]
	}

	// Check if we need to scale up and can re-use draining pods.
	if scalePlan.newPods > 0 && len(draining) > 0 {
		undrain := min(len(draining), scalePlan.newPods)
		// Take draining pods with higher CPU consumption and transition them to running.
		scalePlan.undrainPods = draining[:undrain]
		draining = draining[undrain:]
		scalePlan.newPods -= len(scalePlan.undrainPods)
	}

	// Check if any draining pods need terminating.
	toTerminate := append(draining, staleDraining...)
	tenantKey := metrics.TenantKey{Namespace: tenant.Namespace, TenantID: tenant.Spec.TenantID}
	tenantMetrics := s.MetricManager.Get(tenantKey)
	for _, pod := range toTerminate {
		var podMetrics *metrics.PodMetrics
		if tenantMetrics != nil {
			if pm, ok := tenantMetrics.Pods[pod.Name]; ok {
				podMetrics = &pm
			}
		}
		if isReadyToTerminate(&pod, podMetrics) {
			scalePlan.terminatePods = append(scalePlan.terminatePods, pod)
		}
	}

	// If any draining or stale running pods exist after executing the plan,
	// the operator should requeue.
	drainingExists := (len(draining) + len(staleDraining) + len(scalePlan.drainPods) - len(scalePlan.terminatePods)) > 0
	scalePlan.requeue = drainingExists || staleRunningExists

	return scalePlan, nil
}

// startPendingPod takes an already stamped pod in the PENDING state and moves
// it to the RUNNING state after sending the tenant archive to the manager
// process running on the pending pod. The tenant archive as part of the start
// request will not include any sidecar-related configurations other than the
// cockroachdb configuration itself. Such sidecar-related configurations are
// sent as part of the update path since they require credentials.
func (s *TenantPodScaler) startPendingPod(
	ctx context.Context, pendingPod *corev1.Pod, tenant *crdbv1beta1.CrdbTenant,
) error {
	if err := sendStartRequest(
		ctx, s.Ctl, pendingPod, tenant, nil /* credentials */, s.NodeLocalityReader,
	); err != nil {
		return err
	}
	// Once we sent the archive, we will set the annotation for the SQL pod.
	// Note that this gets persisted (i.e. applied) via setPodState.
	pendingPod.Annotations[manifests.TenantPodRevisionAnnotation] = manifests.GetTenantPodHashRevision(pendingPod, tenant)

	// Mark the pod as RUNNING if the start request was successful.
	if err := s.setPodState(ctx, pendingPod, manifests.TenantPodRunningState); err != nil {
		return err
	}

	// If the pod is adhoc, track the time that has passed since the pod was created
	if manifests.IsAdhocPod(pendingPod) {
		opmetrics.TenantAdhocPodPendingTimeMs.Observe(
			float64(time.Since(pendingPod.CreationTimestamp.Time).Milliseconds()),
		)
	}

	// Immediately trigger a sync of the tenant pod in case it uses credentials
	// for its sidecars. This sync uses a context from the pod syncer so the
	// goroutine will terminate properly if the operator shuts down.
	go func() {
		if err := s.PodSyncer.Sync(tenant, pendingPod); err != nil {
			logging.Errorf(
				context.Background(),
				"error syncing pod %s for %s: %v",
				pendingPod.Name,
				tenant.Name,
				err,
			)
		}
	}()
	return nil
}

// stampTenantPod obtains a stamped PENDING SQL pod for the given tenant. This
// is done by pulling a random pod from the tenant pool and stamping it with the
// "tenant-id" label. If there are insufficient pods available in the tenant
// pool, then stampTenantPod will return an error. SQL pods will only transition
// from the PENDING to RUNNING state once certs have successfully been sent.
// Pods on nodes with CPU usage at or above nodeCPUUsageStampingThreshold are
// filtered away, unless too many nodes exceed that (see nodeCPUUsageStampingMinFilterPercentage),
// in which case we don't filter at all.
func (s *TenantPodScaler) stampTenantPod(
	ctx context.Context,
	cluster *crdbv1beta1.CrdbCluster,
	tenant *crdbv1beta1.CrdbTenant,
	errIfLowCapacity bool,
) (*corev1.Pod, error) {
	// Create label selector to list unstamped pods within the tenant pool. All
	// pods that are tracked by the tenant pool must be in the PENDING state
	// because we only attempt to send certs to them once we stamp the pod.
	unstampedPodSelector := manifests.MakeTenantPoolSelector(*tenant.Spec.TenantPool)

	runningPodFieldSelector := fields.ParseSelectorOrDie("status.phase=Running")

	for {
		// Check for context cancellation or timeout.
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// List running, unstamped PENDING pods from the tenant pool.
		var list corev1.PodList
		if err := s.Ctl.List(ctx, &kube.ListOptions{
			LabelSelector: unstampedPodSelector,
			// New pods within the tenant pool start with an Init phase since
			// we use an init container to copy sqlstarter into the cockroachdb
			// container. Only list pods which are Running (i.e. sqlstarter
			// binary is copied, started, and ready to accept certs).
			FieldSelector: runningPodFieldSelector,
			// The max number of pods is currently around 60 (24 nodes * 2.5 multiplier) per pool.
			// Increasing to 100 so there's a bit of a buffer. We don't want to limit it too low
			// so that the random stamping below is able to select from all available warm pods
			// rather than a possible deterministic subset.
			Limit: 100,
		}, &list); err != nil {
			return nil, errors.Wrap(err, "listing running unstamped PENDING pods")
		}

		// Only consider PENDING pods that have their containers ready.
		// There is a subtle race where we call startPendingPod on the pod
		// returned by this function before the sqlstarter is accepting connections.
		// That results in a stamped pod in the RUNNING state, but it is not actually
		// running the CRDB process. By waiting for the containers to be ready, we ensure
		// the sqlstarter is accepting connections, see manifests.tenantPoolReadinessProbe.
		pods := []corev1.Pod{}
		for _, pod := range list.Items {
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.ContainersReady && condition.Status == corev1.ConditionTrue {
					pods = append(pods, pod)
				}
			}
		}

		// If the caller asks to not stamp a pod for the tenant when the pool is at low capacity
		// then return an error. A capacity of one is chosen since it is a simple way to accomplish
		// the behaviour of not exhausting the tenant pool when scaling a tenant.
		//
		// TODO(alyshan): Ideally the threshold chosen for low capacity would be a factor of the
		// tenant pool size. However, we do not want to make extra calls to the K8s API server on
		// scaling requests. A solution like the one described in CC-8011
		// could be used so that the operator can easily get info about tenant pools.
		if errIfLowCapacity && len(pods) == 1 {
			return nil, errors.Wrapf(lowCapacityErr, "tenant pool %s", *tenant.Spec.TenantPool)
		}

		// No more pods available in the tenant pool. This case could happen if
		// we are using up pods faster than we could pre-warm.
		var pendingPod *corev1.Pod
		if len(pods) == 0 {
			// Attempt to create a newly stamped pod on the fly
			pod, err := s.createAdhocTenantPod(ctx, cluster, tenant)
			if err != nil {
				opmetrics.InsufficientPodsErrorTotal.Inc()
				logging.Warnf(ctx, "failed to create a new stamped tenant pod with error: %v", err)
				return nil, errors.Newf("insufficient pods in tenant pool %s", *tenant.Spec.TenantPool)
			}
			pendingPod = pod
		} else {
			// Pick a pod from the warm pool. If the feature flag ClusterHotNodeAwareTenantPodStamping
			// is enabled, then we'll try to filter the pods so that
			// only pods on nodes with CPU usage below nodeCPUUsageStampingThreshold are considered.
			// This avoids us stamping a pod on a node near CPU capacity. If there are too few pods
			// after filtering, then we don't apply any filters and simply use the full pool.
			//
			// After filtering, there are two options:
			// 1. If the CPU aware feature flag is enabled, and if node metrics are available,
			//    then we probabilistically select a pod where nodes with lower CPU usage
			//    have a greater chance of being selected.
			// 2. Otherwise, either if the feature flag is disabled or node metrics are unavailable,
			//    then we randomly stamp a pod.
			//
			// If ClusterHotNodeAwareTenantPodStamping is not enabled, we don't filter the pods at all.
			var err error
			var podToStamp *corev1.Pod
			nodeMetrics := s.NodeMetricManager.GetNodeMetrics()
			var candidatePods []corev1.Pod
			if cluster.Spec.FeatureEnabled(crdbv1beta1.ClusterHotNodeAwareTenantPodStamping) {
				candidatePods = filterCandidatesByNodeCPUUsage(ctx, pods, nodeMetrics)
			} else {
				candidatePods = pods
			}
			// Default to random selection unless overridden by CPU aware feature flag.
			podToStamp = &candidatePods[int(s.Rng.Int63())%len(candidatePods)]
			if cluster.Spec.FeatureEnabled(crdbv1beta1.ClusterCpuAwareTenantPodStamping) {
				// If no metrics/one node are/is available, use the random selections from
				// candidate pods above and don't override.
				if len(nodeMetrics) == 0 || len(nodeMetrics) == 1 {
					logging.Warnf(ctx, "node metrics are not available")
				} else {
					// Pick a pod from the candidate pool by CPU load of nodes.
					podToStamp = s.pickUnstampedPodByNodeLoad(candidatePods, nodeMetrics)
				}
			}
			pendingPod, err = s.tryStampTenantPod(ctx, podToStamp, tenant)
			if err != nil {
				if !kubeerrors.IsConflict(err) {
					// If the Update call failed with a ResourceVersion conflict,
					// then loop around and try again by picking another pod.
					return nil, err
				}
				continue
			}
		}
		return pendingPod, nil
	}
}

// tryStampTenantPod attempts to stamp the given pod with the "tenant-id" label.
// If another tenant races and stamps the pod first, then a K8s Conflict error
// will be returned, and the caller should retry with a different pod.
//
// NOTE: The given pod to stamp must come directly from the tenantpool. In
// other words, it needs to be in the PENDING state and unstamped.
func (s *TenantPodScaler) tryStampTenantPod(
	ctx context.Context, podToStamp *corev1.Pod, tenant *crdbv1beta1.CrdbTenant,
) (*corev1.Pod, error) {
	// Sanity check to ensure that the given pod is in the PENDING state and
	// is unstamped.
	if !isUnstampedPendingPod(podToStamp) {
		return nil, errors.AssertionFailedf("pod %s should be unstamped and in the PENDING state",
			podToStamp.Name)
	}

	// Make each pod a dependency of the tenant, so that when the tenant is
	// deleted, its pods will be deleted as well. K8s will automatically delete
	// the pods when the owning tenant is deleted.
	// NOTE: This also transfers ownership from the existing OwnerReference that
	// refers to the ReplicaSet that created the pod (as part of the tenant pool
	// Deployment).
	ref, refErr := kube.OwnerReferenceFor(s.Ctl.Scheme(), tenant)
	if refErr != nil {
		return nil, refErr
	}
	ref.Controller = new(true)
	kube.SetOwnerReferences([]metav1.OwnerReference{ref}, podToStamp)

	podToStamp.Labels[manifests.TenantIDLabel] = tenant.Spec.TenantID.String()

	err := s.Ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, podToStamp)
	if err != nil {
		// podToStamp is assumed to come from a general pool. This means that
		// multiple threads could end up trying to stamp the same pod. When that
		// happens, we will get a ResourceVersion conflict here. This is the
		// only case where we should retry.
		return nil, err
	}
	return podToStamp, nil
}

func (s *TenantPodScaler) createAdhocTenantPod(
	ctx context.Context, cluster *crdbv1beta1.CrdbCluster, tenant *crdbv1beta1.CrdbTenant,
) (*corev1.Pod, error) {
	adhocEnabled := manifests.AdhocPodsEnabled(cluster, tenant)
	if !adhocEnabled {
		return nil, errors.New("adhoc pod creation is not enabled on this cluster")
	}

	maxPendingAdhocPods, err := manifests.MaxPendingAdhocPods(cluster)
	if err != nil {
		return nil, errors.Wrap(err, "getting max pending adhoc pods for cluster")
	}
	// There is a potential race here. If multiple go routines enter
	// this function at the same time, they could all get a stale
	// count. However, we're not concerned with enforcing a hard
	// limit. Instead, we're more concerned with ensuring that we
	// don't create an unbounded number of pending pods. This approach
	// will eventually keep pending pods bounded, and doesn't require
	// any concurrency control.
	pendingAdhocPodCount, err := s.countTotalPendingAdhocPods(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "getting the current number of pending adhoc pods")
	}

	if pendingAdhocPodCount >= maxPendingAdhocPods {
		return nil, fmt.Errorf("there are currently too many pending adhoc pods: %d", pendingAdhocPodCount)
	}

	var tenantPool appsv1.Deployment
	if err := s.Ctl.Get(
		ctx,
		fmt.Sprintf("tenantpool-%s", *tenant.Spec.TenantPool),
		&tenantPool,
	); err != nil {
		return nil, errors.Wrap(err, "getting tenantpool deployment")
	}

	podTemplate := tenantPool.Spec.Template
	podPrefix := fmt.Sprintf("%s-adhoc", tenantPool.Name)

	// 63 is the max length of a K8s pod name, 5 is the standard length of the pod hash, and 1 is for the "-"
	if len(podPrefix) > 63-5-1 {
		return nil, fmt.Errorf("deployment name %s is too long to ensure pod name uniqueness", tenantPool.Name)
	}
	podHash := strings.ToLower(base32.StdEncoding.EncodeToString([]byte(strconv.Itoa(rand.Int()))))[:5]
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        fmt.Sprintf("%s-%s", podPrefix, podHash),
			Namespace:   podTemplate.Namespace,
			Labels:      podTemplate.Labels,
			Annotations: podTemplate.Annotations,
		},
		Spec: podTemplate.Spec,
	}

	// Update the owner of the pod to the tenant
	ref, refErr := kube.OwnerReferenceFor(s.Ctl.Scheme(), tenant)
	if refErr != nil {
		return nil, refErr
	}
	ref.Controller = new(true)
	kube.SetOwnerReferences([]metav1.OwnerReference{ref}, newPod)

	// Pre-stamp the pod before creation
	newPod.Labels[manifests.TenantIDLabel] = tenant.Spec.TenantID.String()
	// Label this pod to indicate it is an adhoc pod, for simpler logic later
	newPod.Labels[manifests.TenantPoolAdhocPodLabel] = "true"

	// Create the pod with NoWait. The pod will likely not be ready
	// after this function returns, so callers should be sure to
	// requeue and check the pod status some time in the future.
	if err := s.Ctl.CreateWithOptions(
		ctx,
		kube.CreateOptions{NoWait: true},
		newPod); err != nil {
		return nil, errors.Wrap(err, "creating new pod")
	}

	opmetrics.TenantAdhocPodCreationTotal.Inc()
	return newPod, nil
}

// setPodState updates a pod's "state" label with the given value and applies
// the update to the K8s store. Note that setPodState calls Apply, which will
// retry on any update conflicts (e.g. caused by K8s updates to pod status).
// There will never be conflicts caused by concurrent calls to setPodState,
// since only one operator thread at a time can modify a given tenant's pods
// once they've been stamped.
func (s *TenantPodScaler) setPodState(
	ctx context.Context, pod *corev1.Pod, state manifests.TenantPodState,
) error {
	if err := manifests.SetTenantPodState(pod, state); err != nil {
		return err
	}
	return s.Ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, pod)
}

var replacementReadyBackOff = kube.NewCtlDefaultBackOffFn(60 * time.Second)

// waitForCockroachReady polls until the given pod's CockroachDB container
// reports Ready, or until the timeout expires. This is used during stale
// rollovers to ensure the replacement pod can serve SQL traffic before the
// old pod is drained.
func (s *TenantPodScaler) waitForCockroachReady(ctx context.Context, pod *corev1.Pod) error {
	return s.Ctl.WaitForWithOptions(ctx, kube.WaitForOptions{
		DoneFunc: func(obj kube.Object) error {
			if obj == nil {
				return errors.Newf("pod %s not found", pod.Name)
			}
			p := obj.(*corev1.Pod)
			if !manifests.IsCockroachReady(p) {
				return errors.Newf("pod %s CockroachDB container not ready", pod.Name)
			}
			return nil
		},
		BackOff: replacementReadyBackOff,
	}, pod)
}

func isReadyToTerminate(pod *corev1.Pod, podMetrics *metrics.PodMetrics) bool {
	// Only a DRAINING pod is ready for termination.
	state, ts := manifests.GetTenantPodState(pod)
	if state != manifests.TenantPodDrainingState {
		return false
	}

	// If the cockroachdb container is already in a crash loop, there's no point
	// waiting for it to drain, so we will terminate the pod right away.
	for _, container := range pod.Status.ContainerStatuses {
		// Continue searching for other containers.
		if container.Name != manifests.CockroachDB {
			continue
		}
		// The cockroachdb container isn't in a waiting state.
		waitingState := container.State.Waiting
		if waitingState == nil {
			break
		}
		if waitingState.Reason == "CrashLoopBackOff" {
			return true
		}
	}

	// Only terminate pods which have been draining for >= 1m. CRDB takes
	// 10s for the metrics to be updated right after startup. Imposing a
	// minDrainPeriod value here prevents the operator from scaling down
	// the pod right away during startup, especially when there are no
	// metrics.
	drainingFor := timeutil.Now().Sub(ts)
	if drainingFor < minDrainPeriod {
		return false
	}

	// Force-terminate pods that have been draining for >= ForceDrainTimeout,
	// even if metrics are unavailable (e.g. /_status/load is not served by the
	// tenant SQL pod). Without this, pods whose metrics endpoint is broken
	// remain in DRAINING indefinitely, blocking node drain and K8s upgrades.
	if drainingFor >= ForceDrainTimeout {
		return true
	}

	// Pod should not be terminated if metrics are not yet available.
	if podMetrics == nil {
		return false
	}

	// Pod is ready for termination if it has no open SQL connections.
	if podMetrics.SqlConns != 0 {
		return false
	}

	return true
}

// listStampedPods returns a slice of all pods that have a tenant-id label that
// matches the given tenant ID. The returned list is sorted in this order:
//
//  1. Whether pod is in the DRAINING state (DRAINING pod sorts last)
//  2. Staleness (pods labelled as stale sort last)
//  3. CPU consumption (higher CPU sorts first)
//
// The scaler will pick pods to drain starting at the end of the list and
// working backwards, so this ordering will prefer to drain pods that are stale.
// And if no stale pods exist, then it prefers to drain pods with lower CPU consumption.
// Lastly, the scaler does not re-use stale DRAINING pods for scale ups.
// TODO(alyshan): Consider not doing a custom sorting of pods. The SQL proxy performs
// connection re-balancing, so having the TenantPodScaler drain and un-drain pods based
// on CPU consumption may be unnecessary. Furthermore, partitionPods does not rely on the
// ordering of the list of pods, so sorting by state and staleness is unnecessary as well.
func (s *TenantPodScaler) listStampedPods(
	ctx context.Context, tenant *crdbv1beta1.CrdbTenant,
) ([]corev1.Pod, error) {
	tenantID := tenant.Spec.TenantID
	// Fetch pods in all phases for the purpose of metrics collection.
	// We will filter out all non-running pods later.
	options := kube.ListOptions{
		LabelSelector: manifests.MakeStampedPodSelector(tenantID),
	}
	var stampedPods corev1.PodList
	if err := s.Ctl.List(ctx, &options, &stampedPods); err != nil {
		return nil, errors.Wrap(err, "error listing pods")
	}
	podNames := []string{}
	for _, p := range stampedPods.Items {
		podNames = append(podNames, p.Name)
	}
	logging.Debugf(ctx, "tid %d stamped pods %+v", tenantID, podNames)
	// Get CPU metrics for all pods in the tenant.
	tenantKey := metrics.TenantKey{Namespace: tenant.Namespace, TenantID: tenant.Spec.TenantID}
	tenantMetrics := s.MetricManager.Get(tenantKey)
	var podMetricsMap metrics.PodMetricsMap
	if tenantMetrics != nil {
		podMetricsMap = tenantMetrics.Pods
	}

	// Sort the list with DRAINING pods last, then stale pods last, and
	// finally by CPU consumption.
	sort.Slice(stampedPods.Items, func(i, j int) bool {
		podI := &stampedPods.Items[i]
		podJ := &stampedPods.Items[j]

		stateI, _ := manifests.GetTenantPodState(podI)
		stateJ, _ := manifests.GetTenantPodState(podJ)
		if stateI != stateJ {
			if stateI == manifests.TenantPodDrainingState {
				return false
			} else if stateJ == manifests.TenantPodDrainingState {
				return true
			}
		}

		staleI := manifests.IsTenantPodStale(podI)
		staleJ := manifests.IsTenantPodStale(podJ)
		if staleI && !staleJ {
			return false
		} else if staleJ && !staleI {
			return true
		}

		// Sort by CPU consumption. If there is no label or the label is invalid,
		// then assume these metrics are zero.
		var metricsI, metricsJ metrics.PodMetrics
		if podMetricsMap != nil {
			metricsI = podMetricsMap[podI.Name]
			metricsJ = podMetricsMap[podJ.Name]
		}
		return metricsI.SqlCpuIRate > metricsJ.SqlCpuIRate
	})

	return stampedPods.Items, nil
}

// collectMetrics reports metrics on the status of pods for a tenant
// It will report metrics on:
// - The phase of each pod
// - The PodReady condition
func (s *TenantPodScaler) collectMetrics(pods []corev1.Pod, tenant *crdbv1beta1.CrdbTenant) {
	phaseCount := map[corev1.PodPhase]int{}
	notReadyCount := 0
	for _, pod := range pods {
		phaseCount[pod.Status.Phase] += 1
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionFalse {
				notReadyCount += 1
				break
			}
		}
	}

	for phase, count := range phaseCount {
		if phase == corev1.PodRunning {
			continue
		}
		if count > 0 {
			opmetrics.TenantPodNotRunningCount.WithLabelValues(string(phase)).Observe(float64(count))
		}

	}
	if notReadyCount > 0 {
		opmetrics.TenantPodNotReadyCount.WithLabelValues(tenant.Name).Observe(float64(notReadyCount))
	}
}

func (s *TenantPodScaler) getClusterForTenant(
	ctx context.Context, tenant *crdbv1beta1.CrdbTenant,
) (*crdbv1beta1.CrdbCluster, error) {
	var clusterName string
	for _, owner := range tenant.OwnerReferences {
		if owner.Kind == crdbv1beta1.CrdbClusterKind {
			clusterName = owner.Name
		}
	}
	if clusterName == "" {
		logging.Warnf(ctx, "tenant %s does not have an owning cluster", tenant.Name)
		// only emit the metric if the tenant is active
		if tenant.Spec.Pods > 0 {
			opmetrics.ActiveTenantMissingOwnerCount.WithLabelValues(tenant.Name).Inc()
		}

		var clusters crdbv1beta1.CrdbClusterList
		if err := s.Ctl.List(ctx, &client.ListOptions{}, &clusters); err != nil {
			return nil, err
		}
		if len(clusters.Items) != 1 {
			logging.Warnf(
				ctx,
				"could not determine a crdbcluster for tenant %s. found %d crdbcluster objects",
				tenant.Name, len(clusters.Items))
			return nil, fmt.Errorf("unable to find cluster for tenant %s", tenant.Name)
		}

		// TODO(mwstobo) we should likely run a tenant update here, to set the owner reference properly
		return &clusters.Items[0], nil
	}

	var crdbCluster crdbv1beta1.CrdbCluster
	if err := s.Ctl.Get(ctx, clusterName, &crdbCluster); err != nil {
		return nil, err
	}
	return &crdbCluster, nil
}

func (s *TenantPodScaler) countTotalPendingAdhocPods(ctx context.Context) (int, error) {
	var adhocPods corev1.PodList
	if err := s.Ctl.List(ctx, &kube.ListOptions{
		LabelSelector: manifests.MakePendingAdhocPodSelector(),
	}, &adhocPods); err != nil {
		return 0, err
	}
	return len(adhocPods.Items), nil
}

// maxUnreadyPodReplacements returns the total number of unready pods
// that can be replaced during scaling, taking the current number of
// unready pods into account. There is a cluster level maximum on the
// number of unready pods for resource protection purposes.
func (s *TenantPodScaler) maxUnreadyPodReplacements(
	ctx context.Context, cluster *crdbv1beta1.CrdbCluster,
) (int, error) {
	var allStampedPods corev1.PodList
	if err := s.Ctl.List(ctx, &kube.ListOptions{
		LabelSelector: manifests.MakeStampedPodsSelector(),
	}, &allStampedPods); err != nil {
		return 0, err
	}
	totalUnreadyStampedCount := len(getUnreadyPodsFromList(allStampedPods.Items))

	maxUnreadyStampedCount, err := manifests.MaxUnreadyStampedPods(cluster)
	if err != nil {
		return 0, err
	}
	return max(0, maxUnreadyStampedCount-totalUnreadyStampedCount), nil
}

// removeUnready returns a list of pods with unready pods removed. It
// accounts for the cluster-level replacement maximum. If there are
// more unready pods in the given list than we are allowed to replace,
// it will only remove the allowed number of unready pods. All unready
// pods beyond the maximum will remain in the returned list.
func (s *TenantPodScaler) removeUnready(
	ctx context.Context,
	tenant *crdbv1beta1.CrdbTenant,
	cluster *crdbv1beta1.CrdbCluster,
	pods []corev1.Pod,
) (running []corev1.Pod, err error) {
	ignored := 0
	replacementLimit := -1
	for _, pod := range pods {
		if !manifests.IsCockroachReady(&pod) {
			if replacementLimit == -1 {
				replacementLimit, err = s.maxUnreadyPodReplacements(ctx, cluster)
				if err != nil {
					return nil, err
				}
			}
			if ignored < replacementLimit {
				ignored += 1
				continue
			}
		}
		running = append(running, pod)
	}
	return
}

// getPendingPodsFromList returns the first (and only) PENDING pod in the list.
// If no such pod exists, nil will be returned.
func getPendingPodsFromList(pods []corev1.Pod) []*corev1.Pod {
	pendingPods := []*corev1.Pod{}
	for i := range pods {
		pod := &pods[i]
		state, _ := manifests.GetTenantPodState(pod)
		if state == manifests.TenantPodPendingState && pod.Status.Phase == corev1.PodRunning {
			pendingPods = append(pendingPods, pod)
		}
	}
	return pendingPods
}

func getUnreadyPodsFromList(pods []corev1.Pod) []*corev1.Pod {
	unreadyPods := []*corev1.Pod{}
	for i := range pods {
		pod := &pods[i]
		if !manifests.IsCockroachReady(pod) {
			unreadyPods = append(unreadyPods, pod)
		}
	}
	return unreadyPods
}

// countDrainingPods returns the number of given pods that are in the DRAINING
// state.
// NOTE: This function assumes that DRAINING pods are always grouped at the end
// of the input slice.
func countDrainingPods(pods []corev1.Pod) int {
	i := len(pods) - 1
	for i >= 0 {
		state, _ := manifests.GetTenantPodState(&pods[i])
		if state != manifests.TenantPodDrainingState {
			break
		}
		i--
	}
	return len(pods) - (i + 1)
}

// partitionPods returns slices of pods based on staleness and state.
func partitionPods(
	pods []corev1.Pod,
) (runningPods, staleRunningPods, drainingPods, staleDrainingPods []corev1.Pod) {
	for _, pod := range pods {
		state, _ := manifests.GetTenantPodState(&pod)
		stale := manifests.IsTenantPodStale(&pod)
		draining := state == manifests.TenantPodDrainingState
		switch {
		case stale && draining:
			staleDrainingPods = append(staleDrainingPods, pod)
		case !stale && draining:
			drainingPods = append(drainingPods, pod)
		case stale && !draining:
			staleRunningPods = append(staleRunningPods, pod)
		case !stale && !draining:
			runningPods = append(runningPods, pod)
		}
	}
	return
}

// isUnstampedPendingPod returns true if the given pod is unstamped, and in
// a PENDING state, or false otherwise.
func isUnstampedPendingPod(pod *corev1.Pod) bool {
	state, _ := manifests.GetTenantPodState(pod)
	if state != manifests.TenantPodPendingState {
		return false
	}
	if _, ok := pod.Labels[manifests.TenantIDLabel]; ok {
		return false
	}
	return true
}

// filterCandidatesByNodeCPUUsage returns the warm pool pods on nodes whose
// CpuAvg5m is strictly below nodeCPUUsageStampingThreshold. If fewer than
// nodeCPUUsageStampingMinFilterPercentage of the input pods remain after
// filtering, returns the full warm pool instead.
func filterCandidatesByNodeCPUUsage(
	ctx context.Context, pods []corev1.Pod, nodeMetrics metrics.NodeMetricsMap,
) []corev1.Pod {
	if len(nodeMetrics) == 0 {
		return pods
	}
	var filtered []corev1.Pod
	for _, pod := range pods {
		nodeName := pod.Spec.NodeName
		nodeMetric, ok := nodeMetrics[nodeName]
		if !ok || nodeMetric.MetricsNotReady {
			logging.Debugf(ctx, "could not find node metrics (found: %v) or metrics not ready (NotReady: %v) for node %s", ok, nodeMetric.MetricsNotReady, nodeName)
			continue
		}
		if nodeMetric.CpuAvg5m < float64(nodeCPUUsageStampingThreshold) {
			filtered = append(filtered, pod)
		}
	}
	// same as filtered/pods < nodeCPUUsageStampingMinFilterPercentage/100 but keeping operations as int
	if len(filtered)*100 < len(pods)*nodeCPUUsageStampingMinFilterPercentage || len(filtered) == 0 {
		logging.Warnf(ctx, "%d/%d pods below %d%% CPU (min %d%%), using full warm pool",
			len(filtered), len(pods), nodeCPUUsageStampingThreshold, nodeCPUUsageStampingMinFilterPercentage)
		return pods
	}
	return filtered
}

// pickUnstampedPodByNodeLoad will select a pod using weighted random selection based on the node unused CPU capacity.
// We are not using the below approach to pick the node based on a weighted CPU load as unstamped pods might not exist on all nodes.
// https://github.com/cockroachdb/cockroach/blob/62392dcd0f03a2bef1dc6d0431e505f5125297e8/pkg/ccl/sqlproxyccl/tenant/pod.go#L20-L43
func (s *TenantPodScaler) pickUnstampedPodByNodeLoad(
	pods []corev1.Pod, nodeMetrics metrics.NodeMetricsMap,
) *corev1.Pod {

	// Map to store pods by node.
	podIndexesByNode := make(map[string][]int)
	// Store node names.
	var nodeNames []string
	totalUnusedCPU := 0.0

	// Group pods by node and calculate unused CPU.
	for i, pod := range pods {
		nodeName := pod.Spec.NodeName
		if _, exists := podIndexesByNode[nodeName]; !exists {
			// Calculate unused CPU (100 - CPU usage).
			// This check makes sure we only consider nodes with metrics.
			if nodeMetric, ok := nodeMetrics[nodeName]; ok {
				totalUnusedCPU += 100.0 - nodeMetric.CpuIRate
				podIndexesByNode[nodeName] = []int{}
				nodeNames = append(nodeNames, nodeName)
			}
		}
		podIndexesByNode[nodeName] = append(podIndexesByNode[nodeName], i)
	}

	// Sort node names to ensure deterministic weighted selection
	// regardless of the order pods appear in the input slice.
	sort.Strings(nodeNames)

	// Generate a random number between 0 and 1.
	randomValue := float64(s.Rng.Int63()) / float64(1<<63)
	threshold := 0.0

	// Select a node based on unused CPU proportions.
	var selectedNodeName string
	for _, nodeName := range nodeNames {
		// nodeMetrics[nodeName] is guaranteed to exist here.
		threshold += (100 - nodeMetrics[nodeName].CpuIRate) / totalUnusedCPU
		if randomValue <= threshold {
			selectedNodeName = nodeName
			break
		}
	}

	// Select random pod from chosen node.
	podsOnNode := podIndexesByNode[selectedNodeName]
	selectedPodIndex := podsOnNode[int(s.Rng.Int63())%len(podsOnNode)]
	return &pods[selectedPodIndex]
}

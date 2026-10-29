package clusterctrl

import (
	"context"
	"strconv"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	opmetrics "github.com/cockroachlabs/cockroach-enterprise-operator/controllers/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/safesql"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

var (
	// setClusterVersionQuery updates the `version` cluster setting to the
	// node's binary version. This is used to bump the cluster setting version
	// during a major upgrade of tenant clusters, but may be used for "internal"
	// upgrades, in the case where an alpha release was chosen from the master
	// branch. For all patch upgrades, the binary version remains the same so it
	// is safe to invoke this again.
	setClusterVersionQuery = safesql.Format(
		"SET CLUSTER SETTING %1 = crdb_internal.node_executable_version()",
		safesql.Identifier("version"))

	// getClusterVersionQuery returns the node's binary version.
	getClusterVersionQuery = safesql.Format(
		"SELECT crdb_internal.node_executable_version()")

	// isClusterFinalizedQuery returns a boolean indicating whether the cluster
	// has been finalized (i.e. whether setClusterVersionQuery has been executed).
	isClusterFinalizedQuery = safesql.Format(
		"SELECT version = crdb_internal.node_executable_version() FROM [SHOW CLUSTER SETTING version]")

	// waitFinalizationRequeueAfter is the duration we wait between requeuing
	// requests when the host has not been finalized yet.
	waitFinalizationRequeueAfter = time.Minute

	// poolThrottleError indicates that the upgrade process was throttled
	// because there was insufficient pool capacity. See minPoolCapacity.
	poolThrottleError = errors.New("throttling upgrade due to pool capacity concerns")

	// poolThrottleRequeueAfter is the duration we wait between requeueing
	// when hitting poolThrottleError. 3 seconds is roughly the time it takes
	// for a tenant pool pod to be created.
	poolThrottleRequeueAfter = 3 * time.Second

	// minPoolCapacity is the minimum percentage of pool availability required
	// to perform maintenance on tenants. In particular, finalizing tenants
	// after a major upgrade.
	minPoolCapacity = 0.66
)

// Upgrade ensures that all SQL pods are running an up-to-date CRDB image. This
// does not mean that the host and tenant layers have been finalized. Once all
// stamped pods are up-to-date, we transition the cluster into the tenant
// finalization state, which polls until the host has been finalized before
// performing the operation.
//
// NOTE: Upgrade assumes that tenantpool deployments have already been updated
// to the new CRDB image.
func (h *requestHandler) Upgrade() error {
	// Did we upgrade our tenant pools?
	if !h.cluster.Status.HasAction(crdbv1beta1.Upgrade, crdbv1beta1.Starting) {
		return nil
	}

	logging.Info(h.ctx, "upgrade started")
	if err := h.markOldStampedPodsAsStale(); err != nil {
		logging.Errorf(h.ctx, "failed to mark old stamped pods as stale: %v", err)
		return err
	}

	// If Finalization is enabled, set the AwaitFinalization action.
	if !h.reconciler.DisableTenantFinalization {
		// TODO(alyshan): Only set the AwaitFinalization condition for major upgrades.
		h.cluster.Status.SetAction(crdbv1beta1.AwaitFinalization, crdbv1beta1.Starting)
		opmetrics.TenantsFinalized.Set(0)
	}

	// Mark the upgrade as Finished.
	h.cluster.Status.SetAction(crdbv1beta1.Upgrade, crdbv1beta1.Finished)
	if err := h.ctl.UpdateStatus(h.ctx, h.cluster); err != nil {
		logging.Errorf(h.ctx, "failed to apply CRDB cluster status update: %v", err)
		return err
	}
	logging.Info(h.ctx, "upgrade finished")
	return nil
}

// FinalizeTenants finalizes the tenants by setting the cluster version of
// tenants to their binary versions. This is done asynchronously via NewFinalizer.
// Finalization will not start until the host has been finalized, and the
// preserve_downgrade_option setting is not set for tenants.
// TODO(alyshan): Finalization will be removed from the operator and performed by the control plane.
func (h *requestHandler) FinalizeTenants() error {
	// If we're not awaiting finalization, no-op.
	if !h.cluster.Status.HasAction(crdbv1beta1.AwaitFinalization, crdbv1beta1.Starting) {
		return nil
	}

	// Note(alyshan): We do not expect this flag to be flipped during finalization, in fact
	// the control plane will only allow toggling of the flag in staging environments.
	// If it is flipped, just mark finalization as finished so that the CrdbCluster may
	// be Quiescent. The staging developer will need to decide how they want to handle the
	// state of the host and tenants.
	if h.reconciler.DisableTenantFinalization {
		h.cluster.Status.SetAction(crdbv1beta1.AwaitFinalization, crdbv1beta1.Finished)
		if err := h.ctl.UpdateStatus(h.ctx, h.cluster); err != nil {
			return errors.Wrap(err, "failed to apply CRDB cluster status update")
		}
		logging.Info(h.ctx, "tenant finalization disabled, marking as finished")
		// The TenantsFinalized metric is only reported on *during* finalization.
		opmetrics.TenantsFinalized.Set(0)
		return nil
	}

	finalized, err := h.isHostFinalized()
	if err != nil {
		return errors.Wrap(err, "checking finalization status")
	}

	// If the cluster is in the process of finalizing, wait for it to eventually
	// become finalized. Once it is, upgrade any tenants that may have been
	// created after the upgrade, but before finalization as they will be
	// running on the old cluster version.
	if !finalized {
		logging.Infof(h.ctx, "requeue finalization: host cluster is not yet finalized")
		h.RequeueAfter(waitFinalizationRequeueAfter)
		return nil
	}

	// If finalization is already running, check up on it later.
	f := h.reconciler.Finalizer
	if f.IsFinalizationRunning() {
		h.RequeueAfter(time.Minute)
		return nil
	}

	tenants, stale, err := h.getTenantsForFinalization()
	if err != nil {
		return errors.Wrap(err, "getting tenants for finalization")
	}

	// No tenants left to finalize, mark finalization as finished.
	if len(tenants) == 0 && !stale {
		h.cluster.Status.SetAction(crdbv1beta1.AwaitFinalization, crdbv1beta1.Finished)
		if err := h.ctl.UpdateStatus(h.ctx, h.cluster); err != nil {
			return errors.Wrap(err, "failed to apply CRDB cluster status update")
		}
		logging.Info(h.ctx, "tenant finalization finished")
		// The TenantsFinalized metric is only reported on *during* finalization.
		opmetrics.TenantsFinalized.Set(0)
		return nil
	}

	// Some tenants left to finalize, invoke the Finalizer to perform finalization on the tenants asynchronously.
	// TODO(prod-orch): Treat another reconcile winning the race to start finalization as a
	// normal requeue. The check above and this call use separate lock acquisitions.
	if err := f.FinalizeTenants(tenants, h.ctl); err != nil {
		return err
	}
	h.RequeueAfter(time.Minute)

	return nil
}

// TODO(alyshan): Finalization will be removed from the operator and performed by the control plane.
func (h *requestHandler) getTenantsForFinalization() ([]crdbv1beta1.CrdbTenant, bool, error) {
	// Before finalizing a tenant we will need to ensure no stale pods exists for
	// that tenant before finalizing. Instead of making a K8s list call each time,
	// we list the stale pods up front.
	staleTenants, err := findTenantsWithStalePods(h.ctx, h.ctl)
	if err != nil {
		return nil, false, errors.Wrap(err, "finding tenants with stale pods")
	}

	hostFinalizedVersion, err := h.getHostFinalizedVersion()
	if err != nil {
		return nil, false, errors.Wrap(err, "getting host finalized version")
	}

	tenants := crdbv1beta1.CrdbTenantList{}
	if err := h.ctl.List(h.ctx, &kube.ListOptions{}, &tenants); err != nil {
		return nil, false, errors.Wrap(err, "listing CrdbTenants")
	}
	tenantsToFinalize := []crdbv1beta1.CrdbTenant{}
	tenantsFinalized := 0
	for _, tenant := range tenants.Items {
		// Ignore tenants we've already finalized.
		if isTenantFinalized(tenant, hostFinalizedVersion) {
			tenantsFinalized += 1
			continue
		}

		// Skip tenants that have stale pods. It is important to ensure that
		// we don't attempt to finalize a tenant that has a SQL pod running an
		// older binary version.
		// TODO(alyshan): Could CRDB enforce this? i.e return an error when trying
		// to finalize a tenant that has a SQL pod with lower version binary active.
		if _, ok := staleTenants[tenant.Spec.TenantID]; ok {
			logging.Infof(h.ctx, "skipping setting cluster version on CrdbTenant %v due to staleness", tenant.Name)
			continue
		}
		tenantsToFinalize = append(tenantsToFinalize, tenant)
	}
	opmetrics.TenantsFinalized.Set(float64(tenantsFinalized))

	return tenantsToFinalize, len(staleTenants) > 0, nil
}

// markOldStampedPodsAsStale looks for stamped pods running an outdated CRDB image
// and marks them as stale. Marking a pod as stale ensures that the pod will be terminated
// and replaced with a new stamped pod. This results in all SQL pods using the same
// CRDB image as the one specified on the host CrdbCluster.
// TODO(alyshan): CC-8011 describes a general solution for identifying stale pods.
func (h *requestHandler) markOldStampedPodsAsStale() error {
	// Note(alyshan): We perform stale pod rollovers in situations where we do not
	// need to. Specifically, when an "out-of-sync" pool exists on the host cluster, and
	// the host cluster receives an upgrade, stamped pods from that pool don't need to
	// be rolled over, but they will be. CC-8011 would mitigate this.
	// Since stale pod rollovers are not disruptive to a tenant this isn't a huge concern.
	oldStampedPods, err := h.getOldStampedPods()
	if err != nil {
		return err
	}

	for _, pod := range oldStampedPods.Items {
		// All these pods should have the TenantIDLabel since we filter based
		// on that in getOldStampedPods.
		tenantId, err := crdbv1beta1.ParseTenantID(pod.Labels[manifests.TenantIDLabel])
		if err != nil {
			return errors.Wrap(err, "getting tenantID")
		}

		tenant := &crdbv1beta1.CrdbTenant{}
		err = h.ctl.Get(h.ctx, tenantId.Tag(), tenant)
		if err != nil {
			return errors.Wrap(err, "getting tenant")
		}

		if err := hasPoolCapacity(h.ctx, h.ctl, *tenant.Spec.TenantPool); err != nil {
			return err
		}

		manifests.MarkTenantPodAsStale(&pod)
		if err := h.ctl.ApplyWithOptions(h.ctx, kube.ApplyOptions{NoWait: true}, &pod); err != nil {
			return errors.Wrapf(err, "marking stamped pod %s as stale", pod.Name)
		}
		logging.Infof(h.ctx, "marked pod %v for %v as stale", pod.Name, tenant.Name)
	}

	return nil
}

func findTenantsWithStalePods(
	ctx context.Context, ctl *kube.Ctl,
) (map[crdbv1beta1.TenantID]struct{}, error) {
	tenantsWithStalePods := map[crdbv1beta1.TenantID]struct{}{}

	staleStampedPods := corev1.PodList{}
	err := ctl.List(ctx, &kube.ListOptions{LabelSelector: manifests.MakeStaleStampedPodsSelector()}, &staleStampedPods)
	if err != nil {
		return nil, errors.Wrap(err, "listing stale stamped pods")
	}

	for _, pod := range staleStampedPods.Items {
		tenantIdStr := pod.Labels[manifests.TenantIDLabel]
		tenantId, err := strconv.Atoi(tenantIdStr)
		if err != nil {
			return nil, errors.Wrapf(err, "converting TenantID %v to int", tenantIdStr)
		}
		tenantID := crdbv1beta1.TenantID(tenantId)

		tenantsWithStalePods[tenantID] = struct{}{}
	}

	return tenantsWithStalePods, nil
}

// waitForPoolCapacity waits (up to 1 minute) until the tenantPool is operating at minPoolCapacity.
// If the pool is operating at less than minPoolCapacity, then poolThrottleError will be returned.
// TODO(alyshan): This is not thread-safe. Ideally, the tenant reconciler which
// actually stamps pods would throttle stamping from the pool (see CC-8011).
// Or we find a way to run pods without consuming from the same pool that serves
// users cold starts and/or scale ups.
func waitForPoolCapacity(ctx context.Context, ctl *kube.Ctl, tenantPool string) error {
	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = time.Minute
	// Note(alyshan): Using poolThrottleRequeueAfter maintains the throttling behaviour
	// we're used to using (prior to async finalization via Finalizer).
	b.InitialInterval = poolThrottleRequeueAfter
	return backoff.Retry(func() error {
		return hasPoolCapacity(ctx, ctl, tenantPool)
	}, b)
}

// hasPoolCapacity checks if the tenantPool is operating a minimum of
// minPoolCapacity. If the pool is operating at less than minPoolCapacity, then
// poolThrottleError will be returned.
func hasPoolCapacity(ctx context.Context, ctl *kube.Ctl, tenantPool string) error {
	poolDeployment := appsv1.Deployment{}
	err := ctl.Get(ctx, manifests.MakeTenantPoolName(tenantPool), &poolDeployment)
	if err != nil {
		return errors.Wrapf(err, "getting tenant pool deployment %v", manifests.MakeTenantPoolName(tenantPool))
	}
	hasCapacity := (float64(poolDeployment.Status.ReadyReplicas) / float64(*poolDeployment.Spec.Replicas)) >= minPoolCapacity
	if !hasCapacity {
		return poolThrottleError
	}
	return nil
}

// getOldStampedPods returns all pods that are assigned to a tenant and
// using an image that does not match the Spec.CockroachImage of the CrdbCluster.
func (h *requestHandler) getOldStampedPods() (corev1.PodList, error) {
	toReturn := corev1.PodList{}

	options := kube.ListOptions{
		LabelSelector: manifests.MakeStampedPodsSelector(),
	}
	stampedPods := corev1.PodList{}
	if err := h.ctl.List(h.ctx, &options, &stampedPods); err != nil {
		return toReturn, errors.Wrap(err, "error getting stamped pods")
	}

	poolExpectedImage := map[string]string{}
	for _, pod := range stampedPods.Items {
		var poolName string
		if name, ok := pod.Labels[manifests.TenantPoolNameLabel]; ok {
			poolName = name
		} else {
			return toReturn, errors.Newf("unexpected stamped pod %v with no %v label", pod.Name, manifests.TenantPoolNameLabel)
		}

		if _, ok := poolExpectedImage[poolName]; !ok {
			for _, pool := range h.cluster.Spec.TenantHost.TenantPools {
				if pool.Name == poolName {
					expectedImage := h.cluster.Spec.Template.Spec.Image
					if pool.CockroachImage != "" {
						expectedImage = pool.CockroachImage
					}
					poolExpectedImage[poolName] = expectedImage
				}
			}
		}
		expectedImage := poolExpectedImage[poolName]

		usingCorrectImage := false
		for _, container := range pod.Spec.Containers {
			if container.Name == manifests.CockroachDB {
				if container.Image == expectedImage {
					usingCorrectImage = true
					break
				}
			}
		}
		if !usingCorrectImage {
			toReturn.Items = append(toReturn.Items, pod)
		}
	}

	return toReturn, nil
}

func (h *requestHandler) isHostFinalized() (bool, error) {
	row, err := h.queryRowSql(isClusterFinalizedQuery)
	if err != nil {
		return false, err
	}

	var finalized bool
	if err := row.Scan(&finalized); err != nil {
		return false, err
	}

	return finalized, nil
}

func (h *requestHandler) getHostFinalizedVersion() (string, error) {
	row, err := h.queryRowSql(getClusterVersionQuery)
	if err != nil {
		return "", err
	}
	var hostVersion string
	if err := row.Scan(&hostVersion); err != nil {
		return "", err
	}
	return hostVersion, nil
}

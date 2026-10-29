package tenant

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// TenantPodSyncer ensures that a tenant pod's contents are up-to-date. This is
// used by metrics and log export to ensure that short-term credentials are
// constantly refreshed before they expire.
type TenantPodSyncer interface {
	// Sync ensures that the tenant pod's contents are up-to-date. If the tenant
	// pod requires credentials, this will request for such credentials. This
	// returns nil if the pod is not eligible for a sync operation, or if the
	// operation has succeded.
	Sync(tenant *crdbv1beta1.CrdbTenant, pod *corev1.Pod) error
}

// podSyncPeriod represents the period in which the syncer will attempt to sync
// the list of active tenant pods.
var podSyncPeriod = 5 * time.Minute

// aLongTimeInTheFuture represents a duration of a year. This is used to
// indicate that a tenant pod does not need a sync (e.g. there are no short-term
// credentials, or the pod is already stale).
var aLongTimeInTheFuture = timeutil.Now().Add(365 * 24 * time.Hour)

type tenantPodSyncer struct {
	ctx                context.Context
	ctl                *kube.Ctl
	credProvider       cloud.CredentialProvider
	nodeLocalityReader cloud.NodeLocalityReaderFunc

	mu struct {
		sync.Mutex
		podRenewsAt map[types.UID]time.Time
	}
}

var _ manager.Runnable = &tenantPodSyncer{}
var _ manager.LeaderElectionRunnable = &tenantPodSyncer{}
var _ TenantPodSyncer = &tenantPodSyncer{}

// NewTenantPodSyncer returns a new instance of the tenant pod syncer, which
// will attempt to sync a tenant pod's contents if deemed out-of-date.
//
// NOTE: The ctl *must* be scoped to the cluster's regional namespace. If we
// decide to support more than one region per cluster, this has to be updated.
//
// TODO(jaylim-crl): Add metrics for tenantPodSyncer. Some possible ideas:
// - gauge of tracked pods
// - histogram of credential creation latency
// - counter for credential pushes/failures
func NewTenantPodSyncer(
	ctx context.Context,
	ctl *kube.Ctl,
	credProvider cloud.CredentialProvider,
	nodeLocalityReader cloud.NodeLocalityReaderFunc,
) *tenantPodSyncer {
	s := &tenantPodSyncer{
		ctx:                ctx,
		ctl:                ctl,
		credProvider:       credProvider,
		nodeLocalityReader: nodeLocalityReader,
	}
	s.mu.podRenewsAt = make(map[types.UID]time.Time)
	return s
}

// Start starts the syncer. It will block until context has been closed or an
// error occurs.
//
// Start implements the manager.Runnable interface.
func (s *tenantPodSyncer) Start(ctx context.Context) error {
	// Remove expired entries from the credential provider.
	s.credProvider.GC()

	// Ensure that the timer fires immediately once it starts.
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			if err := s.syncAll(ctx); err != nil {
				logging.Warnf(ctx, "failed to sync all tenant pods: %v", err)
			}
			// +/- 20%. For a sync period of 5 minutes, this is 4-6 minutes.
			timer.Reset(timeutil.Jitter(podSyncPeriod, 0.2))

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// NeedLeaderElection returns true to indicate that the syncer needs to be run
// in the leader election mode. We don't want multiple operator instances trying
// to update the SQL pods at the same time.
//
// NeedLeaderElection implements the manager.LeaderElectionRunnable interface.
func (s *tenantPodSyncer) NeedLeaderElection() bool {
	return true
}

// Sync implements the TenantPodSyncer interface.
func (s *tenantPodSyncer) Sync(tenant *crdbv1beta1.CrdbTenant, pod *corev1.Pod) error {
	// Use syncer's root context here.
	return s.syncWithContext(s.ctx, tenant, pod)
}

// syncWithContext ensures that the tenant pod's contents are up-to-date. If the
// tenant pod requires credentials, this will request for such credentials. This
// returns nil if the pod is not eligible for a sync operation, or if the
// operation has succeded.
func (s *tenantPodSyncer) syncWithContext(
	ctx context.Context, tenant *crdbv1beta1.CrdbTenant, pod *corev1.Pod,
) error {
	// Pending pods should not be synced. We will only sync RUNNING and DRAINING
	// pods. DRAINING pods should be synced since operations are still running
	// on such pods, and they have not fully terminated yet.
	podState, _ := manifests.GetTenantPodState(pod)
	if podState == manifests.TenantPodPendingState {
		return errors.AssertionFailedf("pods in the PENDING state cannot be synced")
	}

	// Pod must be stamped with the given tenant.
	tenantID := tenant.Spec.TenantID
	if label, ok := pod.Labels[manifests.TenantIDLabel]; !ok || label != tenantID.String() {
		return errors.AssertionFailedf("pod %s is not marked with tenant-id=%d", pod.Name, tenantID)
	}

	// Pod is stale, so set pod to never renew.
	if pod.Annotations[manifests.TenantPodRevisionAnnotation] != manifests.GetTenantPodHashRevision(pod, tenant) {
		s.setPodRenewal(pod.UID, aLongTimeInTheFuture)
		return nil
	}

	// Tenant does not support sync.
	if !isSyncSupported(tenant) {
		s.setPodRenewal(pod.UID, aLongTimeInTheFuture)
		return nil
	}

	// Gather all necessary credentials.
	//
	// NOTE: Expiration of credentials returned via the CredentialProvider
	// already includes offsets to renew. Such expiration windows are set within
	// the provider itself.
	minRenewsAt := aLongTimeInTheFuture
	credentials := make(map[string][]byte)
	if tenant.Spec.OtelCollector != nil {
		getAndWriteCreds := func(role_arn string) error {
			if role_arn == "" {
				return nil
			}
			cred, err := s.credProvider.GetAWSCreds(
				ctx,
				role_arn,
				tenant.Spec.OtelCollector.AWSExternalID,
			)
			if err != nil {
				return err
			}
			credentials[role_arn] = []byte(cred.CredentialsFile())
			if cred.ExpiresAt().Before(minRenewsAt) {
				minRenewsAt = cred.ExpiresAt()
			}
			return nil
		}
		if err := getAndWriteCreds(tenant.Spec.OtelCollector.AWSMetricsRoleARN); err != nil {
			return errors.Wrap(err, "writing metrics credentials")
		}
		if err := getAndWriteCreds(tenant.Spec.OtelCollector.AWSLogsRoleARN); err != nil {
			return errors.Wrap(err, "writing logs credentials")
		}

		if tenant.Spec.OtelCollector.GCPLogsServiceAccount != "" {
			c, err := s.credProvider.GetGCPCreds(ctx, tenant.Spec.OtelCollector.GCPLogsServiceAccount)
			if err != nil {
				return errors.Wrap(err, "getting GCP creds")
			}
			cred, err := json.Marshal(c.CredentialsFile())
			if err != nil {
				return err
			}
			credentials[tenant.Spec.OtelCollector.GCPLogsServiceAccount] = cred
			if c.ExpiresAt().Before(minRenewsAt) {
				minRenewsAt = c.ExpiresAt()
			}
		}
	}

	// Send a start request before setting the pod's renewal time.
	//
	// NOTE: There won't be a case where we're sending stale or more up-to-date
	// k8s resources (except certs) to the SQL pod since `sendStartRequest` only
	// reads names of resources from `tenant`, which are tied to checksums. If
	// there was a race, and `tenant` got updated, we just won't be able to find
	// the dependent k8s resources.
	if err := sendStartRequest(
		ctx, s.ctl, pod, tenant, credentials, s.nodeLocalityReader,
	); err != nil {
		// There are various failure scenarios here, so we're doing this on
		// a best-effort basis. If we fail here, the next sync loop will send
		// another update since the pod's renewal time hasn't been updated. Some
		// examples of possible failures are:
		//   1. SQL pod has been terminated.
		//   2. Dependent k8s resources have been deleted.
		//   3. Network issues.
		return errors.Wrap(err, "sending update request")
	}

	s.setPodRenewal(pod.UID, minRenewsAt)
	logging.Infof(ctx, "updated %s pod's contents for %v", pod.Name, tenant.Name)
	return nil
}

// syncAll attempts to sync all SQL pods on a best-effort basis. If the
// credentials for pods are still valid, the update to such pods will be skipped.
func (s *tenantPodSyncer) syncAll(ctx context.Context) error {
	// Take a snapshot of pod renewals before listing pods. We will use this
	// snapshot to indicate which entries to clean up.
	podRenewsAtCopy := s.podRenewalsSnapshot()

	// List all non-stale stamped pods, and collect UIDs of these pods.
	stampedPodsByTenantID, err := s.listNonStaleStampedPodsByTenantID(ctx)
	if err != nil {
		return errors.Wrap(err, "listing non-stale stamped pods")
	}
	activePods := make(map[types.UID]struct{})

	for tid, pods := range stampedPodsByTenantID {
		// Given that we fetch CrdbTenant *after* listing the pods, it is
		// guaranteed that the versions of tenant entries are always newer or
		// equal to those of running pods. It is fine to mark pods as stale or
		// won't renew if its checksum do not match the tenant's at this point.
		var tenant crdbv1beta1.CrdbTenant
		if err := s.ctl.Get(ctx, tid.Tag(), &tenant); err != nil {
			if kubeerrors.IsNotFound(err) {
				// Tenant could be deleted after the list occurs. Continue
				// processing another tenant.
				logging.Warnf(ctx, "CrdbTenant/%v not found, skipping sync", tid.Tag())
				continue
			}
			return errors.Wrap(err, "getting tenant")
		}

		// Process the pods.
		for _, pod := range pods {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			activePods[pod.UID] = struct{}{}

			// Credentials are still valid, or the pod does not need credentials.
			if !s.canPodRenew(pod.UID) {
				continue
			}

			if err := s.syncWithContext(ctx, &tenant, pod); err != nil {
				// Log and continue since failures could be partial, and we're
				// doing this on a best-effort basis. We shouldn't let one
				// temporary/expected failure halt the entire update.
				logging.Warnf(ctx, "syncWithContext: %v", err)
			}
		}
	}

	// Clean up expired credentials.
	s.credProvider.GC()

	// Remove all pods that are no longer active.
	var toRemove []types.UID
	for podUID := range podRenewsAtCopy {
		// If entry isn't in the list of active pods, the pod is likely gone, so
		// we'll remove it.
		if _, ok := activePods[podUID]; !ok {
			toRemove = append(toRemove, podUID)
		}
	}
	s.removePodRenewals(toRemove)
	return nil
}

// listNonStaleStampedPodsByTenantID returns a per-tenant list of stamped SQL
// pods that are not in the PENDING state or stale. Pods are sorted by name for
// determinism.
func (s *tenantPodSyncer) listNonStaleStampedPodsByTenantID(
	ctx context.Context,
) (map[crdbv1beta1.TenantID][]*corev1.Pod, error) {
	// List all stamped SQL pods that are not in the PENDING state or stale.
	var list corev1.PodList
	if err := s.ctl.List(ctx, &kube.ListOptions{
		// i.e. "app=tenantpool,tenant-id,!stale,state!=PENDING"
		LabelSelector: kube.ParseLabelOrDie(fmt.Sprintf(
			"app=%s,%s,!%s,%s!=%s",
			manifests.TenantPoolAppName,
			manifests.TenantIDLabel,
			manifests.TenantPodStaleLabel,
			manifests.TenantPodStateLabel,
			manifests.TenantPodPendingState,
		)),
		FieldSelector: fields.ParseSelectorOrDie("status.phase=Running"),
	}, &list); err != nil {
		return nil, errors.Wrap(err, "listing running, unstamped, PENDING pods")
	}

	// Sort by tenant ID, followed by name.
	sort.Slice(list.Items, func(i, j int) bool {
		tenantI := list.Items[i].Labels[manifests.TenantIDLabel]
		tenantJ := list.Items[j].Labels[manifests.TenantIDLabel]
		// Sort by name as well to ensure consistent results.
		if tenantI == tenantJ {
			return list.Items[i].Name < list.Items[j].Name
		}
		return tenantI < tenantJ
	})

	// Group pods by tenant ID.
	podsByTenantID := make(map[crdbv1beta1.TenantID][]*corev1.Pod)
	for _, p := range list.Items {
		// Assign p to pod so that we can take a copy of it later.
		pod := p
		tid, err := crdbv1beta1.ParseTenantID(pod.Labels[manifests.TenantIDLabel])
		if err != nil {
			return nil, errors.Wrapf(err, "parsing tenantID for pod %s", pod.Name)
		}
		podsByTenantID[tid] = append(podsByTenantID[tid], &pod)
	}
	return podsByTenantID, nil
}

// podRenewalsSnapshot returns a copy of the podRenewsAt map.
func (s *tenantPodSyncer) podRenewalsSnapshot() map[types.UID]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := make(map[types.UID]time.Time, len(s.mu.podRenewsAt))
	maps.Copy(m, s.mu.podRenewsAt)
	return m
}

// removePodRenewals ensures that the list of given pods have been removed from
// the podRenewsAt map.
func (s *tenantPodSyncer) removePodRenewals(toRemove []types.UID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range toRemove {
		delete(s.mu.podRenewsAt, k)
	}
}

// canPodRenew returns true if the pod's renewal time is due (i.e. pod needs an
// update), or false otherwise.
func (s *tenantPodSyncer) canPodRenew(podUID types.UID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	renewsAt, ok := s.mu.podRenewsAt[podUID]
	if !ok {
		// By default, the pod can be renewed right away.
		return true
	}
	return timeutil.Now().After(renewsAt)
}

// setPodRenewal sets the pod's renewal time.
func (s *tenantPodSyncer) setPodRenewal(podUID types.UID, renewsAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mu.podRenewsAt[podUID] = renewsAt
}

// isSyncSupported returns true if the tenant supports sync operations, or
// false otherwise. Concretely, this returns true if the tenant requires
// short-term credentials.
func isSyncSupported(tenant *crdbv1beta1.CrdbTenant) bool {
	if tenant.Spec.OtelCollector != nil && (tenant.Spec.OtelCollector.AWSMetricsRoleARN != "" ||
		tenant.Spec.OtelCollector.AWSLogsRoleARN != "" || tenant.Spec.OtelCollector.GCPLogsServiceAccount != "") {
		return true
	}
	return false
}

package tenantctrl

import (
	"context"
	"math/rand"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/tenant"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"
)

const (
	// TenantFinalizerName is the name of the finalizer that is added to each
	// CrdbTenant when it is created. The finalizer prevents deletion until the
	// operator has had a chance to cleanup the tenant's keyspace.
	TenantFinalizerName = "crdbtenants.crdb.cockroachlabs.com/finalizer"

	// DisableReconcile skips reconciliation of a CrdbTenant when it is present
	// as an annotation. This is used in tests.
	DisableReconcile = "disable-reconcile"

	eventSource = "crdbtenant-controller"
)

// +kubebuilder:rbac:groups=crdb.cockroachlabs.com,resources=crdbtenants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=crdb.cockroachlabs.com,resources=crdbtenants/status,verbs=get;update;patch

// Reconciler reconciles CrdbTenant objects. Its state and methods must be
// thread-safe.
type Reconciler struct {
	Ctl                *kube.Ctl
	MetricsManager     metrics.TenantMetricsReader
	NodeMetricsManager metrics.NodeMetricsReader

	// InTestMode means that the reconciler is being run in testing mode, where
	// it runs in the same process as unit tests. In this mode, copyist and/or
	// kuberecord might be active.
	// NOTE: Try not to modify the behavior of the reconciler based on this
	// flag; only use InTestMode when there's no other way of using the same
	// code paths for both testing and production.
	InTestMode bool

	// Rng is used to generate random numbers in the reconciler. This is
	// automatically set as long as SetupWithManager is called; otherwise, it
	// must be set manually.
	Rng rand.Source

	// ScaleLockTable is used to serialize the reconciler's and rpc server's
	// access to a tenant's pods.
	ScaleLockTable *tenant.LockTable

	// TenantPodSyncer is used to keep the contents of stamped tenantpool pods
	// up-to-date. This is mostly used by metrics and log export to refresh
	// short-term credentials.
	TenantPodSyncer tenant.TenantPodSyncer

	// NodeLocalityReader is a function that reads the locality attached to
	// the node with the given name.
	NodeLocalityReader cloud.NodeLocalityReaderFunc

	// emit can be used to generate K8s events via emit.Info or emit.Warn
	emit kube.EventEmitter
}

var _ controllers.Reconciler = &Reconciler{}

// Reconcile ensures that the actual state of the CrdbTenant resource specified
// by the request matches the desired state. It returns a result that tells the
// K8s controller manager whether reconciliation succeeded and whether and when
// to retry again if it did not succeed.
//
// NOTE: In general, reconciliation helper methods should avoid retrying on
// transient errors. Instead, they should just return an error and let the
// controller manager perform its own retry (with exponential backoff). Doing
// this avoids tying up a goroutine and potentially starving other tenants.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctl := r.Ctl.WithNamespace(req.Namespace)

	// Get the current state of the tenant CRD.
	var tenant crdbv1beta1.CrdbTenant
	if err := ctl.Get(ctx, req.Name, &tenant); err != nil {
		// If resource cannot be found, it's been deleted, so don't requeue.
		if kubeerrors.IsNotFound(err) {
			return controllers.NoRequeue()
		}
		logging.Errorf(ctx, "failed to retrieve CrdbTenant resource: %v", err)
		return controllers.RequeueIfError(err)
	}

	if _, ok := tenant.Annotations[DisableReconcile]; ok {
		// Skip reconciliation of this object. Used for testing.
		return controllers.NoRequeue()
	}

	// Create handler for the request.
	handler := requestHandler{reconciler: r, ctx: ctx, ctl: ctl, tenant: &tenant, emit: &r.emit}

	// Handle pending deletion. Never try to reconcile other fields in this case.
	if !tenant.DeletionTimestamp.IsZero() {
		err := handler.Delete()
		return controllers.RequeueIfError(err)
	}

	// Ensure that resources directly tied to this tenant have been created
	// and/or are up to date. Do this before initializing the tenant to ensure
	// network policies are in place before a tenant is running.
	if err := handler.Deploy(); err != nil {
		return controllers.RequeueIfError(err)
	}

	// Ensure the ownerRef points to the current CRD API version. Now that
	// v1alpha1 is removed, no new stale refs can appear, so this can move
	// back into initialization (#256).
	if err := handler.ensureOwnerRef(); err != nil {
		logging.Errorf(ctx, "failed to ensure ownerRef for tenant %s: %v", tenant.Name, err)
		return controllers.RequeueIfError(err)
	}

	// Ensure that the tenant has been fully initialized.
	if err := handler.Initialize(); err != nil {
		return controllers.RequeueIfError(err)
	}

	// Attempt to lock the tenant. Requeue if the tenant is already locked. The
	// lock is required by Scale to protect the set of pods bound to the tenant.
	release, locked := r.ScaleLockTable.TryLock(req.Name)
	if !locked {
		logging.Infof(ctx, "requeue crdbtenant reconcile: tenant %s is locked", req.Name)
		return controllers.RequeueAfter(5*time.Second, nil)
	}
	defer release()

	// Load the tenant again in case a lock has been grabbed earlier, and the
	// tenant has been updated. This ensures Spec.Pods and Status.Pods are
	// up-to-date.
	//
	// NOTE: If we ever add an ObservedGeneration field to CrdbTenant, we should
	// reconsider this approach, as it would incorrectly advance the generation
	// if we don't update the ObservedGeneration after refreshing the tenant.
	if err := ctl.Get(ctx, req.Name, &tenant); err != nil {
		// If resource cannot be found, it's been deleted, so don't requeue.
		if kubeerrors.IsNotFound(err) {
			return controllers.NoRequeue()
		}
		logging.Errorf(ctx, "failed to retrieve CrdbTenant resource: %v", err)
		return controllers.RequeueIfError(err)
	}

	// Classify any fresh pod restarts and emit structured log events. Runs before
	// Scale so we capture pod state before operator-driven changes.
	if err := handler.RecordRestarts(); err != nil {
		return controllers.RequeueIfError(err)
	}

	// Ensure that the tenant has the right number of active pods.
	if err := handler.Scale(); err != nil {
		return controllers.RequeueIfError(err)
	}

	return handler.Result(), nil
}

// SetTestMode implements controllers.Reconciler.
func (r *Reconciler) SetTestMode(inTestMode bool) {
	r.InTestMode = inTestMode
}

func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	r.emit = kube.EventEmitter{EventRecorder: mgr.GetEventRecorderFor(eventSource)}
	r.Rng = rand.NewSource(time.Now().UTC().UnixNano())
	tenant.LoadTenantEnvVarConfig(ctx)

	// Support up to 32 different tenants being reconciled simultaneously.
	// NOTE: The controller manager ensures that at most one goroutine is
	// reconciling a particular tenant at any given time.
	return ctrl.NewControllerManagedBy(mgr).
		For(&crdbv1beta1.CrdbTenant{}).
		Owns(&corev1.Pod{}).
		WithEventFilter(filterPredicates(ctx)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 32}).
		Complete(r)
}

package clusterctrl

import (
	"context"
	"sync"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/betaclusterctrl"
	opmetrics "github.com/cockroachlabs/cockroach-enterprise-operator/controllers/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	// ClusterVersion is an annotation put on CrdbTenants when the CrdbCluster
	// undergoes an upgrade. It is used to determine which tenants have already
	// been upgraded to the right version.
	ClusterVersion = "cluster-version"

	eventSource = "crdbcluster-controller"

	// Annotation for a node that indicates live migrations have been disabled on the underlying GCP VM
	CrdbNodeLiveMigrationDisabled = "internal.crdb.cockroachlabs.com/live-migration-disabled"
)

// +kubebuilder:rbac:groups=crdb.cockroachlabs.com,resources=crdbclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=crdb.cockroachlabs.com,resources=crdbclusters/status,verbs=get;update;patch

// Reconciler reconciles a CrdbCluster object
type Reconciler struct {
	Env    *cloud.Environment
	Ctl    *kube.Ctl
	Scheme *runtime.Scheme

	// InTestMode means that the reconciler is being run in testing mode, where
	// it runs in the same process as unit tests. In this mode, copyist and/or
	// kuberecord might be active.
	// NOTE: Try not to modify the behavior of the reconciler based on this
	// flag; only use InTestMode when there's no other way of using the same
	// code paths for both testing and production.
	InTestMode bool

	// DisableTenantFinalization when set to true will result in the cluster
	// controller NOT performing finalization of tenants.
	DisableTenantFinalization bool

	// emit can be used to generate K8s events via emit.Info or emit.Warn
	emit kube.EventEmitter

	// reconciledClusters is the set of clusters that have been reconciled. This
	// will be used by the "kubernetes" endpoint watcher to know which clusters
	// to trigger reconciliation for. Use a slice to represent the set to avoid
	// constructing the map whenever the Reconciler object gets created.
	mu struct {
		sync.Mutex
		reconciledClusters []types.NamespacedName
	}

	// BetaReconciler is a reconciler that reconciles CrdbCluster objects
	// into CrdbNodes instead of a statefulset.
	// It is here for the interim while we migrate the beta logic over.
	// TODO(ye): remove once all logic is moved over.
	BetaReconciler *betaclusterctrl.Reconciler

	Finalizer *Finalizer

	InstancesClient InstancesClient
}

var _ controllers.Reconciler = &Reconciler{}

// In CC, each operator reconciles one CrdbCluster, so extra workers normally
// remain idle. A self-hosted operator may watch many clusters, and multiple
// workers let them continue reconciling while one worker is blocked by a slow
// bootstrap or another long-running operation.
//
// Four is an arbitrary starting point. We can raise it or make it configurable
// if self-hosted deployments need more concurrency. This option belongs here
// because clusterctrl is registered with the manager and calls betaclusterctrl
// internally.
var clusterControllerOptions = controller.Options{MaxConcurrentReconciles: 4}

// Reconcile ensures that the actual state of the CrdbCluster resource specified
// by the request matches the desired state. It returns a result that tells the
// K8s controller manager whether reconciliation succeeded and whether and when
// to retry again if it did not succeed.
//
// NOTE: In general, reconciliation helper methods should avoid retrying on
// transient errors. Instead, they should just return an error and let the
// controller manager perform its own retry (with exponential backoff). Doing
// this avoids tying up a goroutine and potentially starving other clusters.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctl := r.Ctl.WithNamespace(req.Namespace)

	// Get the current state of the cluster CRD.
	var cluster crdbv1beta1.CrdbCluster
	if err := ctl.Get(ctx, req.Name, &cluster); err != nil {
		// If resource cannot be found, it's been deleted, so don't requeue.
		if kubeerrors.IsNotFound(err) {
			r.ensureClusterRemoved(req.Name, req.Namespace)
			return controllers.NoRequeue()
		}
		logging.Errorf(ctx, "failed to retrieve CrdbCluster resource: %v", err)
		return controllers.RequeueIfError(err)
	}

	r.ensureCluster(req.Name, req.Namespace)

	// Create handler for the request.
	handler := requestHandler{
		reconciler: r,
		ctx:        ctx,
		ctl:        ctl,
		cluster:    &cluster,
		emit:       &r.emit,
	}

	// Reconcile the CrdbCluster. This happens before any of the Serverless
	// specific logic is executed.
	res, err := r.BetaReconciler.ReconcileCluster(ctx, ctl, &cluster)
	handler.res = res
	if err != nil {
		return requeueOrThrottle(err)
	}

	// ReconcileCluster returns after its limited migration work. This wrapper must
	// also pause host resources and its final status update during the handoff.
	if cluster.Status.Migration != nil && !betaclusterctrl.MigrationHandoffComplete(&cluster) {
		return res, nil
	}

	// When Mode=Disabled the cluster is owned by another operator (v1alpha1 public
	// operator or migration controller). Skip host resource deployment, upgrades,
	// and the final UpdateStatus — that status replacement wipes .status.migration
	// because clusterctrl doesn't know about migration fields.
	if cluster.Spec.Mode != nil && *cluster.Spec.Mode == crdbv1beta1.Disabled {
		return res, nil
	}

	// Ensure host resources like the sql proxy have been created and/or are up
	// to date.
	if err := handler.DeployHostResources(); err != nil {
		return requeueOrThrottle(err)
	}

	// Note(alyshan): The operator currently does not fully support upgrades of
	// CRDB. However, for host clusters, the operator supports upgrading the
	// tenants. This assumes that the KV pods have already been upgraded
	// appropriately (regardless of finalization). If the assumption is
	// violated for major version upgrades, the logic will fail as the SQL pods
	// would not be able to connect to the KV layer lagging a version behind.
	if err := handler.Upgrade(); err != nil {
		return requeueOrThrottle(err)
	}

	// FinalizeTenants will check if we are ready to finalize tenants, and if so,
	// it kicks off finalization asynchronously so that reconciliation is not blocked.
	if err := handler.FinalizeTenants(); err != nil {
		return requeueOrThrottle(err)
	}

	// Live migrations in GCP can cause clock skew on VMs. There is no option to disable
	// this for all nodes in a GKE cluster. We've asked Google to implement this feature,
	// but in the meantime, we can use the reconciler to manually disable live migrations
	// for every node in the cluster.
	// See: https://issuetracker.google.com/issues/149972157
	if r.Env.CloudProvider == cloud.GCP {
		// TODO: Move this cluster-scoped work out of per-cluster reconciliation
		// before supporting this feature with multiple CrdbClusters per operator.
		var nodes corev1.NodeList
		if err := ctl.List(ctx, &kube.ListOptions{}, &nodes); err != nil {
			return requeueOrThrottle(err)
		}

		for _, node := range nodes.Items {
			zone, ok := node.GetLabels()[corev1.LabelTopologyZone]
			if !ok || zone == "" {
				return requeueOrThrottle(errors.Errorf("couldn't find zone for node %s", node.Name))
			}

			value, ok := node.GetAnnotations()[CrdbNodeLiveMigrationDisabled]
			currentlyDisabled := ok && value == "true"
			shouldBeDisabled := cluster.Spec.FeatureEnabled(crdbv1beta1.ClusterDisableGCPLiveMigrations)

			if !currentlyDisabled && shouldBeDisabled {
				if err := r.InstancesClient.DisableLiveMigrations(ctx, node.Name, zone); err != nil {
					return requeueOrThrottle(errors.Wrap(err, "disabling live migrations"))
				}

				// Set an annotation on the node so we can skip this node on the next reconciliation
				annotations := node.GetAnnotations()
				annotations[CrdbNodeLiveMigrationDisabled] = "true"
				node.SetAnnotations(annotations)
				if err := ctl.Update(ctx, &node); err != nil {
					return requeueOrThrottle(errors.Wrap(err, "updating node annotations"))
				}
			} else if currentlyDisabled && !shouldBeDisabled {
				if err := r.InstancesClient.EnableLiveMigrations(ctx, node.Name, zone); err != nil {
					return requeueOrThrottle(errors.Wrap(err, "enabling live migrations"))
				}

				// Remove the annotation from the node
				annotations := node.GetAnnotations()
				delete(annotations, CrdbNodeLiveMigrationDisabled)
				node.SetAnnotations(annotations)
				if err := ctl.Update(ctx, &node); err != nil {
					return requeueOrThrottle(errors.Wrap(err, "updating node annotations"))
				}
			}
		}
	}

	// ObjectMeta.Generation monotonically increases with changes to Spec.
	// Setting ObservedGeneration equal to the current Generation indicates that
	// we have successfully reconciled the latest changes to this CrdbCluster
	// resource. Thus, it is important that this is ONLY set at the END of
	// reconciliation.
	cluster.Status.ObservedGeneration = cluster.ObjectMeta.Generation
	if err := ctl.UpdateStatus(ctx, &cluster); err != nil {
		logging.Errorf(ctx, "failed to set ObservedGeneration on status for CrdbCluster %v: %v", cluster.Name, err)
		return requeueOrThrottle(err)
	}

	// If no errors have been encountered, respect any RequeueAfter requests
	// that may have been set by the handler.
	return handler.Result(), nil
}

func requeueOrThrottle(err error) (ctrl.Result, error) {
	// On a throttling error, we requeue with a specific duration. Err is nil so
	// that we are not subject to the controllers built-in rate-limiting.
	// See https://github.com/kubernetes-sigs/controller-runtime/blob/v0.14.7/pkg/internal/controller/controller.go#L324-L346
	if errors.Is(err, poolThrottleError) {
		return controllers.RequeueAfter(poolThrottleRequeueAfter, nil)
	}
	return controllers.RequeueIfError(err)
}

// SetTestMode implements controllers.Reconciler.
func (r *Reconciler) SetTestMode(inTestMode bool) {
	r.InTestMode = inTestMode
}

func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	r.emit = kube.EventEmitter{EventRecorder: mgr.GetEventRecorderFor(eventSource)}

	return ctrl.NewControllerManagedBy(mgr).
		For(&crdbv1beta1.CrdbCluster{}).
		Owns(&crdbv1beta1.CrdbNode{}).
		Owns(&corev1.Service{}).
		Watches(
			&source.Kind{Type: &corev1.Endpoints{}},
			handler.EnqueueRequestsFromMapFunc(kubernetesEndpointWatcher(r)),
		).
		Watches(
			&source.Kind{Type: &corev1.Node{}},
			handler.EnqueueRequestsFromMapFunc(k8sNodeWatcher(r)),
		).
		WithOptions(clusterControllerOptions).
		Complete(r)
}

func (r *Reconciler) ensureCluster(name, namespace string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, c := range r.mu.reconciledClusters {
		if c.Name == name && c.Namespace == namespace {
			return
		}
	}
	r.mu.reconciledClusters = append(r.mu.reconciledClusters, types.NamespacedName{
		Name:      name,
		Namespace: namespace,
	})
}

func (r *Reconciler) ensureClusterRemoved(name, namespace string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i, c := range r.mu.reconciledClusters {
		if c.Name == name && c.Namespace == namespace {
			l := len(r.mu.reconciledClusters) - 1
			r.mu.reconciledClusters[i], r.mu.reconciledClusters[l] = r.mu.reconciledClusters[l], r.mu.reconciledClusters[i]
			r.mu.reconciledClusters = r.mu.reconciledClusters[:l]
			return
		}
	}
}

func (r *Reconciler) QueueAllClusters() []reconcile.Request {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Reconcile the CrdbCluster objects that the reconciler knows.
	var requests []reconcile.Request
	for _, cluster := range r.mu.reconciledClusters {
		requests = append(requests, reconcile.Request{
			NamespacedName: cluster,
		})
	}

	return requests
}

// kubernetesEndpointWatcher converts the "kubernetes" Endpoint object into
// reconcile requests for all CrdbCluster objects within the cluster. This
// would trigger updates for network policies that reference the Kubernetes API
// server address.
func kubernetesEndpointWatcher(r *Reconciler) handler.MapFunc {
	return func(obj client.Object) []reconcile.Request {
		// The Endpoint object for the Kubernetes API server always resides as
		// "kubernetes" in the "default" namespace.
		if obj.GetNamespace() != "default" || obj.GetName() != "kubernetes" {
			return nil
		}

		ctx := context.Background()
		logging.Infof(ctx, "kubernetes endpoint watcher event: %s", obj)

		opmetrics.KubernetesEndpointWatcherCallsTotal.Inc()

		return r.QueueAllClusters()
	}
}

// k8sNodeWatcher triggers the cluster reconciler whenever the set of k8s nodes
// changes. This ensures the reconciler runs when nodes are removed from the
// cluster.
func k8sNodeWatcher(r *Reconciler) handler.MapFunc {
	return func(obj client.Object) []reconcile.Request {
		logging.Debugf(context.Background(), "k8s node watcher event: %s", obj.GetName())
		// TODO(jeffswenson): once we use labels to map clusters to a specific
		// node pool, we may be able to reconcile a subset of the clusters
		// managed by this operator.
		return r.QueueAllClusters()
	}
}

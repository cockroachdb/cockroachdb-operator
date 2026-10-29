package k8snodectrl

import (
	"context"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch

// Reconciler is a controller for Kubernetes Node objects that handles
// decommissioning logic when nodes are annotated with the decommission annotation.
// It watches for changes to the K8sNodeStartDecommissionAnnotation and marks
// all CrdbNodes on the affected k8s node to start decommissioning.
//
// This separates the concern of k8s node-level decommissioning from the
// CrdbNode controller, which can focus solely on individual CrdbNode lifecycle.
type Reconciler struct {
	Ctl *kube.Ctl
	// APIReader can be used to fetch all resources in the cluster,
	// as opposed to the Ctl, which is namespace-scoped.
	APIReader client.Reader

	// inTestMode means that the reconciler is being run in testing mode, where
	// it runs in the same process as unit tests. In this mode, copyist and/or
	// kuberecord might be active.
	//
	// NOTE: Try not to modify the behavior of the reconciler based on this
	// flag; only use inTestMode when there's no other way of using the same
	// code paths for both testing and production.
	inTestMode bool
	// Exposed for testing purposes only.
	NodeExecutor func(node *crdbv1beta1.CrdbNode, exec kubecrdbnode.Executor) kubecrdbnode.NodeExecutor
}

var _ controllers.Reconciler = &Reconciler{}

func NewReconciler(ctl *kube.Ctl) *Reconciler {
	return &Reconciler{
		Ctl:          ctl,
		NodeExecutor: kubecrdbnode.NewNodeExecutor,
	}
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Get the current state of the k8s node.
	var k8sNode corev1.Node
	if err := r.APIReader.Get(ctx, req.NamespacedName, &k8sNode); err != nil {
		// If a resource cannot be found, it's been deleted, so don't requeue.
		if kubeerrors.IsNotFound(err) {
			return controllers.NoRequeue()
		}
		logging.Errorf(ctx, "failed to retrieve k8s Node resource: %v", err)
		return controllers.RequeueIfError(err)
	}

	// Check if the k8s node has the decommission annotation.
	_, hasDecommissionAnnotation := k8sNode.Annotations[crdbv1beta1.K8sNodeStartDecommissionAnnotation]
	if !hasDecommissionAnnotation {
		// Node is not marked for decommission, nothing to do
		return controllers.NoRequeue()
	}

	// Cordon the k8s node to prevent new pods from being scheduled
	// If cordoning fails, we still proceed with decommissioning
	// Note: If operator is also running on this node, the operator has to be scaled up
	// to run a replica on a different node.
	if !k8sNode.Spec.Unschedulable {
		if err := r.Ctl.Cordon(ctx, &k8sNode); err != nil {
			logging.Warnf(ctx, "Failed to cordon k8s node %s: %v", k8sNode.Name, err)
		}
	}

	crdbNodes, err := r.findCrdbNodesOnK8sNode(ctx, k8sNode.Name)
	if err != nil {
		return controllers.RequeueIfError(err)
	}

	if len(crdbNodes) == 0 {
		return controllers.NoRequeue()
	}

	// Enforce one node decommission at a time.
	// TODO(nishanth): Once batch decommissioning is introduced, we can mark all CrdbNodes at once.

	var candidateNode *crdbv1beta1.CrdbNode

	for i := range crdbNodes {
		node := &crdbNodes[i]

		// Skip nodes that are already decommissioned
		// We do not have to wait till the CrdbNode(pod+pvc) is cleaned up.
		if node.Status.Decommission == crdbv1beta1.Decommissioned {
			continue
		}

		// If any node is currently decommissioning, wait for completion.
		if node.IsDecommissioning() || node.IsMarkedForPendingDecommission() {
			return controllers.RequeueAfter(30*time.Second, nil)
		}

		// Select the first candidate node that's not decommissioned or decommissioning
		if candidateNode == nil {
			candidateNode = node
		}
	}

	// If no candidate nodes are left to decommission, we're done
	if candidateNode == nil {
		return controllers.NoRequeue()
	}

	ctl := r.Ctl.WithNamespace(candidateNode.Namespace)

	// Start decommissioning the node
	if candidateNode.Status.HasCondition(crdbv1beta1.PodRunning, metav1.ConditionTrue) {
		// Get nodeID and start decommissioning immediately for running pods
		exec := r.NodeExecutor(candidateNode, ctl)
		nodeID, err := exec.NodeID(ctx)
		if err != nil {
			return controllers.RequeueIfError(errors.Wrapf(err, "failed to get nodeID for CrdbNode %s", candidateNode.Name))
		}
		candidateNode.Decommission(nodeID)
	} else {
		// For non-running nodes, mark for pending decommission
		// Further action on this node will be taken by the cluster reconciler.
		candidateNode.MarkForPendingDecommission()
	}

	if err = ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, candidateNode); err != nil {
		return controllers.RequeueIfError(errors.Wrapf(err, "starting decommission for CrdbNode %s", candidateNode.Name))
	}

	return controllers.RequeueAfter(10*time.Second, nil)
}

// findCrdbNodesOnK8sNode finds all CrdbNodes that are scheduled on the given k8s node
func (r *Reconciler) findCrdbNodesOnK8sNode(
	ctx context.Context, k8sNodeName string,
) ([]crdbv1beta1.CrdbNode, error) {
	// List all pods on this k8s node, cluster-wide, via APIReader + field selector

	// TODO(nishanth): Once CrdbNode.spec.nodeName is populated (CC-33688), we can directly query CrdbNodes
	// by k8s node instead of listing all pods and then fetching corresponding CrdbNodes.
	var pods corev1.PodList
	sel := fields.OneTermEqualSelector("spec.nodeName", k8sNodeName)
	if err := r.APIReader.List(ctx, &pods, &client.ListOptions{
		FieldSelector: sel,
	}); err != nil {
		return nil, errors.Wrapf(err, "failed to list pods on k8s node %s", k8sNodeName)
	}

	// For each pod, try to get a CrdbNode with the same name in that namespace
	var crdbNodes []crdbv1beta1.CrdbNode
	for i := range pods.Items {
		pod := &pods.Items[i]

		// Check if this pod is owned by a CrdbNode
		var crdbNodeOwnerName string
		for _, ref := range pod.GetOwnerReferences() {
			if ref.Kind == "CrdbNode" &&
				ref.APIVersion == "crdb.cockroachlabs.com/v1beta1" &&
				ref.Controller != nil && *ref.Controller {
				crdbNodeOwnerName = ref.Name
				break
			}
		}
		// Only make the Get call if we know this pod is owned by a CrdbNode.
		if crdbNodeOwnerName != "" {
			var crdbNode crdbv1beta1.CrdbNode
			if err := r.Ctl.WithNamespace(pod.Namespace).Get(ctx, crdbNodeOwnerName, &crdbNode); err == nil {
				crdbNodes = append(crdbNodes, crdbNode)
			} else if !kubeerrors.IsNotFound(err) {
				return nil, errors.Wrapf(err, "failed to get CrdbNode %s in namespace %s", crdbNodeOwnerName, pod.Namespace)
			}
		}
		// If we get a "not found" error, the pod is not a CrdbNode, so we skip it.
	}
	return crdbNodes, nil
}

// SetTestMode implements controllers.Reconciler.
func (r *Reconciler) SetTestMode(inTestMode bool) {
	r.inTestMode = inTestMode
}

func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	r.APIReader = mgr.GetAPIReader() // cluster-wide, no cache namespace scoping

	// Add field index for Pod.spec.nodeName to optimize field selector queries.
	// This allows efficient querying of pods by the k8s node they're scheduled on.
	if err := mgr.GetFieldIndexer().IndexField(ctx, &corev1.Pod{}, "spec.nodeName",
		func(obj client.Object) []string {
			pod := obj.(*corev1.Pod)
			if pod.Spec.NodeName == "" {
				return nil
			}
			return []string{pod.Spec.NodeName}
		}); err != nil {
		return errors.Wrap(err, "failed to setup field indexer for Pod.spec.nodeName")
	}

	// TODO(nishanth): Once CrdbNode.spec.nodeName is populated (CC-33688), add field index for CrdbNodes

	// Predicate: only react when a decommission annotation is added/removed/changed.
	decommAnnotationChanged := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			_, ok := e.Object.GetAnnotations()[crdbv1beta1.K8sNodeStartDecommissionAnnotation]
			return ok
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldA, newA := e.ObjectOld.GetAnnotations(), e.ObjectNew.GetAnnotations()
			ov, ook := oldA[crdbv1beta1.K8sNodeStartDecommissionAnnotation]
			nv, nok := newA[crdbv1beta1.K8sNodeStartDecommissionAnnotation]
			return (ook != nok) || (ook && nok && ov != nv)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}

	// We rely on periodic reconciliation (via RequeueAfter) rather than watching
	// all CrdbNode changes to avoid excessive reconciliations. The k8s node
	// reconciler will requeue itself until all nodes on the k8s node are processed.
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}, builder.WithPredicates(decommAnnotationChanged)).
		Complete(r)
}

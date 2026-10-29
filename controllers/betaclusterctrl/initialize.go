package betaclusterctrl

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// Bootstrap normally finishes within seconds. We allow five minutes so a slow
	// cluster still has time to initialize, while a hung init eventually fails and
	// leaves a useful error in the logs.
	clusterInitializationTimeout = 5 * time.Minute
	// After init fails, we wait before trying again. This keeps a broken cluster
	// from immediately taking another worker that other clusters may need.
	clusterInitializationRetryDelay     = 30 * time.Second
	initializationFailedReason          = "InitializationFailed"
	initializationTimedOutReason        = "InitializationTimedOut"
	initializationNodeUnavailableReason = "InitializationNodeUnavailable"
)

// initializationNode picks the CrdbNode that cockroach init runs against. Once a
// node lands in Status.InitializationNode we keep returning it, however unhealthy
// it gets, and only move on once the CrdbNode itself is gone.
//
//	CrdbNode state             Returned         What the caller does
//	-------------------------  ---------------  --------------------------------
//	Running and healthy        that node        runs init
//	Running but crashlooping   that node        runs init, exec fails, retries it
//	Pinned, pod not Running    the pinned node  records the stall, keeps waiting
//	Pinned, CrdbNode deleted   another Running  pins the replacement, runs init
//	Nothing Running, no pin    nil              waits for a node to come up
//
// The pin exists because CRDB expects a single node to be bootstrapped. Retrying
// on another isolated node can create a separate cluster. It also covers the case
// where the pin already bootstrapped and we lost the response, since a later init
// against that same node reports the cluster already exists.
//
// https://github.com/cockroachdb/cockroach/blob/8812064a015d2faf99d3fc7e15880f94042954b0/pkg/server/init.go#L301-L309
//
// Isolation is a real risk here. JoinService publishes only Ready nodes, and
// before bootstrap no node is Ready, so they may not see each other yet.
//
// https://github.com/cockroachdb/cockroach/issues/103594
func initializationNode(
	cluster *crdbv1beta1.CrdbCluster, nodes crdbv1beta1.CrdbNodeList,
) *crdbv1beta1.CrdbNode {
	pinnedName := cluster.Status.InitializationNode
	var selected *crdbv1beta1.CrdbNode
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if pinnedName != "" && node.Name == pinnedName {
			return node
		}
		if !node.Status.HasCondition(crdbv1beta1.PodRunning, metav1.ConditionTrue) {
			continue
		}
		// Nothing is pinned yet, so we take the first Running node by name. K8s
		// does not guarantee list order, and going by name keeps us landing on the
		// same node across reconciles.
		//
		// Keep in mind Running only tells us the pod's phase. It does not mean the
		// node is healthy, or that it is sticking around (a crashlooping pod still
		// reads as Running, and so does one that a scale down is about to remove).
		// Either way we pin it, init never succeeds, and we wait on it until its
		// CrdbNode is gone.
		if selected == nil || node.Name < selected.Name {
			selected = node
		}
	}
	return selected
}

func pinInitializationNode(
	ctx context.Context,
	ctl *kube.Ctl,
	cluster *crdbv1beta1.CrdbCluster,
	node *crdbv1beta1.CrdbNode,
) error {
	if cluster.Status.InitializationNode == node.Name {
		return nil
	}

	// ObservedGeneration may only exist in memory if this is the first status
	// write. Use the persisted status as the patch base so the required field is
	// written together with the initialization node.
	var base crdbv1beta1.CrdbCluster
	if err := ctl.Get(ctx, cluster.Name, &base); err != nil {
		return errors.Wrap(err, "getting cluster before pinning initialization node")
	}

	patchedCluster := base.DeepCopy()
	patchedCluster.Status.ObservedGeneration = cluster.Status.ObservedGeneration
	patchedCluster.Status.InitializationNode = node.Name
	if err := ctl.PatchStatus(ctx, patchedCluster, &base); err != nil {
		return errors.Wrap(err, "pinning cluster initialization node")
	}

	// Keep the status calculated earlier in this reconcile. We only need the
	// server's new resource version so the next status update does not conflict.
	cluster.ResourceVersion = patchedCluster.ResourceVersion
	cluster.Status.InitializationNode = node.Name
	return nil
}

// recordInitializationNodeUnavailable reports that the pinned node stopped
// running before it could bootstrap the cluster. We wait for that node instead
// of moving on, so this condition is the only signal an operator gets that
// initialization is no longer making progress.
func (r *Reconciler) recordInitializationNodeUnavailable(
	ctx context.Context,
	ctl *kube.Ctl,
	cluster *crdbv1beta1.CrdbCluster,
	nodeName string,
) error {
	message := fmt.Sprintf(
		"CrdbNode/%s was selected to initialize the cluster but is not Running", nodeName)
	logging.Warnf(ctx, "Initialization of CrdbCluster/%s is waiting: %s", cluster.Name, message)
	// We come back through here on every reconcile while we wait, so only
	// announce the stall on the way in.
	condition, found := cluster.Status.FindCondition(crdbv1beta1.ClusterInitialized)
	if !found || condition.Reason != initializationNodeUnavailableReason {
		r.Emit.Warnf(cluster, initializationNodeUnavailableReason, "%s", message)
	}
	cluster.Status.SetConditionWithMessage(
		crdbv1beta1.ClusterInitialized, metav1.ConditionFalse,
		initializationNodeUnavailableReason, message)
	return errors.Wrap(ctl.UpdateStatus(ctx, cluster), "persisting unavailable initialization node")
}

func (r *Reconciler) recordInitializationFailure(
	ctx context.Context,
	ctl *kube.Ctl,
	cluster *crdbv1beta1.CrdbCluster,
	nodeName string,
	initErr error,
) error {
	reason := initializationFailedReason
	message := fmt.Sprintf("cockroach init on CrdbNode/%s failed; see operator logs", nodeName)
	if errors.Is(initErr, context.DeadlineExceeded) {
		reason = initializationTimedOutReason
		message = fmt.Sprintf("cockroach init on CrdbNode/%s exceeded %s; see operator logs",
			nodeName, clusterInitializationTimeout)
		logging.Errorf(ctx, "CockroachDB cluster initialization timed out after %s for CrdbCluster/%s on CrdbNode/%s: %+v",
			clusterInitializationTimeout, cluster.Name, nodeName, initErr)
	} else {
		logging.Errorf(ctx, "Failed to initialize CrdbCluster %v on CrdbNode/%s: %+v",
			cluster.Name, nodeName, initErr)
	}
	r.Emit.Warnf(cluster, reason, "%s", message)
	cluster.Status.SetConditionWithMessage(
		crdbv1beta1.ClusterInitialized, metav1.ConditionFalse, reason, message)
	return errors.Wrap(ctl.UpdateStatus(ctx, cluster), "persisting cluster initialization failure")
}

// Initialize initializes a CRDB cluster by running `cockroach init` on the specified node.
func (r *Reconciler) Initialize(
	ctx context.Context,
	node *crdbv1beta1.CrdbNode,
	regions []crdbv1beta1.CrdbClusterRegion,
	exec kubecrdbnode.Executor,
) error {
	// When we are running Initialize on a multi-region cluster, we need
	// to determine if the region we are trying to initialize in is the
	// first region of a new cluster, or a new region of an existing
	// cluster.
	//
	// If we have gotten here and we are in the first region of a new
	// cluster, then we do want to proceed with initialization. The region
	// will attempt to reach out to all the other regions and see that no
	// other regions are available, and conclude that we should run init.
	//
	// If we have gotten here and we are in a new region of an existing
	// cluster, then we do not want to proceed with initialization. The
	// region will attempt to reach out to all the other regions and see
	// that some have OK status, and conclude that we should not run init.
	//
	// TODO: parallelize this for loop across regions
	for _, region := range regions {
		regionNamespace := region.Code
		if region.Namespace != "" {
			regionNamespace = region.Namespace
		}

		if node.Namespace == regionNamespace {
			continue
		}

		// We call cancelFunc explicitly rather than deferring because defer
		// inside a loop accumulates until the function returns.
		expiringCtx, cancelFunc := context.WithTimeout(ctx, 30*time.Second)

		if err := r.NodeExecutor(node, exec).QueryRegionStatus(expiringCtx, region); err != nil {
			cancelFunc()
			logging.Warnf(ctx, "attempted to contact another region %q (namespace %q) to determine if we are initialized and got: %v",
				region.Code, regionNamespace, err)
		} else {
			cancelFunc()
			// If there is no error, it means another region is OK, so we do
			// not need to run init again.
			logging.Infof(ctx, "successfully probed region %q; skipping init", region.Code)
			return nil
		}
	}

	// We are matching stdout with a special string. This can be brittle.
	// We might need to ask the db team for help to differentiate "already initialized" errors
	// against others.
	// Although the db client also relies on string matching right now:
	// https://github.com/cockroachdb/cockroach/blob/54bc65f212b9f30184c27a328ab27136d2143934/pkg/cli/clierrorplus/decorate_error.go#L245-L252
	initCtx, cancel := context.WithTimeout(ctx, clusterInitializationTimeout)
	defer cancel()

	stdout, stderr, err := r.NodeExecutor(node, exec).Init(initCtx)
	if strings.Contains(stdout, "cluster has already been initialized") {
		logging.Info(ctx, "cluster has already been initialized")
		return nil
	}
	if err == nil {
		return nil
	}
	// A timeout and transport error can arrive together, so we keep both. We use
	// the timeout as the primary error because it explains why initialization
	// stopped.
	if ctxErr := initCtx.Err(); ctxErr != nil {
		err = errors.CombineErrors(ctxErr, err)
	}
	return errors.Wrapf(err, "initializing cluster: stdout: %s\nstderr: %s", stdout, stderr)
}

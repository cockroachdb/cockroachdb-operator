package nodectrl

import (
	"context"
	"strings"
	"sync"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/clusterutil"
	"github.com/cockroachlabs/version"
)

// ErrNodeStillDecommissioning is an error indicating that the node is still in the
// process of decommissioning.
var ErrNodeStillDecommissioning = errors.New("node still decommissioning")

// ErrNodeStillDraining is an error indicating the node is still in the drain
// phase, that is draining connections and leases.
var ErrNodeStillDraining = errors.New("node still draining")

// minVersionForDrain is the minimum crdb version required to run `cockroach
// drain` before `cockroach decommission`
var minVersionForDrain = version.MustParse("v23.1.0")

// minVersionForUA is the minimum CrdbVersion for which CC enables UA (virtual clusters).
var minVersionForUA = version.MustParse("v24.3.0")

// ContainsVirtualClusterDrainFix checks if the given version of crdb
// contains a fix that properly drains virtual clusters.
//
// See https://github.com/cockroachdb/cockroach/pull/155063
func ContainsVirtualClusterDrainFix(v version.Version) bool {
	major := v.Major()
	switch {
	case major.Equals(version.MustParseMajorVersion("v24.3")):
		// 24.3.x must be at least 24.3.22
		return v.AtLeast(version.MustParse("v24.3.22"))
	case major.Equals(version.MustParseMajorVersion("v25.2")):
		// 25.2.x must be at least 25.2.8
		return v.AtLeast(version.MustParse("v25.2.8"))
	case major.Equals(version.MustParseMajorVersion("v25.3")):
		// 25.3.x must be at least 25.3.4
		return v.AtLeast(version.MustParse("v25.3.4"))
	default:
		return v.AtLeast(version.MustParse("v25.4.1"))
	}
}

// Decommission handles the 5-stage decommissioning process for nodes. This
// should only be called when `node.ShouldDecommission()` is true. After each
// step completes successfully, the decommissioning state is updated accordingly.
//
// Step 1: Mark the node as Draining. If necessary, this tells the operator to start
// the drain procedure by issuing "cockroach node drain".
//
// Step 2: Wait for the drain step to complete. Once this step is done, the node
// has no SQL connections or leases.
//
// Step 3: Issue the decommission command. This will start the transferring away
// of replicas from the node
//
// Step 4: Wait for replicas to be 0.
//
// Step 5: Mark the node as Decommissioned. This removes the node from cluster
// membership.
func (r *Reconciler) Decommission(ctx context.Context, node *crdbv1beta1.CrdbNode) error {
	nodeID, err := node.DecommissionNodeID()
	if err != nil {
		return err
	}

	ctl := r.Ctl.WithNamespace(node.Namespace)
	exec := r.NodeExecutor(node, ctl)

	// TODO(prod-orch): When a Basalt node enters decommissioning and its store
	// IDs do not match the configured store count, query them again before
	// draining SQL. This is the last chance to complete an empty or partial
	// readiness-time snapshot.
	crdbversion, err := node.CrdbVersion()
	if err != nil {
		return errors.WithMessagef(err, "reading crdb version on node %s", node.Name)
	}

	if crdbversion.LessThan(minVersionForDrain) {
		return r.runLegacyDecommission(ctx, node, exec, nodeID)
	}

	cluster, err := clusterutil.GetClusterForNode(ctx, ctl, node)
	if err != nil {
		// We don't want different CrdbNodes to have different values
		// for shouldDrainAfterDecommission, as that could lead to
		// strange behaviour.
		return err
	}

	// Draining after decommissioning here (when the feature is on) lets
	// many nodes decommission in parallel without one node's leases
	// blocking its peers. CC-32722 covers the full picture, including why
	// we chose this over finalizing from a healthy peer.
	//
	// The tradeoff is a footgun. Every command here still runs against
	// the target node's own pod, so if the pod gets recreated for a
	// config change right before CRDB severs the node, cluster-wide
	// status checks against the fresh pod would hang.

	// Node controller skips pod eviction for decommissioning nodes to close that trigger.
	//
	// Every exec method already tolerates the "permanently removed" marker except Drain,
	// so issueDrain checks IsFullyDecommissioned before starting a drain that would hang on a severed process.
	shouldDrainAfterDecommission := cluster.Spec.FeatureEnabled(crdbv1beta1.ClusterDrainAfterDecommission)

	switch node.Status.Decommission {
	case "":
		if shouldDrainAfterDecommission {
			// Skip initial draining if possible
			node.Status.Decommission = crdbv1beta1.Drained
		} else {
			node.Status.Decommission = crdbv1beta1.Draining
		}
	case crdbv1beta1.Draining:
		if drained := r.issueDrain(ctx, node.Name, exec, nodeID, crdbversion); !drained {
			return ErrNodeStillDraining
		}
		node.Status.Decommission = crdbv1beta1.Drained
	case crdbv1beta1.Drained:
		if err := exec.Decommission(ctx, nodeID); err != nil {
			return errors.WithStack(err)
		}

		node.Status.Decommission = crdbv1beta1.TransferringReplicas
	case crdbv1beta1.TransferringReplicas:
		if decommissioning, err := exec.IsDecommissioning(ctx, nodeID); err != nil {
			return errors.WithStack(err)
		} else if !decommissioning {
			logging.Warnf(ctx, "Expected CRDB to be decomissioning, but it is not. Retrying decommission...")

			node.Status.Decommission = crdbv1beta1.Drained
			return nil
		}

		remainingReplicas, err := exec.Replicas(ctx, nodeID)
		if err != nil {
			return errors.WithStack(err)
		}

		if remainingReplicas > 0 {
			logging.Infof(ctx, "Node still decommissioning: Node: %s, Replicas: %d", node.Name, remainingReplicas)
			return errors.WithStack(ErrNodeStillDecommissioning)
		}

		node.Status.Decommission = crdbv1beta1.ZeroReplicas
	case crdbv1beta1.ZeroReplicas:
		if shouldDrainAfterDecommission {
			if drained := r.issueDrain(ctx, node.Name, exec, nodeID, crdbversion); !drained {
				return ErrNodeStillDraining
			}
		}

		// No more replicas, safe to finalize the decommissioning process.
		if err := exec.FinalizeDecommission(ctx, nodeID); err != nil {
			return errors.WithStack(err)
		}

		node.Status.Decommission = crdbv1beta1.Decommissioned
	}

	return nil
}

// runLegacyDecommission handles the 3-stage decommissioning process for nodes. This
// should only be called when `node.ShouldDecommission()` is true. After each
// step completes successfully, the decommissioning state is updated accordingly.
//
// Step 1: Mark the node as decommissioning
// Step 2: Wait for replicas to be 0
// Step 3: Decommission the node
//
// NOTE: the legacy decommission procedure does not drain SQL connections or
// leases before decommissioning, which can result in SQL connection impact.
func (r *Reconciler) runLegacyDecommission(
	ctx context.Context, node *crdbv1beta1.CrdbNode, exec kubecrdbnode.NodeExecutor, nodeID uint,
) error {
	switch node.Status.Decommission {
	case "":
		// If we've just started the process, begin by decommissioning the node
		// and updating the state to TransferringReplicas.
		if err := exec.Decommission(ctx, nodeID); err != nil {
			return errors.WithStack(err)
		}

		node.Status.Decommission = crdbv1beta1.TransferringReplicas
	case crdbv1beta1.TransferringReplicas:
		// If decommissioning, keep checking until replicas == 0 and then mark
		// the node as ZeroReplicas.
		remainingReplicas, err := exec.ReplicasDeprecated(ctx, nodeID)
		if err != nil {
			return errors.WithStack(err)
		}

		if remainingReplicas > 0 {
			logging.Infof(ctx, "Node still decommissioning: Node: %s, Replicas: %d", node.Name, remainingReplicas)
			return errors.WithStack(ErrNodeStillDecommissioning)
		}

		node.Status.Decommission = crdbv1beta1.ZeroReplicas
	case crdbv1beta1.ZeroReplicas:
		// No more replicas, safe to finalize the decommissioning process.
		if err := exec.FinalizeDecommissionDeprecated(ctx, nodeID); err != nil {
			return errors.WithStack(err)
		}

		node.Status.Decommission = crdbv1beta1.Decommissioned
	}

	return nil
}

func (r *Reconciler) issueDrain(
	ctx context.Context,
	node string,
	exec kubecrdbnode.NodeExecutor,
	nodeID uint,
	crdbversion version.Version,
) bool {
	// Unlike other exec methods, Drain lacks tolerance for a severed
	// node. If CRDB has already finalized this node, drain against a
	// rejected process would hang in the background goroutine below.
	// Treat it as drained so the state machine can advance.
	//
	// If a drain goroutine was already started on a previous poll, its
	// entry in runningDrains stays orphaned until the operator restarts.
	// Calling Forget here is not safe because the goroutine uses
	// context.Background and closing its channel would panic on write.
	if severed, err := exec.IsFullyDecommissioned(ctx, nodeID); err == nil && severed {
		logging.Infof(ctx, "node %s is already fully decommissioned, skipping drain", node)
		return true
	}

	fn := func() (string, error) {
		return exec.Drain(context.Background(), nodeID)
	}

	res, done := r.runningDrains.Do(node, fn)
	if !done {
		return false
	}

	r.runningDrains.Forget(node)
	if res.err != nil {
		logging.Errorf(ctx, "draining node %s: %v", node, res.err)
		return false
	}

	// If the cluster does not contain the virtual cluster draining
	// fix, and stdout does not contain complete, we need to issue
	// another drain command.
	//
	// See https://github.com/cockroachdb/cockroach/pull/155063
	if crdbversion.AtLeast(minVersionForUA) &&
		!ContainsVirtualClusterDrainFix(crdbversion) &&
		!strings.Contains(res.res, "(complete)") {

		logging.Infof(ctx, "draining returned, but is not yet complete. will retry drain command")
		return false
	}

	logging.Infof(ctx, "successfully drained node %s", node)
	return true
}

type inflight[T any] struct {
	sync.Mutex
	running map[string]chan inflightRes[T]
}

type inflightRes[T any] struct {
	res T
	err error
}

// Do dispatches fn in a goroutine and returns a boolean indicating if fn has
// ran to completion and the error output from fn. The error value has no
// meaning if fn is still running. Do returns immediately after dispatching fn
// in a go-routine. The caller can continuously call Do() with the same key to
// poll until fn completion. If there is already a registered fn associated
// with key, then Do just checks for fn completion. Do never blocks. If fn has
// ran to completion, Do will consistently return the error from fn's original
// execution. To re-trigger an execution of fn, caller must call Forget().
// See: sync/singleflight for more information on general pattern.
func (f *inflight[T]) Do(key string, fn func() (T, error)) (res inflightRes[T], complete bool) {
	f.Lock()
	defer f.Unlock()

	if f.running == nil {
		f.running = make(map[string]chan inflightRes[T])
	}

	ch, ok := f.running[key]
	if !ok {
		ch := make(chan inflightRes[T], 1)
		f.running[key] = ch
		go func() {
			res, err := fn()
			ch <- inflightRes[T]{res, err}
		}()
	}

	select {
	case res := <-ch:
		// Keep value in channel to allow many reads.
		ch <- res
		return res, true
	default:
	}

	return inflightRes[T]{}, false
}

// Forget removes deregisters any function associated with key.
func (f *inflight[T]) Forget(key string) {
	f.Lock()
	defer f.Unlock()

	if f.running == nil {
		f.running = make(map[string]chan inflightRes[T])
	}

	ch, ok := f.running[key]
	if ok {
		close(ch)
		delete(f.running, key)
	}
}

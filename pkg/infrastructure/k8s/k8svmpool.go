package k8s

import (
	"context"
	"sort"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/infrastructure"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	corev1 "k8s.io/api/core/v1"
)

type k8sVMPool struct {
	ctl          *kube.Ctl
	vmPool       infrastructure.VMPoolGroup
	options      infrastructure.VMPoolGroupOptions
	desiredState map[infrastructure.AvailabilityZone]int
}

var errNodeDoesNotExist = errors.New("node does not exist")

// Unlike the default backoff in Ctl objects, this backoff is specific to the
// k8sVMPool object, and has longer intervals.
func defaultBackOff() backoff.BackOff {
	b := &backoff.ExponentialBackOff{
		InitialInterval:     time.Second,
		RandomizationFactor: backoff.DefaultRandomizationFactor,
		Multiplier:          1.2,
		MaxInterval:         10 * time.Second,
		MaxElapsedTime:      5 * time.Minute,
		Clock:               backoff.SystemClock,
	}
	b.Reset()
	return b
}

// NewK8sVMPool returns a kubernetes implementation of the VMPoolGroup interface.
// It incorporates kubernetes level concepts such as node readiness and node
// draining.
func NewK8sVMPool(
	ctl *kube.Ctl, vmPool infrastructure.VMPoolGroup, options infrastructure.VMPoolGroupOptions,
) infrastructure.VMPoolGroup {
	if options.ApplicationHealthCheck == nil {
		// Noop application health check.
		options.ApplicationHealthCheck = func(context.Context) error { return nil }
	}

	if options.ApplicationDrain == nil {
		options.ApplicationDrain = func(context.Context, string) error { return nil }
	}

	if options.BackOff == nil {
		options.BackOff = defaultBackOff
	}

	return &k8sVMPool{
		ctl:          ctl,
		vmPool:       vmPool,
		options:      options,
		desiredState: map[infrastructure.AvailabilityZone]int{},
	}
}

func (k *k8sVMPool) AvailabilityZones(
	ctx context.Context,
) ([]infrastructure.AvailabilityZone, error) {
	return k.vmPool.AvailabilityZones(ctx)
}

func (k *k8sVMPool) State(
	ctx context.Context, az infrastructure.AvailabilityZone,
) (infrastructure.State, error) {
	return k.vmPool.State(ctx, az)
}

func (k *k8sVMPool) Nodes(
	ctx context.Context, az infrastructure.AvailabilityZone,
) ([]infrastructure.VM, error) {
	// TODO(carlo): should we be using k.ctl to fetch nodes at the k8s
	// layer? To do this we must figure out how to only fetch the nodes
	// with a given pool group identifier.
	return k.vmPool.Nodes(ctx, az)
}

func (k *k8sVMPool) Resize(
	ctx context.Context, az infrastructure.AvailabilityZone, size int,
) error {
	s, err := k.State(ctx, az)
	if err != nil {
		return errors.WithMessage(err, "")
	}

	logging.Debugf(ctx, "pool group in zone %s has %d nodes, want %d", az, s.DesiredNodes,
		size)

	// Store the desired state to later
	k.desiredState[az] = size
	if s.DesiredNodes < size {
		// Upscale.
		err := k.vmPool.Resize(ctx, az, size)
		if err != nil {
			return errors.Wrap(err, "resizing VMPool")
		}
	} else if s.DesiredNodes > size {
		// Downscaling is synchronous, so no need to wait after the call
		// returns.
		if err := k.downscale(ctx, az, size); err != nil {
			return errors.WithMessage(err, "downscaling node group")
		}
	} else {
		logging.Infof(ctx, "node count up to date in %s", az)
	}

	return nil
}

func (k *k8sVMPool) downscale(
	ctx context.Context, az infrastructure.AvailabilityZone, size int,
) error {
	nodes, err := k.Nodes(ctx, az)
	if err != nil {
		return errors.WithMessagef(err, "fetching nodes in az %s", az)
	}

	// We sort on an arbitrary field to ensure consistency (i.e. in the case
	// of a failure, a retry of this method would remove try to remove the
	// same node).
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].Name > nodes[j].Name
	})

	for i := range len(nodes) - size {
		removeNode := &nodes[i]
		if err := k.RemoveNode(ctx, removeNode); err != nil {
			return errors.WithMessagef(err, "removing node %s", removeNode.Name)
		}
	}

	return nil
}

func (k *k8sVMPool) WaitUntilReady(ctx context.Context) error {
	if len(k.desiredState) == 0 {
		logging.Infof(ctx, "no pending resize operations")
		return nil
	}

	// First wait for underlying instances to be ready.
	if err := k.vmPool.WaitUntilReady(ctx); err != nil {
		return err
	}

	logging.Debugf(ctx, "ensuring all k8s nodes are ready after scaling")
	// Then wait for the nodes to be ready at the K8s layer.
	for az, size := range k.desiredState {
		err := backoff.Retry(func() error {
			return k.nodeReadinessCheck(ctx, az, size)
		}, backoff.WithContext(k.options.BackOff(), ctx))

		if err != nil {
			return err
		}
	}
	return nil
}

func checkNodes(nodes []corev1.Node, desiredSize int) bool {
	readyCount := 0
	for _, node := range nodes {
		if kube.IsNodeReady(&node) == nil {
			readyCount++
		}
	}
	return readyCount == desiredSize
}

func (k *k8sVMPool) drain(ctx context.Context, vm *infrastructure.VM) error {
	var node corev1.Node
	if err := k.ctl.Get(ctx, vm.Name, &node); err != nil {
		if kubeerrors.IsNotFound(err) {
			return nil
		}
		return errors.Wrapf(err, "getting k8s node %s", vm.Name)
	}

	if err := k.ctl.Cordon(ctx, &node); err != nil {
		return errors.Wrap(err, "cordon")
	}

	// Invoke application drain logic before draining the nodes via k8s.
	if err := k.options.ApplicationDrain(ctx, node.Name); err != nil {
		return errors.Wrap(err, "draining application")
	}

	if err := k.ctl.Drain(ctx, &node); err != nil {
		return errors.Wrap(err, "drain")
	}
	return nil
}

func (k *k8sVMPool) RemoveNode(ctx context.Context, vm *infrastructure.VM) error {
	// An application health check before a node removal minimizes application
	// disruption. If application is partially unavailable, removing a node could
	// increase unavailability.
	logging.Debugf(ctx, "ensuring workload is healthy before removing node %s", vm.Name)
	if err := k.options.ApplicationHealthCheck(ctx); err != nil {
		return errors.WithMessage(err, "doing application health check")
	}

	if err := k.drain(ctx, vm); err != nil {
		return errors.WithMessage(err, "drain")
	}

	// An application health check after draining ensures the recently evicted
	// pods have successfully been rescheduled to a new node.
	//
	// This wait is also necessary to avoid deleting the instance before K8s
	// has a chance to detach the volume. Premature VM deletion triggers K8s
	// failure detection, resulting in a 6m wait to reattach the volume.
	// https://github.com/kubernetes/kubernetes/issues/29166#issuecomment-233548821
	logging.Debugf(ctx, "ensuring workload is healthy after draining node %s", vm.Name)
	if err := k.options.ApplicationHealthCheck(ctx); err != nil {
		return errors.WithMessage(err, "doing application health check")
	}

	// Delete the node at the Kubernetes layer explicitly before we delete it from the cloud provider.
	var node corev1.Node
	node.Name = vm.Name
	if err := k.ctl.Delete(ctx, &node); err != nil {
		if !kubeerrors.IsNotFound(err) {
			return errors.Wrapf(err, "deleting k8s node %s", vm.Name)
		}
	}

	if err := k.vmPool.RemoveNode(ctx, vm); err != nil {
		return errors.WithMessage(err, "removing node")
	}

	return nil
}

func sleep(ctx context.Context, t time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(t):
	}
	return nil
}

func (k *k8sVMPool) nodeReadinessCheck(
	ctx context.Context, az infrastructure.AvailabilityZone, size int,
) error {
	// Gather the names of the nodes in this pool group for this AZ.
	nodes, err := k.Nodes(ctx, az)
	if err != nil {
		return errors.WithMessage(err, "fetching nodes")
	}

	names := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		names[n.Name] = true
	}

	var res corev1.NodeList
	if err := k.ctl.List(ctx, &kube.ListOptions{}, &res); err != nil {
		return errors.WithMessage(err, "listing nodes")
	}

	// Filter out nodes that do not belong to this VMPoolGroup.
	var k8sNodes []corev1.Node
	for _, n := range res.Items {
		if _, ok := names[n.Name]; ok {
			k8sNodes = append(k8sNodes, n)
		}
	}

	if !checkNodes(k8sNodes, size) {
		return errors.New("k8s nodes not at desired size yet")
	}

	return nil
}

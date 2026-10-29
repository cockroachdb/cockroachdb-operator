//go:generate mockgen -package mocks -destination ./mocks/mocks.go github.com/cockroachlabs/cockroach-enterprise-operator/pkg/infrastructure VMPoolGroup
package infrastructure

import (
	"context"
	"reflect"

	"github.com/cenkalti/backoff"
	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
)

type AvailabilityZone string

type State struct {
	DesiredNodes int
}

// VM represents a virtual machine in a managed VM group and Kubernetes cluster,
// such as a Nodepool or ASG.
type VM struct {
	// Name is a unique handle to this machine and its kubernetes node
	Name string
	// AvailabilityZone is the AvailabilityZone this Node is running in
	AvailabilityZone AvailabilityZone
}

// VMPoolGroupOptions specifies an optional settings for handling VMPoolGroup
// operations.
type VMPoolGroupOptions struct {
	// ApplicationHealthCheck registers an application layer health check
	// that the VMPoolGroup can query to minimize disruption to the workloads
	// running on it during updates to the VMPoolGroup state.
	ApplicationHealthCheck func(context.Context) error
	// ApplicationDrain is a function called before a node is drained that
	// drains the applications on the node before draining the node. This
	// can be used to prevent disruptions to the applications running.
	ApplicationDrain func(context.Context, string) error
	// BackOff is used to determine the appropriate amount of time to sleep
	// between polls when waiting for an VM to become ready. The default will
	// start polling once per second with a maximum polling interval of 10s.
	// This is sufficient for most cases. This option should generally be set
	// by tests that want to shorten their runtime.
	// NOTE: BackOff is a function because each invocation of Ctl needs its own
	// private instance, since backoff objects maintain state and are not safe
	// for concurrent use.
	BackOff func() backoff.BackOff
	// ForceRepave causes UpdateVMPoolGroup to update the VMs even if the
	// current pool group is up to date.
	ForceRepave bool
}

// VMPoolGroup represents a set of managed hardware groups on a cloud provider such as Nodepools or ASGs
type VMPoolGroup interface {
	// AvailabilityZones returns the availability zones that this set of hardware groups
	// can be distributed across.
	// The order of AvailabilityZone is stable.
	AvailabilityZones(context.Context) ([]AvailabilityZone, error)

	// State returns the desired state of the managed hardware group for the given availability zone
	State(context.Context, AvailabilityZone) (State, error)

	// WaitUntilReady blocks until all Resize() operations have completed. The
	// definition of healthy depends on the implementation of the VMPoolGroup -
	// cloud-provider specific VMPoolGroups will only wait for the nodes to
	// become healthy according to the cloud provider, while the k8sVMPool will
	// wait for the nodes to become healthy according to Kubernetes. Failure to
	// become healthy will not result in a rollback.
	WaitUntilReady(ctx context.Context) error

	// Resize sets the desired scale for group corresponding to AvailabilityZone without
	// waiting for the underyling nodepool to reflect the change.
	// Resize will arbitrarily remove nodes called with a number lower than DesiredNodes
	Resize(context.Context, AvailabilityZone, int) error

	// Nodes returns all active machines in this set of groups (the VMs may not
	// be ready yet).
	Nodes(context.Context, AvailabilityZone) ([]VM, error)

	// RemoveNode removes the specified Node and drops the desired scale by 1
	// It blocks on the removed node being deleted.
	// IMPORTANT: The k3d implementation may not block on the removal of the k8s nodes.
	RemoveNode(context.Context, *VM) error
}

// MigrateVMPoolGroup scales the 'dst' node pool to match 'topology' node
// distribution and downscales the 'src' node pool to zero. Kubernetes
// handles moving the workloads (along with the persistent volumes) as the
// old node pools are drained. The VMPoolGroups must span the same
// availability zones. 'topology' is a separate argument because there are
// no guarantees the topology generated from 'src' is the desired topology
// (it can deviate in the case of retries or the src being in an unexpected
// distribution).
func MigrateVMPoolGroup(
	ctx context.Context, src VMPoolGroup, dst VMPoolGroup, topology map[AvailabilityZone]int,
) error {
	// Ensure the VM groups span the same availability zones.
	srcAZs, err := src.AvailabilityZones(ctx)
	if err != nil {
		return errors.WithMessage(err, "fetching availability zones")
	}

	dstAZs, err := dst.AvailabilityZones(ctx)
	if err != nil {
		return errors.WithMessage(err, "fetching availability zones")
	}

	if !reflect.DeepEqual(srcAZs, dstAZs) {
		return errors.Newf("availability zone mismatch between src and dst node pools: %v, %v", srcAZs, dstAZs)
	}

	// Ensure the set of AZs in topology is a subset of dstAZs.
	set := make(map[AvailabilityZone]struct{}, len(dstAZs))
	for _, az := range dstAZs {
		set[az] = struct{}{}
	}

	for az := range topology {
		if _, ok := set[az]; !ok {
			return errors.Newf("new node group does not span zone %s", az)
		}
	}

	// Resize the node groups to desired sizes, letting the orchestrator
	// (e.g. kubernetes) do the workload migration, ensuring no downtime.
	for _, az := range dstAZs {
		desiredSize := topology[az]

		dstState, err := dst.State(ctx, az)
		if err != nil {
			return errors.WithMessage(err, "fetching desired size of new node group")
		}

		// It is possible the dst node pool is overscaled, so we must trim off extra
		// nodes before migrating. A concrete example is: in GCP node pools are
		// created with one node per AZ, even for single-node clusters (scaling
		// happens outside pulumi). MigrateVMPoolGroup can be called with newly
		// created node pools, so it must ensure these are not overscaled.
		if dstState.DesiredNodes > desiredSize {
			logging.Infof(ctx, "removing extra nodes in %s", az)
			dstState, err = resize(ctx, dst, az, desiredSize)
			if err != nil {
				return errors.WithMessagef(err, "removing extra nodes in %s", az)
			}
		}

		srcState, err := src.State(ctx, az)
		if err != nil {
			return errors.WithMessage(err, "fetching desired size of old node group")
		}

		dstNodeGroupNotReady, srcNodeGroupNotReady := true, true

		for dstNodeGroupNotReady || srcNodeGroupNotReady {
			dstNodeGroupNotReady = dstState.DesiredNodes < desiredSize
			if dstNodeGroupNotReady {
				logging.Infof(ctx, "adding a node in %s", az)
				dstState, err = resize(ctx, dst, az, dstState.DesiredNodes+1)
				if err != nil {
					return errors.WithMessagef(err, "adding a node in %s", az)
				}
			}

			srcNodeGroupNotReady = srcState.DesiredNodes > 0
			if srcNodeGroupNotReady {
				logging.Infof(ctx, "removing a node in %s", az)
				srcState, err = resize(ctx, src, az, srcState.DesiredNodes-1)
				if err != nil {
					return errors.WithMessagef(err, "removing a node in %s", az)
				}
			}
		}
	}
	return nil
}

func resize(ctx context.Context, vpg VMPoolGroup, az AvailabilityZone, size int) (State, error) {
	if err := vpg.Resize(ctx, az, size); err != nil {
		return State{}, errors.WithMessage(err, "scaling node group")
	}

	if err := vpg.WaitUntilReady(ctx); err != nil {
		return State{}, err
	}

	state, err := vpg.State(ctx, az)
	if err != nil {
		return State{}, errors.WithMessage(err, "fetching desired size of node group")
	}

	return state, nil
}

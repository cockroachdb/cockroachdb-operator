package fake

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/infrastructure"
)

// VMPoolGroupReactor allows the caller of NewVMPoolGroup to inject custom
// logic when adding or removing nodes.
type VMPoolGroupReactor interface {
	AddNode(context.Context, *infrastructure.VM) error
	RemoveNode(context.Context, *infrastructure.VM) error
}

type vmPoolGroup struct {
	reactor VMPoolGroupReactor
	zones   map[infrastructure.AvailabilityZone][]infrastructure.VM
}

type VMPoolGroupOptions struct {
	// Reactor allows the caller of NewVMPoolGroup to inject custom logic when
	// adding or removing nodes.
	Reactor VMPoolGroupReactor
}

// NewVMPoolGroup returns a fake implementation of the VMPoolGroup interface.
// It simulates a cloud provider that adds and removes from a pool.
// fake.VMPoolGroup is used in concert with the kuberecord package, in order to
// simulate a cloud provider and Kubernetes cluster.
func NewVMPoolGroup(
	zones []infrastructure.AvailabilityZone, options VMPoolGroupOptions,
) infrastructure.VMPoolGroup {
	zoneMap := make(map[infrastructure.AvailabilityZone][]infrastructure.VM)
	for _, z := range zones {
		zoneMap[z] = []infrastructure.VM{}
	}

	if options.Reactor == nil {
		options.Reactor = &noopReactor{}
	}

	return &vmPoolGroup{
		zones:   zoneMap,
		reactor: options.Reactor,
	}
}

func (g *vmPoolGroup) WaitUntilReady(ctx context.Context) error {
	return nil
}

// AvailabilityZones implements infrastructure.VMPoolGroup.AvailabilityZones.
func (g *vmPoolGroup) AvailabilityZones(
	context.Context,
) ([]infrastructure.AvailabilityZone, error) {
	zones := make([]infrastructure.AvailabilityZone, 0, len(g.zones))
	for z := range g.zones {
		zones = append(zones, z)
	}

	// AvailabilityZones() method promises stability.
	sort.Slice(zones, func(i, j int) bool {
		return string(zones[i]) < string(zones[j])
	})

	return zones, nil
}

// State implements infrastructure.VMPoolGroup.State.
func (g *vmPoolGroup) State(
	ctx context.Context, zone infrastructure.AvailabilityZone,
) (infrastructure.State, error) {
	vms, ok := g.zones[zone]
	if !ok {
		return infrastructure.State{}, fmt.Errorf("zone not found: %s", zone)
	}

	return infrastructure.State{DesiredNodes: len(vms)}, nil
}

// Resize implements infrastructure.VMPoolGroup.Resize.
func (g *vmPoolGroup) Resize(
	ctx context.Context, zone infrastructure.AvailabilityZone, desiredNodes int,
) error {
	vms, ok := g.zones[zone]
	if !ok {
		return fmt.Errorf("zone not found: %s", zone)
	}

	if desiredNodes < len(vms) {
		for _, vm := range vms[desiredNodes:] {
			if err := g.reactor.RemoveNode(ctx, &vm); err != nil {
				return err
			}
		}
		g.zones[zone] = vms[:desiredNodes]
	} else {
		for i := len(vms); i < desiredNodes; i++ {
			vm := infrastructure.VM{
				Name:             fmt.Sprintf("%s-node-%d", zone, i),
				AvailabilityZone: zone,
			}
			if err := g.reactor.AddNode(ctx, &vm); err != nil {
				return err
			}
			vms = append(vms, vm)
		}
		g.zones[zone] = vms
	}

	return nil
}

// Nodes implements infrastructure.VMPoolGroup.Nodes.
func (g *vmPoolGroup) Nodes(
	ctx context.Context, zone infrastructure.AvailabilityZone,
) ([]infrastructure.VM, error) {
	vms, ok := g.zones[zone]
	if !ok {
		return nil, fmt.Errorf("zone not found: %s", zone)
	}

	cp := make([]infrastructure.VM, len(vms))
	_ = copy(cp, vms)

	return cp, nil
}

// RemoveNode implements infrastructure.VMPoolGroup.RemoveNode.
func (g *vmPoolGroup) RemoveNode(ctx context.Context, vm *infrastructure.VM) error {
	vms, ok := g.zones[vm.AvailabilityZone]
	if !ok {
		return fmt.Errorf("zone not found: %s", vm.AvailabilityZone)
	}

	for i := range vms {
		if vms[i].Name == vm.Name {
			vms := slices.Delete(vms, i, i+1)
			g.zones[vm.AvailabilityZone] = vms
			return nil
		}
	}

	if err := g.reactor.RemoveNode(ctx, vm); err != nil {
		return err
	}

	return fmt.Errorf("VM not found: %s", vm.Name)
}

type noopReactor struct{}

func (r *noopReactor) AddNode(context.Context, *infrastructure.VM) error {
	return nil
}

func (r *noopReactor) RemoveNode(context.Context, *infrastructure.VM) error {
	return nil
}

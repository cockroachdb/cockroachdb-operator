package clusterctrl

import (
	"context"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/compute/metadata"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
)

type InstancesClient interface {
	DisableLiveMigrations(ctx context.Context, instance, zone string) error
	EnableLiveMigrations(ctx context.Context, instance, zone string) error
}

type GCPInstancesClient struct {
	client *compute.InstancesClient
}

func NewGCPInstancesClient(ctx context.Context) (*GCPInstancesClient, error) {
	client, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return nil, err
	}
	return &GCPInstancesClient{client: client}, nil
}

func (c *GCPInstancesClient) DisableLiveMigrations(
	ctx context.Context, instance, zone string,
) error {
	return c.setScheduling(ctx, instance, zone, computepb.Scheduling_TERMINATE)
}

func (c *GCPInstancesClient) EnableLiveMigrations(
	ctx context.Context, instance, zone string,
) error {
	return c.setScheduling(ctx, instance, zone, computepb.Scheduling_MIGRATE)
}

func (c *GCPInstancesClient) setScheduling(
	ctx context.Context,
	instance, zone string,
	onHostMaintenance computepb.Scheduling_OnHostMaintenance,
) error {
	project, err := c.getProject()
	if err != nil {
		return err
	}

	logging.Infof(ctx, "Setting live migrations to %s for instance %s in project %s and zone %s",
		onHostMaintenance.String(), instance, project, zone,
	)

	req := &computepb.SetSchedulingInstanceRequest{
		Project:  project,
		Zone:     zone,
		Instance: instance,
		SchedulingResource: &computepb.Scheduling{
			OnHostMaintenance: new(onHostMaintenance.String()),
		},
	}
	op, err := c.client.SetScheduling(ctx, req)
	if err != nil {
		return err
	}
	// TODO(prod-orch): Give this wait its own timeout so a stalled GCE operation cannot
	// occupy a cluster worker indefinitely.
	return op.Wait(ctx)
}

func (c *GCPInstancesClient) getProject() (string, error) {
	project, err := metadata.ProjectID()
	if err != nil {
		return "", err
	}
	return project, nil
}

type NoopInstancesClient struct{}

func (c *NoopInstancesClient) DisableLiveMigrations(
	ctx context.Context, instance, zone string,
) error {
	return nil
}

func (c *NoopInstancesClient) EnableLiveMigrations(
	ctx context.Context, instance, zone string,
) error {
	return nil
}

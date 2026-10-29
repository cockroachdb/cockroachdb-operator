package opserver

import (
	"context"
	"math/rand"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/autoscaler"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/operatorpb"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/tenant"
	"github.com/gogo/status"
	"google.golang.org/grpc/codes"
)

// ScaleTenant cold starts a tenant. See operator.proto for detailed
// documentation.
func (s *server) ScaleTenant(
	ctx context.Context, req *operatorpb.ScaleTenantRequest,
) (*operatorpb.ScaleTenantResponse, error) {
	if req.Tenant == nil || req.Tenant.Name == "" {
		return nil, errors.New("tenant is a required parameter")
	}
	if req.Tenant.Namespace == "" {
		return nil, errors.New("namespace is a required parameter")
	}

	logging.Infof(ctx, "scaling %s/%s", req.Tenant.Namespace, req.Tenant.Name)

	unlock, err := s.ScaleLockTable.Lock(ctx, req.Tenant.Name)
	if err != nil {
		return nil, errors.Wrap(err, "unable to lock tenant")
	}
	defer unlock()

	ctl := s.Ctl.WithNamespace(req.Tenant.Namespace)
	if err := kube.RetryConflicts(func() error {
		crdbTenant := &crdbv1beta1.CrdbTenant{}
		if err := ctl.Get(ctx, req.Tenant.Name, crdbTenant); err != nil {
			if kubeerrors.IsNotFound(err) {
				return status.Error(codes.NotFound, err.Error())
			}
			return err
		}
		if err := autoscaler.SetMinimumScale(crdbTenant, 1); err != nil {
			return status.Error(codes.FailedPrecondition, err.Error())
		}
		if err := ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, crdbTenant); err != nil {
			return err
		}
		scaler := tenant.TenantPodScaler{
			Ctl:                ctl,
			MetricManager:      s.MetricManager,
			NodeMetricManager:  s.NodeMetricManager,
			Rng:                rand.NewSource(time.Now().Unix()),
			Emit:               &s.EventEmitter,
			PodSyncer:          s.TenantPodSyncer,
			NodeLocalityReader: s.NodeLocalityReader,
		}
		// TODO(mstobo): we might be able to use the requeueDuration
		// returned here to hint at caller retry durations
		_, _, err := scaler.ScaleTenantWithMetrics(ctx, crdbTenant)
		return err
	}); err != nil {
		return nil, err
	}
	return &operatorpb.ScaleTenantResponse{}, nil
}

// TODO(jeffswenson): Scaling a tenant requires many serial interactions with
// Kubernetes and the pod. Exprimentally each interaction with k8s averages out
// to ~20 ms. The overall flow looks like this:
//
// 1. Get The Tenant [R]
// 2. Set Status.Pods = 1 [W]
// 3. List Tenant's Pods [R]
// 4. Stamp the Pod [W]
// 5. Fetch Tenant Certs [R]
// 6. Fetch Cluster CA [R]
// 7. Start Tenant [HTTPS]
// 8. Set Pod Ready [W]
// 9. Update Spec.Pods [W]
//
// All actions up to "Start Tenant" slow down the overall cold start process.
// Actions after Start Tenant are discounted by the fact it takes the tenant
// server time to get ready to serve sql.
//
// Many of these actions do not depend on each other and could be parallized. A
// maximally parallel cold start would look like this:
//
// 1. Get The Tenant [R], List Tenant Pods [R], Fetch Tenant Certs [R], Fetch Cluster CA [R]
// 2. Stamp the Pod [W]
// 3. Start Tenant [HTTPS]
// 4. Set Pod Ready [W], Update Spec.Pods [W]
// 5. Update Status.Pods
//
// The parallel implementation reduces the number of blocking k8s operations
// from 7 to 2. This would save on the order of ~100 ms.
//
// The fastest possible implementation would uses watches, preloading, and
// ownership semantics to avoid k8s operations in the critical path. Each
// operator instance would initialize it's own pool of tenant pods. The unique
// pool allows the operator to send client certificates to a pod without
// stamping the pod in Kubernetes.
//
// 0. Mark a pod in memory as bound to a tenant
// 1. Start Tenant [HTTPS]
// 2. Set Pod Ready [W], Update Spec.Pods [W]
// 3. Update Spec.Pods [W]

package opserver

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cenkalti/backoff"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/autoscaler"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/crypto"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/operatorpb"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/tenant"
	"github.com/cockroachlabs/cockroach-enterprise-operator/tenant/tenantmock"
	"github.com/cockroachlabs/cockroach-enterprise-operator/testmanifest"
	"github.com/gogo/status"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
)

func NewFakeTestServer(
	t *testing.T, ctl *kube.Ctl, tenantPodSyncer tenant.TenantPodSyncer,
) *server {
	options := Options{
		Ctl:               ctl.WithNamespace(""),
		ScaleLockTable:    tenant.NewLockTable(),
		MetricManager:     &metrics.TestTenantManager{},
		NodeMetricManager: &metrics.TestNodeManager{},
		EventEmitter:      kube.EventEmitter{},
		TenantPodSyncer:   tenantPodSyncer,
		NodeLocalityReader: func(ctx context.Context, nodeName string) (cloud.LocalityInfo, error) {
			return cloud.LocalityInfo{Region: "test-region", Zone: "test-zone"}, nil
		},
	}
	return &server{options}
}

func TestScaleTenant(t *testing.T) {
	ctx := context.Background()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	namespace := "test-namespace"
	// The operator retrieves a default configuration for the fluentbit sidecar when stamping
	// tenant pool pods.
	defaultFluentbitTenantConfig := manifests.GetTestTenantNoopConfigMap()
	defaultFluentbitTenantConfig.Namespace = namespace
	ctl := kubetest.NewFakeCtl(kubetest.Options{Options: kube.Options{BackOff: func() backoff.BackOff { return backoff.WithMaxRetries(backoff.NewConstantBackOff(time.Second), 30) }}}, defaultFluentbitTenantConfig).
		WithNamespace(namespace)

	tenantPodSyncer := tenantmock.NewMockTenantPodSyncer(ctrl)
	tenantPodSyncer.EXPECT().Sync(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	server := NewFakeTestServer(t, ctl, tenantPodSyncer)
	factory := testmanifest.NewFactory(namespace)

	fakeSigner, err := crypto.FakeSigner()
	require.NoError(t, err)

	applyCluster := func(t *testing.T, cluster *crdbv1beta1.CrdbCluster) *crdbv1beta1.CrdbCluster {
		require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))
		return cluster
	}

	applyTenant := func(t *testing.T, tenant *crdbv1beta1.CrdbTenant) *corev1.Secret {
		// NB: alternateClusterDomain is not currently used in K3d tests but is
		// used in all other code paths. Rather than build in gating to respect
		// the empty string, we use a dummy value.
		config := manifests.TenantCertConfig(tenant.Spec.TenantID, tenant.ObjectMeta.Namespace, "not.used")
		secret, err := config.ToK8SSecret(ctx, fakeSigner)
		require.NoError(t, err)

		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, tenant, secret))

		return secret
	}

	sqlStarterSecret, err := manifests.SqlStarterCertConfig().ToK8SSecret(ctx, fakeSigner)
	require.NoError(t, err)
	require.NoError(t, ctl.Apply(ctx, sqlStarterSecret))
	for range 20 {
		require.NoError(t, ctl.Apply(ctx, factory.CreateUnstampedPod()))
	}

	// Need to hook SendSqlStarterRequest since new pods need to be stamped.
	defer testutils.HookGlobal(
		&tenant.SendSqlStarterRequest,
		func(client *http.Client, req *http.Request, pod *corev1.Pod) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		},
	)()

	expectPods := func(t *testing.T, tenant *crdbv1beta1.CrdbTenant, num int) {
		options := kube.ListOptions{
			LabelSelector: manifests.MakeStampedPodSelector(tenant.Spec.TenantID),
			FieldSelector: fields.ParseSelectorOrDie("status.phase=Running"),
		}
		var stampedPods corev1.PodList
		require.NoError(t, ctl.List(ctx, &options, &stampedPods))
		require.Len(t, stampedPods.Items, num)
	}

	expectStatus := func(t *testing.T, tenant *crdbv1beta1.CrdbTenant, num int) {
		var currentState crdbv1beta1.CrdbTenant
		require.NoError(t, ctl.Get(ctx, tenant.Name, &currentState))
		require.Equal(t, currentState.Spec.Pods, int32(num))
		require.Equal(t, currentState.Status.Pods, int32(num))
	}

	t.Run("TenantNotFound", func(t *testing.T) {
		_, err := server.ScaleTenant(ctx, &operatorpb.ScaleTenantRequest{
			Tenant: &operatorpb.ResourceID{
				Name:      "tenant-1337",
				Namespace: "foobar",
			},
		})
		require.Error(t, err)
		require.Equal(t, status.Code(err), codes.NotFound)
	})

	t.Run("No Namespace", func(t *testing.T) {
		cluster := applyCluster(t, factory.CreateCluster())
		tenant := factory.CreateTenant(cluster)
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, tenant))
		_, err := server.ScaleTenant(ctx, &operatorpb.ScaleTenantRequest{
			Tenant: &operatorpb.ResourceID{
				Name: tenant.GetName(),
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "namespace is a required parameter")
	})

	t.Run("Missing Tenant ID", func(t *testing.T) {
		_, err := server.ScaleTenant(ctx, &operatorpb.ScaleTenantRequest{
			Tenant: &operatorpb.ResourceID{
				Namespace: "foobar",
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "tenant is a required parameter")
	})

	t.Run("Scale Tenant From Zero", func(t *testing.T) {
		cluster := applyCluster(t, factory.CreateCluster())
		tenant := factory.CreateTenant(cluster)
		applyTenant(t, tenant)

		// Run twice to test for idempotency
		for range 2 {
			_, err := server.ScaleTenant(ctx, &operatorpb.ScaleTenantRequest{
				Tenant: &operatorpb.ResourceID{
					Namespace: namespace,
					Name:      tenant.ObjectMeta.GetName(),
				},
			})
			require.NoError(t, err)

			expectPods(t, tenant, 1)
			expectStatus(t, tenant, 1)
		}
	})

	t.Run("Acquires lock on tenant", func(t *testing.T) {
		cluster := applyCluster(t, factory.CreateCluster())
		tenant := factory.CreateTenant(cluster)
		applyTenant(t, tenant)

		unlock, err := server.ScaleLockTable.Lock(ctx, tenant.Name)
		require.NoError(t, err)
		defer unlock()

		// RPC will time out failing to get the lock
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err = server.ScaleTenant(ctx, &operatorpb.ScaleTenantRequest{
			Tenant: &operatorpb.ResourceID{
				Namespace: namespace,
				Name:      tenant.ObjectMeta.GetName(),
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "unable to lock tenant")

		expectPods(t, tenant, 0)
	})

	t.Run("Tenant Already Has Pods", func(t *testing.T) {
		cluster := applyCluster(t, factory.CreateCluster())
		tenant := factory.CreateTenant(cluster)
		applyTenant(t, tenant)

		_, err := server.ScaleTenant(ctx, &operatorpb.ScaleTenantRequest{
			Tenant: &operatorpb.ResourceID{
				Namespace: namespace,
				Name:      tenant.ObjectMeta.GetName(),
			},
		})
		require.NoError(t, err)

		expectPods(t, tenant, 1)
		expectStatus(t, tenant, 1)
	})

	t.Run("Tenant Has Missing Pods", func(t *testing.T) {
		cluster := applyCluster(t, factory.CreateCluster())
		tenant := factory.CreateTenant(cluster)
		tenant.Spec.Pods = 5
		applyTenant(t, tenant)

		tenant.Status.Pods = 5
		require.NoError(t, ctl.UpdateStatus(ctx, tenant))

		// Sometimes the tenant spec/status is lying. In this case, we should
		// still end up with pods bound for the tenant.
		expectPods(t, tenant, 0)
		expectStatus(t, tenant, 5)

		_, err := server.ScaleTenant(ctx, &operatorpb.ScaleTenantRequest{
			Tenant: &operatorpb.ResourceID{
				Namespace: namespace,
				Name:      tenant.ObjectMeta.GetName(),
			},
		})
		require.NoError(t, err)

		expectPods(t, tenant, 5)
		expectStatus(t, tenant, 5)
	})

	t.Run("Tenant is Disabled", func(t *testing.T) {
		cluster := applyCluster(t, factory.CreateCluster())
		tenant := factory.CreateTenant(cluster)
		tenant.Spec.MaxPods = new(int32(0))
		tenant.Annotations = map[string]string{
			autoscaler.ZeroMaxPodsReason: "For testing reasons.",
		}
		applyTenant(t, tenant)

		_, err := server.ScaleTenant(ctx, &operatorpb.ScaleTenantRequest{
			Tenant: &operatorpb.ResourceID{
				Namespace: namespace,
				Name:      tenant.ObjectMeta.GetName(),
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "For testing reasons.")
		require.Equal(t, status.Code(err), codes.FailedPrecondition)
	})
}

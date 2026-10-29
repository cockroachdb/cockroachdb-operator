package manager_test

import (
	"context"
	"strings"
	"testing"
	"time"

	crdbv1alpha1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1alpha1"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manager"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/testenv"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

func TestIntegrationStart(t *testing.T) {
	// Use the NoLeakTest option to work around the K8s manager goroutine leak:
	// https://github.com/kubernetes-sigs/controller-runtime/issues/1280
	ctx := context.Background()
	env := testenv.New(t, testenv.SingleZoneConfig, testenv.SkipLeakTest)
	restCfg := env.DefaultCluster().Config

	// Run controller manager metrics on a less common port to avoid conflicts
	// (e.g. with CRDB webui port).
	defer testutils.HookGlobal(&metrics.DefaultBindAddress, "127.0.0.1:12080")()

	// Check default manager configuration
	// Global mode - no namespace scoping.
	mgr := manager.New(restCfg, "", nil)

	// When running in test mode, only have it listen on 127.0.0.1.
	// This avoids firewall prompts on some systems.
	mgr.Options.Host = "127.0.0.1"

	mgr.Environment = cloud.Environment{CloudProvider: cloud.K3D, CloudRegion: "default"}
	mgr.InTestMode = true

	// Verify that the scheme includes both v1alpha1 and v1beta1 for conversion webhook support
	require.NotNil(t, mgr.Options.Scheme)
	require.True(t, mgr.Options.Scheme.IsVersionRegistered(crdbv1beta1.GroupVersion))
	require.True(t, mgr.Options.Scheme.IsVersionRegistered(crdbv1alpha1.SchemeGroupVersion))
	for _, obj := range []runtime.Object{&crdbv1beta1.CrdbCluster{}, &crdbv1beta1.CrdbNode{}, &crdbv1beta1.CrdbTenant{}} {
		_, _, err := mgr.Options.Scheme.ObjectKinds(obj)
		require.NoError(t, err, "scheme must have %T registered", obj)
	}
	require.Equal(t, manifests.WebhookServicePort, mgr.Options.Port)
	require.Equal(t, manifests.OperatorCertsDir, mgr.Options.CertDir)

	// Run the manager (InTestMode means it will return after initialization).
	require.NoError(t, mgr.Run(ctx))

	webhookServer := mgr.ControllerManager().GetWebhookServer()
	require.Equal(t, manifests.WebhookServicePort, webhookServer.Port)
	require.Equal(t, manifests.OperatorCertsDir, webhookServer.CertDir)

	// Ensure that webhook and CRD manifests were created.
	ctl := env.DefaultCluster().NewCtl("")

	var webhook admissionv1.ValidatingWebhookConfiguration
	require.NoError(t, ctl.Get(ctx, "cockroach-webhook-config", &webhook))

	var crdbCluster apiextv1.CustomResourceDefinition
	require.NoError(t, ctl.Get(ctx, "crdbclusters.crdb.cockroachlabs.com", &crdbCluster))

	var crdbTenant apiextv1.CustomResourceDefinition
	require.NoError(t, ctl.Get(ctx, "crdbtenants.crdb.cockroachlabs.com", &crdbTenant))
}

func TestIntegrationOperatorLeaderFailover(t *testing.T) {
	// Test that operator leader election works by killing the current operator leader,
	// verifying that a new leader has taken over, and a crdb tenant is able to be created.
	testutils.MarkIntegrationTest(t)

	env := testenv.New(t, testenv.MultiZoneConfig)
	ctl := env.DefaultCluster().Ctl
	ctx := context.Background()

	// Increase number of operator replicas
	var operatorDeployment appsv1.Deployment
	require.NoError(t, ctl.Get(ctx, manifests.OperatorName, &operatorDeployment))
	var numReplicas int32 = 3
	operatorDeployment.Spec.Replicas = &numReplicas
	require.NoError(t, ctl.Apply(ctx, &operatorDeployment))
	t.Cleanup(func() {
		numReplicas = 1
		operatorDeployment.Spec.Replicas = &numReplicas
		_ = ctl.Apply(ctx, &operatorDeployment)
	})

	// Kill leader pod.
	operatorPod := &corev1.Pod{}
	curLeaderPodName := getCurrentLeaderName(t, ctl, ctx)
	require.NotEmpty(t, curLeaderPodName)
	require.NoError(t, ctl.Get(ctx, curLeaderPodName, operatorPod))
	require.NoError(t, ctl.Delete(ctx, operatorPod))
	// NOTE: Transition period is ~10 seconds, so we'll add 2 seconds of buffer.
	require.Eventually(t, func() bool {
		// Confirm that leader has changed.
		newLeaderPodName := getCurrentLeaderName(t, ctl, ctx)
		require.NotEmpty(t, newLeaderPodName)
		return newLeaderPodName != curLeaderPodName
	}, 12*time.Second, time.Second)

	// Ensure that we can create a tenant and it can start up.
	crdbCluster := testenv.CreateCrdbCluster(
		t,
		env,
		"test-cluster",
		&crdbv1beta1.CrdbClusterSpec{
			TLSEnabled: true,
			TenantHost: &crdbv1beta1.TenantHost{
				TenantPools: []crdbv1beta1.TenantPool{
					{Name: "default"},
					{Name: "faster"},
				},
			},
		})
	clusterCtl := env.DefaultCluster().Ctl.WithNamespace(crdbCluster.Namespace)

	const tenantID = crdbv1beta1.TenantID(10)
	tenant := &crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name: tenantID.Tag(),
		},
		Spec: crdbv1beta1.CrdbTenantSpec{TenantID: tenantID},
	}
	require.NoError(t, clusterCtl.Apply(ctx, tenant))
	t.Cleanup(func() {
		_ = clusterCtl.Delete(ctx, tenant)
	})

	require.Equal(t, "default", *tenant.Spec.TenantPool)
}

func getCurrentLeaderName(t *testing.T, ctl *kube.Ctl, ctx context.Context) string {
	// Retrieves the current leader's name.
	leaderLease := &v1.Lease{}
	require.NoError(t, ctl.Get(ctx, manager.LeaderElectionId, leaderLease))
	require.NotNil(t, leaderLease.Spec.HolderIdentity)
	return strings.Split(*leaderLease.Spec.HolderIdentity, "_")[0]
}

func TestIntegrationNamespaceScoping(t *testing.T) {
	// Test that namespace scoping correctly configures the manager's cache.
	// This test verifies the cache is configured properly for each scoping mode.
	testutils.MarkIntegrationTest(t)

	ctx := context.Background()
	env := testenv.New(t, testenv.SingleZoneConfig, testenv.SkipLeakTest)
	restCfg := env.DefaultCluster().Config

	defer testutils.HookGlobal(&metrics.DefaultBindAddress, "127.0.0.1:12081")()

	t.Run("SingleNamespaceScopedMode", func(t *testing.T) {
		// Create manager watching only "scoped-ns" namespace.
		// Use "default" as operator deployment namespace where testenv deploys the operator.
		mgr := manager.New(restCfg, "default", []string{"scoped-ns"})
		mgr.Options.Host = "127.0.0.1"
		mgr.Environment = cloud.Environment{CloudProvider: cloud.K3D, CloudRegion: "default"}
		mgr.InTestMode = true

		// Verify cache configuration for single-namespace mode.
		// Single namespace mode uses the built-in Options.Namespace field.
		require.Equal(t, "scoped-ns", mgr.Options.Namespace, "should watch only specified namespace")
		require.Nil(t, mgr.Options.NewCache, "single-namespace mode uses Namespace field, not NewCache")

		require.NoError(t, mgr.Run(ctx))
	})

	t.Run("MultiNamespaceScopedMode", func(t *testing.T) {
		// Create manager watching multiple namespaces.
		// Use "default" as operator deployment namespace where testenv deploys the operator.
		mgr := manager.New(restCfg, "default", []string{"prod-a", "prod-b", "staging"})
		mgr.Options.Host = "127.0.0.1"
		mgr.Environment = cloud.Environment{CloudProvider: cloud.K3D, CloudRegion: "default"}
		mgr.InTestMode = true

		// Verify cache configuration for multi-namespace mode.
		// Multi-namespace mode uses MultiNamespacedCacheBuilder via Options.NewCache.
		require.Empty(t, mgr.Options.Namespace, "multi-namespace mode should not set Namespace")
		require.NotNil(t, mgr.Options.NewCache, "multi-namespace mode should use MultiNamespacedCacheBuilder")

		require.NoError(t, mgr.Run(ctx))
	})

	t.Run("GlobalMode", func(t *testing.T) {
		// Create manager in global mode (no namespace scoping).
		// Use "default" as operator deployment namespace where testenv deploys the operator.
		mgr := manager.New(restCfg, "default", nil)
		mgr.Options.Host = "127.0.0.1"
		mgr.Environment = cloud.Environment{CloudProvider: cloud.K3D, CloudRegion: "default"}
		mgr.InTestMode = true

		// Verify cache configuration for global mode.
		// Global mode watches all namespaces - neither Options.Namespace nor Options.NewCache is set.
		require.Empty(t, mgr.Options.Namespace, "global mode should not set Namespace")
		require.Nil(t, mgr.Options.NewCache, "global mode should not set NewCache")

		require.NoError(t, mgr.Run(ctx))
	})
}

func TestIntegrationWebhookScoping(t *testing.T) {
	// Test that webhook configurations are created correctly based on operator mode.
	// Verifies webhook naming and namespace selectors for global vs scoped modes.
	testutils.MarkIntegrationTest(t)

	ctx := context.Background()
	env := testenv.New(t, testenv.SingleZoneConfig, testenv.SkipLeakTest)
	restCfg := env.DefaultCluster().Config
	ctl := env.DefaultCluster().NewCtl("")

	defer testutils.HookGlobal(&metrics.DefaultBindAddress, "127.0.0.1:12082")()

	t.Run("GlobalModeWebhookNaming", func(t *testing.T) {
		// Global mode should use fixed webhook names for backward compatibility.
		// Use "default" as operator deployment namespace where testenv deploys the operator.
		mgr := manager.New(restCfg, "default", nil)
		mgr.Options.Host = "127.0.0.1"
		mgr.Environment = cloud.Environment{CloudProvider: cloud.K3D, CloudRegion: "default"}
		mgr.InTestMode = true

		require.NoError(t, mgr.Run(ctx))

		// Verify global webhook config names
		var validatingWebhook admissionv1.ValidatingWebhookConfiguration
		require.NoError(t, ctl.Get(ctx, "cockroach-webhook-config", &validatingWebhook),
			"Global mode should create webhook with fixed name")

		var mutatingWebhook admissionv1.MutatingWebhookConfiguration
		require.NoError(t, ctl.Get(ctx, "cockroach-mutating-webhook-config", &mutatingWebhook),
			"Global mode should create mutating webhook with fixed name")

		// Global mode sets no namespace selector. A nil selector and an empty
		// one are equivalent in K8s, both match every namespace, so the API
		// server may hand back either form. Accept both rather than require nil.
		if validatingWebhook.Webhooks[0].NamespaceSelector != nil {
			require.Empty(t, validatingWebhook.Webhooks[0].NamespaceSelector.MatchExpressions,
				"Global mode should not have namespace selector match expressions")
			require.Empty(t, validatingWebhook.Webhooks[0].NamespaceSelector.MatchLabels,
				"Global mode should not have namespace selector match labels")
		}
		if mutatingWebhook.Webhooks[0].NamespaceSelector != nil {
			require.Empty(t, mutatingWebhook.Webhooks[0].NamespaceSelector.MatchExpressions,
				"Global mode should not have namespace selector match expressions on mutating webhook")
			require.Empty(t, mutatingWebhook.Webhooks[0].NamespaceSelector.MatchLabels,
				"Global mode should not have namespace selector match labels on mutating webhook")
		}

		// Verify service selector
		var webhookService corev1.Service
		require.NoError(t, ctl.Get(ctx, "cockroach-webhook-service", &webhookService))
		require.Equal(t, "cockroach-operator", webhookService.Spec.Selector["app"],
			"Webhook service should use default selector")

		// Cleanup
		_ = ctl.Delete(ctx, &validatingWebhook)
		_ = ctl.Delete(ctx, &mutatingWebhook)
		_ = ctl.Delete(ctx, &webhookService)
	})

	t.Run("ScopedModeWebhookNaming", func(t *testing.T) {
		// Scoped mode should use namespace-qualified webhook names.
		// Use "default" as operator deployment namespace where testenv deploys the operator.
		mgr := manager.New(restCfg, "default", []string{"prod-a", "prod-b"})
		mgr.Options.Host = "127.0.0.1"
		mgr.Environment = cloud.Environment{CloudProvider: cloud.K3D, CloudRegion: "default"}
		mgr.InTestMode = true

		require.NoError(t, mgr.Run(ctx))

		// Verify scoped webhook config names include namespace
		var validatingWebhook admissionv1.ValidatingWebhookConfiguration
		require.NoError(t, ctl.Get(ctx, "cockroach-webhook-config-default", &validatingWebhook),
			"Scoped mode should create webhook with namespace-qualified name")

		var mutatingWebhook admissionv1.MutatingWebhookConfiguration
		require.NoError(t, ctl.Get(ctx, "cockroach-mutating-webhook-config-default", &mutatingWebhook),
			"Scoped mode should create mutating webhook with namespace-qualified name")

		// Verify namespace selectors in scoped mode
		require.NotNil(t, validatingWebhook.Webhooks[0].NamespaceSelector,
			"Scoped mode should have namespace selector")
		require.Len(t, validatingWebhook.Webhooks[0].NamespaceSelector.MatchExpressions, 1)
		require.Equal(t, "kubernetes.io/metadata.name",
			validatingWebhook.Webhooks[0].NamespaceSelector.MatchExpressions[0].Key)
		require.ElementsMatch(t, []string{"prod-a", "prod-b"},
			validatingWebhook.Webhooks[0].NamespaceSelector.MatchExpressions[0].Values,
			"Namespace selector should match watched namespaces")

		require.NotNil(t, mutatingWebhook.Webhooks[0].NamespaceSelector,
			"Scoped mode should have namespace selector on mutating webhook")
		require.ElementsMatch(t, []string{"prod-a", "prod-b"},
			mutatingWebhook.Webhooks[0].NamespaceSelector.MatchExpressions[0].Values)

		// Cleanup
		_ = ctl.Delete(ctx, &validatingWebhook)
		_ = ctl.Delete(ctx, &mutatingWebhook)
	})
}

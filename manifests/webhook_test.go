package manifests

import (
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestGetWebhookConfigName(t *testing.T) {
	t.Run("GlobalModeValidating", func(t *testing.T) {
		name := GetWebhookConfigName(true, "operator-ns", false)
		require.Equal(t, "cockroach-webhook-config", name)
	})

	t.Run("GlobalModeMutating", func(t *testing.T) {
		name := GetWebhookConfigName(true, "operator-ns", true)
		require.Equal(t, "cockroach-mutating-webhook-config", name)
	})

	t.Run("ScopedModeValidating", func(t *testing.T) {
		name := GetWebhookConfigName(false, "prod-operator", false)
		require.Equal(t, "cockroach-webhook-config-prod-operator", name)
	})

	t.Run("ScopedModeMutating", func(t *testing.T) {
		name := GetWebhookConfigName(false, "prod-operator", true)
		require.Equal(t, "cockroach-mutating-webhook-config-prod-operator", name)
	})
}

func TestWebhook_GlobalMode(t *testing.T) {
	opts := WebhookOptions{
		CABundle:        []byte("test-ca-bundle"),
		Namespace:       "operator-ns",
		WatchNamespaces: nil, // Global mode
	}

	objects := Webhook(opts)
	require.Len(t, objects, 3, "Expected 3 objects: MutatingWebhookConfiguration, ValidatingWebhookConfiguration, Service")

	// Check MutatingWebhookConfiguration
	mutatingCfg, ok := objects[0].(*v1.MutatingWebhookConfiguration)
	require.True(t, ok, "First object should be MutatingWebhookConfiguration")
	require.Equal(t, "cockroach-mutating-webhook-config", mutatingCfg.Name, "Global mode should use fixed name")
	require.Nil(t, mutatingCfg.Webhooks[0].NamespaceSelector, "Global mode should have no namespace selector")
	require.Equal(t, opts.CABundle, mutatingCfg.Webhooks[0].ClientConfig.CABundle)
	require.Equal(t, "operator-ns", mutatingCfg.Webhooks[0].ClientConfig.Service.Namespace)

	// Check ValidatingWebhookConfiguration
	validatingCfg, ok := objects[1].(*v1.ValidatingWebhookConfiguration)
	require.True(t, ok, "Second object should be ValidatingWebhookConfiguration")
	require.Equal(t, "cockroach-webhook-config", validatingCfg.Name, "Global mode should use fixed name")
	require.Nil(t, validatingCfg.Webhooks[0].NamespaceSelector, "Global mode should have no namespace selector")
	require.Equal(t, opts.CABundle, validatingCfg.Webhooks[0].ClientConfig.CABundle)
	require.Equal(t, "operator-ns", validatingCfg.Webhooks[0].ClientConfig.Service.Namespace)

	// Check Service
	svc, ok := objects[2].(*corev1.Service)
	require.True(t, ok, "Third object should be Service")
	require.Equal(t, "cockroach-webhook-service", svc.Name)
	require.Equal(t, "operator-ns", svc.Namespace)
	require.Equal(t, "cockroach-operator", svc.Spec.Selector["app"], "Service selector should use new label")
	require.Equal(t, "cockroach-operator", svc.Labels["app"], "Service labels should use new label")
}

func TestWebhook_SingleNamespaceMode(t *testing.T) {
	opts := WebhookOptions{
		CABundle:        []byte("test-ca-bundle"),
		Namespace:       "operator-ns",
		WatchNamespaces: []string{"prod-a"},
	}

	objects := Webhook(opts)
	require.Len(t, objects, 3)

	// Check MutatingWebhookConfiguration
	mutatingCfg := objects[0].(*v1.MutatingWebhookConfiguration)
	require.Equal(t, "cockroach-mutating-webhook-config-operator-ns", mutatingCfg.Name,
		"Scoped mode should include namespace in name")
	require.NotNil(t, mutatingCfg.Webhooks[0].NamespaceSelector, "Scoped mode should have namespace selector")
	require.Len(t, mutatingCfg.Webhooks[0].NamespaceSelector.MatchExpressions, 1)
	require.Equal(t, "kubernetes.io/metadata.name", mutatingCfg.Webhooks[0].NamespaceSelector.MatchExpressions[0].Key)
	require.Equal(t, []string{"prod-a"}, mutatingCfg.Webhooks[0].NamespaceSelector.MatchExpressions[0].Values)

	// Check ValidatingWebhookConfiguration
	validatingCfg := objects[1].(*v1.ValidatingWebhookConfiguration)
	require.Equal(t, "cockroach-webhook-config-operator-ns", validatingCfg.Name,
		"Scoped mode should include namespace in name")
	require.NotNil(t, validatingCfg.Webhooks[0].NamespaceSelector, "Scoped mode should have namespace selector")
	require.Equal(t, []string{"prod-a"}, validatingCfg.Webhooks[0].NamespaceSelector.MatchExpressions[0].Values)
}

func TestWebhook_MultiNamespaceMode(t *testing.T) {
	opts := WebhookOptions{
		CABundle:        []byte("test-ca-bundle"),
		Namespace:       "operator-ns",
		WatchNamespaces: []string{"prod-a", "prod-b", "staging"},
	}

	objects := Webhook(opts)

	mutatingCfg := objects[0].(*v1.MutatingWebhookConfiguration)
	require.Equal(t, "cockroach-mutating-webhook-config-operator-ns", mutatingCfg.Name)
	require.NotNil(t, mutatingCfg.Webhooks[0].NamespaceSelector)
	require.Equal(t, []string{"prod-a", "prod-b", "staging"},
		mutatingCfg.Webhooks[0].NamespaceSelector.MatchExpressions[0].Values,
		"Namespace selector should include all watched namespaces")

	validatingCfg := objects[1].(*v1.ValidatingWebhookConfiguration)
	require.Equal(t, "cockroach-webhook-config-operator-ns", validatingCfg.Name)
	require.Equal(t, []string{"prod-a", "prod-b", "staging"},
		validatingCfg.Webhooks[0].NamespaceSelector.MatchExpressions[0].Values)
}

func TestWebhook_MatchPolicyExact(t *testing.T) {
	opts := WebhookOptions{
		CABundle:  []byte("test-ca-bundle"),
		Namespace: "operator-ns",
	}

	objects := Webhook(opts)

	// matchPolicy: Exact is set in the YAML so enterprise webhooks only fire
	// for v1beta1 requests. This prevents them from intercepting v1alpha1
	// requests when the public operator is present.
	mutatingCfg := objects[0].(*v1.MutatingWebhookConfiguration)
	require.NotNil(t, mutatingCfg.Webhooks[0].MatchPolicy)
	require.Equal(t, v1.Exact, *mutatingCfg.Webhooks[0].MatchPolicy)

	validatingCfg := objects[1].(*v1.ValidatingWebhookConfiguration)
	require.NotNil(t, validatingCfg.Webhooks[0].MatchPolicy)
	require.Equal(t, v1.Exact, *validatingCfg.Webhooks[0].MatchPolicy)
}

func TestWebhook_FailurePolicy(t *testing.T) {
	failurePolicy := v1.Ignore

	opts := WebhookOptions{
		CABundle:        []byte("test-ca-bundle"),
		Namespace:       "operator-ns",
		WatchNamespaces: nil,
		FailurePolicy:   failurePolicy,
	}

	objects := Webhook(opts)

	mutatingCfg := objects[0].(*v1.MutatingWebhookConfiguration)
	require.Equal(t, &failurePolicy, mutatingCfg.Webhooks[0].FailurePolicy)

	validatingCfg := objects[1].(*v1.ValidatingWebhookConfiguration)
	require.Equal(t, &failurePolicy, validatingCfg.Webhooks[0].FailurePolicy)
}

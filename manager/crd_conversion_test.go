package manager

import (
	"context"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// makeCRD creates a minimal CrdbCluster CRD with v1alpha1 and v1beta1 versions,
// both served by default.
func makeCRD() *apiextv1.CustomResourceDefinition {
	return &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "crdbclusters.crdb.cockroachlabs.com"},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Versions: []apiextv1.CustomResourceDefinitionVersion{
				{Name: "v1beta1", Served: true, Storage: true},
				{Name: "v1alpha1", Served: true},
			},
		},
	}
}

// TestConfigureCRDConversion_MigrationEnabledNoV1Alpha1 verifies that when
// migration is enabled but no v1alpha1 objects exist in storage, the conversion
// webhook is not configured and v1alpha1 is marked as not served.
func TestConfigureCRDConversion_MigrationEnabledNoV1Alpha1(t *testing.T) {
	ctx := context.Background()
	crd := makeCRD()
	existing := makeCRD()

	err := configureCRDConversion(ctx, crd, existing, true, false, []byte("ca-data"), "operator-ns")
	require.NoError(t, err)

	assert.Nil(t, crd.Spec.Conversion, "no conversion config should be set when no v1alpha1 objects exist in storage")

	for _, v := range crd.Spec.Versions {
		if v.Name == "v1alpha1" {
			assert.False(t, v.Served, "v1alpha1 should not be served when no objects exist in storage")
		}
	}
}

// TestConfigureCRDConversion_MigrationEnabledWithV1Alpha1 verifies that enabling
// migration with v1alpha1 objects in storage configures a webhook conversion
// strategy pointing at the operator's webhook service.
func TestConfigureCRDConversion_MigrationEnabledWithV1Alpha1(t *testing.T) {
	ctx := context.Background()
	crd := makeCRD()
	existing := makeCRD()

	err := configureCRDConversion(ctx, crd, existing, true, true, []byte("ca-data"), "operator-ns")
	require.NoError(t, err)

	require.NotNil(t, crd.Spec.Conversion)
	assert.Equal(t, apiextv1.WebhookConverter, crd.Spec.Conversion.Strategy)
	require.NotNil(t, crd.Spec.Conversion.Webhook)
	require.NotNil(t, crd.Spec.Conversion.Webhook.ClientConfig)
	require.NotNil(t, crd.Spec.Conversion.Webhook.ClientConfig.Service)
	assert.Equal(t, manifests.WebhookServiceName, crd.Spec.Conversion.Webhook.ClientConfig.Service.Name)
	assert.Equal(t, "operator-ns", crd.Spec.Conversion.Webhook.ClientConfig.Service.Namespace)
	assert.Equal(t, []byte("ca-data"), crd.Spec.Conversion.Webhook.ClientConfig.CABundle)
}

// TestConfigureCRDConversion_DisabledNoV1Alpha1 verifies that when migration is
// disabled and no v1alpha1 objects are in storage, the v1alpha1 version is marked
// as not served to give stale clients a clean error.
func TestConfigureCRDConversion_DisabledNoV1Alpha1(t *testing.T) {
	ctx := context.Background()
	crd := makeCRD()
	existing := makeCRD()

	err := configureCRDConversion(ctx, crd, existing, false, false, nil, "")
	require.NoError(t, err)

	assert.Nil(t, crd.Spec.Conversion, "no conversion config should be set")

	// v1alpha1 should be marked as not served.
	for _, v := range crd.Spec.Versions {
		if v.Name == "v1alpha1" {
			assert.False(t, v.Served, "v1alpha1 should not be served when migration is disabled and no objects exist")
		}
	}
}

// TestConfigureCRDConversion_DisabledWithV1Alpha1_ExistingWebhook verifies that
// when migration is disabled but v1alpha1 objects remain in storage and another
// operator manages the conversion webhook, the existing webhook config is preserved.
func TestConfigureCRDConversion_DisabledWithV1Alpha1_ExistingWebhook(t *testing.T) {
	ctx := context.Background()
	crd := makeCRD()

	existingWebhook := &apiextv1.CustomResourceConversion{
		Strategy: apiextv1.WebhookConverter,
		Webhook: &apiextv1.WebhookConversion{
			ClientConfig: &apiextv1.WebhookClientConfig{
				CABundle: []byte("other-operator-ca"),
			},
			ConversionReviewVersions: []string{"v1"},
		},
	}
	existing := makeCRD()
	existing.Spec.Conversion = existingWebhook

	err := configureCRDConversion(ctx, crd, existing, false, true, nil, "")
	require.NoError(t, err)

	require.NotNil(t, crd.Spec.Conversion)
	assert.Equal(t, apiextv1.WebhookConverter, crd.Spec.Conversion.Strategy)
	assert.Equal(t, []byte("other-operator-ca"), crd.Spec.Conversion.Webhook.ClientConfig.CABundle,
		"should preserve the existing operator's webhook config")
}

// TestConfigureCRDConversion_DisabledWithV1Alpha1_NoWebhook verifies that the
// operator refuses to start when v1alpha1 objects exist in storage but no
// conversion webhook is configured, since applying the CRD without a converter
// would silently corrupt v1alpha1 objects.
func TestConfigureCRDConversion_DisabledWithV1Alpha1_NoWebhook(t *testing.T) {
	ctx := context.Background()
	crd := makeCRD()
	existing := makeCRD()

	err := configureCRDConversion(ctx, crd, existing, false, true, nil, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "v1alpha1 objects in storage")
	assert.Contains(t, err.Error(), "--enable-migration")
}

// TestConfigureCRDConversion_MigrationEnabled_OverridesExisting verifies that
// enabling migration replaces any existing conversion config with the current
// operator's webhook configuration.
func TestConfigureCRDConversion_MigrationEnabled_OverridesExisting(t *testing.T) {
	ctx := context.Background()
	crd := makeCRD()
	// Pre-existing webhook from another operator instance.
	crd.Spec.Conversion = &apiextv1.CustomResourceConversion{
		Strategy: apiextv1.WebhookConverter,
		Webhook: &apiextv1.WebhookConversion{
			ClientConfig: &apiextv1.WebhookClientConfig{
				CABundle: []byte("old-ca"),
			},
		},
	}
	existing := makeCRD()

	err := configureCRDConversion(ctx, crd, existing, true, true, []byte("new-ca"), "new-ns")
	require.NoError(t, err)

	assert.Equal(t, []byte("new-ca"), crd.Spec.Conversion.Webhook.ClientConfig.CABundle,
		"migration enabled should replace existing webhook config")
	assert.Equal(t, "new-ns", crd.Spec.Conversion.Webhook.ClientConfig.Service.Namespace)
}

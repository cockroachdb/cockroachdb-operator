package manifests

import (
	"fmt"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests/internal"
	v1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// WebhookServicePort is the port on which the webhook service listens. This
	// must match the port in webhook.yaml.
	WebhookServicePort = 9443

	// WebhookServiceName is the name of the webhook Kubernetes Service. This
	// must match the name in webhook.yaml.
	WebhookServiceName = "cockroach-webhook-service"

	// ValidatingWebhookConfigNameGlobal is the base name for the
	// ValidatingWebhookConfiguration. In scoped mode GetWebhookConfigName
	// appends the operator namespace to make it unique per deployment.
	ValidatingWebhookConfigNameGlobal = "cockroach-webhook-config"

	// MutatingWebhookConfigNameGlobal is the base name for the
	// MutatingWebhookConfiguration. See ValidatingWebhookConfigNameGlobal.
	MutatingWebhookConfigNameGlobal = "cockroach-mutating-webhook-config"
)

// GetWebhookConfigName returns the webhook configuration name based on operator mode.
// Global mode uses fixed names for backward compatibility. Scoped mode appends the
// operator deployment namespace to avoid conflicts when running multiple operators.
func GetWebhookConfigName(isGlobal bool, namespace string, isMutating bool) string {
	if isGlobal {
		if isMutating {
			return MutatingWebhookConfigNameGlobal
		}
		return ValidatingWebhookConfigNameGlobal
	}
	if isMutating {
		return fmt.Sprintf("%s-%s", MutatingWebhookConfigNameGlobal, namespace)
	}
	return fmt.Sprintf("%s-%s", ValidatingWebhookConfigNameGlobal, namespace)
}

// WebhookOptions are parameters for the Webhook manifest.
type WebhookOptions struct {
	// CABundle is a PEM encoded CA bundle (can be multiple CA certificates) which
	// will be used by K8s to validate the operator's webhook server certificate.
	CABundle []byte

	// Namespace is the kubernetes namespace to deploy to (operator deployment namespace).
	Namespace string

	// WatchNamespaces are the namespaces the operator watches. Empty means global
	// mode (all namespaces). Non-empty means scoped mode with namespace selectors.
	WatchNamespaces []string

	// AppLabel is the "app" label value from the operator Deployment's pod template
	// (Spec.Template.Labels). When provided, updates the webhook service selector
	// to match. Detected at runtime; defaults to OperatorAppLabel if not provided.
	AppLabel string

	// FailurePolicy is used to override the behaviour for local development only
	FailurePolicy v1.FailurePolicyType
}

// Webhook returns the manifests for the operator's mutating and validating webhooks.
// In global mode, uses fixed webhook names and validates all namespaces. In scoped mode,
// uses namespace-qualified webhook names and namespace selectors to filter validation
// requests to watched namespaces only. If AppLabel is provided, updates the webhook
// service selector and labels to match.
func Webhook(options WebhookOptions) []kube.Object {
	if options.CABundle == nil {
		panic(errors.AssertionFailedf("K8s requires a CA certificate bundle to configure a webhook"))
	}

	objects := internal.DecodeYAML("yaml/webhook.yaml")

	isGlobal := len(options.WatchNamespaces) == 0

	// objects[0] is the MutatingWebhookConfiguration
	mutatingWebhookCfg := objects[0].(*v1.MutatingWebhookConfiguration)
	mutatingWebhookCfg.Name = GetWebhookConfigName(isGlobal, options.Namespace, true)
	mutatingWebhookCfg.Webhooks[0].ClientConfig.CABundle = options.CABundle

	// objects[1] is the ValidatingWebhookConfiguration
	validatingWebhookCfg := objects[1].(*v1.ValidatingWebhookConfiguration)
	validatingWebhookCfg.Name = GetWebhookConfigName(isGlobal, options.Namespace, false)
	validatingWebhookCfg.Webhooks[0].ClientConfig.CABundle = options.CABundle

	// Add namespace selectors for scoped mode to filter validation requests
	// to watched namespaces only.
	if !isGlobal {
		namespaceSelector := &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      "kubernetes.io/metadata.name",
					Operator: metav1.LabelSelectorOpIn,
					Values:   options.WatchNamespaces,
				},
			},
		}
		mutatingWebhookCfg.Webhooks[0].NamespaceSelector = namespaceSelector
		validatingWebhookCfg.Webhooks[0].NamespaceSelector = namespaceSelector
	}

	// Set namespaces.
	if options.Namespace != "" {
		mutatingWebhookCfg.Webhooks[0].ClientConfig.Service.Namespace = options.Namespace
		validatingWebhookCfg.Webhooks[0].ClientConfig.Service.Namespace = options.Namespace
		// objects[2] is the Service
		svc := objects[2].(*corev1.Service)
		svc.ObjectMeta.Namespace = options.Namespace
	}

	// Update service selector and labels to match the operator pod label.
	if options.AppLabel != "" {
		// objects[2] is the Service
		svc := objects[2].(*corev1.Service)
		svc.Spec.Selector = map[string]string{"app": options.AppLabel}
		svc.ObjectMeta.Labels = map[string]string{"app": options.AppLabel}
	}

	if options.FailurePolicy != "" {
		validatingWebhookCfg.Webhooks[0].FailurePolicy = &options.FailurePolicy
		mutatingWebhookCfg.Webhooks[0].FailurePolicy = &options.FailurePolicy
	}

	return objects
}

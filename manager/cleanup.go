package manager

import (
	"context"
	"time"

	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// cleanupWebhookConfigs removes webhook configurations and the webhook Service
// that this operator instance applied at startup. These resources are not
// tracked by Helm, so they linger after helm uninstall unless we delete them
// explicitly. Webhook configs are cluster-scoped and can't use ownerReferences
// to a namespace-scoped Deployment, so K8s GC won't help here either.
//
// We only clean up when the operator Deployment is already gone. If the
// Deployment still exists, this is a rolling restart or upgrade and the new
// pod will recreate the resources on startup. Deleting them here would leave
// a gap with no admission webhooks.
//
// In scoped mode the webhook names include the operator namespace, so each
// operator instance cleans up only its own webhooks.
func (m *Manager) cleanupWebhookConfigs() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	deployment := &appsv1.Deployment{}
	deployment.Namespace = m.namespace
	err := m.ctl.Get(ctx, manifests.OperatorName, deployment)
	switch {
	case err == nil:
		logging.Infof(ctx, "operator Deployment %q still exists, skipping webhook cleanup (rolling restart)",
			manifests.OperatorName)
		return
	case !kubeerrors.IsNotFound(err):
		// If we can't determine whether the Deployment exists, don't risk
		// deleting webhooks that a running operator still needs.
		logging.Errorf(ctx, "failed to check operator Deployment %q, skipping webhook cleanup: %v",
			manifests.OperatorName, err)
		return
	}

	isGlobal := len(m.watchNamespaces) == 0

	validatingName := manifests.GetWebhookConfigName(isGlobal, m.namespace, false)
	mutatingName := manifests.GetWebhookConfigName(isGlobal, m.namespace, true)

	validatingWebhook := &admissionregistrationv1.ValidatingWebhookConfiguration{}
	validatingWebhook.Name = validatingName
	if err := m.ctl.Delete(ctx, validatingWebhook); err != nil && !kubeerrors.IsNotFound(err) {
		logging.Errorf(ctx, "failed to delete ValidatingWebhookConfiguration %q on shutdown: %v",
			validatingName, err)
	} else if err == nil {
		logging.Infof(ctx, "deleted ValidatingWebhookConfiguration %q on shutdown", validatingName)
	}

	mutatingWebhook := &admissionregistrationv1.MutatingWebhookConfiguration{}
	mutatingWebhook.Name = mutatingName
	if err := m.ctl.Delete(ctx, mutatingWebhook); err != nil && !kubeerrors.IsNotFound(err) {
		logging.Errorf(ctx, "failed to delete MutatingWebhookConfiguration %q on shutdown: %v",
			mutatingName, err)
	} else if err == nil {
		logging.Infof(ctx, "deleted MutatingWebhookConfiguration %q on shutdown", mutatingName)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      manifests.WebhookServiceName,
			Namespace: m.namespace,
		},
	}
	if err := m.ctl.Delete(ctx, svc); err != nil && !kubeerrors.IsNotFound(err) {
		logging.Errorf(ctx, "failed to delete webhook Service %q in namespace %q on shutdown: %v",
			manifests.WebhookServiceName, m.namespace, err)
	} else if err == nil {
		logging.Infof(ctx, "deleted webhook Service %q in namespace %q on shutdown",
			manifests.WebhookServiceName, m.namespace)
	}
}

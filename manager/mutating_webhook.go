package manager

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/cockroachdb/errors"
	crdbv1alpha1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1alpha1"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// NewMutatingWebhook constructs a new MutatingWebhook instance. When
// migrationEnabled is true, the webhook injects a skip-reconcile label so the
// public operator ignores v1beta1 clusters during migration.
func NewMutatingWebhook(migrationEnabled bool) *MutatingWebhook {
	return &MutatingWebhook{migrationEnabled: migrationEnabled}
}

// MutatingWebhook implements a K8s mutating admission plug-in, which mutates
// incoming K8s API requests that attempt to create or modify CRDB custom
// resources. It provides sensible defaults for fields that users may not want
// to specify explicitly.
type MutatingWebhook struct {
	decoder          *admission.Decoder
	migrationEnabled bool
}

// Handle is called by K8s when a K8s API request needs to be mutated. It
// returns a JSON patch that will be applied to the incoming object.
func (h *MutatingWebhook) Handle(ctx context.Context, req admission.Request) admission.Response {
	// Don't mutate requests to delete objects.
	if req.Operation == admissionv1.Delete {
		return admission.Allowed("OK")
	}

	switch req.Kind.Kind {
	case "CrdbCluster":
		return h.handleCrdbCluster(ctx, req)
	}

	return admission.Allowed("OK")
}

// handleCrdbCluster mutates CrdbCluster objects to provide sensible defaults.
func (h *MutatingWebhook) handleCrdbCluster(
	ctx context.Context, req admission.Request,
) admission.Response {
	// Decode the CrdbCluster from the AdmissionRequest.
	var cluster crdbv1beta1.CrdbCluster
	if err := h.decoder.Decode(req, &cluster); err != nil {
		logging.Errorf(ctx, "mutating webhook: failed to decode cluster: %v", err)
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Track if we made any mutations.
	mutated := false

	// Inject skip-reconcile label so the public operator ignores v1beta1
	// clusters when both operators coexist. Needed for all clusters (fresh
	// and migrating) because the public operator's v1alpha1 informer would
	// otherwise attempt to reconcile them and create conflicting resources.
	if h.migrationEnabled {
		if cluster.Labels == nil {
			cluster.Labels = make(map[string]string)
		}
		if cluster.Labels[crdbv1alpha1.CrdbOperatorMigrationLabel] != "true" {
			cluster.Labels[crdbv1alpha1.CrdbOperatorMigrationLabel] = "true"
			mutated = true
		}
	}

	// If PodTemplate is specified but has no containers, inject a default
	// cockroachdb container with the image from the node spec.
	if cluster.Spec.Template.Spec.PodTemplate != nil {
		if len(cluster.Spec.Template.Spec.PodTemplate.Spec.Containers) == 0 {
			// Inject a default container with the image from the node spec.
			cluster.Spec.Template.Spec.PodTemplate.Spec.Containers = []corev1.Container{
				{
					Name:  manifests.CRDBContainerName,
					Image: cluster.Spec.Template.Spec.Image,
				},
			}
			mutated = true
			logging.Infof(ctx, "mutating webhook: injected default container for CrdbCluster %s/%s",
				cluster.Namespace, cluster.Name)
		}
	}

	// If we didn't mutate anything, just allow the request.
	if !mutated {
		return admission.Allowed("OK")
	}

	// Marshal the mutated cluster back to JSON.
	mutatedBytes, err := json.Marshal(cluster)
	if err != nil {
		logging.Errorf(ctx, "mutating webhook: failed to marshal cluster: %v", err)
		return admission.Errored(http.StatusInternalServerError,
			errors.Wrap(err, "failed to marshal mutated cluster"))
	}

	// Return a patch response.
	return admission.PatchResponseFromRaw(req.Object.Raw, mutatedBytes)
}

// InjectDecoder implements admission.DecoderInjector. This is called by the K8s
// admission runtime to provide a decoder that is used by the Handle method to
// unmarshal objects in the request.
func (h *MutatingWebhook) InjectDecoder(d *admission.Decoder) error {
	h.decoder = d
	return nil
}

package manager_test

import (
	"testing"

	crdbv1alpha1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1alpha1"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manager"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMutatingWebhookForPodTemplate(t *testing.T) {
	handler := manager.NewMutatingWebhook(false)
	invokeHandler := makeInvokeHandler(t, handler)

	// Helper function to create a base CrdbCluster for testing
	createBaseCluster := func() *crdbv1beta1.CrdbCluster {
		return &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "default",
				Labels:    map[string]string{crdbv1alpha1.CrdbOperatorMigrationLabel: "true"},
			},
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: true,
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{
						CloudProvider: "GCP",
						Code:          "us-east1",
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						Image: "cockroachdb/cockroach:v23.1.0",
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
						},
						Certificates: crdbv1beta1.Certificates{
							ExternalCertificates: &crdbv1beta1.ExternalCertificates{
								CAConfigMapName:         "ca-config",
								NodeSecretName:          "node-secret",
								RootSQLClientSecretName: "root-client-secret",
							},
						},
					},
				},
			},
		}
	}

	t.Run("injects default container when PodTemplate has no containers", func(t *testing.T) {
		cluster := createBaseCluster()
		cluster.Spec.Template.Spec.PodTemplate = &crdbv1beta1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				// No containers specified - this should be injected by the webhook.
				Affinity: &corev1.Affinity{
					NodeAffinity: &corev1.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
							NodeSelectorTerms: []corev1.NodeSelectorTerm{
								{
									MatchExpressions: []corev1.NodeSelectorRequirement{
										{
											Key:      "node-type",
											Operator: corev1.NodeSelectorOpIn,
											Values:   []string{"crdb"},
										},
									},
								},
							},
						},
					},
				},
			},
		}

		resp := invokeHandler(cluster, admissionv1.Create)

		// Verify the response is allowed and has patches.
		require.True(t, resp.Allowed, "response should be allowed")
		require.NotNil(t, resp.Patches, "patches should not be nil")
		require.NotEmpty(t, resp.Patches, "patches should not be empty")

		// Verify the patch is for the containers field.
		require.Len(t, resp.Patches, 1)
		patch := resp.Patches[0]
		require.Equal(t, "replace", string(patch.Operation))
		require.Equal(t, "/spec/template/spec/podTemplate/spec/containers", patch.Path)

		// Verify the patch value contains the injected container.
		containersValue, ok := patch.Value.([]any)
		require.True(t, ok, "patch value should be an array")
		require.Len(t, containersValue, 1)

		containerMap, ok := containersValue[0].(map[string]any)
		require.True(t, ok, "container should be a map")
		require.Equal(t, manifests.CRDBContainerName, containerMap["name"])
		require.Equal(t, "cockroachdb/cockroach:v23.1.0", containerMap["image"])
	})

	t.Run("does not mutate when containers are already present", func(t *testing.T) {
		cluster := createBaseCluster()
		cluster.Spec.Template.Spec.PodTemplate = &crdbv1beta1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "custom-container",
						Image: "custom/image:latest",
					},
				},
			},
		}

		resp := invokeHandler(cluster, admissionv1.Create)

		// Verify the response is allowed and no patches were applied.
		require.True(t, resp.Allowed)
		require.Empty(t, resp.Patches)
	})

	t.Run("does not mutate when no PodTemplate is specified", func(t *testing.T) {
		cluster := createBaseCluster()
		// No PodTemplate specified - should not mutate

		resp := invokeHandler(cluster, admissionv1.Create)

		// Verify the response is allowed and no patches were applied.
		require.True(t, resp.Allowed)
		require.Empty(t, resp.Patches)
	})
}

func TestMutatingWebhookSkipReconcileLabel(t *testing.T) {
	handler := manager.NewMutatingWebhook(true)
	invokeHandler := makeInvokeHandler(t, handler)

	t.Run("injects skip-reconcile label on v1beta1 cluster", func(t *testing.T) {
		cluster := &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "new-cluster",
				Namespace: "default",
			},
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: true,
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						Image: "cockroachdb/cockroach:v24.1.0",
					},
				},
			},
		}

		resp := invokeHandler(cluster, admissionv1.Create)

		require.True(t, resp.Allowed)
		require.Len(t, resp.Patches, 1)
		require.Equal(t, "add", string(resp.Patches[0].Operation))
		require.Equal(t, "/metadata/labels", resp.Patches[0].Path)
	})

	t.Run("does not patch when label already present", func(t *testing.T) {
		cluster := &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "existing-cluster",
				Namespace: "default",
				Labels:    map[string]string{crdbv1alpha1.CrdbOperatorMigrationLabel: "true"},
			},
			Spec: crdbv1beta1.CrdbClusterSpec{
				TLSEnabled: true,
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						Image: "cockroachdb/cockroach:v24.1.0",
					},
				},
			},
		}

		resp := invokeHandler(cluster, admissionv1.Create)

		require.True(t, resp.Allowed)
		require.Empty(t, resp.Patches)
	})
}

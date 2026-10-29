package kube

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMarshalObject(t *testing.T) {
	nginxDeployment := nginxDeployment.DeepCopy()

	nginxDeployment.TypeMeta.Kind = "Deployment"
	nginxDeployment.TypeMeta.APIVersion = "apps/v1"

	nginxDeployment.Annotations = map[string]string{
		"Hello":                                 "World",
		restartedAtAnnotation:                   "Now",
		deploymentRevisionAnnotation:            "1",
		deprecatedDaemonsetGenerationAnnotation: "2",
		corev1.LastAppliedConfigAnnotation:      "{}",
	}

	nginxDeployment.ManagedFields = []metav1.ManagedFieldsEntry{
		{Manager: "some-manager"},
	}

	outPatch, err := marshalObject(nginxDeployment)
	require.NoError(t, err)

	require.Contains(t, string(outPatch), `"kind":`)
	require.Contains(t, string(outPatch), `"apiVersion":`)
	require.Contains(t, string(outPatch), fmt.Sprintf(`"%s":`, corev1.LastAppliedConfigAnnotation))
	require.NotContains(t, string(outPatch), fmt.Sprintf(`"%s":`, deploymentRevisionAnnotation))
	require.NotContains(t, string(outPatch), fmt.Sprintf(`"%s":`, deprecatedDaemonsetGenerationAnnotation))
	require.NotContains(t, string(outPatch), fmt.Sprintf(`"%s":`, restartedAtAnnotation))
	require.Contains(t, string(outPatch), `"Hello":"World"`)
	require.NotContains(t, string(outPatch), `"uid":`)
	require.NotContains(t, string(outPatch), `"status":`)
	require.NotContains(t, string(outPatch), `some-manager`)
}

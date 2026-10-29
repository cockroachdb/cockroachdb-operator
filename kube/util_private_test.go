package kube

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestApplyShouldUpdate(t *testing.T) {
	t.Run("AWSCrossZone", func(t *testing.T) {
		svc := corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name: "aws-owes-me-lunch",
				Annotations: map[string]string{
					awsCrossZoneLoadBalancingAnnotation: "true",
				},
			},
		}

		clone := shouldPreApply(&svc)

		require.NotEqual(t, svc, clone)

		// Clone has had the annotation removed.
		require.NotContains(t, clone.(*corev1.Service).Annotations, awsCrossZoneLoadBalancingAnnotation)

		// svc was not modified.
		require.Contains(t, svc.Annotations, awsCrossZoneLoadBalancingAnnotation)
	})

	t.Run("Namespace", func(t *testing.T) {
		ns := corev1.Namespace{}
		require.Nil(t, shouldPreApply(&ns))
	})

	t.Run("Service", func(t *testing.T) {
		svc := corev1.Service{}
		require.Nil(t, shouldPreApply(&svc))
	})
}

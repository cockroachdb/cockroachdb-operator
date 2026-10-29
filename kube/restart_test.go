package kube

import (
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestRestartObject(t *testing.T) {
	t.Run("unsupported", func(t *testing.T) {
		require.Panics(t, func() {
			_ = RestartObject(&corev1.Namespace{})
		})
	})

	t.Run("deployment", func(t *testing.T) {
		dep := &appsv1.Deployment{}

		out := RestartObject(dep)

		require.Contains(t, dep.Spec.Template.Annotations, restartedAtAnnotation)
		require.Contains(t, out.(*appsv1.Deployment).Spec.Template.Annotations, restartedAtAnnotation)
	})

	t.Run("daemonset", func(t *testing.T) {
		ds := &appsv1.DaemonSet{}

		out := RestartObject(ds)

		require.Contains(t, ds.Spec.Template.Annotations, restartedAtAnnotation)
		require.Contains(t, out.(*appsv1.DaemonSet).Spec.Template.Annotations, restartedAtAnnotation)
	})

	t.Run("statefulset", func(t *testing.T) {
		sts := &appsv1.StatefulSet{}

		out := RestartObject(sts)

		require.Contains(t, sts.Spec.Template.Annotations, restartedAtAnnotation)
		require.Contains(t, out.(*appsv1.StatefulSet).Spec.Template.Annotations, restartedAtAnnotation)
	})
}

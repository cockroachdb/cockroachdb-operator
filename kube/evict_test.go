package kube

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestEnsureReplacementReadySameUID(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID("same")}}
	err := ensureReplacementReady(types.UID("same"), pod)
	require.EqualError(t, err, "pod is not yet evicted")
}

func TestEnsureReplacementReadyNotReady(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("new")},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
		},
	}
	err := ensureReplacementReady(types.UID("old"), pod)
	require.Error(t, err)
	require.Contains(t, err.Error(), "pod readiness condition status")
}

func TestEnsureReplacementReadySuccess(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("new")},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	require.NoError(t, ensureReplacementReady(types.UID("old"), pod))
}

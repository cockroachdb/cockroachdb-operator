package kubetest

import (
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

var customQuiescentHandlers []func(object kube.Object) error

// AddCustomQuiescentHandler adds a function that handles the quiescent state
// for a custom object that is not handled by MakeQuiescent. It should be called
// in the init method of a calling package.
func AddCustomQuiescentHandler(handler func(object kube.Object) error) {
	customQuiescentHandlers = append(customQuiescentHandlers, handler)
}

// MakeQuiescent takes a K8s object and attempts to set its fields such that it
// will be declared "quiescent" by the kube.IsQuiescent function. This is useful
// when using the FakeCtl to avoid blocking forever when Apply or other methods
// are called.
// Typical usage:
//
//	ctl := kubetest.NewFakeCtl(t, kube.Options{BeforeMutation: MakeQuiescent})
func MakeQuiescent(object kube.Object) error {
	// All quiescent objects must first be in the ready state.
	if err := MakeReady(object); err != nil {
		return err
	}

	switch t := object.(type) {
	case *appsv1.DaemonSet:
		t.Status.UpdatedNumberScheduled = t.Status.DesiredNumberScheduled

	case *appsv1.Deployment:
		t.Status.UpdatedReplicas = t.Status.Replicas
		t.Status.Conditions = []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
			{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable"},
		}

	case *appsv1.StatefulSet:
		if t.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType {
			break
		}

		if t.Spec.UpdateStrategy.RollingUpdate != nil {
			partition := t.Spec.UpdateStrategy.RollingUpdate.Partition
			if partition != nil {
				t.Status.UpdatedReplicas = t.Status.Replicas - *partition
				break
			}
		}
		t.Status.UpdateRevision = t.Status.CurrentRevision
	}

	return nil
}

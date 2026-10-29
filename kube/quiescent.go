package kube

import (
	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// IsQuiescent returns nil once the given K8s object IsReady and has no pending
// operations being performed on it. This is in contrast to IsReady, which only
// reports when an object is healthy and ready to respond to requests.
//
// For example, a StatefulSet that's performing a rolling update will pass the
// checks of IsReady but will not be considered Quiescent until the update has
// completed.
//
// Additional Is<Type>Quiescent functions may assume that Is<Type>Ready has
// returned nil before invocation.
func IsQuiescent(object Object) error {
	if err := IsReady(object); err != nil {
		return err
	}

	switch t := object.(type) {
	case *crdbv1beta1.CrdbCluster:
		return t.IsQuiescent()
	case *appsv1.DaemonSet:
		return IsDaemonSetQuiescent(t)
	case *appsv1.Deployment:
		return IsDeploymentQuiescent(t)
	case *appsv1.StatefulSet:
		return IsStatefulSetQuiescent(t)
	default:
		// For less complex objects, the IsReady check is sufficient for
		// determining quiescence.
		return nil
	}
}

// IsStatefulSetQuiescent returns nil when the provided stateful has no pending
// updates to perform. This could mean that a rolling update has completed,
// reached its partition, or that the OnDelete strategy is being used.
func IsStatefulSetQuiescent(sts *appsv1.StatefulSet) error {
	// If the update strategy is OnDelete, there are no additional checks to be
	// done, short circuit.
	if sts.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType {
		return nil
	}

	// If the UpdatedStrategy is not OnDelete, assume that it's RollingUpdate
	// which is the default.

	// First, extract the partition value, if any.
	var partition *int32
	if sts.Spec.UpdateStrategy.RollingUpdate != nil {
		partition = sts.Spec.UpdateStrategy.RollingUpdate.Partition
	}

	// If partition was specified, we have to handle manual validation to ensure
	// that all pods with an ordinal < partition have been updated.
	if partition != nil {
		if sts.Status.UpdatedReplicas == sts.Status.Replicas-*partition {
			return nil
		}
		return errors.Newf(
			"only %d/%d partitioned replicas are updated",
			sts.Status.UpdatedReplicas,
			sts.Status.Replicas-*partition,
		)
	}

	// Otherwise, rely on the statefulset controller's rolling update
	// completion [1] to perform any final checks.
	//
	// [1] https://github.com/kubernetes/kubernetes/blob/b8ce285a03a7e9d59e94712248ab96c54a3216a3/pkg/controller/statefulset/stateful_set_control.go#L583-L588
	if sts.Status.CurrentRevision != sts.Status.UpdateRevision {
		return errors.Newf("sts update not yet complete revision %s != %s", sts.Status.CurrentRevision, sts.Status.UpdateRevision)
	}

	return nil
}

// IsDeploymentQuiescent returns nil if the provided deployment's replicas are
// up to date with its template.
func IsDeploymentQuiescent(deployment *appsv1.Deployment) error {
	if deployment.Status.Replicas != deployment.Status.UpdatedReplicas {
		return errors.Newf("only %d/%d deployment replicas are updated", deployment.Status.UpdatedReplicas, deployment.Status.Replicas)
	}

	// Based on docs at https://kubernetes.io/docs/concepts/workloads/controllers/deployment/#complete-deployment
	// and confirmed by watching live deployments, a deployment is known to be complete
	// when its status includes a condition with the following attributes:
	//   - type: Progressing
	//   - status: "True"
	//   - reason: NewReplicaSetAvailable
	deploymentAvailable := false
	deploymentProgessing := false
	for _, condition := range deployment.Status.Conditions {
		switch condition.Type {
		case appsv1.DeploymentProgressing:
			deploymentProgessing = condition.Status == corev1.ConditionTrue && condition.Reason == "NewReplicaSetAvailable"
		case appsv1.DeploymentAvailable:
			deploymentAvailable = condition.Status == corev1.ConditionTrue
		}
	}

	if !(deploymentAvailable && deploymentProgessing) {
		return errors.New("deployment has not fully rolled out yet")
	}

	return nil
}

// IsDaemonSetQuiescent returns nil if the provided daemonset's replicas are up
// to date with its template.
func IsDaemonSetQuiescent(daemonset *appsv1.DaemonSet) error {
	if daemonset.Status.DesiredNumberScheduled != daemonset.Status.UpdatedNumberScheduled {
		return errors.Newf("only %d/%d pods are updated", daemonset.Status.UpdatedNumberScheduled, daemonset.Status.DesiredNumberScheduled)
	}

	return nil
}

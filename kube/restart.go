package kube

import (
	"fmt"
	"time"

	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
	appsv1 "k8s.io/api/apps/v1"
)

const (
	restartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"
)

// RestartObject modifies the given K8s object such that Applying it to
// a cluster will trigger a rolling restart.
// See https://github.com/kubernetes/kubectl/blob/691a3747d01959f2c4e82e317f81348a70518217/pkg/polymorphichelpers/objectrestarter.go
func RestartObject(object Object) Object {
	switch t := object.(type) {
	case *appsv1.Deployment:
		if t.Spec.Template.ObjectMeta.Annotations == nil {
			t.Spec.Template.ObjectMeta.Annotations = make(map[string]string)
		}
		t.Spec.Template.ObjectMeta.Annotations[restartedAtAnnotation] = timeutil.Now().Format(time.RFC3339)
		return t

	case *appsv1.DaemonSet:
		if t.Spec.Template.ObjectMeta.Annotations == nil {
			t.Spec.Template.ObjectMeta.Annotations = make(map[string]string)
		}
		t.Spec.Template.ObjectMeta.Annotations[restartedAtAnnotation] = timeutil.Now().Format(time.RFC3339)
		return t

	case *appsv1.StatefulSet:
		if t.Spec.Template.ObjectMeta.Annotations == nil {
			t.Spec.Template.ObjectMeta.Annotations = make(map[string]string)
		}
		t.Spec.Template.ObjectMeta.Annotations[restartedAtAnnotation] = timeutil.Now().Format(time.RFC3339)
		return t

	default:
		panic(fmt.Sprintf("unhandled object type: %T", object))
	}
}

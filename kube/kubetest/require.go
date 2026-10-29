package kubetest

import (
	"context"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// RequireExists asserts that a [kube.Object] of type obj with the given name
// exists.
func RequireExists(t *testing.T, ctl *kube.Ctl, name string, obj kube.Object) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoErrorf(t, ctl.Get(ctx, name, obj), "%T %q does not exist in namespace %q", obj, name, ctl.Namespace())
}

// RequireNotExists asserts that a [kube.Object] of type obj with the given
// name does not exist.
func RequireNotExists(t *testing.T, ctl *kube.Ctl, name string, obj kube.Object) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := ctl.Get(ctx, name, obj)
	require.Errorf(t, err, "%T %q exists in namespace %q", obj, name, ctl.Namespace())
	require.Truef(t, kubeerrors.IsNotFound(err), "Getting %T %q in namespace %q failed", obj, name, ctl.Namespace())
}

// RequireControllerReference asserts that a [kube.Object] with the given name
// has an owner reference to owner with the controller bit set.
func RequireControllerReference(t *testing.T, ctl *kube.Ctl, name string, obj, owner kube.Object) {
	t.Helper()

	// Load up obj with the results of kube.Ctl.Get.
	RequireExists(t, ctl, name, obj)

	gvk, err := apiutil.GVKForObject(owner, crdbv1beta1.Scheme)
	require.NoError(t, err)

	// Craft an owner reference that matches one created by
	// [controllerutil.SetControllerReference].
	ownerReference := metav1.OwnerReference{
		APIVersion:         gvk.GroupVersion().String(),
		Kind:               gvk.Kind,
		Name:               owner.GetName(),
		UID:                owner.GetUID(),
		BlockOwnerDeletion: new(true),
		Controller:         new(true),
	}

	require.Containsf(t, obj.GetOwnerReferences(), ownerReference, "%T %q is not owned by %T %q", obj, obj.GetName(), owner, owner.GetName())
}

// RequireControllerReference asserts that a [kube.Object] with the given name
// does not have an owner reference to owner with the controller bit set.
func RequireNoControllerReference(
	t *testing.T, ctl *kube.Ctl, name string, obj, owner kube.Object,
) {
	t.Helper()

	// Load up obj with the results of kube.Ctl.Get.
	RequireExists(t, ctl, name, obj)

	gvk, err := apiutil.GVKForObject(owner, crdbv1beta1.Scheme)
	require.NoError(t, err)

	// Craft an owner reference that matches one created by
	// [controllerutil.SetControllerReference].
	ownerReference := metav1.OwnerReference{
		APIVersion:         gvk.GroupVersion().String(),
		Kind:               gvk.Kind,
		Name:               owner.GetName(),
		UID:                owner.GetUID(),
		BlockOwnerDeletion: new(true),
		Controller:         new(true),
	}

	require.NotContainsf(t, obj.GetOwnerReferences(), ownerReference, "%T %q is not owned by %T %q", obj, obj.GetName(), owner, owner.GetName())
}

// RequireValidPodSpec does a sanity check over the pod spec.
// It checks the following:
// - all container volume mounts have a corresponding volume defined.
// Passing this test doesn't mean that the pod spec is
// valid in practice. E.g., we don't (and can't) check if a mounted
// configmap actually exist.
func RequireValidPodSpec(t *testing.T, pod *corev1.Pod) {
	volumes := make(map[string]struct{}, len(pod.Spec.Volumes))
	for i := range pod.Spec.Volumes {
		volumes[pod.Spec.Volumes[i].Name] = struct{}{}
	}
	// Check if every volume mount or device has a corresponding volume.
	for i := range pod.Spec.Containers {
		for _, mount := range pod.Spec.Containers[i].VolumeMounts {
			require.Contains(t, volumes, mount.Name)
		}
		for _, device := range pod.Spec.Containers[i].VolumeDevices {
			require.Contains(t, volumes, device.Name)
		}
	}
}

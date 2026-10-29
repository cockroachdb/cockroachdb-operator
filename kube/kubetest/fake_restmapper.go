package kubetest

import (
	"reflect"

	"github.com/cockroachdb/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

// FakeRESTMapper is a meta.RESTMapper that only responds to the RESTMapping
// method. It may be used during tests to ensure that namespace validation is
// happening as expected.
// TODO(chrisseto): Consider iterating over a Scheme and serializing the values
// of a real mapper and just replaying those values. This could then be used a
// default for the FakeCtl.
// Typical usage:
//
//	ctl := kubetest.NewFakeCtl(t, kube.Options{Mapper: &FakeRESTMapper{}})
type FakeRESTMapper struct{}

var _ meta.RESTMapper = &FakeRESTMapper{}

// RESTMapping identifies a preferred resource mapping for the provided group kind.
func (f *FakeRESTMapper) RESTMapping(
	gk schema.GroupKind, versions ...string,
) (*meta.RESTMapping, error) {
	// We intentionally don't fill out any other fields of the RESTMapping. The
	// goal is to make namespace validation work as expected. A more complete
	// solution could be achieved by recording the calls to a real Kubernetes
	// cluster and serializing the resulting mapping(s).
	mapping := &meta.RESTMapping{}

	switch gk.Group {
	// All resources in these groups are know to be namespaced scoped.
	case appsv1.GroupName:
		mapping.Scope = meta.RESTScopeNamespace
		return mapping, nil
	}

	// Handle other resources on a per resource basis.
	// Kids, don't try this at home
	gvk := gk.WithVersion(versions[0])
	switch t := reflect.New(clientgoscheme.Scheme.AllKnownTypes()[gvk]).Interface().(type) {
	case *corev1.Namespace,
		*rbacv1.ClusterRole,
		*rbacv1.ClusterRoleList,
		*rbacv1.ClusterRoleBinding,
		*rbacv1.ClusterRoleBindingList,
		*schedulingv1.PriorityClass,
		*schedulingv1.PriorityClassList,
		*storagev1.StorageClass:
		mapping.Scope = meta.RESTScopeRoot

	case *corev1.Secret,
		*corev1.SecretList,
		*corev1.ConfigMapList,
		*corev1.ServiceAccount,
		*corev1.ServiceAccountList,
		*corev1.ConfigMap,
		*corev1.PersistentVolume,
		*corev1.PersistentVolumeList,
		*corev1.PersistentVolumeClaim,
		*corev1.PersistentVolumeClaimList,
		*corev1.Service,
		*corev1.ServiceList,
		*policyv1.PodDisruptionBudget,
		*policyv1.PodDisruptionBudgetList:
		mapping.Scope = meta.RESTScopeNamespace

	default:
		return nil, errors.Newf("unknown type %T", t)
	}

	return mapping, nil
}

// KindFor takes a partial resource and returns the single match.  Returns an error if there are multiple matches
func (f *FakeRESTMapper) KindFor(
	resource schema.GroupVersionResource,
) (schema.GroupVersionKind, error) {
	return schema.GroupVersionKind{}, errors.New("not implemented")
}

// KindsFor takes a partial resource and returns the list of potential kinds in priority order
func (f *FakeRESTMapper) KindsFor(
	resource schema.GroupVersionResource,
) ([]schema.GroupVersionKind, error) {
	return nil, errors.New("not implemented")
}

// ResourceFor takes a partial resource and returns the single match.  Returns an error if there are multiple matches
func (f *FakeRESTMapper) ResourceFor(
	input schema.GroupVersionResource,
) (schema.GroupVersionResource, error) {
	return schema.GroupVersionResource{}, errors.New("not implemented")
}

// ResourcesFor takes a partial resource and returns the list of potential resource in priority order
func (f *FakeRESTMapper) ResourcesFor(
	input schema.GroupVersionResource,
) ([]schema.GroupVersionResource, error) {
	return nil, errors.New("not implemented")
}

// RESTMappings returns all resource mappings for the provided group kind if no
// version search is provided. Otherwise identifies a preferred resource mapping for
// the provided version(s).
func (f *FakeRESTMapper) RESTMappings(
	gk schema.GroupKind, versions ...string,
) ([]*meta.RESTMapping, error) {
	return nil, errors.New("not implemented")
}

// ResourceSingularizer implements meta.RESTMapper
func (f *FakeRESTMapper) ResourceSingularizer(resource string) (singular string, err error) {
	return "", errors.New("not implemented")
}

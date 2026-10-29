package kube

import (
	"github.com/cockroachdb/errors"
	crdbv1alpha1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1alpha1"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
)

var (
	// Scheme is the defaultScheme that will be used by NewCtl
	// if one is not supplied. It is equivalent to scheme.Scheme with
	// the addition of CustomResourceDefinitions.
	Scheme        = runtime.NewScheme()
	schemeBuilder = runtime.SchemeBuilder{
		scheme.AddToScheme,
		apiextv1.AddToScheme,
		apiextv1beta1.AddToScheme,
		crdbv1beta1.AddToScheme,
		crdbv1alpha1.AddToScheme,
	}
)

// AddToScheme adds all types of this clientset into the given scheme. This allows composition
// of clientsets, like in:
//
//	import (
//	  "k8s.io/client-go/kubernetes"
//	  clientsetscheme "k8s.io/client-go/kubernetes/scheme"
//	  aggregatorclientsetscheme "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset/scheme"
//	)
//
//	kclientset, _ := kubernetes.NewForConfig(c)
//	_ = aggregatorclientsetscheme.AddToScheme(clientsetscheme.Scheme)
//
// After this, RawExtensions in Kubernetes types will serialize kube-aggregator types
// correctly.
var AddToScheme = schemeBuilder.AddToScheme

func init() {
	utilruntime.Must(AddToScheme(Scheme))
}

// GVKFromScheme determines an object's GroupVersionKind from the scheme.
func GVKFromScheme(scheme *runtime.Scheme, object runtime.Object) (schema.GroupVersionKind, error) {
	schemeGvks, unversioned, err := scheme.ObjectKinds(object)
	if err != nil {
		return schema.GroupVersionKind{}, errors.Wrapf(err, "could not find GVK for object '%v' in the scheme", object)
	} else if unversioned {
		return schema.GroupVersionKind{}, errors.New("unversioned objects not supported")
	}
	return schemeGvks[0], nil
}

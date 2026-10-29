package v1beta1

import (
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

var (
	// Scheme contains all the standard Kubernetes types, plus the
	// CustomResourceDefinition, CrdbCluster, and CrdbTenant types.
	Scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(Scheme))
	utilruntime.Must(apiextv1.AddToScheme(Scheme))
	utilruntime.Must(apiextv1beta1.AddToScheme(Scheme))
	utilruntime.Must(AddToScheme(Scheme))
	addAWSLBControllerToSchema(Scheme)
	addCiliumToSchema(Scheme)
}

func addAWSLBControllerToSchema(schema *runtime.Scheme) {
	schema.AddKnownTypeWithName(runtimeschema.GroupVersionKind{
		Group:   "elbv2.k8s.aws",
		Version: "v1beta1",
		Kind:    "IngressClassParams",
	}, &unstructured.Unstructured{})
	schema.AddKnownTypeWithName(runtimeschema.GroupVersionKind{
		Group:   "elbv2.k8s.aws",
		Version: "v1beta1",
		Kind:    "TargetGroupBinding",
	}, &unstructured.Unstructured{})
}

func addCiliumToSchema(schema *runtime.Scheme) {
	// We must explicitly register the CiliumNetworkPolicy object so Ctl can
	// can [de]serialize the resource. This is a temporary workaround for the
	// inability to import the Cilium pkg because of incompatible dependencies.
	// TODO(carlo): checkup on Prometheus dependency.
	schema.AddKnownTypeWithName(runtimeschema.GroupVersionKind{
		Group:   "cilium.io",
		Version: "v2",
		Kind:    "CiliumNetworkPolicy",
	}, &unstructured.Unstructured{})
	schema.AddKnownTypeWithName(runtimeschema.GroupVersionKind{
		Group:   "cilium.io",
		Version: "v2",
		Kind:    "CiliumClusterwideNetworkPolicy",
	}, &unstructured.Unstructured{})
}

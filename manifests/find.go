package manifests

import (
	"slices"

	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
)

func findResource[T kube.Object](in []kube.Object, name string, kind T) T {
	index := slices.IndexFunc(in, func(obj kube.Object) bool {
		if obj.GetName() != name {
			return false
		}

		gvk, _ := kube.GVKFromScheme(kube.Scheme, kind)
		return obj.GetObjectKind().GroupVersionKind() == gvk
	})

	return in[index].(T)
}

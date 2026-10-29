package kubecrdbtest

import (
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// NewRESTMapper returns a REST mapper for custom CRDB resources. REST mappers
// are used by K8s libraries to get metadata about resources.
func NewRESTMapper() *meta.DefaultRESTMapper {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{})

	register := []struct {
		kind     string
		singular string
		plural   string
	}{
		{kind: "CrdbTenant", singular: "crdbtenant", plural: "crdbtenants"},
		{kind: "CrdbCluster", singular: "crdbcluster", plural: "crdbclusters"},
	}

	for _, r := range register {
		gvk := schema.GroupVersionKind{
			Group:   crdbv1beta1.GroupVersion.Group,
			Version: crdbv1beta1.GroupVersion.Version,
			Kind:    r.kind,
		}
		singular := schema.GroupVersionResource{
			Group:    crdbv1beta1.GroupVersion.Group,
			Version:  crdbv1beta1.GroupVersion.Version,
			Resource: r.singular,
		}
		plural := schema.GroupVersionResource{
			Group:    crdbv1beta1.GroupVersion.Group,
			Version:  crdbv1beta1.GroupVersion.Version,
			Resource: r.plural,
		}
		mapper.AddSpecific(gvk, plural, singular, meta.RESTScopeNamespace)

		gvk.Kind = gvk.Kind + "List"
		mapper.AddSpecific(gvk, plural, singular, meta.RESTScopeNamespace)
	}

	return mapper
}

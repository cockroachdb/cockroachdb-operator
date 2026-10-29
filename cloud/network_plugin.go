package cloud

import (
	"context"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// NetworkPlugin identifies the type of container network plugin (CNI) that is
// installed within Kubernetes. Theoretically, Kubernetes can support multiple
// network plugins at the same time, but additional tooling may be required, and
// this setup isn't supported by the operator for now.
type NetworkPlugin string

func (p NetworkPlugin) String() string {
	return string(p)
}

const (
	// Calico Network Plugin.
	// https://www.tigera.io/project-calico/
	Calico = NetworkPlugin("calico")

	// Cilium Network Plugin.
	// https://cilium.io/
	Cilium = NetworkPlugin("cilium")
)

// GetNetworkPluginForCluster returns the network plugin installed within the
// Kubernetes cluster. This assumes that the cluster is using Calico, if
// Cilium isn't installed.
func GetNetworkPluginForCluster(ctx context.Context, ctl *kube.Ctl) (NetworkPlugin, error) {
	isCiliumInstalled := func() (bool, error) {
		var crdList apiextv1.CustomResourceDefinitionList
		if err := ctl.List(ctx, &kube.ListOptions{}, &crdList); err != nil {
			return false, errors.Wrap(err, "listing CustomResourceDefinitions")
		}
		crds := make(map[string]struct{})
		for _, crd := range crdList.Items {
			crds[crd.Name] = struct{}{}
		}
		for _, name := range []string{
			"ciliumnetworkpolicies.cilium.io",
			"ciliumclusterwidenetworkpolicies.cilium.io",
		} {
			if _, ok := crds[name]; !ok {
				return false, nil
			}
		}
		return true, nil
	}
	cilium, err := isCiliumInstalled()
	if err != nil {
		return NetworkPlugin(""), err
	}
	if cilium {
		return Cilium, nil
	}
	return Calico, nil
}

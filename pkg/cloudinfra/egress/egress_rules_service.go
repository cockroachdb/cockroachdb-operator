package egress

import (
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// AllowAllEgressPolicy returns a CiliumNetworkPolicy that allows egress
// to any and all workloads.
//
// TODO(carlo): this network policy should be constructed using Cilium's
// custom resource definition. We cannot add the Cilium package to the managed
// service repo due to conflicting dependencies, specifically prometheus. Once
// the dependencies are untangled, we should refactor this code.
func AllowAllEgressPolicy(allowLinkLocal bool) kube.Object {
	blockEgress := []any{"224.0.0.0/4"} // Always block multicast
	if !allowLinkLocal {
		blockEgress = append(blockEgress, "169.254.169.254/16")
	}
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cilium.io/v2",
			"kind":       "CiliumNetworkPolicy",
			"metadata": map[string]any{
				"name": manifests.EgressNetworkPolicyName,
			},
			"spec": map[string]any{
				"egress": []any{
					map[string]any{
						"toEntities": []any{"all"},
					},
				},
				"egressDeny": []any{
					map[string]any{
						"toCIDR": blockEgress,
					},
				},
				"endpointSelector": map[string]any{
					"matchLabels": map[string]any{
						"app": "cockroachdb",
					},
				},
			},
		},
	}
}

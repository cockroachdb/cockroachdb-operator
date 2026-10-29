package manifests

import (
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	DenyPolicyName = "deny-network-access"
)

func DenyNetworkingCiliumPolicyForSts(podList []string) kube.Object {
	return denyNetworkingCiliumPolicyWithKey("statefulset.kubernetes.io/pod-name", podList)
}

func DenyNetworkingCiliumPolicyForCrdbNodes(podList []string) kube.Object {
	return denyNetworkingCiliumPolicyWithKey(NodePodNameKey, podList)
}

func denyNetworkingCiliumPolicyWithKey(key string, podList []string) kube.Object {
	// NOTE(all): The only array type k8s expects is []interface{} so we have
	// to perform a conversion.
	pods := make([]any, len(podList))
	for i, pod := range podList {
		pods[i] = pod
	}
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cilium.io/v2",
			"kind":       "CiliumNetworkPolicy",
			"metadata": map[string]any{
				"name": DenyPolicyName,
			},
			"spec": map[string]any{
				"ingressDeny": []any{
					map[string]any{
						"fromEntities": []any{
							"all",
						},
					},
				},
				"egressDeny": []any{
					map[string]any{
						"toEntities": []any{
							"all",
						},
					},
				},
				"endpointSelector": map[string]any{
					"matchExpressions": []any{
						map[string]any{
							"key":      key,
							"operator": "In",
							"values":   pods,
						},
					},
				},
			},
		},
	}
}

package manifests

import (
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TenantNetworkPolicyOptions is a collections of options to tune the
// permissiveness of the resulting NetworkPolicy.
type TenantNetworkPolicyOptions struct {
	// TenantID is the TenantID that the Generate NetworkPolicy will apply to.
	TenantID crdbv1beta1.TenantID
	// NetworkPlugin indicates the type of network plugin that is being used
	// within the Kubernetes cluster.
	NetworkPlugin cloud.NetworkPlugin
}

// TenantNetworkPolicy creates a NetworkPolicy for a CrdbTenant with the
// minimal viable network access. Options may be provided to make the policy
// more permissive.
func TenantNetworkPolicy(opts TenantNetworkPolicyOptions) kube.Object {
	if opts.NetworkPlugin == cloud.Cilium {
		return ciliumTenantNetworkPolicy(opts.TenantID)
	}
	return tenantNetworkPolicy(opts.TenantID)
}

func tenantNetworkPolicy(tenantID crdbv1beta1.TenantID) *netv1.NetworkPolicy {
	tenantSelector := map[string]string{
		TenantIDLabel: tenantID.String(),
	}

	tailscaleSelector := map[string]string{
		TailscaleLabel: TailscaleApp,
	}

	return &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:   tenantID.Tag(),
			Labels: tenantSelector,
		},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: tenantSelector,
			},
			Egress: []netv1.NetworkPolicyEgressRule{
				// Allow connecting to pods with the same tenantID.
				{
					To: []netv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: tenantSelector,
							},
							// Match all namespaces.
							NamespaceSelector: &metav1.LabelSelector{},
						},
					},
				},
			},
			Ingress: []netv1.NetworkPolicyIngressRule{
				// Allow connections from pods with the same tenantID.
				{
					From: []netv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: tenantSelector,
							},
							// Match all namespaces.
							NamespaceSelector: &metav1.LabelSelector{},
						},
					},
				},
				// Allow connections from tailscale agent. This is used for insitu.
				{
					From: []netv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: tailscaleSelector,
							},
							// Match all namespaces.
							NamespaceSelector: &metav1.LabelSelector{},
						},
					},
				},
			},
		},
	}
}

func ciliumTenantNetworkPolicy(tenantID crdbv1beta1.TenantID) *unstructured.Unstructured {
	tenantSelector := map[string]any{
		TenantIDLabel: tenantID.String(),
	}
	tailscaleSelector := map[string]any{
		TailscaleLabel: TailscaleApp,
	}

	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cilium.io/v2",
			"kind":       "CiliumClusterwideNetworkPolicy",
			"metadata": map[string]any{
				"name":   tenantID.Tag(),
				"labels": tenantSelector,
			},
			"spec": map[string]any{
				"egress": []any{
					// Allow connecting to pods with the same tenantID.
					map[string]any{
						"toEndpoints": []any{
							map[string]any{
								"matchLabels": tenantSelector,
							},
						},
					},
				},
				"ingress": []any{
					// Allow connections from pods with the same tenantID.
					map[string]any{
						"fromEndpoints": []any{
							map[string]any{
								"matchLabels": tenantSelector,
							},
						},
					},
					// Allow connections from tailscale agent. This is used for insitu.
					map[string]any{
						"fromEndpoints": []any{
							map[string]any{
								"matchLabels": tailscaleSelector,
							},
						},
					},
				},
				"endpointSelector": map[string]any{
					"matchLabels": tenantSelector,
				},
			},
		},
	}
}

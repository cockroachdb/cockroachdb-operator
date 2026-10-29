package manifests

import (
	"fmt"

	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests/internal"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// K3dClusterBGPConfig describes the bgp config of a cluster.
type K3dClusterBGPConfig struct {
	// AddressAllocation determines the pod cidr, service cidr, asNumber
	// as follows:
	// PodCIDR: 10.<alloc>.0.0/17
	// ServiceCIDR: 10.<alloc>.128.0/17
	// ASNumber: 64512 + <alloc>
	AddressAllocation int
	PeeringNodes      []string
}

// GetASNumber returns the AS number for the cluster.
func (c *K3dClusterBGPConfig) GetASNumber() uint32 {
	return uint32(64512 + c.AddressAllocation)
}

// GetPodCIDR returns the pod cidr for the cluster.
func (c *K3dClusterBGPConfig) GetPodCIDR() string {
	return fmt.Sprintf("10.%d.0.0/17", c.AddressAllocation)
}

// GetServiceCIDR returns the service cidr for the cluster.
func (c *K3dClusterBGPConfig) GetServiceCIDR() string {
	return fmt.Sprintf("10.%d.128.0/17", c.AddressAllocation)
}

// K3DCalicoCNI returns objects that can be used for installing calico
// as the CNI plugin for the k8s cluster.
// This is *only* intended for use with k3d environment for local development
// or testing.
func K3DCalicoCNI(opts K3dClusterBGPConfig) []kube.Object {
	bgpConfig := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "crd.projectcalico.org/v1",
			"kind":       "BGPConfiguration",
			"metadata":   map[string]any{"name": "default"},
			"spec": map[string]any{
				"logSeverityScreen":     "Info",
				"nodeToNodeMeshEnabled": true,
				"asNumber":              opts.GetASNumber(),
				"serviceClusterIPs": []map[string]string{
					{
						"cidr": opts.GetServiceCIDR(),
					},
				},
			},
		},
	}
	// Force legacy iptables backend because the nftables backend does not play well with different versions of
	// iptables which could be present on the host and the k3s container.
	// Users should ensure that the host system is also using legacy iptables, you can generally do this with
	// `update-alternatives` or similar.
	felixConfig := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "crd.projectcalico.org/v1",
			"kind":       "FelixConfiguration",
			"metadata":   map[string]any{"name": "default"},
			"spec": map[string]any{
				"iptablesBackend": "Legacy",
			},
		},
	}
	objects := internal.DecodeYAML("yaml/k3d-calico/calico-cni.yaml")
	for _, obj := range objects {
		if dms, ok := obj.(*appsv1.DaemonSet); ok && dms.Name == "calico-node" {
			for i := range dms.Spec.Template.Spec.Containers {
				if dms.Spec.Template.Spec.Containers[i].Name == "calico-node" {
					dms.Spec.Template.Spec.Containers[i].Env = append(dms.Spec.Template.Spec.
						Containers[i].Env,
						corev1.EnvVar{
							Name:  "CALICO_IPV4POOL_CIDR",
							Value: opts.GetPodCIDR(),
						},
						corev1.EnvVar{
							// This setting allows
							// traffic from any external IP address.
							// We don't technically need to allow the whole
							// IP space,
							// but it's a little involved to get the set of
							// peering IPs to this function call.
							// This is only intended for integration tests,
							// or local development, so this should not be
							// a security concern.
							// We could also potentially add networkpolicies
							// to mitigate.
							Name:  "FELIX_EXTERNALNODESCIDRLIST",
							Value: "0.0.0.0/1,128.0.0.0/1",
						})
				}
			}
		}
	}
	return append(objects, felixConfig, bgpConfig)
}

// RegisterCalicoGVK is a helper method for adding calico objects returned
// by this package to the scheme.
func RegisterCalicoGVK(scheme *runtime.Scheme) {
	// We only register the kinds that we actually need here.
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{
		Group:   "crd.projectcalico.org",
		Version: "v1",
		Kind:    "BGPConfiguration",
	}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{
		Group:   "crd.projectcalico.org",
		Version: "v1",
		Kind:    "IPPool",
	}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{
		Group:   "crd.projectcalico.org",
		Version: "v1",
		Kind:    "BGPPeer",
	}, &unstructured.Unstructured{})
}

// K3dCalicoBGPPeeringOptions determines how to setup bpg peering among
// several k3d clusters.
type K3dCalicoBGPPeeringOptions struct {
	ClusterConfig map[string]K3dClusterBGPConfig
}

// K3dCalicoBGPPeeringObjects returns the objects needed for connecting
// k3d clusters via BGP.
func K3dCalicoBGPPeeringObjects(opts K3dCalicoBGPPeeringOptions) map[string][]kube.Object {
	// TODO(ye): right now, we configure ippool and bgppeers using
	//  Unstructured. We could potentially import caclio's library
	//  and use their go native CRD structs, once we upgrade our
	//  k8s library to >= 0.21.

	objectsByRegion := make(map[string][]kube.Object, len(opts.ClusterConfig))
	for region := range opts.ClusterConfig {
		var objects []kube.Object
		for otherRegion, otherCluster := range opts.ClusterConfig {
			if otherRegion == region {
				continue
			}
			serviceIPPool := map[string]any{
				"apiVersion": "crd.projectcalico.org/v1",
				"kind":       "IPPool",
				"metadata": map[string]any{
					"name": fmt.Sprintf("%s-services", otherRegion),
				},
				"spec": map[string]any{
					"cidr":        otherCluster.GetServiceCIDR(),
					"ipipMode":    "Always",
					"natOutgoing": false,
					"disabled":    true,
				},
			}
			podIPPool := map[string]any{
				"apiVersion": "crd.projectcalico.org/v1",
				"kind":       "IPPool",
				"metadata": map[string]any{
					"name": fmt.Sprintf("%s-pods", otherRegion),
				},
				"spec": map[string]any{
					"cidr":        otherCluster.GetPodCIDR(),
					"ipipMode":    "Always",
					"natOutgoing": false,
					"disabled":    true,
				},
			}
			objects = append(objects,
				&unstructured.Unstructured{Object: serviceIPPool},
				&unstructured.Unstructured{Object: podIPPool})

			for i, addr := range otherCluster.PeeringNodes {
				bgpPeer := map[string]any{
					"apiVersion": "crd.projectcalico.org/v1",
					"kind":       "BGPPeer",
					"metadata": map[string]any{
						"name": fmt.Sprintf("edge-peer-%s-%d", otherRegion, i),
					},
					"spec": map[string]any{
						"nodeSelector": "edge == 'true'",
						"peerIP":       addr,
						"asNumber":     otherCluster.GetASNumber(),
					},
				}
				objects = append(objects, &unstructured.Unstructured{Object: bgpPeer})
			}
		}
		objectsByRegion[region] = objects
	}
	return objectsByRegion
}

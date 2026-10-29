package cloud

import (
	"context"
	"fmt"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	corev1 "k8s.io/api/core/v1"
)

// Environment specifies details of the environment that is hosting a K8s
// cluster.
type Environment struct {
	// CloudProvider is the name of the public cloud provider that is hosting
	// the K8s cluster, if it is known (or cloud.Unknown if not).
	CloudProvider Provider

	// CloudRegion is the name of the region in which the K8s cluster is
	// running, as defined by the cloud provider (empty string can be used in
	// single region tests).
	CloudRegion string
}

// GeGetEnvironmentForNode returns information about the environment in which
// the node of the given name is running.
func GetEnvironmentForNode(
	ctx context.Context, ctl *kube.Ctl, nodeName string,
) (env Environment, err error) {
	var node corev1.Node
	if err = ctl.Get(ctx, nodeName, &node); err != nil {
		return Environment{}, errors.Wrapf(err, "getting node %s", nodeName)
	}
	env.CloudProvider = cloudProviderFromNodeStatus(node.Status)

	region, err := regionFromNodeLabels(node.Labels)
	if err != nil {
		// Note that it is deliberate to not return an error if the label could
		// not be found.
		logging.Warnf(ctx, "error getting region label for node %s", nodeName)
	}
	env.CloudRegion = region
	return env, nil
}

func cloudProviderFromNodeStatus(status corev1.NodeStatus) Provider {
	// Provider defaults to Unknown.
	provider := Unknown

	// Kubernetes version comes from either KubeletVersion or KubeProxyVersion.
	kubeletVersion := status.NodeInfo.KubeletVersion
	// Kernel version is necessary for detecting an Azure environment.
	kernelVersion := status.NodeInfo.KernelVersion
	// Can't check bottlerocket node through the kubeletVersion variable so we
	// extract osImage.
	osImage := status.NodeInfo.OSImage

	// k3d clusters running in cloud environments may include the cloud provider
	// in the kernel version; check the kubelet version for k3s first.
	if strings.Contains(kubeletVersion, "k3s1") {
		provider = K3D
	} else if strings.Contains(kubeletVersion, "gke") {
		provider = GCP
	} else if strings.Contains(kernelVersion, "azure") {
		provider = Azure
	} else if strings.Contains(osImage, "Bottlerocket") ||
		strings.Contains(kubeletVersion, "eks") {
		// New bottlerocket images will set the KubeletVersion and
		// KubeletProxyVersion to just the version string without any "eks"
		// information which can differentiate the cloud provider. So in
		// addition we check the osImage for mentions of "Bottlerocket".
		provider = AWS
	}
	return provider
}

func regionFromNodeLabels(labels map[string]string) (string, error) {
	return getFirstValue(labels, []string{
		corev1.LabelTopologyRegion,
		corev1.LabelFailureDomainBetaRegion, // deprecated
	})
}

func zoneFromNodeLabels(labels map[string]string) (string, error) {
	return getFirstValue(labels, []string{
		corev1.LabelTopologyZone,
		corev1.LabelFailureDomainBetaZone, // deprecated
	})
}

func getFirstValue(haystack map[string]string, needles []string) (string, error) {
	for _, needle := range needles {
		if value, ok := haystack[needle]; ok && value != "" {
			return value, nil
		}
	}
	return "", errors.New("value not found")
}

// NodeLocalityReaderFunc returns the locality information associated with the
// Kubernetes node with the given name.
type NodeLocalityReaderFunc func(ctx context.Context, nodeName string) (LocalityInfo, error)

// LocalityInfo refers to the locality information of a node.
type LocalityInfo struct {
	// Region refers to the name of the region that the node is in. This is
	// prefixed with the cloud provider's code (e.g. "gcp-us-east1").
	Region string
	// Zone refers to the name of the zone that the node is in. This is prefixed
	// with the cloud provider's code (e.g. gcp-us-east1-c).
	Zone string
}

// String returns the string representation of LocalityInfo, and will be used
// by the --locality or --locality-file flag.
//
//  1. This should be an ordered, comma-separated list of key-value pairs that
//     describe the topography of the machine.
//
//  2. The order of tiers is used to determine the priority of the diversity,
//     so the more inclusive localities like region should come before less
//     inclusive localities like availability zone.
//
// String implements the fmt.Stringer interface.
func (i LocalityInfo) String() string {
	return fmt.Sprintf("region=%s,az=%s", i.Region, i.Zone)
}

// MakeLocalityInfoForNode constructs a LocalityInfo object based on the given
// node. If the localities could not be read from such a node, the returned
// object will contain empty fields.
func MakeLocalityInfoForNode(ctx context.Context, node *corev1.Node) LocalityInfo {
	region, err := regionFromNodeLabels(node.Labels)
	if err != nil {
		logging.Warnf(ctx, "error getting region label for node %s", node.Name)
	}
	zone, err := zoneFromNodeLabels(node.Labels)
	if err != nil {
		logging.Warnf(ctx, "error getting zone label for node %s", node.Name)
	}
	provider := cloudProviderFromNodeStatus(node.Status)
	if provider != Unknown {
		if region != "" {
			region = fmt.Sprintf("%s-%s", provider, region)
		}
		if zone != "" {
			zone = fmt.Sprintf("%s-%s", provider, zone)
		}
	}
	return LocalityInfo{Region: region, Zone: zone}
}

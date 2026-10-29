package manifests

import (
	"fmt"
	"maps"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/hashutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// BetaCrdbClusterNodeOwnerLabelKey is used as a label key for CrdbNodes.
	// It is useful for selecting the nodes using the name of the cluster.
	BetaCrdbClusterNodeOwnerLabelKey = "crdb.cockroachlabs.com/cluster"
	// BetaCrdbClusterNodeRevisionAnnotationKey is used as an annotation key for
	// CrdbNodes.
	// It is useful for comparing cluster revisions.
	BetaCrdbClusterNodeRevisionAnnotationKey = "crdb.cockroachlabs.com/clusterNodeRevision"

	// BetaCrdbClusterNodeCloudProviderAnnotationKey is used to identify the cloud
	// provider for the cluster. This value is taken from the CloudProvider value
	// on the CrdbClusterRegion.
	//
	// This is useful for things like prefixing locality flags with the cloud provider
	// name.
	BetaCrdbClusterNodeCloudProviderAnnotationKey = "crdb.cockroachlabs.com/cloudProvider"

	// CrdbClusterNodeTopologyConstraintKey is the key used for setting CrdbNode's
	// topology constraint key.
	// This means that for now the topology constraint is hardcoded to use this value,
	// and users cannot modify it.
	CrdbClusterNodeTopologyConstraintKey = corev1.LabelTopologyZone

	// CrdbNodeFinalizerName is the name of the finalizer for CrdbNode. The
	// finalizer prevents deletion of a CrdbNode until the operator has cleaned
	// up both the Pod and PVC associated with the node.
	// NOTE: CrdbNodeFinalizerName is added automatically to CrdbNodes created
	// by the clusterctrl. Manually managed nodes will have to opt-in to using
	// the finalizer.
	CrdbNodeFinalizerName = "crdbnode.crdb.cockroachlabs.com/finalizer"

	AWSZoneIDAnnotationKey = "topology.k8s.aws/zone-id"
)

// BetaCrdbClusterNode returns the manifests of a crdb node.
// Name field will not be set, and instead GenerateName is set.
func BetaCrdbClusterNode(
	cluster *crdbv1beta1.CrdbCluster, region *crdbv1beta1.CrdbClusterRegion, nodeName string,
) *crdbv1beta1.CrdbNode {
	tpl := cluster.Spec.Template.DeepCopy()
	if tpl.Labels == nil {
		tpl.ObjectMeta.Labels = make(map[string]string, 1)
	}
	tpl.Labels[BetaCrdbClusterNodeOwnerLabelKey] = cluster.Name

	node := &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: cluster.Name + "-", // "-" for readability.
			Namespace:    cluster.Namespace,
			Labels:       tpl.Labels,
			Annotations:  tpl.Annotations,
		},
		Spec: tpl.Spec,
	}

	node.Spec.NodeName = nodeName
	// Populate TLSEnabled from the cluster spec so it's consistently available
	// throughout the node lifecycle and in manifests.
	node.Spec.TLSEnabled = cluster.Spec.TLSEnabled

	// If locality labels are not set, we set them to the default values.
	// This eliminates the need for cloud deployments to set locality labels.
	if len(node.Spec.LocalityLabels) == 0 {
		node.Spec.LocalityLabels = []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone}
	}
	if len(node.Spec.LocalityMappings) == 0 {
		setDefaultLocalityMappings(node, region)
	} else {
		applyLocalityPrefix(node, region)
	}

	// Zero out the Join member, it could accidentally be populated from the
	// node template on the CrdbClusterSpec. Our test fixtures currently set
	// .Join on the template by accident. For now, we're ignoring any input
	// values. In the future, it may be desirable to let the user set a value
	// here.
	node.Spec.Join = ""

	// For each region, generate a --join entry consisting of the service
	// discovery service. Kubernetes (or DNS because this is a headless
	// service) will route to a random Ready CrdbNode Pod. This ensures
	// that any Pods being connected to by the --join flag are already part
	// of an initialized cluster. See also
	// https://github.com/cockroachdb/cockroach/issues/103594.
	for i, region := range cluster.Spec.Regions {
		if i > 0 {
			node.Spec.Join += ","
		}

		// TODO(chrisseto): consider pulling the namespace determination logic into a
		// helper if we find it used anywhere else.

		// Namespace can be explicitly provided via .Namespace, if the cluster
		// isn't deployed into specially named Namespaces. Tests currently
		// utilize this capablity but CockroachCloud should converge on using
		// this field and .Domain in the future as Namespace base DNS chaining
		// is deprecated. NB: region.Namespace and region.Code will likely be
		// the same in production use cases but they are utilized for very
		// different purposes.
		namespace := region.Namespace

		if namespace == "" {
			// If region.Code is provided, it is used instead of cluster.Namespace.
			// This is the behavior that CockroachCloud currently utilizes because
			// we deploy into namespace named after the region's code to Namespace
			// based DNS chaining.
			// TODO(orchestration): Eventually deprecate this fallback as it's
			// not very intuitive outside of CC.
			namespace = region.Code
		}

		if namespace == "" || namespace == "placeholder" {
			// Namespace ultimately fallsback to cluster.Namespace. This case
			// is hit if this is a single region cluster, or if "placeholder"
			// is used as a sentinel value in OLM bundle deployments where the
			// namespace isn't known until deployment time.
			namespace = cluster.Namespace
		}

		node.Spec.Join += fmt.Sprintf("%s:%d", JoinFQDN(cluster.Name, namespace, region.Domain), node.Spec.GetGRPCPort())
	}

	if node.Annotations == nil {
		node.Annotations = make(map[string]string, 1)
	}

	if region != nil && region.CloudProvider != "" {
		node.Annotations[BetaCrdbClusterNodeCloudProviderAnnotationKey] = region.CloudProvider
	}

	if region != nil {
		node.Spec.Domain = region.Domain

		if region.EncryptionAtRest != nil {
			node.Spec.EncryptionAtRest = region.EncryptionAtRest
		}
	}

	// We don't want internal annotations or node specific state to cause the
	// hash to change.
	nodeCopy := *node
	nodeCopy.Annotations, _ = crdbv1beta1.CopyAndFilterInternalNodeAnnotations(node.Annotations)
	nodeCopy.Spec.NodeName = ""

	node.Annotations[BetaCrdbClusterNodeRevisionAnnotationKey] = hashutil.MustFNV(nodeCopy)

	return node
}

func setDefaultLocalityMappings(node *crdbv1beta1.CrdbNode, region *crdbv1beta1.CrdbClusterRegion) {
	var cloudProvider string
	if region != nil {
		cloudProvider = region.CloudProvider
	}

	var prefix string
	if !node.Spec.NoCloudPrefixedLocalities && cloudProvider != "" {
		prefix = fmt.Sprintf("%s-", cloudProvider)
	}
	node.Spec.LocalityMappings = []crdbv1beta1.LocalityMapping{
		{
			NodeLabel:     corev1.LabelTopologyRegion,
			LocalityLabel: "region",
			Prefix:        prefix,
		},
	}
	// Basalt-backed clusters need their zone value to exactly match the
	// zone value configured on the basalt-blob servers.
	if node.Spec.DataStore.IsBasalt() {
		label := corev1.LabelTopologyZone
		provider, _ := cloud.ParseProvider(cloudProvider)
		// AWS clusters should use "zone-id", which is stable across
		// accounts, unlike "zone".
		if provider == cloud.AWS {
			label = AWSZoneIDAnnotationKey
		}
		node.Spec.LocalityMappings = append(node.Spec.LocalityMappings, crdbv1beta1.LocalityMapping{
			NodeLabel:     label,
			LocalityLabel: "zone",
		})
	} else {
		node.Spec.LocalityMappings = append(node.Spec.LocalityMappings, crdbv1beta1.LocalityMapping{
			NodeLabel:     corev1.LabelTopologyZone,
			LocalityLabel: "zone",
			Prefix:        prefix,
		})
	}
}

// applyLocalityPrefix is used to backport a change from
// node.Spec.LocalityMappings that moved the default value for this field
// from kubebuilder (and consequently etcd) to code, while adding a prefix
// field to the locality mapping. Without this function,
// the incorrect locality flags would be applied to existing clusters.
func applyLocalityPrefix(node *crdbv1beta1.CrdbNode, region *crdbv1beta1.CrdbClusterRegion) {
	if node.Spec.NoCloudPrefixedLocalities || region == nil || region.CloudProvider == "" {
		return
	}
	prefix := fmt.Sprintf("%s-", region.CloudProvider)
	for i := range node.Spec.LocalityMappings {
		node.Spec.LocalityMappings[i].Prefix = prefix
	}
}

// UpdateBetaCrdbClusterNode updates the node in-place, based on the input cluster.
// After the function returns, node can be submitted to k8s server to update
// the node.
// If existingPVCSize is non-nil and larger than the new requests, node's storage requests will be
// set to existingPVCSize.
func UpdateBetaCrdbClusterNode(
	cluster *crdbv1beta1.CrdbCluster,
	node *crdbv1beta1.CrdbNode,
	existingPVCSize *resource.Quantity,
	k8sNode string,
) {
	tpl := BetaCrdbClusterNode(cluster, cluster.Region(), k8sNode)
	_, internalAnnotations := crdbv1beta1.CopyAndFilterInternalNodeAnnotations(node.Annotations)

	node.Labels = tpl.Labels
	node.Annotations = tpl.Annotations
	// Preserve internal annotations.
	maps.Copy(node.Annotations, internalAnnotations)
	node.Spec = tpl.Spec

	if existingPVCSize == nil {
		return
	}

	// If storage size decreases, we ignore the change: PVCs are not allowed
	// to shrink in size.
	// Check the integration test [TestShrinkDiskSize] for an example on how one can actually shrink the disk size.
	if pvc := node.Spec.DataStore.VolumeClaimTemplate; pvc != nil && pvc.Spec.Resources.Requests.Storage() != nil {
		if existingPVCSize.Cmp(*pvc.Spec.Resources.Requests.Storage()) > 0 {
			pvc.Spec.Resources.Requests[corev1.ResourceStorage] = *existingPVCSize
		}
	}
}

// GetClusterRevision computes the revision of the cluster.
// This is used for deciding whether CrdbNodes need to be updated.
// The revision is computed by hashing the name, namespace, labels, annotations,
// and the spec of the cluster.
func GetClusterRevision(cluster *crdbv1beta1.CrdbCluster) string {
	return BetaCrdbClusterNode(cluster, cluster.Region(), "").Annotations[BetaCrdbClusterNodeRevisionAnnotationKey]
}

// GetClusterName returns the cluster name of the node, by extracting it
// from the labels.
// The caller needs to make sure that the node is constructed properly
// by using manifests package, or has the correct label.
func GetClusterName(node *crdbv1beta1.CrdbNode) string {
	return node.Labels[BetaCrdbClusterNodeOwnerLabelKey]
}

// RegionJoinNamespace returns the effective Kubernetes namespace used for the
// join service of a region. It mirrors the namespace resolution order used
// when building the --join flag so that callers (e.g. cert generators) can
// produce SANs that match the actual join address.
func RegionJoinNamespace(clusterNamespace string, region crdbv1beta1.CrdbClusterRegion) string {
	ns := region.Namespace
	if ns == "" {
		ns = region.Code
	}
	if ns == "" || ns == "placeholder" {
		ns = clusterNamespace
	}
	return ns
}

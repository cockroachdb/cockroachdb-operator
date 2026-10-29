package v1beta1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// MakeCrdbClusterReady modifies the given cluster such that [CrdbCluster.IsReady]
// returns nil.
func MakeCrdbClusterReady(cluster *CrdbCluster) {
	cluster.Status.ObservedGeneration = cluster.Generation
	cluster.Status.SetCondition(ClusterInitialized, metav1.ConditionTrue)
	if cluster.Region() != nil {
		cluster.Status.ReadyNodes = cluster.Region().Nodes
	}
}

// MakeCrdbClusterQuiescent modifies the given cluster such that
// [CrdbCluster.IsQuiescent] returns nil.
func MakeCrdbClusterQuiescent(cluster *CrdbCluster) {
	MakeCrdbClusterReady(cluster)
}

// MakeCrdbNodeReady modifies the given node such that [CrdbNode.IsReady]
// returns nil.
func MakeCrdbNodeReady(node *CrdbNode) {
	node.Status.ObservedGeneration = node.Generation
	node.Status.SetCondition(PodNeedsUpdate, metav1.ConditionFalse)
	node.Status.SetCondition(PodReady, metav1.ConditionTrue)
	node.Status.SetCondition(PodRunning, metav1.ConditionTrue)
}

// MakeCrdbTenantReady modifies the given tenant such that [CrdbTenant.IsReady]
// returns nil.
func MakeCrdbTenantReady(tenant *CrdbTenant) {
	tenant.Status.Pods = tenant.Spec.Pods
	tenant.Status.SetCondition(TenantInitialized, metav1.ConditionTrue)
}

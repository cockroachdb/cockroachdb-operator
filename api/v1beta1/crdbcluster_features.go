package v1beta1

// ClusterFeature is an enumeration of available features for the operator.
// These can be used to enable/disable certain features such as which objects
// should be reconciled by the operator (as opposed to intrusion).
type ClusterFeature string

const (
	// ClusterSqlProxyDisableConnectionRebalancing disables the connection
	// rebalancing feature within sqlproxy. This will only apply to host
	// clusters with sqlproxy v22.1+ images.
	ClusterSqlProxyDisableConnectionRebalancing ClusterFeature = "disable-sqlproxy-connection-rebalancing"

	// ClusterExportCustomerLogs enables log export to a second
	// Fluentbit port that's used for sending logs to customer cloud
	// sinks.
	ClusterExportCustomerLogs ClusterFeature = "export-customer-logs"

	// ClusterSqlProxyProxyProtocol ensures that the sqlproxy instances require
	// the proxy protocol for their SQL listeners. This should only be turned on
	// for *new* v23.1+ AWS host clusters (that have the AWS load balancer
	// controller installed).
	ClusterSqlProxyProxyProtocol ClusterFeature = "sqlproxy-proxy-protocol"

	// ClusterSqlProxyProxyProtocolListenAddrEnabled controls whether the proxy protocol
	// listen addr flag is passed into SQLProxy.
	ClusterSqlProxyProxyProtocolListenAddrEnabled ClusterFeature = "sqlproxy-proxy-protocol-listen-addr-enabled"

	// ClusterExplicitNodePlacement changes how CrdbNodes are mapped to k8s
	// nodes. Instead of allowing the scheduler to pick the k8s node, the
	// cluster reconciler picks the k8s node. This allows the CrdbCluster to
	// know the specific properties of the k8s node and adjust parameters like
	// cpu request and limit.
	ClusterExplicitNodePlacement ClusterFeature = "explicit-node-placement"

	// ClusterDisableGCPLiveMigrations tells the operator to disable
	// live migrations on nodes in clusters running on GCP. Instead,
	// nodes will be terminated and rebooted if they need
	// maintenance. This feature is safe to enable for clouds other
	// than GCP - the operator will treat it as a no-op.
	ClusterDisableGCPLiveMigrations ClusterFeature = "disable-gcp-live-migrations"

	// ClusterTenantCreateAdhocPods allows the CrdbTenant reconciler
	// to create adhoc pods outside of the warm pool in two cases:
	// during a stale rollover, and when the warm pool is
	// exhausted. With this feature enables, we are able to perform
	// maintenance and recovery operations more quickly, as we don't
	// need to worry about exhausting the warm pool.
	ClusterTenantCreateAdhocPods ClusterFeature = "tenant-create-adhoc-pods"

	// ClusterCpuAwareTenantPodStamping enables the operator to use stamped pods
	// based on the latest algorithm.
	ClusterCpuAwareTenantPodStamping ClusterFeature = "cpu-aware-tenant-pod-stamping"

	// ClusterHotNodeAutoRemediation enables the operator to scan for
	// hot k8s nodes (nodes whose CPU usage exceeds a threshold) and to
	// mark the highest CPU usage pod on that node as stale so
	// it can be evicted from the hot node. On marking it as stale, the operator
	// will also stamp a pod from the warm pool so there's minimal disruption.
	ClusterHotNodeAutoRemediation ClusterFeature = "hot-node-auto-remediation"

	// ClusterHotNodeAwareTenantPodStamping enables the operator to filter
	// warm pods on nodes with high CPU usage when stamping new tenant pods,
	// preventing nodes with high CPU from being overloaded.
	ClusterHotNodeAwareTenantPodStamping ClusterFeature = "hot-node-aware-tenant-pod-stamping"

	// ClusterDrainAfterDecommission enables draining after the
	// decommission step during scale down, but before decommissioning
	// is finalized. This allows a greater level of parallelism when
	// scaling down, as leases will be removed from a node before it is drained.
	ClusterDrainAfterDecommission ClusterFeature = "drain-after-decommission"

	// ClusterSkipUnderReplicatedRangesCheck bypasses the under-replicated ranges
	// gate that blocks rolling restarts and scale-down while ranges are
	// under-replicated. When this feature is enabled on the CR, both SH and CC
	// clusters skip the check. It exists so we can force changes through a cluster
	// whose nodes are already failing, for example to mitigate a CRDB bug or
	// escalation by forcing an upgrade, where the gate would otherwise block the
	// very upgrade that recovers the cluster.
	ClusterSkipUnderReplicatedRangesCheck ClusterFeature = "skip-under-replicated-ranges-check"
)

// AllClusterFeatures contains all defined ClusterFeature constants.
var AllClusterFeatures = []ClusterFeature{
	ClusterSqlProxyDisableConnectionRebalancing,
	ClusterExportCustomerLogs,
	ClusterSqlProxyProxyProtocol,
	ClusterSqlProxyProxyProtocolListenAddrEnabled,
	ClusterExplicitNodePlacement,
	ClusterDisableGCPLiveMigrations,
	ClusterTenantCreateAdhocPods,
	ClusterCpuAwareTenantPodStamping,
	ClusterHotNodeAutoRemediation,
	ClusterHotNodeAwareTenantPodStamping,
	ClusterDrainAfterDecommission,
	ClusterSkipUnderReplicatedRangesCheck,
}

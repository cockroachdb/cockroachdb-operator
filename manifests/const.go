package manifests

import v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	CockroachDB          = "cockroachdb"
	CockroachProcessName = "cockroach"

	// CockroachProxyProtocolPort is the secondary port SQLProxy listens for
	// SQL connections on, where use of the proxy protocol is required.
	CockroachProxyProtocolPort = 26259

	CockroachCertsPath = "/cockroach/cockroach-certs"

	// BasaltCertsPath is where the basalt tenant leaf certificate, its key, and
	// the basalt server CA are projected. This is a separate directory from the
	// CockroachDB certs because both CAs are named ca.crt and would collide.
	BasaltCertsPath = "/cockroach/basalt-certs"

	// InternalLoadBalancerService is the name of the load balancer used for
	// internal applications to connect to the CRDB cluster (SQLProber, SQL
	// Pods, etc).
	InternalLoadBalancerService = "cockroachdb-internal"

	// CockroachPodFormat is the format for a podName, where d is the podOrdinal.
	CockroachPodFormat = CockroachDB + "-%d"

	// Priority class values are arbitrary, but should be less than
	// 2,000,000,000, which is the `system-cluster-critical` priority
	// class value.
	CRDBCritical = "crdb-critical"
	CRDBHigh     = "crdb-high"
	CRDBTenant   = "crdb-tenant"

	// CriticalPriority is the highest priority that CC pods may run at and is
	// reserved for SQL facilitating Pods. This includes SQLProxy, the
	// Cockroach-Operator, and CockroachDB Pods. The Priority values should be
	// shared across manifests to keep them aligned but orthogonal manifests
	// should create their own PriorityClass object to use.
	CriticalPriority = 500000000

	// HighPriority is the second highest priority that CC pods may run at. It
	// is used for high impact services that are not CockroachDB itself. For
	// example, Prometheus, and Alertmanager, run at this level. The Priority
	// values should be shared across manifests to keep them aligned but
	// orthogonal manifests should create their own PriorityClass object to
	// use.
	HighPriority = 400000000

	// TenantPriority is the priority that tenant pods run at. It is lower than
	// KV pods (which run at CRDBCritical) so that k8s prioritizes the evictions
	// of tenant pods before KV pods (assuming all else is equal).
	TenantPriority = 1000

	KubeSystem  = v1.NamespaceSystem
	KubeDefault = v1.NamespaceDefault

	// K8sNamespaceEnvVar is the name of the environment variable used to pass
	// the name of the K8s namespace in which a K8s container is created. This
	// is useful for containers that need to manage namespaced resources via the
	// K8s API service.
	K8sNamespaceEnvVar = "K8S_NAMESPACE"
	// CockroachChannelEnvVar identifies the CockroachDB distribution channel for telemetry.
	CockroachChannelEnvVar = "COCKROACH_CHANNEL"

	// EgressNetworkPolicyName is the name of the CiliumNetworkPolicy resource used
	// to enforce egress Rules.
	EgressNetworkPolicyName = "cc-cockroachdb-egress"
)

// SelfSignedCAConfigMapName returns the name of the ConfigMap that stores the
// CA certificate for a cluster when selfSigned cert generation is used.
func SelfSignedCAConfigMapName(clusterName string) string {
	return clusterName + "-cockroachdb-ca"
}

// SelfSignedNodeCertSecretName returns the name of the Secret that stores the
// node TLS certificate for a cluster when selfSigned cert generation is used.
func SelfSignedNodeCertSecretName(clusterName string) string {
	return clusterName + "-cockroachdb-node-certs"
}

// SelfSignedRootClientSecretName returns the name of the Secret that stores the
// root SQL client certificate for a cluster when selfSigned cert generation is used.
func SelfSignedRootClientSecretName(clusterName string) string {
	return clusterName + "-cockroachdb-root-certs"
}

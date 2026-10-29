// For constants that don't vary by env or customer but are used widely in the
// codebase (e.g. in pulumi code and in GCP firewall code).
package constants

import "os"

const (
	CockroachNodeTag    = "cockroachdb-node"
	FirewallAdminRoleID = "firewallAdmin"
	AWS                 = "aws"
	GCP                 = "gcp"
	Azure               = "azure"
	K3D                 = "k3d"
	Fake                = "fake"

	FluentbitRedactedPort   = 5170
	FluentbitUnredactedPort = 5171

	// OTel collector ports for log ingestion (used during FluentBit to OTel migration).
	OtelRedactedPort   = 5172
	OtelUnredactedPort = 5173

	// Our sidecar image that executes a command whenever file changes are detected.
	// Generally, useful for trigging configuration or certificate reloads.

	// Our prometheus sidecar image that execute a command that will run checks
	// on Prometheus alerting & recording rules to detect potential problems with those rules.
	PintImage = "ghcr.io/cloudflare/pint:0.45.0"

	// LowTrustTenantPool is the name of the tenantpool that's used by clusters
	// owned by organizations that are less trusted.
	LowTrustTenantPool = "low-trust"
	// HighTrustTenantPool is the name of the tenantpool that's used by clusters
	// owned by organizations that are more trusted.
	HighTrustTenantPool = "high-trust"

	// Format for crdb workload role or service account name.
	CockroachWorkloadRoleFormat = "crdb-wi-%s"

	// Format for operator service account name.
	OperatorWorkloadRoleFormat = "operator-wi-%s"

	// Format for fluent-bit workload role or service account name.
	FluentbitWorkloadRoleFormat = "fluent-bit-wi-%s"

	// Format for in-situ workload role or service account name.
	InSituWorkloadRoleFormat = "in-situ-wi-%s"

	// Format for KMS functionality-level role or service account name.
	KMSFunctionalityRoleFormat = "crl-kms-user-%s"

	// Format for DR (Disaster Recovery) store functionality-level role or
	// service account name.
	DRStoreFunctionalityRoleFormat = "crl-dr-store-user-%s"

	// Format for CDC-sink functionality-level role or service account name.
	CDCSinkFunctionalityRoleFormat = "crl-cdc-sink-user-%s"

	// Key for k8s service account annotation related to workload identity bindings.
	WorkloadIdentitySAAnnotationKey = "iam.gke.io/gcp-service-account"
	// Format for k8s service account annotation related to workload identity bindings.
	WorkloadIdentitySAAnnotationFormat = "%s@%s.iam.gserviceaccount.com"

	CRLAppLabelKey   = "crdb.io/app"
	CRLAppLabelValue = "crdb"

	// The kube-system namespace is reserved by Kubernetes.
	KubeSystemNamespace = "kube-system"

	// GKEManagedSystemNamespace is a reserved namespace by GKE.
	GKEManagedSystemNamespace = "gke-managed-system"

	// GCPKeychainServiceAccountName is a reserved service account name for
	// log export. The operator does not use it directly. The service account
	// name has to be between 6-30 characters.
	// Please exercise caution when changing this value.
	GCPKeychainServiceAccountName = "CRLKeychainServiceAccount"

	// IngressNetworkPolicyName is the name of the CiliumNetworkPolicy resource used
	// to enforce ingress Rules.
	IngressNetworkPolicyName = "cc-cockroachdb-ingress"

	// InternalIngressNetworkPolicyName is the name of the CiliumNetworkPolicy resource used
	// to allow internal cluster ingress traffic.
	InternalIngressNetworkPolicyName = "cockroachdb-internal-ingress"
)

// GetInitContainerImage returns the init container image from environment variable
// RELATED_IMAGE_INIT_CONTAINER, or falls back to the default constant.
// This allows Red Hat OpenShift deployments to override the image at runtime.
func GetInitContainerImage() string {
	if img := os.Getenv("RELATED_IMAGE_INIT_CONTAINER"); img != "" {
		return img
	}
	return defaultInitContainerImage
}

// GetReloaderImage returns the reloader/inotifywait image from environment variable
// RELATED_IMAGE_INOTIFYWAIT, or falls back to the default constant.
// This allows Red Hat OpenShift deployments to override the image at runtime.
func GetReloaderImage() string {
	if img := os.Getenv("RELATED_IMAGE_INOTIFYWAIT"); img != "" {
		return img
	}
	return defaultReloaderImage
}

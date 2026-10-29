package manifests

import (
	"fmt"
	"strings"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/crypto"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// CockroachNodeCertSecretName is the name of the K8s secret where the CRDB
	// "node" certificate is stored. The node certificate is used to
	// authenticate CRDB nodes as they connect with one another. It is also used
	// as a TLS Server certificate when other actors want to talk directly to a
	// CRDB node (e.g. an external SQL connection or the internal pod dialer).
	CockroachNodeCertSecretName = "node-certs"
	CockroachNodeCertName       = "node"

	// CockroachClientCertSecretName is the name of the K8s secret where the
	// CRDB "root" client certificate is stored. The root client certificate is
	// used to authenticate a privileged root user that has superuser SQL
	// access. It is used to perform administrative actions on CRDB clusters.
	CockroachClientCertSecretName = "client-certs"
	CockroachClientCertName       = "client.root"

	// CockroachUICertSecretName is the name of the K8s secret where the CRDB
	// "ui" certificate is stored. The UI certificate is used as a TLS
	// certificate for the CRDB HTTP API used by the DB Console and other
	// callers.
	// TODO(andyk): This is currently used as both the UI and SQLProxy
	// certificate in some tests, but this does not match Staging/Production
	// usage, which can have different certs and even different CAs for those
	// use cases.
	CockroachUICertSecretName = "ui-certs"
	CockroachUICertName       = "ui"

	// SqlProxyExternalCertSecretName is the name of the K8s secret where the
	// SQLProxy TLS certificate is stored. The SQL proxy certificate is used
	// to establish secure TLS connections to a CRDB multi-tenant host cluster
	// from outside the cluster using SQL.
	SqlProxyExternalCertSecretName = "sql-proxy-external-certs"
	SqlProxyExternalCertName       = "sql-proxy-external"

	// MoltAICertSecretName is the name of the K8s secret where the MOLT-AI
	// nginx TLS certificate is stored. The MOLT-AI deployment runs an nginx
	// sidecar that terminates TLS on the per-cluster MOLT-AI hostname so
	// customer browsers can reach the migration assistant UI over HTTPS.
	MoltAICertSecretName = "molt-ai-tls-certs"

	// SqlStarterCertSecretName is the name of the K8s secret where the
	// sqlstarter service's TLS certificate is stored. This is used to encrypt
	// the connection between the operator and tenant pool pods.
	SqlStarterCertSecretName = "sqlstarter-certs"
	SqlStarterCertName       = "sqlstarter"

	// TenantDirCertSecretName is the name of the K8s secret mounted into the
	// tenantdir. It is used to authenticate the tenantdir with the operator.
	TenantDirCertSecretName = "tenantdir-certs"
	TenantDirCertName       = "tls"

	// OperatorCertSecretName is the name of the K8s secret where the operator's
	// TLS certificate is stored. This must match the secret name in
	// cockroach-operator.yaml.
	OperatorCertSecretName = "cockroach-operator-certs"
	OperatorCertName       = "tls"

	// CockroachClientCAConfigMapName is the name of the ConfigMap that holds
	// the CA cert for client authentication within the ca.crt key.
	//
	// See ExternalCertificates.ClientCAConfigMapName for more details.
	CockroachClientCAConfigMapName = "cockroach-client-ca"

	// CockroacNodeCAConfigMapName is the name of the ConfigMap that holds
	// the CA cert for node authentication within the ca.crt key.
	//
	// See ExternalCertificates.NodeCAConfigmapName for more details.
	CockroachNodeCAConfigMapName = "cockroach-node-ca"

	// CockroachHTTPSecretName is the name of the kubernetes.io/tls Secret
	// that will be used for serving HTTPS traffic from CockroachDB.
	//
	// See ExternalCertificates.HTTPSecretName for more details.
	CockroachHTTPSecretName = "cockroach-http-certs"

	// CockroachNodeClientSecretName is the name of the kubernetes.io/tls Secret that
	// will be used for connecting to other CockroachDB nodes and select
	// `cockroach` commands.
	//
	// See ExternalCertificates.NodeClientSecretName for more details.
	CockroachNodeClientSecretName = "cockroach-node-client-certs"

	// CockroachNodeSecretName is the name of the kubernetes.io/tls Secret that
	// will be used for receiving connections from other CockroachDB nodes.
	//
	// See ExternalCertificates.NodeSecretName for more details.
	CockroachNodeSecretName = "cockroach-node-certs"

	// CockroachRootSQLClientSecretName is the name of the kubernetes.io/tls
	// Secret that will be used for connecting to Cockroach as the root SQL
	// user.
	//
	// See ExternalCertificates.RootSQLClientSecretName for more details.
	CockroachRootSQLClientSecretName = "cockroach-sql-certs"
)

// CockroachOperatorCertConfig describes the K8s secret that stores the TLS
// certificate that secures communication with the Cockroach operator.
// certificates that secure communication to and within a CRDB cluster. These
// secrets must then be installed in the same namespace as the CRDB cluster.
func CockroachOperatorCertConfig() *crypto.SecretConfig {
	return &crypto.SecretConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: OperatorCertSecretName,
		},
		Certs: []crypto.CertificateConfig{{
			FileName: OperatorCertName,
			SigningRequest: crypto.CertificateSigningRequest{
				Role:       "https",
				CommonName: "cockroach-webhook-service.default.svc",
				DNSNames: []string{
					"cockroach-webhook-service.default.svc",
					"cockroach-operator.default.svc",
				},
			},
		}},
	}
}

var RootClientCert = crypto.CertificateSigningRequest{
	Role:       "client",
	CommonName: utils.RootSQLUser,
}

// CrdbClientCA returns a ConfigMap containing the provided CA under the
// ca.crt key as is recommended by kubernetes to distribute CA certs.
// https://kubernetes.io/docs/tasks/tls/managing-tls-in-a-cluster/#trusting-tls-in-a-cluster
//
// See ExternalCertificates.ClientCAConfigMapName for more information.
func CrdbClientCA(ca []byte) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: CockroachClientCAConfigMapName,
		},
		Data: map[string]string{
			"ca.crt": string(ca),
		},
	}
}

// CrdbNodeCA returns a ConfigMap containing the provided CA under the
// ca.crt key as is recommended by kubernetes to distribute CA certs.
// https://kubernetes.io/docs/tasks/tls/managing-tls-in-a-cluster/#trusting-tls-in-a-cluster
//
// See ExternalCertificates.NodeCAConfigmapName for more information.
func CrdbNodeCA(ca []byte) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: CockroachNodeCAConfigMapName,
		},
		Data: map[string]string{
			"ca.crt": string(ca),
		},
	}
}

// CrdbClusterStsCertConfigs returns the crypto.SecretConfigs required to run a
// StatefulSet and CRD based CockroachDB cluster. Once StatefulSet clusters are
// phased out, CrdbClusterCertConfigs will return [kube.Tombstone]s for
// StatefulSet exclusive secrets.
func CrdbClusterCertConfigs(
	namespace string,
	deprecatedAlternateClusterDomain string,
	alternateClusterDomain string,
	regionalCert *crypto.Bundle,
	altNodeNames []string,
	isCustomerOwnedK8sCluster bool,
) []*crypto.SecretConfig {
	secretConfig := []*crypto.SecretConfig{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: CockroachUICertSecretName,
			},
			Certs: []crypto.CertificateConfig{{
				IncludePEM:     true,
				FileName:       CockroachUICertName,
				ExternalBundle: regionalCert,
			}},
		},
		rootClientCertConfig(),
		nodeCertConfig(
			namespace,
			deprecatedAlternateClusterDomain,
			alternateClusterDomain,
			altNodeNames,
		),
		// NB: All secrets below this line are Secrets for operator managed
		// clusters and are in the kubernetes.io/tls format.
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: CockroachHTTPSecretName,
			},
			ExternalBundle: regionalCert,
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: CockroachNodeSecretName,
			},
			SigningRequest: nodeCertificateSigningRequest(
				namespace,
				deprecatedAlternateClusterDomain,
				alternateClusterDomain,
				altNodeNames,
			),
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: CockroachNodeClientSecretName,
			},
			// NB: Despite being a client certificate, this cert must use the
			// 'node' role in order to use 'node' as a common name. If we ever
			// split SQL (client) and Node CAs, this certificate should use the
			// Node's CA as it is the client certificate for connecting to the
			// RPC service.
			SigningRequest: crypto.CertificateSigningRequest{
				Role:       "node",
				CommonName: "node",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: CockroachRootSQLClientSecretName,
			},
			SigningRequest: RootClientCert,
		},
	}

	return secretConfig
}

// nodeCertConfig describes the K8s secret where the CRDB "node" certificate is
// stored. The node certificate is used to authenticate CRDB nodes as they
// connect with one another. It is also used as a TLS Server certificate when
// other actors want to talk directly to a CRDB node (e.g. an external SQL
// connection or the internal pod dialer).
func nodeCertConfig(
	namespace, deprecatedAlternateClusterDomain, alternateClusterDomain string,
	additionalDnsNames []string,
) *crypto.SecretConfig {
	return &crypto.SecretConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: CockroachNodeCertSecretName,
		},
		Certs: []crypto.CertificateConfig{{
			FileName: CockroachNodeCertName,
			SigningRequest: nodeCertificateSigningRequest(
				namespace,
				deprecatedAlternateClusterDomain,
				alternateClusterDomain,
				additionalDnsNames,
			),
		}, {
			// Include client certs for debugging from the node and kube exec
			// automation.
			FileName:       CockroachClientCertName,
			SigningRequest: RootClientCert,
		}},
	}
}

func nodeCertificateSigningRequest(
	namespace, deprecatedAlternateClusterDomain, alternateClusterDomain string,
	additionalDnsNames []string,
) crypto.CertificateSigningRequest {
	return crypto.CertificateSigningRequest{
		Role:        "node",
		CommonName:  "node",
		IPAddresses: []string{"127.0.0.1"},
		DNSNames: append(
			// External hostnames provided by the provisioner.
			additionalDnsNames,
			// Internal hostname names.
			InternalLoadBalancerService,
			fmt.Sprintf("%s.%s", InternalLoadBalancerService, namespace),
			fmt.Sprintf("%s.%s.svc.cluster.local", InternalLoadBalancerService, namespace),
			fmt.Sprintf("%s.%s.svc.%s", InternalLoadBalancerService, namespace, deprecatedAlternateClusterDomain),
			fmt.Sprintf("%s.%s.svc.%s", InternalLoadBalancerService, namespace, alternateClusterDomain),
			// The "join" service, see [JoinFQDN] for details. NB: This is a
			// bit of a hack, we can safely assume that clusters are always
			// named "cockroach" because we run only create a single
			// CrdbCluster per K8s cluster.
			JoinFQDN("cockroachdb", namespace, "cluster.local"),
			JoinFQDN("cockroachdb", namespace, deprecatedAlternateClusterDomain),
			JoinFQDN("cockroachdb", namespace, alternateClusterDomain),
			// DNS names used for node to node communication!
			// Wildcarded since all nodes in the region use the same cert.
			"*.cockroachdb",
			fmt.Sprintf("*.cockroachdb.%s", namespace),
			fmt.Sprintf("*.cockroachdb.%s.svc.cluster.local", namespace),
			// DNS names for cluster domain based chaining.
			fmt.Sprintf("*.cockroachdb.%s.svc.%s", namespace, deprecatedAlternateClusterDomain),
			fmt.Sprintf("*.cockroachdb.%s.svc.%s", namespace, alternateClusterDomain),
			// DNS names for debugging!
			"localhost",
		),
	}
}

// rootClientCertConfig is a client cert that may be used to communicate with
// the CRDB cluster as the "root" SQL role.
func rootClientCertConfig() *crypto.SecretConfig {
	return &crypto.SecretConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: CockroachClientCertSecretName,
		},
		Certs: []crypto.CertificateConfig{{
			FileName:       CockroachClientCertName,
			SigningRequest: RootClientCert,
		}},
	}
}

// SqlStarterCertConfig describes K8s secret where the sqlstarter service's TLS
// certificate is stored. This is used to encrypt the connection between the
// operator and tenant pool pods.
func SqlStarterCertConfig() *crypto.SecretConfig {
	return &crypto.SecretConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: SqlStarterCertSecretName,
		},
		Certs: []crypto.CertificateConfig{{
			FileName: SqlStarterCertName,
			SigningRequest: crypto.CertificateSigningRequest{
				Role:       "https",
				CommonName: SqlStarterServerName,
				DNSNames:   []string{SqlStarterServerName},
			},
		}},
	}
}

// TenantDirCertConfig is used by the tenantdir to authenticate via mTLS with
// the operator RPC server.
func TenantDirCertConfig() *crypto.SecretConfig {
	return &crypto.SecretConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: TenantDirCertSecretName,
		},
		Certs: []crypto.CertificateConfig{{
			FileName: TenantDirCertName,
			SigningRequest: crypto.CertificateSigningRequest{
				Role:       "https",
				CommonName: "tenantdir",
				URINames:   []string{"crdb://service/tenantdir"},
			},
		}},
	}
}

// SqlProxyCertConfig describes SQLProxy's TLS secret on the inbound side.
// The SQL proxy certificate is used to prove the identity of the SQL proxy
// to clients making secure TLS connections to a CRDB multi-tenant host cluster
// from outside the cluster using SQL.
func SqlProxyCertConfig(serverlessDNS string) (*crypto.SecretConfig, error) {
	if serverlessDNS == "" {
		return nil, errors.Newf("proxy cert config requires non-blank region serverless dns")
	}
	parts := strings.SplitN(serverlessDNS, ".", 3)
	if len(parts) != 3 {
		return nil, errors.Newf("unable to split serverless dns name %q into cluster, region and root", serverlessDNS)
	}
	cluster, regionName, root := parts[0], parts[1], parts[2]

	return &crypto.SecretConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: SqlProxyExternalCertSecretName,
		},
		Certs: []crypto.CertificateConfig{{
			FileName: SqlProxyExternalCertName,
			SigningRequest: crypto.CertificateSigningRequest{
				// For a serverlessDNS of abc.us-east1.example.com, these are
				// *.abc.example.com, *.abc.us-east1.example.com,
				// *.internal-abc.us-east1.example.com, *.us-east1.example.com and
				// *.example.com.
				DNSNames: []string{
					// Needed for the global tenant cluster names
					strings.Join([]string{"*", cluster, root}, "."),
					strings.Join([]string{"*", cluster, regionName, root}, "."),
					// Needed for internal DNS used for private endpoints.
					strings.Join([]string{"*", "internal-" + cluster, regionName, root}, "."),
					// from region cert
					strings.Join([]string{"*", regionName, root}, "."),
					// from region cert
					strings.Join([]string{"*", root}, "."),
				},
				NeedsPublicCA: true,
			},
		}},
	}, nil
}

// UICertConfig describes the K8s secret where the CRDB "ui" certificate is
// stored. The UI certificate is used as a TLS certificate for the CRDB HTTP API
// used by the DB Console and other callers.
//
// TODO(andyk): This is currently used as both the UI and SQLProxy certificate
// in some tests, but this does not match Staging/Production usage, which can
// have different certs and even different CAs for those use cases.
func UICertConfig(regionCert *crypto.Bundle) *crypto.SecretConfig {
	return &crypto.SecretConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: CockroachUICertSecretName,
		},
		Certs: []crypto.CertificateConfig{{
			IncludePEM:     true,
			FileName:       CockroachUICertName,
			ExternalBundle: regionCert,
		}},
	}
}

// MoltAICertConfig describes the K8s secret holding the per-MOLT-AI nginx TLS
// cert. dnsName is the MOLT-AI's stable per-cluster hostname and is the only
// name that goes in the cert. The cert is issued and rotated by ACME (Let's
// Encrypt) via the caller's cert pipeline. The operator never signs it
// directly.
//
// Uses the top-level SigningRequest path so ToK8SSecret produces a standard
// kubernetes.io/tls Secret with tls.crt and tls.key keys, which is the
// format the nginx sidecar expects.
func MoltAICertConfig(dnsName string) (*crypto.SecretConfig, error) {
	if dnsName == "" {
		return nil, errors.Newf("molt-ai cert config requires non-blank dns name")
	}
	return &crypto.SecretConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: MoltAICertSecretName,
		},
		SigningRequest: crypto.CertificateSigningRequest{
			CommonName:    dnsName,
			DNSNames:      []string{dnsName},
			NeedsPublicCA: true,
		},
	}, nil
}

// MakeTenantClientCertSecretName returns the name of the K8s secret that
// contains the client certificate for the given tenant.
func MakeTenantClientCertSecretName(tenantID crdbv1beta1.TenantID) string {
	return fmt.Sprintf("tenant-client-%s-certs", tenantID.String())
}

// MakeTenantClientCertName returns the name of the client certificate for the
// given tenant. This value must match the one defined in the database. See:
// https://github.com/cockroachdb/cockroach/blob/124a8f41/pkg/security/certificate_manager.go#L399-L402.
func MakeTenantClientCertName(tenantID crdbv1beta1.TenantID) string {
	return fmt.Sprintf("client-tenant.%d", tenantID)
}

// MakeTenantSigningCertName returns the name of the signing certificate for the
// given tenant. This value must match the one defined in the database. See:
// https://github.com/cockroachdb/cockroach/blob/124a8f41/pkg/security/certificate_manager.go#L419-L422.
func MakeTenantSigningCertName(tenantID crdbv1beta1.TenantID) string {
	return fmt.Sprintf("tenant-signing.%d", tenantID)
}

// TenantCertConfig describes the secret mounted by tenant sql servers. The
// tenant certificate is used to authenticate to the KV layer as a particular
// tenant. The KV layer will only allow access to that tenant's keyspace. This
// config also includes a tenant signing certificate which will only be used to
// make digital signatures (e.g. in the case of a session revival token for
// connection migration).
func TenantCertConfig(
	tenantID crdbv1beta1.TenantID, namespace, alternateClusterDomain string,
) *crypto.SecretConfig {
	return &crypto.SecretConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: MakeTenantClientCertSecretName(tenantID),
			Labels: map[string]string{
				// TenantIDLLabel is here for human operator consistency. It
				// makes it easier to find resources belonging to a specific
				// tenant.
				TenantIDLabel: tenantID.String(),
			},
		},
		Certs: []crypto.CertificateConfig{
			{
				FileName: MakeTenantClientCertName(tenantID),
				SigningRequest: crypto.CertificateSigningRequest{
					Role:               "tenant",
					OrganizationalUnit: []string{"Tenants"},
					CommonName:         tenantID.String(),
					// TODO(chrisseto): Experiment with the need of ::. It's possible that we
					// don't need it. It was originally added to get TLS hostname verification
					// working but I foolishly did not add a node about why :: was added in.
					// It's being left in to preserve velocity for the release as it's unlucky
					// to cause issues or vulnerablities but we should note why it's present or
					// remove it if not needed.
					//
					// Include loopback addresses to allow tenants to connect to
					// themselves.
					IPAddresses: []string{"127.0.0.1", "::1", "::"},
					// Tenant Pod advertise their address as
					// <pod IP s/./-/>.<namespace>.pod.cluster.local. We can't provide
					// additional namespacing or guess IPs ahead of time so we use a
					// wildcard for the whole namespace. In the future, we may want to
					// consider using a different CA per tenant to prevent certificate
					// re-use but our network policies and certificates are good enough for
					// now.
					//
					// TODO(jaylim-crl): Update the advertised address by tenant
					// pods to use the cluster domain DNS name once all updated
					// certs have been rolled out.
					DNSNames: []string{
						tenantID.Tag(),
						"localhost",
						fmt.Sprintf("*.%s.pod.cluster.local", namespace),
						// DNS name used for cluster domain based chaining.
						fmt.Sprintf("*.%s.pod.%s", namespace, alternateClusterDomain),
					},
				},
			},
			// For now, store tenant-signing certs in the same secret as the
			// tenant node/client certs. Once we start using TLS-based secrets,
			// we'll need to split this into its own secret.
			{
				FileName: MakeTenantSigningCertName(tenantID),
				SigningRequest: crypto.CertificateSigningRequest{
					Role: "tenant-signing",
					// Use a tag here (e.g. tenant-X), so the signing policy's
					// CommonName field can be scoped to "tenant-*" instead of
					// just "*", which means anything. It's unfortunate that
					// we are embedding signing-policy reasoning into the operator,
					// but this is already a problem elsewhere. If we wanted
					// this to be consistent with the tenant node certs, we
					// should make this just the tenant ID by itself.
					CommonName: tenantID.Tag(),
				},
			},
			// The client.root certificate is installed in the tenant to support
			// the cockroach debug zip command. The certificate is scoped to the
			// tenant.
			{
				FileName: "client.root",
				SigningRequest: crypto.CertificateSigningRequest{
					Role:       "tenant-service-account-client",
					CommonName: "root",
					URINames: []string{
						fmt.Sprintf("crdb://tenant/%s/user/root", tenantID),
					},
				},
			},
		},
	}
}

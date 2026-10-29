package kubecrdbtest

import (
	"context"
	"fmt"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/crypto"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	DefaultRollingRestartDelay = 10 * time.Second

	// Use the published tenantdir image by default.
	// TODO(CRDB-51087): Switch to the locally built image once the tenantdir
	// image lives in the same repo as the serverless logic.
	tenantDirDefaultImage = "us-docker.pkg.dev/cockroach-cloud-images/data-plane/tenantdir:master"
)

// CreateCluster creates the supplied cluster using the specified options. By
// default all the necessary certificates, secrets, RBAC, etc. needed to run the
// cluster will be created. This will wait until the cluster has been initialized
// and is ready.
func CreateCluster(
	t *testing.T, ctl *kube.Ctl, cluster *crdbv1beta1.CrdbCluster, createOpts ...Option,
) {
	opts := &options{
		createRBAC:  true,
		createCerts: true,
	}

	for _, opt := range createOpts {
		opt.apply(opts)
	}

	// For single region cluster, default Namespace to the cluster's Namespace.
	if len(cluster.Spec.Regions) == 1 {
		cluster.Spec.Regions[0].Namespace = cluster.Namespace
	} else {
		// Otherwise require that .Namespace is set as CrdbClusters created via
		// CreateCluster can't rely on region.Code being the namespace they are
		// deployed in.
		for _, region := range cluster.Spec.Regions {
			if region.Namespace == "" {
				t.Fatalf("region %q is missing required test field .Namespace", region.Code)
			}
		}
	}

	// handle some defaults so they don't always need to be specified
	node := defaultNode(manifests.BetaCrdbClusterNode(cluster, nil, ""))
	cluster.Spec.Template.Spec = node.Spec
	if cluster.Spec.RollingRestartDelay == nil {
		cluster.Spec.RollingRestartDelay = &metav1.Duration{Duration: DefaultRollingRestartDelay}
	}

	if cluster.Spec.Template.Spec.TerminationGracePeriod == nil {
		cluster.Spec.Template.Spec.TerminationGracePeriod = &metav1.Duration{Duration: 10 * time.Second}
	}

	if opts.createRBAC {
		createNodeRBAC(t, ctl, node)
	}

	// Self-signed clusters have the operator generate their certs during
	// reconciliation; there are no external certs to pre-create.
	if opts.createCerts && !cluster.Spec.Template.Spec.Certificates.SelfSigned {
		createCrdbClusterCerts(t, ctl, cluster)
	}

	if cluster.Spec.TenantHost != nil {
		// Default to a single SQLProxy replicas in tests.
		if cluster.Spec.TenantHost.SqlProxyReplicas == nil {
			cluster.Spec.TenantHost.SqlProxyReplicas = new(int32(1))
		}
		// Always use local tenant directory images in tests.
		if cluster.Spec.TenantHost.TenantDirImage == "" {
			cluster.Spec.TenantHost.TenantDirImage = tenantDirDefaultImage
		}
	}

	require.NoError(t, ctl.ApplyWithOptions(context.Background(), kube.ApplyOptions{NoWait: true}, cluster))
}

// ClusterNodes returns a CrdbNodeList containing the nodes for the specified cluster.
func ClusterNodes(
	t *testing.T, ctl *kube.Ctl, cluster *crdbv1beta1.CrdbCluster,
) crdbv1beta1.CrdbNodeList {
	nodes := crdbv1beta1.CrdbNodeList{}
	selector := kube.ParseLabelOrDie(fmt.Sprintf(
		"%s=%s",
		manifests.BetaCrdbClusterNodeOwnerLabelKey,
		cluster.Name,
	))

	require.NoError(t, ctl.List(context.Background(), &kube.ListOptions{LabelSelector: selector}, &nodes))
	return nodes
}

// createCrdbClusterCerts creates the certificates and K8s secrets for the
// given CrdbCluster in the namespace of the given Ctl.
func createCrdbClusterCerts(t *testing.T, ctl *kube.Ctl, cluster *crdbv1beta1.CrdbCluster) {
	ctx := context.Background()
	signer := crypto.DeterministicFakeSignerForTesting(t)

	// Ensure that cluster.Namespace is set correctly.
	cluster.Namespace = ctl.Namespace()

	// NOTE: Before changing any of these values, ensure that nodeCertConfig in
	// manifests uses the same patterns.
	// TODO(chrisseto): Figure out a way to merge this into the nodeCertConfig
	// code. Having to maintain this list in two places is a lot of overhead.
	dnsNames := []string{
		// localhost for debugging.
		"localhost",
		// NB: *.<cluster.Name> is excluded here because it is not used by the
		// operator.
		// Non-suffixed, non-wildcarded name to support kubenet connections.
		// TODO(orchestration): Why is this required?
		fmt.Sprintf("%s.%s", cluster.Name, cluster.Namespace),
		// wild card of cluster.Name for headless Service created by the
		// clusterctrl, with and without the cluster domain.
		fmt.Sprintf("*.%s.%s.svc.cluster.local", cluster.Name, cluster.Namespace),
		// Direct access to the join service.
		fmt.Sprintf("%s-join.%s.svc.cluster.local", cluster.Name, cluster.Namespace),
		// manifests.InternalLoadBalancerService for a well known Service and
		// backwards compatibility and the various forms thereof.
		manifests.InternalLoadBalancerService,
		fmt.Sprintf("%s.%s", manifests.InternalLoadBalancerService, cluster.Namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", manifests.InternalLoadBalancerService, cluster.Namespace),
	}

	for _, region := range cluster.Spec.Regions {
		// Resolve namespace the same way that we do for building the --join
		// flag.
		namespace := cluster.Namespace
		if region.Code != "" {
			namespace = region.Code
		}
		if region.Namespace != "" {
			namespace = region.Namespace
		}

		domain := region.Domain
		if domain == "" {
			domain = "cluster.local"
		}

		// Append all forms of DNS names possible for this region.
		dnsNames = append(
			dnsNames,
			fmt.Sprintf("*.%s.%s.svc.%s", cluster.Name, namespace, domain),
			fmt.Sprintf("%s-join.%s.svc.%s", cluster.Name, namespace, domain),
		)
	}

	certs := cluster.Spec.Template.Spec.Certificates.ExternalCertificates
	csrs := map[string]crypto.CertificateSigningRequest{
		certs.HTTPSecretName: {
			CommonName:  "ui",
			DNSNames:    dnsNames,
			IPAddresses: []string{"127.0.0.1"},
			TTL:         24 * time.Hour,
		},
		certs.NodeClientSecretName: {
			CommonName: "node",
			TTL:        24 * time.Hour,
		},
		certs.NodeSecretName: {
			CommonName:  "node",
			DNSNames:    dnsNames,
			IPAddresses: []string{"127.0.0.1"},
			TTL:         24 * time.Hour,
		},
		certs.RootSQLClientSecretName: {
			CommonName: "root",
			TTL:        24 * time.Hour,
		},
	}

	resources := make([]kube.Object, 0, len(csrs)+2)
	for name, csr := range csrs {
		bundle, err := signer.Sign(ctx, csr)
		require.NoError(t, err, name)

		resources = append(resources, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: cluster.Namespace,
			},
			Data: kube.StringData(map[string]string{
				"tls.crt": string(bundle.Certificate),
				"tls.key": string(bundle.PrivateKey),
			}),
			Type: corev1.SecretTypeTLS,
		})
	}

	ca, err := signer.Certificate(ctx)
	require.NoError(t, err)

	// In test, we use the same CA for node and clients.
	for _, name := range []string{certs.ClientCAConfigMapName, certs.NodeCAConfigMapName} {
		resources = append(resources, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: cluster.Namespace,
			},
			Data: map[string]string{
				"ca.crt": string(ca),
			},
		})
	}

	require.NoError(t, ctl.Apply(ctx, resources...))
}

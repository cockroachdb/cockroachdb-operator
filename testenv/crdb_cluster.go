package testenv

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/crypto"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbtest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubenet"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	manifestslog "github.com/cockroachlabs/cockroach-enterprise-operator/manifests/log"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pgxsql"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/cloudinfra/egress"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// cockroachDefaultImage is the default image used when a new cluster is
	// created without an image defined in the spec or environment variable.
	cockroachDefaultImage = "us-docker.pkg.dev/cockroach-cloud-images/cockroachdb/cockroach:v24.2.0-cloudonly.1"
	// cockroachDefaultImageEnvVar is the name of the environment variable that
	// can be used to set the CRDB image name.
	cockroachDefaultImageEnvVar = "KUBETEST_DEFAULT_COCKROACH_IMG"
	// An organization name to use for Cockroach DB testing and development
	CockroachDBTestOrganizationName = "Cockroach Labs - Production Testing"
	// An enterprise license to use for Cockroach DB testing and development
	CockroachDBTestEnterpriseLicense = "crl-0-EJL04ukFGAEiI0NvY2tyb2FjaCBMYWJzIC0gUHJvZHVjdGlvbiBUZXN0aW5n"
	DefaultRollingRestartDelay       = 10 * time.Second
	// Use the published tenantdir image by default.
	// TODO(CRDB-51087): Switch to the locally built image once the tenantdir
	// image lives in the same repo as the serverless logic.
	tenantDirDefaultImage = "us-docker.pkg.dev/cockroach-cloud-images/data-plane/tenantdir:master"
)

// Set the default cockroach image used on calls to CreateCrdbCluster.
// If the cockroachDefaultImageEnvVar is set, the flag will use this value.
// Otherwise the default is the latest stable version. This should be the
// full image name, including repo and tag, i.e "cockroachdb/cockroach:v21.1".
var crdbImage = flag.String("crdb-image", "", "specify a default cockroach image for calls to CreateCrdbCluster."+
	fmt.Sprintf("If %s is set, the flag will use this value", cockroachDefaultImageEnvVar)+
	"Otherwise the default image used is the latest stable version. Should be the full image name,"+
	"including repo and tag ex/ cockroachdb/cockroach:v21.1")

// GetDefaultCockroachImage returns the default cockroach image to use for
// cluster creation.
func GetDefaultCockroachImage() string {
	// Use specified crdb image if set through flag or env var.
	// Flag takes priority.
	if *crdbImage != "" {
		return *crdbImage
	} else if v, ok := os.LookupEnv(cockroachDefaultImageEnvVar); ok && v != "" {
		return v
	} else {
		// Otherwise, use latest stable version.
		// TODO(alyshan): Find a way to automatically use the latest stable version.
		return cockroachDefaultImage
	}
}

// CreateCrdbCluster creates a CRDB cluster in the given test environment. The
// CRDB cluster will be placed in a newly created K8s namespace with a name
// the same as the spec.Regions[0]. That namespace will be automatically
// destroyed at the end of the test (unless the debug-env flag is true). Typical
// usage is like this:
//
//	// Create 20.2.2 CRDB cluster with 3 nodes in a new namespace.
//	spec := &crdbv1beta1.CrdbClusterSpec{
//	  CockroachImage: "cockroachdb/cockroach:v20.2.2",
//	  Nodes: 3,
//	}
//	cluster := kubetest.CreateCrdbCluster(t, env, "my-cluster", spec)
//
// CrdbClusterSpec.Region's code will be changed to match the namespace
// if it is specified.
// CrdbClusterSpec fields will be defaulted if they are not specified. The
// created CrdbCluster is returned.
func CreateCrdbCluster(
	t *testing.T, env *Env, name string, spec *crdbv1beta1.CrdbClusterSpec,
) *crdbv1beta1.CrdbCluster {
	ctx := context.Background()
	ctl := env.DefaultCluster().Ctl
	ctl = ctl.WithNamespace(env.defaultRegion)

	crdbCluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ctl.Namespace(),
		},
		Spec: *spec.DeepCopy(),
	}
	cluster := createDedicatedCluster(t, env, name, crdbCluster)

	networkPlugin, err := cloud.GetNetworkPluginForCluster(ctx, ctl)
	require.NoError(t, err)

	// TODO(jeffswenson): This fixes the integration tests, but it would be nice
	// if the CrdbCluster was only read if all the attached resources are ready.
	// See CC-7240.
	if cluster.Spec.TenantHost != nil {
		require.Eventually(t, func() bool {
			var objects []kube.Object
			objects = append(objects, manifests.SqlProxyResources(manifests.SqlProxyOptions{Spec: &cluster.Spec})...)
			objects = append(objects, manifests.TenantPools(&cluster.Spec, &cloud.Environment{
				CloudProvider: cloud.K3D,
				CloudRegion:   env.defaultRegion,
			}, networkPlugin)...)
			return nil == ctl.WaitFor(ctx, kube.IsReady, objects...)
		}, time.Minute*20, time.Millisecond*200)
	}

	return cluster
}

// CreateDedicatedCluster creates a single tenant CRDB cluster in the given test
// environment. The CRDB cluster will have the latest stable version, and will
// be placed in a newly created K8s namespace with a name based upon the current
// test name. That namespace will be automatically destroyed at the end of the
// test (unless the debug-env flag is true). Typical usage is like this:
//
//	// Start a single-zone, single-node K8s cluster.
//	env := kubetest.New(t, kubetest.SingleZoneConfig)
//
//	// Create a dedicated CRDB cluster in a new K8s namespace.
//	cluster := kubetest.CreateDedicatedCluster(t, env)
//
// The created CrdbCluster is returned.
func CreateDedicatedCluster(t *testing.T, env *Env) *crdbv1beta1.CrdbCluster {
	ctl := env.DefaultCluster().Ctl.WithNamespace(env.defaultRegion)
	namespace := ctl.Namespace()
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: namespace,
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Template: crdbv1beta1.CrdbNodeTemplate{
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image: GetDefaultCockroachImage(),
				},
			},
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: namespace, Nodes: 1, CloudProvider: "k3d"},
			},
			TLSEnabled: true,
		},
	}
	return createDedicatedCluster(t, env, "single-tenant-cluster", cluster)
}

// createDedicatedCluster is a helper function that sets up a namespace and
// deploys an operator based CockroachDB cluster into it based off a
// CrdbClusterSpec.
func createDedicatedCluster(
	t *testing.T, env *Env, name string, cluster *crdbv1beta1.CrdbCluster,
) *crdbv1beta1.CrdbCluster {
	ctx := context.Background()
	ctl := env.DefaultCluster().Ctl

	namespace := EnsureCustomNamespace(t, ctl, env.defaultRegion)
	ctl = ctl.WithNamespace(namespace)

	// Register artifact capture before ctl.Apply, whose readiness wait fails via
	// t.FailNow on timeout and would skip a later registration. Placing it after
	// EnsureCustomNamespace keeps LIFO teardown from deleting pods first.
	writeArtifactsOnFailed(t)

	logging.Infof(ctx, "creating %s in namespace '%s'", name, namespace)

	if len(cluster.Spec.Regions) == 0 {
		cluster.Spec.Regions = makeClusterRegions(ctx, t, env)
	}

	// Ensure that CloudProvider is set.
	for i, r := range cluster.Spec.Regions {
		if r.CloudProvider != "k3d" {
			cluster.Spec.Regions[i].CloudProvider = "k3d"
		}
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

	// Populate node defaults so they don't have to be filled out every time.
	// Existing values should be respected.
	node := defaultNode(manifests.BetaCrdbClusterNode(cluster, nil, ""))
	cluster.Spec.Template.Spec = node.Spec
	if cluster.Spec.RollingRestartDelay == nil {
		cluster.Spec.RollingRestartDelay = &metav1.Duration{Duration: DefaultRollingRestartDelay}
	}

	if cluster.Spec.Template.Spec.TerminationGracePeriod == nil {
		cluster.Spec.Template.Spec.TerminationGracePeriod = &metav1.Duration{Duration: 10 * time.Second}
	}

	// TODO(toshi): Consider adding more granular controls.
	createNodeRBAC(t, ctl, node)
	createCrdbClusterCerts(t, ctl, cluster)

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

	//// Install test certificates that are used by the cluster ("root" user
	//// client cert, TLS certs, etc).
	//kubecrdbtest.InstallClusterCerts(t, ctl, cluster.Spec.IsTenantHost())

	CreateAuxiliaryObjects(t, ctl, cluster)

	require.NoError(t, ctl.Apply(context.Background(), cluster))

	return cluster
}

// CreateAuxiliaryObjects creates objects necessary for tenant pools to run.
// This is exported for testing beta controller logic.
func CreateAuxiliaryObjects(t *testing.T, ctl *kube.Ctl, cluster *crdbv1beta1.CrdbCluster) {
	ctx := context.Background()

	resources := []kube.Object{}
	resources = append(resources, manifests.InternalCRDBClusterIPService(cluster.Spec))
	resources = append(resources, manifests.RBAC(manifests.RbacOptions{
		Namespace: cluster.Namespace,
	})...)
	resources = append(resources, manifests.NodeToNodeService(&cluster.Spec.Template.Spec))

	loggingConfigMap := manifestslog.LoggingConfigMap(manifestslog.NewLoggingConfigMapOpts(cluster.Spec.Template.Spec.Image))
	// Use default config map name.
	loggingConfigMap.Name = manifestslog.LoggingConfigMapName("")
	resources = append(resources, loggingConfigMap)

	if IsCiliumCNI(t, ctl) {
		// All clusters are created with an allow-all egress policy. If the cluster
		// later enables egress, this policy will be updated.
		resources = append(resources, egress.AllowAllEgressPolicy(false /* allowLinkLocal */))
	}

	if cluster.Spec.IsTenantHost() {
		tenantloggingConfigMap := manifestslog.TenantLoggingConfigMap(manifestslog.NewTenantLoggingConfigMapOpts())
		// Use default config map name.
		tenantloggingConfigMap.Name = manifestslog.LoggingTenantConfigMapName("")
		resources = append(resources, tenantloggingConfigMap)
		// Default configuration for the fluentbit sidecar in sql pods.
		fluentbitTenantConfig := manifests.GetTestTenantNoopConfigMap()
		resources = append(resources, fluentbitTenantConfig)
	}

	require.NoError(t, ctl.Apply(ctx, resources...))
}

// OpenKubeSql returns a DB object that can connect to a dedicated or tenant
// cluster hosted in the given namespace. Connections are made via the
// PodDialer, which tunnels through the K8s API to directly connect with K8s
// pods.
func OpenKubeSql(t *testing.T, ctl *kube.Ctl, options kubenet.Options) *pgxsql.DB {
	db, err := openKubeSql(ctl, options)
	require.NoError(t, err)
	return db
}

func openKubeSql(ctl *kube.Ctl, options kubenet.Options) (*pgxsql.DB, error) {
	if options.Application == "" {
		options.Application = "$ internal-cockroachdb-operator-test"
	}

	if options.Database == "" {
		options.Database = "defaultdb"
	}

	return kubenet.OpenDB(context.Background(), ctl, options)
}

// ErrorOpenKubeSql is the same as OpenKubeSql, but expects an error and
// returns it.
func ErrorOpenKubeSql(t *testing.T, ctl *kube.Ctl, options kubenet.Options) error {
	_, err := openKubeSql(ctl, options)
	require.Error(t, err)
	return err
}

// TestKubeSql opens a connection to a dedicated or tenant cluster using
// OpenKubeSql and then runs a simple test query against it. This is used to
// verify connectivity in tests.
func TestKubeSql(t *testing.T, ctl *kube.Ctl, options kubenet.Options) {
	db := OpenKubeSql(t, ctl, options)
	defer db.Close()

	var res int
	require.NoError(t, db.QueryRow("SELECT 42;").Scan(&res))
	require.Equal(t, 42, res)
}

// ExecKubeSql opens a connection to a dedicated or tenant cluster using
// OpenKubeSql and then runs the given parameterized statement against it.
func ExecKubeSql(t *testing.T, ctl *kube.Ctl, options kubenet.Options, stmt string, args ...any) {
	db := OpenKubeSql(t, ctl, options)
	defer db.Close()
	_, err := db.Exec(stmt, args...)
	require.NoError(t, err)
}

// GetTLS returns a tls.Config suitable for use in ExecKubeSql.
// TODO(orchestration): Make ExecKubeSql "just work" with CrdbNodes.
func GetTLS(t *testing.T, ctl *kube.Ctl, cluster *crdbv1beta1.CrdbCluster) *tls.Config {
	ctx := context.Background()
	ctl = ctl.WithNamespace(cluster.Namespace)

	var ca corev1.ConfigMap
	require.NoError(t, ctl.Get(ctx, cluster.Spec.Template.Spec.Certificates.ExternalCertificates.ClientCAConfigMapName, &ca))

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM([]byte(ca.Data["ca.crt"])))

	var certs corev1.Secret
	require.NoError(t, ctl.Get(ctx, cluster.Spec.Template.Spec.Certificates.ExternalCertificates.RootSQLClientSecretName, &certs))

	pair, err := tls.X509KeyPair(certs.Data["tls.crt"], certs.Data["tls.key"])
	require.NoError(t, err)

	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		RootCAs:      pool,
	}
}

// DisableReplication sets num_replicas for all known configuration zones to 1.
// This speeds up decommissioning and makes it possible to scale for a 3 node
// cluster to 1 node. Don't do this at home.
// NB: nodes in a cluster that has had replication disabled will become unready
// when the liveness range is relocated. This may look like a bug but is
// expected behavior.
func DisableReplication(t *testing.T, ctl *kube.Ctl, cluster *crdbv1beta1.CrdbCluster) {
	// We probably want to loop over all CONFIGURATION ZONES but this works
	// well enough for now.
	ExecKubeSql(t, ctl, kubenet.Options{TLSConfig: GetTLS(t, ctl, cluster)}, `
		ALTER RANGE meta CONFIGURE ZONE USING num_replicas = 1;
		ALTER RANGE system CONFIGURE ZONE USING num_replicas = 1;
		ALTER RANGE default CONFIGURE ZONE USING num_replicas = 1;
		ALTER RANGE liveness CONFIGURE ZONE USING num_replicas = 1;
		ALTER DATABASE system CONFIGURE ZONE USING num_replicas = 1;
	`)
}

// QueryRowKubeSql opens a connection to a dedicated or tenant cluster using
// OpenKubeSql and then runs the given parameterized statement against it.
func QueryRowKubeSql(
	t *testing.T, ctl *kube.Ctl, options kubenet.Options, query string, args ...any,
) *sql.Row {
	db := OpenKubeSql(t, ctl, options)
	defer db.Close()
	row := db.QueryRow(query, args...)
	require.NoError(t, row.Err())
	return row
}

// makeClusterRegions constructs a CrdbClusterRegion slice from the regions in
// the given environment.
func makeClusterRegions(
	ctx context.Context, t *testing.T, env *Env,
) []crdbv1beta1.CrdbClusterRegion {
	var regions []crdbv1beta1.CrdbClusterRegion
	for region, cluster := range env.Clusters {
		azs, err := cluster.Hardware.AvailabilityZones(ctx)
		require.NoError(t, err)
		nodes := 0
		for _, az := range azs {
			vms, err := cluster.Hardware.Nodes(ctx, az)
			require.NoError(t, err)
			nodes += len(vms)
		}
		regions = append(regions, crdbv1beta1.CrdbClusterRegion{
			CloudProvider: "k3d",
			Code:          region,
			Nodes:         int32(nodes),
		})
	}
	return regions
}

var (
	matchFirstCap = regexp.MustCompile("(.)([A-Z][a-z]+)")
	matchAllCap   = regexp.MustCompile("([a-z0-9])([A-Z])")
)

// generateName converts from camel-cased names typical of Go test names to
// dash-cased names. For example:
//
//	TestIntegrationFoo/SubTest => test-integration-foo-sub-test
func generateName(str string) string {
	name := matchFirstCap.ReplaceAllString(str, "${1}_${2}")
	name = matchAllCap.ReplaceAllString(name, "${1}_${2}")
	name = strings.ToLower(name)
	name = strings.ToLower(strings.Replace(name, "_", "-", -1))
	return strings.ToLower(strings.Replace(name, "/", "-", -1))
}

// createCrdbClusterCerts creates the certificates and K8s secrets for the
// given CrdbCluster in the namespace of the given Ctl.
func createCrdbClusterCerts(t *testing.T, ctl *kube.Ctl, cluster *crdbv1beta1.CrdbCluster) {
	ctx := context.Background()
	signer := crypto.DeterministicFakeSignerForTesting(t)

	// Ensure that cluster.Namespace is set correctly.
	require.Equal(t, cluster.Namespace, ctl.Namespace())

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

	// TODO: Simplify cluster cert installation. There is some overlap.
	kubecrdbtest.InstallClusterCerts(t, ctl, cluster.Spec.IsTenantHost())
}

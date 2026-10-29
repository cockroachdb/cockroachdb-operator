package clusterctrl_test

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbtest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubenet"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/testenv"
	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type VersionPair struct {
	Tag            string
	ClusterVersion string
}

func (vp *VersionPair) Image() string {
	return "us-docker.pkg.dev/cockroach-cloud-images/cockroachdb/cockroach:" + vp.Tag
}

func TestIntegrationCluster(t *testing.T) {
	testutils.MarkIntegrationTest(t)

	suite.Run(t, new(ClusterTestSuite))
}

type ClusterTestSuite struct {
	suite.Suite
	env      *testenv.Env
	ctl      *kube.Ctl
	tenantID uint64
	ctx      context.Context
}

func (s *ClusterTestSuite) SetupSuite() {
	s.env = testenv.New(s.T(), testenv.MultiZoneConfig)
	ctl := s.env.DefaultCluster().Ctl
	namespace := testenv.EnsureNamespace(s.T(), ctl)
	s.ctl = ctl.WithNamespace(namespace)
	s.tenantID = crdbv1beta1.TenantMinID.ToUint64()
	s.ctx = context.Background()
}

func (s *ClusterTestSuite) TestCreate() {
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: testenv.EnsureNamespace(s.T(), s.ctl),
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			TLSEnabled: true,
			Template: crdbv1beta1.CrdbNodeTemplate{
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image: testenv.GetDefaultCockroachImage(),
				},
			},
			// Note(alyshan): "us-east-1" is used as the code b/c that is the region
			// that the Cloud Provider Environment defines (testenv.MultiZoneConfig) and
			// the webhook validates against.
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east-1", Nodes: 3, CloudProvider: "k3d"},
			},
		},
	}
	kubecrdbtest.CreateCluster(s.T(), s.ctl.WithNamespace(cluster.Namespace), cluster)
	s.Require().NoError(s.ctl.WithNamespace(cluster.Namespace).WaitFor(s.ctx, kube.IsQuiescent, cluster))
}

// Try to apply a cluster spec that is not valid.
func (s *ClusterTestSuite) TestInvalidClusterSpec() {
	expectedErr := "host clusters (TenantHost!=nil) do not support insecure clusters " +
		"(TLSEnabled=false)"

	err := s.ctl.Apply(context.Background(), &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Regions: []crdbv1beta1.CrdbClusterRegion{{Code: "us-east-1", Nodes: 1}},
			Template: crdbv1beta1.CrdbNodeTemplate{
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image: "cockroachdb/cockroach:v24.1.0",
				},
			},
			TLSEnabled: false,
			TenantHost: &crdbv1beta1.TenantHost{},
		},
	})

	s.Contains(err.Error(), expectedErr)
}

// TestCreateInsecureCluster verifies that the operator correctly reconciles an insecure
// cluster (TLSEnabled=false) by ensuring pods become ready without TLS certificates,
// the readiness probe uses HTTP instead of HTTPS, and the cert-reloader sidecar is omitted.
func (s *ClusterTestSuite) TestCreateInsecureCluster() {
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-insecure-cluster",
			Namespace: testenv.EnsureNamespace(s.T(), s.ctl),
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Template: crdbv1beta1.CrdbNodeTemplate{
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image: testenv.GetDefaultCockroachImage(),
				},
			},
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east-1", Nodes: 3, CloudProvider: "k3d"},
			},
			TLSEnabled: false,
		},
	}

	kubecrdbtest.CreateCluster(s.T(), s.ctl.WithNamespace(cluster.Namespace), cluster, kubecrdbtest.Certs(false))
	s.Require().NoError(s.ctl.WithNamespace(cluster.Namespace).WaitFor(s.ctx, kube.IsQuiescent, cluster))

	// Verify cluster is ready
	s.Require().NoError(s.ctl.WithNamespace(cluster.Namespace).Get(s.ctx, cluster.Name, cluster))
	s.Require().True(cluster.Status.HasCondition(crdbv1beta1.ClusterInitialized, metav1.ConditionTrue))

	// List all pods for the cluster
	var pods corev1.PodList
	s.NoError(s.ctl.WithNamespace(cluster.Namespace).List(s.ctx, &kube.ListOptions{
		LabelSelector: kube.ParseLabelOrDie("app=cockroachdb"),
	}, &pods))
	s.Require().Len(pods.Items, 3, "expected exactly three CockroachDB pods")

	for _, pod := range pods.Items {
		// Verify readiness probe uses HTTP scheme (not HTTPS)
		found := false
		for _, container := range pod.Spec.Containers {
			if container.Name == manifests.CockroachDB {
				found = true
				s.Require().NotNil(container.ReadinessProbe)
				s.Require().NotNil(container.ReadinessProbe.HTTPGet)
				s.Equal(corev1.URISchemeHTTP, container.ReadinessProbe.HTTPGet.Scheme,
					"readiness probe should use HTTP for insecure clusters")

				// Verify tls env vars
				//COCKROACH_INSECURE must be set and COCKROACH_CERTS_DIR must be absent.
				insecureFound := false
				for _, env := range container.Env {
					s.NotEqual("COCKROACH_CERTS_DIR", env.Name,
						"COCKROACH_CERTS_DIR should not be set for insecure clusters")
					if env.Name == "COCKROACH_INSECURE" {
						insecureFound = true
						s.Equal("true", env.Value,
							"COCKROACH_INSECURE should be set to true for insecure clusters")
					}
				}
				s.True(insecureFound, "COCKROACH_INSECURE env var should be present for insecure clusters")
			}
		}
		s.True(found, "cockroachdb container not found in pod")

		// Verify cert-reloader sidecar is not present
		for _, container := range pod.Spec.Containers {
			s.NotEqual("cert-reloader", container.Name,
				"cert-reloader sidecar should not be present in insecure clusters")
		}

		// Verify shareProcessNamespace is false
		s.Require().NotNil(pod.Spec.ShareProcessNamespace)
		s.False(*pod.Spec.ShareProcessNamespace,
			"shareProcessNamespace should be false for insecure clusters")
	}
}

func (s *ClusterTestSuite) TestKubernetesEndpointWatcher() {
	ctx := context.Background()

	testenv.CreateCrdbCluster(s.T(), s.env, "test", &crdbv1beta1.CrdbClusterSpec{
		TLSEnabled: true,
	})
	ctl := s.ctl.WithNamespace("default")

	// Retrieve the operator's pod dialer.
	var pods corev1.PodList
	s.NoError(ctl.List(ctx, &kube.ListOptions{
		LabelSelector: kube.ParseLabelOrDie(fmt.Sprintf("app=%s", manifests.OperatorAppLabel)),
	}, &pods))
	s.Len(pods.Items, 1, "expected exactly one operator pod")
	dialer, err := kube.NewPodDialer(ctl.RestConfig(), ctl.Namespace())
	s.NoError(err)

	// Construct HTTP client to scrape metrics.
	client := http.Client{
		Transport: &http.Transport{
			DialContext: dialer.DialContext,
			// Rather than reusing TCP connections, we will close them once the
			// HTTP request is done to prevent accidental leakage of goroutines.
			DisableKeepAlives: true,
		},
	}

	scrapeAndParse := func() (int, error) {
		resp, err := client.Get(fmt.Sprintf("http://%s:8080/metrics", pods.Items[0].GetName()))
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return 0, errors.Newf("unexpected HTTP status %s", resp.Status)
		}

		var parser expfmt.TextParser
		metrics, err := parser.TextToMetricFamilies(resp.Body)
		if err != nil {
			return 0, err
		}
		for _, m := range metrics["crdb_cluster_reconciler_endpoint_watcher_calls_total"].GetMetric() {
			return int(m.Counter.GetValue()), nil
		}
		return 0, errors.Newf("could not find endpoint watcher metric")
	}

	// Retrieve initial count.
	count, err := scrapeAndParse()
	s.NoError(err)

	// Update endpoint to trigger watcher.
	var endpoint corev1.Endpoints
	s.NoError(ctl.Get(ctx, "kubernetes", &endpoint))
	endpoint.Labels["foo"] = "bar"
	s.NoError(ctl.UpdateWithOptions(ctx, kube.UpdateOptions{}, &endpoint))

	// Validate that count has incremented.
	s.Eventually(func() bool {
		newCount, err := scrapeAndParse()
		if err != nil {
			return false
		}
		return newCount > count
	}, 10*time.Second, 100*time.Millisecond, "crdb_cluster_reconciler_endpoint_watcher_calls_total should increment")
}

func (s *ClusterTestSuite) TestCreateDeleteHost() {
	ctx := context.Background()

	namespace := testenv.EnsureNamespace(s.T(), s.ctl)
	ctl := s.ctl.WithNamespace(namespace)
	// Install certs so SQLProxy and the TenantPool can become healthy.
	kubecrdbtest.InstallClusterCerts(s.T(), ctl, true /* isHostCluster */)

	// Create a host cluster.
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "host",
			Namespace: namespace,
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Template: crdbv1beta1.CrdbNodeTemplate{
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image: testenv.GetDefaultCockroachImage(),
				},
			},
			// Note(alyshan): "us-east-1" is used as the code b/c that is the region
			// that the Cloud Provider Environment defines (testenv.MultiZoneConfig) and
			// the webhook validates against.
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east-1", Nodes: 3, CloudProvider: "k3d"},
			},
			TLSEnabled: true,
			TenantHost: &crdbv1beta1.TenantHost{},
		},
	}
	testenv.CreateAuxiliaryObjects(s.T(), ctl, cluster)
	kubecrdbtest.CreateCluster(s.T(), ctl, cluster)
	s.NoError(ctl.WaitFor(s.ctx, kube.IsReady, cluster))

	// Assert that the default tenant pools have been created.
	s.Equal([]crdbv1beta1.TenantPool{
		{
			Name: crdbv1beta1.DefaultTenantPoolName,
			Resources: corev1.ResourceRequirements{
				// Requests are omitted and will be filled in by the
				// manifests package's defaults.
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:              resource.MustParse("333m"),
					corev1.ResourceMemory:           resource.MustParse("1500M"),
					corev1.ResourceEphemeralStorage: resource.MustParse("380Mi"),
				},
			},
		},
	}, cluster.Spec.TenantHost.TenantPools)

	// Ensure that SQLProxy is created.
	var sqlProxy appsv1.Deployment
	s.Eventually(func() bool {
		err := ctl.Get(ctx, "sqlProxy", &sqlProxy)
		return err == nil && kube.IsReady(&sqlProxy) == nil
	}, 120*time.Second, time.Second)

	// Ensure that the default tenant pool is created.
	var defaultTenantPool appsv1.Deployment
	s.NoError(ctl.Get(ctx, "tenantpool-default", &defaultTenantPool))
	ctl.WaitForWithOptions(ctx, kube.WaitForOptions{
		DoneFunc: kube.IsReady,
		BackOff:  kube.NewCtlDefaultBackOffFn(30 * time.Second),
	}, &defaultTenantPool)
	s.Equal(int32(3), *defaultTenantPool.Spec.Replicas)

	// Create a tenant in the host cluster; this should be deleted automatically
	// when the host cluster is deleted.
	tenant := testenv.CreateCrdbTenant(s.T(), ctl, "tenant", &crdbv1beta1.CrdbTenantSpec{
		TenantID:         crdbv1beta1.TenantID(10),
		Pods:             1,
		DisableAutoscale: true,
	})

	// Wait until the reconciliation succeeds.
	s.Eventually(func() bool {
		require.NoError(s.T(), ctl.Get(ctx, cluster.Name, cluster))
		if !cluster.Status.HasCondition(crdbv1beta1.ClusterInitialized, metav1.ConditionTrue) {
			s.T().Log("cluster is missing initialized condition")
			return false
		}

		require.NoError(s.T(), ctl.Get(ctx, tenant.Name, tenant))
		if tenant.OwnerReferences == nil {
			s.T().Log("tenant is missing owner reference")
			return false
		}
		return true
	}, 30*time.Second, time.Second)

	// Edit the spec of the CrdbCluster and ensure that ObservedGeneration gets
	// set appropriately.
	s.NoError(ctl.Get(ctx, "host", cluster))
	prevGen := cluster.ObjectMeta.Generation
	if cluster.Spec.ClusterSettings == nil {
		cluster.Spec.ClusterSettings = map[string]string{}
	}
	cluster.Spec.ClusterSettings["cluster.organization"] = testenv.CockroachDBTestOrganizationName + "-test"

	// Apply should block on the CrdbClusterStatus' ObservedGeneration being set appropriately.
	// Wait until the CrdbCluster is quiescent before deleting the cluster.
	s.NoError(ctl.ApplyWithOptions(ctx, kube.ApplyOptions{
		WaitForOptions: kube.WaitForOptions{DoneFunc: kube.IsQuiescent},
	}, cluster))
	s.Greater(cluster.ObjectMeta.Generation, prevGen) // Sanity check.
	s.Equal(cluster.ObjectMeta.Generation, cluster.Status.ObservedGeneration)

	// Now delete the CrdbCluster.
	s.NoError(ctl.Delete(ctx, cluster))

	// Ensure that SQLProxy, tenant, and tenant pool are deleted, since they are
	// owned by the CrdbCluster. This uses Eventually because K8s can take a bit
	// of time to GC these resources once the owning cluster has been deleted.
	s.Eventually(func() bool {
		if !kubeerrors.IsNotFound(ctl.Get(ctx, "sqlProxy", &sqlProxy)) {
			s.T().Log("found the sqlproxy")
			return false
		}
		if !kubeerrors.IsNotFound(ctl.Get(ctx, "tenantpool-default", &defaultTenantPool)) {
			s.T().Log("found the tenantpool")
			return false
		}
		if !kubeerrors.IsNotFound(ctl.Get(ctx, "tenant", tenant)) {
			s.T().Log("found the tenant")
			return false
		}
		return true
	}, 120*time.Second, time.Second)
}

func (s *ClusterTestSuite) TestUpgradeSQLProxy() {
	ctx := context.Background()

	namespace := testenv.EnsureNamespace(s.T(), s.ctl)
	ctl := s.ctl.WithNamespace(namespace)
	// Install certs so SQLProxy and the TenantPool can become healthy.
	kubecrdbtest.InstallClusterCerts(s.T(), ctl, true /* isHostCluster */)

	// Create a host cluster.
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "host",
			Namespace: namespace,
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Template: crdbv1beta1.CrdbNodeTemplate{
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image: testenv.GetDefaultCockroachImage(),
				},
			},
			// Note(alyshan): "us-east-1" is used as the code b/c that is the region
			// that the Cloud Provider Environment defines (testenv.MultiZoneConfig) and
			// the webhook validates against.
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east-1", Nodes: 3, CloudProvider: "k3d"},
			},
			TLSEnabled: true,
			TenantHost: &crdbv1beta1.TenantHost{},
		},
	}
	testenv.CreateAuxiliaryObjects(s.T(), ctl, cluster)
	kubecrdbtest.CreateCluster(s.T(), ctl, cluster)
	s.NoError(ctl.WaitFor(s.ctx, kube.IsReady, cluster))

	// Ensure that SQLProxy is created.
	var sqlProxy appsv1.Deployment
	s.Eventually(func() bool {
		err := ctl.Get(ctx, "sqlproxy", &sqlProxy)
		return err == nil && kube.IsReady(&sqlProxy) == nil
	}, 120*time.Second, time.Second)

	const (
		newSqlProxyImage  = "us-docker.pkg.dev/cockroach-cloud-images/cockroachdb/cockroach:v23.1.14-173-g19ba39bf5c2"
		newTenantDirImage = "us-docker.pkg.dev/cockroach-cloud-images/data-plane/tenantdir:a8cc1b4a2581f8e7efd854218155bd65bd2f6da7"
	)

	// Edit the spec of the CrdbCluster and ensure that ObservedGeneration gets
	// set appropriately.
	s.NoError(ctl.Get(ctx, "host", cluster))
	prevGen := cluster.ObjectMeta.Generation
	cluster.Spec.TenantHost.SqlProxyImage = newSqlProxyImage
	cluster.Spec.TenantHost.TenantDirImage = newTenantDirImage

	// Apply should block on the CrdbClusterStatus' ObservedGeneration being set appropriately.
	s.NoError(ctl.Apply(ctx, cluster))
	s.Greater(cluster.ObjectMeta.Generation, prevGen) // Sanity check.
	s.Equal(cluster.ObjectMeta.Generation, cluster.Status.ObservedGeneration)

	// Wait until the reconciliation succeeds.
	s.Eventually(func() bool {
		if err := ctl.Get(ctx, "sqlproxy", &sqlProxy); err != nil {
			return false
		}
		if sqlProxy.Spec.Template.Spec.Containers[0].Image != newSqlProxyImage {
			return false
		}
		if sqlProxy.Spec.Template.Spec.Containers[1].Image != newTenantDirImage {
			return false
		}
		return kube.IsReady(&sqlProxy) == nil
	}, 120*time.Second, time.Second)
}

func (s *ClusterTestSuite) TestUpgradeHost() {
	// TODO(alyshan): Resumption of a tenant doesn't seem to work in
	// the recording mode. Specifically, after EnsureTenantResumed calls
	// ctl.Update to set the Spec.Pods=1 on a tenant, the AfterMutation hook
	// on the ctl object doesn't fire. As we don't see the tenant reconciler
	// get invoked.

	// TODO(chrisseto): Upgrading takes a long time right now. To minimize test
	// runtime, we only perform a single major upgrade. Once the runtime is
	// more reasonable, we should test a major and minor upgrade together.
	versions := []VersionPair{
		{Tag: "v23.1.14", ClusterVersion: "23.1"},
		{Tag: "v23.2.1", ClusterVersion: "23.2"},
	}
	namespace := testenv.EnsureNamespace(s.T(), s.ctl)
	ctl := s.ctl.WithNamespace(namespace)
	// Install certs so SQLProxy and the TenantPool can become healthy.
	kubecrdbtest.InstallClusterCerts(s.T(), ctl, true /* isHostCluster */)

	// Create a host cluster.
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "host",
			Namespace: namespace,
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Template: crdbv1beta1.CrdbNodeTemplate{
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image: versions[0].Image(),
				},
			},
			// Note(alyshan): "us-east-1" is used as the code b/c that is the region
			// that the Cloud Provider Environment defines (testenv.MultiZoneConfig) and
			// the webhook validates against.
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east-1", Nodes: 3, CloudProvider: "k3d"},
			},
			TLSEnabled: true,
			TenantHost: &crdbv1beta1.TenantHost{},
		},
	}
	testenv.CreateAuxiliaryObjects(s.T(), ctl, cluster)
	kubecrdbtest.CreateCluster(s.T(), ctl, cluster)
	s.NoError(ctl.WaitFor(s.ctx, kube.IsReady, cluster))

	ctx := context.Background()

	for _, vp := range versions[1:] {
		s.doUpgrade(ctx, ctl, cluster, vp)
	}
}

func (s *ClusterTestSuite) doUpgrade(
	ctx context.Context, ctl *kube.Ctl, cluster *crdbv1beta1.CrdbCluster, next VersionPair,
) {
	// Collect the cluster's current version, so we can determine if a major
	// upgrade is required.
	var prevClusterVersion string
	row := testenv.QueryRowKubeSql(s.T(), ctl, kubenet.Options{}, "SHOW CLUSTER SETTING VERSION")
	s.NoError(row.Scan(&prevClusterVersion))

	var tenants []*crdbv1beta1.CrdbTenant
	tenants = append(tenants, testenv.CreateCrdbTenant(s.T(), ctl, "pre-upgrade", &crdbv1beta1.CrdbTenantSpec{
		TenantID: crdbv1beta1.TenantMinID,
	}))

	// Disable auto finalization.
	testenv.ExecKubeSql(
		s.T(),
		ctl,
		kubenet.Options{},
		"SET CLUSTER SETTING cluster.preserve_downgrade_option = crdb_internal.node_executable_version()",
	)

	// Note(alyshan): The operator doesn't support updating the KV layer. We need to
	// manually perform an upgrade of the cockroachdb statefulset, otherwise tenants
	// will fail to set their cluster version. This approach suffices for tests only.

	s.NoError(ctl.Get(ctx, cluster.Name, cluster))
	cluster.Spec.Template.Spec.Image = next.Image()
	s.NoError(ctl.Apply(ctx, cluster))
	// Wait for the CrdbCluster upgrade to complete.
	s.NoError(ctl.WaitFor(ctx, kube.IsQuiescent, cluster))

	// Create a tenant after the STS is updated but before tenant pools are
	// upgraded.
	tenants = append(tenants, testenv.CreateCrdbTenant(s.T(), ctl, "intra-upgrade", &crdbv1beta1.CrdbTenantSpec{
		TenantID: s.nextTenantID(),
	}))

	// Perform a major upgrade that will require finalization. The cluster
	// should be ready quickly since this is mostly only the time taken to
	// update the tenantpool pods.
	s.NoError(ctl.Apply(ctx, cluster))

	// Assert that the KV cluster's has not been finalized OR that this is a
	// patch upgrade and that the cluster version is equal to the previous.
	var kvClusterVersion string
	row = testenv.QueryRowKubeSql(s.T(), ctl, kubenet.Options{}, "SHOW CLUSTER SETTING VERSION")
	s.NoError(row.Scan(&kvClusterVersion))
	s.Equal(prevClusterVersion, kvClusterVersion)

	tenants = append(tenants, testenv.CreateCrdbTenant(s.T(), ctl, "pre-finalize", &crdbv1beta1.CrdbTenantSpec{
		TenantID: s.nextTenantID(),
	}))

	// Finalize the KV cluster, this is a No-op in the case of a patch upgrade.
	testenv.ExecKubeSql(
		s.T(),
		ctl,
		kubenet.Options{},
		"RESET CLUSTER SETTING cluster.preserve_downgrade_option",
	)

	// Wait for the operator to process the full finalization.
	s.T().Logf("Waiting for the operator to process finalization")
	s.NoError(ctl.WaitFor(ctx, func(o kube.Object) error {
		c := o.(*crdbv1beta1.CrdbCluster)
		finalizing := c.Status.HasAction(crdbv1beta1.AwaitFinalization, crdbv1beta1.Finished)
		if !finalizing {
			return errors.New("still finalizing")
		}
		return nil
	}, cluster))

	// Assert that this cluster either has been finalized OR that this was a patch upgrade.
	row = testenv.QueryRowKubeSql(s.T(), ctl, kubenet.Options{}, "SHOW CLUSTER SETTING VERSION")
	s.NoError(row.Scan(&kvClusterVersion))
	s.Equal(next.ClusterVersion, kvClusterVersion)

	// Create a post-upgrade/finalization tenant.
	tenants = append(tenants, testenv.CreateCrdbTenant(s.T(), ctl, "post-upgrade", &crdbv1beta1.CrdbTenantSpec{
		TenantID: s.nextTenantID(),
	}))

	// Assert that all tenants have been successfully upgraded to the latest
	// version.
	for _, tenant := range tenants {
		s.T().Logf("verifying that tenant %d is on version %s", tenant.Spec.TenantID, next.ClusterVersion)
		row := testenv.QueryRowKubeSql(s.T(), ctl, kubenet.Options{
			TenantID: tenant.Spec.TenantID,
		}, "SHOW CLUSTER SETTING VERSION")
		var tenantClusterVersion string
		s.NoError(row.Scan(&tenantClusterVersion))
		s.Equal(
			next.ClusterVersion,
			tenantClusterVersion,
			"tenant %d is not on the right version",
			tenant.Spec.TenantID,
		)
	}
}

func (s *ClusterTestSuite) nextTenantID() crdbv1beta1.TenantID {
	return crdbv1beta1.TenantID(atomic.AddUint64(&s.tenantID, 1))
}

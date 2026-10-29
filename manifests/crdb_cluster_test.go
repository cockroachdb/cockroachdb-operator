package manifests

import (
	"encoding/json"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCrdbClusterPDB(t *testing.T) {
	testCases := []struct {
		Name     string
		Replicas int32
		Budget   int32
	}{
		{
			Name:     "Single",
			Replicas: 3,
			Budget:   1,
		},
		{
			Name:     "Multiple",
			Replicas: 3,
			Budget:   2,
		},
		{
			Name:     "BudgetExceedsReplicas",
			Replicas: 3,
			Budget:   5,
		},
		{
			Name:     "ZeroBudget",
			Replicas: 3,
			Budget:   0,
		},
	}

	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-cluster",
		},
	}

	for _, tc := range testCases {
		mani := CrdbClusterOperatorPDB(cluster, int(tc.Replicas), int(tc.Budget))

		actual, err := json.MarshalIndent(mani, "", "\t")
		assert.NoError(t, err)

		testutils.AssertGoldenJSON(t, tc.Name, actual)
	}
}

func TestCrdbClusterPriorityClasses(t *testing.T) {
	testCases := []struct {
		Name string
	}{
		{
			Name: "",
		},
	}

	for _, tc := range testCases {
		mani := CrdbClusterPriorityClasses()

		actual, err := json.MarshalIndent(mani, "", "\t")
		assert.NoError(t, err)

		testutils.AssertGoldenJSON(t, tc.Name, actual)
	}
}

func TestCrdbTenantPriorityClasses(t *testing.T) {
	testCases := []struct {
		Name string
	}{
		{
			Name: "",
		},
	}

	for _, tc := range testCases {
		mani := crdbTenantPriorityClasses()

		actual, err := json.MarshalIndent(mani, "", "\t")
		assert.NoError(t, err)

		testutils.AssertGoldenJSON(t, tc.Name, actual)
	}
}

// checkManifestForDeploymentImageName checks the name of a manifest's
// deployment's image name.
func checkManifestForDeploymentImageName(
	expectedName string,
	deploymentName string,
	containerName string,
	manifest []kube.Object,
	t *testing.T,
) {
	numImagesChecked := 0
	for _, object := range manifest {
		if deployment, ok := object.(*appsv1.Deployment); ok {
			if deployment.Name != deploymentName {
				continue
			}
			for _, container := range deployment.Spec.Template.Spec.Containers {
				if container.Name != containerName {
					continue
				}
				assert.Equal(t, expectedName, container.Image)
				numImagesChecked++
			}
		}
	}
	assert.Equal(t, 1, numImagesChecked)
}

func TestSqlProxyResources(t *testing.T) {
	now := time.Date(2021, 1, 29, 12, 0, 0, 0, time.UTC)
	defer testutils.HookGlobal(&timeutil.Now, func() time.Time { return now })()

	testCases := []struct {
		Name      string
		Spec      crdbv1beta1.CrdbClusterSpec
		UseCilium bool
	}{
		{
			Name: "defaults",
			Spec: crdbv1beta1.CrdbClusterSpec{
				TenantHost: &crdbv1beta1.TenantHost{},
			},
		},
		{
			Name: "small-cluster",
			Spec: crdbv1beta1.CrdbClusterSpec{
				TenantHost: &crdbv1beta1.TenantHost{},
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{Code: "us-east1", Nodes: 1},
				},
			},
		},
		{
			Name: "large-cluster",
			Spec: crdbv1beta1.CrdbClusterSpec{
				TenantHost: &crdbv1beta1.TenantHost{},
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{Code: "us-east1", Nodes: 20},
				},
			},
		},
		{
			Name: "sqlProxyReplicas-overrides",
			Spec: crdbv1beta1.CrdbClusterSpec{
				TenantHost: &crdbv1beta1.TenantHost{
					SqlProxyReplicas: new(int32(100)),
				},
			},
		},
		{
			Name: "image-overrides",
			Spec: crdbv1beta1.CrdbClusterSpec{
				TenantHost: &crdbv1beta1.TenantHost{
					SqlProxyImage:  "CUSTOMSQLPROXYIMAGEHERE",
					TenantDirImage: "CUSTOMTENANTDIRIMAGEHERE",
				},
			},
		},
		{
			Name: "disable-connection-rebalancing",
			Spec: crdbv1beta1.CrdbClusterSpec{
				TenantHost: &crdbv1beta1.TenantHost{},
				Features: []crdbv1beta1.ClusterFeature{
					crdbv1beta1.ClusterSqlProxyDisableConnectionRebalancing,
				},
			},
		},
		{
			Name: "sqlproxy-proxy-protocol",
			Spec: crdbv1beta1.CrdbClusterSpec{
				TenantHost: &crdbv1beta1.TenantHost{},
				Features: []crdbv1beta1.ClusterFeature{
					crdbv1beta1.ClusterSqlProxyProxyProtocol,
				},
			},
		},
		{
			Name: "cilium-network-plugin",
			Spec: crdbv1beta1.CrdbClusterSpec{
				TenantHost: &crdbv1beta1.TenantHost{},
			},
			UseCilium: true,
		},
	}

	for _, tc := range testCases {
		tc.Spec.Template.Spec.Image = "CUSTOMREPO:CUSTOMCRDBIMAGEVERSIONHERE"
		// Default region if a list was not supplied.
		if len(tc.Spec.Regions) == 0 {
			tc.Spec.Regions = []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east1", Nodes: 5},
			}
		}
		plugin := cloud.Calico
		if tc.UseCilium {
			plugin = cloud.Cilium
		}
		manifest := SqlProxyResources(SqlProxyOptions{
			Spec:                   &tc.Spec,
			KubernetesAPIServerIPs: []string{"127.0.0.1"},
			NetworkPlugin:          plugin,
			ApplyConfigMaps:        true,
		})

		actual, err := json.MarshalIndent(manifest, "", "\t")
		assert.NoError(t, err)

		testutils.AssertGoldenJSON(t, tc.Name, actual)

		// Explicitly check deployment's CockroachImage field for select cases.
		switch tc.Name {
		case "image-overrides":
			checkManifestForDeploymentImageName("CUSTOMSQLPROXYIMAGEHERE", "sqlproxy", "sqlproxy", manifest, t)
			checkManifestForDeploymentImageName("CUSTOMTENANTDIRIMAGEHERE", "sqlproxy", "tenantdir", manifest, t)
		default:
			checkManifestForDeploymentImageName("CUSTOMREPO:CUSTOMCRDBIMAGEVERSIONHERE", "sqlproxy", "sqlproxy", manifest, t)
		}
	}
}

func TestTenantPools(t *testing.T) {
	cockroachPodTemplate := func() *crdbv1beta1.PodTemplateSpec {
		return &crdbv1beta1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: CRDBContainerName,
					Env: []corev1.EnvVar{{
						Name:  CockroachChannelEnvVar,
						Value: "CockroachCloud-Test",
					}},
				}},
			},
		}
	}

	testCases := []struct {
		Name      string
		Spec      crdbv1beta1.CrdbClusterSpec
		Env       *cloud.Environment
		UseCilium bool
	}{
		{
			Name: "defaults",
			Spec: crdbv1beta1.CrdbClusterSpec{
				TenantHost: &crdbv1beta1.TenantHost{},
			},
		},
		{
			Name: "small-aws-cluster",
			Spec: crdbv1beta1.CrdbClusterSpec{
				TenantHost: &crdbv1beta1.TenantHost{
					TenantPools: []crdbv1beta1.TenantPool{
						{Name: "low-trust"},
						{Name: "high-trust", AllowInternetAccess: true},
					},
				},
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{Code: "us-east1", Nodes: 1},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						Image:       "default:image",
						StartFlags:  &crdbv1beta1.Flags{Upsert: []string{maxOffsetFlag + "=250ms"}},
						PodTemplate: cockroachPodTemplate(),
					},
				},
			},
			Env: &cloud.Environment{
				CloudRegion:   "us-east-1",
				CloudProvider: cloud.AWS,
			},
		},
		{
			Name: "small-gcp-cluster",
			Spec: crdbv1beta1.CrdbClusterSpec{
				TenantHost: &crdbv1beta1.TenantHost{
					TenantPools: []crdbv1beta1.TenantPool{
						{Name: "low-trust"},
						{Name: "high-trust", AllowInternetAccess: true},
					},
				},
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{Code: "us-east1", Nodes: 1},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						Image: "default:image",
						Flags: map[string]string{
							maxOffsetFlag: "100ms",
						},
						Env: []corev1.EnvVar{{
							Name:  CockroachChannelEnvVar,
							Value: "CockroachCloud-Test",
						}},
					},
				},
			},
		},
		{
			Name: "multi-region",
			Spec: crdbv1beta1.CrdbClusterSpec{
				TenantHost: &crdbv1beta1.TenantHost{
					TenantPools: []crdbv1beta1.TenantPool{
						{Name: "low-trust"},
						{Name: "high-trust", AllowInternetAccess: true},
					},
				},
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{Code: "us-east1", Nodes: 1},
					{Code: "us-west1", Nodes: 1},
					{Code: "eu-west1", Nodes: 1},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						Image:       "default:image",
						PodTemplate: cockroachPodTemplate(),
					},
				},
			},
		},
		{
			Name: "large-cluster",
			Spec: crdbv1beta1.CrdbClusterSpec{
				TenantHost: &crdbv1beta1.TenantHost{
					TenantPools: []crdbv1beta1.TenantPool{
						{Name: "low-trust"},
						{Name: "high-trust", AllowInternetAccess: true},
					},
				},
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{Code: "us-east1", Nodes: 20},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						Image: "default:image",
					},
				},
			},
		},
		{
			Name: "tenantpool-cilium",
			Spec: crdbv1beta1.CrdbClusterSpec{
				TenantHost: &crdbv1beta1.TenantHost{
					TenantPools: []crdbv1beta1.TenantPool{
						{Name: "low-trust"},
						{Name: "high-trust", AllowInternetAccess: true},
					},
				},
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						Image: "default:image",
					},
				},
			},
			UseCilium: true,
		},
		{
			Name: "tenantPoolImage-overrides",
			Spec: crdbv1beta1.CrdbClusterSpec{
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						Image: "default:image",
					},
				},
				TenantHost: &crdbv1beta1.TenantHost{
					TenantPools: []crdbv1beta1.TenantPool{
						{
							Name:           "low-trust",
							CockroachImage: "CUSTOMREPO:TENANTPOOLOVERRIDEIMAGEVERSION",
						},
					},
				},
			},
		},
		{
			Name: "tenantOtelCollectorImage-overrides",
			Spec: crdbv1beta1.CrdbClusterSpec{
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						Image: "default:image",
					},
				},
				TenantHost: &crdbv1beta1.TenantHost{
					TenantOtelCollectorImage: "custom-otel-image",
					TenantPools: []crdbv1beta1.TenantPool{
						{Name: "low-trust"},
						{Name: "high-trust", AllowInternetAccess: true},
					},
				},
			},
		},
		{
			Name: "priority-overrides",
			Spec: crdbv1beta1.CrdbClusterSpec{
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						Image: "default:image",
					},
				},
				TenantHost: &crdbv1beta1.TenantHost{
					TenantPools: []crdbv1beta1.TenantPool{
						{
							Name:              "mid-trust",
							PriorityClassName: "mid-trust-class",
						},
					},
				},
			},
		},
		{
			Name: "customPools",
			Spec: crdbv1beta1.CrdbClusterSpec{
				Template: crdbv1beta1.CrdbNodeTemplate{
					Spec: crdbv1beta1.CrdbNodeSpec{
						Image: "default:image",
					},
				},
				TenantHost: &crdbv1beta1.TenantHost{
					TenantPools: []crdbv1beta1.TenantPool{
						{
							Name:              "low-trust",
							PriorityClassName: "low-trust-class",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("333m"),
									corev1.ResourceMemory: resource.MustParse("1500M"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("50m"),
									corev1.ResourceMemory: resource.MustParse("750M"),
								},
							},
						},
						{
							Name:              "high-trust",
							CockroachImage:    "CUSTOMREPO:TENANTPOOLOVERRIDEIMAGEVERSION",
							PriorityClassName: "high-trust-class",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("5"),
									corev1.ResourceMemory: resource.MustParse("10Gi"),
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		// Default region if a list was not supplied.
		if len(tc.Spec.Regions) == 0 {
			tc.Spec.Regions = []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east1", Nodes: 5},
			}
		}
		networkPlugin := cloud.Calico
		if tc.UseCilium {
			networkPlugin = cloud.Cilium
		}
		env := tc.Env
		if env == nil {
			env = &cloud.Environment{
				CloudRegion:   "us-east1",
				CloudProvider: cloud.GCP,
			}
		}
		manifest := TenantPools(&tc.Spec, env, networkPlugin)

		actual, err := json.MarshalIndent(manifest, "", "\t")
		assert.NoError(t, err)

		testutils.AssertGoldenJSON(t, tc.Name, actual)

		// Explicitly check deployment's CockroachImage field for select cases.
		switch tc.Name {
		case "tenantPoolImage-overrides":
			checkManifestForDeploymentImageName(
				"CUSTOMREPO:TENANTPOOLOVERRIDEIMAGEVERSION", "tenantpool-low-trust", "cockroachdb", manifest, t,
			)
		case "tenantOtelCollectorImage-overrides":
			checkManifestForDeploymentImageName(
				"custom-otel-image", "tenantpool-low-trust", "otelcol-agent", manifest, t,
			)
		case "customPools":
			checkManifestForDeploymentImageName(
				"default:image", "tenantpool-low-trust", "cockroachdb", manifest, t,
			)
			checkManifestForDeploymentImageName(
				"CUSTOMREPO:TENANTPOOLOVERRIDEIMAGEVERSION", "tenantpool-high-trust", "cockroachdb", manifest, t,
			)
		default:
		}
	}
}

func TestCrdbClusterQuiescentDuration(t *testing.T) {
	noGracePeriod := &crdbv1beta1.CrdbCluster{
		Spec: crdbv1beta1.CrdbClusterSpec{
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{
					Code:  "us-east1",
					Nodes: 3,
				},
			},
		},
	}

	options := &crdbv1beta1.CrdbCluster{
		Spec: crdbv1beta1.CrdbClusterSpec{
			RollingRestartDelay: &metav1.Duration{
				Duration: time.Minute * 2,
			},
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{
					Code:  "us-east1",
					Nodes: 3,
				},
				{
					Code:  "us-west1",
					Nodes: 2,
				},
			},
		},
	}

	minutes := func(minutes float64) time.Duration {
		return time.Duration(float64(time.Minute) * minutes)
	}

	// (grace period + 5 minutes drain duration) * # nodes * 1.2 weight
	require.Equal(t, minutes((1+45)*3*1.2), CrdbClusterQuiescentDuration(noGracePeriod, "us-east1"))
	require.Equal(t, minutes((1+45)*1*1.2), CrdbClusterQuiescentDuration(noGracePeriod, "made-up-region"))
	require.Equal(t, minutes((2+45)*3*1.2), CrdbClusterQuiescentDuration(options, "us-east1"))
	require.Equal(t, minutes((2+45)*2*1.2), CrdbClusterQuiescentDuration(options, "us-west1"))
	require.Equal(t, minutes((2+45)*1*1.2), CrdbClusterQuiescentDuration(options, "made-up-region"))
}

package clusterctrl

import (
	"context"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDeployHostResources(t *testing.T) {
	now := time.Date(2020, 2, 1, 12, 0, 0, 0, time.UTC)
	defer testutils.HookGlobal(&timeutil.Now, func() time.Time { return now })()

	ctx := context.Background()
	namespace := "test-ns"

	// K8s API endpoint is used in deployment.
	k8sEndpoint := corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "kubernetes"},
		Subsets: []corev1.EndpointSubset{
			{Addresses: []corev1.EndpointAddress{{IP: "127.0.0.1"}}},
		},
	}

	host := crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: namespace,
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Template: crdbv1beta1.CrdbNodeTemplate{
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image: "us-docker.pkg.dev/cockroach-cloud-images/cockroachdb/cockroach:v24.2.0-cloudonly.1",
				},
			},
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east1", Nodes: 3},
			},
			TLSEnabled: true,
			TenantHost: &crdbv1beta1.TenantHost{
				TenantPools: []crdbv1beta1.TenantPool{
					{
						Name:                  "test-pool",
						HostReplicaMultiplier: new(float32(2)),
						LoggingConfigMapName:  "cockroachdb-logging-tenant-123",
					},
				},
			},
		},
	}

	opts := kube.Options{BeforeMutation: kubetest.MakeQuiescent}
	r := &Reconciler{
		Env: &cloud.Environment{CloudProvider: cloud.GCP, CloudRegion: "us-east1"},
	}

	t.Run("default", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{Options: opts}, &k8sEndpoint, &host).WithNamespace(namespace)
		require.NoError(t, kubetest.MakeQuiescent(&host))
		h := requestHandler{reconciler: r, ctx: ctx, ctl: ctl, cluster: &host}
		require.NoError(t, h.DeployHostResources())

		actual := kubetest.DumpAll(t, ctl, &appsv1.DeploymentList{})
		testutils.AssertGoldenJSON(t, "Resources", actual)
	})

	// TOOD(jaylim-crl): Remove this subtest once all host clusters are no longer
	// on v24.1.
	t.Run("v24.1 host cluster", func(t *testing.T) {
		defer func(oldImage string) { host.Spec.Template.Spec.Image = oldImage }(host.Spec.Template.Spec.Image)
		host.Spec.Template.Spec.Image = "us-docker.pkg.dev/cockroach-cloud-images/cockroachdb/cockroach:v24.1.2-2-g5a68aaa15d7"

		ctl := kubetest.NewFakeCtl(kubetest.Options{Options: opts}, &k8sEndpoint, &host).WithNamespace(namespace)
		h := requestHandler{reconciler: r, ctx: ctx, ctl: ctl, cluster: &host}
		require.NoError(t, h.DeployHostResources())

		actual := kubetest.DumpAll(t, ctl, &appsv1.DeploymentList{})
		testutils.AssertGoldenJSON(t, "Resources", actual)
	})

	t.Run("Overwrite=true for host cluster resources", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{Options: opts}, &k8sEndpoint, &host).WithNamespace(namespace)
		h := requestHandler{reconciler: r, ctx: ctx, ctl: ctl, cluster: &host}
		require.NoError(t, h.DeployHostResources())

		// Deliberately introduce conflicts to host cluster resources.
		var sqlproxy appsv1.Deployment
		require.NoError(t, ctl.Get(ctx, "sqlproxy", &sqlproxy))
		sqlproxy.Spec.Template.Spec.ServiceAccountName = "another-name"
		updateOpts := kube.UpdateOptions{NoLastAppliedAnnotation: true}
		require.NoError(t, ctl.UpdateWithOptions(ctx, updateOpts, &sqlproxy))

		// Deploy again and no conflicts because we used Overwrite=true.
		require.NoError(t, h.DeployHostResources())
	})

	t.Run("with cilium", func(t *testing.T) {
		cnpCrd := apiextv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ciliumnetworkpolicies.cilium.io",
				Namespace: namespace,
			},
		}
		ccnpCrd := apiextv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ciliumclusterwidenetworkpolicies.cilium.io",
				Namespace: namespace,
			},
		}

		ctl := kubetest.NewFakeCtl(kubetest.Options{Options: opts}, &k8sEndpoint, &host, &cnpCrd, &ccnpCrd).WithNamespace(namespace)
		h := requestHandler{reconciler: r, ctx: ctx, ctl: ctl, cluster: &host}
		require.NoError(t, h.DeployHostResources())

		actual := kubetest.DumpAll(t, ctl, &appsv1.DeploymentList{})
		testutils.AssertGoldenJSON(t, "Resources", actual)
	})
}

func TestCheckPoolsNeedsUpgrade(t *testing.T) {
	ctx := context.Background()

	image := "cockroachdb/cockroach:v21.1.1"
	poolName := "test-pool"
	cluster := crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foobar",
			Namespace: "default",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Template: crdbv1beta1.CrdbNodeTemplate{
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image: image,
					Flags: map[string]string{
						"--max-offset": "250ms",
					},
				},
			},
			TenantHost: &crdbv1beta1.TenantHost{
				TenantPools: []crdbv1beta1.TenantPool{
					{Name: poolName},
				},
			},
			TLSEnabled: true,
		},
	}
	tenantPoolDeployment := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      manifests.MakeTenantPoolName(poolName),
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  manifests.CockroachDB,
							Image: image,
						},
					},
				},
			},
		},
	}

	t.Run("pools not created yet - no upgrade", func(t *testing.T) {
		crdbCluster := cluster
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &crdbCluster)
		h := requestHandler{reconciler: &Reconciler{}, ctx: ctx, ctl: ctl, cluster: &crdbCluster}
		needsUpgrade, err := h.checkPoolsNeedUpgrade()
		require.NoError(t, err)
		require.False(t, needsUpgrade)
	})

	t.Run("default pool has invalid image - upgrade", func(t *testing.T) {
		crdbCluster := cluster
		pool := tenantPoolDeployment
		containers := []corev1.Container{{Name: manifests.CockroachDB, Image: "foobar"}}
		pool.Spec.Template.Spec.Containers = containers
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &crdbCluster, &pool)
		h := requestHandler{reconciler: &Reconciler{}, ctx: ctx, ctl: ctl, cluster: &crdbCluster}
		needsUpgrade, err := h.checkPoolsNeedUpgrade()
		require.NoError(t, err)
		require.True(t, needsUpgrade)
	})

	t.Run("pool with image override has invalid image - upgrade", func(t *testing.T) {
		crdbCluster := cluster
		tenantHost := &crdbv1beta1.TenantHost{
			TenantPools: []crdbv1beta1.TenantPool{
				{Name: poolName, CockroachImage: "abcdef"},
			},
		}
		crdbCluster.Spec.TenantHost = tenantHost
		pool := tenantPoolDeployment
		containers := []corev1.Container{{Name: manifests.CockroachDB, Image: "foobar"}}
		pool.Spec.Template.Spec.Containers = containers
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &crdbCluster, &pool)
		h := requestHandler{reconciler: &Reconciler{}, ctx: ctx, ctl: ctl, cluster: &crdbCluster}
		needsUpgrade, err := h.checkPoolsNeedUpgrade()
		require.NoError(t, err)
		require.True(t, needsUpgrade)
	})

	t.Run("default pool has correct image - no upgrade", func(t *testing.T) {
		crdbCluster := cluster
		pool := tenantPoolDeployment
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &crdbCluster, &pool)
		h := requestHandler{reconciler: &Reconciler{}, ctx: ctx, ctl: ctl, cluster: &crdbCluster}
		needsUpgrade, err := h.checkPoolsNeedUpgrade()
		require.NoError(t, err)
		require.False(t, needsUpgrade)
	})

	t.Run("pool with image override has correct image - no upgrade", func(t *testing.T) {
		crdbCluster := cluster
		tenantHost := &crdbv1beta1.TenantHost{
			TenantPools: []crdbv1beta1.TenantPool{
				{Name: poolName, CockroachImage: image},
			},
		}
		crdbCluster.Spec.TenantHost = tenantHost
		pool := tenantPoolDeployment
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &crdbCluster, &pool)
		h := requestHandler{reconciler: &Reconciler{}, ctx: ctx, ctl: ctl, cluster: &crdbCluster}
		needsUpgrade, err := h.checkPoolsNeedUpgrade()
		require.NoError(t, err)
		require.False(t, needsUpgrade)
	})
}

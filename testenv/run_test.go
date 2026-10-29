package testenv

import (
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbtest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubenet"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestIntegrationRun is a dummy integration test used for starting a local k3d cluster
// with the operatar deployed in the default namespace.
//
// You can get this setup by running `make run`.
func TestIntegrationRun(t *testing.T) {
	opts := []EnvOption{SkipLeakTest}
	// just start the testenv and bail
	New(t, MultiZoneConfig, opts...)
}

func TestIntegrationRunDedicated(t *testing.T) {
	testutils.MarkIntegrationTest(t)

	env := New(t, MultiZoneConfig, SkipLeakTest)

	namespace := EnsureNamespace(t, env.DefaultCluster().Ctl)
	ctl := env.DefaultCluster().Ctl.WithNamespace(namespace)

	kubecrdbtest.CreateCluster(t, ctl, &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: namespace,
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			TLSEnabled: true,
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east-1", Nodes: 3, CloudProvider: "k3d"},
			},
		},
	})
}

func TestIntegrationRunServerless(t *testing.T) {
	testutils.MarkIntegrationTest(t)

	env := New(t, MultiZoneConfig)
	cluster := CreateCrdbCluster(t, env, "host", &crdbv1beta1.CrdbClusterSpec{
		TLSEnabled: true,
		TenantHost: &crdbv1beta1.TenantHost{},
		Features: []crdbv1beta1.ClusterFeature{
			crdbv1beta1.ClusterExplicitNodePlacement,
		},
		Template: crdbv1beta1.CrdbNodeTemplate{
			Spec: crdbv1beta1.CrdbNodeSpec{
				ConditionalResourceRequirements: manifests.HostConditionalResourceRequirements(),
			},
		},
	})
	ctl := env.DefaultCluster().NewCtl(cluster.Namespace)
	type tenantConfig struct {
		name string
		id   uint64
	}
	for _, tenant := range []tenantConfig{
		{"happy-koala", 10},
		{"scary-bear", 11},
		{"eager-beaver", 12},
	} {
		options := kubenet.Options{
			TenantID: crdbv1beta1.TenantID(tenant.id),
		}
		CreateCrdbTenant(t, ctl, tenant.name, &crdbv1beta1.CrdbTenantSpec{
			TenantID: options.TenantID,
			Pods:     1,
		})
		ExecKubeSql(t, ctl, options, "CREATE USER me WITH PASSWORD 'password'")
	}
}

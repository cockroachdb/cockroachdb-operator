package betaclusterctrl_test

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbtest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/infrastructure"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/testenv"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
)

func TestIntegrationNodeReplacement(t *testing.T) {
	// This test simulates the replacment of a k8s node in a node pool. A
	// random zone is extended to include an extra node, then the original node
	// is deleted. The test fails if the cluster is unable to reach full
	// availability within three minutes of adding the additional node.
	testutils.MarkIntegrationTest(t)

	ctx := context.Background()
	env := testenv.New(t, testenv.EnvConfig{
		Name: "node-replacement-test",
		Regions: testenv.Regions{
			"us-east-1": testenv.Zones{
				"us-east-1a": 1,
				"us-east-1b": 1,
				"us-east-1c": 1,
			},
		},
	}, testenv.SkipLeakTest)

	namespace := testenv.EnsureNamespace(t, env.DefaultCluster().Ctl)
	ctl := env.DefaultCluster().Ctl.WithNamespace(namespace)

	crdbCluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: namespace,
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			TLSEnabled: true,
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east-1", Nodes: 3, CloudProvider: "k3d"},
			},
			Features: []crdbv1beta1.ClusterFeature{
				crdbv1beta1.ClusterExplicitNodePlacement,
			},
		},
	}
	kubecrdbtest.CreateCluster(t, ctl, crdbCluster)
	require.NoError(t, ctl.WaitFor(ctx, kube.IsQuiescent, crdbCluster))

	var nodes corev1.NodeList
	require.NoError(t, ctl.List(ctx, &kube.ListOptions{
		FieldSelector: fields.ParseSelectorOrDie("spec.unschedulable=false"),
	}, &nodes))
	require.Len(t, nodes.Items, 3)

	vm, err := infrastructure.NodeToVM(&nodes.Items[rand.Intn(len(nodes.Items))])
	require.NoError(t, err)

	cluster := env.DefaultCluster()

	require.NoError(t, cluster.Hardware.Resize(ctx, vm.AvailabilityZone, 2))
	t.Log("removing node ", vm.Name)
	require.NoError(t, cluster.Hardware.RemoveNode(ctx, vm))

	isHealthy := func() error {
		var pods corev1.PodList
		require.NoError(t, ctl.List(ctx, &kube.ListOptions{
			LabelSelector: kube.ParseLabelOrDie("app=cockroachdb"),
		}, &pods))

		if len(pods.Items) != 3 {
			return errors.Newf("found %d pods expected 3", len(pods.Items))
		}

		for _, pod := range pods.Items {
			if pod.Spec.NodeName == vm.Name {
				return errors.Newf("pod '%s' is still placed on the deleted node '%s'", pod.Spec.NodeName, vm.Name)
			}
			if err := kube.IsPodReady(&pod); err != nil {
				return errors.Wrapf(err, "waiting for crdb pod '%s' to be ready", pod.Name)
			}
		}

		return nil
	}

	t.Log("waiting for cluster to recover from node removal")
	require.Eventually(t, func() bool {
		err := isHealthy()
		if err != nil {
			t.Log(err)
		}
		return err == nil
	}, 3*time.Minute, 5*time.Second)
}

package cloud

import (
	"context"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGetNetworkPluginForCluster(t *testing.T) {
	ctx := context.Background()

	t.Run("cilium", func(t *testing.T) {
		cnpCrd := apiextv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ciliumnetworkpolicies.cilium.io",
				Namespace: "default",
			},
			Spec: apiextv1.CustomResourceDefinitionSpec{
				Group: "cilium.io",
			},
		}
		ccnpCrd := apiextv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ciliumclusterwidenetworkpolicies.cilium.io",
				Namespace: "default",
			},
			Spec: apiextv1.CustomResourceDefinitionSpec{
				Group: "cilium.io",
			},
		}

		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &cnpCrd, &ccnpCrd).WithNamespace("default")
		plugin, err := GetNetworkPluginForCluster(ctx, ctl)
		require.NoError(t, err)
		require.Equal(t, Cilium, plugin)
	})

	t.Run("calico", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{})
		plugin, err := GetNetworkPluginForCluster(ctx, ctl)
		require.NoError(t, err)
		require.Equal(t, Calico, plugin)
	})
}

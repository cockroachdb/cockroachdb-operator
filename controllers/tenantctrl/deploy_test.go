package tenantctrl

import (
	"context"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/stretchr/testify/require"
	netv1 "k8s.io/api/networking/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDeploy(t *testing.T) {
	ctx := context.Background()
	namespace := "test-ns"

	const tenantID = crdbv1beta1.TenantID(20)
	tenant := crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantID.Tag(),
			Namespace: namespace,
		},
		Spec: crdbv1beta1.CrdbTenantSpec{
			TenantID: tenantID,
		},
	}

	opts := kube.Options{BeforeMutation: kubetest.MakeReady}

	t.Run("success", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{Options: opts}).WithNamespace(namespace)
		h := requestHandler{ctx: ctx, ctl: ctl, tenant: &tenant}
		require.NoError(t, h.Deploy())

		actual := kubetest.DumpAll(t, ctl, &netv1.NetworkPolicyList{})
		testutils.AssertGoldenJSON(t, "Resources", actual)
	})

	t.Run("success with cilium", func(t *testing.T) {
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
		ctl := kubetest.NewFakeCtl(kubetest.Options{Options: opts}, &cnpCrd, &ccnpCrd).WithNamespace(namespace)
		h := requestHandler{ctx: ctx, ctl: ctl, tenant: &tenant}
		require.NoError(t, h.Deploy())

		// TODO(jaylim-crl): Uncomment these once we support structured types
		// for cilium network policies. At the moment, DumpAll only works with
		// structured types.
		//
		// actual := kubetest.DumpAll(t, ctl)
		// testutils.AssertGoldenJSON(t, "Resources", actual)
	})
}

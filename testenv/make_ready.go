package testenv

import (
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
)

func init() {
	// Register custom MakeReady handlers for CRDs.
	kubetest.AddCustomReadyHandler(func(object kube.Object) error {
		switch t := object.(type) {
		case *crdbv1beta1.CrdbCluster:
			crdbv1beta1.MakeCrdbClusterReady(t)
		case *crdbv1beta1.CrdbNode:
			crdbv1beta1.MakeCrdbNodeReady(t)
		case *crdbv1beta1.CrdbTenant:
			crdbv1beta1.MakeCrdbTenantReady(t)
		}
		return nil
	})
}

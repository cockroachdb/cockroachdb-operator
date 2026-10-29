package testenv

import (
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
)

func init() {
	// Register custom MakeQuiescent handlers for CRDs.
	kubetest.AddCustomQuiescentHandler(func(object kube.Object) error {
		switch t := object.(type) {
		case *crdbv1beta1.CrdbCluster:
			crdbv1beta1.MakeCrdbClusterQuiescent(t)
		}
		return nil
	})
}

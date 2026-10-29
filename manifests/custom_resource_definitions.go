package manifests

import (
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests/internal"
)

// CustomResourceDefinitions returns the K8s manifest for the operator's custom
// resource definitions: CrdbCluster, CrdbNode, and CrdbTenant.
func CustomResourceDefinitions() []kube.Object {
	clustersCRD := internal.DecodeYAML("yaml/crdb.cockroachlabs.com_crdbclusters.yaml")
	nodesCRD := internal.DecodeYAML("yaml/crdb.cockroachlabs.com_crdbnodes.yaml")
	tenantsCRD := internal.DecodeYAML("yaml/crdb.cockroachlabs.com_crdbtenants.yaml")
	return append(clustersCRD, append(nodesCRD, tenantsCRD...)...)
}

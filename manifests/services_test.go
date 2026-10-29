package manifests

import (
	"encoding/json"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNodeService(t *testing.T) {
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "crdb-test",
			Namespace: "bogus-ns",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Template: crdbv1beta1.CrdbNodeTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"nodeLabel1": "value1",
						"nodeLabel2": "value2",
					},
				},
			},
		},
	}

	enc, err := json.MarshalIndent(NodeService(cluster), "", "\t")
	require.NoError(t, err)
	testutils.AssertGoldenJSON(t, "", enc)
}

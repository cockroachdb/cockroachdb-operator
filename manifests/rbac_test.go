package manifests

import (
	"encoding/json"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/stretchr/testify/assert"
)

func TestRBAC(t *testing.T) {
	testCases := []struct {
		Name    string
		Options RbacOptions
	}{
		{
			Name: "default namespace",
			Options: RbacOptions{
				Namespace: "default",
				ServiceAccountAnnotations: map[string]string{
					"iam.gke.io/gcp-service-account": "crdb-wi-b3ffc8653b8b@cockroach-test.iam.gserviceaccount.com",
				},
			},
		},
		{
			Name: "namespace-qualified cluster role",
			Options: RbacOptions{
				Namespace:          "my-ns",
				ServiceAccountName: "cockroachdb",
				ClusterRoleName:    "my-ns-cockroachdb",
			},
		},
	}

	for _, tc := range testCases {
		mani := RBAC(tc.Options)

		actual, err := json.MarshalIndent(mani, "", "\t")
		assert.NoError(t, err)

		testutils.AssertGoldenJSON(t, tc.Name, actual)
	}
}

package manifests

import (
	"encoding/json"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/stretchr/testify/assert"
)

func TestCockroachDBInitJob(t *testing.T) {
	testCases := []struct {
		Name    string
		Options CockroachInitJobOptions
	}{
		{
			Name: "insecure",
			Options: CockroachInitJobOptions{
				CockroachImage: "cockroachdb/cockroach:v20.2",
				Secure:         false,
			},
		},
		{
			Name: "secure",
			Options: CockroachInitJobOptions{
				CockroachImage: "cockroachdb/cockroach:v20.2",
				Secure:         true,
			},
		},
		{
			Name: "secure with SecurityContexts",
			Options: CockroachInitJobOptions{
				CockroachImage: "cockroachdb/cockroach:v21.2.13",
				Secure:         true,
			},
		},
	}

	for _, tc := range testCases {
		mani := CockroachInitJob(tc.Options)

		actual, err := json.MarshalIndent(mani, "", "\t")
		assert.NoError(t, err)

		testutils.AssertGoldenJSON(t, tc.Name, actual)
	}
}

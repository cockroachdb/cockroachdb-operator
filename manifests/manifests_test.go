package manifests

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/stretchr/testify/require"
)

func manifestContains(t *testing.T, manifest []kube.Object, contains string) bool {
	actual, err := json.MarshalIndent(manifest, "", "\t")
	require.NoError(t, err)
	return strings.Contains(string(actual), contains)
}

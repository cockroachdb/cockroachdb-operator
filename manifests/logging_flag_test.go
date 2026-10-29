package manifests

import (
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/stretchr/testify/require"
)

func TestStderrLogFlag_VersionGated(t *testing.T) {
	nodeWithImage := func(image string) *crdbv1beta1.CrdbNode {
		n := &crdbv1beta1.CrdbNode{}
		n.Spec.Image = image
		return n
	}

	const v263 = `'sinks: {stderr: {channels: {INFO: "HEALTH,OPS,DEV,STORAGE"}}}'`
	const legacy = `'sinks: {stderr: {channels: {INFO: "HEALTH,OPS", WARNING: "STORAGE,DEV"}}}'`

	// v26.3+ lists STORAGE and DEV at INFO since they cannot sit at WARNING.
	require.Equal(t, v263, stderrLogFlag(nodeWithImage("cockroachdb/cockroach:v26.3.0-alpha.1")))
	require.Equal(t, v263, stderrLogFlag(nodeWithImage("cockroachdb/cockroach:v27.1.0")))

	// Older versions keep the quieter WARNING placement.
	require.Equal(t, legacy, stderrLogFlag(nodeWithImage("cockroachdb/cockroach:v25.4.0")))

	// An unparseable or missing image falls back to the legacy form.
	require.Equal(t, legacy, stderrLogFlag(nodeWithImage("cockroachdb/cockroach:latest")))
	require.Equal(t, legacy, stderrLogFlag(&crdbv1beta1.CrdbNode{}))
}

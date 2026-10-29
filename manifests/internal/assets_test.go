package internal_test

import (
	"testing"

	. "github.com/cockroachlabs/cockroach-enterprise-operator/manifests/internal"
	"github.com/stretchr/testify/require"
)

func TestDecodeYAML(t *testing.T) {
	t.Run("panics when asset not found", func(t *testing.T) {
		require.Panics(t, func() { DecodeYAML("whodis") })
	})
}

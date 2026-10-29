package otelcompat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConvertOtelID(t *testing.T) {
	require.Equal(t, "", ToDatadogId(""))
	require.Equal(t, "", ToDatadogId("1234"))
	require.Equal(t, "", ToDatadogId("1234"))
	// Actual values (TM) pulled from Staging Pod Logs.
	require.Equal(t, "10254644363869772871", ToDatadogId("8e4fd0b7ae6b5c47"))
	require.Equal(t, "6535961652189257153", ToDatadogId("0c3d5e2e226d15725ab4667e6050ddc1"))
}

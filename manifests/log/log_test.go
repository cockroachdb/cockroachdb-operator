package log

import (
	"fmt"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/cockroach/logconfig"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/version"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
)

func TestLoggingConfigMap(t *testing.T) {
	t.Run("dedicated", func(t *testing.T) {
		for _, tc := range []struct {
			image string
		}{
			{
				image: "v22.1.1",
			},
			{
				image: "v22.2.1",
			},
			{
				image: "v23.1.1",
			},
		} {
			t.Run(tc.image, func(t *testing.T) {
				cm := LoggingConfigMap(NewLoggingConfigMapOpts(fmt.Sprintf(":%s", tc.image)))

				b, err := yaml.Marshal(&cm)
				require.NoError(t, err)

				testutils.AssertGolden(t, "configmap", b)
			})
		}
	})

	t.Run("tenant", func(t *testing.T) {
		cm := TenantLoggingConfigMap(NewTenantLoggingConfigMapOpts())

		b, err := yaml.Marshal(&cm)
		require.NoError(t, err)

		testutils.AssertGolden(t, "configmap", b)
	})
}

func TestCrdbImageSupportsLogBuffering(t *testing.T) {
	for _, tc := range []struct {
		image     string
		panics    bool
		supported bool
	}{
		{
			image:  "cockroachdb/cockroach",
			panics: true,
		},
		{
			image:     "cockroachdb/cockroach:NotAVersion",
			supported: false,
		},
		{
			image:     "cockroachdb/cockroach:v20.2.0",
			supported: false,
		},
		{
			image:     "cockroachdb/cockroach:v21.1.0",
			supported: false,
		},
		{
			image:     "cockroachdb/cockroach:v21.2.0",
			supported: false,
		},
		{
			image:     "cockroachdb/cockroach:v22.1.2",
			supported: false,
		},
		{
			image:     "cockroachdb/cockroach:v22.1.3",
			supported: true,
		},
		{
			image:     "cockroachdb/cockroach:v22.2.0",
			supported: true,
		},
	} {
		t.Run(tc.image, func(t *testing.T) {
			if tc.panics {
				require.Panics(t, func() { CrdbImageSupportsLogBuffering(tc.image) })
			} else {
				require.Equal(t, tc.supported, CrdbImageSupportsLogBuffering(tc.image))
			}
		})
	}
}

func TestCrdbVersionSupportsLogBuffering(t *testing.T) {
	for _, tc := range []struct {
		version   version.Version
		supported bool
	}{
		{
			version:   version.Version{},
			supported: false,
		},
		{
			version:   version.MustParse("v20.2.0"),
			supported: false,
		},
		{
			version:   version.MustParse("v21.1.0"),
			supported: false,
		},
		{
			version:   version.MustParse("v21.2.0"),
			supported: false,
		},
		{
			version:   version.MustParse("v22.1.2"),
			supported: false,
		},
		{
			version:   version.MustParse("v22.1.3"),
			supported: true,
		},
		{
			version:   version.MustParse("v22.2.0"),
			supported: true,
		},
	} {
		t.Run(tc.version.String(), func(t *testing.T) {
			require.Equal(t, tc.supported, CrdbVersionSupportsLogBuffering(tc.version))
		})
	}
}

func TestBufferingEnabledConfigOpts(t *testing.T) {
	loggingOpts := NewLoggingConfigMapOpts("cockroachdb/cockroach:v24.1.0")

	configMap := LoggingConfigMap(loggingOpts)

	logConfigStr, ok := configMap.Data["logs.yaml"]

	logConfigBytes := []byte(logConfigStr)

	require.True(t, ok, "unable to retrieve logs.yaml")

	config := logconfig.Config{}
	require.NoError(t, yaml.Unmarshal(logConfigBytes, &config))
	require.True(t, *config.FileDefaults.BufferedWrites)

	// These values should not be nil or set
	require.Nil(t, config.FileDefaults.Buffering.MaxBufferSize)
	require.Nil(t, config.FileDefaults.Buffering.MaxStaleness)
	require.Nil(t, config.FileDefaults.Buffering.FlushTriggerSize)

	// Set Bufferring to be true
	loggingOpts.UseBuffering = true
	configMap = LoggingConfigMap(loggingOpts)
	logConfigStr, ok = configMap.Data["logs.yaml"]
	logConfigBytes = []byte(logConfigStr)
	require.True(t, ok, "unable to retrieve logs.yaml")

	config = logconfig.Config{}
	require.NoError(t, yaml.Unmarshal(logConfigBytes, &config))
	require.False(t, *config.FileDefaults.BufferedWrites)

	// With buffered write disabled these fields should all be set since we are using bufferring
	require.NotNil(t, config.FileDefaults.Buffering.MaxBufferSize)
	require.NotNil(t, config.FileDefaults.Buffering.MaxStaleness)
	require.NotNil(t, config.FileDefaults.Buffering.FlushTriggerSize)
}

func TestCrdbImageStorageDevAtInfo(t *testing.T) {
	// v26.3+ implicitly logs STORAGE/DEV at INFO, so the stderr sink must list
	// them at INFO rather than WARNING.
	require.True(t, crdbImageStorageDevAtInfo("cockroachdb/cockroach:v26.3.0-alpha.1"))
	require.True(t, crdbImageStorageDevAtInfo("cockroachdb/cockroach:v27.1.0"))
	// Older versions keep the legacy WARNING placement.
	require.False(t, crdbImageStorageDevAtInfo("cockroachdb/cockroach:v25.4.0"))
	// Unparseable or imageless inputs fall back to the legacy behavior.
	require.False(t, crdbImageStorageDevAtInfo("cockroachdb/cockroach:latest"))
	require.False(t, crdbImageStorageDevAtInfo("noversion"))
}

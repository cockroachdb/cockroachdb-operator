package tenant

import (
	gozip "archive/zip"
	"bytes"
	"context"
	"io"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestFetchSQLPodData(t *testing.T) {
	const tenantID crdbv1beta1.TenantID = 13
	tenant := crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name: tenantID.Tag(),
		},
		Spec: crdbv1beta1.CrdbTenantSpec{
			TenantID: tenantID,
		},
	}

	// The operator retrieves a default configuration for the fluentbit sidecar
	// when stamping tenant pool pods.
	defaultFluentbitTenantConfig := manifests.GetTestTenantNoopConfigMap()
	defaultFluentbitTenantConfig.Namespace = "default"
	fluentConf := []byte(defaultFluentbitTenantConfig.Data["fluent-bit.conf"])
	inputsConf := []byte(defaultFluentbitTenantConfig.Data["inputs.conf"])
	outputsConf := []byte(defaultFluentbitTenantConfig.Data["outputs.conf"])

	// Tenant specific fluentbit configmap.
	fluentbitTenant13Config := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fluent-bit-tenant-13",
			Namespace: "default",
		},
		Data: map[string]string{
			"fluent-bit.conf": "1",
			"inputs.conf":     "2",
			"outputs.conf":    "3",
		},
	}

	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{}, defaultFluentbitTenantConfig, fluentbitTenant13Config)

	locality := cloud.LocalityInfo{
		Region: "k3d-us-central1",
		Zone:   "k3d-zone1",
	}
	t.Run("no tenant certs", func(t *testing.T) {
		data, err := fetchSQLPodData(ctx, ctl, &tenant, nil /* credentials */, locality)
		require.Error(t, err)
		require.Regexp(t, "constructing cockroach-certs: secrets .* not found", err.Error())
		require.Nil(t, data)
	})

	// Make tenant certs.
	makeTenantCerts(t, ctl, tenantID)

	validateCertData := func(t *testing.T, data map[string][]byte) {
		require.Equal(t, testCACert[0:len(testCACert):len(testCACert)], data["cockroach/cockroach-certs/ca.crt"])
		require.Equal(t, []byte{7, 8}, data["cockroach/cockroach-certs/client-tenant.13.crt"])
		require.Equal(t, []byte{9, 10}, data["cockroach/cockroach-certs/client-tenant.13.key"])
		require.Equal(t, []byte{11, 12}, data["cockroach/cockroach-certs/tenant-signing.13.crt"])
		require.Equal(t, []byte{13, 14}, data["cockroach/cockroach-certs/tenant-signing.13.key"])
		require.Equal(t, []byte{15, 16}, data["cockroach/cockroach-certs/client.root.crt"])
		require.Equal(t, []byte{17, 18}, data["cockroach/cockroach-certs/client.root.key"])
		require.Equal(t, []byte("region=k3d-us-central1,az=k3d-zone1\n"), data["cockroach/cockroach-certs/cockroach-locality"])
		require.Equal(t, []byte("13\n"), data["cockroach/cockroach-certs/STARTER_CHECKPOINT"])
	}

	validateDefaultFluentConfigData := func(t *testing.T, data map[string][]byte) {
		require.Equal(t, fluentConf, data["cockroach/fluent-bit-data/fluent-bit.conf"])
		require.Equal(t, inputsConf, data["cockroach/fluent-bit-data/inputs.conf"])
		require.Equal(t, outputsConf, data["cockroach/fluent-bit-data/outputs.conf"])
		require.Equal(t, []byte("13\n"), data["cockroach/fluent-bit-data/STARTER_CHECKPOINT"])
	}

	t.Run("successful/OtelCollector disabled", func(t *testing.T) {
		data, err := fetchSQLPodData(ctx, ctl, &tenant, nil /* credentials */, locality)
		require.NoError(t, err)
		require.Equal(t, 13, len(data))

		validateCertData(t, data)
		validateDefaultFluentConfigData(t, data)
	})

	// Enable otel collector.
	tenant.Spec.OtelCollector = &crdbv1beta1.OtelCollector{
		ConfigMapName: "otelcol-agent-tenant-13",
	}

	t.Run("no OtelCollector config", func(t *testing.T) {
		data, err := fetchSQLPodData(ctx, ctl, &tenant, nil /* credentials */, locality)
		require.Error(t, err)
		require.EqualError(t, err, `constructing otelcol-data: configmaps "otelcol-agent-tenant-13" not found`)
		require.Nil(t, data)
	})

	// Make otel collector config without secrets.
	makeOtelCollectorConfig(t, ctl, tenant.Spec.OtelCollector.ConfigMapName, "")

	t.Run("successful/OtelCollector enabled without secrets", func(t *testing.T) {
		data, err := fetchSQLPodData(ctx, ctl, &tenant, nil /* credentials */, locality)
		require.NoError(t, err)
		require.Equal(t, 15, len(data))

		validateCertData(t, data)

		require.Equal(t, []byte("foo-bar-baz"), data["cockroach/otelcol-data/otelcol-agent-config.yaml"])
		require.Equal(t, []byte("13\n"), data["cockroach/otelcol-data/STARTER_CHECKPOINT"])

		validateDefaultFluentConfigData(t, data)
	})

	tenant.Spec.OtelCollector = &crdbv1beta1.OtelCollector{
		ConfigMapName: "otelcol-agent-tenant-13",
		SecretName:    "test-secret-13",
	}

	t.Run("no OtelCollector secret", func(t *testing.T) {
		data, err := fetchSQLPodData(ctx, ctl, &tenant, nil /* credentials */, locality)
		require.Error(t, err)
		require.EqualError(t, err, `constructing otelcol-data: secrets "test-secret-13" not found`)
		require.Nil(t, data)
	})

	// Make otel collector config with secrets.
	makeOtelCollectorConfig(t, ctl, tenant.Spec.OtelCollector.ConfigMapName, tenant.Spec.OtelCollector.SecretName)

	t.Run("successful/OtelCollector enabled with secrets", func(t *testing.T) {
		data, err := fetchSQLPodData(ctx, ctl, &tenant, nil /* credentials */, locality)
		require.NoError(t, err)
		require.Equal(t, 16, len(data))

		validateCertData(t, data)

		require.Equal(t, []byte("foo-bar-baz"), data["cockroach/otelcol-data/otelcol-agent-config.yaml"])
		require.Equal(t, []byte{100, 200}, data["cockroach/otelcol-data/API_KEY.env"])
		require.Equal(t, []byte("13\n"), data["cockroach/otelcol-data/STARTER_CHECKPOINT"])

		validateDefaultFluentConfigData(t, data)
	})

	t.Run("AWSMetricsRoleARN", func(t *testing.T) {
		tenant.Spec.OtelCollector.AWSMetricsRoleARN = "foo-arn"
		defer func() { tenant.Spec.OtelCollector.AWSMetricsRoleARN = "" }()

		t.Run("successful/no credentials", func(t *testing.T) {
			data, err := fetchSQLPodData(ctx, ctl, &tenant, nil /* credentials */, locality)
			require.NoError(t, err)
			require.Equal(t, 13, len(data))

			validateCertData(t, data)
			validateDefaultFluentConfigData(t, data)
		})

		t.Run("credentials could not be found", func(t *testing.T) {
			data, err := fetchSQLPodData(ctx, ctl, &tenant, make(map[string][]byte), locality)
			require.Error(t, err)
			require.EqualError(t, err, `constructing otelcol-data: could not locate AWS metrics credentials for "foo-arn"`)
			require.Nil(t, data)
		})

		t.Run("successful", func(t *testing.T) {
			credentials := make(map[string][]byte)
			credentials["foo-arn"] = []byte("test-credentials")
			data, err := fetchSQLPodData(ctx, ctl, &tenant, credentials, locality)
			require.NoError(t, err)
			require.Equal(t, 17, len(data))

			validateCertData(t, data)

			require.Equal(t, []byte("foo-bar-baz"), data["cockroach/otelcol-data/otelcol-agent-config.yaml"])
			require.Equal(t, []byte{100, 200}, data["cockroach/otelcol-data/API_KEY.env"])
			require.Equal(t, []byte("test-credentials"), data["cockroach/otelcol-data/aws_metrics_credentials"])
			require.Equal(t, []byte("13\n"), data["cockroach/otelcol-data/STARTER_CHECKPOINT"])

			validateDefaultFluentConfigData(t, data)
		})
	})

	t.Run("AWSLogsRoleARN", func(t *testing.T) {
		tenant.Spec.OtelCollector.AWSLogsRoleARN = "foo-arn"
		defer func() { tenant.Spec.OtelCollector.AWSLogsRoleARN = "" }()

		t.Run("successful/no credentials", func(t *testing.T) {
			data, err := fetchSQLPodData(ctx, ctl, &tenant, nil /* credentials */, locality)
			require.NoError(t, err)
			require.Equal(t, 13, len(data))

			validateCertData(t, data)
			validateDefaultFluentConfigData(t, data)
		})

		t.Run("credentials could not be found", func(t *testing.T) {
			data, err := fetchSQLPodData(ctx, ctl, &tenant, make(map[string][]byte), locality)
			require.Error(t, err)
			require.EqualError(t, err, `constructing otelcol-data: could not locate AWS logs credentials for "foo-arn"`)
			require.Nil(t, data)
		})

		t.Run("successful", func(t *testing.T) {
			credentials := make(map[string][]byte)
			credentials["foo-arn"] = []byte("test-credentials")
			data, err := fetchSQLPodData(ctx, ctl, &tenant, credentials, locality)
			require.NoError(t, err)
			require.Equal(t, 17, len(data))

			validateCertData(t, data)

			require.Equal(t, []byte("foo-bar-baz"), data["cockroach/otelcol-data/otelcol-agent-config.yaml"])
			require.Equal(t, []byte{100, 200}, data["cockroach/otelcol-data/API_KEY.env"])
			require.Equal(t, []byte("test-credentials"), data["cockroach/otelcol-data/aws_logs_credentials"])
			require.Equal(t, []byte("13\n"), data["cockroach/otelcol-data/STARTER_CHECKPOINT"])

			validateDefaultFluentConfigData(t, data)
		})
	})

	t.Run("GCPLogsServiceAccount", func(t *testing.T) {
		tenant.Spec.OtelCollector.GCPLogsServiceAccount = "foo"
		defer func() { tenant.Spec.OtelCollector.GCPLogsServiceAccount = "" }()

		t.Run("successful/no credentials", func(t *testing.T) {
			data, err := fetchSQLPodData(ctx, ctl, &tenant, nil /* credentials */, locality)
			require.NoError(t, err)
			require.Equal(t, 13, len(data))

			validateCertData(t, data)
			validateDefaultFluentConfigData(t, data)
		})

		t.Run("credentials could not be found", func(t *testing.T) {
			data, err := fetchSQLPodData(ctx, ctl, &tenant, make(map[string][]byte), locality)
			require.Error(t, err)
			require.EqualError(t, err, `constructing otelcol-data: could not locate GCP logs credentials for "foo"`)
			require.Nil(t, data)
		})

		t.Run("successful", func(t *testing.T) {
			credentials := make(map[string][]byte)
			credentials["foo"] = []byte("test-credentials")
			data, err := fetchSQLPodData(ctx, ctl, &tenant, credentials, locality)
			require.NoError(t, err)
			require.Equal(t, 18, len(data))

			validateCertData(t, data)

			require.Equal(t, []byte("foo-bar-baz"), data["cockroach/otelcol-data/otelcol-agent-config.yaml"])
			require.Equal(t, []byte{100, 200}, data["cockroach/otelcol-data/API_KEY.env"])
			require.Equal(t, []byte("/cockroach/otelcol-data/access_token_creds"), data["cockroach/otelcol-data/CLOUDSDK_AUTH_ACCESS_TOKEN_PATH.env"])
			require.Equal(t, []byte("test-credentials"), data["cockroach/otelcol-data/access_token_creds"])
			require.Equal(t, []byte("13\n"), data["cockroach/otelcol-data/STARTER_CHECKPOINT"])

			validateDefaultFluentConfigData(t, data)
		})
	})

	// Enable Logs export with a config map that does not exist
	tenant.Spec.Fluentbit = &crdbv1beta1.Fluentbit{
		ConfigMapName: "fluent-bit-tenant-14",
	}

	t.Run("no fluentbit config", func(t *testing.T) {
		data, err := fetchSQLPodData(ctx, ctl, &tenant, nil, locality)
		require.Error(t, err)
		require.EqualError(t, err, `constructing fluent-bit-data: configmaps "fluent-bit-tenant-14" not found`)
		require.Nil(t, data)
	})

	tenant.Spec.Fluentbit = &crdbv1beta1.Fluentbit{
		ConfigMapName: "fluent-bit-tenant-13",
	}

	t.Run("successful/both OtelCollector and Fluentbit enabled", func(t *testing.T) {
		data, err := fetchSQLPodData(ctx, ctl, &tenant, nil, locality)
		require.NoError(t, err)
		require.Equal(t, 16, len(data))

		validateCertData(t, data)

		require.Equal(t, []byte("foo-bar-baz"), data["cockroach/otelcol-data/otelcol-agent-config.yaml"])
		require.Equal(t, []byte{100, 200}, data["cockroach/otelcol-data/API_KEY.env"])
		require.Equal(t, []byte("13\n"), data["cockroach/otelcol-data/STARTER_CHECKPOINT"])

		require.Equal(t, []byte("1"), data["cockroach/fluent-bit-data/fluent-bit.conf"])
		require.Equal(t, []byte("2"), data["cockroach/fluent-bit-data/inputs.conf"])
		require.Equal(t, []byte("3"), data["cockroach/fluent-bit-data/outputs.conf"])
		require.Equal(t, []byte("13\n"), data["cockroach/fluent-bit-data/STARTER_CHECKPOINT"])
	})

	// Finally, disable otel collector.
	tenant.Spec.OtelCollector = nil

	t.Run("successful/fluentbit enabled", func(t *testing.T) {
		data, err := fetchSQLPodData(ctx, ctl, &tenant, nil, locality)
		require.NoError(t, err)
		require.Equal(t, 13, len(data))

		validateCertData(t, data)

		require.Equal(t, []byte("1"), data["cockroach/fluent-bit-data/fluent-bit.conf"])
		require.Equal(t, []byte("2"), data["cockroach/fluent-bit-data/inputs.conf"])
		require.Equal(t, []byte("3"), data["cockroach/fluent-bit-data/outputs.conf"])
		require.Equal(t, []byte("13\n"), data["cockroach/fluent-bit-data/STARTER_CHECKPOINT"])
	})
}

func TestZip(t *testing.T) {
	r, err := zip(map[string][]byte{
		"foo.bin": {1, 2},
		"bar.bin": {3, 4},
	})
	require.NoError(t, err)

	verifyFile := func(f *gozip.File, name string, contents []byte) {
		require.Equal(t, name, f.Name)
		r, err = f.Open()
		require.NoError(t, err)
		b, err := io.ReadAll(r)
		require.NoError(t, err)
		require.Equal(t, contents, b)
	}

	b, err := io.ReadAll(r)
	require.NoError(t, err)
	zipReader, err := gozip.NewReader(bytes.NewReader(b), int64(len(b)))
	require.NoError(t, err)
	require.Len(t, zipReader.File, 2)
	if zipReader.File[0].Name != "foo.bin" {
		// Swap to give deterministic order.
		zipReader.File[0], zipReader.File[1] = zipReader.File[1], zipReader.File[0]
	}
	verifyFile(zipReader.File[0], "foo.bin", []byte{1, 2})
	verifyFile(zipReader.File[1], "bar.bin", []byte{3, 4})
}

func makeOtelCollectorConfig(t *testing.T, ctl *kube.Ctl, configName string, secretName string) {
	t.Helper()
	config := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configName,
			Namespace: "default",
		},
		Data: map[string]string{
			"config": "foo-bar-baz",
		},
	}
	require.NoError(t, ctl.Apply(context.Background(), &config))
	if secretName != "" {
		secret := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: "default",
			},
			Data: map[string][]byte{
				"API_KEY": {100, 200},
			},
		}
		require.NoError(t, ctl.Apply(context.Background(), &secret))
	}
}

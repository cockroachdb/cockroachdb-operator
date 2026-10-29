package manifests_test

import (
	"encoding/json"
	"testing"

	. "github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestSecretVolume(t *testing.T) {
	tests := []struct {
		desc string
		opts SecretVolumeOptions
	}{
		{
			desc: "simple",
			opts: SecretVolumeOptions{
				Name:    "simple",
				Secrets: []string{"simple-secret"},
			},
		},
		{
			desc: "simple with SecurityContexts",
			opts: SecretVolumeOptions{
				Name:    "simple",
				Secrets: []string{"simple-secret"},
			},
		},
		{
			desc: "renames",
			opts: SecretVolumeOptions{
				Name: "simple",
				Renames: map[string][]corev1.KeyToPath{
					"simple-secret": {
						{Key: "simple.crt", Path: "complex.crt"},
					},
				},
				Secrets: []string{"simple-secret"},
			},
		},
		{
			desc: "multiple-secrets",
			opts: SecretVolumeOptions{
				Name:    "multi-secret",
				Secrets: []string{"s1", "s2", "s3"},
			},
		},
		{
			desc: "multiple-secrets-renames",
			opts: SecretVolumeOptions{
				Name: "multi-secret",
				Renames: map[string][]corev1.KeyToPath{
					"s1": {
						{Key: "sqlproxy.crt", Path: "external.crt"},
						{Key: "sqlproxy.key", Path: "external.key"},
					},
					"s3": {
						{Key: "sqlstarter.crt", Path: "starter.crt"},
						{Key: "sqlstarter.key", Path: "starter.key"},
					},
				},
				Secrets: []string{"s1", "s2", "s3"},
			},
		},
		{
			desc: "multiple-secrets-renames-with-securitycontexts",
			opts: SecretVolumeOptions{
				Name: "multi-secret",
				Renames: map[string][]corev1.KeyToPath{
					"s1": {
						{Key: "sqlproxy.crt", Path: "external.crt"},
						{Key: "sqlproxy.key", Path: "external.key"},
					},
					"s3": {
						{Key: "sqlstarter.crt", Path: "starter.crt"},
						{Key: "sqlstarter.key", Path: "starter.key"},
					},
				},
				Secrets: []string{"s1", "s2", "s3"},
			},
		},
	}

	for _, tt := range tests {
		raw, err := json.MarshalIndent(SecretVolume(tt.opts), "", "  ")
		require.NoError(t, err)

		testutils.AssertGoldenJSON(t, tt.desc, raw)
	}
}

func TestVolumeProjection(t *testing.T) {
	mode_pgroup := int32(288)

	testCases := []struct {
		name       string
		secrets    []SecretProjection
		configMaps []ConfigMapProjection
		expected   corev1.Volume
	}{
		{
			name: "empty",
			expected: corev1.Volume{
				Name: "empty",
				VolumeSource: corev1.VolumeSource{
					Projected: &corev1.ProjectedVolumeSource{
						DefaultMode: &mode_pgroup,
						Sources:     []corev1.VolumeProjection{},
					},
				},
			},
		},
		{
			name: "duplicated secrets",
			secrets: []SecretProjection{
				{
					Name: "foo", Renames: []corev1.KeyToPath{
						{Key: "tls.crt", Path: "foo.crt"},
						{Key: "tls.key", Path: "foo.key"},
					},
				},
				{
					Name: "foo", Renames: []corev1.KeyToPath{
						{Key: "tls.crt", Path: "bar.crt"},
						{Key: "tls.key", Path: "bar.key"},
					},
				},
			},
			expected: corev1.Volume{
				Name: "duplicated secrets",
				VolumeSource: corev1.VolumeSource{
					Projected: &corev1.ProjectedVolumeSource{
						DefaultMode: &mode_pgroup,
						Sources: []corev1.VolumeProjection{
							{
								Secret: &corev1.SecretProjection{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "foo",
									},
									Items: []corev1.KeyToPath{
										{Key: "tls.crt", Path: "foo.crt"},
										{Key: "tls.key", Path: "foo.key"},
										{Key: "tls.crt", Path: "bar.crt"},
										{Key: "tls.key", Path: "bar.key"},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "duplicated configmaps",
			configMaps: []ConfigMapProjection{
				{
					Name: "foo", Renames: []corev1.KeyToPath{
						{Key: "tls.crt", Path: "foo.crt"},
						{Key: "tls.key", Path: "foo.key"},
					},
				},
				{
					Name: "foo", Renames: []corev1.KeyToPath{
						{Key: "tls.crt", Path: "bar.crt"},
						{Key: "tls.key", Path: "bar.key"},
					},
				},
			},
			expected: corev1.Volume{
				Name: "duplicated configmaps",
				VolumeSource: corev1.VolumeSource{
					Projected: &corev1.ProjectedVolumeSource{
						DefaultMode: &mode_pgroup,
						Sources: []corev1.VolumeProjection{
							{
								ConfigMap: &corev1.ConfigMapProjection{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "foo",
									},
									Items: []corev1.KeyToPath{
										{Key: "tls.crt", Path: "foo.crt"},
										{Key: "tls.key", Path: "foo.key"},
										{Key: "tls.crt", Path: "bar.crt"},
										{Key: "tls.key", Path: "bar.key"},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "duplicated both",
			configMaps: []ConfigMapProjection{
				{
					Name: "foo", Renames: []corev1.KeyToPath{
						{Key: "tls.crt", Path: "foo.crt"},
						{Key: "tls.key", Path: "foo.key"},
					},
				},
				{
					Name: "foo", Renames: []corev1.KeyToPath{
						{Key: "tls.crt", Path: "bar.crt"},
						{Key: "tls.key", Path: "bar.key"},
					},
				},
			},
			secrets: []SecretProjection{
				{
					Name: "foo", Renames: []corev1.KeyToPath{
						{Key: "tls.crt", Path: "foo.crt"},
						{Key: "tls.key", Path: "foo.key"},
					},
				},
				{
					Name: "foo", Renames: []corev1.KeyToPath{
						{Key: "tls.crt", Path: "bar.crt"},
						{Key: "tls.key", Path: "bar.key"},
					},
				},
			},
			expected: corev1.Volume{
				Name: "duplicated both",
				VolumeSource: corev1.VolumeSource{
					Projected: &corev1.ProjectedVolumeSource{
						DefaultMode: &mode_pgroup,
						Sources: []corev1.VolumeProjection{
							{
								Secret: &corev1.SecretProjection{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "foo",
									},
									Items: []corev1.KeyToPath{
										{Key: "tls.crt", Path: "foo.crt"},
										{Key: "tls.key", Path: "foo.key"},
										{Key: "tls.crt", Path: "bar.crt"},
										{Key: "tls.key", Path: "bar.key"},
									},
								},
							},
							{
								ConfigMap: &corev1.ConfigMapProjection{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "foo",
									},
									Items: []corev1.KeyToPath{
										{Key: "tls.crt", Path: "foo.crt"},
										{Key: "tls.key", Path: "foo.key"},
										{Key: "tls.crt", Path: "bar.crt"},
										{Key: "tls.key", Path: "bar.key"},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			volume := VolumeProjection(tc.name, tc.secrets, tc.configMaps)
			require.Equal(t, tc.expected, volume)
		})
	}
}

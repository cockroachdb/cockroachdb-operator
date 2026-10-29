package crypto

import (
	"context"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestToK8SSecret(t *testing.T) {
	requireCerts := func(t *testing.T, certs map[string][]byte, expected ...string) {
		require.Len(t, certs, len(expected))
		for _, key := range expected {
			require.Contains(t, certs, key)
		}
	}

	t.Run("NoName", func(t *testing.T) {
		config := &SecretConfig{}
		_, err := config.ToK8SSecret(context.Background(), nil)
		require.EqualError(t, err, "missing SecretName in SecretConfig")
	})

	t.Run("WithExtraMetadata", func(t *testing.T) {
		config := &SecretConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name: "the-secret-name",
				Labels: map[string]string{
					"label": "label-value",
				},
				Annotations: map[string]string{
					"annotation": "annotation-value",
				},
			},
		}

		revisionHash, err := config.RevisionHash()
		require.NoError(t, err)

		signer, err := DeterministicFakeSigner()
		require.NoError(t, err)

		secret, err := config.ToK8SSecret(context.Background(), signer)
		require.NoError(t, err)
		require.Equal(t, metav1.ObjectMeta{
			Name: "the-secret-name",
			Labels: map[string]string{
				"label":                 "label-value",
				kube.LabelManagedBy:     LabelManagedByYolloutCertificates,
				LabelSecretRevisionHash: revisionHash,
				LabelSecretTlsNotAfter:  "21010203T040506Z",
			},
			Annotations: map[string]string{
				"annotation": "annotation-value",
			},
		}, secret.ObjectMeta)
	})

	t.Run("WithSigningRequest", func(t *testing.T) {
		config := &SecretConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name: "the-name",
			},
			Certs: []CertificateConfig{{
				FileName:   "cert-name",
				IncludePEM: true,
				SigningRequest: CertificateSigningRequest{
					CommonName: "the-common-name",
				},
			}},
		}

		revisionHash, err := config.RevisionHash()
		require.NoError(t, err)

		signer, err := DeterministicFakeSigner()
		require.NoError(t, err)

		secret, err := config.ToK8SSecret(context.Background(), signer)
		require.NoError(t, err)
		require.Equal(t,
			metav1.ObjectMeta{
				Name: "the-name",
				Labels: map[string]string{
					kube.LabelManagedBy:     LabelManagedByYolloutCertificates,
					LabelSecretRevisionHash: revisionHash,
					LabelSecretTlsNotAfter:  "21010203T040506Z",
				},
			},
			secret.ObjectMeta,
		)
		requireCerts(t, secret.Data, "ca.crt", "cert-name.crt", "cert-name.key", "cert-name.pem")
	})

	t.Run("WithCertsExternalBundle", func(t *testing.T) {
		config := &SecretConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name: "the-name",
			},
			Certs: []CertificateConfig{{
				FileName:       "cert-name",
				ExternalBundle: TestBundle,
			}},
		}

		revisionHash, err := config.RevisionHash()
		require.NoError(t, err)

		signer, err := DeterministicFakeSigner()
		require.NoError(t, err)
		ca, err := signer.Certificate(context.Background())
		require.NoError(t, err)

		secret, err := config.ToK8SSecret(context.Background(), signer)
		require.NoError(t, err)
		require.Equal(t, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "the-name",
				Labels: map[string]string{
					kube.LabelManagedBy:     LabelManagedByYolloutCertificates,
					LabelSecretRevisionHash: revisionHash,
					LabelSecretTlsNotAfter:  "21230624T202024Z",
				},
			},
			Data: map[string][]byte{
				"ca.crt":        ca,
				"cert-name.crt": TestBundle.Certificate,
				"cert-name.key": TestBundle.PrivateKey,
			},
		}, secret)
	})

	t.Run("WithSigningRequest", func(t *testing.T) {
		config := &SecretConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name: "the-name",
			},
			SigningRequest: CertificateSigningRequest{
				CommonName: "the-common-name",
			},
		}

		revisionHash, err := config.RevisionHash()
		require.NoError(t, err)

		signer, err := DeterministicFakeSigner()
		require.NoError(t, err)

		secret, err := config.ToK8SSecret(context.Background(), signer)
		require.NoError(t, err)
		require.Equal(t, secret.ObjectMeta,
			metav1.ObjectMeta{
				Name: "the-name",
				Labels: map[string]string{
					kube.LabelManagedBy:     LabelManagedByYolloutCertificates,
					LabelSecretRevisionHash: revisionHash,
					LabelSecretTlsNotAfter:  "21010203T040506Z",
				},
			})
		require.Equal(t, secret.Type, corev1.SecretTypeTLS)
		requireCerts(t, secret.Data, "tls.crt", "tls.key", "tls.pem")
	})

	t.Run("WithExternalBundle", func(t *testing.T) {
		config := &SecretConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name: "the-name",
			},
			ExternalBundle: TestBundle,
		}

		revisionHash, err := config.RevisionHash()
		require.NoError(t, err)

		signer, err := DeterministicFakeSigner()
		require.NoError(t, err)

		secret, err := config.ToK8SSecret(context.Background(), signer)
		require.NoError(t, err)
		require.Equal(t,
			metav1.ObjectMeta{
				Name: "the-name",
				Labels: map[string]string{
					kube.LabelManagedBy:     LabelManagedByYolloutCertificates,
					LabelSecretRevisionHash: revisionHash,
					LabelSecretTlsNotAfter:  "21230624T202024Z",
				},
			}, secret.ObjectMeta)
		require.Equal(t, secret.Type, corev1.SecretTypeTLS)
		requireCerts(t, secret.Data, "tls.crt", "tls.key", "tls.pem")
	})
}

func TestSecretConfigThumbprint(t *testing.T) {
	var testCases = []struct {
		Name                  string
		ExpectNewRevisionHash bool
		Modification          func(config *SecretConfig)
	}{
		{
			Name:                  "SetSecretName",
			ExpectNewRevisionHash: true,
			Modification: func(config *SecretConfig) {
				config.ObjectMeta.Name = "SecretName"
			},
		},
		{
			Name:                  "AddCertConfig",
			ExpectNewRevisionHash: true,
			Modification: func(config *SecretConfig) {
				config.Certs = []CertificateConfig{
					{
						FileName: "cert-name",
						SigningRequest: CertificateSigningRequest{
							CommonName: "cert-identity",
						},
					},
				}
			},
		},
		{
			Name:                  "ChangeCertName",
			ExpectNewRevisionHash: true,
			Modification: func(config *SecretConfig) {
				config.Certs[0].FileName = "new-file-name"
			},
		},
		{
			Name:                  "SwitchToExternalCert",
			ExpectNewRevisionHash: true,
			Modification: func(config *SecretConfig) {
				config.Certs[0].SigningRequest = CertificateSigningRequest{}
				config.Certs[0].ExternalBundle = &Bundle{}
			},
		},
		{
			Name:                  "ChangeExternalCert",
			ExpectNewRevisionHash: true,
			Modification: func(config *SecretConfig) {
				// The ExternalBundle is included in the json to generate the
				// thumbprint. Changing the PrivateKey should change the
				// thumbprint.
				config.Certs[0].ExternalBundle.PrivateKey = []byte{1, 2, 3}
			},
		},
	}

	config := &SecretConfig{}

	revisionHash, err := config.RevisionHash()
	require.NoError(t, err)
	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			tc.Modification(config)
			newThumbprint, err := config.RevisionHash()
			require.NoError(t, err)
			if tc.ExpectNewRevisionHash {
				require.NotEqual(t, revisionHash, newThumbprint)
			} else {
				require.Equal(t, revisionHash, newThumbprint)
			}
			revisionHash = newThumbprint
		})
	}
}

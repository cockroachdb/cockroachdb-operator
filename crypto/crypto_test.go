package crypto

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestToSecretData(t *testing.T) {
	require.Equal(t, SecretData{
		"ca.crt":          []byte("ImTheCA"),
		"node.crt":        []byte("ImTheNodeCert"),
		"node.key":        []byte("ImTheNodePK"),
		"client.root.crt": []byte("ImTheClientCert"),
		"client.root.key": []byte("ImTheClientPK"),
	}, ToSecretData([]byte("ImTheCA"), map[string]*Bundle{
		"node": {
			Certificate: []byte("ImTheNodeCert"),
			PrivateKey:  []byte("ImTheNodePK"),
		},
		"client.root": {
			Certificate: []byte("ImTheClientCert"),
			PrivateKey:  []byte("ImTheClientPK"),
		},
	}))
}

func TestFromSecretData(t *testing.T) {
	ca, certs := FromSecretData(SecretData{
		"ca.crt":          []byte("ImTheCA"),
		"node.crt":        []byte("ImTheNodeCert"),
		"node.key":        []byte("ImTheNodePK"),
		"client.root.crt": []byte("ImTheClientCert"),
		"client.root.key": []byte("ImTheClientPK"),
	})
	require.Equal(t, []byte("ImTheCA"), ca)
	require.Equal(t, map[string]*Bundle{
		"node": {
			Certificate: []byte("ImTheNodeCert"),
			PrivateKey:  []byte("ImTheNodePK"),
		},
		"client.root": {
			Certificate: []byte("ImTheClientCert"),
			PrivateKey:  []byte("ImTheClientPK"),
		},
	}, certs)
}

func TestCertificateSigningRequest_PEM(t *testing.T) {
	require.Equal(t, []byte("A\nB"), (&Bundle{
		Certificate: []byte("A"),
		PrivateKey:  []byte("B"),
	}).PEM())
}

func TestCertificateSigningRequest_Validate(t *testing.T) {
	testCases := []struct {
		CSR   CertificateSigningRequest
		Error string
	}{
		{
			CSR:   CertificateSigningRequest{},
			Error: "",
		},
		{
			CSR: CertificateSigningRequest{
				DNSNames: []string{
					"this-is-fine.com",
				},
			},
			Error: "",
		},
		{
			CSR: CertificateSigningRequest{
				DNSNames: []string{
					"this_is_fine.com",
				},
			},
			Error: "dns subject alternative names may not contain _'s: this_is_fine.com",
		},
	}

	for _, tc := range testCases {
		err := tc.CSR.Validate()

		if tc.Error != "" {
			require.EqualError(t, err, tc.Error)
		} else {
			require.NoError(t, err)
		}
	}
}

func TestTLSConfigFromBundle(t *testing.T) {
	signer, err := DeterministicFakeSigner()
	require.NoError(t, err)
	ca, err := signer.Certificate(context.Background())
	require.NoError(t, err)

	t.Run("NilClientBundle", func(t *testing.T) {
		config, err := TLSConfigFromBundle(ca, nil, "server")
		require.NoError(t, err)
		require.NotNil(t, config.RootCAs)
		require.Equal(t, "server", config.ServerName)
		require.Nil(t, config.Certificates)
	})

	t.Run("NotNilClientBundle", func(t *testing.T) {
		clientBundle, err := signer.Sign(context.Background(), CertificateSigningRequest{
			Role:       "client",
			CommonName: "user",
		})
		require.NoError(t, err)
		config, err := TLSConfigFromBundle(ca, clientBundle, "server")
		require.NoError(t, err)
		require.NotNil(t, config.RootCAs)
		require.Equal(t, "server", config.ServerName)
		require.Len(t, config.Certificates, 1)
	})
}

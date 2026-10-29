package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path"

	"github.com/cockroachdb/errors"
)

// readCert reads and parses certificates from a kubernetes secret volume.
// readCert expects the format produced by crypto.SecretConfig.
func readCert(directory string, keyname string) (*x509.CertPool, tls.Certificate, error) {
	keyFileName := path.Join(directory, fmt.Sprintf("%s.key", keyname))
	certFileName := path.Join(directory, fmt.Sprintf("%s.crt", keyname))
	caFileName := path.Join(directory, "ca.crt")

	cert, err := tls.LoadX509KeyPair(certFileName, keyFileName)
	if err != nil {
		return nil, tls.Certificate{}, errors.Wrapf(err, "unable to read cert pair for key '%s'", keyFileName)
	}

	caFile, err := os.ReadFile(caFileName)
	if err != nil {
		return nil, tls.Certificate{}, errors.Wrapf(err, "unable to read ca from file '%s'", caFileName)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caFile) {
		return nil, tls.Certificate{}, errors.Errorf("unable to add certificate from directory '%s' to pool", directory)
	}

	return caPool, cert, nil
}

// ClientTLSConfig produces a TLS config for use by a mTLS client.
func ClientTLSConfig(clientCert tls.Certificate, ca *x509.CertPool) *tls.Config {
	return &tls.Config{
		RootCAs:      ca,
		Certificates: []tls.Certificate{clientCert},
	}
}

// ServerTLSConfig produces a TLS config for use by an mtls server. It expects the client cert to contain
// one of the authorizedClientURIs in the URLSan field. If the URI is not present, the client connection
// is rejected.
func ServerTLSConfig(
	serverCert tls.Certificate, ca *x509.CertPool, authorizedClientURIs []string,
) *tls.Config {
	authorizedURISet := map[string]bool{}
	for _, uri := range authorizedClientURIs {
		authorizedURISet[uri] = true
	}

	// verifyConnection checks the peer certificates to see if any contain an
	// authorizedClientURI.
	verifyConnection := func(state tls.ConnectionState) error {
		for _, chain := range state.VerifiedChains {
			if len(chain) == 0 {
				continue
			}
			for _, uri := range chain[0].URIs {
				if authorizedURISet[uri.String()] {
					return nil
				}
			}
		}
		return errors.New("did not find an authorized URI in the client certificate")
	}

	return &tls.Config{
		ClientCAs:        ca,
		Certificates:     []tls.Certificate{serverCert},
		ClientAuth:       tls.RequireAndVerifyClientCert,
		VerifyConnection: verifyConnection,
	}
}

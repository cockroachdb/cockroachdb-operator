package crypto

import "context"

// CertificateSigner is a Certificate Authority capable of signing certificate requests.
// Real deployments should always use a signer backed by a certificate authority.
type CertificateSigner interface {
	// Certificate returns the CA or root cert in PEM format for this signer
	Certificate(ctx context.Context) ([]byte, error)
	// Sign generates a signed certificate for the specified request
	Sign(ctx context.Context, csr CertificateSigningRequest) (*Bundle, error)
}

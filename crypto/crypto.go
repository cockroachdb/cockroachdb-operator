package crypto

//go:generate mockgen -package=crypto -destination=./mocks.go . CertificateSigner

import (
	"crypto/tls"
	"crypto/x509"
	"net/url"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"slices"
)

// CertificateSigningRequest is a slightly more pleasant abstraction over
// x509.CertificateRequest that is tailored to CC's certificate use cases.
// More fields may, and should, be added as required.
//
// Client certificates may be minted by just providing CommonName.
// It is recommended to provide a TTL in production use cases.
//
//	 signer.Sign(ctx, "role", crypto.CertificateSigningRequest{
//		  CommonName: "username",
//	 })
type CertificateSigningRequest struct {
	// Role names the signing role for CA-backed signers that scope issuance
	// by role. It must be specified in all non-test situations. The fake
	// signers in this package ignore it. In the future, role may become its
	// own type and have constants in the crypto package, if a bare string is
	// too cumbersome.
	Role string

	// CommonName is synonymous with x509.CertificateRequest.Subject.Name
	CommonName string

	// IPAddresses is list of IPs to include as Subject Alternatives Names (SANs).
	// It is synonymous with x509.CertificateRequest's IPAddresses field.
	IPAddresses []string

	// DNSNames is a list of DNS Subject Alternatives Names (SANs).
	DNSNames []string

	// URINames is a list of URI Subject Alternatives Names (SANs).
	// Example: "crdb://tenant/4" identifies the certificate as belonging to the
	// tenant with id 4.
	URINames []string

	// OrganizationalUnit (OU) is synonymous with x509.CertificateSigningRequest.Subject.OrganizationalUnit
	OrganizationalUnit []string

	// TTL controls the NotAfter attribute of the signed cert. The fake signers
	// honor it, but other signers may ignore it.
	TTL time.Duration

	// NeedsPublicCA if true means that the certificate has to be signed by
	// a publicly trusted CA.
	NeedsPublicCA bool
}

// Validate ensures the CSR is valid, e.g. contains valid SANs.
func (c *CertificateSigningRequest) Validate() error {
	for _, dnsName := range c.DNSNames {
		// SANs cannot contain _'s due to an encoding issue with ASN1
		// See https://www.digicert.com/kb/ssl-support/underscores-not-allowed-in-fqdns.htm
		if strings.ContainsRune(dnsName, '_') {
			return errors.Newf("dns subject alternative names may not contain _'s: %s", dnsName)
		}
	}
	for _, uriNames := range c.URINames {
		if _, err := url.Parse(uriNames); err != nil {
			return errors.Wrapf(err, "unable to parse uri subject alternative name: %s", uriNames)
		}
	}
	return nil
}

// Bundle is a signed and usable key pair in PEM format.
type Bundle struct {
	// Certificate is a signed x509.Certificate in PEM format.
	Certificate []byte
	// PrivateKey is the private key associated with Certificate in PEM format.
	PrivateKey []byte
}

func (b *Bundle) PEM() []byte {
	pem := slices.Clone(b.Certificate)
	pem = append(pem, '\n')
	pem = append(pem, b.PrivateKey...)

	return pem
}

func (b *Bundle) TLSCertificate() (tls.Certificate, error) {
	return tls.X509KeyPair(b.Certificate, b.PrivateKey)
}

// SecretData holds the CA certificate and one or more named certificate bundles
// signed by that certificate in the Kubernetes corev1.Secret.Data field format.
// The data has this structure:
//
//	ca.crt -> ca
//	<name>.crt -> cert
//	<name>.key -> private key
//
// Client keys should be named client.<username>
type SecretData map[string][]byte

// ToSecretData is a convenience function for generating Kubernetes secret data
// from a set of certificate bundles. If the given "ca" certificate is not nil,
// then it should be the signing certificate for all the certificate bundles,
// and it will be included in the data.
func ToSecretData(ca []byte, certs map[string]*Bundle) SecretData {
	data := SecretData{}
	if ca != nil {
		data["ca.crt"] = ca
	}

	for name, bundle := range certs {
		data[name+".crt"] = bundle.Certificate
		data[name+".key"] = bundle.PrivateKey
	}

	return data
}

// FromSecretData is a convenience function for getting the CA certificate and a
// set of certificate bundles signed by that CA from a Kubernetes secret that
// was created with ToSecretData. If no CA certificate is available, nil is
// returned for it.
func FromSecretData(data SecretData) (ca []byte, certs map[string]*Bundle) {
	// If ca.crt does not exist, ca will be set to nil.
	ca = data["ca.crt"]

	certs = make(map[string]*Bundle)
	for key, value := range data {
		if strings.HasSuffix(key, ".crt") && key != "ca.crt" {
			name := key[:len(key)-4]
			bundle := &Bundle{
				Certificate: value,
				PrivateKey:  data[name+".key"],
			}
			certs[name] = bundle
		}
	}

	return ca, certs
}

// TLSConfigFromBundle creates a tls.Config object using the supplied CA
// certificate and the bundle for adding client certificates.
func TLSConfigFromBundle(ca []byte, clientBundle *Bundle, serverName string) (*tls.Config, error) {
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(ca) {
		return nil, errors.Newf("adding CA to root CA pool")
	}

	// Add client certificate if one was supplied.
	var certificates []tls.Certificate
	if clientBundle != nil {
		clientCert, err := clientBundle.TLSCertificate()
		if err != nil {
			return nil, errors.Wrap(err, "creating TLS client certificate")
		}
		certificates = []tls.Certificate{clientCert}
	}

	return &tls.Config{
		Certificates: certificates,
		RootCAs:      caPool,
		ServerName:   serverName,
	}, nil
}

package crypto

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabelManagedByYolloutCertificates is the managed-by label value on secrets
// that certificate automation owns. Existing clusters carry this value, so it
// must not change.
const LabelManagedByYolloutCertificates = "certificates.yollout.cockroachlabs.cloud"

// LabelSecretRevisionHash is an annotation key used to label the secret with
// the config's thumbprint. The value of the annotation is used to determine if
// automation should mint and push a new secret.
const LabelSecretRevisionHash = "yollout-secret-revision-hash"

// LabelSecretTlsNotAfter is an annotation that labels Tls secrets with their
// Not After timestamp. This annotation is used to determine if Tls secrets
// are close to expiration and need renewals.
const LabelSecretTlsNotAfter = "yollout-secret-tls-not-after"

// LabelSecretTimeFormat is the time format used when encoding/decoding time
// into LabelSecretTlsNotAfter label's string value.
const LabelSecretTimeFormat = "20060102T150405Z"

// SecretConfig is a declarative representation of a Kubernetes secret
// containing certificates.
type SecretConfig struct {
	// ObjectMeta identifying the secret.
	ObjectMeta metav1.ObjectMeta

	// SigningRequest describes how this certificate should be generated. When
	// specified, ToK8SSecret will return a kubernetes.io/tls secret with the
	// keys tls.crt and tls.key.
	// NOTE: Access to CA.crt files should be provided via ConfigMaps
	// https://kubernetes.io/docs/tasks/tls/managing-tls-in-a-cluster/#trusting-tls-in-a-cluster
	SigningRequest CertificateSigningRequest

	// ExternalBundle is a cert bundle that was signed by an external CA. For
	// example, the SQL Proxy's certificates are signed by let's encrypt. When
	// specified, ExternalBundle will take precedence over SigningRequest and
	// will cause ToK8SSecret to return a kubernetes.io/tls Secret containing
	// this bundle.
	ExternalBundle *Bundle `json:"ExternalBundle,omitempty"`

	// Certs describes the certificates contained within the secret.
	// Certs will take precedence over SigningRequest, if specified.
	// Deprecated: Prefer to generate individual kubernetes.io/tls secrets by
	// specifying SigningRequest.
	Certs []CertificateConfig
}

type CertificateConfig struct {
	// FileName is the string used to label the certificate files. Extensions are
	// automatically appended to the filename.
	// Example: "node" will generate files with the name "node.crt", "node.key",
	// and "node.pem"
	FileName string

	// IncludePEM determines whether a pem file is included in the secret.
	IncludePEM bool

	// SigningRequest describes how the certificate should be generated. This
	// should be specified for certificates that are signed with an internal CA.
	SigningRequest CertificateSigningRequest

	// ExternalBundle is a cert bundle that was signed by an external CA. For
	// example, the SQL Proxy's certificates are signed by let's encrypt.
	ExternalBundle *Bundle `json:"ExternalBundle,omitempty"`
}

// bundle is a conveniences method that returns .ExternalBundle if it is not
// nil. Otherwise, it signs the .SigningRequest using signer.
func (c *CertificateConfig) bundle(ctx context.Context, signer CertificateSigner) (*Bundle, error) {
	if c.ExternalBundle != nil {
		return c.ExternalBundle, nil
	}

	bundle, err := signer.Sign(ctx, c.SigningRequest)
	return bundle, errors.Wrapf(err, "unable to create cert %s", c.FileName)
}

// ToK8SSecret converts the secret configuration into a secret that can be
// stored in Kubernetes.
func (sc *SecretConfig) ToK8SSecret(
	ctx context.Context, signer CertificateSigner,
) (*corev1.Secret, error) {
	if sc.ObjectMeta.Name == "" {
		return nil, errors.New("missing SecretName in SecretConfig")
	}

	revisionHash, err := sc.RevisionHash()
	if err != nil {
		return nil, err
	}

	meta := sc.ObjectMeta.DeepCopy()
	if meta.Labels == nil {
		meta.Labels = make(map[string]string)
	}
	meta.Labels[kube.LabelManagedBy] = LabelManagedByYolloutCertificates
	meta.Labels[LabelSecretRevisionHash] = revisionHash

	if len(sc.Certs) == 0 {
		bundle := sc.ExternalBundle
		if bundle == nil {
			var err error
			bundle, err = signer.Sign(ctx, sc.SigningRequest)
			if err != nil {
				return nil, err
			}
		}

		// Parse the leaf (first cert) and add its "Not After" as annotation.
		// There is the implicit assumption that the intermediate CA certs will
		// have "Not After" that is later than the leaf's one.
		der, _ := pem.Decode(bundle.Certificate)
		if der == nil {
			return nil, errors.New("unable to decode certificate")
		}
		leaf, err := x509.ParseCertificate(der.Bytes)
		if err != nil {
			return nil, err
		}
		meta.Labels[LabelSecretTlsNotAfter] = leaf.NotAfter.Format(LabelSecretTimeFormat)

		// NB: TLS secrets require a tls.crt and a tls.key file but may include
		// others. For convenience, we always include a PEM bundle for
		// applications that need a bundle file instead of separate certs and
		// keys. As of writing, the only application that needs a PEM is
		// HAProxy.
		return &corev1.Secret{
			ObjectMeta: *meta,
			Type:       corev1.SecretTypeTLS,
			Data: map[string][]byte{
				"tls.crt": bundle.Certificate,
				"tls.key": bundle.PrivateKey,
				"tls.pem": bundle.PEM(),
			},
		}, nil

	}

	data := SecretData{}
	data["ca.crt"], err = signer.Certificate(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "unable to retrieve certificate")
	}

	var notAfter time.Time
	for _, cert := range sc.Certs {
		bundle, err := cert.bundle(ctx, signer)
		if err != nil {
			return nil, err
		}
		der, _ := pem.Decode(bundle.Certificate)
		if der == nil {
			return nil, errors.New("unable to decode certificate")
		}
		leaf, err := x509.ParseCertificate(der.Bytes)
		if err != nil {
			return nil, err
		}
		if notAfter.IsZero() || notAfter.After(leaf.NotAfter) {
			notAfter = leaf.NotAfter
		}
		data[fmt.Sprintf("%s.crt", cert.FileName)] = bundle.Certificate
		data[fmt.Sprintf("%s.key", cert.FileName)] = bundle.PrivateKey
		if cert.IncludePEM {
			data[fmt.Sprintf("%s.pem", cert.FileName)] = bundle.PEM()
		}
	}

	meta.Labels[LabelSecretTlsNotAfter] = notAfter.Format(LabelSecretTimeFormat)

	return &corev1.Secret{
		ObjectMeta: *meta,
		Data:       data,
	}, nil
}

// RevisionHash returns a string that is a hash of the configuration. If the
// thumbprint changes, it implies a new secret should be generated and pushed.
func (sc *SecretConfig) RevisionHash() (string, error) {
	asJson, err := json.Marshal(sc)
	if err != nil {
		return "", errors.Wrap(err, "unable to serialize sercret config for thumbprint generation")
	}

	hash := fnv.New64()
	hash.Write(asJson)

	return fmt.Sprintf("%x", hash.Sum64()), nil
}

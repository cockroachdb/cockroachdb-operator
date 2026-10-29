package certs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	// OperatorCertSecretName is the name of the secret that stores operator webhook certificates.
	OperatorCertSecretName = "cockroach-operator-certs"

	// managedByLabel is the label key used to identify who manages a resource.
	managedByLabel = "app.kubernetes.io/managed-by"

	// managedByValue is the label value set by the operator on secrets it creates.
	managedByValue = "cockroach-operator"

	// rsaKeyBits is the size of RSA keys generated.
	rsaKeyBits = 4096

	// certValidityYears is the validity period for certificates.
	certValidityYears = 10
)

// SelfSignedCertGenerator generates self-signed certificates for the operator
// and for CockroachDB clusters managed by it.
type SelfSignedCertGenerator struct {
	ctl       *kube.Ctl
	namespace string
	// domain is the Kubernetes cluster domain used to construct service FQDNs
	// for certificate SANs. Defaults to "cluster.local" when not specified.
	domain string
	// clusterName is the CrdbCluster name, used to construct pod subdomain and
	// join service SANs. Defaults to "cockroachdb" when not specified.
	clusterName string
	// joinNamespaces holds the effective Kubernetes namespace for each region's
	// join service. When a cluster spans regions whose join addresses land in a
	// namespace other than the cluster's own (e.g. region code used as
	// namespace), each distinct namespace needs its own join service SAN in the
	// node certificate.
	joinNamespaces []string
	// owner is the Kubernetes object that owns the generated cert resources.
	// When set, all resources created by GenerateCrdbCerts carry an
	// OwnerReference pointing here, so they are garbage-collected when owner
	// is deleted.
	owner metav1.Object
}

// NewSelfSignedCertGenerator creates a new SelfSignedCertGenerator.
func NewSelfSignedCertGenerator(ctl *kube.Ctl, namespace, domain, clusterName string) *SelfSignedCertGenerator {
	if domain == "" {
		domain = "cluster.local"
	}
	if clusterName == "" {
		clusterName = "cockroachdb"
	}
	return &SelfSignedCertGenerator{
		ctl:         ctl.WithNamespace(namespace),
		namespace:   namespace,
		domain:      domain,
		clusterName: clusterName,
	}
}

// WithJoinNamespaces sets the effective Kubernetes namespace for each region's
// join service. The generator adds one join SAN per distinct namespace so that
// nodes using namespace-based DNS routing (e.g. region code as namespace) can
// verify the node certificate.
func (g *SelfSignedCertGenerator) WithJoinNamespaces(namespaces []string) *SelfSignedCertGenerator {
	g.joinNamespaces = namespaces
	return g
}

// WithOwner configures the generator to set a controller OwnerReference on
// every resource it creates via GenerateCrdbCerts. Resources with an
// OwnerReference are garbage-collected automatically when owner is deleted.
func (g *SelfSignedCertGenerator) WithOwner(owner metav1.Object) *SelfSignedCertGenerator {
	g.owner = owner
	return g
}

// setOwner attaches a controller OwnerReference to obj when an owner has been
// configured. Resources without an owner are not garbage-collected automatically.
func (g *SelfSignedCertGenerator) setOwner(obj metav1.Object) error {
	if g.owner == nil {
		return nil
	}
	if err := ctrl.SetControllerReference(g.owner, obj, g.ctl.Scheme()); err != nil {
		return errors.Wrap(err, "failed to set controller owner reference")
	}
	return nil
}

// standaloneCASecretName returns the cluster-scoped name of the Secret used to
// persist the standalone CA so the same CA is reused across operator restarts.
func (g *SelfSignedCertGenerator) standaloneCASecretName() string {
	return g.clusterName + "-cockroachdb-self-signed-ca"
}

// GenerateOperatorCerts generates the operator webhook CA and TLS certificates,
// storing them in the cockroach-operator-certs Secret. The CA key is retained in
// the secret so that GenerateCrdbCerts can use the same CA to sign cluster certs.
func (g *SelfSignedCertGenerator) GenerateOperatorCerts(ctx context.Context) error {
	// Check if operator certs already exist
	var secret corev1.Secret
	err := g.ctl.Get(ctx, OperatorCertSecretName, &secret)
	if err == nil {
		// The secret exists. Verify it was created by the operator (self-signed)
		// rather than provided by the user. A user should not both provide their
		// own cert and request self-signed cert generation.
		if v, ok := secret.Labels[managedByLabel]; !ok || v != managedByValue {
			return errors.Newf(
				"self-signed-operator-certs is enabled, but secret %q already exists "+
					"and was not created by the operator. Either remove the existing "+
					"secret to allow self-signed cert generation, or disable the "+
					"-self-signed-operator-certs flag",
				OperatorCertSecretName,
			)
		}
		logging.Debugf(ctx, "operator certificate secret %s already exists (self-signed)", OperatorCertSecretName)
		return nil
	}

	if !apierrors.IsNotFound(err) {
		return errors.Wrap(err, "failed to get operator cert secret")
	}

	// Generate CA certificate and key
	logging.Infof(ctx, "creating new CA and operator webhook certificates")
	caCert, caKey, caCertPEM, caKeyPEM, err := g.generateCA()
	if err != nil {
		return errors.Wrap(err, "failed to generate CA")
	}

	// Generate webhook certificate signed by the CA
	tlsCertPEM, tlsKeyPEM, err := g.generateWebhookCert(caCert, caKey)
	if err != nil {
		return errors.Wrap(err, "failed to generate webhook certificate")
	}

	operatorSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      OperatorCertSecretName,
			Namespace: g.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "cockroach-operator",
				managedByLabel:                managedByValue,
				"app.kubernetes.io/component": "webhook",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"ca.crt":  caCertPEM,
			"ca.key":  caKeyPEM,
			"tls.crt": tlsCertPEM,
			"tls.key": tlsKeyPEM,
		},
	}

	if err := g.ctl.Create(ctx, operatorSecret); err != nil {
		return errors.Wrap(err, "failed to create operator cert secret")
	}

	logging.Infof(ctx, "created operator certificate secret %s", OperatorCertSecretName)
	return nil
}

// GenerateCrdbCerts generates the resources required for a CockroachDB cluster
// to operate with TLS:
//
//   - cockroachdb-ca         ConfigMap  — CA certificate for pods to verify peers
//   - cockroachdb-node-certs Secret     — node server/peer TLS certificate
//   - cockroachdb-root-certs Secret     — root SQL client certificate
//
// CA resolution order:
//  1. Reuse the CA from cockroach-operator-certs if it contains ca.key (i.e.
//     -self-signed-operator-certs was also used). This ensures all certs share
//     a single CA.
//  2. Load or generate a standalone CA stored in cockroachdb-self-signed-ca.
//     This allows selfSigned:true to be used without -self-signed-operator-certs.
//
// Idempotent: resources that already exist and are operator-managed are left
// unchanged. An error is returned for any resource that exists but was not
// created by the operator.
func (g *SelfSignedCertGenerator) GenerateCrdbCerts(ctx context.Context) error {
	caCert, caKey, caCertPEM, err := g.resolveCA(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to resolve CA for CockroachDB certificates")
	}

	if err := g.ensureCrdbCAConfigMap(ctx, caCertPEM); err != nil {
		return err
	}
	if err := g.ensureCrdbNodeCertSecret(ctx, caCert, caKey, caCertPEM); err != nil {
		return err
	}
	if err := g.ensureCrdbRootClientCertSecret(ctx, caCert, caKey, caCertPEM); err != nil {
		return err
	}
	return nil
}

// resolveCA returns the CA certificate, key, and PEM-encoded cert to use for
// signing CockroachDB cluster certificates. It first looks for a CA key in the
// operator cert secret (cockroach-operator-certs). If that is unavailable, it
// falls back to a standalone CA persisted in cockroachdb-self-signed-ca.
func (g *SelfSignedCertGenerator) resolveCA(
	ctx context.Context,
) (caCert *x509.Certificate, caKey *rsa.PrivateKey, caCertPEM []byte, err error) {
	var operatorSecret corev1.Secret
	if getErr := g.ctl.Get(ctx, OperatorCertSecretName, &operatorSecret); getErr == nil {
		// Only reuse the CA from the operator secret when it is explicitly
		// labeled as operator-managed. If the secret was provided externally
		// and happens to include a CA key, using it would silently expand the
		// blast radius of a rotated or compromised key.
		if operatorSecret.Labels[managedByLabel] == managedByValue {
			certPEM := operatorSecret.Data["ca.crt"]
			keyPEM := operatorSecret.Data["ca.key"]
			if len(certPEM) > 0 && len(keyPEM) > 0 {
				cert, key, parseErr := parseCAFromPEM(certPEM, keyPEM)
				if parseErr == nil {
					logging.Infof(ctx, "reusing CA from %s for CockroachDB certificates", OperatorCertSecretName)
					return cert, key, certPEM, nil
				}
			}
		} else {
			logging.Infof(ctx, "%s exists but is not operator-managed; falling back to standalone CA", OperatorCertSecretName)
		}
	}

	return g.ensureStandaloneCA(ctx)
}

// ensureStandaloneCA loads the standalone CA from cockroachdb-self-signed-ca,
// or generates and persists a new one if it does not exist.
func (g *SelfSignedCertGenerator) ensureStandaloneCA(
	ctx context.Context,
) (caCert *x509.Certificate, caKey *rsa.PrivateKey, caCertPEM []byte, err error) {
	var caSecret corev1.Secret
	getErr := g.ctl.Get(ctx, g.standaloneCASecretName(), &caSecret)
	if getErr == nil {
		if v, ok := caSecret.Labels[managedByLabel]; !ok || v != managedByValue {
			return nil, nil, nil, errors.Newf(
				"standalone CA secret %q already exists and was not created by the operator. "+
					"Either remove the existing secret to allow self-signed cert generation, "+
					"or disable selfSigned",
				g.standaloneCASecretName(),
			)
		}
		cert, key, parseErr := parseCAFromPEM(caSecret.Data["ca.crt"], caSecret.Data["ca.key"])
		if parseErr != nil {
			return nil, nil, nil, errors.Wrapf(parseErr, "failed to parse existing standalone CA from %s", g.standaloneCASecretName())
		}
		logging.Debugf(ctx, "loaded standalone CA from %s", g.standaloneCASecretName())
		return cert, key, caSecret.Data["ca.crt"], nil
	}
	if !apierrors.IsNotFound(getErr) {
		return nil, nil, nil, errors.Wrapf(getErr, "failed to get standalone CA secret %s", g.standaloneCASecretName())
	}

	logging.Infof(ctx, "generating new standalone CA for CockroachDB certificates")
	newCACert, newCAKey, newCACertPEM, newCAKeyPEM, genErr := g.generateCA()
	if genErr != nil {
		return nil, nil, nil, errors.Wrap(genErr, "failed to generate standalone CA")
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      g.standaloneCASecretName(),
			Namespace: g.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "cockroach-operator",
				managedByLabel:                managedByValue,
				"app.kubernetes.io/component": "database",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"ca.crt": newCACertPEM,
			"ca.key": newCAKeyPEM,
		},
	}
	if err := g.setOwner(secret); err != nil {
		return nil, nil, nil, err
	}
	if createErr := g.ctl.Create(ctx, secret); createErr != nil {
		if apierrors.IsAlreadyExists(createErr) {
			// Another reconcile created the secret between our Get and Create.
			// Re-fetch it and return the CA it contains rather than the one we
			// just generated, since the caller must use the persisted CA.
			var existing corev1.Secret
			if getErr := g.ctl.Get(ctx, g.standaloneCASecretName(), &existing); getErr != nil {
				return nil, nil, nil, errors.Wrapf(getErr, "failed to re-get standalone CA secret %s after concurrent create", g.standaloneCASecretName())
			}
			if v, ok := existing.Labels[managedByLabel]; !ok || v != managedByValue {
				return nil, nil, nil, errors.Newf(
					"standalone CA secret %q already exists and was not created by the operator. "+
						"Either remove the existing secret to allow self-signed cert generation, "+
						"or disable selfSigned",
					g.standaloneCASecretName(),
				)
			}
			cert, key, parseErr := parseCAFromPEM(existing.Data["ca.crt"], existing.Data["ca.key"])
			if parseErr != nil {
				return nil, nil, nil, errors.Wrapf(parseErr, "failed to parse standalone CA from %s after concurrent create", g.standaloneCASecretName())
			}
			logging.Debugf(ctx, "standalone CA secret %s already exists (self-signed)", g.standaloneCASecretName())
			return cert, key, existing.Data["ca.crt"], nil
		}
		return nil, nil, nil, errors.Wrapf(createErr, "failed to persist standalone CA secret %s", g.standaloneCASecretName())
	}

	logging.Infof(ctx, "created standalone CA secret %s", g.standaloneCASecretName())
	return newCACert, newCAKey, newCACertPEM, nil
}

// ensureCrdbCAConfigMap creates the cockroachdb-ca ConfigMap if it does not
// already exist as an operator-managed resource.
func (g *SelfSignedCertGenerator) ensureCrdbCAConfigMap(ctx context.Context, caCertPEM []byte) error {
	var existing corev1.ConfigMap
	err := g.ctl.Get(ctx, manifests.SelfSignedCAConfigMapName(g.clusterName), &existing)
	if err == nil {
		if v, ok := existing.Labels[managedByLabel]; !ok || v != managedByValue {
			return errors.Newf(
				"certificates.selfSigned is enabled, but ConfigMap %q already exists "+
					"and was not created by the operator",
				manifests.SelfSignedCAConfigMapName(g.clusterName),
			)
		}
		logging.Debugf(ctx, "CockroachDB CA ConfigMap %s already exists (self-signed)", manifests.SelfSignedCAConfigMapName(g.clusterName))
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return errors.Wrap(err, "failed to get CockroachDB CA ConfigMap")
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      manifests.SelfSignedCAConfigMapName(g.clusterName),
			Namespace: g.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "cockroach-operator",
				managedByLabel:                managedByValue,
				"app.kubernetes.io/component": "database",
			},
		},
		Data: map[string]string{
			"ca.crt": string(caCertPEM),
		},
	}
	if err := g.setOwner(cm); err != nil {
		return err
	}
	if err := g.ctl.Create(ctx, cm); err != nil {
		// Another reconcile may have created the ConfigMap after our initial Get.
		// Re-fetch and verify it is operator-managed before treating this as a success.
		if apierrors.IsAlreadyExists(err) {
			if getErr := g.ctl.Get(ctx, manifests.SelfSignedCAConfigMapName(g.clusterName), &existing); getErr != nil {
				return errors.Wrap(getErr, "failed to re-get CockroachDB CA ConfigMap after concurrent create")
			}
			if v, ok := existing.Labels[managedByLabel]; !ok || v != managedByValue {
				return errors.Newf(
					"certificates.selfSigned is enabled, but ConfigMap %q already exists "+
						"and was not created by the operator",
					manifests.SelfSignedCAConfigMapName(g.clusterName),
				)
			}
			logging.Debugf(ctx, "CockroachDB CA ConfigMap %s already exists (self-signed)", manifests.SelfSignedCAConfigMapName(g.clusterName))
			return nil
		}
		return errors.Wrap(err, "failed to create CockroachDB CA ConfigMap")
	}
	logging.Infof(ctx, "created CockroachDB CA ConfigMap %s", manifests.SelfSignedCAConfigMapName(g.clusterName))
	return nil
}

// ensureCrdbNodeCertSecret creates the cockroachdb-node-certs Secret if it does
// not already exist as an operator-managed resource.
func (g *SelfSignedCertGenerator) ensureCrdbNodeCertSecret(
	ctx context.Context, caCert *x509.Certificate, caKey *rsa.PrivateKey, caCertPEM []byte,
) error {
	var existing corev1.Secret
	err := g.ctl.Get(ctx, manifests.SelfSignedNodeCertSecretName(g.clusterName), &existing)
	if err == nil {
		if v, ok := existing.Labels[managedByLabel]; !ok || v != managedByValue {
			return errors.Newf(
				"certificates.selfSigned is enabled, but secret %q already exists "+
					"and was not created by the operator",
				manifests.SelfSignedNodeCertSecretName(g.clusterName),
			)
		}
		// TODO: if the cluster domain changes after the node cert is first issued,
		// the existing cert's DNS SANs become stale. We skip regeneration here for
		// idempotency, so the mismatch is silently ignored. A future improvement
		// should detect domain changes and reissue the cert.
		logging.Debugf(ctx, "CockroachDB node cert secret %s already exists (self-signed)", manifests.SelfSignedNodeCertSecretName(g.clusterName))
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return errors.Wrap(err, "failed to get CockroachDB node cert secret")
	}

	certPEM, keyPEM, err := g.generateNodeCert(caCert, caKey)
	if err != nil {
		return errors.Wrap(err, "failed to generate CockroachDB node certificate")
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      manifests.SelfSignedNodeCertSecretName(g.clusterName),
			Namespace: g.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "cockroach-operator",
				managedByLabel:                managedByValue,
				"app.kubernetes.io/component": "database",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"ca.crt":  caCertPEM,
			"tls.crt": certPEM,
			"tls.key": keyPEM,
		},
	}
	if err := g.setOwner(secret); err != nil {
		return err
	}
	if err := g.ctl.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if getErr := g.ctl.Get(ctx, manifests.SelfSignedNodeCertSecretName(g.clusterName), &existing); getErr != nil {
				return errors.Wrap(getErr, "failed to re-get CockroachDB node cert secret after concurrent create")
			}
			if v, ok := existing.Labels[managedByLabel]; !ok || v != managedByValue {
				return errors.Newf(
					"certificates.selfSigned is enabled, but secret %q already exists "+
						"and was not created by the operator",
					manifests.SelfSignedNodeCertSecretName(g.clusterName),
				)
			}
			logging.Debugf(ctx, "CockroachDB node cert secret %s already exists (self-signed)", manifests.SelfSignedNodeCertSecretName(g.clusterName))
			return nil
		}
		return errors.Wrap(err, "failed to create CockroachDB node cert secret")
	}
	logging.Infof(ctx, "created CockroachDB node cert secret %s", manifests.SelfSignedNodeCertSecretName(g.clusterName))
	return nil
}

// ensureCrdbRootClientCertSecret creates the cockroachdb-root-certs Secret if it
// does not already exist as an operator-managed resource.
func (g *SelfSignedCertGenerator) ensureCrdbRootClientCertSecret(
	ctx context.Context, caCert *x509.Certificate, caKey *rsa.PrivateKey, caCertPEM []byte,
) error {
	var existing corev1.Secret
	err := g.ctl.Get(ctx, manifests.SelfSignedRootClientSecretName(g.clusterName), &existing)
	if err == nil {
		if v, ok := existing.Labels[managedByLabel]; !ok || v != managedByValue {
			return errors.Newf(
				"certificates.selfSigned is enabled, but secret %q already exists "+
					"and was not created by the operator",
				manifests.SelfSignedRootClientSecretName(g.clusterName),
			)
		}
		logging.Debugf(ctx, "CockroachDB root client cert secret %s already exists (self-signed)", manifests.SelfSignedRootClientSecretName(g.clusterName))
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return errors.Wrap(err, "failed to get CockroachDB root client cert secret")
	}

	certPEM, keyPEM, err := g.generateRootClientCert(caCert, caKey)
	if err != nil {
		return errors.Wrap(err, "failed to generate CockroachDB root client certificate")
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      manifests.SelfSignedRootClientSecretName(g.clusterName),
			Namespace: g.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "cockroach-operator",
				managedByLabel:                managedByValue,
				"app.kubernetes.io/component": "database",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"ca.crt":  caCertPEM,
			"tls.crt": certPEM,
			"tls.key": keyPEM,
		},
	}
	if err := g.setOwner(secret); err != nil {
		return err
	}
	if err := g.ctl.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if getErr := g.ctl.Get(ctx, manifests.SelfSignedRootClientSecretName(g.clusterName), &existing); getErr != nil {
				return errors.Wrap(getErr, "failed to re-get CockroachDB root client cert secret after concurrent create")
			}
			if v, ok := existing.Labels[managedByLabel]; !ok || v != managedByValue {
				return errors.Newf(
					"certificates.selfSigned is enabled, but secret %q already exists "+
						"and was not created by the operator",
					manifests.SelfSignedRootClientSecretName(g.clusterName),
				)
			}
			logging.Debugf(ctx, "CockroachDB root client cert secret %s already exists (self-signed)", manifests.SelfSignedRootClientSecretName(g.clusterName))
			return nil
		}
		return errors.Wrap(err, "failed to create CockroachDB root client cert secret")
	}
	logging.Infof(ctx, "created CockroachDB root client cert secret %s", manifests.SelfSignedRootClientSecretName(g.clusterName))
	return nil
}

// generateCA creates a new self-signed CA certificate and private key.
func (g *SelfSignedCertGenerator) generateCA() (*x509.Certificate, *rsa.PrivateKey, []byte, []byte, error) {
	// Generate CA private key
	caPrivKey, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		return nil, nil, nil, nil, errors.Wrap(err, "failed to generate CA private key")
	}

	// Generate a random serial number
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, nil, nil, errors.Wrap(err, "failed to generate serial number")
	}

	now := time.Now()
	caCertTemplate := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"CockroachDB Operator"},
			CommonName:   "cockroach-ca",
		},
		NotBefore:             now,
		NotAfter:              now.AddDate(certValidityYears, 0, 0),
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	// Self-sign the CA certificate
	caBytes, err := x509.CreateCertificate(rand.Reader, caCertTemplate, caCertTemplate, &caPrivKey.PublicKey, caPrivKey)
	if err != nil {
		return nil, nil, nil, nil, errors.Wrap(err, "failed to create CA certificate")
	}

	// PEM encode the CA certificate
	caCertPEM := new(bytes.Buffer)
	if err := pem.Encode(caCertPEM, &pem.Block{Type: "CERTIFICATE", Bytes: caBytes}); err != nil {
		return nil, nil, nil, nil, errors.Wrap(err, "failed to PEM encode CA certificate")
	}

	// PEM encode the CA private key
	caKeyPEM := new(bytes.Buffer)
	if err := pem.Encode(caKeyPEM, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(caPrivKey)}); err != nil {
		return nil, nil, nil, nil, errors.Wrap(err, "failed to PEM encode CA private key")
	}

	// Parse the certificate back to get the full x509.Certificate
	parsedCACert, err := x509.ParseCertificate(caBytes)
	if err != nil {
		return nil, nil, nil, nil, errors.Wrap(err, "failed to parse CA certificate")
	}

	return parsedCACert, caPrivKey, caCertPEM.Bytes(), caKeyPEM.Bytes(), nil
}

// generateWebhookCert creates a TLS certificate for the operator webhook, signed by the CA.
func (g *SelfSignedCertGenerator) generateWebhookCert(caCert *x509.Certificate, caKey *rsa.PrivateKey) ([]byte, []byte, error) {
	// Generate certificate private key
	certPrivKey, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to generate certificate private key")
	}

	// Generate a random serial number
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to generate serial number")
	}

	now := time.Now()
	certTemplate := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject:      pkix.Name{CommonName: "webhook"},
		NotBefore:    now,
		NotAfter:     now.AddDate(certValidityYears, 0, 0),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		DNSNames: []string{
			fmt.Sprintf("cockroach-webhook-service.%s.svc", g.namespace),
			fmt.Sprintf("cockroach-operator.%s.svc", g.namespace),
			fmt.Sprintf("cockroach-webhook-service.%s.svc.cluster.local", g.namespace),
			fmt.Sprintf("cockroach-operator.%s.svc.cluster.local", g.namespace),
		},
	}

	return signAndEncode(rand.Reader, certTemplate, caCert, &certPrivKey.PublicKey, caKey, certPrivKey)
}

// nodeCertDNSNames returns the DNS SANs for the node certificate. It always
// includes cluster.local FQDNs as the baseline. If a custom cluster domain is
// configured, equivalent FQDNs for that domain are appended so that clients
// using either domain can verify the server cert. This mirrors the pattern used
// by nodeCertificateSigningRequest in manifests/cert_secrets.go.
func (g *SelfSignedCertGenerator) nodeCertDNSNames() []string {
	ns := g.namespace
	cn := g.clusterName
	names := []string{
		// Wildcarded pod subdomain SANs. Pods use <podName>.<clusterName>.<ns>
		// FQDNs because the StatefulSet subdomain is set to the cluster name.
		fmt.Sprintf("*.%s", cn),
		fmt.Sprintf("*.%s.%s", cn, ns),
		fmt.Sprintf("*.%s.%s.svc.cluster.local", cn, ns),
		fmt.Sprintf("%s.%s.svc.cluster.local", cn, ns),
		// The join service is named <clusterName>-join and is used during
		// cluster bootstrapping.
		manifests.JoinFQDN(cn, ns, "cluster.local"),
		"localhost",
	}

	// Append SANs for the custom domain when it differs from the default. This
	// ensures TLS verification succeeds regardless of which domain clients use.
	if g.domain != "cluster.local" {
		names = append(names,
			fmt.Sprintf("*.%s.%s.svc.%s", cn, ns, g.domain),
			fmt.Sprintf("%s.%s.svc.%s", cn, ns, g.domain),
			manifests.JoinFQDN(cn, ns, g.domain),
		)
	}

	// Add join SANs for each region namespace that differs from the
	// generator's own namespace. Each region may use a different namespace
	// for its join service (e.g. when the region code is used as the
	// namespace), so the node cert must cover all of them.
	seen := map[string]bool{ns: true}
	for _, joinNS := range g.joinNamespaces {
		if seen[joinNS] {
			continue
		}
		seen[joinNS] = true
		names = append(names, manifests.JoinFQDN(cn, joinNS, "cluster.local"))
		if g.domain != "cluster.local" {
			names = append(names, manifests.JoinFQDN(cn, joinNS, g.domain))
		}
	}

	return names
}

// generateNodeCert creates a TLS certificate for CockroachDB nodes, signed by the CA.
// The certificate includes DNS SANs for all service names used by the operator
// to route intra-cluster traffic in the given namespace.
func (g *SelfSignedCertGenerator) generateNodeCert(caCert *x509.Certificate, caKey *rsa.PrivateKey) ([]byte, []byte, error) {
	certPrivKey, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to generate node cert private key")
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to generate serial number")
	}

	now := time.Now()
	certTemplate := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject:      pkix.Name{CommonName: "node"},
		NotBefore:    now,
		NotAfter:     now.AddDate(certValidityYears, 0, 0),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		// Include all service DNS names used by the operator for node-to-node
		// and client-to-node communication within the namespace. cluster.local
		// is always included as the default baseline. If a custom cluster domain
		// is configured, SANs for that domain are appended so that clients using
		// either domain can verify the server cert.
		DNSNames:    g.nodeCertDNSNames(),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}

	return signAndEncode(rand.Reader, certTemplate, caCert, &certPrivKey.PublicKey, caKey, certPrivKey)
}

// generateRootClientCert creates a SQL client certificate for the root user,
// signed by the CA.
func (g *SelfSignedCertGenerator) generateRootClientCert(caCert *x509.Certificate, caKey *rsa.PrivateKey) ([]byte, []byte, error) {
	certPrivKey, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to generate root client cert private key")
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to generate serial number")
	}

	now := time.Now()
	certTemplate := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject:      pkix.Name{CommonName: "root"},
		NotBefore:    now,
		NotAfter:     now.AddDate(certValidityYears, 0, 0),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}

	return signAndEncode(rand.Reader, certTemplate, caCert, &certPrivKey.PublicKey, caKey, certPrivKey)
}

// signAndEncode signs a certificate template with the given CA and PEM-encodes
// the resulting certificate and private key.
func signAndEncode(
	randReader interface{ Read([]byte) (int, error) },
	template, parent *x509.Certificate,
	pub *rsa.PublicKey,
	caKey, leafKey *rsa.PrivateKey,
) (certPEM, keyPEM []byte, err error) {
	certBytes, err := x509.CreateCertificate(randReader, template, parent, pub, caKey)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to create certificate")
	}

	// PEM encode the certificate
	certBuf := new(bytes.Buffer)
	if err := pem.Encode(certBuf, &pem.Block{Type: "CERTIFICATE", Bytes: certBytes}); err != nil {
		return nil, nil, errors.Wrap(err, "failed to PEM encode certificate")
	}

	// PEM encode the private key
	keyBuf := new(bytes.Buffer)
	if err := pem.Encode(keyBuf, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(leafKey)}); err != nil {
		return nil, nil, errors.Wrap(err, "failed to PEM encode private key")
	}

	return certBuf.Bytes(), keyBuf.Bytes(), nil
}

// parseCAFromPEM parses a CA certificate and RSA private key from PEM-encoded bytes.
func parseCAFromPEM(caCertPEM, caKeyPEM []byte) (*x509.Certificate, *rsa.PrivateKey, error) {
	certBlock, _ := pem.Decode(caCertPEM)
	if certBlock == nil {
		return nil, nil, errors.New("failed to decode CA certificate PEM block")
	}
	caCert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to parse CA certificate")
	}

	keyBlock, _ := pem.Decode(caKeyPEM)
	if keyBlock == nil {
		return nil, nil, errors.New("failed to decode CA private key PEM block")
	}
	caKey, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to parse CA private key")
	}

	return caCert, caKey, nil
}

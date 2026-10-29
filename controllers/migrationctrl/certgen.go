package migrationctrl

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"maps"
	"math/big"
	"net"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/migrationutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	defaultClusterDomain = migrationutil.DefaultClusterDomain
	nodeCertTTL          = 365 * 24 * time.Hour // 1 year
	rsaKeySize           = 2048
	organizationName     = "Cockroach"
)

// resolveSourceNodeSecretName determines the source node secret name, trying the operator
// naming first ({name}-node) then falling back to Helm naming ({name}-node-secret).
func resolveSourceNodeSecretName(ctx context.Context, ctl *kube.Ctl, clusterName string) string {
	name := clusterName + suffixOperatorNode
	var secret corev1.Secret
	err := ctl.Get(ctx, name, &secret)
	if err == nil {
		return name
	}
	if !apierrors.IsNotFound(err) {
		logging.Warnf(ctx, "unexpected error looking up %s, falling back to Helm naming: %v", name, err)
	}
	return clusterName + suffixNodeSecret
}

// upsertCertSecret creates or updates a TLS Secret with the provided data.
func upsertCertSecret(
	ctx context.Context, ctl *kube.Ctl, name, namespace string, data map[string][]byte,
) error {
	var secret corev1.Secret
	err := ctl.Get(ctx, name, &secret)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return errors.Wrapf(err, "failed to get secret %s", name)
		}
		secret = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Type: corev1.SecretTypeTLS,
			Data: data,
		}
		if err := ctl.Create(ctx, &secret); err != nil {
			return errors.Wrapf(err, "failed to create secret %s", name)
		}
		logging.Infof(ctx, "Created secret %s", name)
		return nil
	}
	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	maps.Copy(secret.Data, data)
	if err := ctl.Update(ctx, &secret); err != nil {
		return errors.Wrapf(err, "failed to update secret %s", name)
	}
	logging.Infof(ctx, "Updated secret %s", name)
	return nil
}

// regenerateNodeCertificatesForMigration regenerates node certificates with additional DNS SANs
// for the join service. During migration, new CrdbNode pods need to connect to old STS pods via
// the join service, so the node certificate must include join service DNS names as SANs.
// Only needed for self-signed certs; cert-manager handles SAN updates automatically.
//
// The destination secret is always {name}-node-secret. For operator migrations this is a new
// secret (the source CA lives in {name}-node). For Helm migrations it regenerates the existing
// secret in place.
func regenerateNodeCertificatesForMigration(
	ctx context.Context,
	ctl *kube.Ctl,
	clusterName string,
	namespace string,
	sourceNodeSecretName string,
	sourceCASecretName string,
	clusterDomain string,
) error {
	logging.Infof(ctx, "Regenerating node certificates with join service DNS SANs for cluster %s", clusterName)

	// Fall back to heuristic resolution if the caller didn't provide source names.
	if sourceNodeSecretName == "" {
		sourceNodeSecretName = resolveSourceNodeSecretName(ctx, ctl, clusterName)
	}

	// Load CA certificate and key from the cluster's existing secrets.
	caCertPEM, caKeyPEM, err := loadCASecretForMigration(ctx, ctl, clusterName, sourceNodeSecretName, sourceCASecretName)
	if err != nil {
		return errors.Wrap(err, "failed to load CA secret")
	}

	caCert, caKey, err := parseCAFromPEM(caCertPEM, caKeyPEM)
	if err != nil {
		return errors.Wrap(err, "failed to parse CA")
	}

	if clusterDomain == "" {
		clusterDomain = defaultClusterDomain
	}

	// Build DNS SANs list for node certificates. Includes the join service DNS names
	// so that CrdbNode pods can verify the TLS identity of old STS pods during migration.
	dnsNames := []string{
		"localhost",
		clusterName,
		fmt.Sprintf("%s.%s", clusterName, namespace),
		fmt.Sprintf("%s.%s.svc.%s", clusterName, namespace, clusterDomain),
		fmt.Sprintf("*.%s", clusterName),
		fmt.Sprintf("*.%s.%s", clusterName, namespace),
		fmt.Sprintf("*.%s.%s.svc.%s", clusterName, namespace, clusterDomain),
		// join service DNS names (critical for migration)
		fmt.Sprintf("%s-join", clusterName),
		fmt.Sprintf("%s-join.%s", clusterName, namespace),
		fmt.Sprintf("%s-join.%s.svc.%s", clusterName, namespace, clusterDomain),
	}

	nodeCertPEM, nodeKeyPEM, err := generateNodeCertificate(caCert, caKey, dnsNames)
	if err != nil {
		return errors.Wrap(err, "failed to generate node certificate")
	}

	// Always update/create the destination secret: {name}-node-secret.
	// This is where CrdbNodes will read certificates from.
	destNodeSecretName := clusterName + suffixNodeSecret
	if err := upsertCertSecret(ctx, ctl, destNodeSecretName, namespace, map[string][]byte{
		"tls.crt":  nodeCertPEM,
		"tls.key":  nodeKeyPEM,
		"node.crt": nodeCertPEM,
		"node.key": nodeKeyPEM,
		"ca.crt":   caCertPEM,
	}); err != nil {
		return errors.Wrapf(err, "failed to upsert node secret %s", destNodeSecretName)
	}

	logging.Infof(ctx, "Successfully regenerated node certificates for %s", clusterName)
	return nil
}

// loadCASecretForMigration loads the CA certificate and key from secrets.
// Try the dedicated CA secret first, then fall back to the node secret if that is
// where the CA cert was stored. Helm usually keeps ca.key in {name}-ca-secret.
// The public operator usually keeps both values in {name}-ca.
func loadCASecretForMigration(
	ctx context.Context,
	ctl *kube.Ctl,
	clusterName string,
	nodeSecretName string,
	caSecretName string,
) ([]byte, []byte, error) {
	// Use the caller-provided CA secret name when available. Fall back to
	// heuristic derivation from the node secret name for backward compatibility.
	if caSecretName == "" {
		if nodeSecretName == clusterName+suffixOperatorNode {
			caSecretName = clusterName + suffixOperatorCA
		} else {
			caSecretName = clusterName + suffixCASecret
		}
	}

	var caSecret corev1.Secret
	if err := ctl.Get(ctx, caSecretName, &caSecret); err != nil {
		// CA secret doesn't exist, try to get everything from node secret
		if apierrors.IsNotFound(err) {
			logging.Infof(ctx, "CA secret %s not found, loading from node secret", caSecretName)
			var nodeSecret corev1.Secret
			if err := ctl.Get(ctx, nodeSecretName, &nodeSecret); err != nil {
				return nil, nil, errors.Wrapf(err, "failed to get node secret %s", nodeSecretName)
			}
			caCertPEM, ok := nodeSecret.Data["ca.crt"]
			if !ok {
				return nil, nil, errors.Errorf("node secret %s missing ca.crt", nodeSecretName)
			}
			caKeyPEM, ok := nodeSecret.Data["ca.key"]
			if !ok {
				return nil, nil, errors.Errorf("node secret %s missing ca.key", nodeSecretName)
			}
			return caCertPEM, caKeyPEM, nil
		}
		return nil, nil, errors.Wrapf(err, "failed to get CA secret %s", caSecretName)
	}

	caKeyPEM, ok := caSecret.Data["ca.key"]
	if !ok {
		return nil, nil, errors.Errorf("CA secret %s missing ca.key", caSecretName)
	}

	caCertPEM, ok := caSecret.Data["ca.crt"]
	if ok {
		logging.Infof(ctx, "Loaded CA certificate and key from CA secret %s", caSecretName)
		return caCertPEM, caKeyPEM, nil
	}

	// ca.crt not in CA secret - get it from node secret (typical for Helm)
	logging.Infof(ctx, "CA secret %s has ca.key but not ca.crt, loading ca.crt from node secret", caSecretName)
	var nodeSecret corev1.Secret
	if err := ctl.Get(ctx, nodeSecretName, &nodeSecret); err != nil {
		return nil, nil, errors.Wrapf(err, "failed to get node secret %s", nodeSecretName)
	}

	caCertPEM, ok = nodeSecret.Data["ca.crt"]
	if !ok {
		return nil, nil, errors.Errorf("node secret %s missing ca.crt", nodeSecretName)
	}

	logging.Infof(ctx, "Loaded ca.key from CA secret %s and ca.crt from node secret %s", caSecretName, nodeSecretName)
	return caCertPEM, caKeyPEM, nil
}

// parseCAFromPEM parses CA certificate and private key from PEM format.
func parseCAFromPEM(certPEM, keyPEM []byte) (*x509.Certificate, *rsa.PrivateKey, error) {
	// Parse CA certificate
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, nil, errors.New("failed to parse CA certificate PEM")
	}
	caCert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to parse CA certificate")
	}

	// Parse CA private key
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, errors.New("failed to parse CA key PEM")
	}

	// Try PKCS1 format first
	// TODO: Support ECDSA and Ed25519 CA certificates in addition to RSA.
	caKey, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		// Try PKCS8 format
		keyInterface, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
		if err != nil {
			return nil, nil, errors.Wrap(err, "failed to parse CA private key")
		}
		var ok bool
		caKey, ok = keyInterface.(*rsa.PrivateKey)
		if !ok {
			return nil, nil, errors.New("CA private key is not RSA")
		}
	}

	return caCert, caKey, nil
}

// generateNodeCertificate creates a new node certificate signed by the provided CA.
func generateNodeCertificate(
	caCert *x509.Certificate, caKey *rsa.PrivateKey, dnsNames []string,
) (certPEM, keyPEM []byte, err error) {
	// Generate new node key pair
	nodeKey, err := rsa.GenerateKey(rand.Reader, rsaKeySize)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to generate node key")
	}

	// Create certificate template
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to generate serial number")
	}

	notBefore := time.Now().Add(-5 * time.Minute) // allow for clock skew
	notAfter := notBefore.Add(nodeCertTTL)

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{organizationName},
			CommonName:   "node",
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	// Create the certificate signed by the CA
	nodeCertDER, err := x509.CreateCertificate(rand.Reader, &template, caCert, &nodeKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to create node certificate")
	}

	// Encode certificate to PEM
	certBuffer := new(bytes.Buffer)
	if err = pem.Encode(certBuffer, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: nodeCertDER,
	}); err != nil {
		return nil, nil, errors.Wrap(err, "failed to encode certificate")
	}

	// Encode key to PEM
	keyBuffer := new(bytes.Buffer)
	if err = pem.Encode(keyBuffer, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(nodeKey),
	}); err != nil {
		return nil, nil, errors.Wrap(err, "failed to encode key")
	}

	return certBuffer.Bytes(), keyBuffer.Bytes(), nil
}

// regenerateClientCertificatesForMigration regenerates the root client certificate used by
// CrdbNodes for SQL and admin operations. The destination is always {name}-client-secret.
// For operator migrations this is a new secret (the original lives in {name}-root-client).
// For Helm migrations it regenerates the existing secret in place.
func regenerateClientCertificatesForMigration(
	ctx context.Context,
	ctl *kube.Ctl,
	clusterName string,
	namespace string,
	sourceNodeSecretName string,
	sourceCASecretName string,
) error {
	logging.Infof(ctx, "Regenerating client certificates for cluster %s", clusterName)

	// Fall back to heuristic resolution if the caller didn't provide source names.
	if sourceNodeSecretName == "" {
		sourceNodeSecretName = resolveSourceNodeSecretName(ctx, ctl, clusterName)
	}

	caCertPEM, caKeyPEM, err := loadCASecretForMigration(ctx, ctl, clusterName, sourceNodeSecretName, sourceCASecretName)
	if err != nil {
		return errors.Wrap(err, "failed to load CA secret")
	}

	caCert, caKey, err := parseCAFromPEM(caCertPEM, caKeyPEM)
	if err != nil {
		return errors.Wrap(err, "failed to parse CA")
	}

	clientCertPEM, clientKeyPEM, err := generateClientCertificate(caCert, caKey, "root")
	if err != nil {
		return errors.Wrap(err, "failed to generate client certificate")
	}

	// Always update/create the destination secret: {name}-client-secret.
	// This is where CrdbNodes will read certificates from.
	destClientSecretName := clusterName + suffixClientSecret
	if err := upsertCertSecret(ctx, ctl, destClientSecretName, namespace, map[string][]byte{
		"tls.crt": clientCertPEM,
		"tls.key": clientKeyPEM,
		"ca.crt":  caCertPEM,
	}); err != nil {
		return errors.Wrapf(err, "failed to upsert client secret %s", destClientSecretName)
	}

	logging.Infof(ctx, "Successfully regenerated client certificates for %s", clusterName)
	return nil
}

// generateClientCertificate creates a new client certificate signed by the provided CA.
func generateClientCertificate(
	caCert *x509.Certificate, caKey *rsa.PrivateKey, username string,
) (certPEM, keyPEM []byte, err error) {
	// Generate new client key pair
	clientKey, err := rsa.GenerateKey(rand.Reader, rsaKeySize)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to generate client key")
	}

	// Create certificate template
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to generate serial number")
	}

	notBefore := time.Now().Add(-5 * time.Minute) // allow for clock skew
	notAfter := notBefore.Add(nodeCertTTL)        // use same TTL as node certs

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{organizationName},
			CommonName:   username,
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}

	// Create the certificate signed by the CA
	clientCertDER, err := x509.CreateCertificate(rand.Reader, &template, caCert, &clientKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to create client certificate")
	}

	// Encode certificate to PEM
	certBuffer := new(bytes.Buffer)
	if err = pem.Encode(certBuffer, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: clientCertDER,
	}); err != nil {
		return nil, nil, errors.Wrap(err, "failed to encode certificate")
	}

	// Encode key to PEM
	keyBuffer := new(bytes.Buffer)
	if err = pem.Encode(keyBuffer, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(clientKey),
	}); err != nil {
		return nil, nil, errors.Wrap(err, "failed to encode key")
	}

	return certBuffer.Bytes(), keyBuffer.Bytes(), nil
}

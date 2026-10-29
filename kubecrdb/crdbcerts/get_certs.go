package crdbcerts

import (
	"context"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/crypto"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	corev1 "k8s.io/api/core/v1"
)

// GetBundle loads the CA certificate from the config map and the TLS certificates
// from the secret. It expects to find a key named ca.crt in the config map and
// values for tls.crt and tls.key in the secret.
func GetBundle(
	ctl *kube.Ctl, caConfigMap, secretName string,
) (caCert []byte, cert *crypto.Bundle, err error) {
	ctx := context.Background()

	var ca corev1.ConfigMap
	if err := ctl.Get(ctx, caConfigMap, &ca); err != nil {
		return nil, nil, errors.Wrapf(err, "getting config map for CA cert: %s", caConfigMap)
	}

	var secret corev1.Secret
	if err := ctl.Get(ctx, secretName, &secret); err != nil {
		return nil, nil, errors.Wrapf(err, "error getting certificate from secret: %s", secretName)
	}

	_, certs := crypto.FromSecretData(secret.Data)
	return []byte(ca.Data["ca.crt"]), certs["tls"], nil
}

// GetRootClientCertBundle returns the "root" user client certificate (and the
// CA that signed it) from the K8s secrets store. For more details, see the
// comment for manifests.CockroachClientCertSecretName.
func GetRootClientCertBundle(ctl *kube.Ctl) (caCert []byte, clientCert *crypto.Bundle, err error) {
	ca, bundle, err := getCertBundle(ctl, manifests.CockroachClientCertSecretName, manifests.CockroachClientCertName)
	if err == nil {
		return ca, bundle, nil
	}
	if !kubeerrors.IsNotFound(err) {
		return nil, nil, err
	}
	return GetBundle(ctl, manifests.CockroachNodeCAConfigMapName, manifests.CockroachRootSQLClientSecretName)
}

// GetNodeCertBundle returns the node certificate (and the CA that signed it)
// from the K8s secrets store. See the manifests.CockroachNodeCertSecretName
// comment for more details.
func GetNodeCertBundle(ctl *kube.Ctl) (caCert []byte, nodeCert *crypto.Bundle, err error) {
	ca, bundle, err := getCertBundle(ctl, manifests.CockroachNodeCertSecretName, manifests.CockroachNodeCertName)
	if err == nil {
		return ca, bundle, nil
	}
	if !kubeerrors.IsNotFound(err) {
		return nil, nil, err
	}
	return GetBundle(ctl, manifests.CockroachNodeCAConfigMapName, manifests.CockroachNodeClientSecretName)
}

// GetUICertBundle returns the UI TLS server certificate from the K8s secrets
// store. See the manifests.CockroachUICertSecretName comment for more details.
func GetUICertBundle(ctl *kube.Ctl) (extCert *crypto.Bundle, err error) {
	// No CA certificate for the UI bundle.
	_, extCert, err = getCertBundle(ctl, manifests.CockroachUICertSecretName, manifests.CockroachUICertName)
	return extCert, err
}

// GetSqlProxyCertBundle returns the external TLS server certificate from the
// K8s secrets store. See the manifests.SqlProxyExternalCertSecretName comment
// for more details.
func GetSqlProxyCertBundle(ctl *kube.Ctl) (proxyCert *crypto.Bundle, err error) {
	// No CA certificate for the SQL proxy bundle.
	_, proxyCert, err = getCertBundle(ctl, manifests.SqlProxyExternalCertSecretName, manifests.SqlProxyExternalCertName)
	return proxyCert, err
}

// GetOperatorCertBundle returns the operator's TLS certificate (and the CA that
// signed it) from the K8s secrets store.
func GetOperatorCertBundle(ctl *kube.Ctl) (caCert []byte, tlsCert *crypto.Bundle, err error) {
	return getCertBundle(ctl, manifests.OperatorCertSecretName, manifests.OperatorCertName)
}

// GetSqlStarterCertBundle returns the sqlstarter service's TLS certificate (and
// the CA that signed it) from the K8s secrets store. For more details, see the
// coment for manifests.SqlStarterCertSecretName.
func GetSqlStarterCertBundle(ctl *kube.Ctl) (caCert []byte, tlsCert *crypto.Bundle, err error) {
	return getCertBundle(ctl, manifests.SqlStarterCertSecretName, manifests.SqlStarterCertName)
}

// GetTenantClientCertBundle returns the given tenant's "client" certificate
// (and the CA that signed it) from the K8s secrets store. For more details, see
// the comment for manifests.TenantClientCertSecretNameFormat.
func GetTenantClientCertBundle(
	ctl *kube.Ctl, tenantID crdbv1beta1.TenantID,
) (caCert []byte, clientCert *crypto.Bundle, err error) {
	secretName := manifests.MakeTenantClientCertSecretName(tenantID)
	certName := manifests.MakeTenantClientCertName(tenantID)
	return getCertBundle(ctl, secretName, certName)
}

// GetTenantDirCirtBundle returns the tenantdir's client certificate and the
// cluster's CA.
func GetTenantDirCertBundle(ctl *kube.Ctl) (caCert []byte, tlsCert *crypto.Bundle, err error) {
	return getCertBundle(ctl, manifests.TenantDirCertSecretName, manifests.TenantDirCertName)
}

func getCertBundle(
	ctl *kube.Ctl, secretName, certName string,
) (caCert []byte, certBundle *crypto.Bundle, err error) {
	var secret corev1.Secret
	if err := ctl.Get(context.Background(), secretName, &secret); err != nil {
		return nil, nil, errors.Wrapf(err, "error getting certificate from %s K8s secret", secretName)
	}
	ca, certs := crypto.FromSecretData(secret.Data)
	return ca, certs[certName], nil
}

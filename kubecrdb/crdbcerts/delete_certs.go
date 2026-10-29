package crdbcerts

import (
	"context"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeleteTenantCerts deletes secrets containing the certificates for the given
// tenant in the K8s cluster. If the certificate is not present, then no-op for
// idempotency.
func DeleteTenantCerts(ctx context.Context, ctl *kube.Ctl, tenantID crdbv1beta1.TenantID) error {
	if err := deleteSecret(ctx, ctl, manifests.MakeTenantClientCertSecretName(tenantID)); err != nil {
		return err
	}
	return nil
}

// deleteCerts deletes the secret with the given name from the K8s cluster.
func deleteSecret(ctx context.Context, ctl *kube.Ctl, secretName string) error {
	secret := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName}}
	if err := ctl.Delete(ctx, &secret); err != nil && !kubeerrors.IsNotFound(err) {
		return err
	}
	return nil
}

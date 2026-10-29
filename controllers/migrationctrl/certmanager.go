package migrationctrl

import (
	"context"
	"fmt"
	"slices"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/migrationutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	certManagerGroup    = "cert-manager.io"
	certManagerVersion  = "v1"
	certificateResource = "certificates"
	issuerResource      = "issuers"
	certificateKind     = "Certificate"
)

// newCertManagerObj returns a fresh unstructured object pre-populated with the cert-manager
// group/version and the given Kind. Callers pass certificateKind or issuerKind.
func newCertManagerObj(kind string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   certManagerGroup,
		Version: certManagerVersion,
		Kind:    kind,
	})
	return obj
}

// warnAboutCertManagerResources checks whether cert-manager Issuer and Certificate CRs
// exist for the cluster and warns the user to back them up manually. Helm upgrade may
// remove these resources, so the warning gives operators a chance to save them before
// proceeding with migration.
func warnAboutCertManagerResources(ctx context.Context, ctl *kube.Ctl, clusterName string) error {
	logging.Infof(ctx, "Checking for cert-manager resources for cluster %s", clusterName)

	resources := []struct {
		name     string
		resource string
		gvr      schema.GroupVersionResource
	}{
		{
			name:     clusterName + suffixOperatorCAIssuer,
			resource: "Issuer",
			gvr: schema.GroupVersionResource{
				Group:    certManagerGroup,
				Version:  certManagerVersion,
				Resource: issuerResource,
			},
		},
		{
			name:     clusterName + suffixOperatorCACert,
			resource: "Certificate",
			gvr: schema.GroupVersionResource{
				Group:    certManagerGroup,
				Version:  certManagerVersion,
				Resource: certificateResource,
			},
		},
	}

	backedUpCount := 0
	for _, res := range resources {
		// Try to get the resource
		obj := newCertManagerObj(res.resource)

		err := ctl.Get(ctx, res.name, obj)
		if apierrors.IsNotFound(err) {
			logging.Infof(ctx, "%s %s not found, skipping backup", res.resource, res.name)
			continue
		}
		if err != nil {
			// If cert-manager CRDs are not installed, we'll get a "no matches for kind" error
			// This is expected and not an error condition
			logging.Infof(ctx, "Could not check for %s %s (cert-manager may not be installed): %v", res.resource, res.name, err)
			continue
		}

		// Check if resource is managed by Helm (has Helm annotations)
		annotations := obj.GetAnnotations()
		if _, ok := annotations["meta.helm.sh/release-name"]; !ok {
			logging.Infof(ctx, "%s %s is not managed by Helm, skipping backup", res.resource, res.name)
			continue
		}

		// The old helper writes these resources to disk. We do not, so warn here
		// while the original Helm-managed objects still exist.
		logging.Warnf(ctx, "Found Helm-managed %s %s - please manually back up cert-manager resources before migration", res.resource, res.name)
		logging.Warnf(ctx, "After Helm upgrade, you may need to recreate: %s", res.name)
		backedUpCount++
	}

	if backedUpCount > 0 {
		logging.Warnf(ctx, "Helm upgrade may remove cert-manager resources - back them up manually before proceeding")
		logging.Warnf(ctx, "Command: kubectl get issuer,certificate -n %s -o yaml > cert-manager-backup.yaml", ctl.Namespace())
	} else {
		logging.Infof(ctx, "No cert-manager resources found or they are not managed by Helm")
	}

	return nil
}

// detectCertManagerCertificates checks if cert-manager Certificates exist for the cluster.
// When cert-manager is in use the migration skips self-signed cert generation.
func detectCertManagerCertificates(ctx context.Context, ctl *kube.Ctl, clusterName string) (bool, error) {
	logging.Infof(ctx, "Detecting cert-manager usage for cluster %s", clusterName)

	nodeCertName := clusterName + suffixOperatorNodeCert
	nodeCert := newCertManagerObj(certificateKind)

	err := ctl.Get(ctx, nodeCertName, nodeCert)
	if apierrors.IsNotFound(err) {
		logging.Infof(ctx, "No cert-manager Certificate found, assuming self-signer certificates")
		return false, nil
	}
	if err != nil {
		// CRDs not installed or other error - assume no cert-manager
		logging.Infof(ctx, "Could not check for cert-manager Certificate (may not be installed)")
		return false, nil
	}

	logging.Infof(ctx, "Detected cert-manager Certificate: %s", nodeCertName)
	return true, nil
}

// getCertManagerCertificate tries each candidate name in order and returns the first
// cert-manager Certificate CR found. The public operator and Helm use different naming
// conventions for the client Certificate CR ({name}-root-client vs {name}-root), so
// callers pass multiple candidates.
func getCertManagerCertificate(ctx context.Context, ctl *kube.Ctl, candidates ...string) (*unstructured.Unstructured, string, bool) {
	for _, name := range candidates {
		cert := newCertManagerObj(certificateKind)
		if err := ctl.Get(ctx, name, cert); err == nil {
			return cert, name, true
		}
	}
	return nil, "", false
}

// ensureCertManagerCertificateSecrets verifies that cert-manager has created the expected Secrets.
func ensureCertManagerCertificateSecrets(ctx context.Context, ctl *kube.Ctl, clusterName string) error {
	logging.Infof(ctx, "Verifying cert-manager certificate Secrets for cluster %s", clusterName)

	certCandidates := [][]string{
		{clusterName + suffixOperatorNodeCert},
		{clusterName + suffixOperatorClientCert, clusterName + suffixHelmClientCert},
	}

	for _, candidates := range certCandidates {
		cert, resolvedName, found := getCertManagerCertificate(ctx, ctl, candidates...)
		if !found {
			logging.Warnf(ctx, "cert-manager Certificate not found for candidates %v", candidates)
			continue
		}

		spec, _ := cert.Object["spec"].(map[string]any)
		secretName := certManagerSecretName(spec, resolvedName)

		secret := &unstructured.Unstructured{}
		secret.SetAPIVersion("v1")
		secret.SetKind("Secret")
		if err := ctl.Get(ctx, secretName, secret); err != nil {
			if apierrors.IsNotFound(err) {
				logging.Warnf(ctx, "Certificate %s exists but Secret %s not found - cert-manager may still be creating it", resolvedName, secretName)
				continue
			}
			return errors.Wrapf(err, "failed to check Secret %s", secretName)
		}

		logging.Infof(ctx, "Verified cert-manager Secret: %s", secretName)
	}

	return nil
}

// resolveCertSecretNames sets stsCtx.NodeSecretName and stsCtx.ClientSecretName to the actual
// K8s Secret names that hold the cluster's certs. If cert-manager leaves spec.secretName
// empty, the Certificate name itself is the Secret name.
func resolveCertSecretNames(ctx context.Context, ctl *kube.Ctl, clusterName string, stsCtx *migrationutil.ParsedMigrationContext) {
	nodeCert, nodeCertName, found := getCertManagerCertificate(ctx, ctl, clusterName+suffixOperatorNodeCert)
	if !found {
		// no cert-manager Certificate CR found; self-signer defaults apply
		return
	}

	spec, _ := nodeCert.Object["spec"].(map[string]any)
	nodeSecretName := certManagerSecretName(spec, nodeCertName)
	stsCtx.SourceNodeSecretName = nodeSecretName
	stsCtx.NodeSecretName = nodeSecretName
	logging.Infof(ctx, "Cert-manager node secret resolved from Certificate CR: %s", nodeSecretName)

	clientCert, clientCertName, found := getCertManagerCertificate(ctx, ctl,
		clusterName+suffixOperatorClientCert, clusterName+suffixHelmClientCert)
	if !found {
		return
	}

	spec, _ = clientCert.Object["spec"].(map[string]any)
	clientSecretName := certManagerSecretName(spec, clientCertName)
	stsCtx.SourceClientSecretName = clientSecretName
	stsCtx.ClientSecretName = clientSecretName
	stsCtx.HTTPSecretName = clientSecretName
	logging.Infof(ctx, "Cert-manager client secret resolved from Certificate CR: %s", clientSecretName)
}

// certManagerSecretName extracts the secretName from a cert-manager Certificate spec.
func certManagerSecretName(spec map[string]any, defaultName string) string {
	if secretName, _ := spec["secretName"].(string); secretName != "" {
		return secretName
	}
	return defaultName
}

// updateCertManagerNodeCertificate updates the node Certificate CR to include join service DNS
// names. During migration, CrdbNode pods connect to old STS pods via the join service. The
// node certificate must include these DNS SANs so TLS verification succeeds across both pod types.
func updateCertManagerNodeCertificate(ctx context.Context, ctl *kube.Ctl, clusterName, namespace, domain string) error {
	logging.Infof(ctx, "Updating cert-manager node Certificate CR to include join service DNS names")

	nodeCertName := clusterName + suffixOperatorNodeCert
	nodeCert := newCertManagerObj(certificateKind)

	if err := ctl.Get(ctx, nodeCertName, nodeCert); err != nil {
		return errors.Wrapf(err, "failed to get node Certificate %s", nodeCertName)
	}

	// Extract current spec
	spec, ok := nodeCert.Object["spec"].(map[string]any)
	if !ok {
		return errors.Errorf("Certificate %s has no spec", nodeCertName)
	}

	// Get current DNS names
	dnsNamesInterface, _ := spec["dnsNames"].([]any)
	currentDNSNames := make([]string, 0, len(dnsNamesInterface))
	for _, name := range dnsNamesInterface {
		if str, ok := name.(string); ok {
			currentDNSNames = append(currentDNSNames, str)
		}
	}

	// Add join service DNS names so CrdbNode pods can discover and verify each other.
	clusterDomain := defaultClusterDomain // overridden for custom-domain clusters
	if domain != "" {
		clusterDomain = domain
	}
	joinDNSNames := []string{
		fmt.Sprintf("%s-join", clusterName),
		fmt.Sprintf("%s-join.%s", clusterName, namespace),
		fmt.Sprintf("%s-join.%s.svc.%s", clusterName, namespace, clusterDomain),
	}

	// Check if we need to add any DNS names
	needsUpdate := false
	for _, joinDNS := range joinDNSNames {
		found := slices.Contains(currentDNSNames, joinDNS)
		if !found {
			currentDNSNames = append(currentDNSNames, joinDNS)
			needsUpdate = true
			logging.Infof(ctx, "Adding DNS name to Certificate: %s", joinDNS)
		}
	}

	if !needsUpdate {
		logging.Infof(ctx, "Certificate %s already has join service DNS names", nodeCertName)
		return nil
	}

	// Convert back to []any for unstructured
	updatedDNSNames := make([]any, len(currentDNSNames))
	for i, name := range currentDNSNames {
		updatedDNSNames[i] = name
	}
	spec["dnsNames"] = updatedDNSNames

	// Update the Certificate CR
	if err := ctl.Update(ctx, nodeCert); err != nil {
		return errors.Wrapf(err, "failed to update Certificate %s", nodeCertName)
	}

	logging.Infof(ctx, "Successfully updated Certificate %s with join service DNS names", nodeCertName)
	logging.Infof(ctx, "cert-manager will automatically regenerate the Secret with new certificates")

	return nil
}

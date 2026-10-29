package certs

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	testNamespace   = "test-ns"
	testClusterName = "test-cluster"
)

func TestGenerateOperatorCerts_NoExistingSecret(t *testing.T) {
	ctl := kubetest.NewFakeCtl(kubetest.Options{})
	gen := NewSelfSignedCertGenerator(ctl, testNamespace, "", "")

	ctx := context.Background()
	require.NoError(t, gen.GenerateOperatorCerts(ctx))

	// Verify the secret was created with the correct labels.
	var secret corev1.Secret
	require.NoError(t, ctl.WithNamespace(testNamespace).Get(ctx, OperatorCertSecretName, &secret))
	require.Equal(t, managedByValue, secret.Labels[managedByLabel])
	require.Contains(t, secret.Data, "ca.crt")
	require.Contains(t, secret.Data, "ca.key")
	require.Contains(t, secret.Data, "tls.crt")
	require.Contains(t, secret.Data, "tls.key")
}

func TestGenerateOperatorCerts_ExistingSelfSignedSecret(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      OperatorCertSecretName,
			Namespace: testNamespace,
			Labels: map[string]string{
				managedByLabel: managedByValue,
			},
		},
		Data: map[string][]byte{
			"ca.crt":  []byte("ca"),
			"tls.crt": []byte("cert"),
			"tls.key": []byte("key"),
		},
	}

	ctl := kubetest.NewFakeCtl(kubetest.Options{}, existing)
	gen := NewSelfSignedCertGenerator(ctl, testNamespace, "", "")

	ctx := context.Background()
	require.NoError(t, gen.GenerateOperatorCerts(ctx))

	// Verify the original secret data is unchanged (no regeneration).
	var secret corev1.Secret
	require.NoError(t, ctl.WithNamespace(testNamespace).Get(ctx, OperatorCertSecretName, &secret))
	require.Equal(t, []byte("ca"), secret.Data["ca.crt"])
}

func TestGenerateOperatorCerts_ExistingUserProvidedSecret(t *testing.T) {
	// Secret exists but was NOT created by the operator (no managed-by label).
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      OperatorCertSecretName,
			Namespace: testNamespace,
		},
		Data: map[string][]byte{
			"tls.crt": []byte("user-cert"),
			"tls.key": []byte("user-key"),
		},
	}

	ctl := kubetest.NewFakeCtl(kubetest.Options{}, existing)
	gen := NewSelfSignedCertGenerator(ctl, testNamespace, "", "")

	ctx := context.Background()
	err := gen.GenerateOperatorCerts(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "was not created by the operator")
}

func TestGenerateOperatorCerts_ExistingSecretWrongManagedByValue(t *testing.T) {
	// Secret exists with managed-by label set to something other than the operator.
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      OperatorCertSecretName,
			Namespace: testNamespace,
			Labels: map[string]string{
				managedByLabel: "cert-manager",
			},
		},
		Data: map[string][]byte{
			"tls.crt": []byte("cert-manager-cert"),
			"tls.key": []byte("cert-manager-key"),
		},
	}

	ctl := kubetest.NewFakeCtl(kubetest.Options{}, existing)
	gen := NewSelfSignedCertGenerator(ctl, testNamespace, "", "")

	err := gen.GenerateOperatorCerts(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "was not created by the operator")
}

func TestGenerateCrdbCerts_CreatesAllResources(t *testing.T) {
	ctl := kubetest.NewFakeCtl(kubetest.Options{})
	gen := NewSelfSignedCertGenerator(ctl, testNamespace, "", testClusterName)
	ctx := context.Background()

	// Operator certs must exist first (they hold the CA key).
	require.NoError(t, gen.GenerateOperatorCerts(ctx))
	require.NoError(t, gen.GenerateCrdbCerts(ctx))

	nsCtl := ctl.WithNamespace(testNamespace)

	// Verify CA ConfigMap.
	var cm corev1.ConfigMap
	require.NoError(t, nsCtl.Get(ctx, manifests.SelfSignedCAConfigMapName(testClusterName), &cm))
	require.Equal(t, managedByValue, cm.Labels[managedByLabel])
	require.Contains(t, cm.Data, "ca.crt")
	require.NotEmpty(t, cm.Data["ca.crt"])

	// Verify node cert secret.
	var nodeSecret corev1.Secret
	require.NoError(t, nsCtl.Get(ctx, manifests.SelfSignedNodeCertSecretName(testClusterName), &nodeSecret))
	require.Equal(t, managedByValue, nodeSecret.Labels[managedByLabel])
	require.Contains(t, nodeSecret.Data, "ca.crt")
	require.Contains(t, nodeSecret.Data, "tls.crt")
	require.Contains(t, nodeSecret.Data, "tls.key")

	// Verify root client cert secret.
	var rootSecret corev1.Secret
	require.NoError(t, nsCtl.Get(ctx, manifests.SelfSignedRootClientSecretName(testClusterName), &rootSecret))
	require.Equal(t, managedByValue, rootSecret.Labels[managedByLabel])
	require.Contains(t, rootSecret.Data, "ca.crt")
	require.Contains(t, rootSecret.Data, "tls.crt")
	require.Contains(t, rootSecret.Data, "tls.key")
}

func TestGenerateCrdbCerts_CASharedWithOperator(t *testing.T) {
	ctl := kubetest.NewFakeCtl(kubetest.Options{})
	gen := NewSelfSignedCertGenerator(ctl, testNamespace, "", testClusterName)
	ctx := context.Background()

	require.NoError(t, gen.GenerateOperatorCerts(ctx))
	require.NoError(t, gen.GenerateCrdbCerts(ctx))

	nsCtl := ctl.WithNamespace(testNamespace)

	var operatorSecret corev1.Secret
	require.NoError(t, nsCtl.Get(ctx, OperatorCertSecretName, &operatorSecret))

	var nodeSecret corev1.Secret
	require.NoError(t, nsCtl.Get(ctx, manifests.SelfSignedNodeCertSecretName(testClusterName), &nodeSecret))

	// All resources must share the same CA certificate.
	require.Equal(t, operatorSecret.Data["ca.crt"], nodeSecret.Data["ca.crt"])

	var cm corev1.ConfigMap
	require.NoError(t, nsCtl.Get(ctx, manifests.SelfSignedCAConfigMapName(testClusterName), &cm))
	require.Equal(t, string(operatorSecret.Data["ca.crt"]), cm.Data["ca.crt"])
}

func TestGenerateCrdbCerts_IdempotentOnExistingResources(t *testing.T) {
	ctl := kubetest.NewFakeCtl(kubetest.Options{})
	gen := NewSelfSignedCertGenerator(ctl, testNamespace, "", testClusterName)
	ctx := context.Background()

	require.NoError(t, gen.GenerateOperatorCerts(ctx))
	require.NoError(t, gen.GenerateCrdbCerts(ctx))

	// Capture node cert before second call.
	nsCtl := ctl.WithNamespace(testNamespace)
	var before corev1.Secret
	require.NoError(t, nsCtl.Get(ctx, manifests.SelfSignedNodeCertSecretName(testClusterName), &before))

	// Second call must succeed and leave existing resources unchanged.
	require.NoError(t, gen.GenerateCrdbCerts(ctx))

	var after corev1.Secret
	require.NoError(t, nsCtl.Get(ctx, manifests.SelfSignedNodeCertSecretName(testClusterName), &after))
	require.Equal(t, before.Data["tls.crt"], after.Data["tls.crt"])
}

func TestGenerateCrdbCerts_ErrorOnUserProvidedNodeCert(t *testing.T) {
	existingNode := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      manifests.SelfSignedNodeCertSecretName(testClusterName),
			Namespace: testNamespace,
		},
		Data: map[string][]byte{"tls.crt": []byte("user-node-cert")},
	}

	ctl := kubetest.NewFakeCtl(kubetest.Options{}, existingNode)
	gen := NewSelfSignedCertGenerator(ctl, testNamespace, "", testClusterName)
	ctx := context.Background()

	require.NoError(t, gen.GenerateOperatorCerts(ctx))

	err := gen.GenerateCrdbCerts(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "was not created by the operator")
}

func TestGenerateCrdbCerts_StandaloneCA_WhenOperatorSecretAbsent(t *testing.T) {
	ctl := kubetest.NewFakeCtl(kubetest.Options{})
	gen := NewSelfSignedCertGenerator(ctl, testNamespace, "", testClusterName)
	ctx := context.Background()

	// Do NOT call GenerateOperatorCerts. The generator must fall back to a
	// standalone CA stored in <clusterName>-cockroachdb-self-signed-ca.
	require.NoError(t, gen.GenerateCrdbCerts(ctx))

	nsCtl := ctl.WithNamespace(testNamespace)

	// Standalone CA secret must have been created.
	var caSecret corev1.Secret
	require.NoError(t, nsCtl.Get(ctx, testClusterName+"-cockroachdb-self-signed-ca", &caSecret))
	require.Contains(t, caSecret.Data, "ca.crt")
	require.Contains(t, caSecret.Data, "ca.key")

	// CRDB resources must exist and share the standalone CA.
	var cm corev1.ConfigMap
	require.NoError(t, nsCtl.Get(ctx, manifests.SelfSignedCAConfigMapName(testClusterName), &cm))
	require.Equal(t, string(caSecret.Data["ca.crt"]), cm.Data["ca.crt"])

	var nodeSecret corev1.Secret
	require.NoError(t, nsCtl.Get(ctx, manifests.SelfSignedNodeCertSecretName(testClusterName), &nodeSecret))
	require.Equal(t, caSecret.Data["ca.crt"], nodeSecret.Data["ca.crt"])

	var rootSecret corev1.Secret
	require.NoError(t, nsCtl.Get(ctx, manifests.SelfSignedRootClientSecretName(testClusterName), &rootSecret))
	require.Equal(t, caSecret.Data["ca.crt"], rootSecret.Data["ca.crt"])
}

func TestGenerateCrdbCerts_StandaloneCA_IdempotentAcrossRestarts(t *testing.T) {
	ctl := kubetest.NewFakeCtl(kubetest.Options{})
	gen := NewSelfSignedCertGenerator(ctl, testNamespace, "", testClusterName)
	ctx := context.Background()

	require.NoError(t, gen.GenerateCrdbCerts(ctx))

	// Capture the standalone CA cert after first run.
	nsCtl := ctl.WithNamespace(testNamespace)
	var caSecret corev1.Secret
	require.NoError(t, nsCtl.Get(ctx, testClusterName+"-cockroachdb-self-signed-ca", &caSecret))
	firstCA := caSecret.Data["ca.crt"]

	// Second call must reuse the same standalone CA.
	require.NoError(t, gen.GenerateCrdbCerts(ctx))
	require.NoError(t, nsCtl.Get(ctx, testClusterName+"-cockroachdb-self-signed-ca", &caSecret))
	require.Equal(t, firstCA, caSecret.Data["ca.crt"])
}

// TestGenerateCrdbCerts_ErrorOnUserProvidedCAConfigMap verifies that
// GenerateCrdbCerts returns an error when cockroachdb-ca already exists but
// was not created by the operator.
func TestGenerateCrdbCerts_ErrorOnUserProvidedCAConfigMap(t *testing.T) {
	existingCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      manifests.SelfSignedCAConfigMapName(testClusterName),
			Namespace: testNamespace,
			// No managed-by label — simulates a user-provided ConfigMap.
		},
		Data: map[string]string{"ca.crt": "user-ca"},
	}

	ctl := kubetest.NewFakeCtl(kubetest.Options{}, existingCM)
	gen := NewSelfSignedCertGenerator(ctl, testNamespace, "", testClusterName)

	err := gen.GenerateCrdbCerts(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "was not created by the operator")
}

// TestGenerateCrdbCerts_ErrorOnUserProvidedRootCert verifies that
// GenerateCrdbCerts returns an error when cockroachdb-root-certs already
// exists but was not created by the operator.
func TestGenerateCrdbCerts_ErrorOnUserProvidedRootCert(t *testing.T) {
	existingRoot := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      manifests.SelfSignedRootClientSecretName(testClusterName),
			Namespace: testNamespace,
		},
		Data: map[string][]byte{"tls.crt": []byte("user-root-cert")},
	}

	ctl := kubetest.NewFakeCtl(kubetest.Options{}, existingRoot)
	gen := NewSelfSignedCertGenerator(ctl, testNamespace, "", testClusterName)

	err := gen.GenerateCrdbCerts(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "was not created by the operator")
}

// TestGenerateCrdbCerts_CASharedWithOperator_NoStandaloneCACreated verifies
// that when the operator CA is reused, the standalone CA secret is never
// created.
func TestGenerateCrdbCerts_CASharedWithOperator_NoStandaloneCACreated(t *testing.T) {
	ctl := kubetest.NewFakeCtl(kubetest.Options{})
	gen := NewSelfSignedCertGenerator(ctl, testNamespace, "", testClusterName)
	ctx := context.Background()

	require.NoError(t, gen.GenerateOperatorCerts(ctx))
	require.NoError(t, gen.GenerateCrdbCerts(ctx))

	// The standalone CA secret must NOT exist when the operator CA is reused.
	var standaloneCA corev1.Secret
	err := ctl.WithNamespace(testNamespace).Get(ctx, testClusterName+"-cockroachdb-self-signed-ca", &standaloneCA)
	require.True(t, apierrors.IsNotFound(err),
		"standalone CA secret should not be created when operator CA is reused")
}

// TestGenerateCrdbCerts_NodeCertSANs verifies that the generated node
// certificate contains all expected DNS SANs and IP addresses.
func TestGenerateCrdbCerts_NodeCertSANs(t *testing.T) {
	ctl := kubetest.NewFakeCtl(kubetest.Options{})
	gen := NewSelfSignedCertGenerator(ctl, testNamespace, "", testClusterName)
	ctx := context.Background()

	require.NoError(t, gen.GenerateCrdbCerts(ctx))

	var nodeSecret corev1.Secret
	require.NoError(t, ctl.WithNamespace(testNamespace).Get(ctx, manifests.SelfSignedNodeCertSecretName(testClusterName), &nodeSecret))

	// Parse the node certificate from the secret.
	block, _ := pem.Decode(nodeSecret.Data["tls.crt"])
	require.NotNil(t, block, "tls.crt must contain a valid PEM block")
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)

	// Verify all expected DNS SANs are present.
	expectedDNSNames := []string{
		fmt.Sprintf("*.%s", testClusterName),
		fmt.Sprintf("*.%s.%s", testClusterName, testNamespace),
		fmt.Sprintf("*.%s.%s.svc.cluster.local", testClusterName, testNamespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", testClusterName, testNamespace),
		manifests.JoinFQDN(testClusterName, testNamespace, "cluster.local"),
		"localhost",
	}
	require.ElementsMatch(t, expectedDNSNames, cert.DNSNames)

	// Verify 127.0.0.1 IP SAN is present.
	var ipStrings []string
	for _, ip := range cert.IPAddresses {
		ipStrings = append(ipStrings, ip.String())
	}
	require.Contains(t, ipStrings, net.ParseIP("127.0.0.1").String())
}

// TestGenerateCrdbCerts_NodeCertSANs_WithJoinNamespaces verifies that when
// region join namespaces differ from the cluster's own namespace (e.g. the
// region code is used as the namespace), the generated node certificate
// includes join service SANs for each distinct namespace.
func TestGenerateCrdbCerts_NodeCertSANs_WithJoinNamespaces(t *testing.T) {
	ctl := kubetest.NewFakeCtl(kubetest.Options{})
	ctx := context.Background()

	const regionNamespace = "us-east-1"
	gen := NewSelfSignedCertGenerator(ctl, testNamespace, "", testClusterName).
		WithJoinNamespaces([]string{regionNamespace})

	require.NoError(t, gen.GenerateCrdbCerts(ctx))

	var nodeSecret corev1.Secret
	require.NoError(t, ctl.WithNamespace(testNamespace).Get(ctx, manifests.SelfSignedNodeCertSecretName(testClusterName), &nodeSecret))

	block, _ := pem.Decode(nodeSecret.Data["tls.crt"])
	require.NotNil(t, block, "tls.crt must contain a valid PEM block")
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)

	// The cert must cover the baseline namespace SANs plus the join SAN for
	// the region namespace, which differs from testNamespace.
	expectedDNSNames := []string{
		fmt.Sprintf("*.%s", testClusterName),
		fmt.Sprintf("*.%s.%s", testClusterName, testNamespace),
		fmt.Sprintf("*.%s.%s.svc.cluster.local", testClusterName, testNamespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", testClusterName, testNamespace),
		manifests.JoinFQDN(testClusterName, testNamespace, "cluster.local"),
		manifests.JoinFQDN(testClusterName, regionNamespace, "cluster.local"),
		"localhost",
	}
	require.ElementsMatch(t, expectedDNSNames, cert.DNSNames)
}

// TestGenerateCrdbCerts_OwnerReferenceSetOnAllResources verifies that all cert
// resources carry an OwnerReference to the CrdbCluster when WithOwner is used.
// This ensures they are garbage-collected when the cluster is deleted.
func TestGenerateCrdbCerts_OwnerReferenceSetOnAllResources(t *testing.T) {
	ctl := kubetest.NewFakeCtl(kubetest.Options{})
	ctx := context.Background()

	owner := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: testNamespace,
			UID:       types.UID("test-cluster-uid"),
		},
	}

	gen := NewSelfSignedCertGenerator(ctl, testNamespace, "", testClusterName).WithOwner(owner)
	require.NoError(t, gen.GenerateCrdbCerts(ctx))

	nsCtl := ctl.WithNamespace(testNamespace)

	var cm corev1.ConfigMap
	require.NoError(t, nsCtl.Get(ctx, manifests.SelfSignedCAConfigMapName(testClusterName), &cm))
	require.Len(t, cm.OwnerReferences, 1)
	require.Equal(t, testClusterName, cm.OwnerReferences[0].Name)

	var nodeSecret corev1.Secret
	require.NoError(t, nsCtl.Get(ctx, manifests.SelfSignedNodeCertSecretName(testClusterName), &nodeSecret))
	require.Len(t, nodeSecret.OwnerReferences, 1)
	require.Equal(t, testClusterName, nodeSecret.OwnerReferences[0].Name)

	var rootSecret corev1.Secret
	require.NoError(t, nsCtl.Get(ctx, manifests.SelfSignedRootClientSecretName(testClusterName), &rootSecret))
	require.Len(t, rootSecret.OwnerReferences, 1)
	require.Equal(t, testClusterName, rootSecret.OwnerReferences[0].Name)

	// The standalone CA (created when no operator cert secret is present)
	// must also be owned so it is cleaned up with the cluster.
	var caSecret corev1.Secret
	require.NoError(t, nsCtl.Get(ctx, testClusterName+"-cockroachdb-self-signed-ca", &caSecret))
	require.Len(t, caSecret.OwnerReferences, 1)
	require.Equal(t, testClusterName, caSecret.OwnerReferences[0].Name)
}

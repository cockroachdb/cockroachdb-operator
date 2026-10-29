package kubecrdbtest

import (
	"context"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// DefaultImage is the default crdb image used by this package, if the Image field
	// is empty.
	DefaultImage = "us-docker.pkg.dev/cockroach-cloud-images/cockroachdb/cockroach:v22.1.9"
)

func defaultNode(node *crdbv1beta1.CrdbNode) *crdbv1beta1.CrdbNode {
	clusterName := manifests.GetClusterName(node)
	newNode := node.DeepCopy()

	if newNode.Spec.Image == "" {
		newNode.Spec.Image = DefaultImage
	}

	if newNode.Spec.ServiceAccountName == "" {
		newNode.Spec.ServiceAccountName = clusterName
	}

	defaultNodeCertificates(newNode)
	defaultNodeDataStore(newNode)
	return newNode
}

// createNodeRBAC creates the service account, roles, and bindings for the cluster.
func createNodeRBAC(t *testing.T, ctl *kube.Ctl, node *crdbv1beta1.CrdbNode) {
	clusterName := node.Labels[manifests.BetaCrdbClusterNodeOwnerLabelKey]
	meta := metav1.ObjectMeta{
		Name:      clusterName,
		Namespace: node.Namespace,
	}

	ctx := context.Background()

	require.NoError(t, ctl.Apply(
		ctx,
		&corev1.ServiceAccount{ObjectMeta: meta},
		&rbacv1.Role{ObjectMeta: meta},
		&rbacv1.RoleBinding{
			ObjectMeta: meta,
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Role",
				Name:     clusterName,
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "ServiceAccount",
					Name:      meta.Name,
					Namespace: meta.Namespace,
				},
			},
		},
	))

	// Cluster role for reading locality labels from the nodes
	meta.Namespace = "" // clear this out
	require.NoError(t, ctl.WithNamespace(metav1.NamespaceSystem).Apply(
		ctx,
		&rbacv1.ClusterRole{
			ObjectMeta: meta,
			Rules: []rbacv1.PolicyRule{
				{
					APIGroups: []string{""},
					Resources: []string{"nodes"},
					Verbs:     []string{"get"},
				},
			},
		},
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: meta,
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "ClusterRole",
				Name:     clusterName,
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "ServiceAccount",
					Name:      clusterName,
					Namespace: node.Namespace,
				},
			},
		},
	))
}

func defaultNodeCertificates(node *crdbv1beta1.CrdbNode) {
	// Insecure clusters must not have certificates configured.
	if !node.Spec.TLSEnabled {
		return
	}
	// Self-signed clusters have the operator generate their certs during
	// reconciliation. Defaulting ExternalCertificates here would cause the
	// webhook to reject the cluster with "cannot set both selfSigned and
	// externalCertificates".
	if node.Spec.Certificates.SelfSigned {
		return
	}
	clusterName := manifests.GetClusterName(node)
	// ensure we've got certs to create
	// NB: ClientCA and NodeCA intentionally use the same configmap/CA to test
	// a regression where sharing secret/configmap names would result in
	// invalid volume projections being built.
	if node.Spec.Certificates.ExternalCertificates == nil {
		node.Spec.Certificates.ExternalCertificates = &crdbv1beta1.ExternalCertificates{
			ClientCAConfigMapName:   clusterName + "-ca",
			NodeCAConfigMapName:     clusterName + "-ca",
			HTTPSecretName:          clusterName + "-http-cert",
			NodeClientSecretName:    clusterName + "-node-client",
			NodeSecretName:          clusterName + "-node-cert",
			RootSQLClientSecretName: clusterName + "-root-client",
		}
	}
}

func defaultNodeDataStore(node *crdbv1beta1.CrdbNode) {
	if node.Spec.DataStore.VolumeClaimTemplate != nil ||
		node.Spec.DataStore.VolumeSource != nil ||
		node.Spec.DataStore.IsBasalt() {
		return
	}

	node.Spec.DataStore = crdbv1beta1.DataStore{
		VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("200Mi"),
					},
				},
			},
		},
	}
}

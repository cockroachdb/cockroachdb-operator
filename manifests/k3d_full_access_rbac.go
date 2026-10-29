package manifests

import (
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	apiv1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const FullAccessServiceAccountSecretName = "full-access-sa"

// FullAccessRBAC is the manifest containing cluster roles, cluster role bindings
// and a service account that assumes these roles.
// These allow us to produce kubeconfig tokens compatible with what containerssh_helper expects
// on k3d clusters.
func FullAccessRBAC(namespace string) []kube.Object {
	return []kube.Object{
		fullAccessServiceAccount(namespace),
		fullAccessClusterRole(),
		fullAccessRoleBinding(namespace),
		fullAccessSecret(namespace),
	}
}

func fullAccessServiceAccount(namespace string) *apiv1.ServiceAccount {
	return &apiv1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "full-access",
			Namespace: namespace,
		},
	}
}

func fullAccessClusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: "full-access",
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"*"},
				Resources: []string{"*"},
				Verbs:     []string{"*"},
			},
		},
	}
}

func fullAccessRoleBinding(namespace string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "full-access",
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "full-access",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "full-access",
				Namespace: namespace,
			},
		},
	}
}

func fullAccessSecret(namespace string) *apiv1.Secret {
	return &apiv1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: FullAccessServiceAccountSecretName,
			Annotations: map[string]string{
				apiv1.ServiceAccountNameKey: "full-access",
			},
			Namespace: namespace,
		},
		Type: apiv1.SecretTypeServiceAccountToken,
	}
}

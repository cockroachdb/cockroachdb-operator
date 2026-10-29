package manifests

import (
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	apiv1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RbacOptions specifies the namespace in which the role granted
// service account should be created.
type RbacOptions struct {
	// The namespace in which the service account for the RBAC
	// will be placed.
	Namespace                 string
	ServiceAccountAnnotations map[string]string
	// ServiceAccountName is the name of the service account to create and bind.
	// If empty, defaults to "cockroachdb".
	ServiceAccountName string
	// Name overrides the resource name for Role and RoleBinding. When set, Role and
	// RoleBinding are named after Name while the RoleBinding subject still references
	// ServiceAccountName. This allows rollback to delete resources by a stable name
	// even when the ServiceAccount name differs. If empty, defaults to ServiceAccountName.
	Name string
	// ClusterRoleName overrides the name for cluster-scoped ClusterRole and ClusterRoleBinding.
	// If empty, defaults to ServiceAccountName (backward compatible). Use namespace-qualified
	// names (e.g., "{namespace}-{name}") to prevent collision when the same cluster name exists
	// in different namespaces.
	ClusterRoleName string
}

// RBAC is the manifest containing roles, cluster roles, role bindings,
// cluster role bindings and a service account that assumes these roles.
// These allow processes in the CRDB cluster to do things like list secrets,
// create certificates etc...
func RBAC(opts RbacOptions) []kube.Object {
	saName := opts.ServiceAccountName
	if saName == "" {
		saName = "cockroachdb"
	}
	name := opts.Name
	if name == "" {
		name = saName
	}
	crName := opts.ClusterRoleName
	if crName == "" {
		crName = saName
	}
	return []kube.Object{
		serviceAccount(opts.Namespace, saName, opts.ServiceAccountAnnotations),
		rbacRole(name),
		rbacClusterRole(crName),
		rbacRoleBinding(opts.Namespace, name, saName),
		rbacClusterRoleBinding(opts.Namespace, saName, crName),
	}
}

func serviceAccount(namespace string, name string, annotations map[string]string) *apiv1.ServiceAccount {
	return &apiv1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
			Labels: map[string]string{
				"app": "cockroachdb",
			},
		},
	}
}

func rbacRole(name string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"app": "cockroachdb",
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"secrets"},
				Verbs:     []string{"create", "get"},
			},
		},
	}
}

func rbacClusterRole(name string) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"app": "cockroachdb",
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"certificates.k8s.io"},
				Resources: []string{"certificatesigningrequests"},
				Verbs:     []string{"create", "get", "watch"},
			},
			// get nodes allows the locality container to work as expected.
			// it pulls the failure-domain.beta.kubernetes.io/zone label to determin node locality.
			// The locality container is currently only used in test k3d clusters.
			{
				APIGroups: []string{""},
				Resources: []string{"nodes"},
				Verbs:     []string{"get"},
			},
		},
	}
}

func rbacRoleBinding(namespace, name, saName string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"app": "cockroachdb",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     name,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: namespace,
			},
		},
	}
}

func rbacClusterRoleBinding(namespace, saName, crName string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: crName,
			Labels: map[string]string{
				"app": "cockroachdb",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     crName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: namespace,
			},
		},
	}
}

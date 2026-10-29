package migrationctrl

import (
	"context"
	"fmt"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// ensureRBAC creates or updates RBAC resources for the migrated cluster.
func ensureRBAC(
	ctx context.Context, ctl *kube.Ctl, namespace, clusterName, serviceAccountName, migrationType string,
) error {
	logging.Infof(ctx, "Ensuring RBAC resources for cluster %s in namespace %s (migration type: %s)",
		clusterName, namespace, migrationType)

	// Reuse the source ServiceAccount when it already exists. Custom service-account
	// names are valid and need the bindings to point at the same identity the pods use.
	existingSA := &corev1.ServiceAccount{}
	saExists := false
	if err := ctl.Get(ctx, serviceAccountName, existingSA); err == nil {
		logging.Infof(ctx, "ServiceAccount %s already exists, reusing", serviceAccountName)
		saExists = true
	}

	// Generate RBAC objects using manifests package. Role and RoleBinding are named
	// after clusterName so rollback can delete them by a stable name even when the
	// ServiceAccount name differs from the cluster name. ClusterRole uses a
	// namespace-qualified name to prevent collision across namespaces.
	rbacObjects := manifests.RBAC(manifests.RbacOptions{
		Namespace:          namespace,
		ServiceAccountName: serviceAccountName,
		Name:               clusterName,
		ClusterRoleName:    fmt.Sprintf("%s-%s", namespace, clusterName),
	})

	for _, obj := range rbacObjects {
		// Skip ServiceAccount creation if it already exists from Helm/public operator
		if _, ok := obj.(*corev1.ServiceAccount); ok && saExists {
			logging.Infof(ctx, "Skipping ServiceAccount creation, already exists")
			continue
		}

		if err := ctl.Apply(ctx, obj); err != nil {
			return errors.Wrapf(err, "failed to apply RBAC resource %s", obj.GetName())
		}
		logging.Infof(ctx, "Applied RBAC resource: %s", obj.GetName())
	}

	logging.Infof(ctx, "RBAC resources ensured for cluster %s", clusterName)
	return nil
}

// deletePodDisruptionBudget deletes the old PDB naming conventions used by the
// public operator and Helm. It does not delete the enterprise PDB ({name}-pdb)
// because finalization creates it immediately after this call. Rollback calls
// deleteAllPodDisruptionBudgets instead to clean up everything.
func deletePodDisruptionBudget(ctx context.Context, ctl *kube.Ctl, clusterName string) error {
	return deletePDBsByName(ctx, ctl, clusterName, []string{
		clusterName,                           // Public Operator format
		fmt.Sprintf("%s-budget", clusterName), // Helm format
	})
}

// deleteAllPodDisruptionBudgets deletes all PDB naming conventions including the
// enterprise format. Used during rollback when all migration artifacts are removed.
func deleteAllPodDisruptionBudgets(ctx context.Context, ctl *kube.Ctl, clusterName string) error {
	return deletePDBsByName(ctx, ctl, clusterName, []string{
		clusterName,                           // Public Operator format
		fmt.Sprintf("%s-budget", clusterName), // Helm format
		fmt.Sprintf("%s-pdb", clusterName),    // Enterprise Operator format
	})
}

func deletePDBsByName(ctx context.Context, ctl *kube.Ctl, clusterName string, pdbNames []string) error {
	logging.Infof(ctx, "Deleting PodDisruptionBudgets for cluster %s", clusterName)

	deletedAny := false
	for _, pdbName := range pdbNames {
		pdb := &policyv1.PodDisruptionBudget{}
		err := ctl.Get(ctx, pdbName, pdb)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			logging.Warnf(ctx, "Failed to check for PDB %s: %v", pdbName, err)
			continue
		}

		if err = ctl.Delete(ctx, pdb); err != nil && !apierrors.IsNotFound(err) {
			return errors.Wrapf(err, "failed to delete PDB %s", pdbName)
		}

		logging.Infof(ctx, "Deleted PodDisruptionBudget %s", pdbName)
		deletedAny = true
	}

	if !deletedAny {
		logging.Infof(ctx, "No PodDisruptionBudgets found for cluster %s", clusterName)
	}

	return nil
}

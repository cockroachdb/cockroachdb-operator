package migrationctrl

import (
	"context"

	"github.com/cockroachdb/errors"
	crdbv1alpha1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1alpha1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// DiscoveryResult holds the discovered resources and migration metadata.
type DiscoveryResult struct {
	Name      string
	Namespace string
	// "helm" or "operator".
	MigrationType string
	// Replicas is the current STS replica count (may be lower than original during migration).
	Replicas int32
	// OriginalReplicas is the immutable original replica count from the source of truth.
	// For operator migrations this is v1alpha1 Spec.Nodes. For Helm migrations this is
	// computed as len(existingCrdbNodes) + currentSTSReplicas during discovery.
	// Use this instead of Replicas for TotalPods since the STS shrinks during migration.
	OriginalReplicas int32

	// Source resources (at least one will be non-nil).
	StatefulSet     *appsv1.StatefulSet
	V1Alpha1Cluster *crdbv1alpha1.CrdbCluster
}

func requireSkipReconcileTrue(cluster *crdbv1alpha1.CrdbCluster) error {
	if cluster == nil {
		return errors.New("v1alpha1 CrdbCluster is required")
	}
	if cluster.Labels[LabelSkipReconcile] != "true" {
		return errors.Errorf("missing required label: %s=true", LabelSkipReconcile)
	}
	return nil
}

func preferredMigrationLabelSource(result *DiscoveryResult) kube.Object {
	if result == nil {
		return nil
	}
	if result.V1Alpha1Cluster != nil && result.MigrationType == MigrationTypeOperator {
		return result.V1Alpha1Cluster
	}
	if result.StatefulSet != nil {
		return result.StatefulSet
	}
	if result.V1Alpha1Cluster != nil {
		return result.V1Alpha1Cluster
	}
	return nil
}

// refreshPreferredMigrationLabelSource re-fetches the preferred label source object from
// the API server and updates the corresponding pointer in result so subsequent callers in
// the same reconcile see fresh data. This prevents stale-object conflicts when the hub has
// been updated between discovery and label write.
func refreshPreferredMigrationLabelSource(
	ctx context.Context, ctl *kube.Ctl, result *DiscoveryResult,
) (kube.Object, error) {
	obj := preferredMigrationLabelSource(result)
	if obj == nil {
		return nil, errors.New("no source resource to update")
	}

	switch obj.(type) {
	case *crdbv1alpha1.CrdbCluster:
		latest := &crdbv1alpha1.CrdbCluster{}
		if err := ctl.Get(ctx, result.Name, latest); err != nil {
			return nil, err
		}
		result.V1Alpha1Cluster = latest
		return latest, nil
	case *appsv1.StatefulSet:
		latest := &appsv1.StatefulSet{}
		if err := ctl.Get(ctx, result.Name, latest); err != nil {
			return nil, err
		}
		result.StatefulSet = latest
		return latest, nil
	default:
		return obj, nil
	}
}

// discoverMigrationResources discovers and validates resources for migration.
// Fetches StatefulSet and/or v1alpha1 CrdbCluster to determine migration type.
func (r *Reconciler) discoverMigrationResources(
	ctx context.Context, ctl *kube.Ctl, name string,
) (*DiscoveryResult, error) {
	// Try StatefulSet first
	var sts appsv1.StatefulSet
	stsErr := ctl.Get(ctx, name, &sts)
	stsExists := stsErr == nil

	// Try v1alpha1 CrdbCluster
	var v1alpha1Cluster crdbv1alpha1.CrdbCluster
	v1alpha1Err := ctl.Get(ctx, name, &v1alpha1Cluster)
	v1alpha1Exists := v1alpha1Err == nil

	// At least one must exist
	if !stsExists && !v1alpha1Exists {
		if apierrors.IsNotFound(stsErr) && apierrors.IsNotFound(v1alpha1Err) {
			return nil, apierrors.NewNotFound(appsv1.Resource("statefulset"), name)
		}
		// Both failed with non-NotFound errors. Return whichever is available;
		// stsErr is always non-nil here because !stsExists.
		return nil, stsErr
	}

	result := &DiscoveryResult{
		Name:      name,
		Namespace: ctl.Namespace(),
	}

	// Determine migration type based on StatefulSet ownerReferences and v1alpha1 existence
	if stsExists {
		if !isCRDBStatefulSet(&sts) {
			return nil, errors.New("not a CockroachDB StatefulSet")
		}
		result.StatefulSet = &sts
		if sts.Spec.Replicas == nil {
			return nil, errors.Errorf("StatefulSet %s has nil Replicas, cannot determine cluster size for migration", name)
		}
		result.Replicas = *sts.Spec.Replicas

		// Check ownerReferences first. This is the most reliable signal for an operator-managed STS.
		if isOwnedByV1Alpha1Cluster(&sts) {
			result.MigrationType = MigrationTypeOperator
			if v1alpha1Exists {
				result.V1Alpha1Cluster = &v1alpha1Cluster
				if err := requireSkipReconcileTrue(result.V1Alpha1Cluster); err != nil {
					return nil, err
				}
			}
		} else if v1alpha1Exists && v1alpha1Cluster.Labels[LabelMigrate] != "" {
			// If the v1alpha1 object has a migrate label, the user explicitly applied
			// it there. This covers operator deployments where the STS was not created
			// with an ownerRef (older public operator versions or manual STS).
			result.MigrationType = MigrationTypeOperator
			result.V1Alpha1Cluster = &v1alpha1Cluster
			if err := requireSkipReconcileTrue(result.V1Alpha1Cluster); err != nil {
				return nil, err
			}
		} else {
			// The STS has no ownerRef pointing to a v1alpha1 CrdbCluster. If a
			// v1alpha1 object exists without a migrate label, it's a conversion
			// artifact from the v1beta1 CrdbCluster the migration controller created
			// earlier. The conversion webhook serves any v1beta1 object back as
			// v1alpha1 on GET, so we can't rely on v1alpha1 existence alone.
			result.MigrationType = MigrationTypeHelm
		}
	} else if v1alpha1Exists && v1alpha1Cluster.Labels[LabelMigrate] != "" {
		// No STS but a v1alpha1 CrdbCluster with a migrate label exists. The user
		// applied the label directly, so this is an operator migration.
		result.MigrationType = MigrationTypeOperator
		result.V1Alpha1Cluster = &v1alpha1Cluster

		if err := requireSkipReconcileTrue(result.V1Alpha1Cluster); err != nil {
			return nil, err
		}

		result.Replicas = v1alpha1Cluster.Spec.Nodes
	} else if v1alpha1Exists {
		// No STS and the v1alpha1 object has no migrate label. This is a conversion
		// artifact from a v1beta1 CrdbCluster. For Helm migrations the STS is gone
		// at this point because finalization deleted it. Return NotFound so the
		// reconciler hits the STS-deleted completion handler.
		return nil, apierrors.NewNotFound(appsv1.Resource("statefulset"), name)
	}

	// Compute OriginalReplicas. This is the immutable total pod count from the source of truth.
	// For operator migrations, v1alpha1 Spec.Nodes never changes during migration (Mode=Disabled).
	// For Helm migrations, we compute it as existingCrdbNodes + currentSTSReplicas since
	// the STS shrinks as pods are migrated.
	if result.V1Alpha1Cluster != nil && result.V1Alpha1Cluster.Spec.Nodes > 0 {
		result.OriginalReplicas = result.V1Alpha1Cluster.Spec.Nodes
	} else if result.StatefulSet != nil {
		// Helm migration: count existing CrdbNodes that belong to this cluster.
		existingNodes, err := listClusterCrdbNodes(ctx, ctl, name)
		if err != nil {
			logging.Warnf(ctx, "Failed to list CrdbNodes for OriginalReplicas: %v", err)
			result.OriginalReplicas = result.Replicas
		} else {
			result.OriginalReplicas = int32(len(existingNodes)) + result.Replicas
		}
	} else {
		result.OriginalReplicas = result.Replicas
	}

	logging.Infof(ctx, "Discovered %s migration for %s (originalReplicas=%d, currentReplicas=%d)",
		result.MigrationType, name, result.OriginalReplicas, result.Replicas)
	return result, nil
}

// isOwnedByV1Alpha1Cluster checks if a StatefulSet is owned by a v1alpha1 CrdbCluster.
// This is a reliable way to detect public operator deployments.
func isOwnedByV1Alpha1Cluster(sts *appsv1.StatefulSet) bool {
	for _, ref := range sts.OwnerReferences {
		if ref.APIVersion == "crdb.cockroachlabs.com/v1alpha1" && ref.Kind == "CrdbCluster" {
			return true
		}
	}
	return false
}

// isCRDBStatefulSet validates that a StatefulSet is a CockroachDB StatefulSet.
func isCRDBStatefulSet(sts *appsv1.StatefulSet) bool {
	if sts == nil {
		return false
	}

	labels := sts.Labels
	if labels == nil {
		return false
	}

	// Check standard labels used by Helm and Public Operator
	if labels["app.kubernetes.io/name"] == "cockroachdb" {
		return true
	}
	if labels["app"] == "cockroachdb" {
		return true
	}
	if labels["app.kubernetes.io/component"] == "database" {
		return true
	}

	// Also check for containers named "db"
	for _, container := range sts.Spec.Template.Spec.Containers {
		if container.Name == "db" {
			return true
		}
	}

	return false
}

// getMigrationLabel retrieves the migration label value from the source resource.
func getMigrationLabel(result *DiscoveryResult) (string, bool) {
	if result == nil {
		return "", false
	}

	// Prefer the v1alpha1 CrdbCluster when it has the migrate label (public operator migration).
	if result.V1Alpha1Cluster != nil {
		if val, ok := result.V1Alpha1Cluster.Labels[LabelMigrate]; ok {
			return val, true
		}
	}

	// Otherwise fall back to the StatefulSet (Helm migration).
	if result.StatefulSet != nil {
		if val, ok := result.StatefulSet.Labels[LabelMigrate]; ok {
			return val, true
		}
	}

	return "", false
}

// updateLabelValue updates the migration label value on the source resource.
// Skips the API call if the label already has the target value.
func updateLabelValue(
	ctx context.Context, ctl *kube.Ctl, result *DiscoveryResult, value string,
) error {
	const maxRetries = 3
	for attempt := range maxRetries {
		obj, err := refreshPreferredMigrationLabelSource(ctx, ctl, result)
		if err != nil {
			return err
		}
		if obj.GetLabels() == nil {
			obj.SetLabels(make(map[string]string))
		}
		if obj.GetLabels()[LabelMigrate] == value {
			return nil
		}
		obj.GetLabels()[LabelMigrate] = value
		err = ctl.Update(ctx, obj)
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
		logging.Warnf(ctx, "Migration label update conflict (attempt %d/%d), retrying", attempt+1, maxRetries)
	}
	return errors.New("failed to update migration label after retries")
}

// removeMigrationLabel removes the migration label from the source resource.
func removeMigrationLabel(ctx context.Context, ctl *kube.Ctl, result *DiscoveryResult) error {
	const maxRetries = 3
	for attempt := range maxRetries {
		obj, err := refreshPreferredMigrationLabelSource(ctx, ctl, result)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if obj.GetLabels() == nil {
			return nil
		}
		if _, ok := obj.GetLabels()[LabelMigrate]; !ok {
			return nil
		}
		delete(obj.GetLabels(), LabelMigrate)
		err = ctl.Update(ctx, obj)
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
		logging.Warnf(ctx, "Migration label removal conflict (attempt %d/%d), retrying", attempt+1, maxRetries)
	}
	return errors.New("failed to remove migration label after retries")
}

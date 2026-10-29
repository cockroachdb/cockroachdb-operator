package nodectrl

import (
	"context"
	"maps"
	"time"

	"github.com/cockroachdb/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8s_resource "k8s.io/apimachinery/pkg/api/resource"
	ctrl "sigs.k8s.io/controller-runtime"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
)

// NodeDiskType identifies the kind of PVC managed for a node. It is used in
// log and error messages; its String form is the human-readable label.
type NodeDiskType int

const (
	DiskTypeData NodeDiskType = iota
	DiskTypeWAL
	DiskTypeLogs
	DiskTypeBasaltScratch
)

func (d NodeDiskType) String() string {
	switch d {
	case DiskTypeData:
		return "data"
	case DiskTypeWAL:
		return "WAL"
	case DiskTypeLogs:
		return "logs"
	case DiskTypeBasaltScratch:
		return "basalt-scratch"
	default:
		return "unknown"
	}
}

// preserveImmutablePVCFields checks for storage class conflicts between desired and existing PVCs,
// logs warnings, and preserves the immutable StorageClassName field.
// The diskType parameter is used in warning messages for clarity.
// Returns the existing PVC (or nil if not found) and an error if Get() fails.
func preserveImmutablePVCFields(
	ctx context.Context,
	r *Reconciler,
	pvcNamespace string,
	pvcName string,
	desired *corev1.PersistentVolumeClaim,
	diskType NodeDiskType,
) (*corev1.PersistentVolumeClaim, error) {
	existingPVC := &corev1.PersistentVolumeClaim{}
	if err := r.Ctl.WithNamespace(pvcNamespace).Get(ctx, pvcName, existingPVC); err != nil {
		if !kubeerrors.IsNotFound(err) {
			return nil, errors.Wrapf(err, "get existing %s pvc %v in %v", diskType, pvcName, pvcNamespace)
		}
		return nil, nil
	}

	// Log storage class conflicts but don't fail - preserve existing storage class
	// because it's immutable after PVC creation
	if existingPVC.Spec.StorageClassName != nil && desired.Spec.StorageClassName != nil &&
		(*existingPVC.Spec.StorageClassName != *desired.Spec.StorageClassName) {
		logging.Warnf(ctx, "ignore conflicting %s storage class name existing %v, but got %v",
			diskType, *existingPVC.Spec.StorageClassName, *desired.Spec.StorageClassName)
	} else if existingPVC.Spec.StorageClassName == nil && desired.Spec.StorageClassName != nil {
		logging.Warnf(ctx, "ignore conflicting %s storage class name existing <nil>, but got %v",
			diskType, *desired.Spec.StorageClassName)
	} else if existingPVC.Spec.StorageClassName != nil && desired.Spec.StorageClassName == nil {
		logging.Warnf(ctx, "ignore conflicting %s storage class name existing %v, but got <nil>",
			diskType, *existingPVC.Spec.StorageClassName)
	}

	// Always preserve the existing storage class name
	desired.Spec.StorageClassName = existingPVC.Spec.StorageClassName

	return existingPVC, nil
}

// monitorPVCResize checks if a PVC resize operation is in progress and returns
// a requeue result if the resize hasn't completed yet.
func monitorPVCResize(
	ctx context.Context,
	desired *corev1.PersistentVolumeClaim,
	existing *corev1.PersistentVolumeClaim,
) (ctrl.Result, error) {
	if existing == nil || existing.Status.Phase != corev1.ClaimBound {
		return controllers.NoRequeue()
	}

	// This means we have a resizing request, requeue to wait for completion
	if *desired.Spec.Resources.Requests.Storage() != *existing.Spec.Resources.Requests.Storage() {
		logging.Warnf(ctx, "pvc %s/%s resize pending: desired %s, existing %s",
			existing.Namespace, existing.Name,
			desired.Spec.Resources.Requests.Storage().String(),
			existing.Spec.Resources.Requests.Storage().String(),
		)
		return controllers.RequeueAfter(5*time.Second, nil)
	}

	if pvcConditionTrue(existing, corev1.PersistentVolumeClaimResizing) {
		logging.Warnf(ctx, "pvc %s/%s resize in progress due to condition %s: desired %s, current %s",
			existing.Namespace, existing.Name, corev1.PersistentVolumeClaimResizing,
			desired.Spec.Resources.Requests.Storage().String(),
			existing.Status.Capacity.Storage().String(),
		)
		return controllers.RequeueAfter(5*time.Second, nil)
	}

	return controllers.NoRequeue()
}

// pvcConditionTrue returns true if the PVC has the given condition with status True.
func pvcConditionTrue(
	existing *corev1.PersistentVolumeClaim,
	conditionType corev1.PersistentVolumeClaimConditionType,
) bool {
	for _, condition := range existing.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		if condition.Type == conditionType {
			return true
		}
	}
	return false
}

// applyManagedPVC stamps the standard metadata onto a PVC template, preserves
// immutable fields against any existing PVC, sets the CrdbNode owner reference,
// applies it, and returns the resize-monitoring result. pvc must be a copy the
// caller owns (e.g. from node.DeepCopy()) so its fields can be mutated freely.
// diskType labels warnings and errors.
func (r *Reconciler) applyManagedPVC(
	ctx context.Context, node *crdbv1beta1.CrdbNode, pvc *corev1.PersistentVolumeClaim, name string, diskType NodeDiskType,
) (ctrl.Result, error) {
	pvc.Name = name
	pvc.Namespace = node.Namespace
	// Clone rather than alias the node's maps: this PVC is one of several built
	// from the same node, and stamping metadata during apply must not mutate the
	// node's live Labels/Annotations (which would then leak into the pod).
	pvc.Labels = maps.Clone(node.Labels)
	pvc.Annotations = maps.Clone(node.Annotations)
	existing, err := preserveImmutablePVCFields(ctx, r, pvc.Namespace, pvc.Name, pvc, diskType)
	if err != nil {
		return controllers.RequeueIfError(err)
	}

	// Set ownerReference for topology visibility; it is cleared while retaining
	// PVCs to prevent accidental GC when the CrdbNode is removed. Set on every
	// apply so the APIVersion stays current across operator upgrades.
	if err := ctrl.SetControllerReference(node, pvc, r.Ctl.Scheme()); err != nil {
		return controllers.RequeueIfError(errors.Wrapf(err, "set ownerReference on %s pvc", diskType))
	}

	if err := r.Ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, pvc); err != nil {
		return controllers.RequeueIfError(errors.Wrapf(err, "apply %s pvc %s in %s", diskType, pvc.Name, pvc.Namespace))
	}

	return monitorPVCResize(ctx, pvc, existing)
}

// upsertPVC creates/updates the node's PVCs (data or basalt scratch, plus logs
// and WAL when configured). Basalt and a data VolumeClaimTemplate are mutually
// exclusive (enforced by the webhook).
func (r *Reconciler) upsertPVC(
	ctx context.Context, node *crdbv1beta1.CrdbNode,
) (ctrl.Result, error) {
	// Basalt nodes keep cluster data on the network store, so they have no data
	// PVC. They may own a dedicated scratch PVC (scratch-<node>) and/or a logs
	// PVC, but no WAL PVC (no local data store).
	if node.Spec.DataStore.Basalt != nil {
		if result, err := r.upsertScratchPVC(ctx, node); err != nil || !result.IsZero() {
			return result, err
		}
		return r.upsertLogsPVC(ctx, node)
	}

	// A node with only a raw VolumeSource (no data template) has no PVC to manage.
	if node.Spec.DataStore.VolumeClaimTemplate == nil {
		return controllers.NoRequeue()
	}

	nodeCopy := node.DeepCopy()
	pvc := nodeCopy.Spec.DataStore.VolumeClaimTemplate
	name := manifests.CrdbNodeStoragePVCName(node.Name, pvc)
	if result, err := r.applyManagedPVC(ctx, node, pvc, name, DiskTypeData); err != nil || !result.IsZero() {
		return result, err
	}

	if result, err := r.upsertLogsPVC(ctx, node); err != nil || !result.IsZero() {
		return result, err
	}

	return r.upsertWALPVC(ctx, node)
}

// upsertScratchPVC creates the basalt node's dedicated scratch PVC when a
// ScratchVolumeClaimTemplate is configured. It is a no-op otherwise.
func (r *Reconciler) upsertScratchPVC(
	ctx context.Context, node *crdbv1beta1.CrdbNode,
) (ctrl.Result, error) {
	b := node.Spec.DataStore.Basalt
	if b == nil || b.ScratchVolumeClaimTemplate == nil {
		return controllers.NoRequeue()
	}
	nodeCopy := node.DeepCopy()
	pvc := nodeCopy.Spec.DataStore.Basalt.ScratchVolumeClaimTemplate
	name := manifests.CrdbNodeScratchPVCName(node.Name, pvc)
	return r.applyManagedPVC(ctx, node, pvc, name, DiskTypeBasaltScratch)
}

// upsertLogsPVC creates the node's logs PVC if LogsStore is configured
// and the PVC does not already exist in the cluster.
func (r *Reconciler) upsertLogsPVC(
	ctx context.Context, node *crdbv1beta1.CrdbNode,
) (ctrl.Result, error) {
	if node.Spec.LogsStore == nil || node.Spec.LogsStore.VolumeClaimTemplate == nil {
		return controllers.NoRequeue()
	}
	nodeCopy := node.DeepCopy()
	pvc := nodeCopy.Spec.LogsStore.VolumeClaimTemplate
	name := manifests.CrdbNodeLogsStoragePVCName(node.Name, pvc)
	return r.applyManagedPVC(ctx, node, pvc, name, DiskTypeLogs)
}

func (r *Reconciler) deletePVC(ctx context.Context, node *crdbv1beta1.CrdbNode) error {
	if r.shouldRetainPVCs(node) {
		logging.Infof(ctx, "PVC retention policy is 'Retain', skipping PVC deletion for node %s", node.Name)
		return nil
	}
	pvcNames := r.getPVCNamesForNode(node)
	if len(pvcNames) == 0 {
		return nil
	}

	var pvc corev1.PersistentVolumeClaim
	for _, pvcName := range pvcNames {
		pvc.Name = pvcName
		pvc.Namespace = node.Namespace
		if err := r.Ctl.DeleteWithOptions(ctx, kube.DeleteOptions{NoWait: true}, &pvc); err != nil {
			if kubeerrors.IsNotFound(err) {
				// already deleted, try the remaining PVCs
				continue
			}
			return errors.Wrapf(err, "delete pvc %s in %s", pvc.Name, pvc.Namespace)
		}
	}

	return nil
}

func (r *Reconciler) shouldRetainPVCs(node *crdbv1beta1.CrdbNode) bool {
	if node.Spec.PersistentVolumeClaimRetentionPolicy == nil {
		return false // default is delete
	}
	return node.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted == appsv1.RetainPersistentVolumeClaimRetentionPolicyType
}

func (r *Reconciler) upsertWALPVC(
	ctx context.Context, node *crdbv1beta1.CrdbNode,
) (ctrl.Result, error) {

	if node.Spec.WALFailoverSpec == nil {
		return controllers.NoRequeue() // Nothing to do
	}

	nodeCopy := node.DeepCopy()
	pvc := nodeCopy.Spec.DataStore.VolumeClaimTemplate
	name := manifests.CrdbNodeWalStoragePVCName(node.Spec.WALFailoverSpec.Name, node.Name, pvc)
	pvc.Spec.Resources.Requests[corev1.ResourceStorage] = k8s_resource.MustParse(node.Spec.WALFailoverSpec.Size)
	if node.Spec.WALFailoverSpec.StorageClassName != "" {
		pvc.Spec.StorageClassName = &node.Spec.WALFailoverSpec.StorageClassName
	}
	return r.applyManagedPVC(ctx, node, pvc, name, DiskTypeWAL)
}

// monitorPVCFilesystemResize checks for the FileSystemResizePending condition
// on all of the node's managed PVCs (data, WAL, logs, and basalt scratch).
// Unlike monitorPVCResize (which runs before pod creation), this runs after the
// pod exists because kubelet needs to mount the volume to complete the
// filesystem expansion.
func (r *Reconciler) monitorPVCFilesystemResize(
	ctx context.Context, node *crdbv1beta1.CrdbNode,
) (ctrl.Result, error) {
	// getPVCNamesForNode also covers the basalt scratch PVC, which has no data
	// VolumeClaimTemplate but can still be resized.
	pvcNames := r.getPVCNamesForNode(node)
	if len(pvcNames) == 0 {
		return controllers.NoRequeue()
	}

	ctl := r.Ctl.WithNamespace(node.Namespace)
	for _, pvcName := range pvcNames {
		var pvc corev1.PersistentVolumeClaim
		if err := ctl.Get(ctx, pvcName, &pvc); err != nil {
			if kubeerrors.IsNotFound(err) {
				continue
			}
			return controllers.RequeueIfError(errors.Wrapf(err, "get pvc %s in %s", pvcName, node.Namespace))
		}
		if pvcConditionTrue(&pvc, corev1.PersistentVolumeClaimFileSystemResizePending) {
			logging.Warnf(ctx, "pvc %s/%s filesystem resize pending", pvc.Namespace, pvc.Name)
			return controllers.RequeueAfter(5*time.Second, nil)
		}
	}

	return controllers.NoRequeue()
}

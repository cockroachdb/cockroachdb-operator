package nodectrl

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/datadriven"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPreserveImmutablePVCFields(t *testing.T) {
	ctx := context.Background()

	// Scenario: New data or WAL PVC — first reconcile, no existing PVC in cluster.
	t.Run("existing PVC not found returns nil and leaves desired unchanged", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{})
		r := &Reconciler{Ctl: ctl}

		desired := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pvc",
				Namespace: "default",
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: new("new-class"),
			},
		}

		existing, err := preserveImmutablePVCFields(ctx, r, "default", "test-pvc", desired, DiskTypeData)
		require.NoError(t, err)
		require.Nil(t, existing)
		require.Equal(t, "new-class", *desired.Spec.StorageClassName)
	})

	// Scenario: User changed VolumeClaimTemplate or WAL storage class after PVC was created; immutable field cannot change.
	t.Run("storage class conflict: both non-nil but different", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{})
		r := &Reconciler{Ctl: ctl}

		existingPVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pvc",
				Namespace: "default",
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: new("old-class"),
			},
		}
		require.NoError(t, ctl.Apply(ctx, existingPVC))

		desired := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pvc",
				Namespace: "default",
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: new("new-class"),
			},
		}

		existing, err := preserveImmutablePVCFields(ctx, r, "default", "test-pvc", desired, DiskTypeWAL)
		require.NoError(t, err)
		require.NotNil(t, existing)
		// Desired storage class should be overwritten with existing
		require.Equal(t, "old-class", *desired.Spec.StorageClassName)
		require.Equal(t, "old-class", *existing.Spec.StorageClassName)
	})

	// Scenario: PVC was created with default storage class (nil); user later set a class in VolumeClaimTemplate or WAL spec.
	t.Run("storage class conflict: existing nil, desired non-nil", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{})
		r := &Reconciler{Ctl: ctl}

		existingPVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pvc",
				Namespace: "default",
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: nil,
			},
		}
		require.NoError(t, ctl.Apply(ctx, existingPVC))

		desired := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pvc",
				Namespace: "default",
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: new("new-class"),
			},
		}

		existing, err := preserveImmutablePVCFields(ctx, r, "default", "test-pvc", desired, DiskTypeData)
		require.NoError(t, err)
		require.NotNil(t, existing)
		require.Nil(t, desired.Spec.StorageClassName)
		require.Nil(t, existing.Spec.StorageClassName)
	})

	// Scenario: Existing PVC has a storage class; desired has nil (e.g. user cleared WALFailoverSpec.StorageClassName or template).
	t.Run("storage class conflict: existing non-nil, desired nil", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{})
		r := &Reconciler{Ctl: ctl}

		existingPVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pvc",
				Namespace: "default",
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: new("existing-class"),
			},
		}
		require.NoError(t, ctl.Apply(ctx, existingPVC))

		desired := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pvc",
				Namespace: "default",
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: nil,
			},
		}

		existing, err := preserveImmutablePVCFields(ctx, r, "default", "test-pvc", desired, DiskTypeWAL)
		require.NoError(t, err)
		require.NotNil(t, existing)
		require.Equal(t, "existing-class", *desired.Spec.StorageClassName)
		require.Equal(t, "existing-class", *existing.Spec.StorageClassName)
	})

	// Scenario: Normal reconcile — no storage class change; desired and existing already match.
	t.Run("no conflict: storage classes match", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{})
		r := &Reconciler{Ctl: ctl}

		existingPVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pvc",
				Namespace: "default",
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: new("same-class"),
			},
		}
		require.NoError(t, ctl.Apply(ctx, existingPVC))

		desired := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pvc",
				Namespace: "default",
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: new("same-class"),
			},
		}

		existing, err := preserveImmutablePVCFields(ctx, r, "default", "test-pvc", desired, DiskTypeData)
		require.NoError(t, err)
		require.NotNil(t, existing)
		// Storage class should remain the same
		require.Equal(t, "same-class", *desired.Spec.StorageClassName)
		require.Equal(t, "same-class", *existing.Spec.StorageClassName)
	})
}

func TestMonitorPVCFilesystemResize(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name          string
		basalt        bool
		withWAL       bool
		pendingPVC    string
		expectedAfter time.Duration
	}{
		{
			name:          "data pvc filesystem resize pending",
			pendingPVC:    "data",
			expectedAfter: 5 * time.Second,
		},
		{
			name:          "wal pvc filesystem resize pending",
			withWAL:       true,
			pendingPVC:    "wal",
			expectedAfter: 5 * time.Second,
		},
		{
			name:          "basalt scratch pvc filesystem resize pending",
			basalt:        true,
			pendingPVC:    "scratch",
			expectedAfter: 5 * time.Second,
		},
		{
			name:    "no filesystem resize pending",
			withWAL: true,
		},
		{
			name:       "pvc not found returns no requeue",
			pendingPVC: "missing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("default")
			reconciler := &Reconciler{Ctl: ctl}

			node := &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "resize-pending",
					Namespace: "default",
				},
			}
			if tc.basalt {
				node.Spec.DataStore.Basalt = &crdbv1beta1.BasaltStoreSpec{
					ControllerAddrs:            []string{"10.0.0.1:26256"},
					TenantID:                   "t",
					ScratchVolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
				}
			} else {
				node.Spec.DataStore.VolumeClaimTemplate = &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "datadir"},
				}
			}
			if tc.withWAL {
				node.Spec.WALFailoverSpec = &crdbv1beta1.CrdbWalFailoverSpec{
					Size: "10Gi",
				}
			}

			if tc.pendingPVC != "missing" {
				var name string
				if tc.basalt {
					name = manifests.CrdbNodeScratchPVCName(node.Name, node.Spec.DataStore.Basalt.ScratchVolumeClaimTemplate)
				} else {
					name = manifests.CrdbNodeStoragePVCName(node.Name, node.Spec.DataStore.VolumeClaimTemplate)
				}
				pending := tc.pendingPVC == "data" || tc.pendingPVC == "scratch"
				objects := []kube.Object{filesystemResizePVC(name, node.Namespace, pending)}
				if tc.withWAL {
					walPVC := filesystemResizePVC(
						manifests.CrdbNodeWalStoragePVCName(
							node.Spec.WALFailoverSpec.Name,
							node.Name,
							node.Spec.DataStore.VolumeClaimTemplate,
						),
						node.Namespace,
						tc.pendingPVC == "wal",
					)
					objects = append(objects, walPVC)
				}
				require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, objects...))
			}

			result, err := reconciler.monitorPVCFilesystemResize(ctx, node)
			require.NoError(t, err)
			require.False(t, result.Requeue)
			require.Equal(t, tc.expectedAfter, result.RequeueAfter)
		})
	}
}

func getDesiredPVCWithStorage(size string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
		},
	}
}

func TestMonitorPVCResize(t *testing.T) {
	ctx := context.Background()

	testCases := []struct {
		name          string
		desiredSize   string
		existing      *corev1.PersistentVolumeClaim
		expectedAfter time.Duration
	}{
		{
			name:        "existing PVC is nil returns NoRequeue",
			desiredSize: "10Gi",
		},
		{
			name:        "existing PVC not bound returns NoRequeue",
			desiredSize: "10Gi",
			existing:    resizeTestPVC(corev1.ClaimPending, "10Gi", "0Gi"),
		},
		{
			name:          "spec storage mismatch returns RequeueAfter 5s",
			desiredSize:   "20Gi",
			existing:      resizeTestPVC(corev1.ClaimBound, "10Gi", "10Gi"),
			expectedAfter: 5 * time.Second,
		},
		{
			name:        "status capacity mismatch with Resizing condition returns RequeueAfter 5s",
			desiredSize: "20Gi",
			existing: resizeTestPVC(
				corev1.ClaimBound,
				"20Gi",
				"10Gi",
				corev1.PersistentVolumeClaimCondition{
					Type:   corev1.PersistentVolumeClaimResizing,
					Status: corev1.ConditionTrue,
				},
			),
			expectedAfter: 5 * time.Second,
		},
		{
			name:        "filesystem resize pending condition returns NoRequeue",
			desiredSize: "20Gi",
			existing: resizeTestPVC(
				corev1.ClaimBound,
				"20Gi",
				"20Gi",
				corev1.PersistentVolumeClaimCondition{
					Type:   corev1.PersistentVolumeClaimFileSystemResizePending,
					Status: corev1.ConditionTrue,
				},
			),
		},
		{
			name:        "filesystem resize pending with lagging capacity returns NoRequeue",
			desiredSize: "20Gi",
			existing: resizeTestPVC(
				corev1.ClaimBound,
				"20Gi",
				"10Gi",
				corev1.PersistentVolumeClaimCondition{
					Type:   corev1.PersistentVolumeClaimFileSystemResizePending,
					Status: corev1.ConditionTrue,
				},
			),
		},
		{
			name:        "resizing condition false returns NoRequeue",
			desiredSize: "20Gi",
			existing: resizeTestPVC(
				corev1.ClaimBound,
				"20Gi",
				"10Gi",
				corev1.PersistentVolumeClaimCondition{
					Type:   corev1.PersistentVolumeClaimResizing,
					Status: corev1.ConditionFalse,
				},
			),
		},
		{
			name:        "status capacity mismatch without Resizing condition returns NoRequeue",
			desiredSize: "20Gi",
			existing:    resizeTestPVC(corev1.ClaimBound, "20Gi", "10Gi"),
		},
		{
			name:        "everything matches returns NoRequeue",
			desiredSize: "10Gi",
			existing:    resizeTestPVC(corev1.ClaimBound, "10Gi", "10Gi"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := monitorPVCResize(ctx, getDesiredPVCWithStorage(tc.desiredSize), tc.existing)
			require.NoError(t, err)
			require.False(t, result.Requeue)
			require.Equal(t, tc.expectedAfter, result.RequeueAfter)
		})
	}
}

func TestMonitorPVCResizeDatadriven(t *testing.T) {
	ctx := context.Background()

	datadriven.RunTest(t, "testdata/pvc_resize.datadriven", func(t *testing.T, d *datadriven.TestData) string {
		switch d.Cmd {
		case "monitor-pvc-resize":
			return runMonitorPVCResizeTest(t, ctx, d)
		default:
			t.Fatalf("unknown command: %q", d.Cmd)
			return ""
		}
	})
}

func runMonitorPVCResizeTest(t *testing.T, ctx context.Context, d *datadriven.TestData) string {
	var desiredSize string
	d.ScanArgs(t, "desired", &desiredSize)
	desired := getDesiredPVCWithStorage(desiredSize)

	existing := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "datadir-node-0",
			Namespace: "default",
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{},
			},
		},
	}

	if d.HasArg("existing") {
		var exists bool
		d.ScanArgs(t, "existing", &exists)
		if !exists {
			existing = nil
		}
	}

	if existing != nil {
		var phase string
		if d.HasArg("phase") {
			d.ScanArgs(t, "phase", &phase)
			existing.Status.Phase = pvcPhase(t, phase)
		}

		var specSize string
		d.ScanArgs(t, "spec", &specSize)
		existing.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse(specSize)

		var capacitySize string
		d.ScanArgs(t, "capacity", &capacitySize)
		existing.Status.Capacity = corev1.ResourceList{
			corev1.ResourceStorage: resource.MustParse(capacitySize),
		}

		if d.HasArg("conditions") {
			var conditions string
			d.ScanArgs(t, "conditions", &conditions)
			existing.Status.Conditions = pvcConditions(t, conditions)
		}
	}

	result, err := monitorPVCResize(ctx, desired, existing)
	if err != nil {
		return fmt.Sprintf("error=%v", err)
	}
	return fmt.Sprintf("requeue=%t\nrequeue_after=%s", result.Requeue, result.RequeueAfter)
}

func resizeTestPVC(
	phase corev1.PersistentVolumeClaimPhase,
	specSize string,
	capacitySize string,
	conditions ...corev1.PersistentVolumeClaimCondition,
) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: phase,
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse(capacitySize),
			},
			Conditions: conditions,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(specSize),
				},
			},
		},
	}
}

func pvcPhase(t *testing.T, phase string) corev1.PersistentVolumeClaimPhase {
	t.Helper()

	switch phase {
	case "Bound":
		return corev1.ClaimBound
	case "Pending":
		return corev1.ClaimPending
	default:
		t.Fatalf("unknown pvc phase: %q", phase)
		return ""
	}
}

func pvcConditions(t *testing.T, conditions string) []corev1.PersistentVolumeClaimCondition {
	t.Helper()

	if conditions == "" || conditions == "none" {
		return nil
	}

	var result []corev1.PersistentVolumeClaimCondition
	for condition := range strings.SplitSeq(conditions, ",") {
		parts := strings.Split(condition, ":")
		require.Len(t, parts, 2)
		result = append(result, corev1.PersistentVolumeClaimCondition{
			Type:   pvcConditionType(t, parts[0]),
			Status: pvcConditionStatus(t, parts[1]),
		})
	}
	return result
}

func pvcConditionType(t *testing.T, conditionType string) corev1.PersistentVolumeClaimConditionType {
	t.Helper()

	switch conditionType {
	case "Resizing":
		return corev1.PersistentVolumeClaimResizing
	case "FileSystemResizePending":
		return corev1.PersistentVolumeClaimFileSystemResizePending
	default:
		t.Fatalf("unknown pvc condition type: %q", conditionType)
		return ""
	}
}

func pvcConditionStatus(t *testing.T, status string) corev1.ConditionStatus {
	t.Helper()

	switch status {
	case "true":
		return corev1.ConditionTrue
	case "false":
		return corev1.ConditionFalse
	default:
		t.Fatalf("unknown pvc condition status: %q", status)
		return ""
	}
}

func filesystemResizePVC(name string, namespace string, pending bool) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
		},
	}
	if pending {
		pvc.Status.Conditions = []corev1.PersistentVolumeClaimCondition{
			{
				Type:   corev1.PersistentVolumeClaimFileSystemResizePending,
				Status: corev1.ConditionTrue,
			},
		}
	}
	return pvc
}

// TestBasaltPVCs verifies that for a basalt-backed node upsertPVC creates only
// the configured PVCs (a scratch PVC and/or a dedicated logs PVC, never a data
// PVC), and that getPVCNamesForNode reports exactly the same set.
func TestBasaltPVCs(t *testing.T) {
	ctx := context.Background()
	sizedTemplate := func(size string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
				},
			},
		}
	}

	for _, tc := range []struct {
		name    string
		scratch bool
		logs    bool
	}{
		{name: "scratch only", scratch: true},
		{name: "logs only", logs: true},
		{name: "scratch and logs", scratch: true, logs: true},
		{name: "neither"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("default")
			r := &Reconciler{Ctl: ctl}

			node := &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{Name: "n1", Namespace: "default"},
				Spec: crdbv1beta1.CrdbNodeSpec{
					DataStore: crdbv1beta1.DataStore{
						Basalt: &crdbv1beta1.BasaltStoreSpec{
							ControllerAddrs: []string{"10.0.0.1:26256"},
							TenantID:        "t",
						},
					},
				},
			}
			var want []string
			if tc.scratch {
				node.Spec.DataStore.Basalt.ScratchVolumeClaimTemplate = sizedTemplate("40Gi")
				want = append(want, manifests.CrdbNodeScratchPVCName("n1", node.Spec.DataStore.Basalt.ScratchVolumeClaimTemplate))
			}
			if tc.logs {
				node.Spec.LogsStore = &crdbv1beta1.CrdbLogsStoreSpec{VolumeClaimTemplate: sizedTemplate("5Gi")}
				want = append(want, manifests.CrdbNodeLogsStoragePVCName("n1", node.Spec.LogsStore.VolumeClaimTemplate))
			}

			result, err := r.upsertPVC(ctx, node)
			require.NoError(t, err)
			require.True(t, result.IsZero())

			// Never a data PVC for basalt; only the configured PVCs exist.
			kubetest.RequireNotExists(t, ctl,
				manifests.CrdbNodeStoragePVCName("n1", &corev1.PersistentVolumeClaim{}), &corev1.PersistentVolumeClaim{})
			for _, name := range want {
				kubetest.RequireExists(t, ctl, name, &corev1.PersistentVolumeClaim{})
			}
			if tc.scratch {
				scratchName := manifests.CrdbNodeScratchPVCName("n1", node.Spec.DataStore.Basalt.ScratchVolumeClaimTemplate)
				kubetest.RequireControllerReference(t, ctl, scratchName, &corev1.PersistentVolumeClaim{}, node)
				var got corev1.PersistentVolumeClaim
				require.NoError(t, ctl.Get(ctx, scratchName, &got))
				q := got.Spec.Resources.Requests[corev1.ResourceStorage]
				require.Equal(t, "40Gi", q.String())
			}

			// getPVCNamesForNode reports exactly the same set (scratch, then logs).
			require.Equal(t, want, r.getPVCNamesForNode(node))
		})
	}
}

// TestDeletePVCBasaltScratch verifies deletePVC removes the basalt scratch PVC
// when no retention policy is set, and retains it when WhenDeleted=Retain.
func TestDeletePVCBasaltScratch(t *testing.T) {
	ctx := context.Background()

	scratch := &corev1.PersistentVolumeClaim{
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")},
			},
		},
	}
	newNode := func(retain bool) *crdbv1beta1.CrdbNode {
		n := &crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{Name: "n1", Namespace: "default"},
			Spec: crdbv1beta1.CrdbNodeSpec{
				DataStore: crdbv1beta1.DataStore{
					Basalt: &crdbv1beta1.BasaltStoreSpec{
						ControllerAddrs:            []string{"10.0.0.1:26256"},
						TenantID:                   "t",
						ScratchVolumeClaimTemplate: scratch.DeepCopy(),
					},
				},
			},
		}
		if retain {
			n.Spec.PersistentVolumeClaimRetentionPolicy = &crdbv1beta1.CrdbNodePersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			}
		}
		return n
	}
	pvcName := manifests.CrdbNodeScratchPVCName("n1", scratch)

	t.Run("deletes when retention not set", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("default")
		r := &Reconciler{Ctl: ctl}
		node := newNode(false)

		_, err := r.upsertPVC(ctx, node)
		require.NoError(t, err)
		kubetest.RequireExists(t, ctl, pvcName, &corev1.PersistentVolumeClaim{})

		require.NoError(t, r.deletePVC(ctx, node))
		kubetest.RequireNotExists(t, ctl, pvcName, &corev1.PersistentVolumeClaim{})
	})

	t.Run("retains when WhenDeleted=Retain", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("default")
		r := &Reconciler{Ctl: ctl}
		node := newNode(true)

		_, err := r.upsertPVC(ctx, node)
		require.NoError(t, err)
		kubetest.RequireExists(t, ctl, pvcName, &corev1.PersistentVolumeClaim{})

		require.NoError(t, r.deletePVC(ctx, node))
		kubetest.RequireExists(t, ctl, pvcName, &corev1.PersistentVolumeClaim{})
	})
}

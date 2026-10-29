package nodectrl

import (
	"context"
	"fmt"
	"maps"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	opmetrics "github.com/cockroachlabs/cockroach-enterprise-operator/controllers/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode/nodemock"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/utils/timeutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/evictionutil"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/hashutil"
	"github.com/golang/mock/gomock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap/zaptest"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestReconcile(t *testing.T) {
	ctx := context.Background()
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()
	ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("default")
	ctl.Apply(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "a-vm",
		},
	})
	now := metav1.Now()

	r := &Reconciler{
		Ctl:            ctl,
		EvictionHelper: &evictionutil.Helper{},
		NodeExecutor: func(node *crdbv1beta1.CrdbNode, _ kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
			nodeExec := nodemock.NewMockNodeExecutor(mockCtrl)
			nodeExec.EXPECT().Kill(gomock.Any()).AnyTimes()
			nodeExec.EXPECT().NodeID(gomock.Any()).Return(int64(1), nil).AnyTimes()
			return nodeExec
		},
	}

	reconcile := func(objects ...kube.Object) {
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, objects...))

		for _, obj := range objects {
			for {
				res, err := r.Reconcile(ctx, ctrl.Request{
					NamespacedName: types.NamespacedName{
						Name:      obj.GetName(),
						Namespace: obj.GetNamespace(),
					},
				})
				require.NoError(t, err)
				if !res.Requeue && res.RequeueAfter == 0 {
					break
				}
			}
		}
	}

	t.Run("test finalizer behavior", func(t *testing.T) {
		withFinalizer := crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "with-finalizer",
				Namespace: "default",
				Finalizers: []string{
					manifests.CrdbNodeFinalizerName,
				},
			},
			Spec: crdbv1beta1.CrdbNodeSpec{
				DataStore: crdbv1beta1.DataStore{
					VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
						ObjectMeta: metav1.ObjectMeta{
							Name: "prefix",
						},
					},
				},
				NoCloudPrefixedLocalities: true,
				NodeName:                  "a-vm",
			},
		}

		withoutFinalizer := crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "without-finalizer",
				Namespace: "default",
			},
			Spec: crdbv1beta1.CrdbNodeSpec{
				DataStore: crdbv1beta1.DataStore{
					VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
						ObjectMeta: metav1.ObjectMeta{
							Name: "prefix",
						},
					},
				},
				NoCloudPrefixedLocalities: true,
				NodeName:                  "a-vm",
			},
		}

		reconcile(&withFinalizer, &withoutFinalizer)

		// Double check that our finalizers haven't changed. The nodectrl will only
		// ever remove the finalizer.
		require.Contains(t, withFinalizer.Finalizers, manifests.CrdbNodeFinalizerName)
		require.NotContains(t, withoutFinalizer.Finalizers, manifests.CrdbNodeFinalizerName)

		// Assert that our Pods and PVCs have been created.
		kubetest.RequireExists(t, ctl, "with-finalizer", &corev1.Pod{})
		kubetest.RequireExists(t, ctl, "without-finalizer", &corev1.Pod{})
		kubetest.RequireExists(t, ctl, "prefix-with-finalizer", &corev1.PersistentVolumeClaim{})
		kubetest.RequireExists(t, ctl, "prefix-without-finalizer", &corev1.PersistentVolumeClaim{})

		// Pods created by the nodectrl should have an owner reference set. Their
		// Pods will be GC'd by Kubernetes upon the deletion of the CrdbNode.
		kubetest.RequireControllerReference(t, ctl, "with-finalizer", &corev1.Pod{}, &withFinalizer)
		kubetest.RequireControllerReference(t, ctl, "without-finalizer", &corev1.Pod{}, &withoutFinalizer)

		// PVCs have an owner reference set for topology visibility (so
		// monitoring tools can display PVCs as children of CrdbNode). Unlike
		// Pods, the operator does NOT rely on K8s GC for PVC cleanup. The
		// finalizer handles explicit deletion, and the ownerRef is cleared in
		// the retention path to prevent accidental GC.
		kubetest.RequireControllerReference(t, ctl, "prefix-with-finalizer", &corev1.PersistentVolumeClaim{}, &withFinalizer)
		kubetest.RequireControllerReference(t, ctl, "prefix-without-finalizer", &corev1.PersistentVolumeClaim{}, &withoutFinalizer)

		// Trigger/simulate delete of our CrdbNodes. This is a bit of a hack, at
		// the time of writing the fakeCtl doesn't support finalizers so we perform
		// an update and set the DeletionTimestamp with is roughly how deletion
		// with finalizers works.
		withFinalizer.ResourceVersion = "2"
		withFinalizer.DeletionTimestamp = &now
		withoutFinalizer.ResourceVersion = "2"
		withoutFinalizer.DeletionTimestamp = &now
		require.NoError(t, ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, &withFinalizer))
		require.NoError(t, ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, &withoutFinalizer))

		reconcile(&withFinalizer, &withoutFinalizer)

		// The node without finalization should not have been cleaned up.
		// NOTE: In a real cluster, the Pod would have been deleted by Kubernetes.
		kubetest.RequireExists(t, ctl, "without-finalizer", &corev1.Pod{})
		kubetest.RequireExists(t, ctl, "prefix-without-finalizer", &corev1.PersistentVolumeClaim{})

		// The node with finalization should have removed its Pod and PVC.
		kubetest.RequireNotExists(t, ctl, "with-finalizer", &corev1.Pod{})
		kubetest.RequireNotExists(t, ctl, "prefix-with-finalizer", &corev1.PersistentVolumeClaim{})

		// Both CrdbNodes should now have their finalizers removed.
		require.NoError(t, ctl.Get(ctx, withFinalizer.Name, &withFinalizer))
		require.NotContains(t, withFinalizer.Finalizers, manifests.CrdbNodeFinalizerName)
		require.NoError(t, ctl.Get(ctx, withoutFinalizer.Name, &withoutFinalizer))
		require.NotContains(t, withoutFinalizer.Finalizers, manifests.CrdbNodeFinalizerName)
	})

	t.Run("test decommissioning metrics", func(t *testing.T) {
		// Set up a node that is being decommissioned.
		decommissioningNode := &crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cockroachdb-node-decommissioning",
				Namespace: "default",
				Annotations: map[string]string{
					crdbv1beta1.CrdbNodeStartDecommissionAnnotation: "true",
				},
				Finalizers: []string{
					manifests.CrdbNodeFinalizerName,
				},
			},
			Spec: crdbv1beta1.CrdbNodeSpec{
				DataStore: crdbv1beta1.DataStore{
					VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
						ObjectMeta: metav1.ObjectMeta{
							Name: "prefix",
						},
					},
				},
				NoCloudPrefixedLocalities: true,
				NodeName:                  "a-vm",
			},
		}
		reconcile(decommissioningNode)
		testutils.RequireMetricEqual(t, opmetrics.NodeDecommissioning.With(
			prometheus.Labels{
				"node": "cockroachdb-node-decommissioning",
			},
		), 1)

		// Now delete the node. The metrics should no longer report it being
		// decommissioned.
		decommissioningNode.ResourceVersion = "2"
		decommissioningNode.DeletionTimestamp = &now
		require.NoError(t, ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, decommissioningNode))
		reconcile(decommissioningNode)
		testutils.RequireMetricEqual(t, opmetrics.NodeDecommissioning.With(
			prometheus.Labels{
				"node": "cockroachdb-node-decommissioning",
			},
		), 0)
	})

	t.Run("test pvc retention", func(t *testing.T) {

		testCases := []struct {
			comment            string
			crdbNode           *crdbv1beta1.CrdbNode
			shouldRetainPVC    bool
			shouldRetainWALPVC bool
		}{
			{
				comment: "default delete retention policy",
				crdbNode: &crdbv1beta1.CrdbNode{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "cockroachdb-node-delete-retention",
						Namespace: "default",
						Finalizers: []string{
							manifests.CrdbNodeFinalizerName,
						},
					},
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
								ObjectMeta: metav1.ObjectMeta{
									Name: "prefix",
								},
								Spec: corev1.PersistentVolumeClaimSpec{
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceStorage: resource.MustParse("10Gi"),
										},
									},
								},
							},
						},
						NoCloudPrefixedLocalities: true,
						NodeName:                  "a-vm",
					},
				},
				shouldRetainPVC:    false,
				shouldRetainWALPVC: false,
			},
			{
				comment: "retain retention policy",
				crdbNode: &crdbv1beta1.CrdbNode{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "cockroachdb-node-retain-retention",
						Namespace: "default",
						Finalizers: []string{
							manifests.CrdbNodeFinalizerName,
						},
					},
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
								ObjectMeta: metav1.ObjectMeta{
									Name: "prefix",
								},
								Spec: corev1.PersistentVolumeClaimSpec{
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceStorage: resource.MustParse("10Gi"),
										},
									},
								},
							},
						},
						PersistentVolumeClaimRetentionPolicy: &crdbv1beta1.CrdbNodePersistentVolumeClaimRetentionPolicy{
							WhenDeleted: "Retain",
						},
						NoCloudPrefixedLocalities: true,
						NodeName:                  "a-vm",
					},
				},
				shouldRetainPVC:    true,
				shouldRetainWALPVC: false,
			},
			{
				comment: "retain retention policy with WAL failover",
				crdbNode: &crdbv1beta1.CrdbNode{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "cockroachdb-node-retain-wal",
						Namespace: "default",
						Finalizers: []string{
							manifests.CrdbNodeFinalizerName,
						},
					},
					Spec: crdbv1beta1.CrdbNodeSpec{
						DataStore: crdbv1beta1.DataStore{
							VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
								ObjectMeta: metav1.ObjectMeta{
									Name: "prefix",
								},
								Spec: corev1.PersistentVolumeClaimSpec{
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceStorage: resource.MustParse("10Gi"),
										},
									},
								},
							},
						},
						WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{
							Size: "10Gi",
						},
						PersistentVolumeClaimRetentionPolicy: &crdbv1beta1.CrdbNodePersistentVolumeClaimRetentionPolicy{
							WhenDeleted: "Retain",
						},
						NoCloudPrefixedLocalities: true,
						NodeName:                  "a-vm",
					},
				},
				shouldRetainPVC:    true,
				shouldRetainWALPVC: true,
			},
		}

		// Verify that retention metadata is written to the pvc in the correct namespace when the
		// CrdbNode lives in a custom namespace.
		t.Run("retain retention policy in non-default namespace", func(t *testing.T) {
			customCtl := ctl.WithNamespace("custom-namespace")
			// Apply "a-vm" via customCtl so it lands in the shared store under the
			// custom-namespace key. upsertPVC and pod creation use r.Ctl directly,
			// so the reconciler's Ctl must also be scoped to custom-namespace.
			customCtl.Apply(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a-vm"}})
			customR := &Reconciler{
				Ctl:            customCtl,
				EvictionHelper: r.EvictionHelper,
				NodeExecutor:   r.NodeExecutor,
			}

			crdbNode := &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "cockroachdb-node-retain-custom-ns",
					Namespace:  "custom-namespace",
					Finalizers: []string{manifests.CrdbNodeFinalizerName},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{Name: "prefix"},
							Spec: corev1.PersistentVolumeClaimSpec{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceStorage: resource.MustParse("10Gi"),
									},
								},
							},
						},
					},
					PersistentVolumeClaimRetentionPolicy: &crdbv1beta1.CrdbNodePersistentVolumeClaimRetentionPolicy{
						WhenDeleted: "Retain",
					},
					NoCloudPrefixedLocalities: true,
					NodeName:                  "a-vm",
				},
			}

			reconcileCustom := func(obj kube.Object) {
				require.NoError(t, customCtl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, obj))
				for {
					res, err := customR.Reconcile(ctx, ctrl.Request{
						NamespacedName: types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()},
					})
					require.NoError(t, err)
					if !res.Requeue && res.RequeueAfter == 0 {
						break
					}
				}
			}

			reconcileCustom(crdbNode)
			kubetest.RequireExists(t, customCtl, crdbNode.Name, &corev1.Pod{})

			dataPVCName := fmt.Sprintf("prefix-%s", crdbNode.Name)
			kubetest.RequireExists(t, customCtl, dataPVCName, &corev1.PersistentVolumeClaim{})

			crdbNode.ResourceVersion = "2"
			crdbNode.DeletionTimestamp = &now
			require.NoError(t, customCtl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, crdbNode))
			reconcileCustom(crdbNode)

			// PVC must be retained and annotated in custom-namespace, not in default.
			var pvc corev1.PersistentVolumeClaim
			require.NoError(t, customCtl.Get(ctx, dataPVCName, &pvc))
			assert.Equal(t, "custom-namespace", pvc.Namespace)
			assert.Equal(t, "Retain", pvc.Labels[retentionPolicy])
			assert.Contains(t, pvc.Annotations, retentionTimeStamp)
			assert.Equal(t, "node-deletion", pvc.Annotations[retentionReason])
		})

		for _, tc := range testCases {
			t.Run(tc.comment, func(t *testing.T) {
				nodeCtl := ctl.WithNamespace(tc.crdbNode.Namespace)

				reconcile(tc.crdbNode)
				kubetest.RequireExists(t, nodeCtl, tc.crdbNode.Name, &corev1.Pod{})

				dataPVCName := fmt.Sprintf("prefix-%s", tc.crdbNode.Name)
				kubetest.RequireExists(t, nodeCtl, dataPVCName, &corev1.PersistentVolumeClaim{})

				var walPVCName string
				if tc.crdbNode.Spec.WALFailoverSpec != nil {
					walPVCName = fmt.Sprintf("wal-prefix-%s", tc.crdbNode.Name)
					kubetest.RequireExists(t, nodeCtl, walPVCName, &corev1.PersistentVolumeClaim{})
				}

				// Now delete the node.
				tc.crdbNode.ResourceVersion = "2"
				tc.crdbNode.DeletionTimestamp = &now
				require.NoError(t, ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, tc.crdbNode))
				reconcile(tc.crdbNode)

				// Check if PVCs are retained or deleted as expected
				if tc.shouldRetainPVC {
					// PVC should still exist with retention metadata
					var pvc corev1.PersistentVolumeClaim
					require.NoError(t, nodeCtl.Get(ctx, dataPVCName, &pvc))
					assert.Equal(t, "Retain", pvc.Labels[retentionPolicy])
					assert.Contains(t, pvc.Annotations, retentionTimeStamp)
					assert.Equal(t, "node-deletion", pvc.Annotations[retentionReason])
					// ownerReference must be cleared on retained PVCs so K8s GC does
					// not delete them when the CrdbNode is eventually removed.
					kubetest.RequireNoControllerReference(t, ctl, dataPVCName, &corev1.PersistentVolumeClaim{}, tc.crdbNode)
				} else {
					kubetest.RequireNotExists(t, nodeCtl, dataPVCName, &corev1.PersistentVolumeClaim{})
				}

				// Check WAL PVC if it exists
				if tc.crdbNode.Spec.WALFailoverSpec != nil {
					if tc.shouldRetainWALPVC {
						var walPVC corev1.PersistentVolumeClaim
						require.NoError(t, nodeCtl.Get(ctx, walPVCName, &walPVC))
						assert.Equal(t, "Retain", walPVC.Labels[retentionPolicy])
						assert.Contains(t, walPVC.Annotations, retentionTimeStamp)
						assert.Equal(t, "node-deletion", walPVC.Annotations[retentionReason])
						// ownerReference must be cleared on retained WAL PVCs too.
						kubetest.RequireNoControllerReference(t, ctl, walPVCName, &corev1.PersistentVolumeClaim{}, tc.crdbNode)
					} else {
						kubetest.RequireNotExists(t, nodeCtl, walPVCName, &corev1.PersistentVolumeClaim{})
					}
				}
			})
		}
	})

	t.Run("test owner reference update", func(t *testing.T) {
		node := &crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "owner-ref-update",
				Namespace: "default",
			},
			Spec: crdbv1beta1.CrdbNodeSpec{
				NodeName: "a-vm",
				DataStore: crdbv1beta1.DataStore{
					VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
						ObjectMeta: metav1.ObjectMeta{
							Name: "prefix",
						},
					},
				},
			},
		}

		// Reconcile to create the pod
		reconcile(node)

		// Get the pod
		var pod corev1.Pod
		require.NoError(t, ctl.Get(ctx, node.Name, &pod))

		// Verify the initial owner ref
		controllerRef := metav1.GetControllerOf(&pod)
		require.NotNil(t, controllerRef)
		expectedGroupVersion := crdbv1beta1.GroupVersion.String()
		require.Equal(t, expectedGroupVersion, controllerRef.APIVersion)

		// Simulate a stale ownerRef from before the v1alpha1 → v1beta1 migration.
		controllerRef.APIVersion = "crdb.cockroachlabs.com/v1alpha1"
		pod.OwnerReferences = []metav1.OwnerReference{*controllerRef}
		require.NoError(t, ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, &pod))

		// Reconcile again
		reconcile(node)

		// Check if the owner reference is updated back to an expected version
		require.NoError(t, ctl.Get(ctx, node.Name, &pod))
		controllerRef = metav1.GetControllerOf(&pod)
		require.NotNil(t, controllerRef)
		require.Equal(t, expectedGroupVersion, controllerRef.APIVersion)
	})

	t.Run("test foregroundDeletion finalizer removal", func(t *testing.T) {
		node := &crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "foreground-delete",
				Namespace: "default",
				Finalizers: []string{
					manifests.CrdbNodeFinalizerName,
					metav1.FinalizerDeleteDependents,
				},
			},
			Spec: crdbv1beta1.CrdbNodeSpec{
				DataStore: crdbv1beta1.DataStore{
					VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
						ObjectMeta: metav1.ObjectMeta{
							Name: "prefix",
						},
					},
				},
				NoCloudPrefixedLocalities: true,
				NodeName:                  "a-vm",
			},
		}

		reconcile(node)
		kubetest.RequireExists(t, ctl, node.Name, &corev1.Pod{})

		node.ResourceVersion = "2"
		node.DeletionTimestamp = &now
		require.NoError(t, ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, node))
		reconcile(node)

		require.NoError(t, ctl.Get(ctx, node.Name, node))
		require.NotContains(t, node.Finalizers, manifests.CrdbNodeFinalizerName)
		require.NotContains(t, node.Finalizers, metav1.FinalizerDeleteDependents)
	})
}

func TestCleanupNodeResourcesKillsTerminatingPod(t *testing.T) {
	ctx := context.Background()
	mockCtrl := gomock.NewController(t)
	t.Cleanup(mockCtrl.Finish)
	now := metav1.Now()
	node := &crdbv1beta1.CrdbNode{ObjectMeta: metav1.ObjectMeta{
		Name:              "cockroachdb-terminating",
		Namespace:         "default",
		DeletionTimestamp: &now,
		Finalizers:        []string{manifests.CrdbNodeFinalizerName},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:              node.Name,
		Namespace:         node.Namespace,
		UID:               types.UID("terminating-pod-uid"),
		DeletionTimestamp: &now,
		Finalizers:        []string{"test.cockroachlabs.com/hold-deletion"},
	}}
	ctl := kubetest.NewFakeCtl(kubetest.Options{}, node, pod).WithNamespace(node.Namespace)
	executor := nodemock.NewMockNodeExecutor(mockCtrl)
	executor.EXPECT().Kill(gomock.Any()).Times(1)
	r := &Reconciler{
		Ctl: ctl,
		NodeExecutor: func(*crdbv1beta1.CrdbNode, kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
			return executor
		},
	}

	result, err := r.cleanupNodeResources(ctx, node)
	require.NoError(t, err)
	require.False(t, result.Requeue)
	require.Equal(t, time.Second, result.RequeueAfter)
}

func TestReconcileCreatesPodWhenFilesystemResizePending(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("default")
	reconciler := &Reconciler{
		Ctl:            ctl,
		EvictionHelper: &evictionutil.Helper{},
	}

	node := &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "resize-pending",
			Namespace: "default",
		},
		Spec: crdbv1beta1.CrdbNodeSpec{
			DataStore: crdbv1beta1.DataStore{
				VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name: "datadir",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("20Gi"),
							},
						},
					},
				},
			},
			NoCloudPrefixedLocalities: true,
		},
	}

	pvc := node.Spec.DataStore.VolumeClaimTemplate.DeepCopy()
	pvc.Name = manifests.CrdbNodeStoragePVCName(node.Name, pvc)
	pvc.Namespace = node.Namespace
	pvc.Status = corev1.PersistentVolumeClaimStatus{
		Phase: corev1.ClaimBound,
		Capacity: corev1.ResourceList{
			corev1.ResourceStorage: resource.MustParse("20Gi"),
		},
		Conditions: []corev1.PersistentVolumeClaimCondition{
			{
				Type:   corev1.PersistentVolumeClaimFileSystemResizePending,
				Status: corev1.ConditionTrue,
			},
		},
	}

	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, node, pvc))

	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      node.Name,
			Namespace: node.Namespace,
		},
	})
	require.NoError(t, err)
	require.True(t, result.Requeue)
	kubetest.RequireExists(t, ctl, node.Name, &corev1.Pod{})
}

func TestReconcileWaitsForFilesystemResizeAfterPodExists(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("default")
	reconciler := &Reconciler{
		Ctl:            ctl,
		EvictionHelper: &evictionutil.Helper{},
	}

	node := &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "resize-pending-existing-pod",
			Namespace:  "default",
			Generation: 7,
		},
		Spec: crdbv1beta1.CrdbNodeSpec{
			DataStore: crdbv1beta1.DataStore{
				VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name: "datadir",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("20Gi"),
							},
						},
					},
				},
			},
			NoCloudPrefixedLocalities: true,
		},
	}

	pvc := node.Spec.DataStore.VolumeClaimTemplate.DeepCopy()
	pvc.Name = manifests.CrdbNodeStoragePVCName(node.Name, pvc)
	pvc.Namespace = node.Namespace
	pvc.Status = corev1.PersistentVolumeClaimStatus{
		Phase: corev1.ClaimBound,
		Capacity: corev1.ResourceList{
			corev1.ResourceStorage: resource.MustParse("20Gi"),
		},
		Conditions: []corev1.PersistentVolumeClaimCondition{
			{
				Type:   corev1.PersistentVolumeClaimFileSystemResizePending,
				Status: corev1.ConditionTrue,
			},
		},
	}

	pod := manifests.CrdbNodePod(node, nil)
	pod.Status.Phase = corev1.PodPending

	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, node, pvc, pod))

	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      node.Name,
			Namespace: node.Namespace,
		},
	})
	require.NoError(t, err)
	require.False(t, result.Requeue)
	require.Equal(t, 5*time.Second, result.RequeueAfter)

	var updated crdbv1beta1.CrdbNode
	require.NoError(t, ctl.Get(ctx, node.Name, &updated))
	require.NotEqual(t, updated.Generation, updated.Status.ObservedGeneration)
}

func TestReconcileTopologyValuesFromRenderedPod(t *testing.T) {
	legacyConstraint := corev1.TopologySpreadConstraint{
		MaxSkew:           2,
		TopologyKey:       corev1.LabelTopologyZone,
		WhenUnsatisfiable: corev1.ScheduleAnyway,
	}
	templateConstraint := corev1.TopologySpreadConstraint{
		MaxSkew:           1,
		TopologyKey:       corev1.LabelTopologyZone,
		WhenUnsatisfiable: corev1.DoNotSchedule,
	}
	customConstraint := corev1.TopologySpreadConstraint{
		MaxSkew:           1,
		TopologyKey:       "topology.example.com/rack",
		WhenUnsatisfiable: corev1.ScheduleAnyway,
	}
	missingConstraint := corev1.TopologySpreadConstraint{
		MaxSkew:           1,
		TopologyKey:       "topology.example.com/missing",
		WhenUnsatisfiable: corev1.ScheduleAnyway,
	}
	requiredMissingConstraint := corev1.TopologySpreadConstraint{
		MaxSkew:           1,
		TopologyKey:       "topology.example.com/required-missing",
		WhenUnsatisfiable: corev1.DoNotSchedule,
	}
	regionConstraint := corev1.TopologySpreadConstraint{
		MaxSkew:           1,
		TopologyKey:       corev1.LabelTopologyRegion,
		WhenUnsatisfiable: corev1.DoNotSchedule,
	}

	testCases := []struct {
		name                    string
		legacyConstraints       []corev1.TopologySpreadConstraint
		templateConstraints     []corev1.TopologySpreadConstraint
		nodeLabels              map[string]string
		updatedNodeLabels       map[string]string
		existingTopology        []string
		expectedPodConstraints  []corev1.TopologySpreadConstraint
		expectedTopology        []string
		expectedUpdatedTopology []string
	}{
		{
			name:                   "deprecated field only",
			legacyConstraints:      []corev1.TopologySpreadConstraint{legacyConstraint},
			expectedPodConstraints: []corev1.TopologySpreadConstraint{legacyConstraint},
			expectedTopology:       []string{"us-west-2a"},
		},
		{
			name:                   "pod template only backfills existing status",
			templateConstraints:    []corev1.TopologySpreadConstraint{templateConstraint},
			existingTopology:       []string{"stale-zone"},
			expectedPodConstraints: []corev1.TopologySpreadConstraint{templateConstraint},
			expectedTopology:       []string{"us-west-2a"},
		},
		{
			name:                   "pod template takes precedence when both fields are set",
			legacyConstraints:      []corev1.TopologySpreadConstraint{legacyConstraint},
			templateConstraints:    []corev1.TopologySpreadConstraint{templateConstraint},
			expectedPodConstraints: []corev1.TopologySpreadConstraint{templateConstraint},
			expectedTopology:       []string{"us-west-2a"},
		},
		{
			name:                   "pod template supports a custom topology key",
			templateConstraints:    []corev1.TopologySpreadConstraint{customConstraint},
			nodeLabels:             map[string]string{customConstraint.TopologyKey: "rack-a"},
			expectedPodConstraints: []corev1.TopologySpreadConstraint{customConstraint},
			expectedTopology:       []string{"rack-a"},
		},
		{
			name:                "multiple topology values preserve constraint order",
			templateConstraints: []corev1.TopologySpreadConstraint{customConstraint, regionConstraint},
			nodeLabels: map[string]string{
				customConstraint.TopologyKey: "rack-a",
				corev1.LabelTopologyRegion:   "us-west-2",
			},
			expectedPodConstraints: []corev1.TopologySpreadConstraint{customConstraint, regionConstraint},
			expectedTopology:       []string{"rack-a", "us-west-2"},
		},
		{
			name: "missing topology keys preserve later value positions",
			templateConstraints: []corev1.TopologySpreadConstraint{
				missingConstraint,
				regionConstraint,
			},
			nodeLabels:              map[string]string{corev1.LabelTopologyRegion: "us-west-2"},
			updatedNodeLabels:       map[string]string{missingConstraint.TopologyKey: "rack-a"},
			existingTopology:        []string{"stale-zone"},
			expectedPodConstraints:  []corev1.TopologySpreadConstraint{missingConstraint, regionConstraint},
			expectedTopology:        []string{"", "us-west-2"},
			expectedUpdatedTopology: []string{"rack-a", "us-west-2"},
		},
		{
			name:                   "missing topology key clears existing status",
			templateConstraints:    []corev1.TopologySpreadConstraint{requiredMissingConstraint},
			existingTopology:       []string{"stale-zone"},
			expectedPodConstraints: []corev1.TopologySpreadConstraint{requiredMissingConstraint},
			expectedTopology:       []string{""},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("default")
			reconciler := NewReconciler(ctl, &evictionutil.Helper{})
			nodeLabels := map[string]string{
				corev1.LabelTopologyZone: "us-west-2a",
			}
			maps.Copy(nodeLabels, tc.nodeLabels)
			k8sNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "a-vm",
					Labels: nodeLabels,
				},
			}
			crdbNode := &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cockroachdb-0",
					Namespace: "default",
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image: "cockroachdb/cockroach:latest",
					DataStore: crdbv1beta1.DataStore{
						VolumeSource: &corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						},
					},
					NodeName:                  k8sNode.Name,
					TopologySpreadConstraints: tc.legacyConstraints,
				},
				Status: crdbv1beta1.CrdbNodeStatus{
					ObservedGeneration: -1,
					TopologyValues:     tc.existingTopology,
				},
			}
			if len(tc.templateConstraints) > 0 {
				crdbNode.Spec.PodTemplate = &crdbv1beta1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						TopologySpreadConstraints: tc.templateConstraints,
					},
				}
			}
			require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, k8sNode, crdbNode))

			request := ctrl.Request{NamespacedName: types.NamespacedName{
				Name:      crdbNode.Name,
				Namespace: crdbNode.Namespace,
			}}
			result, err := reconciler.Reconcile(ctx, request)
			require.NoError(t, err)
			require.True(t, result.Requeue)

			var pod corev1.Pod
			require.NoError(t, ctl.Get(ctx, crdbNode.Name, &pod))
			require.Equal(t, tc.expectedPodConstraints, pod.Spec.TopologySpreadConstraints)
			pod.Spec.NodeName = k8sNode.Name
			pod.Status.Phase = corev1.PodPending
			require.NoError(t, ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, &pod))

			result, err = reconciler.Reconcile(ctx, request)
			require.NoError(t, err)
			require.True(t, result.IsZero())
			require.NoError(t, ctl.Get(ctx, crdbNode.Name, crdbNode))
			require.Equal(t, tc.expectedTopology, crdbNode.Status.TopologyValues)
			require.Equal(t, crdbNode.Generation, crdbNode.Status.ObservedGeneration)

			if tc.expectedUpdatedTopology != nil {
				maps.Copy(k8sNode.Labels, tc.updatedNodeLabels)
				require.NoError(t, ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, k8sNode))

				result, err = reconciler.Reconcile(ctx, request)
				require.NoError(t, err)
				require.True(t, result.IsZero())
				require.NoError(t, ctl.Get(ctx, crdbNode.Name, crdbNode))
				require.Equal(t, tc.expectedUpdatedTopology, crdbNode.Status.TopologyValues)
			}
		})
	}
}

func TestNeedsRestart(t *testing.T) {
	r := NewReconciler(nil, nil)

	dataStoreWithSize := func(size string) crdbv1beta1.DataStore {
		return crdbv1beta1.DataStore{
			VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse(size),
						},
					},
				},
			},
		}
	}

	// oldRevision and newRevision are the two hashes for the same WAL-enabled
	// node. WAL failover is what makes them diverge: oldRevision includes the WAL
	// disk size while newRevision excludes it. Accepting oldRevision on upgrade is
	// what keeps existing pods from restarting.
	canonicalNode := &crdbv1beta1.CrdbNode{
		Spec: crdbv1beta1.CrdbNodeSpec{
			Image:           "v1",
			DataStore:       dataStoreWithSize("10Gi"),
			WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{Size: "1Gi"},
			Join:            "oldConnectionURLs",
		},
	}
	oldRevision := manifests.GetNodeHashRevisionOld(canonicalNode)
	newRevision := manifests.GetNodeHashRevisionNew(canonicalNode)

	testCases := []struct {
		*crdbv1beta1.CrdbNode
		*corev1.Pod
		expected bool
		comment  string
	}{
		{
			CrdbNode: &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 1,
				},
			},
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						manifests.NodeGenerationAnnotationKey: "2",
						manifests.NodeRevisionAnnotationKey:   "",
					},
				},
			},
			expected: false,
			comment:  "an older node generation should not trigger an eviction",
		},
		{
			CrdbNode: &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image:           "v1",
					DataStore:       dataStoreWithSize("10Gi"),
					WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{Size: "1Gi"},
					Join:            "oldConnectionURLs",
				},
			},
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						manifests.NodeGenerationAnnotationKey: "1",
						manifests.NodeRevisionAnnotationKey:   oldRevision,
					},
				},
			},
			expected: false,
			comment:  "matching old hash (WAL size included) should not trigger an eviction",
		},
		{
			CrdbNode: &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image:           "v1",
					DataStore:       dataStoreWithSize("10Gi"),
					WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{Size: "1Gi"},
					Join:            "oldConnectionURLs",
				},
			},
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						manifests.NodeGenerationAnnotationKey: "1",
						manifests.NodeRevisionAnnotationKey:   newRevision,
					},
				},
			},
			expected: false,
			comment:  "matching new hash should not trigger an eviction",
		},
		{
			CrdbNode: &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image:           "v1",
					DataStore:       dataStoreWithSize("20Gi"),
					WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{Size: "1Gi"},
					Join:            "oldConnectionURLs",
				},
			},
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						manifests.NodeGenerationAnnotationKey: "1",
						manifests.NodeRevisionAnnotationKey:   oldRevision,
					},
				},
			},
			expected: false,
			comment:  "different data volume size should not trigger an eviction",
		},
		{
			CrdbNode: &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image:           "v1",
					DataStore:       dataStoreWithSize("10Gi"),
					WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{Size: "2Gi"},
					Join:            "oldConnectionURLs",
				},
			},
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						manifests.NodeGenerationAnnotationKey: "1",
						manifests.NodeRevisionAnnotationKey:   newRevision,
					},
				},
			},
			expected: false,
			comment:  "different wal disk size on new hash should not trigger an eviction",
		},
		{
			CrdbNode: &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image:           "v1",
					DataStore:       dataStoreWithSize("20Gi"),
					WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{Size: "1Gi"},
					Join:            "newConnectionURLs",
				},
			},
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						manifests.NodeGenerationAnnotationKey: "1",
						manifests.NodeRevisionAnnotationKey:   newRevision,
					},
				},
			},
			expected: false,
			comment:  "different join path on new hash should not trigger an eviction",
		},
		{
			CrdbNode: &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Generation:  2,
					Annotations: map[string]string{"key": "value"},
					Labels:      map[string]string{"key": "value"},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image:           "v1",
					DataStore:       dataStoreWithSize("10Gi"),
					WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{Size: "1Gi"},
					Join:            "oldConnectionURLs",
				},
			},
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						manifests.NodeGenerationAnnotationKey: "1",
						manifests.NodeRevisionAnnotationKey:   newRevision,
					},
				},
			},
			expected: false,
			comment:  "changing CrdbNode annotations and labels should not trigger an eviction",
		},
		{
			CrdbNode: &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image:     "v1",
					DataStore: dataStoreWithSize("10Gi"),
					Join:      "oldConnectionURLs",
				},
			},
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						manifests.NodeGenerationAnnotationKey: "1",
						manifests.NodeRevisionAnnotationKey:   "",
					},
				},
			},
			expected: true,
			comment:  "mismatching revision should trigger an eviction",
		},
		{
			CrdbNode: &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image:     "v1",
					DataStore: dataStoreWithSize("10Gi"),
					Join:      "oldConnectionURLs",
				},
			},
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &metav1.Time{Time: timeutils.Now()},
					Annotations: map[string]string{
						manifests.NodeGenerationAnnotationKey: "1",
						manifests.NodeRevisionAnnotationKey:   "",
					},
				},
			},
			expected: false,
			comment:  "already deleted pod should not trigger an eviction",
		},
		{
			CrdbNode: &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image:           "v1",
					DataStore:       dataStoreWithSize("10Gi"),
					WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{Size: "1Gi"},
					Join:            "oldConnectionURLs",
				},
			},
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						manifests.NodeGenerationAnnotationKey: "1",
						manifests.NodeRevisionAnnotationKey:   newRevision,
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
				},
			},
			expected: true,
			comment:  "failed Pod should trigger an eviction even if up to date",
		},
		{
			CrdbNode: &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image:           "v1",
					DataStore:       dataStoreWithSize("10Gi"),
					WALFailoverSpec: &crdbv1beta1.CrdbWalFailoverSpec{Path: "path"},
					Join:            "oldConnectionURLs",
				},
			},
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						manifests.NodeGenerationAnnotationKey: "1",
						manifests.NodeRevisionAnnotationKey:   "",
					},
				},
			},
			expected: true,
			comment:  "changing non-size wal field should trigger a restart",
		},
		{
			CrdbNode: &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image:     "v1",
					DataStore: dataStoreWithSize("10Gi"),
					Join:      "oldConnectionURLs",
				},
			},
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						manifests.NodeGenerationAnnotationKey: "1",
						manifests.NodeRevisionAnnotationKey:   "",
					},
				},
			},
			expected: true,
			comment:  "disabling wal failover should trigger a restart",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.comment, func(t *testing.T) {
			got := r.podNeedsUpdate(context.Background(), tc.CrdbNode, tc.Pod)
			require.Equal(t, tc.expected, got)
		})
	}
}

type EvictionTestSuite struct {
	suite.Suite

	ctx        context.Context
	ctl        *kube.Ctl
	reconciler *Reconciler
	reconcile  func(objects ...kube.Object)
	crdbNode   *crdbv1beta1.CrdbNode
}

func (s *EvictionTestSuite) SetupTest() {
	s.ctx = context.Background()
	if testing.Verbose() {
		s.ctx = logging.WithLogger(s.ctx, zaptest.NewLogger(s.T()))
	}

	s.ctl = kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("default")
	s.ctl.Apply(s.ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "a-vm",
		},
	})
	s.reconciler = NewReconciler(s.ctl, &evictionutil.Helper{})

	s.reconcile = func(objects ...kube.Object) {
		s.Require().NoError(s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{NoWait: true}, objects...))

		for _, obj := range objects {
			_, err := s.reconciler.Reconcile(s.ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      obj.GetName(),
					Namespace: obj.GetNamespace(),
				},
			})
			s.Require().NoError(err)
		}
	}

	s.crdbNode = &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "a-crdb-node",
			Namespace: "default",
			Finalizers: []string{
				manifests.CrdbNodeFinalizerName,
			},
			OwnerReferences: []metav1.OwnerReference{
				{Name: "a-crdb-cluster"},
			},
		},
		Spec: crdbv1beta1.CrdbNodeSpec{
			DataStore: crdbv1beta1.DataStore{
				VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name: "prefix",
					},
				},
			},
			NoCloudPrefixedLocalities: true,
			NodeName:                  "a-vm",
		},
	}

	// Bring everything up
	s.reconcile(s.crdbNode)
}

func TestEviction(t *testing.T) {
	suite.Run(t, new(EvictionTestSuite))
}

func (s *EvictionTestSuite) TestSuccessfulEviction() {
	s.reconciler.EvictionHelper.AllowNextEvictionFor(&crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "a-crdb-cluster",
			Namespace: s.ctl.Namespace(),
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			RollingRestartDelay: &metav1.Duration{
				Duration: time.Nanosecond,
			},
		},
	})

	ctx := context.Background()

	// Make an update to the spec, which means the pod will need an
	// update.
	s.crdbNode.Spec.Env = append(s.crdbNode.Spec.Env, corev1.EnvVar{Name: "test"})
	s.reconcile(s.crdbNode)

	// The new pod has not been brought up with the new spec yet, and as
	// such the node has PodNeedsUpdate set to true.
	s.Require().NoError(s.ctl.Get(ctx, s.crdbNode.Name, s.crdbNode))
	var noPod corev1.Pod
	s.Require().True(kubeerrors.IsNotFound(s.ctl.Get(ctx, s.crdbNode.Name, &noPod)))
	s.Require().True(s.crdbNode.Status.HasCondition(crdbv1beta1.PodNeedsUpdate, metav1.ConditionTrue))

	// Reconcile once to recreate the pod.
	s.reconcile(s.crdbNode)
	// Reconcile again to update the the crdbnode status.
	s.reconcile(s.crdbNode)

	// After another call to reconcile, the pod spec should be updated,
	// and as such the node has PodNeedsUpdate set to false.
	s.Require().NoError(s.ctl.Get(ctx, s.crdbNode.Name, s.crdbNode))
	s.Require().True(s.crdbNode.Status.HasCondition(crdbv1beta1.PodNeedsUpdate, metav1.ConditionFalse))
}

// TestDecommissioningNodeIsNotEvicted covers the ensurePod guard that
// skips spec-drift eviction for decommissioning nodes. The excluded-node
// branch shares the same predicate (node.IsDecommissioning() ||
// node.IsExcluded()) so it doesn't need a separate test.
func (s *EvictionTestSuite) TestDecommissioningNodeIsNotEvicted() {
	// The eviction helper would otherwise let the eviction proceed. The
	// decommissioning guard in ensurePod is what blocks it.
	s.reconciler.EvictionHelper.AllowNextEvictionFor(&crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "a-crdb-cluster",
			Namespace: s.ctl.Namespace(),
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			RollingRestartDelay: &metav1.Duration{
				Duration: time.Nanosecond,
			},
		},
	})

	ctx := context.Background()

	// Capture the pod before the spec change so we can compare later.
	var originalPod corev1.Pod
	s.Require().NoError(s.ctl.Get(ctx, s.crdbNode.Name, &originalPod))

	// Mark the node for decommissioning. This mirrors what the cluster
	// controller does when it picks a node for scale-down.
	if s.crdbNode.Annotations == nil {
		s.crdbNode.Annotations = map[string]string{}
	}
	s.crdbNode.Annotations[crdbv1beta1.CrdbNodeStartDecommissionAnnotation] = "1"

	// Change the spec so podNeedsUpdate would normally return true.
	s.crdbNode.Spec.Env = append(s.crdbNode.Spec.Env, corev1.EnvVar{Name: "test"})
	s.reconcile(s.crdbNode)

	// PodNeedsUpdate stays false because ensurePod short-circuits for
	// decommissioning nodes.
	s.Require().NoError(s.ctl.Get(ctx, s.crdbNode.Name, s.crdbNode))
	s.Require().True(s.crdbNode.Status.HasCondition(crdbv1beta1.PodNeedsUpdate, metav1.ConditionFalse))

	// The pod was not evicted. Its UID is unchanged from before the reconcile.
	var currentPod corev1.Pod
	s.Require().NoError(s.ctl.Get(ctx, s.crdbNode.Name, &currentPod))
	s.Require().Equal(originalPod.UID, currentPod.UID)
	s.Require().True(currentPod.DeletionTimestamp.IsZero())
}

func (s *EvictionTestSuite) TestUnsuccessfulEviction() {
	s.reconciler.EvictionHelper.DisableEvictionsFor(&crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "a-crdb-cluster",
			Namespace: s.ctl.Namespace(),
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			RollingRestartDelay: &metav1.Duration{
				Duration: time.Nanosecond,
			},
		},
	})

	ctx := context.Background()

	var originalPod corev1.Pod
	s.Require().NoError(s.ctl.Get(ctx, s.crdbNode.Name, &originalPod))

	// This very first call just brings everything up
	s.reconcile(s.crdbNode)

	// Make an update to the spec, which means the pod will need an
	// update.
	s.crdbNode.Spec.Env = append(s.crdbNode.Spec.Env, corev1.EnvVar{Name: "test"})
	s.reconcile(s.crdbNode)

	// The pod has been blocked from being evicted by the eviction helper,
	// and the pod spec is unchanged.
	// The node has PodNeedsUpdate set to true.
	s.Require().NoError(s.ctl.Get(ctx, s.crdbNode.Name, s.crdbNode))
	var samePod corev1.Pod
	s.Require().NoError(s.ctl.Get(ctx, s.crdbNode.Name, &samePod))
	s.Require().Equal(hashutil.MustFNV(originalPod), hashutil.MustFNV(samePod))
	s.Require().True(s.crdbNode.Status.HasCondition(crdbv1beta1.PodNeedsUpdate, metav1.ConditionTrue))
}

// TestLegacyRevisionIsRestampedInPlace covers the migration of a pod created
// before WAL disk size was excluded from the revision hash. Such a pod carries
// the WAL-inclusive old hash. Instead of evicting it, the reconciler re-stamps
// its nodeRevision annotation to the new hash so a later WAL size change doesn't
// look like drift and restart it.
func (s *EvictionTestSuite) TestLegacyRevisionIsRestampedInPlace() {
	ctx := s.ctx

	node := &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "wal-node",
			Namespace: "default",
			Finalizers: []string{
				manifests.CrdbNodeFinalizerName,
			},
			OwnerReferences: []metav1.OwnerReference{
				{Name: "a-crdb-cluster"},
			},
		},
		Spec: crdbv1beta1.CrdbNodeSpec{
			DataStore: crdbv1beta1.DataStore{
				VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name: "prefix",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("10Gi"),
							},
						},
					},
				},
			},
			WALFailoverSpec:           &crdbv1beta1.CrdbWalFailoverSpec{Name: "wal", Size: "1Gi"},
			NoCloudPrefixedLocalities: true,
			NodeName:                  "a-vm",
		},
	}

	// Bring the WAL node up so its pod carries the new (WAL-excluded) hash.
	s.reconcile(node)
	s.reconcile(node)

	var pod corev1.Pod
	s.Require().NoError(s.ctl.Get(ctx, node.Name, &pod))
	s.Require().NoError(s.ctl.Get(ctx, node.Name, node))
	s.Require().Equal(manifests.GetNodeHashRevisionNew(node), manifests.GetNodePodRevision(&pod))

	// Rewrite the pod's stored revision to the WAL-inclusive old hash to mimic a
	// pod created by a pre-fix operator.
	originalUID := pod.UID
	pod.Annotations[manifests.NodeRevisionAnnotationKey] = manifests.GetNodeHashRevisionOld(node)
	s.Require().NoError(s.ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, &pod))

	// Reconcile with WAL unchanged. The legacy pod is migrated to the new hash in
	// place rather than evicted.
	s.reconcile(node)

	var restamped corev1.Pod
	s.Require().NoError(s.ctl.Get(ctx, node.Name, &restamped))
	s.Require().Equal(originalUID, restamped.UID)
	s.Require().True(restamped.DeletionTimestamp.IsZero())
	s.Require().Equal(manifests.GetNodeHashRevisionNew(node), manifests.GetNodePodRevision(&restamped))

	// Now that the pod carries the new hash, changing the WAL disk size leaves the
	// revision unchanged and does not evict the pod.
	s.Require().NoError(s.ctl.Get(ctx, node.Name, node))
	node.Spec.WALFailoverSpec.Size = "2Gi"
	s.reconcile(node)

	var afterResize corev1.Pod
	s.Require().NoError(s.ctl.Get(ctx, node.Name, &afterResize))
	s.Require().Equal(originalUID, afterResize.UID)
	s.Require().True(afterResize.DeletionTimestamp.IsZero())

	s.Require().NoError(s.ctl.Get(ctx, node.Name, node))
	s.Require().True(node.Status.HasCondition(crdbv1beta1.PodNeedsUpdate, metav1.ConditionFalse))
}

func TestFilterPodConditions(t *testing.T) {
	someCondition := crdbv1beta1.NodeConditionType("some condition")

	conditions := []crdbv1beta1.NodeCondition{
		{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue},
		{Type: crdbv1beta1.PodReady, Status: metav1.ConditionTrue},
		{Type: someCondition, Status: metav1.ConditionTrue},
	}

	filtered := FilterPodConditions(conditions)
	require.Len(t, filtered, 1)
	require.Equal(t, someCondition, filtered[0].Type)
}

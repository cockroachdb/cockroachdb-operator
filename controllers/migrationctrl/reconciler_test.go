package migrationctrl

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/cockroachdb/errors"
	crdbv1alpha1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1alpha1"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/migrationutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestMigrationTypeDetection verifies that the reconciler distinguishes Helm vs operator
// migrations based on the presence of a StatefulSet and/or a v1alpha1 CrdbCluster.
func TestMigrationTypeDetection(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		stsExists      bool
		v1alpha1Exists bool
		migrateLabel   bool
		expectedType   string
		expectedError  bool
	}{
		{
			name:           "Helm migration - only STS exists",
			stsExists:      true,
			v1alpha1Exists: false,
			expectedType:   MigrationTypeHelm,
			expectedError:  false,
		},
		{
			name:           "Public Operator migration - both STS and CR exist with migrate label",
			stsExists:      true,
			v1alpha1Exists: true,
			migrateLabel:   true,
			expectedType:   MigrationTypeOperator,
			expectedError:  false,
		},
		{
			name:           "Helm migration - v1alpha1 exists but is a conversion artifact",
			stsExists:      true,
			v1alpha1Exists: true,
			migrateLabel:   false,
			expectedType:   MigrationTypeHelm,
			expectedError:  false,
		},
		{
			name:           "Helm completion - no STS, v1alpha1 is conversion artifact",
			stsExists:      false,
			v1alpha1Exists: true,
			migrateLabel:   false,
			expectedType:   "",
			expectedError:  true,
		},
		{
			name:           "No resources - should error",
			stsExists:      false,
			v1alpha1Exists: false,
			expectedType:   "",
			expectedError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use kube.Scheme which includes both v1alpha1 and v1beta1 types.
			ctl := kubetest.NewFakeCtl(kubetest.Options{
				Options: kube.Options{
					Scheme: kube.Scheme,
				},
			}).WithNamespace(testNamespace)
			r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

			clusterName := testClusterName

			if tt.stsExists {
				sts := &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:       clusterName,
						Namespace:  testNamespace,
						Generation: 1,
						Labels: map[string]string{
							"app.kubernetes.io/name": "cockroachdb",
						},
					},
					Spec: appsv1.StatefulSetSpec{
						Replicas: new(int32(3)),
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "db"},
								},
							},
						},
					},
					Status: appsv1.StatefulSetStatus{
						ObservedGeneration: 1,
						Replicas:           3,
						ReadyReplicas:      3,
					},
				}
				require.NoError(t, ctl.Create(ctx, sts))
			}

			if tt.v1alpha1Exists {
				labels := map[string]string{}
				if tt.migrateLabel {
					labels[LabelMigrate] = MigrateStart
					labels[LabelSkipReconcile] = "true"
				}
				v1alpha1Cluster := &crdbv1alpha1.CrdbCluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:       clusterName,
						Namespace:  testNamespace,
						Generation: 1,
						Labels:     labels,
					},
					Status: crdbv1alpha1.CrdbClusterStatus{
						Conditions: []crdbv1alpha1.ClusterCondition{
							{
								Type:   crdbv1alpha1.CrdbInitializedCondition,
								Status: metav1.ConditionTrue,
							},
						},
					},
				}
				require.NoError(t, ctl.Create(ctx, v1alpha1Cluster))
			}

			result, err := r.discoverMigrationResources(ctx, ctl, clusterName)

			if tt.expectedError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expectedType, result.MigrationType)
			}
		})
	}
}

// TestIsCRDBStatefulSet verifies that StatefulSets are identified as CockroachDB
// based on standard Helm and public operator labels.
func TestIsCRDBStatefulSet(t *testing.T) {
	tests := []struct {
		name     string
		sts      *appsv1.StatefulSet
		expected bool
	}{
		{
			name: "Valid CRDB STS with app.kubernetes.io/name label",
			sts: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name": "cockroachdb",
					},
				},
			},
			expected: true,
		},
		{
			name: "Valid CRDB STS with app label",
			sts: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "cockroachdb",
					},
				},
			},
			expected: true,
		},
		{
			name: "Valid CRDB STS with component label",
			sts: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/component": "database",
					},
				},
			},
			expected: true,
		},
		{
			name: "Valid CRDB STS with db container",
			sts: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{},
				},
				Spec: appsv1.StatefulSetSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "db"},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "Invalid STS - no matching labels or containers",
			sts: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "postgres",
					},
				},
			},
			expected: false,
		},
		{
			name:     "Nil STS",
			sts:      nil,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isCRDBStatefulSet(tt.sts)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestGetMigrationLabel verifies that the migration label value is read from the right
// source depending on whether the migration is Helm-based or operator-based.
func TestGetMigrationLabel(t *testing.T) {
	tests := []struct {
		name           string
		result         *DiscoveryResult
		expectedValue  string
		expectedExists bool
	}{
		{
			name: "Label exists on STS",
			result: &DiscoveryResult{
				StatefulSet: &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							LabelMigrate: MigrateStart,
						},
					},
				},
			},
			expectedValue:  MigrateStart,
			expectedExists: true,
		},
		{
			name: "Label exists on CR",
			result: &DiscoveryResult{
				V1Alpha1Cluster: &crdbv1alpha1.CrdbCluster{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							LabelMigrate: MigrateInProgress,
						},
					},
				},
			},
			expectedValue:  MigrateInProgress,
			expectedExists: true,
		},
		{
			name: "Label does not exist",
			result: &DiscoveryResult{
				StatefulSet: &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{},
					},
				},
			},
			expectedValue:  "",
			expectedExists: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, exists := getMigrationLabel(tt.result)
			assert.Equal(t, tt.expectedValue, value)
			assert.Equal(t, tt.expectedExists, exists)
		})
	}
}

// TestMigrationStatusSerialization verifies that MigrationStatus survives a JSON
// round-trip with all fields intact.
func TestMigrationStatusSerialization(t *testing.T) {
	// Create initial status
	status := &crdbv1beta1.MigrationStatus{
		Phase:           PhaseCertMigration,
		Type:            MigrationTypeHelm,
		TotalPods:       3,
		CurrentPodIndex: 2,
		Message:         "Test message",
		MigratedNodes:   []string{"node-0", "node-1"},
	}

	// Marshal to JSON
	data, err := json.Marshal(status)
	require.NoError(t, err)

	// Unmarshal back
	var loaded crdbv1beta1.MigrationStatus
	err = json.Unmarshal(data, &loaded)
	require.NoError(t, err)

	// Verify fields
	assert.Equal(t, PhaseCertMigration, loaded.Phase)
	assert.Equal(t, MigrationTypeHelm, loaded.Type)
	assert.Equal(t, int32(3), loaded.TotalPods)
	assert.Equal(t, int32(2), loaded.CurrentPodIndex)
	assert.Equal(t, "Test message", loaded.Message)
	assert.Equal(t, []string{"node-0", "node-1"}, loaded.MigratedNodes)
}

// TestGetOrCreateCluster_HelmCreatesMinimalCluster verifies that getOrCreateCluster
// creates a minimal CrdbCluster with Mode=Disabled for Helm migrations when no CrdbCluster exists.
func TestGetOrCreateCluster_HelmCreatesMinimalCluster(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)
	replicas := int32(3)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testClusterName,
			Namespace: testNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": "cockroachdb",
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: testClusterName,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: testClusterName + "-sa",
					Containers: []corev1.Container{{
						Name:  migrationutil.CrdbContainerName,
						Image: "cockroachdb/cockroach:v25.1.0",
						Args: []string{
							"start",
							"--insecure",
							"--join=" + testClusterName + "-0." + testClusterName + "." + testNamespace + ".svc.cluster.local:26258",
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "custom-data",
							MountPath: "/cockroach/cockroach-data",
						}},
					}},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "custom-data"},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				},
			}},
		},
	}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, sts))

	discovery := &DiscoveryResult{
		Name:             testClusterName,
		Namespace:        testNamespace,
		MigrationType:    MigrationTypeHelm,
		StatefulSet:      sts,
		Replicas:         replicas,
		OriginalReplicas: replicas,
	}

	cluster, err := r.getOrCreateCluster(ctx, ctl, discovery)
	require.NoError(t, err)
	require.NotNil(t, cluster)

	// Verify minimal cluster was created with Mode=Disabled
	assert.Equal(t, testClusterName, cluster.Name)
	assert.Equal(t, testNamespace, cluster.Namespace)
	require.NotNil(t, cluster.Spec.Mode)
	assert.Equal(t, crdbv1beta1.Disabled, *cluster.Spec.Mode)
	assert.Equal(t, MigrateInProgress, cluster.Labels[LabelMigrationStatus])
	require.Len(t, cluster.Spec.Regions, 1)
	assert.Equal(t, replicas, cluster.Spec.Regions[0].Nodes)
	require.NotNil(t, cluster.Spec.Template.Spec.DataStore.VolumeClaimTemplate)
	assert.Equal(t, "custom-data", cluster.Spec.Template.Spec.DataStore.VolumeClaimTemplate.Name)

	// Verify cluster was persisted (has ResourceVersion)
	assert.NotEmpty(t, cluster.ResourceVersion)

	// Verify it can be fetched from the API
	var fetched crdbv1beta1.CrdbCluster
	require.NoError(t, ctl.Get(ctx, testClusterName, &fetched))
	assert.Equal(t, crdbv1beta1.Disabled, *fetched.Spec.Mode)
}

// TestGetOrCreateCluster_HelmAlreadyExists verifies that getOrCreateCluster returns
// an existing CrdbCluster without creating a new one.
func TestGetOrCreateCluster_HelmAlreadyExists(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	// Pre-create a CrdbCluster
	mode := crdbv1beta1.Disabled
	existing := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testClusterName,
			Namespace: testNamespace,
			Labels: map[string]string{
				LabelMigrationStatus: MigrateInProgress,
			},
		},
		Spec: crdbv1beta1.CrdbClusterSpec{Mode: &mode},
	}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, existing))

	discovery := &DiscoveryResult{
		Name:          testClusterName,
		Namespace:     testNamespace,
		MigrationType: MigrationTypeHelm,
	}

	cluster, err := r.getOrCreateCluster(ctx, ctl, discovery)
	require.NoError(t, err)
	require.NotNil(t, cluster)
	assert.Equal(t, testClusterName, cluster.Name)
	assert.Equal(t, existing.ResourceVersion, cluster.ResourceVersion)
}

// TestGetOrCreateCluster_OperatorMustExist verifies that getOrCreateCluster returns
// an error for operator migrations when no CrdbCluster exists (it should come from
// the conversion webhook).
func TestGetOrCreateCluster_OperatorMustExist(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	discovery := &DiscoveryResult{
		Name:          testClusterName,
		Namespace:     testNamespace,
		MigrationType: MigrationTypeOperator,
	}

	_, err := r.getOrCreateCluster(ctx, ctl, discovery)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Contains(t, err.Error(), "conversion webhook")
}

// TestDiscovery_NilReplicasGuard verifies that discovery returns an error when the
// StatefulSet has nil Replicas instead of panicking.
func TestDiscovery_NilReplicasGuard(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	// Create a STS with nil Replicas using NoWait since readiness check requires replicas.
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testClusterName,
			Namespace:  testNamespace,
			Generation: 1,
			Labels: map[string]string{
				"app.kubernetes.io/name": "cockroachdb",
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: nil, // nil Replicas
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "db"}},
				},
			},
		},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 1},
	}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, sts))

	_, err := r.discoverMigrationResources(ctx, ctl, testClusterName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil Replicas")
}

// TestPhaseStopped_Persistence verifies that PhaseStopped is persisted via UpdateStatus
// before returning NoRequeue.
func TestPhaseStopped_Persistence(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	// Create the CrdbCluster with migration in progress.
	cluster := createTestClusterWithMigration(PhasePodMigration, 3)
	mode := crdbv1beta1.Disabled
	cluster.Spec.Mode = &mode
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

	// Create the STS with stop label.
	sts := createTestStatefulSet(testClusterName, testNamespace, 3)
	sts.Labels[LabelMigrate] = MigrateStop
	require.NoError(t, ctl.Create(ctx, sts))

	// Reconcile with stop label
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sts)}
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)

	// Verify PhaseStopped is persisted in the status sub-resource.
	var fetched crdbv1beta1.CrdbCluster
	require.NoError(t, ctl.Get(ctx, testClusterName, &fetched))
	require.NotNil(t, fetched.Status.Migration)
	assert.Equal(t, PhaseStopped, fetched.Status.Migration.Phase)
}

// TestPhaseStopped_DuringRollback verifies that stopping during rollback persists a
// message telling the user to set the label to "rollback" to resume. Without this
// distinction, the user would set "start" and accidentally resume forward migration
// instead of continuing the rollback.
func TestPhaseStopped_DuringRollback(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	cluster := createTestClusterWithMigration(PhaseRollbackInProgress, 3)
	mode := crdbv1beta1.Disabled
	cluster.Spec.Mode = &mode
	cluster.Status.Migration.MigratedNodes = []string{fmt.Sprintf("%s-2", testClusterName)}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

	sts := createTestStatefulSet(testClusterName, testNamespace, 3)
	sts.Labels[LabelMigrate] = MigrateStop
	require.NoError(t, ctl.Create(ctx, sts))

	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sts)}
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)

	var fetched crdbv1beta1.CrdbCluster
	require.NoError(t, ctl.Get(ctx, testClusterName, &fetched))
	require.NotNil(t, fetched.Status.Migration)
	assert.Equal(t, PhaseStopped, fetched.Status.Migration.Phase)
	assert.Contains(t, fetched.Status.Migration.Message, "rollback",
		"message should mention rollback so the user knows how to resume")
	assert.Contains(t, fetched.Status.Migration.Message, "was in "+PhaseRollbackInProgress)
}

// TestPhaseStopped_ForwardMigrationGapDetection verifies that stopping during
// pod migration correctly handles the gap between STS scale-down and CrdbNode
// creation. When the CrdbNode was never created, the STS is scaled back up to
// avoid cluster degradation. When it already exists, no scaling is needed.
func TestPhaseStopped_ForwardMigrationGapDetection(t *testing.T) {
	tests := []struct {
		name            string
		crdbNodeExists  bool
		wantSTSReplicas int32
		replicasMsg     string
	}{
		{
			name:            "scales STS up when CrdbNode not created",
			crdbNodeExists:  false,
			wantSTSReplicas: 3,
			replicasMsg:     "STS should be restored to avoid cluster degradation during pause",
		},
		{
			name:            "no restore when CrdbNode already exists",
			crdbNodeExists:  true,
			wantSTSReplicas: 2,
			replicasMsg:     "STS should not be scaled up when CrdbNode already exists",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctl := kubetest.NewFakeCtl(kubetest.Options{
				Options: kube.Options{Scheme: kube.Scheme},
			}).WithNamespace(testNamespace)
			r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

			cluster := createTestClusterWithMigration(PhasePodMigration, 3)
			mode := crdbv1beta1.Disabled
			cluster.Spec.Mode = &mode
			cluster.Status.Migration.CurrentPodIndex = 2
			require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

			if tt.crdbNodeExists {
				require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true},
					createTestCrdbNode(testClusterName, testNamespace, 2)))
			}

			sts := createTestStatefulSet(testClusterName, testNamespace, 2)
			sts.Labels[LabelMigrate] = MigrateStop
			require.NoError(t, ctl.Create(ctx, sts))

			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sts)}
			_, err := r.Reconcile(ctx, req)
			require.NoError(t, err)

			var updatedSTS appsv1.StatefulSet
			require.NoError(t, ctl.Get(ctx, testClusterName, &updatedSTS))
			require.NotNil(t, updatedSTS.Spec.Replicas)
			assert.Equal(t, tt.wantSTSReplicas, *updatedSTS.Spec.Replicas, tt.replicasMsg)

			var fetched crdbv1beta1.CrdbCluster
			require.NoError(t, ctl.Get(ctx, testClusterName, &fetched))
			require.NotNil(t, fetched.Status.Migration)
			assert.Equal(t, PhaseStopped, fetched.Status.Migration.Phase)
		})
	}
}

// TestPhaseStopped_DuringRollback_GapRestoration verifies that stopping during
// rollback correctly restores the cluster to full replica count and cleans
// stale MigratedNodes entries, both when the STS needs scaling (gap detection)
// and when it was already scaled by a previous crashed run (idempotent cleanup).
func TestPhaseStopped_DuringRollback_GapRestoration(t *testing.T) {
	tests := []struct {
		name            string
		stsReplicas     int32
		wantSTSReplicas int32
		stsReplicasMsg  string
	}{
		{
			name:            "scales STS up when gap detected",
			stsReplicas:     1,
			wantSTSReplicas: 2,
			stsReplicasMsg:  "STS should be scaled up to restore the pod whose CrdbNode was deleted",
		},
		{
			name:            "idempotent cleanup after crash recovery",
			stsReplicas:     2,
			wantSTSReplicas: 2,
			stsReplicasMsg:  "STS should remain at 2 replicas since it was already scaled up",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctl := kubetest.NewFakeCtl(kubetest.Options{
				Options: kube.Options{Scheme: kube.Scheme},
			}).WithNamespace(testNamespace)
			r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

			cluster := createTestClusterWithMigration(PhaseRollbackInProgress, 3)
			mode := crdbv1beta1.Disabled
			cluster.Spec.Mode = &mode
			cluster.Status.Migration.MigratedNodes = []string{
				fmt.Sprintf("%s-1", testClusterName),
				fmt.Sprintf("%s-2", testClusterName),
			}
			require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

			require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true},
				createTestCrdbNode(testClusterName, testNamespace, 2)))

			sts := createTestStatefulSet(testClusterName, testNamespace, tt.stsReplicas)
			sts.Labels[LabelMigrate] = MigrateStop
			require.NoError(t, ctl.Create(ctx, sts))

			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sts)}
			_, err := r.Reconcile(ctx, req)
			require.NoError(t, err)

			var updatedSTS appsv1.StatefulSet
			require.NoError(t, ctl.Get(ctx, testClusterName, &updatedSTS))
			require.NotNil(t, updatedSTS.Spec.Replicas)
			assert.Equal(t, tt.wantSTSReplicas, *updatedSTS.Spec.Replicas, tt.stsReplicasMsg)

			var updatedCluster crdbv1beta1.CrdbCluster
			require.NoError(t, ctl.Get(ctx, testClusterName, &updatedCluster))
			require.NotNil(t, updatedCluster.Status.Migration)
			assert.Equal(t, PhaseStopped, updatedCluster.Status.Migration.Phase)
			assert.Equal(t, []string{fmt.Sprintf("%s-2", testClusterName)}, updatedCluster.Status.Migration.MigratedNodes,
				"stale cluster-1 should be removed from MigratedNodes")
		})
	}
}

// TestResumeFromStopped verifies that resumeFromStopped advances to the correct
// phase based on how far the migration got before stopping.
func TestResumeFromStopped(t *testing.T) {
	tests := []struct {
		name            string
		totalPods       int32
		migratedNodes   []string
		origReplicas    int32
		currentPodIndex int32
		stsName         string
		wantPhase       string
		wantPodIndex    int32
		checkPodIndex   bool
	}{
		{
			name:          "no nodes migrated resumes to cert migration with correct index",
			totalPods:     3,
			migratedNodes: nil,
			stsName:       "cluster",
			wantPhase:     PhaseCertMigration,
			wantPodIndex:  2,
			checkPodIndex: true,
		},
		{
			name:          "partial migration resumes to pod migration",
			totalPods:     3,
			migratedNodes: []string{"cluster-2", "cluster-1"},
			stsName:       "cluster",
			wantPhase:     PhasePodMigration,
			wantPodIndex:  0,
			checkPodIndex: true,
		},
		{
			name:          "all nodes migrated resumes to finalization",
			totalPods:     3,
			migratedNodes: []string{"cluster-0", "cluster-1", "cluster-2"},
			wantPhase:     PhaseFinalization,
		},
		{
			name:         "lost TotalPods reconstructed from discovery",
			totalPods:    0,
			origReplicas: 3,
			wantPhase:    PhaseCertMigration,
		},
		{
			name:      "zero pods and zero replicas falls back to init",
			totalPods: 0,
			wantPhase: PhaseInit,
		},
		{
			name:            "stale CurrentPodIndex recomputed after rollback stop",
			totalPods:       3,
			migratedNodes:   []string{"cluster-1", "cluster-2"},
			currentPodIndex: -1,
			stsName:         "cluster",
			wantPhase:       PhasePodMigration,
			wantPodIndex:    0,
			checkPodIndex:   true,
		},
		{
			name:            "CurrentPodIndex recomputed with gap in middle",
			totalPods:       5,
			migratedNodes:   []string{"cluster-4", "cluster-3", "cluster-1"},
			currentPodIndex: -1,
			stsName:         "cluster",
			wantPhase:       PhasePodMigration,
			wantPodIndex:    2,
			checkPodIndex:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			migration := &crdbv1beta1.MigrationStatus{
				Phase:           PhaseStopped,
				Type:            MigrationTypeHelm,
				TotalPods:       tt.totalPods,
				MigratedNodes:   tt.migratedNodes,
				CurrentPodIndex: tt.currentPodIndex,
			}
			discovery := &DiscoveryResult{
				Name:             tt.stsName,
				OriginalReplicas: tt.origReplicas,
			}

			resumeFromStopped(ctx, migration, discovery)

			assert.Equal(t, tt.wantPhase, migration.Phase)
			assert.NotEmpty(t, migration.Message, "should set a resume message")
			if tt.checkPodIndex {
				assert.Equal(t, tt.wantPodIndex, migration.CurrentPodIndex, "CurrentPodIndex should be recomputed")
			}
		})
	}
}

// TestReconcile_TerminalLabelValues_ReturnNoRequeue verifies that terminal label values
// (complete, failed, stopped) cause the reconciler to return immediately without processing.
func TestReconcile_TerminalLabelValues_ReturnNoRequeue(t *testing.T) {
	terminalValues := []string{MigrateComplete, MigrateFailed, MigrateStopped}

	for _, labelValue := range terminalValues {
		t.Run(labelValue, func(t *testing.T) {
			ctx := context.Background()
			ctl := kubetest.NewFakeCtl(kubetest.Options{
				Options: kube.Options{Scheme: kube.Scheme},
			}).WithNamespace(testNamespace)
			r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

			sts := createTestStatefulSet(testClusterName, testNamespace, 3)
			sts.Labels[LabelMigrate] = labelValue
			require.NoError(t, ctl.Create(ctx, sts))

			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sts)}
			result, err := r.Reconcile(ctx, req)
			require.NoError(t, err)
			// Terminal labels should never requeue.
			assert.Zero(t, result.RequeueAfter)
			assert.False(t, result.Requeue)
		})
	}
}

func TestNeedsMigrationCompletionRecovery(t *testing.T) {
	mode := func(value crdbv1beta1.ReconciliationMode) *crdbv1beta1.ReconciliationMode {
		return &value
	}

	tests := []struct {
		name      string
		mode      *crdbv1beta1.ReconciliationMode
		migration *crdbv1beta1.MigrationStatus
		label     string
		want      bool
	}{
		{name: "fresh v1beta1 cluster"},
		{
			name:      "active pod migration",
			mode:      mode(crdbv1beta1.Disabled),
			migration: &crdbv1beta1.MigrationStatus{Phase: PhasePodMigration},
		},
		{
			name:      "finalization before Mode write",
			mode:      mode(crdbv1beta1.Disabled),
			migration: &crdbv1beta1.MigrationStatus{Phase: PhaseFinalization},
			want:      true,
		},
		{
			name:      "finalization after Mode write",
			mode:      mode(crdbv1beta1.MutableOnly),
			migration: &crdbv1beta1.MigrationStatus{Phase: PhaseFinalization},
			want:      true,
		},
		{
			name:      "Complete phase before Mode write",
			mode:      mode(crdbv1beta1.Disabled),
			migration: &crdbv1beta1.MigrationStatus{Phase: PhaseComplete},
			want:      true,
		},
		{
			name:      "Complete phase before terminal label",
			mode:      mode(crdbv1beta1.MutableOnly),
			migration: &crdbv1beta1.MigrationStatus{Phase: PhaseComplete},
			want:      true,
		},
		{
			name:      "fully complete handoff",
			mode:      mode(crdbv1beta1.MutableOnly),
			migration: &crdbv1beta1.MigrationStatus{Phase: PhaseComplete},
			label:     MigrateComplete,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := &crdbv1beta1.CrdbCluster{}
			cluster.Spec.Mode = tt.mode
			cluster.Status.Migration = tt.migration
			if tt.label != "" {
				cluster.Labels = map[string]string{LabelMigrate: tt.label}
			}

			assert.Equal(t, tt.want, needsMigrationCompletionRecovery(cluster))
		})
	}
}

func TestMigrationHandoffMarkersChanged(t *testing.T) {
	cluster := func(mode crdbv1beta1.ReconciliationMode, phase, label string) *crdbv1beta1.CrdbCluster {
		return &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{LabelMigrate: label}},
			Spec:       crdbv1beta1.CrdbClusterSpec{Mode: &mode},
			Status: crdbv1beta1.CrdbClusterStatus{
				Migration: &crdbv1beta1.MigrationStatus{Phase: phase},
			},
		}
	}

	tests := []struct {
		name string
		old  *crdbv1beta1.CrdbCluster
		new  *crdbv1beta1.CrdbCluster
		want bool
	}{
		{
			name: "unrelated status update",
			old:  cluster(crdbv1beta1.Disabled, PhaseFinalization, MigrateInProgress),
			new:  cluster(crdbv1beta1.Disabled, PhaseFinalization, MigrateInProgress),
		},
		{
			name: "Mode persisted",
			old:  cluster(crdbv1beta1.Disabled, PhaseFinalization, MigrateInProgress),
			new:  cluster(crdbv1beta1.MutableOnly, PhaseFinalization, MigrateInProgress),
			want: true,
		},
		{
			name: "phase persisted",
			old:  cluster(crdbv1beta1.MutableOnly, PhaseFinalization, MigrateInProgress),
			new:  cluster(crdbv1beta1.MutableOnly, PhaseComplete, MigrateInProgress),
			want: true,
		},
		{
			name: "label persisted",
			old:  cluster(crdbv1beta1.MutableOnly, PhaseComplete, MigrateInProgress),
			new:  cluster(crdbv1beta1.MutableOnly, PhaseComplete, MigrateComplete),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, migrationHandoffMarkersChanged(tt.old, tt.new))
		})
	}
}

// TestCompleteMigration_RecordsPhaseCompleteAndSetsMutableOnly verifies that completeMigration
// persists PhaseComplete, updates the migration label, sets Mode=MutableOnly, and emits
// a MigrationComplete event.
func TestCompleteMigration_RecordsPhaseCompleteAndSetsMutableOnly(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	recorder := record.NewFakeRecorder(10)
	r := NewReconciler(ctl, recorder, migrationutil.DefaultRegion, false)

	cluster := createTestClusterWithMigration(PhaseFinalization, 3)
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

	// completeMigration expects a nil STS in the discovery result (STS is gone).
	discovery := &DiscoveryResult{
		Name:          testClusterName,
		MigrationType: MigrationTypeHelm,
	}

	result, err := r.completeMigration(ctx, ctl, discovery, cluster)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.False(t, result.Requeue)

	// PhaseComplete must be persisted via UpdateStatus.
	var fetched crdbv1beta1.CrdbCluster
	require.NoError(t, ctl.Get(ctx, testClusterName, &fetched))
	require.NotNil(t, fetched.Status.Migration)
	assert.Equal(t, PhaseComplete, fetched.Status.Migration.Phase)
	assert.Equal(t, "Migration complete.", fetched.Status.Migration.Message)

	// Mode=MutableOnly must be set on the spec.
	require.NotNil(t, fetched.Spec.Mode)
	assert.Equal(t, crdbv1beta1.MutableOnly, *fetched.Spec.Mode)

	// ClusterInitialized condition must be set so betaclusterctrl skips cockroach init.
	assert.True(t, fetched.IsInitialized(), "ClusterInitialized condition should be True")

	// Migration status label must be updated to complete.
	assert.Equal(t, MigrateComplete, fetched.Labels[LabelMigrationStatus])

	var pdb policyv1.PodDisruptionBudget
	require.NoError(t, ctl.Get(ctx, testClusterName+"-pdb", &pdb),
		"enterprise PDB must exist before migration completes")

	// A MigrationComplete event must have been emitted.
	select {
	case ev := <-recorder.Events:
		assert.Contains(t, ev, "MigrationComplete")
	default:
		t.Fatal("expected MigrationComplete event, got none")
	}
}

func TestCompleteMigration_OperatorResetsManagedFields(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	cluster := createTestClusterWithMigration(PhaseFinalization, 3)
	cluster.Status.Migration.Type = MigrationTypeOperator
	cluster.ManagedFields = []metav1.ManagedFieldsEntry{
		{
			Manager:    "public-operator",
			Operation:  metav1.ManagedFieldsOperationUpdate,
			APIVersion: crdbv1alpha1.SchemeGroupVersion.String(),
		},
	}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

	result, err := r.completeMigration(ctx, ctl, &DiscoveryResult{
		Name:          testClusterName,
		MigrationType: MigrationTypeOperator,
	}, cluster)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.False(t, result.Requeue)

	var fetched crdbv1beta1.CrdbCluster
	require.NoError(t, ctl.Get(ctx, testClusterName, &fetched))
	for _, entry := range fetched.ManagedFields {
		assert.NotEqual(t, crdbv1alpha1.SchemeGroupVersion.String(), entry.APIVersion)
	}
	require.NotNil(t, fetched.Spec.Mode)
	assert.Equal(t, crdbv1beta1.MutableOnly, *fetched.Spec.Mode)
	assert.Equal(t, PhaseComplete, fetched.Status.Migration.Phase)
	assert.Equal(t, MigrateComplete, fetched.Labels[LabelMigrate])
}

func TestCompleteMigration_OperatorRecoveryResetsLegacyManagedFieldsOnce(t *testing.T) {
	ctx := context.Background()
	cluster := createTestClusterWithMigration(PhaseComplete, 3)
	cluster.Status.Migration.Type = MigrationTypeOperator
	cluster.Status.SetCondition(crdbv1beta1.ClusterInitialized, metav1.ConditionTrue)
	mode := crdbv1beta1.MutableOnly
	cluster.Spec.Mode = &mode
	cluster.ManagedFields = []metav1.ManagedFieldsEntry{
		{
			Manager:    "public-operator",
			Operation:  metav1.ManagedFieldsOperationUpdate,
			APIVersion: crdbv1alpha1.SchemeGroupVersion.String(),
		},
	}

	resetAttempts := 0
	ctl := kubetest.NewFakeCtl(kubetest.Options{Options: kube.Options{
		Scheme: kube.Scheme,
		BeforeMutation: func(obj kube.Object) error {
			if c, ok := obj.(*crdbv1beta1.CrdbCluster); ok && hasV1Alpha1ManagedFields(c) {
				resetAttempts++
			}
			return nil
		},
	}}, cluster.DeepCopy()).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)
	discovery := &DiscoveryResult{Name: testClusterName, MigrationType: MigrationTypeOperator}

	result, err := r.completeMigration(ctx, ctl, discovery, cluster)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.False(t, result.Requeue)
	assert.Equal(t, 1, resetAttempts,
		"recovery must clear legacy ownership even when Mode is already MutableOnly")

	var fetched crdbv1beta1.CrdbCluster
	require.NoError(t, ctl.Get(ctx, testClusterName, &fetched))
	result, err = r.completeMigration(ctx, ctl, discovery, &fetched)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.False(t, result.Requeue)
	assert.Equal(t, 1, resetAttempts, "completion re-entry must not reset managed fields again")
}

func TestCompleteMigration_RecoversPhaseCompleteWithoutMutableOnly(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	recorder := record.NewFakeRecorder(10)
	r := NewReconciler(ctl, recorder, migrationutil.DefaultRegion, false)

	cluster := createTestClusterWithMigration(PhaseComplete, 3)
	mode := crdbv1beta1.Disabled
	cluster.Spec.Mode = &mode
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

	discovery := &DiscoveryResult{
		Name:          testClusterName,
		MigrationType: MigrationTypeHelm,
	}

	result, err := r.completeMigration(ctx, ctl, discovery, cluster)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.False(t, result.Requeue)

	var fetched crdbv1beta1.CrdbCluster
	require.NoError(t, ctl.Get(ctx, testClusterName, &fetched))
	require.NotNil(t, fetched.Spec.Mode)
	assert.Equal(t, crdbv1beta1.MutableOnly, *fetched.Spec.Mode, "Mode must be recovered to MutableOnly")
	require.NotNil(t, fetched.Status.Migration)
	assert.Equal(t, PhaseComplete, fetched.Status.Migration.Phase)
	assert.Equal(t, MigrateComplete, fetched.Labels[LabelMigrate])
}

// TestCompleteMigration_AlreadyMutableOnly_SkipsModeUpdate verifies that completeMigration
// does not issue an extra update when Mode=MutableOnly is already set.
func TestCompleteMigration_AlreadyMutableOnly_SkipsModeUpdate(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	recorder := record.NewFakeRecorder(10)
	r := NewReconciler(ctl, recorder, migrationutil.DefaultRegion, false)

	cluster := createTestClusterWithMigration(PhaseFinalization, 3)
	mode := crdbv1beta1.MutableOnly
	cluster.Spec.Mode = &mode
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

	discovery := &DiscoveryResult{
		Name:          testClusterName,
		MigrationType: MigrationTypeHelm,
	}

	result, err := r.completeMigration(ctx, ctl, discovery, cluster)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.False(t, result.Requeue)

	// PhaseComplete must still be recorded.
	var fetched crdbv1beta1.CrdbCluster
	require.NoError(t, ctl.Get(ctx, testClusterName, &fetched))
	require.NotNil(t, fetched.Status.Migration)
	assert.Equal(t, PhaseComplete, fetched.Status.Migration.Phase)

	// Mode should remain MutableOnly.
	require.NotNil(t, fetched.Spec.Mode)
	assert.Equal(t, crdbv1beta1.MutableOnly, *fetched.Spec.Mode)

	// ClusterInitialized condition must be set.
	assert.True(t, fetched.IsInitialized(), "ClusterInitialized condition should be True")

	// MigrationComplete event should still be emitted.
	select {
	case ev := <-recorder.Events:
		assert.Contains(t, ev, "MigrationComplete")
	default:
		t.Fatal("expected MigrationComplete event, got none")
	}
}

func TestCompleteMigration_HandoffFailureKeepsPhaseFinalization(t *testing.T) {
	tests := []struct {
		name              string
		beforeMutation    func(kube.Object) error
		advancedEmulation bool
		errorContains     string
	}{
		{
			name: "PDB apply rejected",
			beforeMutation: func(obj kube.Object) error {
				if _, ok := obj.(*policyv1.PodDisruptionBudget); ok {
					return errors.New("pdb apply failed")
				}
				return nil
			},
			errorContains: "failed to ensure PDB before migration handoff",
		},
		{
			name: "MutableOnly mode rejected",
			beforeMutation: func(obj kube.Object) error {
				cluster, ok := obj.(*crdbv1beta1.CrdbCluster)
				if ok && cluster.Spec.Mode != nil && *cluster.Spec.Mode == crdbv1beta1.MutableOnly {
					return errors.New("admission webhook denied the request: region mismatch")
				}
				return nil
			},
			advancedEmulation: true,
			errorContains:     "failed to set Mode=MutableOnly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctl := kubetest.NewFakeCtl(kubetest.Options{
				Options:           kube.Options{Scheme: kube.Scheme, BeforeMutation: tt.beforeMutation},
				AdvancedEmulation: tt.advancedEmulation,
			}).WithNamespace(testNamespace)
			r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

			cluster := createTestClusterWithMigration(PhaseFinalization, 3)
			mode := crdbv1beta1.Disabled
			cluster.Spec.Mode = &mode
			require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

			_, err := r.completeMigration(ctx, ctl, &DiscoveryResult{
				Name:          testClusterName,
				MigrationType: MigrationTypeHelm,
			}, cluster)
			require.ErrorContains(t, err, tt.errorContains)

			var fetched crdbv1beta1.CrdbCluster
			require.NoError(t, ctl.Get(ctx, testClusterName, &fetched))
			require.NotNil(t, fetched.Status.Migration)
			assert.Equal(t, PhaseFinalization, fetched.Status.Migration.Phase)
			require.NotNil(t, fetched.Spec.Mode)
			assert.Equal(t, crdbv1beta1.Disabled, *fetched.Spec.Mode)
			assert.NotEqual(t, MigrateComplete, fetched.Labels[LabelMigrate])
		})
	}
}

func TestReconcile_StrandedPhaseCompleteRecoversMode(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	recorder := record.NewFakeRecorder(10)
	r := NewReconciler(ctl, recorder, migrationutil.DefaultRegion, false)

	cluster := createTestClusterWithMigration(PhaseComplete, 3)
	mode := crdbv1beta1.Disabled
	cluster.Spec.Mode = &mode
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(cluster),
	})
	require.NoError(t, err)

	var fetched crdbv1beta1.CrdbCluster
	require.NoError(t, ctl.Get(ctx, testClusterName, &fetched))
	require.NotNil(t, fetched.Spec.Mode)
	assert.Equal(t, crdbv1beta1.MutableOnly, *fetched.Spec.Mode, "Reconcile must recover Mode to MutableOnly")
	assert.Equal(t, MigrateComplete, fetched.Labels[LabelMigrate])
}

// TestMutableOnlyIntercept_AlreadyComplete_NoRequeue verifies the Mode=MutableOnly
// intercept in Reconcile returns NoRequeue when PhaseComplete is already recorded.
func TestMutableOnlyIntercept_AlreadyComplete_NoRequeue(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	recorder := record.NewFakeRecorder(10)
	r := NewReconciler(ctl, recorder, migrationutil.DefaultRegion, false)

	cluster := createTestClusterWithMigration(PhaseComplete, 0)
	mode := crdbv1beta1.MutableOnly
	cluster.Spec.Mode = &mode
	cluster.Labels = map[string]string{
		LabelMigrate: MigrateComplete,
	}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(cluster),
	})
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.False(t, result.Requeue)

	// No MigrationComplete event should have been emitted since nothing changed.
	select {
	case ev := <-recorder.Events:
		t.Fatalf("unexpected event: %s", ev)
	default:
	}
}

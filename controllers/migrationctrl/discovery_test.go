package migrationctrl

import (
	"context"
	"testing"

	crdbv1alpha1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1alpha1"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/migrationutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
)

func TestPreferredMigrationLabelSource_NilResult(t *testing.T) {
	assert.Nil(t, preferredMigrationLabelSource(nil))
}

func TestPreferredMigrationLabelSource_OperatorPrefersV1Alpha1(t *testing.T) {
	v1alpha := &crdbv1alpha1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
	}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
	}
	result := &DiscoveryResult{
		MigrationType:   MigrationTypeOperator,
		V1Alpha1Cluster: v1alpha,
		StatefulSet:     sts,
	}
	obj := preferredMigrationLabelSource(result)
	assert.Equal(t, v1alpha, obj, "operator migration should prefer v1alpha1 cluster")
}

func TestPreferredMigrationLabelSource_HelmPrefersSTS(t *testing.T) {
	v1alpha := &crdbv1alpha1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
	}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
	}
	result := &DiscoveryResult{
		MigrationType:   MigrationTypeHelm,
		V1Alpha1Cluster: v1alpha,
		StatefulSet:     sts,
	}
	obj := preferredMigrationLabelSource(result)
	assert.Equal(t, sts, obj, "Helm migration should prefer StatefulSet")
}

func TestPreferredMigrationLabelSource_FallsBackToV1Alpha1WhenNoSTS(t *testing.T) {
	v1alpha := &crdbv1alpha1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
	}
	result := &DiscoveryResult{
		MigrationType:   MigrationTypeHelm,
		V1Alpha1Cluster: v1alpha,
	}
	obj := preferredMigrationLabelSource(result)
	assert.Equal(t, v1alpha, obj)
}

func TestMigrationResourceKind(t *testing.T) {
	assert.Equal(t, "crdbcluster", migrationResourceKind(MigrationTypeOperator))
	assert.Equal(t, "statefulset", migrationResourceKind(MigrationTypeHelm))
	assert.Equal(t, "statefulset", migrationResourceKind("unknown"))
}

func TestDiscovery_OperatorMigration_OwnerRef(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	// Create STS owned by v1alpha1 CrdbCluster
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testClusterName,
			Namespace:  testNamespace,
			Generation: 1,
			Labels:     map[string]string{"app.kubernetes.io/name": "cockroachdb"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "crdb.cockroachlabs.com/v1alpha1",
				Kind:       "CrdbCluster",
				Name:       testClusterName,
			}},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: new(int32(3)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "db"}}},
			},
		},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, Replicas: 3, ReadyReplicas: 3},
	}
	require.NoError(t, ctl.Create(ctx, sts))

	// Create v1alpha1 with skip-reconcile
	v1alpha := &crdbv1alpha1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testClusterName,
			Namespace:  testNamespace,
			Generation: 1,
			Labels:     map[string]string{LabelSkipReconcile: "true"},
		},
		Status: crdbv1alpha1.CrdbClusterStatus{
			Conditions: []crdbv1alpha1.ClusterCondition{
				{Type: crdbv1alpha1.CrdbInitializedCondition, Status: metav1.ConditionTrue},
			},
		},
	}
	require.NoError(t, ctl.Create(ctx, v1alpha))

	result, err := r.discoverMigrationResources(ctx, ctl, testClusterName)
	require.NoError(t, err)
	assert.Equal(t, MigrationTypeOperator, result.MigrationType)
	assert.NotNil(t, result.V1Alpha1Cluster)
	assert.NotNil(t, result.StatefulSet)
}

// TestDiscovery_OperatorMigration_MissingSkipReconcile verifies that operator
// migrations fail fast when the skip-reconcile label is not set.
func TestDiscovery_OperatorMigration_MissingSkipReconcile(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testClusterName,
			Namespace:  testNamespace,
			Generation: 1,
			Labels:     map[string]string{"app.kubernetes.io/name": "cockroachdb"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "crdb.cockroachlabs.com/v1alpha1",
				Kind:       "CrdbCluster",
				Name:       testClusterName,
			}},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: new(int32(3)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "db"}}},
			},
		},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, Replicas: 3, ReadyReplicas: 3},
	}
	require.NoError(t, ctl.Create(ctx, sts))

	// v1alpha1 WITHOUT skip-reconcile label
	v1alpha := &crdbv1alpha1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: testClusterName, Namespace: testNamespace, Generation: 1,
		},
		Status: crdbv1alpha1.CrdbClusterStatus{
			Conditions: []crdbv1alpha1.ClusterCondition{
				{Type: crdbv1alpha1.CrdbInitializedCondition, Status: metav1.ConditionTrue},
			},
		},
	}
	require.NoError(t, ctl.Create(ctx, v1alpha))

	_, err := r.discoverMigrationResources(ctx, ctl, testClusterName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "skip-reconcile")
}

// TestDiscovery_OriginalReplicas_WithExistingNodes verifies that OriginalReplicas
// accounts for both existing CrdbNodes and current STS replicas during Helm migration.
func TestDiscovery_OriginalReplicas_WithExistingNodes(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	// STS with 2 replicas (scaled down from 3 during migration)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: testClusterName, Namespace: testNamespace, Generation: 1,
			Labels: map[string]string{"app.kubernetes.io/name": "cockroachdb"},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: new(int32(2)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "db"}}},
			},
		},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, Replicas: 2, ReadyReplicas: 2},
	}
	require.NoError(t, ctl.Create(ctx, sts))

	// 1 existing CrdbNode (already migrated)
	node := &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: testClusterName + "-2", Namespace: testNamespace,
			Labels: map[string]string{
				"crdb.cockroachlabs.com/cluster": testClusterName,
			},
		},
	}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, node))

	result, err := r.discoverMigrationResources(ctx, ctl, testClusterName)
	require.NoError(t, err)
	assert.Equal(t, int32(3), result.OriginalReplicas, "should be existingNodes(1) + STSReplicas(2)")
	assert.Equal(t, int32(2), result.Replicas)
}

// TestUpdateLabelValue_AlreadySet verifies that updateLabelValue is a no-op
// when the label already has the desired value.
func TestUpdateLabelValue_AlreadySet(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	sts := createTestStatefulSet(testClusterName, testNamespace, 3)
	sts.Labels[LabelMigrate] = MigrateInProgress
	require.NoError(t, ctl.Create(ctx, sts))

	result := &DiscoveryResult{
		Name:          testClusterName,
		Namespace:     testNamespace,
		MigrationType: MigrationTypeHelm,
		StatefulSet:   sts,
	}

	err := updateLabelValue(ctx, ctl, result, MigrateInProgress)
	require.NoError(t, err)

	// Label should still be in-progress
	var fetched appsv1.StatefulSet
	require.NoError(t, ctl.Get(ctx, testClusterName, &fetched))
	assert.Equal(t, MigrateInProgress, fetched.Labels[LabelMigrate])
}

// TestRemoveMigrationLabel_NotFoundIsNoop verifies that removing the label
// from a non-existent resource succeeds silently.
func TestRemoveMigrationLabel_NotFoundIsNoop(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	result := &DiscoveryResult{
		Name:          testClusterName,
		Namespace:     testNamespace,
		MigrationType: MigrationTypeHelm,
		StatefulSet:   &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace}},
	}

	// STS doesn't exist in the API, so refreshPreferredMigrationLabelSource will return NotFound
	err := removeMigrationLabel(ctx, ctl, result)
	require.NoError(t, err, "removing label from non-existent resource should not error")
}

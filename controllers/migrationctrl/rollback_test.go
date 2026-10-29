package migrationctrl

import (
	"context"
	"fmt"
	"testing"

	crdbv1alpha1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1alpha1"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/migrationutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
)

func TestRollback_StatusSyncBeforeRollback(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	// Create StatefulSet
	sts := createTestStatefulSet(testClusterName, testNamespace, 3)
	require.NoError(t, ctl.Create(ctx, sts))

	// Create 2 CrdbNodes (actual state)
	for i := range 2 {
		node := &crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("test-cluster-%d", i),
				Namespace: testNamespace,
				Labels: map[string]string{
					manifests.BetaCrdbClusterNodeOwnerLabelKey: testClusterName,
				},
			},
		}
		require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, node))
	}

	// Create cluster with status showing only 1 migrated (stale)
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
		Status: crdbv1beta1.CrdbClusterStatus{
			Migration: &crdbv1beta1.MigrationStatus{
				Phase:         PhaseRollbackInProgress,
				Type:          MigrationTypeHelm,
				TotalPods:     3,
				MigratedNodes: []string{"test-cluster-0"}, // Only 1, but 2 actually exist
			},
		},
	}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

	discovery := &DiscoveryResult{
		Name:          testClusterName,
		Namespace:     testNamespace,
		MigrationType: MigrationTypeHelm,
		StatefulSet:   sts,
	}

	// First call should sync status and requeue
	result, err := r.handleRollback(ctx, ctl, discovery, cluster)
	require.NoError(t, err)

	// Get updated cluster
	updatedCluster := &crdbv1beta1.CrdbCluster{}
	require.NoError(t, ctl.Get(ctx, testClusterName, updatedCluster))

	// Status should now reflect actual 2 nodes
	assert.Len(t, updatedCluster.Status.Migration.MigratedNodes, 2)
	assert.True(t, result.Requeue)
}

func TestRollback_CannotRollbackAfterComplete(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
		Status: crdbv1beta1.CrdbClusterStatus{
			Migration: &crdbv1beta1.MigrationStatus{
				Phase:         PhaseComplete,
				Type:          MigrationTypeHelm,
				TotalPods:     3,
				MigratedNodes: []string{"test-cluster-0", "test-cluster-1", "test-cluster-2"},
			},
		},
	}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

	discovery := &DiscoveryResult{
		Name:          testClusterName,
		Namespace:     testNamespace,
		MigrationType: MigrationTypeHelm,
	}

	// handleNoLabel should recognize PhaseComplete and not attempt rollback
	result, err := r.handleNoLabel(ctx, ctl, discovery)
	require.NoError(t, err)

	// Should not requeue - complete migrations stay complete
	assert.False(t, result.Requeue)
}

func TestRollback_EmptyMigratedNodesListButNodesExist(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	// Create StatefulSet
	sts := createTestStatefulSet(testClusterName, testNamespace, 3)
	require.NoError(t, ctl.Create(ctx, sts))

	// Create 2 orphaned CrdbNodes (exist but not in status)
	for i := range 2 {
		node := &crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("test-cluster-%d", i),
				Namespace: testNamespace,
				Labels: map[string]string{
					manifests.BetaCrdbClusterNodeOwnerLabelKey: testClusterName,
				},
			},
		}
		require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, node))
	}

	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
		Status: crdbv1beta1.CrdbClusterStatus{
			Migration: &crdbv1beta1.MigrationStatus{
				Phase:         PhaseRollbackInProgress,
				Type:          MigrationTypeHelm,
				TotalPods:     3,
				MigratedNodes: []string{}, // Empty, but nodes exist
			},
		},
	}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

	discovery := &DiscoveryResult{
		Name:          testClusterName,
		Namespace:     testNamespace,
		MigrationType: MigrationTypeHelm,
		StatefulSet:   sts,
	}

	// First call should discover orphaned nodes and sync status
	result, err := r.handleRollback(ctx, ctl, discovery, cluster)
	require.NoError(t, err)

	updatedCluster := &crdbv1beta1.CrdbCluster{}
	require.NoError(t, ctl.Get(ctx, testClusterName, updatedCluster))

	// Status should now reflect actual 2 nodes
	assert.Len(t, updatedCluster.Status.Migration.MigratedNodes, 2)
	assert.True(t, result.Requeue)
}

func TestDiscoverActualMigratedNodes(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	// Create nodes for different clusters
	nodes := []struct {
		name    string
		cluster string
	}{
		{"cluster-a-0", "cluster-a"},
		{"cluster-a-1", "cluster-a"},
		{"cluster-b-0", "cluster-b"},
		{"unrelated-0", "other"},
	}

	for _, n := range nodes {
		node := &crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      n.name,
				Namespace: testNamespace,
				Labels: map[string]string{
					manifests.BetaCrdbClusterNodeOwnerLabelKey: n.cluster,
				},
			},
		}
		require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, node))
	}

	// Should only find cluster-a nodes
	found, err := discoverActualMigratedNodes(ctx, ctl, "cluster-a")
	require.NoError(t, err)
	assert.Len(t, found, 2)
	assert.Contains(t, found, "cluster-a-0")
	assert.Contains(t, found, "cluster-a-1")
}

func TestListClusterCrdbNodes(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	// Create nodes for test-cluster
	for i := range 3 {
		node := &crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("test-cluster-%d", i),
				Namespace: testNamespace,
				Labels: map[string]string{
					manifests.BetaCrdbClusterNodeOwnerLabelKey: testClusterName,
				},
			},
		}
		require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, node))
	}

	// Create node for another cluster
	otherNode := &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-cluster-0",
			Namespace: testNamespace,
			Labels: map[string]string{
				manifests.BetaCrdbClusterNodeOwnerLabelKey: "other-cluster",
			},
		},
	}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, otherNode))

	nodes, err := listClusterCrdbNodes(ctx, ctl, testClusterName)
	require.NoError(t, err)
	assert.Len(t, nodes, 3)

	// Verify all returned nodes belong to test-cluster
	for _, node := range nodes {
		assert.Equal(t, testClusterName, node.Labels[manifests.BetaCrdbClusterNodeOwnerLabelKey])
	}
}

func TestIsOwnedByV1Alpha1Cluster(t *testing.T) {
	tests := []struct {
		name     string
		sts      *appsv1.StatefulSet
		expected bool
	}{
		{
			name: "owned by v1alpha1 CrdbCluster",
			sts: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: testClusterName,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "crdb.cockroachlabs.com/v1alpha1",
							Kind:       "CrdbCluster",
							Name:       testClusterName,
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "owned by v1beta1 CrdbCluster",
			sts: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: testClusterName,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "crdb.cockroachlabs.com/v1beta1",
							Kind:       "CrdbCluster",
							Name:       testClusterName,
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "no owner references - Helm deployment",
			sts: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            testClusterName,
					OwnerReferences: nil,
				},
			},
			expected: false,
		},
		{
			name: "owned by something else",
			sts: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: testClusterName,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "apps/v1",
							Kind:       "Deployment",
							Name:       "something",
						},
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isOwnedByV1Alpha1Cluster(tt.sts)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestRollback_HelmDeletesCrdbCluster verifies that Helm rollback deletes the
// migration-created CrdbCluster instead of setting Mode=Disabled.
func TestRollback_HelmDeletesCrdbCluster(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	// Create StatefulSet with replicas already at original count
	sts := createTestStatefulSet(testClusterName, testNamespace, 3)
	require.NoError(t, ctl.Create(ctx, sts))

	// Create the minimal CrdbCluster that getOrCreateCluster would have created
	mode := crdbv1beta1.Disabled
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testClusterName,
			Namespace: testNamespace,
			Labels: map[string]string{
				LabelMigrationStatus: MigrateInProgress,
			},
		},
		Spec: crdbv1beta1.CrdbClusterSpec{Mode: &mode},
		Status: crdbv1beta1.CrdbClusterStatus{
			Migration: &crdbv1beta1.MigrationStatus{
				Phase:         PhaseRollbackInProgress,
				Type:          MigrationTypeHelm,
				TotalPods:     3,
				MigratedNodes: []string{},
			},
		},
	}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

	// Create the headless service (required by validateAndClearRollback)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
		Spec:       corev1.ServiceSpec{ClusterIP: "None"},
	}
	require.NoError(t, ctl.Create(ctx, svc))

	discovery := &DiscoveryResult{
		Name:          testClusterName,
		Namespace:     testNamespace,
		MigrationType: MigrationTypeHelm,
		StatefulSet:   sts,
	}

	_, err := r.handleRollback(ctx, ctl, discovery, cluster)
	require.NoError(t, err)

	// Verify CrdbCluster was deleted
	var fetched crdbv1beta1.CrdbCluster
	err = ctl.Get(ctx, testClusterName, &fetched)
	require.True(t, apierrors.IsNotFound(err), "CrdbCluster should be deleted after Helm rollback")
}

// TestRollback_CleansUpCAConfigMap verifies that rollback deletes the CA ConfigMap
// created by ensureCerts.
func TestRollback_CleansUpCAConfigMap(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	// Create the CA ConfigMap that ensureCerts would create
	caCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster-ca-crt",
			Namespace: testNamespace,
		},
		Data: map[string]string{"ca.crt": "fake-ca"},
	}
	require.NoError(t, ctl.Create(ctx, caCM))

	// Verify it exists before cleanup
	var fetched corev1.ConfigMap
	require.NoError(t, ctl.Get(ctx, "test-cluster-ca-crt", &fetched))

	// Call the cleanup function directly
	err := deleteMigrationCertResources(ctx, ctl, testClusterName, MigrationTypeHelm)
	require.NoError(t, err)

	// Verify CA ConfigMap was deleted
	err = ctl.Get(ctx, "test-cluster-ca-crt", &fetched)
	require.True(t, apierrors.IsNotFound(err), "CA ConfigMap should be deleted")
}

// TestRollback_CleansUpOperatorSecrets verifies that operator migration rollback
// also deletes node-secret and client-secret (but Helm rollback does not).
func TestRollback_CleansUpOperatorSecrets(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	// Create resources that would exist after cert migration
	for _, name := range []string{"test-cluster-ca-crt"} {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Data:       map[string]string{"ca.crt": "fake"},
		}
		require.NoError(t, ctl.Create(ctx, cm))
	}
	for _, name := range []string{"test-cluster-node-secret", "test-cluster-client-secret"} {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Data:       map[string][]byte{"tls.crt": []byte("fake")},
		}
		require.NoError(t, ctl.Create(ctx, secret))
	}

	// Operator migration cleanup should delete all three
	err := deleteMigrationCertResources(ctx, ctl, testClusterName, MigrationTypeOperator)
	require.NoError(t, err)

	var cm corev1.ConfigMap
	assert.True(t, apierrors.IsNotFound(ctl.Get(ctx, "test-cluster-ca-crt", &cm)))

	var secret corev1.Secret
	assert.True(t, apierrors.IsNotFound(ctl.Get(ctx, "test-cluster-node-secret", &secret)))
	assert.True(t, apierrors.IsNotFound(ctl.Get(ctx, "test-cluster-client-secret", &secret)))
}

// TestRollback_HelmPreservesSecrets verifies that Helm rollback does NOT delete
// node-secret and client-secret (they are the originals, not migration-created).
func TestRollback_HelmPreservesSecrets(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	for _, name := range []string{"test-cluster-node-secret", "test-cluster-client-secret"} {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Data:       map[string][]byte{"tls.crt": []byte("original")},
		}
		require.NoError(t, ctl.Create(ctx, secret))
	}

	err := deleteMigrationCertResources(ctx, ctl, testClusterName, MigrationTypeHelm)
	require.NoError(t, err)

	// Secrets should still exist for Helm migrations
	var secret corev1.Secret
	assert.NoError(t, ctl.Get(ctx, "test-cluster-node-secret", &secret))
	assert.NoError(t, ctl.Get(ctx, "test-cluster-client-secret", &secret))
}

func TestDeleteAllPodDisruptionBudgets_DeletesEnterprisePDB(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster-pdb",
			Namespace: testNamespace,
		},
	}
	require.NoError(t, ctl.Create(ctx, pdb))

	// deleteAllPodDisruptionBudgets is used during rollback and deletes all
	// formats including the enterprise PDB ({name}-pdb).
	require.NoError(t, deleteAllPodDisruptionBudgets(ctx, ctl, testClusterName))

	var fetched policyv1.PodDisruptionBudget
	assert.True(t, apierrors.IsNotFound(ctl.Get(ctx, "test-cluster-pdb", &fetched)))
}

func TestDeletePodDisruptionBudget_SkipsEnterprisePDB(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	// deletePodDisruptionBudget is used during finalization and must not
	// delete the enterprise PDB since it's created immediately after.
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster-pdb",
			Namespace: testNamespace,
		},
	}
	require.NoError(t, ctl.Create(ctx, pdb))

	require.NoError(t, deletePodDisruptionBudget(ctx, ctl, testClusterName))

	var fetched policyv1.PodDisruptionBudget
	assert.NoError(t, ctl.Get(ctx, "test-cluster-pdb", &fetched),
		"enterprise PDB should survive finalization cleanup")
}

// TestRollback_CleansUpClusterScopedRBAC verifies that rollback deletes
// namespace-qualified ClusterRole and ClusterRoleBinding.
func TestRollback_CleansUpClusterScopedRBAC(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	crName := fmt.Sprintf("default-%s", testClusterName)

	// Create namespace-qualified ClusterRole
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: crName},
		Rules:      []rbacv1.PolicyRule{{Verbs: []string{"get"}, Resources: []string{"nodes"}, APIGroups: []string{""}}},
	}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cr))

	// Create namespace-qualified ClusterRoleBinding
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: crName},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: crName},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: testClusterName, Namespace: testNamespace}},
	}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, crb))

	// Delete them
	deleteMigrationClusterRBAC(ctx, ctl, testClusterName)

	// Verify both are deleted
	var fetchedCR rbacv1.ClusterRole
	assert.True(t, apierrors.IsNotFound(ctl.Get(ctx, crName, &fetchedCR)))

	var fetchedCRB rbacv1.ClusterRoleBinding
	assert.True(t, apierrors.IsNotFound(ctl.Get(ctx, crName, &fetchedCRB)))
}

// TestRollback_OperatorRemovesSkipReconcileLast verifies that for operator migrations,
// the skip-reconcile label is removed in validateAndClearRollback (the last step),
// not during the main rollback flow.
func TestRollback_OperatorRemovesSkipReconcileLast(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	// Create v1alpha1 CrdbCluster with skip-reconcile label
	v1alpha1Cluster := &crdbv1alpha1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testClusterName,
			Namespace: testNamespace,
			Labels: map[string]string{
				LabelSkipReconcile: "true",
			},
		},
	}
	require.NoError(t, ctl.Create(ctx, v1alpha1Cluster))

	// Create STS with ready replicas matching original
	sts := createTestStatefulSet(testClusterName, testNamespace, 3)
	require.NoError(t, ctl.Create(ctx, sts))

	// Create Service (needed for validation)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
		Spec:       corev1.ServiceSpec{ClusterIP: "None"},
	}
	require.NoError(t, ctl.Create(ctx, svc))

	// Create v1beta1 CrdbCluster in RollbackComplete phase
	mode := crdbv1beta1.Disabled
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
		Spec:       crdbv1beta1.CrdbClusterSpec{Mode: &mode},
		Status: crdbv1beta1.CrdbClusterStatus{
			Migration: &crdbv1beta1.MigrationStatus{
				Phase:         PhaseRollbackComplete,
				Type:          MigrationTypeOperator,
				TotalPods:     3,
				MigratedNodes: []string{},
			},
		},
	}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

	discovery := &DiscoveryResult{
		Name:            testClusterName,
		Namespace:       testNamespace,
		MigrationType:   MigrationTypeOperator,
		StatefulSet:     sts,
		V1Alpha1Cluster: v1alpha1Cluster,
	}

	// Call handleRollback, which should delegate to validateAndClearRollback
	_, err := r.handleRollback(ctx, ctl, discovery, cluster)
	require.NoError(t, err)

	// Verify skip-reconcile label was removed from v1alpha1 CrdbCluster
	var updatedV1Alpha1 crdbv1alpha1.CrdbCluster
	require.NoError(t, ctl.Get(ctx, testClusterName, &updatedV1Alpha1))
	_, hasLabel := updatedV1Alpha1.Labels[LabelSkipReconcile]
	assert.False(t, hasLabel, "skip-reconcile label should be removed after validation")

	// Verify migration status is preserved as audit trail (never cleared).
	var updatedCluster crdbv1beta1.CrdbCluster
	require.NoError(t, ctl.Get(ctx, testClusterName, &updatedCluster))
	require.NotNil(t, updatedCluster.Status.Migration, "migration status should be preserved as audit trail")
	assert.Equal(t, PhaseRollbackComplete, updatedCluster.Status.Migration.Phase)
	assert.Contains(t, updatedCluster.Status.Migration.Message, "Rollback validated")
}

// TestRemoveSkipReconcileLabel tests the removeSkipReconcileLabel helper directly.
func TestRemoveSkipReconcileLabel(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	v1alpha1Cluster := &crdbv1alpha1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testClusterName,
			Namespace: testNamespace,
			Labels: map[string]string{
				LabelSkipReconcile: "true",
				"app":              "cockroachdb",
			},
		},
	}
	require.NoError(t, ctl.Create(ctx, v1alpha1Cluster))

	discovery := &DiscoveryResult{
		Name:            testClusterName,
		Namespace:       testNamespace,
		V1Alpha1Cluster: v1alpha1Cluster,
	}

	err := removeSkipReconcileLabel(ctx, ctl, discovery)
	require.NoError(t, err)

	// Verify label is removed but other labels remain
	var updated crdbv1alpha1.CrdbCluster
	require.NoError(t, ctl.Get(ctx, testClusterName, &updated))
	_, hasSkipReconcile := updated.Labels[LabelSkipReconcile]
	assert.False(t, hasSkipReconcile)
	assert.Equal(t, "cockroachdb", updated.Labels["app"])
}

// TestRemoveSkipReconcileLabel_NilDiscovery verifies no-op for nil v1alpha1 cluster.
func TestRemoveSkipReconcileLabel_NilDiscovery(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	discovery := &DiscoveryResult{
		Name:            testClusterName,
		Namespace:       testNamespace,
		V1Alpha1Cluster: nil,
	}

	err := removeSkipReconcileLabel(ctx, ctl, discovery)
	require.NoError(t, err)
}

// TestRollback_GapScenario_SkipsHealthCheckWhenTargetMissing verifies that rollback
// scales the STS up without running a health check when the target CrdbNode was never
// created. This happens when the operator stops between scaling the STS down and
// creating the CrdbNode during forward migration.
func TestRollback_GapScenario_SkipsHealthCheckWhenTargetMissing(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	// 3-node cluster where forward migration scaled STS to 1 replica and created
	// CrdbNode-2, but the operator stopped before creating CrdbNode-1.
	sts := createTestStatefulSet(testClusterName, testNamespace, 1)
	require.NoError(t, ctl.Create(ctx, sts))

	node2 := &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-2", testClusterName),
			Namespace: testNamespace,
			Labels: map[string]string{
				manifests.BetaCrdbClusterNodeOwnerLabelKey: testClusterName,
			},
		},
	}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, node2))

	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
		Status: crdbv1beta1.CrdbClusterStatus{
			Migration: &crdbv1beta1.MigrationStatus{
				Phase: PhaseRollbackInProgress,
				Type:  MigrationTypeHelm,
				// The sync step in handleRollback discovers only CrdbNode-2, so
				// MigratedNodes should match that to avoid a sync-triggered requeue.
				TotalPods:     3,
				MigratedNodes: []string{fmt.Sprintf("%s-2", testClusterName)},
			},
		},
	}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

	discovery := &DiscoveryResult{
		Name:          testClusterName,
		Namespace:     testNamespace,
		MigrationType: MigrationTypeHelm,
		StatefulSet:   sts,
	}

	result, err := r.handleRollback(ctx, ctl, discovery, cluster)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter, "should requeue to wait for new STS pod")

	var updatedSTS appsv1.StatefulSet
	require.NoError(t, ctl.Get(ctx, testClusterName, &updatedSTS))
	require.NotNil(t, updatedSTS.Spec.Replicas)
	assert.Equal(t, int32(2), *updatedSTS.Spec.Replicas,
		"STS should be scaled up to restore the missing pod")
}

// TestRollback_GapScenario_TargetExistsDoesHealthCheck verifies that when the
// target CrdbNode exists, rollbackNextNode runs the health check before draining
// and deleting. This is the normal rollback path where no gap occurred.
func TestRollback_GapScenario_TargetExistsDoesHealthCheck(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	// STS has 1 replica and CrdbNode-1 exists, so the health check path runs.
	sts := createTestStatefulSet(testClusterName, testNamespace, 1)
	require.NoError(t, ctl.Create(ctx, sts))

	node1 := &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-1", testClusterName),
			Namespace: testNamespace,
			Labels: map[string]string{
				manifests.BetaCrdbClusterNodeOwnerLabelKey: testClusterName,
			},
		},
	}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, node1))

	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
		Status: crdbv1beta1.CrdbClusterStatus{
			Migration: &crdbv1beta1.MigrationStatus{
				Phase:         PhaseRollbackInProgress,
				Type:          MigrationTypeHelm,
				TotalPods:     3,
				MigratedNodes: []string{fmt.Sprintf("%s-1", testClusterName)},
			},
		},
	}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

	discovery := &DiscoveryResult{
		Name:          testClusterName,
		Namespace:     testNamespace,
		MigrationType: MigrationTypeHelm,
		StatefulSet:   sts,
	}

	// The health check fails because SQL exec is not available in the fake client.
	// We expect a requeue with an error, without scaling the STS.
	result, err := r.handleRollback(ctx, ctl, discovery, cluster)
	require.Error(t, err, "health check should fail in unit test")
	assert.NotZero(t, result.RequeueAfter, "should requeue due to health check failure")

	// STS should stay at 1 replica because the health check did not pass.
	var updatedSTS appsv1.StatefulSet
	require.NoError(t, ctl.Get(ctx, testClusterName, &updatedSTS))
	require.NotNil(t, updatedSTS.Spec.Replicas)
	assert.Equal(t, int32(1), *updatedSTS.Spec.Replicas,
		"STS should not be scaled up when health check fails")
}

// TestCheckRollbackHealth verifies that the health check returns an error when
// the migrated nodes list contains a node that no longer exists, forcing a
// requeue so the reconciler refreshes state before proceeding.
func TestCheckRollbackHealth(t *testing.T) {
	tests := []struct {
		name          string
		migratedNodes []string
		createNodes   []string
		wantErr       bool
		wantHealthy   bool
	}{
		{
			name:          "empty migrated nodes list is healthy",
			migratedNodes: nil,
			wantHealthy:   true,
		},
		{
			name:          "stale node triggers error",
			migratedNodes: []string{"test-cluster-0", "test-cluster-1"},
			createNodes:   []string{"test-cluster-0"},
			wantErr:       true,
		},
		{
			name:          "first node missing triggers error immediately",
			migratedNodes: []string{"test-cluster-0"},
			createNodes:   nil,
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctl := kubetest.NewFakeCtl(kubetest.Options{
				Options: kube.Options{Scheme: kube.Scheme},
			}).WithNamespace(testNamespace)
			r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

			for _, name := range tt.createNodes {
				node := &crdbv1beta1.CrdbNode{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: testNamespace,
						Labels: map[string]string{
							manifests.BetaCrdbClusterNodeOwnerLabelKey: testClusterName,
						},
					},
				}
				require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, node))
			}

			migration := &crdbv1beta1.MigrationStatus{
				Phase:         PhaseRollbackInProgress,
				Type:          MigrationTypeHelm,
				TotalPods:     3,
				MigratedNodes: tt.migratedNodes,
			}

			healthy, err := r.checkRollbackHealth(ctx, ctl, migration, 3)
			if tt.wantErr {
				require.Error(t, err, "stale migrated nodes list should return error")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantHealthy, healthy)
		})
	}
}

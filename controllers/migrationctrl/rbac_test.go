package migrationctrl

import (
	"context"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	policyv1 "k8s.io/api/policy/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// TestEnsurePriorityClass_CreatesNew verifies that ensurePriorityClass creates
// the CockroachDB priority class when it does not already exist.
func TestEnsurePriorityClass_CreatesNew(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	require.NoError(t, ensurePriorityClass(ctx, ctl))

	var pc schedulingv1.PriorityClass
	require.NoError(t, ctl.Get(ctx, manifests.CRDBCritical, &pc))
	assert.Equal(t, int32(manifests.CriticalPriority), pc.Value)
	assert.False(t, pc.GlobalDefault)
}

// TestEnsurePriorityClass_AlreadyExists verifies that ensurePriorityClass is
// idempotent and does not error when the PriorityClass already exists.
func TestEnsurePriorityClass_AlreadyExists(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	existing := &schedulingv1.PriorityClass{
		ObjectMeta: metav1.ObjectMeta{Name: manifests.CRDBCritical},
		Value:      manifests.CriticalPriority,
	}
	require.NoError(t, ctl.Create(ctx, existing))

	require.NoError(t, ensurePriorityClass(ctx, ctl))
}

// TestDeletePodDisruptionBudget_DeletesOldFormats verifies that
// deletePodDisruptionBudget removes public operator and Helm PDB naming
// conventions but preserves the enterprise operator's PDB.
func TestDeletePodDisruptionBudget_DeletesOldFormats(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	minAvail := intstr.FromInt(1)

	publicPDB := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
		Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: &minAvail},
	}
	helmPDB := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName + "-budget", Namespace: testNamespace},
		Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: &minAvail},
	}
	// Enterprise PDB should NOT be deleted by deletePodDisruptionBudget.
	enterprisePDB := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName + "-pdb", Namespace: testNamespace},
		Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: &minAvail},
	}
	require.NoError(t, ctl.Create(ctx, publicPDB))
	require.NoError(t, ctl.Create(ctx, helmPDB))
	require.NoError(t, ctl.Create(ctx, enterprisePDB))

	require.NoError(t, deletePodDisruptionBudget(ctx, ctl, testClusterName))

	var pdb policyv1.PodDisruptionBudget
	assert.Error(t, ctl.Get(ctx, testClusterName, &pdb), "public PDB should be deleted")
	assert.Error(t, ctl.Get(ctx, testClusterName+"-budget", &pdb), "Helm PDB should be deleted")
	assert.NoError(t, ctl.Get(ctx, testClusterName+"-pdb", &pdb), "enterprise PDB should be preserved")
}

// TestDeleteAllPodDisruptionBudgets_IncludesEnterprise verifies that
// deleteAllPodDisruptionBudgets also removes the enterprise PDB format,
// which is needed during rollback when all migration artifacts are cleaned up.
func TestDeleteAllPodDisruptionBudgets_IncludesEnterprise(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	minAvail := intstr.FromInt(1)

	enterprisePDB := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName + "-pdb", Namespace: testNamespace},
		Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: &minAvail},
	}
	require.NoError(t, ctl.Create(ctx, enterprisePDB))

	require.NoError(t, deleteAllPodDisruptionBudgets(ctx, ctl, testClusterName))

	var pdb policyv1.PodDisruptionBudget
	assert.Error(t, ctl.Get(ctx, testClusterName+"-pdb", &pdb), "enterprise PDB should be deleted during rollback")
}

// TestDeletePodDisruptionBudget_NoneExist verifies that deleting PDBs when none
// exist is a no-op.
func TestDeletePodDisruptionBudget_NoneExist(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	err := deletePodDisruptionBudget(ctx, ctl, testClusterName)
	require.NoError(t, err, "deleting non-existent PDBs should not error")
}

package migrationctrl

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
)

const (
	testClusterName = "test-cluster"
	testNamespace   = "default"
)

func TestInitPhase_AutoRecoversWhenCrdbNodesExist(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	// Create a StatefulSet.
	sts := createTestStatefulSet(testClusterName, testNamespace, 3)
	require.NoError(t, ctl.Create(ctx, sts))

	// Create an existing CrdbNode (simulating a previous interrupted migration).
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true},
		createTestCrdbNode(testClusterName, testNamespace, 0)))

	cluster := createTestClusterWithMigration(PhaseInit, 0)

	discovery := &DiscoveryResult{
		Name:             testClusterName,
		Namespace:        testNamespace,
		MigrationType:    MigrationTypeHelm,
		StatefulSet:      sts,
		Replicas:         3,
		OriginalReplicas: 3,
	}

	_, err := r.handleInitPhase(ctx, ctl, discovery, cluster)
	require.NoError(t, err) // Auto-recovers instead of erroring
	// The controller auto-recovers by syncing MigratedNodes and jumping to CertMigration.
	assert.Equal(t, PhaseCertMigration, cluster.Status.Migration.Phase)
	assert.Contains(t, cluster.Status.Migration.Message, "Auto-recovered")
}

func TestInitPhase_SetsTotalPods(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	replicas := int32(5)
	sts := createTestStatefulSet(testClusterName, testNamespace, replicas)
	require.NoError(t, ctl.Create(ctx, sts))

	cluster := createTestClusterWithMigration(PhaseInit, 0)

	discovery := &DiscoveryResult{
		Name:             testClusterName,
		Namespace:        testNamespace,
		MigrationType:    MigrationTypeHelm,
		StatefulSet:      sts,
		Replicas:         replicas,
		OriginalReplicas: replicas,
	}

	_, err := r.handleInitPhase(ctx, ctl, discovery, cluster)
	require.NoError(t, err)

	assert.Equal(t, replicas, cluster.Status.Migration.TotalPods)
	assert.Equal(t, replicas-1, cluster.Status.Migration.CurrentPodIndex,
		"migration should start from the highest pod index")
	assert.Equal(t, PhaseCertMigration, cluster.Status.Migration.Phase)
}

func TestCertMigrationPhase_FailsIfTotalPodsZero(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	sts := createTestStatefulSet(testClusterName, testNamespace, 3)
	require.NoError(t, ctl.Create(ctx, sts))

	cluster := createTestClusterWithMigration(PhaseCertMigration, 0)

	discovery := &DiscoveryResult{
		Name:          testClusterName,
		Namespace:     testNamespace,
		MigrationType: MigrationTypeHelm,
		StatefulSet:   sts,
	}

	_, err := r.handleCertMigrationPhase(ctx, ctl, discovery, cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TotalPods is 0")
}

func TestPodMigrationPhase_FailsIfCertsNotExist(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	sts := createTestStatefulSet(testClusterName, testNamespace, 3)
	require.NoError(t, ctl.Create(ctx, sts))

	cluster := createTestClusterWithMigration(PhasePodMigration, 3)
	cluster.Status.Migration.CurrentPodIndex = 2
	cluster.Status.Migration.MigratedNodes = []string{}

	discovery := &DiscoveryResult{
		Name:          testClusterName,
		Namespace:     testNamespace,
		MigrationType: MigrationTypeHelm,
		StatefulSet:   sts,
	}

	// No node-secret exists (Helm expects {name}-node-secret).
	_, err := r.handlePodMigrationPhase(ctx, ctl, discovery, cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "node secret")
}

func TestPodMigrationPhase_FailsIfCertsNotExist_OperatorMigration(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	sts := createTestStatefulSet(testClusterName, testNamespace, 3)
	require.NoError(t, ctl.Create(ctx, sts))

	cluster := createTestClusterWithMigration(PhasePodMigration, 3)
	cluster.Status.Migration.Type = MigrationTypeOperator
	cluster.Status.Migration.CurrentPodIndex = 2
	cluster.Status.Migration.MigratedNodes = []string{}

	discovery := &DiscoveryResult{
		Name:          testClusterName,
		Namespace:     testNamespace,
		MigrationType: MigrationTypeOperator,
		StatefulSet:   sts,
	}

	// No node secret exists (Operator expects {name}-node).
	_, err := r.handlePodMigrationPhase(ctx, ctl, discovery, cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "node secret")
}

func TestPodMigrationPhase_TransitionsToFinalization(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	sts := createTestStatefulSet(testClusterName, testNamespace, 3)
	require.NoError(t, ctl.Create(ctx, sts))

	// Create node secret
	require.NoError(t, ctl.Create(ctx,
		createTestNodeSecret("test-cluster-node-secret", testNamespace)))

	// The drift sync reconciles MigratedNodes with actual CrdbNode objects.
	for i := range 3 {
		require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true},
			createTestCrdbNode(testClusterName, testNamespace, i)))
	}

	cluster := createTestClusterWithMigration(PhasePodMigration, 3)
	cluster.Status.Migration.CurrentPodIndex = 0
	cluster.Status.Migration.MigratedNodes = []string{"test-cluster-0", "test-cluster-1", "test-cluster-2"}

	discovery := &DiscoveryResult{
		Name:          testClusterName,
		Namespace:     testNamespace,
		MigrationType: MigrationTypeHelm,
		StatefulSet:   sts,
	}

	_, err := r.handlePodMigrationPhase(ctx, ctl, discovery, cluster)
	require.NoError(t, err)

	assert.Equal(t, PhaseFinalization, cluster.Status.Migration.Phase)
}

// TestPodMigrationPhase_DriftSyncRecomputesCurrentPodIndex verifies that when
// MigratedNodes drifts (same count, different names), the drift sync replaces
// the list from actual CrdbNodes and recomputes CurrentPodIndex to the highest
// unmigrated ordinal. Without the recompute, the controller would target the
// stale index and skip the newly unmigrated pod.
func TestPodMigrationPhase_DriftSyncRecomputesCurrentPodIndex(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	sts := createTestStatefulSet(testClusterName, testNamespace, 3)
	require.NoError(t, ctl.Create(ctx, sts))

	require.NoError(t, ctl.Create(ctx,
		createTestNodeSecret("test-cluster-node-secret", testNamespace)))

	// Actual CrdbNode is cluster-1, but status says cluster-2 was migrated.
	// Same count but different names, so the old len() check would miss this.
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true},
		createTestCrdbNode(testClusterName, testNamespace, 1)))

	cluster := createTestClusterWithMigration(PhasePodMigration, 3)
	cluster.Status.Migration.CurrentPodIndex = 1
	cluster.Status.Migration.MigratedNodes = []string{fmt.Sprintf("%s-2", testClusterName)}

	discovery := &DiscoveryResult{
		Name:          testClusterName,
		Namespace:     testNamespace,
		MigrationType: MigrationTypeHelm,
		StatefulSet:   sts,
	}

	// handlePodMigrationPhase will run the drift sync, then hit the bounds
	// check or target the next pod. We only care that the sync happened.
	_, _ = r.handlePodMigrationPhase(ctx, ctl, discovery, cluster)

	migration := cluster.Status.Migration
	assert.Equal(t, []string{fmt.Sprintf("%s-1", testClusterName)}, migration.MigratedNodes,
		"drift sync should replace MigratedNodes with actual CrdbNode names")
	assert.Equal(t, int32(2), migration.CurrentPodIndex,
		"drift sync should recompute CurrentPodIndex to highest unmigrated ordinal")
}

func TestPodMigrationPhase_OperatorUsesRuntimeNodeSecretName(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	sts := createTestStatefulSet(testClusterName, testNamespace, 3)
	require.NoError(t, ctl.Create(ctx, sts))

	// Public operator source secrets are named test-cluster-node and test-cluster-root-client,
	// but the migrated CrdbNodes should use the enterprise operator destination secret.
	require.NoError(t, ctl.Create(ctx,
		createTestNodeSecret("test-cluster-node-secret", testNamespace)))

	// The drift sync in handlePodMigrationPhase reconciles MigratedNodes with
	// actual CrdbNode objects. Without these the list would be wiped to empty.
	for i := range 3 {
		require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true},
			createTestCrdbNode(testClusterName, testNamespace, i)))
	}

	cluster := createTestClusterWithMigration(PhasePodMigration, 3)
	cluster.Status.Migration.Type = MigrationTypeOperator
	cluster.Status.Migration.CurrentPodIndex = 0
	cluster.Status.Migration.MigratedNodes = []string{"test-cluster-0", "test-cluster-1", "test-cluster-2"}

	discovery := &DiscoveryResult{
		Name:          testClusterName,
		Namespace:     testNamespace,
		MigrationType: MigrationTypeOperator,
		StatefulSet:   sts,
	}

	_, err := r.handlePodMigrationPhase(ctx, ctl, discovery, cluster)
	require.NoError(t, err)
	assert.Equal(t, PhaseFinalization, cluster.Status.Migration.Phase)
}

func TestApplySourceCertDefaults_PreservesCustomHelmSecrets(t *testing.T) {
	mCtx := &migrationutil.ParsedMigrationContext{
		SourceCASecretName:     "custom-ca",
		SourceNodeSecretName:   "custom-node",
		SourceClientSecretName: "custom-client",
	}

	applySourceCertDefaults(mCtx, testClusterName, MigrationTypeHelm)

	assert.Equal(t, "custom-ca", mCtx.SourceCASecretName)
	assert.Equal(t, "custom-node", mCtx.SourceNodeSecretName)
	assert.Equal(t, "custom-client", mCtx.SourceClientSecretName)
	assert.Equal(t, "custom-node", mCtx.NodeSecretName)
	assert.Equal(t, "custom-client", mCtx.ClientSecretName)
}

func TestApplySourceCertDefaults_UsesOperatorSourceDefaults(t *testing.T) {
	mCtx := &migrationutil.ParsedMigrationContext{}

	applySourceCertDefaults(mCtx, testClusterName, MigrationTypeOperator)

	assert.Equal(t, "test-cluster-ca", mCtx.SourceCASecretName)
	assert.Equal(t, "test-cluster-node", mCtx.SourceNodeSecretName)
	assert.Equal(t, "test-cluster-root-client", mCtx.SourceClientSecretName)
	assert.Equal(t, "test-cluster-node-secret", mCtx.NodeSecretName)
	assert.Equal(t, "test-cluster-client-secret", mCtx.ClientSecretName)
}

func TestApplySourceCertDefaults_OverridesHelmCADefaultForOperator(t *testing.T) {
	// ParseStatefulSet sets Helm-style defaults before applySourceCertDefaults runs.
	// For operator migrations, SourceCASecretName must be overridden because the
	// public operator stores the CA key in {name}-ca, not {name}-ca-secret.
	mCtx := &migrationutil.ParsedMigrationContext{
		SourceCASecretName:     "test-cluster-ca-secret",
		SourceNodeSecretName:   "test-cluster-node",
		SourceClientSecretName: "test-cluster-root",
	}

	applySourceCertDefaults(mCtx, testClusterName, MigrationTypeOperator)

	assert.Equal(t, "test-cluster-ca", mCtx.SourceCASecretName,
		"operator migration must override Helm-style CA default")
	assert.Equal(t, "test-cluster-node", mCtx.SourceNodeSecretName,
		"volume-parsed node secret should be preserved")
	assert.Equal(t, "test-cluster-root", mCtx.SourceClientSecretName,
		"volume-parsed client secret should be preserved")
}

func TestFinalizationPhase_FailsIfNodeCountMismatch(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	sts := createTestStatefulSet(testClusterName, testNamespace, 3)
	require.NoError(t, ctl.Create(ctx, sts))

	// Create only 2 CrdbNodes when we expect 3
	for i := range 2 {
		require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true},
			createTestCrdbNode(testClusterName, testNamespace, i)))
	}

	cluster := createTestClusterWithMigration(PhaseFinalization, 3)
	cluster.Status.Migration.MigratedNodes = []string{"test-cluster-0", "test-cluster-1", "test-cluster-2"}

	discovery := &DiscoveryResult{
		Name:          testClusterName,
		Namespace:     testNamespace,
		MigrationType: MigrationTypeHelm,
		StatefulSet:   sts,
	}

	_, err := r.handleFinalizationPhase(ctx, ctl, discovery, cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CrdbNode count mismatch")
}

func TestResumeFromStopped_ResumesToCorrectPhase(t *testing.T) {
	tests := []struct {
		name          string
		migratedNodes []string
		totalPods     int32
		expectedPhase string
	}{
		{
			name:          "init never finished - resume to Init",
			migratedNodes: []string{},
			totalPods:     0,
			expectedPhase: PhaseInit,
		},
		{
			name:          "no nodes migrated - resume to CertMigration",
			migratedNodes: []string{},
			totalPods:     3,
			expectedPhase: PhaseCertMigration,
		},
		{
			name:          "partial nodes migrated - resume to PodMigration",
			migratedNodes: []string{"cluster-0"},
			totalPods:     3,
			expectedPhase: PhasePodMigration,
		},
		{
			name:          "all nodes migrated - resume to Finalization",
			migratedNodes: []string{"cluster-0", "cluster-1", "cluster-2"},
			totalPods:     3,
			expectedPhase: PhaseFinalization,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			migration := &crdbv1beta1.MigrationStatus{
				Phase:         PhaseStopped,
				TotalPods:     tt.totalPods,
				MigratedNodes: tt.migratedNodes,
			}

			resumeFromStopped(ctx, migration, &DiscoveryResult{
				OriginalReplicas: tt.totalPods,
			})

			assert.Equal(t, tt.expectedPhase, migration.Phase)
		})
	}
}

func TestPodMigrationPhase_IndexOutOfBounds(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	sts := createTestStatefulSet(testClusterName, testNamespace, 3)
	require.NoError(t, ctl.Create(ctx, sts))

	// Create node secret so we pass the cert validation.
	require.NoError(t, ctl.Create(ctx,
		createTestNodeSecret("test-cluster-node-secret", testNamespace)))

	cluster := createTestClusterWithMigration(PhasePodMigration, 3)
	cluster.Status.Migration.CurrentPodIndex = 5
	cluster.Status.Migration.MigratedNodes = []string{}

	discovery := &DiscoveryResult{
		Name:          testClusterName,
		Namespace:     testNamespace,
		MigrationType: MigrationTypeHelm,
		StatefulSet:   sts,
	}

	_, err := r.handlePodMigrationPhase(ctx, ctl, discovery, cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "out of bounds")
}

// TestInitPhase_WarnsOnInsecureCluster verifies that an insecure cluster (using --insecure flag)
// emits a warning event and enables TLS for migration.
func TestInitPhase_WarnsOnInsecureCluster(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	recorder := record.NewFakeRecorder(10)
	r := NewReconciler(ctl, recorder, migrationutil.DefaultRegion, false)

	// Create an insecure STS (uses --insecure instead of --certs-dir).
	sts := createInsecureTestStatefulSet(testClusterName, testNamespace, 3)
	require.NoError(t, ctl.Create(ctx, sts))

	cluster := createTestClusterWithMigration(PhaseInit, 0)

	discovery := &DiscoveryResult{
		Name:             testClusterName,
		Namespace:        testNamespace,
		MigrationType:    MigrationTypeHelm,
		StatefulSet:      sts,
		Replicas:         3,
		OriginalReplicas: 3,
	}

	_, err := r.handleInitPhase(ctx, ctl, discovery, cluster)
	require.NoError(t, err)

	// Verify phase transitioned
	assert.Equal(t, PhaseCertMigration, cluster.Status.Migration.Phase)

	// Drain all events and check for InsecureClusterMigration.
	found := false
drainLoop:
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "InsecureClusterMigration") {
				found = true
			}
		default:
			break drainLoop
		}
	}
	assert.True(t, found, "expected InsecureClusterMigration warning event")
}

// TestInitPhase_HandlesLogsPVC verifies that a StatefulSet with a logsdir
// VolumeClaimTemplate transitions correctly during Init (LogsStore is now migrated
// automatically in buildNodeSpec).
func TestInitPhase_HandlesLogsPVC(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	recorder := record.NewFakeRecorder(10)
	r := NewReconciler(ctl, recorder, migrationutil.DefaultRegion, false)

	// Create STS with logsdir VolumeClaimTemplate.
	sts := createTestStatefulSet(testClusterName, testNamespace, 3)
	sts.Spec.VolumeClaimTemplates = append(sts.Spec.VolumeClaimTemplates, corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "logsdir"},
	})
	require.NoError(t, ctl.Create(ctx, sts))

	cluster := createTestClusterWithMigration(PhaseInit, 0)

	discovery := &DiscoveryResult{
		Name:             testClusterName,
		Namespace:        testNamespace,
		MigrationType:    MigrationTypeHelm,
		StatefulSet:      sts,
		Replicas:         3,
		OriginalReplicas: 3,
	}

	_, err := r.handleInitPhase(ctx, ctl, discovery, cluster)
	require.NoError(t, err)
	assert.Equal(t, PhaseCertMigration, cluster.Status.Migration.Phase)
}

// TestFinalizationPhase_UpdatesExistingClusterForHelm verifies that the Helm
// finalization path updates the existing minimal CrdbCluster with full spec and
// Mode=MutableOnly, rather than creating a new one. Phase stays at Finalization
// until the user manually deletes the StatefulSet.
func TestFinalizationPhase_UpdatesExistingClusterForHelm(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	sts := createTestStatefulSetWithPVC(testClusterName, testNamespace, 3)
	require.NoError(t, ctl.Create(ctx, sts))

	// Create 3 CrdbNodes (finalization validates count matches TotalPods).
	for i := range 3 {
		require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true},
			createTestCrdbNode(testClusterName, testNamespace, i)))
	}

	// Create the minimal CrdbCluster (as getOrCreateCluster would).
	cluster := createTestClusterWithMigration(PhaseFinalization, 3)
	cluster.Labels = map[string]string{LabelMigrationStatus: MigrateInProgress}
	mode := crdbv1beta1.Disabled
	cluster.Spec.Mode = &mode
	cluster.Status.Migration.MigratedNodes = []string{"test-cluster-0", "test-cluster-1", "test-cluster-2"}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

	discovery := &DiscoveryResult{
		Name:          testClusterName,
		Namespace:     testNamespace,
		MigrationType: MigrationTypeHelm,
		StatefulSet:   sts,
	}

	_, err := r.handleFinalizationPhase(ctx, ctl, discovery, cluster)
	require.NoError(t, err)

	// Verify CrdbCluster was updated (not re-created).
	var updatedCluster crdbv1beta1.CrdbCluster
	require.NoError(t, ctl.Get(ctx, testClusterName, &updatedCluster))

	// finalizeHelmSpec preserves Mode from the existing spec. The cluster starts
	// with Mode=Disabled during migration and stays Disabled until completeMigration
	// sets Mode=MutableOnly after the user deletes the StatefulSet.
	require.NotNil(t, updatedCluster.Spec.Mode)
	assert.Equal(t, crdbv1beta1.Disabled, *updatedCluster.Spec.Mode,
		"Mode should stay Disabled until STS is deleted and completeMigration runs")

	// Label stays at in-progress; completeMigration updates it to complete.
	assert.Equal(t, MigrateInProgress, updatedCluster.Labels[LabelMigrationStatus])

	// Phase and message are set on the in-memory cluster object and returned to the
	// caller for persistence. handleFinalizationPhase only persists the spec update,
	// not the status, so we check the in-memory object here.
	assert.Equal(t, PhaseFinalization, cluster.Status.Migration.Phase)
	assert.Contains(t, cluster.Status.Migration.Message, "Delete the StatefulSet")
}

// TestFinalizationPhase_HelmAdoptionMessage verifies that the finalization message
// instructs the user to delete the StatefulSet to complete the migration. Phase
// stays at Finalization until the delete watch event fires.
func TestFinalizationPhase_HelmAdoptionMessage(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	sts := createTestStatefulSetWithPVC(testClusterName, testNamespace, 3)
	require.NoError(t, ctl.Create(ctx, sts))

	// Create 3 CrdbNodes.
	for i := range 3 {
		require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true},
			createTestCrdbNode(testClusterName, testNamespace, i)))
	}

	// Create minimal CrdbCluster.
	cluster := createTestClusterWithMigration(PhaseFinalization, 3)
	cluster.Labels = map[string]string{LabelMigrationStatus: MigrateInProgress}
	modeDisabled := crdbv1beta1.Disabled
	cluster.Spec.Mode = &modeDisabled
	cluster.Status.Migration.MigratedNodes = []string{"test-cluster-0", "test-cluster-1", "test-cluster-2"}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

	discovery := &DiscoveryResult{
		Name:          testClusterName,
		Namespace:     testNamespace,
		MigrationType: MigrationTypeHelm,
		StatefulSet:   sts,
	}

	_, err := r.handleFinalizationPhase(ctx, ctl, discovery, cluster)
	require.NoError(t, err)

	// Finalization leaves Phase at Finalization with a message prompting STS deletion.
	// PhaseComplete is only recorded by completeMigration once the STS is gone.
	msg := cluster.Status.Migration.Message
	assert.Equal(t, PhaseFinalization, cluster.Status.Migration.Phase)
	assert.Contains(t, msg, "Delete the StatefulSet")
}

// TestInitPhase_AutoRecovery_TotalPodsFromScaledDownSTS verifies that TotalPods is computed
// as existingCrdbNodes + remaining STS replicas when the STS has already been partially
// scaled down. Using only sts.Spec.Replicas would undercount because one pod has already
// been migrated and removed from the STS.
func TestInitPhase_AutoRecovery_TotalPodsFromScaledDownSTS(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)
	r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

	// STS has been scaled down to 2 replicas (one pod already migrated).
	scaledReplicas := int32(2)
	sts := createTestStatefulSet(testClusterName, testNamespace, scaledReplicas)
	require.NoError(t, ctl.Create(ctx, sts))

	// One CrdbNode already exists for the migrated pod.
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true},
		createTestCrdbNode(testClusterName, testNamespace, 2)))

	cluster := createTestClusterWithMigration(PhaseInit, 0)

	discovery := &DiscoveryResult{
		Name:             testClusterName,
		Namespace:        testNamespace,
		MigrationType:    MigrationTypeHelm,
		StatefulSet:      sts,
		Replicas:         scaledReplicas,
		OriginalReplicas: 3, // 1 existing node + 2 STS replicas
	}

	_, err := r.handleInitPhase(ctx, ctl, discovery, cluster)
	require.NoError(t, err)

	// TotalPods must be 1 existing node + 2 STS replicas = 3, not just sts.Spec.Replicas = 2.
	assert.Equal(t, int32(3), cluster.Status.Migration.TotalPods)
	assert.Equal(t, PhaseCertMigration, cluster.Status.Migration.Phase)
	assert.Contains(t, cluster.Status.Migration.Message, "Auto-recovered")
}

// TestCertMigrationPhase covers the three main cert migration paths: Helm happy path
// with self-signed certs, insecure clusters that skip certs entirely, and operator
// migrations with custom v1alpha1 secret names overriding STS-parsed defaults.
func TestCertMigrationPhase(t *testing.T) {
	tests := []struct {
		name          string
		migrationType string
		insecure      bool
		// setupSecrets creates source secrets and returns the CA cert bytes for
		// later verification. Returns nil for insecure clusters.
		setupSecrets func(t *testing.T, ctl *kube.Ctl) []byte
		// v1alpha1Cluster is set for operator migrations with custom secret names.
		v1alpha1Cluster *crdbv1alpha1.CrdbCluster
		// expectSecrets lists destination secret/configmap names that must exist
		// after migration. Skipped for insecure clusters.
		expectSecrets []string
	}{
		{
			name:          "helm self-signed certs regenerated with join service SANs",
			migrationType: MigrationTypeHelm,
			setupSecrets: func(t *testing.T, ctl *kube.Ctl) []byte {
				caCert, caKey := generateTestCA(t)
				ctx := context.Background()
				require.NoError(t, ctl.Create(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: testClusterName + suffixNodeSecret, Namespace: testNamespace},
					Data:       map[string][]byte{"tls.crt": caCert, "tls.key": caKey, "ca.crt": caCert},
				}))
				require.NoError(t, ctl.Create(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: testClusterName + suffixCASecret, Namespace: testNamespace},
					Data:       map[string][]byte{"ca.crt": caCert, "ca.key": caKey},
				}))
				return caCert
			},
			expectSecrets: []string{
				testClusterName + suffixNodeSecret,
				testClusterName + suffixClientSecret,
				testClusterName + suffixCAConfigMap,
			},
		},
		{
			name:          "insecure cluster skips all cert operations",
			migrationType: MigrationTypeHelm,
			insecure:      true,
			setupSecrets:  func(t *testing.T, ctl *kube.Ctl) []byte { return nil },
		},
		{
			name:          "helm provided certs skip regeneration when ca.key unavailable",
			migrationType: MigrationTypeHelm,
			setupSecrets: func(t *testing.T, ctl *kube.Ctl) []byte {
				caCert, _ := generateTestCA(t)
				ctx := context.Background()
				// Helm provided mode: user supplies node and CA secrets but the CA
				// secret only has ca.crt (no ca.key). Regeneration should be skipped.
				require.NoError(t, ctl.Create(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: testClusterName + suffixNodeSecret, Namespace: testNamespace},
					Data:       map[string][]byte{"tls.crt": caCert, "tls.key": caCert, "ca.crt": caCert},
				}))
				require.NoError(t, ctl.Create(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: testClusterName + suffixCASecret, Namespace: testNamespace},
					Data:       map[string][]byte{"ca.crt": caCert},
				}))
				return caCert
			},
			// Only CA ConfigMap created. No cert regeneration because ca.key is missing.
			expectSecrets: []string{
				testClusterName + suffixCAConfigMap,
			},
		},
		{
			name:          "operator migration with custom secrets skips cert regeneration",
			migrationType: MigrationTypeOperator,
			setupSecrets: func(t *testing.T, ctl *kube.Ctl) []byte {
				caCert, _ := generateTestCA(t)
				ctx := context.Background()
				// the user's custom secrets only have tls.crt, tls.key, and ca.crt.
				// ca.key is not available because the user manages their own CA.
				require.NoError(t, ctl.Create(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: "my-custom-node-cert", Namespace: testNamespace},
					Data:       map[string][]byte{"tls.crt": caCert, "tls.key": caCert, "ca.crt": caCert},
				}))
				return caCert
			},
			v1alpha1Cluster: &crdbv1alpha1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
				Spec: crdbv1alpha1.CrdbClusterSpec{
					NodeTLSSecret:   "my-custom-node-cert",
					ClientTLSSecret: "my-custom-client-cert",
				},
			},
			// Only the CA ConfigMap is created. No new node or client secrets
			// because cert regeneration is skipped for external certs.
			expectSecrets: []string{
				testClusterName + suffixCAConfigMap,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctl := kubetest.NewFakeCtl(kubetest.Options{
				Options: kube.Options{Scheme: kube.Scheme},
			}).WithNamespace(testNamespace)
			r := NewReconciler(ctl, record.NewFakeRecorder(10), migrationutil.DefaultRegion, false)

			var sts *appsv1.StatefulSet
			if tt.insecure {
				sts = createInsecureTestStatefulSet(testClusterName, testNamespace, 3)
			} else {
				sts = createTestStatefulSet(testClusterName, testNamespace, 3)
			}
			require.NoError(t, ctl.Create(ctx, sts))

			caCert := tt.setupSecrets(t, ctl)

			cluster := createTestClusterWithMigration(PhaseCertMigration, 3)
			cluster.Status.Migration.Type = tt.migrationType

			discovery := &DiscoveryResult{
				Name:            testClusterName,
				Namespace:       testNamespace,
				MigrationType:   tt.migrationType,
				StatefulSet:     sts,
				V1Alpha1Cluster: tt.v1alpha1Cluster,
			}

			_, err := r.handleCertMigrationPhase(ctx, ctl, discovery, cluster)
			require.NoError(t, err)
			assert.Equal(t, PhasePodMigration, cluster.Status.Migration.Phase)

			// Verify destination secrets were created with the correct CA.
			for _, secretName := range tt.expectSecrets {
				if strings.HasSuffix(secretName, suffixCAConfigMap) {
					var cm corev1.ConfigMap
					require.NoError(t, ctl.Get(ctx, secretName, &cm), "expected ConfigMap %s to exist", secretName)
					assert.Contains(t, cm.Data, "ca.crt")
				} else {
					var s corev1.Secret
					require.NoError(t, ctl.Get(ctx, secretName, &s), "expected Secret %s to exist", secretName)
					assert.Contains(t, s.Data, "tls.crt")
					// the CA in each destination secret must come from our source
					assert.Equal(t, caCert, s.Data["ca.crt"],
						"CA in %s must match the source CA", secretName)
				}
			}
		})
	}
}

// createTestStatefulSetWithPVC creates a minimal StatefulSet with VolumeClaimTemplates
// for tests that exercise Finalization (which calls generateV1Beta1ClusterSpec / buildNodeSpec).
func createTestStatefulSetWithPVC(name, namespace string, replicas int32) *appsv1.StatefulSet {
	sts := createTestStatefulSet(name, namespace, replicas)
	sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "datadir"},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			},
		},
	}
	return sts
}

// createInsecureTestStatefulSet creates a StatefulSet with --insecure flag (no TLS).
func createInsecureTestStatefulSet(name, namespace string, replicas int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			Generation: 1,
			Labels: map[string]string{
				"app.kubernetes.io/name": "cockroachdb",
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: name,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "db",
							Image:   "cockroachdb/cockroach:v23.1.0",
							Command: []string{"/cockroach/cockroach"},
							Args: []string{
								"start",
								"--join=test-cluster-0.test-cluster",
								"--insecure",
							},
						},
					},
				},
			},
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 1,
			Replicas:           replicas,
			ReadyReplicas:      replicas,
		},
	}
}

// generateTestCA creates a self-signed CA certificate and key for tests that exercise
// cert migration. Returns PEM-encoded cert and key bytes.
func generateTestCA(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"Test"}, CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &caKey.PublicKey, caKey)
	require.NoError(t, err)

	certBuf := new(bytes.Buffer)
	require.NoError(t, pem.Encode(certBuf, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}))

	keyBuf := new(bytes.Buffer)
	require.NoError(t, pem.Encode(keyBuf, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(caKey)}))

	return certBuf.Bytes(), keyBuf.Bytes()
}

// createTestCrdbNode creates a CrdbNode with the standard owner label.
func createTestCrdbNode(clusterName, namespace string, ordinal int) *crdbv1beta1.CrdbNode {
	return &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", clusterName, ordinal),
			Namespace: namespace,
			Labels: map[string]string{
				manifests.BetaCrdbClusterNodeOwnerLabelKey: clusterName,
			},
		},
	}
}

// createTestNodeSecret creates a TLS secret with dummy cert data.
func createTestNodeSecret(name, namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"tls.crt": []byte("cert"),
			"tls.key": []byte("key"),
		},
	}
}

// createTestClusterWithMigration creates a CrdbCluster with a MigrationStatus
// for Helm-type migrations. Callers can set additional fields like Mode,
// CurrentPodIndex, or MigratedNodes on the returned object.
func createTestClusterWithMigration(phase string, totalPods int32) *crdbv1beta1.CrdbCluster {
	return &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testClusterName,
			Namespace: testNamespace,
		},
		Status: crdbv1beta1.CrdbClusterStatus{
			Migration: &crdbv1beta1.MigrationStatus{
				Phase:     phase,
				Type:      MigrationTypeHelm,
				TotalPods: totalPods,
			},
		},
	}
}

// createTestStatefulSet creates a minimal StatefulSet for testing.
func createTestStatefulSet(name, namespace string, replicas int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			Generation: 1,
			Labels: map[string]string{
				"app.kubernetes.io/name": "cockroachdb",
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: name,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "db",
							Image:   "cockroachdb/cockroach:v23.1.0",
							Command: []string{"/cockroach/cockroach"},
							Args: []string{
								"start",
								"--join=test-cluster-0.test-cluster",
								"--certs-dir=/cockroach/cockroach-certs",
							},
						},
					},
				},
			},
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 1,
			Replicas:           replicas,
			ReadyReplicas:      replicas,
		},
	}
}

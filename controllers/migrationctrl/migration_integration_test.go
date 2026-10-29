package migrationctrl_test

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"testing"
	"time"

	crdbv1alpha1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1alpha1"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/migrationctrl"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbtest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/testenv"
	"github.com/stretchr/testify/suite"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// The public operator deploys in this namespace.
	publicOperatorNamespace = "cockroach-operator-system"

	// Public operator deployment name, matching the upstream v2.18.3 install/operator.yaml.
	publicOperatorDeployName = "cockroach-operator-manager"

	// CockroachDB image accessible from k3d via the docker.io registry cache.
	testCockroachImage = "cockroachdb/cockroach:v24.2.4"

	// Upstream manifest URLs for the public operator v2.18.3.
	publicOperatorCRDURL = "https://raw.githubusercontent.com/cockroachdb/cockroach-operator/v2.18.3/install/crds.yaml"
	publicOperatorURL    = "https://raw.githubusercontent.com/cockroachdb/cockroach-operator/v2.18.3/install/operator.yaml"

	// The public operator's webhook service name (in cockroach-operator-system namespace).
	publicWebhookServiceName = "cockroach-operator-webhook-service"

	// The public operator's webhook configuration names. These must be patched with
	// matchPolicy: Exact when creating v1beta1 clusters with both operators present,
	// otherwise the public operator's webhooks (with the default Equivalent policy)
	// intercept v1beta1 requests after converting them to v1alpha1.
	publicValidatingWebhookConfig = "cockroach-operator-validating-webhook-configuration"
	publicMutatingWebhookConfig   = "cockroach-operator-mutating-webhook-configuration"

	// Cluster names used across test cases.
	clusterMigrationTest = "migration-test"
	clusterStopResume    = "stop-resume"
	clusterRollback      = "rollback"
	clusterCoexistPre    = "coexist-pre"
	clusterCoexistNew    = "coexist-new"
	clusterV1Beta1Native = "v1beta1-native"
)

func TestIntegrationMigration(t *testing.T) {
	testutils.MarkIntegrationTest(t)
	suite.Run(t, new(MigrationTestSuite))
}

type MigrationTestSuite struct {
	suite.Suite
	env *testenv.Env
	ctl *kube.Ctl
}

// SetupSuite creates a bare k3d cluster and deploys the public operator using
// the exact upstream manifests. The enterprise operator is NOT deployed here.
// Each test deploys it after skip-reconcile is applied, scoped to the test's
// namespace via WATCH_NAMESPACE. This matches the real migration procedure:
// public operator first, cluster creation, skip-reconcile, then enterprise
// operator.
func (s *MigrationTestSuite) SetupSuite() {
	s.env = testenv.New(s.T(), testenv.MultiZoneConfig, testenv.SkipOperatorDeploy)
	s.ctl = s.env.DefaultCluster().Ctl

	// Install the public operator using the exact upstream manifests. This
	// matches what users do manually and avoids subtle schema or webhook
	// differences from reconstructing resources in Go.
	s.T().Log("Installing public operator CRDs...")
	s.kubectlApply(publicOperatorCRDURL)

	s.T().Log("Installing public operator...")
	s.kubectlApply(publicOperatorURL)

	s.waitForDeploymentReady(context.Background(),
		publicOperatorNamespace, publicOperatorDeployName, 3*time.Minute)

	// The deployment being available doesn't mean the webhook is serving. Wait
	// for the webhook service to have at least one ready endpoint before running
	// tests that create v1alpha1 CrdbClusters.
	s.waitForPublicOperatorWebhook(context.Background(), 60*time.Second)

	// Exact matching keeps the public operator's admission webhooks on v1alpha1 requests.
	s.patchPublicOperatorWebhooksExact()
	s.T().Log("Public operator installed and webhook ready")

	s.T().Cleanup(func() {
		s.kubectlDelete(publicOperatorURL)
		s.kubectlDelete(publicOperatorCRDURL)
	})
}

// deployEnterpriseOperator deploys the enterprise operator scoped to the given
// namespace via WATCH_NAMESPACE. This ensures betaclusterctrl only sees clusters
// in that namespace, preventing interference with public operator clusters in
// other namespaces. The enterprise CRDs (with conversion webhook) are also
// installed, overwriting the v1alpha1-only CRD.
func (s *MigrationTestSuite) deployEnterpriseOperator(ctx context.Context, watchNamespace string) {
	s.T().Logf("Deploying enterprise operator scoped to namespace %s...", watchNamespace)

	objects := manifests.CockroachOperator(manifests.CockroachOperatorOptions{
		Replicas:                  1,
		SkipTenantController:      true,
		EnableMigrationController: true,
		EnvVars: []corev1.EnvVar{
			{
				Name:  "WATCH_NAMESPACE",
				Value: watchNamespace,
			},
		},
	})

	// Both operators produce a ClusterRole named "cockroach-operator-role" and a
	// ClusterRoleBinding named "cockroach-operator-rolebinding". Applying the
	// enterprise operator's version overwrites the public operator's RBAC and
	// breaks its ability to reconcile. Rename the enterprise RBAC objects so
	// both operators can coexist.
	//
	// Both operators also default to app=cockroach-operator as the pod label.
	// The webhook service uses this selector to find the operator pod. When both
	// operators run side-by-side, the service routes to the wrong pod and the
	// conversion webhook returns 502. Use a distinct label for the enterprise
	// operator so each webhook service targets only its own pods.
	const enterpriseAppLabel = "cockroachdb-operator"
	for _, obj := range objects {
		switch r := obj.(type) {
		case *rbacv1.ClusterRole:
			r.Name = "enterprise-" + r.Name
		case *rbacv1.ClusterRoleBinding:
			r.Name = "enterprise-" + r.Name
			r.RoleRef.Name = "enterprise-" + r.RoleRef.Name
		case *appsv1.Deployment:
			r.Labels["app"] = enterpriseAppLabel
			r.Spec.Selector.MatchLabels["app"] = enterpriseAppLabel
			r.Spec.Template.Labels["app"] = enterpriseAppLabel
		}
	}

	s.Require().NoError(s.ctl.Apply(ctx, objects...))

	// Wait for the Deployment rollout to finish. When tests run consecutively
	// the previous operator pod is still running. waitForDeploymentRollout
	// ensures the old pod terminates and the new pod is the one serving
	// webhook requests, avoiding stale-state validation errors.
	s.waitForDeploymentRollout(ctx, "default", manifests.OperatorName, 90*time.Second)

	// Wait for the webhook endpoints to be ready. The enterprise operator
	// installs the conversion webhook on the CRD at startup, so v1alpha1
	// requests will start going through the conversion webhook after this.
	s.waitForWebhookEndpoints(ctx, 60*time.Second)
	s.T().Log("Enterprise operator deployed and webhook ready")
}

// waitForWebhookEndpoints polls until the enterprise webhook service has at
// least one ready endpoint.
func (s *MigrationTestSuite) waitForWebhookEndpoints(ctx context.Context, timeout time.Duration) {
	s.Require().Eventually(func() bool {
		var ep corev1.Endpoints
		if err := s.ctl.Get(ctx, manifests.WebhookServiceName, &ep); err != nil {
			return false
		}
		for _, eps := range ep.Subsets {
			if len(eps.Addresses) > 0 {
				return true
			}
		}
		return false
	}, timeout, 250*time.Millisecond, "enterprise webhook endpoints should become ready")
}

// waitForPublicOperatorWebhook polls until the public operator's webhook
// service has at least one ready endpoint. Without this, creating v1alpha1
// CrdbClusters immediately after the deployment is available returns 502.
func (s *MigrationTestSuite) waitForPublicOperatorWebhook(ctx context.Context, timeout time.Duration) {
	pubCtl := s.ctl.WithNamespace(publicOperatorNamespace)
	s.Require().Eventually(func() bool {
		var ep corev1.Endpoints
		if err := pubCtl.Get(ctx, publicWebhookServiceName, &ep); err != nil {
			return false
		}
		for _, eps := range ep.Subsets {
			if len(eps.Addresses) > 0 {
				return true
			}
		}
		return false
	}, timeout, 250*time.Millisecond, "public operator webhook endpoints should become ready")
}

// createV1Alpha1Cluster retries creation of a v1alpha1 CrdbCluster until the
// public operator's webhook is fully serving. Endpoint readiness alone does not
// guarantee the webhook handler has registered its routes, so the first few
// Create calls may return 502. We use CreateWithOptions(NoWait) because the
// CrdbCluster won't be "ready" until the public operator reconciles it.
func (s *MigrationTestSuite) createV1Alpha1Cluster(ctx context.Context, ctl *kube.Ctl, cluster *crdbv1alpha1.CrdbCluster) {
	s.Require().Eventually(func() bool {
		err := ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster.DeepCopy())
		if err == nil {
			return true
		}
		s.T().Logf("Retrying v1alpha1 cluster creation (webhook may not be serving yet): %v", err)
		return false
	}, 60*time.Second, 2*time.Second, "v1alpha1 CrdbCluster creation should succeed")
}

// kubectlApply applies a manifest from a URL using kubectl.
func (s *MigrationTestSuite) kubectlApply(url string) {
	cmd := exec.Command("kubectl", "apply", "-f", url)
	out, err := cmd.CombinedOutput()
	s.Require().NoError(err, "kubectl apply failed: %s", string(out))
}

// kubectlDelete deletes resources from a manifest URL using kubectl.
func (s *MigrationTestSuite) kubectlDelete(url string) {
	cmd := exec.Command("kubectl", "delete", "-f", url, "--ignore-not-found")
	_ = cmd.Run()
}

// patchPublicOperatorWebhooksExact sets matchPolicy: Exact on the public
// operator's webhook configurations so they only fire for v1alpha1 requests.
// Without this, the default Equivalent policy causes the public operator's
// webhooks to intercept v1beta1 requests (after K8s converts them to v1alpha1).
func (s *MigrationTestSuite) patchPublicOperatorWebhooksExact() {
	patch := `[{"op":"add","path":"/webhooks/0/matchPolicy","value":"Exact"}]`
	for _, name := range []string{publicValidatingWebhookConfig, publicMutatingWebhookConfig} {
		cmd := exec.Command("kubectl", "patch", "validatingwebhookconfiguration", name,
			"--type=json", "-p", patch)
		if name == publicMutatingWebhookConfig {
			cmd = exec.Command("kubectl", "patch", "mutatingwebhookconfiguration", name,
				"--type=json", "-p", patch)
		}
		out, err := cmd.CombinedOutput()
		s.Require().NoError(err, "failed to patch %s with matchPolicy Exact: %s", name, string(out))
		s.T().Logf("Patched %s with matchPolicy: Exact", name)
	}
}

// waitForDeploymentReady polls until the deployment has at least one available
// replica.
func (s *MigrationTestSuite) waitForDeploymentReady(
	ctx context.Context, namespace, name string, timeout time.Duration,
) {
	s.Require().Eventually(func() bool {
		deploy := &appsv1.Deployment{}
		nsCtl := s.ctl.WithNamespace(namespace)
		if err := nsCtl.Get(ctx, name, deploy); err != nil {
			return false
		}
		return deploy.Status.AvailableReplicas > 0
	}, timeout, 5*time.Second, "deployment %s/%s should become available", namespace, name)
}

// waitForDeploymentRollout polls until the deployment's rollout is fully
// complete: the updated replica count matches the desired count and no old
// replicas remain. This is stricter than waitForDeploymentReady which only
// checks AvailableReplicas > 0 (satisfied by the old pod during a rolling
// update).
func (s *MigrationTestSuite) waitForDeploymentRollout(
	ctx context.Context, namespace, name string, timeout time.Duration,
) {
	s.Require().Eventually(func() bool {
		deploy := &appsv1.Deployment{}
		nsCtl := s.ctl.WithNamespace(namespace)
		if err := nsCtl.Get(ctx, name, deploy); err != nil {
			return false
		}
		desired := int32(1)
		if deploy.Spec.Replicas != nil {
			desired = *deploy.Spec.Replicas
		}
		return deploy.Status.UpdatedReplicas == desired &&
			deploy.Status.ReadyReplicas == desired &&
			deploy.Status.Replicas == desired
	}, timeout, 2*time.Second, "deployment %s/%s rollout should complete", namespace, name)
}

// ---------------------------------------------------------------------------
// Migration test: full end-to-end
// ---------------------------------------------------------------------------

// TestPublicOperatorMigration tests the full end-to-end migration from a public operator
// v1alpha1 CrdbCluster to the enterprise operator v1beta1. Each step follows the real
// user migration path:
//
//  1. Public operator is already running, reconciling v1alpha1 CrdbClusters.
//  2. User creates a v1alpha1 CrdbCluster, public operator creates a StatefulSet.
//  3. User annotates with region code and applies skip-reconcile.
//  4. User installs enterprise operator (scoped to this namespace).
//  5. User labels with migrate=start. Migration controller takes over.
//  6. User deletes the StatefulSet to finalize migration.
func (s *MigrationTestSuite) TestPublicOperatorMigration() {
	ctx := context.Background()
	ns := testenv.EnsureNamespace(s.T(), s.ctl)
	ctl := s.ctl.WithNamespace(ns)

	// Step 1: Create a v1alpha1 CrdbCluster. The public operator reconciles it
	// and creates a StatefulSet. No enterprise operator is running yet so the
	// v1alpha1-only CRD stores the resource natively.
	v1alpha1Cluster := newV1Alpha1Cluster(clusterMigrationTest, ns)
	s.createV1Alpha1Cluster(ctx, ctl, v1alpha1Cluster)

	s.T().Log("Waiting for public operator to create StatefulSet...")
	s.waitForStatefulSetReady(ctx, ctl, clusterMigrationTest, v1alpha1Cluster.Spec.Nodes)

	// Capture pre-migration STS state for later assertions.
	var sts appsv1.StatefulSet
	s.Require().NoError(ctl.Get(ctx, clusterMigrationTest, &sts))
	s.Require().NotNil(sts.Spec.Replicas)
	originalReplicas := *sts.Spec.Replicas

	// Step 2: Annotate with region code for the conversion webhook and apply
	// skip-reconcile so the public operator stops reconciling.
	s.annotateRegionCode(ctx, ctl, clusterMigrationTest)
	s.applySkipReconcile(ctx, ctl, clusterMigrationTest)

	// Step 3: Deploy the enterprise operator scoped to this namespace. This
	// overwrites the v1alpha1-only CRD with the enterprise dual-version CRD
	// and conversion webhook. The betaclusterctrl will see the cluster but won't
	// interfere because skip-reconcile was applied first.
	s.deployEnterpriseOperator(ctx, ns)

	// Step 4: Label with migrate=start to trigger the migration controller.
	s.setMigrationLabel(ctx, ctl, clusterMigrationTest, migrationctrl.MigrateStart)

	// Wait for migration to reach Finalization. At this point all pods have been
	// migrated to CrdbNodes but Mode is not yet MutableOnly (set after STS deletion).
	s.T().Log("Waiting for migration to reach Finalization...")
	s.waitForMigrationPhase(ctx, ctl, clusterMigrationTest, migrationctrl.PhaseFinalization, 10*time.Minute)

	// Verify the intermediate state before completing.
	s.assertFinalizationState(ctx, ctl, clusterMigrationTest, int(originalReplicas))

	// Step 5: Delete the StatefulSet to trigger the final phase transition.
	s.T().Log("Deleting StatefulSet to trigger completion...")
	s.Require().NoError(ctl.Delete(ctx, &sts))
	s.waitForMigrationHandoffComplete(ctx, ctl, clusterMigrationTest, 3*time.Minute)

	// Comprehensive assertions on the final migrated state.
	s.assertMigrationComplete(ctx, ctl, clusterMigrationTest, ns, int(originalReplicas))
	s.assertClusterSpec(ctx, ctl, clusterMigrationTest, v1alpha1Cluster)
	s.assertNodeSpecs(ctx, ctl, clusterMigrationTest, ns, v1alpha1Cluster)
	s.assertMigrationStatus(ctx, ctl, clusterMigrationTest, int(originalReplicas))
	s.assertNoStatefulSet(ctx, ctl, clusterMigrationTest)
	s.assertServicesExist(ctx, ctl, clusterMigrationTest)

	// The user adopts an external manager (Helm here, but any SSA client behaves
	// the same) to manage the migrated cluster. The cluster controller has stripped
	// the operator-injected annotations and the conversion webhook now takes the
	// minimal path (Mode=MutableOnly), so repeated Server-Side Applies must succeed
	// without field-ownership conflicts.
	s.assertNoOperatorInjectedAnnotations(ctx, ctl, clusterMigrationTest)
	s.Require().NoError(s.serverSideApplyAsManager(ctx, ctl, clusterMigrationTest, nil),
		"SSA adoption after migration should not conflict")
	s.Require().NoError(s.serverSideApplyAsManager(ctx, ctl, clusterMigrationTest,
		func(c *crdbv1beta1.CrdbCluster) {
			c.Spec.ClusterSettings = map[string]string{
				"sql.defaults.results_buffer.size": "16KiB",
			}
		}),
		"SSA upgrade after migration should not conflict")
	s.assertNoOperatorInjectedAnnotations(ctx, ctl, clusterMigrationTest)
}

// ---------------------------------------------------------------------------
// Migration test: stop and resume
// ---------------------------------------------------------------------------

// TestStopResume tests pausing migration mid-flight and then resuming it to completion.
func (s *MigrationTestSuite) TestStopResume() {
	ctx := context.Background()
	ns := testenv.EnsureNamespace(s.T(), s.ctl)
	ctl := s.ctl.WithNamespace(ns)

	v1alpha1Cluster := newV1Alpha1Cluster(clusterStopResume, ns)
	s.createV1Alpha1Cluster(ctx, ctl, v1alpha1Cluster)
	s.waitForStatefulSetReady(ctx, ctl, clusterStopResume, v1alpha1Cluster.Spec.Nodes)

	s.annotateRegionCode(ctx, ctl, clusterStopResume)
	s.applySkipReconcile(ctx, ctl, clusterStopResume)
	s.deployEnterpriseOperator(ctx, ns)
	s.setMigrationLabel(ctx, ctl, clusterStopResume, migrationctrl.MigrateStart)

	// Wait for at least 1 pod to be migrated before stopping.
	s.waitForMigratedNodeCount(ctx, ctl, clusterStopResume, 1, 5*time.Minute)

	// Pause migration by setting the label to "stop".
	s.setMigrationLabel(ctx, ctl, clusterStopResume, migrationctrl.MigrateStop)
	s.waitForMigrationPhase(ctx, ctl, clusterStopResume, migrationctrl.PhaseStopped, 2*time.Minute)

	// Capture how many nodes were migrated before stopping.
	cluster := &crdbv1beta1.CrdbCluster{}
	s.Require().NoError(ctl.Get(ctx, clusterStopResume, cluster))
	migratedAtStop := len(cluster.Status.Migration.MigratedNodes)
	s.Require().GreaterOrEqual(migratedAtStop, 1)

	// Resume by setting the label back to "start".
	s.setMigrationLabel(ctx, ctl, clusterStopResume, migrationctrl.MigrateStart)
	s.waitForMigrationPhase(ctx, ctl, clusterStopResume, migrationctrl.PhaseFinalization, 10*time.Minute)

	// Delete STS to complete.
	var sts appsv1.StatefulSet
	s.Require().NoError(ctl.Get(ctx, clusterStopResume, &sts))
	s.Require().NoError(ctl.Delete(ctx, &sts))
	s.waitForMigrationHandoffComplete(ctx, ctl, clusterStopResume, 3*time.Minute)

	s.assertMigrationComplete(ctx, ctl, clusterStopResume, ns, int(v1alpha1Cluster.Spec.Nodes))
}

// ---------------------------------------------------------------------------
// Migration test: rollback
// ---------------------------------------------------------------------------

// TestRollbackAfterPartial tests partial migration followed by rollback.
// After rollback, the STS should be restored and all CrdbNodes removed.
func (s *MigrationTestSuite) TestRollbackAfterPartial() {
	ctx := context.Background()
	ns := testenv.EnsureNamespace(s.T(), s.ctl)
	ctl := s.ctl.WithNamespace(ns)

	v1alpha1Cluster := newV1Alpha1Cluster(clusterRollback, ns)
	s.createV1Alpha1Cluster(ctx, ctl, v1alpha1Cluster)
	s.waitForStatefulSetReady(ctx, ctl, clusterRollback, v1alpha1Cluster.Spec.Nodes)

	s.annotateRegionCode(ctx, ctl, clusterRollback)
	s.applySkipReconcile(ctx, ctl, clusterRollback)
	s.deployEnterpriseOperator(ctx, ns)
	s.setMigrationLabel(ctx, ctl, clusterRollback, migrationctrl.MigrateStart)

	// Wait for partial migration.
	s.waitForMigratedNodeCount(ctx, ctl, clusterRollback, 1, 5*time.Minute)

	// Stop then rollback.
	s.setMigrationLabel(ctx, ctl, clusterRollback, migrationctrl.MigrateStop)
	s.waitForMigrationPhase(ctx, ctl, clusterRollback, migrationctrl.PhaseStopped, 2*time.Minute)

	s.setMigrationLabel(ctx, ctl, clusterRollback, migrationctrl.MigrateRollback)
	s.waitForMigrationPhase(ctx, ctl, clusterRollback, migrationctrl.PhaseRollbackComplete, 5*time.Minute)

	// After rollback, all CrdbNodes should be removed and the STS restored.
	s.assertClusterNodeCount(ctx, ctl, clusterRollback, ns, 0)

	var sts appsv1.StatefulSet
	s.Require().NoError(ctl.Get(ctx, clusterRollback, &sts))
	s.Require().NotNil(sts.Spec.Replicas)
	s.Require().Equal(v1alpha1Cluster.Spec.Nodes, *sts.Spec.Replicas,
		"STS replicas should be restored after rollback")
}

// ---------------------------------------------------------------------------
// Coexistence test: non-migrating v1alpha1 clusters
// ---------------------------------------------------------------------------

// TestV1Alpha1Coexistence verifies that non-migrating v1alpha1 clusters
// continue to work after the enterprise operator is installed. The enterprise
// webhooks use matchPolicy: Exact so they only fire for v1beta1 requests. The
// test follows this sequence:
//
//  1. Create a v1alpha1 cluster while only the public operator is running.
//  2. Install the enterprise operator (replaces CRD, adds conversion webhook).
//  3. Verify the existing cluster is still readable and updatable as v1alpha1.
//  4. Create a new v1alpha1 cluster with both operators present.
//  5. Verify the public operator reconciles it normally and the enterprise
//     operator does not create CrdbNodes for either cluster.
func (s *MigrationTestSuite) TestV1Alpha1Coexistence() {
	ctx := context.Background()
	ns := testenv.EnsureNamespace(s.T(), s.ctl)
	ctl := s.ctl.WithNamespace(ns)

	// Create a v1alpha1 cluster before the enterprise operator exists. The
	// public operator reconciles it and creates a StatefulSet normally.
	preExisting := newV1Alpha1Cluster(clusterCoexistPre, ns)
	s.createV1Alpha1Cluster(ctx, ctl, preExisting)
	s.waitForStatefulSetReady(ctx, ctl, clusterCoexistPre, preExisting.Spec.Nodes)

	// Install the enterprise operator. This overwrites the CRD with the
	// dual-version CRD and registers the conversion webhook. All v1alpha1
	// reads and writes now go through conversion.
	s.deployEnterpriseOperator(ctx, ns)

	// The pre-existing cluster should still be readable as v1alpha1.
	existing := &crdbv1alpha1.CrdbCluster{}
	s.Require().NoError(ctl.Get(ctx, clusterCoexistPre, existing),
		"pre-existing v1alpha1 cluster should be readable after enterprise operator install")
	s.Require().Equal(preExisting.Spec.Nodes, existing.Spec.Nodes)

	// Reading the same cluster as v1beta1 should also work. Mode=Disabled
	// tells the enterprise operator to ignore it.
	existingBeta := &crdbv1beta1.CrdbCluster{}
	s.Require().NoError(ctl.Get(ctx, clusterCoexistPre, existingBeta))
	if existingBeta.Spec.Mode != nil {
		s.Require().Equal(crdbv1beta1.Disabled, *existingBeta.Spec.Mode,
			"non-migrating cluster should have Mode=Disabled from conversion")
	}

	// Update the pre-existing cluster as v1alpha1. Without matchPolicy: Exact
	// the enterprise v1beta1 webhook would intercept this and reject it for
	// missing ExternalCertificates. With Exact, only the public operator's
	// v1alpha1 webhooks fire.
	existing.Spec.Cache = "40%"
	s.Require().NoError(ctl.Update(ctx, existing),
		"v1alpha1 update should succeed with matchPolicy: Exact")

	// Create a new v1alpha1 cluster with both operators present.
	newCluster := newV1Alpha1Cluster(clusterCoexistNew, ns)
	s.createV1Alpha1Cluster(ctx, ctl, newCluster)

	// The public operator should reconcile it and create a StatefulSet.
	s.T().Log("Waiting for public operator to create StatefulSet for new v1alpha1 cluster...")
	s.waitForStatefulSetReady(ctx, ctl, clusterCoexistNew, newCluster.Spec.Nodes)

	// The enterprise operator should not create CrdbNodes for either cluster.
	s.assertClusterNodeCount(ctx, ctl, clusterCoexistPre, ns, 0)
	s.assertClusterNodeCount(ctx, ctl, clusterCoexistNew, ns, 0)
}

// ---------------------------------------------------------------------------
// Coexistence test: pure v1beta1 clusters
// ---------------------------------------------------------------------------

// TestV1Beta1NativeCluster verifies that a v1beta1 cluster created by the
// enterprise operator is invisible to the public operator. The enterprise
// mutating webhook injects the skip-reconcile label on all v1beta1 CrdbClusters.
// When the public operator reads the cluster as v1alpha1, it sees this label
// (carried via ObjectMeta) and skips reconciliation.
func (s *MigrationTestSuite) TestV1Beta1NativeCluster() {
	ctx := context.Background()
	ns := testenv.EnsureNamespace(s.T(), s.ctl)
	ctl := s.ctl.WithNamespace(ns)

	// Deploy the enterprise operator first so the v1beta1 CRD, webhooks, and
	// conversion are available.
	s.deployEnterpriseOperator(ctx, ns)

	// Verify the CRD was configured with a conversion webhook and v1alpha1 is
	// still served. Earlier tests created v1alpha1 objects, so storedVersions
	// includes v1alpha1 and the manager should register the webhook.
	s.assertCRDConversionWebhookConfigured()

	// Include the certificates and RBAC needed by the MutableOnly CrdbNodes.
	v1beta1Cluster := newV1Beta1Cluster(clusterV1Beta1Native, ns)
	kubecrdbtest.CreateCluster(s.T(), ctl, v1beta1Cluster)

	// The stored v1beta1 should have the skip-reconcile label (set by
	// mutating webhook, not by conversion).
	storedBeta := &crdbv1beta1.CrdbCluster{}
	s.Require().NoError(ctl.Get(ctx, clusterV1Beta1Native, storedBeta))
	s.Require().Equal("true", storedBeta.Labels[crdbv1alpha1.CrdbOperatorMigrationLabel],
		"mutating webhook should have injected skip-reconcile label on v1beta1 cluster")

	// The v1alpha1 view should carry the label too (ObjectMeta is preserved
	// through conversion).
	v1alpha1View := &crdbv1alpha1.CrdbCluster{}
	s.Require().NoError(ctl.Get(ctx, clusterV1Beta1Native, v1alpha1View),
		"v1beta1 cluster should be readable as v1alpha1 through conversion")
	s.Require().Equal("true", v1alpha1View.Labels[crdbv1alpha1.CrdbOperatorMigrationLabel],
		"v1alpha1 view should carry skip-reconcile label from stored v1beta1")
	s.Require().Equal(
		crdbv1alpha1.ActionStatus(crdbv1alpha1.Finished).String(),
		v1alpha1View.Status.ClusterStatus,
		"v1alpha1 view should not trigger the public operator's initialization status write",
	)

	// Poll to confirm the public operator does not create a StatefulSet.
	s.Require().Never(func() bool {
		var sts appsv1.StatefulSet
		return ctl.Get(ctx, clusterV1Beta1Native, &sts) == nil
	}, 15*time.Second, 2*time.Second,
		"public operator should not create a StatefulSet for a v1beta1-native cluster")

	// Re-fetch after the public operator has observed the object. A status write
	// through the v1alpha1 API used to persist the lightweight projection and
	// replace the full v1beta1 spec.
	storedBeta = &crdbv1beta1.CrdbCluster{}
	s.Require().NoError(ctl.Get(ctx, clusterV1Beta1Native, storedBeta))
	s.Require().Equal(testCockroachImage, storedBeta.Spec.Template.Spec.Image)
	s.Require().Len(storedBeta.Spec.Regions, 1)
	s.Require().Equal(int32(3), storedBeta.Spec.Regions[0].Nodes)
	for _, key := range crdbv1alpha1.MigrationAnnotationKeys() {
		s.Require().NotContains(storedBeta.Annotations, key,
			"fresh v1beta1 cluster should not acquire migration annotation %q", key)
	}

	// A normal v1beta1 update must retain the complete spec while the public
	// operator is still installed.
	if storedBeta.Labels == nil {
		storedBeta.Labels = make(map[string]string)
	}
	storedBeta.Labels["test.cockroachlabs.com/updated"] = "true"
	s.Require().NoError(ctl.Update(ctx, storedBeta))

	updated := &crdbv1beta1.CrdbCluster{}
	s.Require().NoError(ctl.Get(ctx, clusterV1Beta1Native, updated))
	s.Require().Equal("true", updated.Labels["test.cockroachlabs.com/updated"])
	s.Require().Equal(testCockroachImage, updated.Spec.Template.Spec.Image)
	s.Require().Len(updated.Spec.Regions, 1)

	// The external manager introduces clusterSettings before changing it. This tests
	// a normal SSA upgrade without manufacturing ownership from another manager.
	s.Require().NoError(s.serverSideApplyAsManager(ctx, ctl, clusterV1Beta1Native,
		func(c *crdbv1beta1.CrdbCluster) {
			c.Spec.ClusterSettings = map[string]string{
				"sql.defaults.results_buffer.size": "32KiB",
			}
		}),
		"SSA install of a fresh v1beta1 cluster should not conflict")
	s.Require().NoError(s.serverSideApplyAsManager(ctx, ctl, clusterV1Beta1Native,
		func(c *crdbv1beta1.CrdbCluster) {
			c.Spec.ClusterSettings = map[string]string{
				"sql.defaults.results_buffer.size": "16KiB",
			}
		}),
		"SSA upgrade of a fresh v1beta1 cluster should not conflict")
	s.assertNoOperatorInjectedAnnotations(ctx, ctl, clusterV1Beta1Native)
}

// serverSideApplyAsManager simulates an external config manager (Helm, Argo CD,
// Flux, kubectl apply --server-side) owning the CrdbCluster through Server-Side
// Apply with its own field manager. It reads the current spec so the apply is a
// complete, valid MutableOnly spec, applies an optional mutation, and applies as
// field manager "config-manager" without forcing. A field-ownership conflict with
// the operator surfaces as an error.
func (s *MigrationTestSuite) serverSideApplyAsManager(
	ctx context.Context, ctl *kube.Ctl, name string, mutate func(*crdbv1beta1.CrdbCluster),
) error {
	current := &crdbv1beta1.CrdbCluster{}
	s.Require().NoError(ctl.Get(ctx, name, current))

	apply := &crdbv1beta1.CrdbCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: crdbv1beta1.GroupVersion.String(),
			Kind:       crdbv1beta1.CrdbClusterKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      current.Name,
			Namespace: current.Namespace,
			Annotations: map[string]string{
				"meta.helm.sh/release-name":      "cockroachdb",
				"meta.helm.sh/release-namespace": current.Namespace,
			},
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "Helm",
			},
		},
		Spec: current.Spec,
	}
	if mutate != nil {
		mutate(apply)
	}
	applyMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(apply)
	if err != nil {
		return err
	}
	// Template metadata marshals a null creationTimestamp that is not in the CRD schema.
	unstructured.RemoveNestedField(applyMap, "spec", "template", "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(
		applyMap,
		"spec", "template", "spec", "dataStore", "volumeClaimTemplate", "metadata", "creationTimestamp",
	)
	applyObject := &unstructured.Unstructured{Object: applyMap}

	cl, err := ctrlclient.New(s.env.DefaultCluster().Config, ctrlclient.Options{Scheme: crdbv1beta1.Scheme})
	if err != nil {
		return err
	}
	return cl.Patch(ctx, applyObject, ctrlclient.Apply, ctrlclient.FieldOwner("config-manager"))
}

// assertNoOperatorInjectedAnnotations fails if the cluster still carries any
// operator-injected migration annotation. After migration the cluster controller
// strips these so an external manager can own the object without SSA conflicts.
func (s *MigrationTestSuite) assertNoOperatorInjectedAnnotations(
	ctx context.Context, ctl *kube.Ctl, name string,
) {
	cluster := &crdbv1beta1.CrdbCluster{}
	s.Require().NoError(ctl.Get(ctx, name, cluster))
	for _, key := range crdbv1alpha1.OperatorInjectedMigrationAnnotationKeys() {
		s.Require().NotContains(cluster.Annotations, key,
			"operator-injected annotation %q should be stripped after migration", key)
	}
}

// ---------------------------------------------------------------------------
// v1beta1 cluster builder
// ---------------------------------------------------------------------------

// newV1Beta1Cluster creates a minimal v1beta1 CrdbCluster for coexistence testing.
func newV1Beta1Cluster(name, namespace string) *crdbv1beta1.CrdbCluster {
	mode := crdbv1beta1.MutableOnly
	return &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Mode:       &mode,
			TLSEnabled: true,
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east-1", Nodes: 3, CloudProvider: "k3d"},
			},
			Template: crdbv1beta1.CrdbNodeTemplate{
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image: testCockroachImage,
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// v1alpha1 cluster builder
// ---------------------------------------------------------------------------

// newV1Alpha1Cluster creates a v1alpha1 CrdbCluster with comprehensive field
// coverage and TLS enabled. The region code annotation is included at creation
// time because the enterprise CRD with its conversion webhook persists across
// tests. Without the annotation the webhook uses the default "us-east1" which
// doesn't match the k3d cluster's "us-east-1" and the creation is rejected.
func newV1Alpha1Cluster(name, namespace string) *crdbv1alpha1.CrdbCluster {
	nodes := int32(3)
	grpcPort := int32(26258)
	httpPort := int32(8080)
	sqlPort := int32(26257)
	maxUnavailable := int32(1)
	terminationGrace := int64(600)

	return &crdbv1alpha1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Annotations: map[string]string{
				crdbv1alpha1.BetaCrdbClusterNodeRegionCodeAnnotationKey: "us-east-1",
			},
		},
		Spec: crdbv1alpha1.CrdbClusterSpec{
			Nodes: nodes,
			Image: &crdbv1alpha1.PodImage{
				Name: testCockroachImage,
			},
			GRPCPort:       &grpcPort,
			HTTPPort:       &httpPort,
			SQLPort:        &sqlPort,
			TLSEnabled:     true,
			Cache:          "30%",
			MaxSQLMemory:   "30%",
			MaxUnavailable: &maxUnavailable,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
				},
			},
			DataStore: crdbv1alpha1.Volume{
				VolumeClaim: &crdbv1alpha1.VolumeClaim{
					PersistentVolumeClaimSpec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						VolumeMode:  new(corev1.PersistentVolumeFilesystem),
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("10Gi"),
							},
						},
					},
				},
			},
			PodEnvVariables: []corev1.EnvVar{
				{Name: "CUSTOM_ENV_VAR", Value: "migration-test-value"},
			},
			TerminationGracePeriodSecs: terminationGrace,
		},
	}
}

// ---------------------------------------------------------------------------
// Label and annotation helpers
// ---------------------------------------------------------------------------

// annotateRegionCode adds the region code annotation needed by the enterprise
// conversion webhook. Without this the default region "us-east1" doesn't match
// the k3d cluster's "us-east-1" and the webhook rejects the request.
func (s *MigrationTestSuite) annotateRegionCode(ctx context.Context, ctl *kube.Ctl, name string) {
	v1alpha1 := &crdbv1alpha1.CrdbCluster{}
	s.Require().NoError(ctl.Get(ctx, name, v1alpha1))

	if v1alpha1.Annotations == nil {
		v1alpha1.Annotations = make(map[string]string)
	}
	v1alpha1.Annotations[crdbv1alpha1.BetaCrdbClusterNodeRegionCodeAnnotationKey] = "us-east-1"
	s.Require().NoError(ctl.Update(ctx, v1alpha1))
}

// applySkipReconcile tells the public operator to stop reconciling this cluster.
func (s *MigrationTestSuite) applySkipReconcile(ctx context.Context, ctl *kube.Ctl, name string) {
	v1alpha1 := &crdbv1alpha1.CrdbCluster{}
	s.Require().NoError(ctl.Get(ctx, name, v1alpha1))

	if v1alpha1.Labels == nil {
		v1alpha1.Labels = make(map[string]string)
	}
	v1alpha1.Labels[migrationctrl.LabelSkipReconcile] = "true"
	s.Require().NoError(ctl.Update(ctx, v1alpha1))
}

// setMigrationLabel updates only the migration label value on the v1alpha1 CrdbCluster.
func (s *MigrationTestSuite) setMigrationLabel(ctx context.Context, ctl *kube.Ctl, name, value string) {
	v1alpha1 := &crdbv1alpha1.CrdbCluster{}
	s.Require().NoError(ctl.Get(ctx, name, v1alpha1))

	if v1alpha1.Labels == nil {
		v1alpha1.Labels = make(map[string]string)
	}
	v1alpha1.Labels[migrationctrl.LabelMigrate] = value
	s.Require().NoError(ctl.Update(ctx, v1alpha1))
}

// ---------------------------------------------------------------------------
// Wait helpers
// ---------------------------------------------------------------------------

// waitForStatefulSetReady polls until the STS has the expected ready replicas.
func (s *MigrationTestSuite) waitForStatefulSetReady(
	ctx context.Context, ctl *kube.Ctl, name string, expected int32,
) {
	s.Require().Eventually(func() bool {
		var sts appsv1.StatefulSet
		if err := ctl.Get(ctx, name, &sts); err != nil {
			return false
		}
		return sts.Status.ReadyReplicas == expected && sts.Status.Replicas == expected
	}, 5*time.Minute, 5*time.Second, "StatefulSet %s should have %d ready replicas", name, expected)
}

// waitForMigrationPhase polls until the CrdbCluster migration reaches the target phase.
func (s *MigrationTestSuite) waitForMigrationPhase(
	ctx context.Context, ctl *kube.Ctl, name, targetPhase string, timeout time.Duration,
) {
	s.Require().Eventually(func() bool {
		cluster := &crdbv1beta1.CrdbCluster{}
		if err := ctl.Get(ctx, name, cluster); err != nil {
			return false
		}
		if cluster.Status.Migration == nil {
			return false
		}
		s.T().Logf("  %s: phase=%s migrated=%d/%d",
			name, cluster.Status.Migration.Phase,
			len(cluster.Status.Migration.MigratedNodes),
			cluster.Status.Migration.TotalPods)
		return cluster.Status.Migration.Phase == targetPhase
	}, timeout, 5*time.Second, "migration for %s should reach phase %s", name, targetPhase)
}

// waitForMigrationHandoffComplete waits for Mode, status, and the terminal label.
// Waiting for PhaseComplete alone races the following label update.
func (s *MigrationTestSuite) waitForMigrationHandoffComplete(
	ctx context.Context, ctl *kube.Ctl, name string, timeout time.Duration,
) {
	s.Require().Eventually(func() bool {
		cluster := &crdbv1beta1.CrdbCluster{}
		if err := ctl.Get(ctx, name, cluster); err != nil {
			return false
		}
		m := cluster.Status.Migration
		if m == nil {
			return false
		}
		s.T().Logf("  %s: phase=%s migrate=%s migration-status=%s",
			name, m.Phase, cluster.Labels[migrationctrl.LabelMigrate],
			cluster.Labels[migrationctrl.LabelMigrationStatus])
		return m.Phase == migrationctrl.PhaseComplete &&
			cluster.Spec.Mode != nil && *cluster.Spec.Mode == crdbv1beta1.MutableOnly &&
			cluster.Labels[migrationctrl.LabelMigrate] == migrationctrl.MigrateComplete
	}, timeout, 5*time.Second,
		"migration for %s should fully hand off (Mode=MutableOnly, PhaseComplete, migrate=complete)", name)
}

// waitForMigratedNodeCount polls until at least minCount nodes have been migrated.
func (s *MigrationTestSuite) waitForMigratedNodeCount(
	ctx context.Context, ctl *kube.Ctl, name string, minCount int, timeout time.Duration,
) {
	s.Require().Eventually(func() bool {
		cluster := &crdbv1beta1.CrdbCluster{}
		if err := ctl.Get(ctx, name, cluster); err != nil {
			return false
		}
		return cluster.Status.Migration != nil && len(cluster.Status.Migration.MigratedNodes) >= minCount
	}, timeout, 5*time.Second, "cluster %s should have at least %d migrated nodes", name, minCount)
}

// ---------------------------------------------------------------------------
// Assertion helpers
// ---------------------------------------------------------------------------

// assertFinalizationState verifies the migration status at the Finalization phase.
// At this point all pods have been migrated but the STS hasn't been deleted yet.
func (s *MigrationTestSuite) assertFinalizationState(
	ctx context.Context, ctl *kube.Ctl, name string, expectedNodes int,
) {
	cluster := &crdbv1beta1.CrdbCluster{}
	s.Require().NoError(ctl.Get(ctx, name, cluster))

	migration := cluster.Status.Migration
	s.Require().NotNil(migration)
	s.Require().Equal(migrationctrl.PhaseFinalization, migration.Phase)
	s.Require().Equal(int32(expectedNodes), migration.TotalPods)
	s.Require().Len(migration.MigratedNodes, expectedNodes)
	s.Require().NotNil(migration.StartTime)
	s.Require().Equal(migrationctrl.MigrationTypeOperator, migration.Type)

	// Mode is not set during Finalization. completeMigration sets Mode=MutableOnly
	// after the user deletes the StatefulSet.
	if cluster.Spec.Mode != nil {
		s.Require().NotEqual(crdbv1beta1.MutableOnly, *cluster.Spec.Mode,
			"Mode should not be MutableOnly until STS is deleted")
	}
}

// assertMigrationComplete verifies PhaseComplete, Mode=MutableOnly, and correct node count.
func (s *MigrationTestSuite) assertMigrationComplete(
	ctx context.Context, ctl *kube.Ctl, name, namespace string, expectedNodes int,
) {
	cluster := &crdbv1beta1.CrdbCluster{}
	s.Require().NoError(ctl.Get(ctx, name, cluster))
	s.Require().NotNil(cluster.Status.Migration)
	s.Require().Equal(migrationctrl.PhaseComplete, cluster.Status.Migration.Phase)
	s.Require().NotNil(cluster.Spec.Mode)
	s.Require().Equal(crdbv1beta1.MutableOnly, *cluster.Spec.Mode)
	s.Require().Equal(migrationctrl.MigrateComplete, cluster.Labels[migrationctrl.LabelMigrate],
		"migrate label should be complete on the v1beta1 object after migration")
	s.Require().Equal(migrationctrl.MigrateComplete, cluster.Labels[migrationctrl.LabelMigrationStatus],
		"internal migration status label should be complete after migration")
	s.assertClusterNodeCount(ctx, ctl, name, namespace, expectedNodes)
}

// assertMigrationStatus verifies detailed migration status fields after completion.
func (s *MigrationTestSuite) assertMigrationStatus(
	ctx context.Context, ctl *kube.Ctl, name string, expectedNodes int,
) {
	cluster := &crdbv1beta1.CrdbCluster{}
	s.Require().NoError(ctl.Get(ctx, name, cluster))

	migration := cluster.Status.Migration
	s.Require().NotNil(migration)
	s.Require().Equal(migrationctrl.PhaseComplete, migration.Phase)
	s.Require().Equal(migrationctrl.MigrationTypeOperator, migration.Type)
	s.Require().Equal(int32(expectedNodes), migration.TotalPods)
	s.Require().Len(migration.MigratedNodes, expectedNodes)
	s.Require().NotNil(migration.StartTime)
	s.Require().Empty(migration.LastError)

	// The migration label should be updated to "complete" on the v1alpha1 resource.
	v1alpha1 := &crdbv1alpha1.CrdbCluster{}
	if err := ctl.Get(ctx, name, v1alpha1); err == nil {
		s.Require().Equal(migrationctrl.MigrateComplete, v1alpha1.Labels[migrationctrl.LabelMigrate])
	}
}

// assertClusterSpec verifies the v1beta1 CrdbCluster spec matches the original v1alpha1.
func (s *MigrationTestSuite) assertClusterSpec(
	ctx context.Context, ctl *kube.Ctl, name string, v1alpha1 *crdbv1alpha1.CrdbCluster,
) {
	cluster := &crdbv1beta1.CrdbCluster{}
	s.Require().NoError(ctl.Get(ctx, name, cluster))

	s.Require().Equal(v1alpha1.Spec.TLSEnabled, cluster.Spec.TLSEnabled)
	s.Require().NotEmpty(cluster.Spec.Regions)
	s.Require().Equal(v1alpha1.Spec.Nodes, cluster.Spec.Regions[0].Nodes)
	s.Require().Equal(v1alpha1.Spec.Image.Name, cluster.Spec.Template.Spec.Image)
}

// assertNodeSpecs verifies each CrdbNode's spec matches the expected values from the
// v1alpha1 cluster spec and the STS pod template.
func (s *MigrationTestSuite) assertNodeSpecs(
	ctx context.Context, ctl *kube.Ctl, name, namespace string,
	v1alpha1 *crdbv1alpha1.CrdbCluster,
) {
	nodes := s.listClusterNodes(ctx, ctl, name, namespace)
	s.Require().Len(nodes, int(v1alpha1.Spec.Nodes))

	for i := range nodes {
		node := &nodes[i]

		s.Require().Equal(v1alpha1.Spec.Image.Name, node.Spec.Image,
			"node %s: image mismatch", node.Name)

		s.Require().NotNil(node.Spec.GRPCPort)
		s.Require().Equal(*v1alpha1.Spec.GRPCPort, *node.Spec.GRPCPort,
			"node %s: GRPCPort mismatch", node.Name)

		s.Require().NotNil(node.Spec.SQLPort)
		s.Require().Equal(*v1alpha1.Spec.SQLPort, *node.Spec.SQLPort,
			"node %s: SQLPort mismatch", node.Name)

		s.Require().NotNil(node.Spec.HTTPPort)
		s.Require().Equal(*v1alpha1.Spec.HTTPPort, *node.Spec.HTTPPort,
			"node %s: HTTPPort mismatch", node.Name)

		s.Require().Equal(v1alpha1.Spec.TLSEnabled, node.Spec.TLSEnabled,
			"node %s: TLSEnabled mismatch", node.Name)

		if v1alpha1.Spec.TLSEnabled {
			s.Require().NotNil(node.Spec.Certificates.ExternalCertificates,
				"node %s: TLS cluster should have ExternalCertificates", node.Name)
		} else {
			s.Require().Nil(node.Spec.Certificates.ExternalCertificates,
				"node %s: insecure cluster should not have ExternalCertificates", node.Name)
		}

		s.Require().NotNil(node.Spec.DataStore.VolumeClaimTemplate,
			"node %s: DataStore should have VolumeClaimTemplate", node.Name)

		// Verify the cockroachdb container has the expected resources and env vars.
		s.Require().NotNil(node.Spec.PodTemplate)
		s.Require().NotEmpty(node.Spec.PodTemplate.Spec.Containers)
		crdbContainer := node.Spec.PodTemplate.Spec.Containers[0]
		s.Require().Equal("cockroachdb", crdbContainer.Name,
			"node %s: first container should be cockroachdb", node.Name)

		s.Require().Equal(v1alpha1.Spec.Resources.Requests[corev1.ResourceCPU],
			crdbContainer.Resources.Requests[corev1.ResourceCPU],
			"node %s: CPU request mismatch", node.Name)
		s.Require().Equal(v1alpha1.Spec.Resources.Limits[corev1.ResourceMemory],
			crdbContainer.Resources.Limits[corev1.ResourceMemory],
			"node %s: memory limit mismatch", node.Name)

		s.assertEnvVarPresent(crdbContainer.Env, "CUSTOM_ENV_VAR", "migration-test-value",
			"node %s", node.Name)

		s.assertStartFlagPresent(node.Spec.StartFlags, "--cache", v1alpha1.Spec.Cache,
			"node %s", node.Name)
		s.assertStartFlagPresent(node.Spec.StartFlags, "--max-sql-memory", v1alpha1.Spec.MaxSQLMemory,
			"node %s", node.Name)

		if node.Spec.PodTemplate.Spec.TerminationGracePeriodSeconds != nil {
			s.Require().Equal(v1alpha1.Spec.TerminationGracePeriodSecs,
				*node.Spec.PodTemplate.Spec.TerminationGracePeriodSeconds,
				"node %s: termination grace period mismatch", node.Name)
		}

		s.Require().Equal(name, node.Labels[manifests.BetaCrdbClusterNodeOwnerLabelKey],
			"node %s: cluster owner label mismatch", node.Name)
	}
}

// assertEnvVarPresent checks that a named env var with the expected value exists.
func (s *MigrationTestSuite) assertEnvVarPresent(
	envVars []corev1.EnvVar, name, value string, msgAndArgs ...any,
) {
	for _, env := range envVars {
		if env.Name == name {
			s.Require().Equal(value, env.Value, msgAndArgs...)
			return
		}
	}
	s.Require().Failf("env var not found",
		"expected env var %s=%s", name, value)
}

// assertStartFlagPresent checks that a start flag like "--flagName=expectedValue" exists
// in the Flags.Upsert slice.
func (s *MigrationTestSuite) assertStartFlagPresent(
	flags *crdbv1beta1.Flags, flagName, expectedValue string, msgAndArgs ...any,
) {
	s.Require().NotNil(flags, msgAndArgs...)
	expected := fmt.Sprintf("%s=%s", flagName, expectedValue)
	if slices.Contains(flags.Upsert, expected) {
		return
	}
	s.Require().Failf("start flag not found",
		"expected %s in StartFlags.Upsert %v", expected, flags.Upsert)
}

// assertNoStatefulSet verifies no StatefulSet exists for the cluster.
func (s *MigrationTestSuite) assertNoStatefulSet(ctx context.Context, ctl *kube.Ctl, name string) {
	var sts appsv1.StatefulSet
	err := ctl.Get(ctx, name, &sts)
	s.Require().Error(err, "StatefulSet should not exist after migration")
}

// assertServicesExist verifies the public service was created during migration.
func (s *MigrationTestSuite) assertServicesExist(ctx context.Context, ctl *kube.Ctl, name string) {
	var publicSvc corev1.Service
	s.Require().NoError(ctl.Get(ctx, fmt.Sprintf("%s-public", name), &publicSvc),
		"public service should exist")

	portNames := make(map[string]bool)
	for _, port := range publicSvc.Spec.Ports {
		portNames[port.Name] = true
	}
	s.Require().True(portNames["grpc"] || portNames["cockroach"],
		"public service should have a grpc/cockroach port")
	s.Require().True(portNames["http"] || portNames["http-ui"],
		"public service should have an http port")
}

// assertClusterNodeCount verifies a cluster has the expected number of CrdbNodes.
func (s *MigrationTestSuite) assertClusterNodeCount(
	ctx context.Context, ctl *kube.Ctl, name, namespace string, expectedCount int,
) {
	nodes := s.listClusterNodes(ctx, ctl, name, namespace)
	s.Require().Len(nodes, expectedCount,
		"cluster %s should have %d CrdbNodes", name, expectedCount)
}

// listClusterNodes returns all CrdbNodes belonging to a cluster.
func (s *MigrationTestSuite) listClusterNodes(
	ctx context.Context, ctl *kube.Ctl, name, namespace string,
) []crdbv1beta1.CrdbNode {
	allNodes := &crdbv1beta1.CrdbNodeList{}
	s.Require().NoError(ctl.List(ctx, &kube.ListOptions{Namespace: namespace}, allNodes))

	var clusterNodes []crdbv1beta1.CrdbNode
	for _, node := range allNodes.Items {
		if node.Labels[manifests.BetaCrdbClusterNodeOwnerLabelKey] == name {
			clusterNodes = append(clusterNodes, node)
		}
	}
	return clusterNodes
}

// assertCRDConversionWebhookConfigured verifies that the enterprise operator
// configured the CRD with a conversion webhook and left v1alpha1 served. The
// public operator installed v1alpha1 objects before the enterprise operator
// started, so v1alpha1 remains in storedVersions and the manager should
// register the conversion webhook and keep v1alpha1 served.
func (s *MigrationTestSuite) assertCRDConversionWebhookConfigured() {
	var crd apiextv1.CustomResourceDefinition
	s.Require().NoError(s.ctl.Get(context.Background(), "crdbclusters.crdb.cockroachlabs.com", &crd))

	s.Require().NotNil(crd.Spec.Conversion,
		"CRD should have conversion config when v1alpha1 objects exist in storage")
	s.Require().Equal(apiextv1.WebhookConverter, crd.Spec.Conversion.Strategy)
	s.Require().NotNil(crd.Spec.Conversion.Webhook)
	s.Require().NotNil(crd.Spec.Conversion.Webhook.ClientConfig)
	s.Require().NotNil(crd.Spec.Conversion.Webhook.ClientConfig.Service)
	s.Assert().Equal(manifests.WebhookServiceName,
		crd.Spec.Conversion.Webhook.ClientConfig.Service.Name)

	for _, v := range crd.Spec.Versions {
		switch v.Name {
		case "v1alpha1":
			s.Assert().True(v.Served,
				"v1alpha1 should be served when objects exist in storage")
		case "v1beta1":
			s.Assert().True(v.Storage,
				"v1beta1 should be the storage version")
		}
	}

	s.Assert().Contains(crd.Status.StoredVersions, "v1alpha1",
		"storedVersions should include v1alpha1 from the public operator era")
}

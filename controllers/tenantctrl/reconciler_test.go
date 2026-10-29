package tenantctrl_test

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"slices"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/tenantctrl"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kuberecord"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubenet"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/testenv"
	"github.com/stretchr/testify/suite"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestIntegrationTenant(t *testing.T) {
	// These tests rely on the reconciler requeing due to draining pods.
	testutils.MarkIntegrationTest(t)

	suite.Run(t, &TenantTestSuite{name: "default"})
}

func TestIntegrationTenantWithAdhocPods(t *testing.T) {
	// These tests rely on the reconciler requeing due to draining pods.
	testutils.MarkIntegrationTest(t)

	suite.Run(t, &TenantTestSuite{
		name:     "with-adhoc-pods",
		features: []crdbv1beta1.ClusterFeature{crdbv1beta1.ClusterTenantCreateAdhocPods},
	})
}

type TenantTestSuite struct {
	suite.Suite
	name       string
	ctl        *kube.Ctl
	ciliumCNI  bool
	metricsMgr *metrics.TestTenantManager
	features   []crdbv1beta1.ClusterFeature
}

var defaultResourceList corev1.ResourceRequirements

func (s *TenantTestSuite) SetupSuite() {
	s.metricsMgr = &metrics.TestTenantManager{}
	env := testenv.New(s.T(), testenv.MultiZoneConfig, testenv.TenantMetricsManager(s.metricsMgr))

	storageReq, ok := manifests.DefaultTenantResourceRequests[corev1.ResourceEphemeralStorage]
	s.True(ok)

	crdbCluster := testenv.CreateCrdbCluster(
		s.T(),
		env,
		fmt.Sprintf("test-cluster-%s", s.name),
		&crdbv1beta1.CrdbClusterSpec{
			Features:   s.features,
			TLSEnabled: true,
			TenantHost: &crdbv1beta1.TenantHost{
				TenantPools: []crdbv1beta1.TenantPool{
					{
						Name: "default",
						// Set a limit on the default pool. We won't hit this
						// limit unless we explicitly fill up the storage.
						// This avoids needing to update the pool again later
						// for tests that need it.
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceEphemeralStorage: storageReq,
							},
						},
					},
					{
						Name:      "faster",
						Resources: defaultResourceList,
					},
				},
			},
		})
	s.ctl = env.DefaultCluster().Ctl.WithNamespace(crdbCluster.Namespace)
	s.ciliumCNI = testenv.IsCiliumCNI(s.T(), s.ctl)
}

func (s *TenantTestSuite) TestCreate() {
	// Create a new tenant.
	const tenantID = crdbv1beta1.TenantID(10)
	ctx := context.Background()
	tenant := testenv.CreateCrdbTenant(s.T(), s.ctl, "create", &crdbv1beta1.CrdbTenantSpec{
		TenantID:         tenantID,
		Pods:             0,
		DisableAutoscale: true,
	})

	// Validate it was fully initialized.
	s.waitForInitialization(ctx, tenant)

	// Validate a finalizer was added.
	s.True(slices.Contains(tenant.Finalizers, tenantctrl.TenantFinalizerName))

	// Validate that we've been assigned to the default pool.
	s.Equal("default", *tenant.Spec.TenantPool)

	var np kube.Object = &netv1.NetworkPolicy{}
	if s.ciliumCNI {
		np = &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "cilium.io/v2",
				"kind":       "CiliumClusterwideNetworkPolicy",
				"metadata": map[string]any{
					"name": tenantID.Tag(),
				},
			},
		}
	}

	// Ensure that our network policy was created as expected.
	s.NoError(s.ctl.Get(ctx, tenantID.Tag(), np))
	// Ensure that the network policy is owned by this tenant.
	s.Equal(tenant.UID, np.GetOwnerReferences()[0].UID)

	// Validate the tenant actually exists in the host cluster.
	const stmt = `
	SELECT
	CASE
		WHEN NOT EXISTS(SELECT * FROM system.tenants WHERE id = $1)
		THEN crdb_internal.force_error('', 'expected tenant to be created'::STRING)
	END
`
	testenv.ExecKubeSql(s.T(), s.ctl, kubenet.Options{}, stmt, tenantID)

	// Apply the tenant again, expecting it to be idempotent.
	s.NoError(s.ctl.Apply(ctx, tenant))
}

func (s *TenantTestSuite) TestCreateNonDefaultPool() {
	// Create a new tenant.
	const tenantID = crdbv1beta1.TenantID(11)
	ctx := context.Background()
	tenantPool := "faster"
	tenant := testenv.CreateCrdbTenant(s.T(), s.ctl, "pool", &crdbv1beta1.CrdbTenantSpec{
		TenantID:   tenantID,
		Pods:       0,
		TenantPool: &tenantPool,
	})

	// Validate it was fully initialized.
	s.waitForInitialization(ctx, tenant)

	// Validate a finalizer was added.
	s.True(slices.Contains(tenant.Finalizers, tenantctrl.TenantFinalizerName))

	// Validate that we've been assigned to the desired pool.
	s.Equal("faster", *tenant.Spec.TenantPool)

	// Validate the tenant actually exists in the host cluster.
	const stmt = `
	SELECT
	CASE
		WHEN NOT EXISTS(SELECT * FROM system.tenants WHERE id = $1)
		THEN crdb_internal.force_error('', 'expected tenant to be created'::STRING)
	END
`
	testenv.ExecKubeSql(s.T(), s.ctl, kubenet.Options{}, stmt, tenantID)

	// Apply the tenant again, expecting it to be idempotent.
	s.NoError(s.ctl.Apply(ctx, tenant))
	s.NoError(s.ctl.Delete(ctx, tenant))
}

func (s *TenantTestSuite) TestCreateNonExistentPool() {
	// Create a new tenant.
	ctx := context.Background()

	const tenantID = crdbv1beta1.TenantID(12)
	tenantPool := "fastest"
	err := s.ctl.Apply(ctx, &crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name: tenantID.Tag(),
		},
		Spec: crdbv1beta1.CrdbTenantSpec{
			TenantID:   tenantID,
			Pods:       0,
			TenantPool: &tenantPool,
		},
	})

	s.Contains(err.Error(), "pool fastest does not exist")
}

func (s *TenantTestSuite) TestDelete() {
	// Create a new tenant.
	const tenantID = crdbv1beta1.TenantID(20)
	ctx := context.Background()
	tenant := testenv.CreateCrdbTenant(s.T(), s.ctl, "delete", &crdbv1beta1.CrdbTenantSpec{
		TenantID:         tenantID,
		Pods:             1,
		DisableAutoscale: true,
	})

	// Get the pod associated with the tenant.
	selector := manifests.MakeStampedPodSelector(tenantID)
	var pods corev1.PodList
	s.NoError(s.ctl.List(ctx, &kube.ListOptions{LabelSelector: selector}, &pods))
	s.Len(pods.Items, 1)

	// Now delete the tenant.
	s.NoError(s.ctl.Delete(ctx, tenant))

	// Validate the tenant no longer exists in the host cluster.
	const stmt = `
	SELECT
	CASE
		WHEN EXISTS(SELECT * FROM system.tenants WHERE id = $1 AND active)
		THEN crdb_internal.force_error('', 'expected tenant to be deleted'::STRING)
	END
`
	testenv.ExecKubeSql(s.T(), s.ctl, kubenet.Options{}, stmt, tenantID)

	// Ensure that the tenant's pod is automatically terminated.
	s.Eventually(func() bool {
		pod := pods.Items[0]
		err := s.ctl.Get(ctx, pod.Name, &pod)
		return kubeerrors.IsNotFound(err) || pod.DeletionTimestamp != nil
	}, time.Second*10, time.Millisecond*10)

	// Ensure that the tenant's network policy is garbage collected.
	s.Eventually(func() bool {
		var np kube.Object = &netv1.NetworkPolicy{}
		if s.ciliumCNI {
			np = &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "cilium.io/v2",
					"kind":       "CiliumClusterwideNetworkPolicy",
					"metadata": map[string]any{
						"name": tenantID.Tag(),
					},
				},
			}
		}
		err := s.ctl.Get(ctx, tenantID.Tag(), np)
		return kubeerrors.IsNotFound(err)
	}, time.Second*10, time.Millisecond*10)
}

func (s *TenantTestSuite) TestScale() {
	// Create a new tenant.
	const tenantID = crdbv1beta1.TenantID(30)
	ctx := context.Background()
	tenant := testenv.CreateCrdbTenant(s.T(), s.ctl, "test-scale", &crdbv1beta1.CrdbTenantSpec{
		TenantID:         tenantID,
		Pods:             0,
		DisableAutoscale: true,
	})

	// Wait until tenant is fully initialized, with at least three pods in the
	// tenant pool.
	s.waitForInitialization(ctx, tenant)
	s.waitForQuiescentPool(ctx, "default")

	// Resume the tenant.
	tenant.Spec.Pods = 3
	s.NoError(s.ctl.Apply(ctx, tenant))

	// Create a SQL user in the tenant.
	testenv.ExecKubeSql(s.T(), s.ctl, kubenet.Options{
		TenantID: tenantID,
	}, "CREATE USER IF NOT EXISTS me WITH PASSWORD 'password'")

	// Connect through the SQL proxy.
	testenv.TestKubeSql(s.T(), s.ctl, kubenet.Options{
		TenantID:      tenantID,
		TenantCluster: "test-scale",
		User:          url.UserPassword("me", "password"),
	})

	// Validate the stamped pods.
	stampedPods := s.validatePods(
		ctx, tenant, expectedPods{spec: 3, draining: 0})

	// Now mark one of the tenant pods as PENDING, in order to simulate a crash
	// happening before fully starting a pod. Ensure the PENDING pod is resumed.
	s.NoError(manifests.SetTenantPodState(&stampedPods[0], manifests.TenantPodPendingState))
	s.NoError(s.ctl.Apply(ctx, &stampedPods[0]))
	tenant.Status.Pods = 0
	s.NoError(s.ctl.UpdateStatus(ctx, tenant))
	s.validatePods(ctx, tenant, expectedPods{spec: 3})

	// Suspend the tenant by explicitly setting Spec.Pods = 0, which will cause
	// the remaining pod to be marked as DRAINING.
	tenant.Spec.Pods = 0
	s.NoError(s.ctl.Apply(ctx, tenant))
	s.validatePods(ctx, tenant, expectedPods{draining: 3})

	// Resume the tenant by setting Spec.Pods = 1, which should reuse a
	// DRAINING pod.
	tenant.Spec.Pods = 1
	s.NoError(s.ctl.Apply(ctx, tenant))
	s.validatePods(ctx, tenant, expectedPods{spec: 1, draining: 2})

	tenant.Spec.Pods = 0
	s.NoError(s.ctl.Apply(ctx, tenant))
	// This times out occasionally when ran as part of the nightly integration tests.
	// The Apply above, changes the three pods to DRAINING. Locally the three
	// pods terminate after 1m. When ran as part of nightly integration tests,
	// the first pod terminates after 1m, the second 30s after and the third 30s
	// after the second, so it takes 2m+ for the validation.
	s.validatePods(ctx, tenant, expectedPods{})
}

func (s *TenantTestSuite) TestStalePodsRollOver() {
	const tenantID = crdbv1beta1.TenantID(35)
	ctx := context.Background()
	tenant := testenv.CreateCrdbTenant(s.T(), s.ctl, "test-stale-rollover", &crdbv1beta1.CrdbTenantSpec{
		TenantID:         tenantID,
		Pods:             2,
		DisableAutoscale: true,
	})
	s.validatePods(ctx, tenant, expectedPods{spec: 2})

	// Mark the running pods as stale.
	// This should trigger tenant reconciliation and initiate the stale pods rolling over.
	list := &corev1.PodList{}
	s.NoError(s.ctl.List(ctx, &kube.ListOptions{LabelSelector: manifests.MakeStampedPodSelector(tenantID)}, list))
	s.Equal(2, len(list.Items))
	for _, pod := range list.Items {
		manifests.MarkTenantPodAsStale(&pod)
		s.NoError(s.ctl.Apply(ctx, &pod))
	}

	// Exactly one pod should be marked as DRAINING, and a new pod is stamped.
	var drainingPod corev1.Pod
	s.Eventually(func() bool {
		list = &corev1.PodList{}
		s.NoError(s.ctl.List(ctx, &kube.ListOptions{LabelSelector: manifests.MakeStampedPodSelector(tenantID)}, list))
		drainingCount := 0
		for _, pod := range list.Items {
			state, _ := manifests.GetTenantPodState(&pod)
			if state == manifests.TenantPodDrainingState {
				drainingPod = pod
				drainingCount += 1
			}
		}
		return len(list.Items) == 3 && drainingCount == 1
	}, time.Minute, time.Second)

	// The DRAINING pod will eventually terminate.
	s.Eventually(func() bool {
		return kubeerrors.IsNotFound(s.ctl.Get(ctx, drainingPod.Name, &drainingPod))
	}, 2*time.Minute, time.Second)
	// The second stale pod should transition to DRAINING. And again, a new pod takes its place.
	s.validatePods(ctx, tenant, expectedPods{spec: 2, draining: 1})
	// Finally, the last stale pod is terminated.
	s.validatePods(ctx, tenant, expectedPods{spec: 2, draining: 0})
}

func (s *TenantTestSuite) TestChangePool() {
	const tenantID = crdbv1beta1.TenantID(40)
	ctx := context.Background()
	initialPool := "default"
	changePool := "faster"
	tenant := testenv.CreateCrdbTenant(s.T(), s.ctl, "test-change-pool", &crdbv1beta1.CrdbTenantSpec{
		TenantID:         tenantID,
		Pods:             1,
		TenantPool:       &initialPool,
		DisableAutoscale: true,
	})
	s.waitForInitialization(ctx, tenant)

	// Create a SQL user for the tenant.
	testenv.ExecKubeSql(s.T(), s.ctl, kubenet.Options{
		TenantID: tenantID,
	}, "CREATE USER IF NOT EXISTS me WITH PASSWORD 'password'")
	sqloptions := kubenet.Options{
		TenantID:      tenantID,
		TenantCluster: "test-change-pool",
		User:          url.UserPassword("me", "password"),
	}

	// Connect to the original pod via the sqlproxy
	testenv.TestKubeSql(s.T(), s.ctl, sqloptions)

	countStalePods := func() int {
		list := &corev1.PodList{}
		options := &kube.ListOptions{
			LabelSelector: manifests.MakeStalePoolSelector(tenantID, changePool),
		}
		s.NoError(s.ctl.List(ctx, options, list))
		return len(list.Items)
	}
	s.validatePods(ctx, tenant, expectedPods{spec: 1})
	s.Equal(1, countStalePods())

	// Change the pool the tenant belongs to
	tenant.Spec.TenantPool = &changePool
	s.NoError(s.ctl.Apply(ctx, tenant))

	// Wait for the old pod to be marked as stale.
	s.Eventually(func() bool {
		list := &corev1.PodList{}
		options := &kube.ListOptions{
			LabelSelector: manifests.MakeStampedPodSelector(tenantID),
		}
		s.NoError(s.ctl.List(ctx, options, list))
		count := 0
		for _, pod := range list.Items {
			if manifests.IsTenantPodStale(&pod) {
				count += 1
			}
		}
		return count == 1
	}, time.Minute, time.Second)
	// Also, that stale pod should be moved to DRAINING state.
	s.validatePods(ctx, tenant, expectedPods{spec: 1, draining: 1})

	// Ensure we can connect to the new pod
	testenv.TestKubeSql(s.T(), s.ctl, sqloptions)
}

// Test that evicted tenant pods are replaced.
func (s *TenantTestSuite) TestIntegrationEviction() {
	ctx := context.Background()

	// Create a new tenant.
	const numPods = 3
	const tenantID = crdbv1beta1.TenantID(45)
	tenant := testenv.CreateCrdbTenant(s.T(), s.ctl, "create", &crdbv1beta1.CrdbTenantSpec{
		TenantID:         tenantID,
		Pods:             numPods,
		DisableAutoscale: true,
	})

	// Validate pods are initialized.
	s.waitForInitialization(ctx, tenant)

	selector := manifests.MakeStampedPodSelector(tenant.Spec.TenantID)
	var pods corev1.PodList
	s.NoError(s.ctl.List(ctx, &kube.ListOptions{LabelSelector: selector}, &pods))
	s.NotEmpty(pods.Items)

	// Create large files to trigger eviction on all the pods.
	for _, pod := range pods.Items {
		var b bytes.Buffer
		s.NoError(s.ctl.Exec(ctx, pod.Name, "sqlpod-manager", kube.ExecOptions{
			Command: []string{"sh", "-c", "fallocate -l 500M manager-data/largefile"},
			Stdout:  &b,
			Stderr:  &b,
		}))
		s.Empty(b.String())
	}

	// Wait for at least one pod to be evicted.
	s.Eventually(func() bool {
		var pods corev1.PodList
		selector := manifests.MakeStampedPodSelector(tenant.Spec.TenantID)
		if err := s.ctl.List(ctx, &kube.ListOptions{LabelSelector: selector}, &pods); err != nil {
			return false
		}
		var numRunning, numFailed int
		for _, pod := range pods.Items {
			switch pod.Status.Phase {
			case corev1.PodRunning:
				numRunning++
			case corev1.PodFailed:
				numFailed++
			default:
				continue
			}
		}
		// We want the original number of pods to be running while at least one
		// should have failed.
		return numFailed > 0 && numRunning == numPods
	}, 5*time.Minute, time.Second)
}

func (s *TenantTestSuite) waitForQuiescentPool(ctx context.Context, tenantPool string) {
	// TODO(jeffswenson): waitForQuiescentPool would be unecessary if calling
	// IsQuiescent on the crdbcluster required the pools to be quiescent.
	var pool appsv1.Deployment
	s.NoError(s.ctl.Get(ctx, fmt.Sprintf("tenantpool-%s", tenantPool), &pool))
	s.ctl.WaitForWithOptions(ctx, kube.WaitForOptions{
		DoneFunc: kube.IsQuiescent,
	}, &pool)
}

// waitForInitialization spins until the given tenant has been initialized by
// the operator's tenant controller.
func (s *TenantTestSuite) waitForInitialization(
	ctx context.Context, tenant *crdbv1beta1.CrdbTenant,
) {
	s.Eventually(func() bool {
		if err := s.ctl.Get(ctx, tenant.Name, tenant); err != nil {
			return false
		}

		return tenant.Status.HasCondition(crdbv1beta1.TenantInitialized, metav1.ConditionTrue)
	}, 30*time.Second, kuberecord.MaxBackOffInterval(100*time.Millisecond)+1)
}

type expectedPods struct {
	spec     int
	draining int
}

func (s *TenantTestSuite) validatePods(
	ctx context.Context, tenant *crdbv1beta1.CrdbTenant, expected expectedPods,
) []corev1.Pod {
	logging.Infof(ctx, "validating pods: specPods=%d drainingPods=%d",
		expected.spec, expected.draining)

	// Wait for operator to reconcile. It can take time for Prometheus to begin
	// generating all the necessary metrics.
	var stampedPods corev1.PodList
	s.Eventually(func() bool {
		s.NoError(s.ctl.Get(ctx, tenant.Name, tenant))
		if tenant.Spec.Pods != int32(expected.spec) {
			logging.Infof(ctx, "tenant.Spec.Pods=%d vs. specPods=%d", tenant.Spec.Pods, expected.spec)
			return false
		}
		if tenant.Status.Pods != int32(expected.spec) {
			logging.Infof(ctx, "tenant.Status.Pods=%d vs. specPods=%d", tenant.Status.Pods, expected.spec)
			return false
		}

		// Ensure pods match expectations.
		selector := manifests.MakeStampedPodSelector(tenant.Spec.TenantID)
		s.NoError(s.ctl.List(ctx, &kube.ListOptions{LabelSelector: selector}, &stampedPods))

		totalPods := 0
		runningPods := 0
		for i := range stampedPods.Items {
			// Skip pods that are terminating.
			pod := &stampedPods.Items[i]
			if pod.DeletionTimestamp != nil {
				continue
			}
			totalPods++

			state, _ := manifests.GetTenantPodState(pod)
			if state == manifests.TenantPodRunningState {
				runningPods++
			} else {
				// A newly created pod (i.e. not a pod from the warm pool) might still be pending
				if state != manifests.TenantPodDrainingState && state != manifests.TenantPodPendingState {
					s.FailNow("failed", "unexpected tenant pod state: %v", state)
					return false
				}
			}
		}

		if expected.spec != runningPods {
			logging.Infof(ctx, "specPods=%d vs. runningPods=%d", expected.spec, runningPods)
			return false
		}
		if expected.draining != totalPods-runningPods {
			logging.Infof(ctx, "drainingPods=%d vs. totalPods-runningPods=%d",
				expected.draining, totalPods-runningPods)
			return false
		}
		return true
	}, 3*time.Minute, kuberecord.MaxBackOffInterval(time.Second)+1)

	return stampedPods.Items
}

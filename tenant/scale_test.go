package tenant

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	opmetrics "github.com/cockroachlabs/cockroach-enterprise-operator/controllers/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/tenant/tenantmock"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
	gomock "github.com/golang/mock/gomock"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

func TestPlanScale(t *testing.T) {
	ctx := context.Background()
	cluster := crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "cluster",
			Namespace:   "default",
			Annotations: map[string]string{},
		},
	}

	const tenantID crdbv1beta1.TenantID = 5
	tenant := crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantID.Tag(),
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					Name: cluster.Name,
					Kind: crdbv1beta1.CrdbClusterKind,
				},
			},
		},
		Spec: crdbv1beta1.CrdbTenantSpec{
			TenantID: tenantID,
		},
	}

	runningPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stamped-pod",
			Namespace: "default",
			Labels: labels.Set{
				"app":                         manifests.TenantPoolAppName,
				manifests.TenantIDLabel:       tenantID.String(),
				manifests.TenantPoolNameLabel: crdbv1beta1.DefaultTenantPoolName,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: manifests.CockroachDB, Ready: true},
			},
		},
	}
	require.NoError(t, manifests.SetTenantPodState(&runningPod, manifests.TenantPodRunningState))

	drainingPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stamped-pod",
			Namespace: "default",
			Labels: labels.Set{
				"app":                         manifests.TenantPoolAppName,
				manifests.TenantIDLabel:       tenantID.String(),
				manifests.TenantPoolNameLabel: crdbv1beta1.DefaultTenantPoolName,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "sqlpod-manager"},
				{Name: manifests.CockroachDB, Ready: true, State: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{},
				}},
			},
		},
	}
	require.NoError(t, manifests.SetTenantPodState(&drainingPod, manifests.TenantPodDrainingState))

	mgr := &metrics.TestTenantManager{}
	tenantKey := metrics.TenantKey{Namespace: tenant.Namespace, TenantID: tenant.Spec.TenantID}

	t.Run("scale up by stamping", func(t *testing.T) {
		tenant := tenant.DeepCopy()
		tenant.Status.Pods = 1
		tenant.Spec.Pods = 3
		runningPod1 := runningPod.DeepCopy()
		runningPod1.Name = runningPod.Name + "-1"
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &cluster, tenant, runningPod1).WithNamespace("default")
		scaler := &TenantPodScaler{
			Ctl:           ctl,
			MetricManager: mgr,
			Rng:           rand.NewSource(0),
			Emit:          &kube.EventEmitter{},
		}
		list, err := scaler.listStampedPods(ctx, tenant)
		require.NoError(t, err)
		scalePlan, err := scaler.planScale(ctx, tenant, &cluster, list)
		require.NoError(t, err)

		require.Equal(t, 2, scalePlan.newPods)
		require.Equal(t, 0, len(scalePlan.terminatePods))
		require.Equal(t, 0, len(scalePlan.drainPods))
		require.Equal(t, 0, len(scalePlan.undrainPods))
		require.Equal(t, false, scalePlan.requeue)
	})

	t.Run("scale up by undraining", func(t *testing.T) {
		tenant := tenant.DeepCopy()
		tenant.Status.Pods = 1
		tenant.Spec.Pods = 2
		runningPod1 := runningPod.DeepCopy()
		runningPod1.Name = runningPod.Name + "-1"
		drainingPod1 := drainingPod.DeepCopy()
		drainingPod1.Name = drainingPod.Name + "-2"
		drainingPod2 := drainingPod.DeepCopy()
		drainingPod2.Name = drainingPod.Name + "-3"
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &cluster, tenant, runningPod1, drainingPod1).WithNamespace("default")
		// drainingPod1 should be selected for undraining since it has higher CPU.
		tenantMetrics := []*metrics.TenantMetrics{
			{
				Key: tenantKey,
				Pods: metrics.PodMetricsMap{
					drainingPod1.Name: {SqlCpuIRate: 30, SqlConns: 5},
					drainingPod2.Name: {SqlCpuIRate: 20, SqlConns: 5},
				},
			},
		}
		mgr.UpdateMetrics(ctx, tenantMetrics)
		scaler := &TenantPodScaler{
			Ctl:           ctl,
			MetricManager: mgr,
			Rng:           rand.NewSource(0),
			Emit:          &kube.EventEmitter{},
		}
		list, err := scaler.listStampedPods(ctx, tenant)
		require.NoError(t, err)
		scalePlan, err := scaler.planScale(ctx, tenant, &cluster, list)
		require.NoError(t, err)

		require.Equal(t, 0, scalePlan.newPods)
		require.Equal(t, 0, len(scalePlan.terminatePods))
		require.Equal(t, 0, len(scalePlan.drainPods))
		require.Equal(t, 1, len(scalePlan.undrainPods))
		require.Equal(t, drainingPod1.Name, scalePlan.undrainPods[0].Name)
		require.Equal(t, false, scalePlan.requeue)
	})

	t.Run("scale down", func(t *testing.T) {
		tenant := tenant.DeepCopy()
		tenant.Status.Pods = 2
		tenant.Spec.Pods = 1
		runningPod1 := runningPod.DeepCopy()
		runningPod1.Name = runningPod.Name + "-1"
		runningPod2 := runningPod.DeepCopy()
		runningPod2.Name = runningPod.Name + "-2"
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &cluster, tenant, runningPod1, runningPod2).WithNamespace("default")
		// runningPod1 should be selected for draining since it has lower CPU.
		tenantMetrics := []*metrics.TenantMetrics{
			{
				Key: tenantKey,
				Pods: metrics.PodMetricsMap{
					runningPod1.Name: {SqlCpuIRate: 20, SqlConns: 5},
					runningPod2.Name: {SqlCpuIRate: 30, SqlConns: 5},
				},
			},
		}
		mgr.UpdateMetrics(ctx, tenantMetrics)
		scaler := &TenantPodScaler{
			Ctl:           ctl,
			MetricManager: mgr,
			Rng:           rand.NewSource(0),
			Emit:          &kube.EventEmitter{},
		}
		list, err := scaler.listStampedPods(ctx, tenant)
		require.NoError(t, err)
		scalePlan, err := scaler.planScale(ctx, tenant, &cluster, list)
		require.NoError(t, err)

		require.Equal(t, 0, scalePlan.newPods)
		require.Equal(t, 0, len(scalePlan.terminatePods))
		require.Equal(t, 1, len(scalePlan.drainPods))
		require.Equal(t, runningPod1.Name, scalePlan.drainPods[0].Name)
		require.Equal(t, 0, len(scalePlan.undrainPods))
		require.Equal(t, true, scalePlan.requeue)
	})

	t.Run("scale down to MaxPods=0", func(t *testing.T) {
		tenant := tenant.DeepCopy()
		tenant.Status.Pods = 2
		tenant.Spec.Pods = 0
		tenant.Spec.MaxPods = new(int32(0))
		runningPod1 := runningPod.DeepCopy()
		runningPod1.Name = runningPod.Name + "-1"
		runningPod2 := runningPod.DeepCopy()
		runningPod2.Name = runningPod.Name + "-2"
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &cluster, tenant, runningPod1, runningPod2).WithNamespace("default")
		scaler := &TenantPodScaler{
			Ctl:           ctl,
			MetricManager: mgr,
			Rng:           rand.NewSource(0),
			Emit:          &kube.EventEmitter{},
		}
		list, err := scaler.listStampedPods(ctx, tenant)
		require.NoError(t, err)
		scalePlan, err := scaler.planScale(ctx, tenant, &cluster, list)
		require.NoError(t, err)

		require.Equal(t, 0, scalePlan.newPods)
		require.Equal(t, 2, len(scalePlan.terminatePods))
		sort.Slice(scalePlan.terminatePods, func(i, j int) bool {
			return scalePlan.terminatePods[i].Name < scalePlan.terminatePods[j].Name
		})
		require.Equal(t, runningPod1.Name, scalePlan.terminatePods[0].Name)
		require.Equal(t, runningPod2.Name, scalePlan.terminatePods[1].Name)
		require.Equal(t, 0, len(scalePlan.drainPods))
		require.Equal(t, 0, len(scalePlan.undrainPods))
		require.Equal(t, false, scalePlan.requeue)
	})

	t.Run("terminate draining pod", func(t *testing.T) {
		tenant := tenant.DeepCopy()
		tenant.Status.Pods = 0
		tenant.Spec.Pods = 0
		drainingPod1 := drainingPod.DeepCopy()
		drainingPod1.Name = drainingPod.Name + "-1"
		drainingPod2 := drainingPod.DeepCopy()
		drainingPod2.Name = drainingPod.Name + "-2"
		drainingPod2.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			{Type: corev1.ContainersReady, Status: corev1.ConditionFalse},
		}
		drainingPod2.Status.ContainerStatuses = []corev1.ContainerStatus{
			{Name: "sqlpod-manager"},
			{Name: "cockroachdb", State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
			}},
		}
		drainingPod3 := drainingPod.DeepCopy()
		drainingPod3.Name = drainingPod.Name + "-3"
		drainingPod3.Status.ContainerStatuses = []corev1.ContainerStatus{
			{Name: "sqlpod-manager"},
		}
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &cluster, tenant, drainingPod1,
			drainingPod2, drainingPod3).WithNamespace("default")
		// 0 sql connections is a pre-req for termination
		tenantMetrics := []*metrics.TenantMetrics{
			{
				Key: tenantKey,
				Pods: metrics.PodMetricsMap{
					drainingPod1.Name: {SqlCpuIRate: 0, SqlConns: 0},
				},
			},
		}
		mgr.UpdateMetrics(ctx, tenantMetrics)

		// Simulate the draining pod being ready for termination (been in draining state for long time).
		var pod corev1.Pod
		require.NoError(t, ctl.Get(ctx, drainingPod1.Name, &pod))
		state, _ := manifests.GetTenantPodState(&pod)
		require.Equal(t, manifests.TenantPodDrainingState, state)
		ts := timeutil.Now().Add(-10 * time.Minute).UTC()
		pod.Annotations[manifests.TenantPodStateTimestampAnnotation] = ts.Format(manifests.PodStateTimestampFormat)
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, &pod))

		// Simulate pod 3 draining for some time, but no metrics.
		require.NoError(t, ctl.Get(ctx, drainingPod3.Name, &pod))
		state, _ = manifests.GetTenantPodState(&pod)
		require.Equal(t, manifests.TenantPodDrainingState, state)
		ts = timeutil.Now().Add(-10 * time.Minute).UTC()
		pod.Annotations[manifests.TenantPodStateTimestampAnnotation] = ts.Format(manifests.PodStateTimestampFormat)
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, &pod))

		scaler := &TenantPodScaler{
			Ctl:           ctl,
			MetricManager: mgr,
			Rng:           rand.NewSource(0),
			Emit:          &kube.EventEmitter{},
		}
		list, err := scaler.listStampedPods(ctx, tenant)
		require.NoError(t, err)
		scalePlan, err := scaler.planScale(ctx, tenant, &cluster, list)
		require.NoError(t, err)

		require.Equal(t, 0, scalePlan.newPods)
		// All three draining pods should be terminated:
		//   drainingPod1: 0 SQL connections, drained >= 10m
		//   drainingPod2: CrashLoopBackOff (immediate termination)
		//   drainingPod3: no metrics available, but drained >= ForceDrainTimeout
		require.Equal(t, 3, len(scalePlan.terminatePods))
		sort.Slice(scalePlan.terminatePods, func(i, j int) bool {
			return scalePlan.terminatePods[i].Name < scalePlan.terminatePods[j].Name
		})
		require.Equal(t, drainingPod1.Name, scalePlan.terminatePods[0].Name)
		require.Equal(t, drainingPod2.Name, scalePlan.terminatePods[1].Name)
		require.Equal(t, drainingPod3.Name, scalePlan.terminatePods[2].Name)
		require.Equal(t, 0, len(scalePlan.drainPods))
		require.Equal(t, 0, len(scalePlan.undrainPods))
		require.Equal(t, false, scalePlan.requeue)
	})

	t.Run("rollover stale running pod", func(t *testing.T) {
		tenant := tenant.DeepCopy()
		tenant.Status.Pods = 1
		tenant.Spec.Pods = 1
		runningPod1 := runningPod.DeepCopy()
		runningPod1.Name = runningPod.Name + "-1"
		manifests.MarkTenantPodAsStale(runningPod1)
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &cluster, tenant, runningPod1).WithNamespace("default")

		scaler := &TenantPodScaler{
			Ctl:           ctl,
			MetricManager: mgr,
			Rng:           rand.NewSource(0),
			Emit:          &kube.EventEmitter{},
		}
		list, err := scaler.listStampedPods(ctx, tenant)
		require.NoError(t, err)
		scalePlan, err := scaler.planScale(ctx, tenant, &cluster, list)
		require.NoError(t, err)

		require.Equal(t, 1, scalePlan.newPods)
		require.Equal(t, 0, len(scalePlan.terminatePods))
		require.Equal(t, 1, len(scalePlan.drainPods))
		require.Equal(t, 0, len(scalePlan.undrainPods))
		require.Equal(t, true, scalePlan.requeue)
	})

	// During a scale up, there exists a stale running pod. It is not rolled over to draining,
	// but the scalePlan does request a requeue so that it can eventually be rolled over.
	t.Run("stale running pod during scale up", func(t *testing.T) {
		tenant := tenant.DeepCopy()
		tenant.Status.Pods = 1
		tenant.Spec.Pods = 2
		runningPod1 := runningPod.DeepCopy()
		runningPod1.Name = runningPod.Name + "-1"
		manifests.MarkTenantPodAsStale(runningPod1)
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &cluster, tenant, runningPod1).WithNamespace("default")

		scaler := &TenantPodScaler{
			Ctl:           ctl,
			MetricManager: mgr,
			Rng:           rand.NewSource(0),
			Emit:          &kube.EventEmitter{},
		}
		list, err := scaler.listStampedPods(ctx, tenant)
		require.NoError(t, err)
		scalePlan, err := scaler.planScale(ctx, tenant, &cluster, list)
		require.NoError(t, err)

		require.Equal(t, 1, scalePlan.newPods)
		require.Equal(t, 0, len(scalePlan.terminatePods))
		require.Equal(t, 0, len(scalePlan.drainPods))
		require.Equal(t, 0, len(scalePlan.undrainPods))
		require.Equal(t, true, scalePlan.requeue)
	})

	t.Run("replacing unready pods", func(t *testing.T) {
		cluster := cluster.DeepCopy()
		tenant := tenant.DeepCopy()
		cluster.Annotations[manifests.MaxTenantUnreadyStampedPodCountAnnotation] = "4"
		unreadyPod1 := runningPod.DeepCopy()
		unreadyPod1.Name = "unready-pod-1"
		unreadyPod1.Status.ContainerStatuses = []corev1.ContainerStatus{
			{Name: manifests.CockroachDB, Ready: false},
		}
		unreadyPod2 := runningPod.DeepCopy()
		unreadyPod2.Name = "unready-pod-2"
		unreadyPod2.Status.ContainerStatuses = []corev1.ContainerStatus{
			{Name: manifests.CockroachDB, Ready: false},
		}

		tenant.Status.Pods = 2
		tenant.Spec.Pods = 2

		ctl := kubetest.NewFakeCtl(kubetest.Options{}, cluster, tenant, unreadyPod1, unreadyPod2).WithNamespace("default")

		scaler := &TenantPodScaler{
			Ctl:           ctl,
			MetricManager: mgr,
			Rng:           rand.NewSource(0),
			Emit:          &kube.EventEmitter{},
		}

		list, err := scaler.listStampedPods(ctx, tenant)
		require.NoError(t, err)
		scalePlan, err := scaler.planScale(ctx, tenant, cluster, list)
		require.NoError(t, err)

		require.Equal(t, 2, scalePlan.newPods)
		require.Equal(t, 0, len(scalePlan.terminatePods))
		require.Equal(t, 0, len(scalePlan.drainPods))
		require.Equal(t, 0, len(scalePlan.undrainPods))
		require.Equal(t, false, scalePlan.requeue)

		// run scaling again, with the new newly scaled pods
		runningPod1 := runningPod.DeepCopy()
		runningPod2 := runningPod.DeepCopy()
		runningPod2.Name = "running-pod-2"
		require.NoError(t, ctl.Create(ctx, runningPod1, runningPod2))

		list, err = scaler.listStampedPods(ctx, tenant)
		require.NoError(t, err)
		scalePlan, err = scaler.planScale(ctx, tenant, cluster, list)
		require.NoError(t, err)

		require.Equal(t, 0, scalePlan.newPods)
		require.Equal(t, 0, len(scalePlan.terminatePods))
		require.Equal(t, 0, len(scalePlan.drainPods))
		require.Equal(t, 0, len(scalePlan.undrainPods))
		require.Equal(t, false, scalePlan.requeue)
	})
}

func TestScaleTenant(t *testing.T) {
	cluster := crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "default",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Features: []crdbv1beta1.ClusterFeature{crdbv1beta1.ClusterTenantCreateAdhocPods},
		},
	}

	const tenantID crdbv1beta1.TenantID = 10
	tenantPool := crdbv1beta1.DefaultTenantPoolName
	tenant := crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantID.Tag(),
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: crdbv1beta1.CrdbClusterKind, Name: "cluster"},
			},
		},
		Spec: crdbv1beta1.CrdbTenantSpec{
			TenantID:   tenantID,
			Pods:       2,
			TenantPool: &tenantPool,
		},
	}

	unstampedPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unstamped-pod",
			Namespace: "default",
			Labels: labels.Set{
				"app":                         manifests.TenantPoolAppName,
				manifests.TenantPoolNameLabel: tenantPool,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: manifests.CockroachDB, Ready: true},
			},
		},
	}
	unstampedPod.UID = types.UID("unstampedPod")
	require.NoError(t, manifests.SetTenantPodState(&unstampedPod, manifests.TenantPodPendingState))

	unstampedPod2 := unstampedPod
	unstampedPod2.Name = "unstamped-pod-2"
	unstampedPod2.UID = types.UID("unstampedPod2")

	// Create stamped pod that is in PENDING state, to simulate situation where
	// a crash occurs after stamping but before starting.
	pendingPod := unstampedPod
	pendingPod.Name = "pending-pod"
	pendingPod.UID = types.UID("pendingPod")
	pendingPod.Labels = labels.Set{
		"app":                         manifests.TenantPoolAppName,
		manifests.TenantIDLabel:       tenantID.String(),
		manifests.TenantPoolNameLabel: tenantPool,
	}
	require.NoError(t, manifests.SetTenantPodState(&pendingPod, manifests.TenantPodPendingState))

	// Create a SendSqlStarterRequest function that returns the given response
	// and error.
	makeSqlStarterFunc := func(resp *http.Response, err error) func(client *http.Client, req *http.Request, pod *corev1.Pod) (*http.Response, error) {
		return func(client *http.Client, req *http.Request, pod *corev1.Pod) (*http.Response, error) {
			require.Equal(t, "POST", req.Method)
			require.Equal(t, "https://:8081/start?tenant_id=10", req.URL.String())
			require.NotEmpty(t, string(pod.UID))
			require.Equal(t, http.Header{
				"Content-Type": []string{"binary/octet-stream"},
				"X-Pod-Uid":    []string{string(pod.UID)},
			}, req.Header)
			body, bodyErr := io.ReadAll(req.Body)
			require.NoError(t, bodyErr)
			// NB: Use .Equal rather than .Len as .Len will log out the raw
			// bytes as a string and corrupt terminals.
			require.Equal(t, 3604, len(body))
			return resp, err
		}
	}

	// The operator retrieves a default configuration for the fluentbit sidecar when stamping
	// tenant pool pods.
	defaultFluentbitTenantConfig := manifests.GetTestTenantNoopConfigMap()
	defaultFluentbitTenantConfig.Namespace = "default"
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{}, &cluster, &tenant, &unstampedPod, &unstampedPod2, defaultFluentbitTenantConfig)

	scaler := TenantPodScaler{
		Ctl:                ctl,
		MetricManager:      &metrics.TestTenantManager{},
		NodeMetricManager:  &metrics.TestNodeManager{},
		Rng:                rand.NewSource(0),
		Emit:               &kube.EventEmitter{},
		NodeLocalityReader: testReadNodeLocality,
	}
	makeTenantCerts(t, ctl, tenantID)

	t.Run("bad request", func(t *testing.T) {
		defer testutils.HookGlobal(
			&SendSqlStarterRequest,
			makeSqlStarterFunc( // nolint:bodyclose
				&http.Response{
					StatusCode: http.StatusBadRequest,
					Body:       io.NopCloser(strings.NewReader("bad request")),
				},
				nil, /* err */
			),
		)()

		// this failure will not be a scale up or scale down failure
		oldMetric := opmetrics.TenantScaleFailureCount.WithLabelValues("false", "false")
		oldMetricValue := testutil.ToFloat64(oldMetric)

		_, requeueDuration, err := scaler.ScaleTenantWithMetrics(ctx, &tenant)
		require.EqualError(t, err, "sqlstarter request status 400 error: bad request")
		require.Equal(t, time.Duration(0), requeueDuration)
		var stampedPod corev1.Pod
		require.NoError(t, ctl.Get(ctx, unstampedPod2.Name, &stampedPod))
		state, _ := manifests.GetTenantPodState(&stampedPod)
		require.Equal(t, manifests.TenantPodPendingState, state)
		require.Equal(t, tenantID.String(), stampedPod.Labels[manifests.TenantIDLabel])

		testutils.RequireMetricEqual(t, opmetrics.TenantScaleFailureCount, int(oldMetricValue+1))
	})

	verifySuccess := func() {
		// Ensure tenant status was updated.
		var updatedTenant crdbv1beta1.CrdbTenant
		require.NoError(t, ctl.Get(ctx, tenant.Name, &updatedTenant))
		require.Equal(t, tenant.Spec.Pods, updatedTenant.Status.Pods)

		// Ensure pod was stamped and started.
		var stampedPod corev1.Pod
		require.NoError(t, ctl.Get(ctx, unstampedPod2.Name, &stampedPod))
		require.Equal(t, tenantID.String(), stampedPod.Labels[manifests.TenantIDLabel])
		state, _ := manifests.GetTenantPodState(&stampedPod)
		require.Equal(t, manifests.TenantPodRunningState, state)
		podRevision, ok := stampedPod.Annotations[manifests.TenantPodRevisionAnnotation]
		require.True(t, ok)
		require.Equal(t, "9a2a08575d5e39e1", podRevision)
	}

	t.Run("successful scaling", func(t *testing.T) {
		defer func() { scaler.PodSyncer = nil }()

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		var called atomic.Uint32
		tenantPodSyncer := tenantmock.NewMockTenantPodSyncer(ctrl)
		tenantPodSyncer.EXPECT().
			Sync(gomock.Any(), gomock.Any()).
			DoAndReturn(func(inTenant *crdbv1beta1.CrdbTenant, inPod *corev1.Pod) error {
				require.Equal(t, tenant.Name, inTenant.Name)
				require.Contains(t, []string{unstampedPod.Name, unstampedPod2.Name}, inPod.Name)
				// An error on Sync should not impact anything.
				if called.Add(1) == 1 {
					return errors.New("test error")
				}
				return nil
			}).
			Times(2)
		scaler.PodSyncer = tenantPodSyncer

		defer testutils.HookGlobal(
			&SendSqlStarterRequest,
			makeSqlStarterFunc( // nolint:bodyclose
				&http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("")),
				},
				nil, /* err */
			),
		)()

		oldMetric := opmetrics.TenantScaleAttemptCount
		oldMetricValue := testutil.ToFloat64(oldMetric)

		shouldRequeue, _, err := scaler.ScaleTenantWithMetrics(ctx, &tenant)
		require.NoError(t, err)
		require.False(t, shouldRequeue)
		verifySuccess()

		// Wait until goroutine terminates.
		require.Eventually(t, func() bool {
			return called.Load() == 2
		}, 1*time.Second, 50*time.Millisecond)

		// Try again, should be no-op.
		shouldRequeue, _, err = scaler.ScaleTenantWithMetrics(ctx, &tenant)
		require.NoError(t, err)
		require.False(t, shouldRequeue)
		verifySuccess()

		testutils.RequireMetricEqual(t, opmetrics.TenantScaleAttemptCount, int(oldMetricValue+2))
	})

	t.Run("tenant is missing owner", func(t *testing.T) {
		testutils.RequireMetricEqual(t, opmetrics.ActiveTenantMissingOwnerCount.WithLabelValues(tenant.Name), 0)
		tenant.OwnerReferences = []metav1.OwnerReference{}
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, &tenant))

		_, _, err := scaler.ScaleTenantWithMetrics(ctx, &tenant)
		testutils.RequireMetricEqual(t, opmetrics.ActiveTenantMissingOwnerCount.WithLabelValues(tenant.Name), 1)
		require.NoError(t, err)
	})

	t.Run("Status.Pods less than actual", func(t *testing.T) {
		tenant.Status.Pods = 0
		require.NoError(t, ctl.UpdateStatus(ctx, &tenant))

		// Calling this would just fix the status.
		shouldRequeue, _, err := scaler.ScaleTenantWithMetrics(ctx, &tenant)
		require.NoError(t, err)
		require.False(t, shouldRequeue)
		verifySuccess()
	})
}

func TestDrainTenant(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cluster := crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "default",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Features: []crdbv1beta1.ClusterFeature{crdbv1beta1.ClusterTenantCreateAdhocPods},
		},
	}

	// Create tenant with Spec.Pods = 4 and Status.Pods = 4.
	const tenantID crdbv1beta1.TenantID = 10
	const numRunningPods = 4
	tenantPool := crdbv1beta1.DefaultTenantPoolName
	tenant := crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantID.Tag(),
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: crdbv1beta1.CrdbClusterKind, Name: "cluster"},
			},
		},
		Spec: crdbv1beta1.CrdbTenantSpec{
			TenantID:   tenantID,
			Pods:       numRunningPods,
			TenantPool: &tenantPool, // TenantPool must be explicitly set during tests.

		},
		Status: crdbv1beta1.CrdbTenantStatus{
			Pods: numRunningPods,
		},
	}

	unstampedPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unstamped-pod",
			Namespace: "default",
			Labels: labels.Set{
				"app":                         manifests.TenantPoolAppName,
				manifests.TenantPoolNameLabel: crdbv1beta1.DefaultTenantPoolName,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: manifests.CockroachDB, Ready: true},
			},
		},
	}
	require.NoError(t, manifests.SetTenantPodState(&unstampedPod, manifests.TenantPodPendingState))

	// Create running pods.
	runningPod1 := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stamped-pod-1",
			Namespace: "default",
			Labels: labels.Set{
				"app":                         manifests.TenantPoolAppName,
				manifests.TenantIDLabel:       tenantID.String(),
				manifests.TenantPoolNameLabel: crdbv1beta1.DefaultTenantPoolName,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: manifests.CockroachDB, Ready: true},
			},
		},
	}
	require.NoError(t, manifests.SetTenantPodState(&runningPod1, manifests.TenantPodRunningState))

	runningPod2 := runningPod1
	runningPod2.Name = "stamped-pod-2"
	runningPod3 := runningPod1
	runningPod3.Name = "stamped-pod-3"
	runningPod3.Annotations = nil
	runningPod4 := runningPod1
	runningPod4.Name = "stamped-pod-4"
	replaceStaleDrainingPod := unstampedPod
	replaceStaleDrainingPod.Name = "unstamped-pod-2"

	tenantKey := metrics.TenantKey{Namespace: tenant.Namespace, TenantID: tenant.Spec.TenantID}
	tenantMetrics := []*metrics.TenantMetrics{
		{
			Key: tenantKey,
			Pods: metrics.PodMetricsMap{
				unstampedPod.Name:            {SqlCpuIRate: 30, SqlConns: 0},
				runningPod1.Name:             {SqlCpuIRate: 20, SqlConns: 5},
				runningPod2.Name:             {SqlCpuIRate: 10, SqlConns: 1},
				runningPod4.Name:             {SqlCpuIRate: 50, SqlConns: 50},
				replaceStaleDrainingPod.Name: {SqlCpuIRate: 5, SqlConns: 1},
			},
		},
	}

	mgr := &metrics.TestTenantManager{}
	mgr.UpdateMetrics(context.Background(), tenantMetrics)

	// The operator retrieves a default configuration for the fluentbit sidecar
	// when stamping tenantpool pods.
	fluentbitTenantConfig := manifests.GetTestTenantNoopConfigMap()
	fluentbitTenantConfig.Namespace = "default"

	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{},
		&cluster, &tenant, &unstampedPod, &runningPod1,
		&runningPod2, &runningPod3, &runningPod4, fluentbitTenantConfig,
	).
		WithNamespace("default")
	tenantPodSyncer := tenantmock.NewMockTenantPodSyncer(ctrl)
	tenantPodSyncer.EXPECT().Sync(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	scaler := &TenantPodScaler{
		Ctl:                ctl,
		MetricManager:      mgr,
		NodeMetricManager:  &metrics.TestNodeManager{},
		Rng:                rand.NewSource(0),
		Emit:               &kube.EventEmitter{},
		PodSyncer:          tenantPodSyncer,
		NodeLocalityReader: testReadNodeLocality,
	}
	makeTenantCerts(t, ctl, tenantID)

	verifySuccess := func(numPods int, numDraining int) {
		shouldRequeue, _, err := scaler.ScaleTenantWithMetrics(ctx, &tenant)
		require.NoError(t, err)

		// Ensure tenant status was updated.
		var updatedTenant crdbv1beta1.CrdbTenant
		require.NoError(t, ctl.Get(ctx, tenant.Name, &updatedTenant))
		require.Equal(t, int32(numPods-numDraining), updatedTenant.Spec.Pods)
		require.Equal(t, tenant.Spec.Pods, updatedTenant.Status.Pods)

		// Ensure right number of pods are running and draining and that they
		// are sorted correctly.
		pods, err := scaler.listStampedPods(ctx, &tenant)
		require.NoError(t, err)
		require.Len(t, pods, numPods)
		require.Equal(t, numDraining, countDrainingPods(pods))
		require.Equal(t, numDraining > 0, shouldRequeue)

		cpuMin := float64(1000)
		for i := range pods {
			pod := &pods[i]
			state, _ := manifests.GetTenantPodState(pod)
			if i < numPods-numDraining {
				require.Equal(t, manifests.TenantPodRunningState, state)
			} else {
				require.Equal(t, manifests.TenantPodDrainingState, state)
			}

			cpu := tenantMetrics[0].Pods[pod.Name].SqlCpuIRate
			require.Less(t, cpu, cpuMin)
			cpuMin = cpu
		}
	}

	t.Run("scale to 3 then back to 4", func(t *testing.T) {
		tenant.Spec.Pods = 3
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, &tenant))
		verifySuccess(numRunningPods, 1 /* numDraining */)

		tenant.Spec.Pods = 4
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, &tenant))
		verifySuccess(numRunningPods, 0 /* numDraining */)

		// Again, to test idempotency.
		verifySuccess(numRunningPods, 0 /* numDraining */)
	})

	t.Run("scale to 0 then back to 4", func(t *testing.T) {
		tenant.Spec.Pods = 0
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, &tenant))
		verifySuccess(numRunningPods, 4 /* numDraining */)

		tenant.Spec.Pods = 4
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, &tenant))
		verifySuccess(numRunningPods, 0 /* numDraining */)
	})

	t.Run("scale to 2 then to 5", func(t *testing.T) {
		// Need to hook SendSqlStarterRequest since a new pod needs to be
		// stamped. In the previous test cases, we're able to reuse DRAINING
		// pods, so there's no need to stamp a new pod.
		defer testutils.HookGlobal(
			&SendSqlStarterRequest,
			func(client *http.Client, req *http.Request, pod *corev1.Pod) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			},
		)()

		tenant.Spec.Pods = 2
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, &tenant))
		verifySuccess(numRunningPods, 2 /* numDraining */)

		tenant.Spec.Pods = 5
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, &tenant))
		verifySuccess(numRunningPods+1, 0 /* numDraining */)
	})

	t.Run("scale to 1 and terminate 5th pod", func(t *testing.T) {
		tenant.Spec.Pods = 1
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true, Overwrite: true}, &tenant))
		verifySuccess(numRunningPods+1, 4 /* numDraining */)

		// Mark the 5th pod as having DRAINING >=1m. It should be terminated
		// because SqlConns = 0.
		var pod corev1.Pod
		require.NoError(t, ctl.Get(ctx, unstampedPod.Name, &pod))
		state, _ := manifests.GetTenantPodState(&pod)
		require.Equal(t, manifests.TenantPodDrainingState, state)
		ts := timeutil.Now().Add(-1 * time.Minute).UTC()
		pod.Annotations[manifests.TenantPodStateTimestampAnnotation] = ts.Format(manifests.PodStateTimestampFormat)
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, &pod))

		// Ensure the right pod was terminated.
		verifySuccess(numRunningPods, 3 /* numDraining */)
		require.EqualError(t, ctl.Get(ctx, unstampedPod.Name, &pod), "pods \"unstamped-pod\" not found")
	})

	t.Run("force draining pod termination", func(t *testing.T) {
		tenant.Spec.Pods = 2
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true, Overwrite: true}, &tenant))
		verifySuccess(numRunningPods, 2 /* numDraining */)

		// Mark the pod with the least CPU as having been DRAINING for >10 minutes.
		var pod corev1.Pod
		require.NoError(t, ctl.Get(ctx, runningPod2.Name, &pod))
		state, _ := manifests.GetTenantPodState(&pod)
		require.Equal(t, manifests.TenantPodDrainingState, state)
		ts := timeutil.Now().Add(-10 * time.Minute).UTC()
		pod.Annotations[manifests.TenantPodStateTimestampAnnotation] = ts.Format(manifests.PodStateTimestampFormat)
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, &pod))

		// Ensure the right pod was terminated.
		verifySuccess(numRunningPods-1, 1 /* numDraining */)
		require.EqualError(t, ctl.Get(ctx, runningPod2.Name, &pod), "pods \"stamped-pod-2\" not found")
	})

	t.Run("stale draining pod not re-used for scale up", func(t *testing.T) {
		// Get the one draining pod, and label it as stale
		pods, err := scaler.listStampedPods(ctx, &tenant)
		require.NoError(t, err)
		drainingPod := pods[len(pods)-1]
		manifests.MarkTenantPodAsStale(&drainingPod)
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, &drainingPod))
		// Scale the tenant up by one pod, this should not re-use the draining pod, but instead stamp
		// a new pod.
		tenant.Spec.Pods = 3
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true, Overwrite: true}, &tenant))
		// Create a new unstamped pod for Scale to use.
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, &replaceStaleDrainingPod))
		// Need to hook SendSqlStarterRequest since a new pod needs to be
		// stamped.
		defer testutils.HookGlobal(
			&SendSqlStarterRequest,
			func(client *http.Client, req *http.Request, pod *corev1.Pod) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			},
		)()
		// Ensure that the stale draining pod is not re-used.
		verifySuccess(numRunningPods, 1 /* numDraining */)
		pod := &corev1.Pod{}
		require.NoError(t, ctl.Get(ctx, replaceStaleDrainingPod.Name, pod))
		state, _ := manifests.GetTenantPodState(pod)
		require.True(t, state == manifests.TenantPodRunningState)
	})
}

func TestWaitForCockroachReady(t *testing.T) {
	readyPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ready-pod",
			Namespace: "default",
			Labels: labels.Set{
				"app":                         manifests.TenantPoolAppName,
				manifests.TenantPoolNameLabel: crdbv1beta1.DefaultTenantPoolName,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: manifests.CockroachDB, Ready: true},
			},
		},
	}

	notReadyPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "not-ready-pod",
			Namespace: "default",
			Labels: labels.Set{
				"app":                         manifests.TenantPoolAppName,
				manifests.TenantPoolNameLabel: crdbv1beta1.DefaultTenantPoolName,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: manifests.CockroachDB, Ready: false},
			},
		},
	}

	t.Run("returns immediately when pod is already ready", func(t *testing.T) {
		ctx := context.Background()
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &readyPod).WithNamespace("default")
		scaler := &TenantPodScaler{Ctl: ctl}

		err := scaler.waitForCockroachReady(ctx, &readyPod)
		require.NoError(t, err)
	})

	t.Run("returns nil when pod becomes ready during polling", func(t *testing.T) {
		ctx := context.Background()
		pod := notReadyPod.DeepCopy()
		ctl := kubetest.NewFakeCtl(kubetest.Options{
			Options: kube.Options{
				BackOff: kube.NewCtlDefaultBackOffFn(5 * time.Second),
			},
		}, pod).WithNamespace("default")
		scaler := &TenantPodScaler{Ctl: ctl}

		// Update the pod to become ready after a short delay.
		go func() {
			time.Sleep(50 * time.Millisecond)
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{
				{Name: manifests.CockroachDB, Ready: true},
			}
			require.NoError(t, ctl.UpdateStatus(ctx, pod))
		}()

		err := scaler.waitForCockroachReady(ctx, pod)
		require.NoError(t, err)
	})

	t.Run("returns error on timeout", func(t *testing.T) {
		ctx := context.Background()
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &notReadyPod).WithNamespace("default")
		scaler := &TenantPodScaler{Ctl: ctl}

		err := scaler.waitForCockroachReady(ctx, &notReadyPod)
		require.Error(t, err)
		require.Contains(t, err.Error(), "timed out")
	})

	t.Run("returns error on context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		ctl := kubetest.NewFakeCtl(kubetest.Options{
			Options: kube.Options{
				BackOff: kube.NewCtlDefaultBackOffFn(5 * time.Second),
			},
		}, &notReadyPod).WithNamespace("default")
		scaler := &TenantPodScaler{Ctl: ctl}

		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()

		err := scaler.waitForCockroachReady(ctx, &notReadyPod)
		require.Error(t, err)
	})
}

func TestStaleRolloverReadinessGate(t *testing.T) {
	defer testutils.HookGlobal(&replacementReadyBackOff, kube.NewCtlDefaultBackOffFn(200*time.Millisecond))()

	const tenantID crdbv1beta1.TenantID = 10
	tenantPool := crdbv1beta1.DefaultTenantPoolName

	// Cluster without adhoc pods so stale rollovers go through the warm pool.
	cluster := crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "default",
		},
	}

	tenant := crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantID.Tag(),
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: crdbv1beta1.CrdbClusterKind, Name: "cluster"},
			},
		},
		Spec: crdbv1beta1.CrdbTenantSpec{
			TenantID:   tenantID,
			Pods:       1,
			TenantPool: &tenantPool,
		},
		Status: crdbv1beta1.CrdbTenantStatus{
			Pods: 1,
		},
	}

	stalePod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stale-pod",
			Namespace: "default",
			Labels: labels.Set{
				"app":                         manifests.TenantPoolAppName,
				manifests.TenantIDLabel:       tenantID.String(),
				manifests.TenantPoolNameLabel: tenantPool,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: manifests.CockroachDB, Ready: true},
			},
		},
	}
	require.NoError(t, manifests.SetTenantPodState(&stalePod, manifests.TenantPodRunningState))
	manifests.MarkTenantPodAsStale(&stalePod)

	defaultFluentbitTenantConfig := manifests.GetTestTenantNoopConfigMap()
	defaultFluentbitTenantConfig.Namespace = "default"

	defer testutils.HookGlobal(
		&SendSqlStarterRequest,
		func(client *http.Client, req *http.Request, pod *corev1.Pod) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		},
	)()

	t.Run("drain waits for replacement readiness", func(t *testing.T) {
		unstampedPod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "warm-pool-pod",
				Namespace: "default",
				Labels: labels.Set{
					"app":                         manifests.TenantPoolAppName,
					manifests.TenantPoolNameLabel: tenantPool,
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
				},
				ContainerStatuses: []corev1.ContainerStatus{
					{Name: manifests.CockroachDB, Ready: true},
				},
			},
		}
		unstampedPod.UID = types.UID("warmPoolPod")
		require.NoError(t, manifests.SetTenantPodState(&unstampedPod, manifests.TenantPodPendingState))

		// Add a second unstamped pod so the pool doesn't return lowCapacityErr.
		unstampedPod2 := unstampedPod.DeepCopy()
		unstampedPod2.Name = "warm-pool-pod-2"
		unstampedPod2.UID = types.UID("warmPoolPod2")

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		tenantPodSyncer := tenantmock.NewMockTenantPodSyncer(ctrl)
		tenantPodSyncer.EXPECT().Sync(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

		tenantCopy := tenant.DeepCopy()
		stalePodCopy := stalePod.DeepCopy()
		ctx := context.Background()
		ctl := kubetest.NewFakeCtl(kubetest.Options{},
			&cluster, tenantCopy, stalePodCopy, &unstampedPod, unstampedPod2, defaultFluentbitTenantConfig,
		).WithNamespace("default")
		makeTenantCerts(t, ctl, tenantID)

		scaler := &TenantPodScaler{
			Ctl:                ctl,
			MetricManager:      &metrics.TestTenantManager{},
			NodeMetricManager:  &metrics.TestNodeManager{},
			Rng:                rand.NewSource(0),
			Emit:               &kube.EventEmitter{},
			PodSyncer:          tenantPodSyncer,
			NodeLocalityReader: testReadNodeLocality,
		}

		shouldRequeue, _, err := scaler.ScaleTenantWithMetrics(ctx, tenantCopy)
		require.NoError(t, err)
		require.True(t, shouldRequeue)

		// Verify the stale pod was drained.
		var pod corev1.Pod
		require.NoError(t, ctl.Get(ctx, stalePodCopy.Name, &pod))
		state, _ := manifests.GetTenantPodState(&pod)
		require.Equal(t, manifests.TenantPodDrainingState, state)

		// Verify a replacement pod is running (the scaler picks randomly
		// from the pool, so check both candidates).
		stampedPods, err := scaler.listStampedPods(ctx, tenantCopy)
		require.NoError(t, err)
		runningCount := 0
		for i := range stampedPods {
			s, _ := manifests.GetTenantPodState(&stampedPods[i])
			if s == manifests.TenantPodRunningState {
				runningCount++
			}
		}
		require.Equal(t, 1, runningCount)
	})

	t.Run("drain proceeds after readiness timeout", func(t *testing.T) {
		unstampedPod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "warm-pool-pod-timeout",
				Namespace: "default",
				Labels: labels.Set{
					"app":                         manifests.TenantPoolAppName,
					manifests.TenantPoolNameLabel: tenantPool,
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
				},
				// CockroachDB container NOT ready — will cause timeout.
				ContainerStatuses: []corev1.ContainerStatus{
					{Name: manifests.CockroachDB, Ready: false},
				},
			},
		}
		unstampedPod.UID = types.UID("warmPoolPodTimeout")
		require.NoError(t, manifests.SetTenantPodState(&unstampedPod, manifests.TenantPodPendingState))

		unstampedPod2 := unstampedPod.DeepCopy()
		unstampedPod2.Name = "warm-pool-pod-timeout-2"
		unstampedPod2.UID = types.UID("warmPoolPodTimeout2")

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		tenantPodSyncer := tenantmock.NewMockTenantPodSyncer(ctrl)
		tenantPodSyncer.EXPECT().Sync(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

		tenantCopy := tenant.DeepCopy()
		stalePodCopy := stalePod.DeepCopy()
		ctx := context.Background()
		ctl := kubetest.NewFakeCtl(kubetest.Options{},
			&cluster, tenantCopy, stalePodCopy, &unstampedPod, unstampedPod2, defaultFluentbitTenantConfig,
		).WithNamespace("default")
		makeTenantCerts(t, ctl, tenantID)

		scaler := &TenantPodScaler{
			Ctl:                ctl,
			MetricManager:      &metrics.TestTenantManager{},
			NodeMetricManager:  &metrics.TestNodeManager{},
			Rng:                rand.NewSource(0),
			Emit:               &kube.EventEmitter{},
			PodSyncer:          tenantPodSyncer,
			NodeLocalityReader: testReadNodeLocality,
		}

		shouldRequeue, _, err := scaler.ScaleTenantWithMetrics(ctx, tenantCopy)
		require.NoError(t, err)
		require.True(t, shouldRequeue)

		// Even though the replacement pod timed out, the stale pod
		// should still be drained (fallback to current behavior).
		var pod corev1.Pod
		require.NoError(t, ctl.Get(ctx, stalePodCopy.Name, &pod))
		state, _ := manifests.GetTenantPodState(&pod)
		require.Equal(t, manifests.TenantPodDrainingState, state)
	})
}

func TestSuspend(t *testing.T) {
	cluster := crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "default",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Features: []crdbv1beta1.ClusterFeature{crdbv1beta1.ClusterTenantCreateAdhocPods},
		},
	}

	const tenantID = crdbv1beta1.TenantID(10)
	tenant := crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantID.Tag(),
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: crdbv1beta1.CrdbClusterKind, Name: "cluster"},
			},
		},
		Spec: crdbv1beta1.CrdbTenantSpec{
			TenantID:   tenantID,
			Pods:       2,
			TenantPool: new("default"),
		},
		Status: crdbv1beta1.CrdbTenantStatus{
			Pods: 2,
		},
	}

	// Create draining pods.
	drainingPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "draining-pod",
			Namespace: "default",
			Labels: labels.Set{
				"app":                         manifests.TenantPoolAppName,
				manifests.TenantIDLabel:       tenantID.String(),
				manifests.TenantPoolNameLabel: crdbv1beta1.DefaultTenantPoolName,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: manifests.CockroachDB, Ready: true},
			},
		},
	}
	require.NoError(t, manifests.SetTenantPodState(&drainingPod, manifests.TenantPodDrainingState))

	drainingPod2 := drainingPod
	drainingPod2.Name = "draining-pod-2"

	tenantKey := metrics.TenantKey{Namespace: tenant.Namespace, TenantID: tenantID}
	tenantMetrics := []*metrics.TenantMetrics{
		{
			Key: tenantKey,
			Pods: metrics.PodMetricsMap{
				drainingPod.Name:  {SqlCpuIRate: 0.05, SqlConns: 0},
				drainingPod2.Name: {SqlCpuIRate: 10, SqlConns: 0},
			},
		},
	}
	mgr := &metrics.TestTenantManager{}
	mgr.UpdateMetrics(context.Background(), tenantMetrics)

	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{}, &cluster, &tenant, &drainingPod, &drainingPod2)
	ctl = ctl.WithNamespace("default")

	t.Run("terminate 2 pods and suspend tenant", func(t *testing.T) {
		tenant.Spec.Pods = 0
		scaler := &TenantPodScaler{
			Ctl:               ctl,
			MetricManager:     mgr,
			NodeMetricManager: &metrics.TestNodeManager{},
			Rng:               rand.NewSource(0),
			Emit:              &kube.EventEmitter{},
		}
		shouldRequeue, requeueDuration, err := scaler.ScaleTenantWithMetrics(ctx, &tenant)
		require.True(t, shouldRequeue)
		require.Equal(t, DrainingRequeueAfter, requeueDuration)
		require.NoError(t, err)
		require.NoError(t, ctl.Get(ctx, tenant.Name, &tenant))
		require.Equal(t, int32(0), tenant.Spec.Pods)
		require.Equal(t, int32(0), tenant.Status.Pods)

		// Set that the pods have been draining for at least 1m.
		var pod corev1.Pod
		for _, name := range []string{drainingPod.Name, drainingPod2.Name} {
			require.NoError(t, ctl.Get(ctx, name, &pod))
			state, _ := manifests.GetTenantPodState(&pod)
			require.Equal(t, manifests.TenantPodDrainingState, state)
			ts := timeutil.Now().Add(-1 * time.Minute).UTC()
			pod.Annotations[manifests.TenantPodStateTimestampAnnotation] = ts.Format(manifests.PodStateTimestampFormat)
			require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, &pod))
		}

		// Now ensure that pods were terminated.
		shouldRequeue, _, err = scaler.ScaleTenantWithMetrics(ctx, &tenant)
		require.False(t, shouldRequeue)
		require.NoError(t, err)
		require.NoError(t, ctl.Get(ctx, tenant.Name, &tenant))
		require.Equal(t, int32(0), tenant.Spec.Pods)
		require.Equal(t, int32(0), tenant.Status.Pods)
		require.EqualError(t, ctl.Get(ctx, drainingPod.Name, &pod), "pods \"draining-pod\" not found")
		require.EqualError(t, ctl.Get(ctx, drainingPod2.Name, &pod), "pods \"draining-pod-2\" not found")

		// Call ScaleTenant again to test idempotency.
		shouldRequeue, requeueDuration, err = scaler.ScaleTenantWithMetrics(ctx, &tenant)
		require.False(t, shouldRequeue)
		require.Equal(t, DrainingRequeueAfter, requeueDuration)
		require.NoError(t, err)
		require.NoError(t, ctl.Get(ctx, tenant.Name, &tenant))
		require.Equal(t, int32(0), tenant.Spec.Pods)
		require.Equal(t, int32(0), tenant.Status.Pods)
	})
}

func TestStampTenantPod(t *testing.T) {
	cluster := crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "default",
		},
	}
	clusterWithAdhocFeature := cluster.DeepCopy()
	clusterWithAdhocFeature.Spec.Features = []crdbv1beta1.ClusterFeature{crdbv1beta1.ClusterTenantCreateAdhocPods}

	const tenantID crdbv1beta1.TenantID = 10
	tenantPoolName := crdbv1beta1.DefaultTenantPoolName
	tenant := crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantID.Tag(),
			Namespace: "default",
		},
		Spec: crdbv1beta1.CrdbTenantSpec{
			TenantID:   tenantID,
			Pods:       1,
			TenantPool: &tenantPoolName,
		},
	}
	tenantWithAdhocEnabled := tenant.DeepCopy()
	tenantWithAdhocEnabled.Annotations = map[string]string{
		manifests.TenantEnableAdhocPodCreationAnnotation: "true",
	}
	tenantWithAdhocDisabled := tenant.DeepCopy()
	tenantWithAdhocDisabled.Annotations = map[string]string{
		manifests.TenantEnableAdhocPodCreationAnnotation: "false",
	}

	tenantPool := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("tenantpool-%s", crdbv1beta1.DefaultTenantPoolName),
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Labels: labels.Set{
						"app":                         manifests.TenantPoolAppName,
						manifests.TenantPoolNameLabel: tenantPoolName,
						manifests.TenantPodStateLabel: string(manifests.TenantPodPendingState),
					},
				},
				Spec: corev1.PodSpec{},
			},
		},
	}

	runningPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "running-pod",
			Namespace: "default",
			Labels: labels.Set{
				"app":                         manifests.TenantPoolAppName,
				manifests.TenantPoolNameLabel: tenantPoolName,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
			},
		},
	}
	require.NoError(t, manifests.SetTenantPodState(&runningPod, manifests.TenantPodPendingState))

	nonPoolPod := runningPod
	nonPoolPod.Name = "non-pool-pod"
	nonPoolPod.Labels = labels.Set{}
	require.NoError(t, manifests.SetTenantPodState(&nonPoolPod, manifests.TenantPodPendingState))

	otherPoolPod := runningPod
	otherPoolPod.Name = "other-pool-pod"
	otherPoolPod.Labels = labels.Set{
		manifests.TenantPoolNameLabel: "some-other-pool",
	}
	require.NoError(t, manifests.SetTenantPodState(&otherPoolPod, manifests.TenantPodPendingState))

	otherTenantPod := runningPod
	otherTenantPod.Name = "stamped-pod"
	otherTenantPod.Labels = labels.Set{
		"app":                         manifests.TenantPoolAppName,
		manifests.TenantIDLabel:       "123",
		manifests.TenantPoolNameLabel: tenantPoolName,
	}
	require.NoError(t, manifests.SetTenantPodState(&otherTenantPod, manifests.TenantPodPendingState))

	notReadyPod := runningPod
	notReadyPod.Name = "not-ready"
	notReadyPod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		{Type: corev1.ContainersReady, Status: corev1.ConditionFalse},
	}
	require.NoError(t, manifests.SetTenantPodState(&notReadyPod, manifests.TenantPodPendingState))

	options := kube.Options{
		BeforeMutation: func(object kube.Object) error {
			pod, ok := object.(*corev1.Pod)
			if !ok {
				return nil
			}
			if strings.Contains(pod.Name, fmt.Sprintf("%s-adhoc", tenantPoolName)) {
				// The newly created pod needs to be marked ready
				pod.Status.Conditions = []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				}
			}
			return nil
		},
	}

	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{Options: options}, &tenantPool, &nonPoolPod, &otherTenantPod)
	scaler := &TenantPodScaler{
		Ctl:               ctl,
		MetricManager:     &metrics.TestTenantManager{},
		NodeMetricManager: &metrics.TestNodeManager{},
		Rng:               rand.NewSource(0),
		Emit:              &kube.EventEmitter{},
	}

	// The operator should first try to create a new pod when there are none available
	// Adhoc pod creation is turned off on the cluster and tenant, so this should fail
	_, err := scaler.stampTenantPod(ctx, &cluster, &tenant, false)
	require.EqualError(t, err, "insufficient pods in tenant pool default")

	// Adhoc pod creation is turned on at the tenant level, so this should succeed
	_, err = scaler.stampTenantPod(ctx, &cluster, tenantWithAdhocEnabled, false)
	require.NoError(t, err, "should have been able to create a new pod")

	// Adhoc pod creation is turned on at the cluster level, so this should succeed
	_, err = scaler.stampTenantPod(ctx, clusterWithAdhocFeature, &tenant, false)
	require.NoError(t, err, "should have been able to create a new pod")

	// Adhoc pod creation is turned on at the cluster level, but explicitly disabled at the tenant level.
	// This should fail.
	_, err = scaler.stampTenantPod(ctx, clusterWithAdhocFeature, tenantWithAdhocDisabled, false)
	require.EqualError(t, err, "insufficient pods in tenant pool default")

	// Ignore pods that are not in the tenant pool or in the PENDING state.
	_, err = scaler.stampTenantPod(ctx, &cluster, &tenant, false)
	require.EqualError(t, err, "insufficient pods in tenant pool default")

	// Add a pod that is in the PENDING state, and in the correct pool, but
	// who's containers are not Ready.
	require.NoError(t, ctl.Apply(ctx, &notReadyPod))
	_, err = scaler.stampTenantPod(ctx, &cluster, &tenant, false)
	require.EqualError(t, err, "insufficient pods in tenant pool default")

	// Now add the running pod.
	require.NoError(t, ctl.Apply(ctx, &runningPod))

	// Now do not care about exhausting the pool, and stamp the pod.
	stampedPod, err := scaler.stampTenantPod(ctx, &cluster, &tenant, false)
	require.NoError(t, err)
	require.Equal(t, "10", stampedPod.Labels[manifests.TenantIDLabel])
}

// TestStampTenantPodNodeCPUUsageFilter covers stampTenantPod's filter:
// skip hot-node pods when cooler ones exist; fall back to the full pool
// when every node is hot. Requires ClusterHotNodeAwareTenantPodStamping;
// without it the filter is skipped and hot-node pods stay eligible.
func TestStampTenantPodNodeCPUUsageFilter(t *testing.T) {
	const tenantID crdbv1beta1.TenantID = 10
	tenantPoolName := crdbv1beta1.DefaultTenantPoolName
	cluster := crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "default",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Features: []crdbv1beta1.ClusterFeature{
				crdbv1beta1.ClusterHotNodeAwareTenantPodStamping,
			},
		},
	}
	tenant := crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantID.Tag(),
			Namespace: "default",
		},
		Spec: crdbv1beta1.CrdbTenantSpec{
			TenantID:   tenantID,
			Pods:       1,
			TenantPool: &tenantPoolName,
		},
	}

	makePod := func(name, node string) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels: labels.Set{
					"app":                         manifests.TenantPoolAppName,
					manifests.TenantPoolNameLabel: tenantPoolName,
				},
			},
			Spec: corev1.PodSpec{NodeName: node},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
				},
			},
		}
		require.NoError(t, manifests.SetTenantPodState(pod, manifests.TenantPodPendingState))
		return pod
	}
	coldPod := makePod("pod-cold", "node-cold")
	hotPod := makePod("pod-hot", "node-hot")

	ctx := context.Background()

	t.Run("hot node excluded when cold candidate exists", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, coldPod.DeepCopy(), hotPod.DeepCopy())
		nodeManager := &metrics.TestNodeManager{}
		nodeManager.UpdateNodeMetrics(metrics.NodeMetricsMap{
			"node-cold": {CpuAvg5m: 40},
			"node-hot":  {CpuAvg5m: 95},
		})
		scaler := &TenantPodScaler{
			Ctl:               ctl,
			MetricManager:     &metrics.TestTenantManager{},
			NodeMetricManager: nodeManager,
			Rng:               rand.NewSource(0),
			Emit:              &kube.EventEmitter{},
		}
		stamped, err := scaler.stampTenantPod(ctx, &cluster, &tenant, false)
		require.NoError(t, err)
		require.Equal(t, coldPod.Name, stamped.Name)
	})

	t.Run("all hot falls back to full pool", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, coldPod.DeepCopy(), hotPod.DeepCopy())
		nodeManager := &metrics.TestNodeManager{}
		nodeManager.UpdateNodeMetrics(metrics.NodeMetricsMap{
			"node-cold": {CpuAvg5m: 90},
			"node-hot":  {CpuAvg5m: 95},
		})
		scaler := &TenantPodScaler{
			Ctl:               ctl,
			MetricManager:     &metrics.TestTenantManager{},
			NodeMetricManager: nodeManager,
			Rng:               rand.NewSource(0),
			Emit:              &kube.EventEmitter{},
		}
		stamped, err := scaler.stampTenantPod(ctx, &cluster, &tenant, false)
		require.NoError(t, err)
		require.Contains(t, []string{coldPod.Name, hotPod.Name}, stamped.Name,
			"stamping should not fail when every node is hot")
	})
}

// TestStampTenantPodHotNodeAwareFlag toggles ClusterHotNodeAwareTenantPodStamping
// with an otherwise-identical setup to prove the flag gates the hot-node filter.
// With the flag off, hot-node pods are eligible for random selection; with the
// flag on, they are excluded.
func TestStampTenantPodHotNodeAwareFlag(t *testing.T) {
	const tenantID crdbv1beta1.TenantID = 10
	tenantPoolName := crdbv1beta1.DefaultTenantPoolName
	tenant := crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantID.Tag(),
			Namespace: "default",
		},
		Spec: crdbv1beta1.CrdbTenantSpec{
			TenantID:   tenantID,
			Pods:       1,
			TenantPool: &tenantPoolName,
		},
	}

	makePod := func(name, node string) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels: labels.Set{
					"app":                         manifests.TenantPoolAppName,
					manifests.TenantPoolNameLabel: tenantPoolName,
				},
			},
			Spec: corev1.PodSpec{NodeName: node},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
				},
			},
		}
		require.NoError(t, manifests.SetTenantPodState(pod, manifests.TenantPodPendingState))
		return pod
	}

	// unstampPod reverses tryStampTenantPod so a loop can re-stamp.
	unstampPod := func(t *testing.T, ctl *kube.Ctl, pod *corev1.Pod) {
		delete(pod.Labels, manifests.TenantIDLabel)
		pod.OwnerReferences = nil
		require.NoError(t, ctl.Update(context.Background(), pod))
	}

	nodeMetrics := metrics.NodeMetricsMap{
		"node-cold": {CpuAvg5m: 40},
		"node-hot":  {CpuAvg5m: 95},
	}

	cases := []struct {
		name         string
		features     []crdbv1beta1.ClusterFeature
		expectHotHit bool
	}{
		{
			name:         "flag off: no filter, hot pod is eligible",
			features:     nil,
			expectHotHit: true,
		},
		{
			name: "flag on: filter drops hot pod, only cold is eligible",
			features: []crdbv1beta1.ClusterFeature{
				crdbv1beta1.ClusterHotNodeAwareTenantPodStamping,
			},
			expectHotHit: false,
		},
	}

	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster := crdbv1beta1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cluster",
					Namespace: "default",
				},
				Spec: crdbv1beta1.CrdbClusterSpec{Features: tc.features},
			}
			coldPod := makePod("pod-cold", "node-cold")
			hotPod := makePod("pod-hot", "node-hot")
			ctl := kubetest.NewFakeCtl(kubetest.Options{},
				coldPod.DeepCopy(), hotPod.DeepCopy())
			nodeManager := &metrics.TestNodeManager{}
			nodeManager.UpdateNodeMetrics(nodeMetrics)
			scaler := &TenantPodScaler{
				Ctl:               ctl,
				MetricManager:     &metrics.TestTenantManager{},
				NodeMetricManager: nodeManager,
				Rng:               rand.NewSource(0),
				Emit:              &kube.EventEmitter{},
			}
			selections := map[string]int{}
			const iterations = 50
			for range iterations {
				stamped, err := scaler.stampTenantPod(ctx, &cluster, &tenant, false)
				require.NoError(t, err)
				selections[stamped.Name]++
				unstampPod(t, ctl, stamped)
			}
			require.Positive(t, selections[coldPod.Name],
				"cold pod should be selected: %v", selections)
			if tc.expectHotHit {
				require.Positive(t, selections[hotPod.Name],
					"flag off: hot pod should be selected occasionally: %v", selections)
			} else {
				require.Zero(t, selections[hotPod.Name],
					"flag on: hot pod must never be selected: %v", selections)
			}
		})
	}
}

func TestCreateAdhocTenantPod(t *testing.T) {
	testCases := []struct {
		description         string
		tenantPoolName      string
		featureFlagDisabled bool
		maxPendingAdhocPods int
		additionalObjects   []kube.Object
		expectedErrorMsg    string
	}{
		{
			description:         "fails when the feature is not enabled",
			featureFlagDisabled: true,
			expectedErrorMsg:    "adhoc pod creation is not enabled on this cluster",
		},
		{
			description:         "fails when there are too many pending adhoc pods",
			maxPendingAdhocPods: 1,
			additionalObjects: []kube.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pending-adhoc",
						Namespace: "default",
						Labels: map[string]string{
							"app":                             manifests.TenantPoolAppName,
							manifests.TenantPodStateLabel:     string(manifests.TenantPodPendingState),
							manifests.TenantPoolAdhocPodLabel: "true",
						},
					},
					Spec: corev1.PodSpec{},
				},
			},
			expectedErrorMsg: "too many pending adhoc pods",
		},
		{
			description:      "fails when the deployment name is too long",
			tenantPoolName:   "12345678901234567890123456789012345678901",
			expectedErrorMsg: "too long to ensure pod name uniqueness",
		},
		{
			description: "creates an adhoc pod",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			cluster := crdbv1beta1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "cluster",
					Namespace:   "default",
					Annotations: map[string]string{},
				},
			}
			if !testCase.featureFlagDisabled {
				cluster.Spec.Features = append(cluster.Spec.Features, crdbv1beta1.ClusterTenantCreateAdhocPods)
			}
			if testCase.maxPendingAdhocPods > 0 {
				cluster.Annotations[manifests.MaxTenantAdhocPendingPodCountAnnotation] = strconv.Itoa(testCase.maxPendingAdhocPods)
			}
			tenantPoolName := testCase.tenantPoolName
			if tenantPoolName == "" {
				tenantPoolName = crdbv1beta1.DefaultTenantPoolName
			}
			tenantPool := appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("tenantpool-%s", tenantPoolName),
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": manifests.TenantPoolAppName,
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Namespace: "default",
							Labels: map[string]string{
								"app":                         manifests.TenantPoolAppName,
								manifests.TenantPodStateLabel: string(manifests.TenantPodPendingState),
							},
						},
						Spec: corev1.PodSpec{},
					},
				},
			}
			tenant := crdbv1beta1.CrdbTenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tenant",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: cluster.Name,
							Kind: crdbv1beta1.CrdbClusterKind,
						},
					},
				},
				Spec: crdbv1beta1.CrdbTenantSpec{
					TenantPool: &tenantPoolName,
					TenantID:   crdbv1beta1.TenantID(1),
				},
			}

			ctx := context.Background()
			ctl := kubetest.NewFakeCtl(
				kubetest.Options{}, append(testCase.additionalObjects, &cluster, &tenantPool, &tenant)...)
			ctl.WaitFor(ctx, kube.IsQuiescent, &tenantPool)
			scaler := &TenantPodScaler{
				Ctl:           ctl,
				MetricManager: &metrics.TestTenantManager{},
				Rng:           rand.NewSource(0),
				Emit:          &kube.EventEmitter{},
			}

			pod, err := scaler.createAdhocTenantPod(ctx, &cluster, &tenant)
			if testCase.expectedErrorMsg == "" {
				require.NotNil(t, pod)

				require.Equal(t, tenantPool.Spec.Template.Namespace, pod.Namespace)
				require.Subset(t, pod.Labels, tenantPool.Spec.Template.Labels)
				require.Subset(t, pod.Annotations, tenantPool.Spec.Template.Annotations)
				require.Equal(t, tenantPool.Spec.Template.Spec, pod.Spec)

				require.Len(t, pod.OwnerReferences, 1)
				require.Equal(t, tenant.Name, pod.OwnerReferences[0].Name)
				require.Equal(t, pod.OwnerReferences[0].Kind, crdbv1beta1.CrdbTenantKind)

				require.Subset(t, pod.Labels, map[string]string{
					manifests.TenantIDLabel:           tenant.Spec.TenantID.String(),
					manifests.TenantPoolAdhocPodLabel: "true",
				})
			} else {
				require.NotNil(t, err)
				require.ErrorContains(t, err, testCase.expectedErrorMsg)
			}
		})
	}
}

func TestUnreadyPodsReplacementCount(t *testing.T) {
	createPod := func(ready bool) func(string) *corev1.Pod {
		return func(name string) *corev1.Pod {
			return &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: "default",
					Labels: map[string]string{
						"app":                   manifests.TenantPoolAppName,
						manifests.TenantIDLabel: "1",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: manifests.CockroachDB},
					},
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{Name: manifests.CockroachDB, Ready: ready},
					},
				},
			}
		}
	}
	readyPod := createPod(true)
	unreadyPod := createPod(false)

	testCases := []struct {
		description      string
		pods             []kube.Object
		clusterMaximum   int
		replacementCount int
	}{
		{
			description:      "returns 0 when there are no unready pods",
			pods:             []kube.Object{readyPod("pod")},
			clusterMaximum:   5,
			replacementCount: 5,
		},
		{
			description:      "returns correct count when there are unready pods",
			pods:             []kube.Object{unreadyPod("pod")},
			clusterMaximum:   5,
			replacementCount: 4,
		},
		{
			description:      "accounts for cluster maximum limit",
			pods:             []kube.Object{unreadyPod("pod")},
			clusterMaximum:   1,
			replacementCount: 0,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			cluster := crdbv1beta1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "cluster",
					Namespace:   "default",
					Annotations: map[string]string{},
				},
			}
			if testCase.clusterMaximum > 0 {
				cluster.Annotations[manifests.MaxTenantUnreadyStampedPodCountAnnotation] = strconv.Itoa(testCase.clusterMaximum)
			}
			tenant := crdbv1beta1.CrdbTenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tenant",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: cluster.Name,
							Kind: crdbv1beta1.CrdbClusterKind,
						},
					},
				},
				Spec: crdbv1beta1.CrdbTenantSpec{
					TenantPool: new(crdbv1beta1.DefaultTenantPoolName),
					TenantID:   crdbv1beta1.TenantID(1),
				},
			}

			ctx := context.Background()
			ctl := kubetest.NewFakeCtl(kubetest.Options{}, append(testCase.pods, &cluster, &tenant)...)
			scaler := &TenantPodScaler{
				Ctl:           ctl,
				MetricManager: &metrics.TestTenantManager{},
				Rng:           rand.NewSource(0),
				Emit:          &kube.EventEmitter{},
			}
			replacementCount, err := scaler.maxUnreadyPodReplacements(ctx, &cluster)
			require.Nil(t, err)
			require.Equal(t, testCase.replacementCount, replacementCount)
		})
	}
}

// Helper function for testing. This lets us control the random number
// generator's output.
type mockSource struct {
	value int64
}

func (m *mockSource) Int63() int64 {
	return m.value
}

func (m *mockSource) Seed(seed int64) {
	// Not needed for this mock, but required by the interface.
}

func TestPickPodByNodeLoad(t *testing.T) {
	// Create a mock source for deterministic random numbers.
	mockSource := &mockSource{value: 0}

	createPod := func(name, nodeName string) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					"app": manifests.TenantPoolAppName,
				},
			},
			Spec: corev1.PodSpec{
				NodeName: nodeName,
			},
		}
	}

	testCases := []struct {
		name          string
		pods          []corev1.Pod
		nodeMetrics   metrics.NodeMetricsMap
		mockRandValue int64
		expectedPod   string
		description   string
	}{
		{
			name: "Multi node and multiple pods on different nodes",
			pods: []corev1.Pod{
				createPod("pod1", "node1"),
				createPod("pod2", "node2"),
				createPod("pod3", "node3"),
			},
			nodeMetrics: metrics.NodeMetricsMap{
				"node1": {CpuIRate: 80}, // 20% unused
				"node2": {CpuIRate: 30}, // 70% unused
				"node3": {CpuIRate: 60}, // 40% unused
			},
			mockRandValue: (10*(1<<63))/13 + 1, // This will select from node3 pods.
			expectedPod:   "pod3",
			description:   "Should select pod based on the random number generated with weighted unused CPU capacity",
		},
		{
			name: "Multi node and multiple pods on same node - 1",
			pods: []corev1.Pod{
				createPod("pod1", "node1"),
				createPod("pod2", "node1"),
				createPod("pod3", "node2"),
			},
			nodeMetrics: metrics.NodeMetricsMap{
				"node1": {CpuIRate: 60}, // 40% unused
				"node2": {CpuIRate: 30}, // 70% unused
			},
			mockRandValue: (1*(1<<63))/11 + 1, // This will select from node1's pods with index 1
			expectedPod:   "pod2",
			description:   "Should select random pod when multiple pods exist for same node",
		},
		{
			name: "Multi node and multiple pods on same node - 2",
			pods: []corev1.Pod{
				createPod("pod1", "node1"),
				createPod("pod2", "node1"),
				createPod("pod3", "node2"),
			},
			nodeMetrics: metrics.NodeMetricsMap{
				"node1": {CpuIRate: 60}, // 40% unused
				"node2": {CpuIRate: 30}, // 70% unused
			},
			mockRandValue: (5*(1<<63))/11 + 1, // This will select from node2's pods
			expectedPod:   "pod3",
			description:   "Should select pod based on the random number generated with weighted unused CPU capacity",
		},
		{
			name: "Single node multiple pods - 1",
			pods: []corev1.Pod{
				createPod("pod1", "node1"),
				createPod("pod2", "node1"),
			},
			nodeMetrics: metrics.NodeMetricsMap{
				"node1": {CpuIRate: 30},
			},
			mockRandValue: 1,
			expectedPod:   "pod2",
			description:   "Should select random pod when multiple pods exist for same node",
		},
		{
			name: "Single node multiple pods - 2",
			pods: []corev1.Pod{
				createPod("pod1", "node1"),
				createPod("pod2", "node1"),
			},
			nodeMetrics: metrics.NodeMetricsMap{
				"node1": {CpuIRate: 30},
			},
			mockRandValue: 0,
			expectedPod:   "pod1",
			description:   "Should select random pod when multiple pods exist for same node",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mockSource.value = tc.mockRandValue

			// Create a new test node manager for each test case
			nodeManager := &metrics.TestNodeManager{}

			// Initialize the node metrics before creating the scaler
			if tc.nodeMetrics != nil {
				nodeManager.UpdateNodeMetrics(tc.nodeMetrics)
			}

			scaler := &TenantPodScaler{
				Ctl:               kubetest.NewFakeCtl(kubetest.Options{}),
				NodeMetricManager: nodeManager,
				Rng:               mockSource,
				Emit:              &kube.EventEmitter{},
			}

			selectedPod := scaler.pickUnstampedPodByNodeLoad(tc.pods, tc.nodeMetrics)
			require.Equal(t, tc.expectedPod, selectedPod.Name, tc.description)
		})
	}
}

func TestFilterCandidatesByNodeCPUUsage(t *testing.T) {
	pod := func(name, node string) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       corev1.PodSpec{NodeName: node},
		}
	}

	// names extracts and sorts pod names for order-insensitive comparison.
	names := func(pods []corev1.Pod) []string {
		out := make([]string, len(pods))
		for i, p := range pods {
			out[i] = p.Name
		}
		sort.Strings(out)
		return out
	}

	cases := []struct {
		name string
		pods []corev1.Pod
		// overrides nodeCPUUsageStampingMinFilterPercentage when non-zero;
		// 0 keeps the package default.
		minFilterPercentage int
		nodeMetrics         metrics.NodeMetricsMap
		want                []string
	}{
		{
			name: "empty metrics returns all pods",
			pods: []corev1.Pod{pod("a", "n1"), pod("b", "n2")},
			want: []string{"a", "b"},
		},
		{
			name: "all cold returns all pods",
			pods: []corev1.Pod{pod("a", "n1"), pod("b", "n2")},
			nodeMetrics: metrics.NodeMetricsMap{
				"n1": {CpuAvg5m: 40},
				"n2": {CpuAvg5m: 60},
			},
			want: []string{"a", "b"},
		},
		{
			name: "hot node excluded",
			pods: []corev1.Pod{pod("cold", "n1"), pod("hot", "n2")},
			nodeMetrics: metrics.NodeMetricsMap{
				"n1": {CpuAvg5m: 40},
				"n2": {CpuAvg5m: 90},
			},
			want: []string{"cold"},
		},
		{
			// filter uses strict less-than, so a node exactly at the threshold
			// is treated as hot and excluded.
			name: "at threshold counts as hot",
			pods: []corev1.Pod{pod("edge", "n1"), pod("hot", "n2")},
			nodeMetrics: metrics.NodeMetricsMap{
				"n1": {CpuAvg5m: 69.99},
				"n2": {CpuAvg5m: 70},
			},
			want: []string{"edge"},
		},
		{
			name: "all nodes hot falls back to all pods",
			pods: []corev1.Pod{pod("a", "n1"), pod("b", "n2")},
			nodeMetrics: metrics.NodeMetricsMap{
				"n1": {CpuAvg5m: 90},
				"n2": {CpuAvg5m: 85},
			},
			want: []string{"a", "b"},
		},
		{
			// missing-metrics pods are skipped; nothing survives, so
			// min-filter-percentage fallback returns the full pool.
			name: "pod on node without metrics falls back to full pool",
			pods: []corev1.Pod{pod("unknown", "nX"), pod("hot", "n2")},
			nodeMetrics: metrics.NodeMetricsMap{
				"n2": {CpuAvg5m: 90},
			},
			want: []string{"hot", "unknown"},
		},
		{
			// MetricsNotReady pods are skipped like missing-metrics pods;
			// same fallback path.
			name: "MetricsNotReady node falls back to full pool",
			pods: []corev1.Pod{pod("warming", "n1"), pod("hot", "n2")},
			nodeMetrics: metrics.NodeMetricsMap{
				"n1": {
					CpuAvg5m:        90,
					MetricsNotReady: true,
				},
				"n2": {CpuAvg5m: 90},
			},
			want: []string{"hot", "warming"},
		},
		{
			// 2 cold 2 hot - 50% filter rate which is at default
			// so we do keep filtered pods
			name: "exactly at min filter percentage keeps filtered",
			pods: []corev1.Pod{
				pod("cold1", "n1"), pod("cold2", "n2"),
				pod("hot1", "n3"), pod("hot2", "n4"),
			},
			nodeMetrics: metrics.NodeMetricsMap{
				"n1": {CpuAvg5m: 40},
				"n2": {CpuAvg5m: 50},
				"n3": {CpuAvg5m: 90},
				"n4": {CpuAvg5m: 95},
			},
			want: []string{"cold1", "cold2"},
		},
		{
			// 1/4 cold so not enough to hit 50%; return all
			name: "below min filter percentage falls back to full pool",
			pods: []corev1.Pod{
				pod("cold", "n1"),
				pod("hot1", "n2"), pod("hot2", "n3"), pod("hot3", "n4"),
			},
			nodeMetrics: metrics.NodeMetricsMap{
				"n1": {CpuAvg5m: 40},
				"n2": {CpuAvg5m: 90},
				"n3": {CpuAvg5m: 91},
				"n4": {CpuAvg5m: 92},
			},
			want: []string{"cold", "hot1", "hot2", "hot3"},
		},
		{
			// 2/4 nodes cold but min filter percentage is 75%, so return all
			name: "custom min filter percentage raises fallback point",
			pods: []corev1.Pod{
				pod("cold1", "n1"), pod("cold2", "n2"),
				pod("hot1", "n3"), pod("hot2", "n4"),
			},
			minFilterPercentage: 75,
			nodeMetrics: metrics.NodeMetricsMap{
				"n1": {CpuAvg5m: 40},
				"n2": {CpuAvg5m: 50},
				"n3": {CpuAvg5m: 90},
				"n4": {CpuAvg5m: 95},
			},
			want: []string{"cold1", "cold2", "hot1", "hot2"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.minFilterPercentage != 0 {
				orig := nodeCPUUsageStampingMinFilterPercentage
				nodeCPUUsageStampingMinFilterPercentage = tc.minFilterPercentage
				t.Cleanup(func() { nodeCPUUsageStampingMinFilterPercentage = orig })
			}
			got := filterCandidatesByNodeCPUUsage(context.Background(), tc.pods, tc.nodeMetrics)
			require.Equal(t, tc.want, names(got))
		})
	}
}

// TestLoadTenantEnvVarConfig covers the loader's parse-and-override paths for
// each env var: unset, valid override, invalid parse (keeps default).
func TestLoadTenantEnvVarConfig(t *testing.T) {
	const (
		defaultThreshold           = 70
		defaultMinFilterPercentage = 50
	)
	// save and restore so overrides don't leak across subtests or tests.
	origThreshold := nodeCPUUsageStampingThreshold
	origMinFilterPercentage := nodeCPUUsageStampingMinFilterPercentage
	t.Cleanup(func() {
		nodeCPUUsageStampingThreshold = origThreshold
		nodeCPUUsageStampingMinFilterPercentage = origMinFilterPercentage
	})

	cases := []struct {
		name     string
		envKey   string
		envValue string
		target   *int
		want     int
	}{
		{
			name:     "threshold empty env keeps default",
			envKey:   nodeCPUUsageStampingThresholdEnvVar,
			envValue: "",
			target:   &nodeCPUUsageStampingThreshold,
			want:     defaultThreshold,
		},
		{
			name:     "threshold valid override applies",
			envKey:   nodeCPUUsageStampingThresholdEnvVar,
			envValue: "85",
			target:   &nodeCPUUsageStampingThreshold,
			want:     85,
		},
		{
			name:     "threshold invalid string keeps default",
			envKey:   nodeCPUUsageStampingThresholdEnvVar,
			envValue: "not-a-number",
			target:   &nodeCPUUsageStampingThreshold,
			want:     defaultThreshold,
		},
		{
			name:     "min filter percentage empty env keeps default",
			envKey:   nodeCPUUsageStampingMinFilterPercentageEnvVar,
			envValue: "",
			target:   &nodeCPUUsageStampingMinFilterPercentage,
			want:     defaultMinFilterPercentage,
		},
		{
			name:     "min filter percentage valid override applies",
			envKey:   nodeCPUUsageStampingMinFilterPercentageEnvVar,
			envValue: "75",
			target:   &nodeCPUUsageStampingMinFilterPercentage,
			want:     75,
		},
		{
			name:     "min filter percentage invalid string keeps default",
			envKey:   nodeCPUUsageStampingMinFilterPercentageEnvVar,
			envValue: "abc",
			target:   &nodeCPUUsageStampingMinFilterPercentage,
			want:     defaultMinFilterPercentage,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nodeCPUUsageStampingThreshold = defaultThreshold
			nodeCPUUsageStampingMinFilterPercentage = defaultMinFilterPercentage
			t.Setenv(tc.envKey, tc.envValue)
			LoadTenantEnvVarConfig(context.Background())
			require.Equal(t, tc.want, *tc.target)
		})
	}
}

func TestIsUnstampedPendingPod(t *testing.T) {
	pod := &corev1.Pod{}
	require.False(t, isUnstampedPendingPod(pod))
	require.NoError(t, manifests.SetTenantPodState(pod, manifests.TenantPodRunningState))
	require.False(t, isUnstampedPendingPod(pod))
	require.NoError(t, manifests.SetTenantPodState(pod, manifests.TenantPodDrainingState))
	require.False(t, isUnstampedPendingPod(pod))
	require.NoError(t, manifests.SetTenantPodState(pod, manifests.TenantPodPendingState))
	require.True(t, isUnstampedPendingPod(pod))
	pod.Labels[manifests.TenantIDLabel] = "10"
	require.False(t, isUnstampedPendingPod(pod))
}

func TestIsReadyToTerminate(t *testing.T) {
	makePod := func(state manifests.TenantPodState, drainAge time.Duration) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-pod",
				Labels:      map[string]string{},
				Annotations: map[string]string{},
			},
		}
		pod.Labels[manifests.TenantPodStateLabel] = string(state)
		if state == manifests.TenantPodDrainingState {
			ts := timeutil.Now().Add(-drainAge).UTC()
			pod.Annotations[manifests.TenantPodStateTimestampAnnotation] = ts.Format(manifests.PodStateTimestampFormat)
		}
		return pod
	}

	makeCrashLoopPod := func() *corev1.Pod {
		pod := makePod(manifests.TenantPodDrainingState, 0)
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{
			{
				Name:  manifests.CockroachDB,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			},
		}
		return pod
	}

	tests := []struct {
		name       string
		pod        *corev1.Pod
		podMetrics *metrics.PodMetrics
		expected   bool
	}{
		{
			name:       "RunningPodNotTerminated",
			pod:        makePod(manifests.TenantPodRunningState, 0),
			podMetrics: &metrics.PodMetrics{SqlConns: 0},
			expected:   false,
		},
		{
			name:       "DrainingBelowMinDrainPeriod",
			pod:        makePod(manifests.TenantPodDrainingState, 30*time.Second),
			podMetrics: &metrics.PodMetrics{SqlConns: 0},
			expected:   false,
		},
		{
			name:       "DrainingNilMetricsBelowMinDrainPeriod",
			pod:        makePod(manifests.TenantPodDrainingState, 30*time.Second),
			podMetrics: nil,
			expected:   false,
		},
		{
			name:       "DrainingZeroConnsAboveMinDrain",
			pod:        makePod(manifests.TenantPodDrainingState, 2*time.Minute),
			podMetrics: &metrics.PodMetrics{SqlConns: 0},
			expected:   true,
		},
		{
			name:       "DrainingActiveConnsBeforeForceTimeout",
			pod:        makePod(manifests.TenantPodDrainingState, 5*time.Minute),
			podMetrics: &metrics.PodMetrics{SqlConns: 3},
			expected:   false,
		},
		{
			name:       "DrainingActiveConnsAtForceTimeout",
			pod:        makePod(manifests.TenantPodDrainingState, ForceDrainTimeout),
			podMetrics: &metrics.PodMetrics{SqlConns: 3},
			expected:   true,
		},
		{
			name:       "DrainingNilMetricsBeforeForceTimeout",
			pod:        makePod(manifests.TenantPodDrainingState, 5*time.Minute),
			podMetrics: nil,
			expected:   false,
		},
		{
			name:       "DrainingNilMetricsAtForceTimeout",
			pod:        makePod(manifests.TenantPodDrainingState, ForceDrainTimeout),
			podMetrics: nil,
			expected:   true,
		},
		{
			name:       "DrainingNilMetricsPastForceTimeout",
			pod:        makePod(manifests.TenantPodDrainingState, 20*time.Minute),
			podMetrics: nil,
			expected:   true,
		},
		{
			name:       "CrashLoopBackOffTerminatedImmediately",
			pod:        makeCrashLoopPod(),
			podMetrics: nil,
			expected:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, isReadyToTerminate(tc.pod, tc.podMetrics))
		})
	}
}

var testCACert = []byte(`
-----BEGIN CERTIFICATE-----
MIIBVjCB/qADAgECAgEAMAoGCCqGSM49BAMCMCMxITAfBgNVBAMMGGszcy1zZXJ2
ZXItY2FAMTYxNzk0MzAzNjAeFw0yMTA0MDkwNDM3MTZaFw0zMTA0MDcwNDM3MTZa
MCMxITAfBgNVBAMMGGszcy1zZXJ2ZXItY2FAMTYxNzk0MzAzNjBZMBMGByqGSM49
AgEGCCqGSM49AwEHA0IABEG5Yj+Jarlz9vTYcs6oMoher9CIkV+Y/7yIoyaYN+0m
uUMzegqYozdOusSBFBUHOjA6uazFppXdp6ZFpV/Kf9ajIzAhMA4GA1UdDwEB/wQE
AwICpDAPBgNVHRMBAf8EBTADAQH/MAoGCCqGSM49BAMCA0cAMEQCIH8gzLuBS2NM
F7B50nr3lO2RsK1EjpGc4n75bewYVXPxAiBYUic6SR8z2w5yyZ+IGldmQOvhUVvR
piL1sLD1Pvk+SA==
-----END CERTIFICATE-----
`)

func makeTenantCerts(t *testing.T, ctl *kube.Ctl, tenantID crdbv1beta1.TenantID) {
	t.Helper()
	certName := manifests.MakeTenantClientCertName(tenantID)
	signingCertName := manifests.MakeTenantSigningCertName(tenantID)
	clientCert := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      manifests.MakeTenantClientCertSecretName(tenantID),
			Namespace: "default",
		},
		Data: map[string][]byte{
			"ca.crt":                 testCACert,
			certName + ".crt":        {7, 8},
			certName + ".key":        {9, 10},
			signingCertName + ".crt": {11, 12},
			signingCertName + ".key": {13, 14},
			"client.root.crt":        {15, 16},
			"client.root.key":        {17, 18},
		},
	}
	sqlStarterCert := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      manifests.SqlStarterCertSecretName,
			Namespace: "default",
		},
		Data: map[string][]byte{
			"ca.crt":                              testCACert,
			manifests.SqlStarterCertName + ".crt": {19, 20},
			manifests.SqlStarterCertName + ".key": {21, 22},
		},
	}
	require.NoError(t, ctl.Apply(context.Background(), &clientCert, &sqlStarterCert))
}

func testReadNodeLocality(ctx context.Context, nodeName string) (cloud.LocalityInfo, error) {
	return cloud.LocalityInfo{
		Region: nodeName + "-region-1",
		Zone:   nodeName + "-zone-1",
	}, nil
}

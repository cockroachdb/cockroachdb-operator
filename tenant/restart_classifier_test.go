package tenant

import (
	"context"
	"strings"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/restartclass"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// buildTestTenant returns a minimal CrdbTenant suitable for restart-classifier
// integration tests. The Initialized condition is set so FakeCtl considers the
// tenant ready for updates. Annotations starts nil so we can verify the function
// initializes the map when writing eviction-dedup entries.
func buildTestTenant(name string, tenantID uint64) *crdbv1beta1.CrdbTenant {
	tenant := &crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: crdbv1beta1.CrdbTenantSpec{
			TenantID: crdbv1beta1.TenantID(tenantID),
		},
	}
	tenant.Status.SetCondition(crdbv1beta1.TenantInitialized, metav1.ConditionTrue)
	return tenant
}

// buildStampedPod returns a pod that looks like a stamped tenant SQL pod.
// terminated may be nil to simulate a pod with no LastTerminationState.
// statusReason can be set to "Evicted" to simulate a memory-eviction event.
func buildStampedPod(
	name string,
	tenantID uint64,
	terminated *corev1.ContainerStateTerminated,
	statusReason string,
) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				"cluster":                     "test-cluster",
				manifests.TenantIDLabel:       crdbv1beta1.TenantID(tenantID).String(),
				manifests.TenantPodStateLabel: string(manifests.TenantPodRunningState),
			},
		},
		Status: corev1.PodStatus{
			Reason:  statusReason,
			Message: "test eviction message",
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         manifests.CockroachDB,
					RestartCount: 1,
					LastTerminationState: corev1.ContainerState{
						Terminated: terminated,
					},
				},
			},
		},
	}
}

func TestClassifyRestart(t *testing.T) {
	testCases := []struct {
		name      string
		pod       *corev1.Pod
		expected  restartclass.RestartReason
		expectNil bool
	}{
		{
			name: "OOMKilled",
			pod: buildStampedPod("p", 1, &corev1.ContainerStateTerminated{
				ExitCode: 137,
				Reason:   "OOMKilled",
			}, ""),
			expected: restartclass.RestartReasonOOMKilled,
		},
		{
			name: "GracefulShutdown",
			pod: buildStampedPod("p", 1, &corev1.ContainerStateTerminated{
				ExitCode: 0,
				Reason:   "Completed",
			}, ""),
			expected: restartclass.RestartReasonGracefulShutdown,
		},
		{
			name: "UnexpectedSIGKILL",
			pod: buildStampedPod("p", 1, &corev1.ContainerStateTerminated{
				ExitCode: 137,
				Reason:   "",
			}, ""),
			expected: restartclass.RestartReasonUnexpectedSIGKILL,
		},
		{
			name: "KnownExitCode_FatalError",
			pod: buildStampedPod("p", 1, &corev1.ContainerStateTerminated{
				ExitCode: 7,
			}, ""),
			expected: restartclass.RestartReasonFatalError,
		},
		{
			name: "UnknownExitCode",
			pod: buildStampedPod("p", 1, &corev1.ContainerStateTerminated{
				ExitCode: 99,
			}, ""),
			expected: restartclass.RestartReasonUnspecifiedError,
		},
		{
			name:      "NoLastTerminationState",
			pod:       buildStampedPod("p", 1, nil, ""),
			expected:  restartclass.RestartReasonNoLastTerminationState,
			expectNil: true,
		},
		{
			name: "CockroachContainerNotFound",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{Name: "some-sidecar"},
					},
				},
			},
			expected:  restartclass.RestartReasonCockroachContainerNotFound,
			expectNil: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			reason, cs := restartclass.ClassifyRestart(tc.pod)
			require.Equal(t, tc.expected, reason)
			if tc.expectNil {
				require.Nil(t, cs)
			} else {
				require.NotNil(t, cs)
			}
		})
	}
}

func TestRecordPodRestartsIntegration(t *testing.T) {
	ctx := context.Background()

	// Capture all logs so each subtest can assert on what was emitted. Subtests
	// must call observedLogs.TakeAll() before running to clear previous output.
	observedCore, observedLogs := observer.New(zap.InfoLevel)
	logging.SetGlobalLogger(zap.New(observedCore))

	// BeforeMutation: MakeReady auto-stamps readiness on every Apply/Update so
	// ctl.Update on pods and tenants succeeds without timing out on the
	// IsReady wait that FakeCtl checks exactly once.
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{BeforeMutation: kubetest.MakeReady},
	}).WithNamespace("default")

	// restartLogs is a helper that returns only the structured logs emitted by
	// logTenantPodRestart, filtering out unrelated FakeCtl chatter. Callers
	// should consume (via TakeAll on the underlying observer) at the end of
	// each subtest to keep state from leaking across t.Run blocks.
	restartLogs := func() []observer.LoggedEntry {
		entries := observedLogs.FilterMessage("tenant pod restart detected").All()
		observedLogs.TakeAll()
		return entries
	}

	t.Run("OOMKilled", func(t *testing.T) {
		observedLogs.TakeAll()

		tenant := buildTestTenant("oom-tenant", 100)
		pod := buildStampedPod("pod-oom", 100, &corev1.ContainerStateTerminated{
			ExitCode: 137,
			Reason:   "OOMKilled",
		}, "")
		ctl.Apply(ctx, tenant, pod)

		RecordPodRestarts(ctx, ctl, tenant, []corev1.Pod{*pod})

		logs := restartLogs()
		require.Len(t, logs, 1)
		fields := logs[0].ContextMap()
		require.Equal(t, "OOMKilled", fields["restart_reason"])
		require.Equal(t, "100", fields["tenant_id"])
		require.Equal(t, "pod-oom", fields["pod_name"])
	})

	t.Run("CrashRestartDedup", func(t *testing.T) {
		observedLogs.TakeAll()

		tenant := buildTestTenant("dedup-tenant", 101)
		pod := buildStampedPod("pod-dedup", 101, &corev1.ContainerStateTerminated{
			ExitCode:   137,
			Reason:     "OOMKilled",
			FinishedAt: metav1.Time{Time: metav1.Now().Time},
		}, "")
		ctl.Apply(ctx, tenant, pod)

		// Fetch the pod from FakeCtl before each reconcile so both calls see the
		// same stored representation. The real reconciler always does a ctl.Get
		// before passing pods to RecordPodRestarts. If we pass the in-memory pod
		// directly the first time, its FinishedAt has higher precision than the
		// stored copy, and the dedup string written on reconcile 1 will not match
		// the string computed on reconcile 2.
		var first corev1.Pod
		require.NoError(t, ctl.Get(ctx, "pod-dedup", &first))
		RecordPodRestarts(ctx, ctl, tenant, []corev1.Pod{first})
		require.Len(t, restartLogs(), 1)

		var second corev1.Pod
		require.NoError(t, ctl.Get(ctx, "pod-dedup", &second))
		require.Contains(t, second.Annotations, lastRestartLoggedAnnotation)
		RecordPodRestarts(ctx, ctl, tenant, []corev1.Pod{second})
		require.Len(t, restartLogs(), 0)
	})

	t.Run("MemoryEviction", func(t *testing.T) {
		observedLogs.TakeAll()

		tenant := buildTestTenant("evict-tenant", 102)
		pod := buildStampedPod("pod-evict", 102, nil, restartclass.PodReasonEvicted)
		ctl.Apply(ctx, tenant, pod)

		RecordPodRestarts(ctx, ctl, tenant, []corev1.Pod{*pod})

		logs := restartLogs()
		require.Len(t, logs, 1)
		require.Equal(t, "NodePressureEviction", logs[0].ContextMap()["restart_reason"])

		// Verify the dedup annotation was written to the tenant (not the pod, since
		// evicted pods are about to be deleted).
		var refreshed crdbv1beta1.CrdbTenant
		require.NoError(t, ctl.Get(ctx, "evict-tenant", &refreshed))
		require.Contains(t, refreshed.Annotations, evictionKeyFor("pod-evict"))
	})

	t.Run("MemoryEvictionDedup", func(t *testing.T) {
		observedLogs.TakeAll()

		tenant := buildTestTenant("evict-dedup-tenant", 103)
		pod := buildStampedPod("pod-evict-dedup", 103, nil, restartclass.PodReasonEvicted)
		ctl.Apply(ctx, tenant, pod)

		// First reconcile: log fires, tenant annotation written.
		RecordPodRestarts(ctx, ctl, tenant, []corev1.Pod{*pod})
		require.Len(t, restartLogs(), 1)

		// Re-fetch the tenant so we see the annotation.
		var refreshed crdbv1beta1.CrdbTenant
		require.NoError(t, ctl.Get(ctx, "evict-dedup-tenant", &refreshed))

		// Second reconcile with the same evicted pod: log should NOT fire again.
		RecordPodRestarts(ctx, ctl, &refreshed, []corev1.Pod{*pod})
		require.Len(t, restartLogs(), 0)
	})

	t.Run("RollingUpdate", func(t *testing.T) {
		observedLogs.TakeAll()

		// Verify the log shape that scale.go emits when a stale pod transitions
		// to DRAINING. The hook in scale.go calls logTenantPodRestart directly
		// rather than going through RecordPodRestarts, so this test mirrors that.
		tenant := buildTestTenant("rolling-tenant", 105)
		pod := buildStampedPod("pod-rolling", 105, nil, "")
		logTenantPodRestart(context.Background(), pod, tenant, restartclass.RestartReasonRollingUpdate)

		logs := restartLogs()
		require.Len(t, logs, 1)
		fields := logs[0].ContextMap()
		require.Equal(t, "RollingUpdate", fields["restart_reason"])
		require.Equal(t, "105", fields["tenant_id"])
		require.Equal(t, "pod-rolling", fields["pod_name"])
		// RollingUpdate is an intentional drain, not a crash. No exit_code field.
		require.NotContains(t, fields, "exit_code")
	})

	t.Run("MemoryEvictionAnnotationCleared", func(t *testing.T) {
		observedLogs.TakeAll()

		// Tenant starts with a stale eviction annotation for a pod that no longer
		// exists. Cleanup should remove the entry on this reconcile.
		tenant := buildTestTenant("cleanup-tenant", 104)
		tenant.Annotations = map[string]string{
			evictionAnnotationPrefix + "ghost-pod": "true",
		}
		ctl.Apply(ctx, tenant)

		// Pods slice does NOT contain ghost-pod, so the annotation is stale.
		RecordPodRestarts(ctx, ctl, tenant, []corev1.Pod{})

		var refreshed crdbv1beta1.CrdbTenant
		require.NoError(t, ctl.Get(ctx, "cleanup-tenant", &refreshed))
		require.NotContains(t, refreshed.Annotations, evictionAnnotationPrefix+"ghost-pod")
	})
}

func TestEvictionKeyForStaysUnderAnnotationLimit(t *testing.T) {
	longPod := "tenantpool-" + strings.Repeat("x", 200) + "-79b8d84d7d-6zv44"
	key := evictionKeyFor(longPod)
	nameSegment := key[strings.Index(key, "/")+1:]
	require.LessOrEqual(t, len(nameSegment), 63)
}

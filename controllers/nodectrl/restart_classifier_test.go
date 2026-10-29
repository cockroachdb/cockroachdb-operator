package nodectrl

import (
	"context"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/restartclass"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode/nodemock"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/evictionutil"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

// buildPod builds a pod with configurations defined with exitCode and restartReason.
// It always creates a cockroachdb container with LastTerminationState set.
// For NoLastTerminationState and CockroachContainerNotFound cases, use restartclass.ClassifyRestart directly.
func buildPod(exitCode int32, restartReason string) *corev1.Pod {
	pod := corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         manifests.CRDBContainerName,
					RestartCount: 1,
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: exitCode,
							Reason:   restartReason,
						},
					},
				},
			},
		},
	}
	return &pod
}

func TestClassifyRestart(t *testing.T) {
	testCases := []struct {
		name     string
		exitCode int32
		reason   string
		expected restartclass.RestartReason
	}{
		{"OOMKilled", 137, "OOMKilled", restartclass.RestartReasonOOMKilled},
		{"UnexpectedSIGKILL", 137, "Error", restartclass.RestartReasonUnexpectedSIGKILL},
		{"GracefulShutdown", 0, "Completed", restartclass.RestartReasonGracefulShutdown},
		{"UnspecifiedError", 1, "Error", restartclass.RestartReasonUnspecifiedError},
		{"UnspecifiedGoPanic", 2, "Error", restartclass.RestartReasonUnspecifiedGoPanic},
		{"Interrupted", 3, "Error", restartclass.RestartReasonInterrupted},
		{"CommandLineFlagError", 4, "Error", restartclass.RestartReasonCommandLineFlagError},
		{"LoggingStderrUnavailable", 5, "Error", restartclass.RestartReasonLoggingStderrUnavailable},
		{"LoggingFileUnavailable", 6, "Error", restartclass.RestartReasonLoggingFileUnavailable},
		{"FatalError", 7, "Error", restartclass.RestartReasonFatalError},
		{"TimeoutAfterFatalError", 8, "Error", restartclass.RestartReasonTimeoutAfterFatalError},
		{"LoggingNetCollectorUnavailable", 9, "Error", restartclass.RestartReasonLoggingNetCollectorUnavailable},
		{"DiskFull", 10, "Error", restartclass.RestartReasonDiskFull},
		{"DoctorValidationFailed", 125, "Error", restartclass.RestartReasonDoctorValidationFailed},
		{"OperatorKilled", 138, "Error", restartclass.RestartReasonOperatorKilled},
		{"UnexpectedSIGTERM", 143, "Error", restartclass.RestartReasonUnexpectedSIGTERM},
		{"UnknownExitCode", 99, "Error", restartclass.RestartReasonUnspecifiedError},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			currentPod := buildPod(tc.exitCode, tc.reason)
			reason, _ := restartclass.ClassifyRestart(currentPod)
			require.Equal(t, tc.expected, reason)
		})
	}

	t.Run("NoLastTerminationState", func(t *testing.T) {
		pod := &corev1.Pod{
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{Name: "cockroachdb"},
				},
			},
		}
		reason, cs := restartclass.ClassifyRestart(pod)
		require.Equal(t, restartclass.RestartReasonNoLastTerminationState, reason)
		require.Nil(t, cs)
	})

	t.Run("CockroachContainerNotFound", func(t *testing.T) {
		pod := &corev1.Pod{}
		reason, cs := restartclass.ClassifyRestart(pod)
		require.Equal(t, restartclass.RestartReasonCockroachContainerNotFound, reason)
		require.Nil(t, cs)
	})
}

// buildTestObjects builds a CrdbNode and a ready Pod with the given terminated state.
// Pass nil for terminated to simulate NoLastTerminationState.
func buildTestObjects(name string, terminated *corev1.ContainerStateTerminated) (*crdbv1beta1.CrdbNode, *corev1.Pod) {
	crdbNode := &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
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
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         manifests.CRDBContainerName,
					RestartCount: 1,
					LastTerminationState: corev1.ContainerState{
						Terminated: terminated,
					},
				},
			},
		},
	}
	return crdbNode, &pod
}

func TestClassifyRestartIntegration(t *testing.T) {
	ctx := context.Background()
	mockCtrl := gomock.NewController(t)

	// Capture logs so each subtest can assert on what was emitted. Subtests
	// call restartLogs() which returns just the classifier's log entries and
	// clears the buffer so the next subtest starts fresh.
	observedCore, observedLogs := observer.New(zap.InfoLevel)
	logging.SetGlobalLogger(zap.New(observedCore))

	restartLogs := func() []observer.LoggedEntry {
		entries := observedLogs.FilterMessage("pod restart detected").All()
		observedLogs.TakeAll()
		return entries
	}

	ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("default")
	ctl.Apply(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "a-vm",
		},
	})

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

	reconcile := func(name string) {
		req := ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      name,
				Namespace: "default",
			},
		}
		for {
			res, err := r.Reconcile(ctx, req)
			if err != nil {
				t.Logf("reconcile error for %q: %v", name, err)
				break
			}
			if !res.Requeue && res.RequeueAfter == 0 {
				break
			}
		}
	}

	t.Run("OOMKilled", func(t *testing.T) {
		observedLogs.TakeAll()
		crdbNode, pod := buildTestObjects("oom-killed", &corev1.ContainerStateTerminated{
			ExitCode: 137,
			Reason:   string(restartclass.RestartReasonOOMKilled),
		})
		ctl.Apply(ctx, crdbNode, pod)
		reconcile("oom-killed")
		logs := restartLogs()
		require.Len(t, logs, 1)
		fields := logs[0].ContextMap()
		require.Equal(t, "OOMKilled", fields["restart_reason"])
		require.Equal(t, "oom-killed", fields["node_name"])
	})

	t.Run("CrashRestartDedup", func(t *testing.T) {
		observedLogs.TakeAll()
		crdbNode, pod := buildTestObjects("crash-dedup", &corev1.ContainerStateTerminated{
			ExitCode:   137,
			Reason:     string(restartclass.RestartReasonOOMKilled),
			FinishedAt: metav1.Now(),
		})
		ctl.Apply(ctx, crdbNode, pod)
		reconcile("crash-dedup")
		reconcile("crash-dedup")
		require.Len(t, restartLogs(), 1)
	})

	t.Run("NoLastTerminationState", func(t *testing.T) {
		observedLogs.TakeAll()
		crdbNode, pod := buildTestObjects("no-lts", nil)
		ctl.Apply(ctx, crdbNode, pod)
		reconcile("no-lts")
		require.Len(t, restartLogs(), 0)
	})

	t.Run("MemoryEviction", func(t *testing.T) {
		observedLogs.TakeAll()
		crdbNode, _ := buildTestObjects("evicted-node", nil)
		evictedPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "evicted-node",
				Namespace: "default",
				UID:       "evicted-node-uid",
			},
			Status: corev1.PodStatus{
				Phase:   corev1.PodFailed,
				Reason:  "Evicted",
				Message: "The node was low on resource: memory. Threshold quantity: 100Mi, available: 10Mi.",
			},
		}
		ctl.Apply(ctx, crdbNode, evictedPod)
		reconcile("evicted-node")
		logs := restartLogs()
		require.Len(t, logs, 1)
		require.Equal(t, string(restartclass.RestartReasonNodePressureEviction), logs[0].ContextMap()["restart_reason"])

		var updatedNode crdbv1beta1.CrdbNode
		require.NoError(t, ctl.Get(ctx, "evicted-node", &updatedNode))
		require.Equal(t, "evicted-node-uid", updatedNode.Annotations[lastEvictionLoggedAnnotation])
	})

	t.Run("MemoryEvictionDedup", func(t *testing.T) {
		observedLogs.TakeAll()
		crdbNode, _ := buildTestObjects("evicted-node-dedup", nil)
		// The annotation already holds this pod's UID, so the eviction was already
		// logged and this reconcile must skip it.
		crdbNode.Annotations = map[string]string{
			lastEvictionLoggedAnnotation: "evicted-node-dedup-uid",
		}
		evictedPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "evicted-node-dedup",
				Namespace: "default",
				UID:       "evicted-node-dedup-uid",
			},
			Status: corev1.PodStatus{
				Phase:   corev1.PodFailed,
				Reason:  "Evicted",
				Message: "The node was low on resource: memory. Threshold quantity: 100Mi, available: 10Mi.",
			},
		}
		ctl.Apply(ctx, crdbNode, evictedPod)
		reconcile("evicted-node-dedup")
		require.Len(t, restartLogs(), 0)
	})

	t.Run("MemoryEvictionReplacementPodLogs", func(t *testing.T) {
		observedLogs.TakeAll()
		crdbNode, _ := buildTestObjects("evicted-node-repeat", nil)
		// A previous eviction was logged under the old pod's UID. Under sustained
		// pressure the replacement pod (same name, new UID) is evicted again before
		// any reconcile observed it non-evicted, so the annotation was never cleared.
		// The new UID must still be logged as a distinct eviction rather than deduped.
		crdbNode.Annotations = map[string]string{
			lastEvictionLoggedAnnotation: "old-pod-uid",
		}
		evictedPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "evicted-node-repeat",
				Namespace: "default",
				UID:       "new-pod-uid",
			},
			Status: corev1.PodStatus{
				Phase:   corev1.PodFailed,
				Reason:  "Evicted",
				Message: "The node was low on resource: memory. Threshold quantity: 100Mi, available: 10Mi.",
			},
		}
		ctl.Apply(ctx, crdbNode, evictedPod)
		reconcile("evicted-node-repeat")
		logs := restartLogs()
		require.Len(t, logs, 1)
		require.Equal(t, string(restartclass.RestartReasonNodePressureEviction), logs[0].ContextMap()["restart_reason"])

		var updatedNode crdbv1beta1.CrdbNode
		require.NoError(t, ctl.Get(ctx, "evicted-node-repeat", &updatedNode))
		require.Equal(t, "new-pod-uid", updatedNode.Annotations[lastEvictionLoggedAnnotation])
	})

	t.Run("MemoryEvictionAnnotationClears", func(t *testing.T) {
		observedLogs.TakeAll()
		crdbNode, pod := buildTestObjects("eviction-cleared-node", nil)
		crdbNode.Annotations = map[string]string{
			lastEvictionLoggedAnnotation: "eviction-cleared-node",
		}
		ctl.Apply(ctx, crdbNode, pod)
		reconcile("eviction-cleared-node")
		var updatedNode crdbv1beta1.CrdbNode
		require.NoError(t, ctl.Get(ctx, "eviction-cleared-node", &updatedNode))
		require.NotContains(t, updatedNode.Annotations, lastEvictionLoggedAnnotation)
	})

	// RollingUpdate fires when the operator evicts a pod because its deployed
	// revision is out of date and the CrdbNode carries a last-spec-change
	// annotation. The pod here has a stale generation annotation, which makes
	// podNeedsUpdate return true and drive the reconciler down the eviction path.
	t.Run("RollingUpdate", func(t *testing.T) {
		observedLogs.TakeAll()
		crdbNode := &crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "rolling-update-node",
				Namespace:  "default",
				Generation: 2,
				Finalizers: []string{
					manifests.CrdbNodeFinalizerName,
				},
				Annotations: map[string]string{
					manifests.LastSpecChangeAnnotationKey: "spec.image (v24.1 → v24.2)",
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
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "rolling-update-node",
				Namespace: "default",
				Annotations: map[string]string{
					manifests.NodeGenerationAnnotationKey: "1",
				},
			},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{
						Type:   corev1.PodReady,
						Status: corev1.ConditionTrue,
					},
				},
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:         manifests.CRDBContainerName,
						RestartCount: 0,
					},
				},
			},
		}
		ctl.Apply(ctx, crdbNode, pod)
		reconcile("rolling-update-node")
		logs := restartLogs()
		require.Len(t, logs, 1)
		require.Equal(t, string(restartclass.RestartReasonRollingUpdate), logs[0].ContextMap()["restart_reason"])
		var updatedNode crdbv1beta1.CrdbNode
		require.NoError(t, ctl.Get(ctx, "rolling-update-node", &updatedNode))
		require.NotContains(t, updatedNode.Annotations, manifests.LastSpecChangeAnnotationKey)
	})

	// Regression test for the race where pods with prior crash history would
	// miss RollingUpdate classification. The previous design routed any pod
	// with a populated LastTerminationState into the crash branch, which never
	// checked the spec-change annotation. With detection moved to the eviction
	// site, classification now fires regardless of LTS state.
	t.Run("RollingUpdateWithCrashHistory", func(t *testing.T) {
		observedLogs.TakeAll()
		crdbNode := &crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "rolling-update-with-scar",
				Namespace:  "default",
				Generation: 2,
				Finalizers: []string{
					manifests.CrdbNodeFinalizerName,
				},
				Annotations: map[string]string{
					manifests.LastSpecChangeAnnotationKey: "spec.terminationGracePeriod (1m0s → 5m0s)",
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
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "rolling-update-with-scar",
				Namespace: "default",
				Annotations: map[string]string{
					manifests.NodeGenerationAnnotationKey: "1",
				},
			},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{
						Type:   corev1.PodReady,
						Status: corev1.ConditionTrue,
					},
				},
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:         manifests.CRDBContainerName,
						RestartCount: 1,
						LastTerminationState: corev1.ContainerState{
							Terminated: &corev1.ContainerStateTerminated{
								ExitCode: 137,
								Reason:   "OOMKilled",
							},
						},
					},
				},
			},
		}
		ctl.Apply(ctx, crdbNode, pod)
		reconcile("rolling-update-with-scar")
		// A pod with crash history fires its own OOMKilled log from the crash
		// branch. The point of this regression test is that RollingUpdate is
		// still recorded from the eviction path in that case, not that it is
		// the only log emitted. The original metric-based test made the same
		// scope choice: it asserted only on the RollingUpdate counter.
		reasons := []string{}
		for _, log := range restartLogs() {
			reasons = append(reasons, log.ContextMap()["restart_reason"].(string))
		}
		require.Contains(t, reasons, string(restartclass.RestartReasonRollingUpdate))
		var updatedNode crdbv1beta1.CrdbNode
		require.NoError(t, ctl.Get(ctx, "rolling-update-with-scar", &updatedNode))
		require.NotContains(t, updatedNode.Annotations, manifests.LastSpecChangeAnnotationKey)
	})

	// Eviction can be driven by causes other than a tracked spec change, for
	// example a manual revision bump without going through the spec-diff path.
	// In that case the CrdbNode carries no last-spec-change annotation and we
	// must not record the eviction as a RollingUpdate.
	t.Run("RollingUpdateNotFiredWithoutAnnotation", func(t *testing.T) {
		observedLogs.TakeAll()
		crdbNode := &crdbv1beta1.CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "eviction-without-spec-change",
				Namespace:  "default",
				Generation: 2,
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
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "eviction-without-spec-change",
				Namespace: "default",
				Annotations: map[string]string{
					manifests.NodeGenerationAnnotationKey: "1",
				},
			},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{
						Type:   corev1.PodReady,
						Status: corev1.ConditionTrue,
					},
				},
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:         manifests.CRDBContainerName,
						RestartCount: 0,
					},
				},
			},
		}
		ctl.Apply(ctx, crdbNode, pod)
		reconcile("eviction-without-spec-change")
		require.Len(t, restartLogs(), 0)
	})
}

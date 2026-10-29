package tenant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/restartclass"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
)

const (
	// lastRestartLoggedAnnotation is written to a tenant pod to record the
	// FinishedAt timestamp of the last crash already logged. Used to dedup
	// crash events across reconciles.
	lastRestartLoggedAnnotation = "crdb.cockroachlabs.com/last-restart-logged"

	// evictionAnnotationPrefix is the prefix for per-pod eviction-dedup
	// annotations on the tenant. Full key is built by evictionKeyFor. Lives on
	// the tenant (not the pod) because evicted pods are deleted.
	evictionAnnotationPrefix = "crdb.cockroachlabs.com/last-eviction-logged-"
)

// evictionKeyFor returns the tenant annotation key that dedups a pod's eviction.
// The pod name is hashed so the key stays within the 63-character annotation-name
// limit regardless of pod name length.
func evictionKeyFor(podName string) string {
	sum := sha256.Sum256([]byte(podName))
	return evictionAnnotationPrefix + hex.EncodeToString(sum[:8])
}

// logTenantPodRestart emits a structured log line for a single tenant SQL pod
// restart event. Restart events are discrete and rare, so we ship them as logs
// rather than as a per-series counter. Dashboards query these logs directly.
func logTenantPodRestart(
	ctx context.Context,
	pod *corev1.Pod,
	tenant *crdbv1beta1.CrdbTenant,
	reason restartclass.RestartReason,
	extraFields ...zap.Field,
) {
	state, _ := manifests.GetTenantPodState(pod)
	fields := []zap.Field{
		// Tenant pool pods do not carry a cluster label like host KV pods do.
		// The customer-facing cluster name lives on the CrdbTenant CR.
		zap.String("cluster_name", tenant.Labels[crdbv1beta1.TenantClusterNameLabel]),
		zap.String("tenant_id", tenant.Spec.TenantID.String()),
		zap.String("pod_name", pod.Name),
		zap.String("pod_uid", string(pod.UID)),
		zap.String("pod_state", string(state)),
		zap.String("restart_reason", string(reason)),
	}
	fields = append(fields, extraFields...)
	logging.Info(ctx, "tenant pod restart detected", fields...)
}

// RecordPodRestarts iterates the tenant's stamped pods, classifies any fresh
// restart events, and emits a structured log per event. Two cases are handled:
// memory evictions (detected via pod.Status.Reason) and container crashes
// (detected via LastTerminationState). Dedup uses pod or tenant annotations so
// the same event isn't logged on every reconcile.
//
// The function does not return an error. Annotation write failures are demoted
// to warnings because the next reconcile will retry. Returning an error would
// trigger controller-runtime to requeue and block other reconcile work.
func RecordPodRestarts(ctx context.Context, ctl *kube.Ctl, tenant *crdbv1beta1.CrdbTenant, pods []corev1.Pod) {
	// tenantChanged tracks whether anything in this reconcile modified the
	// tenant's annotations. Both the eviction path (adds new entries) and
	// cleanUpAnnotation (removes stale ones) can flip it. We only call
	// ctl.Update on the tenant if at least one of them did, which avoids an
	// extra k8s API call on reconciles where nothing changed.
	tenantChanged := false

	for _, pod := range pods {
		// Memory eviction is checked before ClassifyRestart because evicted pods
		// have no LastTerminationState. If we fell through to the classifier
		// path, the event would be miscategorized as NoLastTerminationState
		// and the eviction would never get logged.
		if pod.Status.Reason == restartclass.PodReasonEvicted {
			evictionKey := evictionKeyFor(pod.Name)

			// Dedup on the tenant rather than the pod. The evicted pod is about
			// to be deleted by the kubelet, so a per-pod annotation would not
			// survive long enough for the next reconcile to read it.
			if _, alreadyLogged := tenant.Annotations[evictionKey]; !alreadyLogged {
				logTenantPodRestart(ctx, &pod, tenant, restartclass.RestartReasonNodePressureEviction,
					zap.String("message", pod.Status.Message),
				)
				if tenant.Annotations == nil {
					tenant.Annotations = make(map[string]string)
				}
				tenant.Annotations[evictionKey] = "true"
				tenantChanged = true
			}
			// Skip the crash path for evicted pods to avoid double-logging.
			continue
		}

		reason, cs := restartclass.ClassifyRestart(&pod)

		// ClassifyRestart returns a nil ContainerStatus only for the
		// NoLastTerminationState and CockroachContainerNotFound reasons, so this
		// check also filters out both of those cases.
		if cs == nil {
			continue
		}

		// currentFinishedAt is the timestamp the kubelet wrote when the
		// container last terminated. We compare it against the pod's
		// lastRestartLoggedAnnotation so the same crash is only logged once
		// across reconciles. A new crash advances FinishedAt and the comparison
		// fails, so the new event is logged.
		currentFinishedAt := cs.LastTerminationState.Terminated.FinishedAt.String()
		if pod.Annotations[lastRestartLoggedAnnotation] == currentFinishedAt {
			continue
		}

		terminated := cs.LastTerminationState.Terminated
		logFields := []zap.Field{
			zap.Int32("exit_code", terminated.ExitCode),
			zap.Int32("restart_count", cs.RestartCount),
		}
		// StartedAt is zero when the container was killed before it finished starting.
		// Subtracting a zero StartedAt yields a nonsensical duration, so only emit the
		// field when StartedAt is set.
		if !terminated.StartedAt.IsZero() {
			logFields = append(logFields, zap.Float64("previous_run_duration_seconds",
				terminated.FinishedAt.Sub(terminated.StartedAt.Time).Seconds()))
		}
		logTenantPodRestart(ctx, &pod, tenant, reason, logFields...)
		if pod.Annotations == nil {
			pod.Annotations = make(map[string]string)
		}
		pod.Annotations[lastRestartLoggedAnnotation] = currentFinishedAt

		// If the annotation write fails, the next reconcile will see the same
		// FinishedAt with no recorded dedup and log the event again. That's a
		// duplicate log, not a correctness failure, so a warning is enough.
		if err := ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, &pod); err != nil {
			logging.Warnf(ctx, "failed to write crash annotation, next reconcile may log again: %v", err)
		}
	}

	// Remove tenant annotations for evictions of pods that no longer exist.
	// Without this, the tenant's annotation map grows monotonically as pods
	// churn through the warm pool and the stamping cycle.
	if cleanUpAnnotation(tenant, pods) {
		tenantChanged = true
	}

	if tenantChanged {
		if err := ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, tenant); err != nil {
			logging.Warnf(ctx, "failed to write tenant annotations: %v", err)
		}
	}
}

// cleanUpAnnotation removes per-pod eviction-dedup annotations from the tenant
// for pods that no longer exist in the live stamped pod set. Without this,
// eviction annotations accumulate on the tenant over time as pods churn.
// Returns true if at least one annotation was removed, so the caller knows
// to persist the tenant via ctl.Update.
func cleanUpAnnotation(tenant *crdbv1beta1.CrdbTenant, pods []corev1.Pod) bool {
	liveKeys := make(map[string]bool, len(pods))
	for _, pod := range pods {
		liveKeys[evictionKeyFor(pod.Name)] = true
	}

	removed := false
	for key := range tenant.Annotations {
		if !strings.HasPrefix(key, evictionAnnotationPrefix) {
			continue
		}
		if !liveKeys[key] {
			delete(tenant.Annotations, key)
			removed = true
		}
	}
	return removed
}

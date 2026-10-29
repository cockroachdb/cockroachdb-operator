// Package restartclass contains the CRDB pod restart classification logic
// shared by nodectrl (host KV pods) and tenant (SQL pods). Both layers run the
// same cockroach binary, so exit codes and kubelet-set termination reasons are
// identical, and the classification lives in one place to avoid drift.
package restartclass

import (
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	corev1 "k8s.io/api/core/v1"
)

// RestartReason classifies why a CockroachDB pod restarted.
type RestartReason string

const (
	RestartReasonOOMKilled                      RestartReason = "OOMKilled"
	RestartReasonNoLastTerminationState         RestartReason = "NoLastTerminationState"
	RestartReasonUnspecifiedError               RestartReason = "UnspecifiedError"
	RestartReasonGracefulShutdown               RestartReason = "GracefulShutdown"
	RestartReasonUnspecifiedGoPanic             RestartReason = "UnspecifiedGoPanic"
	RestartReasonInterrupted                    RestartReason = "Interrupted"
	RestartReasonCommandLineFlagError           RestartReason = "CommandLineFlagError"
	RestartReasonLoggingFileUnavailable         RestartReason = "LoggingFileUnavailable"
	RestartReasonLoggingStderrUnavailable       RestartReason = "LoggingStderrUnavailable"
	RestartReasonFatalError                     RestartReason = "FatalError"
	RestartReasonTimeoutAfterFatalError         RestartReason = "TimeoutAfterFatalError"
	RestartReasonLoggingNetCollectorUnavailable RestartReason = "LoggingNetCollectorUnavailable"
	RestartReasonDiskFull                       RestartReason = "DiskFull"
	RestartReasonUnexpectedSIGTERM              RestartReason = "UnexpectedSIGTERM"
	RestartReasonUnexpectedSIGKILL              RestartReason = "UnexpectedSIGKILL"
	RestartReasonOperatorKilled                 RestartReason = "OperatorKilled"
	RestartReasonDoctorValidationFailed         RestartReason = "DoctorValidationFailed"
	RestartReasonCockroachContainerNotFound     RestartReason = "CockroachContainerNotFound"
	RestartReasonNodePressureEviction           RestartReason = "NodePressureEviction"
	RestartReasonRollingUpdate                  RestartReason = "RollingUpdate"
)

const (
	// PodReasonEvicted is the value kubelet writes to pod.Status.Reason when it
	// evicts a pod due to node resource pressure. Kubernetes does not export it as
	// a typed constant, so we self-declare it here to avoid a magic string.
	PodReasonEvicted = "Evicted"

	// terminatedReasonOOMKilled and terminatedReasonCompleted are the k8s container termination reason
	// strings set by the kubelet. k8s does not expose these as typed constants so we define them here.
	terminatedReasonOOMKilled = "OOMKilled"
	terminatedReasonCompleted = "Completed"
)

// exitCodeToRestartReason maps known CRDB process exit codes to their restart reason.
// Exit codes not in this map fall back to RestartReasonUnspecifiedError.
// Source: cockroachdb/cockroach pkg/cli/exit/codes.go
var exitCodeToRestartReason = map[int32]RestartReason{
	1:   RestartReasonUnspecifiedError,               // process terminated with an error condition
	2:   RestartReasonUnspecifiedGoPanic,             // unhandled signal, usually a Go panic
	3:   RestartReasonInterrupted,                    // Ctrl+C / SIGINT
	4:   RestartReasonCommandLineFlagError,           // error in command-line parameters
	5:   RestartReasonLoggingStderrUnavailable,       // logging error on stderr stream
	6:   RestartReasonLoggingFileUnavailable,         // logging error on file
	7:   RestartReasonFatalError,                     // logical error caused emergency shutdown
	8:   RestartReasonTimeoutAfterFatalError,         // emergency shutdown blocked by logging system
	9:   RestartReasonLoggingNetCollectorUnavailable, // logging error on network collector
	10:  RestartReasonDiskFull,                       // store disk full
	125: RestartReasonDoctorValidationFailed,         // doctor command detected SQL metaschema inconsistency
	137: RestartReasonUnexpectedSIGKILL,              // SIGKILL not from OOM killer (OOMKilled handled above via reason check)
	138: RestartReasonOperatorKilled,                 // SIGUSR1 from operator
	143: RestartReasonUnexpectedSIGTERM,              // SIGTERM from unknown source
}

// ClassifyRestart inspects the cockroachdb container in a pod and returns the
// restart reason along with the matching container status. Callers should
// handle NoLastTerminationState and CockroachContainerNotFound as non-events
// (nothing to log). Both layers use this function because tenant SQL pods and
// host KV pods run the same cockroach binary.
func ClassifyRestart(pod *corev1.Pod) (RestartReason, *corev1.ContainerStatus) {
	for _, cs := range pod.Status.ContainerStatuses {
		// Only the cockroachdb container is classified; sidecars are ignored.
		if cs.Name == manifests.CRDBContainerName {
			terminated := cs.LastTerminationState.Terminated

			// If terminated is nil, we cannot determine the restart reason.
			if terminated == nil {
				return RestartReasonNoLastTerminationState, nil
			}

			if terminated.Reason == terminatedReasonOOMKilled {
				return RestartReasonOOMKilled, &cs
			}

			// If terminated.Reason was Completed, it is a planned shutdown with exit code 0.
			if terminated.Reason == terminatedReasonCompleted {
				return RestartReasonGracefulShutdown, &cs
			}

			// Look up the exit code in the map. Unknown exit codes fall back to UnspecifiedError.
			if reason, ok := exitCodeToRestartReason[terminated.ExitCode]; ok {
				return reason, &cs
			}
			return RestartReasonUnspecifiedError, &cs
		}
	}

	// No container named "cockroachdb" was found in the pod.
	return RestartReasonCockroachContainerNotFound, nil
}

package kube

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
)

// EventEmitter encapsulates an EventRecorder making it a little more ergonomic
// to use for common cases.
type EventEmitter struct {
	// EventRecorder is the underlying EventRecorder.
	record.EventRecorder
}

// Infof emits a normal event with the supplied reason/message, etc.
func (e *EventEmitter) Infof(obj Object, reason, message string, args ...any) {
	if e.EventRecorder != nil {
		e.Eventf(obj, corev1.EventTypeNormal, reason, message, args...)
	}
}

// Warnf emits a warning event with the supplied reason/message, etc.
func (e *EventEmitter) Warnf(obj Object, reason, message string, args ...any) {
	if e.EventRecorder != nil {
		e.Eventf(obj, corev1.EventTypeWarning, reason, message, args...)
	}
}

package otelcompat

import (
	"fmt"
	"strconv"

	"go.opentelemetry.io/otel/trace"
)

// ToDatadogId converts an OpenTelemetry Trace or Span ID into a Datadog
// Trace or Span ID for the purpose of correlating traces and log lines.
//
// See https://docs.datadoghq.com/tracing/connect_logs_and_traces/opentelemetry/
func ToDatadogId(id string) string {
	if len(id) < 16 {
		return ""
	}
	if len(id) > 16 {
		id = id[16:]
	}
	intValue, err := strconv.ParseUint(id, 16, 64)
	if err != nil {
		return ""
	}
	return strconv.FormatUint(intValue, 10)
}

var nilTraceID = trace.TraceID{}
var nilSpanId = trace.SpanID{}

// TraceIdFromDatadogId converts a decimal-encoded Datadog trace ID to an
// OpenTelemtry trace.TraceID.
func TraceIdFromDatadogId(id string) (trace.TraceID, error) {
	intValue, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return nilTraceID, err
	}
	// OpenTelemetry Trace IDs must be 16 hex characters long
	hexValue := fmt.Sprintf("%032x", intValue)
	return trace.TraceIDFromHex(hexValue)
}

// SpanIdFromDatadogId converts a decimal-encoded Datadog span ID to an
// OpenTelemtry trace.SpanID.
func SpanIdFromDatadogId(id string) (trace.SpanID, error) {
	intValue, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return nilSpanId, err
	}
	// OpenTelemetry Span IDs must be 16 hex characters long
	hexValue := fmt.Sprintf("%016x", intValue)
	return trace.SpanIDFromHex(hexValue)
}

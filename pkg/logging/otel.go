package logging

import (
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/datadog/otelcompat"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// spanFieldName is the .Key value of zapcore.Fields that contain otel spans.
const spanFieldName = "OTEL_SPAN"

// spanField returns a zap.Field that allows a zapcore.Core to extract the
// given otel span and attach logs to it via AddEvent.
// The returned zap.Field will otherwise be skipped at encoding time.
func spanField(span trace.Span) zap.Field {
	return zapcore.Field{
		Key:       spanFieldName,
		Type:      zapcore.SkipType,
		Interface: span,
	}
}

func traceID(span trace.SpanContext) zap.Field {
	return zap.String("dd.trace_id", otelcompat.ToDatadogId(span.TraceID().String()))
}

func spanID(span trace.SpanContext) zap.Field {
	return zap.String("dd.span_id", otelcompat.ToDatadogId(span.SpanID().String()))
}

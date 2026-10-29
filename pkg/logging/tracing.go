package logging

import (
	"context"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type loggerOverride struct{}

// WithLogger embeds a zaptest.Logger into the returned context. All
// logging functions that accept a context will prefer this embedded logger
// over the global logger.
func WithLogger(ctx context.Context, logger *zap.Logger) context.Context {
	return context.WithValue(ctx, loggerOverride{}, logger)
}

// L returns the *zap.Logger attached to the context. If the given context has a
// span attached, the tracing ids are attached to the logger. If the context has
// no logger attached, the global Logger is returned.
func L(ctx context.Context) *zap.Logger {
	logger, ok := ctx.Value(loggerOverride{}).(*zap.Logger)
	if !ok {
		logger = zap.L()
	}

	// If these fields are in the request bag attach them to every log
	fields := make([]zapcore.Field, 0)

	span := trace.SpanFromContext(ctx)
	spanInfo := span.SpanContext()
	if spanInfo.IsValid() {
		// Datadog documentation states that trace and span IDs must be
		// formatted as strings
		// https://docs.datadoghq.com/tracing/connect_logs_and_traces/go/
		fields = append(fields, traceID(spanInfo), spanID(spanInfo), spanField(span))
	}

	if len(fields) > 0 {
		return logger.With(fields...)
	}

	return logger
}

// S returns a *zap.SugaredLogger with tracing ids attached, if the given
// context has a span attached.
// If no context is available, use context.Background()
func S(ctx context.Context, opts ...zap.Option) *zap.SugaredLogger {
	return L(ctx).WithOptions(opts...).Sugar()
}

func depthL(ctx context.Context, skip int) *zap.Logger {
	return L(ctx).WithOptions(zap.AddCallerSkip(skip))
}

func Debug(ctx context.Context, msg string, fields ...zap.Field) {
	depthL(ctx, 1).Debug(msg, fields...)
}

func Debugf(ctx context.Context, template string, args ...any) {
	depthL(ctx, 1).Sugar().Debugf(template, args...)
}

func Info(ctx context.Context, msg string, fields ...zap.Field) {
	depthL(ctx, 1).Info(msg, fields...)
}

func Infof(ctx context.Context, template string, args ...any) {
	depthL(ctx, 1).Sugar().Infof(template, args...)
}

func Warn(ctx context.Context, msg string, fields ...zap.Field) {
	depthL(ctx, 1).Warn(msg, fields...)
}

func Warnf(ctx context.Context, template string, args ...any) {
	depthL(ctx, 1).Sugar().Warnf(template, args...)
}

func Error(ctx context.Context, msg string, fields ...zap.Field) {
	depthL(ctx, 1).Error(msg, fields...)
}

func Errorf(ctx context.Context, template string, args ...any) {
	depthL(ctx, 1).Sugar().Errorf(template, args...)
}

func Fatal(ctx context.Context, msg string, fields ...zap.Field) {
	depthL(ctx, 1).Fatal(msg, fields...)
}

func Fatalf(ctx context.Context, template string, args ...any) {
	depthL(ctx, 1).Sugar().Fatalf(template, args...)
}

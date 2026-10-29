package logging

import (
	"fmt"
	"os"

	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/term"
	ctrl "sigs.k8s.io/controller-runtime"
)

// SetGlobalLogger establishes the given logger as the global zap logger. Other
// global loggers, such as the K8s controller-runtime logger and std "log"
// package are redirected to this logger. The new global logger will be
// accessible via these zap methods:
//
//	zap.L() // Structured logger
//	zap.S() // Sugared logger
//
// These methods are convenient to use during development to dump out debug
// information. But avoid using them in production code; prefer to use local
// instance loggers tagged with distributed tracing tags so that request
// processing can be traced across distributed components.
func SetGlobalLogger(logger *zap.Logger) {
	// Redirect the std "log" package to zap.
	if _, err := zap.RedirectStdLogAt(logger, zapcore.InfoLevel); err != nil {
		panic(fmt.Sprintf("could not redirect std-log: %v", err))
	}

	// Redirect the K8s global controller-runtime logger to zap. Adjust the
	// caller depth to reflect the extra zapr call.
	k8sLogger := logger.WithOptions(zap.AddCallerSkip(1))
	ctrl.SetLogger(zapr.NewLogger(k8sLogger))

	zap.ReplaceGlobals(logger)
}

// NewLogger constructs a new zap logger with a format that depends on the
// current environment.
func NewLogger() *zap.Logger {
	// If we're attached to an interactive terminal, we're likely
	// being run on someone's laptop. Send back a dev logger.
	var logger *zap.Logger
	var err error
	if term.IsTerminal(int(os.Stdout.Fd())) {
		logger, err = newDevelopmentLogger()
	} else {
		logger, err = newProductionLogger()
	}
	if err != nil {
		panic(fmt.Sprintf("could not create logger: %v", err))
	}
	return logger
}

func newProductionLogger() (*zap.Logger, error) {
	encoder := zap.NewProductionEncoderConfig()
	encoder.EncodeTime = zapcore.ISO8601TimeEncoder
	zapConfig := zap.NewProductionConfig()
	zapConfig.DisableStacktrace = true
	zapConfig.EncoderConfig = encoder
	return zapConfig.Build()
}

func newDevelopmentLogger() (*zap.Logger, error) {
	zapConfig := zap.NewDevelopmentConfig()
	zapConfig.DisableStacktrace = true
	return zapConfig.Build()
}

package logging

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

func TestConvenienceLoggingCaller(t *testing.T) {
	buf := &zaptest.Buffer{}
	logger, err := newDevelopmentLogger()
	require.NoError(t, err)
	defer zap.ReplaceGlobals(logger)()

	ctx := context.Background()
	Debug(ctx, "Debug")
	Debugf(ctx, "Debugf")
	Info(ctx, "Info")
	Infof(ctx, "Infof")
	Warn(ctx, "Warn")
	Warnf(ctx, "Warnf")
	Error(ctx, "Error")
	Errorf(ctx, "Errorf")

	for _, line := range buf.Lines() {
		parts := strings.Fields(line)
		require.Len(t, parts, 5)
		require.Regexp(t, "^logging/logging_test.go:", parts[3], parts)
	}
}

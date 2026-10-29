package pgxsql

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/jackc/pgx/v4"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
)

func TestToPgxConfig(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		pgxCfg, err := toPGXConfig(&Options{
			Host:         "127.0.0.1",
			User:         url.UserPassword("foo", "bar"),
			Application:  "myapp",
			OtherOptions: []string{"--cluster=dim-dog-2", "-c", "work_mem=256MB"},
			OpenTimeout:  10 * time.Second,
		})
		require.NoError(t, err)
		require.Equal(t, "127.0.0.1", pgxCfg.Host)
		require.Equal(t, uint16(26257), pgxCfg.Port)
		require.Equal(t, "system", pgxCfg.Database)
		require.Equal(t, "foo", pgxCfg.User)
		require.Equal(t, "bar", pgxCfg.Password)
		require.Nil(t, pgxCfg.TLSConfig)
		require.NotNil(t, pgxCfg.DialFunc)
		require.NotNil(t, pgxCfg.LookupFunc)
		require.Equal(t, map[string]string{
			"application_name": "myapp",
			"options":          "--cluster=dim-dog-2 -c work_mem=256MB",
		}, pgxCfg.RuntimeParams)
		require.Equal(t, 10*time.Second, pgxCfg.ConnectTimeout)

		// Verify it's the identity lookup.
		addrs, err := pgxCfg.LookupFunc(nil, "foo")
		require.NoError(t, err)
		require.Equal(t, []string{"foo"}, addrs)
	})

	t.Run("UA cluster", func(t *testing.T) {
		pgxCfg, err := toPGXConfig(&Options{
			Database:     "cluster:dim-dog-2",
			OtherOptions: []string{"--cluster=dim-dog-2", "-c", "work_mem=256MB"},
			OpenTimeout:  10499 * time.Millisecond,
		})
		require.NoError(t, err)
		require.Equal(t, "cluster:dim-dog-2/system", pgxCfg.Database)
		require.Equal(t, map[string]string{
			"options": "--cluster=dim-dog-2 -c work_mem=256MB",
		}, pgxCfg.RuntimeParams)
		require.Equal(t, 10*time.Second, pgxCfg.ConnectTimeout)

		pgxCfg, err = toPGXConfig(&Options{
			Database:     "cluster:dim-dog-2/foodb",
			OtherOptions: []string{"--cluster=dim-dog-2", "-c", "work_mem=256MB"},
			OpenTimeout:  9501 * time.Millisecond,
		})
		require.NoError(t, err)
		require.Equal(t, "cluster:dim-dog-2/foodb", pgxCfg.Database)
		require.Equal(t, map[string]string{
			"options": "--cluster=dim-dog-2 -c work_mem=256MB",
		}, pgxCfg.RuntimeParams)
		require.Equal(t, 10*time.Second, pgxCfg.ConnectTimeout)

		pgxCfg, err = toPGXConfig(&Options{
			Database: "cluster:dim-dog-2/foodb",
		})
		require.NoError(t, err)
		require.Equal(t, 0*time.Second, pgxCfg.ConnectTimeout)
	})
}

func TestPgxLogger(t *testing.T) {
	buf := &zaptest.Buffer{}
	logger := zap.New(zapcore.NewCore(
		zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig()),
		buf,
		zap.NewAtomicLevelAt(zapcore.DebugLevel),
	))
	ctx := logging.WithLogger(context.Background(), logger)

	pgxLogger := pgxLogger{}

	buf.Reset()

	pgxLogger.Log(ctx, pgx.LogLevelError, "test message", map[string]any{
		"err":   "err value",
		"sql":   "sql value",
		"args":  "args value",
		"other": "other value",
	})
	line := buf.Lines()[0]
	require.NotContains(t, line, "sql value")
	require.NotContains(t, line, "args value")
	require.NotContains(t, line, "other value")
	require.Contains(t, line, "err value")
	require.Contains(t, line, "test message")
}

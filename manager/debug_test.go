package manager_test

import (
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/manager"
	"github.com/stretchr/testify/require"
)

func TestMetricsServer(t *testing.T) {
	// Get system-chosen port for testing, in order to ensure this doesn't
	// conflict with ports already in use.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx := t.Context()

	// Use that listener to start the debug server.
	go func() {
		metrics := &manager.DebugServer{Listener: l}
		require.NoError(t, metrics.Start(ctx))
	}()

	// Hit the debug/pprof endpoint.
	resp, err := http.Get(fmt.Sprintf("http://%s/%s", l.Addr().String(), "debug/pprof"))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.True(t, resp.StatusCode == 200)
}

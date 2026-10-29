package manager

import (
	"context"
	"net"
	"net/http"
	"net/http/pprof"

	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
)

// DebugServer supports the standard pprof debug endpoints.
type DebugServer struct {
	// Listener is listening on a network port. The debug server will accept
	// incoming connections from the listener.
	Listener net.Listener
}

// Start is called by the manager when that starts up. It will start listening
// and serving on the debug service port.
// Start implements the K8s manager.Runnable interface.
func (d *DebugServer) Start(ctx context.Context) error {

	// Set up handlers for pprof endpoints.
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	server := &http.Server{Addr: d.Listener.Addr().String(), Handler: mux}

	// Shutdown the server when ctx is canceled.
	go func() {
		<-ctx.Done()

		if err := server.Shutdown(ctx); err != nil {
			// Error from closing listeners, or context timeout.
			logging.Errorf(ctx, "error shutting down the HTTP debug server: %v", err)
		}
	}()

	if err := server.Serve(d.Listener); err != nil && err != http.ErrServerClosed {
		return err
	}

	return nil
}

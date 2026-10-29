package opserver

import (
	"context"
	"net"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/operatorpb"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/mtls"
	"github.com/cockroachlabs/cockroach-enterprise-operator/tenant"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"google.golang.org/grpc"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// Options contains the RPC servers configuration.
type Options struct {
	CertDirectory      string
	CertKeyname        string
	Ctl                *kube.Ctl
	Listener           net.Listener
	ScaleLockTable     *tenant.LockTable
	MetricManager      metrics.TenantMetricsReader
	NodeMetricManager  metrics.NodeMetricsReader
	EventEmitter       kube.EventEmitter
	TenantPodSyncer    tenant.TenantPodSyncer
	NodeLocalityReader cloud.NodeLocalityReaderFunc
}

func New(options Options) manager.Runnable {
	return &server{
		Options: options,
	}
}

// Run starts the operatorpb.OperatorServer and waits for termination.
func (s *server) Start(ctx context.Context) error {
	credentials, err := mtls.GrpcServerCredentials(s.CertDirectory, s.CertKeyname, []string{
		"crdb://service/tenantdir",
	})
	if err != nil {
		return err
	}

	grpcServer := grpc.NewServer(
		credentials,
		grpc.UnaryInterceptor(grpc_prometheus.UnaryServerInterceptor),
		grpc.StreamInterceptor(grpc_prometheus.StreamServerInterceptor),
	)
	operatorpb.RegisterOperatorServer(grpcServer, s)

	grpc_prometheus.Register(grpcServer)

	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	if err := grpcServer.Serve(s.Listener); err != nil {
		return errors.Wrap(err, "error serving operator grpc server")
	}

	return nil
}

// NeedLeaderElection implements manager.LeaderElectionRunnable. Implementing
// the interface guarantees the grpc server is only started once the operator is
// leader elected.
//
// The kubernetes service identifying the operator includes the leader and
// non-leader operator pods. If a connection request is sent to a non-leader
// operator, it is rejected. Operator clients should be configured to
// proactively maintain connections and with short retries for connection
// failures. That allows them to maintain connections to the leader without
// introducing latency on the request path.
func (s *server) NeedLeaderElection() bool {
	return true
}

var _ manager.LeaderElectionRunnable = &server{}

// server implements operatorpb.OperatorServer.
type server struct {
	Options
}

var _ operatorpb.OperatorServer = &server{}

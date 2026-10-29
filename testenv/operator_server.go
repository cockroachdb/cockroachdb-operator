package testenv

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/crdbcerts"
	"github.com/cockroachlabs/cockroach-enterprise-operator/operatorpb"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/mtls"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	corev1 "k8s.io/api/core/v1"
)

// operatorRPCPort is the port used by the cockroach-operator's RPC server in
// the test environment.
const operatorRPCPort = 9070

// DialOperatorServer uses kubernetes port forwarding to connect to
// operatorpb.OperatorServer contained within the cockroach-operator.
func DialOperatorServer(t *testing.T, ctl *kube.Ctl) operatorpb.OperatorClient {
	ctx := context.Background()
	defaultCtl := ctl.WithNamespace("default")

	// Get the tenantdir's operator client certificate
	ca, cert, err := crdbcerts.GetTenantDirCertBundle(ctl)
	require.NoError(t, err)
	x509Cert, err := tls.X509KeyPair(cert.Certificate, cert.PrivateKey)
	require.NoError(t, err)
	caPool := x509.NewCertPool()
	require.True(t, caPool.AppendCertsFromPEM(ca))

	// Create client TLS credentials and override the operator's DNS name. This
	// is necessary because we are dialing the pod instead of the service.
	tlsConfig := mtls.ClientTLSConfig(x509Cert, caPool)
	tlsConfig.ServerName = "cockroach-operator.default.svc"
	tlsCredentials := grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig))

	// Retrieve the operator's pod
	var pods corev1.PodList
	require.NoError(t, defaultCtl.List(ctx, &kube.ListOptions{
		LabelSelector: kube.ParseLabelOrDie("app=cockroach-operator"),
	}, &pods))
	require.Len(t, pods.Items, 1, "expected exactly one operator pod")

	// Connect to the pod
	dialer, err := kube.NewPodDialer(defaultCtl.RestConfig(), defaultCtl.Namespace())
	require.NoError(t, err)
	grpcDailer := grpc.WithContextDialer(func(ctx context.Context, pod string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", pod)
	})
	conn, err := grpc.Dial(
		fmt.Sprintf("%s:%d", pods.Items[0].GetName(), operatorRPCPort),
		grpcDailer, tlsCredentials)
	require.NoError(t, err, "unable to dial operator")
	t.Cleanup(func() { conn.Close() })

	return operatorpb.NewOperatorClient(conn)
}

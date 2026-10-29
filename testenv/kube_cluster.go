package testenv

import (
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/infrastructure"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// KubeCluster abstracts a K8s cluster so that AWS, GCP, and K3D clusters could
// be treated polymorphically. Tests could run against any of these (though
// today only K3D clusters are created by Env).
type KubeCluster struct {
	// Ctl can be used to install objects into the K8s cluster.
	Ctl *kube.Ctl

	// Config is the information needed to connect to and control a K8s cluster.
	Config *rest.Config

	// InternalAPIAddress is the address for accessing the k8s API server
	// from within the k8s cluster, whereas Config and Ctl is for accessing
	// from outside the cluster.
	InternalAPIAddress string

	// Hardware represents a set of VM groups managed by a provider such as GCP,
	// AWS, or K3D. VMs are grouped by availability zone.
	Hardware infrastructure.VMPoolGroup
}

// NewCtl is a helper method that constructs a Ctl that targets the given
// namespace in the K8s cluster.
func (c KubeCluster) NewCtl(namespace string) *kube.Ctl {
	return c.Ctl.WithNamespace(namespace)
}

// NewClientset is a helper method that constructs a Clientset for the K8s
// cluster.
func (c KubeCluster) NewClientset(t *testing.T) *kubernetes.Clientset {
	clientset, err := kubernetes.NewForConfig(c.Config)
	require.NoError(t, err)
	return clientset
}

// NewPodDialer is a helper method that constructs a PodDialer for the K8s
// cluster. The pod dialer uses Kubernetes' portforwarding protocol to create a
// net.Conn to a pod in the cluster.
func (c KubeCluster) NewPodDialer(t *testing.T, namespace string) *kube.PodDialer {
	// Remove any kuberecord wrapper from the K8s config. Pod dialer traffic
	// can't be recorded because it uses a custom protocol.
	clone := *c.Config
	clone.WrapTransport = nil
	dialer, err := kube.NewPodDialer(&clone, namespace)
	require.NoError(t, err)
	return dialer
}

package manifests

import (
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// JoinService creates a headless service that's suitable for use as the
// `--join` argument of `cockroach start`. It is, more or less, an exact
// replica of NodeService but PublishNotReadyAddresses is FALSE. It is
// configured this way to ensure that any newly added CockroachDB nodes will
// always connect to a node that is guarenteed to be part of an initialized
// CockroachDB cluster. When PublishNotReadyAddresses is set to true,
// partitions can be formed in the cluster due to the operator using service
// discovery and this CockroachDB bug [1]. Unfortunately, the
// PublishNotReadyAddresses also affects the creation of Pod DNS. This means we
// must maintain two Services to provide both Pod DNS and a service that only
// accesses Ready Pods.
//
// The operator opted to use service discovery to avoid having to restart nodes
// when new nodes are added and to avoid the complexity of removing the --join
// argument from our reconilation logic.
//
// Notable divergences from NodeService are:
// * PublishNotReadyAddresses is FALSE.
// * BetaCrdbClusterNodeOwnerLabelKey is the only label selector.
// * All Ports use named targets.
//
// [1] https://github.com/cockroachdb/cockroach/issues/103594
func JoinService(cluster *crdbv1beta1.CrdbCluster) *corev1.Service {
	nodeSpec := cluster.Spec.Template.Spec
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name + "-join",
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				BetaCrdbClusterNodeOwnerLabelKey: cluster.Name,
			},
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "None",
			Selector: map[string]string{
				BetaCrdbClusterNodeOwnerLabelKey: cluster.Name,
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "sql",
					Port:       nodeSpec.GetSQLPort(),
					TargetPort: intstr.FromString("sql"),
				},
				{
					Name:       "grpc",
					Port:       nodeSpec.GetGRPCPort(),
					TargetPort: intstr.FromString("grpc"),
				},
				{
					Name:       "http",
					Port:       nodeSpec.GetHTTPPort(),
					TargetPort: intstr.FromString("http"),
				},
			},
		},
	}
}

// NodeService creates a headless service used to generate SRV records for all
// nodes. This service address is used for the `--join` flag at startup leveraging
// the `--experimental-dns-srv` option.
func NodeService(cluster *crdbv1beta1.CrdbCluster) *corev1.Service {
	// For backwards compatilbity, we use the same discovery service as
	// StatefulSet clusters with an updated ObjectMeta and Selector.

	// This ensures that when the operator takes over the discovery service,
	// the service will still work as expected if the cluster is rolled back to
	// a StatefulSet configuration.
	// When StatefulSet clusters are fully deprecated, we can remove the
	// additional app=cockroachdb label and clean up the selectors.
	svc := NodeToNodeService(&cluster.Spec.Template.Spec)

	// NB: cluster.Name will always be cockroachdb (for the foreseeable future),
	// this service intentially conflicts with NodeToNodeService().
	svc.ObjectMeta = metav1.ObjectMeta{
		Name:      cluster.Name,
		Namespace: cluster.Namespace,
		Labels: map[string]string{
			"app":                            "cockroachdb",
			BetaCrdbClusterNodeOwnerLabelKey: cluster.Name,
		},
	}

	svc.Spec.Selector[BetaCrdbClusterNodeOwnerLabelKey] = cluster.Name

	return svc
}

// InternalCRDBClusterIPService is for clients running in the same k8s cluster,
// such as a prober for measuring the health of the service.
func InternalCRDBClusterIPService(spec crdbv1beta1.CrdbClusterSpec) *corev1.Service {
	nodeSpec := spec.Template.Spec
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: InternalLoadBalancerService,
			Labels: map[string]string{
				"app": "cockroachdb",
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name:       "sql",
					Port:       nodeSpec.GetSQLPort(),
					TargetPort: intstr.FromInt(int(nodeSpec.GetSQLPort())),
				},
				{
					Name: "grpc",
					Port: nodeSpec.GetGRPCPort(),
					// For backwards compatilbity, Target gRPC ports by their
					// name. StatefulSet CRDB clusters expose a single port
					// that serves both SQL and gRPC on CockroachSQLPort
					// (27257). This service will route gRPC traffic to that
					// port by name. Operator CRDB clusters have their SQL and
					// gRPC ports separated onto CockroachSQLPort and
					// CockroachGRPCPort, respectively. This service will route
					// the SQL traffic to the SQL Port and gRPC traffic to the
					// named grpc port.
					// Only Serverless clusters are expected to use the
					// CockroachGRPCPort of this service prior to the operator
					// migration.
					TargetPort: intstr.FromString("grpc"),
				},
				{
					Name:       "http",
					Port:       nodeSpec.GetHTTPPort(),
					TargetPort: intstr.FromInt(int(nodeSpec.GetHTTPPort())),
				},
			},
			Selector: map[string]string{
				"app": "cockroachdb",
			},
		},
	}
}

// NodeToNodeService exists to create DNS entries for each pod in the stateful
// set such that they can resolve each other's IP addresses. It does not
// create a load-balanced ClusterIP and should not be used directly by clients.
func NodeToNodeService(node *crdbv1beta1.CrdbNodeSpec) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			// If you change the name of the service, BEWARE, YOU WILL BREAK POD
			// TO POD DNS! If you love implicit naming dependencies, READ ON!
			//
			// https://kubernetes.io/docs/concepts/services-networking/dns-pod-service/#pod-s-hostname-and-subdomain-fields
			Name: CockroachDB,
			Labels: map[string]string{
				"app": "cockroachdb",
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				// NB: The SQL port is left here for backwards compatilbity.
				// Service discovery only needs gRPC but StatefulSet clusters
				// run SQL and gRPC on the same port.
				{
					Name:       "sql",
					Port:       node.GetSQLPort(),
					TargetPort: intstr.FromInt(int(node.GetSQLPort())),
				},
				{
					Name: "grpc",
					Port: node.GetGRPCPort(),
					// For backwards compatilbity, Target gRPC ports by their
					// name. StatefulSet CRDB clusters expose a single port
					// that serves both SQL and gRPC on CockroachSQLPort
					// (27257). This service will route gRPC traffic to that
					// port by name. Operator CRDB clusters have their SQL and
					// gRPC ports separated onto CockroachSQLPort and
					// CockroachGRPCPort, respectively. This service will route
					// the SQL traffic to the SQL Port and gRPC traffic to the
					// named grpc port.
					// Only Serverless clusters are expected to use the
					// CockroachGRPCPort of this service prior to the operator
					// migration.
					TargetPort: intstr.FromString("grpc"),
				},
				// TODO(orchestration): Remove this port post operator
				// migration. Nothing should be using it but why risk cleaning
				// it up ahead of the migration.
				{
					Name:       "http",
					Port:       node.GetHTTPPort(),
					TargetPort: intstr.FromInt(int(node.GetHTTPPort())),
				},
			},
			// We want all pods in the StatefulSet to have their addresses published for
			// the sake of the other CockroachDB pods even before they're ready, since they
			// have to be able to talk to each other in order to become ready.
			PublishNotReadyAddresses: true,
			ClusterIP:                "None",
			Selector: map[string]string{
				"svc": "cockroachdb",
			},
		},
	}
}

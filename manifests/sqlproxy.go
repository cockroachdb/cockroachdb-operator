package manifests

import (
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/buildinfo"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/constants"
	"github.com/cockroachlabs/cockroach-enterprise-operator/securitycontext"
	"google.golang.org/protobuf/proto"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	SqlproxyAppName       = "sqlproxy"
	sqlproxyContainerName = "sqlproxy"
	sqlproxyProcessName   = "cockroach"
	sqlproxyConfigPath    = "/cockroach/config"
	SQLProxyConfigMapName = "sqlproxy-config"
	// The allowlist has it's own configmap due to the 1MB size limit for configmaps.
	SQLProxyAllowlistConfigMapName  = "sqlproxy-allowlist"
	SQLProxyDenylistFilename        = "denylist.yml"
	SQLProxyAllowlistFilename       = "allowlist.json"
	defaultSqlProxyDenylistContent  = "SequenceNumber: 0"
	defaultSqlProxyAllowlistContent = `{"allowlist":{}}`

	// The memory request for the sql proxy pod was determined by recording
	// memory usage during single and multiple tenant workloads.
	memoryRequestMiB = 50

	tenantDirName          = "tenantdir"
	tenantDirContainerName = "tenantdir"

	// TenantDirPort is the port that the tenantdir gRPC service is listening
	// on. The tenantdir container shares the same network namespace as the
	// sqlproxy container, so we should ensure that the ports do not collide.
	TenantDirPort = 8081

	// ProxyHTTPPort is the port the SQL proxy is listening on for it's HTTP server.
	// This is mostly for metrics and healthchecks, but when the pods restart
	// the tenantdir container uses this port to check if the proxy is still online
	// before it shuts down.
	ProxyHTTPPort = 8080

	// sqlProxyTerminationGracePeriod defines how long the SQL Proxy containers should be kept alive.
	// Containers in this grace period will not accept further TCP connections, so this grace period
	// is for existing connections to stay alive, and hopefully be closed by the client before the
	// grace period is over. It is currently set to an hour so clients should be able to recycle
	// connections for a recommended 30 minutes and avoid any unexpected inturruptions.
	sqlProxyTerminationGracePeriod = 3600
)

var sqlProxyMaxSurge = intstr.FromString("50%")

func tenantDirImage() string {
	return buildinfo.ContainerImage("us-docker.pkg.dev/cockroach-cloud-images/data-plane/tenantdir", "master")
}

// SQLProxyOptions contains fields that modify the manifest for the
// deployment of sql proxy pods.
type SQLProxyOptions struct {
	// Namespace is the Kubernetes namespace that the sqlproxy pods are deployed
	// in, e.g. "europe-west1". This is also the namespace in which the service
	// account for the RBAC will be placed in.
	Namespace string

	// Name is the name of the proxy deployment.
	Name string

	// Replicas is the number of replicas to run.
	Replicas uint32

	// Image is the Docker image path for the sqlproxy container,
	// e.g. "cockroachdb/cockroach:v20.1.1".
	Image string

	// TenantDirImage is the Docker image path for the tenantdir container,
	// e.g. "cockroachdb/cockroach:v20.1.1".
	TenantDirImage string

	// K8sAPIServerIPs corresponds to the list of IPs for the Kubernetes API
	// server. It is assumed that the IPs in this set are valid.
	K8sAPIServerIPs []string

	// InTestMode indicates that sqlproxy is being tested with aggressive
	// polling, and without rate limiting.
	InTestMode bool

	// DisableConnectionRebalancing disables the connection rebalancing feature.
	// This will only apply to v22.1+ images.
	DisableConnectionRebalancing bool

	// RequireProxyProtocol enforces the proxy protocol on the SQL listener.
	// This should only be applied to v23.1+ clusters.
	RequireProxyProtocol bool

	// ProxyProtocolListenAddrEnabled controls whether the proxy protocol
	// listen addr flag is passed into SQLProxy.
	ProxyProtocolListenAddrEnabled bool

	// NetworkPlugin indicates the type of network plugin that is being used
	// within the Kubernetes cluster.
	NetworkPlugin cloud.NetworkPlugin

	// ApplyConfigMaps, if true, causes the sqlproxy and sqlproxy
	// allowlist config maps to get applied.
	ApplyConfigMaps bool
}

// SQLProxy returns the manifest of objects required to deploy sqlproxy.
// SQLProxy expects the existence of two secrets:
// - SqlProxyExternalCertSecretName
// - CockroachNodeCertSecretName
func SQLProxy(opts SQLProxyOptions) []kube.Object {
	var objects []kube.Object

	// Ensure that the network policies get applied first.
	if opts.NetworkPlugin == cloud.Cilium {
		objects = append(objects, sqlproxyCiliumNetworkPolicy()...)
	} else {
		objects = append(objects, sqlproxyNetworkPolicy(opts.K8sAPIServerIPs))
	}

	objects = append(
		objects,
		sqlProxyPriorityClass(),
		sqlproxyDeployment(opts),
	)

	if opts.ApplyConfigMaps {
		objects = append(objects,
			SQLProxyConfigMap(defaultSqlProxyDenylistContent),
			SQLProxyAllowlistConfigMap(defaultSqlProxyAllowlistContent),
		)
	}

	objects = append(objects, SqlProxyRBAC(opts.Namespace, SqlproxyAppName)...)
	return objects
}

// SQLProxyConfigMap returns ConfigMap object created from the provided file
// contents.
func SQLProxyConfigMap(contents string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: SQLProxyConfigMapName},
		Data: map[string]string{
			SQLProxyDenylistFilename: contents,
		},
	}
}

// SQLProxyAllowlistConfigMap returns ConfigMap object created from the provided file
// contents.
func SQLProxyAllowlistConfigMap(contents string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: SQLProxyAllowlistConfigMapName},
		Data: map[string]string{
			SQLProxyAllowlistFilename: contents,
		},
	}
}

func sqlProxyPriorityClass() *schedulingv1.PriorityClass {
	return &schedulingv1.PriorityClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: SqlproxyAppName,
		},
		Value: CriticalPriority,
	}
}

func sqlproxyDeployment(opts SQLProxyOptions) *appsv1.Deployment {
	if opts.Image == "" {
		panic(errors.AssertionFailedf(".Image was not set."))
	}

	// Fill in defaults for unspecified options.
	if opts.Name == "" {
		opts.Name = SqlproxyAppName
	}
	if opts.TenantDirImage == "" {
		opts.TenantDirImage = tenantDirImage()
	}
	if opts.Replicas == 0 {
		opts.Replicas = 1
	}

	// When in test mode, disable throttler and use aggressive polling.
	var testFlags string
	if opts.InTestMode {
		testFlags = " --throttle-base-delay=0 --poll-config-interval=100ms --validate-access-interval=5ms"
	}

	// This flag disables connection rebalancing within the sqlproxy. This will
	// only work on v22.1.X+.
	var disableConnectionRebalancingFlag string
	if opts.DisableConnectionRebalancing {
		disableConnectionRebalancingFlag = " --disable-connection-rebalancing"
	}

	// This flag changes the proxy behavior of the default listener to require the PROXY protocol.
	var requireProxyProtocolFlag string
	if opts.RequireProxyProtocol {
		requireProxyProtocolFlag = " --require-proxy-protocol"
	}

	var proxyProtocolListenAddrFlag string
	if opts.ProxyProtocolListenAddrEnabled {
		proxyProtocolListenAddrFlag = " --proxy-protocol-listen-addr=0.0.0.0:" + strconv.Itoa(CockroachProxyProtocolPort)
	}

	labels := map[string]string{"app": SqlproxyAppName}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:   opts.Name,
			Labels: labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(opts.Replicas)),
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.DeploymentStrategyType("RollingUpdate"),
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxSurge: &sqlProxyMaxSurge,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					// The pod's affinity tries to ensure that sqlproxy pods
					// are colocated on different nodes (i.e. if there's already
					// an existing sqlproxy pod, try a different node).
					Affinity: &corev1.Affinity{
						PodAntiAffinity: &corev1.PodAntiAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
								{
									Weight: 1,
									PodAffinityTerm: corev1.PodAffinityTerm{
										LabelSelector: &metav1.LabelSelector{
											MatchExpressions: []metav1.LabelSelectorRequirement{
												{
													Key:      "app",
													Operator: metav1.LabelSelectorOpIn,
													Values:   []string{SqlproxyAppName},
												},
											},
										},
										// All Kubernetes nodes contain this key, so this means
										// we want to consider every node in the cluster.
										TopologyKey: corev1.LabelHostname,
									},
								},
							},
						},
					},
					PriorityClassName: SqlproxyAppName,
					SecurityContext:   securitycontext.PodRunAsNonRoot(),
					Containers: []corev1.Container{
						proxyContainer(
							opts.Image,
							"exec /cockroach/cockroach mt start-proxy"+
								" --listen-addr=0.0.0.0:"+strconv.Itoa(v1beta1.DefaultCockroachSQLPort)+
								proxyProtocolListenAddrFlag+
								" --listen-cert="+CockroachCertsPath+"/external.crt"+
								" --listen-key="+CockroachCertsPath+"/external.key"+
								" --denylist-file="+filepath.Join(sqlproxyConfigPath, "denylist.yml")+
								" --directory=:"+strconv.Itoa(TenantDirPort)+
								testFlags+
								disableConnectionRebalancingFlag+
								requireProxyProtocolFlag+
								" --log=\"{ sinks : { stderr : { filter : info } } }\"",
							opts.RequireProxyProtocol,
						),
						tenantDirContainer(opts.TenantDirImage),
						configReloaderContainer(),
					},
					Volumes: []corev1.Volume{
						proxyCertVolume(),
						proxyConfigVolume("config", SQLProxyConfigMapName),
						proxyConfigVolume("allowlist", SQLProxyAllowlistConfigMapName),
						proxyTenantCACertVolume(),
						tenantDirSecretMount(),
						inflightDirVolume(),
					},
					ServiceAccountName:            SqlproxyAppName,
					ShareProcessNamespace:         new(true),
					TerminationGracePeriodSeconds: proto.Int64(sqlProxyTerminationGracePeriod),
				},
			},
		},
	}
	return deployment
}

func proxyContainer(image, cmd string, requireProxyProtocol bool) corev1.Container {
	healthCheckPort := intstr.FromInt(ProxyHTTPPort)

	container := &corev1.Container{
		Name:            sqlproxyContainerName,
		Image:           image,
		SecurityContext: securitycontext.ContainerRunAsNonRoot(),
		Command:         []string{"/bin/bash", "-ecx", cmd},
		Ports: []corev1.ContainerPort{
			{
				ContainerPort: v1beta1.DefaultCockroachSQLPort,
				Name:          "sql",
				Protocol:      corev1.ProtocolTCP,
			},
			{
				ContainerPort: CockroachProxyProtocolPort,
				// Kubernetes limits port names to 15 characters.
				Name:     "sql-pp",
				Protocol: corev1.ProtocolTCP,
			},
			{
				Name:          prometheusAppName,
				ContainerPort: ProxyHTTPPort,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		LivenessProbe: &corev1.Probe{
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
			TimeoutSeconds:      5,
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   "/_status/healthz",
					Port:   healthCheckPort,
					Scheme: corev1.URISchemeHTTP,
				},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				MountPath: CockroachCertsPath,
				Name:      "certs",
				ReadOnly:  true,
			},
			{
				MountPath: sqlproxyConfigPath,
				Name:      "config",
				ReadOnly:  true,
			},
			// Mount the tenant-root certificate into the system certificate
			// directory such that x509.SystemCertPool() will automatically
			// load it.
			{
				MountPath: "/etc/ssl/certs/tenant-ca.crt",
				SubPath:   "tenant-ca.crt",
				Name:      "tenant-ca-certificate",
				ReadOnly:  true,
			},
			// Mount the InflightDir as an EmptyDir because the cockroach process
			// cannot directly create the directory when user is 1000 (non root).
			{
				MountPath: "/cockroach/inflight_trace_dump",
				Name:      "inflightdir",
			},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: *resource.NewQuantity(memoryRequestMiB<<20, resource.BinarySI),
			},
		},
		Lifecycle: &corev1.Lifecycle{
			PreStop: &corev1.LifecycleHandler{
				Exec: &corev1.ExecAction{
					// the sqlproxy doesn't run on PID 1, and the container doesn't have ps installed
					// so we look for the process in /proc and do some sed magic to get the PID
					// then send a SIGTERM and sleep for 60 seconds.
					// This is meant to help prevent connections from being blackholed by the kube-proxy
					// by closing the TCP listener before the pod is put into a terminating state
					// TODO(pjtatlow): remove this once we can do graceful restarts with externalNetworkPolicy=local
					// 				   https://github.com/kubernetes/enhancements/issues/1669
					Command: []string{
						"/bin/sh",
						"-c",
						"PID=$(ls -l /proc/*/exe 2> /dev/null | grep cockroach/cockroach | sed -E 's/^.*proc\\/([0-9]+).*$/\\1/g'); kill -s TERM $PID; sleep 60",
					},
				},
			},
		},
	}

	// When we turn on the PROXY protocol for AWS, in order for graceful
	// termination to work nicely with NLBs, we will sleep for 10 minutes
	// before turning off the TCP listener. We need to do this because when the
	// pod goes into the Terminating state, AWS will continue sending requests
	// to the deregistering IP target for up to 3 minutes based on experiments.
	// Turning off the TCP listener too soon will cause new connections to fail.
	// 10 minutes should be way more than enough.
	//
	// TODO(jaylim-crl): Investigate this preStop behavior for GCP clusters.
	// A cleaner way would be to implement a --drain-wait flag within sqlproxy,
	// and handle the graceful termination there instead.
	if requireProxyProtocol {
		container.Lifecycle.PreStop.Exec.Command = []string{
			"/bin/sh",
			"-c",
			"PID=$(ls -l /proc/*/exe 2> /dev/null | grep cockroach/cockroach | sed -E 's/^.*proc\\/([0-9]+).*$/\\1/g'); sleep 600; kill -s TERM $PID",
		}
	}
	return *container
}

func tenantDirContainer(image string) corev1.Container {
	// tenantdir is a sidecar container that keeps track and manages tenant
	// backends, related metadata, and their endpoints. This serves as a liaison
	// to the tenant and pod K8s metadata needed by the SQLProxy. For a list of
	// gRPC endpoints, see https://github.com/cockroachdb/cockroach/blob/64dad5f1/pkg/ccl/sqlproxyccl/tenant/directory.proto#L80-L92.
	command := []string{
		"/bin/tenantdir",
		fmt.Sprintf("--listen-addr=0.0.0.0:%d", TenantDirPort),
		"--operator-addr=cockroach-operator.default.svc:9070",
	}
	return corev1.Container{
		Name:            tenantDirContainerName,
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: securitycontext.ContainerRunAsNonRoot(),
		Command:         command,
		Ports: []corev1.ContainerPort{
			{
				Name:          "http",
				ContainerPort: TenantDirPort,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          "td-prometheus",
				ContainerPort: 8082,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		ReadinessProbe: &corev1.Probe{
			// The sqlproxy process connects to the tenant directory server in
			// an async manner. If we mark the container as ready after the
			// first probe (t=0), there is a possibility where the proxy hasn't
			// had the chance to establish a connection via retries, and it has
			// already received a connection via the load balancer, leading to
			// an internal server error on the client.
			//
			// To avoid such a race, we will ensure that we successfully probe
			// at least twice before marking the container as ready. That should
			// be good enough to give the proxy time to establish a connection.
			// By default, PeriodSeconds is 10 seconds, so the container will
			// take at least that amount of time to fully be ready.
			SuccessThreshold: 2,
			ProbeHandler: corev1.ProbeHandler{
				GRPC: &corev1.GRPCAction{
					Port: TenantDirPort,
				},
			},
		},
		Env: []corev1.EnvVar{
			{
				Name: K8sNamespaceEnvVar,
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.namespace",
					},
				},
			},
		},
		VolumeMounts: []corev1.VolumeMount{{
			Name:      "tenantdir-certs",
			MountPath: "/certs",
		}},
	}
}

func configReloaderContainer() corev1.Container {
	// config-reloader is a sidecar container for the SQLProxy pod. It watches
	// for changes to the cockroach certs and config directories and sends a
	// SIGHUP to the sqlproxy process upon a change, which will (eventually)
	// reload sqlproxy.
	return corev1.Container{
		Name:            "cert-reloader",
		Image:           constants.GetReloaderImage(),
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: securitycontext.ContainerRunAsNonRoot(),
		Args:            []string{sqlproxyProcessName, CockroachCertsPath, sqlproxyConfigPath},
		VolumeMounts: []corev1.VolumeMount{
			{
				MountPath: CockroachCertsPath,
				Name:      "certs",
				ReadOnly:  true,
			},
			{
				MountPath: sqlproxyConfigPath,
				Name:      "config",
				ReadOnly:  true,
			},
		},
	}
}

func inflightDirVolume() corev1.Volume {
	return corev1.Volume{
		Name: "inflightdir",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}
}

func tenantDirSecretMount() corev1.Volume {
	return SecretVolume(SecretVolumeOptions{
		Name:    "tenantdir-certs",
		Secrets: []string{TenantDirCertSecretName},
	})
}

func proxyCertVolume() corev1.Volume {
	return SecretVolume(SecretVolumeOptions{
		Name: "certs",
		// We use the externally trusted certificate for the proxy's client side.
		// Rename the files to reflect that it is used for the external
		// interface and doesn't have much to do with the UI (except the fact that
		// the UI need externally trusted cert too and uses the same one)
		// Using two different certificates is possible but is unnecessary.
		Renames: map[string][]corev1.KeyToPath{
			SqlProxyExternalCertSecretName: {
				{Key: SqlProxyExternalCertName + ".crt", Path: "external.crt"},
				{Key: SqlProxyExternalCertName + ".key", Path: "external.key"},
			},
		},
		Secrets: []string{SqlProxyExternalCertSecretName},
	})
}

// proxyTenantCACertVolume returns a volume mount containing the CA
// certificate(s) for all CrdbTenant server certificates. This provides
// certificate verification for proxy <-> tenant SQL connections.
func proxyTenantCACertVolume() corev1.Volume {
	return SecretVolume(SecretVolumeOptions{
		Name: "tenant-ca-certificate",
		Renames: map[string][]corev1.KeyToPath{
			CockroachNodeCertSecretName: {
				{Key: "ca.crt", Path: "tenant-ca.crt"},
			},
		},
		// NB: Using CockroachNodeCertSecretName exploits a detail of
		// CC clusters' certificate topology. Ideally, we'd include the
		// tenant CA in the SqlProxyExternalCertSecretName Secret or
		// its own secret. There's just not a great way to create and
		// backfill certificates at this time, so we rely on the host
		// cluster's node secret.
		// TODO(chrisseto): Create a standalone sqlproxy secret
		// containing the desired CA. Perhaps in conjunction with
		// a refactor serverless' certificate structure.
		Secrets: []string{CockroachNodeCertSecretName},
	})
}

func proxyConfigVolume(volumeName, configMapName string) corev1.Volume {
	return corev1.Volume{
		Name: volumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: configMapName,
				},
				// Read for just user.
				DefaultMode: proto.Int32(0400),
			},
		},
	}
}

// sqlproxyNetworkPolicy returns a network policy that limits ingress to port
// 26257 and 26259, and egress to kube-dns, tenant pods, and the Kubernetes API
// server for the sqlproxy pod. It is assumed that values in k8sAPIServerIPs are
// valid IPs if present.
//
// NOTE: Ingress and egress rules must match up to allow connections: tenants
//
//	must allow ingress from some set of pods that includes the sqlproxy.
func sqlproxyNetworkPolicy(k8sAPIServerIPs []string) *netv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	networkPolicy := &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: SqlproxyAppName,
		},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": SqlproxyAppName,
				},
			},
			Ingress: []netv1.NetworkPolicyIngressRule{
				// Ingress is limited to 26257 and 26259, as specified in
				// SQLProxy's --listen-addr and --proxy-protocol-listen-addr
				// startup arguments.
				{
					Ports: []netv1.NetworkPolicyPort{
						{
							Protocol: &tcp,
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: v1beta1.DefaultCockroachSQLPort,
							},
						},
						{
							Protocol: &tcp,
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: CockroachProxyProtocolPort,
							},
						},
					},
				},
				// Allow prometheus to scrape metrics from the sqlproxy.
				{
					From: []netv1.NetworkPolicyPeer{
						{
							// A namespace selector requires labels on the
							// namespaces themselves, which we currently do not
							// have. For now, we'll bite the bullet and allow
							// all namespaces to access this pod.
							NamespaceSelector: &metav1.LabelSelector{},
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": prometheusAppName,
								},
							},
						},
						{
							NamespaceSelector: &metav1.LabelSelector{},
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": otelcolAppName,
								},
							},
						},
					},
				},
				// Allow sqlproxies to reach one another. This allows sql
				// proxies to forward cancellation requests over HTTP.
				{
					From: []netv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": SqlproxyAppName,
								},
							},
						},
					},
				},
			},
			Egress: []netv1.NetworkPolicyEgressRule{
				// Allow access to kube-dns for resolving tenant names to pod IPs.
				{
					To: []netv1.NetworkPolicyPeer{
						{
							// Empty NamespaceSelector allows cross-namespace access.
							// AFAIK, there is no way to specify just the kube-system namespace.
							NamespaceSelector: &metav1.LabelSelector{},
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"k8s-app": "kube-dns",
								},
							},
						},
					},
				},
				// Allow access to the cockroach-operator for getting CPU load
				// metrics from prometheus.
				{
					To: []netv1.NetworkPolicyPeer{
						{
							// Empty NamespaceSelector allows cross-namespace access.
							// AFAIK, there is no way to specify just the "default" namespace.
							NamespaceSelector: &metav1.LabelSelector{},
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": OperatorAppLabel,
								},
							},
						},
					},
				},
				// Allow access to tenant pods for proxying SQL connections.
				{
					To: []netv1.NetworkPolicyPeer{
						{
							// Currently, we allow access to tenants in all namespaces.
							NamespaceSelector: &metav1.LabelSelector{},
							PodSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "app",
										Operator: metav1.LabelSelectorOpIn,
										Values:   []string{TenantPoolAppName},
									},
								},
							},
						},
					},
				},
				// Allow sqlproxies to reach one another. This allows sql
				// proxies to forward cancellation requests over HTTP.
				{
					To: []netv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": SqlproxyAppName,
								},
							},
						},
					},
				},
			},
		},
	}

	// Allow access to the Kubernetes API server. These IPs are obtained from
	// the "kubernetes" endpoint within the "default" namespace. Note that for
	// EKS clusters, the IPs obtained may change since the user-facing endpoint
	// that was provided by AWS is a DNS name. When that happens, we will need
	// to query the endpoint resource again to retrieve the set of new IPs, and
	// update the network policy accordingly.
	//
	// TODO(chris): The Kubernetes API server is available via the "kubernetes"
	// service in the default namespace, but matching on the namespace does
	// not seem to work. We should try looking into cilium to see if it helps.
	var k8sAPIServerPolicy []netv1.NetworkPolicyPeer
	for _, ip := range k8sAPIServerIPs {
		k8sAPIServerPolicy = append(k8sAPIServerPolicy, netv1.NetworkPolicyPeer{
			IPBlock: &netv1.IPBlock{
				CIDR: ip + "/32",
			},
		})
	}
	if len(k8sAPIServerPolicy) != 0 {
		networkPolicy.Spec.Egress = append(networkPolicy.Spec.Egress, netv1.NetworkPolicyEgressRule{
			To: k8sAPIServerPolicy,
		})
	}
	return networkPolicy
}

func sqlproxyCiliumNetworkPolicy() []kube.Object {
	labelsMap := map[string]any{
		"app": SqlproxyAppName,
	}

	// Rules that apply within the current namespace.
	namespacedPolicy := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cilium.io/v2",
			"kind":       "CiliumNetworkPolicy",
			"metadata": map[string]any{
				"name":   SqlproxyAppName,
				"labels": labelsMap,
			},
			"spec": map[string]any{
				"egress": []any{
					// Allow access to kube-dns for resolving DNS names.
					// (e.g. tenantdir reaching out to the operator server
					// through `cockroach-operator.default.svc`).
					map[string]any{
						"toEndpoints": []any{
							map[string]any{
								"matchLabels": map[string]any{
									"k8s-app":                         "kube-dns",
									"k8s:io.kubernetes.pod.namespace": "kube-system",
								},
							},
						},
					},
					// Allow access to the cockroach-operator for scaling.
					map[string]any{
						"toEndpoints": []any{
							map[string]any{
								"matchLabels": map[string]any{
									"app":                             OperatorAppLabel,
									"k8s:io.kubernetes.pod.namespace": "default",
								},
							},
						},
					},
					// Allow access to tenant pods for proxying SQL connections.
					map[string]any{
						"toEndpoints": []any{
							map[string]any{
								"matchLabels": map[string]any{
									"app": TenantPoolAppName,
								},
							},
						},
					},
					// Allow sqlproxies to reach one another. This allows sql
					// proxies to forward cancellation requests over HTTP.
					map[string]any{
						"toEndpoints": []any{
							map[string]any{
								"matchLabels": map[string]any{
									"app": SqlproxyAppName,
								},
							},
						},
					},
					// Allow access to the Kubernetes API server.
					map[string]any{
						"toEntities": []any{"kube-apiserver"},
					},
				},
				"ingress": []any{
					// Ingress is limited to 26257 and 26259, as specified in
					// SQLProxy's --listen-addr and --proxy-protocol-listen-addr
					// startup arguments.
					map[string]any{
						"toPorts": []any{
							map[string]any{
								"ports": []any{
									map[string]any{
										"port":     fmt.Sprintf("%d", v1beta1.DefaultCockroachSQLPort),
										"protocol": "TCP",
									},
									map[string]any{
										"port":     fmt.Sprintf("%d", CockroachProxyProtocolPort),
										"protocol": "TCP",
									},
								},
							},
						},
					},
					// Allow sqlproxies to reach one another. This allows sql
					// proxies to forward cancellation requests over HTTP.
					map[string]any{
						"fromEndpoints": []any{
							map[string]any{
								"matchLabels": map[string]any{
									"app": SqlproxyAppName,
								},
							},
						},
					},
				},
				"endpointSelector": map[string]any{
					"matchLabels": labelsMap,
				},
			},
		},
	}

	// Rules that apply within the current cluster (e.g. include multi-region
	// setups). Here, the prometheus instances in every region scrapes all
	// regions. Defining a namespace-scoped policy can be difficult so we use
	// a cluster wide policy here.
	clusterWidePolicy := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cilium.io/v2",
			"kind":       "CiliumClusterwideNetworkPolicy",
			"metadata": map[string]any{
				"name":   fmt.Sprintf("%s-clusterwide", SqlproxyAppName),
				"labels": labelsMap,
			},
			"spec": map[string]any{
				"ingress": []any{
					// Allow prometheus to scrape metrics from the sqlproxy.
					map[string]any{
						"fromEndpoints": []any{
							map[string]any{
								"matchLabels": map[string]any{
									// Prometheus runs with the label app=prometheus.
									// There is a constant for this value in the
									// monitoring package but it imports
									// kubecreate and would lead to an import cycle.
									"app": "prometheus",
								},
							},
						},
					},
					// Allow otelcol to scrape metrics from the sqlproxy.
					map[string]any{
						"fromEndpoints": []any{
							map[string]any{
								"matchLabels": map[string]any{
									"app": otelcolAppName,
								},
							},
						},
					},
				},
				"endpointSelector": map[string]any{
					"matchLabels": labelsMap,
				},
			},
		},
	}

	return []kube.Object{namespacedPolicy, clusterWidePolicy}
}

// SqlProxyRBAC returns the manifest of RBAC-related objects used when deploying
// the sqlproxy component. The service account is only used by the tenantdir
// component.
func SqlProxyRBAC(namespace, appName string) []kube.Object {
	var objects []kube.Object

	labels := map[string]string{"app": appName}
	objects = append(objects, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appName,
			Namespace: namespace,
			Labels:    labels,
		},
	})
	objects = append(objects, tenantDirRole(namespace, labels, appName)...)

	return objects
}

// tenantDirRole returns the manifest of role and role binding objects used
// when deploying the tenantdir component. The tenantdir is used as part of the
// SQL Proxy pod, and serves as the liaison to the tenant and pod K8s metadata
// needed by the SQL Proxy.
func tenantDirRole(
	namespace string, labels map[string]string, serviceAccountName string,
) []kube.Object {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantDirName,
			Namespace: namespace,
			Labels:    labels,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"crdb.cockroachlabs.com"},
				Resources: []string{"crdbtenants"},
				Verbs:     []string{"get", "update", "watch"},
			},
			{
				APIGroups: []string{"crdb.cockroachlabs.com"},
				Resources: []string{"crdbclusters"},
				Verbs:     []string{"list"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"list", "watch"},
			},
		},
	}

	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantDirName,
			Namespace: namespace,
			Labels:    labels,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     tenantDirName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      serviceAccountName,
				Namespace: namespace,
			},
		},
	}

	return []kube.Object{role, roleBinding}
}

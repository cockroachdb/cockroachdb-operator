package manifests

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	log "github.com/cockroachlabs/cockroach-enterprise-operator/manifests/log"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/constants"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/securitycontext"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/hashutil"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
	"github.com/cockroachlabs/version"
	"google.golang.org/protobuf/proto"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// TenantPodState specifies the current state of a tenant pool pod. This is
// stored in the pod's Labels set.
type TenantPodState string

const (
	// TenantPodPendingState means that the tenant pod is waiting to be stamped
	// by a tenant, or has been stamped and is waiting for certificates.
	TenantPodPendingState TenantPodState = "PENDING"
	// TenantPodRunningState means that the sqlstarter process on the tenant
	// pod has received certificates and has started the SQL process.
	TenantPodRunningState TenantPodState = "RUNNING"
	// TenantPodDrainingState means that the tenant pod is in the process of
	// shutting down SQL connections (gracefully if possible) so that it can be
	// terminated.
	TenantPodDrainingState TenantPodState = "DRAINING"
	// TenantPodDeletingState means that the tenant pod is currently being
	// terminated by Kubernetes. This status is detected by checking the pod's
	// DeletionTimestamp, NOT via the TenantPodStateLabel.
	TenantPodDeletingState TenantPodState = "DELETING"
)

const (
	// TenantPoolAppName is the name of the Deployment that creates pods that
	// are part of the warm tenant pool.
	TenantPoolAppName = "tenantpool"
	// TenantPoolNameLabel is the name of the labels that tracks inclusion
	// in a given TenantPool by name.
	TenantPoolNameLabel = "tenantpool-name"
	// TenantIDLabel is the name of the label that stamps a tenant pool pod with
	// a tenant's identifier.
	TenantIDLabel = "tenant-id"
	// TenantProberLabel is the name of the label that tracks if the tenant is
	// an internal probing tenant.
	TenantProberLabel = "tenant-prober"
	// TenantPodStateLabel is the name of the label that tracks the current
	// state of a pod in the tenant pool.
	// NOTE: GetTenantPodState and SetTenantPodState must be used to get/set
	// this label.
	TenantPodStateLabel = "state"
	// TenantPodStaleLabel is the name of the label that indicates whether the
	// tenant pod is stale. It's presence implies the pod being stale (the
	// corresponding label value is insignificant).
	//
	// TODO(jaylim-crl): Deprecate this in favor of TenantPodRevisionAnnotation.
	TenantPodStaleLabel = "stale"
	// TenantPodRevisionAnnotation is the name of the annotation containing
	// a hash of the tenantpool pod and its contents. This will be used to
	// indicate whether a given tenant pod is stale.
	//
	// TODO(jaylim-crl): We currently also use the stale=true label to indicate
	// staleness, but the long term plan is to move away from that, and use this
	// instead, so for now, we'll check both.
	TenantPodRevisionAnnotation = "crdb.cockroachlabs.com/tenant-pod-revision"
	// TenantPodStateTimestampAnnotation is the name of the annotation containing
	// the time at which the TenantPodStateLabel was last set. The timestamp is
	// in time.RFC3339 format, in UTC, e.g. 2006-01-02T15:04:05Z07:00.
	// NOTE: PENDING and DELETING pods do not use this annotation, since they use
	// the CreationTimestamp and DeletionTimestamp, respectively, instead.
	TenantPodStateTimestampAnnotation = "state-ts"
	// TenantEnableAdhocPodCreationAnnotation overrides the
	// ClusterFeature ClusterTenantCreateAdhocPods on the per-tenant
	// basis. If this annotation is present on a CrdbTenant, we will
	// allow the CrdbTenant reconciler to create ad-hoc tenantpool
	// pods outside of the warm pool.
	TenantEnableAdhocPodCreationAnnotation = "crdb.cockroachlabs.com/adhoc-pod-creation-enabled"
	// TenantPoolAdhocPodLabel is an label on a pod that indicates it
	// was created in an adhoc manner, i.e. outside of the warm pool.
	TenantPoolAdhocPodLabel = "adhoc"
	// PodStateTimestampFormat is a custom time format that obeys the K8s label
	// alphanumeric character restrictions.
	PodStateTimestampFormat = "2006-01-02T15.04.05Z"
	// SqlStarterServerName is the name of the sqlstarter service, as stored in
	// the TLS server certificate. This same name is used for every pod in the
	// tenant pool.
	SqlStarterServerName = "sqlstarter"
	// TenantPodCheckpointFileName is the name of the checkpoint file used by
	// both the sqlpod-manager and init-sqlsidecar. If such a file was sent as
	// part of the tenant archive, its contents should be in the form of
	// "<tenant-id>\n".
	TenantPodCheckpointFileName = "STARTER_CHECKPOINT"
	// TenantPodLocalityFileName is the name of the locality file used by the
	// cockroachdb process. This should store the locality of the node that the
	// SQL pod is running on. We send the locality data for tenants during the
	// stamping process as it isn't trivial for the SQL pods to reach out to
	// the Kubernetes API server to retrieve the node's locality on their own
	// due to network policy restrictions.
	TenantPodLocalityFileName = "cockroach-locality"
	// OtelCollectorConfigFileName is the name of the config file used by
	// the OpenTelemetry Collector process.
	OtelCollectorConfigFileName = "otelcol-agent-config.yaml"
	// OtelCollectorAWSMetricsCredentialsFileName is the name of the shared
	// credentials file used for exporting metrics to AWS. This will always be
	// stored in OtelCollectorVolumePath.
	OtelCollectorAWSMetricsCredentialsFileName = "aws_metrics_credentials"
	// OtelCollectorAWSLogsCredentialsFileName is the name of the shared
	// credentials file used for exporting logs to AWS. This will always be
	// stored in OtelCollectorVolumePath.
	OtelCollectorAWSLogsCredentialsFileName = "aws_logs_credentials"

	// These refer to MaxSurge and MaxUnavailable values when performing the
	// RollingUpdate strategy on the tenantpool deployment. During an update,
	// we will ensure that the number of terminating tenantpool pods do not go
	// below 25% of its desired replicas.
	tenantPoolRollingUpdateMaxSurge       = "0"
	tenantPoolRollingUpdateMaxUnavailable = "25%"

	// EmptyDir volume used to store all the binaries needed for the sidecar
	// containers. We use a shared volume here since EmptyDir consumes
	// ephemeral storage, and we would like to avoid duplicating the starter
	// binaries multiple times (e.g. one for fluentbit, and another for
	// otelcol-agent).
	sharedBinVolumeName = "bin"
	sharedBinVolumePath = "/cockroach/bin"

	// EmptyDir volume used to store cockroachdb related information (e.g. logs,
	// goroutine dumps, memory profiles).
	//
	// NOTE: If we update this path, we should also ensure that we update the
	// logging config so that logs are written to the new path.
	cockroachDataTenantVolumeName = "cockroach-data"
	cockroachDataTenantVolumePath = "/cockroach/cockroach-data"

	// EmptyDir volume used to store tenant related certs for cockroachdb.
	cockroachCertsTenantVolumeName = "cockroach-certs"
	CockroachCertsTenantVolumePath = "/cockroach/cockroach-certs"

	// EmptyDir volume used to store the sqlpod-manager's data (e.g. internal
	// checkpoint file).
	podManagerVolumeName = "manager-data"
	podManagerVolumePath = "/cockroach/manager-data"

	// EmptyDir volume used to store configuration related to the OpenTelemetry
	// Collector (e.g. environment variables, config files).
	otelCollectorVolumeName = "otelcol-data"
	OtelCollectorVolumePath = "/cockroach/otelcol-data"

	// EmptyDir volume used to store configuration related to FluentBit (e.g config files).
	fluentbitVolumeName = "fluent-bit-data"
	FluentbitVolumePath = "/cockroach/fluent-bit-data"

	// EmptyDir volume used by FluentBit to buffer data.
	fluentbitStorageVolumeName = "fluent-bit-storage"
	FluentbitStorageVolumePath = "/cockroach/fluent-bit-storage"

	// TODO(jaylim-crl): Duplicated from pkg/otel/otel.go. We can't import
	// that directly as there is an import cycle. We'd have to figure out a
	// way to move stuff into the manifests package.
	otelCollectorDefaultImage             = "us-docker.pkg.dev/cockroach-cloud-images/data-plane/opentelemetry-collector-cc:v0.149.0-f0ade0cd72b2"
	otelCollectorMetricsPort              = 9030
	OtelCollectorMetricsPortName          = "otel-metrics"
	otelCollectorPrometheusScrapePort     = 9031
	otelCollectorPrometheusScrapePortName = "otel-scrape"

	// FluentbitMetricsPortName is the name of the port that exposes the metrics
	// endpoint for the service itself. The dataplane's prometheus will be
	// scraping from this endpoint.
	FluentbitMetricsPortName = "fbit-metrics"

	// TODO(alyshan): Duplicated from pkg/logging/dataplane/fluentbit/fluentbit.go. We can't import
	// that directly as there is an import cycle. We'd have to figure out a
	// way to move stuff into the manifests package.
	fluentbitImage = "us-docker.pkg.dev/cockroach-cloud-images/data-plane/fluent-bit-cc:v2.2.2-4622f5d37183"

	minTenantPoolSize         = 3
	SqlStarterPort            = 8081
	sqlStarterCertsVolumeName = "sqlstarter-certs"
	sqlStarterCertsVolumePath = "/cockroach/sqlstarter-certs"

	// Prometheus and otelcol app names are defined in pkg/monitoring, but we
	// repeat them here to avoid import cycles.
	prometheusAppName = "prometheus"
	otelcolAppName    = "otelcol-agent"

	// Tailscale label and selector used for updating cilium network policy.
	TailscaleLabel = "name"
	TailscaleApp   = "tailscale"
)

func sqlStarterImage() string {
	return "us-docker.pkg.dev/cockroach-cloud-images/data-plane/sqlstarter:c7c9546ed424c16d6f715cb7f64a4f69a0ef4406"
}

func initSqlSidecarImage() string {
	return "us-docker.pkg.dev/cockroach-cloud-images/data-plane/init-sqlsidecar:881122ac5f08ef50d574cc5d736338373a386c79"
}

var (
	// DefaultTenantResourceRequests is a collection of defaults for tenant
	// resource requests As ResourceList is a map under the hood, each value is
	// defaulted individually.
	DefaultTenantResourceRequests = corev1.ResourceList{
		// The requests are more or less a safeguard so we don't schedule too
		// many tenant pods on a cluster with too few nodes. Ideally we should
		// be monitoring the cluster usage and add nodes or stop adding new
		// tenants before we hit these.
		corev1.ResourceCPU:    resource.MustParse("50m"),
		corev1.ResourceMemory: resource.MustParse("750M"),
		// Minimum requirements for ephemeral storage:
		// - Container logs up to 100MB (e.g. GKE up to 25MB, EKS up to 100MB).
		// - Ephemeral storage volumes:
		//   - EmptyDir at /cockroach/config to store generated logging config
		//     (~8kb)
		//   - EmptyDir at /cockroach/tenant-data for certs and sqlstarter
		//     binary (~16MB)
		//   - EmptyDir at /cockroach/cockroach-data-tenant to store memory or
		//     goroutine dumps, and logs (~75MB)
		//
		// Note that this resource requests do not account for disk spilling.
		corev1.ResourceEphemeralStorage: resource.MustParse("200M"),
	}
	// publicInternetAccess is a NetworkPolicyEgressRule that allows unfettered
	// access to the public internet
	publicInternetAccess = netv1.NetworkPolicyEgressRule{
		To: []netv1.NetworkPolicyPeer{
			{
				IPBlock: &netv1.IPBlock{
					CIDR: "0.0.0.0/0",
					// Except here prevents tenants from reaching back into our
					// cluster to services that are not explicitly enabled. It
					// prevents this rule from matching requests but does not
					// prevent access that would otherwise be granted.
					// https://en.wikipedia.org/wiki/Private_network
					Except: []string{
						"10.0.0.0/8",     // Private IPs, 10.0.0.0 – 10.255.255.255
						"172.16.0.0/12",  // Private IPs, 172.16.0.0 – 172.31.255.255
						"192.168.0.0/16", // Private IPs, 192.168.0.0 – 192.168.255.255
						"169.254.0.0/16", // link local
						"224.0.0.0/4",    // multicast
					},
				},
			},
		},
	}
)

// TenantPoolOptions modify how the tenant pool Deployment manifest is created.
type TenantPoolOptions struct {
	// Name is the identifier of this tenant pool. All related objects will be
	// named tenantpool-%(Name)s.
	Name string

	// CockroachImage is the location and name of the CockroachDB Docker image
	// to deploy on SQL tenant pods.
	CockroachImage string

	// AllowInternetAccess indicates whether or not tenants in this pool are
	// allowed to contact the public internet. This enables features such as
	// BACKUP and RESTORE.
	AllowInternetAccess bool

	// Replicas is the number of tenant pool pods within its deployment.
	Replicas uint32

	// Resources is the requests and limits of various resource types for this
	// set of tenant pods. Limits are optional. For default requests, see
	// `DefaultTenantResourceRequests` above.
	Resources corev1.ResourceRequirements

	// PriorityClassName is the name of the k8s priority class for pods created
	// from this pool.
	PriorityClassName string

	// Env is used to set the locality flags for the tenant pool.
	Env *cloud.Environment

	// MaxOffset controls the --max-offset flag of the tenant pool.
	// This value must match the host cluster's max offset setting.
	MaxOffset string

	// NetworkPlugin indicates the network plugin that is being used within
	// the Kubernetes cluster. This will be used to determine the type of
	// network policies that will be installed.
	NetworkPlugin cloud.NetworkPlugin

	// MultiRegion is set to true if the tenant pool should support multi-region tenants.
	MultiRegion bool

	// Name of the logging ConfigMap to mount into tenant pool pods.
	LoggingConfigMapName string

	// CockroachChannelEnvVal is the value of the COCKROACH_CHANNEL environment.
	// This field is usually populated by reading the CrdbCluster's spec.
	CockroachChannelEnvVal string

	// OtelCollectorImage is the location and name of the OpenTelemetry
	// Collector Docker image to deploy on SQL pods. If this is left empty, the
	// default in `otelCollectorDefaultImage` will be used.
	OtelCollectorImage string

	// NodeGRPCPort is the port used by the kv node to serve grpc connections.
	NodeGRPCPort int
}

// MakeStampedPodSelector returns a selector that returns all pods that are
// stamped with the given tenant ID.
func MakeStampedPodSelector(id crdbv1beta1.TenantID) labels.Selector {
	query := fmt.Sprintf("app=%s,%s=%d", TenantPoolAppName, TenantIDLabel, id)
	return kube.ParseLabelOrDie(query)
}

// MakeStaleStampedPodSelector returns a selector that returns all pods that are
// stamped with the given tenant ID and are labelled as stale.
func MakeStaleStampedPodSelector(id crdbv1beta1.TenantID) labels.Selector {
	query := fmt.Sprintf("app=%s,%s=%d,%s", TenantPoolAppName, TenantIDLabel, id, TenantPodStaleLabel)
	return kube.ParseLabelOrDie(query)
}

// MakeRunningStampedPodSelector returns a selector that returns all pods that are
// stamped with the given tenant ID and with the TenantPodState label set to RUNNING.
func MakeRunningStampedPodSelector(id crdbv1beta1.TenantID) labels.Selector {
	query := fmt.Sprintf("app=%s,%s=%d,%s=%s", TenantPoolAppName, TenantIDLabel, id, TenantPodStateLabel, TenantPodRunningState)
	return kube.ParseLabelOrDie(query)
}

// MakeTenantPoolSelector returns a selector that returns all unstamped pods
// from the given tenant pool.
func MakeTenantPoolSelector(name string) labels.Selector {
	selector := fmt.Sprintf("!%s,%s=%s,app=%s", TenantIDLabel, TenantPoolNameLabel, name, TenantPoolAppName)
	return kube.ParseLabelOrDie(selector)
}

// MakeStalePoolSelector identifies pods bound to a tenant that were bound from
// a different tenant pool.
func MakeStalePoolSelector(tenantId crdbv1beta1.TenantID, poolName string) labels.Selector {
	selector := fmt.Sprintf("%s,%s!=%s,%s=%d",
		TenantPoolNameLabel,
		TenantPoolNameLabel, poolName,
		TenantIDLabel, tenantId)
	return kube.ParseLabelOrDie(selector)
}

// MakeStampedPodsSelector returns a selector that returns all pods that are
// stamped.
func MakeStampedPodsSelector() labels.Selector {
	query := fmt.Sprintf("app=%s, %s", TenantPoolAppName, TenantIDLabel)
	return kube.ParseLabelOrDie(query)
}

// MakeUnstampedPodsSelector returns a selector that returns all pods that are
// not stamped regardless of tenant pool.
func MakeUnstampedPodsSelector() labels.Selector {
	query := fmt.Sprintf("app=%s, !%s", TenantPoolAppName, TenantIDLabel)
	return kube.ParseLabelOrDie(query)
}

func MakeStaleStampedPodsSelector() labels.Selector {
	selector := fmt.Sprintf("app=%s, %s, %s", TenantPoolAppName, TenantIDLabel, TenantPodStaleLabel)
	return kube.ParseLabelOrDie(selector)
}

func MakePendingAdhocPodSelector() labels.Selector {
	query := fmt.Sprintf(
		"app=%s,%s=%s,%s=true",
		TenantPoolAppName,
		TenantPodStateLabel,
		TenantPodPendingState,
		TenantPoolAdhocPodLabel,
	)
	return kube.ParseLabelOrDie(query)
}

// GetTenantPodState returns the state of the given tenant pod and the timestamp
// at which it entered that state. If the needed tenant labels do not exist or
// are malformed, then the empty string and zero time is returned.
func GetTenantPodState(pod *corev1.Pod) (state TenantPodState, timestamp time.Time) {
	if pod.DeletionTimestamp != nil {
		return TenantPodDeletingState, pod.DeletionTimestamp.Time
	}

	state = TenantPodState(pod.Labels[TenantPodStateLabel])
	if state == TenantPodPendingState {
		// PENDING pods do not use the timestamp annotation.
		return TenantPodPendingState, pod.CreationTimestamp.Time
	}

	timestampStr := pod.Annotations[TenantPodStateTimestampAnnotation]
	timestamp, err := time.Parse(PodStateTimestampFormat, timestampStr)
	if err != nil {
		timestamp = time.Time{}
	}
	return state, timestamp
}

// SetTenantPodState sets the tenant pod state labels on the given pod. This
// includes the labels for the state value and the state timestamp.
func SetTenantPodState(pod *corev1.Pod, state TenantPodState) error {
	switch state {
	case TenantPodPendingState, TenantPodRunningState, TenantPodDrainingState:
		break
	default:
		// Don't allow Deleting state to be set, since that can only be triggered
		// by K8s.
		return errors.AssertionFailedf("cannot set pod state to %s", state)
	}

	// Set the state label.
	if pod.Labels == nil {
		pod.Labels = labels.Set{}
	}
	pod.Labels[TenantPodStateLabel] = string(state)

	// Set the state-ts annotation. This is not set for the PENDING state.
	if state != TenantPodPendingState {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[TenantPodStateTimestampAnnotation] = formatPodStateTimestamp()
	}

	return nil
}

// IsTenantPodStale returns true if the TenantPodStaleLabel is present on the pod.
func IsTenantPodStale(pod *corev1.Pod) bool {
	_, ok := pod.Labels[TenantPodStaleLabel]
	return ok
}

// MarkTenantPodAsStale sets the TenantPodStaleLabel on the pod.
func MarkTenantPodAsStale(pod *corev1.Pod) {
	pod.Labels[TenantPodStaleLabel] = "true"
}

// GetTenantPodHashRevision returns a hash string that represents the current
// configuration of the tenant pod. This includes the tenant archive that gets
// sent to the pod during the stamping phase. If the computed hash is different
// from the pod's hash, the pod is considered stale.
//
// TODO(jaylim-crl): Currently, this only supports the OtelCollector and
// Fluentbit fields in the CrdbTenant spec. The long term plan is to also
// include the pod's spec together with the certificates spec. That would
// enable us to fully remove the stale=true label. Note that we don't do pod
// rollover whenever we rollout a new tenantpool spec today (e.g. adding or
// removing containers), so we can't add the pod's spec now. If we end up doing
// that, we should make sure that the dependent images (e.g. sqlpod-manager)
// are static, and isn't dependent on the operator's SHA. If we didn't do that,
// we will end up with a constant pod churn for every operator deploy, which is
// unnecessary.
func GetTenantPodHashRevision(pod *corev1.Pod, tenant *crdbv1beta1.CrdbTenant) string {
	// NOTE: The ordering matters here. If we update this, all existing SQL pods
	// are considered stale, and *must* rollover.
	return hashutil.MustFNV(tenant.Spec.OtelCollector, tenant.Spec.Fluentbit)
}

// IsAdhocPod checks if a pod was created outside of the warm pool
func IsAdhocPod(pod *corev1.Pod) bool {
	value, ok := pod.Labels[TenantPoolAdhocPodLabel]
	return ok && value == "true"
}

func AdhocPodsEnabled(cluster *crdbv1beta1.CrdbCluster, tenant *crdbv1beta1.CrdbTenant) bool {
	enabled := cluster.Spec.FeatureEnabled(crdbv1beta1.ClusterTenantCreateAdhocPods)
	if override, ok := tenant.Annotations[TenantEnableAdhocPodCreationAnnotation]; ok {
		enabled = override == "true"
	}
	return enabled
}

func IsCockroachReady(pod *corev1.Pod) bool {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == CockroachDB {
			// From testing, this status will be false when the
			// container is running and not-ready AND when the
			// container has crashed. Therefore, this can be used to
			// determine if cockroach is ready to serve traffic.
			// Of note, there is no readiness probe on this
			// container. Ready will only be false in a few rare edge
			// cases, usually when something is slightly wrong with
			// K8s itself, or the container gets into a weird state.
			return status.Ready
		}
	}
	// We couldn't find the container, so therefore cockroach is not ready.
	return false
}

// TenantPool constructs the K8s Deployment config that maintains a warm pool of
// tenant pods, ready to be stamped with a particular tenant's ID. When a pod is
// stamped, it's automatically excluded from the Deployment by its Selector.
func TenantPool(opts TenantPoolOptions) []kube.Object {
	if opts.Name == "" {
		panic(errors.AssertionFailedf(".Name was not set."))
	}

	appLabels := map[string]string{"app": TenantPoolAppName, TenantPoolNameLabel: opts.Name}

	tenantPool := []kube.Object{tenantPoolDeployment(opts, appLabels)}

	if opts.NetworkPlugin == cloud.Cilium {
		tenantPool = append(tenantPool, tenantPoolCiliumNetworkPolicies(opts.Name, opts.AllowInternetAccess, appLabels)...)
	} else {
		tenantPool = append(tenantPool, tenantPoolNetworkPolicy(opts.Name, opts.AllowInternetAccess, appLabels))
	}
	return tenantPool
}

// TenantPoolReplicas returns the number of replicas for the warm pool
// deployment. The minimum number of replicas will be `minTenantPoolSize` unless
// zeroReplicas is set to true.
func TenantPoolReplicas(multiplier float32, hostReplicas int32, zeroReplicas bool) uint32 {
	if zeroReplicas {
		return 0
	}
	return uint32(math.Ceil(math.Max(minTenantPoolSize, float64(multiplier)*float64(hostReplicas))))
}

func tenantPoolDeployment(opts TenantPoolOptions, additional labels.Set) *appsv1.Deployment {
	maxSurge := intstr.Parse(tenantPoolRollingUpdateMaxSurge)
	maxUnavailable := intstr.Parse(tenantPoolRollingUpdateMaxUnavailable)
	pendingTenantpoolLabels := map[string]string{
		"app":               TenantPoolAppName,
		TenantPoolNameLabel: opts.Name,
		TenantPodStateLabel: string(TenantPodPendingState),
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:   MakeTenantPoolName(opts.Name),
			Labels: additional,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(opts.Replicas)),
			Selector: &metav1.LabelSelector{
				MatchLabels: additional,
				MatchExpressions: []metav1.LabelSelectorRequirement{
					// Only unstamped pods will be part of the tenantpool. The
					// stamping process stamps the tenant-id label on the pod. This
					// automatically removes the stamped pod from the deployment,
					// and the deployment will trigger creation of a new unstamped
					// pod. We should make sure that pods which are removed from the
					// deployment are GC'ed properly, and this should be handled by
					// the suspension mechanism.
					{Key: TenantIDLabel, Operator: metav1.LabelSelectorOpDoesNotExist},
				},
			},
			// Use a RollingUpdate strategy to make sure we always have pods to
			// stamp even during an update. With the Recreate policy, the only
			// drawback would just be added latency, so using a RollingUpdate
			// here would just be an improvement.
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxSurge:       &maxSurge,
					MaxUnavailable: &maxUnavailable,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					// By default, all unstamped SQL pods are in the PENDING state.
					// Once certs have been sent, this field will transition to the
					// RUNNING state. Note that it is a deliberate decision to not
					// include this as part of the deployment spec.
					//
					// The idea is that we first reserve the pod by stamping it with
					// the tenant-id label. PENDING here means that the pod is ready
					// to accept certs, and once that has been transitioned to
					// RUNNING, it should be ready to serve traffic.
					Labels: pendingTenantpoolLabels,
				},
				Spec: corev1.PodSpec{
					PriorityClassName: opts.PriorityClassName,
					SecurityContext:   securitycontext.PodRunAsNonRoot(),
					// NOTE: EmptyDir is used here to persist data across
					// container restarts.
					Volumes: []corev1.Volume{
						// Destination of all shared binaries: /cockroach/bin.
						{
							Name: sharedBinVolumeName,
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
						// sqlpod-manager's volume. Stores the internal
						// checkpoint file.
						{
							Name: podManagerVolumeName,
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
						// Volume for TLS certificates used to encrypt
						// communication between the operator and the sqlstarter
						// process.
						{
							Name: sqlStarterCertsVolumeName,
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: SqlStarterCertSecretName,
									Items: []corev1.KeyToPath{
										{
											Key:  SqlStarterCertName + ".crt",
											Path: SqlStarterCertName + ".crt",
										},
										{
											Key:  SqlStarterCertName + ".key",
											Path: SqlStarterCertName + ".key",
										},
									},
									// Read for the group.
									DefaultMode: proto.Int32(0440),
								},
							},
						},
						// otelcol-agent's volume. Stores config files and
						// environment variables for the OpenTelemetry collector.
						{
							Name: otelCollectorVolumeName,
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
						// /cockroach/cockroach-data, but for SQL pods. Data
						// such as logs, goroutine dumps, and memory profiles
						// will be stored here.
						{
							Name: cockroachDataTenantVolumeName,
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
						// /cockroach/cockroach-certs, but for SQL pods. Stores
						// all tenant-related certs.
						{
							Name: cockroachCertsTenantVolumeName,
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
						// The SQL pod logging configuration is different than KV
						// nodes. SQL pods hold fewer logs on disk given their
						// ephemeral nature. They also have additional network sinks
						// to send logs to a fluent-bit sidecar running in the pod.
						log.LoggingConfigMapVolume(opts.LoggingConfigMapName),
						// Configuration for the fluent-bit sidecar.
						{
							Name: fluentbitVolumeName,
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
						// Storage volume for the fluent-bit sidecar.
						{
							Name: fluentbitStorageVolumeName,
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
					Containers: []corev1.Container{
						// TODO(andyk): Do we need a cert-reloader container that
						// monitors changes in sqlstarter-certs? Maybe not, since
						// tenant pool pods turn over quickly and newer pods would
						// get the updated certs.
						cockroachdbContainer(
							opts.CockroachImage,
							opts.AllowInternetAccess,
							opts.Resources,
							opts.MaxOffset,
							opts.Env,
							opts.MultiRegion,
							opts.CockroachChannelEnvVal,
							opts.NodeGRPCPort,
						),
						otelCollectorContainer(opts.OtelCollectorImage, opts.Env),
						fluentbitContainer(),
						sqlpodManagerContainer(),
					},
					InitContainers: []corev1.Container{
						{
							Name:            "init-bin",
							Image:           initSqlSidecarImage(),
							ImagePullPolicy: corev1.PullIfNotPresent,
							SecurityContext: securitycontext.ContainerRunAsNonRoot(),
							VolumeMounts: []corev1.VolumeMount{
								{
									MountPath: sharedBinVolumePath,
									Name:      sharedBinVolumeName,
								},
							},
							Command: []string{
								"sh",
								"-c",
								// Run cp instead of mv since we're running as
								// non-root.
								fmt.Sprintf(
									"cp init-sqlsidecar %s/init-sqlsidecar",
									sharedBinVolumePath,
								),
							},
						},
					},
					// Ensures even pod distribution across nodes and zones to improve availability.
					// Uses `ScheduleAnyway` to avoid scheduling failures if perfect balance isn't possible.
					TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
						{
							MaxSkew:           1,
							TopologyKey:       corev1.LabelHostname,
							WhenUnsatisfiable: corev1.ScheduleAnyway,
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: pendingTenantpoolLabels,
							},
						},
						{
							MaxSkew:           1,
							TopologyKey:       corev1.LabelTopologyZone,
							WhenUnsatisfiable: corev1.ScheduleAnyway,
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: pendingTenantpoolLabels,
							},
						},
					},
				},
			},
		},
	}
}

func sqlpodManagerContainer() corev1.Container {
	return corev1.Container{
		Name:            "sqlpod-manager",
		Image:           sqlStarterImage(),
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: securitycontext.ContainerRunAsNonRoot(),
		Command: []string{
			"/cockroach/sqlstarter",
			"--listen",
			fmt.Sprintf("0.0.0.0:%d", SqlStarterPort),
			"--cert-file",
			fmt.Sprintf("%s/%s.crt", sqlStarterCertsVolumePath, SqlStarterCertName),
			"--key-file",
			fmt.Sprintf("%s/%s.key", sqlStarterCertsVolumePath, SqlStarterCertName),
			"--checkpoint-file-name",
			TenantPodCheckpointFileName,
			"--data-dir",
			podManagerVolumePath,
		},
		Ports: []corev1.ContainerPort{
			{
				ContainerPort: SqlStarterPort,
				Name:          "sqlstarter",
				Protocol:      corev1.ProtocolTCP,
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			// Shared volumes.
			{
				MountPath: sharedBinVolumePath,
				Name:      sharedBinVolumeName,
				ReadOnly:  true,
			},
			// sqlpod-manager volumes.
			{
				MountPath: podManagerVolumePath,
				Name:      podManagerVolumeName,
			},
			{
				MountPath: sqlStarterCertsVolumePath,
				Name:      sqlStarterCertsVolumeName,
				ReadOnly:  true,
			},
			// otelcol-agent volumes.
			{
				MountPath: OtelCollectorVolumePath,
				Name:      otelCollectorVolumeName,
			},
			// fluent-bit volumes.
			{
				MountPath: FluentbitVolumePath,
				Name:      fluentbitVolumeName,
			},
			// cockroachdb volumes.
			{
				MountPath: CockroachCertsTenantVolumePath,
				Name:      cockroachCertsTenantVolumeName,
			},
		},
		StartupProbe: &corev1.Probe{
			PeriodSeconds: 1,
			// FailureThreshold is the number of failed probes that are run
			// before the pod is restarted. The default of 3 proved too short
			// under real load.
			FailureThreshold: 15,
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   "/health",
					Port:   intstr.FromInt(SqlStarterPort),
					Scheme: corev1.URISchemeHTTPS,
				},
			},
		},
		Env: []corev1.EnvVar{
			// This environment variable is used by the sqlpod-manager to
			// ensure that incoming requests are received by the right SQL pod.
			{
				Name: "POD_UID",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.uid",
					},
				},
			},
		},
	}
}

func cockroachdbContainer(
	cockroachImage string,
	allowInternetAccess bool,
	resources corev1.ResourceRequirements,
	maxOffset string,
	cloudEnv *cloud.Environment,
	multiRegion bool,
	cockroachChannelEnvVal string,
	grpcPort int,
) corev1.Container {
	if resources.Requests == nil {
		resources.Requests = make(corev1.ResourceList, 3)
	}

	for resource, request := range DefaultTenantResourceRequests {
		if _, ok := resources.Requests[resource]; !ok {
			resources.Requests[resource] = request
		}
	}

	// Custom flags passed to the cockroach binary. Note that flags here are
	// executed in the context of /bin/bash, so we could use bash-style
	// substitutions.
	crdbFlags := []string{
		"--tenant-id-file",
		filepath.Join(CockroachCertsTenantVolumePath, TenantPodCheckpointFileName),
		"--certs-dir",
		CockroachCertsTenantVolumePath,
		"--kv-addrs",
		fmt.Sprintf("%s:%d", InternalLoadBalancerService, grpcPort),
		"--sql-addr",
		fmt.Sprintf(":%d", crdbv1beta1.DefaultCockroachSQLPort),
		"--listen-addr",
		fmt.Sprintf(":%d", crdbv1beta1.DefaultCockroachGRPCPort),

		// Use mounted logging configuration and replace instances of ${HOST_IP} in
		// configuration with value from environment.
		"--log-config-file", "/cockroach/config/logs.yaml",
		"--log-config-vars", "HOST_IP",

		// Set advertise-addr to enable pod-to-pod communication and TLS server
		// name verification.
		// See: https://kubernetes.io/docs/concepts/services-networking/dns-pod-service/#a-aaaa-records-1
		//
		//     POD_IP="10.0.2.15"
		//     NAMESPACE="us-central1"
		//     advertise-addr="10-0-2-15.us-central1.pod.cluster.local"
		//
		// We want "echo $POD_IP | sed s/\\./-/g", so be careful of the double
		// escapes if we update this.
		"--advertise-addr",
		fmt.Sprintf(
			"$(echo $POD_IP | sed s/\\\\./-/g).$%s.pod.cluster.local",
			K8sNamespaceEnvVar,
		),

		// Start the SQL pod with 30% of the cockroach container's memory limit
		// as max SQL memory. For a 12GB limit that is about 3.6GB. The value
		// came from load experiments. It keeps the SQL pod from being OOMKilled,
		// and we keep it low because tenant pods are oversubscribed.
		"--max-sql-memory",
		"30%",

		// The default cockroach-data directory has the following format:
		// `cockroach-data-tenant-<tenant_id>`, which will be difficult to
		// know during sqlstarter's runtime. To configure an EmptyDir volume,
		// we would need to know the directory ahead of time. Due to that,
		// we will explicitly specify a store that we want to use.
		"--store",
		fmt.Sprintf("path=%s", cockroachDataTenantVolumePath),

		// Default for temp storage is 100MB. Override that to 7.2G based on a
		// disk spilling analysis. We allow users to spill up to 2x of
		// --max-sql-memory, which is 3.6G for a 12GB pod, so 7.2G here.
		//
		// 7.2G assumes a 12GB memory limit. --max-sql-memory follows the limit
		// set on the container, but this value is fixed, so if the tenant pool
		// memory limit changes this value needs to change with it.
		"--max-disk-temp-storage",
		"7.2G",

		// On multi-tenant clusters, we always want to start the cockroach
		// processes with this flag. The use of implicit credentials won't work
		// with metadata access blocked and missing cloud provider credentials.
		// When a user attempts to do so, the user will experience a "hang"
		// until the connection hits a timeout. This will fail-fast, and provide
		// a better user experience.
		"--external-io-disable-implicit-credentials",
	}

	// If internet access is disabled for pods within this tenantpool, start
	// the cockroach process with the --external-io-disabled flag so that we
	// do not even attempt to connect to external resources, which will
	// eventually lead to a timeout. This provides a better user experience
	// for the tenants.
	if !allowInternetAccess {
		crdbFlags = append(crdbFlags, "--external-io-disabled")
	}

	if crdbVersionSupportsLocalityFile(cockroachImage) {
		// The locality file includes the region and zone configuration for the
		// SQL pod. This file is sent as part of the tenant archive during the
		// stamping process, and we had to do this instead of using the init
		// container approach (just like dedicated) because the SQL pods have
		// restricted access to the K8s API server (which is required to fetch
		// the zone).
		//
		// e.g. contents of locality file:
		//      "region=gcp-us-central1,az=gcp-us-central1-f"
		crdbFlags = append(crdbFlags, fmt.Sprintf(
			"--locality-file=%s",
			filepath.Join(CockroachCertsTenantVolumePath, TenantPodLocalityFileName),
		))
	} else {
		// TODO(jaylim-crl): Remove this block once all host clusters are
		// v24.2.0+.
		if cloudEnv != nil && cloudEnv.CloudRegion != "" {
			region := cloudEnv.CloudRegion
			if cloudEnv.CloudProvider != cloud.Unknown {
				region = fmt.Sprintf("%s-%s", cloudEnv.CloudProvider, region)
			}
			crdbFlags = append(crdbFlags, fmt.Sprintf("--locality=region=%s", region))
		}
	}

	if maxOffset != "" {
		crdbFlags = append(crdbFlags, fmt.Sprintf("--max-offset=%s", maxOffset))
	}

	envVars := []corev1.EnvVar{
		// This environment variable denotes the path to the directory
		// containing SSL certificates and keys. It is explicitly specified so
		// that we don't have to add the --certs-dir flag when running
		// `cockroach` commands.
		{
			Name:  "COCKROACH_CERTS_DIR",
			Value: CockroachCertsTenantVolumePath,
		},
		// This is a temporary flag that enables enterprise features for the
		// tenant process.  It should be replaced once it is possible for
		// the tenant processes to read the enterprise license stored in the
		// host cluster. The actual value of this license is not
		// important.  The database just verifies the encoding is correct.
		//
		// The license is intentionally already expired. That way, if a change
		// starts validating the expiration time, it fails immediately rather
		// than at some future date.
		//
		// ./crl-lic -type Enterprise -org "Cockroach Labs" -site -expiration "2000-01-01 00:00 UTC"
		// License Type:	Enterprise
		// Issued To:	Cockroach Labs
		// Expires:	1999-12-31 19:00:00 -0500 EST
		{
			Name:  "COCKROACH_TENANT_LICENSE",
			Value: "crl-0-EICHtcMDGAEiDkNvY2tyb2FjaCBMYWJz",
		},
		// This environment variable turns on a CRDB feature that allows the
		// SQL Proxy to supply the real address of the client via a server
		// status parameter: `crdb:remote_addr`. This is used for logging
		// and authentication purposes. For more information, see:
		// https://github.com/cockroachdb/cockroach/commit/ca3ea6b831d7152d13d496d7fdc5e872cf338acb.
		//
		// Note that this env variable will eventually be removed, and
		// replaced with another mechanism.
		{
			Name:  "COCKROACH_TRUST_CLIENT_PROVIDED_SQL_REMOTE_ADDR",
			Value: "true",
		},
		// This environment variable turns on logging for authn events for
		// SQL pods. For more information, see:
		// https://github.com/cockroachdb/cockroach/commit/42b3e31538236d1b0d65d08273171aff6385cd58.
		//
		// Note that this env variable can be removed since tenants now
		// support cluster settings. For that to work, we will need to
		// ensure that all tenants (both existing and new) have the
		// `server.auth_log.sql_sessions.enabled` cluster setting set to
		// true.
		{
			Name:  "COCKROACH_ALWAYS_LOG_AUTHN_EVENTS",
			Value: "true",
		},
		// This environment variable turns on logging for telemetry events
		// for SQL pods, removing the need to use a cluster setting query
		// to accomplish the same task. For more information, see:
		// https://github.com/cockroachdb/cockroach/issues/70775
		{
			Name:  "COCKROACH_SQL_TELEMETRY_QUERY_SAMPLING_ENABLED",
			Value: "true",
		},
		// This environment variable enables the changefeed usage metrics
		// (changefeed.usage.table_bytes and related) that CockroachDB Cloud
		// billing reads to meter CDC. KV nodes get this via the crdbcluster
		// rollout attribute, but changefeeds on tenants run in the SQL pods,
		// so it must be set here too. It was introduced by
		// https://github.com/cockroachdb/cockroach/pull/123145. See CC-36793.
		{
			Name:  "COCKROACH_ENABLE_CLOUD_BILLING_ACCOUNTING",
			Value: "true",
		},
		// The COCKROACH_MR_SYSTEM_DATABASE environment variable is used to
		// enable the experimental multi-region optimizations. The variable
		// was introduced by
		// https://github.com/cockroachdb/cockroach/pull/90408.
		//
		// TODO(jeffswenson): remove this and the multiRegion argument
		// after the environment variable is removed from CRDB.
		{
			Name: "COCKROACH_MR_SYSTEM_DATABASE",
			Value: func() string {
				if multiRegion {
					return "true"
				}
				return "false"
			}(),
		},
		// These two environment variables are not used by CRDB. It's used by
		// CockroachDB Cloud to denote whether a cockroachdb process was started
		// with split SQL/RPC listeners. The debug zip code relies on this to
		// tell which port to dial.
		{
			Name:  "COCKROACH_RPC_PORT",
			Value: fmt.Sprintf("%d", crdbv1beta1.DefaultCockroachGRPCPort),
		},
		{
			Name:  "COCKROACH_SQL_PORT",
			Value: fmt.Sprintf("%d", crdbv1beta1.DefaultCockroachSQLPort),
		},
		// The following values are used by the sqlstarter, **executed in
		// the context of the CRDB pod**, to synthesize Pod DNS for the
		// --advertise-addr flag for Tenant Pods. Pods from tenant pools
		// are not part of a service and have ephemeral IPs. In order for
		// the server name of their TLS certificates to be validated, they
		// use a wild card for all pods in their namespace and piggypack
		// off Kubernetes' "Pod DNS". Pod DNS grants all pods a domain of
		// <podIP s/./-/>.<namespace>.pod.cluster.local.
		//
		// See `crdbFlags` above.
		{
			Name: K8sNamespaceEnvVar,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.namespace",
				},
			},
		},
		{
			Name: "POD_IP",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "status.podIP",
				},
			},
		},
		{
			Name: "HOST_IP",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "status.hostIP",
				},
			},
		},
	}
	// This is an environment variable that is used for Sentry reporting. That
	// way, we can easily identify which errors came from CC. This usually
	// takes the value of "CockroachCloud-{Staging,Production}".
	if cockroachChannelEnvVal != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name:  CockroachChannelEnvVar,
			Value: cockroachChannelEnvVal,
		})
	}

	// NOTE: For regular KV pods, we supply a readinessProbe since we are using
	// a service to access them. This is not the case here since the tenant
	// directory handles routing and load balancing instead (e.g. we won't route
	// to failed pods and will try to start a new pod instead).
	return corev1.Container{
		Name:            CockroachDB,
		Image:           cockroachImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: securitycontext.ContainerRunAsNonRoot(),
		// We use /bin/bash here so we could support bash-style substitutions
		// for values that need to be evaluated at runtime. For example:
		//     "--advertise-addr=$(echo $POD_IP | sed s/\\./-/g).$NAMESPACE..."
		Command: []string{
			"/bin/bash",
			"-cx",
			"/cockroach/cockroach mt start-sql " + strings.Join(crdbFlags, " "),
		},
		Ports: []corev1.ContainerPort{
			{
				ContainerPort: crdbv1beta1.DefaultCockroachSQLPort,
				Name:          "sql",
				Protocol:      corev1.ProtocolTCP,
			},
			{
				ContainerPort: crdbv1beta1.DefaultCockroachGRPCPort,
				Name:          "grpc",
				Protocol:      corev1.ProtocolTCP,
			},
			{
				ContainerPort: crdbv1beta1.DefaultCockroachHTTPPort,
				Name:          "https",
				Protocol:      corev1.ProtocolTCP,
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				MountPath: cockroachDataTenantVolumePath,
				Name:      cockroachDataTenantVolumeName,
			},
			{
				MountPath: CockroachCertsTenantVolumePath,
				Name:      cockroachCertsTenantVolumeName,
				ReadOnly:  true,
			},
			{
				MountPath: CockroachConfigVolumePath,
				Name:      log.LoggingConfigMapVolumeName,
				ReadOnly:  true,
			},
		},
		Resources: resources,
		Env:       envVars,
	}
}

func fluentbitContainer() corev1.Container {
	return corev1.Container{
		Name:            "fluent-bit",
		Image:           fluentbitImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: securitycontext.ContainerRunAsNonRoot(),
		Command: []string{
			fmt.Sprintf("%s/init-sqlsidecar", sharedBinVolumePath),
			"--tenant-id-file",
			fmt.Sprintf("%s/%s", FluentbitVolumePath, TenantPodCheckpointFileName),
			"--command",
			fmt.Sprintf("/fluent-bit/bin/fluent-bit --config=%s/%s", FluentbitVolumePath, "fluent-bit.conf"),
		},
		// NOTE: All port names should be prefixed with fbit so that they do not
		// collide with ports from other containers. Names should not exceed
		// 15 characters.
		Ports: []corev1.ContainerPort{
			{
				ContainerPort: constants.FluentbitRedactedPort,
				Name:          "fbit-redacted",
			},
			{
				ContainerPort: constants.FluentbitUnredactedPort,
				Name:          "fbit-customer",
			},
			{
				ContainerPort: 2020,
				Name:          FluentbitMetricsPortName,
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      fluentbitVolumeName,
				MountPath: FluentbitVolumePath,
				ReadOnly:  true,
			},
			{
				Name:      fluentbitStorageVolumeName,
				MountPath: FluentbitStorageVolumePath,
			},
			// Shared volumes.
			{
				MountPath: sharedBinVolumePath,
				Name:      sharedBinVolumeName,
				ReadOnly:  true,
			},
		},
	}
}

func otelCollectorContainer(
	otelCollectorImage string, cloudEnv *cloud.Environment,
) corev1.Container {
	envVars := []corev1.EnvVar{
		// Used in the config file to attach a region label to the metrics.
		{
			Name: K8sNamespaceEnvVar,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.namespace",
				},
			},
		},
		// Used in the config file to relabel the instance label.
		{
			Name: "POD_IP",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "status.podIP",
				},
			},
		},
	}

	// For AWS clusters, ensure that AWS_REGION is set to a default value. This
	// is used as the default region by the AWS SDK if a specific region isn't
	// specified when making API calls.
	if cloudEnv != nil && cloudEnv.CloudProvider == cloud.AWS {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "AWS_REGION",
			Value: cloudEnv.CloudRegion,
		})
	}

	agentImage := otelCollectorImage
	if agentImage == "" {
		agentImage = otelCollectorDefaultImage
	}
	return corev1.Container{
		Name:            "otelcol-agent",
		Image:           agentImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: securitycontext.ContainerRunAsNonRoot(),
		Command: []string{
			fmt.Sprintf("%s/init-sqlsidecar", sharedBinVolumePath),
			"--tenant-id-file",
			fmt.Sprintf("%s/%s", OtelCollectorVolumePath, TenantPodCheckpointFileName),
			// Use the same path as the base volume. This is fine as all non
			// .env files will be ignored.
			"--env-var-dir",
			OtelCollectorVolumePath,
			"--command",
			fmt.Sprintf("/otelcol-cc --config %s/%s", OtelCollectorVolumePath, OtelCollectorConfigFileName),
		},
		// NOTE: All port names should be prefixed with otel so that they do not
		// collide with ports from other containers. Names should not exceed 15
		// characters.
		Ports: []corev1.ContainerPort{
			{
				Name:          OtelCollectorMetricsPortName,
				ContainerPort: otelCollectorMetricsPort,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          otelCollectorPrometheusScrapePortName,
				ContainerPort: otelCollectorPrometheusScrapePort,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			// Shared volumes.
			{
				MountPath: sharedBinVolumePath,
				Name:      sharedBinVolumeName,
				ReadOnly:  true,
			},
			// otelcol-agent volumes.
			{
				MountPath: OtelCollectorVolumePath,
				Name:      otelCollectorVolumeName,
				ReadOnly:  true,
			},
		},
		// TODO(jaylim-crl): Update resources once we start stamping tenants.
		// Resources: resources,
		// TODO(jaylim-crl): Add liveness probe to automatically restart the
		// container if the process fails?
		Env: envVars,
	}
}

// tenantPoolNetworkPolicy defines a network policy that constraints all
// tenants in this pool. Rules that apply to tenants as a whole, rather than
// specific tenants should be defined here.
func tenantPoolNetworkPolicy(
	name string, allowInternetAccess bool, labels map[string]string,
) *netv1.NetworkPolicy {
	policy := netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:   MakeTenantPoolName(name),
			Labels: labels,
		},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: labels,
			},
			Egress: []netv1.NetworkPolicyEgressRule{
				// Allow access to the FluentBit DaemonSet for emitting logs.
				{
					To: []netv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{},
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": "fluent-bit",
								},
							},
						},
					},
				},
				// Allow access to the OTel collector DaemonSet for emitting logs.
				{
					To: []netv1.NetworkPolicyPeer{
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
				// Allow connecting to the host KV cluster (which does not currently
				// have any connection limitations).
				{
					To: []netv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": "cockroachdb",
								},
							},
							// Match all namespaces.
							NamespaceSelector: &metav1.LabelSelector{},
						},
					},
				},
				// Allow access to kube-dns for resolving the host cluster's DNS.
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
				// Allow access to Register Service (to send diagnostics reports).
				// The service is accessed via two static IPs that are bound to
				// the register.cockroachdb.com DNS name.
				{
					To: []netv1.NetworkPolicyPeer{
						{
							IPBlock: &netv1.IPBlock{CIDR: "13.248.219.156/32"},
						},
						{
							IPBlock: &netv1.IPBlock{CIDR: "76.223.93.89/32"},
						},
					},
				},
			},
			Ingress: []netv1.NetworkPolicyIngressRule{
				// Allow Prometheus to scrape metrics from tenant pods.
				// TODO(andyk): We should probably allow custom Ingress/Egress rules
				// to be defined on the TenantPool definition. However, it's possible
				// we'll decide to run a Prometheus instance within CRDB clusters, in
				// which case the operator will install and maintain it. For now,
				// hardcode app=prometheus here, and we'll revisit when it becomes
				// clearer whether this is standard or custom configuration.
				{
					From: []netv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{},
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": prometheusAppName,
								},
							},
						},
					},
				},
				// Allow the internal otel collector to scrape metrics from
				// tenant pods.
				{
					From: []netv1.NetworkPolicyPeer{
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
				// Allow all traffic from sqlproxy pods, regardless of namespace.
				{
					From: []netv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{},
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": SqlproxyAppName,
								},
							},
						},
					},
				},
				// Allow the operator's Prometheus instance to scrape metrics
				// for autoscaling.
				{
					From: []netv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{},
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": OperatorAppLabel,
								},
							},
						},
					},
				},
			},
		},
	}

	if allowInternetAccess {
		policy.Spec.Egress = append(policy.Spec.Egress, publicInternetAccess)
	}

	return &policy
}

// tenantPoolCiliumNetworkPolicies defines Cilium-based network policies that
// constraints all tenants in this pool. Rules that apply to tenants as a whole,
// rather than specific tenants should be defined here.
//
// TODO(carlo): this network policy should be constructed using Cilium's
// custom resource definition. We cannot add the Cilium package to the managed
// service repo due to conflicting dependencies, specifically prometheus. Once
// the dependencies are untangled, we should refactor this code.
func tenantPoolCiliumNetworkPolicies(
	name string, allowInternetAccess bool, labels map[string]string,
) []kube.Object {
	labelsMap := make(map[string]any)
	for k, v := range labels {
		labelsMap[k] = v
	}

	// Rules that apply within the current namespace.
	namespacedPolicy := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cilium.io/v2",
			"kind":       "CiliumNetworkPolicy",
			"metadata": map[string]any{
				"name":   MakeTenantPoolName(name),
				"labels": labelsMap,
			},
			"spec": map[string]any{
				"egress": []any{
					// Allow access to kube-dns for resolving DNS names (e.g.
					// connecting to KV pods through "cockroachdb-internal").
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
					// Allow access to Register Service (to send diagnostics
					// reports). The service is accessed via two static IPs that
					// are bound to the register.cockroachdb.com DNS name.
					map[string]any{
						"toCIDR": []any{"13.248.219.156/32", "76.223.93.89/32"},
					},
				},
				"ingress": []any{
					// Allow all traffic from sqlproxy pods.
					map[string]any{
						"fromEndpoints": []any{
							map[string]any{
								"matchLabels": map[string]any{
									"app": "sqlproxy",
								},
							},
						},
					},
					// Allow the operator to resume a tenant.
					map[string]any{
						"fromEndpoints": []any{
							map[string]any{
								"matchLabels": map[string]any{
									"app":                             OperatorAppLabel,
									"k8s:io.kubernetes.pod.namespace": "default",
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

	if allowInternetAccess {
		specMap, ok := namespacedPolicy.Object["spec"].(map[string]any)
		if !ok {
			panic("spec is not a map[string]interface{}")
		}
		egressMap, ok := specMap["egress"].([]any)
		if !ok {
			panic("egress is not a []interface{}")
		}

		// Allow access to the public internet.
		specMap["egress"] = append(egressMap, map[string]any{
			"toCIDR": []any{"0.0.0.0/0"},
		})

		var toCidrList []any
		for _, ip := range publicInternetAccess.To[0].IPBlock.Except {
			toCidrList = append(toCidrList, ip)
		}

		// Except here prevents tenants from reaching back into our cluster to
		// services that are not explicitly enabled. It prevents this rule from
		// matching requests but does not prevent access that would otherwise be
		// granted.
		//
		// See: https://en.wikipedia.org/wiki/Private_network
		specMap["egressDeny"] = []any{
			map[string]any{
				"toCIDR": toCidrList,
			},
		}
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
				"name":   fmt.Sprintf("%s-clusterwide", MakeTenantPoolName(name)),
				"labels": labelsMap,
			},
			"spec": map[string]any{
				"egress": []any{
					// Allow connecting to the host KV cluster (which does not
					// currently have any connection limitations).
					map[string]any{
						"toEndpoints": []any{
							map[string]any{
								"matchLabels": map[string]any{
									"app": "cockroachdb",
								},
							},
						},
					},
					// Allow connecting to fluent-bit and OTel via the host's IP
					map[string]any{
						"toEntities": []any{
							"host",
						},
						"toPorts": []any{
							map[string]any{
								"ports": []any{
									map[string]any{
										"port":     fmt.Sprintf("%d", constants.FluentbitRedactedPort),
										"protocol": "TCP",
									},
									map[string]any{
										"port":     fmt.Sprintf("%d", constants.FluentbitUnredactedPort),
										"protocol": "TCP",
									},
									map[string]any{
										"port":     fmt.Sprintf("%d", constants.OtelRedactedPort),
										"protocol": "TCP",
									},
									map[string]any{
										"port":     fmt.Sprintf("%d", constants.OtelUnredactedPort),
										"protocol": "TCP",
									},
								},
							},
						},
					},
					// Allow access to the FluentBit DaemonSet for emitting logs.
					// Note: this will no longer be necessary once the above egress to fluent bit host ports is
					// rolled out (it definitely will be in once we're upgraded to cilium 1.13)
					map[string]any{
						"toEndpoints": []any{
							map[string]any{
								"matchLabels": map[string]any{
									"app": "fluent-bit",
								},
							},
						},
					},
					// Allow access to the OTel collector DaemonSet for emitting logs.
					map[string]any{
						"toEndpoints": []any{
							map[string]any{
								"matchLabels": map[string]any{
									"app": otelcolAppName,
								},
							},
						},
					},
				},
				"ingress": []any{
					// Allow Prometheus to scrape metrics from tenant pods.
					//
					// TODO(andyk): We should probably allow custom
					// Ingress/Egress rules to be defined on the TenantPool
					// definition. However, it's possible we'll decide to run a
					// Prometheus instance within CRDB clusters, in which case
					// the operator will install and maintain it. For now,
					//  hardcode app=prometheus here, and we'll revisit when it
					// becomes clearer whether this is standard or custom
					// configuration.
					map[string]any{
						"fromEndpoints": []any{
							map[string]any{
								"matchLabels": map[string]any{
									"app": prometheusAppName,
								},
							},
						},
					},
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

// formatPodStateTimestamp formats the current UTC time in RFC3339 format, but
// modified to use "." instead of ":" in the time (since ":" is not a supported
// K8s label character). Example: "2006-01-02T15.04.05Z".
func formatPodStateTimestamp() string {
	return timeutil.Now().UTC().Format(PodStateTimestampFormat)
}

// MakeTenantPoolName formats the name in the crdbv1beta1.TenantPool definition
// and is used as the name of the k8s resources belonging to the tenant pool.
func MakeTenantPoolName(name string) string {
	return fmt.Sprintf("%s-%s", TenantPoolAppName, name)
}

// PoolNameFromVersion is a convenience function to convert a version to a hyphenated string.
// For example, "v23.2.6-cloudonly.2" would be converted to "v23-2-6".
func PoolNameFromVersion(v version.Version) string {
	poolName := v.Format("v%X-%Y-%Z")
	return poolName
}

// crdbVersionSupportsLocalityFile returns true if the given CRDB image supports
// the --locality-file CLI flag, or false otherwise. We will only use this new
// flag for v24.2.0+ clusters.
func crdbVersionSupportsLocalityFile(cockroachImage string) bool {
	parts := strings.Split(cockroachImage, ":")
	if len(parts) != 2 {
		logging.Warnf(context.Background(), "unexpected image format %q", cockroachImage)
		return false
	}
	v, err := version.Parse(parts[len(parts)-1])
	if err != nil {
		logging.Warnf(context.Background(), "parsing CockroachImage %q: %v", cockroachImage, err)
		return false
	}
	// At least v24.2.0. Alphas/betas/rcs should all work, but we are unlikely
	// rolling these out to production host clusters. We can't use IsAtLeast
	// here, as the following would fail:
	//     versions.IsAtLeast("v24.2.0-cloudonly.1", "v24.2.0") => false
	return v.AtLeast(version.MustParse("v24.2.0-alpha.0"))
}

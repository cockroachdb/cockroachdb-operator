package manifests

import (
	"math"
	"strconv"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	manifestslog "github.com/cockroachlabs/cockroach-enterprise-operator/manifests/log"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const maxOffsetFlag = "--max-offset"

const (
	// TenantMaxAdhocPodAgeAnnotation is an annotation that stores the
	// maximum time an adhoc pod can be in the pending phase before it
	// is deleted. It is parsed by time.ParseDuration
	MaxTenantAdhocPendingPodAgeAnnotation = "crdb.cockroachlabs.com/max-adhoc-pending-pod-age"
	// DefaultMaxTenantAdhocPendingPodAge is the amount of time we allow an ad-hoc
	// pod to be in the pending state before killing it and trying to
	// create a new pod.
	DefaultMaxTenantAdhocPendingPodAge = 30 * time.Second
	// MaxTenantAdhocPendingPodCountAnnotation is an annotation that
	// stores the maximum number of pending adhoc tenant pods allowed
	// in a cluster. If the number of pending pods exceeds the value
	// defined in this annotation, pod creation will error out and we
	// will fallback to using the warm pool.
	//
	// Note that this is not a hard limit. The worst case maximum
	// number of pending adhoc pods will be
	// numReconcilerThreads+maxPendingPods-1
	MaxTenantAdhocPendingPodCountAnnotation = "crdb.cockroachlabs.com/max-adhoc-pending-pod-count"
	// DefaultMaxTenantAdhocPendingPodCount is the default maximum
	// value for the number of adhoc tenant pods in a cluster.
	DefaultMaxTenantAdhocPendingPodCount = 20
	// MaxTenantUnreadyStampedPodCountAnnotation is an annotation that
	// store the maximum number of unready, stamped pods in a
	// cluster. When this limit is met, the operator will not long try
	// to scale up to replace unready pods.
	MaxTenantUnreadyStampedPodCountAnnotation = "crdb.cockroachlabs.com/max-unready-stamped-count"
	// DefaultMaxUnreadyStampedPodCount is the default maximum number
	// of unready stamped pods in a cluster. If the above annotation
	// is not set on a cluster, this value will be used.
	DefaultMaxUnreadyStampedPodCount = 10
)

func MaxTenantAdhocPendingPodAge(cluster *crdbv1beta1.CrdbCluster) (time.Duration, error) {
	value, ok := cluster.Annotations[MaxTenantAdhocPendingPodAgeAnnotation]
	if !ok {
		return DefaultMaxTenantAdhocPendingPodAge, nil
	}
	return time.ParseDuration(value)
}

func MaxPendingAdhocPods(cluster *crdbv1beta1.CrdbCluster) (int, error) {
	override, ok := cluster.Annotations[MaxTenantAdhocPendingPodCountAnnotation]
	if !ok {
		return DefaultMaxTenantAdhocPendingPodCount, nil
	}
	return strconv.Atoi(override)
}

func MaxUnreadyStampedPods(cluster *crdbv1beta1.CrdbCluster) (int, error) {
	override, ok := cluster.Annotations[MaxTenantUnreadyStampedPodCountAnnotation]
	if !ok {
		return DefaultMaxUnreadyStampedPodCount, nil
	}
	return strconv.Atoi(override)
}

// CrdbClusterQuiescentDuration computes how long it is expected to take a
// crdbcluster to reach the quiescent state after an update.
func CrdbClusterQuiescentDuration(
	cluster *crdbv1beta1.CrdbCluster, regionName string,
) time.Duration {
	// weight is applied to account for additional latency like network round
	// trips or scheduling delays.
	weight := 1.2
	restartDelay := time.Minute
	if cluster.Spec.RollingRestartDelay != nil {
		restartDelay = cluster.Spec.RollingRestartDelay.Duration
	}
	gracePeriod := CRDBTerminationGracePeriodSeconds * time.Second

	nodes := cluster.Spec.NodesInRegion(regionName)
	if nodes == 0 {
		// If there are no nodes, use a value of 1 to avoid a 0 wait duration.
		nodes = 1
	}

	return time.Duration(float64(restartDelay+gracePeriod) * float64(nodes) * float64(weight))
}

// CrdbClusterOperatorPDB fills in a PodDisruptionBudget that is used to
// specify the amount of unavailability for a given set of pods.
func CrdbClusterOperatorPDB(
	cluster *crdbv1beta1.CrdbCluster, replicas int, budget int,
) *policyv1.PodDisruptionBudget {
	// Convert from a relative budget to an absolute MinAvailable by subtracting
	// from the replica count
	minAvailable := intstr.FromInt(max(0, min(replicas, replicas-budget)))

	if replicas == 0 {
		minAvailable = intstr.FromInt(0)
	}

	// NB: In operator clusters, PDBs can only be specified using
	// MaxUnavailable because CrdbCluster does (and cannot) not support the
	// Scale Kubernetes subresource. Support for the Scale subresource is
	// currently not possible because the ".Replicas" field is not at a static
	// path due to multi-region support.
	// See https://kubernetes.io/docs/tasks/run-application/configure-pdb/#specifying-a-poddisruptionbudget.
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name: cluster.Name + "-pdb",
			Labels: map[string]string{
				BetaCrdbClusterNodeOwnerLabelKey: cluster.Name,
			},
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":                            "cockroachdb",
					BetaCrdbClusterNodeOwnerLabelKey: cluster.Name,
				},
			},
		},
	}
}

// CrdbClusterPriorityClasses return two priority classes CRDBCritical and
// CRDBHigh. If a pod with higher priority cannot be scheduled, pods with lower
// priority will be evicted to make room for it. If a pod needs to be
// evicted due to resource constraints (such as low memory on the node),
// lower-priority pods will be evicted first.
// TODO(chrisseto): Only bundle crdb-critical with the CRDB Sts. Monitoring
// currently relies on CRDBHigh existing and should eventually be decoupled.
func CrdbClusterPriorityClasses() []kube.Object {
	critical := &schedulingv1.PriorityClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: CRDBCritical,
		},
		Value:       CriticalPriority,
		Description: "CRDB critical priority pods",
	}
	high := &schedulingv1.PriorityClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: CRDBHigh,
		},
		Value:       HighPriority,
		Description: "CRDB high priority pods",
	}
	return []kube.Object{critical, high}
}

// crdbTenantPriorityClasses builds the priority class CRDBTenant. If a pod
// with higher priority cannot be scheduled, pods with lower priority will be
// evicted to make room for it. If a pod needs to be evicted due to resource
// constraints (such as low memory on the node), lower-priority pods will be
// evicted first.
func crdbTenantPriorityClasses() []kube.Object {
	tenant := &schedulingv1.PriorityClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: CRDBTenant,
		},
		Value:       TenantPriority,
		Description: "CRDB priority for tenant pods",
	}
	return []kube.Object{tenant}
}

// SqlProxyOptions is a collection of options for modifying the SQL Proxy
// in a CrdbCluster.
type SqlProxyOptions struct {
	// Spec is the CrdbClusterSpec of the host cluster that resources are being
	// created for.
	Spec *crdbv1beta1.CrdbClusterSpec

	// K8sAPIServerIPs specifies the list of IPs for the Kubernetes API server.
	// This will be used to create an egress rule for the sqlproxy pod(s).
	KubernetesAPIServerIPs []string

	// NetworkPlugin indicates the type of network plugin that is being used
	// within the Kubernetes cluster.
	NetworkPlugin cloud.NetworkPlugin

	// ApplyConfigMaps, if true, causes the sqlproxy and sqlproxy
	// allowlist config maps to get applied.
	ApplyConfigMaps bool
}

// SqlProxyResources returns the manifests for the deployment of the
// SQL Proxy. It panics if the provided CrdbCluster is not a Host.
func SqlProxyResources(opts SqlProxyOptions) []kube.Object {
	if opts.Spec.TenantHost == nil {
		panic(errors.AssertionFailedf("Spec.TenantHost was nil"))
	}
	// This shouldn't happen unless we call this directly since we have already
	// validated that there should be exactly one region before invoking
	// `deployHostResources`.
	if len(opts.Spec.Regions) == 0 {
		panic(errors.AssertionFailedf("Spec.Regions was empty"))
	}

	// The operator does not yet support creating multi-region clusters.
	//
	// TODO(jaylim-crl): The above statement is incorrect. We should look for
	// the region object based on region.Code, and use that instead. For now,
	// this works in multi-region host clusters as the number of nodes in
	// every region is the same.
	region := &opts.Spec.Regions[0]

	// Default the number of sqlproxy replicas to the number of nodes, though
	// we will ensure that all clusters have at least 3 replicas. This also
	// avoids manually specifying region.Nodes for tests that don't require them.
	sqlProxyReplicas := uint32(math.Ceil(math.Max(float64(region.Nodes), 3)))
	if opts.Spec.TenantHost.SqlProxyReplicas != nil {
		sqlProxyReplicas = uint32(*opts.Spec.TenantHost.SqlProxyReplicas)
	}

	// Default the sqlproxy image to the same cockroach image.
	sqlProxyImage := opts.Spec.Template.Spec.Image
	if opts.Spec.TenantHost.SqlProxyImage != "" {
		sqlProxyImage = opts.Spec.TenantHost.SqlProxyImage
	}

	objects := SQLProxy(SQLProxyOptions{
		Image:                          sqlProxyImage,
		TenantDirImage:                 opts.Spec.TenantHost.TenantDirImage,
		Replicas:                       sqlProxyReplicas,
		K8sAPIServerIPs:                opts.KubernetesAPIServerIPs,
		DisableConnectionRebalancing:   opts.Spec.FeatureEnabled(crdbv1beta1.ClusterSqlProxyDisableConnectionRebalancing),
		NetworkPlugin:                  opts.NetworkPlugin,
		RequireProxyProtocol:           opts.Spec.FeatureEnabled(crdbv1beta1.ClusterSqlProxyProxyProtocol),
		ProxyProtocolListenAddrEnabled: opts.Spec.FeatureEnabled(crdbv1beta1.ClusterSqlProxyProxyProtocolListenAddrEnabled),
		ApplyConfigMaps:                opts.ApplyConfigMaps,
	})

	return objects
}

func TenantPools(
	spec *crdbv1beta1.CrdbClusterSpec, env *cloud.Environment, networkPlugin cloud.NetworkPlugin,
) []kube.Object {
	objects := crdbTenantPriorityClasses()
	clusterMaxOffset := crdbNodeStartFlagValue(&spec.Template.Spec, maxOffsetFlag)
	cockroachChannelEnvVal := crdbNodeEnvVarValue(&spec.Template.Spec, CockroachChannelEnvVar)

	// TODO(chrisseto): In the future, we'll need some way to clean up
	// tenant pools that are no longer in use. For now, if a tenant pool is
	// created via the operator and later removed from the tenant pool
	// list, the deployment will exist but be unused. If the same name is
	// later reused, the left over deployment will be updated as required.
	for _, pool := range spec.TenantHost.TenantPools {
		var poolHostReplicaMultiplier float32
		zeroReplicas := false
		if pool.HostReplicaMultiplier != nil {
			poolHostReplicaMultiplier = *pool.HostReplicaMultiplier
			if poolHostReplicaMultiplier == 0 {
				zeroReplicas = true
			}
		}

		cockroachImage := spec.Template.Spec.Image
		if pool.CockroachImage != "" {
			cockroachImage = pool.CockroachImage
		}

		// Ensure tenant pods always have a logging config map name set. Real
		// clusters should always set this value but tests need a default value.
		loggingConfigMapName := manifestslog.LoggingTenantConfigMapName("")
		if pool.LoggingConfigMapName != "" {
			loggingConfigMapName = pool.LoggingConfigMapName
		}

		objects = append(objects, TenantPool(TenantPoolOptions{
			Name:                pool.Name,
			CockroachImage:      cockroachImage,
			AllowInternetAccess: pool.AllowInternetAccess,
			Replicas: TenantPoolReplicas(
				poolHostReplicaMultiplier,
				spec.NodesInRegion(env.CloudRegion),
				zeroReplicas,
			),
			Resources:              pool.Resources,
			PriorityClassName:      pool.PriorityClassName,
			Env:                    env,
			MaxOffset:              clusterMaxOffset,
			NetworkPlugin:          networkPlugin,
			MultiRegion:            1 < len(spec.Regions),
			LoggingConfigMapName:   loggingConfigMapName,
			CockroachChannelEnvVal: cockroachChannelEnvVal,
			NodeGRPCPort:           int(spec.Template.Spec.GetGRPCPort()),
			OtelCollectorImage:     spec.TenantHost.TenantOtelCollectorImage,
		})...)
	}

	return objects
}

// crdbNodeStartFlagValue returns the last upserted value, falling back to the deprecated field.
func crdbNodeStartFlagValue(spec *crdbv1beta1.CrdbNodeSpec, flagName string) string {
	if spec.StartFlags == nil {
		return spec.Flags[flagName]
	}

	values := parseStartFlagUpserts(spec.StartFlags.Upsert)[flagName]
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

// crdbNodeEnvVarValue returns the effective environment value after pod template merging.
func crdbNodeEnvVarValue(spec *crdbv1beta1.CrdbNodeSpec, envVarName string) string {
	container := corev1.Container{Name: CRDBContainerName, Env: spec.Env}
	if spec.PodTemplate != nil {
		for _, templateContainer := range spec.PodTemplate.Spec.Containers {
			if templateContainer.Name == CRDBContainerName {
				container = mergeContainer(container, templateContainer)
				break
			}
		}
	}

	var result string
	for _, envVar := range container.Env {
		if envVar.Name == envVarName {
			result = envVar.Value
		}
	}
	return result
}

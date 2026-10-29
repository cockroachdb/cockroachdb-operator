package metrics

import (
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubemetrics"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// TenantsScaledTotal is a prometheus metric which keeps track
	// of the total attempts when the autoscaler is updating
	// a CrdbTenant's Spec.DesiredPods. The result label distinguishes
	// between successful updates and errors.
	TenantsScaledTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "autoscaler_tenants_scaled_total",
			Help: "Number of attempts made by the autoscaler to update " +
				"the DesiredPods for a tenant.",
		}, []string{"result"},
	)

	// These labels are used for the AutoscalerRunsTotal and TenantsScaledTotal
	// metrics above.
	SuccessLabel = prometheus.Labels{"result": "success"}
	ErrorLabel   = prometheus.Labels{"result": "error"}

	// TenantsTotal is a prometheus metric which keeps track of the number
	// of CrdbTenants in the cluster.
	TenantsTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "crdb_tenants_total",
			Help: "The total number of CrdbTenant resources in the cluster",
		},
	)

	// TenantsFinalized is a prometheus metric which keeps track
	// of the progress of tenant finalization. It is only reported on *during*
	// finalization so that we can monitor the pace of finalization. When the
	// AwaitFinalization condition is not present or is Finished this metric will
	// report 0.
	TenantsFinalized = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "crdb_tenants_finalized",
			Help: "The total number of tenants that have been finalized when the CrdbCluster" +
				"has the AwaitFinalization action set to Starting. It is 0 otherwise.",
		},
	)

	// EnsureTenantResumedTime is a prometheus metric which keeps track of the
	// duration of time when performing autoscaler.EnsureTenantResumed. This is
	// used by the tenant directory, and kubesql.Open when a tenant
	// is determined to be without any pods and thus needs to be resumed.
	EnsureTenantResumedTime = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "autoscaler_ensure_tenant_resumed_seconds",
		Help: "Length of time observed when waiting for a tenant to be resumed.",
	})

	// WatcherEstablishedTotal is a prometheus metric which keeps track of the
	// total count of times the kube.Cache establishes its watcher.
	WatcherEstablishedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cache_watcher_established_total",
			Help: "Number of times the kubernetes resource cache establishes a watch on resources. " +
				"Notably used by the autoscaler to watch CrdbTenants",
		}, []string{"resource"},
	)

	// InsufficientPodsErrorTotal is a prometheus metric which keeps track
	// of the total count of errors observed when stamping a pod. Specifically,
	// where the error is due to an insufficient number of pods in the tenant's
	// specified warm pool.
	InsufficientPodsErrorTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "crdb_tenant_reconciler_insufficient_pods_error_total",
			Help: "Number of \"insufficient pods in tenant pool\" errors encountered" +
				" when stamping pods.",
		},
	)

	// TenantAdhocPodCreationTotal is a counter that tracks how many
	// adhoc pods have been created for a given tenant
	TenantAdhocPodCreationTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "crdb_tenant_adhoc_pod_creation_total",
			Help: "Number of adhoc pods successfully created while scaling a CrdbTenant",
		},
	)

	// TenantAdhocPodFailedToStartTotal tracks the number of adhoc
	// pods that have been deleted because they failed to start in
	// time. Useful for tuning MaxTenantAdhocPendingPodAge
	TenantAdhocPodFailedToStartTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "crdb_tenant_adhoc_pod_failed_to_start_total",
			Help: "Number of adhoc pods that did not start within the defined time boundary",
		},
	)

	// TenantAdhocPodPendingRequeueTotal tracks the number of requeues
	// triggered because a pending adhoc pod has yet to become
	// ready. Useful for tuning MaxTenantAdhocPendingPodAge
	TenantAdhocPodPendingRequeueTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "crdb_tenant_adhoc_pod_pending_requeue_total",
			Help: "Number of adhoc pods found to be pending within the defined time boundary, triggering a requeue",
		},
	)

	// TenantAdhocPodPendingTimeMs is a metrics that tracks how long an
	// adhoc pod is pending before being marked as running. Adhoc pods
	// are always created with NoWait: true, so they will be marked as
	// running in a separate reconciler call. We control the period of
	// time between reconciliations, so we can use this metric to
	// optimize that time.
	TenantAdhocPodPendingTimeMs = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "crdb_tenant_adhoc_pod_pending_time_ms",
		Help: "Duration of time that an adhoc tenant pod was in the pending state before being " +
			"marked as running, in milliseconds",
	})

	// PodStampTime is a prometheus metric which keeps track of the duration
	// of time it takes for the CrdbTenant reconciler to pick a warm pod
	// from the tenant pool, assign it to a tenant & start the SQL process.
	PodStampAndStartTime = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "crdb_tenant_reconciler_pod_stamp_and_start_time_seconds",
		Help: "Length of time observed when successfully stamping and starting SQL pods.",
	})

	// NodePodEvictCheckErrorCounter is a prometheus metric which keeps
	// track of errors happened when checking for pod evictions.
	NodePodEvictCheckErrorCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "crdb_node_reconciler_eviction_check_error_total",
			Help: "Number of errors encountered when checking for evictions.",
		},
		[]string{"reason"},
	)

	// KubernetesEndpointWatcherCallsTotal is a prometheus metric which keeps
	// track of the total number of times the kubernetes endpoint watcher gets
	// triggered.
	KubernetesEndpointWatcherCallsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "crdb_cluster_reconciler_endpoint_watcher_calls_total",
			Help: "Number of times the kubernetes endpoint watcher gets triggered.",
		},
	)

	// IsClusterDisrupted reflects if a cluster is disrupted: 0 if not, 1 if
	// so.
	IsClusterDisrupted = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "is_cluster_disrupted",
		Help: "Whether a cluster is intentionally disrupted or not",
	})

	// NodeDecommissioning is a gauge metric which reports that a CRDB node is
	// decommissioning.
	NodeDecommissioning = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_decommissioning",
			Help: "Whether a CRDB node is decommissioning.",
		}, []string{"node"},
	)

	// CrdbPodHardEvictionKillsCounter tracks the number of hard evicted CRDB
	// pods that the operator had to forcefully kill. Hard evictions are
	// kublet-initiated and are only triggered during critical circumstances
	// such as node is out of memory.
	CrdbPodHardEvictionKillsCounter = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "crdb_pod_hard_eviction_kills",
			Help: "Number of hard-evicted CRDB pods killed by the operator.",
		},
	)

	TenantPodNotRunningCount = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "crdb_tenant_pod_not_running_count",
			Help: "Number of pods not running in a host cluster. Broken down by phase",
		},
		[]string{"phase"},
	)

	TenantPodNotReadyCount = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "crdb_tenant_pod_not_ready_count",
			Help: "Number of pods not ready in a host cluster. Broken down by tenant name",
		},
		[]string{"tenant_name"},
	)

	TenantScaleAttemptCount = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "crdb_tenant_scale_attempt_count",
			Help: "Number of scaling attempts for tenants.",
		},
	)

	TenantScaleFailureCount = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "crdb_tenant_scale_failure_count",
			Help: "Number of scaling failures for tenants. Labels indicate the tenant and scaling directions",
		}, []string{"scale_up", "scale_down"},
	)

	ActiveTenantMissingOwnerCount = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "crdb_active_tenant_missing_owner_count",
			Help: "Count when the tenant is active and missing an owning cluster.",
		}, []string{"tenant_name"},
	)

	CrdbNodeMissingCluster = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "crdb_node_missing_cluster",
			Help: "Number of CrdbNodes found to be missing a cluster.",
		}, []string{"node_name", "reason"},
	)

	ClusterFeatureEnabled = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "crdb_cluster_feature_enabled",
			Help: "Whether a cluster feature is enabled (1) or disabled (0).",
		}, []string{"feature"},
	)

	HotNodeRemediationTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "crdb_hot_node_remediation_total",
			Help: "Number of times the hot node watcher marked a tenant pod as stale for eviction from a hot node.",
		},
	)
)

// ResourceLabel returns a prometheus label with they key "resource".
// And the value being the kind of resource.
func ResourceLabel(gvk schema.GroupVersionKind) prometheus.Labels {
	return prometheus.Labels{"resource": gvk.Kind}
}

func init() {
	// Register custom metrics with the global prometheus registry
	metrics.Registry.MustRegister(
		InsufficientPodsErrorTotal,
		PodStampAndStartTime,
		EnsureTenantResumedTime,
		TenantsScaledTotal,
		WatcherEstablishedTotal,
		NodePodEvictCheckErrorCounter,
		KubernetesEndpointWatcherCallsTotal,
		grpc_prometheus.DefaultServerMetrics,
		TenantsFinalized,
		TenantsTotal,
		IsClusterDisrupted,
		NodeDecommissioning,
		CrdbPodHardEvictionKillsCounter,
		TenantPodNotRunningCount,
		TenantPodNotReadyCount,
		TenantAdhocPodCreationTotal,
		TenantAdhocPodFailedToStartTotal,
		TenantAdhocPodPendingRequeueTotal,
		TenantAdhocPodPendingTimeMs,
		TenantScaleAttemptCount,
		TenantScaleFailureCount,
		ActiveTenantMissingOwnerCount,
		CrdbNodeMissingCluster,
		ClusterFeatureEnabled,
		HotNodeRemediationTotal,
	)
	metrics.Registry.MustRegister(kubemetrics.Metrics()...)
	grpc_prometheus.EnableHandlingTimeHistogram()
}

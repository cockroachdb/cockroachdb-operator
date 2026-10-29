package migrationctrl

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// MigrationPhaseGauge is a labeled gauge that sets 1 for the active phase and 0 for all others.
	MigrationPhaseGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "crdb_operator",
			Subsystem: "migration",
			Name:      "phase",
			Help:      "Current migration phase for each cluster",
		},
		[]string{"cluster", "namespace", "phase"},
	)

	// MigrationPodsTotal tracks total pods to migrate
	MigrationPodsTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "crdb_operator",
			Subsystem: "migration",
			Name:      "pods_total",
			Help:      "Total number of pods to migrate",
		},
		[]string{"cluster", "namespace"},
	)

	// MigrationPodsMigrated tracks pods successfully migrated
	MigrationPodsMigrated = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "crdb_operator",
			Subsystem: "migration",
			Name:      "pods_migrated",
			Help:      "Number of pods successfully migrated",
		},
		[]string{"cluster", "namespace"},
	)

	// MigrationDurationSeconds tracks how long the migration takes
	MigrationDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "crdb_operator",
			Subsystem: "migration",
			Name:      "duration_seconds",
			Help:      "Duration of migration in seconds",
			Buckets:   prometheus.ExponentialBuckets(10, 2, 10), // 10s to ~2.5 hours
		},
		[]string{"cluster", "namespace", "phase"},
	)

	// MigrationErrorsTotal tracks migration errors
	MigrationErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "crdb_operator",
			Subsystem: "migration",
			Name:      "errors_total",
			Help:      "Total number of migration errors",
		},
		[]string{"cluster", "namespace", "phase", "error_type"},
	)

	// RollbacksTotal tracks rollback operations
	RollbacksTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "crdb_operator",
			Subsystem: "migration",
			Name:      "rollbacks_total",
			Help:      "Total number of rollback operations",
		},
		[]string{"cluster", "namespace", "reason"},
	)
)

func init() {
	// Register metrics with controller-runtime's registry
	metrics.Registry.MustRegister(
		MigrationPhaseGauge,
		MigrationPodsTotal,
		MigrationPodsMigrated,
		MigrationDurationSeconds,
		MigrationErrorsTotal,
		RollbacksTotal,
	)
}

package utils

const (
	monitoringNamespacePrefix = "monitoring-"

	// loggingNamespacePrefix is the prefix used for the namespace that
	// contains the logging DaemonSet (which in turn runs FluentBit).
	loggingNamespacePrefix = "logging-"
)

func MonitoringNamespaceName(regionName string) string {
	return monitoringNamespacePrefix + regionName
}

// LoggingNamespaceName returns the namespace in which FluentBit DaemonSet
// related resources will be created.
func LoggingNamespaceName(regionName string) string {
	return loggingNamespacePrefix + regionName
}

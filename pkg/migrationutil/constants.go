package migrationutil

// Constants used across migration controller and conversion webhook

const (
	// DefaultRegion is used when we can't figure out the region from pod affinity
	// or annotations during migration.
	DefaultRegion = "us-east1"

	// DefaultCloudProvider is used when the cloudProvider annotation is not set. An empty
	// string signals that the user has not specified a provider, which is valid for local
	// or on-prem clusters. The user should set the crdb.cockroachlabs.com/cloudProvider
	// annotation before migration if cloud-specific behavior is needed.
	DefaultCloudProvider = ""

	// DefaultGRPCPort is the CRDB GRPC port used when none is found in the StatefulSet args.
	DefaultGRPCPort = int32(26258)

	// DefaultSQLPort is the CRDB SQL port used when --port / --sql-addr are absent.
	DefaultSQLPort = int32(26257)

	// DefaultHTTPPort is the CRDB HTTP port used when --http-port / --http-addr are absent.
	DefaultHTTPPort = int32(8080)

	// DefaultClusterDomain is the standard Kubernetes cluster domain. Only stored in
	// ParsedMigrationContext.Domain when the actual domain differs from this default.
	DefaultClusterDomain = "cluster.local"
)

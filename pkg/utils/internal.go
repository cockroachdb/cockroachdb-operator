package utils

// Cockroach roles that we can grant/revoke for users.
const (
	// Fixed SQL users that client certificates are issued for.
	RootSQLUser      = "root"
	SQLProberSQLUser = "managed-sql-prober"
	NodeUser         = "node"
	MigrationUser    = "managed-migration"
	DBConsoleUser    = "managed-db-console"

	// This user will be created and destroyed on-demand depending on when
	// the workload was requested, and will only be present in the database
	// when there is at least one workload deployment in the cluster.
	WorkloadSQLUser = "managed-workload"

	AdminRole = "admin"

	// This user still exists in some clusters, but is not created in new
	// clusters.
	// TODO(jeffswenson): clean up the managed service user.
	ObsoleteIntrusionSQLUser = "managed-service"
)

// InternalUsers is a set of SQL users created for cluster management, not to be used
// by customers. This struct is used to hide specific users in the console and to populate the
// host_based_authentication.configuration cluster setting.
// If you add a new one, please be aware you may have to regenerate hba conf values for some
// clusters.
var InternalUsers = map[string]struct{}{
	RootSQLUser: {},

	ObsoleteIntrusionSQLUser: {},
	SQLProberSQLUser:         {},
	NodeUser:                 {},
	WorkloadSQLUser:          {},
	MigrationUser:            {},
	DBConsoleUser:            {},
}

// These users will be created automatically with `admin` roles for k8s clusters through clustersql.
var InternalAdminUsers = map[string]struct{}{
	SQLProberSQLUser: {},
}

// internalDBs is a set of SQL databases created as part of CRDB, not to be used by customers.
var internalDBs = map[string]struct{}{
	"system":   {},
	"postgres": {},
}

func IsInternalUser(user string) bool {
	_, ok := InternalUsers[user]
	return ok
}

func IsInternalDB(db string) bool {
	_, ok := internalDBs[db]
	return ok
}

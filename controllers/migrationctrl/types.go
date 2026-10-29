// Package migrationctrl automates migration of CockroachDB clusters from Helm StatefulSets
// and Public Operator v1alpha1 to CockroachDB Operator v1beta1 CrdbNodes.
//
// A few constraints matter while reading this package.
//
// Older installs still use cluster-name-only RBAC for some cluster-scoped objects. If two
// namespaces reuse the same cluster name, those old objects collide.
//
// Operator rollback keeps skip-reconcile on the v1alpha1 CrdbCluster until cleanup and
// validation are done. Once that label is gone, the public operator starts rebuilding
// its own RBAC, PDB, and services.
//
// Finalization stops at Mode=MutableOnly. The user still deletes the old StatefulSet by hand.
// That delete event is what lets the controller record PhaseComplete.
package migrationctrl

const (
	// Migration phases
	PhaseInit          = "Init"
	PhaseCertMigration = "CertMigration"
	PhasePodMigration  = "PodMigration"
	PhaseFinalization  = "Finalization"
	PhaseComplete      = "Complete"
	PhaseFailed        = "Failed"
	PhaseStopped       = "Stopped" // User-initiated pause

	// Rollback phases
	PhaseRollbackInProgress = "RollbackInProgress"
	PhaseRollbackComplete   = "RollbackComplete"

	// Migration control labels
	LabelMigrate         = "crdb.io/migrate"
	LabelSkipReconcile   = "crdb.io/skip-reconcile"
	LabelMigrationStatus = "crdb.cockroachlabs.com/migration"

	// Label values - user sets this to begin
	MigrateStart = "start"
	// Label value - user can set this to pause migration
	MigrateStop = "stop"
	// Label value - user can set this to request rollback
	MigrateRollback = "rollback"

	// Label values - controller sets these
	MigrateInProgress = "in-progress"
	MigrateComplete   = "complete"
	MigrateFailed     = "failed"
	MigrateStopped    = "stopped" // Controller sets when user requests stop

	// Migration types
	MigrationTypeHelm     = "helm"
	MigrationTypeOperator = "operator"

	// AnnotationMigrationRegions is set on the source StatefulSet (or v1alpha1 CrdbCluster)
	// by the user before migration to describe the full multi-region topology. Its value is
	// a JSON-encoded []CrdbClusterRegion. When present it overrides the single-region entry
	// derived from the --locality flag. Required for multi-region clusters because a single
	// STS only knows its own region; the full topology cannot be auto-detected.
	//
	// Example annotation value (set this on the STS before labelling it crdb.io/migrate=start):
	//   '[{"code":"us-east-1","nodes":3,"namespace":"ns-east"},{"code":"eu-west-1","nodes":3,"namespace":"ns-west"}]'
	AnnotationMigrationRegions = "crdb.cockroachlabs.com/migration-regions"

	// annotationOriginalPorts is saved on the public service before migration modifies
	// its ports. It holds the JSON-encoded original []corev1.ServicePort so rollback can
	// restore the exact pre-migration configuration.
	annotationOriginalPorts = "crdb.cockroachlabs.com/migration-original-ports"

	// Secret and ConfigMap name suffixes used across cert generation, cert resolution,
	// and rollback cleanup. Helm and operator migrations use different naming conventions
	// for their source secrets. The destination secrets always use the Helm-style suffixes.

	// Helm source naming (also used as destination names).
	suffixNodeSecret   = "-node-secret"
	suffixClientSecret = "-client-secret"
	suffixCASecret     = "-ca-secret"
	suffixCAConfigMap  = "-ca-crt"

	// Public operator source naming for K8s Secrets.
	suffixOperatorNode   = "-node"
	suffixOperatorClient = "-root-client"
	suffixOperatorCA     = "-ca"

	// Public operator cert-manager CR naming. The Certificate CRs share the same
	// name as the Secrets they create, so these intentionally duplicate the Secret
	// suffixes above. Keeping them separate makes call sites self-documenting about
	// whether they reference a Secret or a cert-manager Certificate CR.
	suffixOperatorCAIssuer   = "-ca-issuer"
	suffixOperatorCACert     = "-ca-cert"
	suffixOperatorNodeCert   = "-node"
	suffixOperatorClientCert = "-root-client"

	// Helm cert-manager CR naming. Helm cert-manager uses a different client
	// Certificate CR name than the public operator.
	suffixHelmClientCert = "-root"
)

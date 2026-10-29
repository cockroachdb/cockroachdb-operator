// Package migrationctrl automates migration of CockroachDB clusters from Helm StatefulSets
// and Public Operator v1alpha1 to CockroachDB Operator v1beta1 CrdbNodes.
//
// # Activation
//
// The operator must be started with --enable-migration. The user triggers migration by
// labelling the source resource (StatefulSet or v1alpha1 CrdbCluster) with
// crdb.io/migrate=start. Setting the label to "stop" pauses, "rollback" reverses.
//
// # Phase flow
//
// Init -> CertMigration -> PodMigration -> Finalization -> (user deletes STS) -> Complete
//
// Each phase is persisted in status.migration.phase. The controller picks up where it left
// off after a restart. PodMigration loops once per pod (highest ordinal first), scaling the
// STS down by one and creating a CrdbNode for each. Health checks run between pods.
//
// Finalization writes the full CrdbCluster spec derived from the StatefulSet, sets
// Mode=Disabled, and waits. The user deletes the StatefulSet by hand. That delete watch
// event triggers completeMigration which sets Mode=MutableOnly and Phase=Complete.
//
// # Rollback
//
// The user sets crdb.io/migrate=rollback on the source resource. Rollback deletes all
// CrdbNodes (both tracked in MigratedNodes and orphaned ones found via label query),
// scales the StatefulSet back to OriginalReplicas, and restores service selectors to
// match the original deployment style (Helm or public operator). Rollback is best-effort
// and logs warnings for non-fatal cleanup errors rather than failing.
//
// Rollback is not possible after Phase=Complete because the StatefulSet has been deleted
// and there is nothing to restore.
//
// # Webhook isolation
//
// When migration is enabled, the enterprise operator's v1beta1 admission webhook entries
// use matchPolicy=Exact so they only fire for v1beta1 requests. Without this, the API
// server's default Equivalent matching would convert v1alpha1 requests to v1beta1 and
// send them to the v1beta1 webhooks, which reject them because the converted v1beta1
// object lacks ExternalCertificates and other required fields.
//
// The enterprise operator does not register v1alpha1 webhook entries. The public
// operator's existing v1alpha1 webhooks handle defaulting and validation for v1alpha1
// clusters.
//
// The CRD conversion webhook (separate from admission webhooks) uses the public
// operator's crdb.io/skip-reconcile=true label as its ownership boundary. Ordinary
// v1alpha1 clusters continue through compatibility conversion as Mode=Disabled so the
// public operator can create and manage them normally. Fresh installs and completed
// migrations return a minimal, read-only v1alpha1 projection without compatibility
// annotations. Disabled clusters always use the full conversion path because
// skip-reconcile can be applied before migration status exists. An active handoff also
// stays on the full path until Mode, PhaseComplete, and the terminal label have landed.
//
// Once a skip-labelled cluster has no StatefulSet, ConvertTo rejects v1alpha1 writes so
// they cannot replace the stored v1beta1 spec. The public operator's admission webhooks
// must use matchPolicy=Exact during coexistence. Otherwise Equivalent admission can force
// an enterprise v1beta1 write through the protected conversion. After migration,
// betaclusterctrl removes operator-injected annotations to avoid SSA ownership conflicts.
// See api/v1alpha1/crdbcluster_conversion.go.
//
// # Controller interaction during migration
//
// betaclusterctrl and the clusterctrl wrapper wait for the full handoff before normal
// reconciliation. Mode=MutableOnly alone is not enough because status and the terminal
// label land in later API requests. Until then they avoid node, annotation, host resource,
// and status writes that could race migrationctrl.
//
// # Label transitions
//
// The user sets crdb.io/migrate to "start", "stop", or "rollback". The controller
// changes it to "in-progress" once migration begins, and to "complete" or "failed"
// at the end. Removing the label entirely during active migration also triggers
// rollback for backward compatibility. Stopping during rollback is supported:
// setting "stop" pauses the rollback, and setting "rollback" resumes it.
//
// # Health checks
//
// Between each pod migration, the controller runs SQL health checks via the CrdbNode's
// exec interface: zero under-replicated ranges, correct live node count, pod in Ready
// state, and pod registered in the headless service endpoints. If a pod does not pass
// health checks within 10 minutes, the controller auto-pauses by setting Phase=Stopped.
//
// # File map
//
//   - reconciler.go: entry point, watch setup, label-driven dispatch, metrics emission
//   - discovery.go: detects migration type (Helm vs Operator), resolves the source resource
//   - phases.go: phase state machine (Init, CertMigration, PodMigration, Finalization, Complete)
//   - rollback.go: full rollback logic (scale STS back up, delete CrdbNodes, restore services)
//   - certgen.go: self-signed certificate regeneration with join service DNS SANs
//   - certmanager.go: cert-manager Certificate CR updates for join service DNS SANs
//   - health.go: SQL-based health checks (under-replicated ranges, live node count)
//   - helpers.go: spec generation, ConfigMap key migration, pod labelling, PCR detection
//   - service.go: service selector restoration (rollback) and port normalization
//   - rbac.go: RBAC setup, PDB management (delete old formats, create enterprise format)
//   - metrics.go: Prometheus metrics registration
//   - types.go: constants (phases, labels, annotations, secret suffixes)
//
// # Constraints
//
// Older installs still use cluster-name-only RBAC for some cluster-scoped objects. If two
// namespaces reuse the same cluster name, those old objects collide.
//
// Operator rollback keeps skip-reconcile on the v1alpha1 CrdbCluster until cleanup and
// validation are done. Once that label is gone, the public operator starts rebuilding
// its own RBAC, PDB, and services.
//
// The STS parser lives in pkg/migrationutil, not here. It extracts ports, flags, volumes,
// cert mode, and cluster domain from a StatefulSet into a ParsedMigrationContext. The
// v1alpha1 conversion webhook also uses that parser, which is why it cannot live in this
// package (import cycle).
package migrationctrl

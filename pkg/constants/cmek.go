package constants

import "github.com/cockroachlabs/version"

// When a cluster is enabled for CMEK, we begin encrypting its backups with a
// KMS key that the customer provides. We access this key by first
// assuming/impersonating a customer-provided principal, and then querying the
// KMS for the key.
//
// Backups for large clusters may run for an extended period of time. During
// this time, we've seen short-lived credentials for the KMS expiring, blocking
// the encrypted backup from finishing (see CC-7544). CRDB v22.2 introduces
// support for role chaining and automatic credential refresh - two features
// needed to avoid credential expiry. As a result of this, we've opted to make
// CMEK a feature for only v22.2 clusters. See CC-8202 for more details.
//
// TODO(CC-8202) Remove this flag once all clusters in Cockroach Cloud are
// running v22.2 and above.
var MinVersionSupportedForCMEK = version.MustParse("v22.2.0-alpha.0")

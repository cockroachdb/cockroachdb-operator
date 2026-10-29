package v1beta1

// ClusterConditionType is an enumeration of CrdbCluster conditions, listed
// below. Each condition should start with the "Cluster" prefix and be phrased
// as a state adjective, if possible.
type ClusterConditionType string

const (
	// ClusterInitialized is set to True once a newly created cluster has been
	// fully initialized and is ready for connections.
	ClusterInitialized ClusterConditionType = "Initialized"

	// ClusterRangesUnderReplicated is set to False once there are no
	// under-replicated ranges.
	ClusterRangesUnderReplicated ClusterConditionType = "RangesUnderReplicated"

	// ClusterPostInitSQLApplied is True once all post-init SQL statements
	// have completed successfully at least once, and False on any failure.
	// The condition's Reason carries a short error category and Message
	// carries the failing source label plus the CockroachDB error. The
	// condition is absent until the first execution attempt.
	ClusterPostInitSQLApplied ClusterConditionType = "PostInitSQLApplied"
)

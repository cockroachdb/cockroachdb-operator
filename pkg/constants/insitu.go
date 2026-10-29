package constants

const (
	InSituBucketNameTemplate = "%s-in-situ"

	InSituDefaultStorageLifetimeDays = 30

	InSituDefaultGCSLocation = "US"

	// Namespace in which investigation containers reside.
	InSituK8sNamespace = "investigation"

	InSituIAMUsername = "in-situ"

	// Name of k8s service account associated with In-Situ pods.
	InSituK8sServiceAccountName = "in-situ"

	// Name of node pool on which in-situ pods will run.
	InSituVMNodePoolName           = "in-situ"
	InSituVMNodePoolNameLabelKey   = CRLAppLabelKey
	InSituVMNodePoolNameLabelValue = "in-situ"
	InSituVMNodeTaintValue         = "in-situ"

	// Name of the rclone config env variables
	RCloneRemoteName = "INSITU"
	RCloneObjectACL  = "RCLONE_CONFIG_%s_OBJECT_ACL"
	RCloneBucketACL  = "RCLONE_CONFIG_%s_BUCKET_ACL"
	RCloneLocation   = "RCLONE_CONFIG_%s_LOCATION"
)

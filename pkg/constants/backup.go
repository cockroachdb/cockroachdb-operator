package constants

const (
	// WARNING: Do not change this constant unless you know what you are
	// doing. Compatibility will need to be maintained across older clusters
	// and existing deployments.
	IntrusionRestoreIAMUsername = "intrusion-restore"

	// WARNING: Do not change these constants unless you are sure you know what
	// you are doing. Compatibility will need to be maintained across create
	// cluster and backupper somehow.
	//
	// NOTE: This is kept around so that cloud resources belonging to this
	// deprecated service are removed when clusters are deleted.
	BackupIAMUsername = "backupper"

	// GCP Host clusters use a static service account key for implementing
	// tenant restores. The key is provisioned out of band.
	RestoreServiceAccountName     = "restore-service-account"
	RestoreServiceAccountVaultKey = "restore_service_account_key"

	FullBackupStorageLifetimeDays        = 30
	IncrementalBackupStorageLifetimeDays = 7

	// AWS only fields.
	LongLivedPath  = "long-lived/"
	ShortLivedPath = "short-lived/"
)

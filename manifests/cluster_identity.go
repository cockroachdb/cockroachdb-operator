package manifests

import "fmt"

const (
	CredentialsMountPath  = "/cockroach/cockroach-credentials"
	CredentialsVolumeName = "cluster-identity-credentials"

	AzureCredentialsEnvVar     = "COCKROACH_AZURE_APPLICATION_CREDENTIALS_FILE"
	AzureCredentialsSecretName = "azure-cluster-identity-credentials-secret"
	AzureCredentialsFileName   = "azure-credentials"

	OldCredentialsMountPath  = "/cockroach/cockroach-credentials-old"
	OldCredentialsVolumeName = "old-cluster-identity-credentials"

	OldAzureCredentialsEnvVar = "OLD_COCKROACH_AZURE_APPLICATION_CREDENTIALS_FILE"
)

func AzureCredentialsFullPath() string {
	return fmt.Sprintf("%s/%s", CredentialsMountPath, AzureCredentialsFileName)
}

func OldAzureCredentialsFullPath() string {
	return fmt.Sprintf("%s/%s", OldCredentialsMountPath, AzureCredentialsFileName)
}

package manifests

import (
	"fmt"

	"github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/values"
	corev1 "k8s.io/api/core/v1"
)

const (
	rotationPeriod = "720h" // 30 days
)

type encryptionAtRestOptions struct {
	// The name of the k8s secret containing the (new) store key. If nil,
	// this will be interpreted as "plain" i.e. unencrypted.
	KeySecretName *string
	// The cloud platform whose KMS is used to gate the new CMEK.
	CMEKPlatform v1beta1.EncryptionPlatform
	// The name of the k8s secret containing credentials to authenticate
	// into the customer's KMS. Should be non-nil when CMEKPlatform is set.
	CMEKCredentialsSecretName *string

	// The name of the k8s secret containing the old store key. If nil,
	// this will be interpreted as "plain" i.e. unencrypted.
	OldKeySecretName *string
	// The cloud platform whose KMS was used to encrypt the old store key.
	// Defaults to CMEKPlatform for same-provider rotations.
	OldCMEKPlatform v1beta1.EncryptionPlatform
	// The name of the k8s secret containing credentials for the old KMS.
	// Defaults to CMEKCredentialsSecretName for same-provider rotations.
	OldCMEKCredentialsSecretName *string
	// The relative path where the CMEK key is used to decrypt store files.
	StorePath string
}

func addEncryptionAtRestEnvVars(earOptions *encryptionAtRestOptions) []corev1.EnvVar {
	var envVars []corev1.EnvVar

	// KeySecretName will be nil, if EAR is disabled on an enabled cluster
	if earOptions.KeySecretName != nil {
		envVars = append(envVars, storeKeyEnvVar("STORE_KEY", *earOptions.KeySecretName))
		envVars = append(envVars, cmekEnvVars(
			"", earOptions.CMEKPlatform,
			*earOptions.KeySecretName, earOptions.CMEKCredentialsSecretName,
		)...)
	}

	if earOptions.OldKeySecretName != nil {
		envVars = append(envVars, storeKeyEnvVar("OLD_STORE_KEY", *earOptions.OldKeySecretName))
		envVars = append(envVars, cmekEnvVars(
			"OLD_", earOptions.OldCMEKPlatform,
			*earOptions.OldKeySecretName, earOptions.OldCMEKCredentialsSecretName,
		)...)
	}

	return envVars
}

func storeKeyEnvVar(name, secretName string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  "StoreKeyData",
			},
		},
	}
}

// cmekEnvVars builds the CMEK env vars for a given key (new or old). The
// prefix is "" for the new key and "OLD_" for the old key. keySecretName is
// the secret holding the store key metadata (AuthPrincipal, URI, Region, Type,
// ExternalID). credsSecretName is the secret holding cloud-provider credentials.
// Azure is handled separately because it uses volume-mounted credentials.
func cmekEnvVars(
	prefix string,
	platform v1beta1.EncryptionPlatform,
	keySecretName string,
	credsSecretName *string,
) []corev1.EnvVar {
	if platform == "" || platform == v1beta1.EncryptionPlatformUnknown {
		return nil
	}

	envVars := []corev1.EnvVar{
		secretEnvVar(prefix+"AUTH_PRINCIPAL", keySecretName, "AuthPrincipal"),
		secretEnvVar(prefix+"KEY_URI", keySecretName, "URI"),
		secretEnvVar(prefix+"KEY_REGION", keySecretName, "Region"),
		secretEnvVar(prefix+"CMEK_PLATFORM", keySecretName, "Type"),
	}

	// Azure authenticates via workload identity using a credentials file
	// mounted from a fixed secret (azure-cluster-identity-credentials-secret),
	// not from CMEKCredentialsSecretName. The env var here just tells the
	// init container where to find the mounted file, so it must be emitted
	// regardless of whether credsSecretName is set.
	if platform == v1beta1.EncryptionPlatformAzureKeyVault {
		if prefix == "" {
			envVars = append(envVars, corev1.EnvVar{
				Name:  AzureCredentialsEnvVar,
				Value: AzureCredentialsFullPath(),
			})
		} else {
			// Point the init container at the old credentials path so it
			// can locate the Azure KMS credentials for the old key.
			envVars = append(envVars, corev1.EnvVar{
				Name:  OldAzureCredentialsEnvVar,
				Value: OldAzureCredentialsFullPath(),
			})
		}
		return envVars
	}

	if credsSecretName == nil {
		return envVars
	}

	switch platform {
	case v1beta1.EncryptionPlatformAwsKms:
		envVars = append(envVars,
			secretEnvVar(prefix+"AWS_ACCESS_KEY_ID", *credsSecretName, "aws_access_key_id"),
			secretEnvVar(prefix+"AWS_SECRET_ACCESS_KEY", *credsSecretName, "aws_secret_access_key"),
			secretEnvVar(prefix+"AWS_EXTERNAL_ID", keySecretName, "ExternalID"),
		)
	case v1beta1.EncryptionPlatformGcpCloudKms:
		envVars = append(envVars,
			secretEnvVar(prefix+"GCP_SERVICE_ACCOUNT_KEY", *credsSecretName, values.UserServiceAccountKey),
		)
	}

	return envVars
}

func secretEnvVar(name, secretName, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  key,
			},
		},
	}
}

// addEncryptionAtRestCrdbNodeCmd generates the command-line flags needed for encryption at rest.
func addEncryptionAtRestCrdbNodeCmd(earOptions *encryptionAtRestOptions) []string {
	cmdFlags := []string{}

	newStoreKeyFilename := "plain"
	oldStoreKeyFilename := "plain"
	if earOptions.KeySecretName != nil {
		newStoreKeyFilename = fmt.Sprintf("%s/%s", keyMountPath, "new_store.key")
	}
	if earOptions.OldKeySecretName != nil {
		// When new and old point to the same secret, both keys are identical
		// and we can reuse the same decrypted file path. This avoids writing
		// and decrypting the key twice during non-encryption cluster updates.
		if earOptions.KeySecretName != nil && *earOptions.KeySecretName == *earOptions.OldKeySecretName {
			oldStoreKeyFilename = newStoreKeyFilename
		} else {
			oldStoreKeyFilename = fmt.Sprintf("%s/%s", keyMountPath, "old_store.key")
		}
	}
	encFlag := fmt.Sprintf("--enterprise-encryption=path=%s,key=%s,old-key=%s,rotation-period=%s",
		earOptions.StorePath, newStoreKeyFilename, oldStoreKeyFilename, rotationPeriod)
	cmdFlags = append(cmdFlags, encFlag)
	return cmdFlags
}

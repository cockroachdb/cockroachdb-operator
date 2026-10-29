package manifests

import (
	"fmt"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestAddEncryptionAtRestEnvVarsWithNewAndOldKeys(t *testing.T) {
	newSecret := "new-secret"
	oldSecret := "old-secret"

	opts := &encryptionAtRestOptions{
		KeySecretName:    &newSecret,
		OldKeySecretName: &oldSecret,
		CMEKPlatform:     v1beta1.EncryptionPlatformUnknown,
	}

	envs := addEncryptionAtRestEnvVars(opts)
	require.Len(t, envs, 2)

	assertSecretEnv(t, envs[0], "STORE_KEY", newSecret)
	assertSecretEnv(t, envs[1], "OLD_STORE_KEY", oldSecret)
}

func TestAddEncryptionAtRestEnvVarsOldKeyOnly(t *testing.T) {
	oldSecret := "old-secret"

	opts := &encryptionAtRestOptions{
		OldKeySecretName: &oldSecret,
		CMEKPlatform:     v1beta1.EncryptionPlatformUnknown,
	}

	envs := addEncryptionAtRestEnvVars(opts)
	require.Len(t, envs, 1)

	assertSecretEnv(t, envs[0], "OLD_STORE_KEY", oldSecret)
}

func TestAddEncryptionAtRestCrdbNodeCmdDisable(t *testing.T) {
	oldSecret := "old-secret"
	opts := &encryptionAtRestOptions{
		OldKeySecretName: &oldSecret,
		StorePath:        CockroachDataDirMountPath,
	}

	cmd := addEncryptionAtRestCrdbNodeCmd(opts)
	require.Equal(t, []string{fmt.Sprintf("--enterprise-encryption=path=%s,key=plain,old-key=%s,rotation-period=%s",
		CockroachDataDirMountPath, fmt.Sprintf("%s/%s", keyMountPath, "old_store.key"), rotationPeriod)}, cmd)
}

func TestAddEncryptionAtRestCrdbNodeCmdRotate(t *testing.T) {
	newSecret := "new-secret"
	oldSecret := "old-secret"
	opts := &encryptionAtRestOptions{
		KeySecretName:    &newSecret,
		OldKeySecretName: &oldSecret,
		StorePath:        CockroachDataDirMountPath,
	}

	cmd := addEncryptionAtRestCrdbNodeCmd(opts)
	require.Equal(t, []string{fmt.Sprintf("--enterprise-encryption=path=%s,key=%s,old-key=%s,rotation-period=%s",
		CockroachDataDirMountPath,
		fmt.Sprintf("%s/%s", keyMountPath, "new_store.key"),
		fmt.Sprintf("%s/%s", keyMountPath, "old_store.key"),
		rotationPeriod)}, cmd)
}

func TestAddEncryptionAtRestEnvVarsOldKeyWithCMEK(t *testing.T) {
	oldSecret := "old-secret"
	oldCredSecret := "old-cred-secret"

	opts := &encryptionAtRestOptions{
		OldKeySecretName:             &oldSecret,
		OldCMEKPlatform:              v1beta1.EncryptionPlatformAwsKms,
		OldCMEKCredentialsSecretName: &oldCredSecret,
	}

	envs := addEncryptionAtRestEnvVars(opts)

	// OLD_STORE_KEY + OLD_AUTH_PRINCIPAL + OLD_KEY_URI + OLD_KEY_REGION +
	// OLD_CMEK_PLATFORM + OLD_AWS_ACCESS_KEY_ID + OLD_AWS_SECRET_ACCESS_KEY +
	// OLD_AWS_EXTERNAL_ID = 8
	require.Len(t, envs, 8)
	assertSecretEnvWithKey(t, envs[0], "OLD_STORE_KEY", oldSecret, "StoreKeyData")
	assertSecretEnvWithKey(t, envs[1], "OLD_AUTH_PRINCIPAL", oldSecret, "AuthPrincipal")
	assertSecretEnvWithKey(t, envs[2], "OLD_KEY_URI", oldSecret, "URI")
	assertSecretEnvWithKey(t, envs[3], "OLD_KEY_REGION", oldSecret, "Region")
	assertSecretEnvWithKey(t, envs[4], "OLD_CMEK_PLATFORM", oldSecret, "Type")
	assertSecretEnvWithKey(t, envs[5], "OLD_AWS_ACCESS_KEY_ID", oldCredSecret, "aws_access_key_id")
	assertSecretEnvWithKey(t, envs[6], "OLD_AWS_SECRET_ACCESS_KEY", oldCredSecret, "aws_secret_access_key")
	assertSecretEnvWithKey(t, envs[7], "OLD_AWS_EXTERNAL_ID", oldSecret, "ExternalID")
}

func TestAddEncryptionAtRestEnvVarsOldKeyWithGCPCMEK(t *testing.T) {
	oldSecret := "old-secret"
	oldCredSecret := "old-cred-secret"

	opts := &encryptionAtRestOptions{
		OldKeySecretName:             &oldSecret,
		OldCMEKPlatform:              v1beta1.EncryptionPlatformGcpCloudKms,
		OldCMEKCredentialsSecretName: &oldCredSecret,
	}

	envs := addEncryptionAtRestEnvVars(opts)

	// OLD_STORE_KEY + 4 common + 1 GCP = 6
	require.Len(t, envs, 6)
	assertSecretEnvWithKey(t, envs[0], "OLD_STORE_KEY", oldSecret, "StoreKeyData")
	assertSecretEnvWithKey(t, envs[5], "OLD_GCP_SERVICE_ACCOUNT_KEY", oldCredSecret, "gcp_service_account_key")
}

func TestAddEncryptionAtRestEnvVarsRotationWithNewAndOldCMEK(t *testing.T) {
	newSecret := "new-secret"
	oldSecret := "old-secret"
	newCredSecret := "new-cred-secret"
	oldCredSecret := "old-cred-secret"

	opts := &encryptionAtRestOptions{
		KeySecretName:                &newSecret,
		CMEKPlatform:                 v1beta1.EncryptionPlatformAwsKms,
		CMEKCredentialsSecretName:    &newCredSecret,
		OldKeySecretName:             &oldSecret,
		OldCMEKPlatform:              v1beta1.EncryptionPlatformAwsKms,
		OldCMEKCredentialsSecretName: &oldCredSecret,
	}

	envs := addEncryptionAtRestEnvVars(opts)

	// New: STORE_KEY + 4 common + 3 AWS = 8
	// Old: OLD_STORE_KEY + 4 common + 3 AWS = 8
	// Total = 16
	require.Len(t, envs, 16)

	assertSecretEnvWithKey(t, envs[0], "STORE_KEY", newSecret, "StoreKeyData")
	assertSecretEnvWithKey(t, envs[8], "OLD_STORE_KEY", oldSecret, "StoreKeyData")
}

func TestAddEncryptionAtRestEnvVarsOldKeyNoCMEK(t *testing.T) {
	oldSecret := "old-secret"

	opts := &encryptionAtRestOptions{
		OldKeySecretName: &oldSecret,
		OldCMEKPlatform:  v1beta1.EncryptionPlatformUnknown,
	}

	envs := addEncryptionAtRestEnvVars(opts)

	// Only OLD_STORE_KEY, no CMEK env vars since platform is unknown.
	require.Len(t, envs, 1)
	assertSecretEnvWithKey(t, envs[0], "OLD_STORE_KEY", oldSecret, "StoreKeyData")
}

func TestAddEncryptionAtRestEnvVarsOldKeyAzure(t *testing.T) {
	oldSecret := "old-secret"

	opts := &encryptionAtRestOptions{
		OldKeySecretName: &oldSecret,
		OldCMEKPlatform:  v1beta1.EncryptionPlatformAzureKeyVault,
	}

	envs := addEncryptionAtRestEnvVars(opts)

	// OLD_STORE_KEY + 4 common CMEK vars + Azure credentials file path = 6.
	// Azure uses volume-mounted credentials, not a credentials secret, so
	// OldCMEKCredentialsSecretName is nil here.
	require.Len(t, envs, 6)
	assertSecretEnvWithKey(t, envs[0], "OLD_STORE_KEY", oldSecret, "StoreKeyData")
	assertSecretEnvWithKey(t, envs[1], "OLD_AUTH_PRINCIPAL", oldSecret, "AuthPrincipal")
	assertSecretEnvWithKey(t, envs[2], "OLD_KEY_URI", oldSecret, "URI")
	assertSecretEnvWithKey(t, envs[3], "OLD_KEY_REGION", oldSecret, "Region")
	assertSecretEnvWithKey(t, envs[4], "OLD_CMEK_PLATFORM", oldSecret, "Type")
	azureEnv := envs[5]
	require.Equal(t, "OLD_COCKROACH_AZURE_APPLICATION_CREDENTIALS_FILE", azureEnv.Name)
	require.Equal(t, OldAzureCredentialsFullPath(), azureEnv.Value)
}

func assertSecretEnv(t *testing.T, env corev1.EnvVar, name, secretName string) {
	t.Helper()
	assertSecretEnvWithKey(t, env, name, secretName, "StoreKeyData")
}

func assertSecretEnvWithKey(t *testing.T, env corev1.EnvVar, name, secretName, key string) {
	t.Helper()
	require.Equal(t, name, env.Name)
	require.NotNil(t, env.ValueFrom)
	require.NotNil(t, env.ValueFrom.SecretKeyRef)
	require.Equal(t, secretName, env.ValueFrom.SecretKeyRef.Name)
	require.Equal(t, key, env.ValueFrom.SecretKeyRef.Key)
}

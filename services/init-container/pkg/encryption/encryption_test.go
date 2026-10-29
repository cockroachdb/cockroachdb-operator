package encryption

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteNewStoreKeyMissingInfo(t *testing.T) {
	decryptedKeyMountPath := "a"
	encryptedKeyMountPath := "b"
	newStoreKeyFile := "c"

	missingInfoErrMsg := "must specify DecryptedKeyMountPath, EncryptedKeyMountPath, NewStoreKeyFile"

	tests := []struct {
		testName              string
		decryptedKeyMountPath string
		encryptedKeyMountPath string
		newStoreKeyFile       string
		errMsg                string
	}{
		{
			testName:              "missing decrypted key mount path",
			encryptedKeyMountPath: encryptedKeyMountPath,
			newStoreKeyFile:       newStoreKeyFile,
			errMsg:                missingInfoErrMsg,
		},
		{
			testName:              "missing encrypted key mount path",
			decryptedKeyMountPath: decryptedKeyMountPath,
			newStoreKeyFile:       newStoreKeyFile,
			errMsg:                missingInfoErrMsg,
		},
		{
			testName:              "missing new store key file",
			decryptedKeyMountPath: decryptedKeyMountPath,
			encryptedKeyMountPath: encryptedKeyMountPath,
			errMsg:                missingInfoErrMsg,
		},
		{
			testName:              "missing KMS client",
			decryptedKeyMountPath: decryptedKeyMountPath,
			encryptedKeyMountPath: encryptedKeyMountPath,
			newStoreKeyFile:       newStoreKeyFile,
			errMsg:                "must specify KMSClient",
		},
	}

	for _, test := range tests {
		t.Run(test.testName, func(t *testing.T) {
			eh := EncryptionHandler{
				DecryptedKeyMountPath: test.decryptedKeyMountPath,
				EncryptedKeyMountPath: test.encryptedKeyMountPath,
				NewStoreKeyFile:       test.newStoreKeyFile,
			}
			err := eh.WriteNewStoreKey(context.Background())
			assert.EqualError(t, err, test.errMsg)
		})
	}
}

func TestWriteNewStoreKeySuccess(t *testing.T) {
	newStoreKeyFile := "new_store.key"
	oldStoreKeyFile := "old_store.key"

	decryptedKeyMountPath, err := os.MkdirTemp("", "encryption-test-decrypted")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(decryptedKeyMountPath) })

	encryptedKeyMountPath, err := os.MkdirTemp("", "encryption-test-encrypted")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(encryptedKeyMountPath) })

	secret := "veryextremelysecret"
	encodedSecret := base64.StdEncoding.EncodeToString([]byte(secret))
	t.Setenv(envStoreKey, encodedSecret)
	t.Setenv(envOldStoreKey, "")
	kmsInaccessibilityMetric.Set(0)

	eh := EncryptionHandler{
		DecryptedKeyMountPath: decryptedKeyMountPath,
		EncryptedKeyMountPath: encryptedKeyMountPath,
		NewStoreKeyFile:       newStoreKeyFile,
		OldStoreKeyFile:       oldStoreKeyFile,
		KMSClient:             &NullKMSClient{},
	}

	assert.NoError(t, eh.WriteNewStoreKey(context.Background()))

	decryptedContents, err := os.ReadFile(filepath.Join(decryptedKeyMountPath, newStoreKeyFile))
	assert.NoError(t, err)
	assert.Equal(t, secret, string(decryptedContents))

	// NullKMSClient is handed the key itself rather than ciphertext, so the
	// backup would put the store key in the clear on the data volume. Nothing
	// should be written there.
	encryptedBackupsPath := filepath.Join(encryptedKeyMountPath, "encryption-store-key-backups")
	_, err = os.Stat(encryptedBackupsPath)
	assert.True(t, os.IsNotExist(err),
		"an unwrapped key must not be backed up to the data volume")

	RequireMetricEqual(t, kmsInaccessibilityMetric, 0)
}

// wrappingKMSClient decrypts like NullKMSClient but reports that it wraps the
// key, so the backup path can be exercised without a real KMS.
type wrappingKMSClient struct{ NullKMSClient }

func (c *wrappingKMSClient) WrapsKey() bool { return true }

func TestWriteNewStoreKeyWrappedKeyIsBackedUp(t *testing.T) {
	newStoreKeyFile := "new_store.key"

	decryptedKeyMountPath, err := os.MkdirTemp("", "encryption-test-decrypted")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(decryptedKeyMountPath) })

	encryptedKeyMountPath, err := os.MkdirTemp("", "encryption-test-encrypted")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(encryptedKeyMountPath) })

	encodedSecret := base64.StdEncoding.EncodeToString([]byte("veryextremelysecret"))
	t.Setenv(envStoreKey, encodedSecret)
	t.Setenv(envOldStoreKey, "")
	kmsInaccessibilityMetric.Set(0)

	eh := EncryptionHandler{
		DecryptedKeyMountPath: decryptedKeyMountPath,
		EncryptedKeyMountPath: encryptedKeyMountPath,
		NewStoreKeyFile:       newStoreKeyFile,
		OldStoreKeyFile:       "old_store.key",
		KMSClient:             &wrappingKMSClient{},
	}

	assert.NoError(t, eh.WriteNewStoreKey(context.Background()))

	// A wrapped key is ciphertext, so backing it up next to the data remains
	// useful and must keep working.
	encryptedBackupsPath := filepath.Join(encryptedKeyMountPath, "encryption-store-key-backups")
	encryptedBackups, err := os.ReadDir(encryptedBackupsPath)
	require.NoError(t, err)
	require.Equal(t, 1, len(encryptedBackups))

	encryptedContents, err := os.ReadFile(filepath.Join(encryptedBackupsPath, encryptedBackups[0].Name()))
	assert.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("encoded_encrypted_store_key\n%s", encodedSecret),
		string(encryptedContents))
}

func TestWriteNewStoreKeyFail(t *testing.T) {
	newStoreKeyFile := "new_store.key"

	decryptedKeyMountPath, err := os.MkdirTemp("", "encryption-test-decrypted")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(decryptedKeyMountPath) })

	encryptedKeyMountPath, err := os.MkdirTemp("", "encryption-test-encrypted")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(encryptedKeyMountPath) })

	encodedSecret := base64.StdEncoding.EncodeToString([]byte("veryextremelysecret"))
	t.Setenv(envStoreKey, encodedSecret)
	t.Setenv(envOldStoreKey, "")
	kmsInaccessibilityMetric.Set(0)

	eh := EncryptionHandler{
		DecryptedKeyMountPath: decryptedKeyMountPath,
		EncryptedKeyMountPath: encryptedKeyMountPath,
		NewStoreKeyFile:       newStoreKeyFile,
		OldStoreKeyFile:       "old_store.key",
		KMSClient:             &BrokenKMSClient{},
	}

	assert.Error(t, eh.WriteNewStoreKey(context.Background()))
	RequireMetricEqual(t, kmsInaccessibilityMetric, 1)
}

func TestWriteStoreKeyOldKeyOnly(t *testing.T) {
	newStoreKeyFile := "new_store.key"
	oldStoreKeyFile := "old_store.key"

	decryptedKeyMountPath, err := os.MkdirTemp("", "encryption-test-decrypted")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(decryptedKeyMountPath) })

	encryptedKeyMountPath, err := os.MkdirTemp("", "encryption-test-encrypted")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(encryptedKeyMountPath) })

	oldSecret := "superdupersecret"
	encodedOldSecret := base64.StdEncoding.EncodeToString([]byte(oldSecret))

	t.Setenv(envStoreKey, "")
	t.Setenv(envOldStoreKey, encodedOldSecret)

	eh := EncryptionHandler{
		DecryptedKeyMountPath: decryptedKeyMountPath,
		EncryptedKeyMountPath: encryptedKeyMountPath,
		NewStoreKeyFile:       newStoreKeyFile,
		OldStoreKeyFile:       oldStoreKeyFile,
		KMSClient:             &NullKMSClient{},
	}

	assert.NoError(t, eh.WriteNewStoreKey(context.Background()))

	// New store key should not have been created.
	_, err = os.Stat(filepath.Join(decryptedKeyMountPath, newStoreKeyFile))
	assert.True(t, os.IsNotExist(err))

	// Old store key must be written.
	oldContents, err := os.ReadFile(filepath.Join(decryptedKeyMountPath, oldStoreKeyFile))
	assert.NoError(t, err)
	assert.Equal(t, oldSecret, string(oldContents))

	// No backup file should be created without a new key.
	encryptedBackupsPath := filepath.Join(encryptedKeyMountPath, "encryption-store-key-backups")
	_, err = os.Stat(encryptedBackupsPath)
	assert.True(t, os.IsNotExist(err))

	RequireMetricEqual(t, kmsInaccessibilityMetric, 0)
}

func TestWriteNewStoreKeyRotation(t *testing.T) {
	newStoreKeyFile := "new_store.key"
	oldStoreKeyFile := "old_store.key"

	decryptedKeyMountPath, err := os.MkdirTemp("", "encryption-test-decrypted")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(decryptedKeyMountPath) })

	encryptedKeyMountPath, err := os.MkdirTemp("", "encryption-test-encrypted")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(encryptedKeyMountPath) })

	newSecret := "brandnewsecret"
	oldSecret := "superdupersecret"

	encodedNewSecret := base64.StdEncoding.EncodeToString([]byte(newSecret))
	encodedOldSecret := base64.StdEncoding.EncodeToString([]byte(oldSecret))

	t.Setenv(envStoreKey, encodedNewSecret)
	t.Setenv(envOldStoreKey, encodedOldSecret)

	eh := EncryptionHandler{
		DecryptedKeyMountPath: decryptedKeyMountPath,
		EncryptedKeyMountPath: encryptedKeyMountPath,
		NewStoreKeyFile:       newStoreKeyFile,
		OldStoreKeyFile:       oldStoreKeyFile,
		KMSClient:             &NullKMSClient{},
	}

	assert.NoError(t, eh.WriteNewStoreKey(context.Background()))

	newContents, err := os.ReadFile(filepath.Join(decryptedKeyMountPath, newStoreKeyFile))
	assert.NoError(t, err)
	assert.Equal(t, newSecret, string(newContents))

	oldContents, err := os.ReadFile(filepath.Join(decryptedKeyMountPath, oldStoreKeyFile))
	assert.NoError(t, err)
	assert.Equal(t, oldSecret, string(oldContents))

	// Both keys are unwrapped on this path, so neither is backed up.
	encryptedBackupsPath := filepath.Join(encryptedKeyMountPath, "encryption-store-key-backups")
	_, err = os.Stat(encryptedBackupsPath)
	assert.True(t, os.IsNotExist(err),
		"an unwrapped key must not be backed up to the data volume")

	RequireMetricEqual(t, kmsInaccessibilityMetric, 0)
}

func TestWriteOldStoreKeyWithCMEKDecryption(t *testing.T) {
	oldStoreKeyFile := "old_store.key"

	decryptedKeyMountPath, err := os.MkdirTemp("", "encryption-test-decrypted")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(decryptedKeyMountPath) })

	encryptedKeyMountPath, err := os.MkdirTemp("", "encryption-test-encrypted")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(encryptedKeyMountPath) })

	// NullKMSClient is a passthrough: "encrypted" == plaintext. This simulates
	// the old key being CMEK-encrypted and decrypted via OldKMSClient.
	oldSecret := "cmek-encrypted-old-secret"
	encodedOldSecret := base64.StdEncoding.EncodeToString([]byte(oldSecret))

	t.Setenv(envStoreKey, "")
	t.Setenv(envOldStoreKey, encodedOldSecret)

	eh := EncryptionHandler{
		DecryptedKeyMountPath: decryptedKeyMountPath,
		EncryptedKeyMountPath: encryptedKeyMountPath,
		NewStoreKeyFile:       "new_store.key",
		OldStoreKeyFile:       oldStoreKeyFile,
		KMSClient:             &NullKMSClient{},
		OldKMSClient:          &NullKMSClient{},
	}

	assert.NoError(t, eh.WriteNewStoreKey(context.Background()))

	oldContents, err := os.ReadFile(filepath.Join(decryptedKeyMountPath, oldStoreKeyFile))
	assert.NoError(t, err)
	assert.Equal(t, oldSecret, string(oldContents))

	RequireMetricEqual(t, kmsInaccessibilityMetric, 0)
}

func TestWriteOldStoreKeyWithBrokenOldKMS(t *testing.T) {
	decryptedKeyMountPath, err := os.MkdirTemp("", "encryption-test-decrypted")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(decryptedKeyMountPath) })

	encryptedKeyMountPath, err := os.MkdirTemp("", "encryption-test-encrypted")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(encryptedKeyMountPath) })

	encodedOldSecret := base64.StdEncoding.EncodeToString([]byte("some-encrypted-key"))

	t.Setenv(envStoreKey, "")
	t.Setenv(envOldStoreKey, encodedOldSecret)

	eh := EncryptionHandler{
		DecryptedKeyMountPath: decryptedKeyMountPath,
		EncryptedKeyMountPath: encryptedKeyMountPath,
		NewStoreKeyFile:       "new_store.key",
		OldStoreKeyFile:       "old_store.key",
		KMSClient:             &NullKMSClient{},
		OldKMSClient:          &BrokenKMSClient{},
	}

	err = eh.WriteNewStoreKey(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "decrypting old store key via KMS")
	RequireMetricEqual(t, kmsInaccessibilityMetric, 1)
}

func TestWriteRotationWithBothCMEKClients(t *testing.T) {
	newStoreKeyFile := "new_store.key"
	oldStoreKeyFile := "old_store.key"

	decryptedKeyMountPath, err := os.MkdirTemp("", "encryption-test-decrypted")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(decryptedKeyMountPath) })

	encryptedKeyMountPath, err := os.MkdirTemp("", "encryption-test-encrypted")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(encryptedKeyMountPath) })

	newSecret := "brand-new-cmek-key"
	oldSecret := "old-cmek-key"

	t.Setenv(envStoreKey, base64.StdEncoding.EncodeToString([]byte(newSecret)))
	t.Setenv(envOldStoreKey, base64.StdEncoding.EncodeToString([]byte(oldSecret)))

	eh := EncryptionHandler{
		DecryptedKeyMountPath: decryptedKeyMountPath,
		EncryptedKeyMountPath: encryptedKeyMountPath,
		NewStoreKeyFile:       newStoreKeyFile,
		OldStoreKeyFile:       oldStoreKeyFile,
		KMSClient:             &NullKMSClient{},
		OldKMSClient:          &NullKMSClient{},
	}

	assert.NoError(t, eh.WriteNewStoreKey(context.Background()))

	newContents, err := os.ReadFile(filepath.Join(decryptedKeyMountPath, newStoreKeyFile))
	assert.NoError(t, err)
	assert.Equal(t, newSecret, string(newContents))

	oldContents, err := os.ReadFile(filepath.Join(decryptedKeyMountPath, oldStoreKeyFile))
	assert.NoError(t, err)
	assert.Equal(t, oldSecret, string(oldContents))

	RequireMetricEqual(t, kmsInaccessibilityMetric, 0)
}

func TestWriteOldStoreKeyWithoutOldKMSClient(t *testing.T) {
	oldStoreKeyFile := "old_store.key"

	decryptedKeyMountPath, err := os.MkdirTemp("", "encryption-test-decrypted")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(decryptedKeyMountPath) })

	encryptedKeyMountPath, err := os.MkdirTemp("", "encryption-test-encrypted")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(encryptedKeyMountPath) })

	// When OldKMSClient is nil, old key should be written as raw decoded bytes
	// (non-CMEK EAR behavior).
	oldSecret := "plain-old-secret"
	t.Setenv(envStoreKey, "")
	t.Setenv(envOldStoreKey, base64.StdEncoding.EncodeToString([]byte(oldSecret)))

	eh := EncryptionHandler{
		DecryptedKeyMountPath: decryptedKeyMountPath,
		EncryptedKeyMountPath: encryptedKeyMountPath,
		NewStoreKeyFile:       "new_store.key",
		OldStoreKeyFile:       oldStoreKeyFile,
		KMSClient:             &NullKMSClient{},
		// OldKMSClient intentionally nil
	}

	assert.NoError(t, eh.WriteNewStoreKey(context.Background()))

	oldContents, err := os.ReadFile(filepath.Join(decryptedKeyMountPath, oldStoreKeyFile))
	assert.NoError(t, err)
	assert.Equal(t, oldSecret, string(oldContents))
}

func Test_getPushgatewayHostname(t *testing.T) {
	os.Setenv(envPushgatewayNamespace, "us-west2")
	assert.Equal(t, "pushgateway.us-west2.svc.cluster.local:9091", getPushgatewayHostname())
}

// RequireMetricEqual checks if a prometheus.Collector storing exactly
// one Metric is equal to count. See testutil.ToFloat64 for more.
func RequireMetricEqual(t *testing.T, metric prometheus.Collector, count int) {
	t.Helper()
	require.Equal(t, count, int(testutil.ToFloat64(metric)))
}

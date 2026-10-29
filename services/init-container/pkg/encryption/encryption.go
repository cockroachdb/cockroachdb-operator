package encryption

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/gofrs/uuid"
)

const (
	envStoreKey    = "STORE_KEY"
	envOldStoreKey = "OLD_STORE_KEY"
)

type EncryptionHandler struct {
	DecryptedKeyMountPath string
	EncryptedKeyMountPath string
	NewStoreKeyFile       string
	OldStoreKeyFile       string
	KMSClient             KMSClient
	// OldKMSClient is used to decrypt the old store key when it was encrypted
	// with a different KMS than the current one (e.g. during provider switch
	// or EAR disable). If nil, the old key is base64-decoded without KMS
	// decryption, which is correct for non-CMEK EAR.
	OldKMSClient KMSClient
}

// WriteNewStoreKey performs decryption of the encrypted store key,
// writes the decrypted store key to an ephemeral volume, and writes the
// encrypted store key (and associated info) to the persistent volume.
func (e *EncryptionHandler) WriteNewStoreKey(ctx context.Context) error {
	if e.DecryptedKeyMountPath == "" || e.EncryptedKeyMountPath == "" || e.NewStoreKeyFile == "" {
		return errors.New("must specify DecryptedKeyMountPath, EncryptedKeyMountPath, NewStoreKeyFile")
	}

	if e.KMSClient == nil {
		return errors.New("must specify KMSClient")
	}

	encodedEncryptedStoreKey := os.Getenv(envStoreKey)
	encodedOldStoreKey := os.Getenv(envOldStoreKey)
	hasNewStoreKey := encodedEncryptedStoreKey != ""
	hasOldStoreKey := encodedOldStoreKey != ""

	if !hasNewStoreKey && !hasOldStoreKey {
		return errors.New("attempted to fetch store key but secrets are empty")
	}

	gaugeHandler, err := newGaugeHandler(kmsInaccessibilityMetric)
	if err != nil {
		return err
	}

	if hasOldStoreKey {
		if e.OldStoreKeyFile == "" {
			return errors.New("must specify OldStoreKeyFile when OLD_STORE_KEY is provided")
		}
		decodedOldStoreKey, err := base64.StdEncoding.DecodeString(encodedOldStoreKey)
		if err != nil {
			return errors.Wrap(err, "decoding base 64 old key")
		}
		// If an OldKMSClient is provided, the old key is CMEK-encrypted and
		// needs KMS decryption. Without one the decoded bytes are the raw key.
		if e.OldKMSClient != nil {
			decodedOldStoreKey, err = e.OldKMSClient.Decrypt(decodedOldStoreKey)
			if err != nil {
				kmsInaccessibilityMetric.Set(1)
				if pushErr := gaugeHandler.pushMetrics(); pushErr != nil {
					log.Printf("%+v\n", pushErr)
				}
				return errors.Wrap(err, "decrypting old store key via KMS")
			}
		}
		if err := os.WriteFile(filepath.Join(e.DecryptedKeyMountPath, e.OldStoreKeyFile), decodedOldStoreKey, 0644); err != nil {
			return errors.Wrap(err, "error writing old store key to ephemeral volume")
		}
	}

	if !hasNewStoreKey {
		kmsInaccessibilityMetric.Set(0)
		if pushErr := gaugeHandler.pushMetrics(); pushErr != nil {
			log.Printf("%+v\n", pushErr)
		}
		return nil
	}

	// The encrypted store key is provided to us in base64 encoding so
	// that it doesn't arrive as gibberish. We decode it to bytes here.
	encryptedStoreKey, err := base64.StdEncoding.DecodeString(encodedEncryptedStoreKey)
	if err != nil {
		return errors.Wrap(err, "decoding base 64 key")
	}

	decryptedStoreKey, decryptErr := e.KMSClient.Decrypt(encryptedStoreKey)
	if decryptErr != nil {
		kmsInaccessibilityMetric.Set(1)
	} else {
		kmsInaccessibilityMetric.Set(0)
	}
	if pushErr := gaugeHandler.pushMetrics(); pushErr != nil {
		log.Printf("%+v\n", pushErr)
	}

	if decryptErr != nil {
		return errors.Wrap(decryptErr, "decrypting store key")
	}

	// Write the decrypted store key to the ephemeral volume in memory.
	if err := os.WriteFile(filepath.Join(e.DecryptedKeyMountPath, e.NewStoreKeyFile), decryptedStoreKey, 0644); err != nil {
		return errors.Wrap(err, "error writing decrypted store key to ephemeral volume")
	}

	// Only a client that wraps the key has something safe to back up. When the
	// stored value is the key itself, as it is for a basalt-backed cluster with a
	// CRL-managed encryption key, this file would put the store key in the clear
	// on the volume holding the cluster's data, which defeats keeping the
	// decrypted copy in memory. Nothing is lost by skipping it, since the key's
	// source of truth lives outside the cluster.
	if !e.KMSClient.WrapsKey() {
		return nil
	}

	backupHeader, backupInfo, err := e.KMSClient.GetMetadataCSV()
	if err != nil {
		return errors.Wrap(err, "retrieving KMS client metadata")
	}
	backupHeader = append(backupHeader, "encoded_encrypted_store_key")
	backupInfo = append(backupInfo, encodedEncryptedStoreKey)

	// Write the encrypted store key (and info needed to decrypt it) to
	// the persistent volume.
	uniqueID, err := uuid.NewV4()
	if err != nil {
		return errors.Wrap(err, "error generating uuid")
	}
	backupFileName := fmt.Sprintf("backup-%d-%s", time.Now().UnixNano(), uniqueID.String())
	backupFilePath := filepath.Join(e.EncryptedKeyMountPath, "encryption-store-key-backups")
	if err := os.MkdirAll(backupFilePath, 0777); err != nil {
		return errors.Wrap(err, "error creating backup dir")
	}
	backupFileContents := fmt.Appendf(nil, "%s\n%s", strings.Join(backupHeader, ","), strings.Join(backupInfo, ","))
	if err := os.WriteFile(filepath.Join(backupFilePath, backupFileName), backupFileContents, 0644); err != nil {
		return errors.Wrap(err, "error writing backup file")
	}

	return nil
}

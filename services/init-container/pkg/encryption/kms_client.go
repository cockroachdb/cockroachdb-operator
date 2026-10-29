package encryption

import (
	"context"
	"encoding/base64"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/url"
	"strings"
	"time"

	gcpkms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	awskms "github.com/aws/aws-sdk-go/service/kms"
	"github.com/cockroachdb/errors"
	crlaws "github.com/cockroachlabs/cockroach-enterprise-operator/services/init-container/pkg/aws"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// err*BrokenKMS are errors that are to be used with their BrokenKMSClient
// method counterparts.
var ErrEncryptionBrokenKMS = errors.New("encryption failed")
var ErrDecryptionBrokenKMS = errors.New("decryption failed")
var ErrMetadataBrokenKMS = errors.New("metadata failed")

type KMSClient interface {
	// Encrypt takes plaintext and returns encrypted ciphertext.
	Encrypt([]byte) ([]byte, error)
	// Decrypt takes ciphertext and returns decrypted plaintext.
	Decrypt([]byte) ([]byte, error)

	// GetMetadataCSV returns all the information that the KMS client
	// needs to authenticate and access its specific KMS, for the sake of
	// keeping a backup or in case a human needs to see the details of
	// the KMS client in a friendly format. The first slice returned is a
	// header containing field names, and the second string returned
	// contains the actual corresponding metadata.
	GetMetadataCSV() ([]string, []string, error)

	// WrapsKey reports whether this client envelope-encrypts the store key.
	// A client that does not is handed the key itself, so the stored value is
	// plaintext and must not be written anywhere but the in-memory key volume.
	WrapsKey() bool
}

type NullKMSClient struct{}

func (c *NullKMSClient) Encrypt(plaintext []byte) ([]byte, error) {
	return plaintext, nil
}

func (c *NullKMSClient) Decrypt(ciphertext []byte) ([]byte, error) {
	return ciphertext, nil
}

func (c *NullKMSClient) GetMetadataCSV() ([]string, []string, error) {
	return nil, nil, nil
}

// WrapsKey is false because Decrypt is a passthrough. The value we are handed
// is the store key itself, not ciphertext.
func (c *NullKMSClient) WrapsKey() bool {
	return false
}

type BrokenKMSClient struct{}

func (c *BrokenKMSClient) Encrypt(plaintext []byte) ([]byte, error) {
	return nil, ErrEncryptionBrokenKMS
}

func (c *BrokenKMSClient) Decrypt(ciphertext []byte) ([]byte, error) {
	return nil, ErrDecryptionBrokenKMS
}

func (c *BrokenKMSClient) GetMetadataCSV() ([]string, []string, error) {
	return nil, nil, ErrMetadataBrokenKMS
}

// WrapsKey is true so that this client stands in for a real KMS that is
// failing, rather than for one that does not wrap at all.
func (c *BrokenKMSClient) WrapsKey() bool {
	return true
}

type errMissingInfo struct {
	missingParam string
}

func (e errMissingInfo) Error() string {
	return fmt.Sprintf("must provide %s", e.missingParam)
}

type CloudKMSClient struct {
	AuthPrincipal string
	KeyURI        string
	KeyRegion     string
}

// This is NOT threadsafe. If you want to make it threadsafe, add a
// mutex around usage of the kmsClient struct member.
type GCPKMSClient struct {
	CloudKMSClient

	// EncodedServiceAccountKey is the JSON blob used to authenticate into our
	// GCP service account.
	EncodedServiceAccountKey string

	kmsClient *gcpkms.KeyManagementClient
}

func (c *GCPKMSClient) authenticate() error {
	if c.EncodedServiceAccountKey == "" {
		return errMissingInfo{"service account key"}
	}
	if c.AuthPrincipal == "" {
		return errMissingInfo{"auth principal"}
	}
	if c.KeyURI == "" {
		return errMissingInfo{"key URI"}
	}

	decodedServiceAccountKey, err := base64.StdEncoding.DecodeString(c.EncodedServiceAccountKey)
	if err != nil {
		return errors.Wrap(err, "could not decode service account key")
	}

	// Using our GCP service account's credentials, obtain short-lived
	// credentials to the service account provided, specifically
	// requesting KMS permissions.
	ts, err := impersonate.CredentialsTokenSource(context.Background(), impersonate.CredentialsConfig{
		TargetPrincipal: c.AuthPrincipal,
		Scopes:          []string{"https://www.googleapis.com/auth/cloudkms"},
	}, option.WithCredentialsJSON(decodedServiceAccountKey))
	if err != nil {
		return errors.Wrap(err, "could not impersonate service account")
	}

	// Initialize a KMS client using the short-lived credentials obtained
	// above.
	kmsClient, err := gcpkms.NewKeyManagementClient(
		context.Background(),
		option.WithTokenSource(ts),
	)
	if err != nil {
		return errors.Wrap(err, "could not initialize KMS client")
	}

	c.kmsClient = kmsClient
	return nil
}

func (c *GCPKMSClient) Encrypt(plaintext []byte) ([]byte, error) {
	// Lazily authenticate with GCP
	if c.kmsClient == nil {
		err := c.authenticate()
		if err != nil {
			return nil, err
		}
	}

	// Compute plaintext checksum, as recommended by Google.
	crc32c := func(data []byte) uint32 {
		return crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
	}
	plaintextCRC32C := crc32c(plaintext)

	req := &kmspb.EncryptRequest{
		Name:            c.KeyURI,
		Plaintext:       plaintext,
		PlaintextCrc32C: wrapperspb.Int64(int64(plaintextCRC32C)),
	}

	result, err := c.kmsClient.Encrypt(context.Background(), req)
	if err != nil {
		return nil, err
	}

	// Perform integrity verification on result, as recommended by Google.
	if !result.VerifiedPlaintextCrc32C {
		return nil, errors.New("encrypt: request corrupted in-transit")
	}
	if int64(crc32c(result.Ciphertext)) != result.CiphertextCrc32C.Value {
		return nil, errors.New("encrypt: response corrupted in-transit")
	}

	return result.Ciphertext, nil
}

func (c *GCPKMSClient) Decrypt(ciphertext []byte) ([]byte, error) {
	// Lazily authenticate with GCP
	if c.kmsClient == nil {
		err := c.authenticate()
		if err != nil {
			return nil, err
		}
	}

	// Compute plaintext checksum, as recommended by Google.
	crc32c := func(data []byte) uint32 {
		return crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
	}
	ciphertextCRC32C := crc32c(ciphertext)

	req := &kmspb.DecryptRequest{
		Name:             c.KeyURI,
		Ciphertext:       ciphertext,
		CiphertextCrc32C: wrapperspb.Int64(int64(ciphertextCRC32C)),
	}

	result, err := c.kmsClient.Decrypt(context.Background(), req)
	if err != nil {
		return nil, err
	}

	// Perform integrity verification on result, as recommended by Google.
	if int64(crc32c(result.Plaintext)) != result.PlaintextCrc32C.Value {
		return nil, errors.New("decrypt: response corrupted in-transit")
	}

	return result.Plaintext, nil
}

func (c *GCPKMSClient) WrapsKey() bool {
	return true
}

func (c *GCPKMSClient) GetMetadataCSV() ([]string, []string, error) {
	// Lazily authenticate with GCP. We do this in this func because if
	// we cannot authenticate with GCP, then there is no point in
	// returning non-working metadata.
	if c.kmsClient == nil {
		err := c.authenticate()
		if err != nil {
			return nil, nil, err
		}
	}
	return nil, nil, nil
}

// This is NOT threadsafe. If you want to make it threadsafe, add a
// mutex around usage of the kmsClient struct member.
type AWSKMSClient struct {
	CloudKMSClient

	AccessKeyID     string
	SecretAccessKey string
	ExternalID      string

	kmsClient *awskms.KMS
}

func (c *AWSKMSClient) authenticate() error {
	if c.AccessKeyID == "" {
		return errMissingInfo{"access key ID"}
	}
	if c.SecretAccessKey == "" {
		return errMissingInfo{"secret access key"}
	}
	if c.ExternalID == "" {
		return errMissingInfo{"external ID"}
	}
	if c.AuthPrincipal == "" {
		return errMissingInfo{"auth principal"}
	}
	if c.KeyURI == "" {
		return errMissingInfo{"key URI"}
	}
	if c.KeyRegion == "" {
		return errMissingInfo{"key region"}
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:    10,
			IdleConnTimeout: 10 * time.Second,
		},
		Timeout: 300 * time.Second,
	}

	roleCredentials, err := crlaws.AssumeRole(httpClient, c.AccessKeyID, c.SecretAccessKey, c.ExternalID, c.AuthPrincipal)
	if err != nil {
		return errors.Wrap(err, "assuming customer KMS role")
	}

	if roleCredentials.AccessKeyId == nil || roleCredentials.SecretAccessKey == nil || roleCredentials.SessionToken == nil {
		return errors.New("output of KMS AssumeRole call is missing information")
	}

	// Start another session, this time with the credentials of the
	// customer's KMS role. This allows us to access their KMS and perform
	// a decrypt operation.
	roleSession, err := session.NewSession(&aws.Config{
		Region: &c.KeyRegion,
		Credentials: credentials.NewStaticCredentials(
			*roleCredentials.AccessKeyId,
			*roleCredentials.SecretAccessKey,
			*roleCredentials.SessionToken,
		),
		HTTPClient: httpClient,
	})
	if err != nil {
		return errors.Wrap(err, "error starting role session")
	}

	c.kmsClient = awskms.New(roleSession)

	return nil
}

func (c *AWSKMSClient) Encrypt(plaintext []byte) ([]byte, error) {
	// Lazily authenticate with AWS
	if c.kmsClient == nil {
		err := c.authenticate()
		if err != nil {
			return nil, err
		}
	}

	encryptInput := &awskms.EncryptInput{
		KeyId:     &c.KeyURI,
		Plaintext: plaintext,
	}
	encryptOutput, err := c.kmsClient.Encrypt(encryptInput)
	if err != nil {
		return nil, err
	}
	return encryptOutput.CiphertextBlob, nil
}

func (c *AWSKMSClient) Decrypt(ciphertext []byte) ([]byte, error) {
	// Lazily authenticate with AWS
	if c.kmsClient == nil {
		err := c.authenticate()
		if err != nil {
			return nil, err
		}
	}

	decryptInput := &awskms.DecryptInput{
		KeyId:          &c.KeyURI,
		CiphertextBlob: ciphertext,
	}
	decryptOutput, err := c.kmsClient.Decrypt(decryptInput)
	if err != nil {
		return nil, err
	}
	return decryptOutput.Plaintext, nil
}

func (c *AWSKMSClient) WrapsKey() bool {
	return true
}

func (c *AWSKMSClient) GetMetadataCSV() ([]string, []string, error) {
	// Lazily authenticate with AWS. We do this in this func because if
	// we cannot authenticate with AWS, then there is no point in
	// returning non-working metadata.
	if c.kmsClient == nil {
		err := c.authenticate()
		if err != nil {
			return nil, nil, err
		}
	}

	backupHeader := []string{
		"aws_access_key_id",
		"aws_secret_access_key",
		"aws_external_id",
		"auth_principal",
		"key_uri",
		"key_region",
	}
	backupInfo := []string{
		c.AccessKeyID,
		c.SecretAccessKey,
		c.ExternalID,
		c.AuthPrincipal,
		c.KeyURI,
		c.KeyRegion,
	}
	return backupHeader, backupInfo, nil
}

type AzureKMSClient struct {
	CloudKMSClient

	ClientSecret string
	ClientID     string

	kmsClient *azkeys.Client
}

func getAzureKeyVaultURL(keyURI string) (string, error) {
	parsedURL, err := url.Parse(keyURI)
	if err != nil {
		return "", errors.Wrap(err, "invalid key URI")
	}

	// Reconstruct the vault URL
	vaultURL := fmt.Sprintf("%s://%s", parsedURL.Scheme, parsedURL.Host)
	return vaultURL, nil
}

func getAzureKeyNameAndVersion(keyURI string) (string, string, error) {
	parsedURL, err := url.Parse(keyURI)
	if err != nil {
		return "", "", errors.Wrap(err, "invalid key URI")
	}

	// Expected path format: /keys/<keyName>/<version>
	segments := strings.Split(strings.Trim(parsedURL.Path, "/"), "/")
	if len(segments) < 3 || segments[0] != "keys" {
		return "", "", errors.New("invalid key URI format")
	}

	keyName := segments[1]
	version := segments[2]
	return keyName, version, nil
}

func (c *AzureKMSClient) authenticate() error {
	if c.ClientID == "" {
		return errMissingInfo{"client ID"}
	}
	if c.ClientSecret == "" {
		return errMissingInfo{"client secret"}
	}
	if c.AuthPrincipal == "" {
		return errMissingInfo{"auth principal"}
	}
	if c.KeyURI == "" {
		return errMissingInfo{"key URI"}
	}

	// Authenticate using Service Principal
	cred, err := azidentity.NewClientSecretCredential(c.AuthPrincipal /*tenantID*/, c.ClientID, c.ClientSecret, nil)
	if err != nil {
		return errors.Wrap(err, "could not impersonate service principal")
	}

	keyVaultURL, err := getAzureKeyVaultURL(c.KeyURI)
	if err != nil {
		return errors.Wrap(err, "invalid key vault URL")
	}

	// Create Key Client
	kmsClient, err := azkeys.NewClient(keyVaultURL, cred, nil)
	if err != nil {
		return errors.Wrap(err, "could not initialize KMS client")
	}

	c.kmsClient = kmsClient
	return nil

}

func (c *AzureKMSClient) Encrypt(plaintext []byte) ([]byte, error) {
	// Lazily authenticate with Azure
	if c.kmsClient == nil {
		err := c.authenticate()
		if err != nil {
			return nil, err
		}
	}

	return azureEncrypt(plaintext, c.KeyURI, c.kmsClient)
}

func (c *AzureKMSClient) Decrypt(ciphertext []byte) ([]byte, error) {
	// Lazily authenticate with Azure
	if c.kmsClient == nil {
		err := c.authenticate()
		if err != nil {
			return nil, err
		}
	}

	return azureDecrypt(ciphertext, c.KeyURI, c.kmsClient)
}

func (c *AzureKMSClient) WrapsKey() bool {
	return true
}

func (c *AzureKMSClient) GetMetadataCSV() ([]string, []string, error) {
	// Lazily authenticate with Azure. We do this in this func because if
	// we cannot authenticate with Azure, then there is no point in
	// returning non-working metadata.
	if c.kmsClient == nil {
		err := c.authenticate()
		if err != nil {
			return nil, nil, err
		}
	}

	backupHeader := []string{
		"client_id",
		"client_secret",
		"auth_principal",
		"key_uri",
		"key_region",
	}
	backupInfo := []string{
		c.ClientID,
		c.ClientSecret,
		c.AuthPrincipal,
		c.KeyURI,
		c.KeyRegion,
	}

	return backupHeader, backupInfo, nil
}

// AzureKMSWorkloadIdentityClient is a KMS client that uses
// Azure Workload Identity, currently used only within the init-container.
type AzureKMSWorkloadIdentityClient struct {
	CloudKMSClient

	// Required for authenticating cross tenant. Since the
	// service principal of the cluster identity is present
	// in the customer's account.
	ClusterIdentityClientID string

	kmsClient *azkeys.Client
}

func (c *AzureKMSWorkloadIdentityClient) authenticate() error {
	// --- Validation ---
	if c.AuthPrincipal == "" {
		return errMissingInfo{"auth principal"}
	}
	if c.KeyURI == "" {
		return errMissingInfo{"key URI"}
	}
	if c.ClusterIdentityClientID == "" {
		return errMissingInfo{"cluster identity client ID"}
	}

	keyVaultURL, err := getAzureKeyVaultURL(c.KeyURI)
	if err != nil {
		return errors.Wrap(err, "invalid key vault URL")
	}

	// --- Authentication ---
	// Create a new Workload Identity credential.
	// This credential uses environment variables set by the Azure
	// Workload Identity webhook to authenticate with Azure Active Directory.
	// The underlying mechanism involves exchanging a Kubernetes service
	// account token for an Azure AD access token.
	// The `AdditionallyAllowedTenants` option allows
	// acquiring tokens for any tenant.
	cred, err := azidentity.NewWorkloadIdentityCredential(
		&azidentity.WorkloadIdentityCredentialOptions{
			ClientID:                   c.ClusterIdentityClientID,
			AdditionallyAllowedTenants: []string{"*"},
		})
	if err != nil {
		return errors.Wrap(err, "could not impersonate using workload identity.")
	}

	// --- Client Creation ---
	// Create a client to interact with the Azure Key Vault keys.
	kmsClient, err := azkeys.NewClient(keyVaultURL, cred, nil)
	if err != nil {
		return errors.Wrap(err, "could not initialize KMS client")
	}

	c.kmsClient = kmsClient
	return nil
}

func azureEncrypt(plaintext []byte, keyURI string, kmsClient *azkeys.Client) ([]byte, error) {
	keyName, keyVersion, err := getAzureKeyNameAndVersion(keyURI)
	if err != nil {
		return nil, err
	}

	// RSAOEAP256 is the recommended algorithm for Azure Key Vault
	// ref: https://learn.microsoft.com/en-us/azure/key-vault/keys/about-keys-details
	alg := azkeys.EncryptionAlgorithmRSAOAEP256
	encryptInput := azkeys.KeyOperationParameters{
		Algorithm: &alg,
		Value:     plaintext,
	}

	encryptOutput, err := kmsClient.Encrypt(context.Background(), keyName, keyVersion, encryptInput, nil)
	if err != nil {
		return nil, err
	}
	return encryptOutput.Result, nil
}

func (c *AzureKMSWorkloadIdentityClient) Encrypt(plaintext []byte) ([]byte, error) {
	// Lazily authenticate with Azure
	if c.kmsClient == nil {
		err := c.authenticate()
		if err != nil {
			return nil, err
		}
	}

	return azureEncrypt(plaintext, c.KeyURI, c.kmsClient)
}

func azureDecrypt(ciphertext []byte, keyURI string, kmsClient *azkeys.Client) ([]byte, error) {
	keyName, keyVersion, err := getAzureKeyNameAndVersion(keyURI)
	if err != nil {
		return nil, err
	}

	// RSAOEAP256 is the recommended algorithm for Azure Key Vault
	// ref: https://learn.microsoft.com/en-us/azure/key-vault/keys/about-keys-details
	alg := azkeys.EncryptionAlgorithmRSAOAEP256
	decryptInput := azkeys.KeyOperationParameters{
		Algorithm: &alg,
		Value:     ciphertext,
	}

	decryptOutput, err := kmsClient.Decrypt(context.Background(), keyName, keyVersion, decryptInput, nil)
	if err != nil {
		return nil, err
	}
	return decryptOutput.Result, nil
}

func (c *AzureKMSWorkloadIdentityClient) Decrypt(ciphertext []byte) ([]byte, error) {
	// Lazily authenticate with Azure
	if c.kmsClient == nil {
		err := c.authenticate()
		if err != nil {
			return nil, err
		}
	}

	return azureDecrypt(ciphertext, c.KeyURI, c.kmsClient)
}

func (c *AzureKMSWorkloadIdentityClient) WrapsKey() bool {
	return true
}

func (c *AzureKMSWorkloadIdentityClient) GetMetadataCSV() ([]string, []string, error) {
	// Lazily authenticate with Azure. We do this in this func because if
	// we cannot authenticate with Azure, then there is no point in
	// returning non-working metadata.
	if c.kmsClient == nil {
		err := c.authenticate()
		if err != nil {
			return nil, nil, err
		}
	}

	backupHeader := []string{
		"auth_principal",
		"key_uri",
		"key_region",
	}
	backupInfo := []string{
		c.AuthPrincipal,
		c.KeyURI,
		c.KeyRegion,
	}

	return backupHeader, backupInfo, nil
}

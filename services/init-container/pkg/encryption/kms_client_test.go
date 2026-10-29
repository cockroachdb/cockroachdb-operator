package encryption

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGCPKMSClient(t *testing.T) {
	gcpKMSClient := &GCPKMSClient{}
	fakeKey := []byte("opensesame")

	// Set one required field at a time, until everything necessary is
	// there.
	_, err := gcpKMSClient.Encrypt(fakeKey)
	assert.EqualError(t, err, "must provide service account key")

	gcpKMSClient.EncodedServiceAccountKey = "day to night to morning, keep with me in the moment"
	_, err = gcpKMSClient.Encrypt(fakeKey)
	assert.EqualError(t, err, "must provide auth principal")

	gcpKMSClient.AuthPrincipal = "i'd let you had i known it"
	_, err = gcpKMSClient.Encrypt(fakeKey)
	assert.EqualError(t, err, "must provide key URI")

	// At this point, all necessary information has been provided but it's
	// invalid
	gcpKMSClient.KeyURI = "why don't you say so?"
	_, err = gcpKMSClient.Encrypt(fakeKey)
	// TODO: when we upgrade to stretchr/testify 1.7.1, use ErrorContains
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not decode service account key")
}

func TestAWSKMSClient(t *testing.T) {
	awsKMSClient := &AWSKMSClient{}
	fakeKey := []byte("opensesame")

	// Set one required field at a time, until everything necessary is
	// there.
	_, err := awsKMSClient.Encrypt(fakeKey)
	assert.EqualError(t, err, "must provide access key ID")

	awsKMSClient.AccessKeyID = "abc"
	_, err = awsKMSClient.Encrypt(fakeKey)
	assert.EqualError(t, err, "must provide secret access key")

	awsKMSClient.SecretAccessKey = "def"
	_, err = awsKMSClient.Encrypt(fakeKey)
	assert.EqualError(t, err, "must provide external ID")

	awsKMSClient.ExternalID = "ghi"
	_, err = awsKMSClient.Encrypt(fakeKey)
	assert.EqualError(t, err, "must provide auth principal")

	awsKMSClient.AuthPrincipal = "jkl"
	_, err = awsKMSClient.Encrypt(fakeKey)
	assert.EqualError(t, err, "must provide key URI")

	awsKMSClient.KeyURI = "mno"
	_, err = awsKMSClient.Encrypt(fakeKey)
	assert.EqualError(t, err, "must provide key region")

	// At this point, all necessary information has been provided but it's
	// invalid
	awsKMSClient.KeyRegion = "pqr"
	_, err = awsKMSClient.Encrypt(fakeKey)
	// TODO: when we upgrade to stretchr/testify 1.7.1, use ErrorContains
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error assuming KMS role")
}

func TestAzureKMSClient(t *testing.T) {
	azureKMSClient := &AzureKMSClient{}
	fakeKey := []byte("opensesame")
	cipherText := []byte("cipher")

	testAllFunc := func(errorString string) {
		_, err := azureKMSClient.Encrypt(fakeKey)
		assert.EqualError(t, err, errorString)
		_, err = azureKMSClient.Decrypt(cipherText)
		assert.EqualError(t, err, errorString)
		_, _, err = azureKMSClient.GetMetadataCSV()
		assert.EqualError(t, err, errorString)
	}

	// Set one required field at a time, until everything necessary is
	// there.
	testAllFunc("must provide client ID")

	azureKMSClient.ClientID = "abc"
	testAllFunc("must provide client secret")

	azureKMSClient.ClientSecret = "def"
	testAllFunc("must provide auth principal")

	azureKMSClient.AuthPrincipal = "ghi"
	testAllFunc("must provide key URI")

	azureKMSClient.KeyURI = "jkl"
	_, err := azureKMSClient.Encrypt(fakeKey)
	assert.EqualError(t, err, "invalid key URI format")
	_, err = azureKMSClient.Decrypt(cipherText)
	assert.EqualError(t, err, "invalid key URI format")
}

func TestGetKeyVaultURL(t *testing.T) {
	tests := []struct {
		name    string
		keyURI  string
		wantURL string
		wantErr bool
	}{
		{
			name:    "Valid Key URI",
			keyURI:  "https://myvault.vault.azure.net/keys/mykey/123456",
			wantURL: "https://myvault.vault.azure.net",
			wantErr: false,
		},
		{
			name:    "Invalid Key URI",
			keyURI:  "://myvault.vault.azure.net/keys/mykey/123456",
			wantURL: "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &AzureKMSClient{
				CloudKMSClient: CloudKMSClient{
					KeyURI: tt.keyURI,
				},
			}
			gotURL, err := getAzureKeyVaultURL(client.KeyURI)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.wantURL, gotURL)
			}
		})
	}
}

func TestGetKeyNameAndVersion(t *testing.T) {
	tests := []struct {
		name     string
		keyURI   string
		wantName string
		wantVer  string
		wantErr  bool
	}{
		{
			name:     "Valid Key URI",
			keyURI:   "https://myvault.vault.azure.net/keys/mykey/123456",
			wantName: "mykey",
			wantVer:  "123456",
			wantErr:  false,
		},
		{
			name:     "Invalid Key URI - Missing Version",
			keyURI:   "https://myvault.vault.azure.net/keys/mykey/",
			wantName: "",
			wantVer:  "",
			wantErr:  true,
		},
		{
			name:     "Invalid Key URI - Incorrect Format",
			keyURI:   "https://myvault.vault.azure.net/secrets/mykey/123456",
			wantName: "",
			wantVer:  "",
			wantErr:  true,
		},
		{
			name:     "Invalid Key URI - No Key",
			keyURI:   "https://myvault.vault.azure.net/keys/",
			wantName: "",
			wantVer:  "",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &AzureKMSClient{
				CloudKMSClient: CloudKMSClient{
					KeyURI: tt.keyURI,
				},
			}
			gotName, gotVer, err := getAzureKeyNameAndVersion(client.KeyURI)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.wantName, gotName)
				assert.Equal(t, tt.wantVer, gotVer)
			}
		})
	}
}

func TestAzureKMSWorkloadIdentityClient(t *testing.T) {
	azureKMSClient := &AzureKMSWorkloadIdentityClient{}
	fakeKey := []byte("opensesame")
	cipherText := []byte("cipher")

	testAllFunc := func(errorString string) {
		_, err := azureKMSClient.Encrypt(fakeKey)
		assert.EqualError(t, err, errorString)
		_, err = azureKMSClient.Decrypt(cipherText)
		assert.EqualError(t, err, errorString)
		_, _, err = azureKMSClient.GetMetadataCSV()
		assert.EqualError(t, err, errorString)
	}

	// Set one required field at a time, until everything necessary is
	// there.
	testAllFunc("must provide auth principal")

	azureKMSClient.AuthPrincipal = "ghi"
	testAllFunc("must provide key URI")

	azureKMSClient.KeyURI = "jkl"
	testAllFunc("must provide cluster identity client ID")

	azureKMSClient.ClusterIdentityClientID = "xyz"
	_, err := azureKMSClient.Encrypt(fakeKey)
	assert.Error(t, err)
	_, err = azureKMSClient.Decrypt(cipherText)
	assert.Error(t, err)
}

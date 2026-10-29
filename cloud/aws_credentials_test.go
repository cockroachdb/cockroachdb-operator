package cloud

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/stretchr/testify/require"
)

func TestCredentialProvider_GetAWSCreds(t *testing.T) {
	ctx := context.Background()

	t.Run("unsupported", func(t *testing.T) {
		p := &credentialProvider{}
		c, err := p.GetAWSCreds(ctx, "test-role-arn", "test-external-id")
		require.EqualError(t, err, "GetAWSCreds is not supported")
		require.Nil(t, c)
	})

	t.Run("successful", func(t *testing.T) {
		const (
			testRoleArn    = "test-role-arn"
			testExternalId = "test-external-id"
		)
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		stub := &stubSTS{}
		p := &credentialProvider{}
		p.aws.client = stub
		p.aws.credDuration = 60 * time.Minute
		p.aws.credExpiryWindow = 15 * time.Minute
		p.aws.mu.creds = make(map[awsCacheKey]*credentials.Credentials)

		validateCred := func(t *testing.T, c *AWSCredentials, expExpiry time.Time) {
			t.Helper()
			require.Equal(t, c.AccessKeyID, testRoleArn)
			require.Equal(t, c.SecretAccessKey, "assumedSecretAccessKey")
			require.Equal(t, c.SessionToken, "assumedSessionToken")
			require.Equal(t, expExpiry.Add(-p.aws.credExpiryWindow), c.ExpiresAt())
		}

		retrievedCount := 0
		stub.testInput = func(actx aws.Context, input *sts.AssumeRoleInput) time.Time {
			retrievedCount++
			require.Equal(t, aws.Int64(3600), input.DurationSeconds)
			require.Equal(t, aws.String(testRoleArn), input.RoleArn)
			require.Equal(t, aws.String(testExternalId), input.ExternalId)
			return testExp
		}

		// Not in cache.
		c, err := p.GetAWSCreds(ctx, testRoleArn, testExternalId)
		require.NoError(t, err)
		validateCred(t, c, testExp)
		require.Equal(t, 1, retrievedCount)
		require.Len(t, p.aws.mu.creds, 1)

		cred, ok := p.aws.mu.creds[awsCacheKey{roleARN: testRoleArn, externalID: testExternalId}]
		require.True(t, ok)

		// In cache, and expired, so increment retrievedCount.
		c, err = p.GetAWSCreds(ctx, testRoleArn, testExternalId)
		require.NoError(t, err)
		validateCred(t, c, testExp)
		require.Equal(t, 2, retrievedCount)
		require.Len(t, p.aws.mu.creds, 1)

		// Ensure that we re-use the object from cache.
		credNew, ok := p.aws.mu.creds[awsCacheKey{roleARN: testRoleArn, externalID: testExternalId}]
		require.True(t, ok)
		require.Equal(t, cred, credNew)

		retrievedCount = 0
		stub.testInput = func(actx aws.Context, input *sts.AssumeRoleInput) time.Time {
			retrievedCount++
			// require.Equal(t, ctx, actx.Context)
			require.Equal(t, aws.Int64(3600), input.DurationSeconds)
			require.Equal(t, aws.String(testRoleArn), input.RoleArn)
			require.Nil(t, input.ExternalId)
			return testExpFuture
		}

		// Use a different external ID with a long future date.
		c, err = p.GetAWSCreds(ctx, testRoleArn, "")
		require.NoError(t, err)
		validateCred(t, c, testExpFuture)
		require.Equal(t, 1, retrievedCount)
		require.Len(t, p.aws.mu.creds, 2)

		// Invoke again, but this time, the credential is still active, so
		// retrievedCount remains.
		c, err = p.GetAWSCreds(ctx, testRoleArn, "")
		require.NoError(t, err)
		validateCred(t, c, testExpFuture)
		require.Equal(t, 1, retrievedCount)
		require.Len(t, p.aws.mu.creds, 2)
	})
}

func TestAWSCredentials(t *testing.T) {
	t.Run("ExpiresAt", func(t *testing.T) {
		c := &AWSCredentials{Expiration: testExp}
		require.Equal(t, testExp, c.ExpiresAt())
	})

	t.Run("CredentialsFile", func(t *testing.T) {
		c := &AWSCredentials{
			Value: credentials.Value{
				AccessKeyID:     "1",
				SecretAccessKey: "2",
				SessionToken:    "3",
				ProviderName:    "test-provider",
			},
		}
		const expected = "[default]\naws_access_key_id = 1\naws_secret_access_key = 2\naws_session_token = 3\n"
		require.Equal(t, expected, c.CredentialsFile())
	})
}

type stubSTS struct {
	testInput func(aws.Context, *sts.AssumeRoleInput) time.Time
}

var _ stscreds.AssumeRoler = &stubSTS{}

// AssumeRoleWithContext implements the stscreds.assumeRolerWithContext interface.
func (s *stubSTS) AssumeRoleWithContext(
	actx aws.Context, input *sts.AssumeRoleInput, opts ...request.Option,
) (*sts.AssumeRoleOutput, error) {
	expiry := s.testInput(actx, input)
	return &sts.AssumeRoleOutput{
		Credentials: &sts.Credentials{
			// Just reflect the role arn to the provider.
			AccessKeyId:     input.RoleArn,
			SecretAccessKey: new("assumedSecretAccessKey"),
			SessionToken:    new("assumedSessionToken"),
			Expiration:      &expiry,
		},
	}, nil
}

// AssumeRole implements the stscreds.AssumeRoler interface.
func (s *stubSTS) AssumeRole(input *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
	return s.AssumeRoleWithContext(context.Background(), input)
}

type stubProvider struct {
	expired bool
}

var _ credentials.Provider = &stubProvider{}

// Retrieve implements the credentials.Provider interface.
func (s *stubProvider) Retrieve() (credentials.Value, error) {
	// Put a dummy credential, so that the SDK doesn't think it's expired.
	return credentials.Value{AccessKeyID: "dummy"}, nil
}

// IsExpired implements the credentials.Provider interface.
func (s *stubProvider) IsExpired() bool {
	return s.expired
}

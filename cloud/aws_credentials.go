package cloud

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/cockroachdb/errors"
)

const (
	// AWSIntermediaryRoleARNEnv is the environment variable used to indicate
	// the role ARN in the intermediate account. In the case of CockroachDB
	// Cloud, this will be the role ARN stored in the keychain account. The
	// IAM role is used to enable host clusters act on behalf of the keychain
	// account, and must allow the AssumeRole action.
	AWSIntermediaryRoleARNEnv = "CREDENTIAL_PROVIDER_AWS_INTERMEDIARY_ROLE_ARN"

	// AWSCredDurationEnv is the environment variable used to override the
	// duration of STS credentials.
	AWSCredDurationEnv = "CREDENTIAL_PROVIDER_AWS_CRED_DURATION"

	// AWSCredExpiryWindowEnv is the environment variable used to override the
	// default ExpiryWindow value.
	AWSCredExpiryWindowEnv = "CREDENTIAL_PROVIDER_AWS_CRED_EXPIRY_WINDOW"

	// awsCredDurationDefault represents the default duration of STS credentials.
	// (min = 15 minutes, max = 60 minutes). This can be overridden by the
	// AWSCredDurationEnv environment variable.
	awsCredDurationDefault = 60 * time.Minute

	// awsCredExpiryWindowDefault represents the default duration before the
	// credentials expire where refreshing will be triggered upon the next Get.
	// At a credential duration of 60 minutes, setting this to 20 minutes means
	// that we will trigger refreshing at the 40 minute mark. This can be
	// overridden by the AWSCredExpiryWindowEnv environment variable.
	awsCredExpiryWindowDefault = 20 * time.Minute
)

type awsCacheKey struct {
	roleARN    string
	externalID string
}

// AWSCredentials represents an AWS credential object.
type AWSCredentials struct {
	// Value represents the credentials (e.g. keys and tokens).
	credentials.Value
	// Expiration is the time in which the credentials expire.
	Expiration time.Time
}

// GetAWSCreds implements the CredentialProvider interface.
func (cp *credentialProvider) GetAWSCreds(
	ctx context.Context, roleARN, externalID string,
) (*AWSCredentials, error) {
	if cp.aws.client == nil {
		return nil, errors.New("GetAWSCreds is not supported")
	}

	// While holding a lock on the global credentials map, we initialize the credentials.Credentials
	// provider for this key. Note that the network call to actually generate credentials is performed
	// in GetWithContext.
	key := awsCacheKey{roleARN: roleARN, externalID: externalID}
	cred := func() *credentials.Credentials {
		cp.aws.mu.Lock()
		defer cp.aws.mu.Unlock()

		cred, ok := cp.aws.mu.creds[key]
		if !ok {
			c := stscreds.NewCredentialsWithClient(cp.aws.client, key.roleARN,
				func(p *stscreds.AssumeRoleProvider) {
					p.Duration = cp.aws.credDuration
					p.ExpiryWindow = cp.aws.credExpiryWindow

					// The keychain account is shared by multiple users, so we are
					// exposed to the confused deputy problem. Supply external ID
					// to avoid that.
					if key.externalID != "" {
						p.ExternalID = new(key.externalID)
					}
				},
			)
			cred = c
			cp.aws.mu.creds[key] = cred
		}
		return cred
	}()

	// Generate the credentials, GetWithContext is thread-safe, so we do not need to perform
	// any locking of our own to ensure multiple callers do not duplicate work.
	// Based on experiments, it takes ~15ms to renew such a credential most
	// of the time.
	credValue, err := cred.GetWithContext(ctx)
	if err != nil {
		return nil, err
	}
	expiration, err := cred.ExpiresAt()
	if err != nil {
		return nil, err
	}

	return &AWSCredentials{credValue, expiration}, nil
}

// ExpiresAt returns the expiration of the credential. Given that we support
// an ExpiryWindow field, this would take that into account, and return the
// timestamp in which the credential is *considered* expired. If ExpiryWindow
// is 0, then ExpiresAt returns the timestamp in which the credential is no
// longer valid.
func (c *AWSCredentials) ExpiresAt() time.Time {
	return c.Expiration
}

// CredentialsFile returns the shared credentials file representation of the
// credential as part of the default profile.
//
// When using such a credential file, set the `AWS_SHARED_CREDENTIALS_FILE`
// environment variable to point to it, or write it to "$HOME/.aws/credentials".
// At the same time, ensure that the `AWS_PROFILE` environment variable is left
// blank, or set to "default".
func (c *AWSCredentials) CredentialsFile() string {
	return fmt.Sprintf(`[default]
aws_access_key_id = %s
aws_secret_access_key = %s
aws_session_token = %s
`, c.AccessKeyID, c.SecretAccessKey, c.SessionToken)
}

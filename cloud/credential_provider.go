package cloud

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/cockroachdb/errors"
	"golang.org/x/oauth2/google"
)

// CredentialProvider is a component that manages cloud credentials, and caches
// them if they are still valid.
type CredentialProvider interface {
	// GC cleans up all expired credentials from the provider cache, and blocks
	// the caller until the operation has completed.
	GC()

	// GetAWSCreds returns cached AWS credentials based on the given roleARN and
	// externalID. If the cached credentials expire, a new one will be obtained.
	GetAWSCreds(ctx context.Context, roleARN, externalID string) (*AWSCredentials, error)

	// GetGCPCreds returns cached GCP credentials based on the given saEmailID.
	// If the cached credentials expire, a new one will be obtained.
	GetGCPCreds(ctx context.Context, saEmailID string) (*GCPCredentials, error)
}

type credentialProvider struct {
	aws struct {
		// client represents the AWS STS client that we'll be using to generate
		// short-term credentials. For public host clusters, this will be for
		// the session of the keychain account, whereas for private host
		// clusters, this will be for the session of the host cluster account.
		client stscreds.AssumeRoler

		// credDuration is the expiry duration of the STS credentials.
		credDuration time.Duration

		// credExpiryWindow allows the credentials to trigger refreshing prior
		// to the credentials actually expiring when we invoke Get.
		credExpiryWindow time.Duration

		mu struct {
			sync.Mutex
			// creds stores a list of AWS short-term credentials.
			creds map[awsCacheKey]*credentials.Credentials
		}
	}
	gcp struct {
		// credentialSource generates short term access tokens
		// for specified GCP target principals (service accounts).
		credentialSource *GCPCredentialSource

		// credDuration is the expiry duration of the credentials.
		credDuration time.Duration

		// credExpiryWindow allows the credentials to trigger refreshing prior
		// to the credentials actually expiring when we invoke Get.
		credExpiryWindow time.Duration

		mu struct {
			sync.Mutex
			// creds stores GCP short-term credentials.
			creds map[string]*gcpCredential
		}
	}
	testingKnobs struct {
		awsBaseClient stscreds.AssumeRoler
	}
}

// ProviderOption allows configuration of the credential provider object.
type ProviderOption func(cp *credentialProvider)

// WithTestAWSBaseClient specifies a test stscreds.AssumeRoler to use. This is
// useful in testing scenarios, where a stub can be supplied.
func WithTestAWSBaseClient(c stscreds.AssumeRoler) ProviderOption {
	return func(cp *credentialProvider) {
		cp.testingKnobs.awsBaseClient = c
	}
}

// NewCredentialProvider returns a new instance of the credential provider.
func NewCredentialProvider(env Environment, opts ...ProviderOption) (CredentialProvider, error) {
	var p credentialProvider
	for _, opt := range opts {
		opt(&p)
	}
	switch env.CloudProvider {
	case AWS:
		if err := p.initAWS(); err != nil {
			return nil, err
		}
	case GCP:
		if err := p.initGCP(); err != nil {
			return nil, err
		}
	case Unknown:
		// Return an error instead of allowing the provider to run silently.
		return nil, errors.New("Unknown CloudProvider value")
	}
	return &p, nil
}

func (cp *credentialProvider) initGCP() error {
	cp.gcp.mu.Lock()
	defer cp.gcp.mu.Unlock()

	var err error
	cp.gcp.credDuration, err = maybeDurationEnv(GCPCredDurationEnv, gcpCredDurationDefault)
	if err != nil {
		return errors.Wrap(err, "parsing GCP cred duration")
	}
	cp.gcp.credExpiryWindow, err = maybeDurationEnv(GCPCredExpiryWindowEnv, gcpCredExpiryWindowDefault)
	if err != nil {
		return errors.Wrap(err, "parsing GCP cred expiry window")
	}

	defaultCreds, err := google.FindDefaultCredentials(context.Background(), gcpOauthScope)
	if err != nil {
		return fmt.Errorf("failed to generate default credentials: %v", err)
	}

	cp.gcp.credentialSource = &GCPCredentialSource{
		earlyExpiry:     cp.gcp.credExpiryWindow,
		baseCredentials: defaultCreds,
		delegate:        os.Getenv(GCPIntermediaryServiceAccountEnv),
	}

	cp.gcp.mu.creds = make(map[string]*gcpCredential)

	return nil
}

// initAWS initializes all AWS-related fields for the credential provider.
func (cp *credentialProvider) initAWS() (err error) {
	cp.aws.mu.Lock()
	defer cp.aws.mu.Unlock()

	client := cp.testingKnobs.awsBaseClient
	if client == nil {
		// Use Instance Metadata Service to obtain an AWS session.
		sess, err := session.NewSession()
		if err != nil {
			return errors.Wrap(err, "constructing base AWS session")
		}
		client = sts.New(sess)
	}

	if arn := os.Getenv(AWSIntermediaryRoleARNEnv); arn != "" {
		intCreds := stscreds.NewCredentialsWithClient(client, arn)
		sess, err := session.NewSession(&aws.Config{Credentials: intCreds})
		if err != nil {
			return errors.Wrap(err, "constructing intermediary AWS session")
		}
		client = sts.New(sess)
	}

	cp.aws.client = client
	cp.aws.credDuration, err = maybeDurationEnv(AWSCredDurationEnv, awsCredDurationDefault)
	if err != nil {
		return errors.Wrap(err, "parsing AWS cred duration")
	}
	cp.aws.credExpiryWindow, err = maybeDurationEnv(AWSCredExpiryWindowEnv, awsCredExpiryWindowDefault)
	if err != nil {
		return errors.Wrap(err, "parsing AWS cred expiry window")
	}
	cp.aws.mu.creds = make(map[awsCacheKey]*credentials.Credentials)
	return nil
}

// GC implements the CredentialProvider interface.
func (cp *credentialProvider) GC() {
	gcAWS := func() {
		cp.aws.mu.Lock()
		defer cp.aws.mu.Unlock()
		for key, val := range cp.aws.mu.creds {
			if val.IsExpired() {
				delete(cp.aws.mu.creds, key)
			}
		}
	}
	gcGCP := func() {
		cp.gcp.mu.Lock()
		defer cp.gcp.mu.Unlock()
		for key, val := range cp.gcp.mu.creds {
			if val.IsExpired() {
				delete(cp.gcp.mu.creds, key)
			}
		}
	}
	gcAWS()
	gcGCP()
}

// maybeDurationEnv returns the environment variable value in time.Duration for
// envKey, or defaultValue if it is empty.
func maybeDurationEnv(envKey string, defaultValue time.Duration) (time.Duration, error) {
	v := os.Getenv(envKey)
	if v == "" {
		return defaultValue, nil
	}
	return time.ParseDuration(v)
}

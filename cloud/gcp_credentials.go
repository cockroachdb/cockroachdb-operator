package cloud

import (
	"context"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/utils/timeutils"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
)

const (
	// GCPCredDurationEnv is the environment variable used to override the
	// duration of GCP credentials.
	GCPCredDurationEnv = "CREDENTIAL_PROVIDER_GCP_CRED_DURATION"

	// GCPCredExpiryWindowEnv is the environment variable used to implement an
	// early expiry window for GCP credentials.
	GCPCredExpiryWindowEnv = "CREDENTIAL_PROVIDER_GCP_CRED_EXPIRY_WINDOW"

	// gcpCredDurationDefault represents the default duration of gcp credentials.
	// (min = 15 minutes, max = 60 minutes). This can be overridden by the
	// GCPCredDurationEnv environment variable.
	gcpCredDurationDefault = 60 * time.Minute

	// gcpCredExpiryWindowDefault represents the default duration before the
	// credentials expire where refreshing will be triggered upon the next Get.
	// At a credential duration of 60 minutes, setting this to 20 minutes means
	// that we will trigger refreshing at the 40 minute mark. This can be
	// overridden by the GCPCredExpiryWindowEnv environment variable.
	gcpCredExpiryWindowDefault = 20 * time.Minute

	// GCPIntermediaryServiceAccountEnv is the environment variable used to indicate
	// the service account in an intermediate GCP project. In the case of CockroachDB
	// Cloud, this will be the CRLKeychainServiceAccount in the keychain project. The
	// service account is used to enable host clusters to act on behalf of the keychain
	// account, and must have Token creator role permissions.
	GCPIntermediaryServiceAccountEnv = "CREDENTIAL_PROVIDER_GCP_INTERMEDIARY_SERVICE_ACCOUNT"

	// gcpOauthScope should be used when requesting credentials from GCP.
	// See: https://developers.google.com/identity/protocols/oauth2/scopes
	gcpOauthScope = "https://www.googleapis.com/auth/logging.write"
)

type gcpCredential struct {
	sync.Mutex
	*GCPCredentials
}

func (g *gcpCredential) IsExpired() bool {
	return g.ExpiresAt().Before(timeutils.Now())
}

// initializeGCPCredForKey checks the creds map (protected by mutex) and returns a credential
// if it exists. If the key is not found, then a gcpCredential is initialized and stored in
// the creds map. The gcpCredential is protected by a mutex so that concurrent callers can ensure
// they do not repeat work when generating credentials to store in gcpCredential.
func (cp *credentialProvider) initializeGCPCredforKey(key string) *gcpCredential {
	cp.gcp.mu.Lock()
	defer cp.gcp.mu.Unlock()
	cred, ok := cp.gcp.mu.creds[key]
	if !ok {
		cred = &gcpCredential{}
		cp.gcp.mu.creds[key] = cred
	}
	return cred
}

// GetGCPCreds implements the CredentialProvider interface.
func (cp *credentialProvider) GetGCPCreds(
	ctx context.Context, saEmailId string,
) (*GCPCredentials, error) {
	if cp.gcp.credentialSource == nil {
		return nil, errors.New("GetGCPCreds is not supported")
	}

	cred := cp.initializeGCPCredforKey(saEmailId)
	err := func() error {
		cred.Lock()
		defer cred.Unlock()

		// If the credentials are not found, or they are expired, create them.
		if cred.GCPCredentials == nil || (cred.GCPCredentials != nil && cred.IsExpired()) {
			impersonateConfig := impersonate.CredentialsConfig{
				TargetPrincipal: saEmailId,
				Scopes:          []string{gcpOauthScope},
				Lifetime:        cp.gcp.credDuration,
			}

			c, err := cp.gcp.credentialSource.GetCredentials(ctx, impersonateConfig)
			if err != nil {
				return err
			}

			// Store the credentials.
			cred.GCPCredentials = c
		}

		return nil
	}()
	if err != nil {
		return nil, err
	}

	return cred.GCPCredentials, nil
}

// Variable to hook for testing.
var impersonateCredTokenSource = func(ctx context.Context,
	config impersonate.CredentialsConfig, opts ...option.ClientOption) (*oauth2.Token, error) {

	ts, err := impersonate.CredentialsTokenSource(ctx, config, opts...)
	if err != nil {
		return nil, err
	}

	t, err := ts.Token()
	if err != nil {
		return nil, err
	}

	return t, nil
}

type GCPCredentialSource struct {
	earlyExpiry     time.Duration
	baseCredentials *google.Credentials
	delegate        string
}

func (g *GCPCredentialSource) GetCredentials(
	ctx context.Context, config impersonate.CredentialsConfig,
) (*GCPCredentials, error) {
	if g.delegate != "" {
		config.Delegates = []string{g.delegate}
	}

	t, err := impersonateCredTokenSource(ctx, config, option.WithCredentials(g.baseCredentials))
	if err != nil {
		return nil, err
	}

	return &GCPCredentials{t, t.Expiry.Add(-1 * g.earlyExpiry)}, nil
}

// GCPCredentials represents an GCP credential object.
type GCPCredentials struct {
	Token      *oauth2.Token
	Expiration time.Time
}

// CredentialsFile returns a map that contains the accessToken and expiry
// for the credentials. This map should be marshalled into a json string
// and written to a file.
func (c *GCPCredentials) CredentialsFile() map[string]string {
	accessTokenJson := map[string]string{}
	accessTokenJson["accessToken"] = c.Token.AccessToken
	accessTokenJson["expireTime"] = c.Token.Expiry.Format(time.RFC3339)
	return accessTokenJson
}

// ExpiresAt returns the expiration of the credential.
func (c *GCPCredentials) ExpiresAt() time.Time {
	return c.Expiration
}

package cloud

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/utils/timeutils"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
)

var (
	// CRDB v1.0 was released on 2017-05-10 :)
	testExp       = time.Date(2017, 05, 10, 12, 30, 0, 0, time.UTC)
	testExpFuture = time.Date(2900, 05, 10, 12, 30, 0, 0, time.UTC)
)

func TestNewCredentialProvider(t *testing.T) {
	t.Run("invalid CloudProvider", func(t *testing.T) {
		p, err := NewCredentialProvider(Environment{})
		require.Error(t, err, "Unknown CloudProvider value")
		require.Nil(t, p)
	})

	t.Run("GCP", func(t *testing.T) {
		p, err := NewCredentialProvider(Environment{CloudProvider: GCP})
		require.NoError(t, err)

		cp := p.(*credentialProvider)
		require.Equal(t, gcpCredDurationDefault, cp.gcp.credDuration)
		require.Equal(t, gcpCredExpiryWindowDefault, cp.gcp.credExpiryWindow)
		require.NotNil(t, cp.gcp.mu.creds)
	})

	t.Run("GCP intermediary service account env var", func(t *testing.T) {
		require.NoError(t, os.Setenv(GCPIntermediaryServiceAccountEnv, "foo@bar.iam.gserviceaccount.com"))
		t.Cleanup(func() {
			require.NoError(t, os.Unsetenv(GCPIntermediaryServiceAccountEnv))
		})

		p, err := NewCredentialProvider(Environment{CloudProvider: GCP})
		require.NoError(t, err)

		// Check that the GCPCredentialSource uses the intermediary service account as a delegate when
		// calling impersonateCredTokenSource.
		defer testutils.HookGlobal(&impersonateCredTokenSource, func(ctx context.Context,
			config impersonate.CredentialsConfig, opts ...option.ClientOption) (*oauth2.Token, error) {
			require.Equal(t, []string{"foo@bar.iam.gserviceaccount.com"}, config.Delegates)
			return &oauth2.Token{Expiry: time.Now()}, nil
		})()

		cp := p.(*credentialProvider)
		_, err = cp.gcp.credentialSource.GetCredentials(context.Background(), impersonate.CredentialsConfig{})
		require.NoError(t, err)
	})

	t.Run("GCP custom duration env vars", func(t *testing.T) {
		require.NoError(t, os.Setenv(GCPCredDurationEnv, "15m"))
		require.NoError(t, os.Setenv(GCPCredExpiryWindowEnv, "35s"))
		t.Cleanup(func() {
			require.NoError(t, os.Unsetenv(GCPCredDurationEnv))
			require.NoError(t, os.Unsetenv(GCPCredExpiryWindowEnv))
		})

		p, err := NewCredentialProvider(Environment{CloudProvider: GCP})
		require.NoError(t, err)

		cp := p.(*credentialProvider)
		require.Equal(t, 15*time.Minute, cp.gcp.credDuration)
		require.Equal(t, 35*time.Second, cp.gcp.credExpiryWindow)
	})

	t.Run("GCP invalid credDuration env var", func(t *testing.T) {
		require.NoError(t, os.Setenv(GCPCredDurationEnv, "foo"))
		t.Cleanup(func() {
			require.NoError(t, os.Unsetenv(GCPCredDurationEnv))
		})

		p, err := NewCredentialProvider(Environment{CloudProvider: GCP})
		require.EqualError(t, err, `parsing GCP cred duration: time: invalid duration "foo"`)
		require.Nil(t, p)
	})

	t.Run("GCP invalid credExpiryWindow env var", func(t *testing.T) {
		require.NoError(t, os.Setenv(GCPCredExpiryWindowEnv, "bar"))
		t.Cleanup(func() {
			require.NoError(t, os.Unsetenv(GCPCredExpiryWindowEnv))
		})

		p, err := NewCredentialProvider(Environment{CloudProvider: GCP})
		require.EqualError(t, err, `parsing GCP cred expiry window: time: invalid duration "bar"`)
		require.Nil(t, p)
	})

	t.Run("AWS basic fields", func(t *testing.T) {
		stub := &stubSTS{}

		p, err := NewCredentialProvider(Environment{CloudProvider: AWS}, WithTestAWSBaseClient(stub))
		require.NoError(t, err)

		cp := p.(*credentialProvider)
		require.Equal(t, stub, cp.aws.client)
		require.Equal(t, awsCredDurationDefault, cp.aws.credDuration)
		require.Equal(t, awsCredExpiryWindowDefault, cp.aws.credExpiryWindow)
		require.NotNil(t, cp.aws.mu.creds)
	})

	t.Run("AWS custom duration env vars", func(t *testing.T) {
		require.NoError(t, os.Setenv(AWSCredDurationEnv, "15m"))
		require.NoError(t, os.Setenv(AWSCredExpiryWindowEnv, "35s"))
		t.Cleanup(func() {
			require.NoError(t, os.Unsetenv(AWSCredDurationEnv))
			require.NoError(t, os.Unsetenv(AWSCredExpiryWindowEnv))
		})

		stub := &stubSTS{}

		p, err := NewCredentialProvider(Environment{CloudProvider: AWS}, WithTestAWSBaseClient(stub))
		require.NoError(t, err)

		cp := p.(*credentialProvider)
		require.Equal(t, stub, cp.aws.client)
		require.Equal(t, 15*time.Minute, cp.aws.credDuration)
		require.Equal(t, 35*time.Second, cp.aws.credExpiryWindow)
		require.NotNil(t, cp.aws.mu.creds)
	})

	t.Run("AWS invalid credDuration env var", func(t *testing.T) {
		require.NoError(t, os.Setenv(AWSCredDurationEnv, "foo"))
		t.Cleanup(func() {
			require.NoError(t, os.Unsetenv(AWSCredDurationEnv))
		})

		p, err := NewCredentialProvider(Environment{CloudProvider: AWS}, WithTestAWSBaseClient(&stubSTS{}))
		require.EqualError(t, err, `parsing AWS cred duration: time: invalid duration "foo"`)
		require.Nil(t, p)
	})

	t.Run("AWS invalid credExpiryWindow env var", func(t *testing.T) {
		require.NoError(t, os.Setenv(AWSCredExpiryWindowEnv, "bar"))
		t.Cleanup(func() {
			require.NoError(t, os.Unsetenv(AWSCredExpiryWindowEnv))
		})

		p, err := NewCredentialProvider(Environment{CloudProvider: AWS}, WithTestAWSBaseClient(&stubSTS{}))
		require.EqualError(t, err, `parsing AWS cred expiry window: time: invalid duration "bar"`)
		require.Nil(t, p)
	})

	t.Run("AWS intermediary role arn env var", func(t *testing.T) {
		require.NoError(t, os.Setenv(AWSIntermediaryRoleARNEnv, "foo-bar-baz-arn"))
		t.Cleanup(func() {
			require.NoError(t, os.Unsetenv(AWSIntermediaryRoleARNEnv))
		})

		stub := &stubSTS{}

		p, err := NewCredentialProvider(Environment{CloudProvider: AWS}, WithTestAWSBaseClient(stub))
		require.NoError(t, err)

		cp := p.(*credentialProvider)
		require.NotEqual(t, stub, cp.aws.client)
		require.Equal(t, awsCredDurationDefault, cp.aws.credDuration)
		require.Equal(t, awsCredExpiryWindowDefault, cp.aws.credExpiryWindow)
		require.NotNil(t, cp.aws.mu.creds)

		retrievedCount := 0
		stub.testInput = func(actx aws.Context, input *sts.AssumeRoleInput) time.Time {
			retrievedCount++
			require.Equal(t, new("foo-bar-baz-arn"), input.RoleArn)
			return time.Now()
		}
		_, err = cp.aws.client.AssumeRole(&sts.AssumeRoleInput{
			RoleArn:         new("arn:aws:iam::account:user/user-name-with-path"),
			RoleSessionName: new("test-session-name"),
		})
		// We will get an InvalidClientTokenId error here since the credentials
		// returned by the stub is invalid to begin with. Ignore that, and only
		// test that the client has been chained with stub.
		require.Error(t, err)
		require.Equal(t, 1, retrievedCount)
	})

	t.Run("AWS no test base client", func(t *testing.T) {
		p, err := NewCredentialProvider(Environment{CloudProvider: AWS})
		require.NoError(t, err)

		cp := p.(*credentialProvider)
		require.NotNil(t, cp.aws.client)
		require.Equal(t, awsCredDurationDefault, cp.aws.credDuration)
		require.Equal(t, awsCredExpiryWindowDefault, cp.aws.credExpiryWindow)
		require.NotNil(t, cp.aws.mu.creds)
	})
}

func TestCredentialProvider_GC(t *testing.T) {
	t.Run("AWS", func(t *testing.T) {
		p := &credentialProvider{}
		p.aws.mu.creds = make(map[awsCacheKey]*credentials.Credentials)

		stubs := []*stubProvider{
			{true},
			{false},
			{true},
			{false},
			{true},
		}

		for k, v := range stubs {
			c := credentials.NewCredentials(v)
			_, err := c.Get()
			require.NoError(t, err)
			p.aws.mu.creds[awsCacheKey{roleARN: fmt.Sprintf("role-%d", k)}] = c
		}
		require.Len(t, p.aws.mu.creds, 5)
		p.GC()
		require.Len(t, p.aws.mu.creds, 2)

		for _, v := range stubs {
			v.expired = true
		}
		require.Len(t, p.aws.mu.creds, 2)
		p.GC()
		require.Empty(t, p.aws.mu.creds)
	})

	t.Run("GCP", func(t *testing.T) {
		p := &credentialProvider{}
		p.gcp.mu.creds = make(map[string]*gcpCredential)

		n := time.Now()
		defer testutils.HookGlobal(&timeutils.Now, func() time.Time {
			return n
		})()

		creds := []*GCPCredentials{
			{
				&oauth2.Token{},
				n.Add(-1 * time.Hour), // expired
			},
			{
				&oauth2.Token{},
				n.Add(-1 * time.Minute), // expired
			},
			{
				&oauth2.Token{},
				n.Add(time.Minute), // valid for one more minute.
			},
		}

		for i, cred := range creds {
			g := &gcpCredential{}
			g.GCPCredentials = cred
			p.gcp.mu.creds[fmt.Sprintf("test-%d", i)] = g
		}
		require.Len(t, p.gcp.mu.creds, 3)
		p.GC()
		require.Len(t, p.gcp.mu.creds, 1)

		// Advance now by 2 minutes, now the last credential should be GCd as well.
		n = n.Add(2 * time.Minute)
		defer testutils.HookGlobal(&timeutils.Now, func() time.Time {
			return n
		})()
		p.GC()
		require.Empty(t, p.gcp.mu.creds)
	})
}

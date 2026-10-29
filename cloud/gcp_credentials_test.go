package cloud

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
)

func TestCredentialProvider_GetGCPCreds(t *testing.T) {
	ctx := context.Background()

	t.Run("unsupported", func(t *testing.T) {
		p := &credentialProvider{}
		c, err := p.GetGCPCreds(ctx, "foobar")
		require.EqualError(t, err, "GetGCPCreds is not supported")
		require.Nil(t, c)
	})

	t.Run("successful - creds are cached", func(t *testing.T) {
		const foo = "foo"
		const bar = "bar"

		// Test getting credentials for foo once, then again (cached).
		// Then getting credentials for bar.
		count := 0
		defer testutils.HookGlobal(&impersonateCredTokenSource, func(ctx context.Context,
			config impersonate.CredentialsConfig, opts ...option.ClientOption) (*oauth2.Token, error) {
			if count == 1 {
				require.Equal(t, bar, config.TargetPrincipal)
			} else {
				require.Equal(t, foo, config.TargetPrincipal)
			}
			require.Empty(t, config.Delegates)
			count += 1
			return &oauth2.Token{Expiry: time.Now().Add(time.Hour)}, nil
		})()

		p, err := NewCredentialProvider(Environment{CloudProvider: GCP})
		require.NoError(t, err)

		_, err = p.GetGCPCreds(ctx, foo)
		require.NoError(t, err)
		require.Equal(t, 1, count)

		// Get creds again, they should be cached.
		_, err = p.GetGCPCreds(ctx, foo)
		require.NoError(t, err)
		require.Equal(t, 1, count)

		// Get creds for a different target, they should not be cached.
		_, err = p.GetGCPCreds(ctx, bar)
		require.NoError(t, err)
		require.Equal(t, 2, count)
	})

	t.Run("successful - expired creds are recreated", func(t *testing.T) {
		p, err := NewCredentialProvider(Environment{CloudProvider: GCP})
		require.NoError(t, err)

		// Put an expired credential in the map, and ensure that a credential is created.
		cp := p.(*credentialProvider)
		cred := &gcpCredential{}
		cred.GCPCredentials = &GCPCredentials{Token: &oauth2.Token{Expiry: time.Now()}}
		cp.gcp.mu.creds["foo"] = &gcpCredential{}

		count := 0
		defer testutils.HookGlobal(&impersonateCredTokenSource, func(ctx context.Context,
			config impersonate.CredentialsConfig, opts ...option.ClientOption) (*oauth2.Token, error) {
			count += 1
			return &oauth2.Token{Expiry: time.Now()}, nil
		})()

		_, err = p.GetGCPCreds(ctx, "foo")
		require.NoError(t, err)
		require.Equal(t, 1, count)
	})

	t.Run("successful - delegates are used", func(t *testing.T) {
		const foo = "foo"
		const delegate = "abc@xyz.iam.gserviceaccount.com"

		require.NoError(t, os.Setenv(GCPIntermediaryServiceAccountEnv, delegate))
		t.Cleanup(func() {
			require.NoError(t, os.Unsetenv(GCPIntermediaryServiceAccountEnv))
		})

		// Test getting credentials for foo when the credential provider is set up
		// with an intermediary gcp service account. We should see it in the delegates field.
		count := 0
		defer testutils.HookGlobal(&impersonateCredTokenSource, func(ctx context.Context,
			config impersonate.CredentialsConfig, opts ...option.ClientOption) (*oauth2.Token, error) {
			require.Equal(t, foo, config.TargetPrincipal)
			require.Equal(t, []string{delegate}, config.Delegates)
			count += 1
			return &oauth2.Token{Expiry: time.Now()}, nil
		})()

		p, err := NewCredentialProvider(Environment{CloudProvider: GCP})
		require.NoError(t, err)

		_, err = p.GetGCPCreds(ctx, foo)
		require.NoError(t, err)
		require.Equal(t, 1, count)
	})
}

func TestGCPCredentials(t *testing.T) {
	t.Run("ExpiresAt", func(t *testing.T) {
		c := &GCPCredentials{Expiration: testExp}
		require.Equal(t, testExp, c.ExpiresAt())
	})

	t.Run("CredentialsFile", func(t *testing.T) {
		c := &GCPCredentials{
			Token: &oauth2.Token{AccessToken: "foobar", Expiry: testExp},
		}
		m := c.CredentialsFile()
		expireTime := "2017-05-10T12:30:00Z"
		require.Equal(t, "foobar", m["accessToken"])
		require.Equal(t, expireTime, m["expireTime"])
	})
}

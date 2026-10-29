package kubeconfig

import (
	"context"
	"net/http"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/tracing"
	"golang.org/x/oauth2"
	"k8s.io/client-go/rest"
)

// TokenProviderOptions defines options used for creating TokenSources using a
// TokenProvider.
type TokenProviderOptions struct {
	// ClusterID is the string form of the cluster id.
	// Only needed for AWS.
	ClusterID string

	// These are used only for Azure.
	FriendlyID string
	Region     string // not prefixed with the cloud

	// Cloud    values.CloudProvider
	// CredsKey values.VaultCredsKey
}

// TokenProvider is an interface for fetching credentials/tokensources for
// creating oauth2 clients.
type TokenProvider interface {
	// TokenSource returns a token source using the supplied options.
	TokenSource(context.Context, TokenProviderOptions) (oauth2.TokenSource, error)
}

// RestConfigProvider defines an interface that can be used to create
// rest.Config objects.
type RestConfigProvider interface {
	// RestConfig creates a new rest.Config using the supplied token provider for
	// authentication.
	RestConfig(context.Context, TokenProvider) (*rest.Config, error)
	// Namespace returns the Kubernetes namespace that CockroachDB-related resources
	// are "installed" into. Other areas of concern, for example monitoring of
	// CockroachDB, may base their namespace off the returned value.
	//
	// When creating a Ctl instance for example, you'll want to ensure you call
	// `WithNamespace(provider.Namespace())` to ensure you're working in the correct
	// one. See kubecommon/auth.go for examples.
	Namespace() string
}

// StaticRestConfigProvider is a static container that implements
// RestConfigProvider. It is useful for contexts in which all information
// required for dialing a Kubernetes API server is known but isn't attached to
// anything.
type StaticRestConfigProvider struct {
	TokenProviderOptions

	// APIServerHost is the Host at which to dial this Kubernetes API Server.
	APIServerHost string
	// CAData is the CA for this Kubernetes API Server in byte form.
	CAData []byte
	// ScopedNamespace is the namespace that resulting Kubernetes clients will
	// be scoped to by default.
	ScopedNamespace string
}

var _ RestConfigProvider = StaticRestConfigProvider{}

// Namespace implements RestConfigProvider.Namespace by returning
// .ScopedNamespace.
func (s StaticRestConfigProvider) Namespace() string {
	return s.ScopedNamespace
}

// RestConfig implements RestConfigProvider.RestConfig.
func (s StaticRestConfigProvider) RestConfig(
	ctx context.Context, tp TokenProvider,
) (*rest.Config, error) {
	tokenSource, err := tp.TokenSource(ctx, s.TokenProviderOptions)
	if err != nil {
		return nil, errors.Wrap(err, "creating token source")
	}

	token, err := tokenSource.Token()
	if err != nil {
		return nil, errors.Wrap(err, "unable to fetch initial access token")
	}

	return &rest.Config{
		Host: s.APIServerHost,
		WrapTransport: func(rt http.RoundTripper) http.RoundTripper {
			return &oauth2.Transport{
				Source: oauth2.ReuseTokenSource(token, tokenSource),
				Base:   rt,
			}
		},
		TLSClientConfig: rest.TLSClientConfig{
			CAData: s.CAData,
		},
	}, nil
}

// BuildCtl returns a kube.Ctl instance that targets the K8s cluster described
// by the given RestConfigProvider.
var BuildCtl = func(
	ctx context.Context,
	tokenProvider TokenProvider,
	configProvider RestConfigProvider,
) (_ *kube.Ctl, retError error) {
	ctx, span := tracing.StartSpan(ctx, "kubeconfig.BuildCtl")
	defer func() { tracing.EndWithError(span, retError) }()

	restConfig, err := configProvider.RestConfig(ctx, tokenProvider)
	if err != nil {
		return nil, errors.Wrap(err, "building REST config")
	}
	// Increase 5x from the default 20/30 as we see occasional throttling messages in tests/k3d
	restConfig.QPS, restConfig.Burst = 100, 150

	var ctl *kube.Ctl
	// Rather than constantly relying on HookGlobal, we build in a backdoor
	// that manages fake Ctls for us. Ideally, this function would be an
	// overrideable interface instead of a global.
	if isFake(restConfig) {
		ctl = getFakeCtl(restConfig)
	} else {
		var err error
		ctl, err = kube.NewCtlWithOptions(restConfig, kube.Options{Scheme: crdbv1beta1.Scheme})
		if err != nil {
			return nil, errors.Wrap(err, "constructing kube.Ctl")
		}
	}

	return ctl.WithNamespace(configProvider.Namespace()), nil
}

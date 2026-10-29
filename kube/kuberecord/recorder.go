package kuberecord

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/dnaeon/go-vcr/cassette"
	"github.com/dnaeon/go-vcr/recorder"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

// recordMode defines the behaviour of a Recorder.
type recordMode = recorder.Mode

const (
	// ModeReplaying will return a new *rest.Config that will instead generate
	// HTTP responses from a YAML file on disk. No network calls will be made
	// in this mode.
	ModeReplaying = recorder.ModeReplaying
	// ModeRecording will inject fingerprinted headers into all requests and
	// record the resulting request and response to disk for future playback.
	ModeRecording = recorder.ModeRecording
	// ModeDisabled will disable both recording and replaying. Recorder will
	// No-op in this mode.
	ModeDisabled = recorder.ModeDisabled
	// fingerPrintHeader is the header that checksums of outgoing requests are
	// injected into. In replay mode, the appropriate responses are found by
	// matching on this header.
	fingerPrintHeader = "X-Kuberecord-Fingerprint"
	// kubernetesHost is a replacement for the kubernetes' API hostname. The
	// hostname may change across test runs but that doesn't invalidate our
	// interactions. The recording library we use includes the HTTP Host header
	// as a part of its matching logic. Because of that, we need to have stable
	// hostnames in our recordings. The easiest way to do that is to simply
	// rewrite the header. We don't care about the real name of whatever
	// Kubernetes API server we're talking to.
	kubernetesHost = "KUBERNETES"
	schemeHTTPS    = "https"
)

var (
	prunableRequestHeaders = []string{
		"Authorization",
	}
	prunableResponseHeaders = []string{
		"Date",
		"Audit-Id",
	}
)

// Recorder wraps and returns a *rest.Config according the mode returned by
// Mode.
//
// In ModeDisabled, restCfg is returned unmodified.
// In ModeRecording, all HTTP traffic will be recorded and written to a YAML
// file when the test is cleaned up.
// In ModeReplaying, a YAML file will be loaded from disk and all responses
// will be served from there. If a request is not found the test will panic.
//
// YAML files are written/read to/from testdata/<test name>.vcr.yaml.
//
// In record mode, the recorder hashes the body of every HTTP request and adds
// a header with the checksum.
// In replay mode, the recorder looks up the appropriate response to serve
// using that checksum.
//
// All recorded interactions must be deterministic in order for playback mode
// to work. For tests that use random values, consider setting a constant seed.
// For more complicated situations like key generation, consider writing a
// deterministic implementation of the relevant interfaces. See
// crypto.DeterministicKubernetesSigner for an example.
// If one or more requests is removed from an interaction, a new recording will
// not need to be generated. Failures will only occur when unexpected requests
// occur.
//
// Recorder is not a replacement for test assertions. It is a tool that allows
// Kubernetes tests run quickly within a unittest framework. Test should still
// make assertions about the end state of a cluster and the healthiness of the
// services running in it.
// If a cluster is going to be reused in a test, it is recommended to make
// assertions about the environment ahead before the test runs.
// For example, assert that the namespace the test is running does not exist
// before it is created to ensure no previous data is leaked. Replayed
// interactions will playback just fine but this will ensure that future
// recordings don't run on an unclean environment.
//
// Most test cases should follow the following template:
//
//	var restCfg *rest.Config
//	if kuberecord.Mode() != kuberecorder.ModeReplaying {
//		restCfg = LoadOrCreateARealRestConfig()
//	}
//	restCfg = Recorder(t, restCfg)
//	// Make initial assertions about the environment
//	AssertNamespaceDoesntExist(t, restCfg)
//	// Run your tests and assertions
//	// Cleanup the cluster, ideally back to the initial state.
func Recorder(t *testing.T, restCfg *rest.Config) *rest.Config {
	return RecorderWithOptions(t, t.Name(), Mode(), restCfg)
}

// Recorder wraps and returns a *rest.Config according the specified mode. For
// details about its behavior, see Recorder.
func RecorderWithOptions(
	t *testing.T, recordingName string, mode recorder.Mode, restCfg *rest.Config,
) *rest.Config {
	// No-op in disabled mode.
	if mode == ModeDisabled {
		return restCfg
	}

	if mode == ModeReplaying {
		// When replaying disable any rate limiting that's built into the
		// rest.Config to ensure tests run as fast as possible.
		// Don't inherit any values from the original restCfg as we don't need
		// them and it might be nil or empty.
		restCfg = &rest.Config{QPS: 10000, Burst: 100000000}
	} else {
		// Wrap modifies the Config.WrapTransport field, so make a shallow clone.
		cloneCfg := *restCfg
		restCfg = &cloneCfg
	}

	// Share the same mutex and underlying recorder no matter how many times the
	// returned rest.Config instance is used (e.g. kube.Ctl vs. ClientSet). Each
	// time the rest.Config is used to establish a new K8s connection, Wrap will
	// be called. All requests should be recorded into a single file, regardless
	// of which of those K8s connections is used.
	var mut sync.Mutex
	var rec *recorder.Recorder

	restCfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		mut.Lock()
		defer mut.Unlock()

		// Initialize our recorder if it has not yet been initialized.
		var err error
		if rec == nil {
			rec, err = recorder.NewAsMode(fmt.Sprintf("testdata/%s.vcr", recordingName), mode, rt)
			require.NoError(t, err)

			t.Cleanup(func() {
				require.NoError(t, rec.Stop())
			})

			rec.AddFilter(filter)
			rec.SetMatcher(matcher)
		}

		// Ensure that all our requests will be stamped with a checksum for
		// match _before_ they make it into the recorder.
		return &kubeRecorder{mu: &mut, t: t, Recorder: rec, Transport: rt}
	})

	return restCfg
}

// kubeRecorder records requests sent to the RoundTrip method.
type kubeRecorder struct {
	mu        *sync.Mutex
	t         *testing.T
	Recorder  *recorder.Recorder
	Transport http.RoundTripper
}

func (f *kubeRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Swap in the recorder's underlying transport, since different kubeRecorder
	// instances can have different transports, even though all of them use the
	// same recorder.
	f.Recorder.SetTransport(f.Transport)

	var body []byte
	hasher := sha1.New()

	if r.Body != nil {
		var err error

		body, err = io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}

		// Reset the body to something usable
		r.Body = io.NopCloser(bytes.NewBuffer(body))

		if _, err := hasher.Write(body); err != nil {
			// "It never returns an error."
			// - https://golang.org/pkg/hash/#Hash
			panic(err)
		}
	}

	r.Header[fingerPrintHeader] = []string{fmt.Sprintf("%x", hasher.Sum(nil))}

	resp, err := f.Recorder.RoundTrip(r)

	if errors.Is(err, cassette.ErrInteractionNotFound) {
		f.t.Fatalf(
			`Requested interaction not found.\nMethod: %s\nURL: %s\nBody: %s
Do you need to re-record this interaction?
Pass -kuberecord='' to your tests and -update if Golden files need to be updated.
Also, these tests require special registries. Please ensure you have run: ./pkg/infrastructure/k3d/registries.sh up
`,
			r.Method,
			r.URL,
			body,
		)
	}

	return resp, err
}

func filter(i *cassette.Interaction) error {
	u, err := url.Parse(i.URL)
	if err != nil {
		return err
	}

	// Synonymous to the change made in matcher.
	// Force all URLS to looks like https://KUBERNETES/... to make matching
	// stable across different kubernetes clusters. Hostnames change across
	// kubernetes clusters.
	u.Host = kubernetesHost
	// Similar to how we rewrite the hostname to a constant value, we rewrite
	// the scheme, too, to avoid potential flakes if we end up making requests
	// against a Kubernetes API server without TLS.
	u.Scheme = schemeHTTPS

	// Reset the URL in the cassette to our stablized version. i.URL is what is
	// actually persisted to disk and has no affect on the actual request.
	i.URL = u.String()

	// Sanitize outgoing requests. We don't want to accidentally check in
	// credentials.
	for _, header := range prunableRequestHeaders {
		i.Request.Headers.Del(header)
	}

	// Remove any non-deterministic headers to reduce the churn of our records.
	for _, header := range prunableResponseHeaders {
		i.Response.Headers.Del(header)
	}

	return nil
}

func matcher(r *http.Request, i cassette.Request) bool {
	// Force all URLS to looks like https://KUBERNETES/... to make matching
	// stable across different kubernetes clusters. Hostnames change across
	// kubernetes clusters and the empty rest.Config handed back in replay mode
	// won't use HTTPS unless it is provided with a TLS cert. Overridding the
	// scheme here happens to be easier and just as affective.
	r.URL.Host = kubernetesHost
	r.URL.Scheme = schemeHTTPS

	matches := true
	matches = matches && r.Header[fingerPrintHeader][0] == i.Headers[fingerPrintHeader][0]
	return matches && r.Method == i.Method && r.URL.String() == i.URL
}

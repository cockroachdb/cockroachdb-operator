package kubetest

import (
	"github.com/cenkalti/backoff"
	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/testing"
)

// Options is a collection of knobs and tweaks to augment the behavior of the
// FakeCtl ranging from full Kubernetes emulation to dumb object store.
// TODO(chrisseto): Graduate the Before/After mutation hooks from Ctl to this
// struct and export pre-defined Options for easier usage.
type Options struct {
	kube.Options

	// AdvancedEmulation enables additional emulation of Kubernetes. When enabled:
	// * .Generation will be incremented when changes are made to .Spec,
	//   .Labels, or .Annotations.
	// * Secrets will be created for service accounts.
	AdvancedEmulation bool
}

// NewFakeCtl creates a kube.Ctl instance that wraps the fake client.Client from
// the controller-runtime package. This client maintains a simple in-memory
// store of K8s objects that can be fetched, applied, updated, deleted, etc:
//
//	ctl := kubetest.NewFakeCtl(t, kubetest.Options{}, &addPod)
//	ctl.Get(ctx, "my-pod", &getPod)
//
// This is useful for unit testing components that use K8s, without spinning up
// a real cluster or using kuberecord.
// Known differences between the FakeCtl and a real Kubernetes instance are:
//   - Readiness: FakeCtl will fail immediately waiting for objects to become ready.
//     See MakeReady for mitigations.
//   - Server generated field: Fields set by the Kubernetes API server, such as
//     UID, timestamps, etc, will not be set by the FakeCtl. Use BeforeMutation to
//     inject custom values.
//   - Namespace validation: FakeCtl will NOT perform namespace validation due to
//     the lack of a RESTMapper. See FakeRESTMapper for mitigations.
//   - WaitFor: FakeCtl will perform checks exactly once without any polling to
//     minimize test runtime. Asynchronous tests should provide a custom
//     backoff. e.g. backoff.WithMaxRetries(backoff.NewConstantBackOff(time.Second), 10)
func NewFakeCtl(options Options, initObjs ...kube.Object) *kube.Ctl {
	// NB: This code is exclusively used in tests. It should NEVER be called in
	// production. For that reason, it opts to panic upon encountering errors
	// when being initialized. The majority of error cases are programming
	// errors and would cause the executing binary to exit or fail anyways.

	if options.Scheme == nil {
		options.Scheme = crdbv1beta1.Scheme
	}

	f := Fake{}

	tracker := versionedTracker{
		scheme:        options.Scheme,
		ObjectTracker: testing.NewObjectTracker(options.Scheme, scheme.Codecs.UniversalDecoder()),
	}

	f.AddReactor("*", "*", objectReactionWrapper(tracker))

	if options.AdvancedEmulation {
		// NB: Reactors must be prepended and return false for "handled" to
		// ensure the entire chain runs. .Invokes is being customized, so we
		// may opt to ignore handled in favor of error checking.

		// If BeforeMutation or AfterMutation are specified, adapt them into
		// reactors. This ensures that hooks like MakeReady see changes made by
		// newGenerationReactor and update .Status appropriately. Long term,
		// we'll likey want to remove the hooks entirely in favor of reactors
		// as they allow finer grain control and direct access to the object
		// tracker.
		if options.BeforeMutation != nil {
			f.PrependReactor("*", "*", mutationToReactor(options.BeforeMutation, tracker))
			options.BeforeMutation = nil
		}

		if options.AfterMutation != nil {
			f.AddReactor("*", "*", mutationToReactor(options.AfterMutation, tracker))
			options.AfterMutation = nil
		}

		f.PrependReactor("*", "*", newGenerationReactor(tracker))
		f.PrependReactor("create", "serviceaccounts", newServiceAccountReactor(&f))
	}

	for i := range initObjs {
		if err := tracker.Add(initObjs[i]); err != nil {
			panic(errors.AssertionFailedf(
				"failed to add %T %q to the object tracker (Is it a real kube.Object?): %+v",
				initObjs[i],
				initObjs[i].GetName(),
				err,
			))
		}
	}

	options.Client = &fakeClient{scheme: options.Scheme, fake: &f}

	// Supply a StopBackOff if none was provided. It's expected that all
	// awaited conditions are set via the mutation hooks. Any backoff runs are
	// generally indicative of test failures. Instead of stalling tests,
	// StopBackOff will kick back any errors immediately.
	if options.BackOff == nil {
		options.BackOff = func() backoff.BackOff {
			return &backoff.StopBackOff{}
		}
		options.DisallowCustomBackOffs = true
	}

	// Redirect calls to EvictPod to instead call ctl.Delete.
	options.EvictionUsesDeletion = true

	ctl, err := kube.NewCtlWithOptions(&rest.Config{}, options.Options)
	if err != nil {
		panic(errors.AssertionFailedf(
			"failed to create a NewFakeCtl: %+v",
			err,
		))
	}
	return ctl
}

// objectReactionWrapper is a wrapper around testing.ObjectReaction where
// handled is always false so that subsequent reactors can be run.
func objectReactionWrapper(tracker testing.ObjectTracker) testing.ReactionFunc {
	return func(action testing.Action) (bool, runtime.Object, error) {
		_, ret, err := testing.ObjectReaction(tracker)(action)
		return false, ret, err
	}
}

// Fake is a wrapper around [testing.Fake], which is a hub for running
// "reactors". Generally, there is a "reactor" for a [testing.ObjectTracker] at
// the end of the reactor chain.
// TODO: Come up with a better name than Fake. If we were wrapping something
// other than another "Fake", there might be a more obvious name.
type Fake struct {
	testing.Fake
}

// Invokes is a re-implementation of [testing.Fake.Invokes] that mutates the
// given action, and more importantly the kube.Object inside of it. Mutating
// the inner object makes the the Fake play nice with the [client.Client] and
// [kube.Ctl] interfaces. It also supported nesting calls to [Fake.Invokes].
// This implementation does NOT mutate inner fields and therefore does not lock
// the Mutex embedded in Fake.
func (f *Fake) Invokes(action testing.Action) (ret runtime.Object, err error) {
	wasHandled := false
	handled := false
	for _, reactor := range f.ReactionChain {
		if !reactor.Handles(action) {
			continue
		}

		wasHandled = true

		handled, ret, err = reactor.React(action)
		// We want to record the response from the actual mutation.
		if handled || err != nil {
			return ret, err
		}
	}
	if !wasHandled {
		panic("no matching reactor, this shouldn't happen!")
	}
	return ret, err
}

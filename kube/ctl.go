package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/exec"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// DoneFunc is a convenience type to describe the IsReady and IsQuiescent
// functions.
type DoneFunc func(Object) error

// ListOptions is a convenience alias.
type ListOptions = client.ListOptions

// DefaultWaitTimeout is the default amount of time that the Ctl methods will wait for
// their operation to complete before returning a timeout error.
const DefaultWaitTimeout = 300 * time.Second

const DefaultLimitBytes = 1024 * 1024 // 1MB default

// defaultBackOff is used when the caller does not specify the backoff used to
// delay between K8s API polling attempts.
var defaultBackOff func() backoff.BackOff

// ErrAbortWait is used to signal the function passed to WaitForWithOptions will
// never return true.
var ErrAbortWait = errors.New("abort wait")

// CodeExitError is a convenience alias.
type CodeExitError = exec.CodeExitError

func NewCtlDefaultBackOffFn(timeout time.Duration) func() backoff.BackOff {
	return func() backoff.BackOff {
		b := &backoff.ExponentialBackOff{
			InitialInterval:     10 * time.Millisecond,
			RandomizationFactor: backoff.DefaultRandomizationFactor,
			Multiplier:          2.0,
			MaxInterval:         time.Second,
			MaxElapsedTime:      timeout,
			Clock:               backoff.SystemClock,
		}
		b.Reset()
		return b
	}
}

func init() {
	defaultBackOff = NewCtlDefaultBackOffFn(DefaultWaitTimeout)
}

// MutationCallback defines a callback function that is invoked after kube.Ctl
// has created, updated, or deleted a K8s object.
type MutationCallback func(object Object) error

// Options contains fields that modify the behavior of Ctl during various operations.
// All fields are optional.
type Options struct {
	// Scheme is used to map between structs and GroupVersionKinds. If left
	// unspecified, the default includes all standard Kubernetes GVKs and the
	// GVKs for CustomResourceDefinitions.
	Scheme *runtime.Scheme

	// Mapper is used to map GroupVersionKinds to Kubernetes REST Resources.
	// The default will pull information from Kubernetes itself and is
	// sufficient for most cases.
	Mapper meta.RESTMapper

	// Client is used to communicate with the Kubernetes API server. If left
	// unspecified, a default client will be created using client.New and the
	// rest.Config.
	Client client.Client

	// GVKRestClient is a lower-level wrapper for the K8s REST client than the
	// client wrapper. Prefer to use the client wrapper when possible.
	// gvkRestClient takes a GroupVersionKind object and returns a REST client
	// for interacting with that GroupVersionKind.
	GVKRestClient func(gvk schema.GroupVersionKind) (rest.Interface, error)

	// BackOff is used to determine the appropriate amount of time to sleep
	// between polls when waiting for an object to become ready. The default is
	// will poll roughly once per second and is sufficient for most cases. This
	// option should generally be set by tests that want to shorten their
	// runtime.
	//
	// NOTE: BackOff is a function because each invocation of Ctl needs its own
	// private instance, since backoff objects maintain state and are not safe
	// for concurrent use.
	BackOff func() backoff.BackOff

	// DisallowCustomBackOffs disables custom backoff functions that overrides
	// the Ctl's backoff. This is useful if we want to use the same backoff
	// function across the entire Ctl object regardless of context.
	DisallowCustomBackOffs bool

	// BeforeMutation, if non-nil, is invoked before kube.Ctl has created,
	// updated, or deleted a K8s object. This gives the caller a chance to
	// trigger custom logic. If BeforeMutation returns an error, then the
	// mutation is aborted.
	BeforeMutation func(object Object) error

	// AfterMutation, if non-nil, is invoked after kube.Ctl has created,
	// updated, or deleted a K8s object. This gives the caller a chance to
	// trigger custom logic.
	AfterMutation func(object Object) error

	// EvictionUsesDeletion is a knob that replaces the implementation of Evict
	// Pod with standard Pod deletion. This hook is exposed because Eviction is
	// implemented as a raw HTTP request to the Kubernetes API and doesn't
	// integrate well with the FakeCtl.
	// TODO(orchestration): Technically, eviction is just a SubResource of
	// Pods. Can we use the fake client in a newer version of the SDK?
	// TODO(orchestration): Deprecate GVKRestClient in favor of this option.
	// WARNING: This option is not suitable for us in production.
	EvictionUsesDeletion bool
}

// WaitForOptions contains fields that modify the behavior of the WaitFor
// method.
type WaitForOptions struct {
	// DoneFunc is used to determine if an object is "done" or if polling
	// should continue. If not specified, WaitForWithOptions will panic.
	DoneFunc DoneFunc

	// BackOff is used to determine the sleep intervals when polling readiness
	// in WaitForWithOptions. This can be used to override the default backoff
	// behavior associated with the kube.Ctl, provided that the
	// DisallowCustomBackOffs field is not set.
	BackOff func() backoff.BackOff
}

// CreateOptions contains fields that modify the behavior of the CreateWithOptions
// method.
type CreateOptions struct {
	// WaitForOptions customizes the arguments passed to WaitFor in the case where
	// NoWait=false. If DoneFunc is not specified, IsReady will be used.
	WaitForOptions

	// NoWait does not block waiting for the create operation to complete and for
	// the applied objects to be ready.
	NoWait bool
}

// ApplyOptions contains fields that modify the behavior of the ApplyWithOptions
// method.
type ApplyOptions struct {
	// WaitForOptions customizes the arguments passed to WaitFor in the case where
	// NoWait=false. If DoneFunc is not specified, IsReady will be used.
	WaitForOptions

	// Overwrite controls the behavior when there is a 3-way merge conflict
	// between the current, modified, and last applied state of a K8s object. If
	// Overwrite is true, then conflicts are automatically resolved in favor of
	// the modified state. If false, then the apply operation returns an error.
	//
	// Note that the ForceApplyOverwrite annotation on the target resource will
	// take precedence over this field if a valid value is present.
	Overwrite bool

	// NoWait does not block waiting for the apply operation to complete and for
	// the applied objects to be ready.
	NoWait bool

	// NoLastAppliedAnnotation will cause .Apply to omit adding the
	// last-applied annotation to the object that is being applied.
	//
	// WARNING: omitting this annotation could result in conflicts during
	// future calls to .Apply.
	NoLastAppliedAnnotation bool

	// LogManifests cause .Apply to log the objects.
	// This is useful, e.g., when we need to manually revert a change.
	LogManifests bool

	// noPatch skips the update logic in Apply. If an object does not exist, it
	// will be created, otherwise it will be unmodified.
	//
	// NOTE: This is only used internally to work around bugs (see the
	// shouldPreApply function comments).
	noPatch bool
}

// UpdateOptions contains fields that modify the behavior of the
// UpdateWithOptions method.
type UpdateOptions struct {
	// WaitForOptions customizes the arguments passed to WaitFor in the case where
	// NoWait=false. If DoneFunc is not specified, IsReady will be used.
	WaitForOptions

	// NoWait does not block waiting for the update operation to complete and
	// for the updated object to be ready.
	NoWait bool

	// NoLastAppliedAnnotation will cause .Update to omit adding the
	// last-applied annotation to the object that is being updated.
	// WARNING: omitting this annotation could result in conflicts during
	// future calls to .Apply.
	NoLastAppliedAnnotation bool
}

// DeleteOptions contains fields that modify the behavior of the
// DeleteWithOptions method.
// TODO(andyk): Consider Cascade option.
type DeleteOptions struct {
	// WaitForOptions customizes the arguments passed to WaitFor in the case where
	// NoWait=false. If DoneFunc is not specified, IsDeleted will be used.
	WaitForOptions

	// NoWait does not block waiting for the delete operation to complete.
	NoWait bool

	// GracePeriod is the duration in seconds before the object should be
	// forcefully deleted. The value zero indicates *forceful* deletion
	// immediately. If not specified, the default grace period for the
	// specified type will be used.
	GracePeriodSeconds *int64

	// PropagationPolicy decides if a deletion will propagate to the dependents
	// of the object, and how the garbage collector will handle the
	// propagation. The default policy is decided by the existing finalizer set
	// in the metadata.finalizers and the resource-specific default policy. In
	// most cases, the default is Background.
	PropagationPolicy metav1.DeletionPropagation

	// Preconditions must be fulfilled before a deletion is carried out. If not
	// possible, a 409 Conflict status will be returned. They can be a specific
	// resource version or UID.
	Preconditions *metav1.Preconditions
}

// ExecOptions contains fields that modify the behavior of the Exec method.
type ExecOptions struct {
	// Command is the command to be executed in a container without a shell.
	// Command must start with the executable being run. If a shell is desired
	// the command may be wrapped in a shell. IE []string{"sh", "-c", ""}
	Command []string
	// Stdin, if provided, will be connected to the executed command as its
	// standard input.
	Stdin io.Reader
	// Stdout, if provided, will be connected to the executed command as its
	// standard output.
	Stdout io.Writer
	// Stderr, if provided, will be connected to the executed command as its
	// standard error.
	Stderr io.Writer
	// TTY controls whether or not a TTY will be allocated to executed command.
	TTY bool
}

// GetContainerLogsOptions contains fields that modify the behavior of the GetContainerLogs method.
type GetContainerLogsOptions struct {
	// LimitBytes limits the number of bytes returned from the logs.
	// If not specified, 1MB is returned.
	LimitBytes *int64
}

// Ctl has methods that are inspired by the kubectl tool (though with some
// differences). The methods take K8s manifest objects and use the K8s Rest API
// to perform useful operations on them. Ctl maintains context such as the
// target K8s cluster and target namespace.
//
// NOTE: Try to reuse Ctl instances where possible, or else provide
// Options.Mapper when constructing the Ctl. If a Mapper has to be created from
// scratch, it will force the fetch of K8s resource metadata from the server,
// which is an expensive operation.
//
// NOTE: Ctl methods are thread-safe. However, object fields can be updated by
// Ctl, such as to set the namespace field or fill in fields with default
// values.
type Ctl struct {
	// config contains the information needed to connect with a K8s cluster via
	// its REST interface.
	config *rest.Config
	// namespace, if not empty, means that the Ctl will restrict reads and
	// writes of namespace-scoped objects to stay within this namespace.
	// Root-scoped objects are not affected.
	namespace string
	// client is a convenience wrapper for the K8s REST client that adds some
	// caching and metadata discovery.
	client client.Client
	// gvkRestClient is a lower-level wrapper for the K8s REST client than the
	// client wrapper. Prefer to use the client wrapper when possible.
	// gvkRestClient takes a GroupVersionKind object and returns a REST client
	// for interacting with that GroupVersionKind.
	gvkRestClient func(gvk schema.GroupVersionKind) (rest.Interface, error)
	// scheme contains a mapping to/from typed K8s objects (e.g. corev1.Pod) and
	// the API Group/Version and object Kind for that resource.
	scheme *runtime.Scheme
	// mapper provides additional information about K8s objects, such as its
	// REST API resource name and whether its root or namespace scoped.
	mapper meta.RESTMapper
	// backOff is used to determine the sleep intervals when polling for
	// readiness in WaitForWithOptions.
	backOff func() backoff.BackOff
	// disallowCustomBackOffs disables custom backoff functions that overrides
	// the Ctl's backoff. This is useful if we want to use the same backoff
	// function across the entire Ctl object regardless of context.
	disallowCustomBackOffs bool
	// beforeMutation, if non-nil, is invoked before kube.Ctl has created,
	// updated, or deleted a K8s object. This gives the caller a chance to
	// trigger custom logic. If beforeMutation returns an error, then the
	// mutation is aborted.
	beforeMutation func(object Object) error
	// afterMutation, if non-nil, is invoked after kube.Ctl has created, updated,
	// or deleted a K8s object. This gives the caller a chance to trigger custom
	// logic.
	afterMutation func(object Object) error
	// evictionUsesDeletion is a knob that replaces the implementation of Evict
	// Pod with standard Pod deletion. This hook is exposed because Eviction is
	// implemented as a raw HTTP request to the Kubernetes API and doesn't
	// integrate well with the FakeCtl.
	// TODO(orchestration): Technically, eviction is just a SubResource of
	// Pods. Can we use the fake client in a newer version of the SDK?
	// TODO(orchestration): Deprecate GVKRestClient in favor of this option.
	evictionUsesDeletion bool
}

// NewCtl constructs a new Ctl instance that targets the cluster described by
// the REST config with default options.
var NewCtl = func(config *rest.Config) (*Ctl, error) {
	return NewCtlWithOptions(config, Options{})
}

// NewCtlWithOptions constructs a new Ctl instance that targets the cluster
// described by the REST config and the K8s metadata discovery options.
var NewCtlWithOptions = func(config *rest.Config, options Options) (*Ctl, error) {
	var err error

	// Init a scheme if none provided.
	if options.Scheme == nil {
		options.Scheme = Scheme
	}

	// Init a Mapper if none provided.
	if options.Mapper == nil {
		// TODO(andyk): Grab the mapper from options.Client if that is present.
		// Later versions of the controller-runtime have a RESTMapper method on
		// the client.Client interface. But until we've upgraded to that, we'll
		// leave the mapper nil, since this may be a fake client.Client for
		// testing.
		if options.Client == nil {
			// Default to using the DynamicRESTMapper. Whenever a new CRD is
			// applied to a cluster, the first attempt to create such a CR will
			// miss the RESTMapper's cache. The DynamicRESTMapper will refresh
			// its cache once upon type misses before throwing an error.
			options.Mapper, err = apiutil.NewDynamicRESTMapper(config)
			if err != nil {
				return nil, err
			}
		}
	}

	if options.BackOff == nil {
		// Set up exponential backoff between calls to the Kubernetes API.
		options.BackOff = defaultBackOff
	}

	// Init a client if not provided.
	if options.Client == nil {
		options.Client, err = client.New(config, client.Options{
			Mapper: options.Mapper,
			Scheme: options.Scheme,
		})
		if err != nil {
			return nil, err
		}
	}

	if options.GVKRestClient == nil {
		options.GVKRestClient = func(gvk schema.GroupVersionKind) (rest.Interface, error) {
			codecs := serializer.NewCodecFactory(options.Scheme)
			return apiutil.RESTClientForGVK(gvk, false /*isUnstructured*/, config, codecs)
		}
	}

	return &Ctl{
		config:                 config,
		client:                 options.Client,
		gvkRestClient:          options.GVKRestClient,
		scheme:                 options.Scheme,
		mapper:                 options.Mapper,
		backOff:                options.BackOff,
		disallowCustomBackOffs: options.DisallowCustomBackOffs,
		beforeMutation:         options.BeforeMutation,
		afterMutation:          options.AfterMutation,
		evictionUsesDeletion:   options.EvictionUsesDeletion,
	}, nil
}

// NewCtlFromManager constructs a new kube.Ctl instance from a K8s controller
// manager. The controller manager has already created instances of all the
// objects needed by kube.Ctl to control a cluster.
func NewCtlFromManager(manager ctrl.Manager) (*Ctl, error) {
	return NewCtlWithOptions(
		manager.GetConfig(),
		Options{
			Scheme:  manager.GetScheme(),
			Mapper:  manager.GetRESTMapper(),
			Client:  manager.GetClient(),
			BackOff: defaultBackOff,
		},
	)
}

// RestConfig is the K8s REST configuration used by this kube.Ctl instance to
// connect to the server.
func (c *Ctl) RestConfig() *rest.Config {
	return c.config
}

// Scheme is the registry of K8s types that are known to this kube.Ctl instance.
func (c *Ctl) Scheme() *runtime.Scheme {
	return c.scheme
}

// RestMapper is the K8s REST mapper, which offers a number of methods for
// working with K8s resource metadata fetched from the server and cached by the
// mapper.
func (c *Ctl) RestMapper() meta.RESTMapper {
	return c.mapper
}

// Namespace is the K8s namespace into which this kube.Ctl instance installs
// namespace-scoped K8s objects.
func (c *Ctl) Namespace() string {
	return c.namespace
}

// WithNamespace returns a Ctl instance that intercepts all Ctl calls in order
// to set the namespace fields of all incoming namespace-scoped objects. This
// makes it easy to construct K8s resources in a particular namespace without
// needing to manually update all the objects beforehand.
func (c *Ctl) WithNamespace(namespace string) *Ctl {
	copy := *c
	copy.namespace = namespace
	return &copy
}

// Get fetches an object from the target K8s cluster that matches the given
// name. If the object is namespace-scoped, then the name is qualified by the
// Ctl's namespace scope. The object parameter must be a pointer to the matching
// struct type so that it can be updated with the response returned by the
// server. If the object cannot be found, Get returns a kubeerrors.IsNotFound
// error.
func (c *Ctl) Get(ctx context.Context, name string, object Object) error {
	// If caller did not specify the object's group/version/kind, fill it in
	// now based on the scheme.
	if err := c.fillDefaults(object); err != nil {
		return err
	}
	namespace, err := metadataAccessor.Namespace(object)
	if err != nil {
		return err
	}
	// If Get is being called in a loop, not all fields of object are
	// guaranteed to be properly reset. Most notably, if Status contains any
	// fields that become zero they will never actually get set to zero. This
	// means that scaling to a deployment to zero will stall forever in
	// IsReady, if we don't zero out .Status.
	// We only want to modify object if we can successfully pull it from the
	// cluster, otherwise we'll be handing back an empty object which can
	// result in some unexpected bugs/behavior.
	runtimeObject, err := c.scheme.New(object.GetObjectKind().GroupVersionKind())
	if err != nil {
		return err
	}

	// Unstructured objects lose the GVK fields when c.Scheme.New() is called.
	runtimeObject.GetObjectKind().SetGroupVersionKind(object.GetObjectKind().GroupVersionKind())

	out, err := FromRuntime(runtimeObject)
	if err != nil {
		return err
	}
	if err := c.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, out); err != nil {
		return err
	}
	// No error, it's safe to modify the passed in object
	value := reflect.ValueOf(object)
	value.Elem().Set(reflect.ValueOf(out).Elem())
	return nil
}

// List retrieves a list of K8s objects according to the given options. On a
// successful call, the Items field in the given list object will be populated
// with the result returned from the server.
func (c *Ctl) List(ctx context.Context, opts *ListOptions, list ObjectList) error {
	if err := c.fillDefaultGVK(list); err != nil {
		return errors.Wrap(err, "unable to fill group version kind for list")
	}

	objGvk := list.GetObjectKind().GroupVersionKind()
	setNamespace, err := c.validateNamespace(objGvk, opts.Namespace)
	if err != nil {
		return errors.Wrap(err, "unable to validate namespace")
	}
	if opts.Namespace == "" && setNamespace != "" {
		// No namespace was specified in opts.Namespace, so set it now.
		newOpts := *opts
		newOpts.Namespace = setNamespace
		opts = &newOpts
	}

	return c.client.List(ctx, list, opts)
}

// Create creates the objects with default CreateOptions.
func (c *Ctl) Create(ctx context.Context, object ...Object) error {
	return c.CreateWithOptions(ctx, CreateOptions{}, object...)
}

// CreateWithOptions creates the objects and optionally wait for the
// objects to be ready.
func (c *Ctl) CreateWithOptions(
	ctx context.Context, options CreateOptions, objects ...Object,
) error {
	logger := logging.S(ctx)

	for _, object := range objects {
		// If caller did not specify the object's group/version/kind, fill it in
		// now based on the scheme.
		if err := c.fillDefaults(object); err != nil {
			return err
		}

		// Add annotation that "remembers" the state of the object at the time of
		// creation, so that 3-way diff'ing can be done.
		if err := setLastApplied(object); err != nil {
			return err
		}

		if err := c.fireBeforeMutation(object); err != nil {
			return err
		}

		gvk := object.GetObjectKind().GroupVersionKind()

		if err := c.client.Create(ctx, object); err != nil {
			return errors.Wrapf(err, "failed to create object")
		}

		key := getObjectKeyOrPanic(object)

		logger.Infof("%s/%s created successfully", gvk.Kind, key.Name)

		if err := c.fireAfterMutation(object); err != nil {
			return err
		}
	}

	if !options.NoWait {
		if options.DoneFunc == nil {
			options.DoneFunc = IsReady
		}

		// Wait until all objects are ready.
		if err := c.WaitForWithOptions(ctx, options.WaitForOptions, objects...); err != nil {
			return err
		}

		logger.Info("all created objects ready")
	}

	return nil
}

// Apply calls ApplyWithOptions with default options.
func (c *Ctl) Apply(ctx context.Context, objects ...Object) error {
	return c.ApplyWithOptions(ctx, ApplyOptions{}, objects...)
}

type objectChange struct {
	ExistingObj Object `json:"before"`
	DesiredObj  Object `json:"after"`
}

// ApplyWithOptions idempotently deploys a set of K8s objects to the target
// cluster. If an object already exists, ApplyWithOptions will generate a diff
// and PATCH the existing object. If patching encounters a conflict, then
// ApplyWithOptions returns an error, unless the Overwrite option is true. If
// the ForceApplyOverwrite annotation is present on the target object in the
// server, the behavior of the Overwrite option will be determined based on
// that. ApplyWithOptions blocks until all objects are healthy and ready, unless
// the NoWait option is true.
//
// Use Apply rather than Update when you need to "upsert" an entire K8s manifest
// without needing to read any existing values, and when you want some level of
// conflict detection.
//
// This command behaves like "kubectl apply".
//
// NOTE: The apply operation is NOT transactional; if one object fails to
// apply, others may have applied successfully. Applied objects should have
// a unique set of labels to aid in removal and identification.
//
// NOTE: Objects are not sorted by their dependencies on each other and so
// correct ordering must be handled by the caller.
//
// NOTE: Patch generation hasn't been tested for all object types yet. Also,
// some objects do not allow all fields to be set. Ensure that you write an
// integration test.
//
// NOTE: Since we exit early if an error is thrown when Pruning, if an error is
// raised there is no guarantee on how many objects were pruned.
//
// Note: objects is an input+output parameter. The objects are updated to match
// the actual state on the apiserver.
func (c *Ctl) ApplyWithOptions(ctx context.Context, options ApplyOptions, objects ...Object) error {
	logger := logging.S(ctx)

	// Split objects into tombstones, which should be deleted, and objects,
	// which shoulld be applied.
	tombstones, objects := splitTombstones(objects)

	// If this call has opted out of, we'll respect the option. preApply fixes
	// generally depend on the buggy controller responding to update events. If
	// Patching is disabled, our fixes won't be respected, so we skip the
	// entire block.
	if !options.noPatch {
		// Some object need to be updated (rather than created) in order for
		// the underlying controller to properly respect the changes. We detect
		// these objects and created a modified version so the actual apply
		// call will generate an update event.
		var preApply []Object
		for _, obj := range objects {
			if clone := shouldPreApply(obj); clone != nil {
				preApply = append(preApply, clone)
			}
		}

		// Specify noPatch:true so that these objects are only created if they
		// don't already exist. Always wait for readiness as it's the safest
		// case. The underlying controller is already known to be buggy, so why
		// give it the benefit of the doubt?
		// if preApply is an empty slice, ApplyWithOptions is a noop.
		if err := c.ApplyWithOptions(ctx, ApplyOptions{noPatch: true}, preApply...); err != nil {
			return err
		}
	}

	patched := false
	for _, obj := range objects {
		// If caller did not specify the object's group/version/kind, fill it in
		// now based on the scheme.
		if err := c.fillDefaults(obj); err != nil {
			return err
		}

		if !options.NoLastAppliedAnnotation {
			// Add annotation that "remembers" the state of the object at the
			// time of creation, so that 3-way diff'ing can be done.
			if err := setLastApplied(obj); err != nil {
				return errors.Wrap(err, "failed to set last applied annotation")
			}
		}

		if err := c.fireBeforeMutation(obj); err != nil {
			return err
		}

		for {
			// Check whether resource exists on the server.
			gvk := obj.GetObjectKind().GroupVersionKind()
			key := getObjectKeyOrPanic(obj)
			serverObj := reflect.New(reflect.TypeOf(obj).Elem()).Interface().(Object)
			if unsObj, isUnstructured := serverObj.(*unstructured.Unstructured); isUnstructured {
				// We need to add the gvk manually for unstructured, otherwise
				// the client would have no way to get this info.
				unsObj.SetGroupVersionKind(gvk)
			}
			err := c.client.Get(ctx, key, serverObj)
			if client.IgnoreNotFound(err) != nil {
				return errors.Wrapf(err, "failed to get %s/%s", gvk.Kind, key.Name)
			}

			if kubeerrors.IsNotFound(err) {
				// Create the resource, since it does not yet exist on the server.
				if err := c.client.Create(ctx, obj); err != nil {
					if kubeerrors.IsAlreadyExists(err) {
						// Another actor created object, retry.
						logger.Infof("failed to create %s/%s due to conflict, retrying", gvk.Kind, key.Name)
						continue
					}
					return errors.Wrapf(err, "failed to create %s/%s", gvk.Kind, key.Name)
				}

				logger.Infof("%s/%s created successfully", gvk.Kind, key.Name)
				if options.LogManifests && !shouldAlwaysRedact(gvk) {
					marshalledObj, err := json.Marshal(obj)
					if err != nil {
						logger.Errorf("marshal object %v", obj)
					} else {
						logger.Infof("Object created: %v", string(marshalledObj))
					}
				}
				break
			}

			if options.noPatch {
				logger.Infof("skipping patching for %s/%s", gvk.Kind, key.Name)
				break
			}

			if err := c.errorOnAddonManager(serverObj); err != nil {
				return err
			}

			// Fill in the GroupVersionKind of the server object.
			serverObj.GetObjectKind().SetGroupVersionKind(gvk)

			// Generate the patch.
			patchType, patchBytes, err := GeneratePatch(
				serverObj, obj, c.canOverwriteObject(ctx, serverObj, options.Overwrite),
			)
			if err != nil {
				return errors.Wrapf(err, "unable to generate patch for %s/%s", gvk.Kind, key.Name)
			}

			// Skip empty patches.
			if bytes.Equal(patchBytes, emptyPatch) {
				logger.Debugf("%s/%s is up to date", gvk.Kind, key.Name)
				break
			}
			if options.LogManifests && !shouldAlwaysRedact(gvk) {
				logger.Infof("%s/%s in %s will be patched with type %v %s", gvk.Kind, key.Name, key.Namespace, patchType, string(patchBytes))
			}

			// Submit the patch to the server.
			patch := client.RawPatch(patchType, patchBytes)
			if err := c.client.Patch(ctx, obj, patch); err != nil {
				if kubeerrors.IsConflict(err) {
					// Another actor updated the resource, retry.
					logger.Infof("failed to patch %s/%s due to conflict, retrying", gvk.Kind, key.Name)
					continue
				}
				return errors.Wrapf(err, "failed to patch %s/%s", gvk.Kind, key.Name)
			}

			patched = true
			logger.Infof("%s/%s patched successfully", gvk.Kind, key.Name)
			if options.LogManifests && !shouldAlwaysRedact(gvk) {
				change, err := json.Marshal(objectChange{
					ExistingObj: serverObj,
					DesiredObj:  obj,
				})
				if err != nil {
					logger.Errorf("marshal object %v", obj)
				} else {
					logger.Infof("Object applied: %v", string(change))
				}
			}
			break
		}

		if err := c.fireAfterMutation(obj); err != nil {
			return err
		}
	}

	for _, tombstone := range tombstones {
		// Trigger a deletion. If NoWait is false, we'll block via WaitFor
		// after this loop.
		err := c.DeleteWithOptions(ctx, DeleteOptions{NoWait: true}, tombstone)

		// Tombstone deletions are idempotent. Only return errors that
		// aren't not found errors.
		if err != nil && !kubeerrors.IsNotFound(err) {
			// TODO(chrisseto): We could potentially expose a prometheus
			// counter or make use of a structured logging counter that's
			// incremented when err == nil. When the counter remains 0 for a
			// certain object key, we'll know that it's safe to remove the
			// Tombstone.
			return err
		}
	}

	if !options.NoWait {
		if options.DoneFunc == nil {
			options.DoneFunc = IsReady
		}

		// Wait until all objects are ready.
		if err := c.WaitForWithOptions(ctx, options.WaitForOptions, objects...); err != nil {
			return err
		}

		// Wait until all tombstones have been removed.
		tombstoneOptions := WaitForOptions{BackOff: options.BackOff, DoneFunc: IsDeleted}
		if err := c.WaitForWithOptions(ctx, tombstoneOptions, tombstones...); err != nil {
			return err
		}

		if patched {
			logger.Info("all applied objects ready")
		}
	}

	return nil
}

type Operation string

const (
	Create Operation = "create"
	Update Operation = "update"
	Delete Operation = "delete"
)

type Diff struct {
	Operation Operation
	Patch     json.RawMessage
	// Object is the kube.Object passed to Diff()
	Object Object `json:"-"`
}

// Diff generates the set of json patches that would be applied by Apply but
// returns them instead of applying. Note: we suppress diffs to the last-...
func (c *Ctl) Diff(ctx context.Context, objects ...Object) ([]Diff, error) {
	diffs := []Diff{}

	logger := logging.S(ctx)

	// Split objects into tombstones, which should be deleted, and objects,
	// which should be applied.
	tombstones, objects := splitTombstones(objects)

	for _, obj := range objects {
		// Deep copy the object to avoid mutating the original; we mutate the copy to suppress
		// uninteresting diffs. Note: cargo-cult assign to ensure we don't use the same `obj` in
		// each `Diff`.
		obj := DeepCopy(obj)
		diff := Diff{Object: obj}

		// If caller did not specify the object's group/version/kind, fill it in
		// now based on the scheme.
		if err := c.fillDefaults(obj); err != nil {
			return diffs, err
		}

		if err := c.fireBeforeMutation(obj); err != nil {
			return diffs, err
		}

		// Check whether resource exists on the server.
		gvk := obj.GetObjectKind().GroupVersionKind()
		key := getObjectKeyOrPanic(obj)
		serverObj := reflect.New(reflect.TypeOf(obj).Elem()).Interface().(Object)
		if unsObj, isUnstructured := serverObj.(*unstructured.Unstructured); isUnstructured {
			// We need to add the gvk manually for unstructured, otherwise
			// the client would have no way to get this info.
			unsObj.SetGroupVersionKind(gvk)
		}
		err := c.client.Get(ctx, key, serverObj)
		if kubeerrors.IsNotFound(err) {
			diff.Operation = Create
		} else {
			diff.Operation = Update
		}
		if client.IgnoreNotFound(err) != nil {
			return diffs, errors.Wrapf(err, "failed to get %s/%s", gvk.Kind, key.Name)
		}

		if err := c.errorOnAddonManager(serverObj); err != nil {
			return diffs, err
		}

		// Suppress diffs on resource version and  last applied configuration annotation. This
		// function is meant to be used to help a human inspect a diff, and changes to these
		// automatically managed values aren't interesting in this context.
		obj.SetResourceVersion("")
		serverObj.SetResourceVersion("")
		lastApplied, ok := serverObj.GetAnnotations()["kubectl.kubernetes.io/last-applied-configuration"]
		if ok {
			annotations := obj.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations["kubectl.kubernetes.io/last-applied-configuration"] = lastApplied
			obj.SetAnnotations(annotations)
		}

		// Fill in the GroupVersionKind of the server object.
		serverObj.GetObjectKind().SetGroupVersionKind(gvk)

		// Generate the patch.
		_, patchBytes, err := GeneratePatch(
			serverObj, obj, true,
		)
		if err != nil {
			return diffs, errors.Wrapf(err, "unable to generate patch for %s/%s", gvk.Kind, key.Name)
		}

		// Skip empty patches.
		if bytes.Equal(patchBytes, emptyPatch) {
			logger.Debugf("%s/%s is up to date", gvk.Kind, key.Name)
			continue
		}

		diff.Patch = json.RawMessage(patchBytes)
		diffs = append(diffs, diff)
	}

	for _, obj := range tombstones {
		diffs = append(diffs, Diff{
			Operation: Delete,
			Patch:     nil,
			Object:    obj,
		})
	}

	return diffs, nil
}

// canOverwriteObject determines the final behavior when there is a 3-way merge
// conflict between the current, modified, and last applied state of a K8s
// object by taking the ForceApplyOverwrite annotation on the object into
// consideration as well. For more details, see the Overwrite field of
// ApplyOptions.
//
// Note that if an error occurs while reading the annotation, or an invalid
// value was observed, the default behavior, i.e. applyOverwrite, will be used.
func (c *Ctl) canOverwriteObject(ctx context.Context, obj Object, applyOverwrite bool) bool {
	logger := logging.S(ctx)
	gvk := obj.GetObjectKind().GroupVersionKind()
	key := getObjectKeyOrPanic(obj)

	// Read ForceApplyOverwrite annotation on target object.
	forceOverwrite, err := getAnnotation(obj, ForceApplyOverwriteAnnotation)
	if err != nil {
		logger.Infof("unable to retrieve the ForceApplyOverwrite annotation for %s/%s", gvk.Kind, key.Name)
		// No-op, and return defaults.
		return applyOverwrite
	}

	// No ForceApplyOverwrite annotation. Use the default behavior.
	if forceOverwrite == nil {
		return applyOverwrite
	}

	switch string(forceOverwrite) {
	case "true":
		return true
	case "false":
		return false
	default:
		// No-op.
		logger.Warnf("%s/%s contains an invalid ForceApplyOverwrite annotation", gvk.Kind, key.Name)
	}

	return applyOverwrite
}

// Update is a helper method that calls UpdateWithOptions with default options.
func (c *Ctl) Update(ctx context.Context, object Object) error {
	return c.UpdateWithOptions(ctx, UpdateOptions{}, object)
}

// Patch applies a patch to an existing object. Unlike Update, which sends the
// whole object and makes this operator's field manager co-own every field it
// serializes, a patch only claims ownership of the fields it carries. That
// distinction matters for objects that are also managed by an external
// server-side applier. A full Update on such an object would let the operator
// take co-ownership of spec fields it does not manage, like a Pod template's
// containers, so the external applier's next apply would then conflict. Use
// Patch with a metadata-scoped diff when the operator only needs to touch its
// own fields like finalizers or annotations on a user-managed resource.
func (c *Ctl) Patch(ctx context.Context, object Object, patch client.Patch) error {
	logger := logging.S(ctx)

	// Patch is deliberately thinner than Update and Apply. It does not fill GVK
	// defaults or fire the before/after mutation hooks, because both mutate the
	// object and a client-side patch like client.MergeFrom computes its diff
	// against the live object at send time. Any post-capture mutation would leak
	// into the patch and defeat the scoped write. It also skips the last-applied
	// annotation and readiness wait, which are full-object behaviors. Namespace
	// validation still runs without mutating the object. The caller owns exactly
	// what the patch carries.
	gvk := object.GetObjectKind().GroupVersionKind()
	if gvk.Empty() {
		var err error
		gvk, err = apiutil.GVKForObject(object, c.client.Scheme())
		if err != nil {
			return err
		}
	}
	expectedNamespace, err := c.validateNamespace(gvk, object.GetNamespace())
	if err != nil {
		return err
	}
	if object.GetNamespace() == "" && expectedNamespace != "" {
		return errors.Newf("cannot patch %s %q without a namespace, expected %q", gvk.Kind, object.GetName(), expectedNamespace)
	}

	key := getObjectKeyOrPanic(object)
	if err := c.client.Patch(ctx, object, patch); err != nil {
		logger.Errorf("failed to patch %s/%s: %v", gvk.Kind, key.Name, err)
		return err
	}
	logger.Infof("patching %s/%s", gvk.Kind, key.Name)
	return nil
}

// ResetManagedFields clears all field ownership recorded for an object. Kubernetes
// documents metadata.managedFields=[{}] as the supported reset representation. This
// should only be used at an explicit ownership handoff because it removes ownership
// for every field manager, not only for a particular API version.
func (c *Ctl) ResetManagedFields(ctx context.Context, object Object) error {
	logger := logging.S(ctx)
	if err := c.fillDefaults(object); err != nil {
		return err
	}
	if err := c.fireBeforeMutation(object); err != nil {
		return err
	}

	gkv := object.GetObjectKind().GroupVersionKind()
	key := getObjectKeyOrPanic(object)
	patch := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"managedFields":[{}]}}`))
	if err := c.client.Patch(ctx, object, patch); err != nil {
		logger.Errorf("failed to reset managed fields for %s/%s: %v", gkv.Kind, key.Name, err)
		return err
	}

	// The API server normalizes the reset representation to an empty list. Keep the
	// caller's object consistent so a following update does not send the sentinel again.
	object.SetManagedFields(nil)
	logger.Infof("reset managed fields for %s/%s", gkv.Kind, key.Name)
	return c.fireAfterMutation(object)
}

// UpdateWithOptions modifies an existing K8s object to match the given object.
// If no such object exists, it returns a kubeerrors.IsNotFound error. If the
// NoWait option is false, then it waits for the object to be updated and ready.
//
// The typical calling pattern is to first call Get, to then modify one or more
// fields, and to call Update to persist those changes to the server. This
// pattern automatically uses the K8s ResourceVersion field to detect any
// changes made by other actors in the space between calling Get and Update.
// ResourceVersion is a monotonically increasing integer guaranteed to increase
// after each successful update. See also:
// https://github.com/kubernetes/kubernetes/issues/2115#issuecomment-61498890
//
// This pattern is preferable to using Apply when you know that the object
// exists, and when you want to change one or more fields after first checking
// the current values of those or other fields. This saves a roundtrip, since
// Apply always fetches the current object in order to generate a patch.
//
// This command (plus the preliminary Get) behaves like "kubectl edit".
//
// Note: object is an input+output parameter. It is updated to match the actual
// state on the apiserver.
func (c *Ctl) UpdateWithOptions(ctx context.Context, options UpdateOptions, object Object) error {
	logger := logging.S(ctx)

	// If caller did not specify the object's group/version/kind, fill it in now
	// based on the scheme.
	if err := c.fillDefaults(object); err != nil {
		return err
	}

	if !options.NoLastAppliedAnnotation {
		if err := c.errorOnAddonManager(object); err != nil {
			return err
		}
		if err := setLastApplied(object); err != nil {
			return err
		}
	}

	if err := c.fireBeforeMutation(object); err != nil {
		return err
	}

	gvk := object.GetObjectKind().GroupVersionKind()
	key := getObjectKeyOrPanic(object)

	// Update the existing object.
	if err := c.client.Update(ctx, object); err != nil {
		logger.Errorf("failed to update %s/%s: %v", gvk.Kind, key.Name, err)
		return err
	}

	logger.Infof("updating %s/%s", gvk.Kind, key.Name)

	if err := c.fireAfterMutation(object); err != nil {
		return err
	}

	if !options.NoWait {
		if options.DoneFunc == nil {
			// Wait until all objects are updated and ready.
			options.DoneFunc = IsReady
		}
		if err := c.WaitForWithOptions(ctx, options.WaitForOptions, object); err != nil {
			return err
		}

		logger.Infof("%s/%s ready", gvk.Kind, key.Name)
	}

	return nil
}

// Delete calls DeleteWithOptions with default options.
//
// Note: Unlike most methods in kube, DeleteWithOptions does not modify the
// object argument.
func (c *Ctl) Delete(ctx context.Context, object Object) error {
	return c.DeleteWithOptions(ctx, DeleteOptions{}, object)
}

// DeleteWithOptions ensures that the given K8s object is removed from the
// target cluster, if it exists there. If the NoWait option is false, then
// DeleteWithOptions blocks until the object is no longer present in the K8s
// metadata.
//
// Note: Unlike most methods in kube, DeleteWithOptions does not modify the
// object argument.
func (c *Ctl) DeleteWithOptions(ctx context.Context, options DeleteOptions, object Object) error {
	logger := logging.S(ctx)

	// Deep copy the object to avoid mutating the in-memory representation. This
	// mimics the behavior of client.Delete. It also sidesteps the question of
	// representing a deleted object in-memory. Ideally, the representation of a
	// deleted object is nil. But there is no ergonomic way to set the input
	// argument to nil.
	object = DeepCopy(object)

	// If caller did not specify the object's group/version/kind, fill it in now
	// based on the scheme.
	if err := c.fillDefaults(object); err != nil {
		return err
	}

	if err := c.fireBeforeMutation(object); err != nil {
		return err
	}

	deleteOpts := &client.DeleteOptions{
		GracePeriodSeconds: options.GracePeriodSeconds,
		PropagationPolicy:  &options.PropagationPolicy,
		Preconditions:      options.Preconditions,
	}

	// It's more ergonomic to accept a non-pointer version of
	// PropagationPolicy. If we get a zero string, nil out deleteOpts to
	// ensure that we're not passing an invalid policy.
	if *deleteOpts.PropagationPolicy == "" {
		deleteOpts.PropagationPolicy = nil
	}

	// Delete the object.
	gvk := object.GetObjectKind().GroupVersionKind()
	key := getObjectKeyOrPanic(object)
	if err := c.client.Delete(ctx, object, deleteOpts); err != nil {
		if kubeerrors.IsNotFound(err) {
			logger.Warnf("failed to delete %s/%s: %v", gvk.Kind, key.Name, err)
		} else {
			logger.Errorf("failed to delete %s/%s: %v", gvk.Kind, key.Name, err)
		}
		return err
	}
	logger.Infof("deleting %s/%s", gvk.Kind, key.Name)

	if err := c.fireAfterMutation(object); err != nil {
		return err
	}

	if !options.NoWait {
		if options.DoneFunc == nil {
			options.DoneFunc = IsDeleted
		}

		// Wait until all objects are deleted.
		if err := c.WaitForWithOptions(ctx, options.WaitForOptions, object); err != nil {
			return err
		}

		logger.Infof("object %s/%s deleted", gvk.Kind, key.Name)
	}

	return nil
}

// Drain marks the passed-in node as unschedulable, and then evicts all
// non-daemonset pods running on the node. The behavior emulates the behavior
// of `kubectl drain`:
// https://kubernetes.io/docs/tasks/administer-cluster/safely-drain-node/#use-kubectl-drain-to-remove-a-node-from-service
func (c *Ctl) Drain(ctx context.Context, node *corev1.Node) error {
	logging.Debugf(ctx, "draining node %s", node.Name)
	if err := c.Cordon(ctx, node); err != nil {
		return errors.Wrapf(err, "cordoning node %s", node.Name)
	}
	if err := c.evictPodsFromNode(ctx, node); err != nil {
		return errors.Wrap(err, "evict all pods from node")
	}
	return nil
}

// UpdateStatus triggers an update to the status sub-resource of a K8s object.
// This is not possible to do with other methods on kube.Ctl, because those only
// allow updating fields in the main Spec resource. This method is commonly
// called by a controller reconciliation loop once an object's current state has
// been reconciled with its desired state.
func (c *Ctl) UpdateStatus(ctx context.Context, object Object) error {
	logger := logging.S(ctx)

	// If caller did not specify the object's group/version/kind, fill it in now
	// based on the scheme.
	if err := c.fillDefaults(object); err != nil {
		return err
	}

	if err := c.fireBeforeMutation(object); err != nil {
		return err
	}

	// Update the object's status.
	gvk := object.GetObjectKind().GroupVersionKind()
	key := getObjectKeyOrPanic(object)

	if err := c.client.Status().Update(ctx, object); err != nil {
		logger.Errorf("failed to update status for %s/%s: %v", gvk.Kind, key.Name, err)
		return err
	}

	logger.Debugf("status updated for %s/%s", gvk.Kind, key.Name)

	if err := c.fireAfterMutation(object); err != nil {
		return err
	}

	return nil
}

// PatchStatus applies a merge patch to the object's status sub-resource. Unlike
// UpdateStatus, which replaces the entire status, PatchStatus only modifies the
// fields that differ between old and the current object. This is critical when
// multiple controllers write to different status fields on the same resource.
func (c *Ctl) PatchStatus(ctx context.Context, object Object, old Object) error {
	logger := logging.S(ctx)

	if err := c.fillDefaults(object); err != nil {
		return err
	}

	patch := client.MergeFrom(old)
	gvk := object.GetObjectKind().GroupVersionKind()
	key := getObjectKeyOrPanic(object)
	if err := c.client.Status().Patch(ctx, object, patch); err != nil {
		logger.Errorf("failed to patch status for %s/%s: %v", gvk.Kind, key.Name, err)
		return err
	}

	logger.Debugf("status patched for %s/%s", gvk.Kind, key.Name)
	return nil
}

// RollingRestart triggers a Kubernetes controlled rolling restart of the given
// object by modifying an annotation on the Pod template. RollingRestart will
// wait for the modified object to become ready but it will not wait for all
// outdated replicas to completely terminate. RollingRestart will panic if used
// on an unsupported object type.
func (c *Ctl) RollingRestart(ctx context.Context, object Object) error {
	logger := logging.S(ctx)

	// If caller did not specify the object's group/version/kind, fill it in now
	// based on the scheme.
	if err := c.fillDefaults(object); err != nil {
		return err
	}

	gvk := object.GetObjectKind().GroupVersionKind()
	key := getObjectKeyOrPanic(object)
	if err := c.Apply(ctx, RestartObject(object)); err != nil {
		logger.Errorf("failed to restart %s/%s: %v", gvk.Kind, key.Name, err)
		return err
	}

	logger.Infof("restarted %s/%s", gvk.Kind, key.Name)
	return nil
}

// fillDefaultGVK retrieves the group version kind from the schema and inserts
// it into the object. obj may be an ObjectList or a singular Object.
func (c *Ctl) fillDefaultGVK(obj runtime.Object) error {
	objKind := obj.GetObjectKind()
	objGvk := objKind.GroupVersionKind()

	// Ensure the GVK is set correctly.
	schemeGvk, err := c.getGVKFromScheme(obj)
	if err != nil {
		return err
	}
	if objGvk.Empty() {
		obj.GetObjectKind().SetGroupVersionKind(schemeGvk)
	} else if objGvk != schemeGvk {
		// Ensure object GKV matches the one found in the scheme.
		return fmt.Errorf("object GVK does not match scheme GVK: %v vs. %v", objGvk, schemeGvk)
	}
	return nil
}

// fillDefaults sets missing fields in the given fields that have sensible
// defaults.
//
//	namespace
//	  If the namespace is empty, fillDefaults will set it to "default" if the
//	  object is namespace-scoped. If the object is root-scoped, fillDefaults
//	  validates that the namespace is empty.
//
//	group-version-kind (GVK)
//	  If the GVK is missing, then it's fetched from the current scheme, if
//	  present there. If the GVK is present, then it must match the scheme.
func (c *Ctl) fillDefaults(obj Object) error {
	if err := c.fillDefaultGVK(obj); err != nil {
		return err
	}

	gvk := obj.GetObjectKind().GroupVersionKind()
	setNamespace, err := c.validateNamespace(gvk, obj.GetNamespace())
	if err != nil {
		return err
	}
	if setNamespace != "" {
		obj.SetNamespace(setNamespace)
	}

	return nil
}

// validateNamespace takes metadata for a K8s object, along with the requested
// namespace for that object. If the requested namespace does not match the
// Ctl namespace, validateNamespace returns an error. Here are the matching
// rules:
//
//  1. A root-scoped object should always use the "default" namespace.
//  2. A namespace-scoped object should always use a namespace that matches the
//     Ctl namespace.
//
// If the requested namespace is empty, then validateNamespace does no
// validation, but instead returns the expected matching namespace for the
// object.
func (c *Ctl) validateNamespace(objGvk schema.GroupVersionKind, objNs string) (string, error) {
	// If there is no mapper, then this is probably a testing scenario. Just
	// skip namespace validation.
	if c.mapper == nil {
		if c.namespace == "" {
			return "default", nil
		}
		return c.namespace, nil
	}

	// Ensure the namespace is set correctly on namespace-scoped objects.
	objGk := schema.GroupKind{Group: objGvk.Group, Kind: objGvk.Kind}
	mapping, err := c.mapper.RESTMapping(objGk, objGvk.Version)
	if err != nil {
		return "", err
	}

	isNamespaceScoped := mapping.Scope.Name() == meta.RESTScopeNameNamespace
	if objNs == "" {
		// Only set the namespace on namespace-scoped objects.
		if isNamespaceScoped {
			if c.namespace == "" {
				return "default", nil
			} else {
				return c.namespace, nil
			}
		}
	} else {
		// Validate namespace.
		if isNamespaceScoped {
			if c.namespace != "" && objNs != c.namespace {
				return "", fmt.Errorf("expected %s to have namespace '%s', got '%s'",
					objGvk.Kind, c.namespace, objNs)
			}
		} else {
			return "", fmt.Errorf("root-scoped object '%v' cannot have namespace '%s'", objGvk, objNs)
		}
	}

	return "", nil
}

// WaitFor is a helper method that calls WaitForWithOptions with default options.
func (c *Ctl) WaitFor(ctx context.Context, doneFunc DoneFunc, objects ...Object) error {
	return c.WaitForWithOptions(ctx, WaitForOptions{DoneFunc: doneFunc}, objects...)
}

// WaitForWithOptions blocks until all the given K8s objects are "done", as
// determined by "doneFunc" returning nil. If the K8s object is not present in
// the server, then "doneFunc" is passed nil. Otherwise, it's passed the latest
// server object state.
//
// Note: objects is an input+output parameter. Each poll updates the objects to
// match the state found on the server. The state of the objects is undefined if
// WaitForWithOptions returns an error. If the object was deleted,
// WaitForWithOptions will return the last successful poll results.
//
// This command behaves like "kubectl wait".
// TODO(andyk): Make this use watches instead of polling.
func (c *Ctl) WaitForWithOptions(
	ctx context.Context, opts WaitForOptions, objects ...Object,
) error {
	if opts.BackOff == nil || c.disallowCustomBackOffs {
		opts.BackOff = c.backOff
	}

	if opts.DoneFunc == nil {
		panic("WaitForWithOptions invoked with no DoneFunc")
	}

	// Do not do anything if there are no objects to wait.
	if len(objects) == 0 {
		return nil
	}

	// Log what we're waiting on every N seconds.
	logger := logging.S(ctx)
	const interval = 10 * time.Second
	start := time.Now()
	next := start.Add(interval)
	var lastShownStatus string

	backoffCtx := backoff.WithContext(opts.BackOff(), ctx)

	for len(objects) > 0 {
		// Check to see if object is done before the Get call.
		object := objects[0]

		// Get object kind and name for logging and error messages.
		gvk, err := c.getGVKFromScheme(object)
		if err != nil {
			return err
		}
		key := getObjectKeyOrPanic(object)

		doneErr := opts.DoneFunc(object)
		if doneErr != nil {
			// Retry with the latest object state from server.
			if err := c.Get(ctx, key.Name, object); err != nil {
				// If object is not found, then pass nil to doneFunc.
				if kubeerrors.IsNotFound(err) {
					object = nil
				} else {
					return errors.Wrapf(err, "failed to get %s/%s", gvk.Kind, key.Name)
				}
			}
			doneErr = opts.DoneFunc(object)
		}

		if doneErr == nil {
			// Remove any done object from the slice, so it doesn't need to be
			// checked again.
			objects = objects[1:]
			next = start.Add(interval)
			if lastShownStatus != "" {
				logger.Infof("%s/%s done waiting after %s", gvk.Kind, key.Name,
					time.Since(start).Round(time.Second))
			}
			lastShownStatus = ""
			continue
		}

		// Occasionally log progress so it's easy to see what kube.Ctl is
		// currently waiting on.
		if now := time.Now(); now.After(next) {
			if lastShownStatus != doneErr.Error() {
				logger.Infof("%s/%s waiting: %s", gvk.Kind, key.Name, doneErr.Error())
				lastShownStatus = doneErr.Error()
				next = now.Add(interval)
			}
		}

		if errors.Is(doneErr, ErrAbortWait) {
			return errors.Wrapf(doneErr, "aborting wait for %s/%s", gvk.Kind, key.Name)
		}

		duration := backoffCtx.NextBackOff()
		if duration == backoff.Stop {
			return errors.Wrapf(doneErr, "timed out waiting for %s/%s", gvk.Kind, key.Name)
		}
		time.Sleep(duration)
	}
	return nil
}

// Exec executes a command within a container of a Pod ala kubectl exec.
// NOTE: Exec is not compatible with the FakeCtl nor kuberecord.
func (c *Ctl) Exec(ctx context.Context, pod, container string, opts ExecOptions) error {
	// For ergonomics, leverage the namespace set at the Ctl level. If we find
	// that it's not set, error early.
	if c.namespace == "" {
		return errors.New(".Namespace must have been called with a non-empty value to use Exec")
	}

	// Make a shallow copy of the rest config as we need to set some specific
	// fields on it.
	restConfig := *c.RestConfig()

	// Take from the typed clientset's initialization code for the Corev1
	// interface.
	restConfig.GroupVersion = &corev1.SchemeGroupVersion
	restConfig.APIPath = "/api"
	restConfig.NegotiatedSerializer = scheme.Codecs.WithoutConversion()

	client, err := rest.RESTClientFor(&restConfig)
	if err != nil {
		return errors.WithStack(err)
	}

	req := client.Post().
		Resource("pods").
		Name(pod).
		Namespace(c.namespace).
		SubResource("exec")

	req.VersionedParams(&corev1.PodExecOptions{
		Command:   opts.Command,
		Container: container,
		Stdin:     opts.Stdin != nil,
		Stdout:    opts.Stdout != nil,
		Stderr:    opts.Stderr != nil,
		TTY:       opts.TTY,
	}, runtime.NewParameterCodec(c.Scheme()))

	exec, err := remotecommand.NewSPDYExecutor(c.RestConfig(), "POST", req.URL())
	if err != nil {
		return errors.Wrapf(err, "failed to initialize SPDY executor")
	}

	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  opts.Stdin,
		Stdout: opts.Stdout,
		Stderr: opts.Stderr,
		Tty:    opts.TTY,
	})
}

// GetContainerLogs retrieves logs from a container with configurable limits and options.
func (c *Ctl) GetContainerLogs(
	ctx context.Context, podName, containerName string, options GetContainerLogsOptions,
) (string, error) {
	// For ergonomics, leverage the namespace set at the Ctl level. If we find
	// that it's not set, error early.
	if c.namespace == "" {
		return "", errors.New(".Namespace must have been called with a non-empty value to use logs")
	}

	// Make a shallow copy of the rest config as we need to set some specific
	// fields on it.
	restConfig := *c.RestConfig()

	// Take from the typed clientset's initialization code for the Corev1
	// interface.
	restConfig.GroupVersion = &corev1.SchemeGroupVersion
	restConfig.APIPath = "/api"
	restConfig.NegotiatedSerializer = scheme.Codecs.WithoutConversion()

	client, err := rest.RESTClientFor(&restConfig)
	if err != nil {
		return "", errors.WithStack(err)
	}

	// Build the request with container parameter
	req := client.Get().
		Namespace(c.namespace).
		Name(podName).
		Resource("pods").
		SubResource("log").
		Param("container", containerName)

	// Apply LimitBytes with 1MB default if not specified
	limitBytes := int64(DefaultLimitBytes)
	if options.LimitBytes != nil {
		limitBytes = *options.LimitBytes
	}
	req = req.Param("limitBytes", fmt.Sprintf("%d", limitBytes))

	readCloser, err := req.Stream(ctx)
	if err != nil {
		return "", errors.WithStack(err)
	}

	defer readCloser.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(readCloser); err != nil {
		return "", errors.WithStack(err)
	}

	return buf.String(), nil
}

// FetchJobPodLogs returns up to 32 KiB of logs from the newest Pod created by a
// Kubernetes Job. Jobs can retry with a new Pod, so an older Pod may not contain
// the failure that produced the terminal Job status. Log collection is best
// effort and returns a clear message instead of replacing the reconciliation
// result when the Pod or its logs are already gone.
func (c *Ctl) FetchJobPodLogs(ctx context.Context, job *batchv1.Job) string {
	podSelector := labels.Set{"job-name": job.Name}
	if job.UID != "" {
		podSelector["controller-uid"] = string(job.UID)
	}
	var pods corev1.PodList
	if err := c.List(ctx, &ListOptions{
		Namespace:     job.Namespace,
		LabelSelector: podSelector.AsSelector(),
		Limit:         10,
	}, &pods); err != nil {
		return fmt.Sprintf("could not get logs for Job %s/%s because its Pods could not be listed: %v", job.Namespace, job.Name, err)
	}
	if len(pods.Items) == 0 {
		return fmt.Sprintf("could not get logs for Job %s/%s because no Pods were found", job.Namespace, job.Name)
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[i].CreationTimestamp.After(pods.Items[j].CreationTimestamp.Time)
	})

	newestPod := &pods.Items[0]
	if len(newestPod.Spec.Containers) == 0 {
		return fmt.Sprintf("could not get logs for Job %s/%s because Pod %s has no containers", job.Namespace, job.Name, newestPod.Name)
	}
	// Lifecycle Jobs have one container, but malformed Pods must remain safe.
	jobContainer := newestPod.Spec.Containers[0]
	const logLimitBytes = int64(32 * 1024)
	logs, err := c.WithNamespace(job.Namespace).GetContainerLogs(ctx, newestPod.Name, jobContainer.Name, GetContainerLogsOptions{
		LimitBytes: new(logLimitBytes),
	})
	if err != nil {
		return fmt.Sprintf("could not get logs for Job %s/%s from Pod %s container %s: %v", job.Namespace, job.Name, newestPod.Name, jobContainer.Name, err)
	}
	if logs == "" {
		return fmt.Sprintf("Job %s/%s Pod %s container %s produced no logs", job.Namespace, job.Name, newestPod.Name, jobContainer.Name)
	}
	return logs
}

// getGVKFromScheme determines an object's GroupVersionKind from the scheme.
func (c *Ctl) getGVKFromScheme(object runtime.Object) (schema.GroupVersionKind, error) {
	return GVKFromScheme(c.scheme, object)
}

// errorOnAddonManager returns an error if the given object is managed by
// Kubernetes addon-manager. addon-manager is a k8s component that creates and
// maintains components in a hosted/managed kubernetes cluster. If the managed
// object is modified, all the changes will get reverted when the addon-manager
// runs a reconcile loop. To spare others the pain of discovering this
// themselves, we reject some calls to objects with this label. If you do need
// to modify an object managed by addon-manager, instead use
// UpdateWithOptions(NoLastAppliedAnnotation=true) and be very careful to
// ensure that changes actually stay around.
// https://github.com/kubernetes/kubernetes/blob/master/cluster/addons/addon-manager/README.md
func (c *Ctl) errorOnAddonManager(serverObj Object) error {
	// Get object kind and name for logging and error messages.
	gvk, err := c.getGVKFromScheme(serverObj)
	if err != nil {
		return err
	}
	key := getObjectKeyOrPanic(serverObj)

	labels, err := metadataAccessor.Labels(serverObj)
	if err != nil {
		return errors.Wrapf(err, "unable to read labels from %s/%s", gvk.Kind, key.Name)
	}

	if labels["addonmanager.kubernetes.io/mode"] == "Reconcile" {
		return errors.New("addon-manager managed objects are not supported by Apply or Update. Use UpdateWithOptions(NoLastAppliedAnnotation=true)")
	}

	return nil
}

// fireBeforeMutation invokes the beforeMutation callback function just before a
// K8s object is created, updated, or deleted. This gives the caller a chance to
// trigger custom logic.
func (c *Ctl) fireBeforeMutation(object Object) error {
	if c.beforeMutation != nil {
		// fillDefaults has already succeeded, so no chance of error now.
		if err := c.fillDefaults(object); err != nil {
			panic(err)
		}
		return c.beforeMutation(object)
	}
	return nil
}

// fireAfterMutation invokes the afterMutation callback function when a K8s
// object is created, updated, or deleted. This gives the caller a chance to
// trigger custom logic.
func (c *Ctl) fireAfterMutation(object Object) error {
	if c.afterMutation != nil {
		// fillDefaults has already succeeded, so no chance of error now.
		if err := c.fillDefaults(object); err != nil {
			panic(err)
		}
		return c.afterMutation(object)
	}
	return nil
}

func ensureReplacementReady(oldUID types.UID, pod *corev1.Pod) error {
	if pod.ObjectMeta.UID == oldUID {
		return errors.New("pod is not yet evicted")
	}
	return IsPodReady(pod)
}

func (c *Ctl) evict(ctx context.Context, pods ...corev1.Pod) error {
	podsToWaitFor := make([]Object, 0, len(pods))
	oldUIDs := make(map[client.ObjectKey]types.UID, len(pods))

	allPDBs, err := c.getPodDisruptionBudgets(ctx)
	if err != nil {
		return errors.Wrap(err, "getting Pod Disruption budgets")
	}

	for _, pod := range pods {
		// Important: identify these _before_ evicting, so matches can actually be detected.
		affectedPDBs, err := c.affectedPDBsForPod(allPDBs, &pod)
		if err != nil {
			return errors.Wrap(err, "getting affected PDBs for pods")
		}

		if err := c.waitForDisruptionAllowed(ctx, affectedPDBs); err != nil {
			return errors.Wrap(err, "waiting for PDB disruption allowed")
		}

		err = c.EvictPod(ctx, &pod, metav1.DeleteOptions{})
		if kubeerrors.IsNotFound(err) {
			// pod vanished before we could evict it, nothing to wait for
		} else if err != nil {
			return errors.Wrapf(err, "POSTing eviction for pod %s", pod.Name)
		} else {
			podsToWaitFor = append(podsToWaitFor, &pod)
			oldUIDs[getObjectKeyOrPanic(&pod)] = pod.ObjectMeta.UID
		}

		if err := c.waitForDisruptionAllowed(ctx, affectedPDBs); err != nil {
			return errors.Wrap(err, "waiting for PDB disruption allowed")
		}
	}

	isEvicted := func(object Object) error {
		if object == nil {
			// pod is gone, eviction succeeded
			return nil
		}

		curr, ok := object.(*corev1.Pod)
		if !ok {
			return backoff.Permanent(errors.New("object is not a pod"))
		}

		oldUID, ok := oldUIDs[getObjectKeyOrPanic(curr)]
		if !ok {
			return backoff.Permanent(errors.New("old pod uid not found"))
		}

		return ensureReplacementReady(oldUID, curr)
	}

	options := WaitForOptions{
		DoneFunc: isEvicted,
		BackOff:  NewCtlDefaultBackOffFn(DrainWaitDuration(pods)),
	}
	return c.WaitForWithOptions(ctx, options, podsToWaitFor...)
}

// EvictPod tries to evict a pod using the supplied delete options, and
// returns the k8s error back to the caller (i.e., errors.StatusError).
// The caller is responsible for inspecting the errors and handling them
// correctly.
// The caller can use options to customize the eviction, e.g., setting
// the preconditions to make sure the right pod is evicted.
func (c *Ctl) EvictPod(ctx context.Context, pod *corev1.Pod, options metav1.DeleteOptions) error {
	logger := logging.S(ctx)
	// Evictions must be created via the REST client.
	gvk := schema.GroupVersionKind{
		Group:   "policy",
		Version: "v1beta1",
		Kind:    "Eviction",
	}
	key := getObjectKeyOrPanic(pod)
	logger.Infof("Evicting %s/%s", gvk.Kind, key.Name)

	if c.evictionUsesDeletion {
		return c.DeleteWithOptions(ctx, DeleteOptions{NoWait: true}, pod)
	}

	restClient, err := c.gvkRestClient(gvk)
	if err != nil {
		return errors.Wrapf(err, "getting gvkRestClient for gvk %v", gvk)
	}
	eviction := &policyv1beta1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
		DeleteOptions: &options,
	}
	return errors.WithStack(restClient.Post().
		AbsPath("/api/v1").
		Namespace(eviction.Namespace).
		Resource("pods").
		Name(eviction.Name).
		SubResource("eviction").
		Body(eviction).
		Do(ctx).
		Error())
}

// RestClientForObjType determines the GVK for the provided objType and constructs
// a rest client for that type of resource.
func (c *Ctl) RestClientForObjType(
	objType Object,
) (rest.Interface, schema.GroupVersionKind, error) {
	// Get the GroupValueKind of the object.
	gvk, err := GVKFromScheme(c.Scheme(), objType)
	if err != nil {
		return nil, schema.GroupVersionKind{}, err
	}

	// Set up a REST client that will be used to create the watch.
	restClient, err := c.gvkRestClient(gvk)
	if err != nil {
		return nil, gvk, err
	}

	return restClient, gvk, nil
}

func shouldAlwaysRedact(gvk schema.GroupVersionKind) bool {
	return (gvk.Kind == "Secret" ||
		gvk.Kind == "ConfigMap")
}

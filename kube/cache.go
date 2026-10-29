package kube

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	opmetrics "github.com/cockroachlabs/cockroach-enterprise-operator/controllers/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// GetResourceFunc defines a callback function to fetch a named K8s resource.
type GetResourceFunc func(
	ctx context.Context, key client.ObjectKey, options *metav1.ListOptions,
) (Object, error)

// WatchResourcesFunc defines a callback function to establish a watch for some
// K8s resource.
type WatchResourcesFunc func(
	ctx context.Context, options *metav1.ListOptions,
) (watch.Interface, error)

// Cache is a look through cache for a chosen type of Kubernetes resource (e.g.
// Pods or Deployments). Lookups return the local copy of a resource if
// available, else the cache will query the server to get the resource (and then
// add it to the cache for future lookups). Updates to server objects will
// eventually be reflected in the local cache by using a K8s watch to detect the
// changes and invalidate the local copies. If not invalidated, a resource stays
// in the cache for a configured TTL duration since the last time it was looked
// up. Resources can be cached from any K8s namespace, or the caller can
// configure the cache to target a single namespace. Example usage:
//
//	ctx, cancel := context.WithCancel(context.Background())
//	defer cancel()
//	cache, err := NewCache(ctl, &corev1.Pod{})
//	go cache.Run(ctx)
//	obj, err := cache.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "my-pod"})
//
// The cache makes the following guarantees:
//  1. Calls to Get may return a slightly out-of-date version of a resource
//     temporarily, but will always *eventually* return the latest version,
//     given time and assuming the K8s watch is operating normally.
//  2. If one caller invokes Get and gets back some version of a resource, then
//     all callers that invoke Get after the first call has returned are
//     guaranteed to return that version or a later version (i.e. resource
//     versions returned by Get will never "go backwards").
//  3. These rules work for any kind of resource mutation, including deletions.
//     In case of deletion, Get will eventually return a "not found" error,
//     since the cache will eventually learn of the resource's deletion and
//     remove its local copy.
//
// NOTE: Be careful not to abuse Cache; watches are fairly expensive to
// maintain and put load on the K8s server. Only use this if the alternatives
// have greater problems or put greater load on the server.
type Cache struct {
	// Options can be used to specify a subset of resources that will be managed
	// by the cache by setting Options.LabelSelector or Options.FieldSelector.
	// NOTE: This should not be set after calling Run.
	Options metav1.ListOptions

	// TTL is the amount of time that an un-accessed object will be retained in
	// the cache. Note that an object can stay in the cache indefinitely as long
	// as it is periodically accessed (via Get), at least this often. A TTL of
	// zero means there is no limit (though dirty objects will still be expelled
	// from the cache).
	// NOTE: This should not be set after calling Run.
	TTL time.Duration

	// GetResource is called to fetch a resource with the given key from the
	// server. If nil, a K8s REST API client will be used to fetch the resource.
	// NOTE: This should not be set after calling Run.
	GetResource GetResourceFunc

	// WatchResources is called to establish a K8s watcher that reports changes
	// to resources on the server. If nil, a K8s REST API client will be used to
	// establish the watcher.
	// NOTE: This should not be set after calling Run.
	WatchResources WatchResourcesFunc

	// ctl is the kube.Ctl instance passed to the constructor.
	ctl *Ctl

	// objType is the type of K8s resource stored in the cache.
	objType Object

	mu struct {
		sync.RWMutex

		// objs is an ordered list of object entries, with the least recently
		// accessed entries at the front of the list. Entries that have not been
		// accessed in the TTL interval will be removed from the cache.
		objs *list.List

		// objsByKey indexes the objs list by namespace + name.
		objsByKey map[client.ObjectKey]*list.Element

		// dirtyCount is incremented each time a resource is marked as dirty.
		dirtyCount int

		// restClient is used to access the K8s REST API.
		restClient rest.Interface

		// resource is the K8s resource name of objects in the cache, which is
		// used to call the K8s API (e.g. pods or crdbtenants).
		resource string
	}
}

type objectEntry struct {
	// Object is the cached K8s resource.
	Object Object
	// AccessedAt records the last time Get was called on this object.
	AccessedAt time.Time
}

// NewCache constructs a new cache for K8s resources of the given type. If the
// kube.Ctl is scoped to a single namespace, then only objects from that
// namespace are cached. Otherwise, objects from all namespaces are cached.
// Callers that wish to restrict the set of resources further can set
// Cache.Options (e.g. LabelSelector or FieldSelector).
// NOTE: Callers must call Run on a background goroutine in order to get updates
// to the objects in the cache.
func NewCache(ctl *Ctl, objType Object) *Cache {
	cache := &Cache{
		ctl:     ctl,
		objType: objType,
	}
	cache.GetResource = cache.getResource
	cache.WatchResources = cache.watchResources
	cache.mu.objs = list.New()
	cache.mu.objsByKey = make(map[client.ObjectKey]*list.Element)
	return cache
}

// Run establishes a watch on resources for the cache so that it can invalidate
// any cached resources that have been updated on the server. Run should be
// called after creating the cache on a background goroutine. It will run until
// the given context is canceled. If it encounters errors establishing or
// receiving from the watch, it logs the errors and tries to re-establish the
// watch.
func (c *Cache) Run(ctx context.Context) error {
	var watcher watch.Interface
	var err error

	defer func() {
		if watcher != nil {
			watcher.Stop()
		}
	}()

	for {
		if watcher == nil {
			options := c.Options
			options.Watch = true
			watcher, err = c.WatchResources(ctx, &options)
			if err != nil {
				logging.Errorf(ctx, "error establishing watch: %v", err)
				if !SleepContext(ctx, time.Second) {
					// Cache has been stopped.
					return nil
				}
				continue
			}
			logging.Info(ctx, "watch established")

			gvk, err := GVKFromScheme(c.ctl.Scheme(), c.objType)
			var resourceLabel prometheus.Labels
			if err != nil {
				// If we cannot determine the GVK of the object, we'll still emit the
				// WatcherEstablishedTotal metric, but with a {"resource": "unknown"} label.
				logging.Errorf(ctx, "cannot increment watcher metric with appropriate resource label, "+
					"failed to get the GVK of %T cache object: %v", c.objType, err)
				resourceLabel = prometheus.Labels{"resource": "unknown"}
			} else {
				resourceLabel = opmetrics.ResourceLabel(gvk)
			}
			opmetrics.WatcherEstablishedTotal.With(resourceLabel).Inc()
		}

		var event watch.Event
		select {
		case event = <-watcher.ResultChan():

		case <-ctx.Done():
			// Cache has been stopped.
			return nil
		}

		// If the watcher fails or is closed, log and try to re-establish
		// the watch.
		if event.Type == watch.Error || event.Type == "" {
			// The watcher gets closed very frequently, possibly due to a limit
			// on the watch stream. When that happens, event.Object is nil. Skip
			// logging on those events.
			if event.Object != nil {
				logging.Errorf(ctx, "watcher closed or error: %v", event.Object)
			}
			watcher.Stop()
			watcher = nil
			continue
		}

		// TODO(jeffswenson): add a watch api to kube.Ctl to avoid the need to
		// cast to kube.Object.
		eventObject, err := FromRuntime(event.Object)
		if err != nil {
			logging.Error(ctx, fmt.Sprintf("unexpected object type: %v", event.Object), zap.Error(err))
			watcher.Stop()
			watcher = nil
			continue
		}

		// For all mutation event types, mark the object as dirty in the cache;
		// ignore the actual object data.
		switch event.Type {
		case watch.Added, watch.Modified, watch.Deleted:
			c.MarkAsDirty(eventObject)
		}
	}
}

// Count returns the number of object that are in the cache.
// NOTE: Count is thread-safe.
func (c *Cache) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.mu.objs.Len()
}

// Get looks up a resource by its key (namespace + name). If the resource is
// present in the cache, then Get returns that copy (which might be slightly
// stale). If it's not in the cache, then Get fetches it from the server.
// NOTE: Get is thread-safe.
func (c *Cache) Get(ctx context.Context, key client.ObjectKey) (Object, error) {
	if c.ctl.Namespace() != "" && key.Namespace != c.ctl.Namespace() {
		return nil, errors.Newf("namespace %s passed to Get does not match cache namespace %s",
			key.Namespace, c.ctl.Namespace())
	}

	// First try to find object in cache. If found, return it immediately.
	obj := c.find(key)
	if obj != nil {
		return obj, nil
	}

	// Else try to fetch it from the server.
	dirtyCount := c.startFetch()

	obj, err := c.GetResource(ctx, key, &c.Options)
	if err != nil {
		return nil, err
	}

	return c.endFetch(obj, dirtyCount), nil
}

// startFetch returns the current dirty count. endFetch will use this to
// determine whether any objects were invalidated while the fetch was in
// progress.
func (c *Cache) startFetch() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mu.dirtyCount
}

// endFetch enters the fetched object into the cache, as long as it can
// guarantee it has not been invalidated by the watcher. If any objects have
// been marked as dirty while the fetch was in progress, then the fetched object
// might be invalid, so don't add it to the cache.
//
// endFetch returns the object that should be returned to the caller. This might
// be different than "fetched" in the case of two concurrent callers. Imagine
// that an object is updated during the concurrent fetches from v1 to v2. Caller
// A fetches v1 and caller B fetches v2. If caller A finishes first and enters
// v1 into the cache, then it would be incorrect for B to then return v2. That
// would be a violation of guarantee #2, since if caller B calls Get again, it
// would get the older cached v1 version (and "go backwards"). To prevent this,
// always return any object already added to the cache by a concurrent fetcher.
// Also, if the fetched object might be dirty, do not allow concurrent fetchers
// to add their fetched object to the cache; prevent that by incrementing
// dirtyCount.
//
// TODO(andyk): There is a problem with using the "increment dirtyCount" trick.
// If there are lots of concurrent fetchers, then this could lead to starvation,
// where no fetcher is ever able to add anything to the cache, since each
// fetcher interferes with the next, etc. If this is ever a problem, we could
// address by, e.g., ensuring that only one goroutine can fetch an object of a
// particular name at a time.
func (c *Cache) endFetch(fetched Object, startDirtyCount int) Object {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := getObjectKeyOrPanic(fetched)
	existing, ok := c.mu.objsByKey[key]
	if ok {
		// Another goroutine raced with this one, so use the object it faced in
		// order to fulfill guarantee #2.
		return existing.Value.(*objectEntry).Object
	}

	// Add the object to the cache if it's guaranteed to not be dirty.
	if c.mu.dirtyCount == startDirtyCount {
		entry := &objectEntry{AccessedAt: timeutil.Now(), Object: fetched}
		elem := c.mu.objs.PushBack(entry)
		c.mu.objsByKey[key] = elem
	} else {
		// Object may be dirty, so also prevent all concurrent fetchers from
		// adding their objects to the cache, since it's not known which is the
		// most recent version.
		c.mu.dirtyCount++
	}

	return fetched
}

// find looks for the given object in the cache and returns it if it exists,
// else it returns nil.
func (c *Cache) find(key client.ObjectKey) Object {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if elem, ok := c.mu.objsByKey[key]; ok {
		return elem.Value.(*objectEntry).Object
	}
	return nil
}

// MarkAsDirty is called to invalidate the object. Any copy of that resource
// with the same version is removed from the cache and any objects returned by
// in-progress fetches are marked as dirty.
func (c *Cache) MarkAsDirty(obj Object) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := getObjectKeyOrPanic(obj)
	now := timeutil.Now()
	c.mu.dirtyCount++

	// Remove the dirty entry from the objs list if it exists, in order to force
	// the next Get caller to refetch from the K8s server.
	if objElem, ok := c.mu.objsByKey[key]; ok {
		// No need to remove the entry if its resource version matches the one
		// received by the watcher.
		existingVersion := getAccessorOrPanic(objElem.Value.(*objectEntry).Object).GetResourceVersion()
		newObj := getAccessorOrPanic(obj)
		if newObj.GetDeletionTimestamp() != nil || newObj.GetResourceVersion() != existingVersion {
			c.mu.objs.Remove(objElem)
			delete(c.mu.objsByKey, key)
		}
	}

	// Remove expired entries from the object list if a TTL is specified.
	if c.TTL != 0 {
		for c.mu.objs.Len() != 0 {
			front := c.mu.objs.Front()
			entry := front.Value.(*objectEntry)
			expires := entry.AccessedAt.Add(c.TTL)
			if now.Before(expires) {
				// Entry not yet expired; because list is sorted all subsequent
				// entries also cannot be expired.
				break
			}
			c.mu.objs.Remove(front)
			delete(c.mu.objsByKey, getObjectKeyOrPanic(entry.Object))
		}
	}
}

// getResource is the default implementation of the GetResource method, which
// uses the K8s REST API client to fetch an object with the given key from the
// K8s server.
func (c *Cache) getResource(
	ctx context.Context, key client.ObjectKey, options *metav1.ListOptions,
) (Object, error) {
	restClient, resource, err := c.ensureRESTClient()
	if err != nil {
		return nil, err
	}
	runtimeObject, err := restClient.Get().
		Namespace(key.Namespace).
		Name(key.Name).
		Resource(resource).
		VersionedParams(options, metav1.ParameterCodec).
		Do(ctx).
		Get()
	if err != nil {
		return nil, err
	}
	return FromRuntime(runtimeObject)
}

// watchResources is the default implementation of the WatchResources method,
// which establishes a K8s watch using K8s REST API client.
func (c *Cache) watchResources(
	ctx context.Context, options *metav1.ListOptions,
) (watch.Interface, error) {
	restClient, resource, err := c.ensureRESTClient()
	if err != nil {
		return nil, err
	}
	req := restClient.Get()
	if c.ctl.Namespace() != "" {
		req = req.Namespace(c.ctl.Namespace())
	}
	return req.
		Resource(resource).
		VersionedParams(options, metav1.ParameterCodec).
		Watch(ctx)
}

// ensureRESTClient lazily constructs the K8s REST API client and returns it,
// along with the K8s resource name that can be used to access resources via the
// client.
// NOTE: This method is thread-safe.
func (c *Cache) ensureRESTClient() (restClient rest.Interface, resource string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Construct REST client and resource name if they are not yet available.
	if c.mu.restClient == nil {
		// Get the GroupValueKind of the object.
		gvk, err := GVKFromScheme(c.ctl.Scheme(), c.objType)
		if err != nil {
			return nil, "", errors.Wrapf(err, "cannot get the GVK of %T cache object", c.objType)
		}

		// Get API resource name for objects of this kind.
		mapping, err := c.ctl.RestMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return nil, "", errors.Wrapf(err, "cannot get the API resource name for %v objects", gvk)
		}
		c.mu.resource = mapping.Resource.Resource

		// Set up a REST client that will be used to create the watch.
		codecs := serializer.NewCodecFactory(c.ctl.Scheme())
		c.mu.restClient, err = apiutil.RESTClientForGVK(gvk, false /*isUnstructured*/, c.ctl.RestConfig(), codecs)
		if err != nil {
			return nil, "", errors.Wrapf(err, "cannot create the REST client for %v objects", gvk)
		}
	}

	return c.mu.restClient, c.mu.resource, nil
}

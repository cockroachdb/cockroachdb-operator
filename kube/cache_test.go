package kube_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	opmetrics "github.com/cockroachlabs/cockroach-enterprise-operator/controllers/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/testenv"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestIntegrationCache(t *testing.T) {
	// This test doesn't work with kuberecord because it uses watches.
	testutils.MarkIntegrationTest(t)

	ctx := context.Background()
	env := testenv.New(t, testenv.SingleZoneConfig)
	ctl := env.DefaultCluster().Ctl
	ns1 := testenv.EnsureCustomNamespace(t, ctl, "cache-ns1")
	ns2 := testenv.EnsureCustomNamespace(t, ctl, "cache-ns2")

	role1 := rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "my-role",
			Labels: labels.Set{"test-role": "true"},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get"},
			},
		},
	}
	role2 := role1

	require.NoError(t, ctl.WithNamespace(ns1).Apply(ctx, &role1))
	require.NoError(t, ctl.WithNamespace(ns2).Apply(ctx, &role2))

	cache := kube.NewCache(ctl, &rbacv1.Role{})
	cache.Options.LabelSelector = "test-role=true"

	// Run cache on background goroutine.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := cache.Run(ctx); err != nil {
			t.Errorf("error running cache: %v", err)
		}
	}()

	// Wait for dirty notifications for both roles to arrive.
	require.Eventually(t, func() bool {
		return cache.DirtyCount() == 2
	}, 5*time.Second, time.Millisecond)

	// Call Get to populate cache.
	key1 := client.ObjectKey{Namespace: ns1, Name: "my-role"}
	obj, err := cache.Get(ctx, key1)
	require.NoError(t, err)
	require.Equal(t, "my-role", obj.(*rbacv1.Role).Name)
	require.Equal(t, ns1, obj.(*rbacv1.Role).Namespace)
	require.Equal(t, 1, cache.Count())

	key2 := client.ObjectKey{Namespace: ns2, Name: "my-role"}
	obj, err = cache.Get(ctx, key2)
	require.NoError(t, err)
	require.Equal(t, "my-role", obj.(*rbacv1.Role).Name)
	require.Equal(t, ns2, obj.(*rbacv1.Role).Namespace)
	require.Equal(t, 2, cache.Count())

	// Update the objects and ensure that watcher picks up changes.
	role1.Labels["version"] = "v2"
	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, &role1))
	require.NoError(t, ctl.DeleteWithOptions(ctx, kube.DeleteOptions{NoWait: true}, &role2))

	require.Eventually(t, func() bool {
		obj, err = cache.Get(ctx, key1)
		require.NoError(t, err)
		return obj.(*rbacv1.Role).Labels["version"] == "v2"
	}, 30*time.Second, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		_, err = cache.Get(ctx, key2)
		return kubeerrors.IsNotFound(err)
	}, 30*time.Second, 10*time.Millisecond)
}

func TestCache(t *testing.T) {
	suite.Run(t, new(CacheTestSuite))
}

type CacheTestSuite struct {
	suite.Suite

	pod1         corev1.Pod
	pod1Modified corev1.Pod
	pod1Deleted  corev1.Pod
	pod2         corev1.Pod
	pod3         corev1.Pod
	ctl          *kube.Ctl

	// getResource intercepts calls to Cache.GetResource for testing purposes.
	getResource kube.GetResourceFunc
}

func (s *CacheTestSuite) SetupSuite() {
	// Set up some pods used by the tests.
	s.pod1 = corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod", Namespace: "ns1", ResourceVersion: "v1"},
	}
	s.pod1Modified = s.pod1
	s.pod1Modified.ResourceVersion = "v2"
	s.pod1Deleted = s.pod1Modified
	s.pod1Deleted.DeletionTimestamp = &metav1.Time{}
	s.pod2 = corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod2", Namespace: "ns2", ResourceVersion: "v1"},
	}
	// Use same name as pod1, but in different namespace.
	s.pod3 = corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod", Namespace: "ns3", ResourceVersion: "v1"},
	}
}

// An object is not considered dirty as long as the fetch of a new version
// starts after the dirty notification was received.
func (s *CacheTestSuite) TestPodAfterDirtyEntry() {
	cache, watcher := s.createCache("", 0)
	s.runCache(cache)

	watcher.Add(&s.pod1)
	s.waitForDirtyCount(cache, 1)
	s.testCacheMiss(cache, &s.pod1)
	s.testCacheHit(cache, &s.pod1)
	s.testCacheHit(cache, &s.pod1)
}

// A cached object must be invalidated by modification made to server object.
func (s *CacheTestSuite) TestModifyInvalidation() {
	cache, watcher := s.createCache("", 0)
	s.runCache(cache)

	s.testCacheMiss(cache, &s.pod1)
	watcher.Modify(&s.pod1Modified)
	s.waitForDirtyCount(cache, 1)
	s.testCacheMiss(cache, &s.pod1Modified)
	s.Equal(1, cache.Count())
}

// A cached object must be invalidated by server object being deleted.
func (s *CacheTestSuite) TestDeleteInvalidation() {
	cache, watcher := s.createCache("", 0)
	cache.GetResource = func(
		ctx context.Context, key client.ObjectKey, options *metav1.ListOptions,
	) (kube.Object, error) {
		return nil, errors.New("some error")
	}
	s.runCache(cache)

	watcher.Delete(&s.pod1Deleted)
	s.waitForDirtyCount(cache, 1)

	key := client.ObjectKey{Namespace: s.pod1.Namespace, Name: s.pod1.Name}
	s.Eventually(func() bool {
		_, err := cache.Get(context.Background(), key)
		return err != nil && err.Error() == "some error"
	}, 5*time.Second, time.Millisecond)
	s.Equal(0, cache.Count())
}

// A dirty notification does not invalidate an object if it has not changed
// (i.e. has same ResourceVersion).
func (s *CacheTestSuite) TestMatchingResourceVersions() {
	cache, watcher := s.createCache("", 0)
	s.runCache(cache)

	s.testCacheMiss(cache, &s.pod1)
	watcher.Modify(&s.pod1)
	s.waitForDirtyCount(cache, 1)
	s.testCacheHit(cache, &s.pod1)
}

// If a fetch is in progress and a dirty notification arrives, the results
// of the fetch must be assumed to be dirty.
func (s *CacheTestSuite) TestFetchBeforeDirtyNotification() {
	cache, watcher := s.createCache("", 0)
	s.runCache(cache)

	key := client.ObjectKey{Namespace: s.pod1.Namespace, Name: s.pod1.Name}
	s.getResource = func(
		ctx context.Context, key client.ObjectKey, options *metav1.ListOptions,
	) (kube.Object, error) {
		// Invalidate the pod during the fetch.
		watcher.Modify(&s.pod1Modified)
		s.waitForDirtyCount(cache, 1)
		return &s.pod1, nil
	}

	getPod, err := cache.Get(context.Background(), key)
	s.NoError(err)
	s.Equal(&s.pod1, getPod)

	// Ensure object is not cached.
	s.Equal(0, cache.Count(), "object was incorrectly cached")
	s.testCacheMiss(cache, &s.pod1Modified)
}

// Multiple concurrent fetchers interfere with one another.
func (s *CacheTestSuite) TestConcurrentFetchers() {
	cache, watcher := s.createCache("", 0)

	getCount := 0
	key1 := client.ObjectKey{Namespace: s.pod1.Namespace, Name: s.pod1.Name}
	key2 := client.ObjectKey{Namespace: s.pod2.Namespace, Name: s.pod2.Name}
	key3 := client.ObjectKey{Namespace: s.pod2.Namespace, Name: s.pod2.Name}
	s.getResource = func(
		ctx context.Context, key client.ObjectKey, options *metav1.ListOptions,
	) (kube.Object, error) {
		getCount++
		switch getCount {
		case 1:
			// Trigger re-entrant Get on the same key.
			getPod, err := cache.Get(context.Background(), key)
			s.NoError(err)
			s.Equal(&s.pod1, getPod)

			// Return newer pod version.
			return &s.pod1Modified, nil

		case 2:
			// Return older pod version for case #1.
			return &s.pod1, nil

		case 3:
			// Trigger re-entrant Get on a different key.
			getPod, err := cache.Get(context.Background(), key3)
			s.NoError(err)
			s.Equal(&s.pod3, getPod)

			// Return response for 2nd outer Get call.
			return &s.pod2, nil

		default:
			// Trigger dirty notification that will prevent pod3 from being
			// added to the cache.
			watcher.Modify(&s.pod3)
			s.waitForDirtyCount(cache, 1)

			// Return response for case #3.
			return &s.pod3, nil
		}
	}

	s.runCache(cache)

	// Get will trigger a re-entrant call to Get on same key. The inner Get
	// will add an older version of the pod to the cache, and the outer Get
	// should pick that up rather than the more recent version.
	getPod, err := cache.Get(context.Background(), key1)
	s.NoError(err)
	s.Equal(&s.pod1, getPod)

	// Ensure older object is in cache.
	s.testCacheHit(cache, &s.pod1)
	s.Equal(1, cache.Count())

	// Get will trigger a re-entrant call to Get on a different key. The
	// inner Get will interfere with the outer Get and prevent it from adding
	// its fetched object to the cache.
	getPod, err = cache.Get(context.Background(), key2)
	s.NoError(err)
	s.Equal(&s.pod2, getPod)

	// Ensure pod2 and pod3 were not added to the cache.
	s.testCacheMiss(cache, &s.pod2)
	s.testCacheMiss(cache, &s.pod3)
	s.waitForDirtyCount(cache, 3)
}

func (s *CacheTestSuite) TestSingleNamespace() {
	cache, watcher := s.createCache("ns1", 0)
	s.runCache(cache)

	// Cache rejects other namespaces.
	podKey := client.ObjectKey{Namespace: s.pod3.Namespace, Name: s.pod3.Name}
	_, err := cache.Get(context.Background(), podKey)
	s.EqualError(err, "namespace ns3 passed to Get does not match cache namespace ns1")

	// But ns1 should work.
	s.testCacheMiss(cache, &s.pod1)
	watcher.Modify(&s.pod1Modified)
	s.waitForDirtyCount(cache, 1)
	s.testCacheMiss(cache, &s.pod1)
	s.testCacheHit(cache, &s.pod1)
}

func (s *CacheTestSuite) TestMultipleNamespaces() {
	cache, watcher := s.createCache("", 0)
	s.runCache(cache)

	s.testCacheMiss(cache, &s.pod1)
	s.testCacheMiss(cache, &s.pod2)
	s.testCacheMiss(cache, &s.pod3)

	// Invalidate pod1 but not pod3 (which both have same name).
	watcher.Modify(&s.pod1Modified)
	s.waitForDirtyCount(cache, 1)

	s.testCacheMiss(cache, &s.pod1)
	s.testCacheHit(cache, &s.pod2)
	s.testCacheHit(cache, &s.pod3)
}

func (s *CacheTestSuite) TestExpiration() {
	cache, watcher := s.createCache("", 5*time.Second)
	s.runCache(cache)

	// Give test power to control time. If tests have populated the channel,
	// then timeutil.Now will use that; otherwise, it will use time.Now().
	timeChan := make(chan time.Time, 4)
	defer testutils.HookGlobal(&timeutil.Now, func() time.Time {
		select {
		case tm := <-timeChan:
			return tm
		default:
			return time.Now()
		}
	})()

	// Add first pod as of 5 seconds ago, and second as of now.
	timeChan <- time.Now().Add(-5 * time.Second)

	s.testCacheMiss(cache, &s.pod1)
	s.testCacheMiss(cache, &s.pod2)

	// Trigger removal of expired object from cache by sending unrelated
	// dirty notification.
	watcher.Modify(&s.pod3)
	s.waitForDirtyCount(cache, 1)

	s.testCacheMiss(cache, &s.pod1)
	s.testCacheHit(cache, &s.pod2)
}

func (s *CacheTestSuite) TestWatchFailure() {
	cache, _ := s.createCache("", 0)

	// Don't sleep when watch establishment errors.
	var sleepCount atomic.Int32
	defer testutils.HookGlobal(&kube.SleepContext, func(
		ctx context.Context, duration time.Duration,
	) bool {
		sleepCount.Add(1)
		return true
	})()

	// Return fake watchers to the cache's background goroutine.
	var watcherCount atomic.Int32
	watcher1 := watch.NewFake()
	watcher2 := watch.NewFake()
	watcher3 := watch.NewFake()
	cache.WatchResources = func(
		ctx context.Context, options *metav1.ListOptions,
	) (watch.Interface, error) {
		switch watcherCount.Add(1) {
		case 1:
			// First time, return error.
			return nil, errors.New("some error")

		case 2:
			// Second time, return watcher #1.
			return watcher1, nil

		case 3:
			// Third time, return watcher #2.
			return watcher2, nil

		default:
			// Last time, return watcher #3.
			return watcher3, nil
		}
	}

	podGvk, err := kube.GVKFromScheme(s.ctl.Scheme(), &corev1.Pod{})
	s.NoError(err)

	// Record value of WatcherEstablishedTotal metric, we'll test for it later.
	m := &dto.Metric{}
	s.NoError(opmetrics.WatcherEstablishedTotal.With(opmetrics.ResourceLabel(podGvk)).Write(m))

	// Run cache on background goroutine.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := cache.Run(ctx); err != nil {
			s.T().Errorf("error running cache: %v", err)
		}
	}()

	// Wait until watch is established. This prevents data race on
	// cache.GetResource, which is read on startup by cache.Run.
	s.Eventually(func() bool {
		return watcherCount.Load() == 2
	}, 5*time.Second, time.Millisecond)

	// Use the first watcher.
	s.testCacheMiss(cache, &s.pod1)
	watcher1.Modify(&s.pod1Modified)
	s.waitForDirtyCount(cache, 1)
	s.testCacheMiss(cache, &s.pod1)
	s.testCacheHit(cache, &s.pod1)

	// Trigger watcher error. Dirty list and object cache should be cleared.
	watcher1.Error(&metav1.Status{Status: "Failure"})

	// Wait for original watcher to close.
	s.Eventually(func() bool {
		_, ok := <-watcher1.ResultChan()
		return !ok
	}, 5*time.Second, time.Millisecond)

	// Watcher error should not invalidate objects.
	s.testCacheHit(cache, &s.pod1)

	// Ensure that new watcher is now established.
	watcher2.Modify(&s.pod1Modified)
	s.waitForDirtyCount(cache, 2)
	s.testCacheMiss(cache, &s.pod1)
	s.testCacheHit(cache, &s.pod1)

	// Close watcher2 and ensure that a new watcher is established.
	watcher2.Stop()
	watcher3.Modify(&s.pod1Modified)
	s.waitForDirtyCount(cache, 3)
	s.testCacheMiss(cache, &s.pod1)
	s.testCacheHit(cache, &s.pod1)

	// Watch establishment should have been attempted 4 times and
	// SleepContext once.
	s.Equal(int32(4), watcherCount.Load())
	s.Equal(int32(1), sleepCount.Load())
	// Three watchers were successfully established.
	testutils.RequireMetricEqual(s.T(), opmetrics.WatcherEstablishedTotal.With(
		opmetrics.ResourceLabel(podGvk)), int(*m.Counter.Value)+3)
}

func (s *CacheTestSuite) createCache(
	ns string, ttl time.Duration,
) (cache *kube.Cache, watcher *watch.FakeWatcher) {
	mapper := s.createMapper()
	ctl, err := kube.NewCtlWithOptions(&rest.Config{}, kube.Options{Mapper: mapper})
	s.NoError(err)
	if ns != "" {
		ctl = ctl.WithNamespace(ns)
	}
	s.ctl = ctl

	// Create cache.
	cache = kube.NewCache(ctl, &corev1.Pod{})
	cache.TTL = ttl
	cache.Options.LabelSelector = "some-selector"

	// Reset the getResource callback for this test.
	s.getResource = nil
	cache.GetResource = func(
		ctx context.Context, key client.ObjectKey, options *metav1.ListOptions,
	) (kube.Object, error) {
		return s.getResource(ctx, key, options)
	}

	// Return a fake watcher to the cache's background goroutine.
	watcher = watch.NewFake()
	cache.WatchResources = func(
		ctx context.Context, options *metav1.ListOptions,
	) (watch.Interface, error) {
		if !options.Watch {
			s.T().Error("expected options.Watch to be true")
		}
		if options.LabelSelector != "some-selector" {
			s.T().Errorf("expected %v, got %v", "some-selector", options.LabelSelector)
		}
		return watcher, nil
	}

	return cache, watcher
}

func (s *CacheTestSuite) runCache(cache *kube.Cache) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		err := cache.Run(ctx)
		if err != nil {
			s.T().Errorf("error calling cache.Run: %v", err)
		}
	}()
	s.T().Cleanup(cancel)
}

func (s *CacheTestSuite) testCacheMiss(cache *kube.Cache, pod *corev1.Pod) {
	// Temporarily override the GetResource callback to detect that it's called
	// as expected.
	podKey := client.ObjectKey{Namespace: pod.Namespace, Name: pod.Name}
	wasCalled := false
	defer testutils.HookGlobal(&s.getResource, func(
		ctx context.Context, key client.ObjectKey, options *metav1.ListOptions,
	) (kube.Object, error) {
		wasCalled = true
		s.Equal(podKey, key)
		return pod, nil
	})()

	getPod, err := cache.Get(context.Background(), podKey)
	s.NoError(err)
	s.Equal(pod, getPod)
	s.True(wasCalled)
}

func (s *CacheTestSuite) testCacheHit(cache *kube.Cache, pod *corev1.Pod) {
	// Temporarily override the GetResource callback to ensure it is not called.
	podKey := client.ObjectKey{Namespace: pod.Namespace, Name: pod.Name}
	defer testutils.HookGlobal(&s.getResource, func(
		ctx context.Context, key client.ObjectKey, options *metav1.ListOptions,
	) (kube.Object, error) {
		return nil, errors.New("GetResource should not have been called")
	})()
	getPod, err := cache.Get(context.Background(), podKey)
	s.NoError(err)
	s.Equal(pod, getPod)
}

func (s *CacheTestSuite) waitForDirtyCount(cache *kube.Cache, count int) {
	s.Eventually(func() bool {
		return cache.DirtyCount() == count
	}, 5*time.Second, time.Millisecond)
}

func (s *CacheTestSuite) createMapper() *meta.DefaultRESTMapper {
	gvk := schema.GroupVersionKind{
		Group:   corev1.SchemeGroupVersion.Group,
		Version: corev1.SchemeGroupVersion.Version,
		Kind:    "Pod",
	}
	gvr := schema.GroupVersionResource{
		Group:    corev1.SchemeGroupVersion.Group,
		Version:  corev1.SchemeGroupVersion.Version,
		Resource: "pods",
	}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{})
	mapper.AddSpecific(gvk, gvr, gvr, meta.RESTScopeNamespace)
	return mapper
}

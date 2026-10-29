package tenantpool

import (
	"context"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
)

// Cache caches TenantPool metadata taken from CrdbCluster resources stored in
// the K8s cluster. It is indexed by tenant pool name, qualified by the
// namespace of the CrdbCluster. The cache is periodically invalidated if
// RefreshInterval is set.
type Cache struct {
	mu sync.Mutex
	// RefreshInterval specifies how frequently the cache will be invalidated.
	// If set to 0, it will never be invalidated.
	RefreshInterval time.Duration

	cache map[string]poolsForNs
}

// Get returns the metadata for the tenant pool with the given name. The tenant
// pool must be defined in the host cluster in the kube.Ctl namespace. If the
// tenant pool is already in the cache, it is served from there; else Get will
// fetch it using the K8s API and add it to the cache.
func (c *Cache) Get(
	ctx context.Context, ctl *kube.Ctl, poolName string,
) (*crdbv1beta1.TenantPool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctl.Namespace() == "" {
		return nil, errors.AssertionFailedf("Get received an unnamespaced *kube.Ctl")
	}

	if c.cache == nil {
		c.cache = make(map[string]poolsForNs)
	}

	// Ensure entry exists for the namespace and invalidate an expired entry.
	forNs, ok := c.cache[ctl.Namespace()]
	if !ok || forNs.IsExpired(c.RefreshInterval) {
		forNs = poolsForNs{
			Pools:       make(map[string]*crdbv1beta1.TenantPool),
			LastRefresh: timeutil.Now(),
		}
		c.cache[ctl.Namespace()] = forNs
	}

	if pool, ok := forNs.Pools[poolName]; ok {
		return pool, nil
	}

	// Need to look up the pool in K8s store and add to cache.
	var clusters crdbv1beta1.CrdbClusterList
	if err := ctl.List(ctx, &kube.ListOptions{}, &clusters); err != nil {
		return nil, err
	}

	var host *crdbv1beta1.TenantHost
	for _, cluster := range clusters.Items {
		if cluster.Spec.IsTenantHost() {
			host = cluster.Spec.TenantHost
			break
		}
	}

	if host == nil {
		return nil, errors.Newf("could not find host in namespace %s", ctl.Namespace())
	}

	for i := range host.TenantPools {
		pool := &host.TenantPools[i]
		forNs.Pools[pool.Name] = pool
	}

	if pool, ok := forNs.Pools[poolName]; ok {
		return pool, nil
	}

	return nil, errors.Newf("could not find tenant pool %s in namespace %s", poolName, ctl.Namespace())
}

type poolsForNs struct {
	Pools       map[string]*crdbv1beta1.TenantPool
	LastRefresh time.Time
}

func (p poolsForNs) IsExpired(refreshInterval time.Duration) bool {
	if refreshInterval == 0 {
		return false
	}
	return timeutil.Now().After(p.LastRefresh.Add(refreshInterval))
}

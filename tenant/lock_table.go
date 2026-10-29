package tenant

import (
	"context"
	"sync"
)

// LockTable is used to serialize access to tenants.
//
// Note: LockTable does not GC locks. It creates one lock for every tenant. If
// there is a large amount of tenant churn (e.g. 100k+ tenants per week per host
// cluster), it may be necessary to rewrite LockTable so that it only consumes
// memory for actively locked tenants.
type LockTable struct {
	mu sync.Mutex

	// locks contains a channel for each tenant. The channels are buffered with
	// size 1. The channel is used to implement a mutex with support for
	// cancelation.
	locks map[string]chan struct{}
}

// NewLockTable creates an empty lock table.
func NewLockTable() *LockTable {
	return &LockTable{locks: make(map[string]chan struct{})}
}

// Lock acquires an in memory lock on the tenant. It will block until the lock
// is acquired or the context is canceled.
//
// Example Usage:
// defer tenantLocks.Lock(ctx, tenant)()
func (t *LockTable) Lock(ctx context.Context, tenantID string) (func(), error) {
	lock := t.getLock(tenantID)
	select {
	case <-lock:
		return func() { lock <- struct{}{} }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TryLock attempts to acquire an in memory lock on the tenant. Returns false if
// the tenant is already locked.
//
// Example Usage:
//
//	if unlock, ok := table.TryLock(tenant); ok {
//	    defer unlock()
//	}
//
// defer tenantLocks.Lock(tenant)()
func (t *LockTable) TryLock(tenantID string) (func(), bool) {
	lock := t.getLock(tenantID)
	select {
	case <-lock:
		return func() { lock <- struct{}{} }, true
	default:
		return nil, false
	}
}

func (t *LockTable) getLock(tenantID string) chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()

	lock, ok := t.locks[tenantID]
	if !ok {
		lock = make(chan struct{}, 1)
		lock <- struct{}{}
		t.locks[tenantID] = lock
	}

	return lock
}

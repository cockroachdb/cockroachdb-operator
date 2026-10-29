package kube

// Define test functions that we don't want to export from the non-test package,
// but which need to access unexported variables/functions.

// DirtyCount returns the count of dirty notifications handled by the watcher.
// NOTE: Test-only method.
func (c *Cache) DirtyCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.mu.dirtyCount
}

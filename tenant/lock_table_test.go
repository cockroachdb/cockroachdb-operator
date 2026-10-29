package tenant

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLockTable(t *testing.T) {
	done := make(chan bool)
	table := NewLockTable()

	released := int32(0)
	release1, err := table.Lock(context.Background(), "tenant-1")
	require.NoError(t, err)

	cleanup, locked := table.TryLock("tenant-1")
	require.Nil(t, cleanup)
	require.False(t, locked)

	go func() {
		release, err := table.Lock(context.Background(), "tenant-1")
		require.Equal(t, int32(1), atomic.LoadInt32(&released))
		require.NoError(t, err)
		release()
		close(done)
	}()

	// sleep so the goroutine is likely to reach the blocking lock.
	time.Sleep(5 * time.Millisecond)

	atomic.StoreInt32(&released, 1)
	release1()
	<-done
}

func TestLockTableLockMany(t *testing.T) {
	var cleanup []func()

	type testCase struct {
		target     string
		expectLock bool
	}

	table := NewLockTable()
	for _, tc := range []testCase{
		{"tenant-1", true},
		{"tenant-2", true},
		{"tenant-3", true},
		{"tenant-1", false},
		{"tenant-5", true},
	} {
		c, locked := table.TryLock(tc.target)
		require.Equal(t, locked, tc.expectLock)
		if locked {
			cleanup = append(cleanup, c)
		}
	}

	for _, c := range cleanup {
		c()
	}

	unlock, locked := table.TryLock("tenant-1")
	require.True(t, locked)
	unlock()
}

func TestLockTableCancel(t *testing.T) {
	table := NewLockTable()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	cleanup, err := table.Lock(ctx, "tenant-1")
	require.NoError(t, err)
	defer cleanup()

	go func() {
		cleanup, err := table.Lock(ctx, "tenant-1")

		require.Error(t, err)
		require.Nil(t, cleanup)
		close(done)
	}()

	cancel()
	<-done
}

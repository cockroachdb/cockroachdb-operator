package kubenet

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMaybeWarmupTenantCancelled(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	err := maybeWarmupTenant(canceledCtx, func(ctx context.Context, _ string) error {
		return ctx.Err()
	}, Options{})
	require.Error(t, err)
	require.ErrorIs(t, err, canceledCtx.Err())
}

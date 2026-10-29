package tenantctrl

import (
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/tenant"
	"github.com/stretchr/testify/require"
)

func TestRequestHandler_Scale(t *testing.T) {
	t.Run("low capacity error", func(t *testing.T) {
		h := &requestHandler{reconciler: &Reconciler{}}
		h.testingKnobs.scaleTenant = func(ctx context.Context, t *crdbv1beta1.CrdbTenant) (bool, time.Duration, error) {
			return true, tenant.LowCapacityRequeueAfter, nil
		}
		require.NoError(t, h.Scale())
		require.Equal(t, tenant.LowCapacityRequeueAfter, h.res.RequeueAfter)
	})

	t.Run("other errors", func(t *testing.T) {
		h := &requestHandler{reconciler: &Reconciler{}}
		h.testingKnobs.scaleTenant = func(ctx context.Context, t *crdbv1beta1.CrdbTenant) (bool, time.Duration, error) {
			return false, 0, errors.New("foo")
		}
		require.Error(t, h.Scale())
		require.Equal(t, time.Duration(0), h.res.RequeueAfter)
	})

	t.Run("draining", func(t *testing.T) {
		h := &requestHandler{reconciler: &Reconciler{}}
		h.testingKnobs.scaleTenant = func(ctx context.Context, t *crdbv1beta1.CrdbTenant) (bool, time.Duration, error) {
			return true, tenant.DrainingRequeueAfter, nil
		}
		require.NoError(t, h.Scale())
		require.Equal(t, tenant.DrainingRequeueAfter, h.res.RequeueAfter)
	})

	t.Run("not draining", func(t *testing.T) {
		h := &requestHandler{reconciler: &Reconciler{}}
		h.testingKnobs.scaleTenant = func(ctx context.Context, t *crdbv1beta1.CrdbTenant) (bool, time.Duration, error) {
			return false, 0, nil
		}
		require.NoError(t, h.Scale())
		require.Equal(t, time.Duration(0), h.res.RequeueAfter)
	})
}

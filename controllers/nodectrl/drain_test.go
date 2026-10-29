package nodectrl

import (
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode/nodemock"
	"github.com/cockroachlabs/version"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
)

func TestIssueDrain(t *testing.T) {
	ctx := context.Background()
	r := &Reconciler{}
	node, nodeID := "cockroachdb-66846", uint(4)

	// Version that contains the virtual cluster draining fix
	fixedVersion := version.MustParse("v25.3.5")
	// Version that has the virtual cluster draining bug
	buggedVersion := version.MustParse("v25.3.2")
	// Version that cannot possibly be UA
	nonUAVersion := version.MustParse("v24.2.0")

	drainDone := func(exec kubecrdbnode.NodeExecutor, crdbversion version.Version) func() bool {
		return func() bool {
			return r.issueDrain(ctx, node, exec, nodeID, crdbversion)
		}
	}

	// All tests below set IsFullyDecommissioned to false so the normal
	// drain path is exercised. The severed-node skip is tested separately
	// in TestDrainSkipsFullyDecommissionedNode.
	notSevered := func(exec *nodemock.MockNodeExecutor) {
		exec.EXPECT().IsFullyDecommissioned(ctx, nodeID).Return(false, nil).AnyTimes()
	}

	// Test back to back runs No goroutines are issued, Drain is not called
	// when done is called, Drain command receive context cancelation.
	// Check that runningDrain map is updated accordingly.
	t.Run("no error happy path", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		exec := nodemock.NewMockNodeExecutor(ctrl)
		notSevered(exec)

		exec.EXPECT().Drain(gomock.Any(), nodeID).Return("", nil)
		require.False(t, r.issueDrain(ctx, node, exec, nodeID, fixedVersion))
		require.Eventually(t, drainDone(exec, fixedVersion), time.Second, 50*time.Millisecond)
		require.Equal(t, 0, len(r.runningDrains.running))
	})

	t.Run("no error happy path, version without UA", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		exec := nodemock.NewMockNodeExecutor(ctrl)
		notSevered(exec)

		exec.EXPECT().Drain(gomock.Any(), nodeID).Return("", nil)
		require.False(t, r.issueDrain(ctx, node, exec, nodeID, nonUAVersion))
		require.Eventually(t, drainDone(exec, nonUAVersion), time.Second, 50*time.Millisecond)
		require.Equal(t, 0, len(r.runningDrains.running))
	})

	t.Run("no drain is issued if already running", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		exec := nodemock.NewMockNodeExecutor(ctrl)
		notSevered(exec)

		ch := make(chan struct{})
		exec.EXPECT().Drain(gomock.Any(), nodeID).DoAndReturn(func(context.Context, uint) (string, error) {
			// Block to allow tests to do some checking.
			<-ch
			return "", nil
		})

		// Issue initial drain.
		require.False(t, r.issueDrain(ctx, node, exec, nodeID, fixedVersion))
		// Call several times, mock checks that Drain() is not called again.
		require.Never(t, drainDone(exec, fixedVersion), 500*time.Millisecond, 50*time.Millisecond)
		ch <- struct{}{} // Unblock Drain
		require.Eventually(t, drainDone(exec, fixedVersion), time.Second, 50*time.Millisecond)
	})

	t.Run("requires complete to be printed on clusters without virtual cluster drain fix", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		exec := nodemock.NewMockNodeExecutor(ctrl)
		notSevered(exec)

		call := exec.EXPECT().Drain(gomock.Any(), nodeID).Return("foobar", nil).MinTimes(1)
		require.False(t, r.issueDrain(ctx, node, exec, nodeID, buggedVersion))
		require.Never(t, drainDone(exec, buggedVersion), 500*time.Millisecond, 50*time.Millisecond)
		call.Return("(complete)", nil)
		require.Eventually(t, drainDone(exec, buggedVersion), time.Second, 50*time.Millisecond)
	})

	t.Run("running drain is removed on error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		exec := nodemock.NewMockNodeExecutor(ctrl)
		notSevered(exec)

		exec.EXPECT().Drain(gomock.Any(), nodeID).DoAndReturn(func(ctx context.Context, _ uint) (string, error) {
			return "", errors.New("some error")
		})

		drainRemoved := func() bool {
			_ = r.issueDrain(ctx, node, exec, nodeID, fixedVersion)
			r.runningDrains.Lock()
			defer r.runningDrains.Unlock()
			return len(r.runningDrains.running) == 0
		}
		require.Eventually(t, drainRemoved, time.Second, 50*time.Millisecond)
	})
}

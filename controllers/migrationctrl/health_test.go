package migrationctrl

import (
	"context"
	"testing"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode"
	"github.com/stretchr/testify/require"
)

type healthNodeExecutor struct {
	kubecrdbnode.NodeExecutor
	underReplicated int64
	liveNodes       int32
	err             error
}

func (e healthNodeExecutor) ClusterHealth(context.Context) (int64, int32, error) {
	return e.underReplicated, e.liveNodes, e.err
}

func TestCheckNodeHealth(t *testing.T) {
	for _, tc := range []struct {
		name              string
		underReplicated   int64
		liveNodes         int32
		expectedLiveNodes int32
		execErr           error
		expectedHealthy   bool
		expectedReason    string
		expectErr         string
	}{
		{
			name:              "healthy",
			liveNodes:         3,
			expectedLiveNodes: 3,
			expectedHealthy:   true,
		},
		{
			name:              "under-replicated ranges are unhealthy",
			underReplicated:   4,
			liveNodes:         3,
			expectedLiveNodes: 3,
			expectedReason:    "4 under-replicated ranges",
		},
		{
			// A missing node with zero under-replicated ranges must report the
			// live-node shortfall, not "0 under-replicated ranges".
			name:              "missing live node reports the live-node shortfall",
			liveNodes:         2,
			expectedLiveNodes: 3,
			expectedReason:    "2 live nodes, expected 3",
		},
		{
			// When both are wrong the live-node shortfall is reported first.
			name:              "live-node shortfall takes precedence over under-replication",
			underReplicated:   5,
			liveNodes:         2,
			expectedLiveNodes: 3,
			expectedReason:    "2 live nodes, expected 3",
		},
		{
			name:              "additional live node is accepted",
			liveNodes:         4,
			expectedLiveNodes: 3,
			expectedHealthy:   true,
		},
		{
			name:      "executor error propagates",
			execErr:   errors.New("boom"),
			expectErr: "cluster health check failed: boom",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := healthNodeExecutor{
				underReplicated: tc.underReplicated,
				liveNodes:       tc.liveNodes,
				err:             tc.execErr,
			}
			r := &Reconciler{
				NodeExecutor: func(*crdbv1beta1.CrdbNode, kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
					return executor
				},
			}

			healthy, reason, err := r.checkNodeHealth(
				context.Background(), nil, &crdbv1beta1.CrdbNode{}, tc.expectedLiveNodes,
			)
			if tc.expectErr != "" {
				require.EqualError(t, err, tc.expectErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expectedHealthy, healthy)
			require.Equal(t, tc.expectedReason, reason)
		})
	}
}

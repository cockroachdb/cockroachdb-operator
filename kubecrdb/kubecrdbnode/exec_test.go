package kubecrdbnode_test

import (
	"context"
	"io"
	"testing"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	. "github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode/nodemock"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	gomock "github.com/golang/mock/gomock"
	"github.com/stretchr/testify/suite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type execOpts struct {
	stdout string
	stderr string
	err    error
}

type asynchronousExec struct {
	stop chan struct{}
	done chan struct{}
}

func (e *asynchronousExec) Exec(
	_ context.Context, _, _ string, opts kube.ExecOptions,
) error {
	started := make(chan struct{})
	go func() {
		defer close(e.done)
		_, _ = opts.Stdout.Write([]byte("cockroach init retrying\n"))
		close(started)
		for {
			select {
			case <-e.stop:
				return
			default:
				_, _ = opts.Stdout.Write([]byte("cockroach init retrying\n"))
			}
		}
	}()
	<-started
	return context.DeadlineExceeded
}

func TestNodeExecutorConcurrentStreamShutdown(t *testing.T) {
	exec := &asynchronousExec{stop: make(chan struct{}), done: make(chan struct{})}
	nodeExec := NewNodeExecutor(&crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
	}, exec)

	_, _, _ = nodeExec.Init(context.Background())
	close(exec.stop)
	<-exec.done
}

func TestNodeExecutor(t *testing.T) {
	suite.Run(t, new(NodeExecutorSuite))
}

type NodeExecutorSuite struct {
	suite.Suite

	ctrl     *gomock.Controller
	ctx      context.Context
	exec     *nodemock.MockExecutor
	node     *crdbv1beta1.CrdbNode
	nodeExec NodeExecutor
}

func (s *NodeExecutorSuite) SetupSuite() {
	s.ctrl = gomock.NewController(s.T())
	s.ctx = context.Background()
	s.exec = nodemock.NewMockExecutor(s.ctrl)
	s.node = &crdbv1beta1.CrdbNode{ObjectMeta: metav1.ObjectMeta{Name: "test-node", Namespace: "test-ns", Labels: map[string]string{manifests.BetaCrdbClusterNodeOwnerLabelKey: "test-cluster"}}}
	s.nodeExec = NewNodeExecutor(s.node, s.exec)
}

func (s *NodeExecutorSuite) setupMock(cmd [][]string, testOpts []execOpts) {
	i := 0
	s.exec.EXPECT().
		Exec(s.ctx, s.node.Name, "cockroachdb", gomock.AssignableToTypeOf(kube.ExecOptions{})).Times(len(cmd)).
		DoAndReturn(func(ctx context.Context, _ string, _ string, opts kube.ExecOptions) error {
			s.Require().Equal(cmd[i], opts.Command)

			_, err := opts.Stdout.Write([]byte(testOpts[i].stdout))
			s.Require().NoError(err)
			_, err = opts.Stderr.Write([]byte(testOpts[i].stderr))
			s.Require().NoError(err)
			i++
			return testOpts[i-1].err
		})
}

func (s *NodeExecutorSuite) TestFinalizeDecommission() {
	cmd := [][]string{
		{"/cockroach/cockroach", "node", "decommission", "101", "--dry-run", "--format", "csv", "--port", "26258"},
		{"/cockroach/cockroach", "node", "decommission", "101", "--port", "26258"},
	}

	s.setupMock(cmd, []execOpts{{
		stdout: "warning: node 2 is already decommissioning or decommissioned\n\nid,is_live,replicas,is_decommissioning,membership,is_draining,readiness,blocking_ranges\n2,true,0,true,decommissioning,true,ready,0",
	}, {}})
	s.Require().NoError(s.nodeExec.FinalizeDecommission(s.ctx, 101))

	// If the node is already decommissioned, we don't exec the decommission command.
	s.setupMock(cmd[:1], []execOpts{{
		stdout: "warning: node 2 is already decommissioning or decommissioned\n\nid,is_live,replicas,is_decommissioning,membership,is_draining,readiness,blocking_ranges\n2,true,0,true,decommissioned,true,ready,0",
	}})
	s.Require().NoError(s.nodeExec.FinalizeDecommission(s.ctx, 101))

	// If we get an already decommissioned error for this node, we don't exec the decommission command
	s.setupMock(cmd[:1], []execOpts{{
		stdout: "ERROR: rpc error: code = PermissionDenied desc = failed to connect to n46 at cockroachdb-fcx62.cockroachdb.eu-west-2.svc.cluster.local:26258: initial connection heartbeat failed: grpc: n101 was permanently removed from the cluster at 2025-05-14 15:56:14.339951635 +0000 UTC; it is not allowed to rejoin the cluster [code 7/PermissionDenied], node is decommissioned",
	}})
	s.Require().NoError(s.nodeExec.FinalizeDecommission(s.ctx, 101))

	// Same guard match but the exec exits non-zero. Without the reordered
	// guard in getDecommissionDetails, FinalizeDecommission would
	// propagate the error and the reconciler would hang.
	s.setupMock(cmd[:1], []execOpts{{
		stdout: "ERROR: rpc error: code = PermissionDenied desc = failed to connect to n46 at cockroachdb-fcx62.cockroachdb.eu-west-2.svc.cluster.local:26258: initial connection heartbeat failed: grpc: n101 was permanently removed from the cluster at 2025-05-14 15:56:14.339951635 +0000 UTC; it is not allowed to rejoin the cluster [code 7/PermissionDenied], node is decommissioned",
		err:    errors.New("command terminated with exit code 1"),
	}})
	s.Require().NoError(s.nodeExec.FinalizeDecommission(s.ctx, 101))

	// If we get an already decommissioned error for a _different_ node, we'll error due to unexpected output
	s.setupMock(cmd[:1], []execOpts{{
		stdout: "ERROR: rpc error: code = PermissionDenied desc = failed to connect to n46 at cockroachdb-fcx62.cockroachdb.eu-west-2.svc.cluster.local:26258: initial connection heartbeat failed: grpc: n100 was permanently removed from the cluster at 2025-05-14 15:56:14.339951635 +0000 UTC; it is not allowed to rejoin the cluster [code 7/PermissionDenied], node is decommissioned",
	}})
	s.Require().EqualError(errors.Cause(s.nodeExec.FinalizeDecommission(s.ctx, 101)), "unexpected output: expected csv header prefix 'id,is_live,replicas'")

	s.setupMock(cmd[:1], []execOpts{{err: errors.New("boom")}})
	s.Require().EqualError(errors.Cause(s.nodeExec.FinalizeDecommission(s.ctx, 101)), "boom")
}

func (s *NodeExecutorSuite) TestFinalizeDecommissionDeprecated() {
	cmd := [][]string{
		{"/cockroach/cockroach", "node", "status", "101", "--decommission", "--format", "csv"},
		{"/cockroach/cockroach", "node", "decommission", "101", "--port", "26258"},
	}

	s.setupMock(cmd, []execOpts{{
		stdout: "id,address,sql_address,build,started_at,updated_at,locality,is_available,is_live,gossiped_replicas,is_decommissioning,membership,is_draining\n101,test-node,test-node,0,0,0,l,1,1,302000,false,active,false",
	}, {}})
	s.Require().NoError(s.nodeExec.FinalizeDecommissionDeprecated(s.ctx, 101))

	// If the node is already decommissioned, we don't exec the decommission command.
	s.setupMock(cmd[:1], []execOpts{{
		stdout: "id,address,sql_address,build,started_at,updated_at,locality,is_available,is_live,gossiped_replicas,is_decommissioning,membership,is_draining\n101,test-node,test-node,0,0,0,l,1,1,302000,false,decommissioned,false",
	}})
	s.Require().NoError(s.nodeExec.FinalizeDecommissionDeprecated(s.ctx, 101))

	s.setupMock(cmd[:1], []execOpts{{err: errors.New("boom")}})
	s.Require().EqualError(errors.Cause(s.nodeExec.FinalizeDecommissionDeprecated(s.ctx, 101)), "boom")
}

func (s *NodeExecutorSuite) TestStoreIDs() {
	tests := []struct {
		name    string
		image   string
		stdout  string
		execErr error
		want    []int64
		wantErr bool
	}{
		{name: "multiple stores", image: "cockroachdb/cockroach:v25.4.0", stdout: "store_id\n5\n6\n", want: []int64{5, 6}},
		{name: "timing footer", image: "cockroachdb/cockroach:v25.4.0", stdout: "store_id\n5\n6\n\nTime: 46ms\n", want: []int64{5, 6}},
		{name: "empty result", image: "cockroachdb/cockroach:v25.4.0", stdout: "store_id\n\nTime: 1ms\n"},
		{name: "unsafe internals enabled", image: "cockroachdb/cockroach:v26.1.0", stdout: "SET\nstore_id\n12\n", want: []int64{12}},
		{name: "notice before header", image: "cockroachdb/cockroach:v26.1.0", stdout: "NOTICE: store_id lookup enabled\nSET\nstore_id\n12\n", want: []int64{12}},
		{name: "malformed store id", image: "cockroachdb/cockroach:v26.1.0", stdout: "SET\nstore_id\n12\nnot-an-id\n", wantErr: true},
		{name: "query failure", image: "cockroachdb/cockroach:v25.4.0", execErr: errors.New("boom"), wantErr: true},
	}

	for _, tc := range tests {
		s.Run(tc.name, func() {
			s.node.Spec.Image = tc.image
			cmd := []string{"/cockroach/cockroach", "sql", "--format=csv"}
			if tc.image == "cockroachdb/cockroach:v26.1.0" {
				cmd = append(cmd, "--execute=SET allow_unsafe_internals = on")
			}
			cmd = append(cmd,
				"--execute=SELECT store_id FROM crdb_internal.kv_store_status WHERE node_id = 101 ORDER BY store_id",
				"--port=26257",
			)
			s.setupMock([][]string{cmd}, []execOpts{{stdout: tc.stdout, err: tc.execErr}})

			ids, err := s.nodeExec.StoreIDs(s.ctx, 101)
			if tc.wantErr {
				s.Require().Error(err)
				return
			}
			s.Require().NoError(err)
			s.Require().Equal(tc.want, ids)
		})
	}
	s.node.Spec.Image = ""
}

func (s *NodeExecutorSuite) TestKill() {
	cmd := [][]string{{
		"/bin/sh",
		"-cx",
		`pids=$(ls -l /proc/*/exe | awk -F/ "\$5 == \"cockroach\" {print \$3}"); if [ -n "$pids" ]; then kill -9 $pids; fi`,
	}}
	tests := []struct {
		desc    string
		execErr error
		err     string
	}{
		{desc: "process killed"},
		{desc: "container already gone", execErr: errors.New("container not found")},
		{desc: "containerd task already gone", execErr: errors.New("failed to create exec: task abc123 not found")},
		{desc: "unexpected exec failure", execErr: errors.New("connection refused"), err: "connection refused"},
	}

	for _, tc := range tests {
		s.Run(tc.desc, func() {
			s.setupMock(cmd, []execOpts{{err: tc.execErr}})
			err := s.nodeExec.Kill(s.ctx)
			if tc.err == "" {
				s.Require().NoError(err)
			} else {
				s.Require().ErrorContains(err, tc.err)
			}
		})
	}
}

func (s *NodeExecutorSuite) TestDecommission() {
	cmd := [][]string{{"/cockroach/cockroach", "node", "decommission", "101", "--wait", "none", "--port", "26258"}}

	s.setupMock(cmd, []execOpts{{}})
	s.Require().NoError(s.nodeExec.Decommission(s.ctx, 101))

	s.setupMock(cmd, []execOpts{{err: errors.New("boom")}})
	s.Require().EqualError(errors.Cause(s.nodeExec.Decommission(s.ctx, 101)), "boom")

	// Operator-restart-mid-decommission scenario. If Status never advanced
	// past Drained before a crash, the retry lands here on a node CRDB has
	// already severed. The exec fails with the "permanently removed"
	// marker and Decommission has to swallow it so the state machine can
	// advance to TransferringReplicas.
	s.setupMock(cmd, []execOpts{{
		stdout: "ERROR: rpc error: code = PermissionDenied desc = failed to connect to n46 at cockroachdb-fcx62.cockroachdb.eu-west-2.svc.cluster.local:26258: initial connection heartbeat failed: grpc: n101 was permanently removed from the cluster at 2025-05-14 15:56:14.339951635 +0000 UTC; it is not allowed to rejoin the cluster [code 7/PermissionDenied], node is decommissioned",
		err:    errors.New("command terminated with exit code 1"),
	}})
	s.Require().NoError(s.nodeExec.Decommission(s.ctx, 101))

	// The guard is node-id-specific so an unrelated node's error still
	// propagates.
	s.setupMock(cmd, []execOpts{{
		stdout: "ERROR: rpc error: code = PermissionDenied desc = failed to connect to n46 at cockroachdb-fcx62.cockroachdb.eu-west-2.svc.cluster.local:26258: initial connection heartbeat failed: grpc: n100 was permanently removed from the cluster at 2025-05-14 15:56:14.339951635 +0000 UTC; it is not allowed to rejoin the cluster [code 7/PermissionDenied], node is decommissioned",
		err:    errors.New("command terminated with exit code 1"),
	}})
	s.Require().EqualError(errors.Cause(s.nodeExec.Decommission(s.ctx, 101)), "command terminated with exit code 1")
}

func (s *NodeExecutorSuite) TestInit() {
	cmd := [][]string{{"/cockroach/cockroach", "init", "--host", ":26258"}}

	s.setupMock(cmd, []execOpts{{}})
	_, _, err := s.nodeExec.Init(s.ctx)
	s.Require().NoError(err)

	s.setupMock(cmd, []execOpts{{err: errors.New("boom")}})
	_, _, err = s.nodeExec.Init(s.ctx)
	s.Require().EqualError(errors.Cause(err), "boom")

	customGRPCPort := int32(31258)
	s.node.Spec.GRPCPort = &customGRPCPort
	defer func() { s.node.Spec.GRPCPort = nil }()
	s.setupMock([][]string{{"/cockroach/cockroach", "init", "--host", ":31258"}}, []execOpts{{}})
	_, _, err = s.nodeExec.Init(s.ctx)
	s.Require().NoError(err)
}

func (s *NodeExecutorSuite) TestInitVirtualizedModes() {
	tests := []struct {
		name     string
		mode     crdbv1beta1.VirtualClusterMode
		extraArg string
		stdout   string
	}{
		{
			name:     "Primary mode",
			mode:     crdbv1beta1.VirtualClusterModePrimary,
			extraArg: "--virtualized",
			stdout:   "cluster initialized",
		},
		{
			name:     "Standby mode",
			mode:     crdbv1beta1.VirtualClusterModeStandby,
			extraArg: "--virtualized-empty",
			stdout:   "standby mode",
		},
	}

	for _, tt := range tests {
		s.Run(tt.name, func() {
			original := s.node.Spec.VirtualCluster
			defer func() { s.node.Spec.VirtualCluster = original }()

			s.node.Spec.VirtualCluster = &crdbv1beta1.CrdbVirtualClusterSpec{
				Mode: tt.mode,
			}

			cmd := [][]string{{"/cockroach/cockroach", "init", "--host", ":26258", tt.extraArg}}
			s.setupMock(cmd, []execOpts{{stdout: tt.stdout}})

			out, _, err := s.nodeExec.Init(s.ctx)
			s.Require().NoError(err)
			s.Require().Equal(tt.stdout, out)
		})
	}
}

func (s *NodeExecutorSuite) TestNodeID() {
	cmd := [][]string{{"/cockroach/cockroach", "node", "status", "--decommission", "--format", "csv"}}

	tests := []struct {
		desc string
		csv  string
		id   int64
		err  string
	}{
		{
			desc: "node found",
			csv:  "id,address\n101,other-node.host:26258\n102,test-node.test-cluster.test-ns.host:26258",
			id:   int64(102),
		},
		{
			desc: "node not found",
			csv:  "id,address\n101,other-node.host:26258\n102,alt-node.host:26258",
			err:  "node not found: test-node",
		},
		{
			desc: "invalid node id",
			csv:  "id,address\nNaN,test-node.test-cluster.test-ns.host:26258",
			err:  "invalid syntax",
		},
	}

	for _, tt := range tests {
		s.setupMock(cmd, []execOpts{{stdout: tt.csv}})

		id, err := s.nodeExec.NodeID(s.ctx)
		s.Require().Equal(tt.id, id, tt.desc)

		if tt.err != "" {
			s.Require().EqualError(errors.Cause(err), tt.err, tt.desc)
			continue
		}

		s.Require().NoError(err, tt.desc)
	}
}

func (s *NodeExecutorSuite) TestReplicasDeprecated() {
	cmd := [][]string{{"/cockroach/cockroach", "node", "status", "102", "--decommission", "--format", "csv"}}

	tests := []struct {
		desc string
		csv  string
		err  string
		reps uint64
	}{
		{
			desc: "found replicas",
			csv:  "id,address,sql_address,build,started_at,updated_at,locality,is_available,is_live,gossiped_replicas,is_decommissioning,membership,is_draining\n0,test-node,test-node,0,0,0,l,1,1,302000,false,active,false",
			reps: 302000,
		},
		{
			desc: "node not found",
			csv:  "",
			err:  io.EOF.Error(),
		},
		{
			desc: "invalid replicas value",
			csv:  "id,address,sql_address,build,started_at,updated_at,locality,is_available,is_live,gossiped_replicas,is_decommissioning,membership,is_draining\n0,test-node,test-node,0,0,0,l,1,1,NERP,false,active,false",
			err:  "invalid syntax",
		},
	}

	for _, tt := range tests {
		s.setupMock(cmd, []execOpts{{stdout: tt.csv}})
		reps, err := s.nodeExec.ReplicasDeprecated(s.ctx, 102)
		s.Require().Equal(tt.reps, reps, tt.desc)

		if tt.err != "" {
			s.Require().EqualError(errors.Cause(err), tt.err, tt.desc)
			continue
		}

		s.Require().NoError(err, tt.desc)
	}
}

func (s *NodeExecutorSuite) TestReplicas() {
	cmd := [][]string{{"/cockroach/cockroach", "node", "decommission", "102", "--dry-run", "--format", "csv", "--port", "26258"}}

	tests := []struct {
		desc    string
		stdout  string
		execErr error
		err     string
		reps    uint64
	}{
		{
			desc: "found replicas with warning",
			stdout: `warning: node 2 is already decommissioning or decommissioned

id,is_live,replicas,is_decommissioning,membership,is_draining,readiness,blocking_ranges
2,true,40,true,decommissioning,true,ready,0`,
			reps: 40,
		},
		{
			desc: "found replicas no warning",
			stdout: `
id,is_live,replicas,is_decommissioning,membership,is_draining,readiness,blocking_ranges
2,true,32,true,decommissioning,true,ready,0`,
			reps: 32,
		},
		{
			desc: "replicas column missing",
			stdout: `
id,is_live,is_decommissioning,membership,is_draining,readiness,blocking_ranges
2,true,true,decommissioning,true,ready,0`,
			err: "expected csv header prefix",
		},
		{
			desc: "already decommissioned",
			stdout: `
ERROR: rpc error: code = PermissionDenied desc = failed to connect to n46 at cockroachdb-fcx62.cockroachdb.eu-west-2.svc.cluster.local:26258: initial connection heartbeat failed: grpc: n102 was permanently removed from the cluster at 2025-05-14 15:56:14.339951635 +0000 UTC; it is not allowed to rejoin the cluster [code 7/PermissionDenied], node is decommissioned
`,
			reps: 0,
		},
		{
			// The exec exits non-zero because peers reject the local process,
			// and the "permanently removed" marker lands in stdout. The
			// guard has to recognise the terminal state instead of
			// propagating the error, otherwise the reconciler loops forever.
			desc: "already decommissioned with exec error",
			stdout: `
ERROR: rpc error: code = PermissionDenied desc = failed to connect to n46 at cockroachdb-fcx62.cockroachdb.eu-west-2.svc.cluster.local:26258: initial connection heartbeat failed: grpc: n102 was permanently removed from the cluster at 2025-05-14 15:56:14.339951635 +0000 UTC; it is not allowed to rejoin the cluster [code 7/PermissionDenied], node is decommissioned
`,
			execErr: errors.New("command terminated with exit code 1"),
			reps:    0,
		},
		{
			desc: "different node already decommissioned",
			stdout: `
ERROR: rpc error: code = PermissionDenied desc = failed to connect to n46 at cockroachdb-fcx62.cockroachdb.eu-west-2.svc.cluster.local:26258: initial connection heartbeat failed: grpc: n100 was permanently removed from the cluster at 2025-05-14 15:56:14.339951635 +0000 UTC; it is not allowed to rejoin the cluster [code 7/PermissionDenied], node is decommissioned
`,
			err: "expected csv header prefix",
		},
	}

	for _, tc := range tests {
		s.setupMock(cmd, []execOpts{{stdout: tc.stdout, err: tc.execErr}})
		reps, err := s.nodeExec.Replicas(s.ctx, 102)

		if tc.err != "" {
			s.Require().ErrorContains(err, tc.err)
		} else {
			s.Require().NoError(err, tc.desc)
			s.Require().Equal(tc.reps, reps, tc.desc)
		}
	}
}

func (s *NodeExecutorSuite) TestIsDecommissioning() {
	cmd := [][]string{{"/cockroach/cockroach", "node", "decommission", "102", "--dry-run", "--format", "csv", "--port", "26258"}}
	tests := []struct {
		desc     string
		stdout   string
		execErr  error
		expected bool
		err      string
	}{
		{
			desc: "isDecomissioning true with warning",
			stdout: `warning: node 2 is already decommissioning or decommissioned

id,is_live,replicas,is_decommissioning,membership,is_draining,readiness,blocking_ranges
2,true,40,true,decommissioning,true,ready,0`,
			expected: true,
		},
		{
			desc: "isDecomissioning false with warning",
			stdout: `warning: node 2 is already decommissioning or decommissioned

id,is_live,replicas,is_decommissioning,membership,is_draining,readiness,blocking_ranges
2,true,40,false,decommissioning,true,ready,0`,
			expected: false,
		},
		{
			desc: "isDecomissioning true without warning",
			stdout: `
id,is_live,replicas,is_decommissioning,membership,is_draining,readiness,blocking_ranges
2,true,40,true,decommissioning,true,ready,0`,
			expected: true,
		},
		{
			desc: "isDecomissioning false without warning",
			stdout: `
id,is_live,replicas,is_decommissioning,membership,is_draining,readiness,blocking_ranges
2,true,40,false,decommissioning,true,ready,0`,
			expected: false,
		},
		{
			desc: "isDecomissioning column missing",
			stdout: `
id,is_live,replicas,membership,is_draining,readiness,blocking_ranges
2,true,32,decommissioning,true,ready,0`,
			err: "no column named is_decommissioning",
		},
		{
			// isDecommissioning=true so the reconciler in TransferringReplicas
			// treats the node as still-decommissioning and advances to
			// ZeroReplicas once Replicas reports 0. Matches the live CSV
			// behaviour and keeps the state machine from resetting to Drained.
			desc: "already decommissioned",
			stdout: `
ERROR: rpc error: code = PermissionDenied desc = failed to connect to n46 at cockroachdb-fcx62.cockroachdb.eu-west-2.svc.cluster.local:26258: initial connection heartbeat failed: grpc: n102 was permanently removed from the cluster at 2025-05-14 15:56:14.339951635 +0000 UTC; it is not allowed to rejoin the cluster [code 7/PermissionDenied], node is decommissioned
`,
			expected: true,
		},
		{
			// Peers reject the local process so the exec exits non-zero and
			// the "permanently removed" marker lands in stdout. The guard
			// has to run before the err check.
			desc: "already decommissioned with exec error",
			stdout: `
ERROR: rpc error: code = PermissionDenied desc = failed to connect to n46 at cockroachdb-fcx62.cockroachdb.eu-west-2.svc.cluster.local:26258: initial connection heartbeat failed: grpc: n102 was permanently removed from the cluster at 2025-05-14 15:56:14.339951635 +0000 UTC; it is not allowed to rejoin the cluster [code 7/PermissionDenied], node is decommissioned
`,
			execErr:  errors.New("command terminated with exit code 1"),
			expected: true,
		},
		{
			desc: "different node already decommissioned",
			stdout: `
ERROR: rpc error: code = PermissionDenied desc = failed to connect to n46 at cockroachdb-fcx62.cockroachdb.eu-west-2.svc.cluster.local:26258: initial connection heartbeat failed: grpc: n100 was permanently removed from the cluster at 2025-05-14 15:56:14.339951635 +0000 UTC; it is not allowed to rejoin the cluster [code 7/PermissionDenied], node is decommissioned
`,
			err: "unexpected output: expected csv header prefix 'id,is_live,replicas'",
		},
	}

	for _, tc := range tests {
		s.setupMock(cmd, []execOpts{{stdout: tc.stdout, err: tc.execErr}})
		isDecomissioning, err := s.nodeExec.IsDecommissioning(s.ctx, 102)

		if tc.err != "" {
			s.Require().ErrorContains(err, tc.err)
		} else {
			s.Require().NoError(err, tc.desc)
			s.Require().Equal(tc.expected, isDecomissioning)
		}
	}
}

func (s *NodeExecutorSuite) TestIsFullyDecommissioned() {
	cmd := [][]string{{"/cockroach/cockroach", "node", "decommission", "102", "--dry-run", "--format", "csv", "--port", "26258"}}

	tests := []struct {
		desc     string
		stdout   string
		execErr  error
		expected bool
		err      string
	}{
		{
			desc: "still decommissioning",
			stdout: `
id,is_live,replicas,is_decommissioning,membership,is_draining,readiness,blocking_ranges
2,true,40,true,decommissioning,true,ready,0`,
			expected: false,
		},
		{
			desc: "fully decommissioned via CSV",
			stdout: `
id,is_live,replicas,is_decommissioning,membership,is_draining,readiness,blocking_ranges
2,true,0,true,decommissioned,true,ready,0`,
			expected: true,
		},
		{
			// Peers reject the local process, exec exits non-zero, but the
			// "permanently removed" marker in stdout still tells us the node
			// is at terminal membership.
			desc: "severed node with exec error",
			stdout: `
ERROR: rpc error: code = PermissionDenied desc = failed to connect to n46 at cockroachdb-fcx62.cockroachdb.eu-west-2.svc.cluster.local:26258: initial connection heartbeat failed: grpc: n102 was permanently removed from the cluster at 2025-05-14 15:56:14.339951635 +0000 UTC; it is not allowed to rejoin the cluster [code 7/PermissionDenied], node is decommissioned
`,
			execErr:  errors.New("command terminated with exit code 1"),
			expected: true,
		},
		{
			desc:    "exec error without marker",
			execErr: errors.New("boom"),
			err:     "boom",
		},
	}

	for _, tc := range tests {
		s.setupMock(cmd, []execOpts{{stdout: tc.stdout, err: tc.execErr}})
		result, err := s.nodeExec.IsFullyDecommissioned(s.ctx, 102)

		if tc.err != "" {
			s.Require().ErrorContains(err, tc.err)
		} else {
			s.Require().NoError(err, tc.desc)
			s.Require().Equal(tc.expected, result, tc.desc)
		}
	}
}

func (s *NodeExecutorSuite) TestClusterHealth() {
	cmd := [][]string{{
		"/cockroach/cockroach", "node", "status", "--ranges",
		"--format", "csv", "--port", "26257",
	}}

	for _, tc := range []struct {
		name                    string
		stdout                  string
		execErr                 error
		expectedUnderReplicated int64
		expectedLiveNodes       int32
		expectErr               string
	}{
		{
			name:                    "sums ranges and counts live nodes",
			stdout:                  "id,is_live,ranges_underreplicated\n1,true,2\n2,true,3\n3,false,0\n",
			expectedUnderReplicated: 5,
			expectedLiveNodes:       2,
		},
		{
			name:                    "finds columns by name and tolerates tty line endings",
			stdout:                  "ranges_underreplicated,id,is_live\r\n0,1,true\r\n",
			expectedUnderReplicated: 0,
			expectedLiveNodes:       1,
		},
		{
			// The real `node status --ranges` output carries the full base
			// column set, including a quoted locality that itself contains a
			// comma. Indexing by header name and letting encoding/csv handle
			// the quoting has to survive that shape.
			name: "parses realistic node status --ranges output",
			stdout: "id,address,sql_address,build,started_at,updated_at,locality,is_available,is_live,replicas_leaders,replicas_leaseholders,ranges,ranges_unavailable,ranges_underreplicated\n" +
				"1,10.0.0.1:26257,10.0.0.1:26257,v24.3.0,2026-01-01 00:00:00,2026-01-01 00:05:00,\"region=us-east1,az=a\",true,true,20,18,120,0,2\n" +
				"2,10.0.0.2:26257,10.0.0.2:26257,v24.3.0,2026-01-01 00:00:00,2026-01-01 00:05:00,\"region=us-east1,az=b\",true,true,19,17,118,0,1\n" +
				"3,10.0.0.3:26257,10.0.0.3:26257,v24.3.0,2026-01-01 00:00:00,2026-01-01 00:05:00,\"region=us-west1,az=a\",false,false,0,0,0,0,0\n",
			expectedUnderReplicated: 3,
			expectedLiveNodes:       2,
		},
		{
			// A down or decommissioning node has no store status, so node status
			// left-joins a NULL range count for it. That node counts as zero
			// under-replicated ranges and is not live.
			name:                    "treats NULL range count as zero for a node without store status",
			stdout:                  "id,is_live,ranges_underreplicated\n1,true,2\n2,false,NULL\n",
			expectedUnderReplicated: 2,
			expectedLiveNodes:       1,
		},
		{
			name:                    "skips leading notice line before the header",
			stdout:                  "warning: connecting to cluster over insecure connection\nid,is_live,ranges_underreplicated\n1,true,4\n",
			expectedUnderReplicated: 4,
			expectedLiveNodes:       1,
		},
		{
			name:                    "tolerates a blank line between data rows",
			stdout:                  "id,is_live,ranges_underreplicated\n1,true,2\n   \n2,true,1\n",
			expectedUnderReplicated: 3,
			expectedLiveNodes:       2,
		},
		{
			// A truncated or garbled row must fail the check rather than proceed
			// on a partial count that could hide under-replicated ranges.
			name:      "short data row errors",
			stdout:    "id,is_live,ranges_underreplicated\n1,true,2\n3,true\n",
			expectErr: "unexpected node status CSV row",
		},
		{
			name:      "missing header errors",
			stdout:    "id,is_live\n1,true\n",
			expectErr: "missing ranges_underreplicated or is_live header",
		},
		{
			name:      "no node rows errors",
			stdout:    "id,is_live,ranges_underreplicated\n",
			expectErr: "node status returned no nodes",
		},
		{
			name:      "non-numeric range count errors",
			stdout:    "id,is_live,ranges_underreplicated\n1,true,NaN\n",
			expectErr: "parsing under-replicated range count",
		},
		{
			name:      "invalid live status errors",
			stdout:    "id,is_live,ranges_underreplicated\n1,unknown,0\n",
			expectErr: "parsing live node status",
		},
		{name: "exec failure propagates", execErr: errors.New("boom"), expectErr: "boom"},
	} {
		s.Run(tc.name, func() {
			s.setupMock(cmd, []execOpts{{stdout: tc.stdout, err: tc.execErr}})
			underReplicated, liveNodes, err := s.nodeExec.ClusterHealth(s.ctx)
			if tc.expectErr != "" {
				s.Require().ErrorContains(err, tc.expectErr)
				return
			}
			s.Require().NoError(err)
			s.Require().Equal(tc.expectedUnderReplicated, underReplicated)
			s.Require().Equal(tc.expectedLiveNodes, liveNodes)
		})
	}
}

func (s *NodeExecutorSuite) TestUnderReplicatedRanges() {
	cmd := [][]string{{
		"/cockroach/cockroach", "node", "status", "--ranges",
		"--format", "csv", "--port", "26257",
	}}

	s.setupMock(cmd, []execOpts{{stdout: "id,is_live,ranges_underreplicated\n1,true,3\n"}})
	count, err := s.nodeExec.UnderReplicatedRanges(s.ctx)
	s.Require().NoError(err)
	s.Require().Equal(int64(3), count)
}

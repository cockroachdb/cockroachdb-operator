package betaclusterctrl_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	. "github.com/cockroachlabs/cockroach-enterprise-operator/controllers/betaclusterctrl"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type fakeExec struct {
	fn func(context.Context, string, string, kube.ExecOptions) error
}

func (f *fakeExec) Exec(ctx context.Context, pod, container string, opts kube.ExecOptions) error {
	return f.fn(ctx, pod, container, opts)
}

func TestInitializeCluster(t *testing.T) {
	tests := []struct {
		desc   string
		node   crdbv1beta1.CrdbNode
		errStr string
		execFn func(context.Context, string, string, kube.ExecOptions) error
	}{
		{
			desc: "initialize cluster",
			node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "n1",
					Namespace: "bogus-ns",
				},
				Status: crdbv1beta1.CrdbNodeStatus{
					Conditions: []crdbv1beta1.NodeCondition{
						{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue},
					},
				},
			},
			execFn: func(ctx context.Context, pod, container string, _ kube.ExecOptions) error {
				require.Equal(t, "n1", pod)
				require.Equal(t, "cockroachdb", container)
				deadline, ok := ctx.Deadline()
				require.True(t, ok, "cockroach init context must have a deadline")
				require.WithinDuration(t, time.Now().Add(5*time.Minute), deadline, time.Second)
				return nil
			},
		},
		{
			desc: "error initializing cluster",
			node: crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "n1",
					Namespace: "bogus-ns",
				},
				Status: crdbv1beta1.CrdbNodeStatus{
					Conditions: []crdbv1beta1.NodeCondition{
						{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue},
					},
				},
			},
			execFn: func(ctx context.Context, pod, container string, _ kube.ExecOptions) error {
				return errors.New("Boom")
			},
			errStr: "Boom",
		},
	}

	ctx := context.Background()
	for _, tt := range tests {
		r := NewReconciler(nil, nil, nil)
		err := r.Initialize(ctx, &tt.node, nil, &fakeExec{fn: tt.execFn})
		if tt.errStr != "" {
			require.EqualError(t, errors.Cause(err), tt.errStr, tt.desc)
			continue
		}

		require.NoError(t, err, tt.desc)
	}
}

func TestInitializeReturnsContextError(t *testing.T) {
	node := &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{Name: "n1", Namespace: "bogus-ns"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	r := NewReconciler(nil, nil, nil)
	err := r.Initialize(ctx, node, nil, &fakeExec{
		fn: func(ctx context.Context, _, _ string, _ kube.ExecOptions) error {
			require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
			return errors.New("exec stream closed")
		},
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestInitializeMRCreate(t *testing.T) {
	observedZapCore, observedLogs := observer.New(zap.InfoLevel)
	observedLogger := zap.New(observedZapCore)
	logging.SetGlobalLogger(observedLogger)

	node := &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "n1",
			Namespace: "bogus-ns",
		},
		Status: crdbv1beta1.CrdbNodeStatus{
			Conditions: []crdbv1beta1.NodeCondition{
				{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue},
			},
		},
	}

	regions := []crdbv1beta1.CrdbClusterRegion{
		{
			Code: "bogus-ns",
		},
		{
			Code: "bogus-ns-2",
		},
	}

	fe := &fakeExec{
		fn: func(ctx context.Context, pod, container string, options kube.ExecOptions) error {
			for _, component := range options.Command {
				if strings.Contains(component, "init") {
					return nil
				}
			}
			return errors.New("failed to make contact")
		},
	}

	r := NewReconciler(nil, nil, nil)
	require.NoError(t, r.Initialize(context.Background(), node, regions, fe))

	triedToProbeLog := false
	logs := observedLogs.TakeAll()
	for _, log := range logs {
		if strings.Contains(log.Entry.Message, "attempted to contact another region \"bogus-ns-2\" (namespace \"bogus-ns-2\")") &&
			strings.Contains(log.Entry.Message, "failed to make contact") {
			triedToProbeLog = true
		}
	}
	require.True(t, triedToProbeLog)
}

func TestInitializeMRAddRegion(t *testing.T) {
	tests := []struct {
		desc           string
		node           *crdbv1beta1.CrdbNode
		regions        []crdbv1beta1.CrdbClusterRegion
		wantJoinFQDN   string
		wantSuccessLog string
		wantExecCalls  int
	}{
		{
			desc: "code-as-namespace fallback",
			node: &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "n1",
					Namespace: "bogus-ns",
					Labels: map[string]string{
						manifests.BetaCrdbClusterNodeOwnerLabelKey: "crdb-cockroachdb",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{TLSEnabled: true},
				Status: crdbv1beta1.CrdbNodeStatus{
					Conditions: []crdbv1beta1.NodeCondition{
						{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue},
					},
				},
			},
			regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "bogus-ns"},
				{Code: "bogus-ns-2", Domain: "cluster.local"},
			},
			wantJoinFQDN:   "crdb-cockroachdb-join.bogus-ns-2.svc.cluster.local",
			wantSuccessLog: "successfully probed region \"bogus-ns-2\"; skipping init",
			wantExecCalls:  1,
		},
		{
			desc: "explicit namespace override",
			node: &crdbv1beta1.CrdbNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "n1",
					Namespace: "cockroach-ns",
					Labels: map[string]string{
						manifests.BetaCrdbClusterNodeOwnerLabelKey: "crdb-cockroachdb",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{TLSEnabled: true},
				Status: crdbv1beta1.CrdbNodeStatus{
					Conditions: []crdbv1beta1.NodeCondition{
						{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue},
					},
				},
			},
			regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east1", Namespace: "cockroach-ns", Domain: "cluster.local"},
				{Code: "us-west1", Namespace: "cockroach-ns-2", Domain: "cluster.local"},
			},
			wantJoinFQDN:   "crdb-cockroachdb-join.cockroach-ns-2.svc.cluster.local",
			wantSuccessLog: "successfully probed region \"us-west1\"; skipping init",
			wantExecCalls:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			observedZapCore, observedLogs := observer.New(zap.InfoLevel)
			observedLogger := zap.New(observedZapCore)
			logging.SetGlobalLogger(observedLogger)

			execCalled := 0
			fe := &fakeExec{
				fn: func(ctx context.Context, pod, container string, options kube.ExecOptions) error {
					execCalled++
					require.Equal(t, "n1", pod)
					require.Equal(t, "cockroachdb", container)

					cmd := strings.Join(options.Command, " ")
					require.Contains(t, cmd, tt.wantJoinFQDN)
					require.Contains(t, cmd, "?sslmode=verify-ca")
					return nil
				},
			}

			r := NewReconciler(nil, nil, nil)
			require.NoError(t, r.Initialize(context.Background(), tt.node, tt.regions, fe))
			require.Equal(t, tt.wantExecCalls, execCalled, "unexpected number of exec calls")

			foundSuccessfulProbeLog := false
			logs := observedLogs.TakeAll()
			for _, log := range logs {
				if log.Entry.Message == tt.wantSuccessLog {
					foundSuccessfulProbeLog = true
				}
			}
			require.True(t, foundSuccessfulProbeLog)
		})
	}
}

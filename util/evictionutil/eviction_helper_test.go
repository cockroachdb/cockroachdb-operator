package evictionutil

import (
	"context"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/utils/timeutils"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCanEvictTableDriven(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(h *Helper, now time.Time) *crdbv1beta1.CrdbNode
		preCheck   func(h *Helper, node *crdbv1beta1.CrdbNode, now time.Time)
		wantOk     bool
		wantDelay  time.Duration
		extraDelay time.Duration
	}{
		{
			name: "unowned node always evictable",
			setup: func(h *Helper, now time.Time) *crdbv1beta1.CrdbNode {
				return &crdbv1beta1.CrdbNode{}
			},
			wantOk:    true,
			wantDelay: 0,
		},
		{
			name: "unregistered cluster blocks eviction",
			setup: func(h *Helper, now time.Time) *crdbv1beta1.CrdbNode {
				return nodeWithOwner("cluster-a", "ns")
			},
			wantOk:    false,
			wantDelay: time.Second,
		},
		{
			name: "allowed but no pod timestamp blocks until delay passes",
			setup: func(h *Helper, now time.Time) *crdbv1beta1.CrdbNode {
				cluster := clusterWithDelay("cluster-a", "ns", 5*time.Second)
				h.AllowNextEvictionFor(cluster)
				return nodeWithOwner("cluster-a", "ns")
			},
			wantOk:    false,
			wantDelay: 5 * time.Second,
		},
		{
			// AllowNextEvictionFor sets lastPodStarted to now as a safety floor.
			// We advance time 1m, then push a pod start 2s ago. Since 2s < 5s
			// delay, the eviction is blocked with 3s remaining.
			name:       "allowed with recent pod start within delay window",
			extraDelay: time.Minute,
			setup: func(h *Helper, now time.Time) *crdbv1beta1.CrdbNode {
				cluster := clusterWithDelay("cluster-a", "ns", 5*time.Second)
				h.AllowNextEvictionFor(cluster)
				return nodeWithOwner("cluster-a", "ns")
			},
			preCheck: func(h *Helper, node *crdbv1beta1.CrdbNode, now time.Time) {
				h.UpdateStateFrom(context.Background(), node, runningPodStatus(now.Add(-2*time.Second)))
			},
			wantOk:    false,
			wantDelay: 3 * time.Second,
		},
		{
			name:       "allowed with pod start beyond delay window",
			extraDelay: time.Minute,
			setup: func(h *Helper, now time.Time) *crdbv1beta1.CrdbNode {
				cluster := clusterWithDelay("cluster-a", "ns", 5*time.Second)
				h.AllowNextEvictionFor(cluster)
				return nodeWithOwner("cluster-a", "ns")
			},
			preCheck: func(h *Helper, node *crdbv1beta1.CrdbNode, now time.Time) {
				h.UpdateStateFrom(context.Background(), node, runningPodStatus(now.Add(-10*time.Second)))
			},
			wantOk:    true,
			wantDelay: 0,
		},
		{
			name:       "second eviction blocked after first consumed",
			extraDelay: time.Minute,
			setup: func(h *Helper, now time.Time) *crdbv1beta1.CrdbNode {
				cluster := clusterWithDelay("cluster-a", "ns", 5*time.Second)
				h.AllowNextEvictionFor(cluster)
				return nodeWithOwner("cluster-a", "ns")
			},
			preCheck: func(h *Helper, node *crdbv1beta1.CrdbNode, now time.Time) {
				h.UpdateStateFrom(context.Background(), node, runningPodStatus(now.Add(-10*time.Second)))
				ok, _ := h.CanEvict(context.Background(), node)
				require.True(t, ok)
			},
			wantOk:    false,
			wantDelay: 5 * time.Second,
		},
		{
			// zero delay means sinceLastRestart(0) > delay(0) is false, so the
			// eviction gate still blocks. This is the intended behavior because
			// a zero-duration delay is a strict boundary, not an exemption.
			name: "zero delay still blocks when pod just started",
			setup: func(h *Helper, now time.Time) *crdbv1beta1.CrdbNode {
				cluster := clusterWithDelay("cluster-a", "ns", 0)
				h.AllowNextEvictionFor(cluster)
				node := nodeWithOwner("cluster-a", "ns")
				h.UpdateStateFrom(context.Background(), node, runningPodStatus(now))
				return node
			},
			wantOk:    false,
			wantDelay: 0,
		},
		{
			name:       "disable after allow blocks eviction",
			extraDelay: time.Minute,
			setup: func(h *Helper, now time.Time) *crdbv1beta1.CrdbNode {
				cluster := clusterWithDelay("cluster-a", "ns", 5*time.Second)
				h.AllowNextEvictionFor(cluster)
				h.DisableEvictionsFor(cluster)
				return nodeWithOwner("cluster-a", "ns")
			},
			preCheck: func(h *Helper, node *crdbv1beta1.CrdbNode, now time.Time) {
				h.UpdateStateFrom(context.Background(), node, runningPodStatus(now.Add(-10*time.Second)))
			},
			wantOk:    false,
			wantDelay: 5 * time.Second,
		},
		{
			name:       "two nodes same cluster share eviction gate",
			extraDelay: time.Minute,
			setup: func(h *Helper, now time.Time) *crdbv1beta1.CrdbNode {
				cluster := clusterWithDelay("cluster-a", "ns", 5*time.Second)
				h.AllowNextEvictionFor(cluster)
				return nodeWithOwnerAndName("node-2", "cluster-a", "ns")
			},
			preCheck: func(h *Helper, node *crdbv1beta1.CrdbNode, now time.Time) {
				node1 := nodeWithOwnerAndName("node-1", "cluster-a", "ns")
				h.UpdateStateFrom(context.Background(), node1, runningPodStatus(now.Add(-10*time.Second)))
				h.UpdateStateFrom(context.Background(), node, runningPodStatus(now.Add(-10*time.Second)))
				ok, _ := h.CanEvict(context.Background(), node1)
				require.True(t, ok)
			},
			wantOk:    false,
			wantDelay: 5 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := timeutils.Now()
			defer timeutils.Freeze(func() time.Time { return now })()

			h := &Helper{UnknownDelay: time.Second}
			node := tt.setup(h, now)

			if tt.extraDelay > 0 {
				now = now.Add(tt.extraDelay)
			}

			if tt.preCheck != nil {
				tt.preCheck(h, node, now)
			}

			ok, delay := h.CanEvict(context.Background(), node)
			require.Equal(t, tt.wantOk, ok, "CanEvict ok")
			require.Equal(t, tt.wantDelay, delay, "CanEvict delay")
		})
	}
}

func nodeWithOwner(clusterName, namespace string) *crdbv1beta1.CrdbNode {
	return nodeWithOwnerAndName("node-0", clusterName, namespace)
}

func nodeWithOwnerAndName(nodeName, clusterName, namespace string) *crdbv1beta1.CrdbNode {
	return &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodeName,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{Name: clusterName},
			},
		},
	}
}

func clusterWithDelay(name, namespace string, delay time.Duration) *crdbv1beta1.CrdbCluster {
	return &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			RollingRestartDelay: &metav1.Duration{Duration: delay},
		},
	}
}

func runningPodStatus(startedAt time.Time) corev1.PodStatus {
	return corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{
			{
				State: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{
						StartedAt: metav1.NewTime(startedAt),
					},
				},
			},
		},
	}
}

func TestHelper(t *testing.T) {
	now := timeutils.Now()
	defer timeutils.Freeze(func() time.Time { return now })()

	ctx := context.Background()
	h := &Helper{UnknownDelay: time.Second}

	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "Name",
			Namespace: "Namespace",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			RollingRestartDelay: &metav1.Duration{
				Duration: 5 * time.Second,
			},
		},
	}

	node1 := &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "Node1",
			Namespace: "Namespace",
			OwnerReferences: []metav1.OwnerReference{
				{Name: "Name"},
			},
		},
	}

	node1PodStatus := corev1.PodStatus{}

	// Evictions are always permitted for "unowned" nodes.
	ok, wait := h.CanEvict(ctx, &crdbv1beta1.CrdbNode{})
	require.True(t, ok)
	require.Zero(t, wait)

	// If a cluster hasn't been "registered", evictions are disallowed.
	ok, wait = h.CanEvict(ctx, node1)
	require.False(t, ok)
	require.Equal(t, h.UnknownDelay, wait)

	// If evictions are allowed but a "PodRunning" condition hasn't been recorded,
	// evictions are disallowed.
	h.AllowNextEvictionFor(cluster)
	ok, wait = h.CanEvict(ctx, node1)
	require.False(t, ok)
	require.Equal(t, cluster.Spec.RollingRestartDelay.Duration, wait)

	// Same as above but we push an update from a CrdbNode that has no running
	// Pod.
	h.UpdateStateFrom(ctx, node1, node1PodStatus)
	ok, wait = h.CanEvict(ctx, node1)
	require.False(t, ok)
	require.Equal(t, cluster.Spec.RollingRestartDelay.Duration, wait)

	// Configure node1's Pod to be running.
	node1PodStatus.Phase = corev1.PodRunning
	node1PodStatus.ContainerStatuses = append(node1PodStatus.ContainerStatuses, corev1.ContainerStatus{
		State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{
				StartedAt: metav1.NewTime(timeutils.Now().Add(10 * time.Second)),
			},
		},
	})

	// Updates from the future will cause a panic.
	require.Panics(t, func() {
		h.UpdateStateFrom(ctx, node1, node1PodStatus)
	})

	// Push in a reasonable timestamp and see that it's ignored! The eviction
	// helper assumes that the last restart timestamps happen when it's
	// initialized as a safety mechanism.
	node1PodStatus.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(timeutils.Now().Add(-10 * time.Second))
	h.UpdateStateFrom(ctx, node1, node1PodStatus)
	ok, wait = h.CanEvict(ctx, node1)
	require.False(t, ok)
	require.Equal(t, cluster.Spec.RollingRestartDelay.Duration, wait)

	// March the time ahead 1m and push in a now, correctly in the past
	// timestamp.
	now = now.Add(time.Minute)
	node1PodStatus.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(timeutils.Now().Add(-10 * time.Second))
	h.UpdateStateFrom(ctx, node1, node1PodStatus)

	// Evictions are allowed because we've had a Pod that started running 10
	// seconds ago.
	h.UpdateStateFrom(ctx, node1, node1PodStatus)
	ok, wait = h.CanEvict(ctx, node1)
	require.True(t, ok)
	require.Zero(t, wait)

	// No evictions will be allowed because AllowNextEvictionFor hasn't been
	// called. We're informed to wait the full time because the last start
	// timestamp is longer ago than our delay.
	h.UpdateStateFrom(ctx, node1, node1PodStatus)
	ok, wait = h.CanEvict(ctx, node1)
	require.False(t, ok)
	require.Equal(t, cluster.Spec.RollingRestartDelay.Duration, wait)

	// If there's a later timestamp, UpdateStateFrom will accept it and now
	// will tell us to wait a shorted period of time.
	node1PodStatus.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(timeutils.Now().Add(-2 * time.Second))
	h.UpdateStateFrom(ctx, node1, node1PodStatus)
	ok, wait = h.CanEvict(ctx, node1)
	require.False(t, ok)
	require.Equal(t, 3*time.Second, wait)

	// Allow and then disable evictions after marching ahead 1 minute.
	// Evictions are should still be disabled.
	now = now.Add(time.Minute)
	h.AllowNextEvictionFor(cluster)
	h.DisableEvictionsFor(cluster)

	// They're still disabled.
	ok, wait = h.CanEvict(ctx, node1)
	require.False(t, ok)
	require.Equal(t, cluster.Spec.RollingRestartDelay.Duration, wait)

	// And they're now immediately available once we unbar them.
	h.AllowNextEvictionFor(cluster)
	ok, wait = h.CanEvict(ctx, node1)
	require.True(t, ok)
	require.Zero(t, wait)

	// Push in a state that would require us to wait some time to ensure that
	// allowing and disabling doesn't clear any timestamps.
	node1PodStatus.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(timeutils.Now().Add(-2 * time.Second))
	h.UpdateStateFrom(ctx, node1, node1PodStatus)
	h.AllowNextEvictionFor(cluster)
	h.DisableEvictionsFor(cluster)

	ok, wait = h.CanEvict(ctx, node1)
	require.False(t, ok)
	require.Equal(t, 3*time.Second, wait)

	// Even after allowing evictions, we have to wait 3 more seconds.
	h.AllowNextEvictionFor(cluster)
	ok, wait = h.CanEvict(ctx, node1)
	require.False(t, ok)
	require.Equal(t, 3*time.Second, wait)
}

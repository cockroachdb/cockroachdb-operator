package evictionutil

import (
	"context"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/utils/timeutils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

/*
This file contains a simple in-memory store
of the last time a pod is evicted for a cluster.
This is used by the node reconciler to determine
when it is safe to evict a pod. It will wait
for a configurable amount of time before evicting
the next pod if an update is required.

Note that in the clusterctrl, we have a similar
logic for rolling restart.

The logic here is still necessary, because
if manifests for node -> pod changes (in an operator
version update), nodectrl will evict all pods
without clusterctrl's knowledge.

There are two methods that are used by clusterctrl,
and nodectrl respectively.
clusterctrl uses `SetEvictionVal` to set the
next allowed eviction timestamp, or to clear the
eviction timestamp when it detects unhealthiness in
the cluster.
nodectrl calls `AttemptEviction` before attempting
the next eviction. If the `Attempt` is successful,
it also clears the next eviction timestamp, effectively
blocking the next `AttemptEviction` (called for another
CrdbNode), until the clusterctrl sets the timestamp
again.

If the operator restarts in-between node evctions, the
next eviction will still be delayed for (at least)
the correct amount, because nodectrl will wait until
clusterctrl reads the state of the world and sets
the correct eviction timestamp.
*/

type evictionInfo struct {
	delay          time.Duration
	allowEviction  bool
	lastPodStarted time.Time
}

type Helper struct {
	mu          sync.Mutex
	evictionMap map[types.NamespacedName]evictionInfo

	UnknownDelay time.Duration
}

// AllowNextEvictionFor allows one eviction to occur for the CrdbNodes of the
// given cluster.
func (h *Helper) AllowNextEvictionFor(cluster *crdbv1beta1.CrdbCluster) {
	h.setEvictionStateFor(cluster, true)
}

// DisableEvictionsFor disabled evictions for the given cluster until they are
// explicitly enabled with [AllowNextEvictionFor].
func (h *Helper) DisableEvictionsFor(cluster *crdbv1beta1.CrdbCluster) {
	h.setEvictionStateFor(cluster, false)
}

// UpdateStateFrom records the LastTransitionTime of the given CrdbNode's
// PodRunning state. This timestamp is used to pace evictions of CrdbNode Pods.
func (h *Helper) UpdateStateFrom(
	ctx context.Context, node *crdbv1beta1.CrdbNode, podStatus corev1.PodStatus,
) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.setDefaultsLocked()

	if len(node.OwnerReferences) == 0 {
		logging.Warnf(ctx, "CrdbNode/%s has no OwnerReferences. Ignoring UpdateStateFrom call", node.Name)
		return
	}

	owner := types.NamespacedName{
		Name:      node.OwnerReferences[0].Name,
		Namespace: node.Namespace,
	}

	state, ok := h.evictionMap[owner]
	if !ok {
		return
	}

	// We could rely on the Ready condition but it's possible for that value to
	// flip back and forth in an unhealthy cluster and we don't want to
	// accidentally prevent eviction in such a case.
	// Our next best option is the most recent StartedAt timestamp from all
	// containers of this Pod. This timestamp will represent the time that the
	// Pod was actually running, which isn't otherwise provided.
	ts, ok := mostRecentRunningContainerStartedAt(podStatus)
	// If we can't find a reasonable timestamp or this timestamp is before our
	// recorded time, ignore it!
	if !ok || ts.Before(state.lastPodStarted) {
		return
	}

	now := timeutils.Now()
	if now.Before(ts) {
		panic(errors.AssertionFailedf("provided timestamp of LastRunning is after Now. %s > %s", ts, now))
	}

	state.lastPodStarted = ts
	h.evictionMap[owner] = state
}

// CanEvict checks if the given CrdbNode can be evicted. If it may not be
// evicted, a duration to retry after is provided. If an eviction is allowed,
// [AllowNextEvictionFor] must be called before CanEvict will return true
// again.
// NOTE: If OwnerReferences is empty on CrdbNode, CanEvict will always return
// true.
func (h *Helper) CanEvict(
	ctx context.Context, node *crdbv1beta1.CrdbNode,
) (ok bool, delay time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.setDefaultsLocked()

	if len(node.OwnerReferences) == 0 {
		logging.Warnf(ctx, "CrdbNode/%s has no OwnerReferences. Allowing eviction.", node.Name)
		return true, 0
	}

	owner := types.NamespacedName{
		Name:      node.OwnerReferences[0].Name,
		Namespace: node.Namespace,
	}

	state, ok := h.evictionMap[owner]
	if !ok {
		return false, h.UnknownDelay
	}

	// Sanity check to make sure that we're not bitten by zero value datetime
	// math. This state should be unreachable as all methods that write to
	// state should set lastPodStarted.
	if state.lastPodStarted.IsZero() {
		panic(errors.AssertionFailedf(".lastPodStarted was zero for %s", owner))
	}

	now := timeutils.Now()
	sinceLastRestart := now.Sub(state.lastPodStarted)
	delayThresholdPassed := sinceLastRestart > state.delay

	// Clamp our maximum delay value to be no greater than state.delay.
	delay = state.delay - sinceLastRestart
	if delayThresholdPassed {
		delay = state.delay
	}

	if state.allowEviction && delayThresholdPassed {
		state.allowEviction = false
		h.evictionMap[owner] = state

		return true, 0
	}

	return false, delay
}

func (h *Helper) setDefaultsLocked() {
	if h.evictionMap == nil {
		h.evictionMap = make(map[types.NamespacedName]evictionInfo)
	}

	if h.UnknownDelay == 0 {
		h.UnknownDelay = 30 * time.Second
	}
}

func (h *Helper) setEvictionStateFor(cluster *crdbv1beta1.CrdbCluster, allowEviction bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.setDefaultsLocked()

	name := types.NamespacedName{
		Name:      cluster.Name,
		Namespace: cluster.Namespace,
	}

	now := timeutils.Now()
	state := h.evictionMap[name]

	if state.lastPodStarted.IsZero() {
		state.lastPodStarted = now
	}

	state.allowEviction = allowEviction
	state.delay = cluster.Spec.RollingRestartDelay.Duration

	h.evictionMap[name] = state
}

func mostRecentRunningContainerStartedAt(status corev1.PodStatus) (ts time.Time, ok bool) {
	if status.Phase != corev1.PodRunning {
		return time.Time{}, false
	}

	var mostRecentRunning time.Time
	for _, container := range status.ContainerStatuses {
		if container.State.Running == nil {
			continue
		}
		if mostRecentRunning.Before(container.State.Running.StartedAt.Time) {
			mostRecentRunning = container.State.Running.StartedAt.Time
		}
	}

	if mostRecentRunning.IsZero() {
		return time.Time{}, false
	}

	return mostRecentRunning, true
}

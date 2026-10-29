package manager

import (
	"context"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	opmetrics "github.com/cockroachlabs/cockroach-enterprise-operator/controllers/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
	corev1 "k8s.io/api/core/v1"
)

var oomPollPeriod = 30 * time.Second

// OOMKiller exists to work around a K8s bug that results in CRDB pods waiting
// the full GracePeriodTerminationSeconds even during hard-evictions (e.g. an
// OOM). This can prolong outages for the duration of the pod's grace period.
// The OOMKiller polls for Evicted CRDB pods and SIGKILLs CRDB directly.
// https://github.com/kubernetes/kubernetes/issues/115819
type OOMKiller struct {
	ctl *kube.Ctl
	// empty = global mode
	watchNamespaces []string
	nodeExecutor    func(node *crdbv1beta1.CrdbNode, exec kubecrdbnode.Executor) kubecrdbnode.NodeExecutor
}

func NewOOMKiller(ctl *kube.Ctl, watchNamespaces []string) *OOMKiller {
	return &OOMKiller{
		ctl:             ctl,
		watchNamespaces: watchNamespaces,
		nodeExecutor:    kubecrdbnode.NewNodeExecutor,
	}
}

func (killer *OOMKiller) Start(ctx context.Context) error {
	// Ensure that the timer fires immediately once it starts.
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			logging.Debugf(ctx, "checking for evicted pods due to node resource pressure")
			if err := killer.killHardEvictedCrdbPods(ctx); err != nil {
				logging.Errorf(ctx, "%v", err)
			}
			timer.Reset(timeutil.Jitter(oomPollPeriod, 0.1))
		}
	}
}

func (killer *OOMKiller) killHardEvictedCrdbPods(ctx context.Context) error {
	evicted, err := killer.getAllEvictedCrdbPods(ctx)
	if err != nil {
		return errors.WithMessage(err, "fetching all CockroachDB pods")
	}

	var finalErr error
	for _, pod := range evicted {
		opmetrics.CrdbPodHardEvictionKillsCounter.Inc()
		logging.Infof(ctx, "killing pod %s", pod.Name)

		ctl := killer.ctl.WithNamespace(pod.Namespace)
		node := &crdbv1beta1.CrdbNode{}
		if err := ctl.Get(ctx, pod.Name, node); err != nil {
			finalErr = errors.CombineErrors(finalErr, errors.WithMessagef(err, "fetching CrdbNode for pod %s", pod.Name))
			continue
		}

		exe := killer.nodeExecutor(node, ctl)
		if err := exe.Kill(ctx); err != nil {
			finalErr = errors.CombineErrors(finalErr, err)
		}
	}

	return finalErr
}

// getAllEvictedCrdbPods returns all evicted CRDB pods. In namespace-scoped mode,
// only the watched namespaces are checked. In global mode, all namespaces are
// enumerated.
func (killer *OOMKiller) getAllEvictedCrdbPods(ctx context.Context) ([]*corev1.Pod, error) {
	namespaces := killer.watchNamespaces
	if len(namespaces) == 0 {
		// Global mode: list all namespaces in the cluster.
		nsList := corev1.NamespaceList{}
		if err := killer.ctl.List(ctx, &kube.ListOptions{}, &nsList); err != nil {
			return nil, err
		}
		for _, ns := range nsList.Items {
			namespaces = append(namespaces, ns.Name)
		}
	}

	crdbPods := []*corev1.Pod{}
	for _, ns := range namespaces {
		var pods corev1.PodList

		ctl := killer.ctl.WithNamespace(ns)
		if err := ctl.List(ctx, &kube.ListOptions{LabelSelector: kube.ParseLabelOrDie("app=cockroachdb")}, &pods); err != nil {
			return nil, err
		}

		for i := range pods.Items {
			pod := &pods.Items[i]
			if killer.isPodHardEvicted(pod) {
				crdbPods = append(crdbPods, pod)
			}
		}
	}

	return crdbPods, nil
}

// This function only returns true during kubelet-initiated eviction due
// to resource pressure, which is distinct from API-initiated eviction, for
// example during a node drain.
func (killer *OOMKiller) isPodHardEvicted(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodFailed && pod.Status.Reason == "Evicted"
}

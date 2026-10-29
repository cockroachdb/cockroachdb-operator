package kube

import (
	"context"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/backoff"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

var (
	DisruptionAllowedPollTime = 5 * time.Second
	// We should wait at least as long as we wait for CRDB to terminate.
	// See manifests/crdb_node.go (we can't import here because of an import cycle)
	DisruptionAllowedWaitTime = 45 * time.Minute
)

// Cordon marks the passed-in node as unschedulable.
func (c *Ctl) Cordon(ctx context.Context, node *corev1.Node) error {
	logging.Debugf(ctx, "cordoning node %s", node.Name)

	// Get the latest version of the node to avoid conflicts with null vs false unschedulable field
	var latestNode corev1.Node
	if err := c.Get(ctx, node.Name, &latestNode); err != nil {
		return errors.Wrapf(err, "cannot cordon node %s", node.Name)
	}

	// Only proceed if the node is not already unschedulable
	if latestNode.Spec.Unschedulable {
		// Update the original node object to reflect the current state
		node.Spec.Unschedulable = true
		return nil
	}
	latestNode.Spec.Unschedulable = true
	// Nodes often carry cluster-managed fields and stale client-side apply
	// state. Cordon only needs to flip spec.unschedulable, so preserve the
	// existing last-applied annotation and tolerate drift elsewhere.
	if err := c.ApplyWithOptions(ctx, ApplyOptions{
		Overwrite:               true,
		NoWait:                  true,
		NoLastAppliedAnnotation: true,
	}, &latestNode); err != nil {
		return errors.Wrapf(err, "cordoning node %s", node.Name)
	}

	// Update the original node object to reflect the change for callers
	node.Spec.Unschedulable = true
	return nil
}

func disruptionAllowed(conditionList []metav1.Condition) bool {
	for _, condition := range conditionList {
		if condition.Type != policyv1.DisruptionAllowedCondition {
			continue
		}
		return condition.Status == metav1.ConditionTrue
	}
	return false
}

// waitForDisruptionAllowed polls each PDB until its DisruptionAllowed condition is True.
func (c *Ctl) waitForDisruptionAllowed(
	ctx context.Context, pdbs []policyv1.PodDisruptionBudget,
) error {
	for _, pdb := range pdbs {
		logging.Debugf(ctx, "waiting for Pod Disruption Budget %s/%s to allow disruption", pdb.Namespace, pdb.Name)

		if err := backoff.Run(ctx, func() error {
			var latest policyv1.PodDisruptionBudget
			if err := c.Get(ctx, pdb.Name, &latest); err != nil {
				return errors.Wrapf(err, "getting Pod Disruption Budget")
			}
			if disruptionAllowed(latest.Status.Conditions) {
				logging.Debugf(ctx, "Pod Disruption Budget %s/%s permits disruption (disruptionsAllowed=%d currentHealthy=%d desiredHealthy=%d observedGeneration=%d generation=%d)",
					latest.Namespace,
					latest.Name,
					latest.Status.DisruptionsAllowed,
					latest.Status.CurrentHealthy,
					latest.Status.DesiredHealthy,
					latest.Status.ObservedGeneration,
					latest.Generation)
				return nil
			}
			return errors.Newf("Pod Disruption Budget %s/%s does not allow disruption (disruptionsAllowed=%d currentHealthy=%d desiredHealthy=%d observedGeneration=%d generation=%d)",
				latest.Namespace,
				latest.Name,
				latest.Status.DisruptionsAllowed,
				latest.Status.CurrentHealthy,
				latest.Status.DesiredHealthy,
				latest.Status.ObservedGeneration,
				latest.Generation)
		}, &backoff.Opts{
			// TODO(benbardin): Reevaluate polling period and wait timeout.
			Timeout:     DisruptionAllowedWaitTime,
			MaxInterval: DisruptionAllowedPollTime,
		}); err != nil {
			return errors.Wrapf(err, "waiting for DisruptionAllowed on PDB %s/%s", pdb.Namespace, pdb.Name)
		}

		logging.Debugf(ctx, "Pod Disruption Budget %s/%s now allows disruption", pdb.Namespace, pdb.Name)
	}
	return nil
}

func (c *Ctl) affectedPDBsForPod(
	pdbs []policyv1.PodDisruptionBudget, pod *corev1.Pod,
) ([]policyv1.PodDisruptionBudget, error) {
	affectedPDBs := make([]policyv1.PodDisruptionBudget, 0)
	podLabels := labels.Set(pod.Labels)
	for _, pdb := range pdbs {
		labelSelector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			return nil, errors.Wrap(err, "converting labelSelector to selector")
		}
		if labelSelector.Matches(podLabels) {
			affectedPDBs = append(affectedPDBs, pdb)
		}
	}
	return affectedPDBs, nil
}

func (c *Ctl) evictPodsFromNode(ctx context.Context, node *corev1.Node) error {
	logging.Debugf(ctx, "evicting pods from node %s", node.Name)
	namespaces := corev1.NamespaceList{}
	if err := c.List(ctx, &ListOptions{}, &namespaces); err != nil {
		return err
	}
	for _, ns := range namespaces.Items {
		ctl := c.WithNamespace(ns.Name)
		podsToEvict, err := ctl.getPodsToEvict(ctx, node)
		if err != nil {
			return errors.Wrap(err, "getting pods to evict")
		}

		if err := ctl.evict(ctx, podsToEvict...); err != nil {
			return errors.Wrap(err, "evicting pods")
		}
	}
	return nil
}

func (c *Ctl) getPodDisruptionBudgets(ctx context.Context) ([]policyv1.PodDisruptionBudget, error) {
	pdbList := &policyv1.PodDisruptionBudgetList{}
	err := c.List(ctx, &ListOptions{}, pdbList)
	if err != nil {
		return nil, errors.Wrap(err, "listing pdbs")
	}
	return pdbList.Items, nil
}

func (c *Ctl) getPodsToEvict(ctx context.Context, node *corev1.Node) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	err := c.List(ctx, &ListOptions{
		FieldSelector: PodsOnNodeSelector(node),
	}, podList)
	if err != nil {
		return nil, errors.Wrap(err, "listing pods")
	}
	// Daemon set pods will be immediately recreated after being deleted, even
	// if the K8s node is cordoned. The DaemonSet pods will be stopped when the
	// VM is killed.
	pods := filterOutDaemonSetPods(podList.Items)
	return pods, nil
}

// DrainWaitDuration returns how long an operation should wait for the pods to
// terminate before aborting with a timeout.
func DrainWaitDuration(pods []corev1.Pod) time.Duration {
	baseWait := time.Minute * 5
	var maxPodGracePeriod int64
	for i := range pods {
		podGracePeriod := pods[i].Spec.TerminationGracePeriodSeconds
		if podGracePeriod != nil && maxPodGracePeriod < *podGracePeriod {
			maxPodGracePeriod = *podGracePeriod
		}
	}
	return baseWait + time.Second*time.Duration(maxPodGracePeriod)
}

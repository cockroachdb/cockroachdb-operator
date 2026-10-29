package migrationctrl

import (
	"context"
	"fmt"
	"strings"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// checkNodeHealth verifies cluster health after node migration. It weighs two
// signals, under-replicated ranges for data health and live node count for
// cluster membership, and both have to pass before migration moves on to the
// next pod. When the cluster isn't healthy yet, reason says which signal held
// it back so callers don't have to guess.
//
// The counts come from ClusterHealth, which reads `cockroach node status`
// against the system tenant rather than querying crdb_internal directly, so the
// check keeps working on virtualized clusters. The live node count is the
// is_live column from node status, which leaves out nodes that are down or
// decommissioning, so a missing node drags the count down and holds migration
// back until it rejoins.
func (r *Reconciler) checkNodeHealth(
	ctx context.Context, ctl *kube.Ctl, node *crdbv1beta1.CrdbNode, expectedLiveNodes int32,
) (healthy bool, reason string, err error) {
	underReplicated, liveNodes, err := r.NodeExecutor(node, ctl).ClusterHealth(ctx)
	if err != nil {
		return false, "", errors.Wrap(err, "cluster health check failed")
	}

	logging.Infof(ctx, "Cluster health check: under-replicated ranges=%d, live nodes=%d (expected %d)",
		underReplicated, liveNodes, expectedLiveNodes)

	if liveNodes < expectedLiveNodes {
		return false, fmt.Sprintf("%d live nodes, expected %d", liveNodes, expectedLiveNodes), nil
	}
	if underReplicated > 0 {
		return false, fmt.Sprintf("%d under-replicated ranges", underReplicated), nil
	}
	return true, "", nil
}

// checkHeadlessServiceRegistration verifies the pod is registered in DNS.
func checkHeadlessServiceRegistration(
	ctx context.Context, ctl *kube.Ctl, podName, svcName string,
) bool {
	// Query headless service endpoints
	endpoints := &corev1.Endpoints{}
	if err := ctl.Get(ctx, svcName, endpoints); err != nil {
		if !apierrors.IsNotFound(err) {
			logging.Warnf(ctx, "Failed to get endpoints for service %s: %v", svcName, err)
		}
		return false
	}

	for _, subset := range endpoints.Subsets {
		for _, addr := range subset.Addresses {
			if addr.TargetRef != nil && addr.TargetRef.Name == podName {
				logging.Infof(ctx, "Pod %s is registered in headless service %s", podName, svcName)
				return true
			}
		}
	}

	logging.Warnf(ctx, "Pod %s not found in headless service %s endpoints", podName, svcName)
	return false
}

// checkPodConditions verifies pod readiness conditions.
func checkPodConditions(ctx context.Context, ctl *kube.Ctl, podName string) (bool, error) {
	pod := &corev1.Pod{}
	if err := ctl.Get(ctx, podName, pod); err != nil {
		return false, errors.Wrap(err, "failed to get pod")
	}

	// Check pod phase
	if pod.Status.Phase != corev1.PodRunning {
		return false, errors.Errorf("pod not in running state: %s", pod.Status.Phase)
	}

	// Check pod ready condition
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			if cond.Status == corev1.ConditionTrue {
				return true, nil
			}
			return false, errors.Errorf("pod not ready: %s %s", cond.Reason, cond.Message)
		}
	}

	return false, errors.New("pod ready condition not found")
}

// checkCrdbNodeConditions verifies CrdbNode readiness conditions.
func checkCrdbNodeConditions(ctx context.Context, ctl *kube.Ctl, nodeName string) (bool, error) {
	node := &crdbv1beta1.CrdbNode{}
	if err := ctl.Get(ctx, nodeName, node); err != nil {
		return false, errors.Wrap(err, "failed to get CrdbNode")
	}

	// Check node conditions
	for _, cond := range node.Status.Conditions {
		if cond.Type == crdbv1beta1.PodReady {
			if cond.Status == metav1.ConditionTrue {
				return true, nil
			}
			return false, errors.Errorf("node not ready, status: %v", cond.Status)
		}
	}

	return false, errors.New("node ready condition not found")
}

// performComprehensiveHealthCheck performs all health checks for a migrated node.
func (r *Reconciler) performComprehensiveHealthCheck(
	ctx context.Context,
	ctl *kube.Ctl,
	node *crdbv1beta1.CrdbNode,
	svcName string,
	expectedLiveNodes int32,
) error {
	// 1. Check CrdbNode conditions
	ready, err := checkCrdbNodeConditions(ctx, ctl, node.Name)
	if err != nil {
		return errors.Wrap(err, "CrdbNode condition check failed")
	}
	if !ready {
		return errors.New("CrdbNode not ready")
	}

	// 2. Check Pod conditions
	ready, err = checkPodConditions(ctx, ctl, node.Name)
	if err != nil {
		return errors.Wrap(err, "pod condition check failed")
	}
	if !ready {
		return errors.New("pod not ready")
	}

	// 3. Check cluster health: under-replicated ranges and live node count.
	// expectedLiveNodes is migration.TotalPods. All nodes should be live at this point
	// since the migrated node has joined and the remaining STS pods are still running.
	healthy, reason, err := r.checkNodeHealth(ctx, ctl, node, expectedLiveNodes)
	if err != nil {
		return err
	}
	if !healthy {
		return errors.Errorf("node unhealthy: %s", reason)
	}

	// 4. Check headless service registration
	if svcName != "" {
		registered := checkHeadlessServiceRegistration(ctx, ctl, node.Name, svcName)
		if !registered {
			return errors.New("pod not registered in headless service")
		}
	}

	logging.Infof(ctx, "Comprehensive health check passed for node %s", node.Name)
	return nil
}

// drainLocalNode drains the CockroachDB node running in the given pod. The drain
// targets the local node (no node ID needed) because we're exec'ing into the pod
// that hosts the node we want to drain.
func drainLocalNode(
	ctx context.Context,
	ctl *kube.Ctl,
	podName, containerName string,
	grpcPort int32,
	tlsEnabled bool,
) error {
	cmd := []string{
		"/cockroach/cockroach",
		"node",
		"drain",
		"--self",
		"--port",
		fmt.Sprintf("%d", grpcPort),
	}
	if tlsEnabled {
		cmd = append(cmd, fmt.Sprintf("--certs-dir=%s", manifests.CockroachCertsPath))
	} else {
		cmd = append(cmd, "--insecure")
	}
	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	if err := ctl.Exec(ctx, podName, containerName, kube.ExecOptions{
		Command: cmd,
		Stdout:  stdout,
		Stderr:  stderr,
	}); err != nil {
		return errors.Wrapf(err, "stderr: %s", stderr.String())
	}
	if s := strings.TrimSpace(stdout.String()); s != "" {
		logging.Infof(ctx, "drain output for %s: %s", podName, s)
	}
	return nil
}

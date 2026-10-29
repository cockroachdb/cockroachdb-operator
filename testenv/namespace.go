package testenv

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cenkalti/backoff"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kuberecord"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EnsureNamespace creates a K8s namespace if it does not yet exist that is
// based on the name of the current test. The namespace will be destroyed at the
// end of the test unless the debug-env flag is true. EnsureNamespace returns
// the namespace it creates.
func EnsureNamespace(t *testing.T, ctl *kube.Ctl) string {
	// Make deterministic namespace name that is based on test name, so that
	// re-running the test with debug-env will reuse the same namespace.
	return EnsureCustomNamespace(t, ctl, generateName(t.Name()))
}

// EnsureCustomNamespace creates a K8s namespace of the given name if it does
// not yet exist. The namespace will be destroyed at the end of the test unless
// the debug-env flag is true. EnsureCustomNamespace returns the namespace it
// creates.
func EnsureCustomNamespace(t *testing.T, ctl *kube.Ctl, namespace string) string {
	ctx := context.Background()

	namespaceObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	ctlNs := ctl.WithNamespace(namespace)

	if !*debugEnv {
		// Ensure that any existing namespace left over from a previous run of
		// the current test has been deleted.
		deleteNamespace(ctx, t, ctl, namespace, false /* nowait */)
	}

	// Create the namespace.
	require.NoError(t, ctl.Apply(ctx, namespaceObj))

	// Create default service account in the new namespace. This is created
	// automatically by K8s, but it can take many seconds to do so, especially
	// if the K8s cluster is just starting up from being stopped. Some tests
	// will flake if it is not present.
	require.NoError(t, ctlNs.Apply(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}))

	// Delete the namespace when the test completes, unless the debug-env flag
	// is set.
	// TODO(andyk): Should delete all other namespaces but this one, to keep the
	// shared environments tidy. This would prevent K8s tests from running in
	// parallel, but that's probably not realistic anyway.
	if !*debugEnv {
		t.Cleanup(func() {
			// Let namespace deletion run in the background, so that we can
			// immediately begin executing the next test without waiting.
			deleteNamespace(ctx, t, ctl, namespace, true /* nowait */)
		})
	}

	return namespace
}

// deleteNamespace deletes the given namespace. If nowait=false, it will block
// until the namespace resource is gone. Deleting the namespace is complicated
// by the fact that Delete returns errors if the namespace is in the Terminating
// state. Don't call Delete in that case. Also, do not run finalizers for
// resources in the namespace - these will slow down namespace deletion in the
// best case and block it indefinitely in the worst case (e.g. when a CRDB pod
// or tenant will not terminate).
func deleteNamespace(
	ctx context.Context, t *testing.T, ctl *kube.Ctl, namespace string, nowait bool,
) {
	// The cockroachdb pods have a termination grace period of 5 minutes
	// (see manifests.CRDBTerminationGracePeriodSeconds); in other words, the
	// pods may be held in the Terminating state up to 5 minutes before getting
	// a SIGKILL. Set MaxElapsedTime to 6 minutes to ensure that we wait until
	// the pod fully quiesces, or is forcibly killed by Kubernetes.
	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = 360 * time.Second
	b.MaxInterval = kuberecord.MaxBackOffInterval(3 * time.Second)

	finalizers := false
	logged := false
	err := backoff.Retry(func() error {
		// Get the existing namespace object.
		var namespaceObj corev1.Namespace
		if err := ctl.Get(ctx, namespace, &namespaceObj); kubeerrors.IsNotFound(err) {
			return nil
		} else if err != nil {
			return backoff.Permanent(err)
		}

		// Delete any objects with finalizers that might stop termination. This
		// is not needed in integration tests where the operator is running and
		// can remove the finalizers when resources are GC'd by K8s.
		if !testutils.IntegrationTestsEnabled && !finalizers {
			deleteFinalizedObjects(ctx, t, ctl.WithNamespace(namespace))
			finalizers = true
		}

		// Namespace deletion will sometimes hang on containers with long grace
		// periods (CRDB and SQLProxy). To speed up deletion, loop over all
		// Pods within the namespace and forcefully delete
		// them.
		var pods corev1.PodList
		if err := ctl.WithNamespace(namespace).List(ctx, &kube.ListOptions{}, &pods); err != nil {
			return err
		}

		zero := int64(0)
		for _, pod := range pods.Items {
			_ = ctl.DeleteWithOptions(ctx, kube.DeleteOptions{GracePeriodSeconds: &zero}, &pod)
		}

		// If the namespace is already terminating, then wait for termination to
		// complete.
		if namespaceObj.Status.Phase == corev1.NamespaceTerminating {
			if nowait {
				// Actually, no need to wait.
				return nil
			}

			err := fmt.Errorf("waiting for namespace '%s' to terminate", namespace)
			if !logged {
				logging.Info(ctx, err.Error())
				logged = true
			}
			return err
		}

		// Trigger deletion of the namespace.
		return ctl.DeleteWithOptions(ctx, kube.DeleteOptions{NoWait: nowait}, &namespaceObj)
	}, b)
	require.NoError(t, err)
}

// deleteFinalizedObjects runs through all objects in the namespace that have
// finalizers that might block deletion of the namespace.
func deleteFinalizedObjects(ctx context.Context, t *testing.T, ctl *kube.Ctl) {
	var tenants crdbv1beta1.CrdbTenantList
	require.NoError(t, ctl.List(ctx, &kube.ListOptions{}, &tenants))
	for i := range tenants.Items {
		require.NoError(t, ctl.Delete(ctx, &tenants.Items[i]))
	}
}

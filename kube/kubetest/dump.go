package kubetest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// DumpAll lists all objects in the K8s store having the given list types and
// then converts them to JSON bytes. Example usage:
//
//	bytes := DumpAll(t, ctl, &corev1.PodList{}, &appsv1.DeploymentList{})
func DumpAll(t *testing.T, ctl *kube.Ctl, listTypes ...kube.ObjectList) []byte {
	var allItems []any

	// Iterate over every list type.
	for _, typ := range listTypes {
		// List all K8s items of the type specified in the given "list" object.
		ctx := context.Background()
		require.NoError(t, ctl.List(ctx, &kube.ListOptions{}, typ))

		// Convert to an unstructured type for further processing.
		out, err := runtime.DefaultUnstructuredConverter.ToUnstructured(typ)
		require.NoError(t, err)

		// Skip empty list.
		if v, ok := out["items"]; ok && v == nil {
			continue
		}

		// Extract the individual items.
		items, ok, err := unstructured.NestedSlice(out, "items")
		require.NoError(t, err)
		require.True(t, ok)

		// Strip out fields that make dumps hard to diff.
		for i := range items {
			resource := unstructured.Unstructured{Object: items[i].(map[string]any)}
			stripNonDeterministicFields(resource)
		}

		// Normalize the ordering.
		sort.Slice(items, func(i, j int) bool {
			return fmt.Sprintf("%v", items[i]) < fmt.Sprintf("%v", items[j])
		})

		allItems = append(allItems, items...)
	}

	// Convert to nicely-formatted JSON.
	bytes, err := json.MarshalIndent(allItems, "", "\t")
	require.NoError(t, err)
	return bytes
}

// stripNonDeterministicFields replaces all nondeterministic fields in a given
// Kubernetes resource with <redacted> such that they can be compared across
// clusters and test runs.
// For example, Secrets will have all data keys replaced with <redacted> as the
// values generally change across runs.
// stripNonDeterministicFields performs no error checking and should only be
// used in tests.
func stripNonDeterministicFields(resource unstructured.Unstructured) {
	unstructured.RemoveNestedField(resource.Object, "metadata", "generation")
	unstructured.RemoveNestedField(resource.Object, "metadata", "resourceVersion")
	unstructured.RemoveNestedField(resource.Object, "metadata", "annotations", "endpoints.kubernetes.io/last-change-trigger-time")
	unstructured.RemoveNestedField(resource.Object, "metadata", "annotations", "kubernetes.io/service-account.uid")
	unstructured.RemoveNestedField(resource.Object, "metadata", "annotations", "objectset.rio.cattle.io/applied")
	unstructured.RemoveNestedField(resource.Object, "metadata", "annotations", "volume.kubernetes.io/selected-node")
	unstructured.RemoveNestedField(resource.Object, "metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration")
	unstructured.RemoveNestedField(resource.Object, "metadata", "labels", "controller-revision-hash")
	unstructured.RemoveNestedField(resource.Object, "metadata", "managedFields")

	// Redact uid's of ownerRefs, if they exist. uids are uuids that change
	// with every resource creation. ownerRefs still contain the name of the
	// owner so they are still valuable to record.
	ownerRefs, ok, _ := unstructured.NestedSlice(resource.Object, "metadata", "ownerReferences")
	if ok {
		for i := range ownerRefs {
			ownerRefs[i].(map[string]any)["uid"] = "<redacted>"
		}
		_ = unstructured.SetNestedSlice(resource.Object, ownerRefs, "metadata", "ownerReferences")
	}

	switch resource.GetKind() {
	case "PersistentVolumeClaim":
		// Redact volumeName as it's a new UUID every time
		_ = unstructured.SetNestedField(resource.Object, "<redacted>", "spec", "volumeName")

	case "Secret":
		// Redact all data from secrets but keep the keys for proof of existence.
		data, _, _ := unstructured.NestedStringMap(resource.Object, "data")
		for key := range data {
			_ = unstructured.SetNestedField(resource.Object, "<redacted>", "data", key)
		}

	case "ServiceAccount":
		// Redact all generate secrets from service accounts as their names change across deployments.
		secrets, _, _ := unstructured.NestedSlice(resource.Object, "secrets")
		for i := range secrets {
			secrets[i].(map[string]any)["name"] = "<redacted>"
		}
		_ = unstructured.SetNestedSlice(resource.Object, secrets, "secrets")

	case "Service":
		// Filter out any ephermal port numbers or IPs that have been set by the cluster.
		_ = unstructured.SetNestedField(resource.Object, "<redacted>", "spec", "clusterIP")
		_ = unstructured.SetNestedField(resource.Object, "<redacted>", "spec", "healthCheckNodePort")
		ports, _, _ := unstructured.NestedSlice(resource.Object, "spec", "ports")
		for i := range ports {
			ports[i].(map[string]any)["nodePort"] = "<redacted>"
		}
		_ = unstructured.SetNestedSlice(resource.Object, ports, "spec", "ports")
	}
}

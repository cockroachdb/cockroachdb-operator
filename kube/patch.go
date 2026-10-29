package kube

import (
	"bytes"
	"encoding/json"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/jsonmergepatch"
	"k8s.io/apimachinery/pkg/util/mergepatch"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/kubernetes/scheme"
)

var (
	emptyPatch = []byte("{}")
	// patchSpecialCases is a map of Kubernetes type to a list of fields that
	// should be filtered out of patches. See its usage for a more complete
	// explanation.
	patchSpecialCases = map[reflect.Type][][]string{
		reflect.TypeFor[*corev1.Service](): {
			// clusterIP is an optionally server set value that is immutable
			// after creation.
			{"spec", "clusterIP"},
			// healthCheckNodePort is an optionally server set value that is
			// immutable after creation when externalTrafficPolicy=Local.
			{"spec", "healthCheckNodePort"},
		},
		reflect.TypeFor[*corev1.PersistentVolumeClaim](): {
			// volumeName is automatically set by the K8s api after a PVC is
			// bound. It is immutable.
			{"spec", "volumeName"},
		},
	}
)

// GeneratePatch builds a JSON patch between two kubernetes objects that is
// suitable for use in PATCH calls. The type of patch that should be used in the
// PATCH call is returned along with the actual patch bytes. If overwrite is
// false, GeneratePatch will return errors if there is a merge conflict between
// current, modified, and the last applied annotation on the current object.
func GeneratePatch(
	current, modified Object, overwrite bool,
) (patchType types.PatchType, patchBytes []byte, err error) {
	currentPatch, err := marshalObject(current)
	if err != nil {
		return patchType, nil, err
	}

	modifiedPatch, err := marshalObject(modified)
	if err != nil {
		return patchType, nil, err
	}

	originalPatch, err := lastApplied(current)
	if err != nil {
		return patchType, nil, err
	}
	if originalPatch == nil {
		// Current patch has no last-applied annotation, so just assume there
		// was no change between original and current in order to avoid overly
		// conservative conflict reporting.
		originalPatch = currentPatch
	} else {
		// The last-applied annotation does not contain its own last-applied
		// annotation in order to avoid recursion. Add it back now so that the
		// 3-way diff is comparing apples to apples.
		originalPatch, err = setLastAppliedInPatch(originalPatch)
		if err != nil {
			return patchType, nil, err
		}
	}

	lookupPatchMeta, err := strategicpatch.NewPatchMetaFromStruct(current)
	if err != nil {
		return patchType, nil, err
	}

	// Check whether the object is a structured system-installed type.
	// Use scheme.Scheme to check for known system-installed types.
	_, isUnstructured := modified.(runtime.Unstructured)
	_, _, err = scheme.Scheme.ObjectKinds(modified)
	isStructuredSystemType := !isUnstructured && err == nil

	// Start by trying to generate a 3-way StrategicMergePatch, so that it
	// detects any conflicts the same way, regardless of whether a MergePatch is
	// later used to actually do the patching.
	// Based off of https://github.com/kubernetes/kubernetes/blob/a26e5881d8aa38aaa0a56c78050fd4ea14611c9f/staging/src/k8s.io/kubectl/pkg/cmd/apply/patcher.go#L74
	mergePatch, err := strategicpatch.CreateThreeWayMergePatch(
		originalPatch, modifiedPatch, currentPatch, lookupPatchMeta, overwrite)

	// If this ended in a conflict, nothing more to do. Only return other errors
	// if this is a structured system type, since other types will retry below.
	if mergepatch.IsConflict(err) {
		return patchType, nil, err
	} else if err != nil && isStructuredSystemType {
		return patchType, nil, err
	}

	// StrategicMergePatch is only supported for structured system-installed
	// types, not for unstructured or custom types. In that case, fall back to
	// generic JSON merge patch.
	if isStructuredSystemType {
		patchType = types.StrategicMergePatchType
	} else {
		patchType = types.MergePatchType
		mergePatch, err = jsonmergepatch.CreateThreeWayJSONMergePatch(
			originalPatch, modifiedPatch, currentPatch)
		if err != nil {
			return patchType, nil, err
		}
	}

	// Short-circuit if there are no diffs.
	if bytes.Equal(mergePatch, emptyPatch) {
		return patchType, mergePatch, err
	}

	// Set ResourceVersion so that any changes that have occurred since the
	// server object was fetched will be detected.
	var patchMap map[string]any
	if err := json.Unmarshal(mergePatch, &patchMap); err != nil {
		return patchType, nil, err
	}
	version, err := metadataAccessor.ResourceVersion(current)
	if err != nil {
		return patchType, nil, err
	}
	if version != "" {
		err = unstructured.SetNestedField(patchMap, version, "metadata", "resourceVersion")
		if err != nil {
			return patchType, nil, err
		}
	}

	// Some K8s types have server set / read only fields that can cause issues
	// when generating patches. The most notable case is corev1.Service's
	// .Spec.ClusterIP. ClusterIP is set by the server if empty but it
	// immutable after creation. The current diffing algorithm being used
	// results in the ClusterIP being set to null (Not an empty string) when
	// generating a patch without the lastApplied annotation. Attempts to
	// change or reconfigure the diff algorithm have resulted in subtle bugs
	// popping up elsewhere. Notably, modifying labels often results in
	// incorrect diffs being applied. Instead, the most correct solution
	// appears to be a list of specialCases that are handled on a type by type
	// basis.
	if specialCases, ok := patchSpecialCases[reflect.TypeOf(modified)]; ok {
		for _, specialCase := range specialCases {
			val, _, _ := unstructured.NestedFieldCopy(patchMap, specialCase...)
			// Only clear specialCases if they're either nil or a zero value.
			if val == nil || reflect.ValueOf(val).IsZero() {
				unstructured.RemoveNestedField(patchMap, specialCase...)
			}
		}
	}

	patchBytes, err = json.Marshal(patchMap)
	return patchType, patchBytes, err
}

// setLastAppliedInPatch unmarshals the given patch and then sets the patch as
// its own last-applied annotation.
func setLastAppliedInPatch(patch []byte) ([]byte, error) {
	var originalPatchMap map[string]any
	if err := json.Unmarshal(patch, &originalPatchMap); err != nil {
		return nil, err
	}
	err := unstructured.SetNestedField(
		originalPatchMap, string(patch),
		"metadata", "annotations", corev1.LastAppliedConfigAnnotation)
	if err != nil {
		return nil, err
	}
	return json.Marshal(originalPatchMap)
}

// convertToUnstructured converts a Object into an instance of
// *unstructured.Unstructured.
func convertToUnstructured(typed Object) (*unstructured.Unstructured, error) {
	out, err := runtime.DefaultUnstructuredConverter.ToUnstructured(typed)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{
		Object: out,
	}, nil
}

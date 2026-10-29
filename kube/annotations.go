package kube

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
)

// ForceApplyOverwriteAnnotation is the annotation used to indicate the ctl's
// behavior when invoking Apply on the current resource. This is useful when a
// manual change has introduced the possibility of conflicts during development
// or emergency patching, and we could force an overwrite through annotations.
// Another use case would be to prevent the operator from overriding the
// resource when ctl.Apply was invoked with Overwrite=true in the code.
const ForceApplyOverwriteAnnotation = "force-apply-overwrite"

// This file based off of
// https://github.com/kubernetes/kubernetes/blob/04ecdb9eb60588539c571f9ea343aa7049f29ff2/staging/src/k8s.io/kubectl/pkg/util/apply.go#L29
var metadataAccessor = meta.NewAccessor()

// getAnnotation returns the value of the given annotation in bytes, or nil
// if it doesn't exist.
func getAnnotation(obj Object, annotation string) ([]byte, error) {
	annots, err := metadataAccessor.Annotations(obj)
	if err != nil {
		return nil, err
	}

	if annots == nil {
		return nil, nil
	}

	original, ok := annots[annotation]
	if !ok {
		return nil, nil
	}

	return []byte(original), nil
}

// lastApplied returns the value of the kubectl.kubernetes.io/last-applied-configuration
// annotation as bytes or nil if it doesn't exist.
func lastApplied(obj Object) ([]byte, error) {
	return getAnnotation(obj, corev1.LastAppliedConfigAnnotation)
}

// setLastApplied serializes the given runtime object and sets that serialized
// value as the kubectl.kubernetes.io/last-applied-configuration annotation.
func setLastApplied(obj Object) error {
	// First serialize the object without the annotation to prevent recursion,
	// then add that serialization to it as the annotation and serialize it
	// again.
	var modified []byte

	// Otherwise, use the server side version of the object.
	// Get the current annotations from the object.
	annots, err := metadataAccessor.Annotations(obj)
	if err != nil {
		return err
	}

	if annots == nil {
		annots = map[string]string{}
	}

	delete(annots, corev1.LastAppliedConfigAnnotation)
	if err := metadataAccessor.SetAnnotations(obj, annots); err != nil {
		return err
	}

	// marshalObject is used here to only include fields that should be part of
	// the last-applied annotation.
	modified, err = marshalObject(obj)
	if err != nil {
		return err
	}

	annots[corev1.LastAppliedConfigAnnotation] = string(modified)
	if err := metadataAccessor.SetAnnotations(obj, annots); err != nil {
		return err
	}

	return nil
}

package kube

import (
	"github.com/cockroachdb/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Object represents a single kubernetes object.
//
// Object was introduced to ease a migration to controller-runtime v0.7.
// controller-runtime introduces a client.Object interface that is expected
// instead of runtime.Object.
type Object interface {
	runtime.Object
	metav1.Object
}

// ObjectList represents a list of Objects.
//
// ObjectList was introduced to ease a migration to controller-runtime v0.7.
// controller-runtime introduces a client.ObjectList interface that is expected
// instead of runtime.Object.
type ObjectList interface {
	runtime.Object
	metav1.ListInterface
}

// ToRuntimeSlice converts a slice of objects to runtime.Objects.
func ToRuntimeSlice(object []Object) []runtime.Object {
	result := make([]runtime.Object, len(object))
	for i, o := range object {
		result[i] = o
	}
	return result
}

// FromRuntimeSlice converts a slice of runtime.Objects into a slice of Objects.
// Conversion may fail if one of the input objects is a list of objects.
func FromRuntimeSlice(object []runtime.Object) ([]Object, error) {
	result := make([]Object, len(object))
	var err error
	for i, o := range object {
		result[i], err = FromRuntime(o)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

// FromRuntime converts a runtime.Object into an Object. This may fail if the
// input object is an ObjectList.
func FromRuntime(object runtime.Object) (Object, error) {
	result, ok := object.(Object)
	if ok {
		return result, nil
	}
	return nil, errors.AssertionFailedf("expected a kube.Object and found: %+v", object)
}

// DeepCopy returns a deep copy of the object.
func DeepCopy(object Object) Object {
	// Note: it is technically possible for this to panic, but it would require
	// DeepCopyObject to return an object with a different type, which indicates
	// a bug in the DeepCopyObject implementation.
	return object.DeepCopyObject().(Object)
}

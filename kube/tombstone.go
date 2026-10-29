package kube

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Tombstone marks one of more [Object]s for deletion when passed to Ctl.Apply.
// It may be used to declaratively ensure that objects are cleaned up when they
// are removed from a manifest.
//
// Tombstone Objects will panic if passed to a method other than Ctl.Apply or
// Ctl.ApplyWithOptions.
//
//	ctl.Apply(ctx, NewConfigMap(), Tombstone(OldConfigMap()))
//
// # Note On Safety
//
// If you ever find yourself writing code that automatically creates tombstones
// for unexpected objects in production, see panel 2 of https://xkcd.com/2677/.
// The benefit of tombstones is they require an explicit decision to delete the
// object. Most code generating tombstones should be temporary. Typically
// tombstones are used in a three step roll out.
//
// - Step 1: something is rolled out that makes an object in production redundant.
// - Step 2: the code is updated to generate tombstones for the unnecessary object.
// - Step 3: after the object is cleaned up in production, the code creating the tombstones is deleted.
func Tombstone(objects ...Object) []Object {
	tombstones := make([]Object, len(objects))
	for i := range objects {
		copy, err := FromRuntime(objects[i].DeepCopyObject())
		if err != nil {
			panic(err)
		}
		tombstones[i] = &tombstone{Object: copy}
	}
	return tombstones
}

// splitTombstones splits a slice of [Object]s into an unwrapped slice
// [Tombstone]ed [Object]s and slice of untouched [Object]s.
func splitTombstones(objs []Object) (tombstones []Object, objects []Object) {
	var x int
	for _, obj := range objs {
		if ts, ok := obj.(*tombstone); ok {
			tombstones = append(tombstones, ts.Object)
			continue
		}
		objs[x] = obj
		x++
	}
	return tombstones, objs[:x]
}

// tombstone is a marker object that indicates that the wrapped [Object] should
// be idempotently deleted when [Ctl.ApplyWithOptions] is invoked.
//
// It is a [Object] in name only. It intentionally returns zero values to all
// methods to dissuade use in methods other than [Ctl.ApplyWithOptions].
type tombstone struct {
	Object Object
}

// DeepCopyObject implements runtime.Object by returning nil.
func (t *tombstone) DeepCopyObject() runtime.Object { return nil }

// GetObjectKind implements runtime.Object by returning nil.
func (t *tombstone) GetObjectKind() schema.ObjectKind { return nil }

func (*tombstone) SetAnnotations(annotations map[string]string)               {}
func (*tombstone) SetClusterName(clusterName string)                          {}
func (*tombstone) SetCreationTimestamp(timestamp metav1.Time)                 {}
func (*tombstone) SetDeletionGracePeriodSeconds(*int64)                       {}
func (*tombstone) SetDeletionTimestamp(timestamp *metav1.Time)                {}
func (*tombstone) SetFinalizers(finalizers []string)                          {}
func (*tombstone) SetGenerateName(name string)                                {}
func (*tombstone) SetGeneration(generation int64)                             {}
func (*tombstone) SetLabels(labels map[string]string)                         {}
func (*tombstone) SetManagedFields(managedFields []metav1.ManagedFieldsEntry) {}
func (*tombstone) SetName(name string)                                        {}
func (*tombstone) SetNamespace(namespace string)                              {}
func (*tombstone) SetOwnerReferences([]metav1.OwnerReference)                 {}
func (*tombstone) SetResourceVersion(version string)                          {}
func (*tombstone) SetSelfLink(selfLink string)                                {}
func (*tombstone) SetZZZ_DeprecatedClusterName(clusterName string)            {}
func (*tombstone) SetUID(uid types.UID)                                       {}
func (*tombstone) GetAnnotations() map[string]string                          { return nil }
func (*tombstone) GetClusterName() string                                     { return "" }
func (*tombstone) GetCreationTimestamp() metav1.Time                          { return metav1.Time{} }
func (*tombstone) GetDeletionGracePeriodSeconds() *int64                      { return nil }
func (*tombstone) GetDeletionTimestamp() *metav1.Time                         { return nil }
func (*tombstone) GetFinalizers() []string                                    { return nil }
func (*tombstone) GetGenerateName() string                                    { return "" }
func (*tombstone) GetGeneration() int64                                       { return -1 }
func (*tombstone) GetLabels() map[string]string                               { return nil }
func (*tombstone) GetManagedFields() []metav1.ManagedFieldsEntry              { return nil }
func (*tombstone) GetName() string                                            { return "" }
func (*tombstone) GetNamespace() string                                       { return "" }
func (*tombstone) GetOwnerReferences() []metav1.OwnerReference                { return nil }
func (*tombstone) GetResourceVersion() string                                 { return "" }
func (*tombstone) GetSelfLink() string                                        { return "" }
func (*tombstone) GetUID() types.UID                                          { return "" }
func (*tombstone) GetZZZ_DeprecatedClusterName() string                       { return "" }

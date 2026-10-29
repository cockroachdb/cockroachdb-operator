package kube

import (
	"github.com/cockroachdb/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"maps"
)

// LabelManagedBy is a common k8s label used to identify the automation managing a resource.
// https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
const LabelManagedBy = "app.kubernetes.io/managed-by"

// ParseLabelOrDie parses the given label selector string, panicking if it is
// invalid. This should only be used when that can never happen.
func ParseLabelOrDie(selector string) labels.Selector {
	result, err := labels.Parse(selector)
	if err != nil {
		panic(err)
	}
	return result
}

// AddLabels takes in a set of Objects & adds labels to each
// one in turn. It modifies the passed in objects in place. The set of labels
// applied to each object are the union of the labels already applied to the
// object and the new passed in ones. If a label to add is already present on an
// object, it is overwritten with the new passed in value.
func AddLabels(labels map[string]string, objects ...Object) error {
	for _, obj := range objects {
		accessor, err := meta.Accessor(obj)
		if err != nil {
			return errors.Wrap(err, "unable to create Accessor")
		}
		l := accessor.GetLabels()
		if l == nil { // nil is returned if no labels set on object
			l = make(map[string]string)
		}
		maps.Copy(l, labels)
		accessor.SetLabels(l)
	}
	return nil
}

package kubetest

import (
	"bytes"
	"encoding/json"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/testing"
)

// mutationToReactor adapts a [kube.Ctl] mutation hook, BeforeMutation and
// AfterMutation, to a [testing.ReactionFunc].
func mutationToReactor(
	hook func(kube.Object) error, tracker testing.ObjectTracker,
) testing.ReactionFunc {
	return func(action testing.Action) (handled bool, ret runtime.Object, err error) {
		var obj kube.Object
		switch action := action.(type) {
		case testing.GetActionImpl:
			trackerObj, err := tracker.Get(
				action.GetResource(), action.Namespace, action.GetName())
			if err != nil {
				return false, nil, err
			}
			obj = trackerObj.(kube.Object)
			return false, obj, nil
		case testing.ListActionImpl:
			objs, err := tracker.List(action.GetResource(), action.GetKind(), action.GetNamespace())
			return false, objs, err

		case testing.CreateActionImpl:
			obj = action.Object.(kube.Object)
		case testing.UpdateActionImpl:
			obj = action.Object.(kube.Object)
		case testing.DeleteActionImpl:
			return false, nil, nil // TODO make Delete hooks work
		case testing.PatchActionImpl:
			trackerObj, err := tracker.Get(
				action.GetResource(), action.Namespace, action.GetName())
			if err != nil {
				return false, nil, err
			}
			obj = trackerObj.(kube.Object)
		default:
			// No op on all non-mutating actions.
			return false, nil, nil
		}

		return false, obj, hook(obj)
	}
}

// newServiceAccountReactor returns a [testing.ReactionFunc] that creates
// corresponding [corev1.Secret]s for [corev1.ServiceAccount]s.
func newServiceAccountReactor(f *Fake) testing.ReactionFunc {
	return func(action testing.Action) (handled bool, ret runtime.Object, err error) {
		sa := action.(testing.CreateActionImpl).Object.(*corev1.ServiceAccount)

		secret := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sa.Name + "-service-account",
				Namespace: sa.Namespace,
				Annotations: map[string]string{
					corev1.ServiceAccountNameKey: sa.Name,
				},
			},
			Data: map[string][]byte{
				corev1.ServiceAccountTokenKey: []byte("serviceaccounttoken"),
			},
		}

		_, err = f.Invokes(testing.NewCreateAction(schema.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: "secrets",
		}, sa.Namespace, &secret))
		return false, nil, err
	}
}

// newGenerationReactor return a [testing.ReactionFunc] that increments the
// .Generation of an object in a manner similar to how the Kubernetes API
// functions. See [shouldIncrementGeneration] for details.
func newGenerationReactor(tracker testing.ObjectTracker) testing.ReactionFunc {
	return func(action testing.Action) (handled bool, ret runtime.Object, err error) {
		switch action := action.(type) {
		case testing.CreateActionImpl:
			action.Object.(kube.Object).SetGeneration(1)

		case testing.UpdateActionImpl:
			obj := action.Object.(kube.Object)

			oldObj, err := tracker.Get(action.GetResource(), action.Namespace, obj.GetName())
			if err != nil {
				return false, nil, errors.Wrapf(err, "failed to get old version of: %v", action.Object)
			}

			if shouldIncrementGeneration(obj, oldObj.(kube.Object)) {
				obj.SetGeneration(obj.GetGeneration() + 1)
			}
		}
		return false, nil, nil
	}
}

// shouldIncrementGeneration is a heuristic to determine if an object update
// should cause .Generation to be incremented. If .Spec, .Labels, or
// .Annotations has changed, then .Generation should be incremented. This This
// heuristic is based on observation to the kube API and may be mildly
// incorrect but is sufficient for all our test cases.
func shouldIncrementGeneration(updated, original kube.Object) bool {
	updatedBytes, _ := json.Marshal(updated)
	originalBytes, _ := json.Marshal(original)

	var updatedMap map[string]any
	var originalMap map[string]any

	_ = json.Unmarshal(updatedBytes, &updatedMap)
	_ = json.Unmarshal(originalBytes, &originalMap)

	updatedBody, _ := json.Marshal([]any{updatedMap["annotations"], updatedMap["labels"], updatedMap["spec"]})
	originalBody, _ := json.Marshal([]any{originalMap["annotations"], originalMap["labels"], originalMap["spec"]})

	return !bytes.Equal(updatedBody, originalBody)
}

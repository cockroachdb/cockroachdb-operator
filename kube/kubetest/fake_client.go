// NB: This file was extracted from the controller-runtime package and modified
// to suit the needs of this repository. Rather than using an object tracker
// directly, this fakeClient uses client-go's testing.Fake reactor setup. This
// allows us to inject custom hooks to better emulate Kubernetes.
package kubetest

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors" // nolint:importcheck
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"slices"
)

type versionedTracker struct {
	testing.ObjectTracker
	scheme *runtime.Scheme
}

type fakeClient struct {
	scheme *runtime.Scheme
	fake   *Fake
}

var _ client.Client = &fakeClient{}

const (
	maxNameLength             = 63
	randomLength              = 5
	maxGeneratedNameLength    = maxNameLength - randomLength
	trackerAddResourceVersion = "999"
)

func (t versionedTracker) Add(obj runtime.Object) error {
	var objects []runtime.Object
	if meta.IsListType(obj) {
		var err error
		objects, err = meta.ExtractList(obj)
		if err != nil {
			return err
		}
	} else {
		objects = []runtime.Object{obj}
	}
	for _, obj := range objects {
		accessor, err := meta.Accessor(obj)
		if err != nil {
			return fmt.Errorf("failed to get accessor for object: %w", err)
		}
		if accessor.GetResourceVersion() == "" {
			// We use a "magic" value of 999 here because this field
			// is parsed as uint and and 0 is already used in Update.
			// As we can't go lower, go very high instead so this can
			// be recognized
			accessor.SetResourceVersion(trackerAddResourceVersion)
		}
		if err := t.ObjectTracker.Add(obj); err != nil {
			return err
		}
	}

	return nil
}

func (t versionedTracker) Create(
	gvr schema.GroupVersionResource, obj runtime.Object, ns string,
) error {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return fmt.Errorf("failed to get accessor for object: %v", err)
	}
	if accessor.GetName() == "" {
		return apierrors.NewInvalid(
			obj.GetObjectKind().GroupVersionKind().GroupKind(),
			accessor.GetName(),
			field.ErrorList{field.Required(field.NewPath("metadata.name"), "name is required")})
	}
	if accessor.GetResourceVersion() != "" {
		return apierrors.NewBadRequest("resourceVersion can not be set for Create requests")
	}
	accessor.SetResourceVersion("1")
	if err := t.ObjectTracker.Create(gvr, obj, ns); err != nil {
		accessor.SetResourceVersion("")
		return err
	}
	return nil
}

func (t versionedTracker) Update(
	gvr schema.GroupVersionResource, obj runtime.Object, ns string,
) error {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return fmt.Errorf("failed to get accessor for object: %v", err)
	}

	if accessor.GetName() == "" {
		return apierrors.NewInvalid(
			obj.GetObjectKind().GroupVersionKind().GroupKind(),
			accessor.GetName(),
			field.ErrorList{field.Required(field.NewPath("metadata.name"), "name is required")})
	}

	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk.Empty() {
		gvk, err = apiutil.GVKForObject(obj, t.scheme)
		if err != nil {
			return err
		}
	}

	oldObject, err := t.ObjectTracker.Get(gvr, ns, accessor.GetName())
	if err != nil {
		// If the resource is not found and the resource allows create on update, issue a
		// create instead.
		if apierrors.IsNotFound(err) && allowsCreateOnUpdate(gvk) {
			return t.Create(gvr, obj, ns)
		}
		return err
	}

	oldAccessor, err := meta.Accessor(oldObject)
	if err != nil {
		return err
	}

	// If the new object does not have the resource version set and it allows unconditional update,
	// default it to the resource version of the existing resource
	if accessor.GetResourceVersion() == "" && allowsUnconditionalUpdate(gvk) {
		accessor.SetResourceVersion(oldAccessor.GetResourceVersion())
	}
	if accessor.GetResourceVersion() != oldAccessor.GetResourceVersion() {
		return apierrors.NewConflict(gvr.GroupResource(), accessor.GetName(), errors.New("object was modified"))
	}
	if oldAccessor.GetResourceVersion() == "" {
		oldAccessor.SetResourceVersion("0")
	}
	intResourceVersion, err := strconv.ParseUint(oldAccessor.GetResourceVersion(), 10, 64)
	if err != nil {
		return fmt.Errorf("can not convert resourceVersion %q to int: %v", oldAccessor.GetResourceVersion(), err)
	}
	intResourceVersion++
	accessor.SetResourceVersion(strconv.FormatUint(intResourceVersion, 10))
	return t.ObjectTracker.Update(gvr, obj, ns)
}

func (c *fakeClient) Get(
	ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
) error {
	gvr, err := getGVRFromObject(obj, c.scheme)
	if err != nil {
		return err
	}

	o, err := c.fake.Invokes(testing.NewGetAction(gvr, key.Namespace, key.Name))
	if err != nil {
		return err
	}

	gvk, err := apiutil.GVKForObject(obj, c.scheme)
	if err != nil {
		return err
	}
	ta, err := meta.TypeAccessor(o)
	if err != nil {
		return err
	}
	ta.SetKind(gvk.Kind)
	ta.SetAPIVersion(gvk.GroupVersion().String())

	j, err := json.Marshal(o)
	if err != nil {
		return err
	}
	decoder := scheme.Codecs.UniversalDecoder()
	_, _, err = decoder.Decode(j, nil, obj)
	return err
}

func (c *fakeClient) List(
	ctx context.Context, obj client.ObjectList, opts ...client.ListOption,
) error {
	gvk, err := apiutil.GVKForObject(obj, c.scheme)
	if err != nil {
		return err
	}

	OriginalKind := gvk.Kind

	if !strings.HasSuffix(gvk.Kind, "List") {
		return fmt.Errorf("non-list type %T (kind %q) passed as output", obj, gvk)
	}
	// we need the non-list GVK, so chop off the "List" from the end of the kind
	gvk.Kind = gvk.Kind[:len(gvk.Kind)-4]

	listOpts := client.ListOptions{}
	listOpts.ApplyOptions(opts)

	gvr, _ := meta.UnsafeGuessKindToResource(gvk)

	o, err := c.fake.Invokes(testing.NewListAction(gvr, gvk, listOpts.Namespace, *listOpts.AsListOptions()))
	if err != nil {
		return err
	}

	ta, err := meta.TypeAccessor(o)
	if err != nil {
		return err
	}
	ta.SetKind(OriginalKind)
	ta.SetAPIVersion(gvk.GroupVersion().String())

	j, err := json.Marshal(o)
	if err != nil {
		return err
	}
	decoder := scheme.Codecs.UniversalDecoder()
	_, _, err = decoder.Decode(j, nil, obj)
	if err != nil {
		return err
	}

	if listOpts.LabelSelector != nil {
		objs, err := meta.ExtractList(obj)
		if err != nil {
			return err
		}

		// Extracted from the internal package, objectutil.FilterWithLabels.
		filteredObjs := make([]runtime.Object, 0, len(objs))
		for _, obj := range objs {
			labelSet := labels.Set(obj.(kube.Object).GetLabels())
			if listOpts.LabelSelector != nil && !listOpts.LabelSelector.Matches(labelSet) {
				continue
			}
			filteredObjs = append(filteredObjs, obj.DeepCopyObject())
		}

		err = meta.SetList(obj, filteredObjs)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *fakeClient) Scheme() *runtime.Scheme {
	return c.scheme
}

func (c *fakeClient) RESTMapper() meta.RESTMapper {
	// TODO: Implement a fake RESTMapper.
	return nil
}

func (c *fakeClient) Create(
	ctx context.Context, obj client.Object, opts ...client.CreateOption,
) error {
	createOptions := &client.CreateOptions{}
	createOptions.ApplyOptions(opts)

	if slices.Contains(createOptions.DryRun, metav1.DryRunAll) {
		return nil
	}

	gvr, err := getGVRFromObject(obj, c.scheme)
	if err != nil {
		return err
	}
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return err
	}

	if accessor.GetName() == "" && accessor.GetGenerateName() != "" {
		base := accessor.GetGenerateName()
		if len(base) > maxGeneratedNameLength {
			base = base[:maxGeneratedNameLength]
		}
		accessor.SetName(fmt.Sprintf("%s%s", base, utilrand.String(randomLength)))
	}

	_, err = c.fake.Invokes(testing.NewCreateAction(gvr, accessor.GetNamespace(), obj))
	return err
}

func (c *fakeClient) Delete(
	ctx context.Context, obj client.Object, opts ...client.DeleteOption,
) error {
	gvr, err := getGVRFromObject(obj, c.scheme)
	if err != nil {
		return err
	}

	// TODO: should we support dry run?

	_, err = c.fake.Invokes(testing.NewDeleteAction(gvr, obj.GetNamespace(), obj.GetName()))
	return err
}

func (c *fakeClient) DeleteAllOf(
	ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption,
) error {
	gvk, err := apiutil.GVKForObject(obj, c.scheme)
	if err != nil {
		return err
	}

	dcOptions := client.DeleteAllOfOptions{}
	dcOptions.ApplyOptions(opts)

	gvr, _ := meta.UnsafeGuessKindToResource(gvk)
	o, err := c.fake.Invokes(testing.NewListAction(gvr, gvk, dcOptions.Namespace, nil))
	if err != nil {
		return err
	}

	objs, err := meta.ExtractList(o)
	if err != nil {
		return err
	}

	// Extracted from the internal package, objectutil.FilterWithLabels.
	filteredObjs := make([]runtime.Object, 0, len(objs))
	for _, obj := range objs {
		labelSet := labels.Set(obj.(kube.Object).GetLabels())
		if dcOptions.LabelSelector != nil && !dcOptions.LabelSelector.Matches(labelSet) {
			continue
		}
		filteredObjs = append(filteredObjs, obj.DeepCopyObject())
	}

	for _, o := range filteredObjs {
		accessor, err := meta.Accessor(o)
		if err != nil {
			return err
		}
		if _, err := c.fake.Invokes(testing.NewDeleteAction(gvr, accessor.GetNamespace(), accessor.GetName())); err != nil {
			return err
		}
	}
	return nil
}

func (c *fakeClient) Update(
	ctx context.Context, obj client.Object, opts ...client.UpdateOption,
) error {
	updateOptions := &client.UpdateOptions{}
	updateOptions.ApplyOptions(opts)

	if slices.Contains(updateOptions.DryRun, metav1.DryRunAll) {
		return nil
	}

	gvr, err := getGVRFromObject(obj, c.scheme)
	if err != nil {
		return err
	}

	_, err = c.fake.Invokes(testing.NewUpdateAction(gvr, obj.GetNamespace(), obj))
	return err
}

func (c *fakeClient) Patch(
	ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption,
) error {
	patchOptions := &client.PatchOptions{}
	patchOptions.ApplyOptions(opts)

	if slices.Contains(patchOptions.DryRun, metav1.DryRunAll) {
		return nil
	}

	gvr, err := getGVRFromObject(obj, c.scheme)
	if err != nil {
		return err
	}
	data, err := patch.Data(obj)
	if err != nil {
		return err
	}

	o, err := c.fake.Invokes(testing.NewPatchAction(gvr, obj.GetNamespace(), obj.GetName(), patch.Type(), data))
	if err != nil {
		return errors.WithStack(err)
	}

	gvk, err := apiutil.GVKForObject(obj, c.scheme)
	if err != nil {
		return err
	}
	ta, err := meta.TypeAccessor(o)
	if err != nil {
		return err
	}
	ta.SetKind(gvk.Kind)
	ta.SetAPIVersion(gvk.GroupVersion().String())

	j, err := json.Marshal(o)
	if err != nil {
		return err
	}
	decoder := scheme.Codecs.UniversalDecoder()
	_, _, err = decoder.Decode(j, nil, obj)
	return err
}

func (c *fakeClient) SubResource(subResource string) client.SubResourceClient {
	return &fakeSubResourceClient{client: c}
}

func (c *fakeClient) Status() client.StatusWriter {
	return c.SubResource("status")
}

func getGVRFromObject(
	obj runtime.Object, scheme *runtime.Scheme,
) (schema.GroupVersionResource, error) {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		return schema.GroupVersionResource{}, err
	}
	gvr, _ := meta.UnsafeGuessKindToResource(gvk)
	return gvr, nil
}

type fakeStatusWriter struct {
	client *fakeClient
}

func (sw *fakeStatusWriter) Update(
	ctx context.Context, obj client.Object, opts ...client.UpdateOption,
) error {
	// TODO(droot): This results in full update of the obj (spec + status). Need
	// a way to update status field only.
	return sw.client.Update(ctx, obj, opts...)
}

func (sw *fakeStatusWriter) Patch(
	ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption,
) error {
	// TODO(droot): This results in full update of the obj (spec + status). Need
	// a way to update status field only.
	return sw.client.Patch(ctx, obj, patch, opts...)
}

type fakeSubResourceClient struct {
	client      *fakeClient
	subResource string
}

func (c *fakeSubResourceClient) Get(
	ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceGetOption,
) error {
	return c.client.Get(ctx, client.ObjectKeyFromObject(obj), subResource)
}

func (c *fakeSubResourceClient) Create(
	ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceCreateOption,
) error {
	return c.client.Create(ctx, subResource)
}

func (c *fakeSubResourceClient) Update(
	ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption,
) error {
	return c.client.Update(ctx, obj)
}

func (c *fakeSubResourceClient) Patch(
	ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption,
) error {
	return c.client.Patch(ctx, obj, patch)
}

func allowsUnconditionalUpdate(gvk schema.GroupVersionKind) bool {
	switch gvk.Group {
	case "apps":
		switch gvk.Kind {
		case "ControllerRevision", "DaemonSet", "Deployment", "ReplicaSet", "StatefulSet":
			return true
		}
	case "autoscaling":
		switch gvk.Kind {
		case "HorizontalPodAutoscaler":
			return true
		}
	case "batch":
		switch gvk.Kind {
		case "CronJob", "Job":
			return true
		}
	case "certificates":
		switch gvk.Kind {
		case "Certificates":
			return true
		}
	case "flowcontrol":
		switch gvk.Kind {
		case "FlowSchema", "PriorityLevelConfiguration":
			return true
		}
	case "networking":
		switch gvk.Kind {
		case "Ingress", "IngressClass", "NetworkPolicy":
			return true
		}
	case "policy":
		switch gvk.Kind {
		case "PodSecurityPolicy":
			return true
		}
	case "rbac":
		switch gvk.Kind {
		case "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding":
			return true
		}
	case "scheduling":
		switch gvk.Kind {
		case "PriorityClass":
			return true
		}
	case "settings":
		switch gvk.Kind {
		case "PodPreset":
			return true
		}
	case "storage":
		switch gvk.Kind {
		case "StorageClass":
			return true
		}
	case "":
		switch gvk.Kind {
		case "ConfigMap", "Endpoint", "Event", "LimitRange", "Namespace", "Node",
			"PersistentVolume", "PersistentVolumeClaim", "Pod", "PodTemplate",
			"ReplicationController", "ResourceQuota", "Secret", "Service",
			"ServiceAccount", "EndpointSlice":
			return true
		}
	}

	return false
}

func allowsCreateOnUpdate(gvk schema.GroupVersionKind) bool {
	switch gvk.Group {
	case "coordination":
		switch gvk.Kind {
		case "Lease":
			return true
		}
	case "node":
		switch gvk.Kind {
		case "RuntimeClass":
			return true
		}
	case "rbac":
		switch gvk.Kind {
		case "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding":
			return true
		}
	case "":
		switch gvk.Kind {
		case "Endpoint", "Event", "LimitRange", "Service":
			return true
		}
	}

	return false
}

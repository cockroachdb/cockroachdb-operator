package kube

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// kubeServiceName refers to the name of the service that is backing the
	// Kubernetes API server within the default namespace.
	kubeServiceName = "kubernetes"

	// ConfigsChecksum is a well known annotation that is used to force
	// rollouts when the contents of dependent objects has changed. See
	// [ChecksumConfigs] for more details.
	//
	// This is also used by ApplyChecksumConfigName to avoid duplicate
	// operations.
	ConfigsChecksum = "checksum/config"
)

// ChecksumConfigs is a helper that stably hashes the CONTENTS of
// mountable [Object]s. If a ReplicaSet type (Daemonset, Deployment,
// StatefulSet, etc) relies on configuration that's mounted from an external
// object, the output of this function should be assigned to an annotation on
// the Template. This will trigger a rollout if the configuration has changed.
//
//	configMap := configMap(parameters...)
//	deployment := deployment(parameters...)
//	deployment.Spec.Template.Annotations[kube.ConfigsChecksum] = kube.ChecksumConfigs(configmap)
//	// If configMap has had any changes or updates, deployment will now perform a rollout!
//	ctl.Apply(configMap, deployment)
//
// See also: https://v2.helm.sh/docs/developing_charts/#automatically-roll-deployments-when-configmaps-or-secrets-change
func ChecksumConfigs(objects ...Object) string {
	hasher := fnv.New64()
	for _, object := range objects {
		var out []byte
		var err error

		switch t := object.(type) {
		case *corev1.Secret:
			// NB: json.Marshal consistently orders map keys.
			// NB: Secret.StringData is disallowed by a linter.
			out, err = json.Marshal(t.Data)

		case *corev1.ConfigMap:
			// NB: json.Marshal consistently orders map keys.
			out, err = json.Marshal(t.Data)

		default:
			panic(errors.AssertionFailedf("unhandled object type: %T", object))
		}

		if err != nil {
			panic(errors.AssertionFailedf("unexpected json marshalling error: %v", err))
		}

		// "It never returns an error." - https://pkg.go.dev/hash#Hash
		_, _ = hasher.Write(out)
	}

	return hex.EncodeToString(hasher.Sum(nil))
}

// ApplyChecksumConfigName mutates an object by appending a checksum to the
// input object's name, and annotations. If the objects supplied isn't a
// configmap or secret, this will return an error. Finally, this will return an
// error if callers attempt to call this multiple times.
func ApplyChecksumConfigName(obj Object) error {
	switch obj.(type) {
	case *corev1.Secret, *corev1.ConfigMap:
		if _, ok := obj.GetAnnotations()[ConfigsChecksum]; ok {
			return errors.AssertionFailedf("checksum has already been applied")
		}
		checksum := ChecksumConfigs(obj)
		obj.SetName(fmt.Sprintf("%s-%s", obj.GetName(), checksum))
		obj.SetAnnotations(map[string]string{ConfigsChecksum: checksum})
		return nil
	default:
		return errors.AssertionFailedf("unhandled object type: %T", obj)
	}
}

// GetKubernetesAPIServerIPs retrieves a list of IPs that point to the Kubernetes API server from the "kubernetes" endpoint in the default namespace.
//
// Limitations: We currently only update the IPs during cluster creation,
// re-deployment of SQL Proxy, and reconciliation of the CrdbCluster CRDs.
// One limitation to the current approach is that IPs could get stale (i.e. the
// endpoint IPs got updated). When that happens, the tenantdir component in SQL
// Proxy won't be able to reach out to the Kubernetes API server if the network
// policies are not updated accordingly.
//
// TODO: Handle stale endpoint IPs for the kubernetes endpoint resource. We
// can update the CrdbCluster reconciler to be called when the kubernetes
// endpoints object gets updated through a watcher. See SetupWithManager in
// clusterctrl/reconciler.go for more information.
//
// NOTE: Use a variable so that the function can be hooked in tests.
var GetKubernetesAPIServerIPs = func(
	ctx context.Context,
	ctl *Ctl,
) ([]string, error) {
	ctl = ctl.WithNamespace(metav1.NamespaceDefault)

	// Get current state of the "kubernetes" endpoint.
	var endpoint corev1.Endpoints
	if err := ctl.Get(ctx, kubeServiceName, &endpoint); err != nil {
		return nil, errors.Wrap(err, "retrieving kubernetes endpoints")
	}

	var ips []string
	for _, subset := range endpoint.Subsets {
		// Only consume IP addresses which are marked as ready. It is likely
		// that *all* IP addresses are marked as ready since we are already in
		// the ctl scope (i.e. talking to the Kubernetes API server), so we
		// will omit checking subset.NotReadyAddresses.
		for _, addr := range subset.Addresses {
			if addr.IP != "" {
				ips = append(ips, addr.IP)
			}
		}
	}
	if len(ips) == 0 {
		return nil, errors.New("could not retrieve kubernetes endpoint IPs")
	}
	return ips, nil
}

// StringData transforms a map[string]string into a Secret appropriate
// map[string][]byte in the same fashion that specifying StringData on a Secret
// would.
// .StringData breaks Ctl.Apply's 3-way merge, so we provide access to it via a
// helper instead.
// NOTE: end users of the go-sdk don't need to perform any base64 encoding
// themselves as json/marshal handles this. StringData doesn't need to do
// anything fancy.
// If you're constructing a map[string]string instead consider constructing a
// map[string][]byte and wrapping your strings in []byte().
func StringData(data map[string]string) map[string][]byte {
	encoded := make(map[string][]byte, len(data))
	for key, value := range data {
		encoded[key] = []byte(value)
	}
	return encoded
}

// RetryConflicts retries the action if it returns a k8s API conflict. Conflicts
// occur when there is a race between a k8s read and k8s write for a single
// object.
func RetryConflicts(action func() error) error {
	for {
		err := action()
		if !kubeerrors.IsConflict(err) {
			return err
		}
	}
}

const awsCrossZoneLoadBalancingAnnotation = "service.beta.kubernetes.io/aws-load-balancer-cross-zone-load-balancing-enabled"

// shouldPreApply checks if the provided object requires special handling by
// Ctl.Apply. Certain objects, generally due to 3rd party controllers (ie EKS),
// must be created and subsequently updated with slightly different manifests
// to be properly applied to a Kubernetes clusters. In such a case
// shouldPreApply, will return a modified copy of the object. Otherwise nil
// will be returned.
func shouldPreApply(object Object) Object {
	svc, ok := object.(*corev1.Service)
	if !ok || svc.Annotations == nil {
		return nil
	}

	// AWS' cross zone load balancing annotation reportedly (and observably)
	// does not always work when being applied at the point of creation.
	// Instead, it's been suggested by EKS reps to create services without the
	// annotation and later update them with the annotation. If the annotation
	// is found, we'll pluck it off and let Apply elect to re-Apply with the
	// original object.
	// https://gist.github.com/mgoodness/1a2926f3b02d8e8149c224d25cc57dc1#gistcomment-3322478
	// https://www.getambassador.io/docs/edge-stack/latest/topics/running/ambassador-with-aws/
	if _, ok := svc.Annotations[awsCrossZoneLoadBalancingAnnotation]; !ok {
		return nil
	}

	cp := svc.DeepCopy()
	delete(cp.Annotations, awsCrossZoneLoadBalancingAnnotation)

	return cp
}

// SetOwnerReferences is a convenience method to set the OwnerReferences on a
// list of Objects. It returns the slice passed into it, so it may be
// used as a fluent API. If a malformed object or list object is passed in,
// SetOwnerReferences panics.
//
//	ctl.Apply(ctx, kube.SetOwnerReferences(ownerRefs, objects...)...)
func SetOwnerReferences(ownerReferences []metav1.OwnerReference, objects ...Object) []Object {
	for _, object := range objects {
		accessor, err := meta.Accessor(object)
		// meta.Accessor only returns an error if object is not a scalar
		// kubernetes Object.
		if err != nil {
			panic(err)
		}
		accessor.SetOwnerReferences(ownerReferences)
	}
	return objects
}

// OwnerReferenceFor is a convenience method to generate a metav1.OwnerReference
// object from a given Object. The motivation for this method is that
// TypeMeta is not always populated. The `Controller` and `BlockOwnerDeletion`
// fields in the return object are left unset.
func OwnerReferenceFor(scheme *runtime.Scheme, object Object) (metav1.OwnerReference, error) {
	gvk, err := GVKFromScheme(scheme, object)
	if err != nil {
		return metav1.OwnerReference{}, err
	}

	accessor, err := meta.Accessor(object)
	if err != nil {
		return metav1.OwnerReference{}, errors.Wrapf(
			err, "failed to get accessor %s, %s '%s'",
			object, object.GetObjectKind().GroupVersionKind(),
			getObjectKeyOrPanic(object),
		)
	}

	return metav1.OwnerReference{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Name:       accessor.GetName(),
		UID:        accessor.GetUID(),
	}, nil
}

// SleepContext sleeps for the given duration and returns true, unless the
// context is canceled, in which case it aborts early and returns false.
//
// NOTE: Use a variable so that the function can be hooked in tests.
var SleepContext = func(ctx context.Context, duration time.Duration) bool {
	select {
	case <-ctx.Done():
		return false

	case <-time.After(duration):
		return true
	}
}

// getObjectKeyOrPanic returns the given K8s object's key (namespace and name).
// The caller is responsible to make sure this will never fail.
func getObjectKeyOrPanic(object Object) client.ObjectKey {
	return client.ObjectKey{Namespace: object.GetNamespace(), Name: object.GetName()}
}

// getAccessorOrPanic returns a meta accessor for the given K8s object. The
// caller is responsible to make sure this will never fail.
func getAccessorOrPanic(object Object) metav1.Object {
	accessor, err := meta.Accessor(object)
	if err != nil {
		panic(errors.AssertionFailedf("failed to get object ResourceVersion: %+v", object))
	}
	return accessor
}

// filterOutDaemonSetPods takes a slice of pods and returns a new slice
// of pods containing only pods not managed by a DaemonSet.
func filterOutDaemonSetPods(pods []corev1.Pod) []corev1.Pod {
	retPods := make([]corev1.Pod, 0, len(pods))
	for _, pod := range pods {
		controller := metav1.GetControllerOf(&pod)
		if controller == nil || controller.Kind != "DaemonSet" {
			retPods = append(retPods, pod)
		}
	}
	return retPods
}

func PodsOnNodeSelector(node *corev1.Node) fields.Selector {
	return fields.SelectorFromSet(fields.Set{"spec.nodeName": node.Name})
}

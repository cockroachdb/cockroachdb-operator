package autoscaler

import (
	"context"
	"fmt"
	"strconv"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

var invalidTenantID = errors.New("invalid tenant id")

// currentLoadInformer does a list/watch on all tenant pods and keeps track
// of pod IPs per tenant/namespace. It also calls Add/Update/Delete callbacks
// on Pod changes.
type currentLoadInformer struct {
	reflector *cache.Reflector
	store     *currentLoadInformerStore
}

// handlerFuncs contains the callbacks that should be called on Pod changes.
type handlerFuncs struct {
	AddFunc    func(id string, tenantKey metrics.TenantKey, ip string)
	DeleteFunc func(id string)
}

var _ cache.Store = &currentLoadInformerStore{}

// currentLoadInformerStore implements cache.Store and is used by the Reflector
// instance.
type currentLoadInformerStore struct {
	pods    map[string]podInfo
	handler handlerFuncs
}

// podInfo links together a pod (id) with a tenant (key) to an ip of the pod.
type podInfo struct {
	id  string
	key metrics.TenantKey
	ip  string
}

// Add is called when a new pod is created.
func (i *currentLoadInformerStore) Add(obj any) error {
	p, err := newPodFromK8sObj(obj)
	if err != nil {
		switch {
		case errors.Is(err, invalidTenantID):
			// Log the error but don't pass back to k8s.
			logging.Errorf(context.Background(), "unable to add pod: %s", err)
			return nil
		default:
			return err
		}
	}
	i.pods[p.id] = p
	if len(p.ip) > 0 {
		// Notify only if the pod has a valid IP already.
		i.handler.AddFunc(p.id, p.key, p.ip)
	}
	return nil
}

// Update is called when a pod related information changes.
func (i *currentLoadInformerStore) Update(obj any) error {
	p, err := newPodFromK8sObj(obj)
	if err != nil {
		switch {
		case errors.Is(err, invalidTenantID):
			// Log the error but don't pass back to k8s.
			logging.Errorf(context.Background(), "unable to update pod: %s", err)
			return nil
		default:
			return err
		}
	}
	oldPod := i.pods[p.id]
	i.pods[p.id] = p
	if p.key.TenantID != oldPod.key.TenantID || oldPod.ip != p.ip {
		if p.key.TenantID != oldPod.key.TenantID {
			// This should generally never happen so logging an error.
			logging.Errorf(context.Background(),
				"ns %s tid %d ip %s pod changes to tid %d ip %s",
				oldPod.key.Namespace, oldPod.key.TenantID, oldPod.ip, p.key.TenantID, p.ip,
			)
		}
		// either tne TenantID changes somehow or more likely an IP got assigned
		// to a pod that didn't have one or a pod got its IP removed.
		if len(oldPod.ip) > 0 {
			i.handler.DeleteFunc(oldPod.id)
		}
		if len(p.ip) > 0 {
			i.handler.AddFunc(p.id, p.key, p.ip)
		}
	}
	return nil
}

// Delete is called when a pod is deleted.
func (i *currentLoadInformerStore) Delete(obj any) error {
	p, err := newPodFromK8sObj(obj)
	if err != nil {
		switch {
		case errors.Is(err, invalidTenantID):
			// Log the error but don't pass back to k8s.
			logging.Errorf(context.Background(), "unable to delete pod: %s", err)
			return nil
		default:
			return err
		}
	}
	oldPod := i.pods[p.id]
	delete(i.pods, p.id)
	if len(oldPod.ip) > 0 {
		i.handler.DeleteFunc(oldPod.id)
	}
	return nil
}

// List is not used but is required by the Store interface.
func (i *currentLoadInformerStore) List() []any {
	return []any{}
}

// ListKeys is not used but is required by the Store interface.
func (i *currentLoadInformerStore) ListKeys() []string {
	return []string{}
}

// Get is not used but is required by the Store interface.
func (i *currentLoadInformerStore) Get(obj any) (item any, exists bool, err error) {
	return nil, false, nil
}

// GetByKey is not used but is required by the Store interface.
func (i *currentLoadInformerStore) GetByKey(key string) (item any, exists bool, err error) {
	return nil, false, nil
}

// Replace sets the initial list of objects(pods) on startup.
func (i *currentLoadInformerStore) Replace(i2 []any, resourceVersion string) error {
	for _, pod := range i.pods {
		if len(pod.ip) > 0 {
			i.handler.DeleteFunc(pod.id)
		}
	}
	i.pods = make(map[string]podInfo)
	for _, podObj := range i2 {
		if err := i.Add(podObj); err != nil {
			return err
		}
	}
	return nil
}

// Resync is not used but is required by the Store interface.
func (i *currentLoadInformerStore) Resync() error {
	// do nothing
	return nil
}

// NewInformer creates a thew informer instance that will watch the pods in the
// given namespace and call back handler functions on each change.
func NewInformer(
	ctx context.Context, clientSet kubernetes.Interface, ns string, handler handlerFuncs,
) (*currentLoadInformer, error) {
	store := &currentLoadInformerStore{
		make(map[string]podInfo),
		handler,
	}
	reflector := cache.NewReflector(
		&cache.ListWatch{
			ListFunc: func(options v1.ListOptions) (runtime.Object, error) {
				options.LabelSelector = manifests.TenantIDLabel
				return clientSet.CoreV1().Pods(ns).List(ctx, options)
			},
			WatchFunc: func(options v1.ListOptions) (watch.Interface, error) {
				options.LabelSelector = manifests.TenantIDLabel
				return clientSet.CoreV1().Pods(ns).Watch(ctx, options)
			},
		},
		&corev1.Pod{},
		store,
		0, // No need to do periodic re-syncs.
	)
	return &currentLoadInformer{
		reflector: reflector,
		store:     store,
	}, nil
}

// Run will do a list, followed by watch and run until the stop channel gets
// closed.
func (i *currentLoadInformer) Run(ctx context.Context) {
	i.reflector.Run(ctx.Done())
}

// newPodFromK8sObj expects a k8s pod object and returns a pod probe struct
// that contains the pod id, tenant id and the k8s object typed as k8s pod object.
func newPodFromK8sObj(podObj any) (podInfo, error) {
	meta, err := meta.Accessor(podObj)
	if err != nil {
		return podInfo{}, cache.KeyError{Obj: podObj, Err: err}
	}
	k8sPod, ok := podObj.(*corev1.Pod)
	if !ok {
		return podInfo{}, fmt.Errorf("expected Pod but got %T instead", podObj)
	}
	tenantIDString, hasTenantID := k8sPod.Labels[manifests.TenantIDLabel]
	if !hasTenantID {
		return podInfo{}, errors.Wrapf(invalidTenantID,
			"no tenant id label for pod %s with labels %s", k8sPod.Name, k8sPod.Labels)
	}
	tenantIDInt, err := strconv.ParseUint(tenantIDString, 10, 64)
	if err != nil {
		return podInfo{}, errors.Wrapf(invalidTenantID,
			"tenant id %s is not int for pod %s", tenantIDString, k8sPod.Name)
	}
	return podInfo{
		meta.GetName(),
		metrics.TenantKey{
			Namespace: k8sPod.Namespace, TenantID: v1beta1.TenantID(tenantIDInt),
		},
		k8sPod.Status.PodIP,
	}, nil
}

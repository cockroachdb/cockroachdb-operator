package autoscaler

import (
	"testing"
	"time"

	"github.com/cockroachlabs/cockroach-enterprise-operator/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCurrentLoadInformer(t *testing.T) {
	testutils.GoroutineLeakCheck(t)

	ctx := t.Context()

	// target namespaces and tenant-id label
	pod1 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "1", Namespace: "ns1",
		Labels: map[string]string{"tenant-id": "10"}}}
	pod1.Status.PodIP = "1.1.1.1"
	// target namespaces and tenant-id label #2
	pod2 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "2", Namespace: "ns1",
		Labels: map[string]string{"tenant-id": "20"}}}
	pod2.Status.PodIP = "2.2.2.2"
	// outside of target namespaces but with tenant-id label
	pod3 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "3", Namespace: "ns2",
		Labels: map[string]string{"tenant-id": "30"}}}
	pod3.Status.PodIP = "3.3.3.3"
	// target namespaces but no tenant-id label
	pod4 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "4", Namespace: "ns1"}}
	pod4.Status.PodIP = "4.4.4.4"
	// target namespace and tenant-id label but the tenant-id label is not int
	pod6 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "6", Namespace: "ns1",
		Labels: map[string]string{"tenant-id": "dim-dog"}}}
	pod6.Status.PodIP = "6.6.6.6"
	// target namespaces and tenant-id label but doesn't have ip assigned
	pod7 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "7", Namespace: "ns1",
		Labels: map[string]string{"tenant-id": "70"}}}

	cs := fake.NewSimpleClientset(pod1, pod2, pod3, pod4, pod6, pod7)
	var added []podInfo
	var deleted []string
	handlers := handlerFuncs{
		AddFunc: func(id string, key metrics.TenantKey, ip string) {
			added = append(added, podInfo{id: id, key: key, ip: ip})
		},
		DeleteFunc: func(id string) {
			deleted = append(deleted, id)
		},
	}
	i, err := NewInformer(ctx, cs, "ns1", handlers)
	require.NoError(t, err)
	go i.Run(ctx)
	require.Eventually(t, func() bool {
		return len(added) == 2 && len(deleted) == 0
	}, time.Second, 100*time.Millisecond)
	// pod1 and pod2 only
	require.ElementsMatch(t, added, []podInfo{
		{id: "1", key: metrics.TenantKey{Namespace: "ns1", TenantID: 10}, ip: "1.1.1.1"},
		{id: "2", key: metrics.TenantKey{Namespace: "ns1", TenantID: 20}, ip: "2.2.2.2"},
	})
	added = nil

	// now add a new pod and verify that it gets added
	pod5 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "5", Namespace: "ns1",
		Labels: map[string]string{"tenant-id": "50"}}}
	pod5.Status.PodIP = "5.5.5.5"
	require.NoError(t, cs.Tracker().Add(pod5))
	require.Eventually(t, func() bool { return len(added) == 1 && len(deleted) == 0 }, time.Second, 100*time.Millisecond)
	require.ElementsMatch(t, added, []podInfo{{id: "5", key: metrics.TenantKey{Namespace: "ns1", TenantID: 50}, ip: "5.5.5.5"}})
	added = nil

	pods := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	// delete 2 pods and verify that they are deleted
	require.NoError(t, cs.Tracker().Delete(pods, "ns1", "5"))
	require.NoError(t, cs.Tracker().Delete(pods, "ns1", "2"))
	require.Eventually(t, func() bool { return len(added) == 0 && len(deleted) == 2 }, time.Second, 100*time.Millisecond)
	require.ElementsMatch(t, deleted, []string{"2", "5"})
	deleted = nil

	// now update pod's ip and verify that it gets reflected.
	pod1.Status.PodIP = "11.11.11.11"
	require.NoError(t, cs.Tracker().Update(pods, pod1, "ns1"))
	require.Eventually(t, func() bool { return len(added) == 1 && len(deleted) == 1 }, time.Second, 100*time.Millisecond)
	require.ElementsMatch(t, deleted, []string{"1"})
	require.ElementsMatch(t, added, []podInfo{
		{id: "1", key: metrics.TenantKey{Namespace: "ns1", TenantID: 10}, ip: "11.11.11.11"}},
	)
	added = nil
	deleted = nil

	// if pod's ip is blanked that would result in delete only
	pod1.Status.PodIP = ""
	require.NoError(t, cs.Tracker().Update(pods, pod1, "ns1"))
	require.Eventually(t, func() bool { return len(added) == 0 && len(deleted) == 1 }, time.Second, 100*time.Millisecond)
	require.ElementsMatch(t, deleted, []string{"1"})
	deleted = nil

	// now update something else besides ip and verify that it doesn't result in update.
	pod1.Status.Phase = corev1.PodUnknown
	require.NoError(t, cs.Tracker().Update(pods, pod1, "ns1"))
	time.Sleep(500 * time.Millisecond)
	require.Len(t, added, 0)
	require.Len(t, deleted, 0)
}

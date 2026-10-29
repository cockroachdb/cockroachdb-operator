package testmanifest

import (
	"fmt"
	"log"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func (f *Factory) CreateCluster() *crdbv1beta1.CrdbCluster {
	id := f.NextID()
	return &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("cluster-%d", id),
			Namespace: "default",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Features: []crdbv1beta1.ClusterFeature{crdbv1beta1.ClusterTenantCreateAdhocPods},
		},
	}
}

// CreateTenant returns a minimal tenant in the high trust tenant pool.
func (f *Factory) CreateTenant(cluster *crdbv1beta1.CrdbCluster) *crdbv1beta1.CrdbTenant {
	id := f.NextID()
	return &crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("tenant-%d", id),
			Namespace: f.namespace,
			OwnerReferences: []metav1.OwnerReference{
				{Name: cluster.Name, Kind: crdbv1beta1.CrdbClusterKind},
			},
		},
		Spec: crdbv1beta1.CrdbTenantSpec{
			TenantID:   crdbv1beta1.TenantID(id),
			TenantPool: new("v25-1-2"),
		},
	}
}

// CreateStampedPod returns a pod that is bound to the argument tenant.
func (f *Factory) CreateStampedPod(tenant *crdbv1beta1.CrdbTenant) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("pod-%d", f.NextID()),
			Namespace: "default",
			Labels: labels.Set{
				"app":                         manifests.TenantPoolAppName,
				manifests.TenantPoolNameLabel: *tenant.Spec.TenantPool,
				manifests.TenantIDLabel:       tenant.Spec.TenantID.String(),
				manifests.TenantPodStateLabel: string(manifests.TenantPodRunningState),
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: manifests.CockroachDB, Ready: true},
			},
		},
	}
}

// CreateUnstampedPod returns an unstamped sql pod.
func (f *Factory) CreateUnstampedPod() *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("pod-%d", f.NextID()),
			Namespace: "default",
			Labels: labels.Set{
				"app":                         manifests.TenantPoolAppName,
				manifests.TenantPoolNameLabel: "v25-1-2",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: manifests.CockroachDB, Ready: true},
			},
		},
	}
	if err := manifests.SetTenantPodState(pod, manifests.TenantPodPendingState); err != nil {
		log.Fatalf("unexpected error creating unstamped pod: %v", err)
	}
	return pod
}

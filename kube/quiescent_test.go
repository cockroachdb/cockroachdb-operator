package kube

import (
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// NotReallyARuntimeObject is a non-kubernetes object that implements
// Object. It is used for testing the behavior of functions
// that receive Objects that they may not handle.
type NotReallyARuntimeObject struct {
	Object
}

func (*NotReallyARuntimeObject) GetObjectKind() schema.ObjectKind {
	return nil
}

func (*NotReallyARuntimeObject) DeepCopyObject() runtime.Object {
	return nil
}

var _ Object = &NotReallyARuntimeObject{}

func TestIsQuiescent(t *testing.T) {
	t.Run("unknown type", func(t *testing.T) {
		require.Panics(t, func() {
			_ = IsQuiescent(&NotReallyARuntimeObject{})
		})
	})

	testCases := []struct {
		Name   string
		Object Object
		Err    string
	}{
		{
			Name: "CrdbCluster - Quiescent",
			Object: &crdbv1beta1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Status: crdbv1beta1.CrdbClusterStatus{
					ObservedGeneration: 2,
					Conditions: []crdbv1beta1.ClusterCondition{
						{Type: crdbv1beta1.ClusterInitialized, Status: metav1.ConditionTrue},
					},
				},
			},
		},
		{
			Name: "CrdbCluster - Upgrading",
			Err:  "cluster has a pending Upgrade action with status Starting",
			Object: &crdbv1beta1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Status: crdbv1beta1.CrdbClusterStatus{
					ObservedGeneration: 2,
					Conditions: []crdbv1beta1.ClusterCondition{
						{Type: crdbv1beta1.ClusterInitialized, Status: metav1.ConditionTrue},
					},
					Actions: []crdbv1beta1.ClusterAction{
						{Type: crdbv1beta1.Upgrade, Status: crdbv1beta1.Starting},
					},
				},
			},
		},
		{
			Name: "DaemonSet - Quiescent",
			Object: &appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Status: appsv1.DaemonSetStatus{
					ObservedGeneration:     2,
					NumberReady:            3,
					DesiredNumberScheduled: 3,
					UpdatedNumberScheduled: 3,
				},
			},
		},
		{
			Name: "DaemonSet - Updating",
			Err:  "only 1/3 pods are updated",
			Object: &appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Status: appsv1.DaemonSetStatus{
					ObservedGeneration:     2,
					NumberReady:            3,
					DesiredNumberScheduled: 3,
					UpdatedNumberScheduled: 1,
				},
			},
		},
		{
			Name: "Deployment - Quiescent",
			Object: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: new(int32(4)),
				},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 2,
					Replicas:           4,
					UpdatedReplicas:    4,
					AvailableReplicas:  4,
					Conditions: []appsv1.DeploymentCondition{
						{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
						{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable"},
					},
				},
			},
		},
		{
			Name: "Deployment - ReplicaSet not available",
			Err:  "deployment has not fully rolled out yet",
			Object: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: new(int32(4)),
				},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 2,
					Replicas:           4,
					UpdatedReplicas:    4,
					AvailableReplicas:  4,
					Conditions: []appsv1.DeploymentCondition{
						{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
						{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue},
					},
				},
			},
		},
		{
			Name: "Deployment - Updating",
			Err:  "only 2/3 deployment replicas are updated",
			Object: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: new(int32(3)),
				},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 2,
					Replicas:           3,
					UpdatedReplicas:    2,
					AvailableReplicas:  3,
					Conditions: []appsv1.DeploymentCondition{
						{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
						{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue},
					},
				},
			},
		},
		{
			Name: "Deployment - Not Available",
			Err:  "deployment has not fully rolled out yet",
			Object: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: new(int32(3)),
				},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 2,
					Replicas:           3,
					UpdatedReplicas:    3,
					AvailableReplicas:  3,
					Conditions: []appsv1.DeploymentCondition{
						{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue},
					},
				},
			},
		},
		{
			Name: "STS - Quiescent OnDelete",
			Object: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Spec: appsv1.StatefulSetSpec{
					Replicas: new(int32(3)),
					UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
						Type: appsv1.OnDeleteStatefulSetStrategyType,
					},
				},
				Status: appsv1.StatefulSetStatus{
					UpdateRevision:     "some-hash-1",
					CurrentRevision:    "some-hash-2",
					ObservedGeneration: 2,
					Replicas:           3,
					ReadyReplicas:      3,
					CurrentReplicas:    2,
					UpdatedReplicas:    1,
				},
			},
		},
		{
			Name: "Not Quiescent - RollingUpdate Partition",
			Err:  "only 1/2 partitioned replicas are updated",
			Object: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Spec: appsv1.StatefulSetSpec{
					Replicas: new(int32(3)),
					UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
						Type: appsv1.RollingUpdateStatefulSetStrategyType,
						RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{
							Partition: new(int32(1)),
						},
					},
				},
				Status: appsv1.StatefulSetStatus{
					ObservedGeneration: 2,
					Replicas:           3,
					ReadyReplicas:      3,
					CurrentReplicas:    2,
					UpdatedReplicas:    1,
				},
			},
		},
		{
			Name: "Quiescent - RollingUpdate Partition",
			Object: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Spec: appsv1.StatefulSetSpec{
					Replicas: new(int32(3)),
					UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
						Type: appsv1.RollingUpdateStatefulSetStrategyType,
						RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{
							Partition: new(int32(1)),
						},
					},
				},
				Status: appsv1.StatefulSetStatus{
					ObservedGeneration: 2,
					Replicas:           3,
					ReadyReplicas:      3,
					CurrentReplicas:    1,
					UpdatedReplicas:    2,
				},
			},
		},
		{
			Name: "Quiescent - RollingUpdate",
			Err:  "sts update not yet complete revision some-hash-1 != some-hash-2",
			Object: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Spec: appsv1.StatefulSetSpec{
					Replicas: new(int32(3)),
					UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
						Type: appsv1.RollingUpdateStatefulSetStrategyType,
					},
				},
				Status: appsv1.StatefulSetStatus{
					ObservedGeneration: 2,
					Replicas:           3,
					ReadyReplicas:      3,
					CurrentRevision:    "some-hash-1",
					UpdateRevision:     "some-hash-2",
				},
			},
		},
		{
			Name: "Quiescent - RollingUpdate",
			Object: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
				},
				Spec: appsv1.StatefulSetSpec{
					Replicas: new(int32(3)),
					UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
						Type: appsv1.RollingUpdateStatefulSetStrategyType,
					},
				},
				Status: appsv1.StatefulSetStatus{
					ObservedGeneration: 2,
					Replicas:           3,
					ReadyReplicas:      3,
					CurrentRevision:    "some-hash-2",
					UpdateRevision:     "some-hash-2",
				},
			},
		},
	}

	for _, tc := range testCases {
		err := IsQuiescent(tc.Object)
		if tc.Err == "" {
			require.NoError(t, err)
		} else {
			require.EqualError(t, err, tc.Err)
		}
	}
}

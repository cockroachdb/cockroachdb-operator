package kube

import (
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsDeleted(t *testing.T) {
	t.Run("deleted object", func(t *testing.T) {
		require.NoError(t, IsDeleted(nil))
	})

	t.Run("deleted not object", func(t *testing.T) {
		ns := &corev1.Namespace{
			// Would generally be populated by the CTL.
			TypeMeta: metav1.TypeMeta{
				Kind: "Namespace",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "foobar",
			},
		}
		require.EqualError(t, IsDeleted(ns), "object Namespace/foobar is not yet deleted")
	})
}

func TestIsReady(t *testing.T) {
	t.Run("unknown type", func(t *testing.T) {
		require.Panics(t, func() {
			_ = IsReady(&NotReallyARuntimeObject{})
		})
	})

	t.Run("deleted object", func(t *testing.T) {
		require.EqualError(t, IsReady(nil), "object does not exist or has been deleted: abort wait")
		require.ErrorIs(t, IsReady(nil), ErrAbortWait)
	})

	t.Run("namespace is ready", func(t *testing.T) {
		ns := &corev1.Namespace{Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}}
		require.NoError(t, IsReady(ns))
		ns.Status.Phase = corev1.NamespaceTerminating
		require.EqualError(t, IsReady(ns), "namespace not active: Terminating")
	})

	t.Run("service is ready", func(t *testing.T) {
		// Cluster IP service.
		svc := &corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP}}
		require.EqualError(t, IsReady(svc), "cluster IP is empty")
		svc.Spec.ClusterIP = "127.0.0.1"
		require.NoError(t, IsReady(svc))
		svc.Spec.ClusterIP = "None"
		require.NoError(t, IsReady(svc))

		// Unspecified Types are regarded as "ClusterIP"
		svc.Spec.Type = ""
		svc.Spec.ClusterIP = ""
		require.EqualError(t, IsReady(svc), "cluster IP is empty")
		svc.Spec.ClusterIP = "127.0.0.1"
		require.NoError(t, IsReady(svc))
		require.EqualError(t, IsReady(&corev1.Service{}), "cluster IP is empty")

		// Load balancer service.
		svc.Spec.Type = corev1.ServiceTypeLoadBalancer
		require.EqualError(t, IsReady(svc), "empty load balancer ingress")
		// Some LoadBalancers report both a hostname and IP.
		svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{
			{IP: "127.0.0.1", Hostname: "my-host"},
		}
		require.NoError(t, IsReady(svc))
		// Some LoadBalancer will only report an IP (GCP).
		svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{
			{IP: "127.0.0.1"},
		}
		require.NoError(t, IsReady(svc))
		// Some LoadBalancers will only report a hostname (AWS).
		svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{
			{Hostname: "my-host"},
		}
		require.NoError(t, IsReady(svc))
		// LoadBalancer without an ingress should report a error.
		svc.Status.LoadBalancer.Ingress = nil
		require.Error(t, IsReady(svc))
		// LoadBalancer with the annotation that exempts readiness checking should be considered ready.
		svc.Annotations = map[string]string{
			IgnoreEmptyLoadBalancerAnnotation: "true",
		}
		require.NoError(t, IsReady(svc))
	})

	t.Run("endpoints is ready", func(t *testing.T) {
		endpoints := &corev1.Endpoints{}
		require.EqualError(t, IsReady(endpoints), "no endpoint is ready out of 0 subsets")
		// One endpoint that is not ready.
		endpoints.Subsets = []corev1.EndpointSubset{
			{NotReadyAddresses: []corev1.EndpointAddress{{IP: "127.0.0.1"}}},
		}
		require.EqualError(t, IsReady(endpoints), "no endpoint is ready out of 1 subsets")
		// Another endpoint that is ready in same subset.
		endpoints.Subsets[0].Addresses = []corev1.EndpointAddress{{IP: "127.0.0.1"}}
		require.NoError(t, IsReady(endpoints))
		// Another subset that is not ready.
		endpoints.Subsets = append(endpoints.Subsets, corev1.EndpointSubset{
			NotReadyAddresses: []corev1.EndpointAddress{{IP: "127.0.0.1"}},
		})
		require.NoError(t, IsReady(endpoints))
		// Remove first ready endpoint.
		endpoints.Subsets[0].Addresses = nil
		require.EqualError(t, IsReady(endpoints), "no endpoint is ready out of 2 subsets")
	})

	t.Run("deployment is ready", func(t *testing.T) {
		desired := int32(3)
		deployment := &appsv1.Deployment{
			Status: appsv1.DeploymentStatus{
				ObservedGeneration: 2,
				Replicas:           3,
				UpdatedReplicas:    3,
				AvailableReplicas:  3,
			},
		}

		// Zero Generation.
		require.EqualError(t, IsReady(deployment), ".Generation must be greater than 0 for Readiness checks")
		deployment.Generation = 2

		// Replicas is nil.
		require.EqualError(t, IsReady(deployment), "deployment replicas were nil")

		// Replicas is not-nil.
		deployment.Spec.Replicas = &desired
		require.NoError(t, IsReady(deployment))

		// Replicas doesn't match.
		deployment.Status.Replicas = 4
		require.EqualError(t, IsReady(deployment), "created replicas 4 does not match desired replicas 3")
		deployment.Status.Replicas = 3

		// AvailableReplicas doesn't match.
		deployment.Status.AvailableReplicas = 2
		require.EqualError(t, IsReady(deployment), "only 2/3 deployment replicas are available")
		deployment.Status.AvailableReplicas = 3

		// ObservedGeneration doesn't match.
		deployment.Status.ObservedGeneration = 1
		require.EqualError(t, IsReady(deployment), "observed deployment generation 1 is lower than expected generation 2")
		deployment.Status.ObservedGeneration = 3

		// All should again match.
		require.NoError(t, IsReady(deployment))
	})

	t.Run("statefulset is ready", func(t *testing.T) {
		desired := int32(3)
		sts := &appsv1.StatefulSet{
			Status: appsv1.StatefulSetStatus{
				ObservedGeneration: 2,
				Replicas:           3,
				ReadyReplicas:      3,
			},
		}

		// Zero Generation.
		require.EqualError(t, IsReady(sts), ".Generation must be greater than 0 for Readiness checks")
		sts.Generation = 2

		// Replicas is nil.
		require.EqualError(t, IsReady(sts), "stateful set replicas were nil")

		// Replicas is not-nil.
		sts.Spec.Replicas = &desired
		require.NoError(t, IsReady(sts))

		// Replicas doesn't match.
		sts.Status.Replicas = 4
		require.EqualError(t, IsReady(sts), "created replicas 4 does not match desired replicas 3")
		sts.Status.Replicas = 3

		// ReadyReplicas don't match.
		sts.Status.ReadyReplicas = 2
		require.EqualError(t, IsReady(sts), "only 2/3 stateful set replicas are ready")
		sts.Status.ReadyReplicas = 3

		// ObservedGeneration doesn't match.
		sts.Status.ObservedGeneration = 1
		require.EqualError(t, IsReady(sts), "observed stateful set generation 1 is lower than expected generation 2")
		sts.Status.ObservedGeneration = 3

		// All should again match.
		require.NoError(t, IsReady(sts))
	})

	t.Run("daemonset is ready", func(t *testing.T) {
		daemonset := &appsv1.DaemonSet{
			Status: appsv1.DaemonSetStatus{
				ObservedGeneration:     2,
				DesiredNumberScheduled: 3,
				NumberReady:            3,
			},
		}

		// Zero Generation.
		require.EqualError(t, IsReady(daemonset), ".Generation must be greater than 0 for Readiness checks")
		daemonset.Generation = 2

		// NumberUnavailable > 0
		daemonset.Status.NumberUnavailable = 1
		require.EqualError(t, IsReady(daemonset), "1 replica(s) are unavailable")
		daemonset.Status.NumberUnavailable = 0

		// NumberReady doesn't match.
		daemonset.Status.NumberReady = 2
		require.EqualError(t, IsReady(daemonset), "only 2/3 pods are ready")

		// ObservedGeneration doesn't match.
		daemonset.Status.NumberReady = 3
		daemonset.Status.ObservedGeneration = 1
		require.EqualError(t, IsReady(daemonset), "observed daemon set generation 1 is lower than expected generation 2")

		// All should again match.
		daemonset.Status.ObservedGeneration = 3
		require.NoError(t, IsReady(daemonset))
	})

	t.Run("pod is ready", func(t *testing.T) {
		require.EqualError(t, IsReady(&corev1.Pod{}), "no pod conditions found")
		require.EqualError(t, IsReady(&corev1.Pod{
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{}}}), "no pod readiness condition found")
		require.EqualError(t, IsReady(&corev1.Pod{
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			}}}), "pod readiness condition status is False")
		require.NoError(t, IsReady(&corev1.Pod{
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			}}}))
	})

	t.Run("crd is ready", func(t *testing.T) {
		crd := &apiextv1.CustomResourceDefinition{
			Status: apiextv1.CustomResourceDefinitionStatus{
				Conditions: []apiextv1.CustomResourceDefinitionCondition{
					{Type: apiextv1.NamesAccepted, Status: apiextv1.ConditionTrue},
				},
			},
		}
		require.NoError(t, IsReady(crd))
		crd.Status.Conditions[0].Status = apiextv1.ConditionFalse
		crd.Status.Conditions = append(
			crd.Status.Conditions,
			apiextv1.CustomResourceDefinitionCondition{
				Type:   apiextv1.Established,
				Status: apiextv1.ConditionTrue},
		)
		require.EqualError(t, IsReady(crd), "crd name was not accepted")
	})

	t.Run("pod disruption budget is ready", func(t *testing.T) {
		budget := &policyv1.PodDisruptionBudget{Status: policyv1.PodDisruptionBudgetStatus{
			DesiredHealthy: 3,
			CurrentHealthy: 3,
		}}
		require.NoError(t, IsReady(budget))
		budget.Status.CurrentHealthy = 2
		require.EqualError(t, IsReady(budget), "only 2/3 pods are healthy")
	})

	t.Run("trivial types", func(t *testing.T) {
		require.NoError(t, IsReady(&corev1.ConfigMap{}))
		require.NoError(t, IsReady(&corev1.Secret{}))
		require.NoError(t, IsReady(&corev1.ServiceAccount{}))
		require.NoError(t, IsReady(&rbacv1.ClusterRole{}))
		require.NoError(t, IsReady(&rbacv1.ClusterRoleBinding{}))
		require.NoError(t, IsReady(&rbacv1.Role{}))
		require.NoError(t, IsReady(&rbacv1.RoleBinding{}))
	})
}

func TestIsNodeSchedulable(t *testing.T) {
	readyNode := func() *corev1.Node {
		return &corev1.Node{
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{
					{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
				},
			},
		}
	}

	t.Run("ready untainted node is schedulable", func(t *testing.T) {
		require.NoError(t, IsNodeSchedulable(readyNode()))
	})

	t.Run("not ready node is not schedulable", func(t *testing.T) {
		node := readyNode()
		node.Status.Conditions[0].Status = corev1.ConditionFalse
		require.EqualError(t, IsNodeSchedulable(node), "no node status ready condition found")
	})

	t.Run("cordoned node is not schedulable", func(t *testing.T) {
		node := readyNode()
		node.Spec.Unschedulable = true
		require.EqualError(t, IsNodeSchedulable(node), "node is cordoned")
	})

	t.Run("NoSchedule taint is not schedulable", func(t *testing.T) {
		node := readyNode()
		node.Spec.Taints = []corev1.Taint{
			{Key: "some-random-key", Effect: corev1.TaintEffectNoSchedule},
		}
		require.EqualError(t, IsNodeSchedulable(node), "node has NoSchedule taint some-random-key")
	})

	t.Run("NoExecute taint is not schedulable", func(t *testing.T) {
		node := readyNode()
		node.Spec.Taints = []corev1.Taint{
			{Key: "another-random-key", Effect: corev1.TaintEffectNoExecute},
		}
		require.EqualError(t, IsNodeSchedulable(node), "node has NoExecute taint another-random-key")
	})
}

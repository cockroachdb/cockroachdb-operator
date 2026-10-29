package kubetest

import (
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
)

// Indirection added to set appropriately for tests.
// TODO(CC-24774): By modifying how we interact with the fake ctl we can also
// modify this to always return 127.0.0.1.
var ServiceLoadBalancerReadyIP = ""

var customReadyHandlers []func(object kube.Object) error

// AddCustomReadyHandler adds a function that handles readiness for a custom
// object that is not handled by MakeReady. It should be called in the init
// method of a calling package.
func AddCustomReadyHandler(handler func(object kube.Object) error) {
	customReadyHandlers = append(customReadyHandlers, handler)
}

// MakeReady takes a K8s object and attempts to set its fields such that it will
// be declared "ready" by the kube.IsReady function. This is useful when using
// the FakeCtl to avoid blocking forever when Apply or other methods are called.
// Typical usage:
//
//	ctl := kubetest.NewFakeCtl(t, kube.Options{BeforeMutation: MakeReady})
func MakeReady(object kube.Object) error {
	switch t := object.(type) {
	case *corev1.Namespace:
		t.Status.Phase = corev1.NamespaceActive

	case *corev1.Service:
		switch t.Spec.Type {
		case corev1.ServiceTypeLoadBalancer:
			t.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{
				{
					Hostname: "localhost",
					IP:       ServiceLoadBalancerReadyIP,
				},
			}
		// NB: The default value for .Type is ServiceTypeClusterIP.
		case corev1.ServiceTypeClusterIP, "":
			// TODO(CC-24776): Smooth this out so we don't need to run these
			// checks.
			if t.Name != "alertmanager" && t.Name != "kube-state-metrics" && t.Name != "node-exporter" {
				t.Spec.ClusterIP = "127.0.0.1"
			}
		}

	case *corev1.Endpoints:
		t.Subsets = []corev1.EndpointSubset{
			{Addresses: []corev1.EndpointAddress{{IP: "127.0.0.1"}}},
		}

	case *appsv1.Deployment:
		if t.Generation < 1 {
			t.Generation = 1
		}
		replicas := int32(1)
		if t.Spec.Replicas != nil {
			replicas = *t.Spec.Replicas
		}
		t.Spec.Replicas = new(replicas)
		t.Status.Replicas = replicas
		t.Status.AvailableReplicas = replicas
		t.Status.ObservedGeneration = t.Generation

	case *appsv1.StatefulSet:
		if t.Generation < 1 {
			t.Generation = 1
		}
		replicas := int32(1)
		if t.Spec.Replicas != nil {
			replicas = *t.Spec.Replicas
		}
		t.Spec.Replicas = new(replicas)
		t.Status.Replicas = replicas
		t.Status.ReadyReplicas = replicas
		t.Status.ObservedGeneration = t.Generation

	case *corev1.Pod:
		t.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		}

	case *corev1.Node:
		t.Status.Conditions = []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		}

	case *appsv1.DaemonSet:
		if t.Generation < 1 {
			t.Generation = 1
		}
		t.Status.NumberReady = t.Status.DesiredNumberScheduled
		t.Status.ObservedGeneration = t.Generation

	case *apiextv1beta1.CustomResourceDefinition:
		t.Status.Conditions = []apiextv1beta1.CustomResourceDefinitionCondition{
			{Type: apiextv1beta1.NamesAccepted, Status: apiextv1beta1.ConditionTrue},
		}

	case *apiextv1.CustomResourceDefinition:
		t.Status.Conditions = []apiextv1.CustomResourceDefinitionCondition{
			{Type: apiextv1.NamesAccepted, Status: apiextv1.ConditionTrue},
		}

	case *policyv1.PodDisruptionBudget:
		t.Status.CurrentHealthy = t.Status.DesiredHealthy
	}

	// Invoke all custom handlers.
	for _, handler := range customReadyHandlers {
		if err := handler(object); err != nil {
			return err
		}
	}

	return nil
}

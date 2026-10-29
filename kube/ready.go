package kube

import (
	"fmt"

	"github.com/cockroachdb/errors"
	crdbv1alpha1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1alpha1"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	admissionv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	IgnoreEmptyLoadBalancerAnnotation = "crdb.io/ignore-empty-lb"
)

func IsDeleted(object Object) error {
	if object == nil {
		return nil
	}

	gvk := object.GetObjectKind().GroupVersionKind()
	key := getObjectKeyOrPanic(object)
	return errors.Newf("object %s/%s is not yet deleted", gvk.Kind, key.Name)
}

// IsReady returns nil once the given K8s object is healthy and ready to
// respond to requests. Each different kind of object has its own custom way of
// doing this, so there's nothing better than using a giant switch and including
// every K8s object that we want to handle.
// See https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#container-probes
// for Kubernetes definition of "ready".
var IsReady = func(object Object) error {
	if object == nil {
		return errors.Wrap(ErrAbortWait, "object does not exist or has been deleted")
	}

	switch t := object.(type) {
	case *corev1.Namespace:
		return IsNamespaceReady(t)
	case *corev1.Service:
		return IsServiceReady(t)
	case *corev1.Endpoints:
		return IsEndpointsReady(t)
	case *appsv1.Deployment:
		return IsDeploymentReady(t)
	case *appsv1.StatefulSet:
		return IsStatefulSetReady(t)
	case *corev1.Pod:
		return IsPodReady(t)
	case *corev1.Node:
		return IsNodeReady(t)
	case *appsv1.DaemonSet:
		return IsDaemonSetReady(t)
	case *apiextv1beta1.CustomResourceDefinition:
		return IsCustomResourceDefinitionBetaReady(t)
	case *apiextv1.CustomResourceDefinition:
		return IsCustomResourceDefinitionReady(t)
	case *policyv1.PodDisruptionBudget:
		return IsPodDisruptionBudgetReady(t)
	case *crdbv1beta1.CrdbCluster:
		return t.IsReady()
	case *crdbv1beta1.CrdbTenant:
		return t.IsReady()
	case *crdbv1beta1.CrdbNode:
		return t.IsReady()
	case *corev1.PersistentVolumeClaim:
		return IsPersistentVolumeClaimReady(t)
	case *unstructured.Unstructured:
		return IsUnstructuredReady(t)
	case *corev1.ServiceAccount, *corev1.ConfigMap, *rbacv1.ClusterRole, *rbacv1.ClusterRoleBinding,
		*rbacv1.Role, *rbacv1.RoleBinding, *batchv1.Job, *batchv1.CronJob, *corev1.Secret, *netv1.NetworkPolicy,
		*admissionv1beta1.ValidatingWebhookConfiguration, *schedulingv1.PriorityClass,
		*admissionv1.ValidatingWebhookConfiguration, *admissionv1.MutatingWebhookConfiguration,
		*storagev1.StorageClass, *storagev1.CSIDriver, *netv1.IngressClass, *netv1.Ingress,
		*crdbv1alpha1.CrdbCluster:
		// These are always trivially ready.
		return nil
	default:
		panic(fmt.Sprintf("unhandled object type: %T", object))
	}
}

// IsNamespaceReady returns nil once the namespace is available for use in the
// system.
func IsNamespaceReady(ns *corev1.Namespace) error {
	if ns.Status.Phase != corev1.NamespaceActive {
		return errors.Newf("namespace not active: %s", ns.Status.Phase)
	}
	return nil
}

// IsServiceReady returns nil once ingress is possible.
func IsServiceReady(svc *corev1.Service) error {
	switch svc.Spec.Type {
	case corev1.ServiceTypeLoadBalancer:
		if svc.Annotations[IgnoreEmptyLoadBalancerAnnotation] == "true" {
			// We ignore checking the state of load balancers with this annotation. This is because on k3d,
			// the load balancer will not be assigned an ingress IP until the target pods are, and in CC we
			// create DB pods after the load balancer comes up.
			return nil
		}

		lb := svc.Status.LoadBalancer
		if len(lb.Ingress) == 0 {
			return errors.New("empty load balancer ingress")
		}
		if lb.Ingress[0].Hostname == "" && lb.Ingress[0].IP == "" {
			return errors.New("no load balancer hostname or IP")
		}
		return nil
	// NB: An empty type is interpreted as ServiceTypeClusterIP by Kubernetes.
	case corev1.ServiceTypeClusterIP, "":
		if svc.Spec.ClusterIP == "" {
			return errors.New("cluster IP is empty")
		}
		return nil
	}
	return nil
}

// IsEndpointsReady returns nil once at least one endpoint is ready in at least
// one subset.
func IsEndpointsReady(points *corev1.Endpoints) error {
	for i := range points.Subsets {
		if len(points.Subsets[i].Addresses) != 0 {
			return nil
		}
	}
	return errors.Newf("no endpoint is ready out of %d subsets", len(points.Subsets))
}

// IsDeploymentReady returns nil once the given deployment has all replicas
// available. This may also return nil if a RollingUpdate strategy was used,
// and the deployment is in the middle of an update.
func IsDeploymentReady(deployment *appsv1.Deployment) error {
	// Generation must be > 0 for any field on the object to be considered reliable.
	if deployment.Generation < 1 {
		return errors.Newf(".Generation must be greater than 0 for Readiness checks")
	}

	if deployment.Generation > deployment.Status.ObservedGeneration {
		return errors.Newf("observed deployment generation %d is lower than expected generation %d", deployment.Status.ObservedGeneration, deployment.Generation)
	}

	if deployment.Status.UnavailableReplicas > 0 {
		return errors.Newf("deployment has %d unavailable replicas", deployment.Status.UnavailableReplicas)
	}

	// This shouldn't happen since unspecified Replicas will be defaulted to 1,
	// based on the documentation.
	if deployment.Spec.Replicas == nil {
		return errors.New("deployment replicas were nil")
	}

	if *deployment.Spec.Replicas != deployment.Status.Replicas {
		return errors.Newf("created replicas %d does not match desired replicas %d", deployment.Status.Replicas, *deployment.Spec.Replicas)
	}

	if *deployment.Spec.Replicas != deployment.Status.AvailableReplicas {
		return errors.Newf("only %d/%d deployment replicas are available", deployment.Status.AvailableReplicas, *deployment.Spec.Replicas)
	}
	return nil
}

// IsStatefulSetReady returns nil once the given stateful set has all replicas
// ready. This may return nil if a RollingUpdate strategy was used, and the
// stateful set is in the middle of an update.
func IsStatefulSetReady(sts *appsv1.StatefulSet) error {
	// Generation must be > 0 for any field on the object to be considered reliable.
	if sts.Generation < 1 {
		return errors.Newf(".Generation must be greater than 0 for Readiness checks")
	}

	if sts.Generation > sts.Status.ObservedGeneration {
		return errors.Newf("observed stateful set generation %d is lower than expected generation %d", sts.Status.ObservedGeneration, sts.Generation)
	}

	// This shouldn't happen since unspecified Replicas will be defaulted to 1,
	// based on the documentation.
	if sts.Spec.Replicas == nil {
		return errors.New("stateful set replicas were nil")
	}

	if *sts.Spec.Replicas != sts.Status.Replicas {
		return errors.Newf("created replicas %d does not match desired replicas %d", sts.Status.Replicas, *sts.Spec.Replicas)
	}

	if *sts.Spec.Replicas != sts.Status.ReadyReplicas {
		return errors.Newf("only %d/%d stateful set replicas are ready", sts.Status.ReadyReplicas, *sts.Spec.Replicas)
	}
	return nil
}

// IsPodReady returns nil once the given pod is ready.
//
// Inspired by k8s.io/kubernetes/pkg/api/v1/pod/util.go
func IsPodReady(pod *corev1.Pod) error {
	status := &pod.Status
	if status.Conditions == nil {
		return errors.New("no pod conditions found")
	}

	for _, condition := range status.Conditions {
		if condition.Type == corev1.PodReady {
			if condition.Status != corev1.ConditionTrue {
				return errors.Newf("pod readiness condition status is %s", condition.Status)
			}
			return nil
		}
	}

	return errors.New("no pod readiness condition found")
}

// IsNodeReady returns nil if the given node is ready, and otherwise an error
// indicating how the node is not ready.
func IsNodeReady(node *corev1.Node) error {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			return nil
		}
	}
	return errors.New("no node status ready condition found")
}

// IsNodeSchedulable returns nil if the given node is ready and can accept new
// pods, and otherwise an error indicating why it cannot. This differs from IsNodeReady
// by also checking taints (NoSchedule/NoExecute) and whether the node is cordoned.
func IsNodeSchedulable(node *corev1.Node) error {
	if err := IsNodeReady(node); err != nil {
		return err
	}
	if node.Spec.Unschedulable {
		return errors.New("node is cordoned")
	}
	for _, taint := range node.Spec.Taints {
		if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
			return errors.Newf("node has %s taint %s", taint.Effect, taint.Key)
		}
	}
	return nil
}

// IsPersistentVolumeClaimReady returns nil if the given pvc is ready, and otherwise an error
// indicating how the pvc is not ready.
func IsPersistentVolumeClaimReady(pvc *corev1.PersistentVolumeClaim) error {
	for _, condition := range pvc.Status.Conditions {
		if condition.Status == corev1.ConditionTrue {
			// There are only two statuses that a pvc can have (besides nil).
			switch condition.Type {
			case corev1.PersistentVolumeClaimResizing,
				corev1.PersistentVolumeClaimFileSystemResizePending:
				return errors.Newf("pvc status not ready due to %s", condition.Type)
			default:
				panic(fmt.Sprintf("unknown condition type %s", condition.Type))
			}
		}
	}
	return nil
}

// IsDaemonSetReady returns nil once the given daemon set is ready.
func IsDaemonSetReady(daemonset *appsv1.DaemonSet) error {
	// Generation must be > 0 for any field on the object to be considered reliable.
	if daemonset.Generation < 1 {
		return errors.Newf(".Generation must be greater than 0 for Readiness checks")
	}
	if daemonset.Generation > daemonset.Status.ObservedGeneration {
		return errors.Newf("observed daemon set generation %d is lower than expected generation %d", daemonset.Status.ObservedGeneration, daemonset.Generation)
	}
	if daemonset.Status.NumberUnavailable > 0 {
		return errors.Newf("%d replica(s) are unavailable", daemonset.Status.NumberUnavailable)
	}
	if daemonset.Status.DesiredNumberScheduled != daemonset.Status.NumberReady {
		return errors.Newf("only %d/%d pods are ready", daemonset.Status.NumberReady, daemonset.Status.DesiredNumberScheduled)
	}
	return nil
}

// IsCustomResourceDefinitionBetaReady returns nil once the CRD has been
// established in the system and all of its names have been accepted.
// NOTE: This is used when a new custom resource definition is applied to the
// K8s cluster, not when *instances* of that CRD are applied.
func IsCustomResourceDefinitionBetaReady(crd *apiextv1beta1.CustomResourceDefinition) error {
	for _, condition := range crd.Status.Conditions {
		if condition.Type == apiextv1beta1.NamesAccepted {
			if condition.Status != apiextv1beta1.ConditionTrue {
				return errors.Newf("crd name was not accepted")
			}
			return nil
		}
	}
	return errors.New("not all crd names have been accepted")
}

// IsCustomResourceDefinitionReady returns nil once the CRD has been
// established in the system and all of its names have been accepted.
// NOTE: This is used when a new custom resource definition is applied to the
// K8s cluster, not when *instances* of that CRD are applied.
func IsCustomResourceDefinitionReady(crd *apiextv1.CustomResourceDefinition) error {
	for _, condition := range crd.Status.Conditions {
		if condition.Type == apiextv1.NamesAccepted {
			if condition.Status != apiextv1.ConditionTrue {
				return errors.Newf("crd name was not accepted")
			}
			return nil
		}
	}
	return errors.New("not all crd names have been accepted")
}

// IsPolicyV1BetaPodDisruptionBudgetReady returns nil once the number of healthy pods
// exceeds the number of pods that must be healthy.
func IsPodDisruptionBudgetReady(budget *policyv1.PodDisruptionBudget) error {
	if budget.Status.CurrentHealthy < budget.Status.DesiredHealthy {
		return errors.Newf(
			"only %d/%d pods are healthy", budget.Status.CurrentHealthy, budget.Status.DesiredHealthy)
	}
	return nil
}

// IsUnstructuredReady returns nil once the given K8s object is healthy and ready to
// respond to requests.
func IsUnstructuredReady(object *unstructured.Unstructured) error {
	gvk := object.GroupVersionKind()
	switch gvk.Group {
	case "crd.projectcalico.org":
		switch gvk.Kind {
		case "BGPConfiguration", "IPPool", "BGPPeer":
			{
				// These calico objects are trivially ready.
				return nil
			}
		default:
			{
				panic(fmt.Sprintf("unknown calico object %s", gvk))
			}
		}
	case "cilium.io":
		switch gvk.Kind {
		case "CiliumNetworkPolicy", "CiliumClusterwideNetworkPolicy":
			{
				// These cilium objects are trivially ready.
				return nil
			}
		default:
			{
				panic(fmt.Sprintf("unknown cilium object %s", gvk))
			}
		}
	case "elbv2.k8s.aws":
		switch gvk.Kind {
		case "IngressClassParams", "TargetGroupBinding":
			{
				// These AWS load balancer objects are trivially ready.
				return nil
			}
		default:
			{
				panic(fmt.Sprintf("unknown AWS load balancer object %s", gvk))
			}
		}
	default:
		panic(fmt.Sprintf("unknown gvk %s for an unstructured object", gvk))
	}
}

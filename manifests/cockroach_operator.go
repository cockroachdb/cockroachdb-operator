package manifests

import (
	"fmt"

	"github.com/cockroachlabs/cockroach-enterprise-operator/buildinfo"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests/internal"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/constants"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// OperatorCertsDir specifies where the operator's TLS certificates can be
	// found on the local file system (directory is mounted from the secret).
	// This must match the certs directory in cockroach-operator.yaml.
	OperatorCertsDir = "/certs"

	// OperatorHealthPort is the port on which the operator's health service
	// listens. This must match the livenessProbe port in cockroach-operator.yaml.
	OperatorHealthPort = 9080

	// OperatorMetricsPort is the port on which the operator's debug service
	// listens.
	OperatorDebugPort = 7080

	// OperatorName is the resource name used for the operator Deployment,
	// PriorityClass, and Service. Use OperatorAppLabel for label selectors.
	OperatorName = "cockroach-operator"

	// OperatorAppLabel is the default "app" label value for operator pods and
	// NetworkPolicy selectors. The actual runtime label for webhook service is
	// detected from the Deployment (see manager.detectOperatorAppLabel).
	// Use this to construct selectors:
	// 	fmt.Sprintf("app=%s", OperatorAppLabel)
	// 	map[string]{"app": OperatorAppLabel}
	OperatorAppLabel = "cockroach-operator"

	// OperatorNamespace is the K8s namespace in which the operator is installed
	// (always the default namespace, since it managed the entire K8s cluster).
	OperatorNamespace = KubeDefault
)

// CockroachOperatorImage returns the default non-test image to be used for the
// CockroachOperator. In release mode, the image tag will be the commit sha of
// the current binary, otherwise the image tag will be "latest".
func CockroachOperatorImage() string {
	return buildinfo.ContainerImage("us-docker.pkg.dev/cockroach-cloud-images/data-plane/cockroach-operator", "main")
}

// CockroachOperatorOptions are parameters for the CockroachOperator manifest.
type CockroachOperatorOptions struct {
	// Image is the full location and name of the image, including its registry,
	// repository, name, and tag. If empty, this defaults to
	// "localhost:5000/cockroach-operator:latest".
	Image string

	// CloudRegion specifies the region in which the operator is deployed.
	CloudRegion string

	// Replicas is an optional value that if provided, overrides the number of
	// operator replicas to deploy described in the yaml file, which is 3.
	Replicas int32

	// ClusterRoles specifies a list of additional cluster roles that will be
	// installed as part of the operator, and linked with the operator's service
	// account.
	ClusterRoles []*rbacv1.ClusterRole

	// PodPriority represents the scheduling priority for operator pods. The default
	// is CriticalPriority (500000000).
	PodPriority int

	// SkipTenantController determines whether or not to skip the tenant controller.
	SkipTenantController bool

	// DisableTenantFinalization determines whether the operator will finalize tenants or not.
	DisableTenantFinalization bool

	// EnableK8sNodeController determines whether or not to enable the k8s node controller.
	EnableK8sNodeController bool

	// EnableMigrationController determines whether or not to enable the migration controller.
	EnableMigrationController bool

	// EnvVars specifies a list of additional environment variables that will be
	// added to the cockroach-operator container.
	EnvVars []corev1.EnvVar

	// WorkloadIdentityAnnotation should be specified if the operator's (k8s) service
	// account should specify the "iam.gke.io/gcp-service-account" annotation. See
	// constants.WorkloadIdentitySAAnnotationKey.
	WorkloadIdentityAnnotation string

	// CpuRequest represents the cpu request as a string, i.e. "500m". If not set,
	// default to the cpu request defined in cockroach-operator.yaml.
	CpuRequest string
}

// CockroachOperator returns the K8s manifest for the operator.
// The manifest cockroach-operator.yaml contains the deployment definition
// for the operator as well as the necessary RBAC resources. The operator
// process creates the CRDs and webhook server.
func CockroachOperator(options CockroachOperatorOptions) []kube.Object {
	resources := operatorRBAC(&options)

	resources = append(resources, internal.DecodeYAML("yaml/cockroach-operator.yaml")...)

	configurePodPriority(resources, &options)
	configureDeployment(resources, &options)
	configureServiceAccount(resources, &options)
	configureWebhookRBAC(resources, &options)

	return resources
}

// configureWebhookRBAC adds namespace-suffixed webhook resourceNames to the
// ClusterRole when the operator is deployed in namespace-scoped mode. The
// static YAML only lists the global names; scoped mode creates webhook configs
// named after the operator's deployment namespace (not WATCH_NAMESPACE) that
// also need get/patch/delete permissions.
func configureWebhookRBAC(res []kube.Object, options *CockroachOperatorOptions) {
	var watchNS string
	for _, env := range options.EnvVars {
		if env.Name == "WATCH_NAMESPACE" {
			watchNS = env.Value
			break
		}
	}
	if watchNS == "" {
		return
	}

	role := findResource(res, "cockroach-operator-role", new(rbacv1.ClusterRole))
	// Webhook configs are named after the operator's deployment namespace,
	// not the watched namespace. The operator always deploys to OperatorNamespace.
	validatingScoped := GetWebhookConfigName(false, OperatorNamespace, false)
	mutatingScoped := GetWebhookConfigName(false, OperatorNamespace, true)

	for i := range role.Rules {
		rule := &role.Rules[i]
		if len(rule.ResourceNames) == 0 {
			continue
		}
		for _, rn := range rule.ResourceNames {
			if rn == ValidatingWebhookConfigNameGlobal {
				rule.ResourceNames = append(rule.ResourceNames, validatingScoped)
			}
			if rn == MutatingWebhookConfigNameGlobal {
				rule.ResourceNames = append(rule.ResourceNames, mutatingScoped)
			}
		}
	}
}

func configureServiceAccount(res []kube.Object, options *CockroachOperatorOptions) {
	if options.WorkloadIdentityAnnotation == "" {
		return
	}

	sa := findResource(res, "cockroach-operator-default", new(corev1.ServiceAccount))
	sa.Annotations = map[string]string{constants.WorkloadIdentitySAAnnotationKey: options.WorkloadIdentityAnnotation}
}

func configurePodPriority(res []kube.Object, options *CockroachOperatorOptions) {
	if options.PodPriority == 0 {
		return
	}

	pc := findResource(res, OperatorName, new(schedulingv1.PriorityClass))
	pc.Value = int32(options.PodPriority)
}

func configureDeployment(res []kube.Object, options *CockroachOperatorOptions) {
	deployment := findResource(res, OperatorName, new(appsv1.Deployment))
	if options.Replicas != 0 {
		deployment.Spec.Replicas = &options.Replicas
	}

	container := &deployment.Spec.Template.Spec.Containers[0]
	if options.Image != "" {
		container.Image = options.Image
	}

	if options.SkipTenantController {
		container.Args = append(container.Args, "-skip-tenant-controller=true")
	}
	if options.DisableTenantFinalization {
		container.Args = append(container.Args, "-disable-tenant-finalization=true")
	}
	if options.EnableK8sNodeController {
		container.Args = append(container.Args, "-enable-k8s-node-controller=true")
	}
	if options.EnableMigrationController {
		container.Args = append(container.Args, "-enable-migration=true")
	}

	if options.CpuRequest != "" {
		container.Resources.Requests[corev1.ResourceCPU] = resource.MustParse(options.CpuRequest)
	}

	container.Env = append(container.Env, options.EnvVars...)
}

func operatorRBAC(options *CockroachOperatorOptions) []kube.Object {
	resources := make([]kube.Object, 0, len(options.ClusterRoles)*2)

	for _, role := range options.ClusterRoles {
		resources = append(
			resources,
			role,
			&rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("%s-binding", role.Name),
					Labels: map[string]string{
						"app": OperatorName,
					},
				},
				RoleRef: rbacv1.RoleRef{
					APIGroup: "rbac.authorization.k8s.io",
					Kind:     "ClusterRole",
					Name:     role.Name,
				},
				Subjects: []rbacv1.Subject{
					{
						Kind:      "ServiceAccount",
						Name:      "cockroach-operator-default",
						Namespace: "default",
					},
				},
			},
		)
	}

	return resources
}

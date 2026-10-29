package manifests

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/values"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	coreDNSImage = "coredns/coredns:1.9.2"
	// ExternalDNSServiceName is the name of the service for CC's multi-region
	// DNS setup. Upon the introduction of add region (AKA PutClusterShape RPC)
	// externalDNSServiceName was changed from "cockroachdb-dns-external" to
	// the current value. We discovered some new Kubernetes tricks that allowed
	// us to decouple the code a touch more but needed a way to introduce
	// changes without downtime for existing clusters. We settled on
	// provisioning a new LB for coreDNS while leaving the legacy LBs in place.
	// We'll need to eventually run a fleet wide rollout of the new CoreDNS
	// configuration and clean up the legacy loadbalancers.
	// See SREOPS-1544.
	ExternalDNSServiceName = "crl-core-dns"
)

// CoreDNS is a DNS server and service discovery mechanism used in some flavors
// of kubernetes. CockroachCloud uses it to implement DNS chaining for our
// multi-region clusters that are already networked together by pulumi. It
// allows pods in namespace Z of cluster A to reference pods in namespace Y of
// cluster B by their FQDN. The Kubernetes manifests here are based on
// https://github.com/coredns/deployment/blob/8fb8871a309cc77baaef27f5b227ec0e546daf0c/kubernetes/coredns.yaml.sed
type CoreDNS struct {
	ClusterRole        *rbacv1.ClusterRole
	ClusterRoleBinding *rbacv1.ClusterRoleBinding
	ConfigMap          *corev1.ConfigMap
	Deployment         *appsv1.Deployment
	Service            *corev1.Service
	ServiceAccount     *corev1.ServiceAccount
}

func (c *CoreDNS) Objects(opts CoreDNSOptions) []kube.Object {
	// Azure deploys CoreDNS for all clusters already, so we don't need
	// everything.
	// https://learn.microsoft.com/en-us/azure/aks/coredns-custom
	if opts.Cloud == values.CloudProviderAzure {
		return []kube.Object{
			c.ConfigMap,
		}
	}

	objs := []kube.Object{
		c.ClusterRole,
		c.ClusterRoleBinding,
		c.ConfigMap,
		c.ServiceAccount,
	}

	if opts.EnableExternalLoadBalancer {
		objs = append(objs, c.Service)
	} else {
		objs = append(objs, kube.Tombstone(c.Service)...)
	}

	// Deployment is intentionally last as a micro optimizing for working
	// with kube.Ctl.Apply. All the resources it depends on will be created
	// before it and we might dodge an additional k8s backoff loop.
	objs = append(objs, c.Deployment)

	return objs
}

type CoreDNSOptions struct {
	// Replicas is the number of CoreDNS replicas to spin up in the deployment.
	// In a single node cluster this value should be 1. 2 is sufficient of all
	// other sizes.
	Replicas int32
	// Cloud is the cloud provider that this CoreDNS instance will be deployed
	// onto. EKS deployments will have additional labels.
	Cloud values.CloudProvider
	// AllK8sClusters describes the DNS setup of all k8s clusters that this
	// cluster is connected with. It's a map of unique cluster domain names
	// to a list of CoreDNS cluster specs. It configures the CoreDNS Corefile to
	// forward requests to the specified instances. The result is called DNS
	// Chaining, which allows pods in cluster A to resolve the IPs of Pods in
	// cluster B.
	AllK8sClusters map[string]CoreDNSClusterOption
	// K8sClusterName is the name of the k8s cluster DNS that we are deploying
	// to. It must be a key in the AllK8sClusters map so the consumers of this
	// struct can get the corresponding CoreDNSClusterOption from AllK8sClusters.
	K8sClusterName string
	// LoadBalancerTargetType specifies the load balancer target type. Only
	// applicable to AWS deployments.
	LoadBalancerTargetType string
	// EnableExternalLoadBalancer specifies whether CoreDNS should provision
	// a k8s service and corresponding external load balander. We use the
	// external load balancer for cross-region network access, so we should
	// only enable this flag for multi-region clusters.
	EnableExternalLoadBalancer bool
}

// NOTE: Many of these fields exist for historical purposes. Ideally, in the
// long term, we only need `Domain` and `IPs` to construct forwarding rules.
// All the others can be replaced with the cluster domain name once we no longer
// connect via "cluster.local" in our applications.
type CoreDNSClusterOption struct {
	// Namespace is the namespace where other clusters can resolve
	// services by querying <service-name>.<namespace>.<svc>.cluster.local.
	Namespace string
	// Domain is the cluster domain where other clusters can resolve
	// by querying <service-name>.<any-namespace>.<svc>.<domain>,
	// as if the cluster has a custom domain, even though
	// technically, the cluster is provisioned with default cluster domain
	// cluster.local.
	//
	// TODO(jaylim-crl): Remove DeprecatedDomain once we've fully transitioned
	// to the new domain.
	Domain           string
	DeprecatedDomain string
	// IPs is a list of routable IPs pointing to CoreDNS load balancers. This
	// will be used to configure the Corefile to forward matching requests
	// based on the suffix of DNS names to the specified instances. The result
	// is called DNS Chaining, which allows pods in cluster A to resolve the
	// DNS names matching the defined rules to IPs in cluster B.
	//
	// NOTE: Hostnames MUST be resolved to IPs as CoreDNS does not support
	// resolving the hostnames of chained instances as of Feb 2021. This is a
	// CoreDNS limitation, and while issue has been raised many times, the
	// developers always say no as it would introduce a circular dependency on
	// CoreDNS itself.
	IPs []string
}

type TargetHealthCheckConfig struct {
	Protocol string
	Port     string
	Path     string
}

// InternalServiceAnnotations returns cloud specific annotations for an
// intra-CockroachCloud cluster service.
func InternalServiceAnnotations(
	cloud values.CloudProvider, targetType string, hc *TargetHealthCheckConfig,
) map[string]string {
	// annotations := ExternalServiceAnnotations(cloud, targetType, hc, false, false)
	annotations := map[string]string{}

	switch cloud {
	case values.CloudProviderAWS:
		if targetType == "IP" {
			annotations["service.beta.kubernetes.io/aws-load-balancer-scheme"] = "internal"
		} else {
			annotations["service.beta.kubernetes.io/aws-load-balancer-internal"] = "true"
		}
	case values.CloudProviderAzure:
		// https://learn.microsoft.com/en-us/azure/aks/internal-lb#create-an-internal-load-balancer
		annotations["service.beta.kubernetes.io/azure-load-balancer-internal"] = "true"
	case values.CloudProviderGCP:
		// Set both annotations to cover K8s < 1.17 and >= 1.17
		// https://cloud.google.com/kubernetes-engine/docs/how-to/internal-load-balancing#global_access
		annotations["cloud.google.com/load-balancer-type"] = "Internal"
		annotations["networking.gke.io/load-balancer-type"] = "Internal"

		// Enable global access on K8s >= 1.17 (and also 1.16).
		// https://cloud.google.com/kubernetes-engine/docs/how-to/internal-load-balancing#global_access
		// https://stackoverflow.com/questions/59680679/gcp-internal-load-balancer-global-access-beta-annotation-does-not-work/59684586#59684586
		annotations["networking.gke.io/internal-load-balancer-allow-global-access"] = "true"
	case values.CloudProviderK3D, values.CloudProviderFake:
		// K3d and Fake don't really matter as we can't really network together
		// the clusters. This case is just handled for testing purposes.
	default:
		panic(fmt.Sprintf("unknown cloud: %q", cloud))
	}

	return annotations
}

func NewCoreDNS(opts CoreDNSOptions) *CoreDNS {
	deployment := coreDNSDeployment(opts.Cloud, opts.Replicas)
	configmap := coreDNSConfigMap(opts.K8sClusterName, opts.AllK8sClusters, opts.Cloud)

	deployment.Spec.Template.Annotations[kube.ConfigsChecksum] = kube.ChecksumConfigs(configmap)

	readinessProbe := deployment.Spec.Template.Spec.Containers[0].ReadinessProbe.ProbeHandler.HTTPGet
	hc := &TargetHealthCheckConfig{
		Path: readinessProbe.Path,
	}

	return &CoreDNS{
		ClusterRole:        coreDNSClusterRole(),
		ClusterRoleBinding: coreDNSClusterRoleBinding(),
		Deployment:         deployment,
		Service:            coreDNSService(opts.Cloud, opts.LoadBalancerTargetType, hc),
		ServiceAccount:     coreDNSServiceAccount(),
		ConfigMap:          configmap,
	}
}

func coreDNSConfigMap(
	thisDomain string, allClusters map[string]CoreDNSClusterOption, cloud values.CloudProvider,
) *corev1.ConfigMap {
	thisCluster := allClusters[thisDomain]
	thisDeprecatedDomain := thisCluster.DeprecatedDomain

	// For AWS and GCP, we directly provide the entire Corefile, which we
	// construct using a strings.Builder.
	//
	// For Azure, we need to provide the Corefile information (both
	// modifications to the default server, and additional servers) via a
	// separate config map, which we also construct.
	// https://learn.microsoft.com/en-us/azure/aks/coredns-custom
	//
	// It's redundant to construct both of these given that we will only
	// use one, but we've opted to optimize for D.R.Y. and readability.
	var data strings.Builder
	dataMap := map[string]string{}

	// Our addition to the typical default server configuration.
	quotedDomain := regexp.QuoteMeta(thisDomain)
	rewriteStr := fmt.Sprintf(`rewrite continue {
		name regex ^(.+)\.%s\.?$ {1}.cluster.local
		answer name ^(.+)\.cluster\.local\.?$ {1}.%s
		answer value ^(.+)\.cluster\.local\.?$ {1}.%s
	}`, quotedDomain, thisDomain, thisDomain)
	quotedDeprecatedDomain := regexp.QuoteMeta(thisDeprecatedDomain)
	if thisDeprecatedDomain != "" {
		rewriteStr += fmt.Sprintf(`
	rewrite continue {
		name regex ^(.+)\.%s\.?$ {1}.cluster.local
		answer name ^(.+)\.cluster\.local\.?$ {1}.%s
		answer value ^(.+)\.cluster\.local\.?$ {1}.%s
	}`, quotedDeprecatedDomain, thisDeprecatedDomain, thisDeprecatedDomain)
	}

	// We provide the rewrite string to Azure as a ".override" map entry.
	// We provide it to AWS and GCP by directly constructing the entire
	// default server string.
	dataMap["rewrite.override"] = rewriteStr
	// "It returns the length of s and a nil error, or else it gets the
	// hose again"
	// - https://golang.org/pkg/strings/#Builder.WriteString
	_, _ = data.WriteString(`.:53 {
	errors
	ready
	health
	` + rewriteStr + `
	kubernetes cluster.local in-addr.arpa ip6.arpa {
		pods insecure
		fallthrough in-addr.arpa ip6.arpa
	}
	prometheus :9153
	forward . /etc/resolv.conf
	cache 30
	loop
	reload
	loadbalance
}
`)

	i := 0
	clusters := make([]string, len(allClusters))
	for clusterDomain := range allClusters {
		clusters[i] = clusterDomain
		i++
	}

	// Sort our cluster domains to ensure our output is stable and deterministic.
	sort.Strings(clusters)

	// Construct a list of forwarding rules for remote CoreDNS servers.
	for _, clusterDomain := range clusters {
		// Skip the current kubernetes cluster.
		if clusterDomain == thisDomain {
			continue
		}

		cluster := allClusters[clusterDomain]
		if len(cluster.IPs) == 0 {
			panic(fmt.Errorf("cluster %s has no IPs", clusterDomain))
		}

		// Sort IPs to ensure determinism.
		sort.Strings(cluster.IPs)

		// For a given remote cluster, configure the local CoreDNS with the IP
		// address(es) that point to the remote cluster's CoreDNS load balancer.
		// The local CoreDNS service will then forward any requests matching the
		// format *.<remote-cluster>.svc.cluster.local or
		// *.monitoring-<remote-cluster>.svc.cluster.local to the remote
		// cluster's DNS service, which will resolve the DNS name to an IP
		// address and return it.
		//
		// TODO(joel): force_tcp was originally included because AWS NLBs did
		// not support UDP traffic. Now that they support UDP, experiment with
		// removing this flag.
		ipAddresses := strings.Join(cluster.IPs, " ")

		serverBlockStr := fmt.Sprintf(`{
	log
	errors
	ready
	cache 30
	forward . %s {
		force_tcp
	}
}`, ipAddresses)

		// The server values which will eventually find their way into the
		// Corefile.
		namespace := cluster.Namespace
		var servers []string
		if namespace != "" {
			servers = append(
				servers,
				fmt.Sprintf("%s.svc.cluster.local", namespace),
				fmt.Sprintf("%s.svc.cluster.local", "monitoring-"+namespace),
				fmt.Sprintf("%s.pod.cluster.local", namespace),
			)
		}
		servers = append(servers, clusterDomain)
		if cluster.DeprecatedDomain != "" {
			servers = append(servers, cluster.DeprecatedDomain)
		}

		for _, server := range servers {
			serverStr := fmt.Sprintf("%s:53 %s\n", server, serverBlockStr)
			// We provide the server configuration to Azure as a ".server" map
			// entry.
			// We provide the server configuration to AWS and GCP as a direct
			// entry in the Corefile string.
			dataMap[fmt.Sprintf("%s.server", server)] = serverStr
			// "It returns the length of s and a nil error, or else it gets the
			// hose again"
			// - https://golang.org/pkg/strings/#Builder.WriteString
			_, _ = data.WriteString(serverStr)
		}
	}

	var name string
	if cloud == values.CloudProviderAzure {
		name = "coredns-custom"
	} else {
		name = "coredns"
	}

	var configMapData map[string]string
	if cloud == values.CloudProviderAzure {
		configMapData = dataMap
	} else {
		configMapData = map[string]string{
			"Corefile": data.String(),
		}
	}

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "kube-system",
		},
		Data: configMapData,
	}
}

func coreDNSService(
	cloud values.CloudProvider, targetType string, hc *TargetHealthCheckConfig,
) *corev1.Service {
	annotations := InternalServiceAnnotations(cloud, targetType, hc)

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: annotations,
			Labels: map[string]string{
				"k8s-app": "kube-dns",
			},
			Name:      ExternalDNSServiceName,
			Namespace: "kube-system",
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{
				{
					Name:       "dns",
					Port:       53,
					Protocol:   "TCP",
					TargetPort: intstr.Parse("53"),
				},
			},
			Selector: map[string]string{
				"k8s-app": "kube-dns",
			},
		},
	}
}

func coreDNSServiceAccount() *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name: "coredns",
		},
	}
}

func coreDNSClusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: "system:coredns",
			Labels: map[string]string{
				"kubernetes.io/bootstrapping": "rbac-defaults",
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"discovery.k8s.io"},
				Resources: []string{
					"endpointslices",
				},
				Verbs: []string{"list", "watch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{
					"endpoints",
					"services",
					"pods",
					"namespaces",
				},
				Verbs: []string{"list", "watch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"nodes"},
				Verbs:     []string{"get"},
			},
		},
	}
}

func coreDNSClusterRoleBinding() *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "system:coredns",
			Annotations: map[string]string{
				"rbac.authorization.kubernetes.io/autoupdate": "true",
			},
			Labels: map[string]string{
				"kubernetes.io/bootstrapping": "rbac-defaults",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "system:coredns",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "coredns",
				Namespace: "kube-system",
			},
		},
	}
}

func coreDNSDeployment(cloud values.CloudProvider, replicas int32) *appsv1.Deployment {
	healthCheckPort := intstr.FromInt(8080)
	maxSurge := intstr.FromInt(1)
	readinessPort := intstr.FromInt(8181)

	memoryLimit, err := resource.ParseQuantity("170Mi")
	if err != nil {
		panic(err)
	}
	memoryRequest, err := resource.ParseQuantity("70Mi")
	if err != nil {
		panic(err)
	}
	cpuRequest, err := resource.ParseQuantity("100m")
	if err != nil {
		panic(err)
	}

	// templateLables are the labels applied to the created pods and the labels
	// used to target pods by the deployment.
	templateLabels := map[string]string{
		"k8s-app": "kube-dns",
	}

	// In EKS, the default coreDNS deployment comes with an additional label.
	// To ensure our deployment looks like it, we copy it onto our deployment.
	if cloud == values.CloudProviderAWS {
		templateLabels["eks.amazonaws.com/component"] = "coredns"
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "coredns",
			Labels: map[string]string{
				// The label `k8s-app: kube-dns` is intentional - this allows CoreDNS
				// to be picked up by the existing kube-dns service, which has a
				// selector on this particular label.
				"k8s-app":            "kube-dns",
				"kubernetes.io/name": "CoreDNS",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(replicas),
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxSurge: &maxSurge,
				},
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: templateLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      templateLabels,
					Annotations: map[string]string{},
				},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						SeccompProfile: &corev1.SeccompProfile{Type: "RuntimeDefault"},
					},
					PriorityClassName:  "system-cluster-critical",
					ServiceAccountName: "coredns",
					Affinity: &corev1.Affinity{
						PodAntiAffinity: &corev1.PodAntiAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
								{
									Weight: 100,
									PodAffinityTerm: corev1.PodAffinityTerm{
										LabelSelector: &metav1.LabelSelector{
											MatchLabels: templateLabels,
										},
										TopologyKey: corev1.LabelHostname,
									},
								},
							},
						},
					},
					Tolerations: []corev1.Toleration{
						{
							Key:      "CriticalAddonsOnly",
							Operator: "Exists",
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "coredns",
							Image:           coreDNSImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: memoryLimit,
								},
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: memoryRequest,
									corev1.ResourceCPU:    cpuRequest,
								},
							},
							Args: []string{
								"-conf", "/etc/coredns/Corefile",
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "config-volume",
									MountPath: "/etc/coredns",
									ReadOnly:  true,
								},
							},
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: 53,
									Name:          "dns",
									Protocol:      corev1.ProtocolUDP,
								},
								{
									ContainerPort: 53,
									Name:          "dns-tcp",
									Protocol:      corev1.ProtocolTCP,
								},
								{
									ContainerPort: 9153,
									Name:          "metrics",
									Protocol:      corev1.ProtocolTCP,
								},
							},
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: new(false),
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{
										"NET_BIND_SERVICE",
									},
									Drop: []corev1.Capability{
										"all",
									},
								},
								ReadOnlyRootFilesystem: new(true),
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path:   "/health",
										Port:   healthCheckPort,
										Scheme: corev1.URISchemeHTTP,
									},
								},
								InitialDelaySeconds: 60,
								TimeoutSeconds:      3,
								SuccessThreshold:    1,
								FailureThreshold:    5,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path:   "/ready",
										Port:   readinessPort,
										Scheme: corev1.URISchemeHTTP,
									},
								},
							},
						},
					},
					DNSPolicy: corev1.DNSDefault,
					Volumes: []corev1.Volume{
						{
							Name: "config-volume",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "coredns",
									},
									Items: []corev1.KeyToPath{
										{
											Key:  "Corefile",
											Path: "Corefile",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

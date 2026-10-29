package kubecreate

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	manifests2 "github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/backoff"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/kube/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/values"
	"golang.org/x/sync/errgroup"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// CoreDNSSpec is the spec for a k8s cluster.
type CoreDNSSpec struct {
	// Replicas is the number of CoreDNS replicas to spin up in the deployment.
	// In a single node cluster this value should be 1. 2 is sufficient of all
	// other sizes.
	Replicas int32
	// Region is the basis for names of two unique namespaces,
	// "<region>", and monitoring-<region>" in the cluster.
	Region string
	// Domain is how services in other namespaces are resolved:
	// <service-name>.<namespace>.svc.<domain>, as if the cluster
	// has a custom domain name.
	//
	// TODO(jaylim-crl): Remove the DeprecatedDomain field once we've fully
	// transitioned to the updated domain.
	Domain           string
	DeprecatedDomain string
	Ctl              *kube.Ctl
	// LoadBalancerTargetType specifies the type for the load balancer targets.
	// Only applicable to AWS multi-region deployments.
	LoadBalancerTargetType string
	// ExternalCluster indicates that the spec is for a cluster that isn't part
	// of the current cluster (e.g. a peered region). This will only be set to
	// true for peered regions.
	ExternalCluster bool
	// LoadBalancerIPs points to the comma-separated ingress hostname or IPs of
	// the CoreDNS load balancer. This is only applicable to peered regions.
	LoadBalancerIPs string
}

// DeployCoreDNSLoadBalancer is used to configure the CoreDNS internal load
// balancers in all given k8s clusters.
//
// NOTE: If the supplied spec is for a single-region cluster, and there are no
// external clusters (i.e. internal peerings), this will be a no-op, and a load
// balancer will not be deployed.
func DeployCoreDNSLoadBalancer(
	ctx context.Context, cloud values.CloudProvider, k8sClusters map[string]*CoreDNSSpec,
) (map[string]string, error) {
	var ingressIPs struct {
		sync.Mutex
		store map[string]string
	}
	ingressIPs.store = make(map[string]string)

	hasExternalClusters := false
	numRegions := 0
	for _, s := range k8sClusters {
		if s.ExternalCluster {
			hasExternalClusters = true
			break
		}
		numRegions++
	}
	// We use the CoreDNS service and its associated external load balancer for
	// cross-region connectivity, so they serve no purpose for single-region
	// clusters, or clusters without internal peerings.
	enableLoadBalancer := numRegions > 1 || hasExternalClusters

	group, gCtx := errgroup.WithContext(ctx)
	for clusterDomain := range k8sClusters {
		cluster := k8sClusters[clusterDomain]

		// External regions do not need a load balancer.
		if cluster.ExternalCluster {
			continue
		}

		// This will create/delete the load balancer accordingly.
		group.Go(func() error {
			ips, err := deployCoreDNSService(gCtx, cluster.Ctl, manifests.CoreDNSOptions{
				Replicas:                   cluster.Replicas,
				Cloud:                      cloud,
				LoadBalancerTargetType:     cluster.LoadBalancerTargetType,
				K8sClusterName:             cluster.Domain,
				EnableExternalLoadBalancer: enableLoadBalancer,
				// AllK8sClusters intentionally omitted here as we don't
				// need them here, and we don't yet have the CoreDNS IPs.
				AllK8sClusters: nil,
			})
			if err != nil {
				return errors.Wrapf(err, "deploying service in cluster %s", cluster.Domain)
			}
			if enableLoadBalancer {
				ingressIPs.Lock()
				defer ingressIPs.Unlock()
				ingressIPs.store[cluster.Domain] = ips
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return ingressIPs.store, nil
}

// DeployCoreDNS replaces kube-dns or upgrades coredns in the given kubernetes
// clusters and configures DNS chaining between them. It is idempotent and may
// be used to add or remove new Kubernetes clusters from the chaining scheme.
//
// NOTE: ingressIPs is a map of k8s cluster domains to ingress IPs of the
// CoreDNS services in the corresponding k8s cluster. If a k8s cluster should
// not have a service, there will be no corresponding entry in the returned map.
func DeployCoreDNS(
	ctx context.Context, cloud values.CloudProvider, k8sClusters map[string]*CoreDNSSpec,
) (map[string]string, error) {
	logging.Infof(ctx, "deploying and chaining coreDNS")

	// Step #1: We deploy just the coreDNS service to all our clusters and grab
	// the LoadBalancer's IP. These IPs are required to chain the coreDNS
	// instances together.
	ingressIPs, err := DeployCoreDNSLoadBalancer(ctx, cloud, k8sClusters)
	if err != nil {
		return nil, err
	}

	var mu sync.Mutex
	coreDNSRegions := make(map[string]manifests.CoreDNSClusterOption)

	// Step #2: Resolve all hostnames. As of Feb 2021, CoreDNS can't resolve
	// hostnames for forwarding purposes. The issue has been raised many times
	// and the dev's always say no as it would introduce a circular dependency
	// on CoreDNS itself.
	group, gCtx := errgroup.WithContext(ctx)
	for clusterDomain := range k8sClusters {
		cluster := k8sClusters[clusterDomain]

		group.Go(func() error {
			var resolvedIPs []string
			var err error

			if cluster.ExternalCluster {
				// If the CoreDNS for peered regions isn't configured properly,
				// we will return an error here. It is assumed that by the time
				// we call DeployCoreDNS on the current cluster, all its
				// external dependencies must already be ready.
				ips := cluster.LoadBalancerIPs
				resolvedIPs, err = resolveServiceIPs(ctx, ips)
				if err != nil {
					return errors.Wrapf(err, "resolving service IPs '%s' in cluster %s", ips, cluster.Domain)
				}
			} else {
				ips, ok := ingressIPs[cluster.Domain]
				if ok {
					resolvedIPs, err = resolveServiceIPs(ctx, ips)
					if err != nil {
						return errors.Wrapf(err, "resolving service IPs '%s' in cluster %s", ips, cluster.Domain)
					}
				}
			}

			mu.Lock()
			defer mu.Unlock()
			coreDNSRegions[cluster.Domain] = manifests.CoreDNSClusterOption{
				// TODO(jaylim-crl): Eventually remove Namespace and
				// DeprecatedDomain.
				Namespace:        cluster.Region,
				DeprecatedDomain: cluster.DeprecatedDomain,
				Domain:           cluster.Domain,
				IPs:              resolvedIPs,
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}

	// Step #3: Deploy the entire coreDNS manifest to all of our clusters, now
	// that we know how to point the instances at each other.
	group, gCtx = errgroup.WithContext(ctx)
	for clusterDomain := range k8sClusters {
		cluster := k8sClusters[clusterDomain]

		// Skip external clusters.
		if cluster.ExternalCluster {
			continue
		}

		group.Go(func() error {
			ctl := cluster.Ctl
			_, hasLoadBalancer := ingressIPs[cluster.Domain]

			if err := deployCoreDNS(gCtx, ctl, manifests.CoreDNSOptions{
				Replicas:                   cluster.Replicas,
				Cloud:                      cloud,
				K8sClusterName:             cluster.Domain,
				LoadBalancerTargetType:     cluster.LoadBalancerTargetType,
				EnableExternalLoadBalancer: hasLoadBalancer,
				AllK8sClusters:             coreDNSRegions,
			}); err != nil {
				return errors.Wrapf(err, "deploying manifest in cluster %s", cluster.Domain)
			}

			// Step #2.b GCP runs kube-dns instead of coreDNS and companion
			// deployment that scales kube-dns up or down to match the cluster
			// size. As we want all of our domain resolutions to go through
			// coreDNS, we scale kube-dns and it's scaler down to 0.
			switch cloud {
			case values.CloudProviderGCP:
				if err := scaleDeploymentToZero(
					gCtx, ctl.WithNamespace(manifests2.KubeSystem), "kube-dns-autoscaler",
				); err != nil {
					return errors.Wrapf(
						err,
						"scaling kube-dns-autoscaler deployment to zero in cluster %s",
						cluster.Domain,
					)
				}

				// Even if we scale the deployment down to 0 and wait for
				// quiescence, the autoscaler pods may still exist and
				// subsequently scale kube-dns back up. To prevent this, we need
				// to explicitly wait for there to be 0 kube-dns-autoscaler
				// pods.
				if err := backoff.Run(ctx, func() error {
					var pods corev1.PodList
					if err := ctl.WithNamespace(manifests2.KubeSystem).List(ctx, &kube.ListOptions{
						LabelSelector: kube.ParseLabelOrDie("k8s-app=kube-dns-autoscaler"),
					}, &pods); err != nil {
						return err
					}
					if len(pods.Items) != 0 {
						return errors.New("kube-dns-autoscaler pods still exist")
					}
					return nil
				}, &backoff.Opts{
					Timeout:         5 * time.Minute,
					InitialInterval: 3 * time.Second,
					MaxInterval:     3 * time.Second,
				}); err != nil {
					return errors.Wrap(err, "waiting for kube-dns-autoscaler pods to go away")
				}

				if err := scaleDeploymentToZero(
					gCtx, ctl.WithNamespace(manifests2.KubeSystem), "kube-dns",
				); err != nil {
					return errors.Wrapf(
						err,
						"scaling kube-dns deployment to zero in cluster %s",
						cluster.Domain,
					)
				}
			case values.CloudProviderAzure:
				// If left undisturbed, it takes some time for Azure's built-in
				// CoreDNS deployment to refresh with the new ConfigMap that we
				// provide to it. In order to ensure that it refreshes
				// immediately, we restart the deployment.
				var coreDNSDeployment appsv1.Deployment
				if err := ctl.WithNamespace(manifests2.KubeSystem).Get(ctx, "coredns", &coreDNSDeployment); err != nil {
					return errors.Wrapf(err, "retrieving Azure's built-in CoreDNS deployment")
				}
				if err := ctl.WithNamespace(manifests2.KubeSystem).
					UpdateWithOptions(
						ctx,
						// This option is necessary because Azure says it is
						// necessary for objects that are built-in.
						kube.UpdateOptions{NoLastAppliedAnnotation: true},
						kube.RestartObject(&coreDNSDeployment)); err != nil {
					return errors.Wrapf(err, "restarting Azure's built-in CoreDNS deployment")
				}
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return ingressIPs, nil
}

// deployCoreDNSService deploys the crl-core-dns service to the given k8s
// cluster, and returns a comma-separated list of ingress IPs for it.
func deployCoreDNSService(
	ctx context.Context, ctl *kube.Ctl, opts manifests.CoreDNSOptions,
) (string, error) {
	ctl = ctl.WithNamespace(manifests2.KubeSystem)
	out := manifests.NewCoreDNS(opts)

	// Destroy the load balancer.
	if !opts.EnableExternalLoadBalancer {
		if err := ctl.ApplyWithOptions(
			ctx, kube.ApplyOptions{Overwrite: true}, kube.Tombstone(out.Service)...,
		); err != nil {
			return "", err
		}
		return "", nil
	}

	svc := out.Service

	// The AWS load balancer controller will automatically add a `finalizer`
	// field to K8s services it manages. We don't want that field to conflict
	// during the ctl patch process, so we set overwrite to true.
	if err := ctl.ApplyWithOptions(
		ctx, kube.ApplyOptions{Overwrite: true}, svc,
	); err != nil {
		return "", err
	}

	var ips []string
	populateIPs := func() error {
		if err := ctl.Get(ctx, svc.Name, svc); err != nil {
			return err
		}
		// If there are multiple ingress values, e.g. k3d, concatenate them
		// together. In our setup, GCP/AWS only exposes one external IP/hostname
		// per service.
		for _, ingress := range svc.Status.LoadBalancer.Ingress {
			if ingress.IP != "" {
				ips = append(ips, ingress.IP)
			}
			if ingress.Hostname != "" {
				ips = append(ips, ingress.Hostname)
			}
		}
		if len(ips) == 0 {
			return errors.AssertionFailedf("CoreDNS service has no ingress IPs")
		}
		return nil
	}

	// ctl.Apply generally waits until the service is ready (i.e. when it has
	// an ingress IP). In K3D, we do not do that because the load balancer will
	// not be assigned an ingress IP until the target pods are ready. In this
	// case, we will have to poll until the ingress IP gets assigned.
	if opts.Cloud == values.CloudProviderK3D {
		if err := backoff.Run(ctx, populateIPs, &backoff.Opts{
			Timeout:         5 * time.Minute,
			InitialInterval: 3 * time.Second,
			MaxInterval:     3 * time.Second,
		}); err != nil {
			return "", err
		}
	} else {
		if err := populateIPs(); err != nil {
			return "", err
		}
	}
	return strings.Join(ips, ","), nil
}

// resolveServiceIPs resolves a comma-separated list of IPs or hostnames into
// a list of IPs. All hostnames must resolve to at least 3 IPs each.
func resolveServiceIPs(ctx context.Context, ipOrHostnames string) ([]string, error) {
	var resolvedIPs []string
	ips := strings.SplitSeq(ipOrHostnames, ",")
	for ip := range ips {
		r, err := resolveServiceIP(ctx, ip)
		if err != nil {
			return nil, errors.Wrapf(err, "resolving %s", ip)
		}
		resolvedIPs = append(resolvedIPs, r...)
	}
	return resolvedIPs, nil
}

// resolveServiceIP resolves the given string into a list of IPs. If a hostname
// is provided, it must resolve to at least 3 IPs.
func resolveServiceIP(ctx context.Context, ipOrHostname string) ([]string, error) {
	if ipOrHostname == "" {
		return nil, errors.AssertionFailedf("ingress IP is empty")
	}

	// Is this an IP? This path will always be taken by non-AWS clusters since
	// Kubernetes services expose IPs instead of hostnames.
	ip := net.ParseIP(ipOrHostname)
	if ip != nil {
		return []string{ipOrHostname}, nil
	}

	// Must be a hostname that resolves to at least 3 IPs.
	var ips []string
	fn := func() error {
		var err error
		ips, err = LookupHost(ctx, ipOrHostname)
		if err != nil {
			return err
		}
		// When cross zone load balancing is properly enabled, DNS queries will
		// return an IP for each AZ in the AWS region. However, propagation of
		// these IPs may not happen at the same time. Given that CC only
		// supports regions with > 3 AZs and we don't _need_ all the IPs for
		// CoreDNS to work as expected, we enforce a requirement for a minimum
		// of 3 IPs as it's "good enough".
		if len(ips) < 3 {
			return errors.Newf("expected at least 3 IPs; got: %v", ips)
		}
		return nil
	}
	if err := backoff.Run(ctx, fn, &backoff.Opts{
		Timeout:         15 * time.Minute,
		InitialInterval: 3 * time.Second,
		MaxInterval:     3 * time.Second,
	}); err != nil {
		return nil, err
	}
	return ips, nil
}

func deployCoreDNS(ctx context.Context, ctl *kube.Ctl, opts manifests.CoreDNSOptions) error {
	out := manifests.NewCoreDNS(opts)
	ctl = ctl.WithNamespace(manifests2.KubeSystem)

	applyOpts := kube.ApplyOptions{
		// We have to provide the overwrite option here as some clusters (EKS) have
		// an existing coreDNS deployment that we're completely overriding.
		Overwrite: true,
		// Wait for IsQuiescent to ensure that the (potentially) new
		// configuration has been picked up and distributed to all new CoreDNS
		// instances.
		WaitForOptions: kube.WaitForOptions{
			DoneFunc: kube.IsQuiescent,
		},
	}

	return ctl.ApplyWithOptions(ctx, applyOpts, out.Objects(opts)...)
}

// Get the number of CoreDNS pods to run in a cluster.
// In a single-node cluster, running multiple CoreDNS pods is a waste of
// resources.
func GetCoreDNSReplicaCount(numCRDBPods int32) int32 {
	if numCRDBPods == 1 {
		return 1
	}
	return 2
}

func scaleDeploymentToZero(ctx context.Context, ctl *kube.Ctl, deploymentName string) error {
	var dep appsv1.Deployment
	if err := ctl.Get(ctx, deploymentName, &dep); err != nil {
		return err
	}

	replicas := int32(0)
	dep.Spec.Replicas = &replicas

	// We wait for the deployment to be quiescent before moving on.
	// Previously, we had just waited for the deployment to be ready,
	// and we had moved onto deleting a deployment before its
	// respective autoscaler had fully been scaled down. This allowed
	// for the autoscaler to scale that deployment back up after we
	// had thought that it had been scaled down.
	kubeOpts := kube.UpdateOptions{
		NoLastAppliedAnnotation: true,
		WaitForOptions: kube.WaitForOptions{
			DoneFunc: kube.IsQuiescent,
		},
	}

	// Addon-manager managed objects will have all their changes reverted if
	// you change them too much. In this case, any changes to the last-applied
	// field result in the object being reployed, so we can't use Apply here.
	// Instead, we use Update to only update the Spec.Replicas which doesn't
	// get automatically reverted.
	return ctl.UpdateWithOptions(ctx, kubeOpts, &dep)
}

// LookupHost is called by resolveServiceIP to resolve a LB hostname into at
// least 3 IP addresses. This can be overridden in tests.
var LookupHost = net.DefaultResolver.LookupHost

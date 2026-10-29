package k3d

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/cockroachdb/errors"
	operatorcloud "github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/cloudinfra"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/configtypes"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/infrastructure"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/infrastructure/k3d"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/infrastructure/k8s"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/kube/kubecreate"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/values"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
)

const alphabet = "abcdefgh"

type cloud struct {
	config *configtypes.Config
	// If true, Create() will try to use an existing k3d cluster.
	reuse       bool
	k3dClusters map[string]*k3d.Cluster
	ctls        map[string]*kube.Ctl
}

type kubernetes struct {
	cloud *cloud
	// name is the k3d cluster's name.
	name string
	// region is the name of the region.
	region string
}

// Create creates the k3d cluster.
func (k *kubernetes) Create(ctx context.Context) (*cloudinfra.CloudResources, error) {
	// We call the `cloud.Create` instead, because we can coordinate
	// the AS number allocation, etc..
	return k.cloud.Create(ctx)
}

// Delete deletes the k3d cluster.
func (k *kubernetes) Delete(ctx context.Context) error {
	return errors.Wrapf(k3d.DeleteCluster(k.name), "delete k3d cluster")
}

// UpdateDNS is a noop.
func (k *kubernetes) UpdateDNS(ctx context.Context) error {
	return nil
}

// UpdateMasterVersion is a noop.
func (k *kubernetes) UpdateMasterVersion(ctx context.Context) error {
	return nil
}

// UpdateVMPoolGroup implements cloudinfra.VMPoolGroupUpdater interface.
// It simply replaces the nodes one at a time in the cluster.
// No other changes will be made to the cluster.
// E.g., k8s versions are not actually updated.
func (k *kubernetes) UpdateVMPoolGroup(
	ctx context.Context,
	opts infrastructure.VMPoolGroupOptions,
	distribution map[infrastructure.AvailabilityZone]int,
) (*cloudinfra.UpdateVMPoolGroupOutput, error) {
	cluster, err := k3d.StartCluster(k.name)
	if err != nil {
		return nil, err
	}
	ctl, err := kube.NewCtl(cluster.RestConfig())
	if err != nil {
		return nil, err
	}
	vmpool := k8s.NewK8sVMPool(ctl.WithNamespace(k.region), cluster, opts)
	for zone, count := range distribution {
		existingNodes, err := cluster.Nodes(ctx, zone)
		if err != nil {
			return nil, errors.Wrapf(err, "get nodes for zone %s", zone)
		}
		for len(existingNodes) > count {
			// Remove extra existing nodes.
			if err := vmpool.RemoveNode(ctx, &existingNodes[len(existingNodes)-1]); err != nil {
				return nil, errors.Wrapf(err, "remove node for zone %s", zone)
			}
			existingNodes = existingNodes[0 : len(existingNodes)-1]
		}
		if len(existingNodes) < count {
			if err := vmpool.Resize(ctx, zone, count); err != nil {
				return nil, errors.Wrapf(err, "add node for zone %s", zone)
			}
		}
		for i := range existingNodes {
			// Replace existing nodes one by one.
			if err := cluster.Resize(ctx, zone, count+1); err != nil {
				return nil, errors.Wrapf(err, "scaling up zone %s", zone)
			}
			if err := vmpool.RemoveNode(ctx, &existingNodes[i]); err != nil {
				return nil, errors.Wrapf(err, "remove node for zone %s", zone)
			}
		}
	}

	return &cloudinfra.UpdateVMPoolGroupOutput{VMGroupIdentifier: "", VMPoolReplaced: true}, nil
}

func (k *kubernetes) UpdateServiceQuota(
	context.Context, *cloudinfra.UpdateServiceQuotaRequest,
) (requestId string, err error) {
	return "", errors.New("operation not supported on k3d")
}

func (k *kubernetes) WaitForServiceQuotaUpdate(
	context.Context, *cloudinfra.WaitForServiceQuotaUpdateRequest,
) error {
	return errors.New("operation not supported on k3d")
}

// func (k *kubernetes) GetComponentReader() cloudinfra.ComponentReader {
// 	panic("no implementation for k3d")
// }

// func (k *kubernetes) InstallCilium(
// 	ctx context.Context, ciliumVersion string, ctl *kube.Ctl, ciliumSigner crypto.CertificateSigner,
// ) error {
// 	if len(k.cloud.config.Regions) == 0 {
// 		return errors.New("cannot find region")
// 	}
// 	cfg := &cloudinfra.CiliumConfig{
// 		Version:   ciliumVersion,
// 		Region:    k.region,
// 		ClusterID: k.cloud.config.Regions[0].KubernetesClusterID,
// 	}
// 	return cloudinfra.InstallCiliumBase(ctx, ctl, ciliumSigner, cfg)
// }

var (
	_ cloudinfra.Cloud = &cloud{}
	// _ cloudinfra.Kubernetes         = &kubernetes{}
	_ cloudinfra.VMPoolGroupUpdater = &kubernetes{}
)

// NewCloud returns a new cloud instance that can be used for interacting
// with a local k3d k8s cluster or clusters. Reuse if true, uses existing
// k3d clusters.
func NewCloud(config *configtypes.Config, reuse bool) *cloud {
	return &cloud{
		config:      config,
		reuse:       reuse,
		k3dClusters: make(map[string]*k3d.Cluster, len(config.Regions)),
		ctls:        map[string]*kube.Ctl{},
	}
}

// Get retrieves all of the global and regional resources for this cluster.
//
// Get implements the cloudinfra.Cloud interface.
func (c *cloud) Get(ctx context.Context) (*cloudinfra.CloudResources, error) {
	return nil, errors.New("unimplemented on k3d")
}

// Create creates the k3d clusters according to the config.
// It also resizes the clusters accordingly if they already exist.
// This function is idempotent.
func (c *cloud) Create(ctx context.Context) (*cloudinfra.CloudResources, error) {
	cloudResources := &cloudinfra.CloudResources{KubeResources: map[string]cloudinfra.KubernetesResources{}}

	var reusingInfra bool
	for i := range c.config.Regions {
		region := &c.config.Regions[i]

		topology := makeK3dTopology(region)
		zones := make([]string, len(topology))
		for i := range topology {
			zones[i] = topology[i].Zone
		}

		if k3dCluster, exists := c.k3dClusters[region.Region]; exists {
			for _, topology := range topology {
				az := infrastructure.AvailabilityZone(topology.Zone)
				if err := k3dCluster.Resize(ctx, az, topology.Replicas); err != nil {
					return nil, errors.Wrapf(err, "resize zone %s for cluster %s", topology.Zone,
						k3dCluster.Name)
				}
			}
			region.Output.AvailabilityZones = zones
			cloudResources.KubeResources[region.Region] = cloudinfra.KubernetesResources{
				APIServer: k3dCluster.RestConfig().Host,
				CA:        string(k3dCluster.RestConfig().CAData),
				Zones:     zones,
			}
			continue
		}

		k3dCluster, reused, err := c.getOrCreateCluster(ctx, region)
		if err != nil {
			return nil, errors.Wrapf(err, "create k3d cluster")
		}

		region.Output = configtypes.RegionOutput{
			AvailabilityZones: zones,
			KubernetesConfig: configtypes.KubernetesConfig{
				CertificateAuthority: k3dCluster.RestConfig().CAData,
				ServerAddress:        k3dCluster.RestConfig().Host,
			},
		}

		c.k3dClusters[region.Region] = k3dCluster
		cloudResources.KubeResources[region.Region] = cloudinfra.KubernetesResources{
			APIServer: region.Output.ServerAddress,
			CA:        string(region.Output.CertificateAuthority),
			Zones:     zones,
		}

		ctl, err := kube.NewCtl(k3dCluster.RestConfig())
		if err != nil {
			return nil, errors.Wrapf(err, "construct ctl for region %s", region.Region)
		}

		c.ctls[region.Region] = ctl

		// Skip the CNI and path provisioner configuration if using an existing
		// cluster. This is not required, done for speed optimization.
		if reused {
			reusingInfra = true
			continue
		}

		if err := c.installCNI(ctx, ctl, region); err != nil {
			return nil, err
		}

		if runtime.GOARCH != "arm64" {
			if err := patchLocalPathProvisioner(ctx, ctl); err != nil {
				return nil, errors.Wrap(err, "patch local-path-provisioner")
			}
		} else {
			// skip the custom local-path-provisioner patch on arm64 because
			// the forked image is amd64-only and crashes on Apple Silicon
			if err := patchLocalPathStorageClass(ctx, ctl); err != nil {
				return nil, errors.Wrap(err, "patch local-path storage class")
			}
		}
	}

	// Skip multi-region setup if test is reusing existing infra since
	// network is already set up. This is a speed optimization.
	if !reusingInfra {
		err := c.setupNetworking(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "setup networking")
		}
	}

	// Switch the current kubectl context to the default K8s cluster. This
	// makes inspecting the cluster with kubectl more convenient during a
	// debugging session.
	k3dClusterName := makeK3dClusterName(&c.config.Regions[0])
	contextName := fmt.Sprintf("k3d-%s", k3dClusterName)
	if err := runKubectl("config", "use-context", contextName); err != nil {
		logging.Errorf(ctx, "%+v", err)
	}

	return cloudResources, nil
}

func (c *cloud) installCNI(
	ctx context.Context, ctl *kube.Ctl, region *configtypes.RegionConfig,
) error {
	// if region.KubernetesCNIPlugin == "CILIUM" {
	// 	kubernetes, err := c.KubernetesForRegion(region.Region)
	// 	if err != nil {
	// 		return errors.Wrapf(err, "could not find region %s", region.Region)
	// 	}
	// 	signer, err := crypto.DeterministicFakeSigner()
	// 	if err != nil {
	// 		return err
	// 	}
	// 	return kubernetes.InstallCilium(ctx, cloudinfra.DefaultCiliumK3dVersion, ctl, signer)
	// }

	// Non-cilium single region cluters use the default k3d CNI (flannel) so
	// no need to install a CNI here.
	if len(c.config.Regions) < 2 {
		return nil
	}

	manifests.RegisterCalicoGVK(ctl.Scheme())
	objects := manifests.K3DCalicoCNI(manifests.K3dClusterBGPConfig{
		AddressAllocation: region.KubernetesClusterID,
	})

	// We don't set `LastApplied` annotation because for some CRDs' size exceeds
	// the k8s's annotation limit.
	// We turn on `NoWait` because kube.IsReady can't handle some of the
	// objects yet, and it's slightly faster to get past this stage.
	// Later ctl.Apply done to this cluster will stall until the network
	// stack is up anyways, so waiting here doesn't gives us anything.
	opts := kube.ApplyOptions{Overwrite: true, NoLastAppliedAnnotation: true, NoWait: true}
	if err := ctl.ApplyWithOptions(ctx, opts, objects...); err != nil {
		return err
	}

	return nil
}

func (c *cloud) getOrCreateCluster(
	ctx context.Context, region *configtypes.RegionConfig,
) (*k3d.Cluster, bool, error) {
	// Use the local Docker registry to fetch images if deploying the
	// operator (because its image is pushed to the local registry).
	// Also, don't use caches if CRL_K3D_NOCACHES is set.
	_, noImageCaches := os.LookupEnv("CRL_K3D_NOCACHES")

	opts := &k3d.ClusterOptions{
		NoImageCaches: !testutils.IntegrationTestsEnabled && noImageCaches,
		// Use random port so that we don't conflict with integration tests, etc..
		APIPort:   k3d.RandomAPIPort,
		CiliumCNI: region.KubernetesCNIPlugin == "CILIUM",
	}

	if len(c.config.Regions) > 1 {
		opts.K3dClusterBGPConfig = &manifests.K3dClusterBGPConfig{
			AddressAllocation: region.KubernetesClusterID,
		}
	}

	topology := makeK3dTopology(region)
	k3dClusterName := makeK3dClusterName(region)

	exists, err := k3d.ClusterExists(k3dClusterName)
	if err != nil {
		return nil, false, err
	}

	var cluster *k3d.Cluster
	if exists {
		if c.reuse {
			// Start the cluster if not yet started.
			cluster, err = k3d.StartCluster(k3dClusterName)
			if err != nil {
				// Could not start cluster, so instead delete and create anew.
				logging.Infof(context.Background(), "got error during start: %v", err)
				if err := k3d.DeleteCluster(k3dClusterName); err != nil {
					return nil, false, err
				}
			}

			// Since we are reusing a cluster, a different CNI may be running
			// than the randomly picked CNI. Determine the running CNI and
			// override the cluster option.
			ctl, err := kube.NewCtl(cluster.RestConfig())
			if err != nil {
				return nil, false, err
			}

			cni, err := operatorcloud.GetNetworkPluginForCluster(ctx, ctl)
			if err != nil {
				return nil, false, err
			}

			if cni == operatorcloud.Cilium {
				region.KubernetesCNIPlugin = "CILIUM"
			} else {
				region.KubernetesCNIPlugin = "CALICO"
			}
		} else {
			// Delete the existing cluster, which might be hanging around from a
			// previous failed test run that wasn't able to clean up.
			if err := k3d.DeleteCluster(k3dClusterName); err != nil {
				return nil, false, err
			}
		}
	}

	reusingCluster := cluster != nil
	if cluster == nil {
		// Create cluster from scratch.
		cluster, err = k3d.NewCluster(k3dClusterName, region.Region, topology, opts)
		if err != nil {
			return nil, false, err
		}
	}

	// Ensure that reusable clusters can't be scaled.
	if c.reuse {
		cluster.SetReadOnly()
	}

	return cluster, reusingCluster, nil
}

// setupNetworking ensures there is cross-k3d-cluster network connectivity and
// service discovery.
func (c *cloud) setupNetworking(ctx context.Context) error {
	if len(c.config.Regions) < 2 {
		logging.Infof(ctx, "single region cluster: skipping BGP peering")
		return nil
	}

	netconfig := manifests.K3dCalicoBGPPeeringOptions{
		ClusterConfig: map[string]manifests.K3dClusterBGPConfig{},
	}
	// Mark the master nodes as our bgp edge. These nodes will act as our bgp
	// peers.
	for _, region := range c.config.Regions {
		config := netconfig.ClusterConfig[region.Region]
		config.AddressAllocation = region.KubernetesClusterID

		var nodes corev1.NodeList
		ctl := c.ctls[region.Region]
		err := ctl.List(ctx, &kube.ListOptions{}, &nodes)
		if err != nil {
			return errors.Wrapf(err, "list nodes in %s", region)
		}
		for i := range nodes.Items {
			node := &nodes.Items[i]
			if node.Labels["node-role.kubernetes.io/master"] == "true" {
				node.Annotations["projectcalico.org/labels"] = `{"edge":"true"}`
				if err := ctl.Update(ctx, node); err != nil {
					return errors.Wrapf(err, "annotate node for calico edge")
				}
				for _, nodeAddress := range node.Status.Addresses {
					if nodeAddress.Type == corev1.NodeInternalIP {
						config.PeeringNodes = append(config.PeeringNodes, nodeAddress.Address)
					}
				}
			}
		}
		netconfig.ClusterConfig[region.Region] = config
	}
	objectsByRegion := manifests.K3dCalicoBGPPeeringObjects(netconfig)
	for region, ctl := range c.ctls {
		manifests.RegisterCalicoGVK(ctl.Scheme())
		err := ctl.Apply(context.Background(), objectsByRegion[region]...)
		if err != nil {
			return errors.Wrapf(err, "apply calico objects in region %s", region)
		}
	}

	clusters := map[string]*kubecreate.CoreDNSSpec{}
	for region := range c.k3dClusters {
		domain := GetClusterDomain(c.config.Cluster.ShortID, region)
		clusters[domain] = &kubecreate.CoreDNSSpec{
			Replicas: 1,
			Region:   region,
			Domain:   domain,
			Ctl:      c.ctls[region],
		}
	}
	if _, err := kubecreate.DeployCoreDNS(ctx, values.CloudProviderK3D, clusters); err != nil {
		return err
	}
	return nil
}

// GetClusterDomain returns the k8s cluster domain for the given region name
// in the EnvConfig.Regions map.
func GetClusterDomain(shortID, regionName string) string {
	return fmt.Sprintf("cluster.%s.%s", shortID, regionName)
}

func makeK3dTopology(region *configtypes.RegionConfig) []k3d.Topology {
	const zonesAvailable = 3
	zones := min(region.NodeCount, zonesAvailable)
	replicasPerZone := region.NodeCount / zonesAvailable
	topology := make([]k3d.Topology, zones)
	for i := range zones {
		topology[i].Zone = region.Region + alphabet[i:i+1]
		topology[i].Replicas = replicasPerZone
		if replicasPerZone*zonesAvailable+i < region.NodeCount {
			topology[i].Replicas += 1
		}
	}
	return topology
}

func runKubectl(args ...string) error {
	cmd := exec.Command("kubectl", args...)
	buf := bytes.NewBuffer(nil)

	cmd.Stderr = buf

	return errors.Wrapf(cmd.Run(), "%s", buf.String())
}

func makeK3dClusterName(region *configtypes.RegionConfig) string {
	return region.Cluster.Name + "-" + region.Region
}

// patchLocalPathProvisioner updates the local path disk provisioner to
// use a shared file system path to host pvcs. This is to simulate a typical cloud
// environment where PVCs are backed by networked disks, and are accessible by
// nodes on the same network.
// Without this patch, PVCs can only be accessed by pods on the same node. This
// prevents us from testing certain operations like replacing k8s nodes: pods using
// PVs that are on the replaced node will no longer be schedulable.
//
// How this works:
// We configure k3d cluster to use a docker volume (k3d-cloud-<cluster-name>) to store
// PVCs, and mapped it to /var/lib/rancher/k3s/storage on each k8s node container.
// (See [NewCluster]'s usage of `--volume` flag.)
// We configure local-path-provisioner, the default pv provisioner of k3d,
// to use `/var/lib/rancher/k3s/storage` as a shared file system.
func patchLocalPathProvisioner(ctx context.Context, ctl *kube.Ctl) error {
	ctl = ctl.WithNamespace("kube-system")
	var config corev1.ConfigMap
	if err := ctl.Get(ctx, "local-path-config", &config); err != nil {
		return err
	}
	config.Data["config.json"] = `{"sharedFileSystemPath":"/var/lib/rancher/k3s/storage"}`
	if err := ctl.Apply(ctx, &config); err != nil {
		return err
	}

	var deployment appsv1.Deployment
	if err := ctl.Get(ctx, "local-path-provisioner", &deployment); err != nil {
		return err
	}
	var pods corev1.PodList
	if err := ctl.List(ctx, &kube.ListOptions{LabelSelector: kube.ParseLabelOrDie("app=local-path-provisioner")}, &pods); err != nil {
		return err
	}
	// This image is built from this fork:
	// https://github.com/rancher/local-path-provisioner/compare/master...meridional:local-path-provisioner:add_zone_label_to_sharedFS
	// It adds the zone label to the PVs to better simulate cloud environment.
	deployment.Spec.Template.Spec.Containers[0].Image = "us-docker.pkg.dev/cockroach-cloud-images/development/local-path-provisioner:794db8a-amd64"
	if err := ctl.ApplyWithOptions(ctx, kube.ApplyOptions{WaitForOptions: kube.WaitForOptions{DoneFunc: kube.IsQuiescent}}, &deployment); err != nil {
		return err
	}
	for i := range pods.Items {
		if err := ctl.WaitForWithOptions(ctx, kube.WaitForOptions{
			DoneFunc: func(obj kube.Object) error {
				if obj == nil {
					return nil
				}
				return errors.Newf("pod %s still not deleted", pods.Items[i].Name)
			},
		}, &pods.Items[i]); err != nil {
			return err
		}
	}
	return patchLocalPathStorageClass(ctx, ctl)
}

// patchLocalPathStorageClass creates a new local-path storage class that allows
// disk expansion, and sets it as the default storage class.
func patchLocalPathStorageClass(ctx context.Context, ctl *kube.Ctl) error {
	var localPath storagev1.StorageClass
	if err := ctl.Get(ctx, "local-path", &localPath); err != nil {
		return errors.Wrap(err, "get local-path storage class")
	}
	expandableLocalPath := localPath.DeepCopy()
	t := true
	expandableLocalPath.AllowVolumeExpansion = &t
	expandableLocalPath.UID = ""
	expandableLocalPath.ResourceVersion = ""
	expandableLocalPath.Name = "expandable-local-path"
	// Mark original localpath as non-default.
	// After applying, the new expandableLocalPath will be the new
	// default.
	localPath.Annotations["storageclass.kubernetes.io/is-default-class"] = "false"

	if err := ctl.ApplyWithOptions(ctx, kube.ApplyOptions{
		Overwrite:               true,
		NoLastAppliedAnnotation: true,
	}, &localPath, expandableLocalPath); err != nil {
		return errors.Wrap(err, "update local-path storage classes")
	}
	return nil
}

// Delete deletes the k3d cluster(s).
func (c *cloud) Delete(ctx context.Context) error {
	var firstErr error
	for _, cluster := range c.k3dClusters {
		if err := k3d.DeleteCluster(cluster.Name); err != nil {
			logging.Errorf(ctx, "error deleting cluster %s: %+v", cluster.Name, err)
			if firstErr == nil {
				firstErr = errors.Wrapf(err, "delete cluster %s", cluster.Name)
			}
		}
	}
	return firstErr
}

// KubernetesForRegion returns the kubernetes instance for region.
func (c *cloud) KubernetesForRegion(s string) (cloudinfra.Kubernetes, error) {
	if len(c.config.Regions) == 0 {
		return nil, errors.Newf("cannot find region %s", s)
	}
	var region *configtypes.RegionConfig
	for i := range c.config.Regions {
		if c.config.Regions[i].Region == s {
			region = &c.config.Regions[i]
			break
		}
	}
	if region == nil {
		return nil, errors.Newf("cannot find region %s", s)
	}

	return &kubernetes{cloud: c, name: makeK3dClusterName(region), region: region.Region}, nil
}

// Update updates the k8s clusters according to the config.
func (c *cloud) Update(ctx context.Context) error {
	// Update will handle the resizing.
	_, err := c.Create(ctx)
	return err
}

// UpdateBase is a noop.
func (c *cloud) UpdateBase(ctx context.Context) error { return nil }

// UpdateDNS is a noop.
func (c *cloud) UpdateDNS(ctx context.Context, s ...string) error {
	return nil
}

// UpdateLogging is a noop.
func (c *cloud) UpdateLogging(ctx context.Context) error {
	return nil
}

// UpdateInSitu is a noop.
func (c *cloud) UpdateInSitu(ctx context.Context) error {
	return nil
}

// UpdateInternalPeerings is a noop.
func (c *cloud) UpdateInternalPeerings(ctx context.Context) error {
	return nil
}

// WithConfig returns a new cloud instance with the provided config.
func (c *cloud) WithConfig(config *configtypes.Config) cloudinfra.Cloud {
	return NewCloud(config, c.reuse)
}

// Ctl returns a ctl connection to the given region. This method implements
// cloudinfra.CtlFactory.
func (c *cloud) Ctl(region string, _ *configtypes.KubernetesConfig) (*kube.Ctl, error) {
	cluster, ok := c.k3dClusters[region]
	if !ok {
		return nil, errors.Newf("no cluster for region %s", region)
	}

	return kube.NewCtl(cluster.RestConfig())
}

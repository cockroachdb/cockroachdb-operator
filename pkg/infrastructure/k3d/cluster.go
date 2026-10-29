package k3d

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/backoff"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/constants"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/infrastructure"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/tracing"
	"github.com/gofrs/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

const (
	K3D = "k3d"
	// RandomAPIPort is used as a value for ClusterOptions.APIPort when
	// creating the cluster. A random port will be chosen.
	// 65536 is not a valid port number; we don't use numbers like -1 here,
	// because APIPort field is an unsigned integer.
	RandomAPIPort            = 65536
	kubernetesVersionDefault = "v1.32.13-k3s1"
	localRegistryNetwork     = "k3d-local"
)

// Cluster encapsulates the information needed to interact with a K3D cluster -
// creating it, adding nodes to it, managing it.
type Cluster struct {
	Name string

	kubernetesVersion string
	region            string
	zones             []infrastructure.AvailabilityZone
	config            *rest.Config
	clientSet         kubernetes.Interface
	readOnly          bool
	ciliumCNI         bool
}

// Topology specifies the (simulated) locations of nodes in the K3D cluster, as
// if they were located in availability zones in a Cloud provider region.
type Topology struct {
	Zone     string
	Replicas int
}

// ClusterOptions specify the details of how a K3D cluster will be created.
type ClusterOptions struct {
	// NoImageCaches disables Docker image caches and the local registry. By
	// default, these are used in order to enable faster local development,
	// where images do not need to get pulled from remote servers every time we
	// create new clusters.
	NoImageCaches bool

	// KubernetesVersion is the version of K8s to use when creating the cluster.
	KubernetesVersion string

	// K3dClusterBGPConfig describes how to setup networking for the
	// cluster for connecting with other k8s clusters.
	// It is optional.
	// For single region clusters, leave this as nil: it will greatly speed
	// up the creation time.
	*manifests.K3dClusterBGPConfig

	// APIPort is the localhost port the K3D API Server will be exposed on,
	// defaults to 5880.
	APIPort uint32

	// CiliumCNI if true runs Cilium as the CNI in the cluster. Currently only
	// supported in single-region clusters.
	CiliumCNI bool
}

// MultiRegionNetworkConfig describes how to setup networking for the
// cluster for connecting with other k8s clusters.
type MultiRegionNetworkConfig struct {
	// PodCIDR is the CIDR for pod IPs.
	PodCIDR string

	// ServiceCIDR is the CIDR for service IPs.
	ServiceCIDR string

	// ASNumber is the AS number for the cluster.
	ASNumber uint32
}

// NewCluster constructs a new local K3D cluster with the given name, cloud
// region and zone topology, and then switches the current K8s context to it. If
// a cluster of that name already exists, NewCluster will delete it and create a
// new one.
// NOTE: This calls "k3d cluster create".
func NewCluster(
	name, region string, topologies []Topology, options *ClusterOptions,
) (*Cluster, error) {
	ctx := context.Background()
	if options == nil {
		options = &ClusterOptions{}
	}

	multiregion := options.K3dClusterBGPConfig != nil
	if options.APIPort == 0 {
		options.APIPort = 5880
	}

	size := 0
	zones := make([]infrastructure.AvailabilityZone, len(topologies))
	for i, t := range topologies {
		size += t.Replicas
		zones[i] = infrastructure.AvailabilityZone(t.Zone)
	}
	if size == 0 || len(zones) == 0 {
		return nil, errors.Newf("clusters must have at least 1 zone and 1 node")
	}

	kubeVersion := options.KubernetesVersion
	if kubeVersion == "" {
		kubeVersion = kubernetesVersionDefault
	}

	// Delete any existing cluster.
	if exists, err := ClusterExists(name); err != nil {
		return nil, err
	} else if exists {
		if !errors.Is(err, DeleteCluster(name)) {
			return nil, err
		}
	}

	logging.Infof(ctx, "creating k3d cluster '%s'", name)

	// Create a docker volume to be used as a shared "NFS"
	// for all k8s nodes.
	// See comment for [patchLocalPathProvisioner] for more context.
	if err := exec.Command("docker", "volume", "create", getTempDockerVolumeName(name)).Run(); err != nil {
		logging.Errorf(ctx, "create docker volume: %v", err)
		return nil, errors.WithStack(err)
	}

	createArgs := []string{
		"cluster",
		"create",
		name,
		"--wait",
		"--timeout=5m",
		fmt.Sprintf("--image=%s", getRancherK3sImageName(kubeVersion)),
		// Always use local registry network so that multi region clusters
		// can share the same network.
		"--network",
		localRegistryNetwork,
		"--volume",
		fmt.Sprintf("%s:/var/lib/rancher/k3s/storage@all", getTempDockerVolumeName(name)),
		"--k3s-arg=--tls-san=host.k3d.internal@server:*",
		// Raise the node container's open-file limit. The operator and CRDB open many
		// files such as informer watchers and per-node connections. The default Docker
		// limit surfaces as "too many open files".
		"--runtime-ulimit=nofile=1048576:1048576",
	}

	if options.CiliumCNI {
		// Disable flannel, which is the default CNI in k3d clusters. Disable
		// network policy enforcement since that is Cilium's responsibility.
		createArgs = append(createArgs,
			"--k3s-arg=--flannel-backend=none@server:*",
			"--k3s-arg=--disable-network-policy@all",
		)
	}

	if multiregion {
		createArgs = append(createArgs,
			"--k3s-arg=--flannel-backend=none@server:*",
			"--k3s-arg=--disable-network-policy@all",
			fmt.Sprintf("--k3s-arg=--cluster-cidr=%s@server:*", options.GetPodCIDR()),
			fmt.Sprintf("--k3s-arg=--service-cidr=%s@server:*", options.GetServiceCIDR()),
			"--k3s-arg=--prefer-bundled-bin@server:*",
		)
	}

	if options.APIPort != RandomAPIPort {
		// If we do not select an API Server port, K3D will pick one from the
		// empeheral port range. There is a window of time between when K3D
		// selects a port and when it binds the port. If something that uses
		// ephemeral ports races with K3D cluster creation, it may steal the
		// port selected for the API server.
		createArgs = append(createArgs, fmt.Sprintf("--api-port=%d", options.APIPort))
	}

	if !options.NoImageCaches {
		_, fileName, _, _ := runtime.Caller(0)
		registryFilePath := path.Join(path.Dir(fileName), "registries.yaml")
		createArgs = append(createArgs,
			"--registry-config",
			registryFilePath,
		)
	}

	if err := runK3D(createArgs); err != nil {
		dumpK3DContainerLogs(name)
		return nil, err
	}

	cluster, err := newCluster(name)
	if err != nil {
		return nil, err
	}
	cluster.region = region
	cluster.zones = zones
	cluster.kubernetesVersion = kubeVersion
	cluster.ciliumCNI = options.CiliumCNI

	// Prepare the master node for cilium if necessary.
	master := fmt.Sprintf("k3d-%s-server-0", name)
	if err := cluster.configureCiliumMounts(ctx, master); err != nil {
		return nil, err
	}

	for _, t := range topologies {
		for range t.Replicas {
			if err := cluster.addNode(t.Zone); err != nil {
				_ = cluster.Close()
				return nil, err
			}
		}
	}

	// Cordon the master node so this looks closer to a managed k8s cluster.
	logging.Info(ctx, "cordoning server node in k3d cluster")

	_, err = cluster.clientSet.CoreV1().Nodes().Patch(
		ctx,
		master,
		types.JSONPatchType,
		[]byte(`[{"op": "add", "path": "/spec/unschedulable", "value": true}]`),
		metav1.PatchOptions{},
	)
	if err != nil {
		_ = cluster.Close()
		return nil, errors.Wrap(err, "patching node")
	}

	ctl, err := kube.NewCtl(cluster.RestConfig())
	if err != nil {
		_ = cluster.Close()
		return nil, err
	}

	if err := ctl.ApplyWithOptions(ctx,
		kube.ApplyOptions{Overwrite: true, NoWait: true},
		manifests.FullAccessRBAC("kube-system")...); err != nil {
		return nil, err
	}

	logging.Infof(ctx, "created k3d cluster '%s'", name)

	return cluster, nil
}

// ClusterExists returns true if there is a K3D cluster with the given name.
func ClusterExists(name string) (bool, error) {
	allNames, err := GetClusterNames()
	if err != nil {
		return false, errors.Wrap(err, "failed to get cluster names")
	}
	if slices.Contains(allNames, name) {
		return true, nil
	}
	return false, nil
}

// GetClusterNames returns the names of all K3D clusters that currently exist.
// NOTE: This calls "k3d cluster get".
func GetClusterNames() ([]string, error) {
	buf := bytes.NewBuffer(nil)
	cmd := exec.Command(K3D, "cluster", "get", "--no-headers")
	cmd.Stderr = buf
	cmd.Stdout = buf

	err := cmd.Run()
	if err != nil {
		return nil, errors.Wrapf(err, "%s", buf.String())
	}

	var clusters []string
	for line := range strings.SplitSeq(buf.String(), "\n") {
		if line != "" {
			tokens := strings.Fields(line)
			clusters = append(clusters, tokens[0])
		}
	}
	return clusters, nil
}

// StartCluster ensures that an existing K8s cluster with the given name has
// been started. If no such cluster exists, it returns an error.
// NOTE: This calls "k3d cluster start".
func StartCluster(name string) (*Cluster, error) {
	ctx := context.Background()
	cluster, err := newCluster(name)
	if err != nil {
		return nil, err
	}

	// Ensure cluster is started.
	startArgs := []string{
		"cluster",
		"start",
		name,
	}
	if err := runK3D(startArgs); err != nil {
		return nil, err
	}

	// Get nodes in existing K8s cluster. Try this multiple times, as it can
	// take some time for the just-started cluster to be ready.
	var nodes *corev1.NodeList
	err = wait.Poll(500*time.Millisecond, 30*time.Second, func() (done bool, err error) {
		nodes, err = cluster.clientSet.CoreV1().Nodes().List(ctx, v1.ListOptions{
			LabelSelector: "failure-domain.beta.kubernetes.io/zone",
			FieldSelector: "spec.unschedulable=false",
		})
		if err != nil {
			if strings.Contains(err.Error(), "the server is currently unable to handle the request") {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	// Fill in Kubernetes server version.
	version, err := cluster.clientSet.Discovery().ServerVersion()
	if err != nil {
		return nil, err
	}
	cluster.kubernetesVersion = version.GitVersion
	if strings.HasSuffix(cluster.kubernetesVersion, "+k3s1") {
		// For some reason, the K8s version comes back with a "+" symbol rather
		// than a "-" symbol, so map it back to a "-".
		cluster.kubernetesVersion = strings.ReplaceAll(cluster.kubernetesVersion, "+k3s1", "-k3s1")
	}

	// Get unique set of zones in which those nodes run.
	zones := make(map[infrastructure.AvailabilityZone]int)
	for _, node := range nodes.Items {
		for name, value := range node.Labels {
			if name == "failure-domain.beta.kubernetes.io/region" {
				cluster.region = value
			}
			if name == "failure-domain.beta.kubernetes.io/zone" {
				zones[infrastructure.AvailabilityZone(value)]++
			}
		}
	}
	for zone := range zones {
		cluster.zones = append(cluster.zones, zone)
	}

	return cluster, nil
}

// StopCluster stops an existing K8s cluster with the given name. If the cluster
// does not exist, it returns an error.
// NOTE: This calls "k3d cluster stop".
func StopCluster(name string) error {
	stopArgs := []string{
		"cluster",
		"stop",
		name,
	}
	return runK3D(stopArgs)
}

// DeleteCluster destroys an existing K8s cluster with the given name. If the
// cluster does not exist, this is a no-op.
// NOTE: This calls "k3d cluster delete".
func DeleteCluster(name string) error {
	// If cluster doesn't exist, then this a no-op.
	if exists, err := ClusterExists(name); err != nil || !exists {
		return err
	}

	// Delete the cluster.
	deleteArgs := []string{
		"cluster",
		"delete",
		name,
	}
	if err := runK3D(deleteArgs); err != nil {
		return err
	}

	// Wait for cluster to be gone.
	return backoff.Run(context.Background(), func() error {
		if exists, err := ClusterExists(name); err != nil {
			return err
		} else if exists {
			return errors.Errorf("waiting for cluster '%s' to be deleted", name)
		}

		if err := exec.Command("docker", "volume", "rm", getTempDockerVolumeName(name)).Run(); err != nil {
			logging.Errorf(
				context.Background(),
				"cannot remove docker volume %s %v",
				getTempDockerVolumeName(name),
				err,
			)
		}
		return nil
	}, &backoff.Opts{Timeout: 60 * time.Second, MaxInterval: time.Second})
}

func getTempDockerVolumeName(name string) string {
	return "k3d-cloud-" + name
}

// SetReadOnly prohibits the cluster from being resized after its initial
// creation. This is used to detect cases where tests that perform scaling are
// incorrectly using shared clusters, since that can interfere with other tests
// that rely upon the original shared cluster configuration.
func (c *Cluster) SetReadOnly() {
	c.readOnly = true
}

// AvailabilityZones implements the infrastructure.VMPoolGroup interface.
func (c *Cluster) AvailabilityZones(
	ctx context.Context,
) ([]infrastructure.AvailabilityZone, error) {
	return c.zones, nil
}

// AvailabilityZones implements the infrastructure.VMPoolGroup interface.
func (c *Cluster) Nodes(
	ctx context.Context, zone infrastructure.AvailabilityZone,
) ([]infrastructure.VM, error) {
	nodes, err := c.clientSet.CoreV1().Nodes().List(ctx, v1.ListOptions{
		LabelSelector: fmt.Sprintf("failure-domain.beta.kubernetes.io/zone=%s", zone),
		FieldSelector: "spec.unschedulable=false",
	})
	if err != nil {
		return nil, err
	}

	vms := make([]infrastructure.VM, len(nodes.Items))
	for i, node := range nodes.Items {
		vms[i].Name = node.Name
		vms[i].AvailabilityZone = zone
	}

	return vms, nil
}

// RemoveNode implements the infrastructure.VMPoolGroup interface.
func (c *Cluster) RemoveNode(ctx context.Context, vm *infrastructure.VM) error {
	if c.readOnly {
		return fmt.Errorf("reusable cluster %s cannot be resized", c.Name)
	}

	_, span := tracing.StartSpan(ctx, "Cluster.RemoveNode")
	defer span.End()

	grace := int64(0)
	propPolicy := v1.DeletePropagationForeground

	if err := c.clientSet.CoreV1().Nodes().Delete(ctx, vm.Name, v1.DeleteOptions{
		GracePeriodSeconds: &grace,
		PropagationPolicy:  &propPolicy,
	}); err != nil && !kubeerrors.IsNotFound(err) {
		return err
	}

	return runK3D([]string{"node", "delete", vm.Name})
}

// StopNode stops the k8s node.
func (c *Cluster) StopNode(ctx context.Context, vm *infrastructure.VM) error {
	return runK3D([]string{"node", "stop", vm.Name})
}

// StartNode starts the k8s node.
func (c *Cluster) StartNode(ctx context.Context, vm *infrastructure.VM) error {
	if err := runK3D([]string{"node", "start", vm.Name}); err != nil {
		return errors.Wrap(err, "start node")
	}
	ctl, err := kube.NewCtl(c.config)
	if err != nil {
		return err
	}
	var node corev1.Node
	if err = ctl.Get(ctx, vm.Name, &node); err != nil {
		return err
	}
	return ctl.WaitFor(ctx, kube.IsReady, &node)
}

// State implements the infrastructure.VMPoolGroup interface.
func (c *Cluster) State(
	ctx context.Context, zone infrastructure.AvailabilityZone,
) (infrastructure.State, error) {
	ctx, span := tracing.StartSpan(ctx, "Cluster.State")
	defer span.End()

	nodes, err := c.Nodes(ctx, zone)
	if err != nil {
		return infrastructure.State{}, err
	}

	return infrastructure.State{
		DesiredNodes: len(nodes),
	}, nil
}

// Resize implements the infrastructure.VMPoolGroup interface.
func (c *Cluster) Resize(
	ctx context.Context, zone infrastructure.AvailabilityZone, size int,
) error {
	if c.readOnly {
		return fmt.Errorf("reusable cluster %s cannot be resized", c.Name)
	}

	ctx, span := tracing.StartSpan(ctx, "Cluster.Resize")
	defer span.End()

	nodes, err := c.Nodes(ctx, zone)
	if err != nil {
		return err
	}

	for i := len(nodes); i < size; i++ {
		if err := c.addNode(string(zone)); err != nil {
			return err
		}
	}

	nodes, err = c.Nodes(ctx, zone)
	if err != nil {
		return err
	}

	for _, vm := range nodes[size:] {
		if err := c.RemoveNode(ctx, &vm); err != nil {
			return err
		}
	}

	return nil
}

func (c *Cluster) WaitUntilReady(ctx context.Context) error {
	return nil
}

// Close cleans up the K3D cluster by deleting it.
func (c *Cluster) Close() error {
	return DeleteCluster(c.Name)
}

// RestConfig returns the configuration needed to connect to the K8s cluster.
func (c *Cluster) RestConfig() *rest.Config {
	return c.config
}

// ClientSet returns the strongly-typed client to the K8s cluster.
func (c *Cluster) ClientSet() kubernetes.Interface {
	return c.clientSet
}

// ImportImage() takes an image stored in the host's local image registry and
// copies it into the cluster.
//
// At the moment there are no cluster image caches for K3D v3.x, so when a
// cluster is created, it will need to freshly pull images for all pods in the
// cluster.
func (c *Cluster) ImportImage(imageName string) error {
	return runK3D([]string{
		"image",
		"import",
		imageName,
		fmt.Sprintf("--cluster=%s", c.Name),
	})
}

// addNode creates a new K3D node in the given availability zone.
func (c *Cluster) addNode(zone string) error {
	ctx := context.Background()
	var name string

	// Retry node creation with backoff. The k3s server may not be ready to
	// accept agent registrations immediately after a previous node was added,
	// causing the agent container to crash-loop. Retrying gives the server
	// time to stabilize.
	err := backoff.Run(ctx, func() error {
		name = uuid.Must(uuid.NewV4()).String()
		logging.Infof(ctx, "adding node %v to k3d cluster", name)
		if err := runK3D([]string{
			"node",
			"create",
			name,
			fmt.Sprintf("--cluster=%s", c.Name),
			"--wait",
			"--timeout=2m",
			fmt.Sprintf("--image=%s", getRancherK3sImageName(c.kubernetesVersion)),
			fmt.Sprintf("--k3s-node-label=%s=%s", constants.CRLAppLabelKey, constants.CRLAppLabelValue),
		}); err != nil {
			dumpK3DContainerLogs(c.Name)
			if deleteErr := runK3D([]string{"node", "delete", fmt.Sprintf("k3d-%s-0", name)}); deleteErr != nil {
				logging.Infof(ctx, "failed to delete node %v after failed create: %v", name, deleteErr)
			}
			return err
		}
		return nil
	}, &backoff.Opts{
		Timeout:         8 * time.Minute,
		InitialInterval: 5 * time.Second,
		MaxInterval:     15 * time.Second,
	})
	if err != nil {
		return errors.Wrap(err, "create k3d node")
	}

	name = fmt.Sprintf("k3d-%s-0", name)

	start := time.Now()
	for time.Since(start) < 10*time.Second {
		time.Sleep(250 * time.Millisecond)

		_, err := c.clientSet.CoreV1().Nodes().Get(ctx, name, v1.GetOptions{})
		if err == nil {
			break
		}
	}

	if err := c.configureCiliumMounts(ctx, name); err != nil {
		return err
	}

	_, err = c.clientSet.CoreV1().Nodes().Patch(ctx, name, types.JSONPatchType, fmt.Appendf(nil, `[
	  { "op": "add", "path": "/metadata/labels/failure-domain.beta.kubernetes.io~1zone", "value": "%s" },
	  { "op": "add", "path": "/metadata/labels/failure-domain.beta.kubernetes.io~1region", "value": "%s" }
	]`, zone, c.region), metav1.PatchOptions{})

	logging.Infof(ctx, "started node %v in k3d cluster", name)
	return errors.Wrap(err, "patching node")
}

// CloseIdleConnections closes any connections which were previously
// connected from previous requests but are now sitting idle in
// a "keep-alive" state. It does not interrupt any connections currently
// in use.
func (c *Cluster) CloseIdleConnections() {
	type closeIdler interface {
		CloseIdleConnections()
	}
	if c, ok := c.ClientSet().CoreV1().RESTClient().(*rest.RESTClient); ok {
		t := c.Client.Transport
		for {
			switch tr := t.(type) {
			case closeIdler:
				tr.CloseIdleConnections()
				return
			case net.RoundTripperWrapper:
				t = tr.WrappedRoundTripper()
			default:
				return
			}
		}
	}
}

// newCluster creates and pre-fills a Cluster struct.
func newCluster(name string) (*Cluster, error) {
	cfg, err := config.GetConfigWithContext(fmt.Sprintf("k3d-%s", name))
	if err != nil {
		return nil, errors.Wrap(err, "loading config")
	}

	// The default rate limitter is quite restrictive. For hosted
	// environments, we likely want to respect these limits but for k3d we
	// have a need for speed.
	cfg.QPS = 1000
	cfg.Burst = 1000

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "loading config")
	}

	return &Cluster{
		Name:      name,
		config:    cfg,
		clientSet: cs,
	}, nil
}

func getRancherK3sImageName(kubernetesVersion string) string {
	return fmt.Sprintf("rancher/k3s:%s", kubernetesVersion)
}

type writerFunc func(p []byte) (int, error)

func (wf writerFunc) Write(p []byte) (int, error) { return wf(p) }

func runK3D(args []string) error {
	// Logging the full command line allows executing the commands by hand with
	// simple copy/paste.
	logging.Debugf(context.Background(),
		"executing: %s %s", K3D, strings.Join(args, " "))

	buf := bytes.NewBuffer(nil)
	cmd := exec.Command(K3D, args...)
	logger := logging.S(context.Background()).Named("k3d")
	var f writerFunc = func(p []byte) (int, error) {
		sc := bufio.NewScanner(strings.NewReader(string(p)))
		for sc.Scan() {
			logger.Info(sc.Text())
		}
		return len(p), nil
	}
	// The standard outputs are redirected to a writer so the output is logged
	// as soon as k3d generates it. This makes the logs lines timestamp reflect
	// accurately the occurrence time and gives information about the timing of
	// each step.
	cmd.Stderr = f
	cmd.Stdout = f

	if err := cmd.Run(); err != nil {
		return errors.Wrapf(err, "%s", buf.String())
	}
	return nil
}

// dumpK3DContainerLogs dumps Docker container logs for all containers belonging
// to the given k3d cluster. This is called on failure to capture k3s internal
// errors (e.g. cgroup, iptables, or networking issues) that k3d does not
// surface in its own output.
//
// The last 200 lines of each container's logs are always emitted to the test
// logger. If ARTIFACT_DIR is set, full logs are also written as individual
// files per container for CI artifact collection.
func dumpK3DContainerLogs(clusterName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logger := logging.S(ctx).Named("k3d-container-logs")

	out, err := exec.CommandContext(ctx,
		"docker", "ps", "-a",
		"--filter", "label=app=k3d",
		"--filter", fmt.Sprintf("label=k3d.cluster=%s", clusterName),
		"--format", "{{.Names}}",
	).CombinedOutput()
	if err != nil {
		logger.Infof("failed to list k3d containers for cluster %s: %v: %s", clusterName, err, out)
		return
	}

	artifactsDir := os.Getenv("ARTIFACT_DIR")

	containers := strings.SplitSeq(strings.TrimSpace(string(out)), "\n")
	for container := range containers {
		container = strings.TrimSpace(container)
		if container == "" {
			continue
		}

		var logArgs []string
		if artifactsDir != "" {
			logArgs = []string{"logs", container}
		} else {
			logArgs = []string{"logs", "--tail", "200", container}
		}
		logs, err := exec.CommandContext(ctx, "docker", logArgs...).CombinedOutput()
		if err != nil {
			logger.Infof("failed to get logs for container %s: %v: %s", container, err, logs)
			continue
		}

		if artifactsDir != "" {
			logFile := fmt.Sprintf("%s/k3d-%s.log", artifactsDir, container)
			if writeErr := os.WriteFile(logFile, logs, 0644); writeErr != nil {
				logger.Infof("failed to write log file %s: %v", logFile, writeErr)
			} else {
				logger.Infof("wrote container logs to %s", logFile)
			}
		}

		logger.Infof("=== docker logs (last 200 lines) for container %s ===", container)
		lines := strings.Split(string(logs), "\n")
		if len(lines) > 200 {
			lines = lines[len(lines)-200:]
		}
		for _, line := range lines {
			logger.Info(line)
		}
	}
}

// Prepare node for cilium installation. This function needs to run on every
// agent and server node that is expected to run cilium. We run regardless of
// the CNI to simplify upstream logic.
// https://github.com/cilium/cilium/issues/18675#issuecomment-1050234756
func (c *Cluster) configureCiliumMounts(ctx context.Context, node string) error {
	cmd := exec.Command("docker", "exec", node, "mkdir", "-p", "/sys/fs/bpf")
	if bytes, err := cmd.CombinedOutput(); err != nil {
		return errors.Wrap(err, string(bytes))
	}

	cmd = exec.Command("docker", "exec", node, "mount", "bpffs", "/sys/fs/bpf", "-t", "bpf")
	if bytes, err := cmd.CombinedOutput(); err != nil {
		return errors.Wrap(err, string(bytes))
	}

	cmd = exec.Command("docker", "exec", node, "mount", "--make-shared", "/sys/fs/bpf")
	if bytes, err := cmd.CombinedOutput(); err != nil {
		return errors.Wrap(err, string(bytes))
	}

	cmd = exec.Command("docker", "exec", node, "mkdir", "-p", "/run/cilium/cgroupv2")
	if bytes, err := cmd.CombinedOutput(); err != nil {
		return errors.Wrap(err, string(bytes))
	}

	cmd = exec.Command("docker", "exec", node, "mount", "-t", "cgroup2", "none", "/run/cilium/cgroupv2")
	if bytes, err := cmd.CombinedOutput(); err != nil {
		return errors.Wrap(err, string(bytes))
	}

	cmd = exec.Command("docker", "exec", node, "mount", "--make-shared", "/run/cilium/cgroupv2")
	if bytes, err := cmd.CombinedOutput(); err != nil {
		return errors.Wrap(err, string(bytes))
	}
	return nil
}

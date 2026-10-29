package testenv

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"math/rand"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/betaclusterctrl"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/clusterctrl"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/nodectrl"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/tenantctrl"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kuberecord"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbtest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manager"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/metrics"
	k3dcloud "github.com/cockroachlabs/cockroach-enterprise-operator/pkg/cloudinfra/k3d"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/configtypes"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/infrastructure"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/infrastructure/fake"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/infrastructure/k3d"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/infrastructure/k8s"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/tenant"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/evictionutil"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	admissionv1 "k8s.io/api/admission/v1"
	registrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors" //nolint:importcheck
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// debugEnv is a flag passed to "go test" in order to troubleshoot a failing
// test:
//  1. K8s clusters used by the test are not deleted after a test runs, even if
//     EnvConfig.Reuse is false.
//  2. Any K8s namespace created for the test is not deleted after the test
//     completes.
//  3. If the test is re-run, any existing K8s clusters and any namespace will
//     be reused rather than re-created.
var debugEnv = flag.Bool("debug-env", false, "Reuse current K8s environment.")

// DefaultNumOperatorReplicas is the default number of operator replicas we want for
// integration tests. Currently it makes sense to have a single operator.
var DefaultNumOperatorReplicas int32 = 1

// TestEnvShortID represents the short ID that gets assigned to k3d clusters
// created via testenv.
var TestEnvShortID = "k3ds"

// newMultiVersionScheme creates a runtime.Scheme with both v1alpha1 and v1beta1 API versions.
// This is required for conversion webhook support.
func newMultiVersionScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(kube.AddToScheme(scheme))
	return scheme
}

// Env represents a K8s testing environment that may manage one or more K8s
// clusters. Example usage:
//
//	env := New(t, kubetest.SingleZoneConfig)
//	err := env.DefaultCluster().Ctl.Apply(ctx, myManifest)
//
// By default, kubetest.Env uses kuberecord to record and then playback calls to
// the K8s API. This allows unit tests to run orders of magnitude faster, since
// they don't need to create a real K8s cluster during playback and since they
// can run in parallel. When changes cause a test to call the K8s API
// differently, that test must be re-recorded:
//
//	go test -run TestXXX -record
//
// While recording and playback of K8s API calls is convenient and powerful, we
// still want to regularly run against "real" K8s clusters. Therefore, if
// testutils.IntegrationTestsEnabled is true, kubetest.Env will disable
// kuberecord and always run against a real K8s cluster that contains the real
// cockroach-operator Docker image. This simulates an environment that is as
// close as possible to production conditions.
//
// Do not create your own EnvConfig unless you're testing something very custom.
// Prefer to use the pre-built configs like SingleZoneConfig, since then the Env
// can reuse K8s clusters across tests.
//
// To see verbose test logs, use the "-v" flag:
//
//	go test -run TestXXX -v
type Env struct {
	// Clusters is a map of the K8s clusters managed by this environment,
	// indexed by region name.
	Clusters map[string]*KubeCluster

	// options control how the environment operates.
	options envOptions

	// defaultRegion specifies which of the regions contains the K8s cluster
	// returned by the DefaultCluster method.
	defaultRegion string

	// sequence is used by monotonicallyIncreasingInt() to return an positive
	// increasing sequence of ints. This number is used to uniquely identify a
	// k3d cluster in a set of networked k3d clusters.
	sequenceNumber int
}

// New sets up a testing environment according to the given testing context and
// desired configuration and options.
// TODO(ye): there's a lot of code duplication between this and cloudinfra/k3d
//
//	package. Refactor this to use cloudinfra/k3d instead.
func New(t *testing.T, config EnvConfig, opts ...EnvOption) *Env {
	env := &Env{
		Clusters:       make(map[string]*KubeCluster),
		sequenceNumber: 1,
	}
	for _, opt := range opts {
		opt(&env.options)
	}

	if !env.options.skipLeakTest {
		testutils.GoroutineLeakCheck(t)
	}

	// Set -kuberecord=disable if integration tests are enabled. Otherwise, set
	// both -kuberecord=record and Copyist -record if either is set, or if
	// -debug-env is set.
	setRecordingFlags(t)

	// Never reuse environment when running integration tests. This causes too
	// many hard-to-diagnose flakes, as failures and side effects in one test can
	// cause failures in subsequent tests.
	if testutils.IntegrationTestsEnabled {
		config.Reuse = false
	}

	// If kuberecord is in record or playback mode, then open a copyist session
	// in order to record any SQL requests to the cluster. Also, make time and
	// random numbers deterministic so that recordings do not change based on the
	// current time.
	if kuberecord.IsEnabled() {
		// Seed the test based on its deterministic name, and restore after.
		rand.Seed(int64(hashString(t.Name()))) //nolint:staticcheck

		t.Cleanup(func() {
			rand.Seed(time.Now().UTC().UnixNano()) // nolint:staticcheck
		})

		// Ensure deterministic time. Use known starting point and each time
		// timeutil.Now is called, "tick" up by one hour.
		now := time.Date(2021, 1, 29, 12, 0, 0, 0, time.UTC)
		restore := testutils.HookGlobal(&timeutil.Now, func() time.Time {
			now = now.Add(time.Hour)
			return now
		})
		t.Cleanup(restore)
	}

	// If user requests verbose logging, then dump everything to the test
	// logger.
	if testing.Verbose() {
		logging.SetGlobalLogger(zaptest.NewLogger(t))
	}

	env.start(t, config)
	return env
}

// DefaultCluster returns the K8s cluster in the default region, as specified in
// the EnvConfig used to create the environment. If the default region was not
// specified, or no longer contains a K8s cluster, then an arbitrary region is
// returned.
func (e *Env) DefaultCluster() *KubeCluster {
	cluster, ok := e.Clusters[e.defaultRegion]
	if ok {
		return cluster
	}
	for _, cluster := range e.Clusters {
		return cluster
	}
	panic("the environment contains no clusters")
}

func (e *Env) start(t *testing.T, config EnvConfig) {
	ctx := context.Background()
	require.NotEmpty(t, strings.TrimSpace(config.Name))
	require.NotZero(t, len(config.Regions))

	// Stop all existing clusters that are not part of the environment topology.
	// This avoids burning up the machine, as running multiple k3d clusters at
	// once is very CPU-expensive.
	require.NoError(t, e.stopClusters(config))

	// canReuse is true if the config allows K8s cluster reuse, or the debug-env
	// flag is set.
	config.Reuse = config.Reuse || *debugEnv

	e.startOrCreateClusters(t, ctx, &config)

	for region, cluster := range e.Clusters {
		options := kube.Options{
			Scheme:                 newMultiVersionScheme(),
			BackOff:                kuberecord.BackOff(),
			DisallowCustomBackOffs: true,
		}
		ctl, err := kube.NewCtlWithOptions(cluster.Config, options)
		require.NoError(t, err)

		// Install test certificates that are used by the operator (e.g. TLS
		// certificate for the webhook).
		kubecrdbtest.InstallOperatorCerts(t, ctl)

		// We install the operator cilium-role regardless of the installed CNI
		// to avoid piping the Cilium flag all around.
		clusterRoles := []*rbacv1.ClusterRole{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cockroach-operator-cilium-role",
				},
				Rules: []rbacv1.PolicyRule{
					{
						APIGroups: []string{"cilium.io"},
						Resources: []string{
							"ciliumnetworkpolicies",
							"ciliumnetworkpolicies/status",
							"ciliumnetworkpolicies/finalizers",
							"ciliumclusterwidenetworkpolicies",
							"ciliumclusterwidenetworkpolicies/status",
							"ciliumclusterwidenetworkpolicies/finalizers",
						},
						Verbs: []string{
							"create", "delete", "get", "list", "patch", "update", "watch",
						},
					},
				},
			},
		}

		if e.options.skipOperatorDeploy {
			// The caller wants a bare cluster with no operator, CRDs, or
			// webhooks. This is used by tests that need to control the
			// deployment order themselves (e.g. migration tests that install a
			// public operator first).
		} else if deployOperator() {
			// If the operator should be deployed to the K8s cluster, then re-apply
			// its manifest so that any modifications to it get picked up,
			// particularly in the cluster reuse case.
			objects := manifests.CockroachOperator(manifests.CockroachOperatorOptions{
				Replicas:                  DefaultNumOperatorReplicas,
				SkipTenantController:      e.options.skipTenantController,
				EnableK8sNodeController:   e.options.enableK8sNodeController,
				EnableMigrationController: e.options.enableMigrationController,
				ClusterRoles:              clusterRoles,
				Image:                     e.options.operatorImage,
			})
			require.NoError(t, ctl.Apply(ctx, objects...))
			// Wait until the webhook service endpoints are ready. This will
			// ensure that follow-up operations (that don't retry) won't fail
			// due to webhook being not ready.
			require.Eventually(t, func() bool {
				var ep corev1.Endpoints
				if err := ctl.Get(ctx, manifests.WebhookServiceName, &ep); err != nil {
					return false
				}
				for _, eps := range ep.Subsets {
					if len(eps.Addresses) > 0 {
						return true
					}
				}
				return false
			}, 30*time.Second, 250*time.Millisecond)
		} else {
			// The operator is not being deployed, so delete any existing copy of
			// it from the K8s cluster, so that it does not interfere with
			// reconciliation performed by the MutationCallback method below.
			// Ignore any error, since it may not be present in the cluster.
			deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cockroach-operator"}}
			_ = ctl.Delete(ctx, deployment)

			// Also delete the webhook configuration, for similar reasons.
			webhookCfg := &registrationv1.ValidatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{Name: "cockroach-webhook-config"},
			}
			_ = ctl.Delete(ctx, webhookCfg)

			// Install the custom resource definitions.
			require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoLastAppliedAnnotation: true}, manifests.CustomResourceDefinitions()...))
		}

		// When the operator is not deployed, synchronously trigger webhooks and
		// reconciliation on the operator's custom controllers via the
		// BeforeMutation and AfterMutation callbacks. This simulates what K8s
		// will do, but in the testing process. This makes it possible to use
		// kuberecord and also makes debugging much easier.
		// Skip this entirely when skipOperatorDeploy is set, since the test
		// manages operator lifecycle itself.
		if !deployOperator() && !e.options.skipOperatorDeploy {
			options.BeforeMutation = MakeBeforeMutationCallback(t, ctl, region)
			options.AfterMutation = e.makeAfterMutationCallback(t, ctl, region)

			// Recreate the kube.Ctl instance to include the callbacks.
			ctl, err = kube.NewCtlWithOptions(cluster.Config, options)
			require.NoError(t, err)
		}

		// Wrap the existing Hardware field with a Kubernetes VM pool that
		// incorporates Kubernetes-level concepts such as node readiness and
		// draining.
		vmPoolOptions := infrastructure.VMPoolGroupOptions{}
		if kuberecord.Mode() == kuberecord.ModeReplaying {
			vmPoolOptions.BackOff = kuberecord.BackOff()
		}
		cluster.Hardware = k8s.NewK8sVMPool(ctl, cluster.Hardware, vmPoolOptions)

		cluster.Ctl = ctl
		setInternalAPIAddress(t, cluster)
		e.Clusters[region] = cluster
	}
}

func setInternalAPIAddress(t *testing.T, cluster *KubeCluster) {
	masterNode := getMasterNode(t, cluster.Ctl)
	for _, nodeAddress := range masterNode.Status.Addresses {
		if nodeAddress.Type == corev1.NodeInternalIP {
			// 6443 is the default API server port for k3d.
			// https://k3d.io/v5.0.0/design/defaults/#api-ports
			cluster.InternalAPIAddress = fmt.Sprintf("https://%s:6443",
				nodeAddress.Address)
		}
	}
}

func getMasterNode(t *testing.T, ctl *kube.Ctl) corev1.Node {
	var nodes corev1.NodeList
	require.NoError(t, ctl.List(context.Background(), &kube.ListOptions{
		LabelSelector: kube.ParseLabelOrDie("node-role.kubernetes.io/master=true"),
	}, &nodes))
	require.NotEmpty(t, nodes.Items)
	return nodes.Items[0]
}

func (e *Env) monotonicallyIncreasingInt() (n int) {
	n = e.sequenceNumber
	e.sequenceNumber++
	return
}

// stopClusters stops any existing clusters that are not part of the environment
// topology.
func (e *Env) stopClusters(config EnvConfig) error {
	// If in K8s replay mode, there are no clusters to stop.
	if kuberecord.Mode() == kuberecord.ModeReplaying {
		return nil
	}

	existingNames, err := k3d.GetClusterNames()
	if err != nil {
		return err
	}

	for _, name := range existingNames {
		found := false
		for region := range config.Regions {
			topoName := makeClusterName(config.Name, region, len(config.Regions))
			if name == topoName {
				found = true
			}
		}
		if !found {
			_ = k3d.StopCluster(name)
		}
	}
	return nil
}

func (e *Env) startOrCreateClusters(t *testing.T, ctx context.Context, config *EnvConfig) {
	// If in K8s replay mode, then create a fake cloud environment and intercept
	// K8s calls.
	if kuberecord.Mode() == kuberecord.ModeReplaying {
		for region, zones := range config.Regions {
			ctx := context.Background()
			azs := make([]infrastructure.AvailabilityZone, 0, len(zones))
			for name := range zones {
				azs = append(azs, infrastructure.AvailabilityZone(name))
			}
			hardware := fake.NewVMPoolGroup(azs, fake.VMPoolGroupOptions{})
			for name, nodes := range zones {
				require.NoError(t, hardware.Resize(ctx, infrastructure.AvailabilityZone(name), nodes))
			}
			clusterConfig := kuberecord.RecorderWithOptions(t, t.Name(), kuberecord.Mode(), nil)
			e.Clusters[region] = &KubeCluster{Config: clusterConfig, Hardware: hardware}
		}

		return
	}

	cfg := &configtypes.Config{
		Cluster: configtypes.Cluster{
			Name:    config.Name,
			ShortID: TestEnvShortID,
		},
	}

	// // Pick the CNI at random. Note this could be overwritten if the
	// // test is reusing an existing cluster.
	// cni := func() string {
	// 	if len(config.Regions) > 1 || e.rng.Intn(2) == 1 {
	// 		return "CALICO"
	// 	}
	// 	return "CILIUM"
	// }()
	cni := "CALICO"

	for region, zones := range config.Regions {
		nodes := func() (nodes int) {
			for _, replicas := range zones {
				nodes += replicas
			}
			return
		}()

		cfg.Regions = append(cfg.Regions, configtypes.RegionConfig{
			Cluster:             cfg.Cluster,
			Region:              region,
			NodeCount:           nodes,
			KubernetesCNIPlugin: cni,
			KubernetesClusterID: e.monotonicallyIncreasingInt(),
		})
	}

	cloud := k3dcloud.NewCloud(cfg, config.Reuse)
	_, err := cloud.Create(ctx)
	require.NoError(t, err)

	// Delete the cluster at the end of the test if it cannot be reused.
	if !config.Reuse {
		t.Cleanup(func() {
			if err := cloud.Delete(ctx); err != nil {
				t.Fatalf("destroying k3d clusters: %v", err)
			}
		})
	}

	for _, region := range cfg.Regions {
		// TODO(carlo): this is a break in the cloud abstraction. Ideally the
		// rest config can be grabbed in a cloud-neutral way, like
		// cloud.GetRegion().Ctl(). This interface does not exist yet so we
		// are forced to access the k3d cluster directly.
		k3dClusterName := cfg.Cluster.Name + "-" + region.Region
		cluster, err := k3d.StartCluster(k3dClusterName)
		require.NoError(t, err)

		t.Cleanup(func() {
			// Close idle connections before exiting to prevent leaked go routines.
			cluster.CloseIdleConnections()
		})

		clusterConfig := kuberecord.RecorderWithOptions(
			t, t.Name(), kuberecord.Mode(), cluster.RestConfig(),
		)

		e.Clusters[region.Region] = &KubeCluster{Config: clusterConfig, Hardware: cluster}

		// Set the kube.Ctl instance for the default region.
		if e.defaultRegion == "" {
			// Use an arbitrary region as default region if not set by caller.
			if config.DefaultRegion == "" || config.DefaultRegion == region.Region {
				e.defaultRegion = region.Region
				// Switch the current kubectl context to the default K8s cluster. This
				// makes inspecting the cluster with kubectl more convenient during a
				// debugging session.
				contextName := fmt.Sprintf("k3d-%s", k3dClusterName)
				require.NoError(t, runKubectl("config", "use-context", contextName))
			}
		}
	}
}

// makeBeforeMutationCallback returns a function that is called just before
// kube.Ctl creates, updates, or deletes a K8s object. If the object is a
// CrdbCluster or CrdbTenant, then we'll invoke the admission webhook so that
// the objects can be validated and suspended tenants can be resumed.
// This enables kubetest.Env to support the recording and playback of K8s API
// calls, since the synchronous webhook invocation ensures determinism.
func MakeBeforeMutationCallback(t *testing.T, ctl *kube.Ctl, region string) kube.MutationCallback {
	// Construct the webhook handler for this K8s cluster.
	decoder, err := admission.NewDecoder(newMultiVersionScheme())
	require.NoError(t, err)
	webhook := manager.NewWebhook(ctl, cloud.Environment{CloudProvider: cloud.K3D, CloudRegion: region})
	require.NoError(t, webhook.InjectDecoder(decoder))

	// When kube.Ctl calls back this function, call Handle on the handler.
	return func(object kube.Object) error {
		gvk := object.GetObjectKind().GroupVersionKind()
		if gvk.Group != crdbv1beta1.GroupVersion.Group {
			return nil
		}

		raw, err := json.Marshal(object)
		require.NoError(t, err)

		request := admission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				Kind: metav1.GroupVersionKind{
					Group:   gvk.Group,
					Version: gvk.Version,
					Kind:    gvk.Kind,
				},
				Name:      object.GetName(),
				Namespace: object.GetNamespace(),
				Object: runtime.RawExtension{
					Raw: raw,
				},
			},
		}

		// Don't attempt to create complete admission.Request; only fill out
		// fields that the webhook handler actually uses.
		response := webhook.Handle(context.Background(), request)
		if !response.Allowed {
			statusErr := k8serrors.StatusError{
				ErrStatus: *response.Result,
			}
			return &statusErr
		}
		return nil
	}
}

// makeAfterMutationCallback returns a function that is called any time kube.Ctl
// creates, updates, or deletes a K8s object. If the mutated object is a
// CrdbCluster or CrdbTenant, then we'll trigger synchronous reconciliation.
// This enables kubetest.Env to support the recording and playback of K8s API
// calls, since the synchronous reconciliation ensures determinism.
func (e *Env) makeAfterMutationCallback(
	t *testing.T, ctl *kube.Ctl, region string,
) kube.MutationCallback {
	ctx := context.Background()

	// Specify the environment in which the K8s cluster runs.
	env := &cloud.Environment{CloudProvider: cloud.K3D, CloudRegion: region}

	nodeLocalityReader := func(ctx context.Context, nodeName string) (cloud.LocalityInfo, error) {
		// No need to cache in test situations.
		var node corev1.Node
		if err := ctl.Get(ctx, nodeName, &node); err != nil {
			return cloud.LocalityInfo{}, err
		}
		return cloud.MakeLocalityInfoForNode(ctx, &node), nil
	}

	// Construct custom controllers for this K8s cluster.
	reconcilers := make(map[string]controllers.Reconciler, 2)

	credProvider, err := cloud.NewCredentialProvider(*env)
	require.NoError(t, err)

	tenantReconciler := &tenantctrl.Reconciler{
		Ctl:                ctl,
		MetricsManager:     e.options.metricsMgr,
		Rng:                rand.NewSource(0),
		ScaleLockTable:     tenant.NewLockTable(),
		TenantPodSyncer:    tenant.NewTenantPodSyncer(ctx, ctl, credProvider, nodeLocalityReader),
		NodeLocalityReader: nodeLocalityReader,
	}
	if tenantReconciler.MetricsManager == nil {
		tenantReconciler.MetricsManager = &metrics.TestTenantManager{}
	}

	reconcilers[crdbv1beta1.CrdbTenantKind] = tenantReconciler

	reconcilers[crdbv1beta1.CrdbClusterKind] = &clusterctrl.Reconciler{
		Env:            env,
		Ctl:            ctl,
		Scheme:         newMultiVersionScheme(),
		BetaReconciler: betaclusterctrl.NewReconciler(env, ctl, &evictionutil.Helper{}),
	}
	reconcilers[crdbv1beta1.CrdbNodeKind] = nodectrl.NewReconciler(ctl, &evictionutil.Helper{})

	// If in record or playback mode, then put the reconciler into test mode so
	// that it will use the copyist driver.
	for _, reconciler := range reconcilers {
		reconciler.SetTestMode(kuberecord.IsEnabled())
	}

	// When kube.Ctl calls back this function, call Reconcile on both custom
	// controllers.
	return func(object kube.Object) error {
		gvk := object.GetObjectKind().GroupVersionKind()
		if gvk.Group != crdbv1beta1.GroupVersion.Group {
			return nil
		}

		key := client.ObjectKeyFromObject(object)

		if reconciler, ok := reconcilers[gvk.Kind]; ok {
			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			// Reconciliation should never fail when testing locally like this.
			require.NoError(t, err)
		}

		return nil
	}
}

// deployOperator specifies whether to install the operator in the K8s test
// cluster using a pre-built image. Always do this when running integration
// tests.
func deployOperator() bool {
	return testutils.IntegrationTestsEnabled
}

func makeClusterName(prefix, region string, numRegions int) string {
	clusterName := prefix
	if numRegions > 1 {
		// Only add region to cluster name if there would otherwise be ambiguity.
		clusterName += "-" + region
	}
	return clusterName
}

func makeK3dTopology(t *testing.T, zones Zones) []k3d.Topology {
	var topo []k3d.Topology
	for zone, numNodes := range zones {
		require.NotZero(t, numNodes)
		topo = append(topo, k3d.Topology{Zone: zone, Replicas: numNodes})
	}
	return topo
}

func runKubectl(args ...string) error {
	cmd := exec.Command("kubectl", args...)
	buf := bytes.NewBuffer(nil)

	cmd.Stderr = buf

	return errors.Wrapf(cmd.Run(), "%s", buf.String())
}

// setRecordingFlags disallows recording and playback if Integration tests are
// enabled, since Integration tests should always use the "real" environment.
//
// Also, both -kuberecord and -record to recording mode if either package is
// recording. This enables the user to set either one without needing to
// remember both. Also both are set if -debug-env is set.
func setRecordingFlags(t *testing.T) {
	// Integration tests should operate on real K8s clusters rather than using
	// pre-recorded K8s events, as recorded by kuberecord.
	if testutils.IntegrationTestsEnabled {
		if kuberecord.Mode() == kuberecord.ModeRecording {
			t.Fatal("cannot use -kuberecord=record or -record when running Integration tests")
		}
		require.NoError(t, flag.Set("kuberecord", "disable"))
		return
	}

	if *debugEnv || kuberecord.Mode() == kuberecord.ModeRecording {
		require.NoError(t, flag.Set("kuberecord", "record"))
	}
}

func hashString(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

// IsCiliumCNI returns true if Cilium is the CNI plugin running in the cluster.
func IsCiliumCNI(t *testing.T, ctl *kube.Ctl) bool {
	cni, err := cloud.GetNetworkPluginForCluster(context.Background(), ctl)
	require.NoError(t, err)
	return cni == cloud.Cilium
}

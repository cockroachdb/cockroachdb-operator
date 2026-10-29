package manager

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1alpha1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1alpha1"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/autoscaler"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/betaclusterctrl"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/certs"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/clusterctrl"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/k8snodectrl"
	opmetrics "github.com/cockroachlabs/cockroach-enterprise-operator/controllers/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/migrationctrl"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/nodectrl"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/tenantctrl"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/crdbcerts"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/opserver"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/tenant"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/evictionutil"
	"github.com/fsnotify/fsnotify"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/conversion"
)

// NOTE: Values selected through some preliminary testing resulting in a downtime of around 10 secs,
// but will potentially need tuning over time. We want something that's short enough to have
// tolerable transition times but long enough preventing thrashing.
// NOTE: They are held in vars as opposed to consts since we need to get their addresses.
var (
	// leaseDuration is the valid duration of the leader's lease.
	leaseDuration = 8 * time.Second

	// renewDeadline is the duration that a leader will try and renew its lease before killing itself.
	renewDeadline = 7 * time.Second

	// retryPeriod is the duration between each attempted action.
	retryPeriod = 1 * time.Second
)

const LeaderElectionId = "cockroach-operator-lock"

// Manager starts up and manages all the components that are part of the
// operator:
//
//   - controller manager
//   - health/readiness endpoint
//   - webhook
//   - autoscaler
type Manager struct {
	// Environment contains information about cloud environment in which the
	// operator is running.
	cloud.Environment

	// Options modifies the behavior of the K8s controller manager.
	ctrl.Options

	// InTestMode indicates that the manager is being tested. The Run method
	// will not start up the autoscaler or controller manager.
	InTestMode bool

	// InLocalMode indicates that the manager is being run locally for development.
	// This will run the manager normally with 2 differences:
	// 1. m.Options.LeaderElectionNamespace is set to the namespace passed in via
	//    NAMESPACE env var. This is normally injected into the container by the
	//    kubelet via downwards API to /var/run/secrets/kubernetes.io/serviceaccount/namespace
	// 2. Failures calling the ValidatingWebhookConfiguration are ignored. Normally
	//    the k8s api-server hits the operator to validate CRs, but that's no longer
	//    possible as you can't port-forward api-server -> local port.
	InLocalMode bool

	// WebhooksOnly indicates that the manager should only serve webhooks (mutate/validate/convert)
	// and should not run controllers, autoscaler, or leader election.
	//
	// This is intended for local debugging workflows where controllers run locally, but the
	// Kubernetes apiserver must still be able to reach the webhook endpoints inside the cluster.
	WebhooksOnly bool

	// DisableLeaderElection disables leader election even when not in test mode.
	// This is useful when running the operator locally for debugging against a remote cluster.
	// Only safe if there is a single active controller instance.
	DisableLeaderElection bool

	// RpcAddr is the address the operatorpb.OperatorServer endpoint binds to.
	// Setting RpcAddr == "" disables the rpc server.
	RpcAddr string

	// SkipTenantController skips running tenant controller.
	// This is set to true when we only need to reconcile dedicated clusters.
	SkipTenantController bool

	// EnableK8sNodeController enables running K8s Node controller.
	// This is set to true for clusters when K8s Node decommission functionality is needed.
	EnableK8sNodeController bool

	// EnableMigrationController enables running the Migration controller.
	// This is set to true to automate migration from CR/StatefulSet to this Operator CR.
	EnableMigrationController bool

	// DisableTenantFinalization skips performing finalization of tenants.
	DisableTenantFinalization bool

	// SelfSignedOperatorCerts enables automatic generation of self-signed TLS
	// certificates for the operator webhook at startup. When true, the operator
	// creates the following resource if it does not already exist:
	//
	//   - cockroach-operator-certs  Secret  — operator webhook CA, CA key, and TLS cert
	//
	// The CA key is retained in the secret so that clusters with selfSigned:true
	// can reuse the same CA. Disable this flag when the operator webhook certificate
	// is managed externally (e.g. cert-manager).
	SelfSignedOperatorCerts bool

	restCfg         *rest.Config
	mgr             ctrl.Manager
	ctl             *kube.Ctl
	scaler          *autoscaler.Autoscaler
	tenantPodSyncer tenant.TenantPodSyncer
	namespace       string

	// Namespaces the operator should watch. When empty, the operator watches
	// all namespaces (global mode). When non-empty, only watches the specified
	// namespaces (scoped mode).
	watchNamespaces []string

	// scaleLockTable is used to serialize access to a tenant's pods. Operations
	// guarded by the scaleLockTable must also respect the operator's leader
	// election status.
	scaleLockTable *tenant.LockTable

	// nodeLocalities is used to cache locality data for Kubernetes nodes. The
	// localities stable as they are tied to the names of the nodes. If the
	// localities are updated, a new node name will be assigned by Kubernetes.
	// This allows us to avoid a Kubernetes API call in critical paths. Given
	// that writes are rare, using sync.Map allows us to avoid grabbing a mutex
	// every time.
	nodeLocalities sync.Map // map[string]*LocalityInfo

	// certChangeHandler is called when a certificate change is detected.
	// It can be overridden in tests to verify cert watching behavior.
	// If nil, defaults to calling registerWebhookCABundles.
	certChangeHandler func(context.Context) error

	// certDebounceDelay is the time window for debouncing certificate change events.
	// Multiple events within this window are collapsed into a single update.
	// Defaults to 2 seconds in production can be set to smaller values in tests.
	certDebounceDelay time.Duration

	// v1alpha1InStorage is set during CRD registration based on the existing
	// CRD's status.storedVersions. When no v1alpha1 objects exist, we skip
	// the conversion webhook and mark v1alpha1 as not served even if the
	// migration flag is set.
	v1alpha1InStorage bool
}

// SetCertChangeHandler sets a custom handler for certificate change events.
// This is primarily used for testing to inject a mock handler.
// If handler is nil, the default behavior (calling registerWebhooks) will be used.
func (m *Manager) SetCertChangeHandler(handler func(context.Context) error) {
	m.certChangeHandler = handler
}

// Register v1alpha1 types into the v1beta1 package-level scheme once at init
// time. crdbv1beta1.Scheme already carries core K8s types, CRD extensions, and
// unstructured types (Cilium, AWS LB) via its own init(). Adding v1alpha1 here
// gives us a single scheme with everything the operator needs.
func init() {
	utilruntime.Must(crdbv1alpha1.AddToScheme(crdbv1beta1.Scheme))
}

// New constructs a new operator manager with the given K8s configuration. Set
// any environment and/or controller options, and then call Run to start up all
// the operator's components.
func New(restCfg *rest.Config, namespace string, watchNamespaces []string) *Manager {
	// Default to the standard operator namespace if none is provided.
	if namespace == "" {
		namespace = manifests.OperatorNamespace
	}
	opts := ctrl.Options{
		Scheme:  crdbv1beta1.Scheme,
		Port:    manifests.WebhookServicePort,
		CertDir: manifests.OperatorCertsDir,
	}

	switch len(watchNamespaces) {
	case 0:
		// Global mode: watch all namespaces (existing behavior).
	case 1:
		// Single-namespace mode: use the built-in Namespace field.
		opts.Namespace = watchNamespaces[0]
	default:
		// Multi-namespace mode: use MultiNamespacedCacheBuilder.
		opts.NewCache = cache.MultiNamespacedCacheBuilder(watchNamespaces)
	}

	m := &Manager{
		Options:           opts,
		restCfg:           restCfg,
		scaleLockTable:    tenant.NewLockTable(),
		namespace:         namespace,
		watchNamespaces:   watchNamespaces,
		certDebounceDelay: 2 * time.Second, // Default production value
	}
	m.Options.NewClient = func(cache cache.Cache, config *rest.Config, options client.Options, uncachedObjects ...client.Object) (client.Client, error) {
		// Don't use caching client by default. It serves reads from a local
		// cache that can be stale; when the operator uses this, it always wants
		// to call the K8s API server to get the most up-to-date results.
		return client.New(config, client.Options{
			Mapper: options.Mapper,
			Scheme: options.Scheme,
		})
	}
	return m
}

// ControllerManager returns the K8s controller manager created by the operator
// manager.
func (m *Manager) ControllerManager() ctrl.Manager {
	return m.mgr
}

// Run performs initialization and startup tasks for the operator and then waits
// until the give context is canceled:
//
//  1. Installs the latest webhook and CRD manifests used by the operator.
//  2. Starts any HTTP endpoints needed by the operator.
//  3. Starts the K8s controller manager which will manage the operator's
//     controllers and webhooks.
//  4. Starts the autoscaler.
//
// When the context is canceled, all resources created by Run must be gracefully
// shut down.
func (m *Manager) Run(ctx context.Context) error {
	// Bootstrap operator webhook certificates before registerManifests, which
	// reads cockroach-operator-certs to configure the webhook CA bundle.
	if m.SelfSignedOperatorCerts {
		if err := m.bootstrapOperatorCerts(ctx); err != nil {
			return errors.Wrap(err, "failed to bootstrap operator certificates")
		}
	}

	var err error
	if err = m.registerManifests(ctx); err != nil {
		return err
	}

	if !m.InTestMode && !m.WebhooksOnly && !m.DisableLeaderElection {
		// The two major options for leader election are:
		//   - Leader for Life: A pod is designated as leader until it's destroyed. There's no
		//     possibility for multiple leaders, but transition times are longer and there is
		//     little to no control for configurability.
		//     - More info: https://pkg.go.dev/github.com/operator-framework/operator-sdk/pkg/leader
		//   - Leader with Lease: A leader must periodically renew its lease, and if not it must
		//     gracefully kill itself. There are quicker transition times, and relies on updating a
		//     lock file with the pod's local clock, meaning split brain could occur due to variable
		//     clock skew rates. Multiple variables around the election mechanism are configurable.
		//     - More info: https://pkg.go.dev/github.com/kubernetes-sigs/controller-runtime/pkg/leaderelection
		// We have selected Leader with Lease for the following reasons:
		//   - The tradeoff of having a brief overlap of multiple leaders is preferable to having a
		//     longer transition period with no requests being handled.
		//   - Leader with Lease provides a higher granularity of control over tuning the leader
		//     election mechanism. The alternative does not provide this, and the potential service
		//     outage would be significantly longer.
		//   - Clock skew will be a non issue for us since the operators will be running on VMs
		//     with CRDB which requires skew to be < 500ms, which means we don't expect split brain
		//     to occur in production.
		// The mechanism to deal with clock skew is the ratio of LeaseDuration to RenewDeadline is
		//   equivalent to the maximum tolerated of ratio of the rate of the fastest and slowest
		//   pod's clocks.

		// Set leader election options.
		m.Options.LeaderElection = true
		m.Options.LeaderElectionResourceLock = resourcelock.LeasesResourceLock
		m.Options.LeaderElectionID = LeaderElectionId

		m.Options.LeaseDuration = &leaseDuration
		m.Options.RenewDeadline = &renewDeadline
		m.Options.RetryPeriod = &retryPeriod
	}

	// Normally this is derived from a file projected into the pod via the
	// k8s downwards-api
	if !m.InTestMode && m.InLocalMode {
		m.Options.LeaderElectionNamespace = m.namespace
	}

	m.HealthProbeBindAddress = fmt.Sprintf(":%d", manifests.OperatorHealthPort)
	// The default is /readyz but the manifest uses /health.
	m.ReadinessEndpointName = "/health"

	// Create the K8s controller manager and kube.Ctl instance.
	m.mgr, err = ctrl.NewManager(m.restCfg, m.Options)
	if err != nil {
		return errors.Wrap(err, "unable to create manager")
	}
	m.ctl, err = kube.NewCtlFromManager(m.mgr)
	if err != nil {
		return errors.Wrap(err, "unable to create kube.Ctl from manager")
	}

	if err = m.watchCertificates(ctx); err != nil {
		logging.Warnf(ctx, "Failed to start certificate watcher: %v", err)
	}

	// The conversion webhook fetches the StatefulSet to build a v1beta1 spec from
	// live STS state. It is only needed when v1alpha1 objects exist and the webhook
	// is registered. The common path uses the cache-backed client. Protected
	// (skip-labeled) clusters use the uncached API reader so a just-deleted migration
	// StatefulSet cannot linger in the cache and let a protected v1alpha1 write through.
	if m.EnableMigrationController && m.v1alpha1InStorage {
		crdbv1alpha1.SetConversionClient(m.mgr.GetClient())
		crdbv1alpha1.SetConversionUncachedReader(m.mgr.GetAPIReader())
	}

	m.mgr.AddHealthzCheck("ping", healthz.Ping)
	m.mgr.AddReadyzCheck("ping", healthz.Ping)
	m.mgr.AddReadyzCheck("webhook", m.mgr.GetWebhookServer().StartedChecker())

	if !m.SkipTenantController && !m.WebhooksOnly {
		// Set up the autoscaler.
		m.scaler = autoscaler.New(ctx, autoscaler.Options{
			Ctl: m.ctl,
			Emitter: kube.EventEmitter{
				EventRecorder: m.mgr.GetEventRecorderFor(autoscaler.EventSource),
			},
		})
		// Set up the tenant pod syncer before adding the controllers.
		podSyncer, err := m.setupTenantPodSyncer(ctx)
		if err != nil {
			return err
		}
		m.tenantPodSyncer = podSyncer
	}

	if !m.InTestMode && !m.SkipTenantController && !m.WebhooksOnly {
		if err := m.mgr.Add(m.scaler); err != nil {
			return errors.Wrap(err, "unable to register autoscaler")
		}

		// Node watcher marks pods as stale which requires autoscaler for remediation.
		if _, ok := m.scaler.NodeMetricsManager().(*metrics.NodeManager); ok {
			logging.Infof(ctx, "setting up hot-node watcher")
			if err := m.mgr.Add(NewHotNodeWatcher(m.ctl, m.scaler.NodeMetricsManager(), m.scaler.MetricsManager(), m.scaleLockTable)); err != nil {
				return errors.Wrap(err, "unable to register hot-node watcher")
			}
		}
	}

	if !m.WebhooksOnly {
		// Add the CrdbCluster and CrdbTenant controllers to the manager.
		if err := m.addControllers(ctx); err != nil {
			return err
		}
	}

	// Set up the validating webhook.
	m.setupWebhook(ctx)

	// Set up the debug HTTP endpoint.
	if !m.WebhooksOnly {
		if err := m.setupDebug(ctx); err != nil {
			return err
		}
	}

	if !m.SkipTenantController && !m.WebhooksOnly {
		// Set up the operator GRPC server.
		if err := m.setupOperatorServer(ctx); err != nil {
			return err
		}
	}

	// Set up metric reporting.
	if !m.InTestMode && !m.SkipTenantController && !m.WebhooksOnly {
		if err := m.setupMetricReporting(ctx); err != nil {
			return err
		}
	}

	if !m.WebhooksOnly {
		// Setup OOM Killer.
		logging.Infof(ctx, "setting up CRDB OOM killer")
		if err := m.mgr.Add(NewOOMKiller(m.ctl, m.watchNamespaces)); err != nil {
			return err
		}
	}

	logging.Info(ctx, "starting manager")
	if !m.InTestMode {
		// Start the controller manager on the foreground goroutine. It will stop
		// itself once the context is canceled.
		if err := m.mgr.Start(ctx); err != nil {
			return errors.Wrap(err, "unable to start manager")
		}
		// This runs only during a graceful shutdown (SIGTERM / context cancellation)
		// and not if there is a crash/startup failure, OOMKill/SIGKILL happens to the manager process.

		// We will also not run this during an upgrade or rolling restart.
		// However, there are scenarios such as pods taking too long to terminate or node crashes
		// during uninstallation that could skip this cleanup,
		// so we need an additional safety net to clean up stale resources.
		m.cleanupWebhookConfigs()
	}

	return nil
}

// setupMetricReporting creates watcher(s) that are used to report on metrics
// such as TenantsTotal.
func (m *Manager) setupMetricReporting(ctx context.Context) error {
	restClient, _, err := m.ctl.RestClientForObjType(&crdbv1beta1.CrdbTenant{})
	if err != nil {
		return err
	}

	go func() {
		var watcher watch.Interface

		defer func() {
			if watcher != nil {
				watcher.Stop()
			}
		}()

		// Determine which namespace(s) to watch for metrics reporting.
		// Single-namespace mode: watch only that namespace.
		// Multi-namespace or global mode: watch all namespaces.
		//
		// TODO(TREQ-1577): Fix multi-namespace scoped mode metric reporting.
		// Currently counts tenants across ALL namespaces instead of only watched
		// namespaces. Proper fix requires multiple watchers (one per namespace)
		// with aggregated metric reporting.
		watchNamespace := ""
		if len(m.watchNamespaces) == 1 {
			watchNamespace = m.watchNamespaces[0]
		}

		for {
			if watcher == nil {
				// If we are (re)establishing the watch, reset the metric as we'll re-count it
				// when the watch.Added event is triggered for all CrdbTenants.
				opmetrics.TenantsTotal.Set(0)
				if watchNamespace != "" {
					logging.Infof(ctx, "setting up metric watch for namespace: %q", watchNamespace)
				} else {
					logging.Info(ctx, "setting up metric watch for all namespaces")
				}
				watcher, err = restClient.Get().
					Resource("crdbtenants").
					Namespace(watchNamespace).
					VersionedParams(&v1.ListOptions{Watch: true}, v1.ParameterCodec).
					Watch(ctx)
				if err != nil {
					logging.Errorf(ctx, "failed to establish watch in setupMetricReporting: %v", err)
					if !kube.SleepContext(ctx, time.Second) {
						// Manager has been stopped.
						break
					}
					// Could be a transient error, try again.
					continue
				}
			}

			var event watch.Event
			select {
			case event = <-watcher.ResultChan():
				// New watcher event.
			case <-ctx.Done():
				// Manager has been stopped.
			}

			if ctx.Err() != nil {
				logging.Errorf(ctx, "watcher stopped: %v", ctx.Err())
				return
			}

			// If the watcher fails or is closed, log and try to re-establish the watch.
			if event.Type == watch.Error || event.Type == "" {
				logging.Errorf(ctx, "watcher closed or error: %v", event.Object)
				watcher.Stop()
				watcher = nil
				continue
			}

			switch event.Type {
			case watch.Added:
				opmetrics.TenantsTotal.Inc()
			case watch.Deleted:
				opmetrics.TenantsTotal.Dec()
			}
		}
	}()

	return nil
}

// registerManifests applies webhook configuration and custom resource
// definitions to the K8s cluster. Once this is done, K8s will understand the
// Operator's custom resources (CrdbCluster and CrdbTenant), and will invoke the
// webhook whenever they change.
//
// Webhook configuration behavior:
//   - Global mode (watchNamespaces empty): Uses fixed webhook names
//   - Namespace-scoped mode: Uses unique webhook names per deployment with namespace selectors

// TODO(andyk): This may not be compatible with OpenShift, which may not allow
// operators to install their own webhook. If that's true, then we'll have to
// figure out how to do this differently. The advantage to dynamically creating
// a webhook is that we hope to have the operator create its own CA, from which
// it can generate the webhook TLS certificate. That must happen before the
// webhook can be created, since the webhook needs to be initialized with the
// CA bundle.
// detectOperatorAppLabel reads the operator Deployment's pod template labels
// to determine the "app" label value. Falls back to OperatorAppLabel if not found.
func (m *Manager) detectOperatorAppLabel(ctx context.Context, ctl *kube.Ctl) string {
	deployment := &appsv1.Deployment{}
	err := ctl.Get(ctx, manifests.OperatorName, deployment)

	if err == nil && deployment.Spec.Template.Labels != nil {
		if appLabel, ok := deployment.Spec.Template.Labels["app"]; ok && appLabel != "" {
			logging.Infof(ctx, "Detected operator app label: %s", appLabel)
			return appLabel
		}
	}

	// Fallback for tests/local dev or if label not found
	logging.Infof(ctx, "Using default operator app label: %s", manifests.OperatorAppLabel)
	return manifests.OperatorAppLabel
}

func (m *Manager) registerManifests(ctx context.Context) error {
	// Create a single kube.Ctl instance to share between webhooks and CRDs
	// to reduce duplicate API calls during startup
	ctl, err := kube.NewCtl(m.restCfg)
	if err != nil {
		return err
	}
	ctl = ctl.WithNamespace(m.namespace)

	var caCert []byte
	// Register both webhooks and CRDs at startup
	if caCert, err = m.registerWebhooksWithCtl(ctx, ctl); err != nil {
		return err
	} else if err = m.registerCRDsWithCtl(ctx, ctl, caCert); err != nil {
		return err
	}
	return nil
}

func (m *Manager) registerWebhookCABundles(ctx context.Context) error {
	ctl := m.ctl.WithNamespace(m.namespace)
	caCert, err := m.registerWebhooksWithCtl(ctx, ctl)
	if err != nil {
		return err
	}
	return m.syncCRDConversionCABundle(ctx, caCert)
}

// validateCertDir validates that CertDir is non-empty and an absolute path.
// This validation is shared by registerWebhooksWithCtl and watchCertificates to avoid
// reading from unexpected locations (e.g., current working directory).
func (m *Manager) validateCertDir() error {
	if m.Options.CertDir == "" {
		return errors.New("CertDir is empty, cannot read certificates")
	}
	if !filepath.IsAbs(m.Options.CertDir) {
		return errors.Errorf("CertDir must be an absolute path, got: %s", m.Options.CertDir)
	}
	return nil
}

// registerWebhooksWithCtl applies only the webhook configuration using the provided kube.Ctl.
// This is shared by startup registration and certificate-change handling.
func (m *Manager) registerWebhooksWithCtl(ctx context.Context, ctl *kube.Ctl) ([]byte, error) {
	if err := m.validateCertDir(); err != nil {
		return nil, err
	}

	// Read the CA certificate from the mounted filesystem. This ensures the webhook
	// server and admission webhook configurations use the same certificate source
	// after certificate rotation.
	// The webhook server always reads certs from the filesystem (either mounted from
	// a secret or written by bootstrapOperatorCerts), so we must read the CA from
	// the same location to maintain consistency.
	//
	// Fall back to reading from the K8s secret if the file doesn't exist (e.g., in tests).
	caCertPath := filepath.Join(m.Options.CertDir, "ca.crt")
	caCert, err := os.ReadFile(caCertPath)
	if err != nil {
		if os.IsNotExist(err) {
			logging.Infof(ctx, "CA cert not found at %s, falling back to reading from K8s secret", caCertPath)
			caCert, _, err = crdbcerts.GetOperatorCertBundle(ctl)
			if err != nil {
				return nil, errors.Wrap(err, "error getting operator TLS secret")
			}
		} else {
			return nil, errors.Wrapf(err, "error reading CA certificate from %s", caCertPath)
		}
	}

	// Detect the operator app label from the running Deployment. This enables
	// backward compatibility: existing deployments keep their label, new
	// deployments can use a different label to differentiate from public operator.
	appLabel := m.detectOperatorAppLabel(ctx, ctl)

	// Register the webhook configuration with K8s.
	opts := manifests.WebhookOptions{
		CABundle:        caCert,
		Namespace:       m.namespace,
		WatchNamespaces: m.watchNamespaces,
		AppLabel:        appLabel,
	}
	if m.InLocalMode {
		opts.FailurePolicy = admissionregistrationv1.Ignore
	}

	webhookObjects := manifests.Webhook(opts)

	// Apply webhook manifests that include service, mutating, validating configs.
	// Overwrite is needed because server-defaulted fields on the live object
	// (e.g. matchPolicy: Equivalent) can conflict with the previous last-applied
	// state when the operator introduces an explicit value during upgrades.
	if err = ctl.ApplyWithOptions(ctx, kube.ApplyOptions{Overwrite: true}, webhookObjects...); err != nil {
		return nil, errors.Wrap(err, "error applying webhook manifest")
	}

	return caCert, nil
}

func (m *Manager) syncCRDConversionCABundle(ctx context.Context, caCert []byte) error {
	if !m.EnableMigrationController || !m.v1alpha1InStorage {
		return nil
	}

	clusterCRD := &apiextv1.CustomResourceDefinition{}
	if err := m.ctl.Get(ctx, "crdbclusters.crdb.cockroachlabs.com", clusterCRD); err != nil {
		return errors.Wrap(err, "failed to get CrdbCluster CRD")
	}

	conversion := clusterCRD.Spec.Conversion
	if conversion == nil || conversion.Strategy != apiextv1.WebhookConverter || conversion.Webhook == nil ||
		conversion.Webhook.ClientConfig == nil || conversion.Webhook.ClientConfig.Service == nil {
		return nil
	}

	service := conversion.Webhook.ClientConfig.Service
	if service.Name != manifests.WebhookServiceName || service.Namespace != m.namespace {
		return nil
	}
	if bytes.Equal(conversion.Webhook.ClientConfig.CABundle, caCert) {
		return nil
	}

	base := clusterCRD.DeepCopy()
	conversion.Webhook.ClientConfig.CABundle = caCert
	if err := m.ctl.Patch(ctx, clusterCRD, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return errors.Wrap(err, "failed to update CrdbCluster conversion webhook CA bundle")
	}
	return nil
}

// registerCRDsWithCtl applies only the custom resource definitions using the provided kube.Ctl.
// This is called at startup only (via registerManifests) and should not be called
// during certificate rotation to avoid unnecessary API server load.
func (m *Manager) registerCRDsWithCtl(ctx context.Context, ctl *kube.Ctl, caCert []byte) error {
	crds := manifests.CustomResourceDefinitions()

	clusterCRD := crds[0].(*apiextv1.CustomResourceDefinition)

	// Determine whether v1alpha1 objects still exist in etcd by checking the
	// CRD's status.storedVersions. This is a cluster-wide field managed by the
	// API server. It only shrinks when an admin manually patches it after
	// confirming all objects have been re-stored in a newer version.
	v1alpha1InStorage := false
	existingCRD := &apiextv1.CustomResourceDefinition{}
	if getErr := ctl.Get(ctx, clusterCRD.Name, existingCRD); getErr != nil {
		if !apierrors.IsNotFound(getErr) {
			return errors.Wrap(getErr, "failed to check existing CRD for v1alpha1 storedVersions")
		}
	} else {
		v1alpha1InStorage = slices.Contains(existingCRD.Status.StoredVersions, "v1alpha1")
	}
	m.v1alpha1InStorage = v1alpha1InStorage

	if err := configureCRDConversion(ctx, clusterCRD, existingCRD, m.EnableMigrationController, v1alpha1InStorage, caCert, m.namespace); err != nil {
		return err
	}

	// Don't use the last-applied-configuration annotation for CRDs, since the
	// configuration may be too large to fit into an annotation. If there's a
	// conflict with a previously written last-applied-configuration annotation,
	// ignore it.
	if err := ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoLastAppliedAnnotation: true, Overwrite: true}, crds...); err != nil {
		return errors.Wrap(err, "error applying crd manifests")
	}

	return nil
}

// configureCRDConversion sets the conversion strategy on the CrdbCluster CRD based
// on migration state. If v1alpha1 is not present in the CRD's storedVersions, it
// marks v1alpha1 as not served and returns early regardless of the migration flag.
// When migration is enabled and v1alpha1 objects exist, it configures a webhook
// converter. When disabled, it either preserves an existing webhook (if another
// operator handles conversion) or returns an error if v1alpha1 objects exist but
// no converter is available.
func configureCRDConversion(
	ctx context.Context,
	clusterCRD, existingCRD *apiextv1.CustomResourceDefinition,
	enableMigration, v1alpha1InStorage bool,
	caCert []byte,
	namespace string,
) error {
	// If no v1alpha1 objects exist in storage, there is nothing to convert.
	// Mark v1alpha1 as not served so stale clients get a clean error and no
	// one can accidentally create v1alpha1 objects.
	if !v1alpha1InStorage {
		if enableMigration {
			logging.Infof(ctx,
				"--enable-migration is set but no v1alpha1 objects found in CRD storedVersions; "+
					"skipping conversion webhook setup and marking v1alpha1 as not served")
		}
		markV1Alpha1NotServed(clusterCRD)
		return nil
	}

	if enableMigration {
		// v1alpha1 objects exist in storage. Configure the conversion webhook
		// so the API server can translate v1alpha1 requests to v1beta1.
		convertPath := "/convert"
		port := int32(443)
		clusterCRD.Spec.Conversion = &apiextv1.CustomResourceConversion{
			Strategy: apiextv1.WebhookConverter,
			Webhook: &apiextv1.WebhookConversion{
				ClientConfig: &apiextv1.WebhookClientConfig{
					CABundle: caCert,
					Service: &apiextv1.ServiceReference{
						Name:      manifests.WebhookServiceName,
						Namespace: namespace,
						Path:      &convertPath,
						Port:      &port,
					},
				},
				ConversionReviewVersions: []string{"v1"},
			},
		}
		return nil
	}

	// Migration is disabled but v1alpha1 objects still exist. Check whether
	// another operator instance is already managing the conversion webhook.
	// If so, preserve its config. If not, no one can serve v1alpha1 objects
	// correctly and the operator must not start.
	if existingCRD.Spec.Conversion != nil && existingCRD.Spec.Conversion.Strategy == apiextv1.WebhookConverter {
		logging.Warnf(ctx,
			"migration is disabled but CRD storedVersions still contains v1alpha1; "+
				"preserving existing conversion webhook from another operator instance")
		clusterCRD.Spec.Conversion = existingCRD.Spec.Conversion
		return nil
	}

	return errors.New(
		"CRD has v1alpha1 objects in storage (status.storedVersions contains \"v1alpha1\") " +
			"but no conversion webhook is configured. If migrating from the public operator, " +
			"start with --enable-migration. If upgrading from an earlier enterprise operator " +
			"version that used v1alpha1, remove the stale storedVersion entry first:\n" +
			"  kubectl patch crd crdbclusters.crdb.cockroachlabs.com --subresource=status " +
			"--type json -p '[{\"op\":\"replace\",\"path\":\"/status/storedVersions\",\"value\":[\"v1beta1\"]}]'",
	)
}

// markV1Alpha1NotServed sets Served=false on the v1alpha1 version entry so the
// API server rejects v1alpha1 requests instead of silently passing them through
// the None conversion strategy.
func markV1Alpha1NotServed(crd *apiextv1.CustomResourceDefinition) {
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Name == "v1alpha1" {
			crd.Spec.Versions[i].Served = false
			return
		}
	}
}

// addControllers adds the CrdbCluster and CrdbTenant controllers to the K8s
// controller manager.
func (m *Manager) addControllers(ctx context.Context) error {
	reconcilers := make(map[string]controllers.Reconciler)

	evictionHelper := &evictionutil.Helper{}

	if !m.SkipTenantController {
		reconcilers[crdbv1beta1.CrdbTenantKind] = &tenantctrl.Reconciler{
			Ctl:                m.ctl,
			MetricsManager:     m.scaler.MetricsManager(),
			NodeMetricsManager: m.scaler.NodeMetricsManager(),
			ScaleLockTable:     m.scaleLockTable,
			TenantPodSyncer:    m.tenantPodSyncer,
			NodeLocalityReader: m.readLocalityForNode,
		}
	}

	var instancesClient clusterctrl.InstancesClient
	if m.Environment.CloudProvider == cloud.GCP {
		var err error
		instancesClient, err = clusterctrl.NewGCPInstancesClient(ctx)
		if err != nil {
			return err
		}
	} else {
		instancesClient = &clusterctrl.NoopInstancesClient{}
	}

	reconcilers[crdbv1beta1.CrdbClusterKind] = &clusterctrl.Reconciler{
		Env:                       &m.Environment,
		Ctl:                       m.ctl,
		Scheme:                    m.mgr.GetScheme(),
		BetaReconciler:            betaclusterctrl.NewReconciler(&m.Environment, m.ctl, evictionHelper),
		Finalizer:                 clusterctrl.NewFinalizer(ctx),
		DisableTenantFinalization: m.DisableTenantFinalization,
		InstancesClient:           instancesClient,
	}
	reconcilers[crdbv1beta1.CrdbNodeKind] = nodectrl.NewReconciler(m.ctl, evictionHelper)

	// Helm migrations operate on StatefulSets and v1beta1 objects only, so the
	// migration controller must be registered whenever the flag is set regardless
	// of whether v1alpha1 objects exist.
	if m.EnableMigrationController {
		reconcilers["Migration"] = migrationctrl.NewReconciler(
			m.ctl,
			m.mgr.GetEventRecorderFor("migration-controller"),
			m.Environment.CloudRegion,
			m.v1alpha1InStorage,
		)
	}

	// Add the k8s Node controller for handling decommission annotations on k8s nodes.
	if m.EnableK8sNodeController {
		reconcilers["K8sNode"] = k8snodectrl.NewReconciler(m.ctl)
	}

	for kind, reconciler := range reconcilers {
		logging.Infof(context.Background(), "setting up manager for %s", kind)
		if err := reconciler.SetupWithManager(ctx, m.mgr); err != nil {
			return errors.Wrapf(err, "unable to create %s controller %T", kind, reconciler)
		}
	}

	return nil
}

// setupWebhook configures the HTTP server that K8s will call to validate CRD
// updates and trigger tenant resumption.
func (m *Manager) setupWebhook(ctx context.Context) {
	webhookServer := m.mgr.GetWebhookServer()

	// The skip-reconcile label is only needed when the public operator might
	// be running alongside us, which implies v1alpha1 objects in storage.
	webhookServer.Register("/mutate", &webhook.Admission{
		Handler: NewMutatingWebhook(m.EnableMigrationController && m.v1alpha1InStorage),
	})

	// Register the validating webhook endpoint.
	webhookServer.Register("/validate", &webhook.Admission{
		Handler: NewWebhook(m.ctl, m.Environment),
	})

	// The conversion endpoint translates v1alpha1 API requests to v1beta1. If
	// no v1alpha1 objects exist in storage, v1alpha1 is not served and the API
	// server will never call this endpoint.
	if m.EnableMigrationController && m.v1alpha1InStorage {
		webhookServer.Register("/convert", &conversion.Webhook{})
	}

	logging.Infof(ctx, "webhook server starting on port %d, with certs in %s",
		webhookServer.Port, webhookServer.CertDir)
}

// setupDebug configures HTTP endpoints for pprof.
func (m *Manager) setupDebug(ctx context.Context) error {
	webhook := m.mgr.GetWebhookServer()
	debugAddr := fmt.Sprintf("%s:%d", webhook.Host, manifests.OperatorDebugPort)
	l, err := net.Listen("tcp", debugAddr)
	if err != nil {
		return err
	}
	debugServer := &DebugServer{Listener: l}

	if err := m.mgr.Add(debugServer); err != nil {
		return errors.Wrap(err, "unable to register debug server")
	}

	logging.Infof(ctx, "debug server starting on port %d", manifests.OperatorDebugPort)
	return nil
}

// setupOperatorServer configures the operator GRPC server, which will be used
// by the tenantdir component for cold starts.
func (m *Manager) setupOperatorServer(ctx context.Context) error {
	listener, err := net.Listen("tcp", m.RpcAddr)
	if err != nil {
		return errors.Wrapf(err, "operator grpc server unable to listen on address %s", m.RpcAddr)
	}

	operatorServer := opserver.New(opserver.Options{
		Ctl:               m.ctl,
		Listener:          listener,
		CertDirectory:     manifests.OperatorCertsDir,
		CertKeyname:       manifests.OperatorCertName,
		ScaleLockTable:    m.scaleLockTable,
		MetricManager:     m.scaler.MetricsManager(),
		NodeMetricManager: m.scaler.NodeMetricsManager(),
		EventEmitter: kube.EventEmitter{
			EventRecorder: m.mgr.GetEventRecorderFor("cockroach-operator-server"),
		},
		TenantPodSyncer:    m.tenantPodSyncer,
		NodeLocalityReader: m.readLocalityForNode,
	})

	if err := m.mgr.Add(operatorServer); err != nil {
		return errors.Wrap(err, "unable to register operator server")
	}

	logging.Infof(ctx, "operator server starting on address '%s'", m.RpcAddr)
	return nil
}

// setupTenantPodSyncer configures the TenantPodSyncer component to keep the
// contents of the tenantpool pods up-to-date, while refreshing credentials
// periodically.
func (m *Manager) setupTenantPodSyncer(ctx context.Context) (tenant.TenantPodSyncer, error) {
	// This currently only supports a single host per Kubernetes cluster. If
	// we decide to support more than one in the future, this has to be updated.
	namespace := m.Environment.CloudRegion
	if namespace == "" {
		return nil, errors.New("CloudRegion must be supplied")
	}
	ctl := m.ctl.WithNamespace(namespace)

	credProvider, err := cloud.NewCredentialProvider(m.Environment)
	if err != nil {
		return nil, errors.Wrap(err, "initializing credential provider")
	}

	podSyncer := tenant.NewTenantPodSyncer(ctx, ctl, credProvider, m.readLocalityForNode)
	if err := m.mgr.Add(podSyncer); err != nil {
		return nil, errors.Wrap(err, "unable to register tenant pod syncer")
	}

	logging.Info(ctx, "registered tenant pod syncer")
	return podSyncer, nil
}

// readLocalityForNode reads the locality data from the Kubernetes node with the
// given name. This caches the localities to prevent additional calls to the
// API server on subsequent method calls.
func (m *Manager) readLocalityForNode(
	ctx context.Context, nodeName string,
) (cloud.LocalityInfo, error) {
	loc, ok := m.nodeLocalities.Load(nodeName)
	if ok {
		return *(loc.(*cloud.LocalityInfo)), nil
	}
	var node corev1.Node
	if err := m.ctl.Get(ctx, nodeName, &node); err != nil {
		return cloud.LocalityInfo{}, err
	}
	localityInfo := cloud.MakeLocalityInfoForNode(ctx, &node)
	m.nodeLocalities.Store(nodeName, &localityInfo)
	return localityInfo, nil
}

// bootstrapOperatorCerts generates self-signed TLS certificates for the operator
// webhook if they do not already exist, then writes them to the cert directory
// so the webhook server can load them at startup.
func (m *Manager) bootstrapOperatorCerts(ctx context.Context) error {
	ctl, err := kube.NewCtl(m.restCfg)
	if err != nil {
		return errors.Wrap(err, "failed to create kube client")
	}
	ctl = ctl.WithNamespace(m.namespace)

	certGenerator := certs.NewSelfSignedCertGenerator(ctl, m.namespace, "", "")
	if err := certGenerator.GenerateOperatorCerts(ctx); err != nil {
		return errors.Wrap(err, "failed to generate operator certificates")
	}

	// Read the cert bundle and write it to the filesystem for the webhook server.
	caCert, tlsBundle, err := crdbcerts.GetOperatorCertBundle(ctl)
	if err != nil {
		return errors.Wrap(err, "failed to get operator certificates from secret")
	}

	certDir := m.Options.CertDir
	if err := os.MkdirAll(certDir, 0700); err != nil {
		return errors.Wrapf(err, "failed to create cert directory %s", certDir)
	}
	if err := os.WriteFile(filepath.Join(certDir, "tls.crt"), tlsBundle.Certificate, 0600); err != nil {
		return errors.Wrap(err, "failed to write tls.crt")
	}
	if err := os.WriteFile(filepath.Join(certDir, "tls.key"), tlsBundle.PrivateKey, 0600); err != nil {
		return errors.Wrap(err, "failed to write tls.key")
	}
	if err := os.WriteFile(filepath.Join(certDir, "ca.crt"), caCert, 0600); err != nil {
		return errors.Wrap(err, "failed to write ca.crt")
	}

	logging.Infof(ctx, "operator certificates bootstrapped to %s", certDir)
	return nil
}

// watchCertificates monitors the CA certificate file and keeps webhook CA bundles
// in sync with the certificate that the webhook server is using.
//
// The webhook server uses controller-runtime's certwatcher which automatically reloads
// certificates when the mounted files change.
func (m *Manager) watchCertificates(ctx context.Context) error {
	if err := m.validateCertDir(); err != nil {
		return err
	}

	caCertPath := filepath.Join(m.Options.CertDir, "ca.crt")

	// Check if the cert file exists before watching. If it doesn't exist, we skip
	// watching since registerWebhooksWithCtl will fall back to reading from K8s secret.
	if _, err := os.Stat(caCertPath); err != nil {
		if os.IsNotExist(err) {
			logging.Infof(ctx, "CA cert file %s does not exist, skipping certificate watch", caCertPath)
			return nil
		}
		// Handle other stat errors (permission, IO issues, etc.) to ensure deterministic startup
		return errors.Wrapf(err, "failed to stat CA cert file %s", caCertPath)
	}

	watcher, err := m.setupCertWatcher()
	if err != nil {
		return err
	}

	logging.Infof(ctx, "Started watching %s for certificate changes", m.Options.CertDir)

	go m.runCertWatcher(ctx, watcher)

	return nil
}

// setupCertWatcher creates and configures a file system watcher for the certificate directory.
func (m *Manager) setupCertWatcher() (*fsnotify.Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, errors.Wrap(err, "failed to create file watcher")
	}

	// Watch the parent directory since Kubernetes uses atomic writes via symlinks.
	// The actual file is a symlink to ..data/ca.crt, and Kubernetes updates ..data
	// when the secret changes. Watching the directory catches these updates.
	certDir := m.Options.CertDir
	if err := watcher.Add(certDir); err != nil {
		watcher.Close()
		return nil, errors.Wrapf(err, "failed to watch cert directory %s", certDir)
	}

	return watcher, nil
}

// runCertWatcher runs the certificate watching loop in a goroutine.
// Uses trailing-edge debouncing: resets a timer on each relevant event and only
// triggers handleCertChange after a quiet period (no events for certDebounceDelay).
// This ensures we update after the LAST event in a burst, not the first, which is
// critical during Kubernetes atomic secret updates where intermediate events (e.g.,
// Remove of "..data" symlink) should not trigger premature updates.
func (m *Manager) runCertWatcher(ctx context.Context, watcher *fsnotify.Watcher) {
	defer watcher.Close()

	// debounceTimer and debounceTimerChan are used for trailing-edge debouncing.
	// We maintain both the timer and channel separately:
	// - debounceTimer allows us to stop/reset the timer when new events arrive
	// - debounceTimerChan is used in the select statement to wait for timer expiry
	// Both start as nil, which means select won't trigger on a nil channel initially.
	var debounceTimer *time.Timer
	var debounceTimerChan <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			// Context cancelled - clean up and exit gracefully
			if debounceTimer != nil {
				debounceTimer.Stop() // Prevent any pending timer from firing
			}
			logging.Info(ctx, "Certificate watcher stopped")
			return

		case event, ok := <-watcher.Events:
			// Channel closed - watcher is shutting down
			if !ok {
				return
			}

			// Filter out irrelevant events (e.g., Chmod)
			// Only process relevant cert events (Write, Create, Remove, Rename)
			if !m.isRelevantCertEvent(event) {
				continue
			}

			// Trailing-edge debounce: reset timer on each relevant event.
			// If a timer is already running from a previous event, stop it first.
			// This is key to trailing-edge behavior: we keep pushing out the timer
			// as long as events keep arriving, ensuring we only process when events stop.
			if debounceTimer != nil {
				debounceTimer.Stop() // Cancel the previous timer
			}
			// Start a new timer. The handler will only be called if this timer
			// expires without any new events arriving (i.e., after the burst ends).
			debounceTimer = time.NewTimer(m.certDebounceDelay)
			debounceTimerChan = debounceTimer.C

		case <-debounceTimerChan:
			// Check if context is canceled before processing.
			// When shutting down, both ctx.Done() and the timer can be ready,
			// and select chooses arbitrarily - avoid noisy logs/API calls during shutdown.
			if ctx.Err() != nil {
				debounceTimer = nil
				debounceTimerChan = nil
				return
			}

			// Timer expired - no events received for certDebounceDelay duration.
			// This means we've seen the final event in the burst, and it's safe to process.
			// At this point, any atomic updates (symlink swaps, etc.) should be complete.
			m.handleCertChange(ctx)

			// Reset timer state to nil so we don't process stale timer events.
			// A nil channel in select will never trigger, which is what we want.
			debounceTimer = nil
			debounceTimerChan = nil

		case err, ok := <-watcher.Errors:
			// Channel closed - watcher is shutting down
			if !ok {
				return
			}
			// Log the error but continue watching. fsnotify errors are typically
			// non-fatal (e.g., buffer overflows on very busy directories).
			logging.Errorf(ctx, "Certificate watcher error: %v", err)
		}
	}
}

// isRelevantCertEvent checks if an fsnotify event is relevant for certificate updates.
// Handles Write, Create, Remove, and Rename events, as Kubernetes' atomic secret updates
// can surface as any of these (symlink swaps). This follows controller-runtime's certwatcher pattern.
func (m *Manager) isRelevantCertEvent(event fsnotify.Event) bool {
	// We're interested in Write, Create, Remove, and Rename events on the cert directory.
	// Kubernetes updates happen as atomic writes which can manifest as:
	// - Create new file in ..data_tmp
	// - Rename/Remove old ..data symlink
	// - Create/Write new ..data symlink
	return event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0
}

// handleCertChange updates webhook CA bundles after detecting a certificate change.
// Uses exponential backoff retry to handle transient errors.
func (m *Manager) handleCertChange(ctx context.Context) {
	logging.Infof(ctx, "Detected certificate change in %s, updating webhook CA bundles", m.Options.CertDir)

	handler := m.certChangeHandler
	if handler == nil {
		handler = m.registerWebhookCABundles
	}

	// Retry with exponential backoff to handle transient errors (network issues,
	// temporary API server unavailability, etc.). This ensures eventual consistency
	// even if the first attempt fails.
	const maxRetries = 5
	backoff := 1 * time.Second
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		if err := handler(ctx); err != nil {
			lastErr = err
			logging.Warnf(ctx, "Failed to update webhook CA bundles (attempt %d/%d): %v", attempt, maxRetries, err)

			// Check if context was cancelled - don't retry if shutting down
			if ctx.Err() != nil {
				logging.Infof(ctx, "Context cancelled during webhook update, stopping retries")
				return
			}

			// Don't retry on the last attempt
			if attempt < maxRetries {
				logging.Infof(ctx, "Retrying webhook CA bundle update in %v", backoff)
				select {
				case <-time.After(backoff):
					backoff *= 2
				case <-ctx.Done():
					logging.Infof(ctx, "Context cancelled during backoff, stopping retries")
					return
				}
			}
		} else {
			// Success
			if attempt > 1 {
				logging.Infof(ctx, "Successfully updated webhook CA bundles (after %d attempts)", attempt)
			} else {
				logging.Info(ctx, "Successfully updated webhook CA bundles")
			}
			return
		}
	}

	// All retries exhausted
	logging.Errorf(ctx, "Failed to update webhook CA bundles after %d attempts, giving up: %v", maxRetries, lastErr)
}

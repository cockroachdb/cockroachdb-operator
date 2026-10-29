package main

import (
	"flag"
	"math"
	"os"
	"strings"
	"time"

	"github.com/cockroachlabs/cockroach-enterprise-operator/buildinfo"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manager"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
)

func main() {
	var printVersion, skipTenantController, enableK8sNodeController, disableTenantFinalization, enableMigration bool
	var webhookOnly, disableLeaderElection, localMode bool
	var selfSignedOperatorCerts bool
	var metricsAddr, certsDir, rpcAddr string
	var resyncPeriod time.Duration
	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "Address the metric endpoint binds to.")
	flag.StringVar(
		&rpcAddr,
		"rpc-addr",
		"",
		"The address the rpc endpoint binds to. The RPC server is only started if rpc-addr != \"\".",
	)
	flag.StringVar(&certsDir, "certs-dir", manifests.OperatorCertsDir, "Location of TLS certificates used by the webhook.")
	flag.BoolVar(&printVersion, "version", false, "Prints version information")

	// 30 days is longer than our typical release cycle, so on average there are
	// no periodic reconciles.
	//
	// Note: there is still a reconcile when the operator starts up. That
	// reconcile is needed to avoid dropping updates that occur before the
	// operator starts its watch.
	flag.DurationVar(
		&resyncPeriod,
		"resync-period",
		time.Hour*24*30,
		"How long between periodic resyncs. Defaults to once per 30 days.",
	)
	flag.BoolVar(
		&skipTenantController,
		"skip-tenant-controller",
		false,
		"Don't run tenant controller. This should only be true for dedicated clusters. Defaults to false.",
	)
	flag.BoolVar(
		&enableK8sNodeController,
		"enable-k8s-node-controller",
		false,
		"Enable k8s node controller. This should be true for clusters utilizing k8s node decommission feature. Defaults to false.",
	)
	flag.BoolVar(
		&enableMigration,
		"enable-migration",
		false,
		"Enables migration for automated Public Operator CR and StatefulSets to CockroachDB operator migration. Defaults to false.",
	)
	flag.BoolVar(
		&disableTenantFinalization,
		"disable-tenant-finalization",
		false,
		"Don't perform finalization of tenants. Defaults to false.",
	)
	flag.BoolVar(
		&webhookOnly,
		"webhook-only",
		false,
		"Run only the webhook server (mutate/validate/convert) without starting controllers or leader election. "+
			"Useful to keep webhooks running in-cluster while running controllers locally for debugging.",
	)
	flag.BoolVar(
		&disableLeaderElection,
		"disable-leader-election",
		false,
		"Disable leader election. Recommended for running the operator locally against a remote cluster "+
			"(ensure only one non-webhook-only operator instance is running).",
	)
	flag.BoolVar(
		&localMode,
		"local-mode",
		false,
		"Run in local mode. Enables local debugging features like ignoring webhook validation. "+
			"Can also be set via LOCAL_MODE env var.",
	)
	flag.BoolVar(
		&selfSignedOperatorCerts,
		"self-signed-operator-certs",
		false,
		"Automatically generate self-signed TLS certificates for the operator webhook "+
			"(cockroach-operator-certs Secret). Disable when certificates are managed externally.",
	)
	flag.Parse()

	if printVersion {
		buildinfo.Print("cockroach-operator")
		return
	}

	ctx := ctrl.SetupSignalHandler()

	logging.SetGlobalLogger(logging.NewLogger())
	// Log the hostname/pod name so it can be used to lookup out the pod when
	// needed to debug. Having the pod name in the log eliminates the need for
	// the lookup. For local staging clusters/integration tests having the pod
	// eliminates the need to look through every operator pod.
	hostName, err := os.Hostname()
	if err != nil {
		logging.Fatalf(ctx, "unable to get host name: %v", err)
	}
	logging.Infof(ctx, "Hostname: %s", hostName)
	logging.Infof(ctx, "Operator version: %s, revision: %s", buildinfo.OperatorVersion(), buildinfo.Revision())
	restCfg := ctrl.GetConfigOrDie()
	restCfg.QPS, restCfg.Burst = 1000, 1000
	logging.Infof(ctx, "QPS %.f, Burst %d, RateLimiter %#v", restCfg.QPS, restCfg.Burst, restCfg.RateLimiter)
	namespace := os.Getenv("NAMESPACE")

	// Parse WATCH_NAMESPACE for namespace-scoped mode. Unset or empty means
	// global mode (watch all namespaces).
	// K8s allows watching namespaces that don't exist yet, the operator will start reconciling once they're created.
	var watchNamespaces []string
	if watchNS := os.Getenv("WATCH_NAMESPACE"); watchNS != "" {
		seen := make(map[string]bool)
		for ns := range strings.SplitSeq(watchNS, ",") {
			ns = strings.TrimSpace(ns)
			if ns == "" {
				continue
			}
			if seen[ns] {
				logging.Warnf(ctx, "Duplicate namespace in WATCH_NAMESPACE: %s", ns)
				continue
			}
			// Validate namespace name is a valid DNS label.
			if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
				logging.Fatalf(ctx, "Invalid namespace name in WATCH_NAMESPACE: %s (errors: %v)", ns, errs)
			}
			seen[ns] = true
			watchNamespaces = append(watchNamespaces, ns)
		}
		if len(watchNamespaces) == 0 {
			logging.Fatal(ctx, "WATCH_NAMESPACE set but no valid namespaces found")
		}
		logging.Infof(ctx, "Namespace scoping enabled: watching %v", watchNamespaces)
	} else {
		logging.Info(ctx, "Watching all namespaces (global mode)")
	}

	mgr := manager.New(restCfg, namespace, watchNamespaces)

	if (math.MaxInt64/100)*89 < int64(resyncPeriod) {
		logging.Fatal(ctx, "setting --resync-period to a value near MaxInt64 risks integer overflows")
	}

	if os.Getenv("LOCAL_MODE") == "true" {
		localMode = true
	}

	if localMode {
		mgr.InLocalMode = true
	}

	// Derive manager options from command line flags.
	mgr.Options.MetricsBindAddress = metricsAddr
	mgr.Options.CertDir = certsDir
	mgr.Options.SyncPeriod = &resyncPeriod
	mgr.RpcAddr = rpcAddr
	mgr.SkipTenantController = skipTenantController
	mgr.DisableTenantFinalization = disableTenantFinalization
	mgr.EnableK8sNodeController = enableK8sNodeController
	mgr.EnableMigrationController = enableMigration
	mgr.WebhooksOnly = webhookOnly
	mgr.DisableLeaderElection = disableLeaderElection
	mgr.SelfSignedOperatorCerts = selfSignedOperatorCerts

	// Derive cloud provider information from environment variables. If
	// NODE_NAME is not set, then default to the "unknown" cloud provider and
	// region. Only single-region clusters would be supported in that case. This
	// is also useful when running the operator from the command-line during
	// development.
	nodeName := os.Getenv("NODE_NAME")
	if nodeName != "" {
		ctl, err := kube.NewCtl(restCfg)
		if err != nil {
			logging.Fatalf(ctx, "unable to create kube.Ctl: %v", err)
		}
		env, err := cloud.GetEnvironmentForNode(ctx, ctl, nodeName)
		if err != nil || env.CloudRegion == "" {
			env.CloudRegion = os.Getenv("CLOUD_REGION")
		}
		mgr.Environment = env

		var node corev1.Node
		if err = ctl.Get(ctx, nodeName, &node); err != nil {
			logging.Fatalf(ctx, "unable to get node %s: %v", nodeName, err)
		}
		// This is just the node name. It is useful to have in the logs as we
		// run multiple instances of the operator and the active one may switch
		// when doing upgrades etc. Also - if you know the node name, you can
		// easily SSH to the node, ps the process id and inspect various things
		// about the container.
		logging.Infof(ctx, "Node provider ID: %s", node.Spec.ProviderID)
		logging.Infof(ctx, "Resolved cloud environment: provider=%s, region=%q", env.CloudProvider, env.CloudRegion)

		// The region has to be either derived from the node's metadata, or passed
		// in via env var. It's possible for this to be empty if there's an
		// transient error with getting the node's metadata
		if env.CloudRegion == "" {
			logging.Fatalf(ctx,
				"no cloud region resolved for node %s: neither the %q label nor CLOUD_REGION is set",
				nodeName, corev1.LabelTopologyRegion)
		}
	}

	// Run the manager.
	if err := mgr.Run(ctx); err != nil {
		logging.Fatalf(ctx, "unable to start manager: %v", err)
	}
}

package clusterctrl

import (
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// hostResourcesWaitTimeout is the max amount of time that the Apply operation
// for the host resources will wait before returning a timeout error.
const hostResourcesWaitTimeout = 15 * time.Minute

// DeployHostResources will install additional resources only needed by a
// tenant host cluster, such as the SQL proxy and tenant pools.
func (h *requestHandler) DeployHostResources() error {
	// Don't deploy anything on non-host clusters.
	if !h.cluster.Spec.IsTenantHost() {
		return nil
	}

	ownerRef, refErr := kube.OwnerReferenceFor(h.ctl.Scheme(), h.cluster)
	if refErr != nil {
		return refErr
	}
	ownerRef.Controller = new(true)
	ownerRefs := []metav1.OwnerReference{ownerRef}

	networkPlugin, err := cloud.GetNetworkPluginForCluster(h.ctx, h.ctl)
	if err != nil {
		logging.Errorf(h.ctx, "failed to lookup network plugin for cluster: %v", err)
		return err
	}

	// deploySqlProxy does a non blocking apply for the SQL Proxy.
	if err := h.deploySqlProxy(ownerRefs, networkPlugin); err != nil {
		return err
	}

	if h.isKVReconciling() {
		// Wait for kv reconciliation first.
		// This is important especially if we are upgrading versions.
		// Also this check seems to prevent a "bug" where the synchronous
		// `Apply` below makes the reconciler miss events.
		// TODO(ye): investigate the cause of the "bug".
		return nil
	}
	// If this is a host cluster, reconcile all host related services.
	// (SQLProxy, TenantPools, etc). If updating the cockroach image on
	// tenant pools, set the Upgrade action.
	setUpgrade, err := h.checkPoolsNeedUpgrade()
	if err != nil {
		logging.Errorf(h.ctx, "failed to determine if default pools are being upgraded: %v", err)
		return err
	}
	if setUpgrade {
		logging.Info(h.ctx, "setting upgrade action to starting")
		h.cluster.Status.SetAction(crdbv1beta1.Upgrade, crdbv1beta1.Starting)
		if err := h.ctl.UpdateStatus(h.ctx, h.cluster); err != nil {
			logging.Errorf(h.ctx, "failed to apply CRDB cluster status update: %v", err)
			return err
		}
	}

	// Set owner references of these services to the CrdbCluster CRD, so
	// they get automatically garbage collected by kubernetes when the CRD
	// is deleted.
	// TODO(chrisseto): TenantPools removed from the host cluster's tenant
	// pool list won't get cleaned up until the CrdbCluster is deleted.
	// Ideally, removed tenant pools should be cleaned up whenever they are
	// removed from the list. If a pool is removed and then re-added, it
	// will just be overridden, there's no correctness bugs here.
	tenantPools := kube.SetOwnerReferences(
		ownerRefs,
		manifests.TenantPools(&h.cluster.Spec, h.reconciler.Env, networkPlugin)...,
	)

	// Apply tenant pool resources with a custom backoff. We want a longer timeout
	// here since there could be a large number of tenantpools and the default value
	// of 5 minutes may not be enough.
	//
	// TODO(jay): Have a custom wait timeout that depends on the number of tenantpools
	tenantPoolApplyOpts := kube.ApplyOptions{
		// This is deliberate as we always want the latest version of manifests
		// from the operator.
		Overwrite: true,
		WaitForOptions: kube.WaitForOptions{
			// Use IsQuiescent here since some of the host cluster resources
			// (e.g. tenantpool) does a RollingUpdate strategy, which isn't handled
			// by IsReady. We want to be sure that all tenantpool pods have been
			// updated before returning since the next step (Upgrade) relies on this
			// invariant. A RollingUpdate strategy is used to ensure that we don't
			// end up with no pods to stamp whenever an update is in progress.
			DoneFunc: kube.IsQuiescent,
			BackOff:  kube.NewCtlDefaultBackOffFn(hostResourcesWaitTimeout),
		},
	}
	// TODO(cc-intrusion): Consider setting NoWait=true when applying these
	// resources. We should never block a reconciliation goroutine on a
	// potentially long-running operation.
	//
	// See CC-5697.
	if err := h.ctl.ApplyWithOptions(h.ctx, tenantPoolApplyOpts, tenantPools...); err != nil {
		logging.Errorf(h.ctx, "failed to apply tenant pool resources: %v", err)
		return err
	}

	return nil
}

// deploySqlProxy deploys the resources for the SQL Proxy. NoWait is used when applying
// as the sqlproxy deployment can take a long time since it waits up to an hour
// for connections to drop, and we don't want to block the rest of the reconciliation process.
func (h *requestHandler) deploySqlProxy(
	ownerRefs []metav1.OwnerReference, networkPlugin cloud.NetworkPlugin,
) error {
	k8sAPIServerIPs, err := kube.GetKubernetesAPIServerIPs(h.ctx, h.ctl)
	if err != nil {
		logging.Errorf(h.ctx, "failed to lookup kubernetes API server IPs: %v", err)
		return err
	}

	// Apply the config maps if either of the sqlproxy-config or
	// sqlproxy-allowlist config maps are missing.
	//
	// TODO(platform): in the longer term, we should put this data into
	// the CrdbCluster spec and declaratively generate the config maps,
	// and get rid of this flag.
	var applyConfigMaps bool
	var sqlProxyConfigMap corev1.ConfigMap
	if err := h.ctl.Get(h.ctx, manifests.SQLProxyConfigMapName, &sqlProxyConfigMap); err != nil {
		if kubeerrors.IsNotFound(err) {
			applyConfigMaps = true
		} else {
			return err
		}
	}
	// This is wrapped in an if condition to prevent an unnecessary back
	// and forth with the K8s API server if we already know we need to
	// apply the config maps.
	if !applyConfigMaps {
		var sqlProxyAllowlistConfigMap corev1.ConfigMap
		if err := h.ctl.Get(h.ctx, manifests.SQLProxyAllowlistConfigMapName, &sqlProxyAllowlistConfigMap); err != nil {
			if kubeerrors.IsNotFound(err) {
				applyConfigMaps = true
			} else {
				return err
			}
		}
	}

	sqlProxyResources := kube.SetOwnerReferences(ownerRefs, manifests.SqlProxyResources(manifests.SqlProxyOptions{
		Spec:                   &h.cluster.Spec,
		KubernetesAPIServerIPs: k8sAPIServerIPs,
		NetworkPlugin:          networkPlugin,
		ApplyConfigMaps:        applyConfigMaps,
	})...)
	sqlProxyApplyOpts := kube.ApplyOptions{
		Overwrite: true,
		NoWait:    true,
	}
	if err := h.ctl.ApplyWithOptions(h.ctx, sqlProxyApplyOpts, sqlProxyResources...); err != nil {
		logging.Errorf(h.ctx, "failed to apply host resources: %v", err)
		return err
	}

	return nil
}

// checkPoolsNeedUpgrade compares the cockroach image specified on tenant pools with
// the image used by the pool deployments. If any pool has an image that does not match
// then this will return true.
func (h *requestHandler) checkPoolsNeedUpgrade() (bool, error) {
	for _, tenantPool := range h.cluster.Spec.TenantHost.TenantPools {
		specImage := h.cluster.Spec.Template.Spec.Image
		if tenantPool.CockroachImage != "" {
			specImage = tenantPool.CockroachImage
		}

		var poolDeployment appsv1.Deployment
		if err := h.ctl.Get(h.ctx, manifests.MakeTenantPoolName(tenantPool.Name), &poolDeployment); err != nil {
			// Pool deployments are not created yet.
			// TODO(alyshan): Implement and use an Initialized condition for CrdbCluster instead.
			if kubeerrors.IsNotFound(err) {
				return false, nil
			}
			return false, errors.Newf("failed to get deployment for tenant pool: %v", tenantPool.Name)
		}

		for _, container := range poolDeployment.Spec.Template.Spec.Containers {
			if container.Name == manifests.CockroachDB && container.Image != specImage {
				return true, nil
			}
		}
	}
	return false, nil
}

func (h *requestHandler) isKVReconciling() bool {
	err := kube.IsQuiescent(h.cluster)
	if err != nil {
		logging.Infof(h.ctx, "cluster is not quiescent: %v", err)
		return true
	}
	return false
}

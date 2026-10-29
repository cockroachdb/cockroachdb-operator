package manager

import (
	"context"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/tenantctrl"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/gofrs/uuid"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// NewWebhook constructs a new Webhook instance and provides a method to inject
// a ctl instance.
func NewWebhook(ctl *kube.Ctl, env cloud.Environment) *Webhook {
	return &Webhook{
		ctl: ctl,
		env: env,
	}
}

// Webhook implements a K8s validating admission plug-in, which validates
// incoming K8s API requests that attempt to modify any of the CRDB custom
// resources, such as CrdbCluster or CrdbTenant. It is also used to trigger
// low-latency resumption of suspended tenants.
type Webhook struct {
	decoder *admission.Decoder
	ctl     *kube.Ctl
	env     cloud.Environment
}

// Handle is called by K8s when a K8s API request needs to be validated. If the
// request is not valid, then Handle returns an error, and K8s will reject the
// API request rather than persisting it to etcd.
func (h *Webhook) Handle(ctx context.Context, req admission.Request) admission.Response {
	// Don't validate requests to delete objects.
	if req.Operation == admissionv1.Delete {
		return admission.Allowed("OK")
	}

	switch req.Kind.Kind {
	// TODO(chrisseto): enforce a limitation of 1 host cluster per namespace.
	// TODO(chrisseto): prevent deletions of TenantPools that are still in use.
	case "CrdbCluster":
		cluster, err := h.decodeCluster(req)
		if err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}

		// Skip validation for clusters in Disabled mode. These have incomplete specs
		// from conversion defaults and should not be validated until the migration
		// controller sets Mode=MutableOnly, which is validated normally.
		if h.isMigrationBypass(ctx, req, &cluster) {
			return admission.Allowed("OK")
		}

		// Perform validation checks on the cluster spec.
		if cluster.Spec.IsTenantHost() && !cluster.Spec.TLSEnabled {
			err := errors.New("host clusters (TenantHost!=nil) do not support insecure clusters (TLSEnabled=false)")
			return admission.Errored(http.StatusBadRequest, err)
		}

		if cluster.Spec.IsTenantHost() {
			// Ensure that tenant pools don't have conflicting names.
			poolNames := map[string]struct{}{}
			for _, pool := range cluster.Spec.TenantHost.TenantPools {
				if _, ok := poolNames[pool.Name]; ok {
					err := errors.Newf("duplicate tenant pool %s", pool.Name)
					return admission.Errored(http.StatusBadRequest, err)
				}
				poolNames[pool.Name] = struct{}{}

				if pool.HostReplicaMultiplier != nil && *pool.HostReplicaMultiplier < 0 {
					err := errors.Newf("HostReplicaMultiplier has to be non-negative (>= 0)")
					return admission.Errored(http.StatusBadRequest, err)
				}
			}
		}

		clusterCerts := cluster.Spec.Template.Spec.Certificates
		if cluster.Spec.TLSEnabled {
			// TLS clusters require exactly one certificate source.
			if !clusterCerts.SelfSigned && clusterCerts.ExternalCertificates == nil {
				err := errors.New("one of selfSigned or externalCertificates must be provided when tlsEnabled is true")
				return admission.Errored(http.StatusBadRequest, err)
			}
			if clusterCerts.SelfSigned && clusterCerts.ExternalCertificates != nil {
				err := errors.New("cannot set both selfSigned and externalCertificates")
				return admission.Errored(http.StatusBadRequest, err)
			}
			if clusterCerts.ExternalCertificates != nil {
				if err := clusterCerts.ExternalCertificates.Validate(); err != nil {
					return admission.Errored(http.StatusBadRequest, err)
				}
			}
		} else if clusterCerts.ExternalCertificates != nil || clusterCerts.SelfSigned {
			// Insecure clusters must not have certificates configured as they would
			// be silently ignored and could cause confusing pod failures if the
			// referenced secrets don't exist.
			err := errors.New("certificates must not be configured when tlsEnabled is false")
			return admission.Errored(http.StatusBadRequest, err)
		}

		foundThisRegion := false
		for i := range cluster.Spec.Regions {
			region := &cluster.Spec.Regions[i]
			if region.Code == h.env.CloudRegion {
				foundThisRegion = true
				break
			}
		}

		if !foundThisRegion {
			return admission.Errored(http.StatusBadRequest,
				errors.Newf("Cannot find the region %s for reconciliation. "+
					"Please fill in the correct region code.",
					h.env.CloudRegion))
		}

		if err := validateDataStore(cluster.Spec.Template.Spec.DataStore); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}

		if cluster.Spec.Template.Spec.DataStore.IsBasalt() {
			// EncryptionAtRest is deliberately unvalidated here. A CRL-owned KMS
			// key and a customer's have the same shape in this spec, so there is
			// no "reject CMEK on basalt" rule to express.

			// Basalt clusters are regional and the operator creates a single
			// tenant against one set of controllerAddrs, so multi-region basalt
			// is not supported yet.
			// TODO(prod-orch): support multi-region basalt with per-region
			// controllers and tenant creation, then remove this guard.
			if len(cluster.Spec.Regions) > 1 {
				return admission.Errored(http.StatusBadRequest,
					errors.New("basalt: multi-region clusters are not yet supported with basalt storage"))
			}

			// WAL failover derives its volume from the data PVC template, which
			// basalt does not create. Allowing both would dereference a nil
			// template and panic during pod construction.
			// TODO(prod-orch): support a basalt-compatible WAL failover path,
			// then remove this guard.
			if cluster.Spec.Template.Spec.WALFailoverSpec != nil {
				return admission.Errored(http.StatusBadRequest,
					errors.New("basalt: WAL failover is not yet supported with basalt storage"))
			}

			// Multi-store is not yet supported. The directory layout and
			// decommission path need more work before we can safely run
			// multiple stores per node. For now, we only reject on create so the
			// immutability check handles updates for existing clusters.
			// TODO(prod-orch): remove this guard once multi-store is ready.
			if req.Operation == admissionv1.Create && cluster.Spec.Template.Spec.DataStore.Basalt.GetStoreCount() > 1 {
				return admission.Errored(http.StatusBadRequest,
					errors.New("basalt: multiple stores per node are not yet supported (storeCount must be 1)"))
			}

			// The operator generates --store flags for basalt clusters.
			// So we reject any User supplied store flag for Basalt enabled clusters,
			// as they would conflict with or duplicate the generated paths (being defensive to not support multi-store).
			if sf := cluster.Spec.Template.Spec.StartFlags; sf != nil {
				for _, f := range sf.Upsert {
					if f == "--store" || strings.HasPrefix(f, "--store=") {
						return admission.Errored(http.StatusBadRequest,
							errors.New("basalt: --store flags are generated by the operator and cannot be set via startFlags.upsert"))
					}
				}
				for _, f := range sf.Omit {
					if f == "--store" || strings.HasPrefix(f, "--store=") {
						return admission.Errored(http.StatusBadRequest,
							errors.New("basalt: --store flags are generated by the operator and cannot be omitted via startFlags.omit"))
					}
				}
			}
		}

		// Todo(nishanth): remove this once flags is deprecated.
		if cluster.Spec.Template.Spec.StartFlags != nil && len(cluster.Spec.Template.Spec.Flags) > 0 {
			err := errors.New("cannot set both Flags and StartFlags; use StartFlags only as Flags is deprecated")
			return admission.Errored(http.StatusBadRequest, err)
		}

		nodeSpec := cluster.Spec.Template.Spec
		newLoggingCM := nodeSpec.LoggingConfigMapName

		// Validate encryption at rest field consistency across regions.
		for i := range cluster.Spec.Regions {
			ear := cluster.Spec.Regions[i].EncryptionAtRest
			if ear == nil {
				continue
			}
			if err := validateEncryptionAtRest(ear); err != nil {
				return admission.Errored(http.StatusBadRequest, err)
			}
		}

		// Validate WAL failover configuration changes during updates
		if req.Operation == admissionv1.Update {
			oldCluster, err := h.decodeClusterRaw(req)
			if err != nil {
				return admission.Errored(http.StatusBadRequest, errors.Wrap(err, "failed to decode old cluster object"))
			}

			// Check if WAL failover configuration has changed
			oldWALSpec := oldCluster.Spec.Template.Spec.WALFailoverSpec
			newWALSpec := cluster.Spec.Template.Spec.WALFailoverSpec

			// Compare WAL failover specifications
			if !walFailoverSpecsEqual(oldWALSpec, newWALSpec) {
				err := errors.New("Name field of WAL failover configuration cannot be modified after cluster creation")
				return admission.Errored(http.StatusBadRequest, err)
			}

			// The storage backend is immutable. Switching between basalt and a
			// local volume would orphan the old data and bring nodes up on an
			// empty store.
			oldBackend := dataStoreBackend(oldCluster.Spec.Template.Spec.DataStore)
			newBackend := dataStoreBackend(cluster.Spec.Template.Spec.DataStore)
			if oldBackend != newBackend {
				return admission.Errored(http.StatusBadRequest,
					errors.Newf("data store backend is immutable: cannot change from %s to %s", oldBackend, newBackend))
			}

			// TenantID is immutable. The control plane creates the basalt tenant
			// before it creates the cluster, so changing it here would point nodes at a
			// tenant that may not exist and would strand the old tenant's data.
			oldBasalt := oldCluster.Spec.Template.Spec.DataStore.Basalt
			newBasalt := cluster.Spec.Template.Spec.DataStore.Basalt
			if oldBasalt != nil && newBasalt != nil && oldBasalt.TenantID != newBasalt.TenantID {
				return admission.Errored(http.StatusBadRequest,
					errors.New("basalt: tenantID is immutable after cluster creation"))
			}

			// Whether a basalt cluster is encrypted is fixed at creation. Adding a
			// key to one that already wrote plaintext leaves the range-shared
			// filesystem fail-closed on those files, and removing the key from an
			// encrypted one makes its data unreadable. Both directions brick the
			// cluster on the next pod restart, so reject the update instead of
			// letting an operator rollback or a control-plane bug do it silently.
			//
			// Scoped to basalt deliberately. A volume-backed cluster legitimately
			// gains and loses EncryptionAtRest as CMEK is enabled and disabled,
			// and its store-local registry tolerates files that predate
			// encryption, so the same immutability must not be imposed there.
			//
			// The backend check above already rejected a change of store type, so
			// an old basalt store implies a new one.
			if oldBasalt != nil {
				oldEAREnabled := basaltEARConfigured(oldCluster)
				newEAREnabled := basaltEARConfigured(cluster)
				if oldEAREnabled != newEAREnabled {
					return admission.Errored(http.StatusBadRequest,
						errors.Newf("basalt: encryptionAtRest is immutable after cluster creation (enabled=%t -> %t)",
							oldEAREnabled, newEAREnabled))
				}
			}

			// StoreCount is immutable. Changing it would leave orphaned store
			// directories in basalt or point nodes at directories that don't exist.
			// TODO(prod-orch): lift this restriction once per-store decommission is
			// implemented and store directories can be safely added or removed.
			if oldBasalt != nil && newBasalt != nil &&
				oldBasalt.GetStoreCount() != newBasalt.GetStoreCount() {
				return admission.Errored(http.StatusBadRequest,
					errors.New("basalt: storeCount is immutable after cluster creation"))
			}

			// Prevent removing logsStore while loggingConfigMapName is still set.
			// Without logsStore, there is no writable volume at the custom log path
			// specified in the logging ConfigMap, which will cause CockroachDB to
			// crash on startup with "read-only file system" error.
			oldLogsStore := oldCluster.Spec.Template.Spec.LogsStore
			newLogsStore := cluster.Spec.Template.Spec.LogsStore
			if oldLogsStore != nil && newLogsStore == nil && newLoggingCM != "" {
				return admission.Errored(http.StatusBadRequest,
					errors.Newf("cannot remove logsStore while loggingConfigMapName %q is still set; "+
						"to remove logsStore, unset loggingConfigMapName to fall back to default logging behavior",
						newLoggingCM))
			}
		}

		// Validate that the logging ConfigMap exists when referenced.
		// If it doesn't exist, the pod will fail to start with a volume mount error.
		if newLoggingCM != "" {
			ctl := h.ctl.WithNamespace(cluster.Namespace)
			var cm corev1.ConfigMap
			if err := ctl.Get(ctx, newLoggingCM, &cm); err != nil {
				if kubeerrors.IsNotFound(err) {
					return admission.Errored(http.StatusBadRequest,
						errors.Newf("logging ConfigMap %q referenced by loggingConfigMapName not found", newLoggingCM))
				}
				return admission.Errored(http.StatusInternalServerError,
					errors.Wrapf(err, "failed to fetch logging ConfigMap %q", newLoggingCM))
			}
		}

		// When LogsStore is specified, exactly one of VolumeClaimTemplate or VolumeSource must be set.
		if nodeSpec.LogsStore != nil && nodeSpec.LogsStore.VolumeClaimTemplate == nil && nodeSpec.LogsStore.VolumeSource == nil {
			return admission.Errored(http.StatusBadRequest,
				errors.New("logsStore: one of VolumeClaimTemplate or VolumeSource must be specified"))
		}

		// When LogsStore is configured with a custom logging ConfigMap, require LogsStore.MountPath
		// to be explicitly set. The logging ConfigMap may specify a custom file-defaults.dir,
		// and the user must set mountPath to match so the dedicated logs volume is
		// mounted where CockroachDB writes logs.
		if nodeSpec.LogsStore != nil && newLoggingCM != "" && nodeSpec.LogsStore.MountPath == "" {
			return admission.Errored(http.StatusBadRequest,
				errors.Newf("logsStore.mountPath must be set when both logsStore and loggingConfigMapName are configured; "+
					"set logsStore.mountPath to match the file-defaults.dir value in the logging ConfigMap %q",
					newLoggingCM))
		}

	case crdbv1beta1.CrdbTenantKind:
		tenant, err := h.decodeTenant(req)
		if err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}

		return h.handleTenant(ctx, req, tenant)
	}

	return admission.Allowed("OK")
}

func (h *Webhook) handleTenant(
	ctx context.Context, req admission.Request, tenant crdbv1beta1.CrdbTenant,
) admission.Response {
	if tenant.Name != tenant.Spec.TenantID.Tag() {
		err := errors.Newf("tenant name must be 'tenant-%d', not '%s'",
			tenant.Spec.TenantID, tenant.Name)
		return admission.Errored(http.StatusBadRequest, err)
	}

	// If Reconciliation is disabled, short circuit so tests don't have to
	// create host clusters in order to create tenants.
	if _, ok := tenant.Annotations[tenantctrl.DisableReconcile]; ok {
		return admission.Allowed("OK")
	}

	ctl := h.ctl.WithNamespace(tenant.Namespace)

	// Pull out all CrdbClusters in the namespace of this tenant so we can
	// validate that a host cluster and the requested tenantpool exists.
	var clusters crdbv1beta1.CrdbClusterList
	if err := ctl.List(ctx, &kube.ListOptions{}, &clusters); err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	var hostCluster *crdbv1beta1.CrdbCluster
	for _, cluster := range clusters.Items {
		if cluster.Spec.IsTenantHost() {
			hostCluster = &cluster
			// Break out after finding a host cluster as we currently only
			// expect a single host to exist per Kubernetes namespace.
			break
		}
	}

	if hostCluster == nil {
		if req.Operation != admissionv1.Create {
			// The tenant has been orphaned somehow. Let all updates through so
			// that the finalizer can be removed and the tenant deleted.
			return admission.Allowed("OK")
		}
		err := errors.Newf("no host cluster found in namespace %s", tenant.Namespace)
		return admission.Errored(http.StatusBadRequest, err)
	}

	// It appears that validating webhooks only receive the "raw" incoming
	// request and therefore do not have CRD defaulting applied for them. CRD
	// Defaulting is a kube-apiserver feature and doesn't have a way to be
	// leveraged by the operator prior to an object being written to etcd. The
	// KEP, however, indicates that validating webhooks SHOULD get defaulted
	// fields.
	// https://github.com/kubernetes/enhancements/tree/master/keps/sig-api-machinery/575-crd-defaulting
	// TODO(chrisseto): Dig deeper into this. A single line is not worth the
	// (additional) effort.
	tenantPoolName := crdbv1beta1.DefaultTenantPoolName
	if tenant.Spec.TenantPool != nil {
		tenantPoolName = *tenant.Spec.TenantPool
	}

	tenantPool, ok := hostCluster.Spec.TenantHost.TenantPool(tenantPoolName)
	if !ok {
		err := errors.Newf("tenant pool %s does not exist", tenantPoolName)
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Check that the number of stamped, running pods for this tenant does not
	// exceed the specified MaxPods for the tenant.
	maxPods := tenant.Spec.MaxPods
	if maxPods == nil {
		maxPods = tenantPool.MaxPods
	}

	if maxPods != nil && tenant.Spec.Pods > *maxPods {
		err := errors.Newf("tenant cannot have more than %d pod(s)", *maxPods)
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Do not allow empty entries of private endpoints.
	if slices.Contains(tenant.Spec.AllowedPrivateEndpoints, "") {
		err := errors.Newf("tenant cannot have private endpoint IDs which are empty")
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Validate CIDR ranges.
	for _, cidr := range tenant.Spec.AllowedCIDRRanges {
		_, _, err := net.ParseCIDR(cidr)
		if err != nil {
			err := errors.Wrap(err, "tenant cannot have invalid cidr")
			return admission.Errored(http.StatusBadRequest, err)
		}
	}

	// Validate OtelCollector.
	if tenant.Spec.OtelCollector != nil {
		if tenant.Spec.OtelCollector.ConfigMapName == "" {
			err := errors.Newf("ConfigMap name for otelCollector cannot be empty")
			return admission.Errored(http.StatusBadRequest, err)
		}

		// Check that ConfigMapName is valid.
		var cm corev1.ConfigMap
		if err := ctl.Get(ctx, tenant.Spec.OtelCollector.ConfigMapName, &cm); err != nil {
			if kubeerrors.IsNotFound(err) {
				err := errors.Newf("%s ConfigMap not found", tenant.Spec.OtelCollector.ConfigMapName)
				return admission.Errored(http.StatusBadRequest, err)
			}
			return admission.Errored(http.StatusInternalServerError, err)
		}

		// Check that SecretName is valid only if it's not empty.
		if tenant.Spec.OtelCollector.SecretName != "" {
			var s corev1.Secret
			if err := ctl.Get(ctx, tenant.Spec.OtelCollector.SecretName, &s); err != nil {
				if kubeerrors.IsNotFound(err) {
					err := errors.Newf("%s Secret not found", tenant.Spec.OtelCollector.SecretName)
					return admission.Errored(http.StatusBadRequest, err)
				}
				return admission.Errored(http.StatusInternalServerError, err)
			}
		}
	}

	// Validate Fluentbit.
	if tenant.Spec.Fluentbit != nil {
		if tenant.Spec.Fluentbit.ConfigMapName == "" {
			err := errors.Newf("ConfigMap name for fluentbit cannot be empty")
			return admission.Errored(http.StatusBadRequest, err)
		}

		// Check that ConfigMapName is valid.
		var cm corev1.ConfigMap
		if err := ctl.Get(ctx, tenant.Spec.Fluentbit.ConfigMapName, &cm); err != nil {
			if kubeerrors.IsNotFound(err) {
				err := errors.Newf("%s ConfigMap not found", tenant.Spec.Fluentbit.ConfigMapName)
				return admission.Errored(http.StatusBadRequest, err)
			}
			return admission.Errored(http.StatusInternalServerError, err)
		}
	}

	return admission.Allowed("OK")
}

// walFailoverSpecsEqual compares two WAL failover specifications for equality.
// Returns true if the WAL failover name field is unchanged (only the name field is checked for immutability).
func walFailoverSpecsEqual(old, new *crdbv1beta1.CrdbWalFailoverSpec) bool {
	// If both are nil, they are equal
	if old == nil && new == nil {
		return true
	}
	// Allow adding WAL failover to existing clusters (old == nil, new != nil)
	if old == nil && new != nil {
		return true
	}
	// Allow removing WAL failover from existing clusters (old != nil, new == nil)
	if old != nil && new == nil {
		return true
	}
	// Compare only the Name field - other fields (Size, StorageClassName, Status, Path) can be changed
	// Once a name is set, it cannot be changed
	return old.Name == new.Name
}

// dataStoreBackend returns a stable name for the configured storage backend, so
// an Update can detect a switch between backends.
func dataStoreBackend(ds crdbv1beta1.DataStore) string {
	switch {
	case ds.Basalt != nil:
		return "basalt"
	case ds.VolumeClaimTemplate != nil:
		return "volumeClaimTemplate"
	case ds.VolumeSource != nil:
		return "volumeSource"
	default:
		return "none"
	}
}

// validateDataStore enforces that exactly one storage backend is configured and
// that a basalt-backed store carries the fields the operator needs. The basalt
// tenant and its derived directory namespace are the unit of data isolation, so
// a missing or non-unique TenantID would let two clusters collide on one tenant.
func validateDataStore(ds crdbv1beta1.DataStore) error {
	count := 0
	if ds.VolumeSource != nil {
		count++
	}
	if ds.VolumeClaimTemplate != nil {
		count++
	}
	if ds.Basalt != nil {
		count++
	}
	if count != 1 {
		return errors.New("exactly one of VolumeClaimTemplate, VolumeSource, or Basalt must be specified")
	}

	if ds.Basalt == nil {
		return nil
	}
	if len(ds.Basalt.ControllerAddrs) == 0 {
		return errors.New("basalt: controllerAddrs must not be empty")
	}
	if ds.Basalt.TenantID == "" {
		return errors.New("basalt: tenantID is required and must be globally unique across clusters sharing the basalt infrastructure")
	}
	// TenantID is the tenant identity, the directory namespace, and flows into
	// the basalt store URI and the tenant-create command. Requiring a UUID keeps
	// it globally unique and shell-safe, since a UUID string has no whitespace or
	// shell or URI metacharacters.
	if _, err := uuid.FromString(ds.Basalt.TenantID); err != nil {
		return errors.Newf("basalt: tenantID %q must be a valid UUID", ds.Basalt.TenantID)
	}
	// Controller addresses are joined into the --basalt-cluster flag and the
	// store path, so reject whitespace or commas that would corrupt them.
	for _, addr := range ds.Basalt.ControllerAddrs {
		if addr == "" || strings.ContainsAny(addr, " \t\n,") {
			return errors.Newf("basalt: controllerAddr %q must be a non-empty host:port with no whitespace or commas", addr)
		}
	}
	// Neither field set means insecure. Setting tls is what makes it secure, and
	// insecure only asserts what tls already decides, so the two ways of saying
	// the opposite of it are rejected here.
	//
	// Insecure only works if the basalt controllers and blob servers run insecure
	// too, since they take either --insecure or --certs-dir and never both. That
	// is the control plane's call and the operator cannot check it.
	if ds.Basalt.Insecure != nil {
		if *ds.Basalt.Insecure && ds.Basalt.TLS != nil {
			return errors.New("basalt: tls must not be set when insecure is true")
		}
		if !*ds.Basalt.Insecure && ds.Basalt.TLS == nil {
			return errors.New("basalt: tls is required when insecure is false")
		}
	}
	if ds.Basalt.TLS != nil {
		// Only the names are checked. We never look the objects up, because the
		// control plane may well create them after the cluster.
		if ds.Basalt.TLS.ClientSecretName == "" {
			return errors.New("basalt: clientSecretName is required when tls is set")
		}
		if ds.Basalt.TLS.CAConfigMapName == "" {
			return errors.New("basalt: caConfigMapName is required when tls is set")
		}
	}
	return nil
}

// basaltEARConfigured reports whether any part of the cluster asks for basalt
// encryption. A non-nil EncryptionAtRest with a nil KeySecretName is how the CRD
// spells "encryption disabled", so the key secret is what counts.
func basaltEARConfigured(cluster crdbv1beta1.CrdbCluster) bool {
	if ear := cluster.Spec.Template.Spec.EncryptionAtRest; ear != nil && ear.KeySecretName != nil {
		return true
	}
	for i := range cluster.Spec.Regions {
		if ear := cluster.Spec.Regions[i].EncryptionAtRest; ear != nil && ear.KeySecretName != nil {
			return true
		}
	}
	return false
}

// validateEncryptionAtRest checks that old CMEK fields are only set when
// OldKeySecretName is present, and that AWS/GCP platforms have credentials.
func validateEncryptionAtRest(ear *crdbv1beta1.EncryptionAtRest) error {
	if ear.OldKeySecretName == nil {
		if ear.OldPlatform != "" {
			return errors.New("OldPlatform must not be set when OldKeySecretName is nil")
		}
		if ear.OldCMEKCredentialsSecretName != nil {
			return errors.New("OldCMEKCredentialsSecretName must not be set when OldKeySecretName is nil")
		}
		return nil
	}

	oldPlatform := ear.ResolvedOldPlatform()

	// Cross-provider switches need their own credentials secret. Falling back
	// to the current credentials would read the wrong provider's keys (e.g.
	// AWS keys from a GCP secret).
	if oldPlatform != ear.Platform && ear.OldCMEKCredentialsSecretName == nil {
		switch oldPlatform {
		case crdbv1beta1.EncryptionPlatformAwsKms, crdbv1beta1.EncryptionPlatformGcpCloudKms:
			return errors.Newf("OldCMEKCredentialsSecretName is required when switching providers (OldPlatform %s != Platform %s)", oldPlatform, ear.Platform)
		}
	}

	oldCreds := ear.ResolvedOldCMEKCredentialsSecretName()

	switch oldPlatform {
	case crdbv1beta1.EncryptionPlatformAwsKms, crdbv1beta1.EncryptionPlatformGcpCloudKms:
		if oldCreds == nil {
			return errors.Newf("credentials secret required for old CMEK platform %s", oldPlatform)
		}
	case crdbv1beta1.EncryptionPlatformAzureKeyVault:
		// Azure uses workload identity via a fixed credentials secret, not
		// OldCMEKCredentialsSecretName. Reject if set to avoid silent ignore.
		if ear.OldCMEKCredentialsSecretName != nil {
			return errors.New("OldCMEKCredentialsSecretName must not be set when OldPlatform is Azure Key Vault")
		}
	}

	return nil
}

// decodeCluster decodes a CrdbCluster from the admission request.
func (h *Webhook) decodeCluster(req admission.Request) (crdbv1beta1.CrdbCluster, error) {
	var cluster crdbv1beta1.CrdbCluster
	if err := h.decoder.Decode(req, &cluster); err != nil {
		return cluster, err
	}
	return cluster, nil
}

// decodeClusterRaw decodes the OldObject from an update request.
func (h *Webhook) decodeClusterRaw(req admission.Request) (crdbv1beta1.CrdbCluster, error) {
	var cluster crdbv1beta1.CrdbCluster
	if err := h.decoder.DecodeRaw(req.OldObject, &cluster); err != nil {
		return cluster, err
	}
	return cluster, nil
}

// decodeTenant decodes a CrdbTenant from the admission request's new object.
func (h *Webhook) decodeTenant(req admission.Request) (crdbv1beta1.CrdbTenant, error) {
	var tenant crdbv1beta1.CrdbTenant
	if err := h.decoder.Decode(req, &tenant); err != nil {
		return tenant, err
	}
	return tenant, nil
}

// isMigrationBypass returns true when validation should be skipped for clusters
// managed by the migration controller. A cluster in Disabled mode has an incomplete
// spec from conversion defaults and should not be validated. The Disabled→MutableOnly
// transition is intentionally validated so migration cannot complete until the spec
// is fully correct (region, certs, datastore, etc.).
func (h *Webhook) isMigrationBypass(
	_ context.Context, _ admission.Request, cluster *crdbv1beta1.CrdbCluster,
) bool {
	return cluster.Spec.Mode != nil && *cluster.Spec.Mode == crdbv1beta1.Disabled
}

// InjectDecoder implements admission.DecoderInjector. This is called by the K8s
// admission runtime to provide a decoder that is used by the Handle method to
// unmarshal objects in the request.
func (v *Webhook) InjectDecoder(d *admission.Decoder) error {
	v.decoder = d
	return nil
}

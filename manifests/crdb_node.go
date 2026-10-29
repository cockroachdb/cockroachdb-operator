package manifests

import (
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	manifestslog "github.com/cockroachlabs/cockroach-enterprise-operator/manifests/log"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/constants"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/utils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/hashutil"
)

const (
	CRDBContainerName = "cockroachdb"
	// CRDBTerminationGracePeriodSeconds determines the time available to CRDB
	// for graceful drain. K8s will send SIGTERM to CRDB, wait
	// CRDBTerminationGracePeriodSeconds, and then send SIGKILL. The grace
	// period should be enough for CRDB to shed all its SQL connections,
	// distributed statements, and leases. That is the grace period should be
	// greater than the sum of all values below:
	//
	// server.shutdown.drain_wait
	// server.shutdown.connection_wait
	// 2*server.shutdown.query_wait
	// time to drain leases (variable on cluster size)
	//
	// We need to do some more observability work, giving us clear data on how
	// many restarts are done gracefully across the CC fleet. We are looking
	// more generally into graceful drain issues in CC-25439. For logic on the
	// specific value, see CC-25441.
	//
	// Note this value is referenced in kube/drain.go as the wait time
	// for disruption. If you update this value, please update it
	// there as well (it can't be imported due to a cycle).
	CRDBTerminationGracePeriodSeconds = 45 * 60

	// How long to wait in between a rolling restart of a CRDB cluster
	RollingRestartDelaySeconds = 180

	cockroachEnvMountPath        = "/etc/cockroach-env"
	CockroachDataDirVolumeName   = "datadir"
	CockroachLogsDirVolumeName   = "logsdir"
	CockroachDataDirMountPath    = "cockroach-data"
	CockroachDataDirMountAbsPath = "/cockroach/" + CockroachDataDirMountPath

	CockroachConfigVolumePath = "/cockroach/config"
	// DefaultMaxClockOffset is the default clock offset in CC. This is
	// different than the default of CRDB itself which is 500ms.
	DefaultMaxClockOffset = "250ms"

	keyMountPath  = "/etc/cockroach-key"
	keyVolumeName = "cockroach-key"

	// LastSpecChangeAnnotationKey is written on the CrdbNode by logCrdbNodeSpecDiff when a rolling
	// update changes the spec. The restart classifier reads it to log rolling update restarts with
	// the spec change context, then clears it. Defined here so both betaclusterctrl and nodectrl
	// can import it without duplicating the string.
	LastSpecChangeAnnotationKey = "crdb.cockroachlabs.com/last-spec-change"

	// NodeRevisionAnnotationKey is used for a pod's annotation.

	// Its value represents the revision of the node that created the pod.
	// It should be used for detecting if the pod's manifests deviated from
	// the node.
	// Exported for use in testing.
	// See GetNodePodRevision for extracting the revision from a pod.
	NodeRevisionAnnotationKey = "crdb.cockroachlabs.com/nodeRevision"
	// NodeGenerationAnnotationKey is used for a pod's annotation.
	// Its value is the Generation (from ObjectMeta) of the node that created
	// the pod. It is used when deciding whether a pod needs to evicted.
	// If a pod's generation is >= the node's generation, we should consider
	// the pod to be up-to-date.
	// Exported for use in testing.
	// See GetNodePodGeneration for extracting the generation from a pod.
	NodeGenerationAnnotationKey = "crdb.cockroachlabs.com/nodeGeneration"

	// NodeTopologyContraintKey is used to set a pod's topology constraint.
	NodeTopologyContraintKey = corev1.LabelTopologyZone

	// NodePodNameKey is used to set the label of a pod to match its name.
	// This way, we can use label selectors to select specific nodes.
	NodePodNameKey = "crdb.cockroachlabs.com/podName"

	// DefaultLogsMountPath is the default mount path for the dedicated logs volume.
	// This matches CockroachDB's default log directory (<store-dir>/logs).
	DefaultLogsMountPath = CockroachDataDirMountAbsPath + "/logs"

	// DefaultWalMountPath is the path provided to CRDB for WAL failover in the event of a primary
	// disk experiencing a stall
	DefaultWalMountPath = "/cockroach/wal_failover"

	// WalDirVolumeName is the name of the WAL pvc volume
	WalDirVolumeName = "wal-failover"

	WalCmdFlag = "--wal-failover"

	// Keys to be used for setting the cache and SQL max memory
	CacheKey             = "--cache"
	MaxSQLMemoryKey      = "--max-sql-memory"
	Store                = "--store"
	EnterpriseEncryption = "--enterprise-encryption"
	Locality             = "--locality"
	LocalityFile         = "--locality-file"
	hostIp               = "HOST_IP"
	// 25% string
	TwentyFivePercent = "25%"
)

// resolveLogsMountPath returns the provided logsMountPath if non-empty,
// otherwise falls back to DefaultLogsMountPath. The logsMountPath is
// expected to come from CrdbLogsStoreSpec.MountPath.
func resolveLogsMountPath(logsMountPath string) string {
	if logsMountPath != "" {
		return logsMountPath
	}
	return DefaultLogsMountPath
}

// walMountPath returns the configured WAL mount path, defaulting to DefaultWalMountPath if not set.
func walMountPath(node *crdbv1beta1.CrdbNode) string {
	if path := node.Spec.WALFailoverSpec.Path; path != "" {
		return path
	}
	return DefaultWalMountPath
}

// GetNodePodGeneration returns the generation from a pod's annotations.
// Returns 0 if the annotation is empty, or is ill-formed.
func GetNodePodGeneration(pod *corev1.Pod) int64 {
	gen, _ := strconv.Atoi(pod.Annotations[NodeGenerationAnnotationKey])
	return int64(gen)
}

// GetNodePodRevision returns the revision from a pod's annotations.
// Returns empty string if the annotation is missing.
func GetNodePodRevision(pod *corev1.Pod) string {
	return pod.Annotations[NodeRevisionAnnotationKey]
}

// CrdbNodePod returns the expected pod from the CrdbNode object.
func CrdbNodePod(node *crdbv1beta1.CrdbNode, k8sNode *corev1.Node) *corev1.Pod {
	providerString := node.Annotations[BetaCrdbClusterNodeCloudProviderAnnotationKey]
	provider, _ := cloud.ParseProvider(providerString)

	tlsEnabled := node.Spec.TLSEnabled

	volumes := nodeVolumes(node.Name, provider, &node.Spec, GetClusterName(node))
	volumeMounts := []corev1.VolumeMount{
		{Name: "cockroach-env", MountPath: "/etc/cockroach-env", ReadOnly: true},
		{Name: CockroachDataDirVolumeName, MountPath: CockroachDataDirMountAbsPath},
	}
	if tlsEnabled {
		volumeMounts = append([]corev1.VolumeMount{CockroachCertsVolumeMount()}, volumeMounts...)
	}
	// The basalt PKI is separate from the CockroachDB one, so this mount keys off
	// basalt alone. A cluster can run insecure itself and still reach basalt over
	// mTLS, or the other way round.
	if node.Spec.DataStore.IsSecureBasalt() {
		volumeMounts = append(volumeMounts, BasaltCertsVolumeMount())
	}

	// If dedicated logs storage is configured, mount it at the path specified by
	// LogsStore.MountPath, or the default path (/cockroach/cockroach-data/logs) if not set.
	if node.Spec.LogsStore != nil {
		volumeMounts = append(volumeMounts,
			corev1.VolumeMount{
				Name:      CockroachLogsDirVolumeName,
				MountPath: resolveLogsMountPath(node.Spec.LogsStore.MountPath),
			})
	}

	if node.Spec.LoggingConfigMapName != "" {
		volumeMounts = append(volumeMounts,
			corev1.VolumeMount{Name: manifestslog.LoggingConfigMapVolumeName, MountPath: CockroachConfigVolumePath, ReadOnly: true})
	}
	if node.Spec.EncryptionAtRest != nil {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			MountPath: keyMountPath,
			Name:      keyVolumeName,
			ReadOnly:  true,
		})
	}
	if provider == cloud.Azure {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			MountPath: CredentialsMountPath,
			Name:      CredentialsVolumeName,
			ReadOnly:  true,
		})
	}

	if node.Spec.WALFailoverSpec != nil {
		volumeMounts = append(volumeMounts,
			corev1.VolumeMount{
				Name:      WalDirVolumeName,
				MountPath: walMountPath(node),
			})
	}

	annotations := make(map[string]string)
	for k, v := range node.Spec.PodAnnotations {
		if k != corev1.LastAppliedConfigAnnotation {
			annotations[k] = v
		}
	}
	annotations[NodeRevisionAnnotationKey] = GetNodeHashRevisionNew(node)
	annotations[NodeGenerationAnnotationKey] = strconv.FormatInt(node.Generation, 10)

	labels := make(map[string]string)
	maps.Copy(labels, node.Spec.PodLabels)
	// We need this right now
	// 1) for `kubecrdb/kubenet.OpenDB` to work:
	//   it looks for these labels when selecting pods;
	// 2) for the internalCrdbClusterIPService to work.
	// TODO(joel): Move these labels to the CrdbNode.Spec.PodLabels field.
	labels["app"] = "cockroachdb"
	labels["svc"] = "cockroachdb"
	labels["cluster"] = node.Labels["cluster"]
	labels[BetaCrdbClusterNodeOwnerLabelKey] = node.Labels[BetaCrdbClusterNodeOwnerLabelKey]
	labels[NodePodNameKey] = node.Name
	if provider == cloud.Azure {
		// Azure pods use workload identity to authorize CMEK.
		labels["azure.workload.identity/use"] = "true"
	}

	terminationTerminationPeriod := &metav1.Duration{Duration: CRDBTerminationGracePeriodSeconds * time.Second}
	if node.Spec.TerminationGracePeriod != nil {
		terminationTerminationPeriod = node.Spec.TerminationGracePeriod
	}

	podSecurityContext := node.PodSecurityContext()
	// NOTE: Using hostname may seem indirect because pods have a NodeName
	// field on their spec that is semantically the same as the CrdbNode's
	// NodeName. The problem is specifying NodeName on the pod's spec skips the
	// k8s scheduler and the k8s scheduler is involved in creating
	// 'WaitForFirstConsumer' PVs on pod creation. So specifying
	// pod.Spec.NodeName with a new PVC hangs with a pending PVC.
	//
	// See https://kubernetes.io/docs/concepts/storage/storage-classes/#volume-binding-mode
	nodeSelector := make(map[string]string)
	if node.Spec.NodeName != "" {
		nodeSelector[corev1.LabelHostname] = node.Spec.NodeName
	}
	maps.Copy(nodeSelector, node.Spec.NodeSelector)

	// Use the affinity policy on the node spec if set, ensuring that we always
	// configure pod anti-affinity such that multiple crdb pods aren't scheduled
	// to the same node.
	affinity := node.Spec.Affinity
	if affinity == nil {
		affinity = &corev1.Affinity{}
	}
	if affinity.PodAntiAffinity == nil {
		affinity.PodAntiAffinity = &corev1.PodAntiAffinity{}
	}
	podAntiAffinity := affinity.PodAntiAffinity
	if podAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		podAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = []corev1.PodAffinityTerm{}
	}
	antiAffinityTerms := podAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	podAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(antiAffinityTerms, corev1.PodAffinityTerm{
		LabelSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app":                            CockroachDB,
				BetaCrdbClusterNodeOwnerLabelKey: node.Labels[BetaCrdbClusterNodeOwnerLabelKey],
			},
		},
		Namespaces:  []string{node.Namespace},
		TopologyKey: corev1.LabelHostname,
	})

	crdbPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        node.Name,
			Namespace:   node.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			NodeSelector:                  nodeSelector,
			InitContainers:                crdbNodeInitContainers(node),
			Containers:                    crdbNodeContainers(node, k8sNode, volumeMounts, tlsEnabled),
			Tolerations:                   node.Spec.Tolerations,
			TerminationGracePeriodSeconds: new(int64(terminationTerminationPeriod.Seconds())),
			// Ensure `hostname -f` returns <podName>.<clusterName> which will guarantee
			// that the node cert contains this node in its alternative names. The result
			// of `hostname -f` will be of the following form:
			//
			//   <podName>.<clusterName>.<namespace>.svc.cluster.local
			Hostname:                  node.Name,
			Subdomain:                 node.Labels[BetaCrdbClusterNodeOwnerLabelKey],
			SecurityContext:           podSecurityContext,
			ServiceAccountName:        node.Spec.ServiceAccountName,
			Volumes:                   volumes,
			Affinity:                  affinity,
			TopologySpreadConstraints: node.Spec.TopologySpreadConstraints,
			PriorityClassName:         CRDBCritical,
			// The cert-reloader sidecar finds the CockroachDB process by name to
			// SIGHUP it, and that only works across containers when the pod shares
			// its process namespace. A pod with no certs to watch gets no sidecar,
			// so it does not need the sharing either.
			ShareProcessNamespace: new(tlsEnabled || node.Spec.DataStore.IsSecureBasalt()),
		},
	}
	return ApplyPodTemplate(node.Spec.PodTemplate, crdbPod)
}

// crdbNodeContainers builds the container list for a CockroachDB pod. The
// cert-reloader sidecar only watches the PKI domains the pod actually uses, and
// a pod that is insecure in both gets no sidecar at all.
func crdbNodeContainers(
	node *crdbv1beta1.CrdbNode,
	k8sNode *corev1.Node,
	volumeMounts []corev1.VolumeMount,
	tlsEnabled bool,
) []corev1.Container {
	crdbContainer := corev1.Container{
		Name:  CRDBContainerName,
		Image: node.Spec.Image,
		Command: []string{
			"/bin/bash",
			"-exc",
			crdbNodeCmd(node),
		},
		Env: crdbNodeEnv(node, tlsEnabled),
		Ports: []corev1.ContainerPort{
			{
				ContainerPort: node.Spec.GetGRPCPort(),
				Name:          "grpc",
				Protocol:      corev1.ProtocolTCP,
			},
			{
				ContainerPort: node.Spec.GetHTTPPort(),
				Name:          "http",
				Protocol:      corev1.ProtocolTCP,
			},
			{
				ContainerPort: node.Spec.GetSQLPort(),
				Name:          "sql",
				Protocol:      corev1.ProtocolTCP,
			},
		},
		ReadinessProbe:  getNodeReadinessProbe(node.Spec.ReadinessProbe, tlsEnabled),
		Resources:       crdbNodeResources(node, k8sNode),
		SecurityContext: node.ContainerSecurityContext(),
		VolumeMounts:    volumeMounts,
	}

	containers := append(node.Spec.SideCars.Containers, crdbContainer)

	var reloaderMounts []corev1.VolumeMount
	if tlsEnabled {
		reloaderMounts = append(reloaderMounts, CockroachCertsVolumeMount())
	}
	// CockroachDB reads the basalt TLS config once at startup, so a SIGHUP does
	// not pick up a rotated basalt cert yet. We watch the directory anyway so the
	// wiring is already there the day CockroachDB can act on it. Tracked in
	// https://github.com/cockroachdb/cockroach/issues/173441.
	if node.Spec.DataStore.IsSecureBasalt() {
		reloaderMounts = append(reloaderMounts, BasaltCertsVolumeMount())
	}
	if len(reloaderMounts) > 0 {
		containers = append(containers, CertReloaderContainer(reloaderMounts...))
	}
	return containers
}

// getConditionalQuantity returns the largest quantity with a required capacity
// that is less than or equal to capacity.
func getConditionalQuantity(
	capacity resource.Quantity, quantities []crdbv1beta1.ConditionalQuantity,
) (result resource.Quantity, found bool) {
	for _, quantity := range quantities {
		hasEnoughCapacity := quantity.RequiredCapacity.Cmp(capacity) <= 0
		isLarger := 0 < quantity.Quantity.Cmp(result)
		if hasEnoughCapacity && isLarger {
			result = quantity.Quantity
			found = true
		}
	}
	return result, found
}

// crdbNodeResources combines the nodes `resourceRequirements` and
// `conditionalResources` to compute the crdb container's `resources`.
func crdbNodeResources(
	crdbNode *crdbv1beta1.CrdbNode, k8sNode *corev1.Node,
) corev1.ResourceRequirements {
	result := *crdbNode.Spec.ResourceRequirements.DeepCopy()

	if k8sNode == nil {
		return result
	}

	conditionalResourceRequirements := crdbNode.Spec.ConditionalResourceRequirements
	if conditionalResourceRequirements == nil {
		conditionalResourceRequirements = &crdbv1beta1.ConditionalResourceRequirements{}
	}

	if _, ok := result.Limits[corev1.ResourceMemory]; !ok {
		if result.Limits == nil {
			result.Limits = make(corev1.ResourceList)
		}
		result.Limits[corev1.ResourceMemory] = k8sNode.Status.Allocatable[corev1.ResourceMemory]
	}
	if _, ok := result.Requests[corev1.ResourceMemory]; !ok {
		if result.Requests == nil {
			result.Requests = make(corev1.ResourceList)
		}
		result.Requests[corev1.ResourceMemory] = resource.MustParse("0Gi")
	}

	proportionalLimits := conditionalResourceRequirements.Limits
	proportionalRequests := conditionalResourceRequirements.Requests

	// We use `Allocatable` instead of `Capacity` because it accounts for
	// memory reserved for the system.
	// https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources/#node-allocatable
	for kind, capacity := range k8sNode.Status.Allocatable {
		if newLimit, found := getConditionalQuantity(capacity, proportionalLimits[kind]); found {
			if result.Limits == nil {
				result.Limits = corev1.ResourceList{}
			}
			result.Limits[kind] = newLimit
		}
		if newRequest, found := getConditionalQuantity(capacity, proportionalRequests[kind]); found {
			if result.Requests == nil {
				result.Requests = corev1.ResourceList{}
			}
			result.Requests[kind] = newRequest
		}
	}
	return result
}

func nodeFQDN(node *crdbv1beta1.CrdbNode) string {
	return fmt.Sprintf("%s.%s", node.Name, ClusterFQDN(GetClusterName(node), node.Namespace, node.Spec.Domain))
}

// JoinFQDN returns the FQDN of a CrdbCluster's "join" service. The join
// service supports the same target as the cluster's service discovery service.
// However, it requires that the underlying Pods report themselves as Ready.
// See [JoinService].
func JoinFQDN(cluster, namespace, domain string) string {
	// Piggy back off ClusterFQDN. We use all the same logic but point to a
	// different service.
	return ClusterFQDN(cluster+"-join", namespace, domain)
}

// ClusterFQDN constructs the fully qualified domain name for a cluster,
// that one can use to contact the cluster and perform operations like
// querying node status.
func ClusterFQDN(cluster, namespace, domain string) string {
	// If Domain is an empty string, fallback to cluster.local. This is a safe
	// default for hosted K8s clusters. We could omit the domain and just rely
	// use .svc but our certificates aren't correctly signed for that.
	if domain == "" {
		domain = "cluster.local"
	}
	return fmt.Sprintf("%s.%s.svc.%s", cluster, namespace, domain)
}

func crdbNodeCmd(node *crdbv1beta1.CrdbNode) string {
	// Check if podTemplate overrides the cockroachdb container command
	// If so, startFlags should be ignored
	if node.Spec.PodTemplate != nil {
		for _, container := range node.Spec.PodTemplate.Spec.Containers {
			if container.Name == CRDBContainerName && len(container.Command) > 0 {
				// PodTemplate specifies a command for cockroachdb container,
				// so we return a placeholder that will be overridden by the template
				return ""
			}
		}
	}

	// Todo(nishanth): Update code paths once flags is deprecated.
	// Either flags or startFlags will be allowed to configure but not both.
	if node.Spec.StartFlags != nil {
		return parseStartFlags(node)
	} else {
		return parseFlags(node)
	}
}

func crdbNodeEnv(node *crdbv1beta1.CrdbNode, tlsEnabled bool) []corev1.EnvVar {
	defaultEnv := []corev1.EnvVar{
		{
			// Specified so --host isn't required for most commands (e.g. cockroach sql).
			// Some commands may still require specifying the host in order to supply a
			// different host/port value (e.g. cockroach init).
			Name:  "COCKROACH_HOST",
			Value: fmt.Sprintf("localhost:%d", node.Spec.GetSQLPort()),
		},
		{
			Name:  "COCKROACH_RPC_PORT",
			Value: fmt.Sprintf("%d", node.Spec.GetGRPCPort()),
		},
		{
			Name:  "COCKROACH_SQL_PORT",
			Value: fmt.Sprintf("%d", node.Spec.GetSQLPort()),
		},
	}
	if tlsEnabled {
		// COCKROACH_CERTS_DIR allows cockroach CLI subcommands to find TLS certs
		// without explicitly passing --certs-dir on every invocation.
		defaultEnv = append([]corev1.EnvVar{{
			Name:  "COCKROACH_CERTS_DIR",
			Value: CockroachCertsPath,
		}}, defaultEnv...)
	} else {
		// COCKROACH_INSECURE allows cockroach CLI subcommands exec'd into the
		// container to connect without TLS, symmetrically to how COCKROACH_CERTS_DIR
		// works for TLS clusters.
		defaultEnv = append([]corev1.EnvVar{{
			Name:  "COCKROACH_INSECURE",
			Value: "true",
		}}, defaultEnv...)
	}

	// HOST_IP from the downward API as a guard for self-hosted deployments.
	// The default --log-config-vars=HOST_IP flag means CRDB expects this
	// env var to exist. Without it, SH deployments that provide a logging
	// config not referencing HOST_IP would fail on expansion.

	// Todo(nishanth): Remove this once CC sets LoggingConfigVars explicitly.
	defaultEnv = append(defaultEnv, corev1.EnvVar{
		Name: hostIp,
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				APIVersion: "v1",
				FieldPath:  "status.hostIP",
			},
		},
	})

	// Basalt-backed nodes read the controller addresses and tenant id from these
	// env vars in their start flags, so the values stay out of the shell command
	// and can't inject shell.
	if node.Spec.DataStore.IsBasalt() {
		defaultEnv = append(defaultEnv, BasaltEnvVars(node.Spec.DataStore.Basalt)...)
	}

	// According to Kubernetes docs, values later on in the slice will take
	// precedence. This will allow users to override any default values that
	// get set by the operator. It's unlikely they'll ever need to but why not
	// let them touch wet paint.
	return append(defaultEnv, node.Spec.Env...)
}

// GetNodeHashRevisionNew returns a short hash string that represents everything
// that goes into calculating the node's corresponding pod manifest, i.e.,
// the node's name, namespace, and spec. New pods are created with this hash.
//
// We don't include the node.Annotations or node.Labels in the input to the
// hash. Changing the annotations and labels on the CrdbNode does not require
// the corresponding pod to be replaced.
//
// Data volume size, WAL disk size, and the basalt scratch PVC size are all
// excluded so that resizing any of those disks happens in place without
// evicting the pod.
func GetNodeHashRevisionNew(node *crdbv1beta1.CrdbNode) string {
	// Make a copy of the nodeSpec so our mutations don't affect the real resource.
	specCopy := node.Spec.DeepCopy()
	// We don't want changes to the disk size to result in pod restarts.
	if specCopy.DataStore.VolumeClaimTemplate != nil {
		specCopy.DataStore.VolumeClaimTemplate.Spec.Resources.Requests = nil
	}
	// We don't want changes to the WAL disk size to result in pod restarts.
	if specCopy.WALFailoverSpec != nil {
		specCopy.WALFailoverSpec.Size = ""
	}
	// We don't want changes to the basalt scratch disk size to result in pod
	// restarts. The pod references the scratch PVC by name, so its size never
	// appears in the pod spec and the PVC can be resized in place.
	if b := specCopy.DataStore.Basalt; b != nil && b.ScratchVolumeClaimTemplate != nil {
		b.ScratchVolumeClaimTemplate.Spec.Resources.Requests = nil
	}
	// Changes to the Join field should not trigger an update. Those are unnecessary,
	// since new instances will have updated Join fields themselves and deleted instances will be
	// safely ignored.
	specCopy.Join = ""

	nodeCopy := &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: node.Namespace,
		},
		Spec: *specCopy,
	}

	return hashutil.MustFNV(nodeCopy)
}

// GetNodeHashRevisionOld is the hash function used before WAL disk size was
// excluded from the revision. It is identical to GetNodeHashRevisionNew except
// that it includes WALFailoverSpec.Size in the hash. Pods created by an earlier
// operator carry this value, so we still accept it as a match to avoid restarting
// them on upgrade. Remove once all pods have been recreated with the new hash.
func GetNodeHashRevisionOld(node *crdbv1beta1.CrdbNode) string {
	// Make a copy of the nodeSpec so our mutations don't affect the real resource.
	specCopy := node.Spec.DeepCopy()
	// We don't want changes to the disk size to result in pod restarts.
	if specCopy.DataStore.VolumeClaimTemplate != nil {
		specCopy.DataStore.VolumeClaimTemplate.Spec.Resources.Requests = nil
	}
	// Changes to the Join field should not trigger an update. Those are unnecessary,
	// since new instances will have updated Join fields themselves and deleted instances will be
	// safely ignored.
	specCopy.Join = ""

	nodeCopy := &crdbv1beta1.CrdbNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: node.Namespace,
		},
		Spec: *specCopy,
	}

	return hashutil.MustFNV(nodeCopy)
}

func getNodeReadinessProbe(customProbe *corev1.Probe, tlsEnabled bool) *corev1.Probe {
	if customProbe != nil {
		return customProbe
	}
	scheme := corev1.URISchemeHTTPS
	if !tlsEnabled {
		// When started with --insecure the HTTP health endpoint serves plain
		// HTTP rather than HTTPS.
		scheme = corev1.URISchemeHTTP
	}
	return &corev1.Probe{
		FailureThreshold:    2,
		InitialDelaySeconds: 10,
		PeriodSeconds:       10,
		SuccessThreshold:    1,
		TimeoutSeconds:      10,
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   "/health?ready=1",
				Port:   intstr.FromString("http"),
				Scheme: scheme,
			},
		},
	}
}

// CrdbNodeStoragePVCName generates the name for the PVC dedicated to
// storage for a CrdbNode. If the PersistentVolumeClaim has a Name
// specified, that value will be used as a prefix to the unprefixed name
// provided, similarly to how StatefulSets name their PVCs. If there is
// no name, the unprefixed name will be used. Regardless, the
// PersistentVolumeClaim pointer provided cannot be nil.
func CrdbNodeStoragePVCName(unprefixedName string, vct *corev1.PersistentVolumeClaim) string {
	if vct.Name == "" {
		return unprefixedName
	}
	return fmt.Sprintf("%s-%s", vct.Name, unprefixedName)
}

func CrdbNodeWalStoragePVCName(
	walFailoverPrefixName string, unprefixedName string, vct *corev1.PersistentVolumeClaim,
) string {
	// Check if the value is present in the name field for the WalFailoverSpec,
	// If yes, add that as the prefix to the pvc name creation,
	// otherwise, have "wal" as the prefix name.
	if walFailoverPrefixName == "" {
		return fmt.Sprintf("wal-%s", CrdbNodeStoragePVCName(unprefixedName, vct))
	}
	return fmt.Sprintf("%s-%s", walFailoverPrefixName, unprefixedName)
}

// CrdbNodeLogsStoragePVCName generates the PVC name for logs storage.
// If a custom name is provided in the VolumeClaimTemplate, it will be used as a prefix.
// Otherwise, defaults to "logsdir-{nodeName}".
func CrdbNodeLogsStoragePVCName(unprefixedName string, vct *corev1.PersistentVolumeClaim) string {
	if vct.Name != "" {
		return fmt.Sprintf("%s-%s", vct.Name, unprefixedName)
	}
	return fmt.Sprintf("logsdir-%s", unprefixedName)
}

// CrdbNodeScratchPVCName generates the PVC name for a basalt node's local
// scratch dir. If a custom name is provided in the template it is used as a
// prefix. Otherwise it defaults to "scratch-{nodeName}". Kept distinct from the
// data-dir PVC name so a basalt scratch PVC is not confused with a non-basalt
// cluster's datadir PVC.
func CrdbNodeScratchPVCName(unprefixedName string, vct *corev1.PersistentVolumeClaim) string {
	if vct.Name != "" {
		return fmt.Sprintf("%s-%s", vct.Name, unprefixedName)
	}
	return fmt.Sprintf("scratch-%s", unprefixedName)
}

// nodeVolumes returns the set of volumes to be mounted in the crdb container. It
// includes (at a minimum) the datadir, certs, and env volumes.
func nodeVolumes(
	name string, provider cloud.Provider, spec *crdbv1beta1.CrdbNodeSpec, clusterName string,
) []corev1.Volume {
	var dataDirSource corev1.VolumeSource

	switch {
	case spec.DataStore.IsBasalt():
		// Basalt holds all data over the network. We still mount a writable
		// scratch dir at /cockroach/cockroach-data for temp files and dumps.
		//
		// When a ScratchVolumeClaimTemplate is configured, back that scratch dir
		// with a dedicated PVC so temp/spill/dumps/logs land on a bounded,
		// node-lifetime disk instead of the Kubernetes node's ephemeral
		// (boot-disk) storage, where a runaway temp could fill the boot disk.
		// Otherwise, fall back to an ephemeral EmptyDir (legacy behavior).
		if pvc := spec.DataStore.Basalt.ScratchVolumeClaimTemplate; pvc != nil {
			dataDirSource = corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: CrdbNodeScratchPVCName(name, pvc),
				},
			}
		} else {
			// TODO(prod-orch): set a SizeLimit on this EmptyDir so a runaway
			// temp/spill can't fill the node's boot disk.
			dataDirSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
		}
	case spec.DataStore.VolumeSource != nil:
		dataDirSource = *spec.DataStore.VolumeSource
	case spec.DataStore.VolumeClaimTemplate != nil:
		// Note: It's assumed that the VolumeClaimTemplate is inherited from
		// CrdbCluster.Spec.Template, which means that the node controller must
		// be in charge of generating a deterministic name.
		pvc := spec.DataStore.VolumeClaimTemplate
		dataDirSource = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: CrdbNodeStoragePVCName(name, pvc),
			},
		}
	}

	volumes := append(spec.SideCars.Volumes,
		corev1.Volume{Name: CockroachDataDirVolumeName, VolumeSource: dataDirSource},
		corev1.Volume{Name: "cockroach-env", VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		}},
	)

	// If dedicated logs storage is configured, create logs volume
	if spec.LogsStore != nil {
		var logsDirSource corev1.VolumeSource

		if spec.LogsStore.VolumeSource != nil {
			logsDirSource = *spec.LogsStore.VolumeSource
		} else if pvc := spec.LogsStore.VolumeClaimTemplate; pvc != nil {
			logsDirSource = corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: CrdbNodeLogsStoragePVCName(name, pvc),
				},
			}
		}

		volumes = append(volumes, corev1.Volume{
			Name:         CockroachLogsDirVolumeName,
			VolumeSource: logsDirSource,
		})
	}

	switch {
	case spec.Certificates.SelfSigned && spec.Certificates.ExternalCertificates == nil:
		volumes = append(volumes, VolumeProjection(
			"certs",
			certSecretProjections(&crdbv1beta1.ExternalCertificates{
				NodeSecretName:          SelfSignedNodeCertSecretName(clusterName),
				RootSQLClientSecretName: SelfSignedRootClientSecretName(clusterName),
			}),
			caCertConfigMapProjections(&crdbv1beta1.ExternalCertificates{
				CAConfigMapName: SelfSignedCAConfigMapName(clusterName),
			}),
		))
	case spec.Certificates.ExternalCertificates != nil:
		volumes = append(volumes, VolumeProjection(
			"certs",
			certSecretProjections(spec.Certificates.ExternalCertificates),
			caCertConfigMapProjections(spec.Certificates.ExternalCertificates),
		))
	default:
		// Eventually, some form of certs will always be required. This is just here
		// temporarily until that day comes.
		volumes = append(volumes, corev1.Volume{
			Name: "certs",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
	}

	if spec.DataStore.IsSecureBasalt() {
		volumes = append(volumes, BasaltCertsVolume(spec.DataStore.Basalt))
	}

	if spec.LoggingConfigMapName != "" {
		volumes = append(volumes, manifestslog.LoggingConfigMapVolume(spec.LoggingConfigMapName))
	}

	if spec.EncryptionAtRest != nil {
		volumes = append(volumes, corev1.Volume{
			Name: keyVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium: corev1.StorageMediumMemory,
				},
			},
		})
	}

	if provider == cloud.Azure {
		// Secret is optional - If the Secret does not exist, the pod still starts with an empty volume mount
		volumes = append(volumes, corev1.Volume{
			Name: CredentialsVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: AzureCredentialsSecretName,
					Optional:   new(true),
				},
			},
		})
	}

	// If the old store key was encrypted with Azure KMS and the current
	// provider is not Azure, we need a separate volume for the old Azure
	// credentials so the init container can decrypt the old key. The
	// credentials secret must be retained until old-key decryption completes.
	if spec.EncryptionAtRest != nil && spec.EncryptionAtRest.OldKeySecretName != nil &&
		spec.EncryptionAtRest.ResolvedOldPlatform() == crdbv1beta1.EncryptionPlatformAzureKeyVault &&
		provider != cloud.Azure {
		volumes = append(volumes, corev1.Volume{
			Name: OldCredentialsVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: AzureCredentialsSecretName,
				},
			},
		})
	}

	if spec.WALFailoverSpec != nil {
		walDirSource := corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: CrdbNodeWalStoragePVCName(spec.WALFailoverSpec.Name, name, spec.DataStore.VolumeClaimTemplate),
			},
		}

		volumes = append(volumes, corev1.Volume{
			Name:         WalDirVolumeName,
			VolumeSource: walDirSource,
		})
	}

	return volumes
}

func certSecretProjections(extCerts *crdbv1beta1.ExternalCertificates) []SecretProjection {
	// Project required certificate secrets.
	projections := []SecretProjection{
		{Name: extCerts.NodeSecretName, Renames: []corev1.KeyToPath{
			{Key: "tls.crt", Path: "node.crt"},
			{Key: "tls.key", Path: "node.key"},
		}},
		{Name: extCerts.RootSQLClientSecretName, Renames: []corev1.KeyToPath{
			{Key: "tls.crt", Path: "client.root.crt"},
			{Key: "tls.key", Path: "client.root.key"},
		}},
	}
	// Project optional certificate secrets if provided.
	if extCerts.NodeClientSecretName != "" {
		projections = append(projections, SecretProjection{Name: extCerts.NodeClientSecretName, Renames: []corev1.KeyToPath{
			{Key: "tls.crt", Path: "client.node.crt"},
			{Key: "tls.key", Path: "client.node.key"},
		}})
	}
	if extCerts.HTTPSecretName != "" {
		projections = append(projections, SecretProjection{Name: extCerts.HTTPSecretName, Renames: []corev1.KeyToPath{
			{Key: "tls.crt", Path: "ui.crt"},
			{Key: "tls.key", Path: "ui.key"},
		}})
	}
	return projections
}

func caCertConfigMapProjections(extCerts *crdbv1beta1.ExternalCertificates) []ConfigMapProjection {
	// Project a single CA certificate, if using
	if extCerts.CAConfigMapName != "" {
		return []ConfigMapProjection{
			{Name: extCerts.CAConfigMapName, Renames: []corev1.KeyToPath{
				{Key: "ca.crt", Path: "ca.crt"},
			}},
		}
	} else {
		return []ConfigMapProjection{
			{Name: extCerts.NodeCAConfigMapName, Renames: []corev1.KeyToPath{
				{Key: "ca.crt", Path: "ca.crt"},
			}},
			{Name: extCerts.ClientCAConfigMapName, Renames: []corev1.KeyToPath{
				{Key: "ca.crt", Path: "ca-client.crt"},
			}},
		}
	}
}

func crdbNodeInitContainers(node *crdbv1beta1.CrdbNode) []corev1.Container {
	containers := []corev1.Container{}

	command := []string{"init-container", "-locality-dest", cockroachEnvMountPath}

	if len(node.Spec.LocalityMappings) > 0 {
		b, err := json.Marshal(node.Spec.LocalityMappings)
		if err != nil {
			panic(err)
		}
		command = append(command, "-locality-mappings", string(b))
	}

	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "cockroach-env",
			MountPath: cockroachEnvMountPath,
		},
	}

	parsedProvider, _ := cloud.ParseProvider(node.Annotations[BetaCrdbClusterNodeCloudProviderAnnotationKey])

	if parsedProvider == cloud.Azure {
		// Adding cluster-identity credentials as
		// volume mount in init-container. Cluster Identity
		// client ID is required for cross tenant CMEK access.
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			MountPath: CredentialsMountPath,
			Name:      CredentialsVolumeName,
			ReadOnly:  true,
		})
	}

	// When the old store key was encrypted with Azure KMS, mount old
	// credentials so the init container can decrypt the old key. If the
	// current provider is already Azure the volume is already present at
	// CredentialsMountPath, so we mount a second copy at a different path.
	if node.Spec.EncryptionAtRest != nil && node.Spec.EncryptionAtRest.OldKeySecretName != nil &&
		node.Spec.EncryptionAtRest.ResolvedOldPlatform() == crdbv1beta1.EncryptionPlatformAzureKeyVault {
		if parsedProvider == cloud.Azure {
			volumeMounts = append(volumeMounts, corev1.VolumeMount{
				MountPath: OldCredentialsMountPath,
				Name:      CredentialsVolumeName,
				ReadOnly:  true,
			})
		} else {
			volumeMounts = append(volumeMounts, corev1.VolumeMount{
				MountPath: OldCredentialsMountPath,
				Name:      OldCredentialsVolumeName,
				ReadOnly:  true,
			})
		}
	}

	envVars := []corev1.EnvVar{
		{
			Name: "KUBERNETES_NODE",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "spec.nodeName",
				},
			},
		},
		{
			// TODO(ye): this is hardcoded, and we need to adjust this
			// for external users. See [CC-8406].
			Name:  "PUSHGATEWAY_NAMESPACE",
			Value: utils.MonitoringNamespaceName(node.Namespace),
		},
		// Error if requested k8s node labels are missing.
		//
		// TODO: Make this the default behavior in the init container.
		{
			Name:  "ERROR_ON_MISSING_LABELS",
			Value: "1",
		},
	}

	if parsedProvider == cloud.Azure {
		envVars = append(envVars, corev1.EnvVar{
			Name:  AzureCredentialsEnvVar,
			Value: AzureCredentialsFullPath(),
		})
	}

	if node.Spec.EncryptionAtRest != nil {
		// TODO: rename this flag to -update-encryption when support for key
		// rotation is added
		command = append(command, "-use-new-store-key")

		// A basalt store consumes the key directory via --cluster-key-dir, which
		// requires the files to be named "current" and "old". Every other store
		// gets the paths named verbatim in --enterprise-encryption, so it keeps
		// the init container's default names.
		//
		// Only encrypted basalt nodes are sent these, so an image predating the
		// flags is reached only once such a cluster exists. It then exits naming
		// the flag rather than writing the key somewhere CockroachDB will not
		// look for it. See defaultInitContainerImage.
		if basaltClusterKeyEnabled(node) {
			command = append(command,
				"-new-store-key-file", BasaltClusterKeyCurrentFile,
				"-old-store-key-file", BasaltClusterKeyOldFile,
			)
		}

		volumeMounts = append(volumeMounts,
			corev1.VolumeMount{
				// This is the ephemeral volume in memory where the decrypted
				// store key goes.
				MountPath: keyMountPath,
				Name:      keyVolumeName,
			},
			corev1.VolumeMount{
				// Encrypted store key backups are stored alongside the actual
				// cockroachdb data. This is a bit of a stopgap until we store all
				// this required information in the control plane; in the public
				// operator the users will be responsible for backing up the
				// encrypted store key themselves.
				Name:      CockroachDataDirVolumeName,
				MountPath: "/cockroach/cockroach-data",
			},
		)

		earOptions := &encryptionAtRestOptions{
			KeySecretName:             node.Spec.EncryptionAtRest.KeySecretName,
			CMEKPlatform:              node.Spec.EncryptionAtRest.Platform,
			CMEKCredentialsSecretName: node.Spec.EncryptionAtRest.CMEKCredentialsSecretName,
			OldKeySecretName:          node.Spec.EncryptionAtRest.OldKeySecretName,
		}
		if node.Spec.EncryptionAtRest.OldKeySecretName != nil {
			earOptions.OldCMEKPlatform = node.Spec.EncryptionAtRest.ResolvedOldPlatform()
			earOptions.OldCMEKCredentialsSecretName = node.Spec.EncryptionAtRest.ResolvedOldCMEKCredentialsSecretName()
		}
		envVars = append(envVars, addEncryptionAtRestEnvVars(earOptions)...)
	}

	image := node.Spec.InitContainerImage
	if image == "" {
		image = constants.GetInitContainerImage()
	}
	containers = append(containers, corev1.Container{
		Name:         fmt.Sprintf("%s-init", CRDBContainerName),
		Image:        image,
		Command:      command,
		Env:          envVars,
		VolumeMounts: volumeMounts,
	})

	return append(node.Spec.SideCars.InitContainers, containers...)
}

// HostConditionalResourceRequirements returns conditional resource
// requirements configured for KV servers in a Host cluster.
//
// The resource requirements are intended to give KV servers about 60% of the
// CPU capacity for each node. KV servers are given up to 60% of the node
// because the typical breakdown of work between sql and kv is 1/3 vs 2/3.
//
// # Question: Why do we set a CPU request on KV servers?
// SQL servers are configured with a CPU Limit of 4 and a very small CPU
// request. Requests are not absolute. The linux CPU scheduler prioritizes
// requests in proportion to other tasks that are competing for CPU usage. So
// the small SQL server CPU requests have an infinitely higher priority than
// the absent KV server requests. Individual SQL servers are unable to starve a
// KV server, but in aggregate they could consume all of the CPU on the node.
//
// # Question: Why do we set a CPU limit
// Serverless wants to oversubscribe resources. So it is tempting to set no
// limit on the KV servers so that each KV server can use spare capacity. The
// issue is resource requests are prioritized proportionally to the current
// demand. Since KV servers have a large request, KV servers would starve SQL
// servers. If all of the KV cpu consumption was from latency sensitive
// workloads, it may be desirable to let KV consume all of the CPU resources.
// But the KV server may be handling non-critical workloads like backups. So we
// set a limit on KV server consumption to prevent backups from starving the
// SQL servers.
//
// # Question: Why do we set a CPU limit equal to the CPU request
// Setting request == limit is expected to behave better with the go runtime
// and admission control. The go scheduler uses the CPU limit to configure
// GOMAXPROCS. CPU requests are enforced over 100 ms increments. So setting CPU
// requests to half of the limit means a KV server may consume 100% of the
// limit for 50ms, then get throttled for 50ms. This delays when admission
// control kicks in and may increase tail latency for critical KV operations.
func HostConditionalResourceRequirements() *crdbv1beta1.ConditionalResourceRequirements {
	cpuResources := []crdbv1beta1.ConditionalQuantity{
		{RequiredCapacity: resource.MustParse("3"), Quantity: resource.MustParse("2")},
		{RequiredCapacity: resource.MustParse("7"), Quantity: resource.MustParse("5")},
		{RequiredCapacity: resource.MustParse("15"), Quantity: resource.MustParse("10")},
		{RequiredCapacity: resource.MustParse("31"), Quantity: resource.MustParse("19")},
	}
	return &crdbv1beta1.ConditionalResourceRequirements{
		Limits: map[corev1.ResourceName][]crdbv1beta1.ConditionalQuantity{
			"cpu": cpuResources,
		},
		Requests: map[corev1.ResourceName][]crdbv1beta1.ConditionalQuantity{
			"cpu": cpuResources,
		},
	}
}

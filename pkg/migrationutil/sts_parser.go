package migrationutil

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

const (
	// CrdbContainerName is the name of the CockroachDB container
	CrdbContainerName = "db"

	// DefaultPVCName is the default PVC template name used by CRDB StatefulSets.
	DefaultPVCName = "datadir"

	// Command line flag prefixes
	joinStrPrefix          = "--join="
	portPrefix             = "--port="
	httpPortPrefix         = "--http-port="
	insecureFlag           = "--insecure"
	localityFlag           = "--locality"
	logtostderrFlag        = "--logtostderr"
	logFlag                = "--log"
	listenAddrPrefix       = "--listen-addr="
	sqlAddrPrefix          = "--sql-addr="
	httpAddrPrefix         = "--http-addr="
	advertiseAddrPrefix    = "--advertise-addr="
	advertiseHostPrefix    = "--advertise-host="
	advertiseSQLAddrPrefix = "--advertise-sql-addr="
	certsDirPrefix         = "--certs-dir="
)

// operatorManagedPrefixes lists flag prefixes that the operator derives from first-class
// spec fields (ports, TLS, join, logging). Flags with these prefixes must not be persisted
// in AdditionalArgs because the operator regenerates them each reconcile.
var operatorManagedPrefixes = []string{
	listenAddrPrefix, sqlAddrPrefix, httpAddrPrefix,
	advertiseAddrPrefix, advertiseHostPrefix, advertiseSQLAddrPrefix,
	certsDirPrefix, joinStrPrefix,
	logFlag + "=", "--log-config-file=", "--log-config-vars=",
	"--wal-failover=", localityFlag + "=", "--locality-file=",
	portPrefix, httpPortPrefix, logtostderrFlag,
}

// IsOperatorManagedFlag returns true for flags that the operator regenerates from
// first-class fields each reconcile. These must not appear in AdditionalArgs.
func IsOperatorManagedFlag(flag string) bool {
	for _, prefix := range operatorManagedPrefixes {
		if strings.HasPrefix(flag, prefix) {
			return true
		}
	}
	return flag == insecureFlag
}

// ParsedMigrationContext holds parsed information from the existing StatefulSet.
// It is used by both the conversion webhook and the migration controller.
type ParsedMigrationContext struct {
	SQLPort          int32
	GRPCPort         int32
	HTTPPort         int32
	TLSEnabled       bool
	LocalityLabels   []string
	LoggingConfigMap string
	StartFlags       *v1beta1.Flags
	Image            string
	EnvVars          []corev1.EnvVar
	Region           string
	// Domain is the Kubernetes cluster domain, extracted from the --join flag addresses.
	// Empty means cluster.local (the universal default). Only non-empty for custom-domain clusters.
	Domain string

	// Certificate Info
	SourceCASecretName     string
	SourceNodeSecretName   string
	SourceClientSecretName string
	CASecretName           string
	NodeSecretName         string
	ClientSecretName       string
	HTTPSecretName         string
	CAConfigMap            string

	// PVC Info
	PVCTemplateName string

	// WAL Failover Info (extracted from VolumeClaimTemplates and --wal-failover flag)
	WALFailoverSpec *v1beta1.CrdbWalFailoverSpec

	// Physical Cluster Replication (PCR) Info (detected from init job)
	VirtualClusterSpec *v1beta1.CrdbVirtualClusterSpec

	// Ingress Info (detected from Ingress resource, Helm-only)
	IngressConfig any // stored as a raw interface to avoid api/v1alpha1 import in pkg

	// Scheduling & Pod Attributes
	PriorityClassName string
	InitContainers    []corev1.Container
	CustomVolumes     []corev1.Volume
	// CustomVolumeMounts are non-operator mounts on the CRDB container (e.g. secret-* from
	// statefulset.secretMounts, statefulset.volumeMounts). Operator-managed mounts
	// (datadir, certs, certs-secret, client-secret, failoverdir, logsdir, log-config)
	// are excluded because the CockroachDB Operator regenerates them from first-class fields.
	CustomVolumeMounts            []corev1.VolumeMount
	ImagePullSecrets              []corev1.LocalObjectReference
	ServiceAccountName            string
	TerminationGracePeriodSeconds *int64
	// PodSecurityContext is the pod-level security context from the STS (fsGroup, runAs*).
	PodSecurityContext *corev1.PodSecurityContext
}

// NormalizeRuntimeTLSConfig rewrites legacy public-operator secret names to the
// names the enterprise operator uses after migration. Helm defaults and custom
// cert-manager secret names are left alone.
func NormalizeRuntimeTLSConfig(pc *ParsedMigrationContext, clusterName string) {
	if pc == nil {
		return
	}

	legacyNodeSecretName := fmt.Sprintf("%s-node", clusterName)
	legacyClientSecretName := fmt.Sprintf("%s-root-client", clusterName)
	nodeSecretName := pc.SourceNodeSecretName
	if nodeSecretName == "" {
		nodeSecretName = pc.NodeSecretName
	}
	clientSecretName := pc.SourceClientSecretName
	if clientSecretName == "" {
		clientSecretName = pc.ClientSecretName
	}

	if nodeSecretName == "" || nodeSecretName == legacyNodeSecretName {
		pc.NodeSecretName = fmt.Sprintf("%s-node-secret", clusterName)
	} else {
		pc.NodeSecretName = nodeSecretName
	}
	if clientSecretName == "" || clientSecretName == legacyClientSecretName {
		pc.ClientSecretName = fmt.Sprintf("%s-client-secret", clusterName)
	} else {
		pc.ClientSecretName = clientSecretName
	}
	// HTTPSecretName should always track ClientSecretName unless explicitly overridden.
	// ParseStatefulSet initializes both to {name}-client-secret, but parseVolumesAndSecrets
	// only updates SourceClientSecretName from the STS volumes, not HTTPSecretName. Without
	// this unconditional assignment, cert-manager clusters keep the stale default while
	// ClientSecretName gets the resolved cert-manager secret name.
	pc.HTTPSecretName = pc.ClientSecretName
	if pc.SourceCASecretName != "" {
		pc.CASecretName = pc.SourceCASecretName
	}
	if pc.CAConfigMap == "" {
		pc.CAConfigMap = fmt.Sprintf("%s-ca-crt", clusterName)
	}
}

// BuildExternalCertificates constructs the Certificates spec from a parsed migration
// context. The conversion webhook and migration controller both need to map the same
// parsed secret names into ExternalCertificates, and having two copies means one can
// drift without the other noticing.
func BuildExternalCertificates(pc *ParsedMigrationContext) v1beta1.Certificates {
	return v1beta1.Certificates{
		ExternalCertificates: &v1beta1.ExternalCertificates{
			CAConfigMapName:         pc.CAConfigMap,
			NodeSecretName:          pc.NodeSecretName,
			RootSQLClientSecretName: pc.ClientSecretName,
			HTTPSecretName:          pc.HTTPSecretName,
		},
	}
}

// ParseStatefulSet extracts configuration from the StatefulSet for migration.
func ParseStatefulSet(
	sts *appsv1.StatefulSet, clusterName string,
) (*ParsedMigrationContext, error) {
	pc := &ParsedMigrationContext{
		TLSEnabled:      true, // default assumption, overridden by --insecure flag
		GRPCPort:        DefaultGRPCPort,
		PVCTemplateName: DefaultPVCName,
	}

	// Defaults that must exist even when StatefulSet is missing (conversion without STS).
	pc.ServiceAccountName = clusterName
	pc.SourceCASecretName = fmt.Sprintf("%s-ca-secret", clusterName)
	pc.SourceNodeSecretName = fmt.Sprintf("%s-node-secret", clusterName)
	pc.SourceClientSecretName = fmt.Sprintf("%s-client-secret", clusterName)
	pc.CASecretName = pc.SourceCASecretName
	pc.NodeSecretName = pc.SourceNodeSecretName
	pc.ClientSecretName = pc.SourceClientSecretName
	pc.HTTPSecretName = pc.ClientSecretName
	pc.CAConfigMap = fmt.Sprintf("%s-ca-crt", clusterName)

	if sts == nil {
		return pc, nil
	}

	parsePodAttributes(pc, sts)
	parseInitContainers(pc, sts)
	pc.PVCTemplateName = findDataPVCTemplateName(sts)
	parseVolumesAndSecrets(pc, sts)
	// parseCRDBContainer populates StartFlags, which parseWALFailover reads.
	if err := parseCRDBContainer(pc, sts); err != nil {
		return pc, err
	}
	parseWALFailover(pc, sts)
	NormalizeRuntimeTLSConfig(pc, clusterName)

	return pc, nil
}

// parsePodAttributes extracts pod-level scheduling and security attributes from the STS template.
func parsePodAttributes(pc *ParsedMigrationContext, sts *appsv1.StatefulSet) {
	pc.PriorityClassName = sts.Spec.Template.Spec.PriorityClassName
	pc.ImagePullSecrets = sts.Spec.Template.Spec.ImagePullSecrets
	if sts.Spec.Template.Spec.ServiceAccountName != "" {
		pc.ServiceAccountName = sts.Spec.Template.Spec.ServiceAccountName
	}
	pc.TerminationGracePeriodSeconds = sts.Spec.Template.Spec.TerminationGracePeriodSeconds
	pc.PodSecurityContext = sts.Spec.Template.Spec.SecurityContext
}

// parseInitContainers copies init containers from the STS, filtering out operator-managed
// ones such as "copy-certs" which the CockroachDB Operator recreates from its own logic.
func parseInitContainers(pc *ParsedMigrationContext, sts *appsv1.StatefulSet) {
	ignoreInitContainers := []string{"copy-certs"}
	for _, initContainer := range sts.Spec.Template.Spec.InitContainers {
		if !slices.Contains(ignoreInitContainers, initContainer.Name) {
			pc.InitContainers = append(pc.InitContainers, initContainer)
		}
	}
}

// parseVolumesAndSecrets extracts volume configurations and certificate secret names.
// Operator-managed volumes (datadir, certs, etc.) are excluded so the CockroachDB Operator
// can regenerate them from first-class spec fields.
// "logsdir" is excluded here because buildNodeSpec migrates it to CrdbNodeSpec.LogsStore
// directly from the VolumeClaimTemplates list. "failoverdir" is handled separately via WALFailoverSpec.
func parseVolumesAndSecrets(pc *ParsedMigrationContext, sts *appsv1.StatefulSet) {
	ignoreVolumes := map[string]bool{
		"datadir": true, "certs": true, "certs-secret": true,
		"client-secret": true, "ca-certs": true, "emptydir": true,
		"failoverdir": true, "logsdir": true, "log-config": true,
	}

	for _, volume := range sts.Spec.Template.Spec.Volumes {
		// Capture log config source. Helm uses Secrets; CockroachDB Operator uses ConfigMaps.
		// If log-config is a Secret, convertLogConfigSecretToConfigMap will handle conversion.
		if volume.Name == "log-config" {
			if volume.Secret != nil {
				pc.LoggingConfigMap = volume.Secret.SecretName
			} else if volume.ConfigMap != nil {
				pc.LoggingConfigMap = volume.ConfigMap.Name
			}
		}

		// Heuristically identify certificate secrets from cert volumes. Helm
		// self-signer uses "certs" (projected with node+client secrets). Helm
		// cert-manager and provided-cert paths use "certs-secret" for the node
		// cert and "client-secret" for the client cert.
		switch volume.Name {
		case "certs":
			if volume.Projected != nil {
				for _, src := range volume.Projected.Sources {
					if src.Secret != nil {
						if strings.Contains(src.Secret.Name, "node") {
							pc.SourceNodeSecretName = src.Secret.Name
						} else if strings.Contains(src.Secret.Name, "root") || strings.Contains(src.Secret.Name, "client") {
							pc.SourceClientSecretName = src.Secret.Name
						}
					}
				}
			} else if volume.Secret != nil {
				pc.SourceNodeSecretName = volume.Secret.SecretName
			}
		case "certs-secret":
			// cert-manager and provided-cert paths mount node cert here. Helm charts
			// use exactly one secret source per projected volume, so iterating all
			// sources and keeping the last one is safe.
			if volume.Projected != nil {
				for _, src := range volume.Projected.Sources {
					if src.Secret != nil {
						pc.SourceNodeSecretName = src.Secret.Name
					}
				}
			} else if volume.Secret != nil {
				pc.SourceNodeSecretName = volume.Secret.SecretName
			}
		case "client-secret":
			// Cert-manager and provided-cert paths mount client cert here.
			if volume.Projected != nil {
				for _, src := range volume.Projected.Sources {
					if src.Secret != nil {
						pc.SourceClientSecretName = src.Secret.Name
					}
				}
			} else if volume.Secret != nil {
				pc.SourceClientSecretName = volume.Secret.SecretName
			}
		case "ca-certs":
			if volume.Secret != nil {
				pc.SourceCASecretName = volume.Secret.SecretName
			}
		}

		if !ignoreVolumes[volume.Name] {
			pc.CustomVolumes = append(pc.CustomVolumes, volume)
		}
	}
}

// parseCRDBContainer extracts image, env vars, custom volume mounts, and start flags from
// the CockroachDB container. Returns an error if flag parsing fails.
// Prefers the container named "db", then a single container pod, then a container whose
// image or command clearly points at CockroachDB.
func parseCRDBContainer(pc *ParsedMigrationContext, sts *appsv1.StatefulSet) error {
	container := FindCRDBContainer(sts)
	if container == nil {
		return nil
	}

	pc.Image = container.Image
	pc.EnvVars = dedupeEnvVars(container.Env)

	// Copy non-operator-managed volume mounts. Operator-managed mounts are excluded
	// because the CockroachDB Operator regenerates them from first-class fields
	// (DataStore, Certificates, WALFailoverSpec, LogsStore).
	operatorMounts := map[string]bool{
		"datadir": true, "certs": true, "certs-secret": true,
		"client-secret": true, "failoverdir": true, "logsdir": true, "log-config": true,
	}
	for _, mount := range container.VolumeMounts {
		// skip datadir-N mounts (multi-store)
		if !operatorMounts[mount.Name] && !strings.HasPrefix(mount.Name, "datadir-") {
			pc.CustomVolumeMounts = append(pc.CustomVolumeMounts, mount)
		}
	}

	args := extractContainerArgs(container)
	if err := extractFlags(pc, args, sts.Name, sts.Namespace); err != nil {
		return err
	}

	// Defensive de-dupe: some StatefulSets carry repeated args or semantically equivalent
	// sources. Dedupe so that StartFlags.Upsert output is stable before conversion.
	if pc.StartFlags != nil && len(pc.StartFlags.Upsert) > 1 {
		pc.StartFlags.Upsert = dedupeStrings(pc.StartFlags.Upsert)
	}
	return nil
}

// FindCRDBContainer returns a pointer to the CockroachDB container in the StatefulSet.
// It prefers the container named "db" (Helm/public operator convention), then falls back
// to any container whose image or command contains "cockroach". Returns nil if no match
// is found, including single-container pods with wrapper images that don't reference
// "cockroach" in their image name or command.
func FindCRDBContainer(sts *appsv1.StatefulSet) *corev1.Container {
	if sts == nil {
		return nil
	}
	containers := sts.Spec.Template.Spec.Containers
	for i := range containers {
		if containers[i].Name == CrdbContainerName {
			return &containers[i]
		}
	}
	for i := range containers {
		if containerLooksLikeCockroach(&containers[i]) {
			return &containers[i]
		}
	}
	return nil
}

func containerLooksLikeCockroach(container *corev1.Container) bool {
	if container == nil {
		return false
	}
	if strings.Contains(container.Image, "cockroach") {
		return true
	}
	for _, part := range slices.Concat(container.Command, container.Args) {
		if strings.Contains(part, "cockroach") {
			return true
		}
	}
	return false
}

func findDataPVCTemplateName(sts *appsv1.StatefulSet) string {
	if len(sts.Spec.VolumeClaimTemplates) == 0 {
		return DefaultPVCName
	}
	if len(sts.Spec.VolumeClaimTemplates) == 1 {
		return sts.Spec.VolumeClaimTemplates[0].Name
	}

	crdbContainer := FindCRDBContainer(sts)
	if crdbContainer != nil {
		for _, mount := range crdbContainer.VolumeMounts {
			if strings.HasPrefix(strings.TrimSuffix(mount.MountPath, "/"), "/cockroach/cockroach-data") {
				for i := range sts.Spec.VolumeClaimTemplates {
					if sts.Spec.VolumeClaimTemplates[i].Name == mount.Name {
						return mount.Name
					}
				}
			}
		}
	}
	for i := range sts.Spec.VolumeClaimTemplates {
		if sts.Spec.VolumeClaimTemplates[i].Name == DefaultPVCName {
			return DefaultPVCName
		}
	}
	return sts.Spec.VolumeClaimTemplates[0].Name
}

// extractContainerArgs returns the cockroachdb start args from a container's Command or Args.
// Handles the common bash wrapper pattern: ["/bin/bash", "-c", "exec cockroach start ..."].
func extractContainerArgs(container *corev1.Container) []string {
	var args []string
	if len(container.Command) > 0 {
		for _, cmd := range container.Command {
			if strings.Contains(cmd, " start ") {
				args = strings.Fields(cmd)
				break
			}
		}
	}
	if len(args) == 0 {
		args = container.Args
	}
	// Common STS pattern: args contain "shell -ecx" and the full start command as a single arg.
	for _, a := range args {
		if strings.Contains(a, " start ") {
			return strings.Fields(a)
		}
	}
	return args
}

// parseWALFailover detects WAL failover configuration from VolumeClaimTemplates and start flags.
// Must be called after parseCRDBContainer so that pc.StartFlags is populated.
func parseWALFailover(pc *ParsedMigrationContext, sts *appsv1.StatefulSet) {
	if pc.StartFlags == nil {
		return
	}
	for _, vct := range sts.Spec.VolumeClaimTemplates {
		if !strings.Contains(strings.ToLower(vct.Name), "failover") {
			continue
		}
		for _, flag := range pc.StartFlags.Upsert {
			if !strings.HasPrefix(flag, "--wal-failover=") {
				continue
			}
			val := strings.TrimPrefix(flag, "--wal-failover=")
			if val == "disabled" || val == "among-stores" || !strings.HasPrefix(val, "path=") {
				break
			}
			path := strings.TrimPrefix(val, "path=")
			size := ""
			if storage := vct.Spec.Resources.Requests[corev1.ResourceStorage]; !storage.IsZero() {
				size = storage.String()
			}
			storageClass := ""
			if vct.Spec.StorageClassName != nil {
				storageClass = *vct.Spec.StorageClassName
			}
			pc.WALFailoverSpec = &v1beta1.CrdbWalFailoverSpec{
				Name:             vct.Name,
				Status:           v1beta1.WalEnable,
				Path:             path,
				Size:             size,
				StorageClassName: storageClass,
			}
			break
		}
		break
	}
}

// operatorManagedEnvVars are env vars the public operator sets that the CockroachDB Operator
// manages through its own pod generation. Carrying them over from the STS causes conflicts.
// COCKROACH_HOST, for instance, interferes with the NodeID exec command.
var operatorManagedEnvVars = map[string]bool{
	"COCKROACH_HOST":     true,
	"COCKROACH_RPC_PORT": true,
	"COCKROACH_SQL_PORT": true,
}

func dedupeEnvVars(in []corev1.EnvVar) []corev1.EnvVar {
	seen := make(map[string]struct{}, len(in))
	out := make([]corev1.EnvVar, 0, len(in))
	for _, env := range in {
		if operatorManagedEnvVars[env.Name] {
			continue
		}
		if _, ok := seen[env.Name]; ok {
			continue
		}
		seen[env.Name] = struct{}{}
		out = append(out, env)
	}
	return out
}

// extractAddrPort returns the port number from an address string like ":26258" or "0.0.0.0:26258".
// Returns 0 if the address cannot be parsed or has no port.
func extractAddrPort(addr string) int32 {
	if idx := strings.LastIndex(addr, ":"); idx >= 0 {
		if n, err := strconv.ParseInt(addr[idx+1:], 10, 32); err == nil && n > 0 {
			return int32(n)
		}
	}
	return 0
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// flagRegex matches a --key=value CLI flag and captures the key and value groups.
var flagRegex = regexp.MustCompile(`--([\w-]+)=(.*)`)

func extractFlags(pc *ParsedMigrationContext, args []string, clusterName, namespace string) error {
	pc.StartFlags = &v1beta1.Flags{}

	// pendingJoinArg defers --join rewriting until after all flags are parsed.
	// applyJoinFlag bakes pc.GRPCPort into the join service DNS, so it must run
	// after --listen-addr (which may appear after --join in the STS args) has had
	// the chance to update pc.GRPCPort to the actual port.
	var pendingJoinArg string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case strings.HasPrefix(arg, joinStrPrefix):
			// Defer join rewriting. See pendingJoinArg comment above.
			pendingJoinArg = arg

		case strings.HasPrefix(arg, portPrefix):
			num, err := strconv.ParseInt(strings.TrimPrefix(arg, portPrefix), 10, 32)
			if err != nil {
				return errors.Wrapf(err, "invalid --port value")
			}
			pc.SQLPort = int32(num)

		case strings.HasPrefix(arg, httpPortPrefix):
			num, err := strconv.ParseInt(strings.TrimPrefix(arg, httpPortPrefix), 10, 32)
			if err != nil {
				return errors.Wrapf(err, "invalid --http-port value")
			}
			pc.HTTPPort = int32(num)

		case strings.HasPrefix(arg, insecureFlag):
			pc.TLSEnabled = false

		case strings.HasPrefix(arg, localityFlag+"="):
			value := strings.TrimPrefix(arg, localityFlag+"=")
			for label := range strings.SplitSeq(value, ",") {
				parts := strings.Split(label, "=")
				if len(parts) > 0 {
					pc.LocalityLabels = append(pc.LocalityLabels, parts[0])
					if parts[0] == "region" && len(parts) > 1 {
						pc.Region = parts[1]
					}
				}
			}

		// Extract port from address flags but do not add them to StartFlags. The operator
		// regenerates --listen-addr/--sql-addr/--http-addr from first-class port fields,
		// so keeping these flags would override the correct computed values.
		case strings.HasPrefix(arg, listenAddrPrefix):
			if port := extractAddrPort(strings.TrimPrefix(arg, listenAddrPrefix)); port > 0 {
				pc.GRPCPort = port
			}
		case strings.HasPrefix(arg, sqlAddrPrefix):
			if port := extractAddrPort(strings.TrimPrefix(arg, sqlAddrPrefix)); port > 0 {
				pc.SQLPort = port
			}
		case strings.HasPrefix(arg, httpAddrPrefix):
			if port := extractAddrPort(strings.TrimPrefix(arg, httpAddrPrefix)); port > 0 {
				pc.HTTPPort = port
			}

		// Skip advertise/certs flags. They are derived from port + FQDN or TLSEnabled.
		// --advertise-host is the legacy alias for --advertise-addr used by the public operator.
		case strings.HasPrefix(arg, advertiseAddrPrefix),
			strings.HasPrefix(arg, advertiseHostPrefix),
			strings.HasPrefix(arg, advertiseSQLAddrPrefix),
			strings.HasPrefix(arg, certsDirPrefix):
			continue

		// Skip logs/start/binary
		case strings.HasPrefix(arg, logtostderrFlag), strings.HasPrefix(arg, logFlag),
			strings.HasPrefix(arg, "start"), strings.HasPrefix(arg, "/cockroach"):
			continue

		default:
			if matches := flagRegex.FindStringSubmatch(arg); len(matches) == 3 {
				pc.StartFlags.Upsert = append(pc.StartFlags.Upsert, fmt.Sprintf("--%s=%s", matches[1], matches[2]))
			} else if strings.HasPrefix(arg, "--") {
				// The arg has no =value suffix. It's either a bare boolean flag like
				// --accept-sql-without-tls, or a space-separated key value pair where
				// the value is the next element in the args array (e.g. --cache 25%).
				if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
					value := args[i+1]
					i++ // consume the value arg
					// Shell expressions like $(expr ...) can't be evaluated outside a
					// shell and produce garbage values. Skip them as the enterprise
					// operator computes its own defaults for cache and max-sql-memory.
					if strings.Contains(value, "$(") || strings.Contains(value, "${") {
						continue
					}
					pc.StartFlags.Upsert = append(pc.StartFlags.Upsert, fmt.Sprintf("%s=%s", arg, value))
				} else {
					// Genuine bare boolean flag.
					pc.StartFlags.Upsert = append(pc.StartFlags.Upsert, arg)
				}
			}
		}
	}

	// Apply the join flag now that GRPCPort is finalized. The port may have been
	// updated by --listen-addr parsed later in the loop, so deferring here ensures
	// the join service DNS uses the correct port even when --join precedes --listen-addr.
	if pendingJoinArg != "" {
		applyJoinFlag(pc, pendingJoinArg, clusterName, namespace)
	}

	return nil
}

// applyJoinFlag extracts the cluster domain from a --join flag and replaces the static pod
// address list with a dynamic join service DNS. The domain is stored in pc.Domain only when
// it differs from the default "cluster.local".
func applyJoinFlag(pc *ParsedMigrationContext, joinArg, clusterName, namespace string) {
	if pc.Domain == "" {
		pc.Domain = extractDomainFromJoin(joinArg)
	}
	joinDomain := DefaultClusterDomain
	if pc.Domain != "" {
		joinDomain = pc.Domain
	}
	joinService := fmt.Sprintf("--join=%s-join.%s.svc.%s:%d", clusterName, namespace, joinDomain, pc.GRPCPort)
	pc.StartFlags.Upsert = append(pc.StartFlags.Upsert, joinService)
}

// extractDomainFromJoin parses the Kubernetes cluster domain from the first address in a
// --join flag. Addresses have the form <pod>.<svc>.<ns>.svc.<domain>:<port>. Returns an
// empty string for the default "cluster.local" domain or when the address cannot be parsed.
func extractDomainFromJoin(joinArg string) string {
	raw := strings.TrimPrefix(joinArg, joinStrPrefix)
	// take only the first address (all share the same domain)
	first := strings.SplitN(raw, ",", 2)[0]
	// strip port
	if idx := strings.LastIndex(first, ":"); idx != -1 {
		first = first[:idx]
	}
	if _, domain, ok := strings.Cut(first, ".svc."); ok {
		if domain != "" && domain != "cluster.local" {
			return domain
		}
	}
	return ""
}

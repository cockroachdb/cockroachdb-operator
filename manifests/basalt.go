package manifests

import (
	"fmt"
	"maps"
	"sort"
	"strings"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/hashutil"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// defaultBasaltCLIImage is the basalt CLI image used by the store cleanup
	// Jobs when BasaltStoreSpec.CLIImage is empty. The floor is v0.1.14, the
	// first tag whose basalt rm supports the recursive -r flag the cleanup
	// commands rely on.
	//
	// Note: When bumping this image, re-verify the CLI error contract the
	// idempotent cleanup commands depend on. A command error must still exit
	// non-zero and render gRPC AlreadyExists as "already exists" and NotFound as
	// "no such object or directory" (see basaltIdempotentCmd).
	defaultBasaltCLIImage = "us-docker.pkg.dev/releases-prod/cockroachdb-basalt-releases/basalt-cli:v0.1.19"

	// basaltControllerAlias is the alias referenced by the basalt store path. The
	// --basalt-cluster flag binds this alias to the controller addresses, and the
	// --store path references it.
	basaltControllerAlias = "default"

	// basaltTenantEnvVar and basaltControllerAddrsEnvVar carry the tenant ID and
	// controller addresses into the container as env vars. Both the CockroachDB
	// start command and the lifecycle Jobs reference these quoted, so the values
	// are passed as data and never interpreted as shell syntax. They are prefixed
	// to avoid colliding with other env vars on the CockroachDB container.
	//
	// The tenant ID also serves as the Basalt namespace directory. The
	// --basalt-namespace flag and the store URL both reference this value, so the
	// same UUID scopes both tenant identity and the top-level directory.
	basaltTenantEnvVar          = "BASALT_TENANT_ID"
	basaltControllerAddrsEnvVar = "BASALT_CONTROLLER_ADDRS"

	// BasaltLifecycleResourceUIDAnnotation identifies the exact CrdbNode a cleanup
	// Job belongs to. Cleanup Jobs intentionally outlive the node they clean up
	// after, so name equality alone cannot distinguish a stale Job from one
	// created for a later node with the same name.
	BasaltLifecycleResourceUIDAnnotation = "crdb.cockroachlabs.com/basalt-lifecycle-resource-uid"

	// BasaltClusterKeyCurrentFile and BasaltClusterKeyOldFile are the file names
	// CockroachDB looks for inside the --cluster-key-dir directory. "current" is
	// required whenever encryption is on. "old" exists only during a key
	// rotation window. The names are fixed by CockroachDB
	// (pkg/storage/fs/cluster_key_manager.go), which stats them by name rather
	// than enumerating the directory. Other files in the mount are therefore
	// harmless, so we can reuse the existing key volume.
	BasaltClusterKeyCurrentFile = "current"
	BasaltClusterKeyOldFile     = "old"

	// basaltCertsVolumeName is the pod volume carrying the projected basalt
	// tenant leaf certificate, its key, and the basalt cluster CA.
	basaltCertsVolumeName = "basalt-certs"

	// basaltCAFileName is the filename basaltclient hardcodes for the trust
	// anchor inside a certs directory. The control plane writes the CA under the
	// same key in the ConfigMap, so the projection needs no rename.
	basaltCAFileName = "ca.crt"
)

// basaltTenantCertFileName and basaltTenantKeyFileName return the filenames the
// basalt client looks for inside a certs directory. Basalt fixes the naming, so
// the projection has to rename the tls.crt and tls.key keys on the way in.
func basaltTenantCertFileName(tenantID string) string {
	return fmt.Sprintf("tenant.%s.crt", tenantID)
}

func basaltTenantKeyFileName(tenantID string) string {
	return fmt.Sprintf("tenant.%s.key", tenantID)
}

// basaltCertsQuery returns the URL query that points basalt at the projected
// certificate directory, or the empty string for an insecure store.
// CockroachDB rejects a node whose basalt stores disagree on their cert paths,
// so every generated --store carries the identical query.
func basaltCertsQuery(spec *crdbv1beta1.BasaltStoreSpec) string {
	if !spec.UsesTLS() {
		return ""
	}
	return "?certs-dir=" + BasaltCertsPath
}

// BasaltCertsVolume projects the tenant credentials and the basalt cluster CA
// into a single read-only volume, with every file named the way basalt expects
// to find it in a certs-dir.
func BasaltCertsVolume(spec *crdbv1beta1.BasaltStoreSpec) corev1.Volume {
	return VolumeProjection(
		basaltCertsVolumeName,
		[]SecretProjection{{
			Name: spec.TLS.ClientSecretName,
			Renames: []corev1.KeyToPath{
				{Key: corev1.TLSCertKey, Path: basaltTenantCertFileName(spec.TenantID)},
				{Key: corev1.TLSPrivateKeyKey, Path: basaltTenantKeyFileName(spec.TenantID)},
			},
		}},
		[]ConfigMapProjection{{
			Name:    spec.TLS.CAConfigMapName,
			Renames: []corev1.KeyToPath{{Key: basaltCAFileName, Path: basaltCAFileName}},
		}},
	)
}

// BasaltCertsVolumeMount mounts the basalt certs volume at the path the
// --store certs-dir query points to.
func BasaltCertsVolumeMount() corev1.VolumeMount {
	return corev1.VolumeMount{
		Name:      basaltCertsVolumeName,
		MountPath: BasaltCertsPath,
		ReadOnly:  true,
	}
}

// basaltClusterKeyEnabled reports whether this node should run with basalt
// encryption at rest, i.e. whether the operator should render --cluster-key-dir
// and suppress --enterprise-encryption.
//
// A nil KeySecretName is how the CRD encodes "encryption disabled" even when
// EncryptionAtRest itself is set, so it is not enough to check for a basalt
// store. An existing unencrypted basalt cluster must keep rendering exactly
// the flags it renders today, and it has no key volume mounted to point the
// flag at.
func basaltClusterKeyEnabled(node *crdbv1beta1.CrdbNode) bool {
	return node.Spec.DataStore.IsBasalt() &&
		node.Spec.EncryptionAtRest != nil &&
		node.Spec.EncryptionAtRest.KeySecretName != nil
}

// basaltDisableRSEngine reports whether to pass --disable-rsengine. RSEngine is
// now enabled by default for basalt clusters. Set DisableRSEngine to true to
// explicitly disable it.
func basaltDisableRSEngine(spec *crdbv1beta1.BasaltStoreSpec) bool {
	if spec.DisableRSEngine == nil {
		return false
	}
	return *spec.DisableRSEngine
}

// basaltConfigFlags returns the non-repeatable cockroach start flags for basalt
// storage. The --store flags are returned separately by basaltStoreArgs because
// a node may have multiple stores and maps cannot hold duplicate keys.
func basaltConfigFlags(node *crdbv1beta1.CrdbNode) map[string]string {
	spec := node.Spec.DataStore.Basalt
	flags := map[string]string{
		"--basalt-cluster":   fmt.Sprintf(`"%s@$%s"`, basaltControllerAlias, basaltControllerAddrsEnvVar),
		"--basalt-namespace": fmt.Sprintf(`"$%s"`, basaltTenantEnvVar),
	}
	// When a scratch PVC backs the local scratch dir, point CRDB's temp/spill at
	// it so disk spills land on the bounded PVC instead of the node's boot disk.
	if spec.ScratchVolumeClaimTemplate != nil {
		flags["--temp-dir"] = CockroachDataDirMountAbsPath
	}
	if basaltDisableRSEngine(spec) {
		flags["--disable-rsengine"] = ""
	}
	// On a basalt store the cluster key is the sole encryption key. It covers
	// both the range-shared and store-local filesystems, so it replaces
	// --enterprise-encryption rather than supplementing it. Supplying the flag
	// is what turns encryption on. Omitting it leaves basalt unencrypted.
	if basaltClusterKeyEnabled(node) {
		flags["--cluster-key-dir"] = keyMountPath
	}
	return flags
}

// basaltStoreArgs returns the --store flags for a basalt-backed node. A single
// store uses the directory <node.Name>/store. Multiple stores use
// <node.Name>/store1, <node.Name>/store2, etc. The store-cleanup path relies on
// this being a subtree of <node.Name> so a single rm -r reclaims it.
//
// TODO(prod-orch): basalt#650 (BAS-540) will reserve node/store ids before start
// and name these dirs n<node-id>/s<store-id> to match CRDB's own identifiers.
// When it lands, this naming and the matching cleanup path must change together.
func basaltStoreArgs(node *crdbv1beta1.CrdbNode) []string {
	spec := node.Spec.DataStore.Basalt
	count := spec.GetStoreCount()
	certsQuery := basaltCertsQuery(spec)
	stores := make([]string, count)
	for i := range count {
		var storeName string
		if count == 1 {
			storeName = fmt.Sprintf("%s/store", node.Name)
		} else {
			storeName = fmt.Sprintf("%s/store%d", node.Name, i+1)
		}
		// The tenant stays in the store URL even in secure mode, because that is
		// what basaltclient derives the tenant.<id>.crt and .key filenames from.
		// It carries no authority of its own. The basalt controller authenticates
		// the connection off the presented certificate, which has to have both
		// OU=BasaltTenant and a basalt://<id>.tenant.basalt.internal/ URI SAN.
		//
		// CockroachDB splits a --store value on the first "=", so a bare URL with a
		// certs-dir query gets chopped there and rejected. path= keeps the whole
		// URL on the value side.
		stores[i] = fmt.Sprintf(`--store="path=basalt://$%s@%s/%s%s"`,
			basaltTenantEnvVar, basaltControllerAlias, storeName, certsQuery)
	}
	return stores
}

// basaltStoreFlagStrings returns all basalt flags (config + store) formatted as
// command arguments, sorted for stable output. The deprecated parseFlags path
// uses this to append basalt flags to its command slice.
func basaltStoreFlagStrings(node *crdbv1beta1.CrdbNode) []string {
	var out []string
	for k, v := range basaltConfigFlags(node) {
		if v == "" {
			out = append(out, k)
		} else {
			out = append(out, fmt.Sprintf("%s=%s", k, v))
		}
	}
	sort.Strings(out)
	out = append(out, basaltStoreArgs(node)...)
	return out
}

// BasaltCLIImage returns the basalt CLI image for the lifecycle Jobs,
// honoring an explicit override and otherwise using the built-in default.
func BasaltCLIImage(spec *crdbv1beta1.BasaltStoreSpec) string {
	if spec.CLIImage != "" {
		return spec.CLIImage
	}
	return defaultBasaltCLIImage
}

// BasaltEnvVars returns the env vars that carry the tenant id and controller
// addresses into a container. Both the crdb container (for the start flags) and
// the lifecycle Jobs read these. Passing the values as env vars keeps them
// out of the shell command string, so a malicious value cannot inject shell.
func BasaltEnvVars(spec *crdbv1beta1.BasaltStoreSpec) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: basaltControllerAddrsEnvVar, Value: strings.Join(spec.ControllerAddrs, ",")},
		{Name: basaltTenantEnvVar, Value: spec.TenantID},
	}
}

// basaltIdempotentCmd wraps a Basalt CLI invocation so it is safe to retry and
// safe to chain with other commands. The generated shell does four things in order:
//
//  1. Run the Basalt command, capturing combined stdout and stderr in $out.
//  2. Record the exit code in $rc and echo the output for the Pod logs.
//  3. If $rc is non-zero and the output does not match benignPattern, exit with
//     $rc so the Job fails and retries.
//  4. Otherwise fall through so the next snippet in the same shell can run.
//
// It must not emit an explicit "exit 0" on success. Several snippets are joined
// in one shell, and an early exit would skip the remaining cleanup commands.
//
// The Basalt CLI returns exit code 1 for every command error and prints the
// message to stderr, which is why the invocation is captured with 2>&1. It
// renders gRPC AlreadyExists as "already exists" and NotFound as "no such
// object or directory", so matching its output is the only way to distinguish
// a no-op from a real failure. defaultBasaltCLIImage carries the reminder to
// re-check these patterns whenever that image is bumped.
//
// Controller addresses and the tenant ID reach the invocation through quoted
// env vars from BasaltEnvVars, so they are never interpreted as shell syntax.
// Values interpolated directly by a caller must be safe by construction. The
// cleanup paths only interpolate a DNS-safe Pod name and int64 store IDs.
func basaltIdempotentCmd(invocation, benignPattern string) string {
	return fmt.Sprintf(
		`out=$(%[1]s 2>&1); rc=$?; echo "$out"; `+
			`if [ $rc -ne 0 ] && ! echo "$out" | grep -qi "%[2]s"; then exit $rc; fi`,
		invocation, benignPattern,
	)
}

// basaltRemoveDirCmd recursively removes a path below the tenant namespace. An
// empty path removes the namespace itself. A path that is already gone counts
// as success, so a retry after a partial run still passes. certsQuery carries
// the certs-dir for a secure store and is empty otherwise.
//
// TODO(prod-orch): basalt rm -r walks the tree on the client and is not atomic.
// A Job retry can finish an interrupted walk, but data left by a force-swept
// cluster still needs manual or sweeper cleanup. Use an atomic server operation
// when Basalt provides one.
func basaltRemoveDirCmd(relPath, certsQuery string) string {
	// The Basalt namespace directory is the tenant ID, which the CockroachDB Pod
	// sets through --basalt-namespace. The rm command does not read that flag, so
	// its URL must include the namespace as the leading path segment. An empty
	// relPath intentionally targets the namespace root.
	path := fmt.Sprintf(`$%s`, basaltTenantEnvVar)
	if relPath != "" {
		path = fmt.Sprintf(`$%s/%s`, basaltTenantEnvVar, relPath)
	}
	// basalt rm takes one URL. The tenant in userinfo selects the authenticated
	// tenant, while the tenant in the path selects its namespace directory.
	inv := fmt.Sprintf(`basalt rm -r "//$%s@$%s/%s%s"`,
		basaltTenantEnvVar, basaltControllerAddrsEnvVar, path, certsQuery)
	return basaltIdempotentCmd(inv, "no such object or directory")
}

// BasaltStoreCleanupShellCommand removes both kinds of data owned by a node:
// its store-local subtree under <node.Name>, and each range-shared
// s<store-id> directory at the namespace root. The store IDs are captured while
// the node still serves SQL because Basalt cannot recover this mapping after
// decommissioning.
//
// TODO(prod-orch): basalt#650 (BAS-540) will reserve node/store ids up front and
// name the store-local dirs n<node-id>/s<store-id>. Once that lands, switch the
// store-local path here from <node.Name> to the reserved id form.
func BasaltStoreCleanupShellCommand(
	spec *crdbv1beta1.BasaltStoreSpec, nodeName string, storeIDs []int64,
) string {
	certsQuery := basaltCertsQuery(spec)
	cmds := []string{basaltRemoveDirCmd(nodeName, certsQuery)}
	for _, id := range storeIDs {
		cmds = append(cmds, basaltRemoveDirCmd(fmt.Sprintf("s%d", id), certsQuery))
	}
	return strings.Join(cmds, "; ")
}

// BasaltStoreCleanupJobName returns the name of the Job that cleans up basalt
// store directories for a node. The Job name becomes the job-name label on the
// Pods it creates, and a label value only holds 63 characters, so a long node
// name gets truncated and hashed to stay unique.
func BasaltStoreCleanupJobName(nodeName string) string {
	const suffix = "-basalt-cleanup"
	const hashLength = 8

	if len(nodeName) <= validation.DNS1123LabelMaxLength-len(suffix) {
		return nodeName + suffix
	}

	hash := hashutil.MustFNV(nodeName)[:hashLength]
	prefixLength := validation.DNS1123LabelMaxLength - len(suffix) - len(hash) - 1
	return nodeName[:prefixLength] + "-" + hash + suffix
}

// BuildBasaltCleanupJob constructs a one-shot Job that removes a node's Basalt
// store directories.
func BuildBasaltCleanupJob(
	node *crdbv1beta1.CrdbNode,
	spec *crdbv1beta1.BasaltStoreSpec,
	labels, annotations map[string]string,
) *batchv1.Job {
	// Copy labels and annotations into fresh maps so the Job does not share or
	// mutate the source object's maps when the API client writes metadata back on
	// create. The component label also keeps this Pod out of unrelated selectors.
	jobLabels := make(map[string]string)
	maps.Copy(jobLabels, labels)
	jobLabels["app.kubernetes.io/component"] = "basalt-store-cleanup"
	jobAnnotations := make(map[string]string)
	maps.Copy(jobAnnotations, annotations)
	jobAnnotations[BasaltLifecycleResourceUIDAnnotation] = string(node.UID)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        BasaltStoreCleanupJobName(node.Name),
			Namespace:   node.Namespace,
			Labels:      jobLabels,
			Annotations: jobAnnotations,
		},
		Spec: basaltJobSpec(
			"cleanup-basalt-stores",
			BasaltStoreCleanupShellCommand(spec, node.Name, node.Status.StoreIDs),
			spec,
			jobLabels,
			jobAnnotations,
		),
	}
}

// BasaltJobFailed reports whether a basalt lifecycle Job has exhausted its
// retries and reached a terminal failure.
func BasaltJobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// basaltJobSpec returns the JobSpec used by the basalt lifecycle Jobs. A secure
// store gets the same projected certs volume the CockroachDB pod uses, since
// the CLI reads the same certs-dir.
func basaltJobSpec(
	containerName, command string,
	spec *crdbv1beta1.BasaltStoreSpec,
	labels, annotations map[string]string,
) batchv1.JobSpec {
	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount
	if spec.UsesTLS() {
		volumes = append(volumes, BasaltCertsVolume(spec))
		volumeMounts = append(volumeMounts, BasaltCertsVolumeMount())
	}

	return batchv1.JobSpec{
		BackoffLimit:            new(int32(3)),
		ActiveDeadlineSeconds:   new(int64(300)),
		TTLSecondsAfterFinished: new(int32(600)),
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      maps.Clone(labels),
				Annotations: maps.Clone(annotations),
			},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Volumes:       volumes,
				Containers: []corev1.Container{
					{
						Name:         containerName,
						Image:        BasaltCLIImage(spec),
						Command:      []string{"sh", "-c"},
						Args:         []string{command},
						Env:          BasaltEnvVars(spec),
						VolumeMounts: volumeMounts,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("200m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
					},
				},
			},
		},
	}
}

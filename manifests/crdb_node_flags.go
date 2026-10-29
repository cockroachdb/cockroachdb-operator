package manifests

import (
	"fmt"
	"maps"
	"sort"
	"strings"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	manifestslog "github.com/cockroachlabs/cockroach-enterprise-operator/manifests/log"
	"github.com/cockroachlabs/version"
)

// Todo(nishanth): Remove this function once flags is deprecated.
func parseFlags(node *crdbv1beta1.CrdbNode) string {
	advertiseAddr := nodeFQDN(node)

	// See https://www.cockroachlabs.com/docs/stable/cockroach-start.html

	// About `ulimit -S -n 1048576`:
	// Raise the soft limit of file descriptors to 1048576. On AWS we use Bottlerocket for our instance AMI image,
	// which currently sets the soft limit to 65536. This can cause low amount of available open file descriptors on
	// some clusters. We will want to remove this ulimit command and set the process soft limit flag when a Bottlerocket
	// version that fixes this issue: https://github.com/bottlerocket-os/bottlerocket/issues/1703 is released.
	cmd := []string{
		"ulimit -S -n 1048576; ",
		"exec",
		"/cockroach/cockroach",
		"start",
		certsOrInsecureFlag(node.Spec.TLSEnabled),
		// standard bind addresses for clients
		fmt.Sprintf("--listen-addr 0.0.0.0:%d", node.Spec.GetGRPCPort()),
		fmt.Sprintf("--http-addr 0.0.0.0:%d", node.Spec.GetHTTPPort()),
		fmt.Sprintf("--sql-addr 0.0.0.0:%d", node.Spec.GetSQLPort()),
		// advertise (gRPC) to other nodes using hostname:port
		fmt.Sprintf("--advertise-addr %s:%d", advertiseAddr, node.Spec.GetGRPCPort()),
		// advertise (SQL) to other nodes used hostname:port
		fmt.Sprintf("--advertise-sql-addr %s:%d", advertiseAddr, node.Spec.GetSQLPort()),
		// pass service address for join flag
		fmt.Sprintf("--join %s", node.Spec.Join),
	}

	// TODO(jmcarp): Drop `dns` locality label after confirming it's unused.
	// TODO(jmcarp): Use `--locality-file` rather than `--locality`.
	if len(node.Spec.LocalityLabels) > 0 || len(node.Spec.LocalityMappings) > 0 {
		// The init container writes this file. See
		// services/init-container/README.md. The operator points it at
		// /etc/cockroach-env with -locality-dest.
		cmd = append(cmd, fmt.Sprintf("$(cat /etc/cockroach-env/locality),dns=%s", advertiseAddr))
	} else {
		cmd = append(cmd, fmt.Sprintf("--locality dns=%s", advertiseAddr))
	}

	logFlags := map[string]string{}
	buildLoggingFlag(node, logFlags)

	// buildLoggingFlag can produce multiple entries (--log-config-file and
	// --log-config-vars). If there are multiple keys, map iteration is
	// nondeterministic, so we sort them to keep the command string stable
	// across reconciles and prevent golden file diffs from flip-flopping.
	logFlagKeys := make([]string, 0, len(logFlags))
	for k := range logFlags {
		logFlagKeys = append(logFlagKeys, k)
	}
	sort.Strings(logFlagKeys)
	for _, k := range logFlagKeys {
		cmd = append(cmd, fmt.Sprintf("%s=%s", k, logFlags[k]))
	}

	if node.Spec.WALFailoverSpec != nil {
		walPath := walMountPath(node)
		if node.Spec.WALFailoverSpec.Status == crdbv1beta1.WalEnable {
			cmd = append(cmd, fmt.Sprintf("%s=path=%s", WalCmdFlag, walPath))
		} else {
			cmd = append(cmd, fmt.Sprintf("%s=disabled,prev_path=%s", WalCmdFlag, walPath))
		}
	}

	var additionalFlags []string
	for k, v := range node.Spec.Flags {
		if v == "" {
			additionalFlags = append(additionalFlags, k)
		} else {
			additionalFlags = append(additionalFlags, fmt.Sprintf("%s=%s", k, v))
		}
	}

	// Check for max SQL memory and cache flags
	if _, exists := node.Spec.Flags[CacheKey]; !exists {
		additionalFlags = append(additionalFlags, fmt.Sprintf("%s=%s", CacheKey, TwentyFivePercent))
	}

	if _, exists := node.Spec.Flags[MaxSQLMemoryKey]; !exists {
		additionalFlags = append(additionalFlags, fmt.Sprintf("%s=%s", MaxSQLMemoryKey, TwentyFivePercent))
	}

	// Append the flags in sorted order so that golden files don't change.
	sort.Strings(additionalFlags)
	cmd = append(cmd, additionalFlags...)

	// Inject basalt store flags so basalt works on the deprecated flags path
	// too, not just through StartFlags/parseStartFlags.
	if node.Spec.DataStore.IsBasalt() {
		cmd = append(cmd, basaltStoreFlagStrings(node)...)
	}

	// Get all store and encrypted flags. Basalt is excluded because its
	// encryption is configured by --cluster-key-dir (rendered in
	// basaltConfigFlags), and --enterprise-encryption would point at
	// CockroachDataDirMountPath, which on a basalt node is local scratch rather
	// than the basalt store.
	if node.Spec.EncryptionAtRest != nil && !node.Spec.DataStore.IsBasalt() {
		earOptions := &encryptionAtRestOptions{
			KeySecretName:             node.Spec.EncryptionAtRest.KeySecretName,
			CMEKPlatform:              node.Spec.EncryptionAtRest.Platform,
			CMEKCredentialsSecretName: node.Spec.EncryptionAtRest.CMEKCredentialsSecretName,
			OldKeySecretName:          node.Spec.EncryptionAtRest.OldKeySecretName,
			StorePath:                 CockroachDataDirMountPath,
		}
		cmd = append(cmd, addEncryptionAtRestCrdbNodeCmd(earOptions)...)
		// If WAL failover is enabled then we need to add the CMEK key there as well.
		if node.Spec.WALFailoverSpec != nil {
			earOptions.StorePath = walMountPath(node)
			cmd = append(cmd, addEncryptionAtRestCrdbNodeCmd(earOptions)...)
		}

	}
	return strings.Join(cmd, " ")
}

// parseStartFlags builds the command string for starting a CockroachDB node using the new StartFlags field.
// It processes default flags, upsert flags, and omit flags to generate the final command.
func parseStartFlags(node *crdbv1beta1.CrdbNode) string {
	advertiseAddr := nodeFQDN(node)

	// See https://www.cockroachlabs.com/docs/stable/cockroach-start.html

	// About `ulimit -S -n 1048576`:
	// Raise the soft limit of file descriptors to 1048576. On AWS we use Bottlerocket for our instance AMI image,
	// which currently sets the soft limit to 65536. This can cause low amount of available open file descriptors on
	// some clusters. We will want to remove this ulimit command and set the process soft limit flag when a Bottlerocket
	// version that fixes this issue: https://github.com/bottlerocket-os/bottlerocket/issues/1703 is released.
	cmd := []string{
		"ulimit -S -n 1048576; ",
		"exec",
		"/cockroach/cockroach",
		"start",
	}

	defaultFlags := getDefaultFlags(node, advertiseAddr)

	// When basalt storage is configured, inject its config flags (basalt-cluster,
	// basalt-namespace, disable-rsengine) into the default set. Store flags are
	// appended separately after sorting because a node may have multiple stores.
	if node.Spec.DataStore.IsBasalt() {
		maps.Copy(defaultFlags, basaltConfigFlags(node))
	}

	buildLoggingFlag(node, defaultFlags)
	buildWalFailoverFlag(node, defaultFlags)

	// Extract flags from node spec.
	var upsertFlags, omitFlags []string
	if node.Spec.StartFlags != nil {
		upsertFlags = node.Spec.StartFlags.Upsert
		omitFlags = node.Spec.StartFlags.Omit
	}

	parsedUpsertFlags := parseStartFlagUpserts(upsertFlags)

	// Remove omitted flags from default flags.
	for _, flag := range omitFlags {
		delete(defaultFlags, flag)
	}

	var flags []string

	// First, add default flags that have not been overridden via upsert.
	// Boolean flags (like --insecure) store an empty value and must be emitted
	// without "=", cockroach rejects "--insecure=" as an invalid flag value.
	for k, v := range defaultFlags {
		if _, ok := parsedUpsertFlags[k]; !ok {
			if v == "" {
				flags = append(flags, k)
			} else {
				flags = append(flags, fmt.Sprintf("%s=%s", k, v))
			}
		}
	}

	flags = append(flags, buildLocalityFlags(node, parsedUpsertFlags, advertiseAddr)...)

	for flagName, flagValues := range parsedUpsertFlags {
		// For other flags that should only have one value, use the last one.
		if len(flagValues) > 0 {
			value := flagValues[len(flagValues)-1]
			if value == "" {
				flags = append(flags, flagName)
			} else {
				flags = append(flags, fmt.Sprintf("%s=%s", flagName, value))
			}
		}
	}

	// Append the flags in sorted order so that golden files don't change.
	sort.Strings(flags)
	// Basalt store flags go after sorting because there may be multiple --store
	// entries (one per store) and they are already ordered by index. The webhook
	// rejects --store in StartFlags.Upsert and Omit for basalt clusters.
	if node.Spec.DataStore.IsBasalt() {
		flags = append(flags, basaltStoreArgs(node)...)
	}
	// Always have the enterprise-encryption flags at the end.
	flags = append(flags, buildEncryptionAtRestFlags(node)...)

	cmd = append(cmd, flags...)
	return strings.Join(cmd, " ")
}

// parseStartFlagUpserts groups upserted values by flag name in declaration order.
func parseStartFlagUpserts(upsertFlags []string) map[string][]string {
	parsedUpsertFlags := make(map[string][]string)
	for _, flag := range upsertFlags {
		flagName, flagValue, _ := strings.Cut(flag, "=")
		parsedUpsertFlags[flagName] = append(parsedUpsertFlags[flagName], flagValue)
	}
	return parsedUpsertFlags
}

// getDefaultFlags returns a map of default flags that are used when starting a CockroachDB node.
func getDefaultFlags(node *crdbv1beta1.CrdbNode, advertiseAddr string) map[string]string {
	flags := map[string]string{
		CacheKey:               TwentyFivePercent,
		MaxSQLMemoryKey:        TwentyFivePercent,
		"--listen-addr":        fmt.Sprintf("0.0.0.0:%d", node.Spec.GetGRPCPort()),
		"--http-addr":          fmt.Sprintf("0.0.0.0:%d", node.Spec.GetHTTPPort()),
		"--sql-addr":           fmt.Sprintf("0.0.0.0:%d", node.Spec.GetSQLPort()),
		"--advertise-addr":     fmt.Sprintf("%s:%d", advertiseAddr, node.Spec.GetGRPCPort()),
		"--advertise-sql-addr": fmt.Sprintf("%s:%d", advertiseAddr, node.Spec.GetSQLPort()),
		"--join":               node.Spec.Join,
	}

	if node.Spec.TLSEnabled {
		flags["--certs-dir"] = CockroachCertsPath
	} else {
		flags["--insecure"] = ""
	}
	return flags
}

// buildLocalityFlags constructs the locality flags for the CockroachDB node.
// If the user has provided their own locality flags, those are used instead of the default.
func buildLocalityFlags(
	node *crdbv1beta1.CrdbNode, parsedUpsertFlags map[string][]string, advertiseAddr string,
) []string {
	// Handle locality flags.
	userProvidedLocality := false
	if _, ok := parsedUpsertFlags[Locality]; ok {
		userProvidedLocality = true
	}
	if _, ok := parsedUpsertFlags[LocalityFile]; ok {
		userProvidedLocality = true
	}
	var flags []string
	if !userProvidedLocality {
		// TODO(jmcarp): Drop `dns` locality label after confirming it's unused.
		// TODO(jmcarp): Use `--locality-file` rather than `--locality`.
		if len(node.Spec.LocalityLabels) > 0 || len(node.Spec.LocalityMappings) > 0 {
			flags = append(flags, fmt.Sprintf("$(cat /etc/cockroach-env/locality),dns=%s", advertiseAddr))
		} else {
			flags = append(flags, fmt.Sprintf("%s=dns=%s", Locality, advertiseAddr))
		}
	}
	return flags
}

// buildEncryptionAtRestFlags constructs the flags needed for encryption at rest.
// It adds the appropriate flags for both the data store and WAL directory if configured.
func buildEncryptionAtRestFlags(node *crdbv1beta1.CrdbNode) []string {
	var flags []string
	// Basalt is excluded for the same reason as in parseFlags. Its encryption
	// config rides on --cluster-key-dir, and --enterprise-encryption's StorePath
	// would name local scratch rather than the basalt store.
	if node.Spec.EncryptionAtRest != nil && !node.Spec.DataStore.IsBasalt() {
		earOptions := &encryptionAtRestOptions{
			KeySecretName:             node.Spec.EncryptionAtRest.KeySecretName,
			CMEKPlatform:              node.Spec.EncryptionAtRest.Platform,
			CMEKCredentialsSecretName: node.Spec.EncryptionAtRest.CMEKCredentialsSecretName,
			OldKeySecretName:          node.Spec.EncryptionAtRest.OldKeySecretName,
			StorePath:                 CockroachDataDirMountPath,
		}
		flags = append(flags, addEncryptionAtRestCrdbNodeCmd(earOptions)...)

		// If WAL failover is enabled, add WAL path for encryption; then we need to add the CMEK key there as well.
		if node.Spec.WALFailoverSpec != nil {
			earOptions.StorePath = walMountPath(node)
			flags = append(flags, addEncryptionAtRestCrdbNodeCmd(earOptions)...)
		}
	}
	return flags
}

// buildLoggingFlag configures the logging settings for the CockroachDB node.
func buildLoggingFlag(node *crdbv1beta1.CrdbNode, defaultFlags map[string]string) {
	// If the config map name is specified, set logging config to the
	// file mounted in the volume. Otherwise, default to info logs writing
	// to stderr.
	if node.Spec.LoggingConfigMapName != "" {
		// Use mounted logging configuration and replace placeholders like ${HOST_IP} in
		// the configuration with value from the environment.
		defaultFlags["--log-config-file"] = fmt.Sprintf("%s/logs.yaml", CockroachConfigVolumePath)
		// Replaces placeholders like ${HOST_IP} in logging configuration
		// with values from the environment.

		// We default to HOST_IP when no vars are specified, so CC deployments work without needing
		// to set LoggingConfigVars in the control plane.

		// Todo(nishanth): Remove this default, once CC sets LoggingConfigVars explicitly.
		if len(node.Spec.LoggingConfigVars) > 0 {
			defaultFlags["--log-config-vars"] = strings.Join(node.Spec.LoggingConfigVars, ",")
		} else {
			defaultFlags["--log-config-vars"] = hostIp
		}
	} else {
		defaultFlags["--log"] = stderrLogFlag(node)
	}
}

// stderrLogFlag returns the inline --log value for the stderr sink. CRDB v26.3
// implicitly logs STORAGE and DEV at INFO, so listing them at WARNING is
// rejected with "already listed at severity INFO". On v26.3+ we list every
// channel at INFO. Older versions keep STORAGE and DEV at WARNING so their
// stderr stays less verbose. If the image version can't be parsed we fall back
// to the older form to avoid changing existing clusters.
func stderrLogFlag(node *crdbv1beta1.CrdbNode) string {
	if v, err := node.CrdbVersion(); err == nil && v.AtLeast(version.MustParse(manifestslog.MinStorageDevAtInfoVersion)) {
		return `'sinks: {stderr: {channels: {INFO: "HEALTH,OPS,DEV,STORAGE"}}}'`
	}
	return `'sinks: {stderr: {channels: {INFO: "HEALTH,OPS", WARNING: "STORAGE,DEV"}}}'`
}

// buildWalFailoverFlag configures the WAL failover flags based on the node's WAL failover spec.
func buildWalFailoverFlag(node *crdbv1beta1.CrdbNode, defaultFlags map[string]string) {
	if spec := node.Spec.WALFailoverSpec; spec != nil {
		walPath := walMountPath(node)
		if node.Spec.WALFailoverSpec.Status == crdbv1beta1.WalEnable {
			defaultFlags[WalCmdFlag] = fmt.Sprintf("path=%s", walPath)
		} else {
			defaultFlags[WalCmdFlag] = fmt.Sprintf("disabled,prev_path=%s", walPath)
		}
	}
}

// certsOrInsecureFlag returns the appropriate TLS flag for cockroach start:
// --certs-dir when TLS is enabled, or --insecure when it is not.
func certsOrInsecureFlag(tlsEnabled bool) string {
	if tlsEnabled {
		return fmt.Sprintf("--certs-dir %s", CockroachCertsPath)
	}
	return "--insecure"
}

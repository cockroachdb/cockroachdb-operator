package log

import (
	"fmt"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/cockroach/logconfig"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/cockroach/logpb"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/constants"
	"github.com/cockroachlabs/version"
	yaml "gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
)

const (
	loggingConfigMapName            = "cockroachdb-logging"
	loggingTenantConfigMapName      = "cockroachdb-logging-tenant"
	LoggingConfigMapVolumeName      = "cockroachdb-logging"
	LoggingConfigMapVolumeMountName = "cockroachdb-logging"
	Minimum211Version               = "v21.1.0-alpha.00000000"
	Minimum212Version               = "v21.2.0-alpha.00000000"
	// Log buffering was backported to v22.1.3 and isn't available in earlier releases
	// in the v22.1 series.
	Minimum221Version = "v22.1.3"
)

// LoggingConfigMapOpts controls  the configuration of the logging logic within
// CRDB.
type LoggingConfigMapOpts struct {
	IncludeFluentSinkBuffering bool
	IsSQLPod                   bool
	UseBuffering               bool
	// StorageDevAtInfo lists the STORAGE and DEV channels at INFO on the stderr
	// sink instead of WARNING. CRDB v26.3 implicitly logs them at INFO, so
	// listing them at WARNING is rejected. Older versions keep them at WARNING.
	StorageDevAtInfo bool
}

// MinStorageDevAtInfoVersion is the first CRDB version that implicitly logs the
// STORAGE and DEV channels at INFO, which makes listing them at WARNING invalid.
const MinStorageDevAtInfoVersion = "v26.3.0-alpha.0"

// crdbImageStorageDevAtInfo reports whether the CRDB image is v26.3+ and so
// needs STORAGE and DEV listed at INFO rather than WARNING. An unparseable image
// version falls back to the older WARNING behavior.
func crdbImageStorageDevAtInfo(image string) bool {
	parts := strings.Split(image, ":")
	if len(parts) < 2 {
		return false
	}
	v, err := version.Parse(parts[len(parts)-1])
	if err != nil {
		return false
	}
	return v.AtLeast(version.MustParse(MinStorageDevAtInfoVersion))
}

var (
	// TRUE represents the boolean true for use within the package.
	TRUE bool = true
	// FALSE represents the boolean false for use within the package.
	FALSE                   bool               = false
	defaultFileFormat       string             = logconfig.DefaultFileFormat
	defaultStderrFormat     string             = logconfig.DefaultStderrFormat
	ccJsonFluentFormat      string             = "json-fluent"
	oneKiB                                     = logconfig.ByteSize(1024)
	twoHundredFiftySixKiB                      = oneKiB * 256
	oneMiB                  logconfig.ByteSize = logconfig.ByteSize(1048576) // 1 MiB
	tenMiB                  logconfig.ByteSize = oneMiB * 10
	fiftyMiB                logconfig.ByteSize = oneMiB * 50
	oneHundredMiB           logconfig.ByteSize = tenMiB * 10
	oneSecond                                  = time.Second
	fiveSeconds             time.Duration      = time.Second * 5
	loggingConfigurationDir string             = "/cockroach/cockroach-data/logs"
)

func defaultLoggingConfiguration() logconfig.Config {
	return logconfig.Config{
		// https://www.cockroachlabs.com/docs/v21.1/configure-logs#default-logging-configuration
		// The YAML payload below represents the default logging behavior
		// of cockroach start and cockroach start-single-node. To retain
		// backward compatibility with v20.2 and earlier, these settings
		// match the log filenames used in previous versions.
		FileDefaults: logconfig.FileDefaults{
			MaxFileSize:    &tenMiB,        // 10MiB
			MaxGroupSize:   &oneHundredMiB, // 100MiB
			BufferedWrites: &TRUE,
			CommonSinkConfig: logconfig.CommonSinkConfig{
				Filter:      logpb.Severity_INFO,
				Format:      &defaultFileFormat,
				Redact:      &FALSE,
				Redactable:  &TRUE,
				Criticality: &TRUE, // exit-on-error
				Auditable:   &FALSE,
			},
			// By default, if no dir is specified, CRDB will derive a log
			// directory path from the first on-disk store (i.e.
			// /cockroach/cockroach-data). We do this override here because
			// that behavior isn't applicable to tenants, and we do want
			// logs to end up in the same directory for them. See
			// https://github.com/cockroachdb/cockroach/blob/a5e32e1d18aa07a65eb44063177a6c196623f360/pkg/cli/log_flags.go#L222.
			Dir: &loggingConfigurationDir,
		},

		FluentDefaults: logconfig.FluentDefaults{
			CommonSinkConfig: logconfig.CommonSinkConfig{
				Filter:      logpb.Severity_INFO,
				Format:      &ccJsonFluentFormat,
				Redact:      &FALSE,
				Redactable:  &TRUE,
				Criticality: &FALSE, // exit-on-error
				Auditable:   &FALSE,
			},
		},

		// Currently, we log specific channels to disk as that mirrors the
		// current behavior configured by the YAML manifests.
		// TODO(logston): Update these structs to log all channels to a single
		// log file (since ALL EXCEPT is not possible with structs) to ensure
		// new channels are captured in on-disk logs.
		Sinks: logconfig.SinkConfig{
			FileGroups: map[string]*logconfig.FileSinkConfig{
				"default": {
					Channels: logconfig.ChannelFilters{
						Filters: map[logpb.Severity]logconfig.ChannelList{
							logpb.Severity_INFO: {
								Channels: []logpb.Channel{
									logpb.Channel_OPS,
									logpb.Channel_SQL_SCHEMA,
									logpb.Channel_USER_ADMIN,
									logpb.Channel_PRIVILEGES,
								},
							},
						},
					},
				},
				"dev": {
					Channels: logconfig.ChannelFilters{
						Filters: map[logpb.Severity]logconfig.ChannelList{
							logpb.Severity_INFO: {
								Channels: []logpb.Channel{
									logpb.Channel_DEV,
								},
							},
						},
					},
				},
				"health": {
					Channels: logconfig.ChannelFilters{
						Filters: map[logpb.Severity]logconfig.ChannelList{
							logpb.Severity_INFO: {
								Channels: []logpb.Channel{
									logpb.Channel_HEALTH,
								},
							},
						},
					},
				},
				"pebble": {
					Channels: logconfig.ChannelFilters{
						Filters: map[logpb.Severity]logconfig.ChannelList{
							logpb.Severity_INFO: {
								Channels: []logpb.Channel{
									logpb.Channel_STORAGE,
								},
							},
						},
					},
				},
				"sql-audit": {
					Channels: logconfig.ChannelFilters{
						Filters: map[logpb.Severity]logconfig.ChannelList{
							logpb.Severity_INFO: {
								Channels: []logpb.Channel{
									logpb.Channel_SENSITIVE_ACCESS,
								},
							},
						},
					},
				},
				"sql-auth": {
					Channels: logconfig.ChannelFilters{
						Filters: map[logpb.Severity]logconfig.ChannelList{
							logpb.Severity_INFO: {
								Channels: []logpb.Channel{
									logpb.Channel_SESSIONS,
								},
							},
						},
					},
				},
				"sql-exec": {
					Channels: logconfig.ChannelFilters{
						Filters: map[logpb.Severity]logconfig.ChannelList{
							logpb.Severity_INFO: {
								Channels: []logpb.Channel{
									logpb.Channel_SQL_EXEC,
								},
							},
						},
					},
				},
				"sql-slow": {
					Channels: logconfig.ChannelFilters{
						Filters: map[logpb.Severity]logconfig.ChannelList{
							logpb.Severity_INFO: {
								Channels: []logpb.Channel{
									logpb.Channel_SQL_PERF,
								},
							},
						},
					},
				},
				"sql-slow-internal-only": {
					Channels: logconfig.ChannelFilters{
						Filters: map[logpb.Severity]logconfig.ChannelList{
							logpb.Severity_INFO: {
								Channels: []logpb.Channel{
									logpb.Channel_SQL_INTERNAL_PERF,
								},
							},
						},
					},
				},
				"telemetry": {
					Channels: logconfig.ChannelFilters{
						Filters: map[logpb.Severity]logconfig.ChannelList{
							logpb.Severity_INFO: {
								Channels: []logpb.Channel{
									logpb.Channel_TELEMETRY,
								},
							},
						},
					},
				},
			},
			// FluentServers are used to send logs over TCP to a logging sink
			// (no necessarily FluentBit). We do send to FluentBit though, and
			// we send logs to an instance of FluentBit that is on the same
			// node as cockroachdb. We don't have named endpoints for these
			// node local FluentBit pods because k8s did not support such a
			// thing when this code was first rolled out. These FluentBit pods
			// are node ports, so CRDB interpolates its HOST_IP env var into
			// the configuration generated by this struct on startup.
			FluentServers: map[string]*logconfig.FluentSinkConfig{
				"fluentbit-redacted": {
					Channels: logconfig.ChannelFilters{
						Filters: map[logpb.Severity]logconfig.ChannelList{
							logpb.Severity_INFO: {
								Channels: logconfig.AllChannels(),
							},
						},
					},
					Net:     "tcp",
					Address: fmt.Sprintf("${HOST_IP}:%d", constants.FluentbitRedactedPort),
					FluentDefaults: logconfig.FluentDefaults{
						CommonSinkConfig: logconfig.CommonSinkConfig{
							Filter: logpb.Severity_INFO,
							Redact: &TRUE,
						},
					},
				},
				"fluentbit-customer": {
					Channels: logconfig.ChannelFilters{
						Filters: map[logpb.Severity]logconfig.ChannelList{
							logpb.Severity_INFO: {
								Channels: logconfig.AllChannels(),
							},
						},
					},
					Net:     "tcp",
					Address: fmt.Sprintf("${HOST_IP}:%d", constants.FluentbitUnredactedPort),
					FluentDefaults: logconfig.FluentDefaults{
						CommonSinkConfig: logconfig.CommonSinkConfig{
							Filter: logpb.Severity_INFO,
							Redact: &FALSE,
						},
					},
				},
				// Additional fluent sinks for OTel collector log ingestion.
				// During the migration from FluentBit to OTel, both
				// collectors run in parallel on separate ports. These
				// sinks will be removed once the migration is complete.
				"otel-redacted": {
					Channels: logconfig.ChannelFilters{
						Filters: map[logpb.Severity]logconfig.ChannelList{
							logpb.Severity_INFO: {
								Channels: logconfig.AllChannels(),
							},
						},
					},
					Net:     "tcp",
					Address: fmt.Sprintf("${HOST_IP}:%d", constants.OtelRedactedPort),
					FluentDefaults: logconfig.FluentDefaults{
						CommonSinkConfig: logconfig.CommonSinkConfig{
							Filter: logpb.Severity_INFO,
							Redact: &TRUE,
						},
					},
				},
				"otel-customer": {
					Channels: logconfig.ChannelFilters{
						Filters: map[logpb.Severity]logconfig.ChannelList{
							logpb.Severity_INFO: {
								Channels: logconfig.AllChannels(),
							},
						},
					},
					Net:     "tcp",
					Address: fmt.Sprintf("${HOST_IP}:%d", constants.OtelUnredactedPort),
					FluentDefaults: logconfig.FluentDefaults{
						CommonSinkConfig: logconfig.CommonSinkConfig{
							Filter: logpb.Severity_INFO,
							Redact: &FALSE,
						},
					},
				},
			},
			Stderr: logconfig.StderrSinkConfig{
				Channels: logconfig.ChannelFilters{
					Filters: map[logpb.Severity]logconfig.ChannelList{
						logpb.Severity_INFO: {
							Channels: []logpb.Channel{
								logpb.Channel_HEALTH,
								logpb.Channel_OPS,
							},
						},
						logpb.Severity_WARNING: {
							Channels: []logpb.Channel{
								logpb.Channel_DEV,
								logpb.Channel_STORAGE,
							},
						},
					},
				},
				CommonSinkConfig: logconfig.CommonSinkConfig{
					Filter: logpb.Severity_NONE,
					Format: &defaultStderrFormat,
					//   Any logs sent to stdout/stderr will be picked up by standard k8s
					//   tooling. Thus, services like GCP's Cloud Logging will have access to
					//   these streams and could store them for periods that are longer than
					//   allowed by PCI constraints. The default retention period in GCP Cloud
					//   Logging is 30 days. In order to maintain PCI compliance, all logs
					//   send to std* should be redacted. Unredacted logs are still stored
					//   on the CRDB node disk.
					Redact:      &TRUE,
					Criticality: &TRUE, // exit-on-error
				},
			},
		},

		CaptureFd2: logconfig.CaptureFd2Config{
			Enable:       true,
			MaxGroupSize: &oneHundredMiB,
		},
	}
}

func getConfiguredConfigMap(opts LoggingConfigMapOpts) *corev1.ConfigMap {
	c := defaultLoggingConfiguration()

	if opts.StorageDevAtInfo {
		// CRDB v26.3 implicitly logs STORAGE and DEV at INFO, so they cannot be
		// listed at WARNING. List every stderr channel at INFO instead.
		c.Sinks.Stderr.Channels = logconfig.ChannelFilters{
			Filters: map[logpb.Severity]logconfig.ChannelList{
				logpb.Severity_INFO: {
					Channels: []logpb.Channel{
						logpb.Channel_HEALTH,
						logpb.Channel_OPS,
						logpb.Channel_DEV,
						logpb.Channel_STORAGE,
					},
				},
			},
		}
	}

	if opts.IncludeFluentSinkBuffering {
		c.FluentDefaults.Buffering.CommonBufferSinkConfig = logconfig.CommonBufferSinkConfig{
			MaxStaleness:     &fiveSeconds,
			FlushTriggerSize: &oneMiB,
			MaxBufferSize:    &fiftyMiB,
		}
	}

	if opts.IsSQLPod {
		c.FileDefaults.MaxFileSize = &oneMiB
		c.FileDefaults.MaxGroupSize = &oneMiB
		c.CaptureFd2.MaxGroupSize = &oneMiB
		// Add additional sinks to send logs to a fluentbit sidecar.
		// This is to support log export for serverless.
		// If a tenant does not have log export enabled, the fluentbit sidecar will be set up
		// to simply drop these logs.
		c.Sinks.FluentServers["fluentbit-sidecar-redacted"] = &logconfig.FluentSinkConfig{
			Channels: logconfig.ChannelFilters{
				Filters: map[logpb.Severity]logconfig.ChannelList{
					logpb.Severity_INFO: {
						Channels: logconfig.AllChannels(),
					},
				},
			},
			Net:     "tcp",
			Address: fmt.Sprintf("localhost:%d", constants.FluentbitRedactedPort),
			FluentDefaults: logconfig.FluentDefaults{
				CommonSinkConfig: logconfig.CommonSinkConfig{
					Filter: logpb.Severity_INFO,
					Redact: &TRUE,
				},
			},
		}
		c.Sinks.FluentServers["fluentbit-sidecar-customer"] = &logconfig.FluentSinkConfig{
			Channels: logconfig.ChannelFilters{
				Filters: map[logpb.Severity]logconfig.ChannelList{
					logpb.Severity_INFO: {
						Channels: logconfig.AllChannels(),
					},
				},
			},
			Net:     "tcp",
			Address: fmt.Sprintf("localhost:%d", constants.FluentbitUnredactedPort),
			FluentDefaults: logconfig.FluentDefaults{
				CommonSinkConfig: logconfig.CommonSinkConfig{
					Filter: logpb.Severity_INFO,
					Redact: &FALSE,
				},
			},
		}
		// Additional sidecar sinks for OTel collector log ingestion.
		// During the migration from FluentBit to OTel, both collectors
		// run in parallel on separate ports. These sinks will be
		// removed once the migration is complete.
		c.Sinks.FluentServers["otel-sidecar-redacted"] = &logconfig.FluentSinkConfig{
			Channels: logconfig.ChannelFilters{
				Filters: map[logpb.Severity]logconfig.ChannelList{
					logpb.Severity_INFO: {
						Channels: logconfig.AllChannels(),
					},
				},
			},
			Net:     "tcp",
			Address: fmt.Sprintf("localhost:%d", constants.OtelRedactedPort),
			FluentDefaults: logconfig.FluentDefaults{
				CommonSinkConfig: logconfig.CommonSinkConfig{
					Filter: logpb.Severity_INFO,
					Redact: &TRUE,
				},
			},
		}
		c.Sinks.FluentServers["otel-sidecar-customer"] = &logconfig.FluentSinkConfig{
			Channels: logconfig.ChannelFilters{
				Filters: map[logpb.Severity]logconfig.ChannelList{
					logpb.Severity_INFO: {
						Channels: logconfig.AllChannels(),
					},
				},
			},
			Net:     "tcp",
			Address: fmt.Sprintf("localhost:%d", constants.OtelUnredactedPort),
			FluentDefaults: logconfig.FluentDefaults{
				CommonSinkConfig: logconfig.CommonSinkConfig{
					Filter: logpb.Severity_INFO,
					Redact: &FALSE,
				},
			},
		}
	}

	if opts.UseBuffering {
		// BufferedWrites is the old style of buffering that was replaced via
		// https://github.com/cockroachdb/cockroach/pull/120428 which enables the same buffering logic as
		// in the fluentd/networking sinks
		c.FileDefaults.BufferedWrites = &FALSE

		// The defaults here are set based on https://github.com/cockroachdb/cockroach/pull/120428
		c.FileDefaults.Buffering.MaxStaleness = &oneSecond
		c.FileDefaults.Buffering.FlushTriggerSize = &twoHundredFiftySixKiB
		c.FileDefaults.Buffering.MaxBufferSize = &fiftyMiB
	}

	out, err := yaml.Marshal(c)
	if err != nil {
		panic(errors.Wrap(err, "unable to marshal logging configuration"))
	}

	return &corev1.ConfigMap{
		Data: map[string]string{
			"logs.yaml": string(out),
		},
	}
}

// LoggingConfigMap defines the ConfigMap that will be used to configure
// logging for a dedicated crdb pod or a KV pod.
func LoggingConfigMap(configMapOpts LoggingConfigMapOpts) *corev1.ConfigMap {
	return getConfiguredConfigMap(configMapOpts)
}

// TenantLoggingConfigMap defines the ConfigMap that will be used to configure
// logging for a SQL pod.
func TenantLoggingConfigMap(configMapOpts LoggingConfigMapOpts) *corev1.ConfigMap {
	return getConfiguredConfigMap(configMapOpts)
}

func NewTenantLoggingConfigMapOpts() LoggingConfigMapOpts {
	return LoggingConfigMapOpts{
		IncludeFluentSinkBuffering: true,
		IsSQLPod:                   true,
		UseBuffering:               false,
	}
}

func NewLoggingConfigMapOpts(image string) LoggingConfigMapOpts {
	return LoggingConfigMapOpts{
		IncludeFluentSinkBuffering: CrdbImageSupportsLogBuffering(image),
		IsSQLPod:                   false,
		UseBuffering:               false,
		StorageDevAtInfo:           crdbImageStorageDevAtInfo(image),
	}
}

// LoggingConfigMapName returns an appropriately suffixed name for a ConfigMap
// holding the logging configuration of CockroachDB Pod. if version is an empty
// string, an unsuffixed name will be returned.
func LoggingConfigMapName(version string) string {
	// New regions are created without a version because the region does not
	// exist in the control plane database yet.
	if version == "" {
		return loggingConfigMapName
	}

	return fmt.Sprintf("%s-%s", loggingConfigMapName, version)
}

// IsDedicatedConfigMap returns true if the ConfigMap name is of the form used
// for dedicated (or KV) pods.
func IsDedicatedConfigMap(cmName string) bool {
	return strings.HasPrefix(cmName, loggingConfigMapName) && !IsTenantConfigMap(cmName)
}

// LoggingTenantConfigMapName returns an appropriately suffixed name for a
// ConfigMap holding the logging configuration of a SQL Pod. if version is an
// empty string, an unsuffixed name will be returned.
func LoggingTenantConfigMapName(version string) string {
	// New regions are created without a version because the region does not
	// exist in the control plane database yet.
	if version == "" {
		return loggingTenantConfigMapName
	}

	return fmt.Sprintf("%s-%s", loggingTenantConfigMapName, version)
}

// IsTenantConfigMap returns true if the ConfigMap name is of the form used for
// SQL pods.
func IsTenantConfigMap(cmName string) bool {
	return strings.HasPrefix(cmName, loggingTenantConfigMapName)
}

// LoggingConfigMapVolume defines the volume that will be mounted to the
// cockroach container to configure its logging.
func LoggingConfigMapVolume(cmName string) corev1.Volume {
	return corev1.Volume{
		Name: LoggingConfigMapVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: cmName,
				},
			},
		},
	}
}

// CrdbImageSupportsLogBuffering identifies if a particular build of cockroachdb
// can support buffering network logs to disk rather than blocking on failures.
func CrdbImageSupportsLogBuffering(image string) bool {
	parts := strings.Split(image, ":")

	if len(parts) < 2 {
		panic(errors.Errorf("unable to get version from image string %s", image))
	}

	v, err := version.Parse(parts[len(parts)-1])
	if err != nil {
		// Sometimes a version string is not a well formatted version string
		// (eg. 'v20.1.1'). Instead they can be SHAs, etc. Those cases are rare
		// usually in development environments where a developer has chosen
		// "latest-X.Y.Z build" in the AdminUI. This logic provides a safe
		// fallback to legacy behavior (not supporting telemetry) in
		// the event that a version string does not look like a well formatted
		// version string. This does not act as the primary verification of the
		// version string (for the whole cluster creation process), thus not
		// returning an error here is okay.
		return false
	}
	return CrdbVersionSupportsLogBuffering(v)
}

func CrdbVersionSupportsLogBuffering(v version.Version) bool {
	// v22.1.0 is the lowest version that supports the buffering configuration.
	w, err := version.Parse(Minimum221Version)
	if err != nil {
		panic(errors.Wrapf(err, "unable to parse expected version %s", Minimum212Version))
	}

	return v.AtLeast(w)
}

package manifests

import (
	"fmt"

	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/constants"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	TenantNoopConfigMapName = "fluent-bit-tenant-noop"
)

// GetTestTenantNoopConfigMap returns the ConfigMap used to configure the FluentBit
// sidecar container in SQL pods. It is a "no op" configuration that simply
// drops the logs it receives.
//
// Note: this is only used for testing purposes.
func GetTestTenantNoopConfigMap() *apiv1.ConfigMap {
	service := fmt.Sprintf(`[SERVICE]
    # Flush
    # =====
    # Set an interval of seconds before to flush records to a destination
    Flush        1

    # Daemon
    # ======
    # Instruct Fluent Bit to run in foreground or background mode.
    Daemon       Off

    # Metrics
    # =======
    # Run metrics server for Prometheus metrics scraping.
    HTTP_Server  On
    HTTP_Listen  0.0.0.0
    HTTP_PORT    2020

    # Log_Level
    # =========
    # Set the verbosity level of the service, values can be:
    #
    # - error
    # - warning
    # - info
    # - debug
    # - trace
    #
    # By default 'info' is set, that means it includes 'error' and 'warning'.
    Log_Level    info

    # storage.path sets a location in the file system to store streams and
    # chunks of data. If storage.path is not set, Input plugins can only use 
    # in-memory buffering.
    # storage.metrics turns on prometheus metrics for fluentbit.
    # storage.max_chunks_up limits the number of chunks in memory. When 
    # storage.max_chunks_up is reached, all chunks will only be stored on disk,
    # and then brought back up into memory when they are ready to be delivered.
    storage.path            %s
    storage.metrics  	    On
    storage.max_chunks_up   30
    storage.sync full
    storage.checksum On
    storage.delete_irrecoverable_chunks On

@INCLUDE inputs.conf
@INCLUDE outputs.conf`, FluentbitStorageVolumePath)

	inputs := fmt.Sprintf(`[INPUT]
    Name tcp
    Tag cockroach.redacted
    Listen 0.0.0.0
    Port %d
    Format json
    storage.type filesystem
    Alias tcp-cockroach-redacted
[INPUT]
    Name tcp
    Tag cockroach.customer
    Listen 0.0.0.0
    Port %d
    Format json
    storage.type filesystem
    Alias tcp-cockroach-unredacted`, constants.FluentbitRedactedPort, constants.FluentbitUnredactedPort)
	outputs := `[OUTPUT]
	Name null
	Match *`
	return &apiv1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: TenantNoopConfigMapName,
		},
		Data: map[string]string{
			"fluent-bit.conf": service,
			"inputs.conf":     inputs,
			"outputs.conf":    outputs,
		},
	}
}

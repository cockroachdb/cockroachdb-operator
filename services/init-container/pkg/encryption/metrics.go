package encryption

import (
	"fmt"
	"os"

	"github.com/cockroachdb/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/push"
)

const (
	// envPushgatewayNamespace is the namespace that Pushgateway is running in.
	envPushgatewayNamespace = "PUSHGATEWAY_NAMESPACE"

	// pushgatewayHostnameFmtStr is used for k8s DNS.
	pushgatewayHostnameFmtStr = "pushgateway.%s.svc.cluster.local:9091"
)

var kmsInaccessibilityMetric = promauto.NewGauge(prometheus.GaugeOpts{
	Subsystem: "cmek",
	Name:      "kms_init_failure",
	Help:      "Whether an init container was unable to start due to KMS",
})

type gaugeHandler struct {
	registry *prometheus.Registry
	pusher   *push.Pusher
}

func newGaugeHandler(gauge prometheus.Gauge) (*gaugeHandler, error) {
	registry := prometheus.NewRegistry()
	if err := registry.Register(gauge); err != nil {
		return nil, errors.Wrap(err, "registering gauge")
	}
	return &gaugeHandler{
		registry: registry,
		pusher:   push.New(getPushgatewayHostname(), "init-container").Gatherer(registry),
	}, nil
}

func (g *gaugeHandler) pushMetrics() error {
	if err := g.pusher.Push(); err != nil {
		return errors.Wrap(err, "pushing metrics to Pushgateway")
	}
	return nil
}

func getPushgatewayHostname() string {
	return fmt.Sprintf(pushgatewayHostnameFmtStr, os.Getenv(envPushgatewayNamespace))
}

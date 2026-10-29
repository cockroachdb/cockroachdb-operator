package kubemetrics

import (
	"context"
	"net/url"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/tools/metrics"
)

// latencyMetric is an adapter that converts a prometheus histogram into a k8s
// client metric.
type latencyMetric struct {
	histogram prometheus.Histogram
}

var _ metrics.LatencyMetric = &latencyMetric{}

func (l *latencyMetric) Observe(
	ctx context.Context, verb string, u url.URL, latency time.Duration,
) {
	l.histogram.Observe(float64(latency / time.Second))
}

var (
	requestLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "k8s_api_request_latency",
		Help: "Length of time in seconds spent waiting for k8s api server requests",
	})

	throttleLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "k8s_api_throttle_latency",
		Help: "Length of time in seconds spent waiting for client side throttle",
	})
)

// once ensures we only register the metrics with go-client a single time.
var once sync.Once

// Metrics registers prometheus metrics with the kubernetes go-client and
// returns the metrics so they may be added to a prometheus registry.
func Metrics() []prometheus.Collector {
	once.Do(func() {
		// Note: this is a bit of a hack. The go-client library contains a
		// metrics.Register method that we are supposed to use. metrics.Register
		// de-duplicate calls and only registers the metrics provided by the
		// first call. The k8s controller runtime registers one of the go-client
		// metrics in `metrics/client_go_adapter.go`. Since the controller
		// runtime is a dependency, its init functions will always run first,
		// and call metrics.Register before we have a chance to do so.
		metrics.RateLimiterLatency = &latencyMetric{throttleLatency}
		metrics.RequestLatency = &latencyMetric{requestLatency}
	})
	return []prometheus.Collector{
		requestLatency,
		throttleLatency,
	}
}

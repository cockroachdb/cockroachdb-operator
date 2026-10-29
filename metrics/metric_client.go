package metrics

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

const metricEndpointTemplate = "https://%s/_status/load"

// metricClient facilitates the connections to all pods for fetching metrics.
type metricClient struct {
	ctx        context.Context
	cancel     func()
	httpClient *http.Client
	transport  *http.Transport
}

// metricRequest instance is created for each pod to track the pod IP address.
// The prometheus metric parser is not allowed to be called concurrently so each
// client keeps a separate instance.
type metricRequest struct {
	metricClient *metricClient
	req          *http.Request
	parser       expfmt.TextParser
}

// NewMetricClient creates a new metric client.
func NewMetricClient(ctx context.Context) *metricClient {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	metricTransport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          1000,
		MaxIdleConnsPerHost:   1,
		IdleConnTimeout:       90 * time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	ctx, cancel := context.WithCancel(ctx)
	result := &metricClient{
		ctx:       ctx,
		cancel:    cancel,
		transport: metricTransport,
		httpClient: &http.Client{
			// Limit the wait to retrieve metrics as it is possible for hang and block
			// the client for a long period of time. A pod load is assumed to be zero if
			// the pod is not accessible.
			// The wait also shouldn't be extremely low as it is possible,
			// under load, for the request to take longer.
			Timeout:   500 * time.Millisecond,
			Transport: metricTransport,
		},
	}
	go func() {
		<-ctx.Done()
		result.transport.DisableKeepAlives = true
		result.transport.CloseIdleConnections()
	}()
	return result
}

// NewMetricRequest constructs a new request to a specific endpoint.
func (mc *metricClient) NewMetricRequest(addr string) (*metricRequest, error) {
	url := fmt.Sprintf(metricEndpointTemplate, addr)
	req, err := http.NewRequestWithContext(mc.ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return &metricRequest{
		metricClient: mc,
		req:          req,
	}, nil
}

// currentMetrics is the full set of metrics obtainable by directly connecting
// to the tenant pods.
type currentMetrics struct {
	cpuElapsed    float64
	timeElapsed   float64
	runningJobs   float64
	sqlQueryCount float64
	sqlConns      float64
	sqlNewConns   float64
}

// Get does a pull from the current metric endpoint, parses and metrics and
// returns the result or error.
func (mr *metricRequest) Get() (m currentMetrics, err error) {
	result := currentMetrics{}
	resp, err := mr.metricClient.httpClient.Do(mr.req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	metrics, err := mr.parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return result, err
	}
	cpuUserElapsed, err := extractGaugeMetric(metrics, "sys_cpu_user_ns")
	if err != nil {
		return result, err
	}
	cpuSysElapsed, err := extractGaugeMetric(metrics, "sys_cpu_sys_ns")
	if err != nil {
		return result, err
	}
	timeElapsed, err := extractGaugeMetric(metrics, "sys_cpu_now_ns")
	if err != nil {
		return result, err
	}
	sqlQueryCount, err := extractCounterMetric(metrics, "sql_query_count")
	if err != nil {
		return result, err
	}
	sqlConns, err := extractGaugeMetric(metrics, "sql_conns")
	if err != nil {
		return result, err
	}
	// TODO(jaylim-crl): The DB metric change will backported to 23.2 and 24.1.
	// We will default the metric to 0 if it's not found yet. Once all clusters
	// have been upgraded to a version that supports it, we can remove the
	// custom check.
	sqlNewConns, err := extractCounterMetric(metrics, "sql_new_conns")
	if err != nil && !strings.Contains(err.Error(), "not found") {
		return result, err
	}
	jobs, err := extractGaugeMetric(metrics, "jobs_running_non_idle")
	if err != nil {
		return result, err
	}

	result.cpuElapsed = cpuUserElapsed + cpuSysElapsed
	result.timeElapsed = timeElapsed
	result.sqlQueryCount = sqlQueryCount
	result.sqlConns = sqlConns
	result.sqlNewConns = sqlNewConns
	result.runningJobs = jobs
	return result, nil
}

// extractGaugeMetric looks for a specific gauge metric in the set of metrics
// returned by prometheus metric parser, casts it as float and return the value
// or error.
func extractGaugeMetric(
	metrics map[string]*io_prometheus_client.MetricFamily, name string,
) (float64, error) {
	family, found := metrics[name]
	if !found {
		return 0, fmt.Errorf("metric %s not found, all metrics %+v", name, metrics)
	}
	value := family.GetMetric()
	if len(value) != 1 {
		return 0, fmt.Errorf("expected one metric %s but got %d", name, len(value))
	}
	if family.GetType() != io_prometheus_client.MetricType_GAUGE {
		return 0, fmt.Errorf("expected GAUGE type metric %s but got %s", name, family.GetType())
	}
	return value[0].GetGauge().GetValue(), nil
}

// extractCounterMetric looks for a specific counter metric in the set of metrics
// returned by prometheus metric parser, casts it as float and return the value
// or error.
func extractCounterMetric(
	metrics map[string]*io_prometheus_client.MetricFamily, name string,
) (float64, error) {
	family, found := metrics[name]
	if !found {
		return 0, fmt.Errorf("metric %s not found, all metrics %+v", name, metrics)
	}
	value := family.GetMetric()
	if len(value) != 1 {
		return 0, fmt.Errorf("expected one metric %s but got %d", name, len(value))
	}
	if family.GetType() != io_prometheus_client.MetricType_COUNTER {
		return 0, fmt.Errorf("expected COUNTER type metric %s but got %s", name, family.GetType())
	}
	return value[0].GetCounter().GetValue(), nil
}

package metrics

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/require"
)

type MockCurrentMetricSource struct {
	CPUUserNS          atomic.Int64
	CPUNowNS           atomic.Int64
	CPUSysNS           atomic.Int64
	JobsRunningNonIdle atomic.Int64
	SqlQueryCount      atomic.Int64
	SqlConns           atomic.Int64
	SqlNewConns        atomic.Int64

	metrics []prometheus.Metric

	ReportLoadPct atomic.Int64

	httpPort int
	listener net.Listener
	srv      *http.Server
}

// NewMockMetricSource always creates a metrics source with the sql_new_conns
// metric. Once we enforce the existence of that metric, we can remove the
// extended version, and fold this back.
func NewMockMetricSource(t *testing.T) (*MockCurrentMetricSource, string) {
	return NewMockMetricSourceExtended(t, true /* withNewConns */)
}

func NewMockMetricSourceExtended(
	t *testing.T, withNewConns bool,
) (*MockCurrentMetricSource, string) {
	// Grab a free port for the server
	var err error
	s := &MockCurrentMetricSource{}
	s.metrics = append(s.metrics,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "sys_cpu_user_ns", Help: "Total user cpu time"}, func() float64 {
			return float64(s.CPUUserNS.Load())
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "sys_cpu_now_ns",
			Help: "Number of nanoseconds elapsed since January 1, 1970 UTC"}, func() float64 {
			return float64(s.CPUNowNS.Load())
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "jobs_running_non_idle",
			Help: "Number of running jobs that are not idle"}, func() float64 {
			return float64(s.JobsRunningNonIdle.Load())
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "sys_cpu_sys_ns",
			Help: "Total system cpu time"}, func() float64 {
			return float64(s.CPUSysNS.Load())
		}),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "sql_query_count",
			Help: "Number of SQL queries executed"}, func() float64 {
			return float64(s.SqlQueryCount.Load())
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "sql_conns",
			Help: "Number of open SQL connections"}, func() float64 {
			return float64(s.SqlConns.Load())
		}),
	)
	if withNewConns {
		s.metrics = append(s.metrics,
			prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "sql_new_conns",
				Help: "Number of SQL connections created"}, func() float64 {
				return float64(s.SqlNewConns.Load())
			}),
		)
	}

	s.listener, err = net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	s.httpPort = s.listener.Addr().(*net.TCPAddr).Port

	s.CPUNowNS.Store(time.Now().UnixNano())

	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(s)

	mux := http.NewServeMux()
	mux.Handle("/", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	// self-signed cert for the server
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IsCA:         true,
	}
	cer, err := x509.CreateCertificate(rand.Reader, &template, &template, pub, priv)
	require.NoError(t, err)
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{cer},
			PrivateKey:  priv,
		}}}
	s.srv = &http.Server{TLSConfig: tlsConf, Handler: mux}
	go func() {
		_ = s.srv.ServeTLS(s.listener, "", "")
	}()
	return s, fmt.Sprintf(":%d", s.httpPort)
}

func (s *MockCurrentMetricSource) Describe(ch chan<- *prometheus.Desc) {
	for _, m := range s.metrics {
		ch <- m.Desc()
	}
}

func (s *MockCurrentMetricSource) Collect(ch chan<- prometheus.Metric) {
	// Set values to show the given load
	timeNow := time.Now().UnixNano()
	timeSinceLastFetch := timeNow - s.CPUNowNS.Load()
	cpuSinceLastFetch := s.ReportLoadPct.Load() * timeSinceLastFetch / 100
	s.CPUUserNS.Add(cpuSinceLastFetch)
	s.CPUNowNS.Add(timeSinceLastFetch)

	for _, m := range s.metrics {
		ch <- m
	}
}

// Reset clears all state and simulates a restart of the process being monitored.
func (s *MockCurrentMetricSource) Reset() {
	s.CPUUserNS.Store(0)
	s.CPUSysNS.Store(0)
	s.CPUNowNS.Store(time.Now().UnixNano())
	s.JobsRunningNonIdle.Store(0)
	s.SqlQueryCount.Store(0)
	s.SqlConns.Store(0)
	s.SqlNewConns.Store(0)
}

func (s *MockCurrentMetricSource) Shutdown() error {
	if s.srv != nil {
		// The context that is passed here is for canceling the shutdown while
		// it is in progress. No need to do that for the mock source.
		return s.srv.Shutdown(context.Background())
	}
	return nil
}

func TestMetricClient(t *testing.T) {
	testFn := func(t *testing.T, pod *MockCurrentMetricSource, podAddr string) {
		defer func() { require.NoError(t, pod.Shutdown()) }()
		pod.JobsRunningNonIdle.Store(28)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mc := NewMetricClient(ctx)

		mr, err := mc.NewMetricRequest(podAddr)
		require.NoError(t, err)

		cm1, err := mr.Get()
		require.NoError(t, err)
		require.EqualValues(t, 0, cm1.cpuElapsed)
		require.Less(t, 0., cm1.timeElapsed)
		require.EqualValues(t, 28, cm1.runningJobs)

		pod.ReportLoadPct.Store(40)
		time.Sleep(100 * time.Millisecond)
		cm2, err := mr.Get()
		require.NoError(t, err)
		require.InEpsilon(t,
			float64(cm2.timeElapsed-cm1.timeElapsed)*0.4, float64(cm2.cpuElapsed),
			1e-3,
		)

		pod.ReportLoadPct.Store(250)
		time.Sleep(100 * time.Millisecond)
		cm3, err := mr.Get()
		require.NoError(t, err)
		require.InEpsilon(t,
			float64(cm3.timeElapsed-cm2.timeElapsed)*2.5, float64(cm3.cpuElapsed-cm2.cpuElapsed),
			1e-3,
		)
	}

	// This test is for backward compatibility. Existing SQL pods won't have the
	// sql_new_conns metric until the a new CRDB version including that metric
	// gets rolled out.
	t.Run("missing sql_new_conns should not error", func(t *testing.T) {
		pod, podAddr := NewMockMetricSourceExtended(t, false /* withNewConns */)
		testFn(t, pod, podAddr)
	})

	t.Run("with sql_new_conns", func(t *testing.T) {
		pod, podAddr := NewMockMetricSourceExtended(t, true /* withNewConns */)
		testFn(t, pod, podAddr)
	})
}

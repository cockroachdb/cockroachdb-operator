package metrics

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"math"
	"net"
	"strconv"
	"sync"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// The port on which each pod will serve metrics
const metricPort = "8080"

// The interval between subsequent current metric pulls.
var metricPollInterval = 3 * time.Second

// The maximum/avg load being reported/tracked will be over a 5 minutes sliding
// window.
var (
	movingMaxSlidingWindow = 5 * time.Minute
	movingAvgSlidingWindow = 5 * time.Minute
)

// The presence of connection attempts or queries that are considered over the
// last 2 minutes.
var deltaNewConnOrQueryWindow = 2 * time.Minute

// TenantKey identifies a tenant in a given namespace.
type TenantKey struct {
	Namespace string
	TenantID  crdbv1beta1.TenantID
}

// ObjectKey constructs an ObjectKey from the TenantKey to use when fetching
// from tenant cache.
func (tk TenantKey) ObjectKey() client.ObjectKey {
	return client.ObjectKey{
		Namespace: tk.Namespace,
		Name:      fmt.Sprintf("tenant-%d", tk.TenantID),
	}
}

// TenantMetricsMap defines a map from fully qualified tenant name to the set of
// metrics associated with that tenant.
type TenantMetricsMap map[TenantKey]*TenantMetrics

// PodMetricsMap defines a map from pod name to the set of metrics associated
// with that pod.
type PodMetricsMap map[string]PodMetrics

// TenantManagerOption defines the type of NewTenantManger options
type TenantManagerOption func(manager *TenantManager)

// WithUpdateFunc sets a callback to be invoked each time a new set of tenant
// metrics gets scraped and aggregated.
func WithUpdateFunc(f func(ctx context.Context, metrics *TenantMetrics)) TenantManagerOption {
	return func(manager *TenantManager) {
		manager.updateFunc = f
	}
}

// TenantMetricsReader is used by various clients that only need read only
// access to current metrics.
type TenantMetricsReader interface {
	Get(key TenantKey) *TenantMetrics
}

// TenantMetricsManager implementors should be able to responds to pods
// starting/shutting down and be able to report metrics for individual tenant
// or for all tenants.
type TenantMetricsManager interface {
	TenantMetricsReader
	AddPod(id string, key TenantKey, ip string)
	DeletePod(id string)
}

var _ TenantMetricsManager = &TenantManager{}

// TenantManager manages metrics for all tenants that have SQL pods
// running in the region. Its metrics are used to scale the number of pods per
// tenant up and down, and to decide when it's safe to terminate pods with zero
// connections.
// For each new active tenant, it starts a go routine, responsible for
// monitoring the total load of the tenant. The go routine for the given tenant
// connects to the current metric endpoints of each tenant pod and obtains the
// metrics needed to compute the pod load. This happens through polling
// every three seconds.
// The data retrieved from the pods is aggregated and used to compute
// the total tenant cpu load.
// NOTE: All public methods on the TenantManager are thread-safe.
type TenantManager struct {
	ctx    context.Context
	cancel func()
	ctl    *kube.Ctl
	// updateFunc, if set, will be called on each pull of the tenant's metrics
	updateFunc   func(ctx context.Context, metrics *TenantMetrics)
	metricClient *metricClient
	mu           struct {
		sync.RWMutex
		// tenants contains the map of all current load monitors for all tenants
		// that have at least one running pod, indexed by tenant id/namespace.
		tenants        map[TenantKey]*tenantLoad
		tenantsByPodID map[string]TenantKey
	}
}

// tenantLoad contains information for the number of pods and aggregated CPU
// usage of a single tenant.
type tenantLoad struct {
	manager   *TenantManager
	tenantKey TenantKey
	ctx       context.Context
	cancel    context.CancelFunc
	movingMax *movingMax
	movingAvg *movingAvg
	// Keeping the time of the last query is needed to determine
	// if there were new queries in the last 2m (DeltaQueries2m metric).
	lastQueryTime time.Time
	// Keeping the time of the last connection attempt is needed to determine
	// if there were new connections in the last 2m (DeltaNewConns2m metric).
	lastNewConnsTime time.Time
	startTime        time.Time
	runningJobs      int

	// metrics scoped to the tenant,ns
	cpuMax prometheus.Gauge
	cpuAvg prometheus.Gauge
	jobs   prometheus.Gauge
	conns  prometheus.Gauge
	pods   prometheus.Gauge

	mu struct {
		sync.Mutex
		podLoadByID map[string]*podLoad
		metrics     TenantMetrics
	}
}

// podLoad instance is created for each pod. It keeps a client to the pod's
// current metric endpoint (to reuse connections) as well as the last reported
// wall time and cpu usage time (needed to compute the delta).
type podLoad struct {
	addr             string
	request          *metricRequest
	cpuAvg           *movingAvg
	prevCPUElapsed   float64
	prevTimeElapsed  float64
	prevQueryCount   float64
	prevNewConns     float64
	lastQueryTime    time.Time
	lastNewConnsTime time.Time
}

// NewTenantManager constructs a new tenant manager instance.
func NewTenantManager(ctx context.Context, opts ...TenantManagerOption) *TenantManager {
	ctx, cancel := context.WithCancel(ctx)
	tm := &TenantManager{
		ctx:          ctx,
		cancel:       cancel,
		updateFunc:   func(ctx context.Context, metrics *TenantMetrics) {},
		metricClient: NewMetricClient(ctx),
	}
	tm.mu.tenants = make(map[TenantKey]*tenantLoad)
	tm.mu.tenantsByPodID = make(map[string]TenantKey)
	// Process options
	for _, opt := range opts {
		opt(tm)
	}
	return tm
}

// Get will return the cached metric set for a specific tenant. If there is no
// cached metric set for the tenant, it will return nil.
func (m *TenantManager) Get(key TenantKey) *TenantMetrics {
	var tl *tenantLoad
	var found bool
	func() {
		m.mu.RLock()
		defer m.mu.RUnlock()
		tl, found = m.mu.tenants[key]
	}()
	if !found {
		return nil
	}
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tm := tl.mu.metrics
	return &tm
}

// AddPod is called when a new pod appears. The tenant pods are generally
// starting as pool pods, not associated with a specific tenant and
// can be ignored.
func (m *TenantManager) AddPod(id string, key TenantKey, ip string) {
	m.addPodToMonitor(id, key, net.JoinHostPort(ip, metricPort))
}

// DeletePod is called when a pod is deleted. The pod monitor is removed.
func (m *TenantManager) DeletePod(id string) {
	m.deletePodFromMonitor(id)
}

// addPodMonitor will add a new IP for the given tenant and start a monitor if
// there isn't one running.
func (m *TenantManager) addPodToMonitor(id string, key TenantKey, podAddr string) {
	logging.Debugf(m.ctx, "add monitor namespace %s tenant_id %d pod %s addr %s ",
		key.Namespace, key.TenantID, id, podAddr)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mu.tenantsByPodID[id] = key
	tl, existingTenant := m.mu.tenants[key]
	if existingTenant {
		tl.mu.Lock()
		defer tl.mu.Unlock()
		if _, existingPod := tl.mu.podLoadByID[id]; !existingPod {
			tl.addPodLoad(id, podAddr)
		}
	} else {
		// create new tenant load
		tl = &tenantLoad{
			manager:   m,
			tenantKey: key,
			startTime: time.Now(),
		}
		m.mu.tenants[key] = tl
		tl.mu.metrics = TenantMetrics{Key: key}
		tl.movingMax = newMovingMax(movingMaxSlidingWindow)
		tl.movingAvg = newMovingAvg(int(movingAvgSlidingWindow / metricPollInterval))
		tl.ctx, tl.cancel = context.WithCancel(m.ctx)
		tl.mu.podLoadByID = make(map[string]*podLoad)
		tl.addPodLoad(id, podAddr)
		tl.registerMetrics()
		go tl.monitor(tl.ctx)
	}
}

// registerMetrics is called as soon as the first pod for a given tenant starts.
func (tl *tenantLoad) registerMetrics() {
	logging.Debugf(tl.ctx, "register metrics for tl %v", tl)
	labels := prometheus.Labels{
		"namespace": tl.tenantKey.Namespace,
		"tenant_id": tl.tenantKey.TenantID.String(),
	}

	tl.cpuMax = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem:   "autoscaler",
		Name:        "cpu_max",
		Help:        "Maximum CPU over rolling interval",
		ConstLabels: labels,
	})
	metrics.Registry.Register(tl.cpuMax)

	tl.cpuAvg = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem:   "autoscaler",
		Name:        "cpu_avg",
		Help:        "Maximum CPU over rolling interval",
		ConstLabels: labels,
	})
	metrics.Registry.Register(tl.cpuAvg)

	tl.jobs = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem:   "autoscaler",
		Name:        "jobs",
		Help:        "Number of currently running jobs",
		ConstLabels: labels,
	})
	metrics.Registry.Register(tl.jobs)

	tl.conns = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem:   "autoscaler",
		Name:        "conns",
		Help:        "Number of current connections",
		ConstLabels: labels,
	})
	metrics.Registry.Register(tl.conns)

	tl.pods = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem:   "autoscaler",
		Name:        "pods",
		Help:        "Number of current pods",
		ConstLabels: labels,
	})
	metrics.Registry.Register(tl.pods)
}

// unRegisterMetrics is called when the last pod for a tenant shuts down.
func (tl *tenantLoad) unRegisterMetrics() {
	logging.Debugf(tl.ctx, "unregister metrics for tl %v", tl)
	metrics.Registry.Unregister(tl.cpuMax)
	metrics.Registry.Unregister(tl.cpuAvg)
	metrics.Registry.Unregister(tl.jobs)
	metrics.Registry.Unregister(tl.conns)
	metrics.Registry.Unregister(tl.pods)
}

// updateMetrics will update the metrics that are exported by the operator.
func (tl *tenantLoad) updateMetrics(tm *TenantMetrics) {
	tl.cpuMax.Set(tm.MaxCpu5m)
	tl.cpuAvg.Set(tm.AvgCpu5m)
	tl.jobs.Set(tm.JobsRunning)
	tl.conns.Set(tm.ConnsOpen)
	tl.pods.Set(float64(len(tm.Pods)))
}

// addPodLoad will add a new pod load with the associated metric client.
// If the ctx is canceled, it won't do anything. That should be OK as the
// context cancellation occurs only when the metric manager is shutting down.
func (tl *tenantLoad) addPodLoad(id string, addr string) {
	metricRequest, err := tl.manager.metricClient.NewMetricRequest(addr)
	if err != nil {
		logging.Error(
			tl.ctx,
			fmt.Sprintf("cannot create new metric request for namespace %s tenant_id %d addr %s ",
				tl.tenantKey.Namespace, tl.tenantKey.TenantID, addr),
			zap.Error(err),
		)
		return
	}
	tl.mu.podLoadByID[id] = &podLoad{
		addr:    addr,
		request: metricRequest,
		cpuAvg:  newMovingAvg(int(movingAvgSlidingWindow / metricPollInterval)),
	}
}

// deletePodFromMonitor removes an existing pod from monitor for the given tenant.
func (m *TenantManager) deletePodFromMonitor(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := m.mu.tenantsByPodID[id]
	delete(m.mu.tenantsByPodID, id)
	tl, existingTenant := m.mu.tenants[key]
	if !existingTenant {
		return
	}
	tl.mu.Lock()
	defer tl.mu.Unlock()
	delete(tl.mu.podLoadByID, id)
}

// tryDeleteTenantLoad will try to remove an existing tenant load monitor from the tracker. It only
// succeeds if there are no current pods for the tenant. Will return true if succeeded or false
// otherwise.
func (m *TenantManager) tryDeleteTenantLoad(ctx context.Context, key TenantKey) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	tl, existingTenant := m.mu.tenants[key]
	if !existingTenant {
		return true
	}
	tl.mu.Lock()
	defer tl.mu.Unlock()
	if len(tl.mu.podLoadByID) == 0 {
		tl.unRegisterMetrics()
		delete(m.mu.tenants, key)
		return true
	}
	return false
}

type currentPolledState struct {
	cpu               float64
	runningJobs       float64
	sqlConnections    float64
	lastQueryTime     time.Time
	lastNewConnsTime  time.Time
	pods              PodMetricsMap
	metricsCalculated bool
}

func (tl *tenantLoad) makeCurrentMetrics(
	ctx context.Context, state currentPolledState,
) *TenantMetrics {
	tl.movingMax.add(state.cpu)
	// This truncates the CPU usage to 1% of one core which is
	// accurate enough for making scaling decision.
	tl.movingAvg.add(state.cpu)

	var tm TenantMetrics
	func() {
		tl.mu.Lock()
		defer tl.mu.Unlock()
		tl.mu.metrics.AvgCpu5m = tl.movingAvg.avg()
		tl.mu.metrics.MaxCpu5m = tl.movingMax.max()
		tl.mu.metrics.FillRatio = tl.movingAvg.fillRatio()
		tl.mu.metrics.DeltaQueries2m = time.Since(state.lastQueryTime) < deltaNewConnOrQueryWindow
		tl.mu.metrics.DeltaNewConns2m = time.Since(state.lastNewConnsTime) < deltaNewConnOrQueryWindow
		tl.mu.metrics.JobsRunning = state.runningJobs
		tl.mu.metrics.ConnsOpen = state.sqlConnections
		tl.mu.metrics.Uptime = time.Since(tl.startTime).Seconds()
		tl.mu.metrics.Pods = state.pods
		tm = tl.mu.metrics
	}()

	logging.Debugf(ctx, "### tenant_id %d MaxCpu5m %.0f%% AvgCpu5m %.0f%% -> %d",
		tl.tenantKey.TenantID,
		tm.MaxCpu5m*100,
		tm.AvgCpu5m*100,
		len(state.pods))

	// Update the exported autoscaler metrics
	tl.updateMetrics(&tm)
	return &tm
}

// monitor will start infinite loop, and every 3 seconds, will connect to every
// pod assigned to the given tenant to collect the current metrics. Canceling
// the context will also terminate the loop.
func (tl *tenantLoad) monitor(ctx context.Context) {
	// Set the initial poll interval low and revert back once the first pod
	// metrics are scraped successfully.
	pollInterval := 200 * time.Millisecond
	for {
		// Clone the id -> client map to isolate from changes while pulling metrics.
		var m map[string]*podLoad
		func() {
			tl.mu.Lock()
			defer tl.mu.Unlock()
			m = maps.Clone(tl.mu.podLoadByID)
		}()

		if len(m) == 0 {
			// No more active pods - shutdown the load tracker for the tenant.
			if tl.manager.tryDeleteTenantLoad(ctx, tl.tenantKey) {
				// exit the monitor
				break
			}
			// process the tenant load again - some pods got added
			continue
		}

		state := currentPolledState{pods: PodMetricsMap{}}
		for id, pod := range m {
			// Pull the metrics.
			cm, err := pod.request.Get()
			if err == nil {
				pollInterval = metricPollInterval
				podMetrics := PodMetrics{
					Addr:     pod.addr,
					Up:       true,
					SqlConns: cm.sqlConns,
				}
				if pod.prevTimeElapsed != 0 &&
					// This is needed in case the sql process restarts and the
					// elapsed cpu time starts from zero.
					cm.cpuElapsed >= pod.prevCPUElapsed {
					state.metricsCalculated = true
					// Add to the cpu usage.
					podMetrics.SqlCpuIRate = (cm.cpuElapsed - pod.prevCPUElapsed) /
						(cm.timeElapsed - pod.prevTimeElapsed)
					pod.cpuAvg.add(podMetrics.SqlCpuIRate)
					podMetrics.SqlCpuAvg5m = pod.cpuAvg.avg()
					state.cpu += podMetrics.SqlCpuIRate
				}
				pod.prevCPUElapsed = cm.cpuElapsed
				pod.prevTimeElapsed = cm.timeElapsed
				state.runningJobs += cm.runningJobs
				state.sqlConnections += cm.sqlConns

				if pod.prevQueryCount < cm.sqlQueryCount {
					pod.lastQueryTime = time.Now()
				}
				if state.lastQueryTime.Before(pod.lastQueryTime) {
					state.lastQueryTime = pod.lastQueryTime
				}
				pod.prevQueryCount = cm.sqlQueryCount

				if pod.prevNewConns < cm.sqlNewConns {
					pod.lastNewConnsTime = time.Now()
				}
				if state.lastNewConnsTime.Before(pod.lastNewConnsTime) {
					state.lastNewConnsTime = pod.lastNewConnsTime
				}
				pod.prevNewConns = cm.sqlNewConns

				state.pods[id] = podMetrics
			} else {
				logging.Debugf(ctx, "### tenant_id %d metric get err %s", tl.tenantKey.TenantID, err)
				podMetrics := state.pods[id]
				podMetrics.Up = false
				state.pods[id] = podMetrics
			}
		}
		if state.metricsCalculated {
			tm := tl.makeCurrentMetrics(ctx, state)
			tl.manager.updateFunc(ctx, tm)
		}
		select {
		case <-ctx.Done():
			logging.Debugf(ctx,
				"loop namespace %s tenant_id %d select done",
				tl.tenantKey.Namespace, tl.tenantKey.TenantID)
			return
		case <-time.After(pollInterval):
			break
		}
	}
}

// TenantMetrics records the metrics used for tenant scaling.
type TenantMetrics struct {
	// Key is the combination of namespace and tenant id uniquely identifying
	// the tenant for which this metrics apply.
	Key TenantKey

	// AvgCpu5m is the average amount of CPU used across all tenant pods during
	// the previous 5 minute period.
	AvgCpu5m float64

	// MaxCpu5m is the peak CPU usage at any sampling point during the previous
	// 5 minute period.
	MaxCpu5m float64

	// DeltaQueries2m when true indicates that there were queries that ran in
	// the last 2m.
	DeltaQueries2m bool

	// JobsRunning is the number of jobs currently running for this tenant, as
	// of the time of the last sample.
	JobsRunning float64

	// ConnsOpen is the total number of connections currently open across all
	// tenant pods.
	ConnsOpen float64

	// DeltaNewConns2m when true indicates that there were connection attempts
	// in the last 2m.
	DeltaNewConns2m bool

	// Uptime of the longest-running SQL process on any of a tenant's pods, in
	// seconds.
	// NOTE: This can be different from the amount of time that at least one pod
	// has been continuously running for the tenant.
	// It currently is computed locally by tracking the start time of each pod.
	// Once uptime is added to the metrics available on the /_status/load
	// endpoint and all clusters are updated to 23.1, it will be possible to
	// use the pod's uptime to get a better approximation (especially in case
	// of operator restart).
	Uptime float64

	// FillRatio is the fraction of the sliding window that has been filled
	// with samples, from 0.0 (empty) to 1.0 (full).
	FillRatio float64

	// Pods contains metrics for the pods owned by the tenant, indexed by pod
	// name.
	Pods PodMetricsMap
}

// ID constructs an ID based on the tenant id and the tenant's namespace. This
// is used as an unique key
func (m TenantMetrics) ID() string {
	return fmt.Sprintf("%s/%s", m.Key.Namespace, m.Key.TenantID)
}

// String creates a string containing the metrics information, suitable for
// printing.
func (m TenantMetrics) String() string {
	var buf bytes.Buffer
	if len(m.Pods) != 0 {
		buf.WriteString(", pods:")
		for _, pod := range m.Pods {
			fmt.Fprintf(&buf, " {%v}", pod.String())
		}
	}
	return fmt.Sprintf("avg: %s, max: %s, queries2m: %t, jobs: %s, conns: %s, newConns2m: %t, uptime: %v, fillRatio: %s%s",
		formatFloat(m.AvgCpu5m), formatFloat(m.MaxCpu5m), m.DeltaQueries2m,
		formatFloat(m.JobsRunning), formatFloat(m.ConnsOpen), m.DeltaNewConns2m, m.Uptime, formatFloat(m.FillRatio), buf.String())
}

// PodMetrics records the metrics collected for an individual pod.
type PodMetrics struct {
	// Addr contains the IP:port metric endpoint of the pod
	Addr string

	// Up is true if the pod is running or false if not, as of the time of the
	// last sample.
	Up bool

	// SqlConns is the number of active CPU connections to this pod, as of the
	// time of the last sample.
	SqlConns float64

	// SqlCpuIRate is the current rate of CPU usage on this pod, as of the time
	// of the last sample.
	SqlCpuIRate float64

	// SqlCpuAvg5m is the average CPU usage on this pod over the previous
	// 5 minute window.
	SqlCpuAvg5m float64
}

func (m PodMetrics) String() string {
	return fmt.Sprintf("addr: %s, conns: %s, cpu: %s",
		m.Addr, formatFloat(m.SqlConns), formatFloat(m.SqlCpuIRate))
}

// formatFloat uses %f formatting, but rounds to at most 4 fraction digits and
// strips trailing fractional zeros.
func formatFloat(f float64) string {
	return strconv.FormatFloat(math.Round(f*10000)/10000, 'f', -1, 64)
}

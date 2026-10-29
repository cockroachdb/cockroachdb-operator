package autoscaler

import (
	"math"

	"github.com/cockroachlabs/cockroach-enterprise-operator/metrics"
)

const (
	// desiredPodsUpdateThreshold specifies how much a tenant's desired pod
	// count must change from the current capacity before the tenant's pods
	// will be changed. Having a 10% "deadzone" reduces "ping-ponging" between
	// two similar pod counts.
	desiredPodsUpdateThreshold = 0.1

	// minUptime is the amount of time in seconds, from the time a tenant gets
	// its first pod started to the point at which a pod would be considered for
	// termination if there are no sql connection, no queries over the last 2m
	// and no jobs running.
	minUptime = 360
)

func calculateClampedPods(currentPods int32, maxPods *int32, m *metrics.TenantMetrics) int32 {
	ideal := calculateIdealPods(m)

	// Snap desired pods to its previous version if it hasn't changed enough to
	// avoid "flapping" between two different integral values. Never "snap to"
	// zero, since even a little bit of traffic requires the tenant to resume.
	delta := math.Abs(ideal-float64(currentPods)) / float64(currentPods)
	if currentPods != 0 && delta <= desiredPodsUpdateThreshold {
		ideal = float64(currentPods)
	}

	request := int32(math.Ceil(ideal))

	// Operator restarts empty the sliding window for metrics, so we don't allow downscales
	// until it's fully populated again.
	// Incidentally, this also gives some buffer time for m.DeltaQueries2m and m.DeltaNewConns2m
	// to be properly measured in the suspend case in calculateIdealPods.
	if m.FillRatio < 1.0 && request < currentPods {
		request = currentPods
	}

	if maxPods != nil {
		if *maxPods < request {
			request = *maxPods
		}
	}

	return request
}

// calculateIdealPods returns the exact number of pods desired by a tenant with
// the given metrics. Note that this number is the ideal fractional number of
// pods that will then need to be rounded and constrained (e.g. by MaxPods).
func calculateIdealPods(m *metrics.TenantMetrics) float64 {
	// Maintain one pod per average vCPU used over last 5 minutes.
	desired := max(m.MaxCpu5m/3, m.AvgCpu5m)

	// Suspend the tenant if there have been no new connections and queries over
	// the last 2 minutes, no currently running jobs, no currently open
	// transactions, and one of its remaining pods has been running for at least
	// 6 minutes.
	//
	// TODO(jay): The job scheduler for scheduled jobs in CRDB only performs an
	// initial scan after 2 to 5 minutes from startup time. Terminating the pod
	// right at the 2m mark means that scheduled jobs may not have a chance to
	// even run. We will temporarily increase the minimum uptime of a SQL pod
	// from 2m to 6m (5m + 1m buffer for metrics to be updated and jobs to be
	// spawned) so that scheduled jobs can run. We should reduce this back to
	// 2m once a patch has been backported to 21.2.1 to address this issue.
	//
	// Note(alyshan): Jobs running should not be negative, however there appears to be a bug
	// in the reporting of that metric. See CC-25182.
	if !m.DeltaQueries2m && !m.DeltaNewConns2m && m.JobsRunning <= 0 && m.ConnsOpen == 0 && m.Uptime >= minUptime {
		return 0
	}

	// Ensure that we have at least one SQL pod even if the CPU is zero. This
	// can happen when the pod(s) in a tenant has just started. We should only
	// suspend the tenant once it has been resumed for at least 2 minutes.
	if desired <= 0 {
		return 1
	}
	return desired
}

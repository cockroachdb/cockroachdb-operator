package betaclusterctrl

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/version"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReleaseType represents the type of a release
type ReleaseType int

const (
	// Regular releases are mandatory upgrades that cannot be skipped.
	Regular ReleaseType = iota
	// Innovation releases are optional upgrades that can be skipped.
	Innovation
	// versionCheckerJobNamePrefix is the prefix for version checker job names
	versionCheckerJobNamePrefix = "version-checker"
	// detectedVersionAnnotation caches the version read from job logs so that
	// a successful result survives pod garbage collection.
	detectedVersionAnnotation = "crdb.cockroachlabs.com/detected-version"
)

// errNoPodsFound is returned by readVersionFromJobLogs when the kubelet has
// garbage-collected all pods for the version checker job. This is the only
// log-read error that is safe to retry by deleting the job and recreating it.
var errNoPodsFound = errors.New("no pods found for version checker job")

type VersionCheckerJob interface {
	readVersionFromJobLogs() (version.Version, error)
	cleanupVersionCheckerJob() error
	validateClusterUpgrade(cluster *crdbv1beta1.CrdbCluster, currentVersion version.Version) ValidationResponse
	buildVersionCheckerJob(cluster *crdbv1beta1.CrdbCluster) *batchv1.Job
	checkVersionCheckerJobStatus() (JobStatus, version.Version, error)
}

type versionCheckerJob struct {
	ctx     context.Context
	jobName string
	ctl     *kube.Ctl

	// Function field for testing - allows injecting custom log reading behavior
	logReader func() (version.Version, error)
}

func NewVersionCheckerJob(ctx context.Context, ctl *kube.Ctl, jobName string) VersionCheckerJob {
	return &versionCheckerJob{
		ctx:     ctx,
		jobName: jobName,
		ctl:     ctl,
	}
}

// WithLogReader sets a custom log reader function for testing
func (v *versionCheckerJob) WithLogReader(
	logReader func() (version.Version, error),
) *versionCheckerJob {
	v.logReader = logReader
	return v
}

// readVersionFromJobLogs reads the logs from the version checker job pod and extracts the version
func (v *versionCheckerJob) readVersionFromJobLogs() (version.Version, error) {
	// Use injected function if available (for testing)
	if v.logReader != nil {
		return v.logReader()
	}

	// Real implementation
	var pods corev1.PodList
	listOptions := &kube.ListOptions{
		LabelSelector: kube.ParseLabelOrDie(fmt.Sprintf("job-name=%s", v.jobName)),
	}
	if err := v.ctl.List(v.ctx, listOptions, &pods); err != nil {
		return version.Version{}, errors.Wrapf(err, "failed to list pods for job %s", v.jobName)
	}

	if len(pods.Items) == 0 {
		return version.Version{}, errors.Wrapf(errNoPodsFound, "%s", v.jobName)
	}

	// Job retries can leave multiple pods in arbitrary order. We prefer succeeded
	// pods over others and among pods in the same phase we pick the most recently created.
	pod := pods.Items[0]
	podSucceeded := pod.Status.Phase == corev1.PodSucceeded
	for _, p := range pods.Items[1:] {
		pSucceeded := p.Status.Phase == corev1.PodSucceeded
		switch {
		case pSucceeded && !podSucceeded:
			// succeeded beats non-succeeded regardless of age
			pod = p
			podSucceeded = true
		case pSucceeded == podSucceeded && p.CreationTimestamp.After(pod.CreationTimestamp.Time):
			// same phase, so pick the more recently created one
			pod = p
		}
	}

	// If no succeeded pod was found among the listed pods, the kubelet likely
	// garbage-collected it while leaving older failed retry pods behind. Reading
	// logs from a failed pod would produce wrong output, so we treat this the
	// same as no pods found and let the caller delete the job and retry.
	if !podSucceeded {
		return version.Version{}, errors.Wrapf(errNoPodsFound, "job %s has pods but none succeeded (succeeded pod likely garbage-collected)", v.jobName)
	}

	// Get logs from the pod using the REST client
	logs, err := v.ctl.GetContainerLogs(v.ctx, pod.Name, "version-checker", kube.GetContainerLogsOptions{})
	if err != nil {
		return version.Version{}, errors.Wrapf(err, "failed to get logs from pod %s", pod.Name)
	}

	logging.Infof(v.ctx, "Version checker job %s logs: %s", v.jobName, logs)

	// Parse version from logs and trim any whitespace or newlines
	ver, err := version.Parse(strings.TrimSpace(logs))
	if err != nil {
		return version.Version{}, errors.Wrapf(err, "failed to parse version from logs: %s", logs)
	}
	return ver, nil
}

func (v *versionCheckerJob) createVersionCheckerJobIfNotExists(
	cluster *crdbv1beta1.CrdbCluster,
) error {
	if v.ctl == nil {
		return errors.New("kubernetes client is nil")
	}
	var job batchv1.Job
	if err := v.ctl.Get(v.ctx, v.jobName, &job); err != nil {
		if kubeerrors.IsNotFound(err) {
			job := v.buildVersionCheckerJob(cluster)
			if err := v.ctl.Create(v.ctx, job); err != nil {
				return errors.Wrapf(err, "failed to create version checker job %s", v.jobName)
			}
			return nil
		}
		return errors.Wrap(err, "failed to get version checker job")
	}

	return nil
}

// buildVersionCheckerJob creates a Kubernetes job that runs `cockroach version --build-tag`
// to detect the version of the target image without starting the full CockroachDB process.
func (v *versionCheckerJob) buildVersionCheckerJob(cluster *crdbv1beta1.CrdbCluster) *batchv1.Job {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        v.jobName,
			Namespace:   cluster.Namespace,
			Labels:      cluster.Spec.Template.Labels,
			Annotations: cluster.Spec.Template.Annotations,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          new(int32(3)),
			ActiveDeadlineSeconds: new(int64(300)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      cluster.Spec.Template.Labels,
					Annotations: cluster.Spec.Template.Annotations,
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:  "version-checker",
							Image: cluster.Spec.Template.Spec.Image,
							Command: []string{
								"/cockroach/cockroach",
								"version",
								"--build-tag",
							},
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
		},
	}

	ownerRef, err := kube.OwnerReferenceFor(v.ctl.Scheme(), cluster)
	if err == nil {
		ownerRef.Controller = new(true)
		job.SetOwnerReferences([]metav1.OwnerReference{ownerRef})
	} else {
		logging.Warnf(v.ctx, "failed to get owner reference for cluster %s: %v", cluster.Name, err)
	}

	return job
}

// cleanupVersionCheckerJob removes the version checker job
func (v *versionCheckerJob) cleanupVersionCheckerJob() error {
	if v.ctl == nil {
		return errors.New("kubernetes client is nil")
	}
	var job batchv1.Job
	if err := v.ctl.Get(v.ctx, v.jobName, &job); err != nil {
		if kubeerrors.IsNotFound(err) {
			return nil // Already cleaned up
		}
		return errors.Wrap(err, "failed to get version checker job for cleanup")
	}

	// Delete the job with cascade delete to remove associated pods
	deletePolicy := metav1.DeletePropagationForeground
	if err := v.ctl.DeleteWithOptions(v.ctx, kube.DeleteOptions{
		GracePeriodSeconds: new(int64(0)),
		PropagationPolicy:  deletePolicy,
	}, &job); err != nil {
		return errors.Wrap(err, "failed to delete version checker job")
	}

	return nil
}

// ValidationResult represents the result of version validation
type ValidationResult int

const (
	// ValidationPending indicates the validation job is still running
	ValidationPending ValidationResult = iota
	// ValidationSucceeded indicates the validation passed
	ValidationSucceeded
	// ValidationFailed indicates the validation failed
	ValidationFailed
)

// ValidationResponse contains the result and any detected version
type ValidationResponse struct {
	Result  ValidationResult
	Version string
	Error   error
}

// validateClusterUpgrade validates the cluster upgrade by checking version compatibility
// Returns ValidationResponse indicating the current state of validation
func (v *versionCheckerJob) validateClusterUpgrade(
	cluster *crdbv1beta1.CrdbCluster, currentVersion version.Version,
) ValidationResponse {
	if v.ctl == nil {
		return ValidationResponse{
			Result: ValidationFailed,
			Error:  errors.New("kubernetes client is nil"),
		}
	}
	// If a job already exists for a different target image, delete it so a fresh
	// one is created for the current image. This handles the case where the user
	// changes spec.image while a validation job is still running or completed.
	var existingJob batchv1.Job
	if err := v.ctl.Get(v.ctx, v.jobName, &existingJob); err == nil {
		if len(existingJob.Spec.Template.Spec.Containers) > 0 &&
			existingJob.Spec.Template.Spec.Containers[0].Image != cluster.Spec.Template.Spec.Image {
			logging.Infof(v.ctx, "Version checker job %s was created for a different image, recreating for %s", v.jobName, cluster.Spec.Template.Spec.Image)
			if cleanupErr := v.cleanupVersionCheckerJob(); cleanupErr != nil {
				logging.Warnf(v.ctx, "Failed to clean up stale version checker job %s: %v", v.jobName, cleanupErr)
			}
			return ValidationResponse{Result: ValidationPending}
		}
	}

	// Check if version checker job exists and its status
	jobStatus, detectedVersion, err := v.checkVersionCheckerJobStatus()
	if err != nil {
		// If there's an error checking job status, try to create the job
		if kubeerrors.IsNotFound(err) {
			// Job doesn't exist, create it
			image := cluster.Spec.Template.Spec.Image
			logging.Infof(v.ctx, "Creating version checker job %s for image %s", v.jobName, image)

			if createErr := v.createVersionCheckerJobIfNotExists(cluster); createErr != nil {
				return ValidationResponse{
					Result: ValidationFailed,
					Error:  errors.Wrapf(createErr, "failed to create version checker job %s", v.jobName),
				}
			}

			// Job created, return pending to requeue
			return ValidationResponse{Result: ValidationPending}
		}

		// Other error checking job status
		return ValidationResponse{
			Result: ValidationFailed,
			Error:  errors.Wrapf(err, "failed to check version checker job %s status", v.jobName),
		}
	}

	switch jobStatus {
	case JobSucceeded:
		// Job completed successfully, validate the upgrade path
		if detectedVersion == (version.Version{}) {
			return ValidationResponse{
				Result: ValidationFailed,
				Error:  errors.Newf("version checker job %s succeeded but no version detected", v.jobName),
			}
		}

		// If the upgrade path is invalid, clean up the completed job. Without this
		// the next reconcile sees the same completed job, reads the same logs, and
		// fails again on the same version.
		if validationErr := validateUpgradeVersion(v.ctx, currentVersion, detectedVersion); validationErr != nil {
			if cleanupErr := v.cleanupVersionCheckerJob(); cleanupErr != nil {
				logging.Warnf(v.ctx, "Failed to clean up version checker job %s after invalid upgrade path: %v", v.jobName, cleanupErr)
			}
			return ValidationResponse{
				Result: ValidationFailed,
				Error:  validationErr,
			}
		}

		return ValidationResponse{
			Result:  ValidationSucceeded,
			Version: detectedVersion.String(),
		}

	case JobFailed:
		// Job failed, clean up and return failure
		if cleanupErr := v.cleanupVersionCheckerJob(); cleanupErr != nil {
			logging.Warnf(v.ctx, "Failed to clean up failed version checker job %s: %v", v.jobName, cleanupErr)
		}

		return ValidationResponse{
			Result: ValidationFailed,
			Error:  errors.Newf("version checker job %s failed", v.jobName),
		}

	case JobActive:
		// Job is still running, return pending to requeue
		logging.Debugf(v.ctx, "Version checker job %s is still running, will requeue", v.jobName)
		return ValidationResponse{Result: ValidationPending}

	default:
		return ValidationResponse{
			Result: ValidationFailed,
			Error:  errors.Newf("unknown job status for version checker job %s", v.jobName),
		}
	}
}

// JobStatus represents the status of a Kubernetes job
type JobStatus int

const (
	// JobActive indicates the job is still running
	JobActive JobStatus = iota
	// JobSucceeded indicates the job completed successfully
	JobSucceeded
	// JobFailed indicates the job failed
	JobFailed
)

// checkVersionCheckerJobStatus checks the status of a version checker job without waiting
// Returns (JobStatus, detectedVersion, error)
func (v *versionCheckerJob) checkVersionCheckerJobStatus() (JobStatus, version.Version, error) {
	if v.ctl == nil {
		return JobActive, version.Version{}, errors.New("kubernetes client is nil")
	}

	var job batchv1.Job
	if err := v.ctl.Get(v.ctx, v.jobName, &job); err != nil {
		return JobActive, version.Version{}, err
	}

	// Check if job completed successfully
	if job.Status.Succeeded > 0 {
		logging.Infof(v.ctx, "Version checker job %s completed successfully", v.jobName)

		// Return the cached version if a previous reconcile already read the
		// logs. This avoids depending on the pod still existing.
		if cached, ok := job.Annotations[detectedVersionAnnotation]; ok {
			ver, err := version.Parse(cached)
			if err != nil {
				// We wrote this annotation ourselves, so a parse failure is
				// unexpected. Treat it as a cache miss and try reading logs.
				logging.Warnf(v.ctx, "Failed to parse cached version annotation on job %s, treating as cache miss: %v", v.jobName, err)
				delete(job.Annotations, detectedVersionAnnotation)
			} else {
				return JobSucceeded, ver, nil
			}
		}

		ver, err := v.readVersionFromJobLogs()
		if err != nil {
			// Only retry when the pod was garbage-collected. Other errors like
			// log parse failures or API errors will repeat on every attempt, so
			// surface them to trigger ValidationFailed and set the rejected-image
			// annotation that stops the cycle.
			if errors.Is(err, errNoPodsFound) {
				logging.Warnf(v.ctx, "Job %s succeeded but pod was garbage-collected, deleting job to retry: %v", v.jobName, err)
				if cleanupErr := v.cleanupVersionCheckerJob(); cleanupErr != nil {
					logging.Warnf(v.ctx, "Failed to clean up stale version checker job %s: %v", v.jobName, cleanupErr)
				}
				return JobActive, version.Version{}, nil
			}
			return JobSucceeded, version.Version{}, errors.Wrapf(err, "failed to read logs from completed job %s", v.jobName)
		}

		// Cache the version on the job so subsequent reconciles don't depend
		// on the pod still existing.
		if job.Annotations == nil {
			job.Annotations = make(map[string]string)
		}
		job.Annotations[detectedVersionAnnotation] = ver.String()
		if updateErr := v.ctl.Update(v.ctx, &job); updateErr != nil {
			logging.Warnf(v.ctx, "Failed to cache detected version on job %s: %v", v.jobName, updateErr)
		}
		return JobSucceeded, ver, nil
	}

	// Check if job failed
	if job.Status.Failed > 0 {
		return JobFailed, version.Version{}, nil
	}

	// Job is still running
	return JobActive, version.Version{}, nil
}

// validateUpgradeVersion validates whether the proposed upgrade is allowed.
// This is a simplified version that validates upgrade paths based on version strings.
func validateUpgradeVersion(ctx context.Context, currentVersion, newVersion version.Version) error {
	if isPatch(newVersion, currentVersion) || isMajorUpgradeAllowed(newVersion, currentVersion) || isMajorRollbackAllowed(newVersion, currentVersion) {
		logging.Infof(ctx, "Version upgrade validated: %s -> %s", currentVersion, newVersion)
		return nil
	}

	return errors.Newf("invalid upgrade path: cannot go from %s to %s", currentVersion, newVersion)
}

// isMajorUpgradeAllowed determines whether an upgrade from the current version to the desired version is allowed
// based on the release cycle and upgrade policies.
//
// Upgrade Rules:
// 1. For versions prior to 2024:
//   - A major upgrade is allowed if
//   - The major version remains the same and the minor version increments by 1 (e.g., 19.1 -> 19.2).
//   - The major version increments by 1 and the minor version resets to 1. (e.g., 19.2 -> 20.1).
//
// 2. For version of 2024:
//   - There are 3 releases (24.1, 24.2, 24.3) where 24.2 being innovation release
//
// 3. For versions from 2025 onwards:
//   - Releases follow a quarterly cycle, with some releases designated as innovation.
//   - Users are allowed to skip upgrading to an innovation release.
//   - The upgrade is permitted if the desired version matches one of the next valid releases.
//
// Parameters:
// - wantVersion: The target version to which an upgrade is requested.
// - currentVersion: The currently installed version.
//
// Returns:
// - true if the upgrade is allowed based on the release rules; false otherwise.
func isMajorUpgradeAllowed(wantVersion version.Version, currentVersion version.Version) bool {
	// Two cases:
	// 19.1 to 19.2 -> same year
	// 19.2 to 20.1 -> next year

	// Since 2025, we have adopted a quarterly release cycle, with two of the four annual releases designated
	// as innovation releases. Users have the option to skip upgrading to an innovation release.
	// For 2024, we just had 3 releases overall, with 24.2 as the innovation release.
	if currentVersion.Major().Year >= 24 {
		// Four Cases:
		// 24.1 to 24.2 -> Same year without skipping innovation release
		// 24.1 to 24.3 -> Same year with skipping innovation release
		// 24.3 to 25.1 -> Next year without skipping innovation release
		// 24.3 to 25.2 -> Next year with skipping innovation release
		nextPossibleRelease := getNextReleases(fmt.Sprintf("%d.%d", currentVersion.Major().Year, currentVersion.Major().Ordinal))
		return slices.Contains(nextPossibleRelease, fmt.Sprintf("%d.%d", wantVersion.Major().Year, wantVersion.Major().Ordinal))
	}

	return (currentVersion.Major().Year == wantVersion.Major().Year && currentVersion.Major().Ordinal+1 == wantVersion.Major().Ordinal) ||
		(currentVersion.Major().Year+1 == wantVersion.Major().Year && currentVersion.Major().Ordinal-1 == wantVersion.Major().Ordinal)
}

// isMajorRollbackAllowed determines whether rolling back from the current version to the desired version is allowed
// based on the release cycle and rollback policies.
//
// Rollback Rules:
// 1. For versions prior to 2024:
//   - A rollback is allowed if:
//   - The major version remains the same and the minor version decrements by 1 (e.g., 19.2 -> 19.1).
//   - The major version decrements by 1 and the minor version is the last release. (e.g., 20.1 -> 19.2).
//
// 2. For versions from 2024 onwards:
//   - Users can skip rolling back to an innovation release.
//   - The rollback is permitted if the desired version matches one of the valid previous releases.
//
// Parameters:
// - wantVersion: The target version to which a rollback is requested.
// - currentVersion: The currently installed version.
//
// Returns:
// - true if the rollback is allowed based on the rollback policies; false otherwise.
func isMajorRollbackAllowed(wantVersion version.Version, currentVersion version.Version) bool {
	// Two cases:
	// 19.2 to 19.1 -> same year
	// 20.1 to 19.2 -> previous year

	// Since 2024, users have the option to skip rollback to an innovation release.
	if wantVersion.Major().Year >= 24 {
		// Four cases:
		// 24.2 -> 24.1 -> Same year without skipping innovation release
		// 24.3 -> 24.1 -> Same year with skipping innovation release
		// 25.1 -> 24.3 -> Previous year without skipping innovation release
		// 25.2 -> 24.3 -> Previous year with skipping innovation release
		rollbackReleases := getPreviousReleases(fmt.Sprintf("%d.%d", currentVersion.Major().Year, currentVersion.Major().Ordinal))
		return slices.Contains(rollbackReleases, fmt.Sprintf("%d.%d", wantVersion.Major().Year, wantVersion.Major().Ordinal))
	}

	return (currentVersion.Major().Year == wantVersion.Major().Year && currentVersion.Major().Ordinal == wantVersion.Major().Ordinal+1) ||
		(currentVersion.Major().Year == wantVersion.Major().Year+1 && currentVersion.Major().Ordinal == wantVersion.Major().Ordinal-1)
}

func isPatch(wantVersion version.Version, currentVersion version.Version) bool {
	return currentVersion.Major().Year == wantVersion.Major().Year && currentVersion.Major().Ordinal == wantVersion.Major().Ordinal
}

func getReleaseType(major, minor int) ReleaseType {
	// Before 25.1, we define them explicitly
	switch fmt.Sprintf("%d.%d", major, minor) {
	case "24.1", "24.3":
		return Regular
	case "24.2":
		return Innovation
	}

	// Post 25.1: Odd releases (1,3) are Innovation, Even (2,4) are Regular
	if minor%2 == 1 {
		return Innovation
	}
	return Regular
}

// generateReleases generates a list of release versions from a 24.1 to the specified year.
// The releases follow a "YY.Q" format, where YY represents the last two digits of the year, and Q represents the quarter.
//
// Parameters:
// - upToYear: The last two digits of the year up to which releases should be generated.
//
// Returns:
//   - A slice of strings containing release versions from "24.1" onward, up to the specified year, with four
//     releases per year (except 2024).
func generateReleases(upToYear int) []string {
	var releases = []string{"24.1", "24.2", "24.3"}

	for year := 25; year <= upToYear; year++ {
		for quarter := 1; quarter <= 4; quarter++ {
			releases = append(releases, fmt.Sprintf("%d.%d", year, quarter))
		}
	}

	return releases
}

// releaseYear extracts the two-digit year from a "YY.Q" release string (e.g., "24.1" → 24).
// Callers always pass well-formed version strings, so the parse error is discarded.
func releaseYear(v string) int {
	year, _ := strconv.Atoi(strings.Split(v, ".")[0])
	return year
}

// getNextReleases returns a list of possible upgradable release versions following the given currentVersion.
// It iterates through the generated release versions for the current year and collects the next releases
// after finding the currentVersion. The function stops collecting releases once it encounters the next regular release.
//
// Parameters:
// - currentVersion: The current release version in the format "YY.Q" (e.g., "24.1").
//
// Returns:
// - A slice of strings containing the possible upgradable release versions, stopping at the next regular release.
func getNextReleases(currentVersion string) []string {
	var nextReleases []string
	var found bool

	// Look two years past the current version so upgrades that cross a calendar
	// year boundary are covered.
	releases := generateReleases(releaseYear(currentVersion) + 2)
	for _, release := range releases {
		year, _ := strconv.Atoi(strings.Split(release, ".")[0])
		quarter, _ := strconv.Atoi(strings.Split(release, ".")[1])

		if found {
			nextReleases = append(nextReleases, release)
			if getReleaseType(year, quarter) == Regular {
				break // Stop at the next regular release
			}
		}
		if release == currentVersion {
			found = true
		}
	}

	return nextReleases
}

// getPreviousReleases returns a list of possible rollback release versions preceding the given currentVersion.
// It iterates through the generated release versions in reverse order and collects previous releases
// until it encounters the last regular release before the currentVersion.
//
// Parameters:
// - currentVersion: The current release version in the format "YY.Q" (e.g., "24.1").
//
// Returns:
// - A slice of strings containing possible rollback release versions, stopping at the last regular release before the currentVersion.
func getPreviousReleases(currentVersion string) []string {
	var prevReleases []string
	var found bool

	// Rollbacks only go backwards, so we only need releases up to the current version's year.
	releases := generateReleases(releaseYear(currentVersion))
	for _, v := range slices.Backward(releases) {
		release := v
		year, _ := strconv.Atoi(strings.Split(release, ".")[0])
		quarter, _ := strconv.Atoi(strings.Split(release, ".")[1])
		if found {
			prevReleases = append(prevReleases, release)
			if getReleaseType(year, quarter) == Regular {
				break
			}
		}
		if release == currentVersion {
			found = true
		}
	}

	return prevReleases
}

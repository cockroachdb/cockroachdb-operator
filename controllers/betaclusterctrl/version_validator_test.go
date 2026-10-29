package betaclusterctrl

import (
	"context"
	"reflect"
	"testing"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestValidateUpgradeVersion(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name        string
		currentVer  version.Version
		newVer      version.Version
		expectError bool
		errorMsg    string
	}{
		{
			name:        "valid patch upgrade",
			currentVer:  version.MustParse("v23.1.0"),
			newVer:      version.MustParse("v23.1.1"),
			expectError: false,
		},
		{
			name:        "valid major upgrade pre-2024",
			currentVer:  version.MustParse("v23.1.0"),
			newVer:      version.MustParse("v23.2.0"),
			expectError: false,
		},
		{
			name:        "valid year boundary upgrade pre-2024",
			currentVer:  version.MustParse("v23.2.0"),
			newVer:      version.MustParse("v24.1.0"),
			expectError: false,
		},
		{
			name:        "valid upgrade skipping innovation release",
			currentVer:  version.MustParse("v24.1.0"),
			newVer:      version.MustParse("v24.3.0"),
			expectError: false,
		},
		{
			name:        "valid rollback",
			currentVer:  version.MustParse("v24.2.0"),
			newVer:      version.MustParse("v24.1.0"),
			expectError: false,
		},
		{
			name:        "invalid upgrade - skip too many versions",
			currentVer:  version.MustParse("v23.1.0"),
			newVer:      version.MustParse("v25.1.0"),
			expectError: true,
			errorMsg:    "invalid upgrade path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateUpgradeVersion(ctx, tt.currentVer, tt.newVer)

			if tt.expectError {
				require.Error(t, err, "Expected an error but got none")
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg, "Error message doesn't contain expected text")
				}
			} else {
				require.NoError(t, err, "Expected no error but got: %v", err)
			}
		})
	}
}

func TestIsPatch(t *testing.T) {
	tests := []struct {
		description    string
		wantVersion    version.Version
		currentVersion version.Version
		result         bool
	}{
		{
			"isPatch true",
			version.MustParse("v20.1.6"),
			version.MustParse("v20.1.5"),
			true,
		},
		{
			"isPatch false",
			version.MustParse("v19.2.6"),
			version.MustParse("v20.1.5"),
			false,
		},
		{
			"isPatch false test two",
			version.MustParse("v19.2.6"),
			version.MustParse("v19.3.5"),
			false,
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			require.True(t, isPatch(test.wantVersion, test.currentVersion) == test.result, "patch result failed")
		})
	}

}

func TestIsMajorUpgradeAllowed(t *testing.T) {
	tests := []struct {
		description    string
		wantVersion    version.Version
		currentVersion version.Version
		result         bool
	}{
		{
			"forward minor update",
			version.MustParse("v20.1.6"),
			version.MustParse("v20.1.5"),
			false,
		},
		{
			"forward major update",
			version.MustParse("v20.1.6"),
			version.MustParse("v19.2.5"),
			true,
		},
		{
			"backward minor version",
			version.MustParse("v19.2.6"),
			version.MustParse("v19.3.5"),
			false,
		},
		{
			"is backward major version",
			version.MustParse("v19.2.6"),
			version.MustParse("v21.3.5"),
			false,
		},
		{
			"is two major",
			version.MustParse("v22.2.6"),
			version.MustParse("v19.3.5"),
			false,
		},
		{
			"is skipping innovation release same year",
			version.MustParse("v24.3.0"),
			version.MustParse("v24.1.0"),
			true,
		},
		{
			"is skipping innovation release next year",
			version.MustParse("v25.2.0"),
			version.MustParse("v24.3.0"),
			true,
		},
		{
			"is skipping innovation and regular release",
			version.MustParse("v25.1.0"),
			version.MustParse("v24.1.0"),
			false,
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			require.True(t, isMajorUpgradeAllowed(test.wantVersion, test.currentVersion) == test.result, "isMajorUpgradeAllowed test failed")
		})
	}
}

func TestIsMajorRollbackAllowed(t *testing.T) {
	tests := []struct {
		description    string
		wantVersion    version.Version
		currentVersion version.Version
		result         bool
	}{
		{
			"forward minor update",
			version.MustParse("v20.1.6"),
			version.MustParse("v20.1.5"),
			false,
		},
		{
			"backward major version 20 to 19",
			version.MustParse("v19.2.6"),
			version.MustParse("v20.1.5"),
			true,
		},
		{
			"backward major version 21 to 20",
			version.MustParse("v20.2.6"),
			version.MustParse("v21.1.5"),
			true,
		},
		{
			"is two major",
			version.MustParse("v22.2.6"),
			version.MustParse("v19.3.5"),
			false,
		},
		{
			"is skipping innovation release for same year",
			version.MustParse("v24.1.0"),
			version.MustParse("v24.3.0"),
			true,
		},
		{
			"is skipping innovation release previous year",
			version.MustParse("v24.3.0"),
			version.MustParse("v25.2.0"),
			true,
		},
		{
			"is skipping innovation and regular release",
			version.MustParse("v24.1.0"),
			version.MustParse("v25.1.0"),
			false,
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			require.True(t, isMajorRollbackAllowed(test.wantVersion, test.currentVersion) == test.result, "isMajorRollbackAllowed test failed")
		})
	}
}

func TestGetNextReleases(t *testing.T) {
	tests := []struct {
		description    string
		currentVersion string
		result         []string
	}{
		{
			"returns the possible upgrade targets for 24.1",
			"24.1",
			[]string{"24.2", "24.3"},
		},
		{
			"returns the possible upgrade targets for 24.3",
			"24.3",
			[]string{"25.1", "25.2"},
		},
	}
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			require.True(t, reflect.DeepEqual(getNextReleases(test.currentVersion), test.result),
				"getNextReleases test failed")
		})
	}
}

func TestGetPreviousReleases(t *testing.T) {
	tests := []struct {
		description    string
		currentVersion string
		result         []string
	}{
		{
			"returns the possible rollback targets for 24.3",
			"24.3",
			[]string{"24.2", "24.1"},
		},
		{
			"returns the possible upgrade targets for 25.2",
			"25.2",
			[]string{"25.1", "24.3"},
		},
	}
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			require.True(t, reflect.DeepEqual(getPreviousReleases(test.currentVersion), test.result),
				"getPreviousReleases test failed")
		})
	}
}

func TestGenerateReleases(t *testing.T) {
	tests := []struct {
		description string
		uptoYear    int
		result      []string
	}{
		{
			"returns possible releases till 2025",
			25,
			[]string{"24.1", "24.2", "24.3", "25.1", "25.2", "25.3", "25.4"},
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			require.True(t, reflect.DeepEqual(generateReleases(test.uptoYear), test.result), "generateReleases test failed")
		})
	}
}

func TestGetReleaseType(t *testing.T) {
	tests := []struct {
		description  string
		major, minor int
		result       ReleaseType
	}{
		{
			"returns releaseType of a 24.2 innovation",
			24, 2,
			Innovation,
		},
		{
			"returns releaseType of a 25.1 innovation",
			25, 1,
			Innovation,
		},
		{
			"returns releaseType of a 24.3 regular",
			24, 3,
			Regular,
		},
	}
	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			require.True(t, reflect.DeepEqual(getReleaseType(test.major, test.minor), test.result),
				"getReleaseType test failed")
		})
	}
}

func TestBuildVersionCheckerJob(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name        string
		jobName     string
		namespace   string
		image       string
		clusterName string
	}{
		{
			name:        "basic job creation",
			jobName:     "test-version-checker-123",
			namespace:   "test-namespace",
			image:       "cockroachdb/cockroach:v23.1.0",
			clusterName: "test-cluster",
		},
		{
			name:        "job creation with 2024 image",
			jobName:     "version-checker-456",
			namespace:   "production",
			image:       "cockroachdb/cockroach:v24.2.0",
			clusterName: "prod-cluster",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a test cluster object
			cluster := &crdbv1beta1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tt.clusterName,
					Namespace: tt.namespace,
				},
				Spec: crdbv1beta1.CrdbClusterSpec{
					Template: crdbv1beta1.CrdbNodeTemplate{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app":          "cockroach-version-checker",
								"crdb-cluster": tt.clusterName,
							},
							Annotations: map[string]string{
								"test-annotation": "test-value",
							},
						},
						Spec: crdbv1beta1.CrdbNodeSpec{
							Image: tt.image,
						},
					},
				},
			}

			// Create a fake ctl for testing
			ctl := kubetest.NewFakeCtl(kubetest.Options{AdvancedEmulation: true})

			// Create the version checker job
			vcj := NewVersionCheckerJob(ctx, ctl, tt.jobName)
			job := vcj.buildVersionCheckerJob(cluster)

			// Verify job metadata
			assert.Equal(t, tt.jobName, job.ObjectMeta.Name)
			assert.Equal(t, tt.namespace, job.ObjectMeta.Namespace)
			assert.Equal(t, cluster.Spec.Template.Labels, job.ObjectMeta.Labels)
			assert.Equal(t, cluster.Spec.Template.Annotations, job.ObjectMeta.Annotations)

			// Verify owner reference
			require.Len(t, job.ObjectMeta.OwnerReferences, 1)
			assert.Equal(t, "CrdbCluster", job.ObjectMeta.OwnerReferences[0].Kind)
			assert.Equal(t, tt.clusterName, job.ObjectMeta.OwnerReferences[0].Name)
			require.NotNil(t, job.ObjectMeta.OwnerReferences[0].Controller)
			assert.True(t, *job.ObjectMeta.OwnerReferences[0].Controller)

			// Verify job spec
			assert.Equal(t, int32(3), *job.Spec.BackoffLimit)
			// Verify pod template spec
			assert.Equal(t, corev1.RestartPolicyNever, job.Spec.Template.Spec.RestartPolicy)
			assert.Equal(t, cluster.Spec.Template.Labels, job.Spec.Template.ObjectMeta.Labels)
			assert.Equal(t, cluster.Spec.Template.Annotations, job.Spec.Template.ObjectMeta.Annotations)

			// Verify container
			require.Len(t, job.Spec.Template.Spec.Containers, 1)
			container := job.Spec.Template.Spec.Containers[0]
			assert.Equal(t, "version-checker", container.Name)
			assert.Equal(t, tt.image, container.Image)
			assert.Equal(t, []string{"/cockroach/cockroach", "version", "--build-tag"}, container.Command)

			// Verify resource requirements
			assert.Equal(t, resource.MustParse("100m"), container.Resources.Requests[corev1.ResourceCPU])
			assert.Equal(t, resource.MustParse("128Mi"), container.Resources.Requests[corev1.ResourceMemory])
			assert.Equal(t, resource.MustParse("200m"), container.Resources.Limits[corev1.ResourceCPU])
			assert.Equal(t, resource.MustParse("256Mi"), container.Resources.Limits[corev1.ResourceMemory])
		})
	}
}

func TestRequiresUpgradeValidation(t *testing.T) {
	tests := []struct {
		description string
		cluster     *crdbv1beta1.CrdbCluster
		result      bool
	}{
		{
			description: "cluster with current version but nil ctl - job creation would fail",
			cluster: &crdbv1beta1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "test-ns",
				},
				Status: crdbv1beta1.CrdbClusterStatus{
					Image:   "cockroachdb/cockroach:v24.1.0",
					Version: "v24.1.0",
				},
				Spec: crdbv1beta1.CrdbClusterSpec{
					Template: crdbv1beta1.CrdbNodeTemplate{
						Spec: crdbv1beta1.CrdbNodeSpec{
							Image: "cockroachdb/cockroach:v24.2.0",
						},
					},
				},
			},
			result: true,
		},
		{
			description: "cluster with current version and ctl - job creation would succeed",
			cluster: &crdbv1beta1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "test-ns",
				},
				Status: crdbv1beta1.CrdbClusterStatus{
					Image:   "cockroachdb/cockroach:v24.1.0",
					Version: "v24.1.0",
				},
				Spec: crdbv1beta1.CrdbClusterSpec{
					Template: crdbv1beta1.CrdbNodeTemplate{
						Spec: crdbv1beta1.CrdbNodeSpec{
							Image: "cockroachdb/cockroach:v24.1.0",
						},
					},
				},
			},
			result: false,
		},
		{
			description: "cluster with empty current version - validation skipped",
			cluster: &crdbv1beta1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "test-ns",
				},
				Status: crdbv1beta1.CrdbClusterStatus{
					Image:   "",
					Version: "", // Empty current version
				},
				Spec: crdbv1beta1.CrdbClusterSpec{
					Template: crdbv1beta1.CrdbNodeTemplate{
						Spec: crdbv1beta1.CrdbNodeSpec{
							Image: "cockroachdb/cockroach:v24.2.0",
						},
					},
				},
			},
			result: false,
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			needsValidation, currentVersion := requiresUpgradeValidation(test.cluster)
			require.True(t, needsValidation == test.result, "requiresUpgradeValidation test failed")
			if test.result {
				// Only check currentVersion when validation is actually needed
				require.True(t, currentVersion.String() == test.cluster.Status.Version, "currentVersion test failed")
			}
		})
	}
}

func TestMockVersionCheckerJob(t *testing.T) {
	tests := []struct {
		name           string
		cluster        *crdbv1beta1.CrdbCluster
		mockLogs       string
		mockLogsError  error
		expectedResult ValidationResult
	}{
		{
			name: "successful validation with mock logs",
			cluster: &crdbv1beta1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "test-ns",
				},
				Status: crdbv1beta1.CrdbClusterStatus{
					Image:   "cockroachdb/cockroach:v24.1.0",
					Version: "v24.1.0",
				},
				Spec: crdbv1beta1.CrdbClusterSpec{
					Template: crdbv1beta1.CrdbNodeTemplate{
						Spec: crdbv1beta1.CrdbNodeSpec{
							Image: "cockroachdb/cockroach:v24.2.0",
						},
					},
				},
			},
			mockLogs:       "v24.2.0",
			expectedResult: ValidationSucceeded,
		},
		{
			name: "failed validation due to invalid upgrade path",
			cluster: &crdbv1beta1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "test-ns",
				},
				Status: crdbv1beta1.CrdbClusterStatus{
					Image:   "cockroachdb/cockroach:v24.1.0",
					Version: "v24.1.0",
				},
				Spec: crdbv1beta1.CrdbClusterSpec{
					Template: crdbv1beta1.CrdbNodeTemplate{
						Spec: crdbv1beta1.CrdbNodeSpec{
							Image: "cockroachdb/cockroach:v25.1.0", // Invalid jump
						},
					},
				},
			},
			mockLogs:       "v25.1.0",
			expectedResult: ValidationFailed,
		},
		{
			name: "retries validation when pod is garbage-collected",
			cluster: &crdbv1beta1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "test-ns",
				},
				Status: crdbv1beta1.CrdbClusterStatus{
					Image:   "cockroachdb/cockroach:v24.1.0",
					Version: "v24.1.0",
				},
				Spec: crdbv1beta1.CrdbClusterSpec{
					Template: crdbv1beta1.CrdbNodeTemplate{
						Spec: crdbv1beta1.CrdbNodeSpec{
							Image: "cockroachdb/cockroach:v24.2.0",
						},
					},
				},
			},
			mockLogs:       "",
			mockLogsError:  errors.Wrap(errNoPodsFound, "test"),
			expectedResult: ValidationPending,
		},
		{
			name: "fails validation on non-retriable log error",
			cluster: &crdbv1beta1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "test-ns",
				},
				Status: crdbv1beta1.CrdbClusterStatus{
					Image:   "cockroachdb/cockroach:v24.1.0",
					Version: "v24.1.0",
				},
				Spec: crdbv1beta1.CrdbClusterSpec{
					Template: crdbv1beta1.CrdbNodeTemplate{
						Spec: crdbv1beta1.CrdbNodeSpec{
							Image: "cockroachdb/cockroach:v24.2.0",
						},
					},
				},
			},
			mockLogs:       "",
			mockLogsError:  assert.AnError,
			expectedResult: ValidationFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			// Create mock version checker job with proper namespace setup
			ctl := kubetest.NewFakeCtl(kubetest.Options{AdvancedEmulation: true})
			vcj := NewVersionCheckerJob(ctx, ctl, "test-job").(*versionCheckerJob).WithLogReader(func() (version.Version, error) {
				if tt.mockLogsError != nil {
					return version.Version{}, tt.mockLogsError
				}
				return version.Parse(tt.mockLogs)
			})

			// Create a successful job in the fake client to simulate job completion
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-job",
					Namespace: tt.cluster.Namespace,
				},
				Status: batchv1.JobStatus{
					Succeeded: 1, // Job completed successfully
				},
			}

			// Create a pod for the job (required for readVersionFromJobLogs)
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-job-pod",
					Namespace: tt.cluster.Namespace,
					Labels: map[string]string{
						"job-name": "test-job",
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodSucceeded,
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			}

			// Add the job and pod to the fake client with NoWait option
			require.NoError(t, ctl.Create(ctx, job))
			require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, pod))

			currentVer := version.MustParse(tt.cluster.Status.Version)
			resp := vcj.validateClusterUpgrade(tt.cluster, currentVer)

			assert.Equal(t, tt.expectedResult, resp.Result)

			if tt.expectedResult == ValidationFailed {
				assert.Error(t, resp.Error)
			} else {
				assert.NoError(t, resp.Error)
			}
		})
	}
}

func TestReadVersionFromJobLogs(t *testing.T) {
	tests := []struct {
		name          string
		mockLogs      string
		mockLogsError error
		expectedVer   string
		expectError   bool
	}{
		{
			name:        "successful version parsing",
			mockLogs:    "v24.1.0",
			expectedVer: "v24.1.0",
			expectError: false,
		},
		{
			name:        "version parsing with rc",
			mockLogs:    "v24.2.0-rc.1",
			expectedVer: "v24.2.0-rc.1",
			expectError: false,
		},
		{
			name:        "version parsing with beta",
			mockLogs:    "v25.1.0-beta.1",
			expectedVer: "v25.1.0-beta.1",
			expectError: false,
		},
		{
			name:          "failed to get logs",
			mockLogs:      "",
			mockLogsError: assert.AnError,
			expectError:   true,
		},
		{
			name:        "invalid version in logs",
			mockLogs:    "invalid-version",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctl := kubetest.NewFakeCtl(kubetest.Options{AdvancedEmulation: true})
			vcj := NewVersionCheckerJob(ctx, ctl, "test-job").(*versionCheckerJob).WithLogReader(func() (version.Version, error) {
				if tt.mockLogsError != nil {
					return version.Version{}, tt.mockLogsError
				}
				return version.Parse(tt.mockLogs)
			})

			ver, err := vcj.readVersionFromJobLogs()

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectedVer, ver.String())
			}
		})
	}
}

func TestCheckVersionCheckerJobStatus(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name            string
		jobExists       bool
		jobSucceeded    int32
		jobFailed       int32
		jobActive       int32
		expectedStatus  JobStatus
		expectedVersion string
		expectError     bool
	}{
		{
			name:           "job does not exist",
			jobExists:      false,
			expectedStatus: JobActive,
			expectError:    true,
		},
		{
			name:           "job is active",
			jobExists:      true,
			jobActive:      1,
			expectedStatus: JobActive,
			expectError:    false,
		},
		{
			name:            "job succeeded",
			jobExists:       true,
			jobSucceeded:    1,
			expectedStatus:  JobSucceeded,
			expectedVersion: "v24.1.0",
			expectError:     false,
		},
		{
			name:           "job failed",
			jobExists:      true,
			jobFailed:      1,
			expectedStatus: JobFailed,
			expectError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.jobExists {
				// Create a fake ctl with a job
				ctl := kubetest.NewFakeCtl(kubetest.Options{AdvancedEmulation: true})

				job := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-job",
						Namespace: "test-ns",
					},
					Status: batchv1.JobStatus{
						Active:    tt.jobActive,
						Succeeded: tt.jobSucceeded,
						Failed:    tt.jobFailed,
					},
				}

				require.NoError(t, ctl.Create(ctx, job))

				var vcj VersionCheckerJob
				if tt.jobSucceeded > 0 {
					// Inject a log reader so checkVersionCheckerJobStatus can read the version.
					vcj = NewVersionCheckerJob(ctx, ctl, "test-job").(*versionCheckerJob).WithLogReader(func() (version.Version, error) {
						return version.MustParse(tt.expectedVersion), nil
					})
				} else {
					vcj = NewVersionCheckerJob(ctx, ctl, "test-job")
				}

				status, ver, err := vcj.checkVersionCheckerJobStatus()

				if tt.expectError {
					assert.Error(t, err)
				} else {
					assert.NoError(t, err)
					assert.Equal(t, tt.expectedStatus, status)
					if tt.expectedVersion != "" {
						assert.Equal(t, tt.expectedVersion, ver.String())
					}
				}
			} else {
				// Test with empty fake ctl (no job exists)
				ctl := kubetest.NewFakeCtl(kubetest.Options{AdvancedEmulation: true})
				vcj := NewVersionCheckerJob(ctx, ctl, "test-job")

				_, _, err := vcj.checkVersionCheckerJobStatus()
				assert.Error(t, err) // Should get NotFound error
			}
		})
	}
}

func TestCheckVersionCheckerJobStatusCaching(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name            string
		jobAnnotations  map[string]string
		logReaderErr    error
		logVersion      string
		expectedStatus  JobStatus
		expectedVersion string
		expectError     bool
		expectCacheSet  bool
	}{
		{
			name:            "reads from pod logs and caches version on job",
			logVersion:      "v24.2.0",
			expectedStatus:  JobSucceeded,
			expectedVersion: "v24.2.0",
			expectCacheSet:  true,
		},
		{
			name: "returns cached version when pod logs are unavailable",
			jobAnnotations: map[string]string{
				detectedVersionAnnotation: "v24.2.0",
			},
			logReaderErr:    assert.AnError,
			expectedStatus:  JobSucceeded,
			expectedVersion: "v24.2.0",
		},
		{
			name: "falls back to logs when cached annotation is unparsable",
			jobAnnotations: map[string]string{
				detectedVersionAnnotation: "not-a-version",
			},
			logVersion:      "v24.2.0",
			expectedStatus:  JobSucceeded,
			expectedVersion: "v24.2.0",
			expectCacheSet:  true,
		},
		{
			name:           "retries when pod is garbage-collected and no cached annotation",
			logReaderErr:   errors.Wrap(errNoPodsFound, "test"),
			expectedStatus: JobActive,
		},
		{
			name:           "fails on non-retriable log error without cached annotation",
			logReaderErr:   assert.AnError,
			expectedStatus: JobSucceeded,
			expectError:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctl := kubetest.NewFakeCtl(kubetest.Options{AdvancedEmulation: true})

			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-job",
					Namespace:   "test-ns",
					Annotations: tt.jobAnnotations,
				},
				Status: batchv1.JobStatus{
					Succeeded: 1,
				},
			}
			require.NoError(t, ctl.Create(ctx, job))

			vcj := NewVersionCheckerJob(ctx, ctl, "test-job").(*versionCheckerJob).WithLogReader(func() (version.Version, error) {
				if tt.logReaderErr != nil {
					return version.Version{}, tt.logReaderErr
				}
				return version.MustParse(tt.logVersion), nil
			})

			status, ver, err := vcj.checkVersionCheckerJobStatus()
			assert.Equal(t, tt.expectedStatus, status)

			if tt.expectError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.expectedVersion, ver.String())

			if tt.expectCacheSet {
				var updated batchv1.Job
				require.NoError(t, ctl.Get(ctx, "test-job", &updated))
				assert.Equal(t, tt.expectedVersion, updated.Annotations[detectedVersionAnnotation])
			}
		})
	}
}

// TestCheckVersionCheckerJobStatus_SucceededPodGCdFailedPodRemains verifies
// that when the job succeeded but the kubelet garbage-collected the succeeded
// pod while an older failed pod remains, we delete the job and retry instead
// of reading stale logs from the failed pod and permanently blocking the
// upgrade.
func TestCheckVersionCheckerJobStatus_SucceededPodGCdFailedPodRemains(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{AdvancedEmulation: true})

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-job",
			Namespace: "test-ns",
		},
		Status: batchv1.JobStatus{
			Succeeded: 1,
			Failed:    1,
		},
	}
	require.NoError(t, ctl.Create(ctx, job))

	// Only a failed pod exists; the succeeded pod was garbage-collected.
	failedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-job-failed",
			Namespace: "test-ns",
			Labels:    map[string]string{"job-name": "test-job"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}
	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, failedPod))

	// Use the real readVersionFromJobLogs (no injected logReader) so the pod
	// selection logic runs. The function should detect that no succeeded pod
	// exists and return errNoPodsFound, which triggers the delete-and-retry
	// path in checkVersionCheckerJobStatus.
	vcj := NewVersionCheckerJob(ctx, ctl, "test-job")

	status, _, err := vcj.checkVersionCheckerJobStatus()
	require.NoError(t, err)
	assert.Equal(t, JobActive, status, "should return JobActive to signal retry, not JobSucceeded with wrong logs")
}

func TestVersionParsing(t *testing.T) {
	tests := []struct {
		name          string
		versionStr    string
		expectError   bool
		expectedMajor string
	}{
		{
			name:          "stable version 2023",
			versionStr:    "v23.1.0",
			expectError:   false,
			expectedMajor: "v23.1",
		},
		{
			name:          "stable version 2024",
			versionStr:    "v24.2.0",
			expectError:   false,
			expectedMajor: "v24.2",
		},
		{
			name:          "rc version 2024",
			versionStr:    "v24.1.0-rc.1",
			expectError:   false,
			expectedMajor: "v24.1",
		},
		{
			name:          "beta version 2025",
			versionStr:    "v25.1.0-beta.1",
			expectError:   false,
			expectedMajor: "v25.1",
		},
		{
			name:          "patch version 2024",
			versionStr:    "v24.3.12",
			expectError:   false,
			expectedMajor: "v24.3",
		},
		{
			name:          "new year version 2025",
			versionStr:    "v25.1.0",
			expectError:   false,
			expectedMajor: "v25.1",
		},
		{
			name:        "invalid version",
			versionStr:  "invalid",
			expectError: true,
		},
		{
			name:        "version without v prefix",
			versionStr:  "24.1.0",
			expectError: true,
		},
		{
			name:        "malformed version",
			versionStr:  "v24.1",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := version.Parse(tt.versionStr)

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectedMajor, v.Major().String())
			}
		})
	}
}

func TestVersionCheckerJobConfiguration(t *testing.T) {
	// Test the constants and configuration values
	assert.Equal(t, "version-checker", versionCheckerJobNamePrefix)

	// Test job name generation pattern
	clusterName := "test-cluster"

	// Create a test cluster object
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName,
			Namespace: "test-ns",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Template: crdbv1beta1.CrdbNodeTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "cockroach-version-checker",
					},
				},
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image: "test-image",
				},
			},
		},
	}

	// Create a version checker job instance
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{AdvancedEmulation: true})
	vcj := NewVersionCheckerJob(ctx, ctl, "test-job")

	// Test the job creation
	job := vcj.buildVersionCheckerJob(cluster)
	assert.Contains(t, job.ObjectMeta.Name, "test-job")
}

func TestReleaseTypeConstants(t *testing.T) {
	// Test that the constants are defined correctly
	assert.Equal(t, ReleaseType(0), Regular)
	assert.Equal(t, ReleaseType(1), Innovation)
}

func TestNewVersionCheckerJob(t *testing.T) {
	ctx := context.Background()
	jobName := "test-job"
	ctl := kubetest.NewFakeCtl(kubetest.Options{AdvancedEmulation: true})

	vcj := NewVersionCheckerJob(ctx, ctl, jobName)

	// Verify the interface is implemented correctly
	assert.NotNil(t, vcj)

	// Test that we can call the interface methods
	_, err := vcj.readVersionFromJobLogs()
	assert.Error(t, err) // Should fail because no pods exist in fake client

	err = vcj.cleanupVersionCheckerJob()
	assert.NoError(t, err) // Should succeed because job doesn't exist (cleanup is idempotent)
}

func TestNilCtlGuards(t *testing.T) {
	ctx := context.Background()

	// All methods that directly use v.ctl must return an error rather than panic when ctl is nil.
	vcj := &versionCheckerJob{ctx: ctx, jobName: "test-job"}

	err := vcj.cleanupVersionCheckerJob()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "kubernetes client is nil")

	err = vcj.createVersionCheckerJobIfNotExists(&crdbv1beta1.CrdbCluster{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "kubernetes client is nil")

	_, _, err = vcj.checkVersionCheckerJobStatus()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "kubernetes client is nil")

	resp := vcj.validateClusterUpgrade(&crdbv1beta1.CrdbCluster{}, version.Version{})
	assert.Equal(t, ValidationFailed, resp.Result)
	assert.Contains(t, resp.Error.Error(), "kubernetes client is nil")
}

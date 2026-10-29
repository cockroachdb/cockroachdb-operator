package manifests

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func TestTenantPool(t *testing.T) {
	testCases := []TenantPoolOptions{
		{
			Name:           "custom-resources",
			CockroachImage: "us.gcr.io/cockroach-cloud-images/cockroachdb/cockroach:v22.1.3",
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:              resource.MustParse("1000m"),
					corev1.ResourceMemory:           resource.MustParse("1000M"),
					corev1.ResourceEphemeralStorage: resource.MustParse("1000M"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:              resource.MustParse("1m"),
					corev1.ResourceMemory:           resource.MustParse("1M"),
					corev1.ResourceEphemeralStorage: resource.MustParse("1M"),
				},
			},
		},
		{
			Name:                "internet-access",
			CockroachImage:      "us.gcr.io/cockroach-cloud-images/cockroachdb/cockroach:v22.1.3",
			AllowInternetAccess: true,
		},
		{
			Name:           "replicas",
			CockroachImage: "us.gcr.io/cockroach-cloud-images/cockroachdb/cockroach:v22.1.3",
			Replicas:       4,
		},
		{
			Name:           "locality-flags",
			CockroachImage: "us.gcr.io/cockroach-cloud-images/cockroachdb/cockroach:v22.1.3",
			Env: &cloud.Environment{
				CloudProvider: "aws",
				CloudRegion:   "region-thingy",
			},
		},
		{
			Name:                   "custom-cockroachchannel-env",
			CockroachImage:         "us.gcr.io/cockroach-cloud-images/cockroachdb/cockroach:v22.1.3",
			CockroachChannelEnvVal: "foo",
		},
		{
			Name:           "with-locality-file",
			CockroachImage: "us.gcr.io/cockroach-cloud-images/cockroachdb/cockroach:v24.2.0-cloudonly.1",
		},
		{
			Name:               "custom-otel-image",
			OtelCollectorImage: "us-docker.pkg.dev/cockroach-cloud-images/data-plane/opentelemetry-collector-cc:CUSTOM",
		},
		{
			Name:         "node-grpc-port",
			NodeGRPCPort: 26258,
		},
	}

	for _, options := range testCases {
		manifest := TenantPool(options)

		deployment := manifest[0].(*appsv1.Deployment)

		// Assertions that hold true for every tenant pool created that are
		// relied upon by the operator to create selectors.
		require.Equal(t, TenantPoolAppName, deployment.Labels["app"])
		require.Equal(t, options.Name, deployment.Labels[TenantPoolNameLabel])
		require.Equal(t, options.Name, deployment.Spec.Template.Labels[TenantPoolNameLabel])
		require.Equal(t, fmt.Sprintf("%s-%s", TenantPoolAppName, options.Name), deployment.Name)

		// Ensure that the tenant pool selector matches both the pool itself
		// and pods created by the pool.
		selector := MakeTenantPoolSelector(options.Name)
		require.True(t, selector.Matches(labels.Set(deployment.Labels)))
		require.True(t, selector.Matches(labels.Set(deployment.Spec.Template.Labels)))

		// If no requests have been specified, assert that the defaults have
		// been applied.
		if options.Resources.Requests == nil {
			require.Equal(t, DefaultTenantResourceRequests, deployment.Spec.Template.Spec.Containers[0].Resources.Requests)
		}

		// Ensure that SecurityContexts have been set accordingly.
		require.NotNil(t, deployment.Spec.Template.Spec.SecurityContext)
		for _, c := range deployment.Spec.Template.Spec.Containers {
			require.NotNil(t, c.SecurityContext)
		}

		actual, err := json.MarshalIndent(manifest, "", "\t")
		assert.NoError(t, err)
		testutils.AssertGoldenJSON(t, options.Name, actual)
	}
}

func TestTenantPoolReplicas(t *testing.T) {
	testCases := []struct {
		multiplier   float32
		hostReplicas int32
		zeroReplicas bool
		expected     uint32
	}{
		{0.33, 12, false, 4},
		{0, 1, false, 3},
		{0, 1, true, 0},
		{1, 1, false, 3},
		{1, 1, true, 0},
		{0.25, 12, false, 3},
		{0.25, 1, false, 3},
		{-0.1, 5, false, 3},
	}
	for i, tc := range testCases {
		name := fmt.Sprintf("#%d:multiplier=%v,hostReplicas=%d,zeroReplicas=%v", i, tc.multiplier,
			tc.hostReplicas, tc.zeroReplicas)
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.expected, TenantPoolReplicas(tc.multiplier, tc.hostReplicas, tc.zeroReplicas))
		})
	}
}

func TestTenantPodState(t *testing.T) {
	now := time.Date(2021, 1, 29, 12, 0, 0, 0, time.UTC)
	defer testutils.HookGlobal(&timeutil.Now, func() time.Time { return now })()

	nonTenantPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{},
	}
	deletingTenant := nonTenantPod
	deletingTenant.DeletionTimestamp = &metav1.Time{Time: now.Add(time.Hour)}
	pendingTenant := nonTenantPod
	pendingTenant.CreationTimestamp = metav1.Time{Time: now.Add(2 * time.Hour)}
	require.NoError(t, SetTenantPodState(&pendingTenant, TenantPodPendingState))
	drainingTenant := nonTenantPod
	require.NoError(t, SetTenantPodState(&drainingTenant, TenantPodDrainingState))
	runningTenant := nonTenantPod
	require.NoError(t, SetTenantPodState(&runningTenant, TenantPodRunningState))

	state, ts := GetTenantPodState(&nonTenantPod)
	require.Equal(t, TenantPodState(""), state)
	require.Equal(t, time.Time{}, ts)
	state, ts = GetTenantPodState(&deletingTenant)
	require.Equal(t, TenantPodDeletingState, state)
	require.Equal(t, deletingTenant.DeletionTimestamp.Time, ts)
	state, ts = GetTenantPodState(&pendingTenant)
	require.Equal(t, TenantPodPendingState, state)
	require.Equal(t, pendingTenant.CreationTimestamp.Time, ts)
	state, ts = GetTenantPodState(&drainingTenant)
	require.Equal(t, TenantPodDrainingState, state)
	require.Equal(t, now, ts)
	state, ts = GetTenantPodState(&runningTenant)
	require.Equal(t, TenantPodRunningState, state)
	require.Equal(t, now, ts)
}

func TestStalePoolSelector(t *testing.T) {
	type test struct {
		name    string
		labels  map[string]string
		matches bool
	}

	tests := []test{
		{
			name: "stale pool correct tenant",
			labels: map[string]string{
				TenantPoolNameLabel: "stale-pool",
				TenantIDLabel:       "1",
			},
			matches: true,
		},
		{
			name: "correct pool wrong tenant",
			labels: map[string]string{
				TenantPoolNameLabel: "correct-pool",
				TenantIDLabel:       "2",
			},
			matches: false,
		},
		{
			name: "correct pool correct tenant",
			labels: map[string]string{
				TenantPoolNameLabel: "correct-pool",
				TenantIDLabel:       "1",
			},
			matches: false,
		},
		{
			name: "stale pool wrong tenant",
			labels: map[string]string{
				TenantPoolNameLabel: "stale-pool",
				TenantIDLabel:       "2",
			},
			matches: false,
		},
		{
			name: "missing pool label correct tenant",
			labels: map[string]string{
				TenantIDLabel: "1",
			},
			matches: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selector := MakeStalePoolSelector(crdbv1beta1.TenantID(1), "correct-pool")
			require.Equal(t, test.matches, selector.Matches(labels.Set(test.labels)))
		})
	}
}

func TestGetTenantPodHashRevision(t *testing.T) {
	for _, tc := range []struct {
		name         string
		pod          *corev1.Pod
		tenant       *crdbv1beta1.CrdbTenant
		expectedHash string
	}{
		{
			name:         "empty",
			pod:          &corev1.Pod{},
			tenant:       &crdbv1beta1.CrdbTenant{},
			expectedHash: "9a2a08575d5e39e1",
		},
		{
			name: "empty",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "foo-bar",
				},
			},
			tenant:       &crdbv1beta1.CrdbTenant{},
			expectedHash: "9a2a08575d5e39e1",
		},
		{
			name: "with OtelCollector",
			pod:  &corev1.Pod{},
			tenant: &crdbv1beta1.CrdbTenant{
				Spec: crdbv1beta1.CrdbTenantSpec{
					OtelCollector: &crdbv1beta1.OtelCollector{
						SecretName:        "test-secret",
						AWSMetricsRoleARN: "test-arn",
					},
				},
			},
			expectedHash: "86cade09355548f4",
		},
		{
			name: "with Fluentbit",
			pod:  &corev1.Pod{},
			tenant: &crdbv1beta1.CrdbTenant{
				Spec: crdbv1beta1.CrdbTenantSpec{
					Fluentbit: &crdbv1beta1.Fluentbit{
						ConfigMapName: "test-configmap",
					},
				},
			},
			expectedHash: "8283d6a0930cb998",
		},
		{
			name: "complete spec",
			pod:  &corev1.Pod{},
			tenant: &crdbv1beta1.CrdbTenant{
				Spec: crdbv1beta1.CrdbTenantSpec{
					OtelCollector: &crdbv1beta1.OtelCollector{
						SecretName:        "test-secret",
						AWSMetricsRoleARN: "test-arn",
					},
					Fluentbit: &crdbv1beta1.Fluentbit{
						ConfigMapName: "test-configmap",
					},
				},
			},
			expectedHash: "fe431e9edebfb2c9",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expectedHash, GetTenantPodHashRevision(tc.pod, tc.tenant))
		})
	}
}

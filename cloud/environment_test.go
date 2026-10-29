package cloud

import (
	"context"
	"fmt"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGetEnvironmentForNode(t *testing.T) {
	testCases := []struct {
		kubeletVersion string
		kernelVersion  string
		osimage        string
		label          string
		provider       Provider
		region         string
	}{
		{
			kubeletVersion: "v1.17.17-gke.4900",
			label:          "topology.kubernetes.io/region",
			provider:       GCP,
			region:         "europe-west1",
		},
		{
			kubeletVersion: "v1.17.17",
			osimage:        "Bottlerocket OS 1.9.2 (aws-k8s-1.21)",
			label:          "failure-domain.beta.kubernetes.io/region",
			provider:       AWS,
			region:         "us-east-1",
		},
		{
			kubeletVersion: "v1.17.17-eks.4900",
			label:          "failure-domain.beta.kubernetes.io/region",
			provider:       AWS,
			region:         "us-east-1",
		},
		{
			kernelVersion: "5.4.0-1105-azure",
			provider:      Azure,
		},
		{
			kubeletVersion: "v1.18.20-k3s1",
			provider:       K3D,
		},
		{
			kernelVersion:  "6.8.0-1017-azure",
			kubeletVersion: "v1.31.2+k3s1",
			provider:       K3D,
		},
		{
			label:    "topology.kubernetes.io/region",
			provider: Unknown,
			region:   "some-region",
		},
		{
			kubeletVersion: "v1.16.15-something",
			label:          "failure-domain.beta.kubernetes.io/region",
			provider:       Unknown,
			region:         "some-region",
		},
	}

	ctx := context.Background()

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("%s=%s", tc.label, tc.region), func(t *testing.T) {
			node := corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "node-name",
				},
				Status: corev1.NodeStatus{
					NodeInfo: corev1.NodeSystemInfo{
						KubeletVersion: tc.kubeletVersion,
						KernelVersion:  tc.kernelVersion,
						OSImage:        tc.osimage,
					},
				},
			}
			if tc.label != "" {
				node.Labels = map[string]string{tc.label: tc.region}
			}

			ctl := kubetest.NewFakeCtl(kubetest.Options{}, &node)
			env, err := GetEnvironmentForNode(ctx, ctl, "node-name")
			require.NoError(t, err)
			require.Equal(t, tc.provider, env.CloudProvider)
			require.Equal(t, tc.region, env.CloudRegion)
		})
	}

	_, err := GetEnvironmentForNode(ctx, kubetest.NewFakeCtl(kubetest.Options{}), "not-found")
	require.EqualError(t, err, "getting node not-found: nodes \"not-found\" not found")
}

func TestGetFirstValue(t *testing.T) {
	for i, tc := range []struct {
		haystack    map[string]string
		needles     []string
		expected    string
		expectedErr string
	}{
		// Empty haystack and needles.
		{
			haystack:    map[string]string{},
			needles:     []string{},
			expectedErr: "value not found",
		},
		// No values exist.
		{
			haystack: map[string]string{
				"0": "a",
				"4": "b",
			},
			needles:     []string{"1", "2", "3"},
			expectedErr: "value not found",
		},
		// Values found, but empty.
		{
			haystack: map[string]string{
				"0": "",
				"1": "",
				"2": "",
			},
			needles:     []string{"0", "1", "2"},
			expectedErr: "value not found",
		},
		// First value found.
		{
			haystack: map[string]string{
				"0": "test-value",
			},
			needles:  []string{"0"},
			expected: "test-value",
		},
		// Subsequent value found.
		{
			haystack: map[string]string{
				"0": "",
				"1": "test-value-1",
			},
			needles:  []string{"9", "0", "1"},
			expected: "test-value-1",
		},
		// All values are found. Only return first.
		{
			haystack: map[string]string{
				"0": "test-value",
				"1": "test-value-1",
				"2": "test-value-2",
			},
			needles:  []string{"0", "1", "2"},
			expected: "test-value",
		},
	} {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			val, err := getFirstValue(tc.haystack, tc.needles)
			if tc.expectedErr == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, tc.expectedErr)
			}
			require.Equal(t, tc.expected, val)
		})
	}
}

func TestMakeLocalityInfoForNode(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		node        *corev1.Node
		labels      map[string]string
		expectedLoc LocalityInfo
	}{
		{
			name: "missing region label",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"topology.kubernetes.io/zone": "us-east-1-d",
					},
				},
			},
			expectedLoc: LocalityInfo{
				Zone: "us-east-1-d",
			},
		},
		{
			name: "missing zone label",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"topology.kubernetes.io/region": "us-east-1",
					},
				},
			},
			expectedLoc: LocalityInfo{
				Region: "us-east-1",
			},
		},
		{
			name: "unknown cloud provider",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"topology.kubernetes.io/region":          "us-east-1",
						"failure-domain.beta.kubernetes.io/zone": "us-east-1-a",
					},
				},
			},
			expectedLoc: LocalityInfo{
				Region: "us-east-1",
				Zone:   "us-east-1-a",
			},
		},
		{
			name: "successful with deprecated labels",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"failure-domain.beta.kubernetes.io/region": "us-east-1",
						"failure-domain.beta.kubernetes.io/zone":   "us-east-1-a",
					},
				},
				Status: corev1.NodeStatus{
					NodeInfo: corev1.NodeSystemInfo{
						KubeletVersion: "eks",
					},
				},
			},
			expectedLoc: LocalityInfo{
				Region: "aws-us-east-1",
				Zone:   "aws-us-east-1-a",
			},
		},
		{
			name: "successful",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"topology.kubernetes.io/region": "us-central1",
						"topology.kubernetes.io/zone":   "us-central1-d",
					},
				},
				Status: corev1.NodeStatus{
					NodeInfo: corev1.NodeSystemInfo{
						KubeletVersion: "gke",
					},
				},
			},
			expectedLoc: LocalityInfo{
				Region: "gcp-us-central1",
				Zone:   "gcp-us-central1-d",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expectedLoc, MakeLocalityInfoForNode(ctx, tc.node))
		})
	}
}

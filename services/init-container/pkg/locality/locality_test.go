package locality

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestGetLocality(t *testing.T) {
	nodes := []kube.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"topology.kubernetes.io/fakeLabel":  "fakeLabelValue",
					"topology.kubernetes.io/fakeLabel2": "fakeLabelValue2",
				},
			},
		},
	}
	t.Run("one label", func(t *testing.T) {
		l := LocalityChecker{
			Clientset:            fake.NewSimpleClientset(kube.ToRuntimeSlice(nodes)...),
			ErrorOnMissingLabels: true,
			Mappings: []LocalityMapping{
				{NodeLabel: "topology.kubernetes.io/fakeLabel", LocalityLabel: "fakeLabel"},
			},
		}
		labels, err := l.getLabels(context.Background())
		require.NoError(t, err)
		require.Equal(t, []string{"fakeLabel=fakeLabelValue"}, labels)
	})

	t.Run("prefix", func(t *testing.T) {
		l := LocalityChecker{
			Clientset:            fake.NewSimpleClientset(kube.ToRuntimeSlice(nodes)...),
			ErrorOnMissingLabels: true,
			Mappings: []LocalityMapping{
				{NodeLabel: "topology.kubernetes.io/fakeLabel", LocalityLabel: "fakeLabel", Prefix: "prefix-"},
			},
		}
		labels, err := l.getLabels(context.Background())
		require.NoError(t, err)
		require.Equal(t, []string{"fakeLabel=prefix-fakeLabelValue"}, labels)
	})

	t.Run("two labels", func(t *testing.T) {
		l := LocalityChecker{
			Clientset:            fake.NewSimpleClientset(kube.ToRuntimeSlice(nodes)...),
			ErrorOnMissingLabels: true,
			Mappings: []LocalityMapping{
				{NodeLabel: "topology.kubernetes.io/fakeLabel", LocalityLabel: "fakeLabel"},
				{NodeLabel: "topology.kubernetes.io/fakeLabel2", LocalityLabel: "fakeLabel2"},
			},
		}
		labels, err := l.getLabels(context.Background())
		require.NoError(t, err)
		require.Equal(t, []string{"fakeLabel=fakeLabelValue", "fakeLabel2=fakeLabelValue2"}, labels)
	})

	t.Run("missing", func(t *testing.T) {
		l := LocalityChecker{
			Clientset:            fake.NewSimpleClientset(kube.ToRuntimeSlice(nodes)...),
			ErrorOnMissingLabels: true,
			Mappings: []LocalityMapping{
				{NodeLabel: "topology.kubernetes.io/missingLabel", LocalityLabel: "missingLabel"},
			},
		}
		_, err := l.getLabels(context.Background())
		require.ErrorContains(t, err, "node label topology.kubernetes.io/missingLabel not found")
	})
}

func TestWriteLabels(t *testing.T) {
	// Create a temporary directory for test.
	tempDir, err := os.MkdirTemp("", "locality-test")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	l := LocalityChecker{
		WritePath: tempDir,
	}

	err = l.writeLabels([]string{"fakeLabel1=fakeLabelValue1", "fakeLabel2=fakeLabelValue2"})
	require.NoError(t, err)

	// Check if the locality file exists and contains the expected content.
	localityFilePath := filepath.Join(tempDir, "locality")
	fileContent, err := os.ReadFile(localityFilePath)
	require.NoError(t, err)

	// Should be in the format "--locality=label1=value1,label2=value2"
	expectedContent := "--locality=fakeLabel1=fakeLabelValue1,fakeLabel2=fakeLabelValue2"
	require.Equal(t, expectedContent, string(fileContent))
}

package v1beta1

import (
	"testing"
	"time"

	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCrdbNodePendingDecommissionAnnotation(t *testing.T) {
	t.Run("MarkForPendingDecommission sets correct annotation", func(t *testing.T) {
		node := &CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-node",
			},
		}

		require.False(t, node.IsMarkedForPendingDecommission())

		node.MarkForPendingDecommission()

		require.True(t, node.IsMarkedForPendingDecommission())
		require.False(t, node.IsDecommissioning())
	})

	t.Run("IsMarkedForPendingDecommission handles nil annotations", func(t *testing.T) {
		node := &CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-node",
				Annotations: nil,
			},
		}

		require.False(t, node.IsMarkedForPendingDecommission())
	})

	t.Run("IsMarkedForPendingDecommission works with existing annotations", func(t *testing.T) {
		node := &CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-node",
				Annotations: map[string]string{
					"some-other-annotation":                    "value",
					CrdbNodeStartPendingDecommissionAnnotation: "true",
				},
			},
		}

		require.True(t, node.IsMarkedForPendingDecommission())
	})
}

func TestCrdbNodeDecommissionAnnotations(t *testing.T) {
	t.Run("MarkForDecommission handles nil annotations", func(t *testing.T) {
		node := &CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-node",
				Annotations: nil,
			},
		}

		node.MarkForDecommission()

		require.True(t, node.IsMarkedForDecommission())
		require.False(t, node.IsDecommissioning())
	})

	t.Run("Decommission handles nil annotations", func(t *testing.T) {
		node := &CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-node",
				Annotations: nil,
			},
		}

		node.Decommission(123)

		require.True(t, node.IsDecommissioning())
		require.Equal(t, "123", node.Annotations[CrdbNodeStartDecommissionAnnotation])

		nodeID, err := node.DecommissionNodeID()
		require.NoError(t, err)
		require.Equal(t, uint(123), nodeID)
	})

	t.Run("IsDecommissioning handles nil annotations", func(t *testing.T) {
		node := &CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-node",
				Annotations: nil,
			},
		}

		require.False(t, node.IsDecommissioning())
		require.False(t, node.IsMarkedForDecommission())
	})

	t.Run("DecommissionNodeID handles nil annotations", func(t *testing.T) {
		node := &CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-node",
				Annotations: nil,
			},
		}

		_, err := node.DecommissionNodeID()
		require.Error(t, err)
		require.Contains(t, err.Error(), "annotation")
	})
}

func TestCrdbNodeExcludedAnnotation(t *testing.T) {
	t.Run("IsExcluded handles nil annotations", func(t *testing.T) {
		node := &CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-node",
				Annotations: nil,
			},
		}

		require.False(t, node.IsExcluded())
	})

	t.Run("IsExcluded works with excluded annotation", func(t *testing.T) {
		node := &CrdbNode{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-node",
				Annotations: map[string]string{
					CrdbNodeExcludeAnnotation: "true",
				},
			},
		}

		require.True(t, node.IsExcluded())
	})
}

func TestCopyAndFilterInternalNodeAnnotations(t *testing.T) {
	t.Run("filters internal annotations correctly", func(t *testing.T) {
		input := map[string]string{
			"external-annotation":                      "value1",
			CrdbNodeDecommissionAnnotation:             "true",
			"another-external":                         "value2",
			CrdbNodeStartDecommissionAnnotation:        "123",
			CrdbNodeStartPendingDecommissionAnnotation: "true",
			"third-external":                           "value3",
		}

		nonInternal, internal := CopyAndFilterInternalNodeAnnotations(input)

		// Verify non-internal annotations
		require.Len(t, nonInternal, 3)
		require.Equal(t, "value1", nonInternal["external-annotation"])
		require.Equal(t, "value2", nonInternal["another-external"])
		require.Equal(t, "value3", nonInternal["third-external"])

		// Verify internal annotations
		require.Len(t, internal, 3)
		require.Equal(t, "true", internal[CrdbNodeDecommissionAnnotation])
		require.Equal(t, "123", internal[CrdbNodeStartDecommissionAnnotation])
		require.Equal(t, "true", internal[CrdbNodeStartPendingDecommissionAnnotation])
	})

	t.Run("handles nil input", func(t *testing.T) {
		nonInternal, internal := CopyAndFilterInternalNodeAnnotations(nil)

		require.Empty(t, nonInternal)
		require.Empty(t, internal)
	})

	t.Run("handles empty input", func(t *testing.T) {
		nonInternal, internal := CopyAndFilterInternalNodeAnnotations(map[string]string{})

		require.Empty(t, nonInternal)
		require.Empty(t, internal)
	})
}

func TestCrdbNodeStatus(t *testing.T) {
	{
		var st CrdbNodeStatus
		_, _, ok := st.GetCondition("SomeCondition")
		require.False(t, ok)
		require.False(t, st.HasCondition("SomeCondition", metav1.ConditionFalse))
		require.False(t, st.HasCondition("SomeCondition", metav1.ConditionTrue))
	}

	{
		var st CrdbNodeStatus
		expectedTs := time.Unix(1, 0)
		st.SetConditionWithTimestamp("SomeCondition", metav1.ConditionFalse, expectedTs)
		{
			cond, ts, ok := st.GetCondition("SomeCondition")
			require.Equal(t, metav1.ConditionFalse, cond)
			require.Equal(t, expectedTs, ts)
			require.True(t, ok)
			require.True(t, st.HasCondition("SomeCondition", metav1.ConditionFalse))
		}

		anotherExpectedTs := time.Unix(2, 0)
		st.SetConditionWithTimestamp("AnotherCondition", metav1.ConditionTrue, anotherExpectedTs)
		{
			cond, ts, ok := st.GetCondition("SomeCondition")
			require.Equal(t, metav1.ConditionFalse, cond)
			require.Equal(t, expectedTs, ts)
			require.True(t, ok)
			require.True(t, st.HasCondition("SomeCondition", metav1.ConditionFalse))
			require.False(t, st.HasCondition("SomeCondition", metav1.ConditionTrue))
		}
		{
			cond, ts, ok := st.GetCondition("AnotherCondition")
			require.Equal(t, anotherExpectedTs, ts)
			require.Equal(t, metav1.ConditionTrue, cond)
			require.True(t, ok)
			require.True(t, st.HasCondition("AnotherCondition", metav1.ConditionTrue))
			require.False(t, st.HasCondition("AnotherCondition", metav1.ConditionFalse))
		}
	}

	{
		var st CrdbNodeStatus
		now := time.Date(2021, 5, 21, 11, 31, 0, 0, time.UTC)
		defer testutils.HookGlobal(&timeutil.Now, func() time.Time { return now })()

		st.SetCondition("SomeCondition", metav1.ConditionTrue)
		require.Equal(t, []NodeCondition{
			{
				Type:               "SomeCondition",
				Status:             metav1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(now),
			},
		}, st.Conditions)

		// Add condition.
		st.SetCondition("AnotherCondition", metav1.ConditionUnknown)
		require.Equal(t, []NodeCondition{
			{
				Type:               "SomeCondition",
				Status:             metav1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(now),
			},
			{
				Type:               "AnotherCondition",
				Status:             metav1.ConditionUnknown,
				LastTransitionTime: metav1.NewTime(now),
			},
		}, st.Conditions)

		// Overwrite existing value.
		st.SetCondition("SomeCondition", metav1.ConditionFalse)
		require.Equal(t, []NodeCondition{
			{
				Type:               "SomeCondition",
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.NewTime(now),
			},
			{
				Type:               "AnotherCondition",
				Status:             metav1.ConditionUnknown,
				LastTransitionTime: metav1.NewTime(now),
			},
		}, st.Conditions)

		// A condition with the same timestamp should be NOT ignored.
		st.SetConditionWithTimestamp("SomeCondition", metav1.ConditionUnknown, now)
		require.Equal(t, []NodeCondition{
			{
				Type:               "SomeCondition",
				Status:             metav1.ConditionUnknown,
				LastTransitionTime: metav1.NewTime(now),
			},
			{
				Type:               "AnotherCondition",
				Status:             metav1.ConditionUnknown,
				LastTransitionTime: metav1.NewTime(now),
			},
		}, st.Conditions)
	}
}

func TestCrdbNodeIsReady(t *testing.T) {
	var node CrdbNode
	node.Status.SetCondition(PodNeedsUpdate, metav1.ConditionFalse)
	node.Status.SetCondition(PodRunning, metav1.ConditionTrue)
	node.Status.SetCondition(PodReady, metav1.ConditionTrue)

	// ObservedGeneration == Generation
	require.NoError(t, node.IsReady())

	// ObservedGeneration > Generation
	node.Status.ObservedGeneration = 5
	require.NoError(t, node.IsReady())

	// ObservedGeneration < Generation
	node.Generation = 10
	require.EqualError(t, node.IsReady(), "observed *v1beta1.CrdbNode generation 5 is lower than expected generation 10")

	node.Generation = 5
	node.Status.SetCondition(PodNeedsUpdate, metav1.ConditionTrue)
	node.Status.SetCondition(PodRunning, metav1.ConditionFalse)
	node.Status.SetCondition(PodReady, metav1.ConditionFalse)
	require.EqualError(t, node.IsReady(), "*v1beta1.CrdbNode's pod has not been updated yet")

	node.Status.SetCondition(PodNeedsUpdate, metav1.ConditionFalse)
	node.Status.SetCondition(PodRunning, metav1.ConditionFalse)
	node.Status.SetCondition(PodReady, metav1.ConditionTrue)
	require.EqualError(t, node.IsReady(), "*v1beta1.CrdbNode's pod is not running yet")

	node.Status.SetCondition(PodNeedsUpdate, metav1.ConditionFalse)
	node.Status.SetCondition(PodRunning, metav1.ConditionTrue)
	node.Status.SetCondition(PodReady, metav1.ConditionFalse)
	require.EqualError(t, node.IsReady(), "*v1beta1.CrdbNode's pod is not ready yet")
}

func TestCrdbVersion(t *testing.T) {
	node := CrdbNode{}

	t.Run("no error", func(t *testing.T) {
		node.Spec.Image = "us-docker.pkg.dev/cockroach-cloud-images/cockroachdb/cockroach:v21.2.4"
		v, err := node.CrdbVersion()
		require.NoError(t, err)
		require.Equal(t, "v21.2.4", v.String())
	})

	t.Run("err", func(t *testing.T) {
		node.Spec.Image = "us-docker.pkg.dev/cockroach-cloud-images/cockroachdb/cockroach:y8987"
		_, err := node.CrdbVersion()
		require.Error(t, err)
	})
}

func TestExternalCerts(t *testing.T) {
	t.Run("no ca certs", func(t *testing.T) {
		certs := ExternalCertificates{}
		err := certs.Validate()
		require.Error(t, err)
	})

	t.Run("consolidated ca cert", func(t *testing.T) {
		certs := ExternalCertificates{
			CAConfigMapName: "ca-config-map-name",
		}
		err := certs.Validate()
		require.NoError(t, err)
	})

	t.Run("split ca cert", func(t *testing.T) {
		certs := ExternalCertificates{
			NodeCAConfigMapName:   "node-ca-config-map-name",
			ClientCAConfigMapName: "client-ca-config-map-name",
		}
		err := certs.Validate()
		require.NoError(t, err)
	})

	t.Run("consolidated and split ca cert", func(t *testing.T) {
		certs := ExternalCertificates{
			CAConfigMapName:       "ca-config-map-name",
			NodeCAConfigMapName:   "node-ca-config-map-name",
			ClientCAConfigMapName: "client-ca-config-map-name",
		}
		err := certs.Validate()
		require.Error(t, err)
	})

	t.Run("partial split ca cert", func(t *testing.T) {
		certs := ExternalCertificates{
			NodeCAConfigMapName: "node-ca-config-map-name",
		}
		err := certs.Validate()
		require.Error(t, err)
	})
}

func TestCrdbPorts(t *testing.T) {
	t.Run("default ports", func(t *testing.T) {
		node := CrdbNode{}
		require.Equal(t, int32(26257), node.Spec.GetSQLPort())
		require.Equal(t, int32(26258), node.Spec.GetGRPCPort())
		require.Equal(t, int32(8080), node.Spec.GetHTTPPort())
	})

	t.Run("custom ports", func(t *testing.T) {
		node := CrdbNode{
			Spec: CrdbNodeSpec{
				SQLPort:  new(int32(36257)),
				GRPCPort: new(int32(36258)),
				HTTPPort: new(int32(8081)),
			},
		}
		require.Equal(t, int32(36257), node.Spec.GetSQLPort())
		require.Equal(t, int32(36258), node.Spec.GetGRPCPort())
		require.Equal(t, int32(8081), node.Spec.GetHTTPPort())
	})
}

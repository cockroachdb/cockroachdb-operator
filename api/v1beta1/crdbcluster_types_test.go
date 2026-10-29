package v1beta1

import (
	"testing"
	"time"

	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTenantHostTenantPool(t *testing.T) {
	th := TenantHost{
		TenantPools: []TenantPool{
			{Name: "default"},
		},
	}

	pool, ok := th.TenantPool("default")
	require.True(t, ok)
	require.Equal(t, "default", pool.Name)

	pool, ok = th.TenantPool("anythingelse")
	require.False(t, ok)
	require.Nil(t, pool)
}

func TestClusterGetAndHasCondition(t *testing.T) {
	var st CrdbClusterStatus
	_, _, ok := st.GetCondition("SomeCondition")
	require.False(t, ok)
	require.False(t, st.HasCondition("SomeCondition", metav1.ConditionFalse))
	require.False(t, st.HasCondition("SomeCondition", metav1.ConditionTrue))

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

func TestClusterSetCondition(t *testing.T) {
	now := time.Date(2021, 5, 21, 11, 31, 0, 0, time.UTC)
	defer testutils.HookGlobal(&timeutil.Now, func() time.Time { return now })()

	var st CrdbClusterStatus
	st.SetCondition("SomeCondition", metav1.ConditionTrue)
	require.Equal(t, []ClusterCondition{
		{
			Type:               "SomeCondition",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.NewTime(now),
		},
	}, st.Conditions)

	// Add condition.
	st.SetCondition("AnotherCondition", metav1.ConditionUnknown)
	require.Equal(t, []ClusterCondition{
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
	require.Equal(t, []ClusterCondition{
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

	// A condition with a timestamp in the past should be ignored.
	st.SetConditionWithTimestamp("SomeCondition", metav1.ConditionUnknown, now.Add(-time.Second))
	require.Equal(t, []ClusterCondition{
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
	require.Equal(t, []ClusterCondition{
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

func TestClusterHasAction(t *testing.T) {
	var st CrdbClusterStatus
	require.False(t, st.HasAction("SomeAction", Unknown))
	st.SetAction("SomeAction", Unknown)
	require.True(t, st.HasAction("SomeAction", Unknown))
	st.SetAction("SomeAction", Starting)
	require.True(t, st.HasAction("SomeAction", Starting))
	require.False(t, st.HasAction("AnotherAction", Failed))
}

func TestClusterActionPresent(t *testing.T) {
	var st CrdbClusterStatus
	require.False(t, st.ActionPresent("SomeAction"))
	st.SetAction("SomeAction", Unknown)
	require.True(t, st.ActionPresent("SomeAction"))
}

func TestClusterSetAction(t *testing.T) {
	now := time.Date(2021, 5, 21, 11, 31, 0, 0, time.UTC)
	defer testutils.HookGlobal(&timeutil.Now, func() time.Time { return now })()

	var st CrdbClusterStatus
	st.SetAction("SomeAction", Starting)
	require.Equal(t, []ClusterAction{
		{
			Type:               "SomeAction",
			Status:             Starting,
			LastTransitionTime: metav1.NewTime(now),
		},
	}, st.Actions)

	// Add action.
	st.SetAction("AnotherAction", Unknown)
	require.Equal(t, []ClusterAction{
		{
			Type:               "SomeAction",
			Status:             Starting,
			LastTransitionTime: metav1.NewTime(now),
		},
		{
			Type:               "AnotherAction",
			Status:             Unknown,
			LastTransitionTime: metav1.NewTime(now),
		},
	}, st.Actions)

	// Overwrite existing value.
	st.SetAction("SomeAction", Failed)
	require.Equal(t, []ClusterAction{
		{
			Type:               "SomeAction",
			Status:             Failed,
			LastTransitionTime: metav1.NewTime(now),
		},
		{
			Type:               "AnotherAction",
			Status:             Unknown,
			LastTransitionTime: metav1.NewTime(now),
		},
	}, st.Actions)
}

func TestClusterSpecHasFeature(t *testing.T) {
	spec := &CrdbClusterSpec{
		Features: []ClusterFeature{
			ClusterDisableGCPLiveMigrations,
			ClusterSqlProxyDisableConnectionRebalancing,
		},
	}

	require.True(t, spec.FeatureEnabled(ClusterDisableGCPLiveMigrations))
	require.True(t, spec.FeatureEnabled(ClusterSqlProxyDisableConnectionRebalancing))
	require.False(t, spec.FeatureEnabled("UnknownFeature"))
}

func TestClusterIsInitialized(t *testing.T) {
	cluster := &CrdbCluster{}
	require.False(t, cluster.IsInitialized())

	cluster.Status.SetCondition(ClusterInitialized, metav1.ConditionTrue)
	require.True(t, cluster.IsInitialized())
}

func TestClusterIsManagedMode(t *testing.T) {
	tests := map[ReconciliationMode]bool{
		CreateOnly:  false,
		MutableOnly: true,
		Disabled:    false,
	}

	for mode, managed := range tests {
		cluster := &CrdbCluster{
			Spec: CrdbClusterSpec{Mode: &mode},
		}

		require.Equal(t, managed, cluster.IsManagedMode())
	}
}

func TestClusterIsReady(t *testing.T) {
	cluster := &CrdbCluster{}
	require.EqualError(t, cluster.IsReady(), "cluster is not initialized")

	cluster.Status.SetConditionWithMessage(
		ClusterInitialized,
		metav1.ConditionFalse,
		"InitializationTimedOut",
		"cockroach init on CrdbNode/node-b exceeded 5m0s; see operator logs",
	)
	require.EqualError(t, cluster.IsReady(),
		"cluster is not initialized: InitializationTimedOut: cockroach init on CrdbNode/node-b exceeded 5m0s; see operator logs")

	cluster.Status = CrdbClusterStatus{
		Conditions: []ClusterCondition{
			{Type: ClusterInitialized, Status: metav1.ConditionTrue},
		},
	}
	require.NoError(t, cluster.IsReady())

	cluster.Generation = 1
	require.EqualError(t, cluster.IsReady(), "observed CrdbCluster generation 0 is lower than expected generation 1")
}

func TestClusterIsQuiescent(t *testing.T) {
	require.NoError(t, (&CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Generation: 2,
		},
		Status: CrdbClusterStatus{
			ReconciledByBetaController: true,
			ObservedGeneration:         2,
			Reconciled:                 true,
			Conditions: []ClusterCondition{
				{Type: ClusterInitialized, Status: metav1.ConditionTrue},
			},
		},
	}).IsQuiescent())

	require.EqualError(
		t,
		(&CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{
				Generation: 2,
			},
			Status: CrdbClusterStatus{
				ReconciledByBetaController: true,
				ObservedGeneration:         2,
				Conditions: []ClusterCondition{
					{Type: ClusterInitialized, Status: metav1.ConditionTrue},
				},
				Actions: []ClusterAction{
					{Type: Upgrade, Status: Starting},
				},
			},
		}).IsQuiescent(),
		"cluster has a pending Upgrade action with status Starting",
	)
}

func TestEncryptionPlatform(t *testing.T) {
	require.Equal(t, EncryptionPlatformAwsKms, ParseEncryptionPlatform(("AWS_KMS")))
	require.Equal(t, EncryptionPlatformGcpCloudKms, ParseEncryptionPlatform(("GCP_CLOUD_KMS")))
	require.Equal(t, EncryptionPlatformAzureKeyVault, ParseEncryptionPlatform(("AZURE_KEY_VAULT")))
	require.Equal(t, EncryptionPlatformUnknown, ParseEncryptionPlatform(("")))
}

// TestDomain covers CrdbCluster.Domain's preference order: current region's
// Domain first, then spec.template.spec.domain, then empty.
func TestDomain(t *testing.T) {
	const (
		regionDomain   = "cluster1.local"
		templateDomain = "cluster2.local"
		thisRegion     = "us-east1"
		otherRegion    = "us-west1"
	)

	makeCluster := func(regionDom, templateDom, statusRegion string, includeRegion bool) *CrdbCluster {
		c := &CrdbCluster{}
		c.Spec.Template.Spec.Domain = templateDom
		if includeRegion {
			c.Spec.Regions = []CrdbClusterRegion{
				{
					Code:   thisRegion,
					Nodes:  3,
					Domain: regionDom,
				},
			}
		}
		c.Status.Region = statusRegion
		return c
	}

	testCases := []struct {
		name    string
		cluster *CrdbCluster
		want    string
	}{
		{
			name:    "region domain set, template domain empty -> region wins",
			cluster: makeCluster(regionDomain, "", thisRegion, true),
			want:    regionDomain,
		},
		{
			name:    "region domain empty, template domain set -> template fallback",
			cluster: makeCluster("", templateDomain, thisRegion, true),
			want:    templateDomain,
		},
		{
			name:    "both set -> region wins",
			cluster: makeCluster(regionDomain, templateDomain, thisRegion, true),
			want:    regionDomain,
		},
		{
			name:    "status region does not match any regions[].code -> template fallback",
			cluster: makeCluster(regionDomain, templateDomain, otherRegion, true),
			want:    templateDomain,
		},
		{
			name:    "no regions at all -> template fallback",
			cluster: makeCluster("", templateDomain, thisRegion, false),
			want:    templateDomain,
		},
		{
			name:    "both empty -> empty (JoinFQDN handles final fallback)",
			cluster: makeCluster("", "", thisRegion, true),
			want:    "",
		},
		{
			name:    "status region empty -> template fallback",
			cluster: makeCluster(regionDomain, "", "", true),
			want:    "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cluster.Domain()
			require.Equal(t, tc.want, got)
		})
	}
}

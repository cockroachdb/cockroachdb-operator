package v1beta1

import (
	"testing"
	"time"

	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTenantHasCondition(t *testing.T) {
	var st CrdbTenantStatus
	require.False(t, st.HasCondition(TenantInitialized, metav1.ConditionFalse))
	require.False(t, st.HasCondition(TenantInitialized, metav1.ConditionTrue))
	st.SetCondition(TenantInitialized, metav1.ConditionFalse)
	require.True(t, st.HasCondition(TenantInitialized, metav1.ConditionFalse))
	st.SetCondition("AnotherCondition", metav1.ConditionTrue)
	require.True(t, st.HasCondition(TenantInitialized, metav1.ConditionFalse))
	require.True(t, st.HasCondition("AnotherCondition", metav1.ConditionTrue))
	require.False(t, st.HasCondition("AnotherCondition", metav1.ConditionFalse))
}

func TestTenantSetCondition(t *testing.T) {
	now := time.Date(2021, 5, 21, 11, 31, 0, 0, time.UTC)
	defer testutils.HookGlobal(&timeutil.Now, func() time.Time { return now })()

	var st CrdbTenantStatus
	st.SetCondition(TenantInitialized, metav1.ConditionTrue)
	require.Equal(t, []TenantCondition{
		{
			Type:               TenantInitialized,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.NewTime(now),
		},
	}, st.Conditions)

	// Add condition.
	st.SetCondition("AnotherCondition", metav1.ConditionUnknown)
	require.Equal(t, []TenantCondition{
		{
			Type:               TenantInitialized,
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
	st.SetCondition(TenantInitialized, metav1.ConditionFalse)
	require.Equal(t, []TenantCondition{
		{
			Type:               TenantInitialized,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.NewTime(now),
		},
		{
			Type:               "AnotherCondition",
			Status:             metav1.ConditionUnknown,
			LastTransitionTime: metav1.NewTime(now),
		},
	}, st.Conditions)
}

func TestCrdbTenantIsReady(t *testing.T) {
	tenant := &CrdbTenant{}
	require.EqualError(t, tenant.IsReady(), "tenant is not initialized")
	tenant.Status = CrdbTenantStatus{
		Conditions: []TenantCondition{
			{Type: TenantInitialized, Status: metav1.ConditionTrue},
		},
	}
	require.NoError(t, tenant.IsReady())
	tenant.Spec.Pods = 3
	tenant.Status.Pods = 3
	require.NoError(t, tenant.IsReady())
	tenant.Status.Pods = 2
	require.EqualError(t, tenant.IsReady(), "only 2/3 pods are ready")
}

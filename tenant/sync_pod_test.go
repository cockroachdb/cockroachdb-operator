package tenant

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud/cloudmock"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/timeutil"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

func TestTenantPodSyncer_Start(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	defer testutils.HookGlobal(&podSyncPeriod, 300*time.Millisecond)()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	credProvider := cloudmock.NewMockCredentialProvider(ctrl)
	credProvider.EXPECT().GC().Return().AnyTimes()
	credProvider.EXPECT().GetAWSCreds(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(makeAWSCred(t, timeutil.Now()), nil).AnyTimes()
	credProvider.EXPECT().GetGCPCreds(gomock.Any(), gomock.Any()).
		Return(makeGCPCred(t, timeutil.Now()), nil).AnyTimes()

	tenant := makeCrdbTenant(t)
	pod := makePod(t, tenant)
	ctl := kubetest.NewFakeCtl(kubetest.Options{}, tenant, pod)

	s := NewTenantPodSyncer(ctx, ctl, credProvider, nil /* nodeLocalityReader */)

	syncCount := 0
	defer testutils.HookGlobal(&sendStartRequest, func(
		inCtx context.Context,
		_ *kube.Ctl,
		_ *corev1.Pod,
		_ *crdbv1beta1.CrdbTenant,
		_ map[string][]byte,
		_ cloud.NodeLocalityReaderFunc,
	) error {
		syncCount++
		return nil
	})()

	require.EqualError(t, s.Start(ctx), context.DeadlineExceeded.Error())

	// At a timeout of 2 seconds, we should be able to run 6 times + 1 initial.
	// Here, only check that it's more than half.
	require.Greater(t, syncCount, 3)
}

// This test only checks that we're using the right context, so the result of
// Sync does not matter.
func TestTenantPodSyncer_Sync(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tenant := makeCrdbTenant(t)
	pod := makePod(t, tenant)

	// As long as EXPECT() matches on the ctx, we are done.
	ctx := context.Background()
	credProvider := cloudmock.NewMockCredentialProvider(ctrl)
	credProvider.EXPECT().GetAWSCreds(
		ctx,
		tenant.Spec.OtelCollector.AWSMetricsRoleARN,
		tenant.Spec.OtelCollector.AWSExternalID,
	).Return(nil, errors.New("test error"))

	s := NewTenantPodSyncer(ctx, nil /* ctl */, credProvider, nil /* nodeLocalityReader */)

	err := s.Sync(tenant, pod)
	require.EqualError(t, err, "writing metrics credentials: test error")
}

func TestTenantPodSyncer_syncWithContext(t *testing.T) {
	ctx := context.Background()

	t.Run("error on PENDING pod", func(t *testing.T) {
		s := NewTenantPodSyncer(ctx, nil /* ctl */, nil /* credProvider */, nil /* nodeLocalityReader */)

		tenant := makeCrdbTenant(t)
		pod := makePod(t, tenant)
		pod.Labels[manifests.TenantPodStateLabel] = string(manifests.TenantPodPendingState)

		err := s.syncWithContext(ctx, tenant, pod)
		require.EqualError(t, err, "pods in the PENDING state cannot be synced")
	})

	t.Run("error on unstamped pod", func(t *testing.T) {
		s := NewTenantPodSyncer(ctx, nil /* ctl */, nil /* credProvider */, nil /* nodeLocalityReader */)

		tenant := makeCrdbTenant(t)
		pod := makePod(t, tenant)
		delete(pod.Labels, manifests.TenantIDLabel)

		err := s.syncWithContext(ctx, tenant, pod)
		require.EqualError(t, err, "pod test-pod is not marked with tenant-id=42")
	})

	t.Run("error on tenant-id label mismatch", func(t *testing.T) {
		s := NewTenantPodSyncer(ctx, nil /* ctl */, nil /* credProvider */, nil /* nodeLocalityReader */)

		tenant := makeCrdbTenant(t)
		pod := makePod(t, tenant)
		pod.Labels[manifests.TenantIDLabel] = "random-id"

		err := s.syncWithContext(ctx, tenant, pod)
		require.EqualError(t, err, "pod test-pod is not marked with tenant-id=42")
	})

	t.Run("stale pod", func(t *testing.T) {
		s := NewTenantPodSyncer(ctx, nil /* ctl */, nil /* credProvider */, nil /* nodeLocalityReader */)

		tenant := makeCrdbTenant(t)
		pod := makePod(t, tenant)
		pod.Annotations[manifests.TenantPodRevisionAnnotation] = "stale"

		err := s.syncWithContext(ctx, tenant, pod)
		require.NoError(t, err)

		snapshot := s.podRenewalsSnapshot()
		require.Len(t, snapshot, 1)
		require.Equal(t, map[types.UID]time.Time{
			pod.UID: aLongTimeInTheFuture,
		}, snapshot)
	})

	t.Run("sync unsupported", func(t *testing.T) {
		s := NewTenantPodSyncer(ctx, nil /* ctl */, nil /* credProvider */, nil /* nodeLocalityReader */)

		tenant := makeCrdbTenant(t)
		tenant.Spec.OtelCollector = nil
		pod := makePod(t, tenant)

		err := s.syncWithContext(ctx, tenant, pod)
		require.NoError(t, err)

		snapshot := s.podRenewalsSnapshot()
		require.Len(t, snapshot, 1)
		require.Equal(t, map[types.UID]time.Time{
			pod.UID: aLongTimeInTheFuture,
		}, snapshot)
	})

	t.Run("error on retrieving otel collector metrics credentials", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		tenant := makeCrdbTenant(t)
		pod := makePod(t, tenant)

		credProvider := cloudmock.NewMockCredentialProvider(ctrl)
		credProvider.EXPECT().GetAWSCreds(
			ctx,
			tenant.Spec.OtelCollector.AWSMetricsRoleARN,
			tenant.Spec.OtelCollector.AWSExternalID,
		).Return(nil, errors.New("test error"))

		s := NewTenantPodSyncer(ctx, nil /* ctl */, credProvider, nil /* nodeLocalityReader */)

		err := s.syncWithContext(ctx, tenant, pod)
		require.EqualError(t, err, "writing metrics credentials: test error")
	})

	t.Run("error on retrieving otel collector (aws) logs credentials", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		tenant := makeCrdbTenant(t)
		pod := makePod(t, tenant)

		credProvider := cloudmock.NewMockCredentialProvider(ctrl)
		// metrics credentials are retrieved successfully.
		cred := makeAWSCred(t, aLongTimeInTheFuture.Add(100*time.Minute))
		credProvider.EXPECT().GetAWSCreds(
			ctx,
			tenant.Spec.OtelCollector.AWSMetricsRoleARN,
			tenant.Spec.OtelCollector.AWSExternalID,
		).Return(cred, nil)
		// log credentials fail.
		credProvider.EXPECT().GetAWSCreds(
			ctx,
			tenant.Spec.OtelCollector.AWSLogsRoleARN,
			tenant.Spec.OtelCollector.AWSExternalID,
		).Return(nil, errors.New("test error"))

		s := NewTenantPodSyncer(ctx, nil /* ctl */, credProvider, nil /* nodeLocalityReader */)

		err := s.syncWithContext(ctx, tenant, pod)
		require.EqualError(t, err, "writing logs credentials: test error")
	})

	t.Run("error on retrieving otel collector (gcp) logs credentials", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		tenant := makeCrdbTenant(t)
		pod := makePod(t, tenant)

		credProvider := cloudmock.NewMockCredentialProvider(ctrl)
		// metrics credentials are retrieved successfully.
		cred := makeAWSCred(t, aLongTimeInTheFuture.Add(100*time.Minute))
		credProvider.EXPECT().GetAWSCreds(
			ctx,
			tenant.Spec.OtelCollector.AWSMetricsRoleARN,
			tenant.Spec.OtelCollector.AWSExternalID,
		).Return(cred, nil)
		// aws log credentials are retrieved successfully.
		credProvider.EXPECT().GetAWSCreds(
			ctx,
			tenant.Spec.OtelCollector.AWSLogsRoleARN,
			tenant.Spec.OtelCollector.AWSExternalID,
		).Return(cred, nil)
		// gcp log credentials error.
		credProvider.EXPECT().GetGCPCreds(
			ctx,
			tenant.Spec.OtelCollector.GCPLogsServiceAccount,
		).Return(nil, errors.New("test error"))

		s := NewTenantPodSyncer(ctx, nil /* ctl */, credProvider, nil /* nodeLocalityReader */)

		err := s.syncWithContext(ctx, tenant, pod)
		require.EqualError(t, err, "getting GCP creds: test error")
	})

	t.Run("error on start request", func(t *testing.T) {
		defer testutils.HookGlobal(&sendStartRequest, func(
			ctx context.Context,
			ctl *kube.Ctl,
			pod *corev1.Pod,
			tenant *crdbv1beta1.CrdbTenant,
			credentials map[string][]byte,
			nodeLocalityReader cloud.NodeLocalityReaderFunc,
		) error {
			return errors.New("start error")
		})()

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		tenant := makeCrdbTenant(t)
		pod := makePod(t, tenant)

		cred := makeAWSCred(t, timeutil.Now())
		credProvider := cloudmock.NewMockCredentialProvider(ctrl)
		credProvider.EXPECT().GetAWSCreds(
			ctx,
			tenant.Spec.OtelCollector.AWSMetricsRoleARN,
			tenant.Spec.OtelCollector.AWSExternalID,
		).Return(cred, nil)
		credProvider.EXPECT().GetAWSCreds(
			ctx,
			tenant.Spec.OtelCollector.AWSLogsRoleARN,
			tenant.Spec.OtelCollector.AWSExternalID,
		).Return(cred, nil)
		gcpCred := makeGCPCred(t, timeutil.Now())
		credProvider.EXPECT().GetGCPCreds(
			ctx,
			tenant.Spec.OtelCollector.GCPLogsServiceAccount,
		).Return(gcpCred, nil)

		s := NewTenantPodSyncer(ctx, nil /* ctl */, credProvider, nil /* nodeLocalityReader */)

		err := s.syncWithContext(ctx, tenant, pod)
		require.EqualError(t, err, "sending update request: start error")
	})

	// minRenewsAt should not be updated.
	t.Run("successful with longer expiration", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		tenant := makeCrdbTenant(t)
		pod := makePod(t, tenant)

		ctl := kubetest.NewFakeCtl(kubetest.Options{})

		// Use a long expiration.
		cred := makeAWSCred(t, aLongTimeInTheFuture.Add(100*time.Minute))
		credProvider := cloudmock.NewMockCredentialProvider(ctrl)
		credProvider.EXPECT().GetAWSCreds(
			ctx,
			tenant.Spec.OtelCollector.AWSMetricsRoleARN,
			tenant.Spec.OtelCollector.AWSExternalID,
		).Return(cred, nil)
		credProvider.EXPECT().GetAWSCreds(
			ctx,
			tenant.Spec.OtelCollector.AWSLogsRoleARN,
			tenant.Spec.OtelCollector.AWSExternalID,
		).Return(cred, nil)
		gcpCred := makeGCPCred(t, aLongTimeInTheFuture.Add(100*time.Minute))
		credProvider.EXPECT().GetGCPCreds(
			ctx,
			tenant.Spec.OtelCollector.GCPLogsServiceAccount,
		).Return(gcpCred, nil)

		defer testutils.HookGlobal(&sendStartRequest, func(
			inCtx context.Context,
			inCtl *kube.Ctl,
			inPod *corev1.Pod,
			inTenant *crdbv1beta1.CrdbTenant,
			inCredentials map[string][]byte,
			inNodeLocalityReader cloud.NodeLocalityReaderFunc,
		) error {
			// Does not matter as we tested this in the successful case.
			return nil
		})()

		s := NewTenantPodSyncer(ctx, ctl, credProvider, nil /* nodeLocalityReader */)

		err := s.syncWithContext(ctx, tenant, pod)
		require.NoError(t, err)

		snapshot := s.podRenewalsSnapshot()
		require.Len(t, snapshot, 1)
		require.Equal(t, map[types.UID]time.Time{
			pod.UID: aLongTimeInTheFuture,
		}, snapshot)
	})

	t.Run("successful", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		tenant := makeCrdbTenant(t)
		pod := makePod(t, tenant)

		ctl := kubetest.NewFakeCtl(kubetest.Options{})

		// Credentials for logs will have an earlier expiry, as a result we should
		// see that credential's expiry for the pod.
		cred := makeAWSCred(t, timeutil.Now())
		credProvider := cloudmock.NewMockCredentialProvider(ctrl)
		credProvider.EXPECT().GetAWSCreds(
			ctx,
			tenant.Spec.OtelCollector.AWSMetricsRoleARN,
			tenant.Spec.OtelCollector.AWSExternalID,
		).Return(cred, nil)
		credEarlierExpiry := makeAWSCred(t, timeutil.Now().Add(-2*time.Minute))
		credProvider.EXPECT().GetAWSCreds(
			ctx,
			tenant.Spec.OtelCollector.AWSLogsRoleARN,
			tenant.Spec.OtelCollector.AWSExternalID,
		).Return(credEarlierExpiry, nil)
		gcpCred := makeGCPCred(t, time.Now())
		credProvider.EXPECT().GetGCPCreds(
			ctx,
			tenant.Spec.OtelCollector.GCPLogsServiceAccount,
		).Return(gcpCred, nil)

		startCalled := false
		defer testutils.HookGlobal(&sendStartRequest, func(
			inCtx context.Context,
			inCtl *kube.Ctl,
			inPod *corev1.Pod,
			inTenant *crdbv1beta1.CrdbTenant,
			inCredentials map[string][]byte,
			inNodeLocalityReader cloud.NodeLocalityReaderFunc,
		) error {
			require.Equal(t, ctx, inCtx)
			require.Equal(t, ctl, inCtl)
			require.Equal(t, pod, inPod)
			require.Equal(t, tenant, inTenant)
			require.NotNil(t, inNodeLocalityReader)

			info, err := inNodeLocalityReader(ctx, "foo")
			require.NoError(t, err)
			require.Equal(t, cloud.LocalityInfo{
				Region: "foo-region-1",
				Zone:   "foo-zone-1",
			}, info)

			require.Len(t, inCredentials, 3)
			c, ok := inCredentials[tenant.Spec.OtelCollector.AWSMetricsRoleARN]
			require.True(t, ok)
			c2, ok := inCredentials[tenant.Spec.OtelCollector.AWSLogsRoleARN]
			require.True(t, ok)
			c3, ok := inCredentials[tenant.Spec.OtelCollector.GCPLogsServiceAccount]
			require.True(t, ok)

			require.Equal(t, []byte(cred.CredentialsFile()), c)
			require.Equal(t, []byte(credEarlierExpiry.CredentialsFile()), c2)
			marshalledCred, err := json.Marshal(gcpCred.CredentialsFile())
			require.NoError(t, err)
			require.Equal(t, marshalledCred, c3)

			startCalled = true
			return nil
		})()

		s := NewTenantPodSyncer(ctx, ctl, credProvider, testReadNodeLocality)

		err := s.syncWithContext(ctx, tenant, pod)
		require.NoError(t, err)
		require.True(t, startCalled)

		snapshot := s.podRenewalsSnapshot()
		require.Len(t, snapshot, 1)
		require.Equal(t, map[types.UID]time.Time{
			pod.UID: credEarlierExpiry.ExpiresAt(),
		}, snapshot)
	})
}

func TestTenantPodSyncer_syncAll(t *testing.T) {
	ctx := context.Background()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	makeTenantWithID := func(id int) *crdbv1beta1.CrdbTenant {
		tenant := makeCrdbTenant(t)
		tenant.Name = fmt.Sprintf("tenant-%d", id)
		tenant.Spec.TenantID = crdbv1beta1.TenantID(id)
		return tenant
	}

	makePodWithName := func(name string, tenant *crdbv1beta1.CrdbTenant) *corev1.Pod {
		pod := makePod(t, tenant)
		pod.Name = name
		pod.UID = types.UID(fmt.Sprintf("%s-uid", name))
		return pod
	}

	deletedTenant := makeTenantWithID(55)
	tenant42 := makeTenantWithID(42)
	tenant99 := makeTenantWithID(99)

	objects := []kube.Object{
		// CrdbTenant objects. deletedTenant isn't included intentionally.
		tenant42,
		tenant99,
		// Running pods.
		makePodWithName("deleted-1", deletedTenant),
		makePodWithName("deleted-2", deletedTenant),
		makePodWithName("deleted-3", deletedTenant),
		makePodWithName("99-pod-3", tenant99),
		makePodWithName("99-pod-2", tenant99),
		makePodWithName("99-pod-1", tenant99),
		makePodWithName("42-pod-3", tenant42),
		makePodWithName("42-pod-2", tenant42),
		makePodWithName("42-pod-1", tenant42),
	}
	ctl := kubetest.NewFakeCtl(kubetest.Options{}, objects...)

	testTime := time.Date(2023, 12, 31, 12, 0, 0, 0, time.UTC)
	defer testutils.HookGlobal(&timeutil.Now, func() time.Time {
		return testTime
	})()

	credProvider := cloudmock.NewMockCredentialProvider(ctrl)
	credProvider.EXPECT().GC().Return()
	credProvider.EXPECT().GetAWSCreds(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(makeAWSCred(t, timeutil.Now()), nil).Times(8)
	credProvider.EXPECT().GetGCPCreds(gomock.Any(), gomock.Any()).
		Return(makeGCPCred(t, timeutil.Now()), nil).Times(4)

	s := NewTenantPodSyncer(ctx, ctl, credProvider, nil /* nodeLocalityReader */)

	var calls map[string][]string
	defer testutils.HookGlobal(&sendStartRequest, func(
		inCtx context.Context,
		inCtl *kube.Ctl,
		inPod *corev1.Pod,
		inTenant *crdbv1beta1.CrdbTenant,
		inCredentials map[string][]byte,
		inNodeLocalityReader cloud.NodeLocalityReaderFunc,
	) error {
		require.Equal(t, ctx, inCtx)
		require.Equal(t, ctl, inCtl)
		require.NotNil(t, inPod)
		require.NotNil(t, inTenant)
		require.Len(t, inCredentials, 3)

		// Simulate the case where a pod was stamped after we list. This
		// entry shouldn't be deleted after GC runs.
		if calls == nil {
			calls = make(map[string][]string)
			s.setPodRenewal(types.UID("some-other-pod-uid"), aLongTimeInTheFuture)
		}
		calls[inTenant.Name] = append(calls[inTenant.Name], inPod.Name)
		return nil
	})()

	// Add a few old, expired pods.
	s.setPodRenewal("old-pod-1-uid", aLongTimeInTheFuture)
	s.setPodRenewal("old-pod-2-uid", testTime.Add(-10*time.Minute))

	// Make pod-2 for t42 and pod-3 for t99 as haven't expired.
	s.setPodRenewal("42-pod-2-uid", aLongTimeInTheFuture)
	s.setPodRenewal("99-pod-3-uid", aLongTimeInTheFuture)

	require.NoError(t, s.syncAll(ctx))
	require.Equal(t, map[string][]string{
		"tenant-42": {"42-pod-1", "42-pod-3"},
		"tenant-99": {"99-pod-1", "99-pod-2"},
	}, calls)

	snapshot := s.podRenewalsSnapshot()
	require.Len(t, snapshot, 7)
	require.Equal(t, map[types.UID]time.Time{
		types.UID("42-pod-1-uid"):       testTime,
		types.UID("42-pod-2-uid"):       aLongTimeInTheFuture,
		types.UID("42-pod-3-uid"):       testTime,
		types.UID("99-pod-1-uid"):       testTime,
		types.UID("99-pod-2-uid"):       testTime,
		types.UID("99-pod-3-uid"):       aLongTimeInTheFuture,
		types.UID("some-other-pod-uid"): aLongTimeInTheFuture,
	}, snapshot)
}

func TestListNonStaleStampedPodsByTenantID(t *testing.T) {
	ctx := context.Background()

	makeTestPod := func(name string, id int, state manifests.TenantPodState, stale bool) *corev1.Pod {
		pod := makePodForTenantID(t, id)
		pod.Name = name
		manifests.SetTenantPodState(pod, state)
		if stale {
			manifests.MarkTenantPodAsStale(pod)
		}
		return pod
	}

	unstampedPod := makeTestPod("unstamped-pod", 10, manifests.TenantPodRunningState, false)
	delete(unstampedPod.Labels, manifests.TenantIDLabel)

	pods := []kube.Object{
		// Good pods.
		makeTestPod("test-pod-1", 10, manifests.TenantPodDrainingState, false),
		makeTestPod("test-pod-2", 42, manifests.TenantPodRunningState, false),
		makeTestPod("test-pod-3", 5, manifests.TenantPodRunningState, false),
		makeTestPod("test-pod-4", 3, manifests.TenantPodDrainingState, false),
		makeTestPod("test-pod-5", 5, manifests.TenantPodRunningState, false),
		makeTestPod("test-pod-6", 10, manifests.TenantPodRunningState, false),
		// Stale pods.
		makeTestPod("stale-pod-1", 10, manifests.TenantPodDrainingState, true),
		makeTestPod("stale-pod-2", 42, manifests.TenantPodRunningState, true),
		makeTestPod("stale-pod-3", 5, manifests.TenantPodRunningState, true),
		// Pending pods.
		makeTestPod("pending-pod-1", 42, manifests.TenantPodPendingState, false),
		makeTestPod("pending-pod-2", 5, manifests.TenantPodPendingState, false),
		// Unstamped pods.
		unstampedPod,
		// Non-tenantpool pods.
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "kv-pod"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		},
	}
	ctl := kubetest.NewFakeCtl(kubetest.Options{}, pods...)
	s := NewTenantPodSyncer(ctx, ctl, nil /* credProvider */, nil /* nodeLocalityReader */)

	group, err := s.listNonStaleStampedPodsByTenantID(ctx)
	require.NoError(t, err)

	names := make(map[crdbv1beta1.TenantID][]string)
	for tid, pods := range group {
		for _, p := range pods {
			names[tid] = append(names[tid], p.Name)
		}
	}
	require.Equal(t, map[crdbv1beta1.TenantID][]string{
		crdbv1beta1.TenantID(3):  {"test-pod-4"},
		crdbv1beta1.TenantID(5):  {"test-pod-3", "test-pod-5"},
		crdbv1beta1.TenantID(10): {"test-pod-1", "test-pod-6"},
		crdbv1beta1.TenantID(42): {"test-pod-2"},
	}, names)
}

func TestTenantPodSyncerPodRenewals(t *testing.T) {
	ctx := context.Background()

	t.Run("podRenewalsSnapshot", func(t *testing.T) {
		testTime := timeutil.Now()
		s := NewTenantPodSyncer(ctx, nil /* ctl */, nil /* credProvider */, nil /* nodeLocalityReader */)
		s.mu.podRenewsAt[types.UID("1")] = testTime
		s.mu.podRenewsAt[types.UID("2")] = testTime
		s.mu.podRenewsAt[types.UID("3")] = testTime

		snapshot := s.podRenewalsSnapshot()
		require.Len(t, snapshot, 3)

		// Insert more entries.
		s.mu.podRenewsAt[types.UID("4")] = testTime
		s.mu.podRenewsAt[types.UID("5")] = testTime

		// Delete entries.
		delete(s.mu.podRenewsAt, types.UID("1"))

		// Snapshot should not change.
		require.Len(t, snapshot, 3)
		require.Equal(t, map[types.UID]time.Time{
			types.UID("1"): testTime,
			types.UID("2"): testTime,
			types.UID("3"): testTime,
		}, snapshot)
		require.Len(t, s.mu.podRenewsAt, 4)
		require.Equal(t, map[types.UID]time.Time{
			types.UID("2"): testTime,
			types.UID("3"): testTime,
			types.UID("4"): testTime,
			types.UID("5"): testTime,
		}, s.mu.podRenewsAt)

		// If we modified the snapshot, existing map should not be affected.
		delete(snapshot, types.UID("2"))
		require.Len(t, snapshot, 2)
		require.Len(t, s.mu.podRenewsAt, 4)
	})

	t.Run("removePodRenewals", func(t *testing.T) {
		testTime := timeutil.Now()
		s := NewTenantPodSyncer(ctx, nil /* ctl */, nil /* credProvider */, nil /* nodeLocalityReader */)
		s.mu.podRenewsAt[types.UID("1")] = testTime
		s.mu.podRenewsAt[types.UID("2")] = testTime
		s.mu.podRenewsAt[types.UID("3")] = testTime

		s.removePodRenewals([]types.UID{"1", "3", "4", "8"})
		require.Equal(t, map[types.UID]time.Time{
			types.UID("2"): testTime,
		}, s.mu.podRenewsAt)
	})

	t.Run("canPodRenew", func(t *testing.T) {
		testTime := time.Date(2023, 12, 31, 12, 0, 0, 0, time.UTC)
		defer testutils.HookGlobal(&timeutil.Now, func() time.Time {
			return testTime
		})()

		s := NewTenantPodSyncer(ctx, nil /* ctl */, nil /* credProvider */, nil /* nodeLocalityReader */)
		s.mu.podRenewsAt[types.UID("boundary")] = testTime
		s.mu.podRenewsAt[types.UID("expired")] = testTime.Add(-3 * time.Minute)
		s.mu.podRenewsAt[types.UID("active")] = testTime.Add(10 * time.Minute)

		require.False(t, s.canPodRenew(types.UID("boundary")))
		require.False(t, s.canPodRenew(types.UID("active")))
		require.True(t, s.canPodRenew(types.UID("expired")))

		testTime = testTime.Add(20 * time.Minute)
		require.True(t, s.canPodRenew(types.UID("boundary")))
		require.True(t, s.canPodRenew(types.UID("active")))
		require.True(t, s.canPodRenew(types.UID("expired")))
	})

	t.Run("setPodRenewal", func(t *testing.T) {
		testTime := timeutil.Now()

		s := NewTenantPodSyncer(ctx, nil /* ctl */, nil /* credProvider */, nil /* nodeLocalityReader */)
		s.setPodRenewal(types.UID("test-pod1"), testTime)
		s.setPodRenewal(types.UID("test-pod2"), testTime)

		require.Len(t, s.mu.podRenewsAt, 2)
		e, ok := s.mu.podRenewsAt[types.UID("test-pod1")]
		require.True(t, ok)
		require.Equal(t, testTime, e)
		e, ok = s.mu.podRenewsAt[types.UID("test-pod2")]
		require.True(t, ok)
		require.Equal(t, testTime, e)
	})

	t.Run("multiple callers", func(t *testing.T) {
		const (
			numCallers    = 50
			numOperations = 100
		)
		testTime := timeutil.Now()

		s := NewTenantPodSyncer(ctx, nil /* ctl */, nil /* credProvider */, nil /* nodeLocalityReader */)

		var wg sync.WaitGroup
		wg.Add(numCallers)
		for i := range numCallers {
			go func(i int) {
				defer wg.Done()

				// Randomize the operations. There should not be a concurrent
				// read/write.
				pod := types.UID(fmt.Sprintf("%d", i))
				for range numOperations {
					r := rand.Intn(3)
					switch r {
					case 0:
						s.setPodRenewal(pod, testTime.Add(-1*time.Minute))
					case 1:
						require.True(t, s.canPodRenew(pod))
					case 2:
						// Put a random entry in it.
						snapshot := s.podRenewalsSnapshot()
						snapshot[types.UID(fmt.Sprintf("%d-random", i))] = testTime
					}
				}
			}(i)
		}
		wg.Wait()
	})
}

func TestIsSyncSupported(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tenant   *crdbv1beta1.CrdbTenant
		expected bool
	}{
		{
			name:     "default",
			tenant:   &crdbv1beta1.CrdbTenant{},
			expected: false,
		},
		{
			name: "otel collector without aws exporters",
			tenant: &crdbv1beta1.CrdbTenant{
				Spec: crdbv1beta1.CrdbTenantSpec{
					OtelCollector: &crdbv1beta1.OtelCollector{
						ConfigMapName: "foobar",
					},
				},
			},
			expected: false,
		},
		{
			name: "otel collector with aws metrics cloudwatch",
			tenant: &crdbv1beta1.CrdbTenant{
				Spec: crdbv1beta1.CrdbTenantSpec{
					OtelCollector: &crdbv1beta1.OtelCollector{
						AWSMetricsRoleARN: "custom-role-arn",
					},
				},
			},
			expected: true,
		},
		{
			name: "otel collector with aws logs cloudwatch",
			tenant: &crdbv1beta1.CrdbTenant{
				Spec: crdbv1beta1.CrdbTenantSpec{
					OtelCollector: &crdbv1beta1.OtelCollector{
						AWSLogsRoleARN: "custom-role-arn",
					},
				},
			},
			expected: true,
		},
		{
			name: "otel collector with gcp logs",
			tenant: &crdbv1beta1.CrdbTenant{
				Spec: crdbv1beta1.CrdbTenantSpec{
					OtelCollector: &crdbv1beta1.OtelCollector{
						GCPLogsServiceAccount: "foobar",
					},
				},
			},
			expected: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, isSyncSupported(tc.tenant))
		})
	}
}

func makeCrdbTenant(t *testing.T) *crdbv1beta1.CrdbTenant {
	t.Helper()
	return &crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-42",
			Namespace: "default",
		},
		Spec: crdbv1beta1.CrdbTenantSpec{
			TenantID: crdbv1beta1.TenantID(42),
			OtelCollector: &crdbv1beta1.OtelCollector{
				ConfigMapName:         "test-configmap-name",
				AWSMetricsRoleARN:     "arn:aws:iam::000011112222:role/test-role",
				AWSLogsRoleARN:        "arn:aws:iam::000011112222:role/test-role-2",
				AWSExternalID:         "test-external-id",
				GCPLogsServiceAccount: "foobar",
			},
		},
	}
}

func makePodForTenantID(t *testing.T, id int) *corev1.Pod {
	t.Helper()
	return makePod(t, &crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("tenant-%d", id)},
		Spec:       crdbv1beta1.CrdbTenantSpec{TenantID: crdbv1beta1.TenantID(id)},
	})
}

func makePod(t *testing.T, tenant *crdbv1beta1.CrdbTenant) *corev1.Pod {
	t.Helper()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: labels.Set{
				"app":                         manifests.TenantPoolAppName,
				manifests.TenantIDLabel:       tenant.Spec.TenantID.String(),
				manifests.TenantPodStateLabel: string(manifests.TenantPodRunningState),
			},
			Annotations: map[string]string{
				manifests.TenantPodRevisionAnnotation: manifests.GetTenantPodHashRevision(nil /* pod */, tenant),
			},
			UID: types.UID("test-pod-uid"),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func makeAWSCred(t *testing.T, expiry time.Time) *cloud.AWSCredentials {
	return &cloud.AWSCredentials{
		Value: credentials.Value{
			AccessKeyID:     "test-access-key-id",
			SecretAccessKey: "test-secret-access-key",
			SessionToken:    "test-session-token",
		},
		Expiration: expiry,
	}
}

func makeGCPCred(t *testing.T, expiry time.Time) *cloud.GCPCredentials {
	return &cloud.GCPCredentials{
		Token:      &oauth2.Token{AccessToken: "foobar", Expiry: time.Now()},
		Expiration: expiry,
	}
}

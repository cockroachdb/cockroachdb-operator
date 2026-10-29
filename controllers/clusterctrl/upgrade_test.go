package clusterctrl

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubenet"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pgxsql"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMarkOldStampedPodsAsStale(t *testing.T) {
	ctx := context.Background()

	oldImage := "cockroachdb/cockroach:v21.1.0"
	newImage := "cockroachdb/cockroach:v21.1.4"
	poolName := "nameofpool"
	replicas := int32(3)

	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foobar",
			Namespace: "default",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Template: crdbv1beta1.CrdbNodeTemplate{
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image: newImage,
				},
			},
			TenantHost: &crdbv1beta1.TenantHost{
				TenantPools: []crdbv1beta1.TenantPool{
					{Name: poolName},
				},
			},
			TLSEnabled: true,
		},
	}

	deployment := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      manifests.MakeTenantPoolName(poolName),
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
		},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: replicas,
		},
	}

	bravo := crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:        crdbv1beta1.TenantMinID.Tag(),
			Namespace:   "default",
			Annotations: map[string]string{},
		},
		Spec: crdbv1beta1.CrdbTenantSpec{
			TenantID:   crdbv1beta1.TenantMinID,
			TenantPool: &poolName,
		},
	}

	charlie := crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:        (crdbv1beta1.TenantMinID + 1).Tag(),
			Namespace:   "default",
			Annotations: map[string]string{},
		},
		Spec: crdbv1beta1.CrdbTenantSpec{
			TenantID:   crdbv1beta1.TenantMinID + 1,
			TenantPool: &poolName,
		},
	}

	stampedPodBravo := stampedPodForTenant(crdbv1beta1.TenantMinID, oldImage, poolName)
	stampedPodCharlie := stampedPodForTenant(crdbv1beta1.TenantMinID+1, newImage, poolName)

	ctl := kubetest.NewFakeCtl(kubetest.Options{}, cluster, &stampedPodCharlie, &stampedPodBravo, &bravo, &charlie, &deployment)
	h := requestHandler{reconciler: &Reconciler{}, ctx: ctx, ctl: ctl, cluster: cluster}
	require.NoError(t, h.markOldStampedPodsAsStale())

	// Confirm that bravo's pod was marked as stale, and charlie's stamped pod is left untouched.
	podList := &corev1.PodList{}
	query := fmt.Sprintf("app=%s, %s", manifests.TenantPoolAppName, manifests.TenantIDLabel)
	options := kube.ListOptions{
		LabelSelector: kube.ParseLabelOrDie(query),
	}
	err := ctl.List(ctx, &options, podList)
	require.NoError(t, err)
	foundCharlie := false
	foundBravo := false
	for _, pod := range podList.Items {
		// Charlie's pod
		if pod.Labels[manifests.TenantIDLabel] == "3" {
			require.False(t, manifests.IsTenantPodStale(&pod))
			foundCharlie = true
		}

		// Bravo's pod
		if pod.Labels[manifests.TenantIDLabel] == "2" {
			require.True(t, manifests.IsTenantPodStale(&pod))
			foundBravo = true
		}
	}
	require.True(t, foundCharlie && foundBravo)

	// Make the tenant pool have an image override. Now both bravos and charlies pod should be marked as stale.
	cluster.Spec.TenantHost = &crdbv1beta1.TenantHost{
		TenantPools: []crdbv1beta1.TenantPool{
			{Name: poolName, CockroachImage: "foobar"},
		},
	}
	ctl = kubetest.NewFakeCtl(kubetest.Options{}, cluster, &stampedPodCharlie, &stampedPodBravo, &bravo, &charlie, &deployment)
	h = requestHandler{reconciler: &Reconciler{}, ctx: ctx, ctl: ctl, cluster: cluster}
	require.NoError(t, h.markOldStampedPodsAsStale())
	podList = &corev1.PodList{}
	query = fmt.Sprintf("app=%s, %s", manifests.TenantPoolAppName, manifests.TenantIDLabel)
	options = kube.ListOptions{
		LabelSelector: kube.ParseLabelOrDie(query),
	}
	err = ctl.List(ctx, &options, podList)
	require.NoError(t, err)
	require.Equal(t, 2, len(podList.Items))
	for _, pod := range podList.Items {
		require.True(t, manifests.IsTenantPodStale(&pod))
	}
}

func TestFindTenantsWithStalePods(t *testing.T) {
	bravoID := crdbv1beta1.TenantMinID
	charlieID := bravoID + 1
	deltaID := charlieID + 1

	stampedPodBravo := stampedPodForTenant(bravoID, "", "")
	stampedPodCharlie := stampedPodForTenant(charlieID, "", "")
	stampedPodDelta := stampedPodForTenant(deltaID, "", "")

	manifests.MarkTenantPodAsStale(&stampedPodDelta)
	manifests.MarkTenantPodAsStale(&stampedPodCharlie)

	ctl := kubetest.NewFakeCtl(kubetest.Options{}, &stampedPodCharlie, &stampedPodBravo, &stampedPodDelta)
	tenantsWithStalePods, err := findTenantsWithStalePods(context.Background(), ctl)
	require.NoError(t, err)

	_, ok := tenantsWithStalePods[bravoID]
	require.False(t, ok)
	_, ok = tenantsWithStalePods[charlieID]
	require.True(t, ok)
	_, ok = tenantsWithStalePods[deltaID]
	require.True(t, ok)
}

func TestHasPoolCapacity(t *testing.T) {
	ctx := context.Background()

	nineReplicas := int32(9)
	fiveReplicas := int32(5)
	sevenReplicas := int32(7)

	poolName := "nameofpool"
	deployment := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      manifests.MakeTenantPoolName(poolName),
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &nineReplicas,
		},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: sevenReplicas,
		},
	}

	t.Run("has capacity", func(t *testing.T) {
		deployment2 := deployment
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &deployment2)
		require.NoError(t, hasPoolCapacity(ctx, ctl, poolName))
	})

	t.Run("does not have capacity", func(t *testing.T) {
		deployment2 := deployment
		deployment2.Status.ReadyReplicas = fiveReplicas
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &deployment2)
		require.ErrorIs(t, hasPoolCapacity(ctx, ctl, poolName), poolThrottleError)
	})
}

func TestGetTenantsForFinalization(t *testing.T) {
	ctx := context.Background()

	image := "cockroachdb/cockroach:v21.1.1"
	defaultPool := "default"
	nonDefaultPool := "non-default"
	cluster := crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foobar",
			Namespace: "default",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Template: crdbv1beta1.CrdbNodeTemplate{
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image: image,
				},
			},
			TenantHost: &crdbv1beta1.TenantHost{
				TenantPools: []crdbv1beta1.TenantPool{
					{Name: defaultPool},
					{Name: nonDefaultPool, CockroachImage: "foobar"},
				},
			},
			TLSEnabled: true,
		},
	}
	tenant := crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "bravo",
			Namespace:   "default",
			Annotations: map[string]string{},
		},
		Spec: crdbv1beta1.CrdbTenantSpec{
			TenantID:   crdbv1beta1.TenantMinID,
			TenantPool: &defaultPool,
		},
	}

	replicas := int32(3)
	defaultPoolDeployment := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      manifests.MakeTenantPoolName(defaultPool),
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
		},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: replicas,
		},
	}

	mockHostDB := func() (*sql.DB, sqlmock.Sqlmock) {
		db, databaseMock, err := sqlmock.New()
		require.NoError(t, err)

		rows := sqlmock.NewRows([]string{"version"}).AddRow("21.1")
		databaseMock.ExpectQuery(getClusterVersionQuery).WillReturnRows(rows)
		databaseMock.ExpectClose()
		return db, databaseMock
	}

	t.Run("non default tenant included", func(t *testing.T) {
		cluster2 := cluster
		tenant2 := tenant
		tenant2.Spec.TenantPool = &nonDefaultPool
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &cluster2, &tenant2)
		h := requestHandler{reconciler: &Reconciler{}, ctx: ctx, ctl: ctl, cluster: &cluster2}

		// This mock only expects one query to get the cluster version of the host.
		hostDB, hostDatabaseMock := mockHostDB()
		defer hostDB.Close()

		defer testutils.HookGlobal(&kubenet.OpenDB,
			func(ctx context.Context, ctl *kube.Ctl, options kubenet.Options) (*pgxsql.DB, error) {
				if options.TenantID >= crdbv1beta1.TenantMinID {
					return nil, errors.New("unexpected DB call to tenant")
				}
				return &pgxsql.DB{DB: hostDB}, nil
			},
		)()

		tenants, stale, err := h.getTenantsForFinalization()
		require.NoError(t, err)
		require.False(t, stale)
		require.Equal(t, 1, len(tenants))
		require.NoError(t, hostDatabaseMock.ExpectationsWereMet())
	})

	t.Run("default tenant included", func(t *testing.T) {
		cluster2 := cluster
		tenant2 := tenant
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &cluster2, &tenant2, &defaultPoolDeployment)
		h := requestHandler{reconciler: &Reconciler{}, ctx: ctx, ctl: ctl, cluster: &cluster2}

		// This mock only expects one query to get the cluster version of the host.
		hostDB, hostDatabaseMock := mockHostDB()
		defer hostDB.Close()

		defer testutils.HookGlobal(&kubenet.OpenDB,
			func(ctx context.Context, ctl *kube.Ctl, options kubenet.Options) (*pgxsql.DB, error) {
				if options.TenantID >= crdbv1beta1.TenantMinID {
					return nil, errors.New("unexpected DB call to tenant")
				}
				return &pgxsql.DB{DB: hostDB}, nil
			},
		)()

		tenants, stale, err := h.getTenantsForFinalization()
		require.NoError(t, err)
		require.False(t, stale)
		require.Equal(t, 1, len(tenants))
		require.NoError(t, hostDatabaseMock.ExpectationsWereMet())
	})

	t.Run("default tenant with cluster-version annotation not included", func(t *testing.T) {
		cluster2 := cluster
		tenant2 := tenant
		tenant2.Annotations = map[string]string{ClusterVersion: "21.1"}
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &cluster2, &tenant2, &defaultPoolDeployment)
		h := requestHandler{reconciler: &Reconciler{}, ctx: ctx, ctl: ctl, cluster: &cluster2}

		// This mock only expects one query to get the cluster version of the host.
		hostDB, hostDatabaseMock := mockHostDB()
		defer hostDB.Close()

		defer testutils.HookGlobal(&kubenet.OpenDB,
			func(ctx context.Context, ctl *kube.Ctl, options kubenet.Options) (*pgxsql.DB, error) {
				if options.TenantID >= crdbv1beta1.TenantMinID {
					return nil, errors.New("unexpected DB call to tenant")
				}
				return &pgxsql.DB{DB: hostDB}, nil
			},
		)()

		tenants, stale, err := h.getTenantsForFinalization()
		require.NoError(t, err)
		require.False(t, stale)
		require.Equal(t, 0, len(tenants))
		require.NoError(t, hostDatabaseMock.ExpectationsWereMet())
	})

	t.Run("tenant with stale pod not included", func(t *testing.T) {
		cluster2 := cluster
		tenant2 := tenant
		stampedPod := stampedPodForTenant(tenant2.Spec.TenantID, "", defaultPool)
		manifests.MarkTenantPodAsStale(&stampedPod)
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &stampedPod, &cluster2, &tenant2, &defaultPoolDeployment)
		h := requestHandler{reconciler: &Reconciler{}, ctx: ctx, ctl: ctl, cluster: &cluster2}

		// This mock only expects one query to get the cluster version of the host.
		hostDB, hostDatabaseMock := mockHostDB()
		defer hostDB.Close()

		defer testutils.HookGlobal(&kubenet.OpenDB,
			func(ctx context.Context, ctl *kube.Ctl, options kubenet.Options) (*pgxsql.DB, error) {
				if options.TenantID >= crdbv1beta1.TenantMinID {
					return nil, errors.New("unexpected DB call to tenant")
				}
				return &pgxsql.DB{DB: hostDB}, nil
			},
		)()
		tenants, stale, err := h.getTenantsForFinalization()
		require.NoError(t, err)
		require.True(t, stale)
		require.Equal(t, 0, len(tenants))
		require.NoError(t, hostDatabaseMock.ExpectationsWereMet())
	})
}

func TestFinalizeTenants(t *testing.T) {
	ctx := context.Background()

	image := "cockroachdb/cockroach:v21.1.1"
	defaultPool := "default"
	nonDefaultPool := "non-default"

	cluster := crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foobar",
			Namespace: "default",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Template: crdbv1beta1.CrdbNodeTemplate{
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image: image,
				},
			},
			TenantHost: &crdbv1beta1.TenantHost{
				TenantPools: []crdbv1beta1.TenantPool{
					{Name: defaultPool},
					{Name: nonDefaultPool, CockroachImage: "foobar"},
				},
			},
			TLSEnabled: true,
		},
		Status: crdbv1beta1.CrdbClusterStatus{},
	}
	cluster.Status.SetAction(crdbv1beta1.AwaitFinalization, crdbv1beta1.Starting)

	mockHostDBIsFinalized := func() (*sql.DB, sqlmock.Sqlmock) {
		db, databaseMock, err := sqlmock.New()
		require.NoError(t, err)

		rows := sqlmock.NewRows([]string{"result"}).AddRow("true")
		databaseMock.ExpectQuery(regexp.QuoteMeta(isClusterFinalizedQuery)).WillReturnRows(rows)
		databaseMock.ExpectClose()
		return db, databaseMock
	}
	mockHostDBGetClusterVersion := func() (*sql.DB, sqlmock.Sqlmock) {
		db, databaseMock, err := sqlmock.New()
		require.NoError(t, err)

		rows := sqlmock.NewRows([]string{"version"}).AddRow("21.1")
		databaseMock.ExpectQuery(getClusterVersionQuery).WillReturnRows(rows)
		databaseMock.ExpectClose()
		return db, databaseMock
	}

	t.Run("finalization is disabled", func(t *testing.T) {
		cluster2 := cluster.DeepCopy()
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, cluster2)
		h := requestHandler{reconciler: &Reconciler{DisableTenantFinalization: true}, ctx: ctx, ctl: ctl, cluster: cluster2}

		err := h.FinalizeTenants()
		require.NoError(t, err)
		err = ctl.Get(ctx, cluster2.Name, cluster2)
		require.NoError(t, err)
		require.True(t, cluster2.Status.HasAction(crdbv1beta1.AwaitFinalization, crdbv1beta1.Finished))
	})

	t.Run("Finalizer already running", func(t *testing.T) {
		cluster2 := cluster.DeepCopy()
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, cluster2)
		f := NewFinalizer(ctx)
		f.mu.isRunning = true
		defer func() {
			f.mu.isRunning = false
		}()
		h := requestHandler{reconciler: &Reconciler{Finalizer: f}, ctx: ctx, ctl: ctl, cluster: cluster2}

		defer testutils.HookGlobal(&kubenet.OpenDB,
			func(ctx context.Context, ctl *kube.Ctl, options kubenet.Options) (*pgxsql.DB, error) {
				// We shouldn't attempt to finalize the tenant.
				if options.TenantID >= crdbv1beta1.TenantMinID {
					return nil, errors.New("unexpected DB call to tenant")
				}
				hostDB, _ := mockHostDBIsFinalized()
				return &pgxsql.DB{DB: hostDB}, nil
			},
		)()

		err := h.FinalizeTenants()
		require.NoError(t, err)
		require.Equal(t, time.Minute, h.res.RequeueAfter)
	})

	t.Run("no tenants to finalize - mark as finished", func(t *testing.T) {
		cluster2 := cluster.DeepCopy()
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, cluster2)
		f := NewFinalizer(ctx)
		h := requestHandler{reconciler: &Reconciler{Finalizer: f}, ctx: ctx, ctl: ctl, cluster: cluster2}

		count := 0
		defer testutils.HookGlobal(&kubenet.OpenDB,
			func(ctx context.Context, ctl *kube.Ctl, options kubenet.Options) (*pgxsql.DB, error) {
				// We shouldn't attempt to finalize the tenant.
				if options.TenantID >= crdbv1beta1.TenantMinID {
					return nil, errors.New("unexpected DB call to tenant")
				}
				var hostDB *sql.DB
				if count == 0 {
					hostDB, _ = mockHostDBIsFinalized()
				} else if count == 1 {
					hostDB, _ = mockHostDBGetClusterVersion()
				}
				count += 1
				return &pgxsql.DB{DB: hostDB}, nil
			},
		)()

		err := h.FinalizeTenants()
		require.NoError(t, err)
		err = ctl.Get(ctx, cluster2.Name, cluster2)
		require.NoError(t, err)
		require.True(t, cluster2.Status.HasAction(crdbv1beta1.AwaitFinalization, crdbv1beta1.Finished))
	})
}

func stampedPodForTenant(tenantID crdbv1beta1.TenantID, crdbImage, poolName string) corev1.Pod {
	name := tenantID.Tag()
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				"app":                         manifests.TenantPoolAppName,
				manifests.TenantIDLabel:       tenantID.String(),
				manifests.TenantPoolNameLabel: poolName,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: manifests.CockroachDB, Image: crdbImage},
			},
		},
	}
}

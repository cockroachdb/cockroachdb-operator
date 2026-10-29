package clusterctrl

import (
	"context"
	"testing"

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestFinalizeTenant(t *testing.T) {
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

	t.Run("Finalizer already running", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{})
		f := NewFinalizer(ctx)
		f.mu.isRunning = true
		defer func() {
			f.mu.isRunning = false
		}()

		err := f.FinalizeTenants(nil, ctl)
		require.EqualError(t, err, "Finalizer is currently running")
	})

	t.Run("finalizeTenant", func(t *testing.T) {
		cluster2 := cluster
		tenant2 := tenant
		// TODO(alyshan): Not sure why the using map[string]string{} results in a
		// "panic: assignment to entry in nil map". So put a bogus entry in there.
		tenant2.Annotations = map[string]string{"foobar": ""}
		// Make tenant ready after applying the annotation.
		opts := kube.Options{BeforeMutation: kubetest.MakeReady}
		ctl := kubetest.NewFakeCtl(kubetest.Options{Options: opts}, &cluster2, &tenant2, &defaultPoolDeployment)
		f := NewFinalizer(ctx)
		f.ctl = ctl

		db, databaseMock, err := sqlmock.New()
		require.NoError(t, err)
		defer db.Close()

		// We expect to set the cluster version for this tenant.
		rows := sqlmock.NewRows([]string{"version"}).AddRow("21.1")
		databaseMock.ExpectExec(setClusterVersionQuery).WillReturnResult(sqlmock.NewResult(1, 1))
		databaseMock.ExpectQuery(getClusterVersionQuery).WillReturnRows(rows)
		databaseMock.ExpectClose()

		defer testutils.HookGlobal(&kubenet.OpenDB,
			func(ctx context.Context, ctl *kube.Ctl, options kubenet.Options) (*pgxsql.DB, error) {
				if options.TenantID >= crdbv1beta1.TenantMinID {
					require.Equal(t, options.TenantID, tenant2.Spec.TenantID)
					return &pgxsql.DB{DB: db}, nil
				}
				return nil, errors.New("unexpected call")
			},
		)()

		f.finalizeTenant(tenant2)
		// Tenant should have annotation set.
		require.NoError(t, ctl.Get(ctx, tenant2.Name, &tenant2))
		require.Equal(t, "21.1", tenant2.Annotations[ClusterVersion])
		require.NoError(t, databaseMock.ExpectationsWereMet())
	})

	t.Run("error finalizing tenant", func(t *testing.T) {
		cluster2 := cluster
		tenant2 := tenant
		ctl := kubetest.NewFakeCtl(kubetest.Options{}, &cluster2, &tenant2, &defaultPoolDeployment)
		f := NewFinalizer(ctx)
		f.ctl = ctl

		db, databaseMock, err := sqlmock.New()
		require.NoError(t, err)
		defer db.Close()

		// We expect to set the cluster version for this tenant.
		databaseMock.ExpectExec(setClusterVersionQuery).WillReturnError(errors.New("foobar"))
		databaseMock.ExpectClose()

		defer testutils.HookGlobal(&kubenet.OpenDB,
			func(ctx context.Context, ctl *kube.Ctl, options kubenet.Options) (*pgxsql.DB, error) {
				if options.TenantID >= crdbv1beta1.TenantMinID {
					require.Equal(t, options.TenantID, tenant2.Spec.TenantID)
					return &pgxsql.DB{DB: db}, nil
				}
				return nil, errors.New("unexpected call")
			},
		)()

		f.finalizeTenant(tenant2)
		require.Equal(t, uint32(1), f.numFinalizationErr)
		// Tenant should have no annotation set.
		require.NoError(t, ctl.Get(ctx, tenant2.Name, &tenant2))
		require.Equal(t, 0, len(tenant2.Annotations))
		require.NoError(t, databaseMock.ExpectationsWereMet())
	})
}

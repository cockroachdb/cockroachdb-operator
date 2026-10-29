package kubenet_test

import (
	"context"
	"crypto/tls"
	"net/url"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/crypto"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubenet"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/testenv"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestOpen(t *testing.T) {
	t.Run("missing password", func(t *testing.T) {
		db, err := kubenet.OpenDB(context.Background(), nil, kubenet.Options{
			TenantCluster: "dim-dog",
		})
		require.EqualError(t, err, "Options.Password must be specified for SQL proxy connections")
		require.Nil(t, db)
	})

	t.Run("missing tenant ID", func(t *testing.T) {
		db, err := kubenet.OpenDB(context.Background(), nil, kubenet.Options{
			TenantCluster: "dim-dog",
			User:          url.UserPassword("foo", "bar"),
		})
		require.EqualError(t, err, "Options.TenantID must be specified for SQL proxy connections")
		require.Nil(t, db)
	})
}

func TestLocatePods_SystemTenant(t *testing.T) {
	ctx := context.Background()

	t.Run("no running pods", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{})
		names, err := kubenet.LocatePods(ctx, ctl, &kubenet.Options{
			TenantID: crdbv1beta1.TenantSystemID,
		})
		require.Error(t, err)
		require.Regexp(t, "no available pods for tenant 1", err.Error())
		require.Nil(t, names)
	})

	t.Run("successful", func(t *testing.T) {
		makeKVPod := func(name string) *corev1.Pod {
			return &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: name,
					Labels: map[string]string{
						"app": "cockroachdb",
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			}
		}
		ctl := kubetest.NewFakeCtl(kubetest.Options{})
		require.NoError(t, ctl.Apply(ctx, makeKVPod("pod1")))
		require.NoError(t, ctl.Apply(ctx, makeKVPod("pod2")))
		names, err := kubenet.LocatePods(ctx, ctl, &kubenet.Options{
			TenantID: crdbv1beta1.TenantSystemID,
		})
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"pod1", "pod2"}, names)
	})
}

func TestLocatePods_ReadyPodsOnly(t *testing.T) {
	ctx := context.Background()

	makeKVPod := func(name string, ready bool) *corev1.Pod {
		status := corev1.ConditionFalse
		if ready {
			status = corev1.ConditionTrue
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					"app": "cockroachdb",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: status},
				},
			},
		}
	}

	// applyPods adds pods to the fake ctl. NoWait is used to skip the
	// readiness wait so that non-ready pods can be created.
	applyPods := func(t *testing.T, ctl *kube.Ctl, pods ...*corev1.Pod) {
		t.Helper()
		for _, p := range pods {
			require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, p))
		}
	}

	t.Run("filters out non-ready pods", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{})
		applyPods(t, ctl,
			makeKVPod("crdb-0", true),
			makeKVPod("crdb-1", false),
			makeKVPod("crdb-2", true),
		)

		names, err := kubenet.LocatePods(ctx, ctl, &kubenet.Options{
			TenantID:      crdbv1beta1.TenantSystemID,
			ReadyPodsOnly: true,
		})
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"crdb-0", "crdb-2"}, names)
	})

	t.Run("returns all pods when disabled", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{})
		applyPods(t, ctl,
			makeKVPod("crdb-0", true),
			makeKVPod("crdb-1", false),
			makeKVPod("crdb-2", true),
		)

		names, err := kubenet.LocatePods(ctx, ctl, &kubenet.Options{
			TenantID:      crdbv1beta1.TenantSystemID,
			ReadyPodsOnly: false,
		})
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"crdb-0", "crdb-1", "crdb-2"}, names)
	})

	t.Run("errors when all pods are non-ready", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{})
		applyPods(t, ctl,
			makeKVPod("crdb-0", false),
			makeKVPod("crdb-1", false),
		)

		names, err := kubenet.LocatePods(ctx, ctl, &kubenet.Options{
			TenantID:      crdbv1beta1.TenantSystemID,
			ReadyPodsOnly: true,
		})
		require.Error(t, err)
		require.Regexp(t, "ready pods only required but there were none", err.Error())
		require.Nil(t, names)
	})
}

func TestIntegrationSecureClusterSql(t *testing.T) {
	testutils.MarkIntegrationTest(t)

	ctx := context.Background()
	env := testenv.New(t, testenv.MultiZoneConfig)

	cluster := testenv.CreateCrdbCluster(t, env, "test", &crdbv1beta1.CrdbClusterSpec{
		TLSEnabled: true,
		TenantHost: &crdbv1beta1.TenantHost{},
	})
	ctl := env.DefaultCluster().NewCtl(cluster.Namespace)

	t.Run("system tenant", func(t *testing.T) {
		testenv.TestKubeSql(t, ctl, kubenet.Options{
			TenantID: crdbv1beta1.TenantSystemID,
		})
	})

	t.Run("system tenant with ordinal", func(t *testing.T) {
		testenv.TestKubeSql(t, ctl, kubenet.Options{
			TenantID: crdbv1beta1.TenantSystemID,
		})
	})

	signer := crypto.DeterministicFakeSignerForTesting(t)
	const certificateTTL = time.Minute

	t.Run("system tenant with TLSConfig", func(t *testing.T) {
		testenv.TestKubeSql(t, ctl, kubenet.Options{
			TenantID:  crdbv1beta1.TenantSystemID,
			TLSConfig: &tls.Config{},
		})

		testenv.ExecKubeSql(t, ctl, kubenet.Options{
			TenantID: crdbv1beta1.TenantSystemID,
		}, "CREATE USER IF NOT EXISTS certuser")

		// Create certuser certificate.
		certuser, err := signer.Sign(ctx, crypto.CertificateSigningRequest{
			Role:       "client",
			CommonName: "certuser",
			TTL:        certificateTTL,
		})
		require.NoError(t, err)
		ca, err := signer.Certificate(ctx)
		require.NoError(t, err)
		tlsConfig, err := crypto.TLSConfigFromBundle(ca, certuser, "")
		require.NoError(t, err)

		testenv.TestKubeSql(t, ctl, kubenet.Options{
			TenantID:  crdbv1beta1.TenantSystemID,
			User:      url.User("certuser"),
			TLSConfig: tlsConfig,
		})

		err = testenv.ErrorOpenKubeSql(t, ctl, kubenet.Options{
			TenantID:  crdbv1beta1.TenantSystemID,
			User:      url.UserPassword("certuser", "shrek is love"),
			TLSConfig: tlsConfig,
		})
		require.EqualError(t, err, "cannot have both a password and a client certificate set")

		err = testenv.ErrorOpenKubeSql(t, ctl, kubenet.Options{
			TenantID: crdbv1beta1.TenantSystemID,
			User:     url.User("certuser"),
		})
		require.EqualError(t, err, "user must be root if no client certificate is set")
	})

	t.Run("system tenant with username/password", func(t *testing.T) {
		// First create SQL user by tunneling directly to CRDB pod.
		testenv.ExecKubeSql(t, ctl, kubenet.Options{
			TenantID: crdbv1beta1.TenantSystemID,
		}, "CREATE USER IF NOT EXISTS me WITH PASSWORD 'password'")

		testenv.TestKubeSql(t, ctl, kubenet.Options{
			TenantID: crdbv1beta1.TenantSystemID,
			User:     url.UserPassword("me", "password"),
		})
	})

	t.Run("regular tenant, don't use sql proxy", func(t *testing.T) {
		const tenantID = crdbv1beta1.TenantID(11)
		tenant := testenv.CreateCrdbTenant(t, ctl, "no-proxy", &crdbv1beta1.CrdbTenantSpec{
			TenantID: tenantID,
			// Explicitly set Pods to 0 as kubesql will automatically trigger
			// resumption.
			Pods: 0,
		})

		options := kubenet.Options{
			TenantID: tenant.Spec.TenantID,
		}
		db := testenv.OpenKubeSql(t, ctl, options)
		defer db.Close()

		// Ensure connection is created and then returned to pool. This will take
		// some time, since the tenant needs to be resumed, which takes O(100ms).
		var res int
		require.NoError(t, db.QueryRow("SELECT 42;").Scan(&res))
		require.Equal(t, 42, res)
		stats := db.DB.Stats()
		require.Equal(t, 1, stats.Idle)

		// Ensure the connection pooled by the previous query is used.
		require.NoError(t, db.QueryRow("SELECT 42;").Scan(&res))
		require.Equal(t, 42, res)
		stats = db.DB.Stats()
		require.Equal(t, 1, stats.Idle)

		// NOTE: This block cannot be used with copyist because of the issue
		// described in kubenet.OpenDB (see TODO near the isCopyistDriver check).
		// It will just run in nightly integration tests.
		if testutils.IntegrationTestsEnabled {
			// Scale down the tenant.
			tenant.Spec.Pods = 0
			require.NoError(t, ctl.Apply(ctx, tenant))

			// The connection to the tenant should still be open.
			require.NoError(t, db.QueryRow("SELECT 42;").Scan(&res))
			require.Equal(t, 42, res)
			stats = db.DB.Stats()
			require.Equal(t, 1, stats.Idle)
			// Specifically the connection is to a stamped pod for the tenant which is marked as DRAINING.
			var stampedPods corev1.PodList
			require.NoError(t, ctl.List(ctx, &kube.ListOptions{
				LabelSelector: manifests.MakeStampedPodSelector(tenant.Spec.TenantID),
			}, &stampedPods))
			require.Len(t, stampedPods.Items, 1)
			require.Equal(t, string(manifests.TenantPodDrainingState), stampedPods.Items[0].Labels[manifests.TenantPodStateLabel])

			// Opening a connection to the tenant again should transition the stamped pod to RUNNING.
			db2 := testenv.OpenKubeSql(t, ctl, options)
			defer db2.Close()
			require.NoError(t, db2.QueryRow("SELECT 42;").Scan(&res))
			require.Equal(t, 42, res)
			pod := corev1.Pod{}
			require.NoError(t, ctl.Get(ctx, stampedPods.Items[0].Name, &pod))
			require.Equal(t, string(manifests.TenantPodRunningState), pod.Labels[manifests.TenantPodStateLabel])

			// Delete the pod.
			require.NoError(t, ctl.Delete(ctx, &pod))
		}

		// Make sure that re-connection to database eventually works, even though
		// pod name has changed (which can trigger a transient connection error).
		require.Eventually(t, func() bool {
			return db.QueryRow("SELECT 42;").Scan(&res) == nil
		}, 10*time.Second, 10*time.Millisecond)
		require.Equal(t, 42, res)
		stats = db.DB.Stats()
		require.Equal(t, 1, stats.Idle)

		require.NoError(t, ctl.Delete(ctx, tenant))
	})

	t.Run("regular tenant, use sql proxy", func(t *testing.T) {
		const clusterName = "via-proxy"
		const tenantID = crdbv1beta1.TenantID(12)
		tenant := testenv.CreateCrdbTenant(t, ctl, clusterName, &crdbv1beta1.CrdbTenantSpec{
			TenantID: tenantID,
			Pods:     1,
		})

		// First create SQL user by tunneling directly to SQL pod.
		testenv.ExecKubeSql(t, ctl, kubenet.Options{
			TenantID: tenant.Spec.TenantID,
		}, "CREATE USER IF NOT EXISTS me WITH PASSWORD 'password'")

		// Now execute statement through proxy.
		testenv.TestKubeSql(t, ctl, kubenet.Options{
			TenantID:      tenant.Spec.TenantID,
			TenantCluster: clusterName,
			User:          url.UserPassword("me", "password"),
		})

		require.NoError(t, ctl.Delete(ctx, tenant))
	})

	t.Run("regular tenant with TLSConfig, use sql proxy", func(t *testing.T) {
		const clusterName = "via-proxy"
		const tenantID = crdbv1beta1.TenantID(13)
		tenant := testenv.CreateCrdbTenant(t, ctl, clusterName, &crdbv1beta1.CrdbTenantSpec{
			TenantID: tenantID,
			Pods:     1,
		})
		testenv.ExecKubeSql(t, ctl, kubenet.Options{
			TenantID: crdbv1beta1.TenantSystemID,
		}, "CREATE USER IF NOT EXISTS certuser")

		// Create certuser certificate.
		certuser, err := signer.Sign(ctx, crypto.CertificateSigningRequest{
			Role:       "client",
			CommonName: "certuser",
			TTL:        certificateTTL,
		})
		require.NoError(t, err)
		ca, err := signer.Certificate(ctx)
		require.NoError(t, err)
		tlsConfig, err := crypto.TLSConfigFromBundle(ca, certuser, "")
		require.NoError(t, err)

		// Nil out RootCAs and ensure that kubenet will default it to the root
		// certificate's CA.
		tlsConfig.RootCAs = nil

		testenv.TestKubeSql(t, ctl, kubenet.Options{
			TenantID:  crdbv1beta1.TenantSystemID,
			User:      url.User("certuser"),
			TLSConfig: tlsConfig,
		})

		require.NoError(t, ctl.Delete(ctx, tenant))
	})
}

func TestIntegrationTenantHasEnterpriseLicense(t *testing.T) {
	testutils.MarkIntegrationTest(t)

	env := testenv.New(t, testenv.MultiZoneConfig)
	cluster := testenv.CreateCrdbCluster(t, env, "test", &crdbv1beta1.CrdbClusterSpec{
		TLSEnabled: true,
		TenantHost: &crdbv1beta1.TenantHost{},
	})
	ctl := env.DefaultCluster().NewCtl(cluster.Namespace)

	options := kubenet.Options{
		TenantID: crdbv1beta1.TenantID(1337),
	}
	testenv.CreateCrdbTenant(t, ctl, "happy-koala", &crdbv1beta1.CrdbTenantSpec{
		TenantID: options.TenantID,
		Pods:     1,
	})

	testenv.ExecKubeSql(t, ctl, options, `BACKUP INTO 'userfile:///backup'`)
	testenv.ExecKubeSql(t, ctl, options, `BACKUP INTO LATEST IN 'userfile:///backup'`)
}

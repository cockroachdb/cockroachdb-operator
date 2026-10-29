package clusterctrl

import (
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/betaclusterctrl"
	opmetrics "github.com/cockroachlabs/cockroach-enterprise-operator/controllers/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/migrationctrl"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/evictionutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestReconcilerReconciledClusters(t *testing.T) {
	r := &Reconciler{}
	require.Empty(t, r.mu.reconciledClusters)

	// Add a cluster. Idempotency.
	r.ensureCluster("foo", "bar")
	r.ensureCluster("foo", "bar")
	require.ElementsMatch(t, []types.NamespacedName{
		{Name: "foo", Namespace: "bar"},
	}, r.mu.reconciledClusters)

	// Add a cluster with a different namespace.
	r.ensureCluster("foo", "bar2")
	require.ElementsMatch(t, []types.NamespacedName{
		{Name: "foo", Namespace: "bar"},
		{Name: "foo", Namespace: "bar2"},
	}, r.mu.reconciledClusters)

	// Add a cluster with a different name.
	r.ensureCluster("foo2", "bar2")
	require.ElementsMatch(t, []types.NamespacedName{
		{Name: "foo", Namespace: "bar"},
		{Name: "foo", Namespace: "bar2"},
		{Name: "foo2", Namespace: "bar2"},
	}, r.mu.reconciledClusters)

	// Remove non-existent clusters.
	r.ensureClusterRemoved("foo2", "test")
	r.ensureClusterRemoved("test", "bar2")
	require.ElementsMatch(t, []types.NamespacedName{
		{Name: "foo", Namespace: "bar"},
		{Name: "foo", Namespace: "bar2"},
		{Name: "foo2", Namespace: "bar2"},
	}, r.mu.reconciledClusters)

	// Remove a cluster. Idempotency.
	r.ensureClusterRemoved("foo", "bar2")
	r.ensureClusterRemoved("foo", "bar2")
	require.ElementsMatch(t, []types.NamespacedName{
		{Name: "foo", Namespace: "bar"},
		{Name: "foo2", Namespace: "bar2"},
	}, r.mu.reconciledClusters)

	// Remove remaining clusters.
	r.ensureClusterRemoved("foo", "bar")
	require.ElementsMatch(t, []types.NamespacedName{
		{Name: "foo2", Namespace: "bar2"},
	}, r.mu.reconciledClusters)
	r.ensureClusterRemoved("foo2", "bar2")
	require.Empty(t, r.mu.reconciledClusters)
}

func TestKubernetesEndpointWatchMapper(t *testing.T) {
	r := &Reconciler{}
	mapper := kubernetesEndpointWatcher(r)

	t.Run("non-kubernetes endpoint", func(t *testing.T) {
		require.Nil(t, mapper(&corev1.Endpoints{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "apple",
				Namespace: "default",
			},
		}))

		m := &dto.Metric{}
		require.NoError(t, opmetrics.KubernetesEndpointWatcherCallsTotal.Write(m))
		require.Equal(t, 0, int(*m.Counter.Value))
	})

	t.Run("kubernetes endpoint in non-default namespace", func(t *testing.T) {
		require.Nil(t, mapper(&corev1.Endpoints{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kubernetes",
				Namespace: "us-central1",
			},
		}))

		m := &dto.Metric{}
		require.NoError(t, opmetrics.KubernetesEndpointWatcherCallsTotal.Write(m))
		require.Equal(t, 0, int(*m.Counter.Value))
	})

	t.Run("kubernetes endpoint in default namespace", func(t *testing.T) {
		clusters := []types.NamespacedName{
			{Name: "foobar", Namespace: "ns1"},
			{Name: "baz", Namespace: "ns2"},
		}
		for _, c := range clusters {
			r.ensureCluster(c.Name, c.Namespace)
		}
		defer func() {
			for _, c := range clusters {
				r.ensureClusterRemoved(c.Name, c.Namespace)
			}
		}()

		requests := mapper(&corev1.Endpoints{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kubernetes",
				Namespace: "default",
			},
		})
		require.Len(t, requests, 2)
		require.ElementsMatch(t, []reconcile.Request{
			{
				NamespacedName: types.NamespacedName{
					Name:      "foobar",
					Namespace: "ns1",
				},
			},
			{
				NamespacedName: types.NamespacedName{
					Name:      "baz",
					Namespace: "ns2",
				},
			},
		}, requests)

		m := &dto.Metric{}
		require.NoError(t, opmetrics.KubernetesEndpointWatcherCallsTotal.Write(m))
		require.Equal(t, 1, int(*m.Counter.Value))
	})
}

func TestKubernetesNodeWatcher(t *testing.T) {
	r := &Reconciler{}
	mapper := k8sNodeWatcher(r)

	clusters := []types.NamespacedName{
		{Name: "foobar", Namespace: "ns1"},
		{Name: "baz", Namespace: "ns2"},
	}
	for _, c := range clusters {
		r.ensureCluster(c.Name, c.Namespace)
	}

	queueRequests := mapper(&corev1.Node{})
	require.ElementsMatch(t, []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Name:      "foobar",
				Namespace: "ns1",
			},
		},
		{
			NamespacedName: types.NamespacedName{
				Name:      "baz",
				Namespace: "ns2",
			},
		},
	}, queueRequests)
}

func TestDisableLiveMigrations(t *testing.T) {
	testCases := []struct {
		description          string
		cloudProvider        cloud.Provider
		zone                 string
		featureEnabled       bool
		alreadyDisabled      bool
		disableError         error
		enableError          error
		expectedErrorMessage string
	}{
		{
			description:     "successfully disables live migrations",
			cloudProvider:   cloud.GCP,
			zone:            "us-east-1a",
			featureEnabled:  true,
			alreadyDisabled: false,
		},
		{
			description:     "successfully enables live migrations",
			cloudProvider:   cloud.GCP,
			zone:            "us-east-1a",
			featureEnabled:  false,
			alreadyDisabled: true,
		},
		{
			description:          "errors if disable call to instances client fails",
			cloudProvider:        cloud.GCP,
			zone:                 "us-east-1a",
			featureEnabled:       true,
			alreadyDisabled:      false,
			disableError:         errors.New("a unique error"),
			expectedErrorMessage: "a unique error",
		},
		{
			description:          "errors if enable call to instances client fails",
			cloudProvider:        cloud.GCP,
			zone:                 "us-east-1a",
			featureEnabled:       false,
			alreadyDisabled:      true,
			enableError:          errors.New("a unique error"),
			expectedErrorMessage: "a unique error",
		},
		{
			description:          "errors if zone is blank",
			cloudProvider:        cloud.GCP,
			zone:                 "",
			featureEnabled:       false,
			alreadyDisabled:      true,
			expectedErrorMessage: "couldn't find zone for node",
		},
		{
			description:     "doesn't call instances client if feature is disabled and migrations are not disabled",
			cloudProvider:   cloud.GCP,
			zone:            "us-east-1a",
			featureEnabled:  false,
			alreadyDisabled: false,
			disableError:    errors.New("shouldn't matter"),
			enableError:     errors.New("shouldn't matter"),
		},
		{
			description:     "doesn't call instances client if feature is enabled and migrations are disabled",
			cloudProvider:   cloud.GCP,
			zone:            "us-east-1a",
			featureEnabled:  true,
			alreadyDisabled: true,
			disableError:    errors.New("shouldn't matter"),
			enableError:     errors.New("shouldn't matter"),
		},
		{
			description:     "doesn't call disable if cloud isn't GCP",
			cloudProvider:   cloud.AWS,
			zone:            "us-east-1a",
			featureEnabled:  true,
			alreadyDisabled: false,
			disableError:    errors.New("shouldn't matter"),
		},
		{
			description:     "doesn't call enable if cloud isn't GCP",
			cloudProvider:   cloud.AWS,
			zone:            "us-east-1a",
			featureEnabled:  false,
			alreadyDisabled: true,
			disableError:    errors.New("shouldn't matter"),
		},
	}

	for _, testCase := range testCases {
		env := &cloud.Environment{
			CloudProvider: testCase.cloudProvider,
		}

		cluster := createCrdbCluster(testCase.featureEnabled)

		ctl := kubetest.NewFakeCtl(kubetest.Options{})
		require.NoError(t, ctl.Apply(context.Background(), cluster))
		require.NoError(t, ctl.Apply(context.Background(), createNode(testCase.alreadyDisabled, testCase.zone)))

		r := &Reconciler{
			Ctl:             ctl,
			Env:             env,
			BetaReconciler:  betaclusterctrl.NewReconciler(env, ctl, &evictionutil.Helper{}),
			InstancesClient: &fakeInstancesClient{disableErr: testCase.disableError, enableErr: testCase.enableError},
		}

		_, err := r.Reconcile(context.Background(), controllerruntime.Request{
			NamespacedName: types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace},
		})

		if testCase.expectedErrorMessage != "" {
			require.ErrorContains(t, err, testCase.expectedErrorMessage)
		} else {
			require.NoError(t, err)
		}
	}
}

func TestReconcileWaitsForMigrationHandoff(t *testing.T) {
	env := &cloud.Environment{CloudProvider: cloud.AWS, CloudRegion: "us-east-1"}
	cluster := createCrdbCluster(false)
	mode := crdbv1beta1.MutableOnly
	cluster.Spec.Mode = &mode
	cluster.Generation = 7
	cluster.Labels = map[string]string{
		migrationctrl.LabelMigrate: migrationctrl.MigrateInProgress,
	}
	cluster.Status.Migration = &crdbv1beta1.MigrationStatus{Phase: migrationctrl.PhaseComplete}

	ctl := kubetest.NewFakeCtl(kubetest.Options{}, cluster.DeepCopy())
	r := &Reconciler{
		Ctl:            ctl,
		Env:            env,
		BetaReconciler: betaclusterctrl.NewReconciler(env, ctl, &evictionutil.Helper{}),
	}

	result, err := r.Reconcile(context.Background(), controllerruntime.Request{
		NamespacedName: types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace},
	})
	require.NoError(t, err)
	require.False(t, result.Requeue)
	require.Zero(t, result.RequeueAfter)

	var stored crdbv1beta1.CrdbCluster
	require.NoError(t, ctl.WithNamespace(cluster.Namespace).Get(context.Background(), cluster.Name, &stored))
	require.Zero(t, stored.Status.ObservedGeneration,
		"clusterctrl must not publish status before migrationctrl finishes the handoff")
	require.Empty(t, stored.Status.Region,
		"the beta reconciler's in-memory status changes must not be persisted during handoff")
}

func createCrdbCluster(featureEnabled bool) *crdbv1beta1.CrdbCluster {
	var mode crdbv1beta1.ReconciliationMode = crdbv1beta1.CrdbClusterKind

	features := []crdbv1beta1.ClusterFeature{}
	if featureEnabled {
		features = append(features, crdbv1beta1.ClusterDisableGCPLiveMigrations)
	}

	return &crdbv1beta1.CrdbCluster{
		ObjectMeta: controllerruntime.ObjectMeta{
			Name:      "testCluster",
			Namespace: "test",
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Regions:             []crdbv1beta1.CrdbClusterRegion{{Nodes: 1}},
			Features:            features,
			Mode:                &mode,
			RollingRestartDelay: &metav1.Duration{Duration: 30 * time.Second},
			Template: crdbv1beta1.CrdbNodeTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"cluster": "friendlyId"},
				},
			},
		},
		Status: crdbv1beta1.CrdbClusterStatus{
			Conditions: []crdbv1beta1.ClusterCondition{
				{Type: crdbv1beta1.ClusterInitialized, Status: metav1.ConditionTrue},
			},
		},
	}
}

func createNode(alreadyDisabled bool, zone string) *corev1.Node {
	var annotations map[string]string
	if alreadyDisabled {
		annotations = map[string]string{CrdbNodeLiveMigrationDisabled: "true"}
	}

	labels := map[string]string{"topology.kubernetes.io/zone": zone}

	return &corev1.Node{
		ObjectMeta: controllerruntime.ObjectMeta{
			Name:        "node1",
			Annotations: annotations,
			Labels:      labels,
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

type fakeInstancesClient struct {
	disableErr error
	enableErr  error
}

func (f *fakeInstancesClient) DisableLiveMigrations(
	ctx context.Context, instance, zone string,
) error {
	return f.disableErr
}

func (f *fakeInstancesClient) EnableLiveMigrations(
	ctx context.Context, instance, zone string,
) error {
	return f.enableErr
}

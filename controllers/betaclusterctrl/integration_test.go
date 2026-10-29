package betaclusterctrl_test

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	errors "github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbtest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubenet"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/testenv"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
)

func TestIntegrationBetaClusterCtrl(t *testing.T) {
	testutils.MarkIntegrationTest(t)

	suite.Run(t, new(TestSuite))
}

type TestSuite struct {
	suite.Suite

	env *testenv.Env
	ctl *kube.Ctl
}

func (s *TestSuite) SetupSuite() {
	s.env = testenv.New(s.T(), testenv.MultiZoneConfig, testenv.SkipTenantController)
	ctl := s.env.DefaultCluster().Ctl
	s.ctl = s.env.DefaultCluster().Ctl.WithNamespace(testenv.EnsureNamespace(s.T(), ctl))
}

// TestBasaltStorage exercises secure and insecure CockroachDB connections to
// Basalt. The secure case also covers the normal scale-down and cleanup flow.
func (s *TestSuite) TestBasaltStorage() {
	testCases := []struct {
		name         string
		tenantID     string
		secure       bool
		nodeCount    int32
		decommission bool
	}{
		{
			name:         "secure",
			tenantID:     "6f4d2b84-bd91-4f1d-8a8a-73bb07db7f45",
			secure:       true,
			nodeCount:    2,
			decommission: true,
		},
		{
			name:      "insecure",
			tenantID:  "32583827-d313-4f33-9f1d-6cbc8cf79b05",
			nodeCount: 1,
		},
	}

	for _, tc := range testCases {
		s.T().Run(tc.name, func(t *testing.T) {
			namespace := testenv.EnsureCustomNamespace(t, s.ctl, "basalt-storage-"+tc.name)
			ctl := s.ctl.WithNamespace(namespace)
			basalt := testenv.CreateBasaltCluster(t, ctl, tc.tenantID, tc.secure)

			cluster := &crdbv1beta1.CrdbCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "basalt-cluster", Namespace: namespace},
				Spec: crdbv1beta1.CrdbClusterSpec{
					// CockroachDB transport TLS stays enabled. The secure field
					// controls only the connection between CockroachDB and Basalt.
					TLSEnabled: true,
					Regions: []crdbv1beta1.CrdbClusterRegion{
						{Code: "us-east-1", Nodes: tc.nodeCount, CloudProvider: "k3d"},
					},
				},
			}
			basalt.ConfigureCluster(cluster)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			kubecrdbtest.CreateCluster(t, ctl, cluster)
			require.NoError(t, ctl.WaitFor(ctx, kube.IsQuiescent, cluster),
				"Basalt-backed cluster should become quiescent")

			var applied crdbv1beta1.CrdbCluster
			require.NoError(t, ctl.Get(ctx, cluster.Name, &applied))
			basaltSpec := applied.Spec.Template.Spec.DataStore.Basalt
			require.NotNil(t, basaltSpec)
			require.Equal(t, tc.tenantID, basaltSpec.TenantID)
			require.NotNil(t, basaltSpec.Insecure)
			require.Equal(t, !tc.secure, *basaltSpec.Insecure)
			if tc.secure {
				require.NotNil(t, basaltSpec.TLS)
			} else {
				require.Nil(t, basaltSpec.TLS)
			}

			nodes := kubecrdbtest.ClusterNodes(t, ctl, &applied)
			require.Len(t, nodes.Items, int(tc.nodeCount))
			var sqlNode *crdbv1beta1.CrdbNode
			for i := range nodes.Items {
				if nodes.Items[i].Status.NodeID != nil && len(nodes.Items[i].Status.StoreIDs) > 0 {
					sqlNode = nodes.Items[i].DeepCopy()
					break
				}
			}
			require.NotNil(t, sqlNode, "no ready CrdbNode had Basalt store IDs")

			db := testenv.OpenKubeSql(t, ctl,
				kubenet.Options{TLSConfig: testenv.GetTLS(t, ctl, cluster)})
			defer func() { _ = db.Close() }()
			_, err := db.ExecContext(ctx,
				`CREATE TABLE basalt_storage_test (id INT PRIMARY KEY, value STRING)`)
			require.NoError(t, err)
			_, err = db.ExecContext(ctx,
				`INSERT INTO basalt_storage_test VALUES (1, 'stored-on-basalt')`)
			require.NoError(t, err)
			var value string
			require.NoError(t, db.QueryRowContext(ctx,
				`SELECT value FROM basalt_storage_test WHERE id = 1`).Scan(&value))
			require.Equal(t, "stored-on-basalt", value)

			if tc.decommission {
				testenv.DisableReplication(t, ctl, cluster)
				_ = db.Close()

				decommissionedNode := sqlNode.DeepCopy()
				decommissionedNode.MarkForDecommission()
				require.NoError(t, ctl.Update(ctx, decommissionedNode))

				require.NoError(t, ctl.Get(ctx, cluster.Name, &applied))
				require.Len(t, applied.Spec.Regions, 1)
				for i := range applied.Spec.Regions {
					applied.Spec.Regions[i].Nodes--
				}
				require.NoError(t, ctl.Update(ctx, &applied))
				require.NoError(t, ctl.WaitFor(ctx, kube.IsQuiescent, &applied))
				waitForBasaltNodeCleanup(t, ctx, ctl, decommissionedNode)

				nodes = kubecrdbtest.ClusterNodes(t, ctl, &applied)
				require.Len(t, nodes.Items, 1)
				db = testenv.OpenKubeSql(t, ctl,
					kubenet.Options{TLSConfig: testenv.GetTLS(t, ctl, cluster)})
				value = ""
				require.NoError(t, db.QueryRowContext(ctx,
					`SELECT value FROM basalt_storage_test WHERE id = 1`).Scan(&value))
				require.Equal(t, "stored-on-basalt", value)
			}

			nodes = kubecrdbtest.ClusterNodes(t, ctl, &applied)
			_ = db.Close()
			require.NoError(t, ctl.Delete(ctx, &applied))
			// Wait for every CrdbNode to be deleted before removing the Basalt tenant.
			for i := range nodes.Items {
				require.NoError(t, ctl.WaitFor(ctx, kube.IsDeleted, &nodes.Items[i]))
			}
			require.Empty(t, kubecrdbtest.ClusterNodes(t, ctl, &applied).Items)
			for i := range nodes.Items {
				var job batchv1.Job
				err := ctl.Get(ctx,
					manifests.BasaltStoreCleanupJobName(nodes.Items[i].Name), &job)
				require.True(t, errors.IsNotFound(err),
					"cluster deletion created a cleanup Job for %s", nodes.Items[i].Name)
			}

			basalt.DeleteTenant(t)
		})
	}
}

func waitForBasaltNodeCleanup(
	t *testing.T, ctx context.Context, ctl *kube.Ctl, node *crdbv1beta1.CrdbNode,
) {
	t.Helper()
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      manifests.BasaltStoreCleanupJobName(node.Name),
		Namespace: node.Namespace,
	}}
	require.Eventually(t, func() bool {
		if err := ctl.Get(ctx, job.Name, job); err != nil {
			return false
		}
		return job.Status.Succeeded > 0
	}, 5*time.Minute, time.Second, "Basalt cleanup Job did not succeed for %s", node.Name)
	require.Equal(t, string(node.UID),
		job.Annotations[manifests.BasaltLifecycleResourceUIDAnnotation])
	require.NoError(t, ctl.WaitFor(ctx, kube.IsDeleted, node),
		"CrdbNode finalizer was not released after Basalt cleanup")
}

func (s *TestSuite) TestCreateAndUpdate() {
	const (
		image1 = "us-docker.pkg.dev/cockroach-cloud-images/cockroachdb/cockroach:v22.1.9"
		image2 = "us-docker.pkg.dev/cockroach-cloud-images/cockroachdb/cockroach:v22.1.10"
	)

	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: testenv.EnsureNamespace(s.T(), s.ctl),
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			TLSEnabled: true,
			ClusterSettings: map[string]string{
				"cluster.organization": "testorg",
			},
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east-1", Nodes: 3, CloudProvider: "k3d"},
			},
			Template: crdbv1beta1.CrdbNodeTemplate{
				Spec: crdbv1beta1.CrdbNodeSpec{
					// Use a custom SQL port to verify if cluster settings are
					// being applied correctly.
					SQLPort: new(int32(36257)),
					StartFlags: &crdbv1beta1.Flags{
						Upsert: []string{
							"--max-offset=100ms",
							// Test flag without an argument.
							"--verbose",
						},
					},
					Image: image1,
					PodTemplate: &crdbv1beta1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							InitContainers: []corev1.Container{
								{Name: "test-init", Image: "busybox", Command: []string{"/bin/echo", "initialized"}},
							},
							Containers: []corev1.Container{
								{
									Name: manifests.CRDBContainerName,
									Env: []corev1.EnvVar{{
										Name:  manifests.CockroachChannelEnvVar,
										Value: "CockroachCloud-Test",
									}},
								},
								{Name: "sidecar", Image: "alpine", Command: []string{"/bin/sleep", "infinity"}},
							},
						},
					},
				},
			},
		},
	}

	ctx := context.Background()
	ctl := s.ctl.WithNamespace(cluster.Namespace)

	// Create the cluster
	kubecrdbtest.CreateCluster(s.T(), ctl, cluster)
	s.Require().NoError(ctl.WaitFor(ctx, kube.IsQuiescent, cluster))

	// Validate that cluster settings were applied correctly via the custom SQL port.
	names, err := kubenet.LocatePods(ctx, ctl, &kubenet.Options{
		TenantID: crdbv1beta1.TenantSystemID,
	})
	s.Require().NoError(err, "should be able to locate pods")
	s.Require().GreaterOrEqual(len(names), 1, "should be able to locate at least 1 pod")

	var stdout bytes.Buffer
	err = ctl.Exec(ctx, names[0], "cockroachdb",
		kube.ExecOptions{
			Command: []string{"cockroach", "sql", "--port", "36257", "-e", "SHOW CLUSTER SETTING cluster.organization"},
			Stdout:  &stdout,
		})
	s.Require().NoError(err, "should be able to query cluster setting via custom SQL port")
	s.Require().Contains(stdout.String(), "testorg", "cluster setting should be applied correctly")

	// Update to image2.
	s.Require().NoError(ctl.Get(ctx, cluster.Name, cluster))
	cluster.Spec.Template.Spec.Image = image2
	s.Require().NoError(ctl.Apply(ctx, cluster))
	s.Require().NoError(ctl.WaitFor(ctx, kube.IsQuiescent, cluster))

	// Check the nodes.
	nodes := kubecrdbtest.ClusterNodes(s.T(), ctl, cluster)
	s.Require().Len(nodes.Items, int(cluster.Region().Nodes))

	// Sort by PodRunning condition timestamp to match the assertion below.
	sort.Slice(nodes.Items, func(i, j int) bool {
		statusI, tsI, existsI := nodes.Items[i].Status.GetCondition(crdbv1beta1.PodRunning)
		s.Require().True(existsI)
		s.Require().Equal(metav1.ConditionTrue, statusI)

		statusJ, tsJ, existsJ := nodes.Items[j].Status.GetCondition(crdbv1beta1.PodRunning)
		s.Require().True(existsJ)
		s.Require().Equal(metav1.ConditionTrue, statusJ)

		return tsI.Before(tsJ)
	})

	for i := range len(nodes.Items) - 1 {
		statusA, tsA, existsA := nodes.Items[i].Status.GetCondition(crdbv1beta1.PodRunning)
		s.Require().True(existsA)
		s.Require().Equal(metav1.ConditionTrue, statusA)

		statusB, tsB, existsB := nodes.Items[i+1].Status.GetCondition(crdbv1beta1.PodRunning)
		s.Require().True(existsB)
		s.Require().Equal(metav1.ConditionTrue, statusB)

		// B's PodRunning transition should occur at least after
		// A's PodRunning transition + DefaultRollingRestartDelay.
		// This approximates the eviction helper's behavior,
		// which paces evictions based on the time the previous pod's
		// containers started running (not based on PodReady transition).

		// We use "not after" instead of "before"
		// to allow for the 1s granularity of condition timestamps.
		s.Require().True(!tsA.Add(kubecrdbtest.DefaultRollingRestartDelay).After(tsB))
	}

	// Test update to storage requests, and storage class name.
	// Only storage requests will go through.
	uniqueSize := resource.MustParse("1234Mi")
	cluster.Spec.Template.Spec.DataStore.VolumeClaimTemplate.Spec.Resources.Requests["storage"] = uniqueSize
	nonExistingName := "non-existing-name"
	cluster.Spec.Template.Spec.DataStore.VolumeClaimTemplate.Spec.StorageClassName = &nonExistingName
	s.Require().NoError(ctl.Apply(ctx, cluster))

	var pvcs corev1.PersistentVolumeClaimList
	s.Require().NoError(ctl.List(ctx, &kube.ListOptions{
		Namespace:     cluster.Namespace,
		LabelSelector: kube.ParseLabelOrDie(fmt.Sprintf("%s=%s", manifests.BetaCrdbClusterNodeOwnerLabelKey, cluster.Name)),
	}, &pvcs))
	s.Require().NotEmpty(pvcs.Items)
	for i := range pvcs.Items {
		s.Require().Equal(pvcs.Items[i].Spec.Resources.Requests["storage"], uniqueSize)
	}
}

func (s *TestSuite) TestReconciliationMode() {
	// This is a patch downgrade from the cluster's image.
	image := "us-docker.pkg.dev/cockroach-cloud-images/cockroachdb/cockroach:v22.1.9"
	mode := crdbv1beta1.Disabled

	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: testenv.EnsureNamespace(s.T(), s.ctl),
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			TLSEnabled: true,
			Mode:       &mode,
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east-1", Nodes: 2, CloudProvider: "k3d"},
			},
			Template: crdbv1beta1.CrdbNodeTemplate{
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image: "us-docker.pkg.dev/cockroach-cloud-images/cockroachdb/cockroach:v22.1.10",
				},
			},
		},
	}

	ctx := context.Background()
	ctl := s.ctl.WithNamespace(cluster.Namespace)

	// disabled so we should end up with 0 nodes
	kubecrdbtest.CreateCluster(s.T(), ctl, cluster)
	s.Require().Never(func() bool {
		return len(kubecrdbtest.ClusterNodes(s.T(), ctl, cluster).Items) > 0
	}, 5*time.Second, time.Second)

	// reload cluster with all defaults applied
	s.Require().NoError(ctl.Get(ctx, cluster.Name, cluster))

	// set the mode to MutableOnly to create the cluster
	mode = crdbv1beta1.MutableOnly
	cluster.Spec.Mode = &mode
	s.Require().NoError(ctl.Apply(ctx, cluster))
	s.Require().NoError(ctl.WaitFor(ctx, kube.IsQuiescent, cluster))
	s.Require().Len(kubecrdbtest.ClusterNodes(s.T(), ctl, cluster).Items, 2)

	// set the mode to CreateOnly and bump the number of nodes
	mode = crdbv1beta1.CreateOnly
	cluster.Spec.Mode = &mode
	cluster.Spec.Regions[0].Nodes = 3
	s.Require().NoError(ctl.Apply(ctx, cluster))
	s.Require().NoError(ctl.WaitFor(ctx, kube.IsQuiescent, cluster))
	s.Require().Len(kubecrdbtest.ClusterNodes(s.T(), ctl, cluster).Items, 3)

	// update the image on one of the nodes
	nodes := kubecrdbtest.ClusterNodes(s.T(), ctl, cluster)
	nodes.Items[0].Spec.Image = image
	s.Require().NoError(ctl.Apply(ctx, &nodes.Items[0]))
	s.Require().NoError(ctl.WaitFor(ctx, kube.IsQuiescent, cluster))

	nodesWithUpdateImage := 0
	for _, node := range kubecrdbtest.ClusterNodes(s.T(), ctl, cluster).Items {
		if node.Spec.Image == image {
			s.Require().True(node.Status.HasCondition(crdbv1beta1.PodReady, metav1.ConditionTrue))
			s.Require().True(node.Status.HasCondition(crdbv1beta1.PodRunning, metav1.ConditionTrue))
			nodesWithUpdateImage++
		}
	}
	s.Require().Equal(1, nodesWithUpdateImage)
}

func (s *TestSuite) TestShrinkDiskSize() {
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: testenv.EnsureNamespace(s.T(), s.ctl),
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			TLSEnabled: true,
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east-1", Nodes: 2, CloudProvider: "k3d"},
			},
		},
	}

	ctx := context.Background()
	ctl := s.ctl.WithNamespace(cluster.Namespace)

	t := s.T()
	// create and reload cluster with all defaults applied
	kubecrdbtest.CreateCluster(s.T(), ctl, cluster)
	s.Require().NoError(ctl.Get(ctx, cluster.Name, cluster))
	require.NoError(t, ctl.WaitFor(ctx, kube.IsQuiescent, cluster))

	testenv.DisableReplication(t, ctl, cluster)

	s.refreshCluster(cluster)
	nodeCount := int(cluster.Spec.Regions[0].Nodes)
	nodes := kubecrdbtest.ClusterNodes(t, ctl, cluster)
	require.Len(t, nodes.Items, nodeCount)

	newRequest := resource.MustParse("100Mi")
	require.True(
		t,
		newRequest.Cmp(
			cluster.Spec.Template.Spec.DataStore.VolumeClaimTemplate.Spec.Resources.Requests[corev1.ResourceStorage],
		) < 0,
	)
	cluster.Spec.Template.Spec.DataStore.VolumeClaimTemplate.Spec.Resources.Requests[corev1.ResourceStorage] = newRequest
	require.NoError(t, ctl.Apply(ctx, cluster))
	require.NoError(t, ctl.WaitFor(ctx, kube.IsQuiescent, cluster))

	// We shrink the disk size by creating new nodes, and decommission old ones one-by-one,
	// and confirm that the new node is spinned up with a shrunk disk size.
	// In practice, we can create X nodes then decommission X nodes to save number of times
	// that we touch the manifests at the cost of deploying more hardware (for accommodating
	// X more nodes).
	for range int(cluster.Spec.Regions[0].Nodes) {
		cluster.Spec.Regions[0].Nodes++
		require.NoError(t, ctl.Apply(ctx, cluster))
		require.NoError(t, ctl.WaitFor(ctx, kube.IsQuiescent, cluster))

		nodes := kubecrdbtest.ClusterNodes(t, ctl, cluster)
		sort.Slice(nodes.Items, func(i, j int) bool {
			return (&nodes.Items[i].CreationTimestamp).Before(&nodes.Items[j].CreationTimestamp)
		})
		nodes.Items[0].MarkForDecommission()
		require.NoError(t, ctl.Apply(ctx, &nodes.Items[0]))

		cluster.Spec.Regions[0].Nodes--
		require.NoError(t, ctl.Apply(ctx, cluster))
		require.NoError(t, ctl.WaitFor(ctx, kube.IsQuiescent, cluster))
	}

	nodes = kubecrdbtest.ClusterNodes(t, ctl, cluster)

	for i := range nodes.Items {
		var pvc corev1.PersistentVolumeClaim
		require.NoError(t, s.ctl.WithNamespace(cluster.Namespace).Get(ctx, nodes.Items[i].Name, &pvc))
		require.True(t, newRequest.Equal(*pvc.Spec.Resources.Requests.Storage()))
	}
}

func (s *TestSuite) refreshCluster(cluster *crdbv1beta1.CrdbCluster) {
	require.NoError(s.T(), s.ctl.WithNamespace(cluster.Namespace).Get(context.Background(), cluster.Name, cluster))
}

func (s *TestSuite) TestScaling() {
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: testenv.EnsureNamespace(s.T(), s.ctl),
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			TLSEnabled: true,
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east-1", Nodes: 1, CloudProvider: "k3d"},
			},
		},
	}

	ctx := context.Background()
	ctl := s.ctl.WithNamespace(cluster.Namespace)

	// create and reload cluster with all defaults applied
	kubecrdbtest.CreateCluster(s.T(), ctl, cluster)
	s.Require().NoError(ctl.Get(ctx, cluster.Name, cluster))

	s.T().Run("scaling up", func(t *testing.T) {
		s.refreshCluster(cluster)
		cluster.Spec.Regions[0].Nodes += 2
		require.NoError(t, ctl.Update(ctx, cluster))
		require.NoError(t, ctl.WaitFor(ctx, kube.IsQuiescent, cluster))

		nodes := kubecrdbtest.ClusterNodes(t, ctl, cluster)
		require.Len(t, nodes.Items, int(cluster.Spec.Regions[0].Nodes))

		for _, node := range nodes.Items {
			require.True(t, node.Status.HasCondition(crdbv1beta1.PodReady, metav1.ConditionTrue))
			require.True(t, node.Status.HasCondition(crdbv1beta1.PodRunning, metav1.ConditionTrue))
		}
		requireBalance(t, nodes)
	})

	s.T().Run("decommission node", func(t *testing.T) {
		testenv.DisableReplication(t, ctl, cluster)

		s.refreshCluster(cluster)
		nodeCount := int(cluster.Spec.Regions[0].Nodes)
		nodes := kubecrdbtest.ClusterNodes(t, ctl, cluster)
		require.Len(t, nodes.Items, nodeCount)

		// Save all node names prior to decommissioning a node
		oldNodeNames := getNodeNames(nodes)

		cluster.Spec.Regions[0].Nodes--
		require.NoError(t, ctl.Update(ctx, cluster))
		require.NoError(t, ctl.WaitFor(ctx, kube.IsQuiescent, cluster))
		nodes = kubecrdbtest.ClusterNodes(t, ctl, cluster)
		require.Len(t, nodes.Items, nodeCount-1)

		requireBalance(t, nodes)

		// Save all node names after decommissioning
		newNodeNames := getNodeNames(nodes)

		deletedNodeNames := findDeletedNodeNames(oldNodeNames, newNodeNames)

		for _, nodeName := range deletedNodeNames {
			var node crdbv1beta1.CrdbNode

			node.Name = nodeName
			node.Namespace = cluster.Namespace

			require.NoError(t, ctl.WaitFor(ctx, kube.IsDeleted, &node))

			require.True(t, errors.IsNotFound(ctl.Get(ctx, nodeName, &corev1.Pod{})))
			require.True(t, errors.IsNotFound(ctl.Get(ctx, nodeName, &corev1.PersistentVolumeClaim{})))
		}
	})

	s.T().Run("decommission specific node", func(t *testing.T) {
		testenv.DisableReplication(t, ctl, cluster)

		s.refreshCluster(cluster)
		nodeCount := int(cluster.Spec.Regions[0].Nodes)
		nodes := kubecrdbtest.ClusterNodes(t, ctl, cluster)
		require.Len(t, nodes.Items, nodeCount)
		requireBalance(t, nodes)

		// mark a node for decommissioning
		deletedNode := &nodes.Items[1]

		// check that node 1 has pod and pvc before deletion.
		require.NoError(t, ctl.Get(ctx, deletedNode.Name, &corev1.Pod{}))
		require.NoError(t, ctl.Get(ctx, deletedNode.Name, &corev1.PersistentVolumeClaim{}))

		deletedNode.MarkForDecommission()
		require.NoError(t, ctl.Update(ctx, deletedNode))

		cluster.Spec.Regions[0].Nodes--
		require.NoError(t, ctl.Update(ctx, cluster))
		require.NoError(t, ctl.WaitFor(ctx, kube.IsQuiescent, cluster))

		nodes = kubecrdbtest.ClusterNodes(t, ctl, cluster)
		require.Len(t, nodes.Items, nodeCount-1)
		for _, node := range nodes.Items {
			require.NotEqual(t, deletedNode.Name, node.Name)
		}

		require.NoError(t, ctl.WaitFor(ctx, kube.IsDeleted, deletedNode))

		// check that node 1's pod and pvc have been deleted.
		require.True(t, errors.IsNotFound(ctl.Get(ctx, deletedNode.Name, &corev1.Pod{})))
		require.True(t, errors.IsNotFound(ctl.Get(ctx, deletedNode.Name, &corev1.PersistentVolumeClaim{})))
	})

	s.T().Run("decommission multiple nodes", func(t *testing.T) {
		testenv.DisableReplication(t, ctl, cluster)

		s.refreshCluster(cluster)
		nodeCount := int(cluster.Spec.Regions[0].Nodes)
		nodes := kubecrdbtest.ClusterNodes(t, ctl, cluster)
		require.Len(t, nodes.Items, nodeCount)

		// scale up
		cluster.Spec.Regions[0].Nodes += 2
		require.NoError(t, ctl.Update(ctx, cluster))
		require.NoError(t, ctl.WaitFor(ctx, kube.IsQuiescent, cluster))

		// ensure we're good
		nodes = kubecrdbtest.ClusterNodes(t, ctl, cluster)
		require.Len(t, nodes.Items, nodeCount+2)

		oldNodeNames := getNodeNames(nodes)

		// scale down by 2 nodes
		cluster.Spec.Regions[0].Nodes -= 2
		require.NoError(t, ctl.Update(ctx, cluster))
		require.NoError(t, ctl.WaitFor(ctx, kube.IsQuiescent, cluster))

		// ensure we're good
		nodes = kubecrdbtest.ClusterNodes(t, ctl, cluster)
		require.Len(t, nodes.Items, nodeCount)
		requireBalance(t, nodes)

		newNodeNames := getNodeNames(nodes)

		deletedNodeNames := findDeletedNodeNames(oldNodeNames, newNodeNames)

		// check that nodes that have been deleted don't have existing pods and pvcs.
		for _, nodeName := range deletedNodeNames {
			var node crdbv1beta1.CrdbNode

			node.Name = nodeName
			node.Namespace = cluster.Namespace

			require.NoError(t, ctl.WaitFor(ctx, kube.IsDeleted, &node))

			require.True(t, errors.IsNotFound(ctl.Get(ctx, nodeName, &corev1.Pod{})))
			require.True(t, errors.IsNotFound(ctl.Get(ctx, nodeName, &corev1.PersistentVolumeClaim{})))
		}
	})
}

// Returns a lsit of names (strings) of all nodes in the nodes list.
//
//nolint:unused
func getNodeNames(nodes crdbv1beta1.CrdbNodeList) []string {
	var nodeNames []string

	for _, node := range nodes.Items {
		nodeNames = append(nodeNames, node.Name)
	}

	return nodeNames
}

// Returns a list of names (strings) of all the nodes that are present in oldNodeNames
// but not present in newNodeNames.
//
//nolint:unused
func findDeletedNodeNames(oldNodeNames []string, newNodeNames []string) []string {
	newSet := make(map[string]struct{}, len(newNodeNames))

	for _, cur := range newNodeNames {
		newSet[cur] = struct{}{}
	}

	var result []string

	for _, cur := range oldNodeNames {
		_, found := newSet[cur]

		if !found {
			result = append(result, cur)
		}
	}

	return result
}

func requireBalance(t *testing.T, nodes crdbv1beta1.CrdbNodeList) {
	zones := map[string]int{}
	largest := 0
	for i := range nodes.Items {
		key := nodes.Items[i].Status.GetTopologyValue()
		zones[key]++
		if zones[key] > largest {
			largest = zones[key]
		}
	}
	for i := range nodes.Items {
		key := nodes.Items[i].Status.GetTopologyValue()
		require.LessOrEqual(t, largest-1, zones[key])
	}
}

func TestIntegrationMultiRegion(t *testing.T) {
	testutils.MarkFlakyTest(t, "CC-30487")
	testutils.MarkIntegrationTest(t)

	region1 := "us-east-1"
	region2 := "us-east-2"
	config := testenv.EnvConfig{
		Name:  "multi-region",
		Reuse: true,
		Regions: map[string]testenv.Zones{
			region1: {
				"us-east-1a": 1,
				"us-east-1b": 1,
				"us-east-1c": 1,
			},
			region2: {
				"us-east-2a": 1,
			},
		},
	}
	env := testenv.New(t, config)
	ctx := context.Background()

	ctl1 := env.Clusters[region1].Ctl
	ctl2 := env.Clusters[region2].Ctl

	namespace := testenv.EnsureNamespace(t, ctl1)
	_ = testenv.EnsureNamespace(t, ctl2)
	ctl1 = ctl1.WithNamespace(namespace)
	ctl2 = ctl2.WithNamespace(namespace)

	const (
		name  = "test-cluster"
		image = "us-docker.pkg.dev/cockroach-cloud-images/cockroachdb/cockroach:v21.2.15"
	)
	spec := crdbv1beta1.CrdbClusterSpec{
		TLSEnabled: true,
		Regions: []crdbv1beta1.CrdbClusterRegion{
			{
				CloudProvider: "k3d",
				Code:          region1,
				Namespace:     namespace,
				Nodes:         3,
				Domain:        fmt.Sprintf("cluster.%s.%s", testenv.TestEnvShortID, region1),
			},
			{
				CloudProvider: "k3d",
				Code:          region2,
				Nodes:         1,
				Namespace:     namespace,
				Domain:        fmt.Sprintf("cluster.%s.%s", testenv.TestEnvShortID, region2),
			},
		},
		Template: crdbv1beta1.CrdbNodeTemplate{
			Spec: crdbv1beta1.CrdbNodeSpec{
				Image: image,
			},
		},
	}
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: spec,
	}
	// Copy so that resource versions, etc. is not filled for the creation in the second region.
	cluster2 := cluster.DeepCopy()
	kubecrdbtest.CreateCluster(t, ctl1, cluster)
	kubecrdbtest.CreateCluster(t, ctl2, cluster2)

	requireRegion := func(ctl *kube.Ctl, region string) func() bool {
		return func() bool {
			var podList corev1.PodList
			require.NoError(t, ctl.List(ctx,
				&kube.ListOptions{
					LabelSelector: kube.ParseLabelOrDie(fmt.Sprintf("%s=%s",
						manifests.BetaCrdbClusterNodeOwnerLabelKey, name)),
				},
				&podList),
			)
			if len(podList.Items) == 0 {
				return false
			}
			var stdout bytes.Buffer
			// TODO(ye): figure out what is required to use kubenet package, instead
			//  of directly exec-ing into the container.
			err := ctl.Exec(ctx, podList.Items[0].Name, "cockroachdb",
				kube.ExecOptions{
					Command: []string{"cockroach", "sql", "-e", "show regions from cluster"},
					Stdout:  &stdout,
				})
			if err != nil {
				return false
			}
			reg := regexp.MustCompile(`\s+`)
			expected := reg.ReplaceAllString(fmt.Sprintf(`
region zones
%s
`, region), "")
			return expected == reg.ReplaceAllString(stdout.String(), "")
		}
	}

	multiRegionString := `
k3d-us-east-1 {k3d-us-east-1a,k3d-us-east-1b,k3d-us-east-1c}
k3d-us-east-2 {k3d-us-east-2a}
 `

	require.Eventually(t, requireRegion(ctl1, multiRegionString), 150*time.Second, 10*time.Second)
	require.Eventually(t, requireRegion(ctl2, multiRegionString), 150*time.Second, 10*time.Second)

	// After we fully scale down the second region, we should expect only 1 region exists.
	cluster2.Spec.Regions[1].Nodes = 0
	require.NoError(t, ctl2.Apply(ctx, cluster2))
	require.Eventually(
		t,
		requireRegion(ctl1, "k3d-us-east-1 {k3d-us-east-1a,k3d-us-east-1b,k3d-us-east-1c}"),
		2*time.Minute,
		10*time.Second,
	)
}

func TestIntegrationPlacementFeatureFlag(t *testing.T) {
	testutils.MarkIntegrationTest(t)

	env := testenv.New(t, testenv.MultiZoneConfig)
	cluster := testenv.CreateCrdbCluster(t, env, "crdb", &crdbv1beta1.CrdbClusterSpec{
		TLSEnabled: true,
	})

	originalFeatures := slices.Clone(cluster.Spec.Features)

	ctx := context.Background()
	ctl := env.DefaultCluster().NewCtl(cluster.Namespace)

	checkAssignment := func() (assignedNodes int, assignmentError error) {
		var nodeList crdbv1beta1.CrdbNodeList
		require.NoError(t, ctl.List(ctx, &kube.ListOptions{
			Namespace: cluster.Namespace,
		}, &nodeList))

		for _, node := range nodeList.Items {
			if node.Spec.NodeName != "" {
				assignedNodes += 1

				var pod corev1.Pod
				err := ctl.Get(ctx, node.Name, &pod)
				if errors.IsNotFound(err) {
					return 0, fmt.Errorf("unable to find pod for '%s'", node.Name)
				}
				require.NoError(t, err)

				if pod.Spec.NodeName != node.Spec.NodeName {
					return 0, fmt.Errorf(
						"pod '%s' is on node '%s' when it should be on '%s'",
						pod.Name, pod.Spec.NodeName, node.Spec.NodeName)
				}
			}
		}
		return assignedNodes, nil
	}

	// Ensure there are no nodes assigned
	assignedNodes, err := checkAssignment()
	require.Equal(t, assignedNodes, 0)
	require.NoError(t, err)

	// Set the feature flag
	cluster.Spec.Features = append(cluster.Spec.Features, crdbv1beta1.ClusterExplicitNodePlacement)
	require.NoError(t, ctl.Apply(ctx, cluster))

	require.Eventually(t, func() bool {
		assignedNodes, assignmentError := checkAssignment()
		if assignmentError != nil {
			t.Logf("waiting for a valid assigment: '%s'", assignmentError)
		}

		if assignedNodes != 3 {
			t.Logf("waiting for all nodes to have an assigment (%d / 3 found)", assignedNodes)
		}

		return true
	}, 2*time.Minute, 10*time.Second)

	// Remove the feature flag
	cluster.Spec.Features = originalFeatures
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		assignedNodes, err := checkAssignment()
		if assignedNodes == 0 {
			t.Logf("waiting for all nodes to have their assigment removed (%d assigments found)", assignedNodes)
			return false
		}
		require.NoError(t, err)
		return true
	}, 2*time.Minute, 10*time.Second)
}

func TestIntegrationWALFailover(t *testing.T) {
	testutils.MarkIntegrationTest(t)

	env := testenv.New(t, testenv.MultiZoneConfig)
	ctx := context.Background()

	crdbSpec := &crdbv1beta1.CrdbClusterSpec{
		TLSEnabled: true,
		Template: crdbv1beta1.CrdbNodeTemplate{
			Spec: crdbv1beta1.CrdbNodeSpec{
				Image: "us-docker.pkg.dev/cockroach-cloud-images/cockroachdb/cockroach:v24.1.0-cloudonly.1",
			},
		},
	}

	cluster := testenv.CreateCrdbCluster(t, env, "crdb", crdbSpec)

	ctl := env.DefaultCluster().NewCtl(cluster.Namespace)

	// Create the cluster
	kubecrdbtest.CreateCluster(t, ctl, cluster)

	require.NoError(t, ctl.WaitFor(ctx, kube.IsQuiescent, cluster))

	// Update to image2.
	require.NoError(t, ctl.Get(ctx, cluster.Name, cluster))
	cluster.Spec.Template.Spec.WALFailoverSpec = &crdbv1beta1.CrdbWalFailoverSpec{
		Status: crdbv1beta1.WalEnable,
		Size:   "25Mi",
	}
	require.NoError(t, ctl.Apply(ctx, cluster))
	require.NoError(t, ctl.WaitFor(ctx, kube.IsQuiescent, cluster))

	// Get the nodes
	var nodeList crdbv1beta1.CrdbNodeList
	require.NoError(t, ctl.List(ctx, &kube.ListOptions{
		Namespace: cluster.Namespace,
	}, &nodeList))

	for _, node := range nodeList.Items {
		if node.Spec.NodeName != "" {

			var pod corev1.Pod
			err := ctl.Get(ctx, node.Name, &pod)
			if errors.IsNotFound(err) {
				t.Errorf("unable to find pod for '%s'", node.Name)
			}
			require.NoError(t, err)

			if pod.Spec.NodeName != node.Spec.NodeName {
				t.Errorf("pod '%s' is on node '%s' when it should be on '%s'",
					pod.Name, pod.Spec.NodeName, node.Spec.NodeName)
			}

			// Check if it has a wal failover volume
			walFound := false
			for _, volume := range pod.Spec.Volumes {
				if walFound = volume.Name == manifests.WalDirVolumeName; walFound {
					var pvc corev1.PersistentVolumeClaim
					err := ctl.Get(ctx, volume.PersistentVolumeClaim.ClaimName, &pvc)
					if errors.IsNotFound(err) {
						t.Errorf("unable to find wal pvc for '%s'", node.Name)
					}
					require.NoError(t, err)
					break
				}
			}
			require.True(t, walFound)

			for _, container := range pod.Spec.Containers {
				if container.Name == manifests.CRDBContainerName {
					// Check for default WAL mount path since no custom path is configured
					expectedWalPath := manifests.DefaultWalMountPath
					if node.Spec.WALFailoverSpec != nil && node.Spec.WALFailoverSpec.Path != "" {
						expectedWalPath = node.Spec.WALFailoverSpec.Path
					}
					require.True(t, strings.Contains(container.Command[2], expectedWalPath))
				}
			}
		}
	}
}

// TestIntegrationK8sNodeDecommission tests decommissioning nodes when the
// underlying k8s node is annotated with K8sNodeStartDecommissionAnnotation.
func TestIntegrationK8sNodeDecommission(t *testing.T) {
	testutils.MarkIntegrationTest(t)

	env := testenv.New(t, testenv.MultiZoneConfig, testenv.SkipTenantController, testenv.EnableNodeController)
	ctx := context.Background()

	const (
		clusterName   = "test-k8s-node-decommission"
		initialNodes  = 6
		deployTimeout = 10 * time.Minute
		cloudImage    = "us-docker.pkg.dev/cockroach-cloud-images/cockroachdb/cockroach:v24.1.0-cloudonly.1"
	)

	// We leverage podTemplate to override the default anti-affinity rules to
	// ensure that we have 6 crdb nodes provisioned on the 3 node k3d cluster
	clusterSpec := &crdbv1beta1.CrdbClusterSpec{
		TLSEnabled: true,
		Template: crdbv1beta1.CrdbNodeTemplate{
			Spec: crdbv1beta1.CrdbNodeSpec{
				Image: cloudImage,
				PodTemplate: &crdbv1beta1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Affinity: &corev1.Affinity{
							PodAntiAffinity: &corev1.PodAntiAffinity{
								PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
									{
										Weight: 100,
										PodAffinityTerm: corev1.PodAffinityTerm{
											LabelSelector: &metav1.LabelSelector{
												MatchLabels: map[string]string{
													"app": "cockroachdb",
												},
											},
											TopologyKey: corev1.LabelHostname,
										},
									},
								},
							},
						},
						Containers: []corev1.Container{
							{
								Name:  manifests.CRDBContainerName,
								Image: cloudImage,
							},
						},
					},
				},
			},
		},
		Regions: []crdbv1beta1.CrdbClusterRegion{
			{Code: "us-east-1", Nodes: initialNodes, CloudProvider: "k3d"},
		},
	}

	cluster := testenv.CreateCrdbCluster(t, env, "crdb", clusterSpec)

	ctx, cancel := context.WithTimeout(context.Background(), deployTimeout)
	defer cancel()

	ctl := env.DefaultCluster().NewCtl(cluster.Namespace)

	t.Logf("creating cluster '%s' with %d nodes", clusterName, initialNodes)
	kubecrdbtest.CreateCluster(t, ctl, cluster)
	require.NoError(t, ctl.WaitFor(ctx, kube.IsQuiescent, cluster), "failed to create initial %d-node cluster", initialNodes)

	initialNodesList := kubecrdbtest.ClusterNodes(t, ctl, cluster)
	require.Len(t, initialNodesList.Items, initialNodes)

	initialPods := &corev1.PodList{}
	require.NoError(t, ctl.List(ctx, &kube.ListOptions{
		Namespace: cluster.Namespace,
	}, initialPods))

	t.Logf("annotating k8s node with %s to trigger decommission", crdbv1beta1.K8sNodeStartDecommissionAnnotation)
	var k8sNodes corev1.NodeList
	require.NoError(t, ctl.List(context.Background(), &kube.ListOptions{
		FieldSelector: fields.ParseSelectorOrDie("spec.unschedulable=false"),
	}, &k8sNodes))
	decommissioningK8sNode := k8sNodes.Items[0]
	var decommissioningCrdbNodes []string
	for _, pod := range initialPods.Items {
		if pod.Spec.NodeName == decommissioningK8sNode.Name {
			decommissioningCrdbNodes = append(decommissioningCrdbNodes, pod.Name)
		}
	}
	require.NoError(t, ctl.Get(ctx, decommissioningK8sNode.Name, &decommissioningK8sNode))
	nodeAnnotations := decommissioningK8sNode.GetAnnotations()
	nodeAnnotations[crdbv1beta1.K8sNodeStartDecommissionAnnotation] = "true"
	decommissioningK8sNode.SetAnnotations(nodeAnnotations)
	require.NoError(t, ctl.Update(ctx, &decommissioningK8sNode), "failed to update k8s node")

	// Wait for the k8s node to be cordoned
	require.Eventually(t, func() bool {
		var updatedNode corev1.Node
		if err := ctl.Get(ctx, decommissioningK8sNode.Name, &updatedNode); err != nil {
			t.Logf("failed to get updated k8s node: %v", err)
			return false
		}
		return updatedNode.Spec.Unschedulable
	}, 2*time.Minute, 5*time.Second, "k8s node should be cordoned after annotating with decommission annotation")

	require.NoError(t, ctl.WaitFor(ctx, kube.IsQuiescent, cluster), "cluster failed to reach quiescent state after scaling down")

	for _, nodeName := range decommissioningCrdbNodes {
		var node crdbv1beta1.CrdbNode
		require.NoError(t, ctl.Get(ctx, nodeName, &node), "failed to get crdb node %s", nodeName)
		require.NoError(t, ctl.WaitFor(ctx, kube.IsDeleted, &node), "timed out waiting for decommissioned node %s to be deleted", nodeName)
		require.True(t, errors.IsNotFound(ctl.Get(ctx, nodeName, &corev1.Pod{})), "pod for decommissioned node %s still exists", nodeName)
		require.True(t, errors.IsNotFound(ctl.Get(ctx, nodeName, &corev1.PersistentVolumeClaim{})), "pvc for decommissioned node %s still exists", nodeName)
	}

	t.Logf("k8s node decommission is successful")
}

// TestSelfSignedCerts verifies that a cluster with certificates.selfSigned:true
// reaches quiescent state and that the operator creates all expected cert
// resources with the managed-by label and an OwnerReference to the cluster.
func (s *TestSuite) TestSelfSignedCerts() {
	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "self-signed-cluster",
			Namespace: testenv.EnsureNamespace(s.T(), s.ctl),
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			TLSEnabled: true,
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east-1", Nodes: 3, CloudProvider: "k3d"},
			},
			Template: crdbv1beta1.CrdbNodeTemplate{
				Spec: crdbv1beta1.CrdbNodeSpec{
					Certificates: crdbv1beta1.Certificates{
						SelfSigned: true,
					},
				},
			},
		},
	}

	ctx := context.Background()
	ctl := s.ctl.WithNamespace(cluster.Namespace)

	// The operator generates certs during reconciliation for self-signed
	// clusters; CreateCluster skips external cert pre-creation automatically.
	kubecrdbtest.CreateCluster(s.T(), ctl, cluster)
	s.Require().NoError(ctl.WaitFor(ctx, kube.IsQuiescent, cluster))

	const managedByLabel = "app.kubernetes.io/managed-by"
	const managedByValue = "cockroach-operator"

	// Verify the CA ConfigMap was created and labeled as operator-managed.
	var caConfigMap corev1.ConfigMap
	s.Require().NoError(ctl.Get(ctx, manifests.SelfSignedCAConfigMapName(cluster.Name), &caConfigMap))
	s.Require().Equal(managedByValue, caConfigMap.Labels[managedByLabel])
	s.Require().Contains(caConfigMap.Data, "ca.crt")

	// Verify the node cert secret was created and labeled as operator-managed.
	var nodeCertSecret corev1.Secret
	s.Require().NoError(ctl.Get(ctx, manifests.SelfSignedNodeCertSecretName(cluster.Name), &nodeCertSecret))
	s.Require().Equal(managedByValue, nodeCertSecret.Labels[managedByLabel])
	s.Require().Contains(nodeCertSecret.Data, "tls.crt")
	s.Require().Contains(nodeCertSecret.Data, "tls.key")

	// Verify the root client cert secret was created and labeled as operator-managed.
	var rootCertSecret corev1.Secret
	s.Require().NoError(ctl.Get(ctx, manifests.SelfSignedRootClientSecretName(cluster.Name), &rootCertSecret))
	s.Require().Equal(managedByValue, rootCertSecret.Labels[managedByLabel])
	s.Require().Contains(rootCertSecret.Data, "tls.crt")
	s.Require().Contains(rootCertSecret.Data, "tls.key")

	// All cert resources must carry an OwnerReference to the cluster so they
	// are garbage-collected when the cluster is deleted.
	s.Require().Len(caConfigMap.OwnerReferences, 1)
	s.Require().Equal(cluster.Name, caConfigMap.OwnerReferences[0].Name)
	s.Require().Len(nodeCertSecret.OwnerReferences, 1)
	s.Require().Equal(cluster.Name, nodeCertSecret.OwnerReferences[0].Name)
	s.Require().Len(rootCertSecret.OwnerReferences, 1)
	s.Require().Equal(cluster.Name, rootCertSecret.OwnerReferences[0].Name)
}

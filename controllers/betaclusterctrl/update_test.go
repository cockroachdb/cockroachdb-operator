package betaclusterctrl

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/datadriven"
	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbnode"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/utils/timeutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/util/evictionutil"
	"github.com/cockroachlabs/version"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
)

func TestChangedJSONPaths(t *testing.T) {
	t.Run("scalar fields changed with before and after values", func(t *testing.T) {
		previous := crdbv1beta1.CrdbNodeSpec{
			Image: "cockroachdb/cockroach:v24.1.0",
		}
		current := previous
		current.TLSEnabled = true
		current.Image = "cockroachdb/cockroach:v24.2.0"

		changes := changedJSONPaths("spec", previous, current)
		require.Len(t, changes, 2)
		require.Equal(t, "spec.image", changes[0].path)
		require.Equal(t, "cockroachdb/cockroach:v24.1.0", changes[0].previous)
		require.Equal(t, "cockroachdb/cockroach:v24.2.0", changes[0].current)
		require.Equal(t, "spec.tlsEnabled", changes[1].path)
		require.Nil(t, changes[1].previous)
		require.Equal(t, true, changes[1].current)
	})

	t.Run("no changes returns empty", func(t *testing.T) {
		spec := crdbv1beta1.CrdbNodeSpec{Image: "cockroachdb/cockroach:v24.1.0"}
		require.Empty(t, changedJSONPaths("spec", spec, spec))
	})

	t.Run("nested object changed", func(t *testing.T) {
		previous := crdbv1beta1.CrdbNodeSpec{
			Image: "cockroachdb/cockroach:v24.1.0",
			Certificates: crdbv1beta1.Certificates{
				ExternalCertificates: &crdbv1beta1.ExternalCertificates{
					NodeSecretName: "old-certs",
				},
			},
		}
		current := *previous.DeepCopy()
		current.Certificates.ExternalCertificates.NodeSecretName = "new-certs"

		changes := changedJSONPaths("spec", previous, current)
		require.Len(t, changes, 1)
		require.Equal(t, "spec.certificates.externalCertificates.nodeSecretName", changes[0].path)
		require.Equal(t, "old-certs", changes[0].previous)
		require.Equal(t, "new-certs", changes[0].current)
	})

	t.Run("map value changed", func(t *testing.T) {
		previous := crdbv1beta1.CrdbNodeSpec{
			Flags: map[string]string{"--cache": "25%"},
		}
		current := previous
		current.Flags = map[string]string{"--cache": "50%"}

		changes := changedJSONPaths("spec", previous, current)
		require.Len(t, changes, 1)
		require.Equal(t, "spec.flags.--cache", changes[0].path)
		require.Equal(t, "25%", changes[0].previous)
		require.Equal(t, "50%", changes[0].current)
	})

	t.Run("field added", func(t *testing.T) {
		previous := crdbv1beta1.CrdbNodeSpec{}
		current := crdbv1beta1.CrdbNodeSpec{
			Image: "cockroachdb/cockroach:v24.1.0",
		}

		changes := changedJSONPaths("spec", previous, current)
		require.Len(t, changes, 1)
		require.Equal(t, "spec.image", changes[0].path)
		require.Nil(t, changes[0].previous)
		require.Equal(t, "cockroachdb/cockroach:v24.1.0", changes[0].current)
	})

	t.Run("field removed", func(t *testing.T) {
		previous := crdbv1beta1.CrdbNodeSpec{
			Image: "cockroachdb/cockroach:v24.1.0",
		}
		current := crdbv1beta1.CrdbNodeSpec{}

		changes := changedJSONPaths("spec", previous, current)
		require.Len(t, changes, 1)
		require.Equal(t, "spec.image", changes[0].path)
		require.Equal(t, "cockroachdb/cockroach:v24.1.0", changes[0].previous)
		require.Nil(t, changes[0].current)
	})

	t.Run("formatChanges produces readable output", func(t *testing.T) {
		changes := []specChange{
			{path: "spec.image", previous: "v24.1.0", current: "v24.2.0"},
			{path: "spec.tlsEnabled", previous: nil, current: true},
			{path: "spec.flags.--cache", previous: "25%", current: nil},
		}
		require.Equal(t,
			`spec.image (v24.1.0 → v24.2.0), spec.tlsEnabled (added: true), spec.flags.--cache (removed: 25%)`,
			formatChanges(changes))
	})
}

// This test simulates the output from RollingUpdate given nodes in various
// states.
func TestRollingUpdate(t *testing.T) {
	ctx := context.Background()
	if testing.Verbose() {
		ctx = logging.WithLogger(ctx, zaptest.NewLogger(t))
	}

	nodePool := &NodePool{
		crdbToK8s: map[string]string{
			"test-cluster-0": "k8s-node-0",
			"test-cluster-1": "k8s-node-1",
			"test-cluster-2": "k8s-node-2",
		},
	}

	noop := func(t *testing.T, prev, curr *crdbv1beta1.CrdbNode) {
		require.Equal(t, prev, curr)
	}

	for _, tc := range []struct {
		name         string
		mutateNode   func(node *crdbv1beta1.CrdbNode) *crdbv1beta1.CrdbNode
		expectedCond func(t *testing.T, prev, curr *crdbv1beta1.CrdbNode)
	}{
		{
			name: "noop all nodes up to date",
			mutateNode: func(node *crdbv1beta1.CrdbNode) *crdbv1beta1.CrdbNode {
				return node
			},
			expectedCond: noop,
		},

		{
			name: "node gets scheduled on new node",
			mutateNode: func(node *crdbv1beta1.CrdbNode) *crdbv1beta1.CrdbNode {
				if node.Name == "test-cluster-2" {
					// Assign to a different node.
					node.Spec.NodeName = "k8s-node-8"
				}
				return node
			},
			expectedCond: func(t *testing.T, prev, curr *crdbv1beta1.CrdbNode) {
				// Node test-cluster-2 should have been rescheduled on the right node.
				if curr.Name == "test-cluster-2" {
					require.NotEqual(t, prev, curr)
				} else {
					require.Equal(t, prev, curr)
				}
			},
		},

		{
			name: "unscheduled node gets scheduled regardless of unready pod",
			mutateNode: func(node *crdbv1beta1.CrdbNode) *crdbv1beta1.CrdbNode {
				switch node.Name {
				case "test-cluster-0":
					// Clear the ready condition set by default. To check that pod scheduling
					// takes precedence over unready pods.
					node.Status.Conditions = []crdbv1beta1.NodeCondition{{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue, LastTransitionTime: metav1.NewTime(timeutils.Now())}}
				case "test-cluster-2":
					// Clear the ready and pod running conditions set by default.
					node.Status.Conditions = []crdbv1beta1.NodeCondition{}
					// Schedule node on a different node.
					node.Spec.NodeName = "k8s-node-8"
				}
				return node
			},
			expectedCond: func(t *testing.T, prev, curr *crdbv1beta1.CrdbNode) {
				// Node test-cluster-2 should have been rescheduled on the right node.
				if curr.Name == "test-cluster-2" {
					require.NotEqual(t, prev, curr)
				} else {
					require.Equal(t, prev, curr)
				}
			},
		},
		{
			name: "unknown revision on two nodes",
			mutateNode: func(node *crdbv1beta1.CrdbNode) *crdbv1beta1.CrdbNode {
				// Set the annotation to an unexpected value.
				if node.Name == "test-cluster-0" || node.Name == "test-cluster-1" {
					node.Annotations[manifests.BetaCrdbClusterNodeRevisionAnnotationKey] = "2"
				}
				return node
			},
			expectedCond: func(t *testing.T, prev, curr *crdbv1beta1.CrdbNode) {
				// Both nodes with unexpected annotations should have had Apply() called, and so
				// prev and curr should differ.
				if curr.Name == "test-cluster-0" || curr.Name == "test-cluster-1" {
					require.NotEqual(t, prev, curr)
				} else {
					require.Equal(t, prev, curr)
				}
			},
		},
		{
			// Nodectrl clears PodNeedsUpdate for decommissioning nodes (see
			// ensurePod), but a stale True from an earlier reconcile would
			// otherwise block the whole cluster's rolling update at the gate.
			name: "decommissioning node with PodNeedsUpdate does not block cluster",
			mutateNode: func(node *crdbv1beta1.CrdbNode) *crdbv1beta1.CrdbNode {
				if node.Name == "test-cluster-2" {
					node.Status.SetCondition(crdbv1beta1.PodNeedsUpdate, metav1.ConditionTrue)
					node.Annotations[crdbv1beta1.CrdbNodeStartDecommissionAnnotation] = "5"
				}
				return node
			},
			expectedCond: noop,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster, ctl := newCluster(t, ctx)

			nodes := crdbv1beta1.CrdbNodeList{}
			require.NoError(t, ctl.List(ctx, &kube.ListOptions{}, &nodes))

			for i := range nodes.Items {
				nodes.Items[i] = *tc.mutateNode(&nodes.Items[i])
			}

			r := Reconciler{
				Ctl:            ctl,
				EvictionHelper: &evictionutil.Helper{},
				Emit:           kube.EventEmitter{EventRecorder: record.NewFakeRecorder(10)},
				NodeExecutor:   StubNodeExecutorFactory(0),
			}

			updatedNodes := *nodes.DeepCopy()
			_, err := r.RollingUpdate(ctx, cluster, updatedNodes, nodePool)
			require.NoError(t, err)

			for i := range nodes.Items {
				tc.expectedCond(t, &nodes.Items[i], &updatedNodes.Items[i])
			}
		})
	}
}

// TestRollingUpdateRefreshesUnderReplicatedCondition checks that a stale
// RangesUnderReplicated=True clears on its own once the ranges recover, even
// with no roll in flight, and that a healthy cluster still avoids the query.
func TestRollingUpdateRefreshesUnderReplicatedCondition(t *testing.T) {
	ctx := context.Background()
	if testing.Verbose() {
		ctx = logging.WithLogger(ctx, zaptest.NewLogger(t))
	}
	nodePool := &NodePool{
		crdbToK8s: map[string]string{
			"test-cluster-0": "k8s-node-0",
			"test-cluster-1": "k8s-node-1",
			"test-cluster-2": "k8s-node-2",
		},
	}

	newReconciler := func(ctl *kube.Ctl, underReplicated int64) Reconciler {
		return Reconciler{
			Ctl:            ctl,
			EvictionHelper: &evictionutil.Helper{},
			Emit:           kube.EventEmitter{EventRecorder: record.NewFakeRecorder(10)},
			NodeExecutor:   StubNodeExecutorFactory(underReplicated),
		}
	}

	t.Run("single-node cluster skips the query and still rolls", func(t *testing.T) {
		cluster, ctl := newCluster(t, ctx)
		cluster.Spec.Regions[0].Nodes = 1

		previousRevision := cluster.Status.CurrentRevision
		cluster.Spec.Template.Spec.Image = "cockroachdb/cockroach:v24.1.1"
		cluster.Status.PreviousRevision = previousRevision
		cluster.Status.CurrentRevision = manifests.GetClusterRevision(cluster)
		require.NotEqual(t, previousRevision, cluster.Status.CurrentRevision)

		var nodes crdbv1beta1.CrdbNodeList
		require.NoError(t, ctl.List(ctx, &kube.ListOptions{}, &nodes))
		require.NotEmpty(t, nodes.Items)
		// Keep one fixture node as the sole cluster member. The shared node pool
		// contains placement entries for every fixture node.
		nodes.Items = nodes.Items[:1]
		soleNode := &nodes.Items[0]

		// If the query runs, this count would block the roll and record
		// UnderReplicated instead of CheckSkipped.
		r := newReconciler(ctl, 5)
		_, err := r.RollingUpdate(ctx, cluster, nodes, nodePool)
		require.NoError(t, err)

		require.Equal(t,
			cluster.Status.CurrentRevision,
			soleNode.Annotations[manifests.BetaCrdbClusterNodeRevisionAnnotationKey],
			"the sole node should have been rolled",
		)
		condition, ok := cluster.Status.FindCondition(crdbv1beta1.ClusterRangesUnderReplicated)
		require.True(t, ok)
		require.Equal(t, metav1.ConditionFalse, condition.Status)
		require.Equal(t, "CheckSkipped", condition.Reason)
	})

	t.Run("clears a stale True once ranges recover with no roll pending", func(t *testing.T) {
		cluster, ctl := newCluster(t, ctx)
		// Pretend an earlier rollout left the condition stuck on True. Every node is
		// up to date so nothing needs rolling, and the query comes back with 0, so
		// the gate should flip the condition back to False on its own.
		cluster.Status.SetConditionWithMessage(
			crdbv1beta1.ClusterRangesUnderReplicated, metav1.ConditionTrue, "UnderReplicated",
			"82 range(s) are under-replicated",
		)
		nodes := crdbv1beta1.CrdbNodeList{}
		require.NoError(t, ctl.List(ctx, &kube.ListOptions{}, &nodes))

		r := newReconciler(ctl, 0)
		_, err := r.RollingUpdate(ctx, cluster, nodes, nodePool)
		require.NoError(t, err)

		cond, ok := cluster.Status.FindCondition(crdbv1beta1.ClusterRangesUnderReplicated)
		require.True(t, ok)
		require.Equal(t, metav1.ConditionFalse, cond.Status)
		require.Equal(t, "AllReplicated", cond.Reason)
	})

	t.Run("skips the query on a healthy steady cluster", func(t *testing.T) {
		cluster, ctl := newCluster(t, ctx)
		// Nothing is rolling and the condition was never set. The stub would report
		// 5 under-replicated ranges if the gate reached the query, so a condition
		// that's still unset afterward tells us we skipped it.
		nodes := crdbv1beta1.CrdbNodeList{}
		require.NoError(t, ctl.List(ctx, &kube.ListOptions{}, &nodes))

		r := newReconciler(ctl, 5)
		_, err := r.RollingUpdate(ctx, cluster, nodes, nodePool)
		require.NoError(t, err)

		_, ok := cluster.Status.FindCondition(crdbv1beta1.ClusterRangesUnderReplicated)
		require.False(t, ok,
			"the under-replicated query must be skipped when no roll is pending and the condition is not already True")
	})

	t.Run("does not block completion when ranges stay under-replicated with no roll pending", func(t *testing.T) {
		cluster, ctl := newCluster(t, ctx)
		// Every node is already rolled, but the condition is True and the ranges still
		// haven't recovered (stub reports 5). Completion must not wait on under-replication
		// that may never clear, so RollingUpdate refreshes the condition and falls through
		// instead of requeuing.
		cluster.Status.SetConditionWithMessage(
			crdbv1beta1.ClusterRangesUnderReplicated, metav1.ConditionTrue, "UnderReplicated",
			"5 range(s) are under-replicated",
		)
		nodes := crdbv1beta1.CrdbNodeList{}
		require.NoError(t, ctl.List(ctx, &kube.ListOptions{}, &nodes))

		r := newReconciler(ctl, 5)
		result, err := r.RollingUpdate(ctx, cluster, nodes, nodePool)
		require.NoError(t, err)
		require.Zero(t, result.RequeueAfter, "completion must not be blocked by under-replication once nodes are rolled")

		// The condition still reflects reality (ranges are genuinely under-replicated).
		cond, ok := cluster.Status.FindCondition(crdbv1beta1.ClusterRangesUnderReplicated)
		require.True(t, ok)
		require.Equal(t, metav1.ConditionTrue, cond.Status)
	})
}

func newCluster(t *testing.T, ctx context.Context) (*crdbv1beta1.CrdbCluster, *kube.Ctl) {
	mutableOnly := crdbv1beta1.MutableOnly
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{},
	})

	ctl = ctl.WithNamespace("test-namespace")

	cluster := &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-cluster",
			Annotations: map[string]string{
				manifests.BetaCrdbClusterNodeRevisionAnnotationKey: "4",
			},
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			Mode:                &mutableOnly,
			RollingRestartDelay: &metav1.Duration{},
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Nodes: 3},
			},
			Template: crdbv1beta1.CrdbNodeTemplate{
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image: "cockroachdb/cockroach:v24.1.0",
					DataStore: crdbv1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							Spec: corev1.PersistentVolumeClaimSpec{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceStorage: resource.MustParse("5Gi"),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, cluster))

	cluster.Status.CurrentRevision = manifests.GetClusterRevision(cluster)
	cluster.Status.PreviousRevision = "3"
	cluster.Status.SetCondition(crdbv1beta1.ClusterInitialized, metav1.ConditionTrue)
	require.NoError(t, ctl.UpdateStatus(ctx, cluster))

	for i := int32(0); i < cluster.Spec.Regions[0].Nodes; i++ {
		node := upToDateNodeFor(cluster, fmt.Sprintf("%s-%d", cluster.Name, i))
		node.Spec.NodeName = fmt.Sprintf("k8s-node-%d", i)
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, node, upToDatePVCFor(node)))
	}

	return cluster, ctl
}

func TestRollingUpdateAdvancedEmulation(t *testing.T) {
	const namespace = "TEST-NAMESPACE"

	datadriven.Walk(t, "testdata/datadriven", func(t *testing.T, path string) {
		now := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
		defer timeutils.Freeze(func() time.Time { return now })()

		h := &evictionutil.Helper{}
		ctx := context.Background()
		ctl := kubetest.NewFakeCtl(kubetest.Options{
			AdvancedEmulation: true,
		}).WithNamespace(namespace)

		if testing.Verbose() {
			ctx = logging.WithLogger(ctx, zaptest.NewLogger(t))
		}

		// logReaderErr can be set by the "set-log-reader-error" command to
		// simulate pod garbage collection before logs can be read.
		var logReaderErr error

		// underReplicated is the cluster-wide under-replicated range count the
		// stubbed executor reports. The "set-under-replicated" command changes it
		// to exercise the gate that holds off rolling restarts.
		var underReplicated int64

		r := Reconciler{
			Ctl:            ctl,
			EvictionHelper: h,
			Emit:           kube.EventEmitter{EventRecorder: record.NewFakeRecorder(10)},
			NodeExecutor: func(*crdbv1beta1.CrdbNode, kubecrdbnode.Executor) kubecrdbnode.NodeExecutor {
				return stubNodeExecutor{count: underReplicated}
			},
			VersionCheckerJob: func(ctx context.Context, ctl *kube.Ctl, jobName string) VersionCheckerJob {
				return NewVersionCheckerJob(ctx, ctl, jobName).(*versionCheckerJob).WithLogReader(func() (version.Version, error) {
					if logReaderErr != nil {
						return version.Version{}, logReaderErr
					}
					return version.MustParse("v24.2.0"), nil
				})
			},
		}

		nodePool := NewNodePool(nil)

		datadriven.RunTest(t, path, func(t *testing.T, d *datadriven.TestData) string {
			now = now.Add(time.Minute)
			logging.Debugf(ctx, "Running command %s:%q", d.Pos, d.Cmd)

			switch d.Cmd {
			case "create-cluster":
				var name string
				d.ScanArgs(t, "name", &name)

				nodes := int32(3)
				image := "cockroachdb/cockroach:v24.1.0"
				mode := crdbv1beta1.MutableOnly

				if d.HasArg("nodes") {
					d.ScanArgs(t, "nodes", &nodes)
				}

				if d.HasArg("image") {
					d.ScanArgs(t, "image", &image)
				}

				cluster := &crdbv1beta1.CrdbCluster{
					ObjectMeta: metav1.ObjectMeta{
						Name: name,
					},
					Spec: crdbv1beta1.CrdbClusterSpec{
						Mode: &mode,
						RollingRestartDelay: &metav1.Duration{
							Duration: time.Second,
						},
						Regions: []crdbv1beta1.CrdbClusterRegion{
							{Nodes: nodes},
						},
						Template: crdbv1beta1.CrdbNodeTemplate{
							Spec: crdbv1beta1.CrdbNodeSpec{
								Image: image,
								DataStore: crdbv1beta1.DataStore{
									VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
										Spec: corev1.PersistentVolumeClaimSpec{
											Resources: corev1.ResourceRequirements{
												Requests: corev1.ResourceList{
													corev1.ResourceStorage: resource.MustParse("5Gi"),
												},
											},
										},
									},
								},
								TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
									{
										TopologyKey:       manifests.CrdbClusterNodeTopologyConstraintKey,
										MaxSkew:           1,
										WhenUnsatisfiable: corev1.DoNotSchedule,
										LabelSelector: &metav1.LabelSelector{
											MatchLabels: map[string]string{
												manifests.BetaCrdbClusterNodeOwnerLabelKey: name,
											},
										},
									},
								},
							},
						},
					},
				}

				require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, cluster))

				cluster.Status.CurrentRevision = manifests.GetClusterRevision(cluster)
				cluster.Status.PreviousRevision = manifests.GetClusterRevision(cluster)
				cluster.Status.SetCondition(crdbv1beta1.ClusterInitialized, metav1.ConditionTrue)
				require.NoError(t, ctl.UpdateStatus(ctx, cluster))

				for i := int32(0); i < cluster.Spec.Regions[0].Nodes; i++ {
					node := upToDateNodeFor(cluster, fmt.Sprintf("%s-%d", cluster.Name, i))
					require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, node, upToDatePVCFor(node)))
				}

			// reconcile-cluster does exactly what it sounds though it only
			// calls RollingUpdate right now.
			case "reconcile-cluster":
				var name string
				d.ScanArgs(t, "name", &name)

				repeat := 1
				if d.HasArg("repeat") {
					d.ScanArgs(t, "repeat", &repeat)
				}

				var state string
				for i := range repeat {
					var cluster crdbv1beta1.CrdbCluster
					require.NoError(t, ctl.Get(ctx, name, &cluster))

					var nodes crdbv1beta1.CrdbNodeList
					require.NoError(t, ctl.List(ctx, &kube.ListOptions{}, &nodes))

					_, err := r.RollingUpdate(ctx, &cluster, nodes, nodePool)
					require.NoError(t, err)

					// Push any status updates how we would expect calling
					// .Reconcile to do.
					require.NoError(t, ctl.UpdateStatus(ctx, &cluster))

					newState, err := dumpClusterState(ctx, ctl, nodePool)
					require.NoError(t, err)

					if i > 0 {
						require.Equal(t, state, newState, "repeated reconciles resulted in changes")
					}

					state = newState
				}

			// reconcile-node loosely emulates what the nodectrl does. In
			// the future we'll likely want to make this suit more complete
			// and directly run .Reconcile on both clusterctrl and nodectrl.
			case "reconcile-node":
				var name string
				d.ScanArgs(t, "name", &name)

				var node crdbv1beta1.CrdbNode
				require.NoError(t, ctl.Get(ctx, name, &node))

				// HACK: Rather than keeping track of the Pod and the CrdbNode,
				// we extrapolate the PodStatus from the CrdbNode's Status
				// which are close to semantically equivalent, for this test.
				var podStatus corev1.PodStatus
				if st, ts, _ := node.Status.GetCondition(crdbv1beta1.PodReady); st == metav1.ConditionTrue {
					podStatus.ContainerStatuses = append(podStatus.ContainerStatuses, corev1.ContainerStatus{
						State: corev1.ContainerState{
							Running: &corev1.ContainerStateRunning{
								StartedAt: metav1.NewTime(ts),
							},
						},
					})
				}

				h.UpdateStateFrom(ctx, &node, podStatus)

				couldEvict, delay := h.CanEvict(ctx, &node)
				if !couldEvict {
					// ObservedGeneration only advances when the node controller
					// completes without requeue. A blocked eviction returns
					// RequeueAfter, so ObservedGeneration stays behind.
					node.Status.SetConditionWithTimestamp(crdbv1beta1.PodNeedsUpdate, metav1.ConditionTrue, timeutils.Now())

					require.NoError(t, ctl.UpdateStatus(ctx, &node))

					return fmt.Sprintf("Evicting CrdbNode/%s not permitted. Must wait %s (or set eviction=false).", node.Name, delay)
				}

				node.Status.ObservedGeneration = node.Generation
				node.Status.SetConditionWithTimestamp(crdbv1beta1.PodReady, metav1.ConditionTrue, timeutils.Now())
				node.Status.SetConditionWithTimestamp(crdbv1beta1.PodNeedsUpdate, metav1.ConditionFalse, timeutils.Now())
				require.NoError(t, ctl.UpdateStatus(ctx, &node))

			case "assign-node":
				var crdbName, k8sName string
				d.ScanArgs(t, "name", &crdbName)
				d.ScanArgs(t, "k8sNode", &k8sName)

				nodePool.k8sToCrdb[k8sName] = ""
				nodePool.ClaimNode(k8sName, crdbName)

				assignment, ok := nodePool.GetPlacement(crdbName)
				require.True(t, ok)
				require.Equal(t, k8sName, assignment)

			case "set-cluster-image":
				var name string
				d.ScanArgs(t, "name", &name)

				var cluster crdbv1beta1.CrdbCluster
				require.NoError(t, ctl.Get(ctx, name, &cluster))

				d.ScanArgs(t, "image", &cluster.Spec.Template.Spec.Image)

				require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, &cluster))

				// We update CurrentRevision to make test output a bit
				// nicer. The next call to reconcile-cluster will set it
				// anyways.
				cluster.Status.CurrentRevision = manifests.GetClusterRevision(&cluster)
				require.NoError(t, ctl.UpdateStatus(ctx, &cluster))

			case "set-cluster-action":
				var name string
				d.ScanArgs(t, "name", &name)

				var cluster crdbv1beta1.CrdbCluster
				require.NoError(t, ctl.Get(ctx, name, &cluster))

				var actionType, actionStatus string
				d.ScanArgs(t, "action", &actionType)
				d.ScanArgs(t, "status", &actionStatus)

				statusMap := map[string]crdbv1beta1.ActionStatus{
					"Failed":   crdbv1beta1.Failed,
					"Starting": crdbv1beta1.Starting,
					"Finished": crdbv1beta1.Finished,
				}
				status, ok := statusMap[actionStatus]
				require.True(t, ok, "unknown action status %q", actionStatus)
				cluster.Status.SetAction(crdbv1beta1.ActionType(actionType), status)
				require.NoError(t, ctl.UpdateStatus(ctx, &cluster))

			case "set-cluster-current-version":
				var name string
				d.ScanArgs(t, "name", &name)

				var cluster crdbv1beta1.CrdbCluster
				require.NoError(t, ctl.Get(ctx, name, &cluster))

				var image, version string
				d.ScanArgs(t, "image", &image)
				d.ScanArgs(t, "version", &version)

				// Set the ImageAndVersion status to simulate a currently deployed version
				cluster.Status.Image = image
				cluster.Status.Version = version
				require.NoError(t, ctl.UpdateStatus(ctx, &cluster))

			case "create-version-checker-job":
				var name string
				d.ScanArgs(t, "name", &name)

				var cluster crdbv1beta1.CrdbCluster
				require.NoError(t, ctl.Get(ctx, name, &cluster))

				jobName := fmt.Sprintf("%s-%s", versionCheckerJobNamePrefix, cluster.Name)
				vcj := NewVersionCheckerJob(ctx, ctl, jobName)
				job := vcj.buildVersionCheckerJob(&cluster)
				require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, job))

			case "set-version-checker-job-state":
				var name string
				d.ScanArgs(t, "name", &name)

				var state string
				d.ScanArgs(t, "state", &state)

				var cluster crdbv1beta1.CrdbCluster
				var job batchv1.Job
				require.NoError(t, ctl.Get(ctx, name, &cluster))

				jobName := fmt.Sprintf("%s-%s", versionCheckerJobNamePrefix, cluster.Name)
				require.NoError(t, ctl.Get(ctx, jobName, &job))

				switch state {
				case "success":
					job.Status.Succeeded = 1
				case "failure":
					job.Status.Failed = 1
				}

				require.NoError(t, ctl.Update(ctx, &job))

			case "set-volume-size":
				var name string
				d.ScanArgs(t, "name", &name)

				var cluster crdbv1beta1.CrdbCluster
				require.NoError(t, ctl.Get(ctx, name, &cluster))

				var nodes crdbv1beta1.CrdbNodeList
				require.NoError(t, ctl.List(ctx, &kube.ListOptions{}, &nodes))

				var size string
				d.ScanArgs(t, "size", &size)
				parsedSize := resource.MustParse(size)

				cluster.Spec.Template.Spec.DataStore.VolumeClaimTemplate.Spec.Resources.Requests[corev1.ResourceStorage] = parsedSize

				require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, &cluster))

				for i := int32(0); i < cluster.Spec.Regions[0].Nodes; i++ {
					var node crdbv1beta1.CrdbNode
					require.NoError(t, ctl.Get(ctx, fmt.Sprintf("%s-%d", cluster.Name, i), &node))
					node.Spec.DataStore.VolumeClaimTemplate.Spec.Resources.Requests[corev1.ResourceStorage] = parsedSize
					require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, &node))
				}

				_, err := r.handlePVCResize(ctx, &cluster, nodes)
				require.NoError(t, err)

			// Eventually, we'll want to make this set-node-pod-state once
			// the nodectrl is integrated into this test.
			case "set-node-state":
				var name string
				d.ScanArgs(t, "name", &name)

				var node crdbv1beta1.CrdbNode
				require.NoError(t, ctl.Get(ctx, name, &node))

				if d.HasArg("ready") {
					var ready bool
					d.ScanArgs(t, "ready", &ready)
					if ready {
						node.Status.SetConditionWithTimestamp(crdbv1beta1.PodReady, metav1.ConditionTrue, timeutils.Now())
					} else {
						node.Status.SetConditionWithTimestamp(crdbv1beta1.PodReady, metav1.ConditionFalse, timeutils.Now())
					}
				}

				if d.HasArg("podNeedsUpdate") {
					var needsUpdate bool
					d.ScanArgs(t, "podNeedsUpdate", &needsUpdate)
					if needsUpdate {
						node.Status.SetConditionWithTimestamp(crdbv1beta1.PodNeedsUpdate, metav1.ConditionTrue, timeutils.Now())
					} else {
						node.Status.SetConditionWithTimestamp(crdbv1beta1.PodNeedsUpdate, metav1.ConditionFalse, timeutils.Now())
					}
				}

				if d.HasArg("observedGeneration") {
					var gen int64
					d.ScanArgs(t, "observedGeneration", &gen)
					node.Status.ObservedGeneration = gen
				}

				require.NoError(t, ctl.UpdateStatus(ctx, &node))

			// reset-eviction-helper simulates an operator restart by replacing
			// the eviction helper with a fresh empty instance.
			case "reset-eviction-helper":
				newHelper := &evictionutil.Helper{}
				r.EvictionHelper = newHelper
				h = newHelper

			// set-under-replicated sets the cluster-wide under-replicated range
			// count the stubbed executor reports, so we can exercise the gate that
			// holds off rolling restarts while ranges are under-replicated.
			case "set-under-replicated":
				var count int
				d.ScanArgs(t, "count", &count)
				underReplicated = int64(count)

			// set-log-reader-error simulates pod garbage collection by making
			// the log reader return an error on the next job status check.
			case "set-log-reader-error":
				var enabled bool
				d.ScanArgs(t, "enabled", &enabled)
				if enabled {
					logReaderErr = errors.Wrap(errNoPodsFound, "simulated")
				} else {
					logReaderErr = nil
				}

			default:
				t.Fatalf("unknown command: %q", d.Cmd)
			}

			state, err := dumpClusterState(ctx, ctl, nodePool)
			require.NoError(t, err)

			return state
		})
	})
}

// TestRequiresUpgradeValidationGating verifies the gating logic that decides whether
// version validation must run before a rolling update proceeds.
func TestRequiresUpgradeValidationGating(t *testing.T) {
	const (
		oldImage = "cockroachdb/cockroach:v24.1.0"
		newImage = "cockroachdb/cockroach:v24.2.0"
		version  = "v24.1.0"
	)

	makeCluster := func(specImage, statusImage, statusVersion string, actions ...crdbv1beta1.ClusterAction) *crdbv1beta1.CrdbCluster {
		c := &crdbv1beta1.CrdbCluster{}
		c.Spec.Template.Spec.Image = specImage
		c.Status.Image = statusImage
		c.Status.Version = statusVersion
		c.Status.Actions = actions
		return c
	}

	action := func(typ crdbv1beta1.ActionType, st crdbv1beta1.ActionStatus) crdbv1beta1.ClusterAction {
		return crdbv1beta1.ClusterAction{Type: typ, Status: st}
	}

	for _, tc := range []struct {
		name      string
		cluster   *crdbv1beta1.CrdbCluster
		wantNeeds bool
	}{
		{
			name:      "no status image yet",
			cluster:   makeCluster(newImage, "", version),
			wantNeeds: false,
		},
		{
			name:      "no status version yet",
			cluster:   makeCluster(newImage, oldImage, ""),
			wantNeeds: false,
		},
		{
			name:      "images match, no upgrade needed",
			cluster:   makeCluster(oldImage, oldImage, version),
			wantNeeds: false,
		},
		{
			name:      "image changed, no prior actions",
			cluster:   makeCluster(newImage, oldImage, version),
			wantNeeds: true,
		},
		{
			// Validation passed and the upgrade is in progress. Subsequent reconciles
			// during the rolling update must not re-run validation.
			name: "validation finished and upgrade starting - short-circuit during rolling update",
			cluster: makeCluster(newImage, oldImage, version,
				action(crdbv1beta1.ValidateVersion, crdbv1beta1.Finished),
				action(crdbv1beta1.Upgrade, crdbv1beta1.Starting),
			),
			wantNeeds: false,
		},
		{
			// The previous upgrade cycle completed. A new image change should trigger
			// fresh validation, even though ValidateVersion: Finished is still set.
			name: "validation finished but upgrade also finished - new upgrade cycle",
			cluster: makeCluster(newImage, oldImage, version,
				action(crdbv1beta1.ValidateVersion, crdbv1beta1.Finished),
				action(crdbv1beta1.Upgrade, crdbv1beta1.Finished),
			),
			wantNeeds: true,
		},
		{
			// Validation failed. requiresUpgradeValidation still returns true — the block
			// is enforced by RollingUpdate directly before calling requiresUpgradeValidation.
			// When the user changes to a different image the annotation won't match and
			// RollingUpdate lets the call through, so requiresUpgradeValidation must return true.
			name: "validation failed - must not bypass validation gate",
			cluster: makeCluster(newImage, oldImage, version,
				action(crdbv1beta1.ValidateVersion, crdbv1beta1.Failed),
				action(crdbv1beta1.Upgrade, crdbv1beta1.Finished),
			),
			wantNeeds: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			needs, _ := requiresUpgradeValidation(tc.cluster)
			require.Equal(t, tc.wantNeeds, needs)
		})
	}
}

func dumpClusterState(ctx context.Context, ctl *kube.Ctl, nodePool *NodePool) (string, error) {
	var buf strings.Builder

	var clusters crdbv1beta1.CrdbClusterList
	if err := ctl.List(ctx, &kube.ListOptions{}, &clusters); err != nil {
		return "", err
	}

	for _, cluster := range clusters.Items {
		var nodes crdbv1beta1.CrdbNodeList
		if err := ctl.List(ctx, &kube.ListOptions{}, &nodes); err != nil {
			return "", err
		}

		_, _ = fmt.Fprintf(&buf, "CrdbCluster/%s\n", cluster.Name)
		_, _ = fmt.Fprintf(&buf, "\tCurrent Revision:  %q\n", cluster.Status.CurrentRevision)
		_, _ = fmt.Fprintf(&buf, "\tPrevious Revision: %q\n", cluster.Status.PreviousRevision)
		_, _ = fmt.Fprintf(&buf, "\tSpec Image:        %q\n", cluster.Spec.Template.Spec.Image)
		_, _ = fmt.Fprintf(&buf, "\tCurrent Version:   %q\n", cluster.Status.Version)

		// Show upgrade-related action statuses if present
		for _, action := range cluster.Status.Actions {
			switch action.Type {
			case crdbv1beta1.ValidateVersion:
				_, _ = fmt.Fprintf(&buf, "\tValidation Status: %s\n", action.Status)
			case crdbv1beta1.Upgrade:
				_, _ = fmt.Fprintf(&buf, "\tUpgrade Status:    %s\n", action.Status)
			}
		}

		for _, node := range nodes.Items {
			_, _ = fmt.Fprintf(&buf, "CrdbNode/%s\n", node.Name)
			_, _ = fmt.Fprintf(&buf, "\tDesired Revision: %q\n", node.Annotations[manifests.BetaCrdbClusterNodeRevisionAnnotationKey])
			_, _ = fmt.Fprintf(&buf, "\tGeneration: %d, Observed: %d\n", node.Generation, node.Status.ObservedGeneration)
			_, _ = fmt.Fprintf(&buf, "\tDisk size: %s\n", node.Spec.DataStore.VolumeClaimTemplate.Spec.Resources.Requests.Storage())
			k8sNode, _ := nodePool.GetPlacement(node.Name)
			_, _ = fmt.Fprintf(&buf, "\tAssigned K8s Node: '%s'\n", k8sNode)
			_, _ = fmt.Fprintf(&buf, "\tSpec K8s Node: '%s'\n", node.Spec.NodeName)
			for _, condition := range node.Status.Conditions {
				_, _ = fmt.Fprintf(&buf, "\t%-15s %-8s %s\n", condition.Type, condition.Status, condition.LastTransitionTime.UTC())
			}
		}

	}

	return buf.String(), nil
}

func upToDateNodeFor(cluster *crdbv1beta1.CrdbCluster, name string) *crdbv1beta1.CrdbNode {
	node := manifests.BetaCrdbClusterNode(cluster, &cluster.Spec.Regions[0], "")

	node.Name = name
	node.GenerateName = ""
	node.Status = crdbv1beta1.CrdbNodeStatus{
		ObservedGeneration: 1,
		Conditions: []crdbv1beta1.NodeCondition{
			{Type: crdbv1beta1.PodReady, Status: metav1.ConditionTrue, LastTransitionTime: metav1.NewTime(timeutils.Now())},
			{Type: crdbv1beta1.PodRunning, Status: metav1.ConditionTrue, LastTransitionTime: metav1.NewTime(timeutils.Now())},
		},
	}
	node.Spec.DataStore = *cluster.Spec.Template.Spec.DataStore.DeepCopy()

	return node
}

func upToDatePVCFor(node *crdbv1beta1.CrdbNode) *corev1.PersistentVolumeClaim {
	pvc := node.Spec.DataStore.VolumeClaimTemplate.DeepCopy()
	pvc.Name = manifests.CrdbNodeStoragePVCName(node.Name, pvc)
	return pvc
}

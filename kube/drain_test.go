package kube_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDrain(t *testing.T) {
	ctx := context.Background()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-1",
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
	podStatusRunning := corev1.PodStatus{
		Phase: corev1.PodRunning,
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		},
	}
	truePtr := true
	pods := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-to-evict",
				Namespace: "default",
			},
			Spec: corev1.PodSpec{
				NodeName: "node-1",
			},
			Status: podStatusRunning,
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "another-pod-to-evict",
				Namespace: "custom-namespace",
				Labels:    map[string]string{"app": "another-app-to-evict"},
			},
			Spec: corev1.PodSpec{
				NodeName: "node-1",
			},
			Status: podStatusRunning,
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "daemonset-pod",
				Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{
					{
						Controller: &truePtr,
						Kind:       "DaemonSet",
					},
				},
			},
			Spec: corev1.PodSpec{
				NodeName: "node-1",
			},
			Status: podStatusRunning,
		},
	}
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{
			BeforeMutation: kubetest.MakeReady,
		},
	}).WithNamespace("default")

	var pdbForAppAndStatus = func(appName string, s metav1.ConditionStatus) *policyv1.PodDisruptionBudget {
		return &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name: "another-app-pdb",
			},
			Spec: policyv1.PodDisruptionBudgetSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": appName},
				},
			},
			Status: policyv1.PodDisruptionBudgetStatus{Conditions: []metav1.Condition{
				{
					Type:   policyv1.DisruptionAllowedCondition,
					Status: s,
				},
			}},
		}
	}

	require.NoError(t, ctl.Apply(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}))
	require.NoError(t, ctl.Apply(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "custom-namespace"}}))
	require.NoError(t, ctl.Apply(ctx, node))
	for _, p := range pods {
		require.NoError(t, ctl.WithNamespace(p.ObjectMeta.Namespace).Apply(ctx, p))
	}
	require.NoError(t, ctl.WithNamespace("custom-namespace").Apply(ctx, pdbForAppAndStatus("another-app-to-evict", "False")))

	var getPod corev1.Pod

	// All pods should return before the drain.
	require.NoError(t, ctl.Get(ctx, "pod-to-evict", &getPod))
	require.NoError(t, ctl.Get(ctx, "daemonset-pod", &getPod))
	require.NoError(t, ctl.WithNamespace("custom-namespace").Get(ctx, "another-pod-to-evict", &getPod))

	defer testutils.HookGlobal(&kube.DisruptionAllowedPollTime, 10*time.Millisecond)()
	defer testutils.HookGlobal(&kube.DisruptionAllowedWaitTime, 100*time.Millisecond)()
	err := ctl.Drain(ctx, node)
	require.ErrorContains(t, err, "waiting for DisruptionAllowed on PDB custom-namespace/another-app-pdb")

	// Change DisruptionAllowed to True.
	require.NoError(t, ctl.WithNamespace("custom-namespace").Apply(ctx, pdbForAppAndStatus("another-app-to-evict", "True")))
	require.NoError(t, ctl.WithNamespace("custom-namespace").Apply(ctx, pdbForAppAndStatus("will-not-match", "False"))) // Should be ignored
	require.NoError(t, ctl.Drain(ctx, node))

	// Only the daemonset pod should be returned after draining.
	require.True(t, kubeerrors.IsNotFound(ctl.Get(ctx, "pod-to-evict", &getPod)))
	require.True(t, kubeerrors.IsNotFound(ctl.WithNamespace("custom-namespace").Get(ctx, "another-pod-to-evict", &getPod)))
	require.NoError(t, ctl.Get(ctx, "daemonset-pod", &getPod))

	require.True(t, node.Spec.Unschedulable)
}

func TestCordonDoesNotRewriteLastApplied(t *testing.T) {
	ctx := context.Background()
	const nodeName = "test-node"
	const lastAppliedKey = "kubectl.kubernetes.io/last-applied-configuration"
	const staleLastApplied = `{"apiVersion":"v1","kind":"Node","metadata":{"name":"test-node"},"spec":{"unschedulable":true}}`

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				lastAppliedKey: staleLastApplied,
			},
			Labels: map[string]string{},
		},
	}

	ctl := kubetest.NewFakeCtl(kubetest.Options{AdvancedEmulation: true})

	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, node))

	var annotated corev1.Node
	require.NoError(t, ctl.Get(ctx, nodeName, &annotated))
	annotated.Annotations[lastAppliedKey] = staleLastApplied
	require.NoError(t, ctl.UpdateWithOptions(ctx, kube.UpdateOptions{
		NoWait:                  true,
		NoLastAppliedAnnotation: true,
	}, &annotated))

	var stale corev1.Node
	require.NoError(t, ctl.Get(ctx, nodeName, &stale))

	require.NoError(t, ctl.Cordon(ctx, &stale))

	var final corev1.Node
	require.NoError(t, ctl.Get(ctx, nodeName, &final))
	require.True(t, final.Spec.Unschedulable)
	require.Equal(t, staleLastApplied, final.Annotations[lastAppliedKey])
}

func TestDrainIgnoresNotFoundDuringEviction(t *testing.T) {
	ctx := context.Background()

	defer testutils.HookGlobal(&kube.DisruptionAllowedPollTime, 10*time.Millisecond)()
	defer testutils.HookGlobal(&kube.DisruptionAllowedWaitTime, 200*time.Millisecond)()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-node",
			Labels: map[string]string{},
		},
	}
	firstPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "first-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
		},
		Spec: corev1.PodSpec{
			NodeName: node.Name,
		},
	}
	secondPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "second-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
		},
		Spec: corev1.PodSpec{
			NodeName: node.Name,
		},
	}
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pdb",
			Namespace: "default",
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
		},
		Status: policyv1.PodDisruptionBudgetStatus{
			Conditions: []metav1.Condition{
				{
					Type:   policyv1.DisruptionAllowedCondition,
					Status: metav1.ConditionTrue,
				},
			},
		},
	}
	var ctl *kube.Ctl
	var (
		setupDone            bool
		firstDeleteInjected  bool
		secondDeleteObserved bool
		recoverPDB           sync.Once
		recoverPDBRequested  = make(chan struct{})
		recoverPDBApplied    = make(chan error, 1)
	)
	ctl = kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{
			BeforeMutation: func(object kube.Object) error {
				if !setupDone {
					return nil
				}

				switch obj := object.(type) {
				case *corev1.Pod:
					switch obj.Name {
					case firstPod.Name:
						if firstDeleteInjected {
							return nil
						}
						firstDeleteInjected = true

						currentPDB := pdb.DeepCopy()
						currentPDB.Status.Conditions[0].Status = metav1.ConditionFalse
						if err := ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, currentPDB); err != nil {
							return err
						}
						if err := ctl.DeleteWithOptions(ctx, kube.DeleteOptions{NoWait: true},
							&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: firstPod.Name, Namespace: firstPod.Namespace}}); err != nil {
							return err
						}
						recoverPDB.Do(func() {
							close(recoverPDBRequested)
						})
						if err := <-recoverPDBApplied; err != nil {
							return err
						}
					case secondPod.Name:
						secondDeleteObserved = true
						var latestPDB policyv1.PodDisruptionBudget
						if err := ctl.Get(ctx, pdb.Name, &latestPDB); err != nil {
							return err
						}
						for _, condition := range latestPDB.Status.Conditions {
							if condition.Type == policyv1.DisruptionAllowedCondition &&
								condition.Status == metav1.ConditionTrue {
								return nil
							}
						}
						return errors.New("second eviction attempted before the shared PDB recovered")
					}
				}
				return nil
			},
		},
	}).WithNamespace("default")
	go func() {
		<-recoverPDBRequested
		recoveredPDB := pdb.DeepCopy()
		recoveredPDB.Status.Conditions[0].Status = metav1.ConditionTrue
		recoverPDBApplied <- ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, recoveredPDB)
	}()
	t.Cleanup(func() {
		recoverPDB.Do(func() {
			close(recoverPDBRequested)
		})
	})

	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, node, firstPod, secondPod, pdb))
	setupDone = true

	require.NoError(t, ctl.Drain(ctx, node))

	require.True(t, firstDeleteInjected, "first pod should disappear during eviction")
	require.True(t, secondDeleteObserved, "second pod should still be evicted")

	var finalNode corev1.Node
	require.NoError(t, ctl.Get(ctx, node.Name, &finalNode))
	require.True(t, finalNode.Spec.Unschedulable)

	err := ctl.Get(ctx, firstPod.Name, &corev1.Pod{})
	require.True(t, kubeerrors.IsNotFound(err))
	err = ctl.Get(ctx, secondPod.Name, &corev1.Pod{})
	require.True(t, kubeerrors.IsNotFound(err))
}

func TestDrainWaitDuration(t *testing.T) {
	type testCase struct {
		name         string
		gracePeriods []*int64
		expect       time.Duration
	}

	cases := []testCase{
		{
			name:   "empty",
			expect: time.Minute * 5,
		},
		{
			name:         "no grace period",
			expect:       time.Minute * 5,
			gracePeriods: []*int64{nil, nil, nil},
		},
		{
			name:         "adds grace period",
			expect:       time.Minute*5 + time.Second*30,
			gracePeriods: []*int64{nil, new(int64(30)), nil},
		},
		{
			name:         "uses max grace period left to right",
			expect:       time.Minute*5 + time.Second*600,
			gracePeriods: []*int64{nil, new(int64(30)), new(int64(600))},
		},
		{
			name:         "uses max grace period right to left",
			expect:       time.Minute*5 + time.Second*600,
			gracePeriods: []*int64{new(int64(600)), new(int64(30)), nil},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var pods []corev1.Pod
			for _, gracePeriod := range tc.gracePeriods {
				pods = append(pods, corev1.Pod{
					Spec: corev1.PodSpec{
						TerminationGracePeriodSeconds: gracePeriod,
					},
				})
			}
			require.Equal(t, tc.expect, kube.DrainWaitDuration(pods))
		})
	}
}

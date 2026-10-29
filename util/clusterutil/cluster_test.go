package clusterutil

import (
	"context"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsClusterForNodeBeingDeleted(t *testing.T) {
	ctx := context.Background()
	node := &crdbv1beta1.CrdbNode{ObjectMeta: metav1.ObjectMeta{
		Name:      "cockroachdb-abc",
		Namespace: "default",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: crdbv1beta1.GroupVersion.String(),
			Kind:       crdbv1beta1.CrdbClusterKind,
			Name:       "cockroachdb",
		}},
	}}

	for _, tc := range []struct {
		name     string
		cluster  *crdbv1beta1.CrdbCluster
		expected bool
	}{
		{name: "active", cluster: &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cockroachdb", Namespace: "default"},
		}},
		{name: "terminating", cluster: &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cockroachdb", Namespace: "default", DeletionTimestamp: new(metav1.Now()),
			},
		}, expected: true},
		{name: "deleted", expected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctl := kubetest.NewFakeCtl(kubetest.Options{}).WithNamespace("default")
			if tc.cluster != nil {
				require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, tc.cluster))
			}

			deleting, err := IsClusterForNodeBeingDeleted(ctx, ctl, node)
			require.NoError(t, err)
			require.Equal(t, tc.expected, deleting)
		})
	}
}

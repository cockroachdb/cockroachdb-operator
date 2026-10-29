package testenv

import (
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestDefaultNodeDataStore(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dataStore crdbv1beta1.DataStore
	}{
		{
			name: "volume claim template",
			dataStore: crdbv1beta1.DataStore{
				VolumeClaimTemplate: &corev1.PersistentVolumeClaim{},
			},
		},
		{
			name: "volume source",
			dataStore: crdbv1beta1.DataStore{
				VolumeSource: &corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
		},
		{
			name: "basalt",
			dataStore: crdbv1beta1.DataStore{
				Basalt: &crdbv1beta1.BasaltStoreSpec{TenantID: "tenant"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := &crdbv1beta1.CrdbNode{Spec: crdbv1beta1.CrdbNodeSpec{DataStore: tc.dataStore}}
			want := *tc.dataStore.DeepCopy()

			defaultNodeDataStore(node)

			require.Equal(t, want, node.Spec.DataStore)
		})
	}

	t.Run("empty", func(t *testing.T) {
		node := &crdbv1beta1.CrdbNode{}

		defaultNodeDataStore(node)

		require.NotNil(t, node.Spec.DataStore.VolumeClaimTemplate)
	})
}

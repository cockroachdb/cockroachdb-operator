package kube

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetObjectKey(t *testing.T) {
	nginxDeployment := nginxDeployment.DeepCopy()

	nginxDeployment.TypeMeta.Kind = "Deployment"
	nginxDeployment.TypeMeta.APIVersion = "apps/v1"

	key := getObjectKeyOrPanic(nginxDeployment)

	require.Equal(t, "/nginx", key.String())
}

func TestGVKFromScheme(t *testing.T) {
	gvk, err := GVKFromScheme(Scheme, nginxDeployment)
	require.NoError(t, err)

	require.Equal(t, "apps/v1, Kind=Deployment", gvk.String())
}

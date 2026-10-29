package kube

import (
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

func TestGetAnnotation(t *testing.T) {
	d := nginxDeployment.DeepCopy()

	require.NotContains(t, d.Annotations, ForceApplyOverwriteAnnotation)

	// Check that a nil value is returned.
	val, err := getAnnotation(d, ForceApplyOverwriteAnnotation)
	require.NoError(t, err)
	require.Nil(t, val)

	// Make some changes.
	d.Annotations = map[string]string{}
	d.Annotations[ForceApplyOverwriteAnnotation] = "baz"

	// Validate that changes are reflected properly.
	val, err = getAnnotation(d, ForceApplyOverwriteAnnotation)
	require.NoError(t, err)
	require.Equal(t, "baz", string(val))
}

func TestLastApplied(t *testing.T) {
	d := nginxDeployment.DeepCopy()

	require.NotContains(t, d.Annotations, corev1.LastAppliedConfigAnnotation)

	config, err := lastApplied(d)

	require.NoError(t, err)
	require.Nil(t, config)
}

func TestSetLastApplied(t *testing.T) {
	d := nginxDeployment.DeepCopy()

	require.NoError(t, setLastApplied(d))

	lastBytes, err := lastApplied(d)
	require.NoError(t, err)

	var last appsv1.Deployment
	decoder := serializer.NewCodecFactory(clientgoscheme.Scheme).UniversalDeserializer()
	err = runtime.DecodeInto(decoder, lastBytes, &last)
	require.NoError(t, err)

	// Remove the annotation and k8s decoder uses nil rather than an empty map.
	d.Annotations = nil

	require.Equal(t, &last, d)

	// Reset the annotation.
	require.NoError(t, setLastApplied(d))

	// Make some changes.
	d.Annotations["Foo/Bar"] = "baz"

	// Update the annotation.
	require.NoError(t, setLastApplied(d))

	// Decode ourselves again.
	lastBytes, err = lastApplied(d)
	require.NoError(t, err)

	err = runtime.DecodeInto(decoder, lastBytes, &last)
	require.NoError(t, err)

	// Delete our annotation.
	delete(d.Annotations, corev1.LastAppliedConfigAnnotation)

	require.Equal(t, &last, d)
	require.Equal(t, "baz", last.Annotations["Foo/Bar"])

	// Regression/Sanity test for handling of Secret.Data. Kubernetes (and the
	// go SDK for that matter) specifies .Data must be base64 encoded bytes.
	// HOWEVER, this encoding is handled transparently for the user by
	// json.Marshal which can be observed in the last-applied annotation. This
	// test ensures that the kube package doesn't accidentally doubly encode
	// Secret data.
	s := corev1.Secret{
		Data: StringData(map[string]string{
			"key": "value",
		}),
	}

	require.NoError(t, setLastApplied(&s))
	require.Equal(t, `{"data":{"key":"dmFsdWU="},"metadata":{}}`+"\n", s.Annotations[corev1.LastAppliedConfigAnnotation])
}

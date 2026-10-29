package kube_test

import (
	"context"
	"testing"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/tenantctrl"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

func TestStringData(t *testing.T) {
	require.Equal(t, map[string][]byte{}, kube.StringData(nil))
	require.Equal(t, map[string][]byte{}, kube.StringData(map[string]string{}))
	require.Equal(t, map[string][]byte{
		"foo": []byte("bar"),
	}, kube.StringData(map[string]string{
		"foo": "bar",
	}))
	require.Equal(t, map[string][]byte{
		"foo": []byte("bar"),
		"baz": []byte("longerstring"),
	}, kube.StringData(map[string]string{
		"foo": "bar",
		"baz": "longerstring",
	}))
}

func TestOwnerReferenceFor(t *testing.T) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-role",
			UID:  "my-uid",
		},
	}
	ref, err := kube.OwnerReferenceFor(scheme, role)
	require.NoError(t, err)
	expectedReference := metav1.OwnerReference{
		APIVersion: "rbac.authorization.k8s.io/v1",
		Kind:       "Role",
		Name:       "my-role",
		UID:        types.UID("my-uid"),
	}
	require.Equal(t, expectedReference, ref)

	const tenantID = crdbv1beta1.TenantID(1)
	tenant := &crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:        tenantID.Tag(),
			Finalizers:  []string{"finalizer2", "finalizer1"},
			Annotations: map[string]string{tenantctrl.DisableReconcile: "true"},
		},
		Spec: crdbv1beta1.CrdbTenantSpec{TenantID: tenantID},
	}
	_, tenantErr := kube.OwnerReferenceFor(scheme, tenant)

	require.Contains(t, tenantErr.Error(), "could not find GVK for object")
}
func TestGetKubernetesAPIServerIPs(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{})

	t.Run("no endpoint", func(t *testing.T) {
		ips, err := kube.GetKubernetesAPIServerIPs(ctx, ctl)
		require.Regexp(t, "retrieving kubernetes endpoints", err)
		require.Nil(t, ips)
	})

	t.Run("not ready", func(t *testing.T) {
		endpoints := &corev1.Endpoints{
			ObjectMeta: metav1.ObjectMeta{
				Name: "kubernetes",
			},
			Subsets: []corev1.EndpointSubset{
				{NotReadyAddresses: []corev1.EndpointAddress{{IP: "127.0.0.1"}}},
			},
		}
		// Apply with NoWait = true so we don't wait for all the addresses to be
		// ready, which will never occur.
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, endpoints))

		ips, err := kube.GetKubernetesAPIServerIPs(ctx, ctl)
		require.EqualError(t, err, "could not retrieve kubernetes endpoint IPs")
		require.Nil(t, ips)
	})

	t.Run("some ready", func(t *testing.T) {
		endpoints := &corev1.Endpoints{
			ObjectMeta: metav1.ObjectMeta{
				Name: "kubernetes",
			},
			Subsets: []corev1.EndpointSubset{
				{Addresses: []corev1.EndpointAddress{
					{IP: "1.2.3.4"},
					{IP: ""},
				}},
				{NotReadyAddresses: []corev1.EndpointAddress{{IP: "5.6.7.8"}}},
				{Addresses: []corev1.EndpointAddress{
					{IP: "9.10.11.12"},
					{IP: "13.14.15.16"},
				}},
			},
		}
		// Apply with NoWait = true so we don't wait for all the addresses to be
		// ready, which will never occur.
		require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, endpoints))

		ips, err := kube.GetKubernetesAPIServerIPs(ctx, ctl)
		require.NoError(t, err)
		require.EqualValues(t, []string{"1.2.3.4", "9.10.11.12", "13.14.15.16"}, ips)
	})
}

func TestSetOwnerReferences(t *testing.T) {
	ownerRefs := []metav1.OwnerReference{{
		Name: "a-test",
	}}

	objects := []kube.Object{
		&corev1.Pod{},
		&corev1.Service{},
	}

	// Assert that the original slice is being mutated by this method.
	require.Equal(t, objects, kube.SetOwnerReferences(ownerRefs, objects...))

	// Assert that .OwnerReferences has been set.
	require.Equal(t, ownerRefs, objects[0].(*corev1.Pod).OwnerReferences)
	require.Equal(t, ownerRefs, objects[1].(*corev1.Service).OwnerReferences)
}

func TestRetryConflicts(t *testing.T) {
	type tc struct {
		retries  int
		finalErr error
	}
	tests := []tc{
		{0, errors.New("error")},
		{1, errors.New("error")},
		{5, errors.New("error")},
		{0, nil},
		{1, nil},
		{5, nil},
	}
	for _, tc := range tests {
		count := 0
		result := kube.RetryConflicts(func() error {
			if count == tc.retries {
				return tc.finalErr
			}
			count += 1
			return kubeerrors.NewConflict(schema.GroupResource{}, "", nil)
		})
		require.Equal(t, result, tc.finalErr)
		require.Equal(t, count, tc.retries)
	}
}

func TestChecksumConfigs(t *testing.T) {
	secret := &corev1.Secret{
		Data: map[string][]byte{
			"key1": []byte("value1"),
			"key2": []byte("value2"),
			"key3": []byte("value3"),
		},
	}

	configMap := &corev1.ConfigMap{
		Data: map[string]string{
			"key4": "value4",
			"key5": "value5",
			"key6": "value6",
		},
	}

	// Include a single hard coded test to ensure that hashing is consitent
	// across test invocations. (looking at you golang's map iteration
	// ordering).
	require.Equal(t, "727ce00a662d9031", kube.ChecksumConfigs(secret, configMap))

	// Hashing is consistent.
	require.Equal(t, kube.ChecksumConfigs(secret, configMap), kube.ChecksumConfigs(secret, configMap))
	require.Equal(t, kube.ChecksumConfigs(configMap, secret), kube.ChecksumConfigs(configMap, secret))

	require.NotEqual(t, kube.ChecksumConfigs(secret, configMap), kube.ChecksumConfigs(configMap, secret))

	// Change a key and observe that the hash changes.
	cs1 := kube.ChecksumConfigs(secret, configMap)
	secret.Data["key1"] = []byte("value9001")
	cs2 := kube.ChecksumConfigs(secret, configMap)

	require.NotEqual(t, cs2, cs1)

	require.Panics(t, func() {
		// Unmountable objects will panic.
		kube.ChecksumConfigs(&corev1.Service{})
	})
}

func TestApplyChecksumConfigName(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-secret",
		},
		Data: map[string][]byte{
			"key1": []byte("value1"),
			"key2": []byte("value2"),
			"key3": []byte("value3"),
		},
	}
	require.NoError(t, kube.ApplyChecksumConfigName(secret))
	require.Equal(t, "my-secret-553641960055658d", secret.GetName())
	require.Equal(t, map[string]string{
		"checksum/config": "553641960055658d",
	}, secret.ObjectMeta.Annotations)

	err := kube.ApplyChecksumConfigName(secret)
	require.Error(t, err)
	require.Regexp(t, "checksum has already been applied", err.Error())

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-configmap",
		},
		Data: map[string]string{
			"key4": "value4",
			"key5": "value5",
			"key6": "value6",
		},
	}
	require.NoError(t, kube.ApplyChecksumConfigName(configMap))
	require.Equal(t, "my-configmap-adcb267f8c90be39", configMap.GetName())
	require.Equal(t, map[string]string{
		"checksum/config": "adcb267f8c90be39",
	}, configMap.ObjectMeta.Annotations)

	err = kube.ApplyChecksumConfigName(configMap)
	require.Error(t, err)
	require.Regexp(t, "checksum has already been applied", err.Error())

	pod := &corev1.Pod{}
	require.Regexp(t, "unhandled object type", kube.ApplyChecksumConfigName(pod))
}

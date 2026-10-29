package manifests

import (
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/constants"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCrdbOperator(t *testing.T) {
	manifest := CockroachOperator(CockroachOperatorOptions{})
	require.True(t, manifestContains(t, manifest, `"kind": "Deployment"`))
	require.True(t, manifestContains(t, manifest, `"image": "localhost:5000/cockroach-operator:latest`))
	require.True(t, manifestContains(t, manifest, `"secretName": "cockroach-operator-certs"`))
	require.True(t, manifestContains(t, manifest, `"replicas": 3`))
	require.True(t, manifestContains(t, manifest, `"priorityClassName": "cockroach-operator"`))
	require.False(t, manifestContains(t, manifest, "disable-tenant-finalization"))

	manifest = CockroachOperator(CockroachOperatorOptions{
		Image:      "us.gcr.io/cockroach-cloud-images/cockroach-operator:v0.0.1",
		Replicas:   1,
		CpuRequest: "250m",
	})
	require.True(t, manifestContains(t, manifest, `"image": "us.gcr.io/cockroach-cloud-images/cockroach-operator:v0.0.1"`))
	require.True(t, manifestContains(t, manifest, `"replicas": 1`))
	require.True(t, manifestContains(t, manifest, `"cpu": "250m"`))
	require.False(t, manifestContains(t, manifest, `"-custom-clusterctrl"`))

	manifest = CockroachOperator(CockroachOperatorOptions{
		ClusterRoles: []*rbacv1.ClusterRole{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "role-1",
				},
				Rules: []rbacv1.PolicyRule{
					{
						APIGroups: []string{"cilium.io"},
						Resources: []string{"ciliumnetworkpolicies"},
						Verbs:     []string{"foobarbaz-1"},
					},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "role-2",
				},
				Rules: []rbacv1.PolicyRule{
					{
						APIGroups: []string{"cilium.io"},
						Resources: []string{"ciliumclusterwidenetworkpolicies"},
						Verbs:     []string{"foobarbaz-2"},
					},
				},
			},
		},
	})
	require.True(t, manifestContains(t, manifest, `role-1`))
	require.True(t, manifestContains(t, manifest, `role-1-binding`))
	require.True(t, manifestContains(t, manifest, `role-2`))
	require.True(t, manifestContains(t, manifest, `role-2-binding`))
	require.True(t, manifestContains(t, manifest, `ciliumnetworkpolicies`))
	require.True(t, manifestContains(t, manifest, `ciliumclusterwidenetworkpolicies`))
	require.True(t, manifestContains(t, manifest, `foobarbaz-1`))
	require.True(t, manifestContains(t, manifest, `foobarbaz-2`))

	// Ensure not prometheus details included when it should be skipped.
	manifest = CockroachOperator(CockroachOperatorOptions{SkipTenantController: true})
	require.False(t, manifestContains(t, manifest, "prometheus"))
	require.False(t, manifestContains(t, manifest, "9090"))

	// Ensure that we have additional environment variables.
	manifest = CockroachOperator(CockroachOperatorOptions{
		EnvVars: []corev1.EnvVar{
			{
				Name:  "FOO_BAR",
				Value: "baz",
			},
			{
				Name:  "FOO_BAR_2",
				Value: "baz2",
			},
		},
	})
	require.True(t, manifestContains(t, manifest, "FOO_BAR"))
	require.True(t, manifestContains(t, manifest, "baz"))
	require.True(t, manifestContains(t, manifest, "FOO_BAR_2"))
	require.True(t, manifestContains(t, manifest, "baz2"))

	manifest = CockroachOperator(CockroachOperatorOptions{WorkloadIdentityAnnotation: "foobar"})
	require.True(t, manifestContains(t, manifest, "foobar"))
	require.True(t, manifestContains(t, manifest, constants.WorkloadIdentitySAAnnotationKey))

	manifest = CockroachOperator(CockroachOperatorOptions{DisableTenantFinalization: true})
	require.True(t, manifestContains(t, manifest, "disable-tenant-finalization"))
}

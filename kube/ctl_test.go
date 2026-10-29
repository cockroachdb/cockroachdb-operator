package kube_test

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/tenantctrl"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kuberecord"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/testenv"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/rest/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestPatchValidatesNamespace(t *testing.T) {
	ctl := kubetest.NewFakeCtl(kubetest.Options{Options: kube.Options{
		Mapper: &kubetest.FakeRESTMapper{},
	}}).WithNamespace("expected")
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:      "config",
		Namespace: "other",
	}}

	err := ctl.Patch(context.Background(), configMap, client.MergeFrom(configMap.DeepCopy()))
	require.EqualError(t, err, "expected ConfigMap to have namespace 'expected', got 'other'")
}

func TestFetchJobPodLogsUnavailable(t *testing.T) {
	const (
		namespace = "test"
		jobName   = "cleanup"
		jobUID    = types.UID("cleanup-uid")
	)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      jobName,
		Namespace: namespace,
		UID:       jobUID,
	}}
	labels := map[string]string{
		"job-name":       jobName,
		"controller-uid": string(jobUID),
	}
	olderPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:              "cleanup-old",
		Namespace:         namespace,
		Labels:            labels,
		CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
	}}
	newerPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:              "cleanup-new",
		Namespace:         namespace,
		Labels:            labels,
		CreationTimestamp: metav1.Now(),
	}}

	tests := []struct {
		name     string
		objects  []kube.Object
		expected string
	}{
		{name: "no Pods", expected: "because no Pods were found"},
		{
			name:     "newest Pod has no containers",
			objects:  []kube.Object{newerPod, olderPod},
			expected: "because Pod cleanup-new has no containers",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctl := kubetest.NewFakeCtl(kubetest.Options{}, test.objects...).WithNamespace(namespace)
			logs := ctl.FetchJobPodLogs(context.Background(), job)

			require.Contains(t, logs, "could not get logs for Job test/cleanup")
			require.Contains(t, logs, test.expected)
		})
	}
}

func TestIntegrationCtl(t *testing.T) {
	suite.Run(t, new(CtlTestSuite))
}

type CtlTestSuite struct {
	suite.Suite
	ctl    *kube.Ctl
	ctx    context.Context
	newCtl func(kube.Options) *kube.Ctl
}

func (s *CtlTestSuite) SetupSuite() {
	env := testenv.New(s.T(), testenv.SingleZoneConfig)
	ctl := env.DefaultCluster().Ctl
	s.ctl = ctl.WithNamespace(testenv.EnsureNamespace(s.T(), ctl))
	s.ctx = context.Background()
	s.newCtl = func(opts kube.Options) *kube.Ctl {
		ctl, err := kube.NewCtlWithOptions(env.DefaultCluster().Config, opts)
		s.NoError(err)
		return ctl.WithNamespace(s.ctl.Namespace())
	}
}

// TestDefaultScheme ensures that our ctl instance uses the default Scheme.
func (s *CtlTestSuite) TestDefaultScheme() {
	// Get globally-scoped kube.Ctl instance.
	ctl := s.ctl.WithNamespace("")

	var crdbTenant apiextv1.CustomResourceDefinition

	// Ensure that we can interact with CRDs
	s.NoError(ctl.Get(s.ctx, "crdbtenants.crdb.cockroachlabs.com", &crdbTenant))
}

// TestCustomResourceDefinitionUpdate ensures that CustomResourceDefinition
// objects can be updated. This object is structured, and yet contains an Open
// API v3 schema field. This unstructured field doesn't work with the
// StrategicMergePatch, so kube.Ctl must fall back to the older MergePatch.
// This test regresses the error that was causing Apply operations on CRDs to
// fail:
//
//	"merging an object in json but data type is not struct, instead is: map"
func (s *CtlTestSuite) TestOpenAPISchema() {
	// Get globally-scoped kube.Ctl instance.
	ctl := s.ctl.WithNamespace("")

	// Fetch the CRD for CrdbTenant.
	var crdbTenant apiextv1.CustomResourceDefinition
	s.NoError(ctl.Get(s.ctx, "crdbtenants.crdb.cockroachlabs.com", &crdbTenant))

	// Attempt to update the Validation field, which uses Open API v3 schema.
	props := crdbTenant.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties
	apiVersion := props["apiVersion"]
	apiVersion.Description = "new description"
	props["apiVersion"] = apiVersion

	s.NoError(ctl.Apply(s.ctx, &crdbTenant))
}

func (s *CtlTestSuite) TestNamespaceScopedRole() {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "my-role",
			Labels: map[string]string{"app": "my-app"},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get"},
			},
		},
	}

	// Make sure to remove all finalizers when the test ends, or this
	// resource and the test namespace can't be deleted, which causes hangs.
	s.T().Cleanup(func() {
		role.Finalizers = nil
		_ = s.ctl.Apply(s.ctx, role)
		_ = s.ctl.Delete(s.ctx, role)
	})

	// Role resource not found.
	unknownRole := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "unknown-role"}}
	s.EqualError(s.ctl.Delete(s.ctx, unknownRole), "roles.rbac.authorization.k8s.io \"unknown-role\" not found")

	// Create role with Apply.
	s.NoError(s.ctl.Apply(s.ctx, role))
	getRole := &rbacv1.Role{}
	s.NoError(s.ctl.Get(s.ctx, "my-role", getRole))
	s.Equal(map[string]string{"app": "my-app"}, getRole.Labels)
	s.Equal(1, len(getRole.Rules))

	// Apply encounters error. This error also tests that lists are merged
	// using the strategic-merge-patch methodology - there would be no error
	// if both finalizer values weren't merged into one list.
	errorRole := role.DeepCopy()
	errorRole.Finalizers = []string{"orphan"}
	s.NoError(s.ctl.UpdateWithOptions(s.ctx, kube.UpdateOptions{NoLastAppliedAnnotation: true}, errorRole))
	errorRole.Finalizers = []string{"foregroundDeletion"}
	err := s.ctl.Apply(s.ctx, errorRole)
	s.Error(err)
	s.Contains(err.Error(), "finalizer orphan and foregroundDeletion cannot be both set")

	// Update existing role.
	updateRole := &rbacv1.Role{}
	s.NoError(s.ctl.Get(s.ctx, "my-role", updateRole))
	updateRole.Rules[0].Verbs[0] = "list"
	updateRole.Finalizers = nil
	s.NoError(s.ctl.UpdateWithOptions(s.ctx, kube.UpdateOptions{NoLastAppliedAnnotation: true}, updateRole))
	getRole = &rbacv1.Role{}
	s.NoError(s.ctl.Get(s.ctx, "my-role", getRole))
	s.Equal("list", getRole.Rules[0].Verbs[0])

	// Ensure that Update does not set last-applied by triggering a conflict
	// between "list" and "get" verbs.
	err = s.ctl.Apply(s.ctx, role)
	s.Error(err)
	s.Contains(err.Error(), "unable to generate patch")

	// Apply to existing role.
	role.Rules[0].Verbs[0] = "list"
	role.Labels = map[string]string{"app2": "my-app2"}
	s.NoError(s.ctl.Apply(s.ctx, role))
	var list rbacv1.RoleList
	s.NoError(s.ctl.List(s.ctx, &kube.ListOptions{}, &list))
	s.Len(list.Items, 1)
	s.Equal(map[string]string{"app2": "my-app2"}, list.Items[0].Labels)
	s.Equal("list", list.Items[0].Rules[0].Verbs[0])

	// Delete role.
	deleteRole := &rbacv1.Role{ObjectMeta: role.ObjectMeta}
	s.NoError(s.ctl.Delete(s.ctx, deleteRole))
	s.EqualError(s.ctl.Get(s.ctx, "my-role", deleteRole), "roles.rbac.authorization.k8s.io \"my-role\" not found")
	s.EqualError(s.ctl.Delete(s.ctx, deleteRole), "roles.rbac.authorization.k8s.io \"my-role\" not found")
}

func (s *CtlTestSuite) TestApplyMultipleObjects() {
	// Apply two roles at once.
	role1 := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "my-role-1",
			Labels: map[string]string{"app": "my-app"},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get"},
			},
		},
	}

	role2 := role1.DeepCopy()
	role2.Name = "my-role-2"
	role2.ObjectMeta.Labels = nil

	// Ensure the roles are always cleaned up.
	s.T().Cleanup(func() {
		_ = s.ctl.Delete(s.ctx, role1)
		_ = s.ctl.Delete(s.ctx, role2)
	})

	s.NoError(s.ctl.Apply(s.ctx, role1, role2))
	getRole := &rbacv1.Role{}
	s.NoError(s.ctl.Get(s.ctx, "my-role-1", getRole))
	s.Equal(1, len(getRole.Rules))
	s.NoError(s.ctl.Get(s.ctx, "my-role-2", getRole))
	s.Equal(1, len(getRole.Rules))

	// Apply again with updated roles.
	role1.Rules[0].Verbs[0] = "list"
	role2.Rules[0].Verbs[0] = "list"
	s.NoError(s.ctl.Apply(s.ctx, role1, role2))
	s.NoError(s.ctl.Get(s.ctx, "my-role-1", getRole))
	s.Equal("list", getRole.Rules[0].Verbs[0])
	s.NoError(s.ctl.Get(s.ctx, "my-role-2", getRole))
	s.Equal("list", getRole.Rules[0].Verbs[0])

	// List multiple roles and with options.
	var list rbacv1.RoleList
	s.NoError(s.ctl.List(s.ctx, &kube.ListOptions{}, &list))
	s.Len(list.Items, 2)
	selector := kube.ParseLabelOrDie("app=my-app")
	s.NoError(s.ctl.List(s.ctx, &kube.ListOptions{LabelSelector: selector}, &list))
	s.Len(list.Items, 1)
}

func (s *CtlTestSuite) TestDiff() {
	rolePatchOrig := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "my-role-1",
			Labels: map[string]string{"app": "my-app"},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get"},
			},
		},
	}

	rolePatchUpdate := rolePatchOrig.DeepCopy()
	rolePatchUpdate.ObjectMeta.Annotations = map[string]string{}
	rolePatchUpdate.ObjectMeta.Labels["test-label"] = "test-value"

	roleCreate := rolePatchOrig.DeepCopy()
	roleCreate.ObjectMeta.Name = "role-patch-create"

	roleDelete := rolePatchOrig.DeepCopy()
	roleDelete.ObjectMeta.Name = "role-patch-delete"

	// Ensure the role is always cleaned up.
	s.T().Cleanup(func() {
		_ = s.ctl.Delete(s.ctx, rolePatchOrig)
		_ = s.ctl.Delete(s.ctx, roleCreate)
		_ = s.ctl.Delete(s.ctx, roleDelete)
	})

	s.NoError(s.ctl.Apply(s.ctx, rolePatchOrig))

	objs := []kube.Object{rolePatchUpdate, roleCreate}
	objs = append(objs, kube.Tombstone(roleDelete)...)
	diffs, err := s.ctl.Diff(s.ctx, objs...)
	s.NoError(err)
	s.Len(diffs, 3)

	s.Equal(diffs[0].Operation, kube.Update)
	s.Equal(string(diffs[0].Patch), `{"metadata":{"labels":{"test-label":"test-value"}}}`)

	s.Equal(diffs[1].Operation, kube.Create)
	s.Equal(string(diffs[1].Patch), `{"metadata":{"labels":{"app":"my-app"},"name":"role-patch-create","namespace":"test-integration-ctl"},"rules":[{"apiGroups":[""],"resources":["pods"],"verbs":["get"]}]}`)

	s.Equal(diffs[2].Operation, kube.Delete)
	s.Nil(diffs[2].Patch)
}

func (s *CtlTestSuite) TestTombstones() {
	svcFn := func(name string) *corev1.Service {
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				Labels: map[string]string{"app": "my-app"},
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{
					{Port: 80},
				},
				Selector: map[string]string{
					"app": name,
				},
			},
		}
	}

	// Create closures to return out Roles, similar to how the manifest style
	// packages work.
	// Additionally, objects can't be recreated if their ResourceVersion is set
	// so we work around that by recreating them every time.
	svc1 := func() *corev1.Service { return svcFn("my-service-1") }
	svc2 := func() *corev1.Service { return svcFn("my-service-2") }

	// Ensure the roles are always cleaned up.
	s.T().Cleanup(func() {
		_ = s.ctl.Delete(s.ctx, svc1())
		_ = s.ctl.Delete(s.ctx, svc2())
	})

	assertExists := func(name string, obj kube.Object) {
		s.NoErrorf(s.ctl.Get(s.ctx, name, obj), "%T '%s' not found", obj, name)
	}

	assertNotExists := func(name string, obj kube.Object) {
		err := s.ctl.Get(s.ctx, name, obj)
		s.True(kubeerrors.IsNotFound(err), "%T '%s' should not exist", obj, name)
	}

	s.NoError(s.ctl.Apply(s.ctx, svc1(), svc2()))
	assertExists("my-service-1", &corev1.Service{})
	assertExists("my-service-2", &corev1.Service{})

	s.NoError(s.ctl.Apply(s.ctx, kube.Tombstone(svc1(), svc2())...))
	assertNotExists("my-service-1", &corev1.Service{})
	assertNotExists("my-service-2", &corev1.Service{})

	s.NoError(s.ctl.Apply(s.ctx, kube.Tombstone(svc1(), svc2())...))
	assertNotExists("my-service-1", &corev1.Service{})
	assertNotExists("my-service-2", &corev1.Service{})

	s.NoError(s.ctl.Apply(s.ctx, append(kube.Tombstone(svc2()), svc1())...))
	assertExists("my-service-1", &corev1.Service{})
	assertNotExists("my-service-2", &corev1.Service{})

	s.NoError(s.ctl.Apply(s.ctx, append(kube.Tombstone(svc1()), svc2())...))
	assertNotExists("my-service-1", &corev1.Service{})
	assertExists("my-service-2", &corev1.Service{})

	s.Run("tombstone target is immutable", func() {
		// Caller creates some resources.
		obj1, obj2 := *svc1(), *svc2()
		s.NoError(s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{Overwrite: true}, &obj1, &obj2))

		// Caller marks some resource with a tombstone, needs to pass in ptr per
		// the signature.
		tomb := kube.Tombstone(&obj1)

		// Caller re-uses the variable for other things and applies the
		// tombstone after.
		obj1 = obj2
		s.ctl.Apply(s.ctx, tomb...)

		// The tombstone target is unaffected despite the obj1 pointing to some
		// other resource.
		assertNotExists("my-service-1", &corev1.Service{})
		assertExists("my-service-2", &corev1.Service{})
	})
}

func (s *CtlTestSuite) TestRootLevelObject() {
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "my-role",
			Labels: map[string]string{"app": "my-app"},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get"},
			},
		},
	}

	// Ensure role is always cleaned up.
	s.T().Cleanup(func() {
		_ = s.ctl.Delete(s.ctx, role)
	})

	// Create role with Apply.
	s.NoError(s.ctl.Apply(s.ctx, role))
	getRole := &rbacv1.ClusterRole{}
	s.NoError(s.ctl.Get(s.ctx, "my-role", getRole))
	s.Equal(1, len(getRole.Rules))

	// Update existing role.
	updateRole := &rbacv1.ClusterRole{}
	s.NoError(s.ctl.Get(s.ctx, "my-role", updateRole))
	updateRole.Rules[0].Verbs[0] = "list"
	s.NoError(s.ctl.UpdateWithOptions(s.ctx, kube.UpdateOptions{NoLastAppliedAnnotation: true}, updateRole))
	var list rbacv1.ClusterRoleList
	selector := kube.ParseLabelOrDie("app=my-app")
	s.NoError(s.ctl.List(s.ctx, &kube.ListOptions{LabelSelector: selector}, &list))
	s.Len(list.Items, 1)
	s.Equal("list", list.Items[0].Rules[0].Verbs[0])

	// Delete role.
	deleteRole := &rbacv1.ClusterRole{ObjectMeta: role.ObjectMeta}
	s.NoError(s.ctl.Delete(s.ctx, deleteRole))
	s.EqualError(s.ctl.Get(s.ctx, "my-role", deleteRole), "clusterroles.rbac.authorization.k8s.io \"my-role\" not found")
}

func (s *CtlTestSuite) TestRemoveAnnotations() {
	// NB: This CRD has been left as extv1beta1 rather than being upgrade to
	// extv1 to ensure test coverage of extv1beta1.
	crd := &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mycrds.crdb.cockroachlabs.com",
			Annotations: map[string]string{
				"First":  "1st",
				"Second": "2nd",
			},
		},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Scope: "Namespaced",
			Group: "crdb.cockroachlabs.com",
			Names: apiextv1.CustomResourceDefinitionNames{
				Kind:     "MyCrd",
				ListKind: "MyCrdList",
				Plural:   "mycrds",
				Singular: "mycrd",
			},
			Versions: []apiextv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1",
					Storage: true,
					Schema: &apiextv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextv1.JSONSchemaProps{
							// spec.validation.openAPIV3Schema.type must be non empty
							// https://github.com/kubernetes/kubernetes/issues/81445
							Type: "object",
						},
					},
				},
			},
		},
	}

	// Ensure CRD is always cleaned up.
	s.T().Cleanup(func() {
		_ = s.ctl.Delete(s.ctx, crd)
	})

	s.NoError(s.ctl.Apply(s.ctx, crd))

	// Clear all our annotations.
	crd.Annotations = nil
	s.NoError(s.ctl.Apply(s.ctx, crd))

	getCrd := &apiextv1.CustomResourceDefinition{}
	s.NoError(s.ctl.Get(s.ctx, "mycrds.crdb.cockroachlabs.com", getCrd))

	// First and Second annotations should have been removed, but a
	// last-applied-configuration annotation should still be present.
	s.Len(getCrd.Annotations, 1)
	s.Contains(getCrd.Annotations, "kubectl.kubernetes.io/last-applied-configuration")
}

func (s *CtlTestSuite) TestCustomResource() {
	const tenantID = crdbv1beta1.TenantID(10)
	tenant := &crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:        tenantID.Tag(),
			Finalizers:  []string{"finalizer2", "finalizer1"},
			Annotations: map[string]string{tenantctrl.DisableReconcile: "true"},
		},
		Spec: crdbv1beta1.CrdbTenantSpec{TenantID: tenantID},
	}

	// Make sure to remove all finalizers when the test ends, or this resource
	// and the test namespace can't be deleted, which causes hangs.
	s.T().Cleanup(func() {
		tenant.Finalizers = nil
		_ = s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{NoWait: true}, tenant)
		_ = s.ctl.Delete(s.ctx, tenant)
	})

	// Create the tenant with Apply.
	s.NoError(s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{NoWait: true}, tenant))
	getTenant := &crdbv1beta1.CrdbTenant{}
	s.NoError(s.ctl.Get(s.ctx, tenant.Name, getTenant))
	s.Equal(tenantID, getTenant.Spec.TenantID)
	s.Equal([]string{"finalizer2", "finalizer1"}, getTenant.Finalizers)

	// Update the finalizers (without setting last-applied) in order to test
	// Apply list merging behavior below.
	tenant.Finalizers = []string{"finalizer1", "finalizer2", "finalizer3"}
	s.NoError(s.ctl.UpdateWithOptions(s.ctx, kube.UpdateOptions{NoLastAppliedAnnotation: true, NoWait: true}, tenant))

	// Apply changes to existing tenant. Note that strategic-merge-patch
	// doesn't work for custom resources, so the Finalizers list will simply
	// be overwritten rather than merged.
	tenant = &crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:        tenantID.Tag(),
			Finalizers:  []string{"finalizer4"},
			Annotations: map[string]string{tenantctrl.DisableReconcile: "true"},
		},
		Spec: crdbv1beta1.CrdbTenantSpec{TenantID: tenantID},
	}
	s.NoError(s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{NoWait: true}, tenant))
	getTenant = &crdbv1beta1.CrdbTenant{}
	s.NoError(s.ctl.Get(s.ctx, tenant.Name, getTenant))
	s.Equal([]string{"finalizer4"}, getTenant.Finalizers)

	// Update the tenant.
	var updateTenant crdbv1beta1.CrdbTenant
	s.NoError(s.ctl.Get(s.ctx, tenant.Name, &updateTenant))
	updateTenant.Finalizers = nil
	updateTenant.Spec.DisableAutoscale = true
	s.NoError(s.ctl.UpdateWithOptions(s.ctx, kube.UpdateOptions{NoWait: true}, &updateTenant))
	getTenant = &crdbv1beta1.CrdbTenant{}
	s.NoError(s.ctl.Get(s.ctx, tenant.Name, getTenant))
	s.Equal(true, getTenant.Spec.DisableAutoscale)

	// Update the tenant status.
	updateTenant.Status.Pods = 1
	s.NoError(s.ctl.UpdateStatus(s.ctx, &updateTenant))
	getTenant = &crdbv1beta1.CrdbTenant{}
	s.NoError(s.ctl.Get(s.ctx, tenant.Name, getTenant))
	s.Equal(int32(1), getTenant.Status.Pods)
	updateTenant.Status.Pods = 0
	s.NoError(s.ctl.UpdateStatus(s.ctx, &updateTenant))

	// Delete the tenant.
	s.NoError(s.ctl.Delete(s.ctx, tenant))
}

func (s *CtlTestSuite) TestApplyConflict() {
	// Create the tenant with Apply so that last-applied annotation gets
	// added to the object.
	const tenantID = crdbv1beta1.TenantID(30)
	tenant := &crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:        tenantID.Tag(),
			Annotations: map[string]string{tenantctrl.DisableReconcile: "true"},
		},
		Spec: crdbv1beta1.CrdbTenantSpec{TenantID: tenantID},
	}
	s.NoError(s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{NoWait: true}, tenant))

	// Ensure the tenant is always cleaned up.
	s.T().Cleanup(func() {
		_ = s.ctl.Delete(s.ctx, tenant)
	})

	// Update the object without updating the last-applied annotation.
	tenant.Annotations["foo"] = "bar"
	s.NoError(s.ctl.UpdateWithOptions(s.ctx, kube.UpdateOptions{NoLastAppliedAnnotation: true, NoWait: true}, tenant))

	// Try to apply the tenant with a new value, and expect conflict.
	tenant.Annotations["foo"] = "box"
	err := s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{NoWait: true}, tenant)
	s.Error(err)
	s.Contains(err.Error(), "unable to generate patch for CrdbTenant/tenant-30")

	// Try again, this time with Overwrite=true.
	err = s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{NoWait: true, Overwrite: true}, tenant)
	s.NoError(err)
	getTenant := &crdbv1beta1.CrdbTenant{}
	s.NoError(s.ctl.Get(s.ctx, tenant.Name, getTenant))
	s.Equal("box", getTenant.Annotations["foo"])

	// Now set the ForceApplyOverwrite annotation to true on the server.
	tenant.Annotations[kube.ForceApplyOverwriteAnnotation] = "true"
	s.NoError(s.ctl.UpdateWithOptions(s.ctx, kube.UpdateOptions{NoWait: true}, tenant))

	// Update the object without updating the last-applied annotation.
	tenant.Annotations["foo"] = "roach"
	s.NoError(s.ctl.UpdateWithOptions(s.ctx, kube.UpdateOptions{NoLastAppliedAnnotation: true, NoWait: true}, tenant))

	// Try to apply the tenant with a new value, and expect no conflicts
	// because we forced Overwrite=true.
	err = s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{NoWait: true}, tenant)
	s.NoError(err)
	s.NoError(s.ctl.Get(s.ctx, tenant.Name, getTenant))
	s.Equal("roach", getTenant.Annotations["foo"])

	// Now set the ForceApplyOverwrite annotation to an invalid value on the server.
	tenant.Annotations[kube.ForceApplyOverwriteAnnotation] = "invalid-value"
	s.NoError(s.ctl.UpdateWithOptions(s.ctx, kube.UpdateOptions{NoWait: true}, tenant))

	// Update the object without updating the last-applied annotation.
	tenant.Annotations["foo"] = "bar"
	s.NoError(s.ctl.UpdateWithOptions(s.ctx, kube.UpdateOptions{NoLastAppliedAnnotation: true, NoWait: true}, tenant))

	// Try to apply the tenant with a new value, and expect conflicts. There
	// should not be an implicit Overwrite=true here.
	tenant.Annotations["foo"] = "box"
	err = s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{NoWait: true}, tenant)
	s.Error(err)
	s.Contains(err.Error(), "unable to generate patch for CrdbTenant/tenant-30")

	// Try again, this time with Overwrite=true. There should not be an implicit
	// Overwrite=false here.
	err = s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{NoWait: true, Overwrite: true}, tenant)
	s.NoError(err)
	s.NoError(s.ctl.Get(s.ctx, tenant.Name, getTenant))
	s.Equal("box", getTenant.Annotations["foo"])

	// Finally, set the ForceApplyOverwrite annotation to false on the server.
	// This has to be last because doing this destroys the object, and we
	// can't do Overwrite=true anymore if a conflict exists.
	tenant.Annotations[kube.ForceApplyOverwriteAnnotation] = "false"
	s.NoError(s.ctl.UpdateWithOptions(s.ctx, kube.UpdateOptions{NoWait: true}, tenant))

	// Update the object without updating the last-applied annotation.
	tenant.Annotations["foo"] = "bar"
	s.NoError(s.ctl.UpdateWithOptions(s.ctx, kube.UpdateOptions{NoLastAppliedAnnotation: true, NoWait: true}, tenant))

	// Try to apply the tenant with a new value, and expect conflicts.
	tenant.Annotations["foo"] = "roacher"
	err = s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{NoWait: true}, tenant)
	s.Error(err)
	s.Contains(err.Error(), "unable to generate patch for CrdbTenant/tenant-30")

	// Trying with Overwrite=true would also result in a conflict since we
	// forced Overwrite=false.
	err = s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{NoWait: true, Overwrite: true}, tenant)
	s.Error(err)
	s.Contains(err.Error(), "unable to generate patch for CrdbTenant/tenant-30")

	// Final value should be bar.
	s.NoError(s.ctl.Get(s.ctx, tenant.Name, getTenant))
	s.Equal("bar", getTenant.Annotations["foo"])
}

func (s *CtlTestSuite) TestUpdateApplyNoConflicts() {
	tenant := &crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "tenant-70",
			Annotations: map[string]string{tenantctrl.DisableReconcile: "true"},
		},
		Spec: crdbv1beta1.CrdbTenantSpec{TenantID: 70},
	}
	s.NoError(s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{NoWait: true}, tenant))

	// Ensure the tenant is always cleaned up.
	s.T().Cleanup(func() {
		_ = s.ctl.Delete(s.ctx, tenant)
	})

	// convenience function to generate string pointers.
	tp := func(s string) *string {
		return &s
	}

	tenant.Spec.TenantPool = tp("first")
	s.NoError(s.ctl.UpdateWithOptions(s.ctx, kube.UpdateOptions{NoWait: true}, tenant))

	// No conflict generated as Update sets the last applied annotation
	tenant.Spec.TenantPool = tp("second")
	s.NoError(s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{NoWait: true}, tenant))

	// Generate a conflict by not updating the last-applied annotation.
	tenant.Spec.TenantPool = tp("third")
	s.NoError(s.ctl.UpdateWithOptions(s.ctx, kube.UpdateOptions{NoLastAppliedAnnotation: true, NoWait: true}, tenant))

	// Expect an error now that last-applied has gotten out of sync.
	tenant.Spec.TenantPool = tp("fourth")
	err := s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{NoWait: true}, tenant)
	s.Error(err)
	s.Contains(err.Error(), "unable to generate patch for CrdbTenant/tenant-70")
}

func (s *CtlTestSuite) TestWait() {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-pod",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "nginx",
					Image: "nginx:1.14.2",
				},
			},
		},
	}

	// Ensure the pod is always cleaned up.
	s.T().Cleanup(func() {
		_ = s.ctl.Delete(s.ctx, pod)
	})

	// First call Apply with NoWait=true.
	applyPod := pod.DeepCopy()
	s.NoError(s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{NoWait: true}, applyPod))
	s.Error(kube.IsPodReady(applyPod))

	// Now call Apply with NoWait=false.
	s.NoError(s.ctl.Apply(s.ctx, applyPod))
	s.NoError(kube.IsPodReady(applyPod))
	s.Contains(applyPod.Status.ContainerStatuses[0].Image, "nginx:1.14.2")

	// Ensure the pod was actually created and is ready.
	getPod := &corev1.Pod{}
	s.NoError(s.ctl.Get(s.ctx, "my-pod", getPod))
	s.NoError(kube.IsPodReady(getPod))
	s.Contains(applyPod.Status.ContainerStatuses[0].Image, "nginx:1.14.2")

	// First call Delete with NoWait=true.
	s.NoError(s.ctl.DeleteWithOptions(s.ctx, kube.DeleteOptions{NoWait: true}, getPod))
	s.NoError(s.ctl.Get(s.ctx, "my-pod", getPod))

	// Now call Delete with NoWait=false.
	s.NoError(s.ctl.Delete(s.ctx, getPod))
	s.EqualError(s.ctl.Get(s.ctx, "my-pod", getPod), "pods \"my-pod\" not found")
}

func (s *CtlTestSuite) TestDeployment() {
	// Deliberately keep Spec.Replicas set to nil, in order to ensure that K8s
	// updates it after an Apply call.
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-deployment",
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "my-app"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "my-app"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "nginx",
							Image: "nginx:1.14.2",
						},
					},
				},
			},
		},
	}

	// Ensure the deployment is always cleaned up.
	s.T().Cleanup(func() {
		_ = s.ctl.Delete(s.ctx, deployment)
	})

	// First create deployment.
	createDeployment := *deployment
	s.NoError(s.ctl.Apply(s.ctx, &createDeployment))
	s.NoError(kube.IsReady(&createDeployment))
	s.NotNil(createDeployment.Spec.Replicas)
	s.Equal(int32(1), *createDeployment.Spec.Replicas)
	s.Equal(int64(1), createDeployment.Status.ObservedGeneration)

	// Then patch existing deployment with new image.
	patchDeployment := *deployment
	patchDeployment.Spec.Template.Spec.Containers[0].Image = "nginx:1.18.0"
	s.NoError(s.ctl.Apply(s.ctx, &patchDeployment))
	s.NoError(kube.IsReady(&patchDeployment))
	s.NotNil(patchDeployment.Spec.Replicas)
	s.Equal(int32(1), *patchDeployment.Spec.Replicas)
	s.Equal(int64(2), patchDeployment.Status.ObservedGeneration)

	// Update replicas in a way that is opaque to Apply to simulate deployments
	// that are scaled by another service in the cluster. (calico-typha and
	// calico-typha-autoscaler for example). As replicas is nil, not 0, patches
	// should continue to work as expected.
	var got appsv1.Deployment
	s.NoError(s.ctl.Get(s.ctx, deployment.Name, &got))
	// nil replicas defaults to 1, so 2 is picked to ensure that replicas
	// actually changes.
	replicas := int32(2)
	got.Spec.Replicas = &replicas
	s.NoError(s.ctl.UpdateWithOptions(s.ctx, kube.UpdateOptions{NoLastAppliedAnnotation: true}, &got))

	// Scale the deployment to zero replicas.
	s.NoError(s.ctl.Get(s.ctx, deployment.Name, &got))
	replicas = int32(0)
	got.Spec.Replicas = &replicas
	// TODO(chrisseto): it's unclear why Overwrite is required here. We're just
	// testing that scale to zero works as expected but patch generation is a
	// bit funky.
	s.NoError(s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{Overwrite: true}, &got))
}

func (s *CtlTestSuite) TestStatefulSet() {
	// Deliberately keep Spec.Replicas set to nil, in order to ensure that K8s
	// updates it after an Apply call.
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-sts",
		},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "my-app"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "my-app"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "nginx",
							Image: "nginx:1.14.2",
						},
					},
				},
			},
		},
	}

	// Ensure the sts is always cleaned up.
	s.T().Cleanup(func() {
		_ = s.ctl.Delete(s.ctx, sts)
	})

	// First create sts.
	createSts := *sts
	s.NoError(s.ctl.Apply(s.ctx, &createSts))
	s.NoError(kube.IsReady(&createSts))
	s.NotNil(createSts.Spec.Replicas)
	s.Equal(int32(1), *createSts.Spec.Replicas)
	s.Equal(int64(1), createSts.Status.ObservedGeneration)

	// Then patch existing sts with new image.
	patchSts := *sts
	patchSts.Spec.Template.Spec.Containers[0].Image = "nginx:1.18.0"
	s.NoError(s.ctl.Apply(s.ctx, &patchSts))
	s.Equal(int32(0), patchSts.Status.UpdatedReplicas)

	s.NoError(s.ctl.WaitFor(s.ctx, kube.IsQuiescent, &patchSts))
	s.NotNil(patchSts.Spec.Replicas)
	s.Equal(int32(1), *patchSts.Spec.Replicas)
	s.Equal(int64(2), patchSts.Status.ObservedGeneration)
	s.Equal(int32(1), patchSts.Status.UpdatedReplicas)
}

func (s *CtlTestSuite) TestNoLastAppliedAnnotations() {
	svc := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-svc",
		},
		Spec: corev1.ServiceSpec{
			Type:                  corev1.ServiceTypeNodePort,
			Selector:              map[string]string{"app": "myapp"},
			ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyTypeLocal,
			Ports: []corev1.ServicePort{
				{Port: 32521, TargetPort: intstr.FromInt(32521)},
			},
		},
	}

	// Create a service and strip it's last-applied annotation to mess with our
	// diffing algorithm. Services are "special" because they have a server set
	// field .Spec.ClusterIP which is immutable after creation.
	legacy := svc.DeepCopy()
	s.NoError(s.ctl.Apply(s.ctx, legacy))
	s.NoError(s.ctl.Get(s.ctx, legacy.Name, legacy))
	s.NotZero(legacy.Spec.ClusterIP)
	legacy.Annotations = nil
	s.NoError(s.ctl.UpdateWithOptions(s.ctx, kube.UpdateOptions{NoLastAppliedAnnotation: true}, legacy))

	// Ensure that no-op updates apply without any errors.
	cp := svc.DeepCopy()
	s.NoError(s.ctl.Apply(s.ctx, cp))

	deployment := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-deployment",
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "my-app"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "my-app"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "nginx",
							Image: "nginx:1.14.2",
						},
					},
				},
			},
		},
	}

	legacyDep := deployment.DeepCopy()
	s.NoError(s.ctl.Apply(s.ctx, legacyDep))
	legacyDep.Annotations = nil
	s.NoError(s.ctl.UpdateWithOptions(s.ctx, kube.UpdateOptions{NoLastAppliedAnnotation: true}, legacyDep))

	// Ensure that no-op updates apply without any errors.
	s.NoError(s.ctl.Apply(s.ctx, deployment.DeepCopy()))
}

func (s *CtlTestSuite) TestAddonMangerReconcile() {
	// Create a deployment that looks like it's managed by the addon manager.
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-deployment",
			Labels: map[string]string{
				"addonmanager.kubernetes.io/mode": "Reconcile",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "my-app"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "my-app"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "nginx",
							Image: "nginx:1.14.2",
						},
					},
				},
			},
		},
	}

	// Ensure the deployment is always cleaned up.
	s.T().Cleanup(func() {
		_ = s.ctl.Delete(s.ctx, deployment)
	})

	// First create deployment.
	createDeployment := *deployment
	s.NoError(s.ctl.Apply(s.ctx, &createDeployment))

	// Now, try to modify it and verify that Apply and Update rejects our calls.
	s.EqualError(s.ctl.Apply(s.ctx, deployment), "addon-manager managed objects are not supported by Apply or Update. Use UpdateWithOptions(NoLastAppliedAnnotation=true)")
	s.EqualError(s.ctl.Update(s.ctx, deployment), "addon-manager managed objects are not supported by Apply or Update. Use UpdateWithOptions(NoLastAppliedAnnotation=true)")
	// However, s.ctl.UpdateOptions allows us to bypass this check.
	s.NoError(s.ctl.UpdateWithOptions(s.ctx, kube.UpdateOptions{NoLastAppliedAnnotation: true}, deployment))
}

func (s *CtlTestSuite) TestConcurrentApply() {
	// kube recordings don't work with concurrent calls to K8s API.
	testutils.SkipIfKuberecord(s.T())

	const tenantID = crdbv1beta1.TenantID(40)
	tenant := &crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:        tenantID.Tag(),
			Annotations: map[string]string{tenantctrl.DisableReconcile: "true"},
		},
		Spec: crdbv1beta1.CrdbTenantSpec{TenantID: tenantID, Pods: 0},
	}
	s.NoError(s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{NoWait: true}, tenant))

	// Ensure the tenant is always cleaned up.
	s.T().Cleanup(func() {
		_ = s.ctl.Delete(s.ctx, tenant)
	})

	var wait sync.WaitGroup

	wait.Go(func() {
		for range 5 {
			var updateTenant crdbv1beta1.CrdbTenant
			_ = s.ctl.Get(s.ctx, tenant.Name, &updateTenant)
			updateTenant.Status.Pods++
			_ = s.ctl.UpdateStatus(s.ctx, &updateTenant)
		}
	})

	for range 5 {
		tenant.Spec.Pods++
		s.NoError(s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{NoWait: true}, tenant))
	}

	wait.Wait()
}

func (s *CtlTestSuite) TestRollingRestart() {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "engine-x",
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "nginx"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "nginx"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "nginx",
							Image: "nginx:1.14.2",
						},
					},
				},
			},
		},
	}

	// Ensure the deployment is always cleaned up.
	s.T().Cleanup(func() {
		_ = s.ctl.Delete(s.ctx, deployment)
	})

	// First create deployment.
	s.NoError(s.ctl.Apply(s.ctx, deployment))

	labelSelector := kube.ParseLabelOrDie("app=nginx")
	fieldSelector := fields.ParseSelectorOrDie("status.phase=Running")

	var preRestartPods corev1.PodList
	s.NoError(s.ctl.List(s.ctx, &kube.ListOptions{
		LabelSelector: labelSelector,
		FieldSelector: fieldSelector,
	}, &preRestartPods))
	s.Len(preRestartPods.Items, 1)

	// Then restart it.
	s.NoError(s.ctl.RollingRestart(s.ctx, deployment))
	s.NoError(kube.IsReady(deployment))

	// It's RollingRestart doesn't guarantee that the old pod will be
	// terminated so we may have to wait a short period of time before the
	// expired pods officially enter the terminating state.
	s.Eventually(func() bool {
		var postRestartPods corev1.PodList
		s.NoError(s.ctl.List(s.ctx, &kube.ListOptions{
			LabelSelector: labelSelector,
			FieldSelector: fieldSelector,
		}, &postRestartPods))

		// Ensure that new pods were actually introduced
		return len(postRestartPods.Items) == 1 &&
			preRestartPods.Items[0].UID != postRestartPods.Items[0].UID
	}, time.Minute, kuberecord.MaxBackOffInterval(time.Second)+1)
}

func (s *CtlTestSuite) TestUpdateResourceVersion() {
	const tenantID = crdbv1beta1.TenantID(70)
	tenant := &crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:        tenantID.Tag(),
			Annotations: map[string]string{tenantctrl.DisableReconcile: "true"},
		},
		Spec: crdbv1beta1.CrdbTenantSpec{TenantID: tenantID},
	}
	s.NoError(s.ctl.ApplyWithOptions(s.ctx, kube.ApplyOptions{NoWait: true}, tenant))

	// Ensure the tenant is always cleaned up.
	s.T().Cleanup(func() {
		_ = s.ctl.Delete(s.ctx, tenant)
	})

	err := s.ctl.Get(s.ctx, tenant.Name, tenant)
	s.NoError(err)
	rv := tenant.ResourceVersion

	tenant.Annotations["foo"] = "bar"
	s.NoError(s.ctl.UpdateWithOptions(s.ctx, kube.UpdateOptions{NoWait: true}, tenant))

	err = s.ctl.Get(s.ctx, tenant.Name, tenant)
	s.NoError(err)

	// Make sure that ResourceVersion is increased after successful update.
	s.Greater(tenant.ResourceVersion, rv)

	// Test that different ResourceVersion is rejected.
	tenant.Annotations["foo"] = "box"
	tenant.ResourceVersion = tenant.ResourceVersion + "1"
	s.Error(s.ctl.UpdateWithOptions(s.ctx, kube.UpdateOptions{NoWait: true}, tenant))
}

func (s *CtlTestSuite) TestReappliedObjects() {
	afterMutationCalls := 0

	ctl := s.newCtl(kube.Options{
		AfterMutation: func(object kube.Object) error {
			// Only count services
			if _, ok := object.(*corev1.Service); !ok {
				return nil
			}

			switch afterMutationCalls {
			case 0:
				s.NotContains(
					object.(*corev1.Service).Annotations,
					"service.beta.kubernetes.io/aws-load-balancer-cross-zone-load-balancing-enabled",
				)
			case 1:
				s.Contains(
					object.(*corev1.Service).Annotations,
					"service.beta.kubernetes.io/aws-load-balancer-cross-zone-load-balancing-enabled",
				)
			default:
				s.Fail("unexpected call to AfterMutation")
			}

			afterMutationCalls += 1
			return nil
		},
	})

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "aws-also-owes-joel-lunch",
			Annotations: map[string]string{
				// matches awsCrosszoneLoadBalancingAnnotation in util.go
				"service.beta.kubernetes.io/aws-load-balancer-cross-zone-load-balancing-enabled": "true",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "myapp"},
			Ports: []corev1.ServicePort{
				{Port: 32523, TargetPort: intstr.FromInt(32523)},
			},
		},
	}

	s.NoError(ctl.Apply(s.ctx, svc))

	serverSvc := &corev1.Service{}
	s.NoError(ctl.Get(s.ctx, svc.Name, serverSvc))

	// Our object has had its server side fields populated
	s.NotNil(svc.Spec.ClusterIP)
	// Our object's annotations set to their expected values
	s.Equal("true", svc.Annotations["service.beta.kubernetes.io/aws-load-balancer-cross-zone-load-balancing-enabled"])
	// Our service object has the annotation as expected
	s.Equal("true", serverSvc.Annotations["service.beta.kubernetes.io/aws-load-balancer-cross-zone-load-balancing-enabled"])
	// afterMutation has been called for 1 creation and 1 deletion
	s.Equal(2, afterMutationCalls)
}

func (s *CtlTestSuite) TestSecrets() {
	stringDataSecret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "string-data",
		},
		Data: kube.StringData(map[string]string{
			"key": "value",
		}),
	}

	stdSecret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "normal-data",
		},
		Data: map[string][]byte{
			"key": {00, 01, 02, 03},
		},
	}

	s.T().Cleanup(func() {
		_ = s.ctl.Delete(s.ctx, &stdSecret)
		_ = s.ctl.Delete(s.ctx, &stringDataSecret)
	})

	// Assert that we can re-apply secrets. This is not the case if the
	// StringData member is used because it's a write only field.
	s.NoError(s.ctl.Apply(s.ctx, &stdSecret, &stringDataSecret))
	s.NoError(s.ctl.Apply(s.ctx, &stdSecret, &stringDataSecret))

	// Re-fetch the secrets to ensure we have Kubernetes' encoding of the data.
	s.NoError(s.ctl.Get(s.ctx, stdSecret.Name, &stdSecret))
	s.NoError(s.ctl.Get(s.ctx, stringDataSecret.Name, &stringDataSecret))

	// Verify that our values have made it through unscathed.
	s.Equal([]byte("value"), stringDataSecret.Data["key"])
	s.Equal([]byte{00, 01, 02, 03}, stdSecret.Data["key"])
}

func TestApplyWithCustomBackOff(t *testing.T) {
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-pod",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "nginx",
					Image: "nginx:1.14.2",
				},
			},
		},
	}

	defaultBackoff, customBackoff := 0, 0
	resetBackoffCount := func() {
		defaultBackoff, customBackoff = 0, 0
	}
	options := kube.Options{
		BeforeMutation: kubetest.MakeReady,
		BackOff: func() backoff.BackOff {
			defaultBackoff++
			return &backoff.ZeroBackOff{}
		},
	}
	ctl := kubetest.NewFakeCtl(kubetest.Options{Options: options})

	// Invoke custom backoff. Custom backoff shouldn't replace global backoff.
	newPod1 := pod.DeepCopy()
	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{
		WaitForOptions: kube.WaitForOptions{
			BackOff: func() backoff.BackOff {
				customBackoff++
				return &backoff.ZeroBackOff{}
			},
		},
	}, newPod1))
	require.Equal(t, 1, customBackoff)
	require.Equal(t, 0, defaultBackoff)
	require.NoError(t, ctl.Delete(ctx, newPod1))
	resetBackoffCount()

	// Invoke default backoff.
	newPod2 := pod.DeepCopy()
	require.NoError(t, ctl.Apply(ctx, newPod2))
	require.Equal(t, 0, customBackoff)
	require.Equal(t, 1, defaultBackoff)
	require.NoError(t, ctl.Delete(ctx, newPod2))
	resetBackoffCount()

	// Create a new ctl with DisallowCustomBackOffs=true.
	options.DisallowCustomBackOffs = true
	ctl = kubetest.NewFakeCtl(kubetest.Options{Options: options})

	// Supply a custom backoff, which shouldn't be called.
	newPod3 := pod.DeepCopy()
	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{
		WaitForOptions: kube.WaitForOptions{
			BackOff: func() backoff.BackOff {
				customBackoff++
				return &backoff.ZeroBackOff{}
			},
		},
	}, newPod3))
	require.Equal(t, 0, customBackoff)
	require.Equal(t, 1, defaultBackoff)
	require.NoError(t, ctl.Delete(ctx, newPod3))
	resetBackoffCount()

	// Default backoff should still work.
	newPod4 := pod.DeepCopy()
	require.NoError(t, ctl.Apply(ctx, newPod4))
	require.Equal(t, 0, customBackoff)
	require.Equal(t, 1, defaultBackoff)
	require.NoError(t, ctl.Delete(ctx, newPod4))
	resetBackoffCount()
}

func TestWithCustomDoneFunc(t *testing.T) {
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-pod",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "nginx",
					Image: "nginx:1.14.2",
				},
			},
		},
	}

	ctl := kubetest.NewFakeCtl(kubetest.Options{Options: kube.Options{BackOff: func() backoff.BackOff { return &backoff.StopBackOff{} }}})

	// Invoke default doneFunc. Should timeout since pod isn't ready.
	newPod1 := pod.DeepCopy()
	err := ctl.Apply(ctx, newPod1)
	require.Regexp(t, `timed out waiting`, err.Error())

	// Invoke custom doneFunc now.
	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{
		WaitForOptions: kube.WaitForOptions{
			DoneFunc: func(obj kube.Object) error {
				if err := kubetest.MakeReady(obj); err != nil {
					return err
				}
				return kube.IsReady(obj)
			},
		},
	}, newPod1))

	done := false
	newPod1.Annotations = map[string]string{"foo": "bar"}
	require.NoError(t, ctl.UpdateWithOptions(ctx, kube.UpdateOptions{
		WaitForOptions: kube.WaitForOptions{
			DoneFunc: func(obj kube.Object) error {
				done = true
				return kube.IsReady(obj)
			},
		},
	}, newPod1))
	require.True(t, done)

	done = false
	require.NoError(t, ctl.DeleteWithOptions(ctx, kube.DeleteOptions{
		WaitForOptions: kube.WaitForOptions{
			DoneFunc: func(obj kube.Object) error {
				done = true
				return kube.IsDeleted(obj)
			},
		},
	}, newPod1))
	require.True(t, done)
}

// TestCustomClient uses kube.Ctl with a custom client.Client implementation.
func TestCustomClient(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-pod",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "nginx",
					Image: "nginx:1.14.2",
				},
			},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	// kubetest.NewFakeCtl constructs a Ctl with a fake client.Client that
	// maintains an in-memory K8s object store.
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{})
	require.NoError(t, ctl.Apply(ctx, pod))

	var myPod corev1.Pod
	require.NoError(t, ctl.Get(ctx, "my-pod", &myPod))
	require.Len(t, myPod.Spec.Containers, 1)
	require.Equal(t, "nginx", myPod.Spec.Containers[0].Name)
}

func (s *CtlTestSuite) TestDrain() {
	var nodeList corev1.NodeList
	nodeLabelSelector, err := labels.Parse("node-role.kubernetes.io/master!=true")
	s.NoError(err)
	s.NoError(s.ctl.List(s.ctx, &kube.ListOptions{
		LabelSelector: nodeLabelSelector,
	}, &nodeList))
	s.NotEmpty(nodeList.Items)
	nodeToDrain := &nodeList.Items[0]

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-pod",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "nginx",
					Image: "nginx:1.14.2",
				},
			},
			NodeSelector: map[string]string{
				"kubernetes.io/hostname": nodeToDrain.Name,
			},
		},
	}

	s.T().Cleanup(func() {
		nodeToDrain.Spec.Unschedulable = false
		_ = s.ctl.Apply(s.ctx, nodeToDrain)
		_ = s.ctl.Delete(s.ctx, pod)
	})

	s.NoError(s.ctl.Apply(s.ctx, pod))
	var getPod corev1.Pod
	s.NoError(s.ctl.Get(s.ctx, "my-pod", &getPod))

	s.NoError(s.ctl.Drain(s.ctx, nodeToDrain))

	s.True(nodeToDrain.Spec.Unschedulable)
	s.True(kubeerrors.IsNotFound(s.ctl.Get(s.ctx, "my-pod", &getPod)))
}

func (s *CtlTestSuite) TestDrainReschedule() {
	ctx := context.Background()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node",
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-pod",
			UID:  types.UID("55443"),
		},
	}

	var ctl *kube.Ctl

	// Pod eviction is applied through the GVKRestClient, so we must fake
	// the http rest calls. Instead of evicting, we update the pod UID to
	// simulate rescheduling.
	fakeRoundTripper := func(*http.Request) (*http.Response, error) {
		pod.ObjectMeta.UID = types.UID("987654")
		s.NoError(ctl.Update(ctx, pod))
		return nil, nil
	}

	ctl = kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{
			BeforeMutation: kubetest.MakeReady,
			GVKRestClient: func(gvk schema.GroupVersionKind) (rest.Interface, error) {
				return &fake.RESTClient{
					Client: fake.CreateHTTPClient(fakeRoundTripper),
				}, nil
			},
		},
	})

	s.NoError(ctl.Apply(ctx, node))
	s.NoError(ctl.Drain(ctx, node))

	// Pod has been rescheduled, so we expect fetching to succeed.
	var getPod corev1.Pod
	s.Error(ctl.Get(ctx, "my-pod", &getPod))
	s.NotEqual(getPod.UID, pod.UID)
}

func (s *CtlTestSuite) TestWaitForWithOptionsAbort() {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "abort-wait-pod",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "nginx",
					Image: "nginx:1.14.2",
				},
			},
		},
	}

	// Deep copy the pod in order to drop Apply's in memory update. Having a
	// non-ready in-memory representation forces the WaitForWithOptions to
	// consult the API server's state.
	require.NoError(s.T(), s.ctl.Apply(s.ctx, kube.DeepCopy(pod)))
	require.NoError(s.T(), s.ctl.Delete(s.ctx, pod))

	waitErr := s.ctl.WaitForWithOptions(s.ctx, kube.WaitForOptions{
		DoneFunc: kube.IsReady,
	}, pod)
	require.ErrorIs(s.T(), waitErr, kube.ErrAbortWait)
}

func (s *CtlTestSuite) TestExec() {
	s.EqualError(
		s.ctl.WithNamespace("").Exec(s.ctx, "", "", kube.ExecOptions{}),
		".Namespace must have been called with a non-empty value to use Exec",
	)

	// kube recordings don't work with exec calls to K8s API as they use SPDY
	// streams.
	testutils.SkipIfKuberecord(s.T())

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "exec-pod",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "alpine",
					Image:   "alpine:latest",
					Command: []string{"sleep", "1000"},
				},
			},
			// Set TerminationGracePeriodSeconds to a low value to speed up the
			// test runtime.
			TerminationGracePeriodSeconds: new(int64(1)),
		},
	}

	s.Assert().NoError(s.ctl.Apply(s.ctx, &pod))

	s.T().Cleanup(func() {
		_ = s.ctl.Delete(s.ctx, &pod)
	})

	var stdout bytes.Buffer
	s.Assert().NoError(s.ctl.Exec(s.ctx, pod.Name, "alpine", kube.ExecOptions{
		Command: []string{"echo", "hello world"},
		Stdout:  &stdout,
	}))

	s.Equal("hello world\n", stdout.String())

	stdout.Reset()
	err := s.ctl.Exec(s.ctx, pod.Name, "alpine", kube.ExecOptions{
		Command: []string{"fake-binary"},
		Stdout:  &stdout,
	})

	s.Contains(err.Error(), "exec: \"fake-binary\": executable file not found in $PATH")

	stdout.Reset()
	err = s.ctl.Exec(s.ctx, pod.Name, "alpine", kube.ExecOptions{
		Command: []string{"ls", "-fakeflag"},
		Stdout:  &stdout,
	})

	// NB: Using IsError results in a panic here because CodeExitError expects
	// an internally set error.
	exitErr, ok := err.(kube.CodeExitError)
	s.True(ok)
	s.Equal(1, exitErr.ExitStatus())
}

func (s *CtlTestSuite) TestEvictPod() {
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-eviction",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "nginx",
					Image: "nginx:1.14.2",
				},
			},
		},
	}
	s.Assert().NoError(s.ctl.Apply(ctx, pod))

	actualResourceVersion := pod.ResourceVersion
	actualUID := pod.UID

	randomResourceVersion := "random"
	randomUID := types.UID("random-uid")

	testCases := []struct {
		comment         string
		uid             types.UID
		resourceVersion string
		expectedReason  metav1.StatusReason
	}{
		{
			comment:         "evict wrong uid should cause 409 error",
			uid:             randomUID,
			resourceVersion: actualResourceVersion,
			expectedReason:  metav1.StatusReasonConflict,
		},
		{
			comment:         "evict wrong version should cause 409 error",
			uid:             actualUID,
			resourceVersion: randomResourceVersion,
			expectedReason:  metav1.StatusReasonConflict,
		},
		{
			comment:         "evict wrong version and uid should cause 409 error",
			uid:             randomUID,
			resourceVersion: randomResourceVersion,
			expectedReason:  metav1.StatusReasonConflict,
		},
		{
			comment:         "eviction should succeed if uid and resource version is correct",
			uid:             actualUID,
			resourceVersion: actualResourceVersion,
		},
		{
			comment:         "double eviction should cause 404 error",
			uid:             actualUID,
			resourceVersion: actualResourceVersion,
			expectedReason:  metav1.StatusReasonNotFound,
		},
	}
	for _, tc := range testCases {
		err := s.ctl.EvictPod(ctx, pod, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{
				UID:             &tc.uid,
				ResourceVersion: &tc.resourceVersion,
			},
		})
		if len(tc.expectedReason) == 0 {
			s.Assert().NoError(err, tc.comment)
			s.Assert().NoError(s.ctl.WaitForWithOptions(s.ctx, kube.WaitForOptions{
				DoneFunc: kube.IsDeleted,
			}, pod), tc.comment)
			continue
		}
		s.Assert().Error(err, tc.comment)
		s.Assert().Equal(tc.expectedReason, kubeerrors.ReasonForError(err), tc.comment)
	}
}

func (s *CtlTestSuite) TestCreate() {
	// Custom resource.
	const tenantID = crdbv1beta1.TenantID(10)
	tenant := &crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:        tenantID.Tag(),
			Annotations: map[string]string{tenantctrl.DisableReconcile: "true"},
		},
		Spec: crdbv1beta1.CrdbTenantSpec{TenantID: tenantID},
	}
	tenant2 := *tenant

	// Generated name.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-creation-generatename",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "nginx",
					Image: "nginx:1.14.2",
				},
			},
		},
	}

	// Ensure the objects are always cleaned up.
	s.T().Cleanup(func() {
		_ = s.ctl.Delete(s.ctx, tenant)
		_ = s.ctl.Delete(s.ctx, pod)
	})

	// Create custom resource with custom DoneFunc.
	done := false
	s.NoError(s.ctl.CreateWithOptions(s.ctx, kube.CreateOptions{
		WaitForOptions: kube.WaitForOptions{
			DoneFunc: func(object kube.Object) error {
				// Rather than use the built in IsReady function, always
				// consider this tenant "ready" as long as it's a tenant
				// object. We're just testing that CreateWithOptions gets
				// called and changes to the CrdbTenant could otherwise cause
				// this test to fail.
				done = true
				if _, ok := object.(*crdbv1beta1.CrdbTenant); ok {
					return nil
				}
				return errors.Newf("want %T. got %T", &crdbv1beta1.CrdbTenant{}, object)
			},
		},
	}, tenant))
	s.True(done, "custom DoneFunc was not called")
	getTenant := &crdbv1beta1.CrdbTenant{}
	s.NoError(s.ctl.Get(s.ctx, tenant.Name, getTenant))
	s.Equal(tenantID, getTenant.Spec.TenantID)
	s.Equal(s.ctl.Namespace(), getTenant.Namespace, "expected namespace to be auto-filled")
	s.Len(getTenant.Annotations, 2)
	s.Contains(getTenant.Annotations, "kubectl.kubernetes.io/last-applied-configuration")

	// Try to create it again and verify error.
	s.True(kubeerrors.IsAlreadyExists(s.ctl.Create(s.ctx, &tenant2)), "expected already exists error")

	// Create pod with generated name and no wait.
	s.NoError(s.ctl.CreateWithOptions(s.ctx, kube.CreateOptions{NoWait: true}, pod))
	s.True(len(pod.Name) > len(pod.GenerateName), "expected generated name")
	s.True(pod.Name[0:len(pod.GenerateName)] == pod.GenerateName)
}

func (s *CtlTestSuite) TestDelete() {
	replics := int32(1)
	sts := appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "statefulset",
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replics,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "nginx-sts",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "nginx-sts",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "nginx", Image: "nginx:1.14.2"},
					},
				},
			},
		},
	}

	s.NoError(s.ctl.Apply(s.ctx, &sts))
	s.NoError(s.ctl.DeleteWithOptions(s.ctx, kube.DeleteOptions{
		PropagationPolicy: metav1.DeletePropagationOrphan,
	}, &sts))

	var pod corev1.Pod
	s.NoError(s.ctl.Get(s.ctx, "statefulset-0", &pod))
	s.NoError(s.ctl.Delete(s.ctx, &pod))
}

func (s *CtlTestSuite) TestLogManifest() {
	// Apply two roles at once.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-secret",
		},
		StringData: map[string]string{
			"key": "value",
		},
	}

	config := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-config",
		},
		Data: map[string]string{
			"field": "value",
		},
	}

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "my-role",
			Labels: map[string]string{"app": "my-app"},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get"},
			},
		},
	}

	buf := &zaptest.Buffer{}
	logger := zap.New(zapcore.NewCore(
		zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig()),
		buf,
		zap.NewAtomicLevelAt(zapcore.DebugLevel),
	))
	ctx := logging.WithLogger(s.ctx, logger)
	buf.Reset()

	// Ensure the objects are always cleaned up.
	s.T().Cleanup(func() {
		_ = s.ctl.Delete(s.ctx, secret)
		_ = s.ctl.Delete(s.ctx, config)
		_ = s.ctl.Delete(s.ctx, role)
	})

	opts := kube.ApplyOptions{
		LogManifests: true,
	}

	s.NoError(s.ctl.ApplyWithOptions(ctx, opts, secret, role, config))
	var getSecret corev1.Secret
	s.NoError(s.ctl.Get(s.ctx, "my-secret", &getSecret))

	var getRole rbacv1.Role
	s.NoError(s.ctl.Get(s.ctx, "my-role", &getRole))

	var getConfigmap corev1.ConfigMap
	s.NoError(s.ctl.Get(s.ctx, "my-config", &getConfigmap))

	lines := buf.Lines()
	s.Len(lines, 4)
	// Note that the "Object created" only occurs on the non-secret and
	// non-configmap object
	s.Contains(lines[0], "Secret/my-secret created")
	s.Contains(lines[1], "Role/my-role created")
	s.Contains(lines[2], "Object created: {")
	s.Contains(lines[3], "ConfigMap/my-config created")
}

func (s *CtlTestSuite) TestGetContainerLogs() {
	_, err := s.ctl.WithNamespace("").GetContainerLogs(s.ctx, "", "", kube.GetContainerLogsOptions{})
	s.EqualError(err, ".Namespace must have been called with a non-empty value to use logs")

	// kube recordings don't work with raw REST calls to K8s API as they use
	// direct HTTP streams.
	testutils.SkipIfKuberecord(s.T())

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "logs-pod",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "alpine",
					Image:   "alpine:latest",
					Command: []string{"sh", "-c", "echo 'Hello from logs test' && sleep 1000"},
				},
			},
			// Set TerminationGracePeriodSeconds to a low value to speed up the
			// test runtime.
			TerminationGracePeriodSeconds: new(int64(1)),
		},
	}

	s.Assert().NoError(s.ctl.Apply(s.ctx, &pod))

	s.T().Cleanup(func() {
		_ = s.ctl.Delete(s.ctx, &pod)
	})

	// Test successful log retrieval
	logs, err := s.ctl.GetContainerLogs(s.ctx, pod.Name, "alpine", kube.GetContainerLogsOptions{})
	s.Assert().NoError(err)
	s.Assert().Contains(logs, "Hello from logs test")

	// Test error case: non-existent pod
	_, err = s.ctl.GetContainerLogs(s.ctx, "non-existent-pod", "alpine", kube.GetContainerLogsOptions{})
	s.Assert().Error(err)
	s.Assert().Contains(err.Error(), "not found")

	// Test error case: non-existent container
	_, err = s.ctl.GetContainerLogs(s.ctx, pod.Name, "non-existent-container", kube.GetContainerLogsOptions{})
	s.Assert().Error(err)
	s.Assert().Contains(err.Error(), "container non-existent-container is not valid for pod logs-pod")
}

package migrationctrl

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// TestRestoreServiceSelectors verifies that rollback replaces the selector
// wholesale with the expected keys for each migration type.
func TestRestoreServiceSelectors(t *testing.T) {
	tests := []struct {
		name             string
		migrationType    string
		expectedSelector map[string]string
	}{
		{
			name:          "helm migration",
			migrationType: MigrationTypeHelm,
			expectedSelector: map[string]string{
				"app.kubernetes.io/instance": testClusterName,
				"app.kubernetes.io/name":     "cockroachdb",
			},
		},
		{
			name:          "operator migration",
			migrationType: MigrationTypeOperator,
			expectedSelector: map[string]string{
				"app.kubernetes.io/component": "database",
				"app.kubernetes.io/instance":  testClusterName,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctl := kubetest.NewFakeCtl(kubetest.Options{
				Options: kube.Options{Scheme: kube.Scheme},
			}).WithNamespace(testNamespace)

			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{
						manifests.BetaCrdbClusterNodeOwnerLabelKey: testClusterName,
						"some-stale-key": "leftover",
					},
				},
			}
			require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, svc))

			require.NoError(t, restoreServiceSelectors(ctx, ctl, testClusterName, tt.migrationType))

			var updated corev1.Service
			require.NoError(t, ctl.Get(ctx, testClusterName, &updated))
			assert.Equal(t, tt.expectedSelector, updated.Spec.Selector,
				"selector should be replaced wholesale, no stale keys")
		})
	}
}

// TestRestoreServiceSelectors_EdgeCases covers no-op paths for selector restoration.
func TestRestoreServiceSelectors_EdgeCases(t *testing.T) {
	tests := []struct {
		name string
		svc  *corev1.Service
	}{
		{
			name: "service not found",
			svc:  nil,
		},
		{
			name: "selectors already correct",
			svc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{
						"app.kubernetes.io/instance": testClusterName,
						"app.kubernetes.io/name":     "cockroachdb",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctl := kubetest.NewFakeCtl(kubetest.Options{
				Options: kube.Options{Scheme: kube.Scheme},
			}).WithNamespace(testNamespace)

			if tt.svc != nil {
				require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, tt.svc))
			}

			err := restoreServiceSelectors(ctx, ctl, testClusterName, MigrationTypeHelm)
			require.NoError(t, err)
		})
	}
}

// TestServicePortAnnotation verifies that updatePublicServicePorts saves the
// original port configuration and that subsequent calls don't overwrite it.
func TestServicePortAnnotation(t *testing.T) {
	originalPorts := []corev1.ServicePort{
		{Name: "grpc", Protocol: corev1.ProtocolTCP, Port: 26257, TargetPort: intstr.FromInt(26257)},
		{Name: "http", Protocol: corev1.ProtocolTCP, Port: 8080, TargetPort: intstr.FromInt(8080)},
	}

	tests := []struct {
		name      string
		callCount int
		grpcPort  int32
		sqlPort   int32
		wantSaved []corev1.ServicePort
	}{
		{
			name:      "first call saves original ports",
			callCount: 1,
			grpcPort:  26257,
			sqlPort:   26257,
			wantSaved: originalPorts,
		},
		{
			name:      "idempotent across reconciliations",
			callCount: 3,
			grpcPort:  26300,
			sqlPort:   26299,
			wantSaved: originalPorts,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctl := kubetest.NewFakeCtl(kubetest.Options{
				Options: kube.Options{Scheme: kube.Scheme},
			}).WithNamespace(testNamespace)

			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: testClusterName + "-public", Namespace: testNamespace},
				Spec:       corev1.ServiceSpec{Ports: originalPorts},
			}
			require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, svc))

			for range tt.callCount {
				require.NoError(t, updatePublicServicePorts(ctx, ctl, testClusterName, tt.grpcPort, tt.sqlPort))
			}

			var updated corev1.Service
			require.NoError(t, ctl.Get(ctx, testClusterName+"-public", &updated))

			raw := updated.Annotations[annotationOriginalPorts]
			require.NotEmpty(t, raw, "annotation should be set")

			var saved []corev1.ServicePort
			require.NoError(t, json.Unmarshal([]byte(raw), &saved))
			assert.Equal(t, tt.wantSaved, saved, "annotation must always reflect pre-migration ports")
		})
	}
}

// TestRestoreServicePorts verifies the full round-trip: forward migration saves
// the annotation, rollback restores the original ports and removes it.
func TestRestoreServicePorts(t *testing.T) {
	originalPorts := []corev1.ServicePort{
		{Name: "grpc", Protocol: corev1.ProtocolTCP, Port: 26257, TargetPort: intstr.FromInt(26257)},
		{Name: "http", Protocol: corev1.ProtocolTCP, Port: 8080, TargetPort: intstr.FromInt(8080)},
	}

	tests := []struct {
		name          string
		setupSvc      *corev1.Service
		runForward    bool
		wantPorts     []corev1.ServicePort
		wantAnnotGone bool
		wantErr       bool
	}{
		{
			name: "restores original ports after migration",
			setupSvc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: testClusterName + "-public", Namespace: testNamespace},
				Spec:       corev1.ServiceSpec{Ports: originalPorts},
			},
			runForward:    true,
			wantPorts:     originalPorts,
			wantAnnotGone: true,
		},
		{
			name: "no annotation is a silent no-op",
			setupSvc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: testClusterName + "-public", Namespace: testNamespace},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{{Name: "grpc", Port: 26257}},
				},
			},
			runForward: false,
		},
		{
			name:     "service not found is a silent no-op",
			setupSvc: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctl := kubetest.NewFakeCtl(kubetest.Options{
				Options: kube.Options{Scheme: kube.Scheme},
			}).WithNamespace(testNamespace)

			if tt.setupSvc != nil {
				require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, tt.setupSvc))
			}

			if tt.runForward {
				require.NoError(t, updatePublicServicePorts(ctx, ctl, testClusterName, 26257, 26257))
			}

			err := restoreServicePorts(ctx, ctl, testClusterName)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			if tt.wantPorts != nil {
				var restored corev1.Service
				require.NoError(t, ctl.Get(ctx, testClusterName+"-public", &restored))
				assert.Equal(t, tt.wantPorts, restored.Spec.Ports)
			}
			if tt.wantAnnotGone && tt.setupSvc != nil {
				var restored corev1.Service
				require.NoError(t, ctl.Get(ctx, testClusterName+"-public", &restored))
				_, exists := restored.Annotations[annotationOriginalPorts]
				assert.False(t, exists, "annotation should be removed after restore")
			}
		})
	}
}

// TestRemoveOriginalPortsAnnotation verifies post-migration cleanup removes the
// annotation without touching the port configuration.
func TestRemoveOriginalPortsAnnotation(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	ports := []corev1.ServicePort{
		{Name: "grpc", Port: 26257, TargetPort: intstr.FromString("grpc")},
		{Name: "sql", Port: 26257, TargetPort: intstr.FromString("sql")},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testClusterName + "-public",
			Namespace:   testNamespace,
			Annotations: map[string]string{annotationOriginalPorts: `[{"name":"grpc","port":26257}]`},
		},
		Spec: corev1.ServiceSpec{Ports: ports},
	}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, svc))

	removeOriginalPortsAnnotation(ctx, ctl, testClusterName)

	var updated corev1.Service
	require.NoError(t, ctl.Get(ctx, testClusterName+"-public", &updated))
	_, exists := updated.Annotations[annotationOriginalPorts]
	assert.False(t, exists, "annotation should be removed")
	assert.Equal(t, ports, updated.Spec.Ports, "ports should be untouched by cleanup")
}

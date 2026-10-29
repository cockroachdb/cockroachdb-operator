package migrationctrl

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	crdbv1alpha1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1alpha1"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubetest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/migrationutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestDetectPCRFromInitJob verifies that the --virtualized flag on the init job
// is correctly detected and mapped to the right VirtualClusterMode.
func TestDetectPCRFromInitJob(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name        string
		job         *batchv1.Job
		expectPCR   bool
		expectEmpty bool
	}{
		{
			name: "Job with --virtualized flag",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-init",
					Namespace: testNamespace,
				},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name: "cluster-init",
									Args: []string{
										"--virtualized",
										"--join=test-cluster-0.test-cluster",
									},
								},
							},
						},
					},
				},
			},
			expectPCR:   true,
			expectEmpty: false,
		},
		{
			name: "Job with --virtualized-empty flag",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-init",
					Namespace: testNamespace,
				},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name: "cluster-init",
									Args: []string{
										"--virtualized-empty",
										"--join=test-cluster-0.test-cluster",
									},
								},
							},
						},
					},
				},
			},
			expectPCR:   true,
			expectEmpty: true,
		},
		{
			name: "Job without virtualization",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-init",
					Namespace: testNamespace,
				},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name: "cluster-init",
									Args: []string{
										"--join=test-cluster-0.test-cluster",
									},
								},
							},
						},
					},
				},
			},
			expectPCR:   false,
			expectEmpty: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctl := kubetest.NewFakeCtl(kubetest.Options{
				Options: kube.Options{
					Scheme: kube.Scheme,
				},
			}).WithNamespace(testNamespace)

			require.NoError(t, ctl.Create(ctx, tt.job))

			result, err := detectPCRFromInitJob(ctx, ctl, testClusterName)
			require.NoError(t, err)

			if tt.expectPCR {
				require.NotNil(t, result)
				if tt.expectEmpty {
					assert.Equal(t, crdbv1beta1.VirtualClusterModeStandby, result.Mode)
				} else {
					assert.Equal(t, crdbv1beta1.VirtualClusterModePrimary, result.Mode)
				}
			} else {
				assert.Nil(t, result)
			}
		})
	}
}

// TestDetectIngressFromHelm verifies that an existing Ingress resource is extracted
// into the annotation format the CrdbCluster spec expects.
func TestDetectIngressFromHelm(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name          string
		ingress       *networkingv1.Ingress
		expectIngress bool
	}{
		{
			name: "Ingress with full config",
			ingress: &networkingv1.Ingress{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-ingress",
					Namespace: testNamespace,
					Annotations: map[string]string{
						"nginx.ingress.kubernetes.io/ssl-redirect": "true",
					},
				},
				Spec: networkingv1.IngressSpec{
					IngressClassName: new("nginx"),
					Rules: []networkingv1.IngressRule{
						{
							Host: "test-cluster.example.com",
							IngressRuleValue: networkingv1.IngressRuleValue{
								HTTP: &networkingv1.HTTPIngressRuleValue{
									Paths: []networkingv1.HTTPIngressPath{
										{
											Path:     "/",
											PathType: new(networkingv1.PathTypePrefix),
										},
									},
								},
							},
						},
					},
					TLS: []networkingv1.IngressTLS{
						{
							Hosts:      []string{"test-cluster.example.com"},
							SecretName: "test-cluster-tls",
						},
					},
				},
			},
			expectIngress: true,
		},
		{
			name:          "No ingress",
			ingress:       nil,
			expectIngress: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctl := kubetest.NewFakeCtl(kubetest.Options{
				Options: kube.Options{
					Scheme: kube.Scheme,
				},
			}).WithNamespace(testNamespace)

			if tt.ingress != nil {
				require.NoError(t, ctl.Create(ctx, tt.ingress))
			}

			result, err := detectIngressFromHelm(ctx, ctl, testClusterName)
			require.NoError(t, err)

			if tt.expectIngress {
				require.NotNil(t, result)
				configMap, ok := result.(map[string]any)
				require.True(t, ok)
				ui, ok := configMap["ui"].(map[string]any)
				require.True(t, ok)
				className, ok := ui["ingressClassName"].(*string)
				require.True(t, ok, "ingressClassName should be a *string")
				assert.Equal(t, "nginx", *className)
				assert.Equal(t, "test-cluster.example.com", ui["host"])
			} else {
				assert.Nil(t, result)
			}
		})
	}
}

// TestConvertLogConfigSecretToConfigMap verifies the Secret-to-ConfigMap conversion
// renames the Helm key "log-config.yaml" to the operator-expected "logs.yaml".
func TestConvertLogConfigSecretToConfigMap(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name        string
		secret      *corev1.Secret
		expectError bool
	}{
		{
			name: "Valid log config secret",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-log-config",
					Namespace: testNamespace,
				},
				Data: map[string][]byte{
					"log-config.yaml": []byte("sinks:\n  file-groups:\n    default:\n      channels: all\n"),
				},
			},
			expectError: false,
		},
		{
			name:        "No secret exists",
			secret:      nil,
			expectError: false, // Should not error, just skip
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctl := kubetest.NewFakeCtl(kubetest.Options{
				Options: kube.Options{
					Scheme: kube.Scheme,
				},
			}).WithNamespace(testNamespace)

			if tt.secret != nil {
				require.NoError(t, ctl.Create(ctx, tt.secret))
			}

			err := convertLogConfigSecretToConfigMap(ctx, ctl, "test-cluster-log-config", testNamespace)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)

				if tt.secret != nil {
					// Verify ConfigMap was created
					cm := &corev1.ConfigMap{}
					err := ctl.Get(ctx, "test-cluster-log-config", cm)
					require.NoError(t, err)
					assert.Contains(t, cm.Data, "logs.yaml")
				}
			}
		})
	}
}

// TestBuildNodeSpec_EmptyVCT verifies that buildNodeSpec does not panic when the
// StatefulSet has no VolumeClaimTemplates.
func TestBuildNodeSpec_EmptyVCT(t *testing.T) {
	sts := createTestStatefulSet(testClusterName, testNamespace, 3)
	sts.Spec.VolumeClaimTemplates = nil // Empty VCT

	ctx := &migrationutil.ParsedMigrationContext{
		Image:      "cockroachdb/cockroach:v23.1.0",
		TLSEnabled: true,
		GRPCPort:   26257,
		SQLPort:    26257,
		HTTPPort:   8080,
	}

	spec := buildNodeSpec(sts, "", ctx, nil)
	// DataStore should be zero value (no VCT)
	assert.Nil(t, spec.DataStore.VolumeClaimTemplate)
}

// TestBuildNodeSpec_LogsStoreVCT verifies that buildNodeSpec populates LogsStore
// when a logsdir VolumeClaimTemplate is present.
func TestBuildNodeSpec_LogsStoreVCT(t *testing.T) {
	sts := createTestStatefulSetWithPVC(testClusterName, testNamespace, 3)
	sts.Spec.VolumeClaimTemplates = append(sts.Spec.VolumeClaimTemplates, corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "logsdir"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	})

	ctx := &migrationutil.ParsedMigrationContext{
		Image:      "cockroachdb/cockroach:v23.1.0",
		TLSEnabled: true,
		GRPCPort:   26257,
		SQLPort:    26257,
		HTTPPort:   8080,
	}

	spec := buildNodeSpec(sts, "", ctx, nil)
	require.NotNil(t, spec.LogsStore, "LogsStore should be populated for logsdir VCT")
	require.NotNil(t, spec.LogsStore.VolumeClaimTemplate)
	assert.Equal(t, "logsdir", spec.LogsStore.VolumeClaimTemplate.Name)
}

func TestNormalizeRuntimeTLSConfig_RewritesLegacyOperatorSecrets(t *testing.T) {
	ctx := &migrationutil.ParsedMigrationContext{
		NodeSecretName:   "test-cluster-node",
		ClientSecretName: "test-cluster-root-client",
		HTTPSecretName:   "test-cluster-root-client",
	}

	migrationutil.NormalizeRuntimeTLSConfig(ctx, testClusterName)

	assert.Equal(t, "test-cluster-node-secret", ctx.NodeSecretName)
	assert.Equal(t, "test-cluster-client-secret", ctx.ClientSecretName)
	assert.Equal(t, "test-cluster-client-secret", ctx.HTTPSecretName)
	assert.Equal(t, "test-cluster-ca-crt", ctx.CAConfigMap)
}

func TestNormalizeRuntimeTLSConfig_PreservesCustomCertManagerSecrets(t *testing.T) {
	ctx := &migrationutil.ParsedMigrationContext{
		NodeSecretName:   "custom-node-secret",
		ClientSecretName: "custom-client-secret",
		HTTPSecretName:   "custom-http-secret",
		CAConfigMap:      "existing-ca-config",
	}

	migrationutil.NormalizeRuntimeTLSConfig(ctx, testClusterName)

	assert.Equal(t, "custom-node-secret", ctx.NodeSecretName)
	assert.Equal(t, "custom-client-secret", ctx.ClientSecretName)
	// HTTPSecretName always tracks ClientSecretName. ParseStatefulSet initializes
	// both to the same value, and parseVolumesAndSecrets only updates
	// SourceClientSecretName, so there is no real path where HTTPSecretName
	// diverges from ClientSecretName.
	assert.Equal(t, "custom-client-secret", ctx.HTTPSecretName)
	assert.Equal(t, "existing-ca-config", ctx.CAConfigMap)
}

func TestCertManagerSecretName_DefaultsToCertificateName(t *testing.T) {
	assert.Equal(t, "test-cluster-node", certManagerSecretName(map[string]any{}, "test-cluster-node"))
	assert.Equal(t, "custom-node", certManagerSecretName(map[string]any{"secretName": "custom-node"}, "test-cluster-node"))
}

// TestExtractContextFromSTS_DefaultsSQLPort verifies that SQLPort is defaulted to 26257 when
// neither --port nor --sql-addr appears in the STS args. Without the default, buildNodeSpec
// writes &int32(0) into CrdbNodeSpec.SQLPort, causing "cockroach sql --port=0" in health checks.
func TestExtractContextFromSTS_DefaultsSQLPort(t *testing.T) {
	// createTestStatefulSet has --join and --certs-dir only — no --port or --sql-addr.
	sts := createTestStatefulSet("mycluster", testNamespace, 3)

	mCtx, err := extractContextFromSTS(sts)
	require.NoError(t, err)
	assert.Equal(t, int32(26257), mCtx.SQLPort)
}

// TestUpdatePublicServicePorts_UsesResolvedPorts verifies that the ports from the parsed
// StatefulSet are propagated into the service, not the old hard-coded 26258/26257 defaults.
func TestUpdatePublicServicePorts_UsesResolvedPorts(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "mycluster-public", Namespace: testNamespace},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: "grpc", Protocol: corev1.ProtocolTCP, Port: 26258},
			},
		},
	}
	require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, svc))

	// Non-default ports — a cluster running GRPC on 26300, SQL on 26299.
	require.NoError(t, updatePublicServicePorts(ctx, ctl, "mycluster", 26300, 26299))

	updated := &corev1.Service{}
	require.NoError(t, ctl.Get(ctx, "mycluster-public", updated))

	portsByName := make(map[string]corev1.ServicePort)
	for _, p := range updated.Spec.Ports {
		portsByName[p.Name] = p
	}
	assert.Equal(t, int32(26300), portsByName["grpc"].Port, "GRPC port must reflect resolved value")
	assert.Equal(t, int32(26299), portsByName["sql"].Port, "SQL port must reflect resolved value")
}

// TestBuildNodeSpec_OperatorMigrationChownInitContainer verifies that operator
// migrations (v1alpha1Cluster != nil) add a chown init container to fix data
// directory ownership. The public operator runs as root (uid 0) so all data
// files are root-owned. The enterprise operator switches to uid 1000, and
// without the chown step the cockroach process gets permission denied.
func TestBuildNodeSpec_OperatorMigrationChownInitContainer(t *testing.T) {
	sts := createTestStatefulSetWithPVC(testClusterName, testNamespace, 3)

	pc := &migrationutil.ParsedMigrationContext{
		Image:      "cockroachdb/cockroach:v23.1.0",
		TLSEnabled: true,
		GRPCPort:   26258,
		SQLPort:    26257,
		HTTPPort:   8080,
	}

	v1alpha1Cluster := &crdbv1alpha1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
	}

	spec := buildNodeSpec(sts, "", pc, v1alpha1Cluster)

	require.Len(t, spec.PodTemplate.Spec.InitContainers, 1, "operator migration should add chown init container")
	initC := spec.PodTemplate.Spec.InitContainers[0]
	assert.Equal(t, "chown-data-dir", initC.Name)
	assert.Equal(t, pc.Image, initC.Image)
	assert.Equal(t, []string{"chown", "-R", "1000:1000", manifests.CockroachDataDirMountAbsPath}, initC.Command)
	require.NotNil(t, initC.SecurityContext)
	require.NotNil(t, initC.SecurityContext.RunAsUser)
	assert.Equal(t, int64(0), *initC.SecurityContext.RunAsUser, "init container must run as root to chown")

	require.Len(t, initC.VolumeMounts, 1)
	assert.Equal(t, manifests.CockroachDataDirVolumeName, initC.VolumeMounts[0].Name)
	assert.Equal(t, manifests.CockroachDataDirMountAbsPath, initC.VolumeMounts[0].MountPath)
}

// TestBuildNodeSpec_HelmMigrationNoChownInitContainer verifies that Helm
// migrations (v1alpha1Cluster == nil) do not add the chown init container.
// Helm deployments preserve their own SecurityContext and init containers.
func TestBuildNodeSpec_HelmMigrationNoChownInitContainer(t *testing.T) {
	sts := createTestStatefulSetWithPVC(testClusterName, testNamespace, 3)

	pc := &migrationutil.ParsedMigrationContext{
		Image:      "cockroachdb/cockroach:v23.1.0",
		TLSEnabled: true,
		GRPCPort:   26258,
		SQLPort:    26257,
		HTTPPort:   8080,
	}

	spec := buildNodeSpec(sts, "", pc, nil)

	// No init containers should be added for Helm migrations when pc has none.
	assert.Empty(t, spec.PodTemplate.Spec.InitContainers,
		"Helm migration without custom init containers should not add chown")
}

// TestMoveConfigMapKey_RenamesKey verifies that moveConfigMapKey renames
// "logging.yaml" to "logs.yaml" and removes the old key.
func TestMoveConfigMapKey_RenamesKey(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "log-config", Namespace: testNamespace},
		Data:       map[string]string{"logging.yaml": "sinks:\n  stderr:\n    channels: all\n"},
	}
	require.NoError(t, ctl.Create(ctx, cm))

	require.NoError(t, moveConfigMapKey(ctx, ctl, "log-config"))

	var updated corev1.ConfigMap
	require.NoError(t, ctl.Get(ctx, "log-config", &updated))
	assert.Contains(t, updated.Data, "logs.yaml")
	assert.NotContains(t, updated.Data, "logging.yaml")
}

// TestMoveConfigMapKey_AlreadyMigrated is a no-op when the key is already "logs.yaml".
func TestMoveConfigMapKey_AlreadyMigrated(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "log-config", Namespace: testNamespace},
		Data:       map[string]string{"logs.yaml": "sinks:\n  stderr:\n    channels: all\n"},
	}
	require.NoError(t, ctl.Create(ctx, cm))

	require.NoError(t, moveConfigMapKey(ctx, ctl, "log-config"))

	var updated corev1.ConfigMap
	require.NoError(t, ctl.Get(ctx, "log-config", &updated))
	assert.Contains(t, updated.Data, "logs.yaml")
}

// TestMoveConfigMapKey_NotFound succeeds when the ConfigMap doesn't exist.
func TestMoveConfigMapKey_NotFound(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	err := moveConfigMapKey(ctx, ctl, "nonexistent-cm")
	require.NoError(t, err)
}

// TestReverseConfigMapKeyMove_RevertsKey verifies that reverseConfigMapKeyMove
// renames "logs.yaml" back to "logging.yaml" during rollback.
func TestReverseConfigMapKeyMove_RevertsKey(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "my-log-config", Namespace: testNamespace},
		Data:       map[string]string{"logs.yaml": "sinks:\n  stderr:\n    channels: all\n"},
	}
	require.NoError(t, ctl.Create(ctx, cm))

	require.NoError(t, reverseConfigMapKeyMove(ctx, ctl, testClusterName, "my-log-config"))

	var updated corev1.ConfigMap
	require.NoError(t, ctl.Get(ctx, "my-log-config", &updated))
	assert.Contains(t, updated.Data, "logging.yaml")
	assert.NotContains(t, updated.Data, "logs.yaml")
}

// TestReverseConfigMapKeyMove_FallsBackToCommonNames verifies the fallback
// naming patterns when logConfigMapName is empty.
func TestReverseConfigMapKeyMove_FallsBackToCommonNames(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	// Create ConfigMap using the fallback name pattern "<cluster>-log-config".
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName + "-log-config", Namespace: testNamespace},
		Data:       map[string]string{"logs.yaml": "config data"},
	}
	require.NoError(t, ctl.Create(ctx, cm))

	require.NoError(t, reverseConfigMapKeyMove(ctx, ctl, testClusterName, ""))

	var updated corev1.ConfigMap
	require.NoError(t, ctl.Get(ctx, testClusterName+"-log-config", &updated))
	assert.Contains(t, updated.Data, "logging.yaml")
}

// TestReverseConfigMapKeyMove_SkipsWhenBothKeysExist verifies that no rename
// happens when both "logs.yaml" and "logging.yaml" are present.
func TestReverseConfigMapKeyMove_SkipsWhenBothKeysExist(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme},
	}).WithNamespace(testNamespace)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
		Data: map[string]string{
			"logs.yaml":    "new config",
			"logging.yaml": "old config",
		},
	}
	require.NoError(t, ctl.Create(ctx, cm))

	require.NoError(t, reverseConfigMapKeyMove(ctx, ctl, testClusterName, ""))

	var updated corev1.ConfigMap
	require.NoError(t, ctl.Get(ctx, testClusterName, &updated))
	assert.Equal(t, "old config", updated.Data["logging.yaml"], "original key should be untouched")
	assert.Equal(t, "new config", updated.Data["logs.yaml"], "new key should not be deleted")
}

// TestLabelExistingPods_LabelsAllPods verifies that labelExistingPods applies the
// cluster owner label and svc label to each pod in the range.
func TestLabelExistingPods_LabelsAllPods(t *testing.T) {
	ctx := context.Background()
	// MakeReady is required because labelExistingPods calls ctl.Update on pods,
	// which waits for readiness by default. This matches the pattern used in
	// betaclusterctrl and tenantctrl tests for pod manipulation.
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme, BeforeMutation: kubetest.MakeReady},
	}).WithNamespace(testNamespace)

	for i := range int32(3) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: testClusterName + "-" + fmt.Sprintf("%d", i), Namespace: testNamespace,
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "db", Image: "cockroachdb/cockroach:v23.1.0"}}},
		}
		require.NoError(t, ctl.Create(ctx, pod))
	}

	require.NoError(t, labelExistingPods(ctx, ctl, testClusterName, 3))

	for i := range int32(3) {
		var pod corev1.Pod
		require.NoError(t, ctl.Get(ctx, testClusterName+"-"+fmt.Sprintf("%d", i), &pod))
		assert.Equal(t, testClusterName, pod.Labels[manifests.BetaCrdbClusterNodeOwnerLabelKey])
		assert.Equal(t, "cockroachdb", pod.Labels["svc"])
	}
}

// TestLabelExistingPods_SkipsMissingPods verifies that missing pods are skipped
// without returning an error.
func TestLabelExistingPods_SkipsMissingPods(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme, BeforeMutation: kubetest.MakeReady},
	}).WithNamespace(testNamespace)

	// Only create pod-0, skip pod-1 and pod-2.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName + "-0", Namespace: testNamespace},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "db", Image: "cockroachdb/cockroach:v23.1.0"}}},
	}
	require.NoError(t, ctl.Create(ctx, pod))

	err := labelExistingPods(ctx, ctl, testClusterName, 3)
	require.NoError(t, err, "missing pods should be skipped")
}

// TestLabelExistingPods_IdempotentWhenAlreadyLabeled verifies that re-labeling
// pods that already have the correct labels is a no-op.
func TestLabelExistingPods_IdempotentWhenAlreadyLabeled(t *testing.T) {
	ctx := context.Background()
	ctl := kubetest.NewFakeCtl(kubetest.Options{
		Options: kube.Options{Scheme: kube.Scheme, BeforeMutation: kubetest.MakeReady},
	}).WithNamespace(testNamespace)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: testClusterName + "-0", Namespace: testNamespace,
			Labels: map[string]string{
				manifests.BetaCrdbClusterNodeOwnerLabelKey: testClusterName,
				"svc": "cockroachdb",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "db", Image: "cockroachdb/cockroach:v23.1.0"}}},
	}
	require.NoError(t, ctl.Create(ctx, pod))

	err := labelExistingPods(ctx, ctl, testClusterName, 1)
	require.NoError(t, err)
}

// TestGenerateV1Beta1ClusterSpec_MultiRegionAnnotation verifies that the
// multi-region annotation on the STS is parsed into CrdbClusterRegion entries.
func TestGenerateV1Beta1ClusterSpec_MultiRegionAnnotation(t *testing.T) {
	ctx := context.Background()
	sts := createTestStatefulSet(testClusterName, testNamespace, 3)

	regions := []crdbv1beta1.CrdbClusterRegion{
		{Code: "us-east1", Nodes: 3, Namespace: testNamespace},
		{Code: "us-west1", Nodes: 3, Namespace: "west-ns"},
	}
	regionsJSON, err := json.Marshal(regions)
	require.NoError(t, err)
	sts.Annotations = map[string]string{AnnotationMigrationRegions: string(regionsJSON)}

	stsCtx, err := extractContextFromSTS(sts)
	require.NoError(t, err)

	spec := generateV1Beta1ClusterSpec(ctx, sts, stsCtx, 3, "us-east1")

	require.Len(t, spec.Regions, 2)
	assert.Equal(t, "us-east1", spec.Regions[0].Code)
	assert.Equal(t, "us-west1", spec.Regions[1].Code)
}

// TestGenerateV1Beta1ClusterSpec_SingleRegionFallback verifies that when no
// multi-region annotation is present, a single region is created from the
// operator's cloud region.
func TestGenerateV1Beta1ClusterSpec_SingleRegionFallback(t *testing.T) {
	ctx := context.Background()
	sts := createTestStatefulSet(testClusterName, testNamespace, 3)

	stsCtx, err := extractContextFromSTS(sts)
	require.NoError(t, err)

	spec := generateV1Beta1ClusterSpec(ctx, sts, stsCtx, 3, "us-central1")

	require.Len(t, spec.Regions, 1)
	assert.Equal(t, "us-central1", spec.Regions[0].Code)
	assert.Equal(t, int32(3), spec.Regions[0].Nodes)
	assert.Equal(t, testNamespace, spec.Regions[0].Namespace)
}

// TestGenerateV1Beta1ClusterSpec_InvalidRegionAnnotationFallback verifies that
// malformed JSON in the regions annotation falls back to a single region.
func TestGenerateV1Beta1ClusterSpec_InvalidRegionAnnotationFallback(t *testing.T) {
	ctx := context.Background()
	sts := createTestStatefulSet(testClusterName, testNamespace, 3)
	sts.Annotations = map[string]string{AnnotationMigrationRegions: "not-valid-json"}

	stsCtx, err := extractContextFromSTS(sts)
	require.NoError(t, err)

	spec := generateV1Beta1ClusterSpec(ctx, sts, stsCtx, 3, "us-east1")

	require.Len(t, spec.Regions, 1, "invalid annotation should fall back to single region")
	assert.Equal(t, "us-east1", spec.Regions[0].Code)
}

func TestSyncRegionAnnotations(t *testing.T) {
	ctx := context.Background()

	t.Run("updates stale region from annotation", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{
			Options: kube.Options{Scheme: kube.Scheme},
		}).WithNamespace(testNamespace)

		cluster := &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testClusterName,
				Namespace: testNamespace,
				Annotations: map[string]string{
					crdbv1alpha1.BetaCrdbClusterNodeRegionCodeAnnotationKey:    "ca-central-1",
					crdbv1alpha1.BetaCrdbClusterNodeCloudProviderAnnotationKey: "aws",
				},
			},
			Spec: crdbv1beta1.CrdbClusterSpec{
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{Code: "us-east1", CloudProvider: "gcp", Nodes: 3, Namespace: testNamespace},
				},
			},
		}
		require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

		updated, err := syncRegionAnnotations(ctx, ctl, cluster)
		require.NoError(t, err)
		assert.True(t, updated)
		assert.Equal(t, "ca-central-1", cluster.Spec.Regions[0].Code)
		assert.Equal(t, "aws", cluster.Spec.Regions[0].CloudProvider)
	})

	t.Run("no-op when annotations match spec", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{
			Options: kube.Options{Scheme: kube.Scheme},
		}).WithNamespace(testNamespace)

		cluster := &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testClusterName,
				Namespace: testNamespace,
				Annotations: map[string]string{
					crdbv1alpha1.BetaCrdbClusterNodeRegionCodeAnnotationKey:    "us-east1",
					crdbv1alpha1.BetaCrdbClusterNodeCloudProviderAnnotationKey: "gcp",
				},
			},
			Spec: crdbv1beta1.CrdbClusterSpec{
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{Code: "us-east1", CloudProvider: "gcp", Nodes: 3, Namespace: testNamespace},
				},
			},
		}
		require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

		updated, err := syncRegionAnnotations(ctx, ctl, cluster)
		require.NoError(t, err)
		assert.False(t, updated)
	})

	t.Run("no annotations means no update", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{
			Options: kube.Options{Scheme: kube.Scheme},
		}).WithNamespace(testNamespace)

		cluster := &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testClusterName,
				Namespace: testNamespace,
			},
			Spec: crdbv1beta1.CrdbClusterSpec{
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{Code: "us-east1", CloudProvider: "gcp"},
				},
			},
		}
		require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

		updated, err := syncRegionAnnotations(ctx, ctl, cluster)
		require.NoError(t, err)
		assert.False(t, updated)
	})

	t.Run("regions JSON annotation replaces spec", func(t *testing.T) {
		ctl := kubetest.NewFakeCtl(kubetest.Options{
			Options: kube.Options{Scheme: kube.Scheme},
		}).WithNamespace(testNamespace)

		regionsJSON, err := json.Marshal([]crdbv1beta1.CrdbClusterRegion{
			{Code: "eu-west-1", CloudProvider: "aws", Nodes: 3, Namespace: "eu-ns"},
			{Code: "eu-west-2", CloudProvider: "aws", Nodes: 3, Namespace: "eu-ns-2"},
		})
		require.NoError(t, err)

		cluster := &crdbv1beta1.CrdbCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testClusterName,
				Namespace: testNamespace,
				Annotations: map[string]string{
					crdbv1alpha1.BetaCrdbClusterRegionsAnnotationKey: string(regionsJSON),
				},
			},
			Spec: crdbv1beta1.CrdbClusterSpec{
				Regions: []crdbv1beta1.CrdbClusterRegion{
					{Code: "us-east1", CloudProvider: "gcp", Nodes: 3},
				},
			},
		}
		require.NoError(t, ctl.CreateWithOptions(ctx, kube.CreateOptions{NoWait: true}, cluster))

		updated, err := syncRegionAnnotations(ctx, ctl, cluster)
		require.NoError(t, err)
		assert.True(t, updated)
		require.Len(t, cluster.Spec.Regions, 2)
		assert.Equal(t, "eu-west-1", cluster.Spec.Regions[0].Code)
		assert.Equal(t, "eu-west-2", cluster.Spec.Regions[1].Code)
	})
}

// TestMigrationRegionsAnnotationKeyConsistency guards against the hardcoded
// string in api/v1alpha1 drifting from the constant in this package.
func TestMigrationRegionsAnnotationKeyConsistency(t *testing.T) {
	keys := crdbv1alpha1.MigrationAnnotationKeys()
	found := slices.Contains(keys, AnnotationMigrationRegions)
	require.True(t, found,
		"MigrationAnnotationKeys() must contain AnnotationMigrationRegions (%q) — "+
			"the hardcoded string in api/v1alpha1 may have drifted", AnnotationMigrationRegions)
}

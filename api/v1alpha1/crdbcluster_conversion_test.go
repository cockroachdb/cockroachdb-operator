package v1alpha1

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/migrationutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newBasicV1Alpha1Cluster() *CrdbCluster {
	return &CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
			Annotations: map[string]string{
				BetaCrdbClusterNodeCloudProviderAnnotationKey: "gcp",
				BetaCrdbClusterNodeRegionCodeAnnotationKey:    "us-central1",
			},
		},
		Spec: CrdbClusterSpec{
			Nodes:      3,
			TLSEnabled: true,
			Image: &PodImage{
				Name: "cockroachdb/cockroach:v25.4.1",
			},
			DataStore: Volume{
				VolumeClaim: &VolumeClaim{
					PersistentVolumeClaimSpec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("10Gi"),
							},
						},
					},
				},
			},
		},
	}
}

func newBasicV1Beta1Cluster() *v1beta1.CrdbCluster {
	return &v1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
		Spec: v1beta1.CrdbClusterSpec{
			TLSEnabled: true,
			Regions: []v1beta1.CrdbClusterRegion{
				{
					Code:          "us-central1",
					Nodes:         3,
					CloudProvider: "gcp",
					Namespace:     "default",
				},
			},
			Template: v1beta1.CrdbNodeTemplate{
				Spec: v1beta1.CrdbNodeSpec{
					Image: "cockroachdb/cockroach:v25.4.1",
					DataStore: v1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{Name: "datadir"},
							Spec: corev1.PersistentVolumeClaimSpec{
								AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceStorage: resource.MustParse("10Gi"),
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// withStatefulSet sets up a fake K8s client with StatefulSets for conversion
// tests that need the migration path. Pass cluster names that need a matching
// STS. If none are provided, it uses "test-cluster".
func withStatefulSet(t *testing.T, names ...string) {
	t.Helper()
	if len(names) == 0 {
		names = []string{"test-cluster"}
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, name := range names {
		for _, ns := range []string{"default", "cockroach-migration-test"} {
			sts := newStatefulSetForTesting()
			sts.Name = name
			sts.Namespace = ns
			builder = builder.WithObjects(sts)
		}
	}
	fakeClient := builder.Build()
	// Point both readers at the same fake so protected (skip-labeled) tests, which
	// read uncached, find the StatefulSet too.
	SetConversionClient(fakeClient)
	SetConversionUncachedReader(fakeClient)
	t.Cleanup(resetConversionReaders)
}

func newStatefulSetForTesting() *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](3),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "cockroachdb",
					},
					Annotations: map[string]string{
						"test-annotation": "test-value",
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "test-cluster",
					PriorityClassName:  "high-priority",
					ImagePullSecrets: []corev1.LocalObjectReference{
						{Name: "registry-secret"},
					},
					Containers: []corev1.Container{
						{
							Name:  "db",
							Image: "cockroachdb/cockroach:v25.4.1",
							Args: []string{
								"shell",
								"-ecx",
								"exec /cockroach/cockroach start --join=test-cluster-0.test-cluster.default.svc.cluster.local,test-cluster-1.test-cluster.default.svc.cluster.local,test-cluster-2.test-cluster.default.svc.cluster.local --port=26257 --http-port=8080 --locality=region=us-central1,zone=us-central1-a --cache=25% --max-sql-memory=25%",
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1"),
									corev1.ResourceMemory: resource.MustParse("2Gi"),
								},
							},
							Env: []corev1.EnvVar{
								{Name: "COCKROACH_CHANNEL", Value: "official"},
							},
						},
					},
					Affinity: &corev1.Affinity{
						PodAntiAffinity: &corev1.PodAntiAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
								{Weight: 100},
							},
						},
					},
					TerminationGracePeriodSeconds: ptr.To[int64](300),
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "datadir",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("10Gi"),
							},
						},
					},
				},
			},
		},
	}
}

// Conversion tests

// TestConvertToPublicClusterWithoutStatefulSet verifies that a new v1alpha1
// cluster remains public-operator owned before its StatefulSet is created.
func TestConvertToPublicClusterWithoutStatefulSet(t *testing.T) {
	src := newBasicV1Alpha1Cluster()
	dst := &v1beta1.CrdbCluster{}
	err := src.ConvertTo(dst)
	require.NoError(t, err)

	require.NotNil(t, dst.Spec.Mode)
	assert.Equal(t, v1beta1.Disabled, *dst.Spec.Mode)
	assert.True(t, dst.Spec.TLSEnabled)
	require.Len(t, dst.Spec.Regions, 1)
	assert.Equal(t, int32(3), dst.Spec.Regions[0].Nodes)
	assert.Equal(t, "cockroachdb/cockroach:v25.4.1", dst.Spec.Template.Spec.Image)
}

// TestRoundTripNoStatefulSetFallback guards the crdbContainer == nil fallback in
// ConvertTo, which builds the PodTemplate from v1alpha1 spec fields when no
// StatefulSet is available (an ordinary v1alpha1 cluster before its STS exists, or
// an STS with no crdb container). The STS-derived path is covered elsewhere; this
// keeps the spec-fallback path guarded against regressions.
func TestRoundTripNoStatefulSetFallback(t *testing.T) {
	// No conversion readers, so fetchStatefulSet returns nil and ConvertTo takes the
	// spec-fallback path.
	resetConversionReaders()

	affinity := &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      "node-type",
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{"crdb"},
					}},
				}},
			},
		},
	}

	src := newBasicV1Alpha1Cluster()
	src.Spec.Image.PullPolicyName = ptr.To(corev1.PullAlways)
	src.Spec.Image.PullSecret = new("registry-creds")
	src.Spec.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		},
	}
	src.Spec.PriorityClassName = "high-priority"
	src.Spec.TerminationGracePeriodSecs = 300
	src.Spec.NodeSelector = map[string]string{"disktype": "ssd"}
	src.Spec.Tolerations = []corev1.Toleration{{
		Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "crdb", Effect: corev1.TaintEffectNoSchedule,
	}}
	src.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
		MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone", WhenUnsatisfiable: corev1.DoNotSchedule,
	}}
	src.Spec.Affinity = affinity
	src.Spec.AdditionalLabels = map[string]string{"app.kubernetes.io/component": "database"}
	src.Spec.AdditionalAnnotations = map[string]string{"prometheus.io/scrape": "true"}

	// ConvertTo builds the PodTemplate from spec fields (no STS).
	dst := &v1beta1.CrdbCluster{}
	require.NoError(t, src.ConvertTo(dst))

	pt := dst.Spec.Template.Spec.PodTemplate
	require.NotNil(t, pt)
	require.Len(t, pt.Spec.Containers, 1)
	assert.Equal(t, corev1.PullAlways, pt.Spec.Containers[0].ImagePullPolicy)
	assert.Equal(t, resource.MustParse("2"), *pt.Spec.Containers[0].Resources.Requests.Cpu())
	assert.Equal(t, resource.MustParse("4Gi"), *pt.Spec.Containers[0].Resources.Requests.Memory())
	require.Len(t, pt.Spec.ImagePullSecrets, 1)
	assert.Equal(t, "registry-creds", pt.Spec.ImagePullSecrets[0].Name)
	require.NotNil(t, pt.Spec.TerminationGracePeriodSeconds)
	assert.Equal(t, int64(300), *pt.Spec.TerminationGracePeriodSeconds)
	assert.Equal(t, "high-priority", pt.Spec.PriorityClassName)
	assert.Equal(t, "ssd", pt.Spec.NodeSelector["disktype"])
	require.Len(t, pt.Spec.Tolerations, 1)
	assert.Equal(t, "dedicated", pt.Spec.Tolerations[0].Key)
	require.Len(t, pt.Spec.TopologySpreadConstraints, 1)
	assert.Equal(t, "topology.kubernetes.io/zone", pt.Spec.TopologySpreadConstraints[0].TopologyKey)
	assert.Equal(t, affinity, pt.Spec.Affinity)
	assert.Equal(t, "database", pt.Metadata.Labels["app.kubernetes.io/component"])
	assert.Equal(t, "true", pt.Metadata.Annotations["prometheus.io/scrape"])

	// ConvertFrom must restore the same fields from the PodTemplate.
	restored := &CrdbCluster{}
	require.NoError(t, restored.ConvertFrom(dst))

	require.NotNil(t, restored.Spec.Image)
	require.NotNil(t, restored.Spec.Image.PullPolicyName)
	assert.Equal(t, corev1.PullAlways, *restored.Spec.Image.PullPolicyName)
	require.NotNil(t, restored.Spec.Image.PullSecret)
	assert.Equal(t, "registry-creds", *restored.Spec.Image.PullSecret)
	assert.Equal(t, resource.MustParse("2"), *restored.Spec.Resources.Requests.Cpu())
	assert.Equal(t, int64(300), restored.Spec.TerminationGracePeriodSecs)
	assert.Equal(t, "high-priority", restored.Spec.PriorityClassName)
	assert.Equal(t, "ssd", restored.Spec.NodeSelector["disktype"])
	require.Len(t, restored.Spec.Tolerations, 1)
	assert.Equal(t, "dedicated", restored.Spec.Tolerations[0].Key)
	require.Len(t, restored.Spec.TopologySpreadConstraints, 1)
	assert.Equal(t, "topology.kubernetes.io/zone", restored.Spec.TopologySpreadConstraints[0].TopologyKey)
	assert.Equal(t, affinity, restored.Spec.Affinity)
	assert.Equal(t, "database", restored.Spec.AdditionalLabels["app.kubernetes.io/component"])
	assert.Equal(t, "true", restored.Spec.AdditionalAnnotations["prometheus.io/scrape"])
}

func TestConvertToMigrationPathWithSkipReconcileLabel(t *testing.T) {
	withStatefulSet(t)
	src := newBasicV1Alpha1Cluster()
	src.Labels = map[string]string{
		CrdbOperatorMigrationLabel: "true",
		"crdb.io/migrate":          "start",
	}
	dst := &v1beta1.CrdbCluster{}
	err := src.ConvertTo(dst)
	require.NoError(t, err)

	// The source StatefulSet makes the full migration projection available.
	require.NotNil(t, dst.Spec.Mode)
	assert.Equal(t, v1beta1.Disabled, *dst.Spec.Mode)
}

func TestConvertToRejectsProtectedClusterWithoutStatefulSet(t *testing.T) {
	SetConversionClient(nil)
	SetConversionUncachedReader(nil)
	t.Cleanup(resetConversionReaders)

	t.Run("direct v1alpha1 write", func(t *testing.T) {
		src := newBasicV1Alpha1Cluster()
		src.Labels = map[string]string{
			CrdbOperatorMigrationLabel: "true",
			"crdb.io/migrate":          "complete",
		}
		dst := &v1beta1.CrdbCluster{}
		err := src.ConvertTo(dst)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot convert protected v1alpha1 CrdbCluster")
	})

	t.Run("incomplete v1beta1 handoff round trip", func(t *testing.T) {
		src := newBasicV1Beta1Cluster()
		mode := v1beta1.MutableOnly
		src.Spec.Mode = &mode
		src.Labels = map[string]string{
			CrdbOperatorMigrationLabel: "true",
			"crdb.io/migrate":          "in-progress",
		}
		src.Status.Migration = &v1beta1.MigrationStatus{Phase: "Finalization"}

		alpha := &CrdbCluster{}
		require.NoError(t, alpha.ConvertFrom(src))
		require.Equal(t, string(v1beta1.MutableOnly), alpha.Annotations[BetaCrdbClusterModeAnnotationKey],
			"full ConvertFrom must carry the mode annotation during the handoff")

		back := &v1beta1.CrdbCluster{}
		err := alpha.ConvertTo(back)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot convert protected v1alpha1 CrdbCluster")
	})
}

// errReader is a client whose Get always fails, used to simulate a StatefulSet
// fetch error (not a NotFound) during conversion. It embeds client.Client so it
// satisfies both SetConversionClient (cached) and SetConversionUncachedReader.
type errReader struct {
	client.Client
	err error
}

func (r errReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.err
}

func resetConversionReaders() {
	SetConversionClient(nil)
	SetConversionUncachedReader(nil)
}

func TestConvertToFetchErrorOrdinaryClusterUsesDefaults(t *testing.T) {
	// Ordinary clusters read through the cached client.
	SetConversionClient(errReader{err: errors.New("api server timeout")})
	t.Cleanup(resetConversionReaders)

	// An unlabeled public cluster has nothing to protect, so a transient fetch
	// error should fall back to defaults and still convert.
	src := newBasicV1Alpha1Cluster()
	dst := &v1beta1.CrdbCluster{}
	require.NoError(t, src.ConvertTo(dst))
	require.NotNil(t, dst.Spec.Mode)
	assert.Equal(t, v1beta1.Disabled, *dst.Spec.Mode)
}

func TestConvertToFetchErrorProtectedClusterFailsClosed(t *testing.T) {
	// Protected (skip-labeled) clusters read through the uncached reader.
	SetConversionUncachedReader(errReader{err: errors.New("api server timeout")})
	t.Cleanup(resetConversionReaders)

	// A skip-labeled cluster is enterprise-owned, so a fetch error must fail closed
	// rather than convert from defaults and overwrite its stored spec.
	src := newBasicV1Alpha1Cluster()
	src.Labels = map[string]string{CrdbOperatorMigrationLabel: "true"}
	dst := &v1beta1.CrdbCluster{}
	err := src.ConvertTo(dst)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to fetch StatefulSet for protected")
}

// TestConvertToReaderSelection verifies that protected (skip-labeled) clusters read
// the StatefulSet through the uncached reader while ordinary clusters use the cache.
// The cache says the STS is present (stale); the uncached reader says it is gone.
func TestConvertToReaderSelection(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	stsPresent := newStatefulSetForTesting()
	stsPresent.Name = "test-cluster"
	stsPresent.Namespace = "default"
	cached := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stsPresent).Build()
	uncached := fake.NewClientBuilder().WithScheme(scheme).Build() // STS gone

	SetConversionClient(cached)
	SetConversionUncachedReader(uncached)
	t.Cleanup(resetConversionReaders)

	t.Run("protected cluster uses the uncached reader", func(t *testing.T) {
		src := newBasicV1Alpha1Cluster()
		src.Labels = map[string]string{CrdbOperatorMigrationLabel: "true"}
		dst := &v1beta1.CrdbCluster{}
		err := src.ConvertTo(dst)
		// Uncached says the STS is gone, so a protected write is rejected.
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot convert protected")
	})

	t.Run("ordinary cluster uses the cached client", func(t *testing.T) {
		src := newBasicV1Alpha1Cluster()
		dst := &v1beta1.CrdbCluster{}
		// Cache says the STS is present, so conversion parses it and succeeds.
		require.NoError(t, src.ConvertTo(dst))
		require.NotNil(t, dst.Spec.Mode)
		assert.Equal(t, v1beta1.Disabled, *dst.Spec.Mode)
	})
}

// TestConvertToWithStatefulSet verifies v1alpha1 to v1beta1 conversion when a
// StatefulSet exists. The converter should extract configuration from the
// StatefulSet's pod template and command-line flags.
func TestConvertToWithStatefulSet(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	sts := newStatefulSetForTesting()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sts).
		Build()

	SetConversionClient(fakeClient)
	t.Cleanup(func() { SetConversionClient(nil) })

	src := newBasicV1Alpha1Cluster()
	dst := &v1beta1.CrdbCluster{}
	err := src.ConvertTo(dst)
	require.NoError(t, err)

	assert.Equal(t, "test-cluster", dst.Spec.Template.Spec.ServiceAccountName)
	assert.Equal(t, "high-priority", dst.Spec.Template.Spec.PodTemplate.Spec.PriorityClassName)

	require.Len(t, dst.Spec.Template.Spec.PodTemplate.Spec.ImagePullSecrets, 1)
	assert.Equal(t, "registry-secret", dst.Spec.Template.Spec.PodTemplate.Spec.ImagePullSecrets[0].Name)

	require.NotNil(t, dst.Spec.Template.Spec.SQLPort)
	assert.Equal(t, int32(26257), *dst.Spec.Template.Spec.SQLPort)
	require.NotNil(t, dst.Spec.Template.Spec.HTTPPort)
	assert.Equal(t, int32(8080), *dst.Spec.Template.Spec.HTTPPort)

	assert.Contains(t, dst.Spec.Template.Spec.LocalityLabels, "region")
	assert.Contains(t, dst.Spec.Template.Spec.LocalityLabels, "zone")

	require.NotNil(t, dst.Spec.Template.Spec.StartFlags)
	hasJoinFlag := false
	for _, flag := range dst.Spec.Template.Spec.StartFlags.Upsert {
		if strings.HasPrefix(flag, "--join=") {
			hasJoinFlag = true
			break
		}
	}
	assert.True(t, hasJoinFlag, "join flag should be present")

	assert.Equal(t, "datadir", dst.Spec.Template.Spec.DataStore.VolumeClaimTemplate.Name)
	assert.Equal(t, "10Gi", dst.Spec.Template.Spec.DataStore.VolumeClaimTemplate.Spec.Resources.Requests.Storage().String())

	assert.Equal(t, "cockroachdb", dst.Spec.Template.Spec.PodTemplate.Metadata.Labels["app"])
	assert.Equal(t, "test-value", dst.Spec.Template.Spec.PodTemplate.Metadata.Annotations["test-annotation"])

	assert.NotNil(t, dst.Spec.Template.Spec.PodTemplate.Spec.Affinity)
	assert.NotNil(t, dst.Spec.Template.Spec.PodTemplate.Spec.Affinity.PodAntiAffinity)

	assert.Equal(t, int64(300), *dst.Spec.Template.Spec.PodTemplate.Spec.TerminationGracePeriodSeconds)
}

// TestConvertFrom verifies v1beta1 to v1alpha1 conversion, ensuring basic
// fields are correctly mapped back to the v1alpha1 structure.
func TestConvertFrom(t *testing.T) {
	disabled := v1beta1.Disabled
	src := newBasicV1Beta1Cluster()
	src.Spec.Mode = &disabled
	src.Spec.Template.Spec.ResourceRequirements = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
	}

	dst := &CrdbCluster{}
	err := dst.ConvertFrom(src)
	require.NoError(t, err)

	assert.Equal(t, "test-cluster", dst.Name)
	assert.Equal(t, "default", dst.Namespace)
	assert.True(t, dst.Spec.TLSEnabled)
	assert.Equal(t, int32(3), dst.Spec.Nodes)
	assert.Equal(t, "cockroachdb/cockroach:v25.4.1", dst.Spec.Image.Name)

	assert.Equal(t, "1", dst.Spec.Resources.Requests.Cpu().String())
	assert.Equal(t, "2Gi", dst.Spec.Resources.Requests.Memory().String())

	assert.NotNil(t, dst.Spec.DataStore.VolumeClaim)
	assert.Equal(t, "10Gi", dst.Spec.DataStore.VolumeClaim.PersistentVolumeClaimSpec.Resources.Requests.Storage().String())
}

// TestConvertToMinimalInsecure mirrors 01-minimal-insecure.yaml.
// Only required fields are set; every omitempty field is absent. Verifies that no
// nil-pointer panics occur and that the insecure cluster has no ExternalCertificates.
func TestConvertToMinimalInsecure(t *testing.T) {
	withStatefulSet(t, "minimal-insecure")
	src := &CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "minimal-insecure",
			Namespace: "cockroach-migration-test",
			Annotations: map[string]string{
				BetaCrdbClusterNodeRegionCodeAnnotationKey:    "us-central1",
				BetaCrdbClusterNodeCloudProviderAnnotationKey: "gcp",
			},
		},
		Spec: CrdbClusterSpec{
			Nodes:      3,
			TLSEnabled: false,
			Image:      &PodImage{Name: "cockroachdb/cockroach:v24.1.0"},
			DataStore: Volume{
				VolumeClaim: &VolumeClaim{
					PersistentVolumeClaimSpec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("10Gi"),
							},
						},
					},
				},
			},
		},
	}

	dst := &v1beta1.CrdbCluster{}
	require.NoError(t, src.ConvertTo(dst))

	assert.False(t, dst.Spec.TLSEnabled)
	assert.Nil(t, dst.Spec.Template.Spec.Certificates.ExternalCertificates,
		"insecure cluster must have no ExternalCertificates")
	require.Len(t, dst.Spec.Regions, 1)
	assert.Equal(t, int32(3), dst.Spec.Regions[0].Nodes)

	// Round-trip back: optional fields must not be invented on restore.
	restored := &CrdbCluster{}
	require.NoError(t, restored.ConvertFrom(dst))
	assert.False(t, restored.Spec.TLSEnabled)
	assert.Empty(t, restored.Spec.NodeTLSSecret)
	assert.Empty(t, restored.Spec.ClientTLSSecret)
	assert.Nil(t, restored.Spec.MinAvailable)
	assert.False(t, restored.Spec.AutomountServiceAccountToken)
}

// TestRoundTripMinimalTLS mirrors 02-minimal-tls.yaml.
// The critical invariant: nodeTLSSecret and clientTLSSecret must survive
// ConvertTo→ConvertFrom unchanged via the v1alpha1-fields annotation. The enterprise
// operator creates new secrets with different names during migration; the originals
// must be restored so the public operator can find them again on rollback.
func TestRoundTripMinimalTLS(t *testing.T) {
	withStatefulSet(t, "minimal-tls")
	src := &CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "minimal-tls",
			Namespace: "cockroach-migration-test",
			Annotations: map[string]string{
				BetaCrdbClusterNodeRegionCodeAnnotationKey:    "us-central1",
				BetaCrdbClusterNodeCloudProviderAnnotationKey: "gcp",
			},
		},
		Spec: CrdbClusterSpec{
			Nodes:           3,
			TLSEnabled:      true,
			NodeTLSSecret:   "crdb-node-tls",
			ClientTLSSecret: "crdb-root-tls",
			Image:           &PodImage{Name: "cockroachdb/cockroach:v24.1.0"},
			DataStore: Volume{
				VolumeClaim: &VolumeClaim{
					PersistentVolumeClaimSpec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("10Gi"),
							},
						},
					},
				},
			},
		},
	}

	dst := &v1beta1.CrdbCluster{}
	require.NoError(t, src.ConvertTo(dst))
	assert.True(t, dst.Spec.TLSEnabled)
	assert.NotEmpty(t, dst.Annotations[BetaCrdbClusterV1Alpha1FieldsAnnotationKey],
		"preserved-fields annotation must be written during ConvertTo")

	// ConvertFrom must restore the original secret names. The ExternalCertificates in
	// v1beta1 use enterprise-operator-generated names; the annotation holds the originals.
	restored := &CrdbCluster{}
	require.NoError(t, restored.ConvertFrom(dst))
	assert.Equal(t, "crdb-node-tls", restored.Spec.NodeTLSSecret,
		"nodeTLSSecret must come from annotation, not ExternalCertificates.NodeSecretName")
	assert.Equal(t, "crdb-root-tls", restored.Spec.ClientTLSSecret,
		"clientTLSSecret must come from annotation, not ExternalCertificates.RootSQLClientSecretName")
}

// TestRoundTripFullCustom mirrors 03-full-custom.yaml: every v1alpha1 spec field that
// has a round-trip mapping is set to a non-default value and verified to survive
// ConvertTo→ConvertFrom unchanged. This is the comprehensive regression guard for the
// conversion webhook.
func TestRoundTripFullCustom(t *testing.T) {
	withStatefulSet(t, "full-custom")
	pullPolicy := corev1.PullAlways
	pullSecret := "registry-credentials"
	maxUnavailable := int32(2)
	minAvailable := int32(3)
	grpcPort := int32(26258)
	httpPort := int32(8080)
	sqlPort := int32(26257)

	src := &CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "full-custom",
			Namespace: "cockroach-migration-test",
			Annotations: map[string]string{
				BetaCrdbClusterNodeRegionCodeAnnotationKey:    "us-central1",
				BetaCrdbClusterNodeCloudProviderAnnotationKey: "gcp",
			},
		},
		Spec: CrdbClusterSpec{
			Nodes:      5,
			TLSEnabled: true,
			Image: &PodImage{
				Name:           "cockroachdb/cockroach:v24.1.0",
				PullPolicyName: &pullPolicy,
				PullSecret:     &pullSecret,
			},
			GRPCPort:        &grpcPort,
			HTTPPort:        &httpPort,
			SQLPort:         &sqlPort,
			NodeTLSSecret:   "prod-crdb-node-tls",
			ClientTLSSecret: "prod-crdb-root-tls",
			MaxUnavailable:  &maxUnavailable,
			MinAvailable:    &minAvailable,
			Cache:           "512MiB",
			MaxSQLMemory:    "512MiB",
			AdditionalArgs:  []string{"--cluster-name=prod-crdb"},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
			DataStore: Volume{
				VolumeClaim: &VolumeClaim{
					PersistentVolumeClaimSpec: corev1.PersistentVolumeClaimSpec{
						AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						StorageClassName: new("fast-ssd"),
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("100Gi"),
							},
						},
					},
				},
			},
			PriorityClassName: "high-priority",
			PodEnvVariables: []corev1.EnvVar{
				{Name: "COCKROACH_CHANNEL", Value: "operator"},
			},
			Affinity: &corev1.Affinity{
				PodAntiAffinity: &corev1.PodAntiAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
						{TopologyKey: "kubernetes.io/hostname"},
					},
				},
			},
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
				{
					MaxSkew:           1,
					TopologyKey:       "topology.kubernetes.io/zone",
					WhenUnsatisfiable: corev1.DoNotSchedule,
				},
			},
			AdditionalLabels: map[string]string{
				"app.kubernetes.io/component": "database",
				"team":                        "platform",
			},
			AdditionalAnnotations: map[string]string{
				"prometheus.io/scrape": "true",
				"prometheus.io/port":   "8080",
			},
			Tolerations: []corev1.Toleration{
				{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "cockroachdb", Effect: corev1.TaintEffectNoSchedule},
			},
			NodeSelector:                 map[string]string{"node-type": "crdb"},
			LogConfigMap:                 "crdb-log-config",
			AutomountServiceAccountToken: true,
			TerminationGracePeriodSecs:   600,
		},
	}

	// ConvertTo uses the STS as source of truth for image, pull secrets, and
	// termination grace period. Spec-level values are overridden by STS values.
	dst := &v1beta1.CrdbCluster{}
	require.NoError(t, src.ConvertTo(dst))

	require.Len(t, dst.Spec.Template.Spec.PodTemplate.Spec.Containers, 1)

	// ImagePullPolicy comes from v1alpha1 spec, ImagePullSecrets and
	// TerminationGracePeriodSeconds come from the STS pod template.
	assert.Equal(t, corev1.PullAlways,
		dst.Spec.Template.Spec.PodTemplate.Spec.Containers[0].ImagePullPolicy)
	require.Len(t, dst.Spec.Template.Spec.PodTemplate.Spec.ImagePullSecrets, 1)
	assert.Equal(t, "registry-secret",
		dst.Spec.Template.Spec.PodTemplate.Spec.ImagePullSecrets[0].Name)
	require.NotNil(t, dst.Spec.Template.Spec.PodTemplate.Spec.TerminationGracePeriodSeconds)
	assert.Equal(t, int64(300),
		*dst.Spec.Template.Spec.PodTemplate.Spec.TerminationGracePeriodSeconds)

	require.NotNil(t, dst.Spec.DisruptionBudget)
	assert.Equal(t, 2, *dst.Spec.DisruptionBudget)
	assert.NotEmpty(t, dst.Annotations[BetaCrdbClusterV1Alpha1FieldsAnnotationKey])
	assert.Contains(t, dst.Spec.Template.Spec.StartFlags.Upsert, "--cache=512MiB")
	assert.Contains(t, dst.Spec.Template.Spec.StartFlags.Upsert, "--max-sql-memory=512MiB")
	assert.Contains(t, dst.Spec.Template.Spec.StartFlags.Upsert, "--cluster-name=prod-crdb")

	// Full round-trip: ConvertFrom must restore v1alpha1 spec fields.
	restored := &CrdbCluster{}
	require.NoError(t, restored.ConvertFrom(dst))

	assert.Equal(t, int32(5), restored.Spec.Nodes)

	require.NotNil(t, restored.Spec.GRPCPort)
	assert.Equal(t, int32(26258), *restored.Spec.GRPCPort)
	require.NotNil(t, restored.Spec.HTTPPort)
	assert.Equal(t, int32(8080), *restored.Spec.HTTPPort)
	require.NotNil(t, restored.Spec.SQLPort)
	assert.Equal(t, int32(26257), *restored.Spec.SQLPort)

	// TLS secret names restored from annotation, not from ExternalCertificates.
	assert.Equal(t, "prod-crdb-node-tls", restored.Spec.NodeTLSSecret)
	assert.Equal(t, "prod-crdb-root-tls", restored.Spec.ClientTLSSecret)

	// PDB fields: MaxUnavailable from DisruptionBudget, MinAvailable from annotation.
	require.NotNil(t, restored.Spec.MaxUnavailable)
	assert.Equal(t, int32(2), *restored.Spec.MaxUnavailable)
	require.NotNil(t, restored.Spec.MinAvailable)
	assert.Equal(t, int32(3), *restored.Spec.MinAvailable)

	assert.Equal(t, "512MiB", restored.Spec.Cache)
	assert.Equal(t, "512MiB", restored.Spec.MaxSQLMemory)
	assert.Contains(t, restored.Spec.AdditionalArgs, "--cluster-name=prod-crdb")

	// Resources come from STS container, not v1alpha1 spec.
	assert.Equal(t, resource.MustParse("1"), *restored.Spec.Resources.Requests.Cpu())
	assert.Equal(t, resource.MustParse("2Gi"), *restored.Spec.Resources.Requests.Memory())

	assert.Equal(t, "high-priority", restored.Spec.PriorityClassName)

	// COCKROACH_CHANNEL survives; HOST_IP/GODEBUG are filtered by filterOperatorEnvVars.
	require.Len(t, restored.Spec.PodEnvVariables, 1)
	assert.Equal(t, "COCKROACH_CHANNEL", restored.Spec.PodEnvVariables[0].Name)
	assert.Equal(t, "operator", restored.Spec.PodEnvVariables[0].Value)

	require.NotNil(t, restored.Spec.Affinity)
	require.NotNil(t, restored.Spec.Affinity.PodAntiAffinity)

	// TopologySpreadConstraints from STS template take precedence. The test STS
	// does not set them, so the round-tripped value may differ from the spec.

	// Labels and annotations come from the STS pod template, not the v1alpha1 spec.
	assert.Equal(t, "cockroachdb", restored.Spec.AdditionalLabels["app"])
	assert.Equal(t, "test-value", restored.Spec.AdditionalAnnotations["test-annotation"])

	// Tolerations and NodeSelector from the STS template take precedence.
	// The test STS does not set them.

	assert.Equal(t, "crdb-log-config", restored.Spec.LogConfigMap)

	// v1alpha1-only fields from annotation.
	assert.True(t, restored.Spec.AutomountServiceAccountToken)
	// TerminationGracePeriodSecs comes from STS (300), not v1alpha1 spec (600).
	assert.Equal(t, int64(300), restored.Spec.TerminationGracePeriodSecs)
}

// TestPVCNameConsistency verifies that PVC names are always set to "datadir"
// to maintain compatibility with both public and enterprise operator naming
// conventions. This ensures existing PVCs created by StatefulSets are reused.
func TestPVCNameConsistency(t *testing.T) {
	withStatefulSet(t, "cockroachdb")
	src := &CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cockroachdb",
			Namespace: "default",
			Annotations: map[string]string{
				BetaCrdbClusterNodeRegionCodeAnnotationKey:    "us-central1",
				BetaCrdbClusterNodeCloudProviderAnnotationKey: "gcp",
			},
		},
		Spec: CrdbClusterSpec{
			Nodes:      3,
			TLSEnabled: true,
			DataStore: Volume{
				VolumeClaim: &VolumeClaim{
					PersistentVolumeClaimSpec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("10Gi"),
							},
						},
					},
				},
			},
		},
	}

	dst := &v1beta1.CrdbCluster{}
	err := src.ConvertTo(dst)
	require.NoError(t, err)

	assert.Equal(t, "datadir", dst.Spec.Template.Spec.DataStore.VolumeClaimTemplate.Name,
		"PVC template name must be 'datadir' to match existing PVCs created by StatefulSet")
}

// TestRoundTripConversionPreservesBetaFields verifies that v1beta1-specific
// fields survive a round-trip conversion (v1beta1 -> v1alpha1 -> v1beta1).
// This is critical for helm upgrades to work correctly, as the conversion
// webhook must preserve fields that don't exist in v1alpha1.
func TestRoundTripConversionPreservesBetaFields(t *testing.T) {
	withStatefulSet(t)
	disabled := v1beta1.Disabled
	original := &v1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
		Spec: v1beta1.CrdbClusterSpec{
			Mode: &disabled,
			RollingRestartDelay: &metav1.Duration{
				Duration: 5 * time.Minute,
			},
			ClusterSettings: map[string]string{
				"kv.snapshot_rebalance.max_rate": "128MiB",
				"server.time_until_store_dead":   "5m0s",
			},
			Features: []v1beta1.ClusterFeature{
				"feature1",
				"feature2",
			},
			TLSEnabled: true,
			Regions: []v1beta1.CrdbClusterRegion{
				{
					Code:          "us-central1",
					Nodes:         5,
					CloudProvider: "gcp",
					Namespace:     "default",
					Domain:        "cluster.local",
				},
				{
					Code:          "us-east1",
					Nodes:         3,
					CloudProvider: "gcp",
					Namespace:     "production",
					Domain:        "cluster.local",
				},
			},
			Template: v1beta1.CrdbNodeTemplate{
				Spec: v1beta1.CrdbNodeSpec{
					Image: "cockroachdb/cockroach:v25.4.1",
					StartFlags: &v1beta1.Flags{
						Upsert: []string{"--cache=30%", "--max-sql-memory=30%"},
						Omit:   []string{},
					},
					DataStore: v1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{Name: "datadir"},
							Spec: corev1.PersistentVolumeClaimSpec{
								AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceStorage: resource.MustParse("100Gi"),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	intermediate := &CrdbCluster{}
	err := intermediate.ConvertFrom(original)
	require.NoError(t, err)

	assert.Equal(t, "Disabled", intermediate.Annotations[BetaCrdbClusterModeAnnotationKey])
	assert.Equal(t, "5m0s", intermediate.Annotations[BetaCrdbClusterRestartDelayAnnotationKey])
	assert.NotEmpty(t, intermediate.Annotations[BetaCrdbClusterSettingsAnnotationKey])
	assert.NotEmpty(t, intermediate.Annotations[BetaCrdbClusterFeaturesAnnotationKey])
	assert.NotEmpty(t, intermediate.Annotations[BetaCrdbClusterRegionsAnnotationKey])

	roundTripped := &v1beta1.CrdbCluster{}
	err = intermediate.ConvertTo(roundTripped)
	require.NoError(t, err)

	require.NotNil(t, roundTripped.Spec.Mode)
	assert.Equal(t, v1beta1.Disabled, *roundTripped.Spec.Mode)

	require.NotNil(t, roundTripped.Spec.RollingRestartDelay)
	assert.Equal(t, 5*time.Minute, roundTripped.Spec.RollingRestartDelay.Duration)

	assert.Equal(t, original.Spec.ClusterSettings, roundTripped.Spec.ClusterSettings)
	assert.Equal(t, original.Spec.Features, roundTripped.Spec.Features)

	require.Len(t, roundTripped.Spec.Regions, 2)
	assert.Equal(t, "us-central1", roundTripped.Spec.Regions[0].Code)
	assert.Equal(t, int32(5), roundTripped.Spec.Regions[0].Nodes)
	assert.Equal(t, "cluster.local", roundTripped.Spec.Regions[0].Domain)
	assert.Equal(t, "us-east1", roundTripped.Spec.Regions[1].Code)
	assert.Equal(t, int32(3), roundTripped.Spec.Regions[1].Nodes)
}

// TestRoundTripConversionDisabledModePersists verifies that Mode=Disabled
// survives a round-trip through the full migration conversion path. Only
// Disabled-mode clusters take the annotation-heavy path; all other modes
// use the minimal fast path that skips annotations entirely.
func TestRoundTripConversionDisabledModePersists(t *testing.T) {
	withStatefulSet(t)
	disabled := v1beta1.Disabled
	migrationCluster := &v1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
		Spec: v1beta1.CrdbClusterSpec{
			Mode:       &disabled,
			TLSEnabled: true,
			Regions: []v1beta1.CrdbClusterRegion{
				{
					Code:          "us-central1",
					Nodes:         3,
					CloudProvider: "gcp",
					Namespace:     "default",
				},
			},
			Template: v1beta1.CrdbNodeTemplate{
				Spec: v1beta1.CrdbNodeSpec{
					Image: "cockroachdb/cockroach:v25.4.1",
					DataStore: v1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{Name: "datadir"},
							Spec: corev1.PersistentVolumeClaimSpec{
								AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceStorage: resource.MustParse("10Gi"),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	alpha := &CrdbCluster{}
	err := alpha.ConvertFrom(migrationCluster)
	require.NoError(t, err)

	beta := &v1beta1.CrdbCluster{}
	err = alpha.ConvertTo(beta)
	require.NoError(t, err)

	require.NotNil(t, beta.Spec.Mode)
	assert.Equal(t, v1beta1.Disabled, *beta.Spec.Mode,
		"Disabled mode must persist after round-trip conversion")
}

// TestRoundTripConversionWithTenantHost verifies that TenantHost configuration
// (including SQL proxy and tenant pool settings) survives round-trip conversion.
func TestRoundTripConversionWithTenantHost(t *testing.T) {
	withStatefulSet(t, "tenant-host-cluster")
	disabled := v1beta1.Disabled
	original := &v1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-host-cluster",
			Namespace: "default",
		},
		Spec: v1beta1.CrdbClusterSpec{
			Mode:       &disabled,
			TLSEnabled: true,
			TenantHost: &v1beta1.TenantHost{
				SqlProxyImage:    "us.gcr.io/cockroach-cloud-images/sql-proxy:v1.0.0",
				TenantDirImage:   "us.gcr.io/cockroach-cloud-images/tenantdir:v1.0.0",
				SqlProxyReplicas: ptr.To[int32](3),
				TenantPools: []v1beta1.TenantPool{
					{
						Name: "default",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("333m"),
								corev1.ResourceMemory: resource.MustParse("1500M"),
							},
						},
					},
				},
			},
			Regions: []v1beta1.CrdbClusterRegion{
				{Code: "us-central1", Nodes: 3, CloudProvider: "gcp"},
			},
			Template: v1beta1.CrdbNodeTemplate{
				Spec: v1beta1.CrdbNodeSpec{
					Image: "cockroachdb/cockroach:v25.4.1",
					DataStore: v1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{Name: "datadir"},
							Spec: corev1.PersistentVolumeClaimSpec{
								AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceStorage: resource.MustParse("10Gi"),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	alpha := &CrdbCluster{}
	err := alpha.ConvertFrom(original)
	require.NoError(t, err)

	beta := &v1beta1.CrdbCluster{}
	err = alpha.ConvertTo(beta)
	require.NoError(t, err)

	require.NotNil(t, beta.Spec.TenantHost)
	assert.Equal(t, original.Spec.TenantHost.SqlProxyImage, beta.Spec.TenantHost.SqlProxyImage)
	assert.Equal(t, original.Spec.TenantHost.TenantDirImage, beta.Spec.TenantHost.TenantDirImage)
	require.NotNil(t, beta.Spec.TenantHost.SqlProxyReplicas)
	assert.Equal(t, int32(3), *beta.Spec.TenantHost.SqlProxyReplicas)
	require.Len(t, beta.Spec.TenantHost.TenantPools, 1)
	assert.Equal(t, "default", beta.Spec.TenantHost.TenantPools[0].Name)
}

// TestConvertFromStartFlagsNilPointerFix verifies the fix for a nil pointer bug
// where ConvertFrom checked the deprecated Flags field but accessed StartFlags.
// This ensures that StartFlags are properly converted even when Flags is nil.
func TestConvertFromStartFlagsNilPointerFix(t *testing.T) {
	disabled := v1beta1.Disabled
	src := &v1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
		Spec: v1beta1.CrdbClusterSpec{
			Mode:       &disabled,
			TLSEnabled: true,
			Regions: []v1beta1.CrdbClusterRegion{
				{Code: "us-central1", Nodes: 3, CloudProvider: "gcp"},
			},
			Template: v1beta1.CrdbNodeTemplate{
				Spec: v1beta1.CrdbNodeSpec{
					Image: "cockroachdb/cockroach:v25.4.1",
					StartFlags: &v1beta1.Flags{
						Upsert: []string{"--cache=25%", "--max-sql-memory=25%", "--custom-flag=value"},
						Omit:   []string{},
					},
					DataStore: v1beta1.DataStore{
						VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{Name: "datadir"},
							Spec: corev1.PersistentVolumeClaimSpec{
								AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceStorage: resource.MustParse("10Gi"),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	dst := &CrdbCluster{}
	err := dst.ConvertFrom(src)
	require.NoError(t, err)

	assert.Equal(t, "25%", dst.Spec.Cache)
	assert.Equal(t, "25%", dst.Spec.MaxSQLMemory)
	assert.Contains(t, dst.Spec.AdditionalArgs, "--custom-flag=value")
}

// TestConsistencyBetweenMigrationControllerAndWebhook checks that the controller path (Helm migration)
// and webhook path (Operator migration) produce compatible specs from the same StatefulSet.
// If these diverge, we'd end up with different cluster configs depending on migration type.
func TestConsistencyBetweenMigrationControllerAndWebhook(t *testing.T) {
	// Standard StatefulSet that could come from either Helm or Public Operator
	replicas := int32(3)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/name": "cockroachdb",
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: "test-cluster",
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name": "cockroachdb",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name": "cockroachdb",
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "test-cluster-sa",
					Containers: []corev1.Container{
						{
							Name:  "db",
							Image: "cockroachdb/cockroach:v25.4.1",
							Args: []string{
								"start",
								"--certs-dir=/cockroach/cockroach-certs",
								"--advertise-host=$(POD_NAME).test-cluster",
								"--http-addr=0.0.0.0",
								"--join=test-cluster-0.test-cluster,test-cluster-1.test-cluster,test-cluster-2.test-cluster",
								"--cache=256MiB",
								"--max-sql-memory=256MiB",
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("2Gi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("2"),
									corev1.ResourceMemory: resource.MustParse("8Gi"),
								},
							},
							Ports: []corev1.ContainerPort{
								{Name: "grpc", ContainerPort: 26257},
								{Name: "http", ContainerPort: 8080},
							},
						},
					},
					Affinity: &corev1.Affinity{
						PodAntiAffinity: &corev1.PodAntiAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
								{
									Weight: 100,
									PodAffinityTerm: corev1.PodAffinityTerm{
										LabelSelector: &metav1.LabelSelector{
											MatchLabels: map[string]string{
												"app.kubernetes.io/name": "cockroachdb",
											},
										},
										TopologyKey: "kubernetes.io/hostname",
									},
								},
							},
						},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "datadir"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("10Gi"),
							},
						},
					},
				},
			},
		},
	}

	// Parse the StatefulSet to make sure the utility works (controller uses this)
	_, err := migrationutil.ParseStatefulSet(sts, sts.Name)
	require.NoError(t, err)

	// Set up a v1alpha1 cluster to simulate Operator migration path
	v1alpha1Cluster := &CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
			Annotations: map[string]string{
				BetaCrdbClusterNodeRegionCodeAnnotationKey:    "us-central1",
				BetaCrdbClusterNodeCloudProviderAnnotationKey: "gcp",
			},
		},
		Spec: CrdbClusterSpec{
			Nodes:      3,
			TLSEnabled: true,
			Image: &PodImage{
				Name: "cockroachdb/cockroach:v25.4.1",
			},
		},
	}

	// Run the conversion (webhook path)
	v1beta1Cluster := &v1beta1.CrdbCluster{}

	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts).Build()
	SetConversionClient(fakeClient)
	t.Cleanup(func() { SetConversionClient(nil) })

	err = v1alpha1Cluster.ConvertTo(v1beta1Cluster)
	require.NoError(t, err)

	webhookNodeSpec := v1beta1Cluster.Spec.Template.Spec

	// We can't call buildNodeSpec directly (it's not exported from migrationctrl),
	// so just verify that the key fields match what we'd expect from the controller path.

	assert.Equal(t, "test-cluster-sa", webhookNodeSpec.ServiceAccountName)

	// Resources should match
	assert.Equal(t, sts.Spec.Template.Spec.Containers[0].Resources.Requests,
		webhookNodeSpec.PodTemplate.Spec.Containers[0].Resources.Requests,
		"Resources.Requests should match between webhook and controller paths")

	assert.Equal(t, sts.Spec.Template.Spec.Containers[0].Resources.Limits,
		webhookNodeSpec.PodTemplate.Spec.Containers[0].Resources.Limits)

	// Affinity should match
	assert.Equal(t, sts.Spec.Template.Spec.Affinity,
		webhookNodeSpec.PodTemplate.Spec.Affinity)

	// Image should match
	assert.Equal(t, "cockroachdb/cockroach:v25.4.1", webhookNodeSpec.Image)

	// StartFlags should have the parsed flags from the args
	require.NotNil(t, webhookNodeSpec.StartFlags)
	assert.Contains(t, webhookNodeSpec.StartFlags.Upsert, "--cache=256MiB")
	assert.Contains(t, webhookNodeSpec.StartFlags.Upsert, "--max-sql-memory=256MiB")

	// Certificate naming should be consistent
	require.NotNil(t, webhookNodeSpec.Certificates.ExternalCertificates)
	assert.Equal(t, "test-cluster-node-secret", webhookNodeSpec.Certificates.ExternalCertificates.NodeSecretName)
	assert.Equal(t, "test-cluster-client-secret", webhookNodeSpec.Certificates.ExternalCertificates.RootSQLClientSecretName)
	assert.Equal(t, "test-cluster-ca-crt", webhookNodeSpec.Certificates.ExternalCertificates.CAConfigMapName)

	// PVC should match
	assert.Equal(t, "datadir", webhookNodeSpec.DataStore.VolumeClaimTemplate.Name)
	assert.Equal(t, sts.Spec.VolumeClaimTemplates[0].Spec,
		webhookNodeSpec.DataStore.VolumeClaimTemplate.Spec)

	// Note: InitContainers and custom Volumes differ intentionally:
	// - Webhook (Operator migration): skips them since v1alpha1 doesn't support them
	// - Controller (Helm migration): includes custom InitContainers/Volumes from Helm values
}

// TestEnvVarDeduplicationOnConvertTo verifies that duplicate env var names in the
// StatefulSet container do not produce duplicate entries in the converted v1beta1 spec.
// This is the env-var parallel to the defensive de-dupe already applied to StartFlags.Upsert.
func TestEnvVarDeduplicationOnConvertTo(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	// StatefulSet carries COCKROACH_CHANNEL twice. This can happen when Helm templates
	// or migration tooling injects the same var from multiple sources.
	sts := newStatefulSetForTesting()
	sts.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: "COCKROACH_CHANNEL", Value: "official"},
		{Name: "HOST_IP", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.hostIP"},
		}},
		// duplicate — first occurrence must win
		{Name: "COCKROACH_CHANNEL", Value: "unofficial"},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts).Build()
	SetConversionClient(fakeClient)
	t.Cleanup(func() { SetConversionClient(nil) })

	dst := &v1beta1.CrdbCluster{}
	err := newBasicV1Alpha1Cluster().ConvertTo(dst)
	require.NoError(t, err)

	containers := dst.Spec.Template.Spec.PodTemplate.Spec.Containers
	require.Len(t, containers, 1)

	seen := make(map[string]int)
	for _, e := range containers[0].Env {
		seen[e.Name]++
	}
	for name, count := range seen {
		assert.Equal(t, 1, count, "env var %q appears %d times, expected 1", name, count)
	}

	// the first COCKROACH_CHANNEL value must be preserved, not the duplicate
	for _, e := range containers[0].Env {
		if e.Name == "COCKROACH_CHANNEL" {
			assert.Equal(t, "official", e.Value)
			break
		}
	}
}

// TestOperatorEnvVarsNotRoundTripped verifies that env vars the enterprise operator injects
// internally (HOST_IP, GODEBUG) do not survive a round-trip into v1alpha1 PodEnvVariables.
// If they did, the v1alpha1 operator would re-add them alongside its own auto-generated
// vars and the STS pod would end up with duplicate env entries.
func TestOperatorEnvVarsNotRoundTripped(t *testing.T) {
	// Start with a v1beta1 cluster whose container env already has HOST_IP and GODEBUG
	// (as if ConvertTo ran previously and stored them).
	disabled := v1beta1.Disabled
	betaCluster := &v1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"},
		Spec: v1beta1.CrdbClusterSpec{
			Mode:       &disabled,
			TLSEnabled: true,
			Regions:    []v1beta1.CrdbClusterRegion{{Code: "us-central1", Nodes: 3, Namespace: "default"}},
			Template: v1beta1.CrdbNodeTemplate{
				Spec: v1beta1.CrdbNodeSpec{
					PodTemplate: &v1beta1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name: "cockroachdb",
									Env: []corev1.EnvVar{
										{Name: "CUSTOM_VAR", Value: "user-set"},
										{Name: "HOST_IP", ValueFrom: &corev1.EnvVarSource{
											FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.hostIP"},
										}},
										{Name: "GODEBUG", Value: "disablethp=1"},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	alpha := &CrdbCluster{}
	err := alpha.ConvertFrom(betaCluster)
	require.NoError(t, err)

	// Only the user-set var should survive; HOST_IP and GODEBUG must be stripped.
	names := make(map[string]bool, len(alpha.Spec.PodEnvVariables))
	for _, env := range alpha.Spec.PodEnvVariables {
		names[env.Name] = true
	}
	assert.True(t, names["CUSTOM_VAR"], "user-set env var must survive round-trip")
	assert.False(t, names["HOST_IP"], "HOST_IP must not survive into PodEnvVariables")
	assert.False(t, names["GODEBUG"], "GODEBUG must not survive into PodEnvVariables")
}

// TestMigrationStatusRoundTripPreserved verifies that v1beta1 migration status survives a
// v1beta1 -> v1alpha1 -> v1beta1 round trip. This mirrors a user updating labels on the
// v1alpha1 view of the resource while migration is in progress.
func TestMigrationStatusRoundTripPreserved(t *testing.T) {
	withStatefulSet(t)
	disabled := v1beta1.Disabled
	startTime := metav1.NewTime(time.Unix(1700000000, 0).UTC())
	updateTime := metav1.NewTime(time.Unix(1700000300, 0).UTC())
	original := &v1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
		Spec: v1beta1.CrdbClusterSpec{
			Mode:       &disabled,
			TLSEnabled: true,
			Regions: []v1beta1.CrdbClusterRegion{
				{Code: "us-central1", Nodes: 3, Namespace: "default"},
			},
			Template: v1beta1.CrdbNodeTemplate{
				Spec: v1beta1.CrdbNodeSpec{
					Image: "cockroachdb/cockroach:v25.4.1",
				},
			},
		},
		Status: v1beta1.CrdbClusterStatus{
			Version: "v25.4.1",
			Image:   "cockroachdb/cockroach:v25.4.1",
			Migration: &v1beta1.MigrationStatus{
				Phase:           "Stopped",
				Type:            "operator",
				StartTime:       &startTime,
				LastUpdateTime:  &updateTime,
				CurrentPodIndex: 1,
				TotalPods:       3,
				MigratedNodes:   []string{"test-cluster-2"},
				Message:         "Migration paused by user.",
				LastError:       "",
			},
		},
	}

	alpha := &CrdbCluster{}
	require.NoError(t, alpha.ConvertFrom(original))
	require.Contains(t, alpha.Annotations, BetaCrdbClusterMigrationStatusAnnotationKey)

	roundTripped := &v1beta1.CrdbCluster{}
	require.NoError(t, alpha.ConvertTo(roundTripped))

	require.NotNil(t, roundTripped.Status.Migration)
	assert.Equal(t, original.Status.Migration.Phase, roundTripped.Status.Migration.Phase)
	assert.Equal(t, original.Status.Migration.Type, roundTripped.Status.Migration.Type)
	assert.Equal(t, original.Status.Migration.TotalPods, roundTripped.Status.Migration.TotalPods)
	assert.Equal(t, original.Status.Migration.CurrentPodIndex, roundTripped.Status.Migration.CurrentPodIndex)
	assert.Equal(t, original.Status.Migration.MigratedNodes, roundTripped.Status.Migration.MigratedNodes)
	assert.Equal(t, original.Status.Migration.Message, roundTripped.Status.Migration.Message)
	require.NotNil(t, roundTripped.Status.Migration.StartTime)
	assert.True(t, original.Status.Migration.StartTime.Equal(roundTripped.Status.Migration.StartTime))
	require.NotNil(t, roundTripped.Status.Migration.LastUpdateTime)
	assert.True(t, original.Status.Migration.LastUpdateTime.Equal(roundTripped.Status.Migration.LastUpdateTime))
}

// TestAdvertiseHostFilteredFromAdditionalArgs verifies that --advertise-host (the legacy
// public operator flag for the gRPC advertise address) is not carried into AdditionalArgs
// during a ConvertFrom round-trip. The enterprise operator generates --advertise-addr from
// first-class fields and --advertise-host in StartFlags would conflict with that.
func TestAdvertiseHostFilteredFromAdditionalArgs(t *testing.T) {
	disabled := v1beta1.Disabled
	betaCluster := &v1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"},
		Spec: v1beta1.CrdbClusterSpec{
			Mode:       &disabled,
			TLSEnabled: true,
			Regions:    []v1beta1.CrdbClusterRegion{{Code: "us-central1", Nodes: 3, Namespace: "default"}},
			Template: v1beta1.CrdbNodeTemplate{
				Spec: v1beta1.CrdbNodeSpec{
					StartFlags: &v1beta1.Flags{
						Upsert: []string{
							"--advertise-host=$(POD_NAME).crdb-1.cockroach-random-2",
							"--custom-flag=keep-me",
						},
					},
				},
			},
		},
	}

	alpha := &CrdbCluster{}
	err := alpha.ConvertFrom(betaCluster)
	require.NoError(t, err)

	for _, arg := range alpha.Spec.AdditionalArgs {
		assert.False(t, strings.HasPrefix(arg, "--advertise-host"),
			"--advertise-host must not appear in AdditionalArgs, got: %q", arg)
	}
	assert.Contains(t, alpha.Spec.AdditionalArgs, "--custom-flag=keep-me",
		"non-operator flags must be preserved in AdditionalArgs")
}

// Fresh v1beta1 cluster conversion tests — these verify the no-op fast path
// that skips annotation injection for clusters that were never part of a migration.

func TestIsNonMigrationCluster(t *testing.T) {
	mode := func(value v1beta1.ReconciliationMode) *v1beta1.ReconciliationMode {
		return &value
	}

	tests := []struct {
		name   string
		mode   *v1beta1.ReconciliationMode
		status *v1beta1.MigrationStatus
		labels map[string]string
		want   bool
	}{
		{name: "nil mode and no migration", want: true},
		{name: "MutableOnly mode", mode: mode(v1beta1.MutableOnly), want: true},
		{name: "CreateOnly mode", mode: mode(v1beta1.CreateOnly), want: true},
		{
			name: "Disabled without skip label is an ordinary v1alpha1 cluster",
			mode: mode(v1beta1.Disabled),
		},
		{
			name:   "Disabled skip-labeled cluster mid-migration",
			mode:   mode(v1beta1.Disabled),
			status: &v1beta1.MigrationStatus{Phase: "PodMigration"},
			labels: map[string]string{CrdbOperatorMigrationLabel: "true"},
		},
		{
			name:   "Disabled skip-labeled cluster before migration starts uses full path",
			mode:   mode(v1beta1.Disabled),
			labels: map[string]string{CrdbOperatorMigrationLabel: "true"},
		},
		{
			name:   "MutableOnly mid-handoff stays on the full path",
			mode:   mode(v1beta1.MutableOnly),
			status: &v1beta1.MigrationStatus{Phase: "Complete"},
		},
		{
			name:   "MutableOnly fully handed off uses the minimal path",
			mode:   mode(v1beta1.MutableOnly),
			status: &v1beta1.MigrationStatus{Phase: "Complete"},
			labels: map[string]string{"crdb.io/migrate": "complete"},
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := newBasicV1Beta1Cluster()
			cluster.Spec.Mode = tt.mode
			cluster.Status.Migration = tt.status
			cluster.Labels = tt.labels

			assert.Equal(t, tt.want, isNonMigrationCluster(cluster))
		})
	}
}

func TestConvertFromFreshV1beta1Cluster(t *testing.T) {
	src := newBasicV1Beta1Cluster()
	src.Labels = map[string]string{CrdbOperatorMigrationLabel: "true"}

	dst := &CrdbCluster{}
	err := dst.ConvertFrom(src)
	require.NoError(t, err)

	assert.Equal(t, true, dst.Spec.TLSEnabled)
	assert.Equal(t, int32(3), dst.Spec.Nodes)
	assert.Equal(t, "cockroachdb/cockroach:v25.4.1", dst.Spec.Image.Name)
	assert.Equal(t, "true", dst.Labels[CrdbOperatorMigrationLabel])
	assert.Equal(t, ActionStatus(Finished).String(), dst.Status.ClusterStatus)
	assert.Empty(t, dst.Status.Conditions)

	// No migration annotations should be present.
	migrationAnnotations := []string{
		BetaCrdbClusterModeAnnotationKey,
		BetaCrdbClusterHubSpecAnnotationKey,
		BetaCrdbClusterRegionsAnnotationKey,
		BetaCrdbClusterRestartDelayAnnotationKey,
		BetaCrdbClusterV1Alpha1FieldsAnnotationKey,
		BetaCrdbClusterMigrationStatusAnnotationKey,
		BetaCrdbClusterNodeRegionCodeAnnotationKey,
		BetaCrdbClusterNodeCloudProviderAnnotationKey,
	}
	for _, key := range migrationAnnotations {
		_, exists := dst.Annotations[key]
		assert.False(t, exists, "migration annotation %q should not be present on fresh cluster", key)
	}
}

func TestConvertFromFreshV1beta1ClusterMinimalFields(t *testing.T) {
	src := newBasicV1Beta1Cluster()
	src.Spec.Template.Spec.PodTemplate = &v1beta1.PodTemplateSpec{
		Metadata: v1beta1.PodMeta{
			Labels: map[string]string{"app": "crdb"},
		},
		Spec: corev1.PodSpec{
			PriorityClassName: "high",
		},
	}

	dst := &CrdbCluster{}
	err := dst.ConvertFrom(src)
	require.NoError(t, err)

	// Fresh path only maps basic fields — PodTemplate details are not mapped
	// because nobody should read non-migration clusters via v1alpha1.
	assert.Equal(t, int32(3), dst.Spec.Nodes)
	assert.Equal(t, "cockroachdb/cockroach:v25.4.1", dst.Spec.Image.Name)
	assert.True(t, dst.Spec.TLSEnabled)
}

func TestConvertFromMigrationClusterPreservesAnnotations(t *testing.T) {
	tests := []struct {
		name   string
		status *v1beta1.MigrationStatus
		labels map[string]string
	}{
		{
			name:   "active migration",
			status: &v1beta1.MigrationStatus{Phase: "Init"},
		},
		{
			name:   "staged before migration status exists",
			labels: map[string]string{CrdbOperatorMigrationLabel: "true"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newBasicV1Beta1Cluster()
			mode := v1beta1.Disabled
			src.Spec.Mode = &mode
			src.Status.Migration = tt.status
			src.Labels = tt.labels

			dst := &CrdbCluster{}
			require.NoError(t, dst.ConvertFrom(src))

			_, hasMode := dst.Annotations[BetaCrdbClusterModeAnnotationKey]
			assert.True(t, hasMode, "migration cluster must have mode annotation")

			_, hasRegions := dst.Annotations[BetaCrdbClusterRegionsAnnotationKey]
			assert.True(t, hasRegions, "migration cluster must have regions annotation")

			// The full v1beta1-spec annotation is no longer written. It remains in the
			// cleanup lists so betaclusterctrl can strip it from older objects.
			_, hasSpec := dst.Annotations[BetaCrdbClusterHubSpecAnnotationKey]
			assert.False(t, hasSpec, "v1beta1-spec annotation must no longer be written")
		})
	}
}

func TestConvertRoundTripPreservesRegionThroughPreMigrationWindow(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	sts := newStatefulSetForTesting()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts).Build()
	SetConversionClient(fakeClient)
	t.Cleanup(func() { SetConversionClient(nil) })

	v1a := newBasicV1Alpha1Cluster()
	v1a.Annotations[BetaCrdbClusterNodeRegionCodeAnnotationKey] = "us-east-1"

	// Start with the Disabled hub created from the public operator object.
	hub := &v1beta1.CrdbCluster{}
	require.NoError(t, v1a.ConvertTo(hub))
	require.NotEmpty(t, hub.Spec.Regions)
	require.Equal(t, "us-east-1", hub.Spec.Regions[0].Code)

	// skip-reconcile arrives before migration status during normal setup
	hub.Labels = map[string]string{CrdbOperatorMigrationLabel: "true"}
	require.Nil(t, hub.Status.Migration)

	// The v1alpha1 view must keep the region through this setup window.
	back := &CrdbCluster{}
	require.NoError(t, back.ConvertFrom(hub))
	require.Equal(t, "us-east-1", back.Annotations[BetaCrdbClusterNodeRegionCodeAnnotationKey])

	// Applying migrate=start writes the v1alpha1 view back through conversion.
	hub2 := &v1beta1.CrdbCluster{}
	require.NoError(t, back.ConvertTo(hub2))
	require.NotEmpty(t, hub2.Spec.Regions)
	require.Equal(t, "us-east-1", hub2.Spec.Regions[0].Code,
		"region must survive the pre-migration conversion round-trip")
}

func TestConvertToNewPublicV1alpha1Cluster(t *testing.T) {
	src := &CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fresh-cluster",
			Namespace: "default",
		},
		Spec: CrdbClusterSpec{
			Nodes:      3,
			TLSEnabled: true,
			Image:      &PodImage{Name: "cockroachdb/cockroach:v26.2.1"},
		},
	}

	dst := &v1beta1.CrdbCluster{}
	err := src.ConvertTo(dst)
	require.NoError(t, err)

	require.NotNil(t, dst.Spec.Mode)
	assert.Equal(t, v1beta1.Disabled, *dst.Spec.Mode)
	assert.True(t, dst.Spec.TLSEnabled)
	require.Len(t, dst.Spec.Regions, 1)
	assert.Equal(t, int32(3), dst.Spec.Regions[0].Nodes)
	assert.Equal(t, "cockroachdb/cockroach:v26.2.1", dst.Spec.Template.Spec.Image)
}

func TestConvertToPublicV1alpha1ClusterWithoutSkipLabel(t *testing.T) {
	src := &CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "migrated-cluster",
			Namespace: "default",
			Annotations: map[string]string{
				BetaCrdbClusterNodeRegionCodeAnnotationKey: "us-east1",
			},
		},
		Spec: CrdbClusterSpec{
			Nodes:      3,
			TLSEnabled: true,
		},
	}

	dst := &v1beta1.CrdbCluster{}
	err := src.ConvertTo(dst)
	require.NoError(t, err)
	require.NotNil(t, dst.Spec.Mode)
	assert.Equal(t, v1beta1.Disabled, *dst.Spec.Mode)
}

func TestRoundTripFreshV1beta1ClusterConvertFromSkipsAnnotations(t *testing.T) {
	original := newBasicV1Beta1Cluster()
	original.Labels = map[string]string{CrdbOperatorMigrationLabel: "true"}

	// v1beta1 → v1alpha1 (ConvertFrom fast path — no annotations injected)
	alpha := &CrdbCluster{}
	err := alpha.ConvertFrom(original)
	require.NoError(t, err)

	assert.Equal(t, original.Spec.TLSEnabled, alpha.Spec.TLSEnabled)
	assert.Equal(t, original.Spec.Regions[0].Nodes, alpha.Spec.Nodes)
	assert.Equal(t, original.Spec.Template.Spec.Image, alpha.Spec.Image.Name)
	assert.Equal(t, ActionStatus(Finished).String(), alpha.Status.ClusterStatus)
	assert.Equal(t, "true", alpha.Labels[CrdbOperatorMigrationLabel])

	// No migration annotations on the v1alpha1 object.
	for _, key := range []string{
		BetaCrdbClusterModeAnnotationKey,
		BetaCrdbClusterHubSpecAnnotationKey,
		BetaCrdbClusterV1Alpha1FieldsAnnotationKey,
		BetaCrdbClusterRegionsAnnotationKey,
	} {
		_, exists := alpha.Annotations[key]
		assert.False(t, exists, "v1alpha1 should not have %q", key)
	}

	// The projection is readable, but it cannot be written back after the
	// source StatefulSet has been removed.
	converted := &v1beta1.CrdbCluster{}
	err = alpha.ConvertTo(converted)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot convert protected v1alpha1 CrdbCluster")
}

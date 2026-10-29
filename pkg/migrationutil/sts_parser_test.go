package migrationutil_test

import (
	"strings"
	"testing"

	"github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/migrationutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func makeSTS(name, namespace string, replicas int32, args []string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.StatefulSetSpec{
			Replicas: new(replicas),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "db", Image: "cockroachdb/cockroach:v24.1.0", Args: args},
					},
				},
			},
		},
	}
}

// TestParseStatefulSet_JoinPortOrdering verifies that the --join flag in StartFlags always
// uses the correct GRPC port regardless of whether --join appears before or after --listen-addr.
// This is a regression test for the single-pass ordering bug where --join baked the initial
// default port before --listen-addr had a chance to update it.
func TestParseStatefulSet_JoinPortOrdering(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		expectedPort int32
		portInJoin   string
	}{
		{
			name: "join precedes listen-addr",
			args: []string{
				"start",
				"--join=mydb-0.mydb.ns.svc.cluster.local:26300,mydb-1.mydb.ns.svc.cluster.local:26300",
				"--listen-addr=:26300",
				"--advertise-addr=mydb-0.mydb.ns.svc.cluster.local:26300",
				"--certs-dir=/cockroach/cockroach-certs",
			},
			expectedPort: 26300,
			portInJoin:   ":26300",
		},
		{
			name: "listen-addr precedes join",
			args: []string{
				"start",
				"--listen-addr=:26300",
				"--join=mydb-0.mydb.ns.svc.cluster.local:26300",
				"--certs-dir=/cockroach/cockroach-certs",
			},
			expectedPort: 26300,
			portInJoin:   ":26300",
		},
		{
			name: "no listen-addr uses default port",
			args: []string{
				"start",
				"--join=mydb-0.mydb.ns.svc.cluster.local:26258",
				"--certs-dir=/cockroach/cockroach-certs",
			},
			expectedPort: 26258,
			portInJoin:   ":26258",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sts := makeSTS("mydb", "ns", 3, tt.args)
			pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
			require.NoError(t, err)

			assert.Equal(t, tt.expectedPort, pc.GRPCPort)

			require.NotNil(t, pc.StartFlags)
			var joinFlag string
			for _, f := range pc.StartFlags.Upsert {
				if strings.HasPrefix(f, "--join=") {
					joinFlag = f
					break
				}
			}
			require.NotEmpty(t, joinFlag, "--join flag must appear in StartFlags.Upsert")
			assert.Contains(t, joinFlag, tt.portInJoin)
		})
	}
}

func TestParseStatefulSet_NilSTS(t *testing.T) {
	pc, err := migrationutil.ParseStatefulSet(nil, "mydb")
	require.NoError(t, err)
	require.NotNil(t, pc)

	// defaults are populated even without a STS
	assert.True(t, pc.TLSEnabled)
	assert.Equal(t, int32(26258), pc.GRPCPort)
	assert.Equal(t, "mydb", pc.ServiceAccountName)
	assert.Equal(t, "mydb-ca-secret", pc.SourceCASecretName)
	assert.Equal(t, "mydb-node-secret", pc.SourceNodeSecretName)
	assert.Equal(t, "mydb-client-secret", pc.SourceClientSecretName)
	assert.Equal(t, "mydb-ca-crt", pc.CAConfigMap)
	assert.Equal(t, migrationutil.DefaultPVCName, pc.PVCTemplateName)
}

func TestParseStatefulSet_InsecureCluster(t *testing.T) {
	sts := makeSTS("mydb", "ns", 3, []string{
		"start",
		"--insecure",
		"--join=mydb-0.mydb.ns.svc.cluster.local:26258",
	})

	pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
	require.NoError(t, err)
	assert.False(t, pc.TLSEnabled)
}

func TestParseStatefulSet_CustomPorts(t *testing.T) {
	sts := makeSTS("mydb", "ns", 3, []string{
		"start",
		"--port=26257",
		"--http-port=8081",
		"--listen-addr=:26300",
		"--sql-addr=:26257",
		"--http-addr=:8081",
		"--join=mydb-0.mydb.ns.svc.cluster.local:26300",
		"--certs-dir=/cockroach/cockroach-certs",
	})

	pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
	require.NoError(t, err)

	assert.Equal(t, int32(26300), pc.GRPCPort, "listen-addr sets GRPCPort")
	assert.Equal(t, int32(26257), pc.SQLPort, "sql-addr overrides --port")
	assert.Equal(t, int32(8081), pc.HTTPPort)
}

// TestParseStatefulSet_Locality verifies locality parsing, including the --locality-file
// edge case where a greedy prefix match would incorrectly treat --locality-file as a
// --locality flag.
func TestParseStatefulSet_Locality(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		expectedLabels []string
		expectedRegion string
	}{
		{
			name: "full locality with region",
			args: []string{
				"start",
				"--locality=region=us-east1,zone=us-east1-b,rack=42",
				"--join=mydb-0.mydb.ns.svc.cluster.local:26258",
				"--certs-dir=/cockroach/cockroach-certs",
			},
			expectedLabels: []string{"region", "zone", "rack"},
			expectedRegion: "us-east1",
		},
		{
			name: "locality without region tier",
			args: []string{
				"start",
				"--locality=datacenter=dc1,rack=r1",
				"--join=mydb-0.mydb.ns.svc.cluster.local:26258",
				"--certs-dir=/cockroach/cockroach-certs",
			},
			expectedLabels: []string{"datacenter", "rack"},
			expectedRegion: "",
		},
		{
			name: "locality-file does not get parsed as locality",
			args: []string{
				"start",
				"--locality-file=/etc/locality",
				"--join=mydb-0.mydb.ns.svc.cluster.local:26258",
				"--certs-dir=/cockroach/cockroach-certs",
			},
			expectedLabels: nil,
			expectedRegion: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sts := makeSTS("mydb", "ns", 3, tt.args)
			pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
			require.NoError(t, err)

			assert.Equal(t, tt.expectedLabels, pc.LocalityLabels)
			assert.Equal(t, tt.expectedRegion, pc.Region)
		})
	}
}

func TestParseStatefulSet_BashWrapperCommand(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "mydb", Namespace: "ns"},
		Spec: appsv1.StatefulSetSpec{
			Replicas: new(int32(3)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "db",
						Image: "cockroachdb/cockroach:v24.1.0",
						Command: []string{
							"/bin/bash", "-ecx",
							"exec /cockroach/cockroach start --join=mydb-0.mydb.ns.svc.cluster.local:26258 --listen-addr=:26258 --insecure",
						},
					}},
				},
			},
		},
	}

	pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
	require.NoError(t, err)
	assert.False(t, pc.TLSEnabled, "insecure flag detected from bash wrapper")
	assert.Equal(t, int32(26258), pc.GRPCPort)
}

func TestParseStatefulSet_PVCTemplateName(t *testing.T) {
	sts := makeSTS("mydb", "ns", 3, []string{"start", "--insecure"})
	sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{
		{ObjectMeta: metav1.ObjectMeta{Name: "custom-data"}},
	}

	pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
	require.NoError(t, err)
	assert.Equal(t, "custom-data", pc.PVCTemplateName)
}

func TestParseStatefulSet_PVCTemplateNameFromDataMount(t *testing.T) {
	sts := makeSTS("mydb", "ns", 3, []string{"start", "--insecure"})
	sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{
		{ObjectMeta: metav1.ObjectMeta{Name: "logsdir"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "data-primary"}},
	}
	sts.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
		{Name: "data-primary", MountPath: "/cockroach/cockroach-data"},
	}

	pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
	require.NoError(t, err)
	assert.Equal(t, "data-primary", pc.PVCTemplateName)
}

func TestParseStatefulSet_PodAttributes(t *testing.T) {
	grace := int64(300)
	sts := makeSTS("mydb", "ns", 3, []string{"start", "--insecure"})
	sts.Spec.Template.Spec.PriorityClassName = "high-priority"
	sts.Spec.Template.Spec.ServiceAccountName = "custom-sa"
	sts.Spec.Template.Spec.TerminationGracePeriodSeconds = &grace
	sts.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{
		{Name: "my-registry-secret"},
	}
	sts.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{
		RunAsUser: new(int64(1000)),
	}

	pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
	require.NoError(t, err)

	assert.Equal(t, "high-priority", pc.PriorityClassName)
	assert.Equal(t, "custom-sa", pc.ServiceAccountName)
	assert.Equal(t, int64(300), *pc.TerminationGracePeriodSeconds)
	require.Len(t, pc.ImagePullSecrets, 1)
	assert.Equal(t, "my-registry-secret", pc.ImagePullSecrets[0].Name)
	require.NotNil(t, pc.PodSecurityContext)
	assert.Equal(t, int64(1000), *pc.PodSecurityContext.RunAsUser)
}

func TestParseStatefulSet_InitContainerFiltering(t *testing.T) {
	sts := makeSTS("mydb", "ns", 3, []string{"start", "--insecure"})
	sts.Spec.Template.Spec.InitContainers = []corev1.Container{
		{Name: "copy-certs", Image: "busybox"},
		{Name: "custom-init", Image: "alpine"},
		{Name: "setup-dirs", Image: "busybox"},
	}

	pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
	require.NoError(t, err)

	// copy-certs is operator-managed and should be filtered out
	require.Len(t, pc.InitContainers, 2)
	assert.Equal(t, "custom-init", pc.InitContainers[0].Name)
	assert.Equal(t, "setup-dirs", pc.InitContainers[1].Name)
}

func TestParseStatefulSet_VolumeAndSecretParsing(t *testing.T) {
	sts := makeSTS("mydb", "ns", 3, []string{"start", "--certs-dir=/cockroach/cockroach-certs"})
	sts.Spec.Template.Spec.Volumes = []corev1.Volume{
		// operator-managed volumes that should be excluded from CustomVolumes
		{Name: "datadir", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "certs", VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "mydb-node-tls"}}},
					{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "mydb-root-client-tls"}}},
				},
			},
		}},
		{Name: "log-config", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: "mydb-log-config"},
		}},
		// custom volume that should be preserved
		{Name: "extra-config", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "app-config"},
			},
		}},
	}

	pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
	require.NoError(t, err)

	assert.Equal(t, "mydb-node-tls", pc.SourceNodeSecretName)
	assert.Equal(t, "mydb-root-client-tls", pc.SourceClientSecretName)
	assert.Equal(t, "mydb-log-config", pc.LoggingConfigMap)

	// only the custom volume should survive filtering
	require.Len(t, pc.CustomVolumes, 1)
	assert.Equal(t, "extra-config", pc.CustomVolumes[0].Name)
}

func TestParseStatefulSet_CASecretFromVolume(t *testing.T) {
	sts := makeSTS("mydb", "ns", 3, []string{"start", "--certs-dir=/cockroach/cockroach-certs"})
	sts.Spec.Template.Spec.Volumes = []corev1.Volume{
		{Name: "ca-certs", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: "custom-ca-secret"},
		}},
	}

	pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
	require.NoError(t, err)
	assert.Equal(t, "custom-ca-secret", pc.SourceCASecretName)
}

func TestParseStatefulSet_CustomVolumeMountFiltering(t *testing.T) {
	sts := makeSTS("mydb", "ns", 3, []string{"start", "--insecure"})
	sts.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
		{Name: "datadir", MountPath: "/cockroach/cockroach-data"},
		{Name: "certs", MountPath: "/cockroach/cockroach-certs"},
		{Name: "certs-secret", MountPath: "/cockroach/certs-secret"},
		{Name: "client-secret", MountPath: "/cockroach/client-secret"},
		{Name: "failoverdir", MountPath: "/cockroach/failover"},
		{Name: "logsdir", MountPath: "/cockroach/logs"},
		{Name: "log-config", MountPath: "/cockroach/log-config"},
		{Name: "datadir-1", MountPath: "/cockroach/cockroach-data-1"},
		{Name: "extra-mount", MountPath: "/mnt/extra"},
	}

	pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
	require.NoError(t, err)

	// only extra-mount should survive
	require.Len(t, pc.CustomVolumeMounts, 1)
	assert.Equal(t, "extra-mount", pc.CustomVolumeMounts[0].Name)
}

func TestParseStatefulSet_EnvVarDeduplication(t *testing.T) {
	sts := makeSTS("mydb", "ns", 3, []string{"start", "--insecure"})
	sts.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: "MY_VAR", Value: "first"},
		{Name: "MY_VAR", Value: "duplicate"},
		{Name: "COCKROACH_HOST", Value: "should-be-removed"},
		{Name: "COCKROACH_RPC_PORT", Value: "26258"},
		{Name: "COCKROACH_SQL_PORT", Value: "26257"},
		{Name: "CUSTOM_VAR", Value: "kept"},
	}

	pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
	require.NoError(t, err)

	require.Len(t, pc.EnvVars, 2)
	assert.Equal(t, "MY_VAR", pc.EnvVars[0].Name)
	assert.Equal(t, "first", pc.EnvVars[0].Value, "first occurrence wins in dedup")
	assert.Equal(t, "CUSTOM_VAR", pc.EnvVars[1].Name)
}

func TestParseStatefulSet_AdditionalFlagsPassthrough(t *testing.T) {
	sts := makeSTS("mydb", "ns", 3, []string{
		"start",
		"--join=mydb-0.mydb.ns.svc.cluster.local:26258",
		"--listen-addr=:26258",
		"--certs-dir=/cockroach/cockroach-certs",
		"--cache=.25",
		"--max-sql-memory=.25",
		"--max-disk-temp-storage=0",
	})

	pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
	require.NoError(t, err)
	require.NotNil(t, pc.StartFlags)

	// operator-managed flags should not appear in Upsert
	for _, f := range pc.StartFlags.Upsert {
		assert.False(t, strings.HasPrefix(f, "--listen-addr="), "listen-addr is operator-managed")
		assert.False(t, strings.HasPrefix(f, "--certs-dir="), "certs-dir is operator-managed")
		assert.False(t, strings.HasPrefix(f, "--advertise-"), "advertise flags are operator-managed")
	}

	// custom flags should pass through
	hasCache := false
	hasSQLMem := false
	for _, f := range pc.StartFlags.Upsert {
		if f == "--cache=.25" {
			hasCache = true
		}
		if f == "--max-sql-memory=.25" {
			hasSQLMem = true
		}
	}
	assert.True(t, hasCache, "cache flag should pass through")
	assert.True(t, hasSQLMem, "max-sql-memory flag should pass through")
}

// TestParseStatefulSet_ClusterDomain verifies that a custom cluster domain is extracted from
// the --join flag and stored in Domain, while the default cluster.local is omitted.
func TestParseStatefulSet_ClusterDomain(t *testing.T) {
	tests := []struct {
		name           string
		joinArg        string
		expectedDomain string
		joinContains   string
	}{
		{
			name:           "custom domain is stored",
			joinArg:        "--join=mydb-0.mydb.ns.svc.custom.domain:26258",
			expectedDomain: "custom.domain",
			joinContains:   "svc.custom.domain:",
		},
		{
			name:           "default cluster.local is omitted",
			joinArg:        "--join=mydb-0.mydb.ns.svc.cluster.local:26258",
			expectedDomain: "",
			joinContains:   "svc.cluster.local:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sts := makeSTS("mydb", "ns", 3, []string{
				"start",
				tt.joinArg,
				"--certs-dir=/cockroach/cockroach-certs",
			})

			pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
			require.NoError(t, err)
			assert.Equal(t, tt.expectedDomain, pc.Domain)

			var joinFlag string
			for _, f := range pc.StartFlags.Upsert {
				if strings.HasPrefix(f, "--join=") {
					joinFlag = f
					break
				}
			}
			require.NotEmpty(t, joinFlag)
			assert.Contains(t, joinFlag, tt.joinContains)
		})
	}
}

// TestParseStatefulSet_WALFailover verifies that --wal-failover is parsed correctly,
// including the disabled case where no spec should be created.
func TestParseStatefulSet_WALFailover(t *testing.T) {
	t.Run("enabled with VCT", func(t *testing.T) {
		sts := makeSTS("mydb", "ns", 3, []string{
			"start",
			"--wal-failover=path=/cockroach/failover",
			"--join=mydb-0.mydb.ns.svc.cluster.local:26258",
			"--certs-dir=/cockroach/cockroach-certs",
		})
		sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{
			{ObjectMeta: metav1.ObjectMeta{Name: "datadir"}},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "failoverdir"},
				Spec: corev1.PersistentVolumeClaimSpec{
					StorageClassName: new("premium-rwo"),
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("50Gi"),
						},
					},
				},
			},
		}

		pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
		require.NoError(t, err)
		require.NotNil(t, pc.WALFailoverSpec)
		assert.Equal(t, "failoverdir", pc.WALFailoverSpec.Name)
		assert.Equal(t, v1beta1.WalEnable, pc.WALFailoverSpec.Status)
		assert.Equal(t, "/cockroach/failover", pc.WALFailoverSpec.Path)
		assert.Equal(t, "50Gi", pc.WALFailoverSpec.Size)
		assert.Equal(t, "premium-rwo", pc.WALFailoverSpec.StorageClassName)
	})

	t.Run("disabled value produces no spec", func(t *testing.T) {
		sts := makeSTS("mydb", "ns", 3, []string{
			"start",
			"--wal-failover=disabled",
			"--join=mydb-0.mydb.ns.svc.cluster.local:26258",
			"--certs-dir=/cockroach/cockroach-certs",
		})
		sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{
			{ObjectMeta: metav1.ObjectMeta{Name: "failoverdir"}},
		}

		pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
		require.NoError(t, err)
		assert.Nil(t, pc.WALFailoverSpec, "disabled WAL failover should not create a spec")
	})
}

func TestParseStatefulSet_Image(t *testing.T) {
	sts := makeSTS("mydb", "ns", 3, []string{"start", "--insecure"})
	sts.Spec.Template.Spec.Containers[0].Image = "us.gcr.io/cockroach-cloud/cockroach:v24.2.5"

	pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
	require.NoError(t, err)
	assert.Equal(t, "us.gcr.io/cockroach-cloud/cockroach:v24.2.5", pc.Image)
}

func TestParseStatefulSet_LogConfigFromConfigMap(t *testing.T) {
	sts := makeSTS("mydb", "ns", 3, []string{"start", "--insecure"})
	sts.Spec.Template.Spec.Volumes = []corev1.Volume{
		{Name: "log-config", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "mydb-logging"},
			},
		}},
	}

	pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
	require.NoError(t, err)
	assert.Equal(t, "mydb-logging", pc.LoggingConfigMap)
}

func TestParseStatefulSet_NoDBContainer(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "mydb", Namespace: "ns"},
		Spec: appsv1.StatefulSetSpec{
			Replicas: new(int32(3)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "sidecar", Image: "nginx"},
					},
				},
			},
		},
	}

	pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
	require.NoError(t, err)
	// no container named "db" means image and flags are empty
	assert.Empty(t, pc.Image)
	assert.Nil(t, pc.StartFlags)
}

func TestParseStatefulSet_SingleContainerFallback(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "mydb", Namespace: "ns"},
		Spec: appsv1.StatefulSetSpec{
			Replicas: new(int32(3)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "cockroach",
							Image: "cockroachdb/cockroach:v24.1.0",
							Args: []string{
								"start",
								"--insecure",
								"--join=mydb-0.mydb.ns.svc.cluster.local:26258",
							},
						},
					},
				},
			},
		},
	}

	pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
	require.NoError(t, err)
	assert.Equal(t, "cockroachdb/cockroach:v24.1.0", pc.Image)
	require.NotNil(t, pc.StartFlags)
	assert.False(t, pc.TLSEnabled)
}

func TestExtractFlags_SpaceSeparatedAndBareFlags(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		expectedFlags []string
		excludedFlags []string
	}{
		{
			name: "bare boolean flag preserved",
			args: []string{
				"start",
				"--accept-sql-without-tls",
				"--certs-dir=/cockroach/cockroach-certs",
			},
			expectedFlags: []string{"--accept-sql-without-tls"},
		},
		{
			name: "space-separated flag combined into key=value",
			args: []string{
				"start",
				"--cache", "4GiB",
				"--max-sql-memory", "2GiB",
				"--certs-dir=/cockroach/cockroach-certs",
			},
			expectedFlags: []string{"--cache=4GiB", "--max-sql-memory=2GiB"},
		},
		{
			name: "shell expression values are skipped",
			args: []string{
				"start",
				"--cache", "$(expr $MEMORY_LIMIT_MIB / 4)MiB",
				"--max-sql-memory", "$(expr $MEMORY_LIMIT_MIB / 4)MiB",
				"--certs-dir=/cockroach/cockroach-certs",
			},
			excludedFlags: []string{"--cache", "--max-sql-memory"},
		},
		{
			name: "shell expression with curly brace syntax skipped",
			args: []string{
				"start",
				"--cache", "${CACHE_SIZE}",
				"--certs-dir=/cockroach/cockroach-certs",
			},
			excludedFlags: []string{"--cache"},
		},
		{
			name: "bare flag followed by another flag",
			args: []string{
				"start",
				"--accept-sql-without-tls",
				"--cache=4GiB",
				"--certs-dir=/cockroach/cockroach-certs",
			},
			expectedFlags: []string{"--accept-sql-without-tls", "--cache=4GiB"},
		},
		{
			name: "bare flag at end of args",
			args: []string{
				"start",
				"--certs-dir=/cockroach/cockroach-certs",
				"--accept-sql-without-tls",
			},
			expectedFlags: []string{"--accept-sql-without-tls"},
		},
		{
			name: "full public operator command with shell expressions and fragmented log JSON",
			args: []string{
				`exec /cockroach/cockroach.sh start --advertise-host=$(POD_NAME).crdb-test.cockroachiswzfx --certs-dir=/cockroach/cockroach-certs/ --http-port=8080 --sql-addr=:26257 --listen-addr=:26258 --log="{"sinks": {"file-groups": {"dev": {"channels": "DEV", "filter": "WARNING"}}}}" --cache $(expr $MEMORY_LIMIT_MIB / 4)MiB --max-sql-memory $(expr $MEMORY_LIMIT_MIB / 4)MiB --join=crdb-test-0.crdb-test.cockroachiswzfx:26258,crdb-test-1.crdb-test.cockroachiswzfx:26258,crdb-test-2.crdb-test.cockroachiswzfx:26258`,
			},
			excludedFlags: []string{"--cache", "--max-sql-memory", "--log", "--advertise-host", "--certs-dir"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sts := makeSTS("mydb", "default", 3, tt.args)
			pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
			require.NoError(t, err)
			require.NotNil(t, pc.StartFlags)

			for _, expected := range tt.expectedFlags {
				assert.Contains(t, pc.StartFlags.Upsert, expected,
					"expected %q in StartFlags.Upsert %v", expected, pc.StartFlags.Upsert)
			}
			for _, excluded := range tt.excludedFlags {
				for _, flag := range pc.StartFlags.Upsert {
					assert.False(t, strings.HasPrefix(flag, excluded+"=") || flag == excluded,
						"expected %q to NOT be in StartFlags.Upsert, found %q", excluded, flag)
				}
			}
		})
	}
}

func TestIsOperatorManagedFlag(t *testing.T) {
	managed := []string{
		"--listen-addr=:26258",
		"--sql-addr=:26257",
		"--http-addr=:8080",
		"--advertise-addr=pod.svc:26258",
		"--advertise-host=pod.svc",
		"--advertise-sql-addr=pod.svc:26257",
		"--certs-dir=/cockroach/cockroach-certs",
		"--join=a.b:26258",
		"--log=file-defaults: {dir: /logs}",
		"--log-config-file=/etc/cockroach/logs.yaml",
		"--log-config-vars=LOGDIR=/cockroach-logs",
		"--wal-failover=path=/failover",
		"--locality=region=us-east1",
		"--locality-file=/etc/locality",
		"--insecure",
		"--logtostderr",
		"--port=26257",
		"--http-port=8080",
	}
	for _, f := range managed {
		assert.True(t, migrationutil.IsOperatorManagedFlag(f), "expected %q to be operator-managed", f)
	}

	unmanaged := []string{
		"--cache=.25",
		"--max-sql-memory=.25",
		"--max-disk-temp-storage=0",
		"--cluster-name=test",
	}
	for _, f := range unmanaged {
		assert.False(t, migrationutil.IsOperatorManagedFlag(f), "expected %q to NOT be operator-managed", f)
	}
}

func TestNormalizeRuntimeTLSConfig(t *testing.T) {
	t.Run("nil context", func(t *testing.T) {
		// should not panic
		migrationutil.NormalizeRuntimeTLSConfig(nil, "mydb")
	})

	t.Run("legacy operator secret names are normalized", func(t *testing.T) {
		pc := &migrationutil.ParsedMigrationContext{
			SourceNodeSecretName:   "mydb-node",
			SourceClientSecretName: "mydb-root-client",
			NodeSecretName:         "mydb-node",
			ClientSecretName:       "mydb-root-client",
		}
		migrationutil.NormalizeRuntimeTLSConfig(pc, "mydb")
		assert.Equal(t, "mydb-node-secret", pc.NodeSecretName)
		assert.Equal(t, "mydb-client-secret", pc.ClientSecretName)
	})

	t.Run("custom cert-manager names are preserved", func(t *testing.T) {
		pc := &migrationutil.ParsedMigrationContext{
			SourceNodeSecretName:   "mydb-node-tls",
			SourceClientSecretName: "mydb-root-client-tls",
			NodeSecretName:         "mydb-node-tls",
			ClientSecretName:       "mydb-root-client-tls",
		}
		migrationutil.NormalizeRuntimeTLSConfig(pc, "mydb")
		assert.Equal(t, "mydb-node-tls", pc.NodeSecretName)
		assert.Equal(t, "mydb-root-client-tls", pc.ClientSecretName)
	})

	t.Run("empty names get normalized to enterprise defaults", func(t *testing.T) {
		pc := &migrationutil.ParsedMigrationContext{}
		migrationutil.NormalizeRuntimeTLSConfig(pc, "mydb")
		assert.Equal(t, "mydb-node-secret", pc.NodeSecretName)
		assert.Equal(t, "mydb-client-secret", pc.ClientSecretName)
		assert.Equal(t, "mydb-ca-crt", pc.CAConfigMap)
	})

	t.Run("source CA secret is propagated", func(t *testing.T) {
		pc := &migrationutil.ParsedMigrationContext{
			SourceCASecretName: "custom-ca",
		}
		migrationutil.NormalizeRuntimeTLSConfig(pc, "mydb")
		assert.Equal(t, "custom-ca", pc.CASecretName)
	})

	t.Run("HTTP secret defaults to client secret", func(t *testing.T) {
		pc := &migrationutil.ParsedMigrationContext{}
		migrationutil.NormalizeRuntimeTLSConfig(pc, "mydb")
		assert.Equal(t, pc.ClientSecretName, pc.HTTPSecretName)
	})
}

func TestParseStatefulSet_DuplicateFlagsDeduped(t *testing.T) {
	sts := makeSTS("mydb", "ns", 3, []string{
		"start",
		"--cache=.25",
		"--cache=.25",
		"--max-sql-memory=.25",
		"--join=mydb-0.mydb.ns.svc.cluster.local:26258",
		"--certs-dir=/cockroach/cockroach-certs",
	})

	pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
	require.NoError(t, err)

	cacheCount := 0
	for _, f := range pc.StartFlags.Upsert {
		if f == "--cache=.25" {
			cacheCount++
		}
	}
	assert.Equal(t, 1, cacheCount, "duplicate flags should be deduped")
}

func TestParseStatefulSet_CertsVolumeWithSecretDirect(t *testing.T) {
	// some STS configs use a direct Secret volume for certs instead of Projected
	sts := makeSTS("mydb", "ns", 3, []string{"start", "--certs-dir=/cockroach/cockroach-certs"})
	sts.Spec.Template.Spec.Volumes = []corev1.Volume{
		{Name: "certs", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: "mydb-direct-cert"},
		}},
	}

	pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
	require.NoError(t, err)
	assert.Equal(t, "mydb-direct-cert", pc.SourceNodeSecretName)
}

// TestParseStatefulSet_CertVolumeScenarios covers the three Helm chart TLS paths:
// self-signer, cert-manager, and provided (BYO) certs. Each path uses different
// volume names and different naming conventions for secrets.
func TestParseStatefulSet_CertVolumeScenarios(t *testing.T) {
	t.Run("self-signer uses certs projected volume with fullname-based secrets", func(t *testing.T) {
		// Helm self-signer mounts a projected "certs" volume containing both
		// node and client secrets derived from the release fullname.
		sts := makeSTS("crdb-test-cockroachdb", "ns", 3, []string{"start", "--certs-dir=/cockroach/cockroach-certs"})
		sts.Spec.Template.Spec.Volumes = []corev1.Volume{
			{Name: "certs", VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{
						{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "crdb-test-cockroachdb-node-secret"}}},
						{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "crdb-test-cockroachdb-client-secret"}}},
					},
				},
			}},
			{Name: "ca-certs", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: "crdb-test-cockroachdb-ca-secret"},
			}},
		}

		pc, err := migrationutil.ParseStatefulSet(sts, "crdb-test-cockroachdb")
		require.NoError(t, err)
		assert.Equal(t, "crdb-test-cockroachdb-node-secret", pc.SourceNodeSecretName)
		assert.Equal(t, "crdb-test-cockroachdb-client-secret", pc.SourceClientSecretName)
		assert.Equal(t, "crdb-test-cockroachdb-ca-secret", pc.SourceCASecretName)
	})

	t.Run("cert-manager uses certs-secret and client-secret projected volumes", func(t *testing.T) {
		// Helm cert-manager mounts "certs-secret" for node and "client-secret"
		// for client. The secret names come from values.yaml and do not include
		// the release prefix. CA is embedded in the projected sources.
		sts := makeSTS("crdb-test-cockroachdb", "ns", 3, []string{"start", "--certs-dir=/cockroach/cockroach-certs"})
		sts.Spec.Template.Spec.Volumes = []corev1.Volume{
			{Name: "certs-secret", VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{
						{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "cockroachdb-node"}}},
					},
				},
			}},
			{Name: "client-secret", VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{
						{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "cockroachdb-root"}}},
					},
				},
			}},
		}

		pc, err := migrationutil.ParseStatefulSet(sts, "crdb-test-cockroachdb")
		require.NoError(t, err)
		assert.Equal(t, "cockroachdb-node", pc.SourceNodeSecretName)
		assert.Equal(t, "cockroachdb-root", pc.SourceClientSecretName)
	})

	t.Run("provided BYO certs use certs-secret and client-secret with custom names", func(t *testing.T) {
		// Users can set custom secret names via values.yaml when providing
		// their own certificates. The chart still uses certs-secret and
		// client-secret volume names.
		sts := makeSTS("mydb", "ns", 3, []string{"start", "--certs-dir=/cockroach/cockroach-certs"})
		sts.Spec.Template.Spec.Volumes = []corev1.Volume{
			{Name: "certs-secret", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: "my-custom-node-tls"},
			}},
			{Name: "client-secret", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: "my-custom-client-tls"},
			}},
			{Name: "ca-certs", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: "my-custom-ca"},
			}},
		}

		pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
		require.NoError(t, err)
		assert.Equal(t, "my-custom-node-tls", pc.SourceNodeSecretName)
		assert.Equal(t, "my-custom-client-tls", pc.SourceClientSecretName)
		assert.Equal(t, "my-custom-ca", pc.SourceCASecretName)
	})

	t.Run("cert-manager without ca-certs volume keeps default CA name", func(t *testing.T) {
		// When cert-manager embeds CA in the projected node volume instead of
		// a separate ca-certs volume, the parser keeps the default CA name.
		sts := makeSTS("crdb-test-cockroachdb", "ns", 3, []string{"start", "--certs-dir=/cockroach/cockroach-certs"})
		sts.Spec.Template.Spec.Volumes = []corev1.Volume{
			{Name: "certs-secret", VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{
						{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "cockroachdb-node"}}},
					},
				},
			}},
		}

		pc, err := migrationutil.ParseStatefulSet(sts, "crdb-test-cockroachdb")
		require.NoError(t, err)
		assert.Equal(t, "cockroachdb-node", pc.SourceNodeSecretName)
		// no ca-certs volume, so CA stays as the default from ParseStatefulSet
		assert.Equal(t, "crdb-test-cockroachdb-ca-secret", pc.SourceCASecretName)
	})

	t.Run("certs-secret and client-secret volumes are excluded from CustomVolumes", func(t *testing.T) {
		sts := makeSTS("mydb", "ns", 3, []string{"start", "--certs-dir=/cockroach/cockroach-certs"})
		sts.Spec.Template.Spec.Volumes = []corev1.Volume{
			{Name: "certs-secret", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: "cockroachdb-node"},
			}},
			{Name: "client-secret", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: "cockroachdb-root"},
			}},
			{Name: "extra-config", VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "app-config"},
				},
			}},
		}

		pc, err := migrationutil.ParseStatefulSet(sts, "mydb")
		require.NoError(t, err)
		require.Len(t, pc.CustomVolumes, 1)
		assert.Equal(t, "extra-config", pc.CustomVolumes[0].Name)
	})
}

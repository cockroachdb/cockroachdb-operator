package manifests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/cloud"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

const testTenantID = "5e0c10f6-5bc7-5be1-bbb7-78cec71d8570"

// basaltNode returns an insecure basalt node. Secure is the production default,
// but most of these tests assert on the store URL and command shape, which is
// easier to read without the certs-dir query. secureBasalt adds the credentials.
func basaltNode(t *testing.T, clusterName string, addrs ...string) *crdbv1beta1.CrdbNode {
	t.Helper()
	node := &crdbv1beta1.CrdbNode{}
	node.Name = clusterName + "-abc"
	node.Labels = map[string]string{BetaCrdbClusterNodeOwnerLabelKey: clusterName}
	node.Spec.DataStore.Basalt = &crdbv1beta1.BasaltStoreSpec{
		ControllerAddrs: addrs,
		TenantID:        testTenantID,
		Insecure:        new(true),
	}
	return node
}

// secureBasalt attaches the TLS references that flip a basalt store to mTLS.
func secureBasalt(spec *crdbv1beta1.BasaltStoreSpec) *crdbv1beta1.BasaltStoreSpec {
	spec.Insecure = nil
	spec.TLS = &crdbv1beta1.BasaltTLSSpec{
		ClientSecretName: "basalt-tenant-client",
		CAConfigMapName:  "basalt-server-ca",
	}
	return spec
}

func TestBasaltConfigFlags(t *testing.T) {
	node := basaltNode(t, "cockroachdb", "10.0.0.2:26256", "10.0.0.3:26256")
	flags := basaltConfigFlags(node)
	require.Equal(t, `"default@$BASALT_CONTROLLER_ADDRS"`, flags["--basalt-cluster"])
	require.Equal(t, `"$BASALT_TENANT_ID"`, flags["--basalt-namespace"])
	// --temp-dir is only set when a scratch PVC backs the local scratch dir.
	_, hasTempDir := flags["--temp-dir"]
	require.False(t, hasTempDir)
	// RSEngine is enabled by default, so --disable-rsengine should not be present.
	_, hasRS := flags["--disable-rsengine"]
	require.False(t, hasRS)
	// --store is not in config flags. It comes from basaltStoreArgs.
	_, hasStore := flags["--store"]
	require.False(t, hasStore)

	// With a scratch PVC configured, --temp-dir points CRDB's disk spills at the
	// scratch mount.
	node.Spec.DataStore.Basalt.ScratchVolumeClaimTemplate = &corev1.PersistentVolumeClaim{}
	require.Equal(t, CockroachDataDirMountAbsPath, basaltConfigFlags(node)["--temp-dir"])
}

func TestBasaltConfigFlags_RSEngineExplicitlyDisabled(t *testing.T) {
	node := basaltNode(t, "cockroachdb", "10.0.0.2:26256")
	node.Spec.DataStore.Basalt.DisableRSEngine = new(true)
	flags := basaltConfigFlags(node)
	_, hasRS := flags["--disable-rsengine"]
	require.True(t, hasRS)
}

// TestBasaltStoreArgs covers store URL generation across store counts and both
// security modes. A secure node differs only by the certs-dir query, and every
// store has to carry the identical one, because CockroachDB rejects a node
// whose basalt stores disagree on their cert paths.
func TestBasaltStoreArgs(t *testing.T) {
	const certs = "?certs-dir=" + BasaltCertsPath

	testCases := []struct {
		name string
		mut  func(*crdbv1beta1.BasaltStoreSpec)
		want []string
	}{
		{
			name: "insecure single store",
			want: []string{`--store="path=basalt://$BASALT_TENANT_ID@default/cockroachdb-abc/store"`},
		},
		{
			name: "insecure multi store",
			mut:  func(s *crdbv1beta1.BasaltStoreSpec) { s.StoreCount = new(int32(3)) },
			want: []string{
				`--store="path=basalt://$BASALT_TENANT_ID@default/cockroachdb-abc/store1"`,
				`--store="path=basalt://$BASALT_TENANT_ID@default/cockroachdb-abc/store2"`,
				`--store="path=basalt://$BASALT_TENANT_ID@default/cockroachdb-abc/store3"`,
			},
		},
		{
			name: "secure single store",
			mut:  func(s *crdbv1beta1.BasaltStoreSpec) { secureBasalt(s) },
			want: []string{`--store="path=basalt://$BASALT_TENANT_ID@default/cockroachdb-abc/store` + certs + `"`},
		},
		{
			name: "secure multi store",
			mut:  func(s *crdbv1beta1.BasaltStoreSpec) { secureBasalt(s).StoreCount = new(int32(2)) },
			want: []string{
				`--store="path=basalt://$BASALT_TENANT_ID@default/cockroachdb-abc/store1` + certs + `"`,
				`--store="path=basalt://$BASALT_TENANT_ID@default/cockroachdb-abc/store2` + certs + `"`,
			},
		},
		{
			// An object that bypassed the webhook has no credentials to point
			// certs-dir at, so it renders an insecure URL and fails to connect
			// rather than failing to render.
			name: "secure without tls renders an insecure url",
			mut:  func(s *crdbv1beta1.BasaltStoreSpec) { s.Insecure = nil },
			want: []string{`--store="path=basalt://$BASALT_TENANT_ID@default/cockroachdb-abc/store"`},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			node := basaltNode(t, "cockroachdb", "10.0.0.2:26256")
			if tc.mut != nil {
				tc.mut(node.Spec.DataStore.Basalt)
			}
			require.Equal(t, tc.want, basaltStoreArgs(node))
		})
	}
}

func TestBasaltStoreFlagStrings_MultiStore(t *testing.T) {
	node := basaltNode(t, "cockroachdb", "10.0.0.2:26256")
	node.Spec.DataStore.Basalt.StoreCount = new(int32(2))
	flags := basaltStoreFlagStrings(node)
	require.Contains(t, flags, `--store="path=basalt://$BASALT_TENANT_ID@default/cockroachdb-abc/store1"`)
	require.Contains(t, flags, `--store="path=basalt://$BASALT_TENANT_ID@default/cockroachdb-abc/store2"`)
	require.Contains(t, flags, `--basalt-cluster="default@$BASALT_CONTROLLER_ADDRS"`)
}

func TestParseStartFlags_Basalt(t *testing.T) {
	node := basaltNode(t, "cockroachdb", "10.0.0.2:26256")
	node.Spec.DataStore.Basalt.ScratchVolumeClaimTemplate = &corev1.PersistentVolumeClaim{}
	cmd := parseStartFlags(node)
	require.Contains(t, cmd, `--basalt-cluster="default@$BASALT_CONTROLLER_ADDRS"`)
	require.Contains(t, cmd, `--basalt-namespace="$BASALT_TENANT_ID"`)
	require.Contains(t, cmd, "--temp-dir="+CockroachDataDirMountAbsPath)
	require.Contains(t, cmd, `--store="path=basalt://$BASALT_TENANT_ID@default/cockroachdb-abc/store"`)
	require.NotContains(t, cmd, "--disable-rsengine")
}

func TestParseStartFlags_BasaltMultiStore(t *testing.T) {
	node := basaltNode(t, "cockroachdb", "10.0.0.2:26256")
	node.Spec.DataStore.Basalt.StoreCount = new(int32(2))
	cmd := parseStartFlags(node)
	require.Contains(t, cmd, `--store="path=basalt://$BASALT_TENANT_ID@default/cockroachdb-abc/store1"`)
	require.Contains(t, cmd, `--store="path=basalt://$BASALT_TENANT_ID@default/cockroachdb-abc/store2"`)
}

// withClusterKey turns on basalt encryption at rest the way the control plane
// does. The key secret names no KMS platform, so the stored value is the raw
// key rather than ciphertext.
func withClusterKey(node *crdbv1beta1.CrdbNode) *crdbv1beta1.CrdbNode {
	secret := "ear-store-us-east-1-v-0"
	node.Spec.EncryptionAtRest = &crdbv1beta1.EncryptionAtRest{
		KeySecretName: &secret,
		Platform:      crdbv1beta1.EncryptionPlatformUnknown,
	}
	return node
}

func TestBasaltConfigFlags_ClusterKey(t *testing.T) {
	t.Run("absent without encryption", func(t *testing.T) {
		flags := basaltConfigFlags(basaltNode(t, "cockroachdb", "10.0.0.2:26256"))
		_, ok := flags["--cluster-key-dir"]
		require.False(t, ok, "unencrypted basalt clusters must render exactly as before")
	})

	t.Run("present with encryption", func(t *testing.T) {
		flags := basaltConfigFlags(withClusterKey(basaltNode(t, "cockroachdb", "10.0.0.2:26256")))
		require.Equal(t, keyMountPath, flags["--cluster-key-dir"])
	})

	// A non-nil EncryptionAtRest with no key secret is how the CRD spells
	// "encryption disabled"; the key volume is not mounted in that case, so the
	// flag would point at nothing.
	t.Run("absent when the key secret is nil", func(t *testing.T) {
		node := basaltNode(t, "cockroachdb", "10.0.0.2:26256")
		node.Spec.EncryptionAtRest = &crdbv1beta1.EncryptionAtRest{}
		flags := basaltConfigFlags(node)
		_, ok := flags["--cluster-key-dir"]
		require.False(t, ok)
	})
}

// The two command builders are separate implementations and the operator still
// supports both, so both are asserted.
// Getting this wrong renders --enterprise-encryption against local scratch on a
// basalt node, which fails only once a pod actually starts.
func TestBasaltClusterKey_BothFlagPaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(*crdbv1beta1.CrdbNode) string
	}{
		{"parseFlags", parseFlags},
		{"parseStartFlags", func(n *crdbv1beta1.CrdbNode) string {
			n.Spec.StartFlags = &crdbv1beta1.Flags{}
			return parseStartFlags(n)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := tc.build(withClusterKey(basaltNode(t, "cockroachdb", "10.0.0.2:26256")))
			require.Contains(t, cmd, "--cluster-key-dir="+keyMountPath)
			require.NotContains(t, cmd, EnterpriseEncryption)
		})
	}
}

// withRotatingClusterKey stages a second key the way a two-phase rotation
// would, with the incoming and outgoing keys present at once.
func withRotatingClusterKey(node *crdbv1beta1.CrdbNode) *crdbv1beta1.CrdbNode {
	node = withClusterKey(node)
	old := "ear-store-us-east-1-v-1"
	node.Spec.EncryptionAtRest.OldKeySecretName = &old
	return node
}

func TestBasaltClusterKey_Rotation(t *testing.T) {
	secretRef := func(env []corev1.EnvVar, name string) (string, bool) {
		for _, e := range env {
			if e.Name == name && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				return e.ValueFrom.SecretKeyRef.Name, true
			}
		}
		return "", false
	}

	node := withRotatingClusterKey(basaltNode(t, "cockroachdb", "10.0.0.2:26256"))

	// Both secrets reach the init container, which writes them side by side into
	// the key directory as current and old.
	c := crdbNodeInitContainers(node)
	require.Len(t, c, 1)

	name, ok := secretRef(c[0].Env, "STORE_KEY")
	require.True(t, ok)
	require.Equal(t, "ear-store-us-east-1-v-0", name)

	name, ok = secretRef(c[0].Env, "OLD_STORE_KEY")
	require.True(t, ok)
	require.Equal(t, "ear-store-us-east-1-v-1", name)

	// The start command is what it is with one key. --cluster-key-dir names the
	// directory, so staging a second key does not change it, and it must not
	// bring --enterprise-encryption back either.
	for _, cmd := range []string{
		parseFlags(withRotatingClusterKey(basaltNode(t, "cockroachdb", "10.0.0.2:26256"))),
		parseStartFlags(func() *crdbv1beta1.CrdbNode {
			n := withRotatingClusterKey(basaltNode(t, "cockroachdb", "10.0.0.2:26256"))
			n.Spec.StartFlags = &crdbv1beta1.Flags{}
			return n
		}()),
	} {
		require.Contains(t, cmd, "--cluster-key-dir="+keyMountPath)
		require.NotContains(t, cmd, EnterpriseEncryption)
	}
}

// Non-basalt stores must be untouched by any of this.
func TestNonBasaltEncryptionAtRestUnchanged(t *testing.T) {
	node := &crdbv1beta1.CrdbNode{}
	node.Name = "cockroachdb-abc"
	secret := "cmek-store-us-east-1-v-0"
	node.Spec.EncryptionAtRest = &crdbv1beta1.EncryptionAtRest{
		KeySecretName: &secret,
		Platform:      crdbv1beta1.EncryptionPlatformAwsKms,
	}
	cmd := parseFlags(node)
	require.Contains(t, cmd, EnterpriseEncryption)
	require.NotContains(t, cmd, "--cluster-key-dir")
}

func TestCrdbNodeInitContainers_BasaltKeyFileNames(t *testing.T) {
	flagValue := func(argv []string, name string) (string, bool) {
		for i, a := range argv {
			if a == name && i+1 < len(argv) {
				return argv[i+1], true
			}
		}
		return "", false
	}

	t.Run("basalt selects current/old", func(t *testing.T) {
		c := crdbNodeInitContainers(withClusterKey(basaltNode(t, "cockroachdb", "10.0.0.2:26256")))
		require.Len(t, c, 1)

		v, ok := flagValue(c[0].Command, "-new-store-key-file")
		require.True(t, ok)
		require.Equal(t, BasaltClusterKeyCurrentFile, v)

		v, ok = flagValue(c[0].Command, "-old-store-key-file")
		require.True(t, ok)
		require.Equal(t, BasaltClusterKeyOldFile, v)
	})

	t.Run("non-basalt keeps the defaults", func(t *testing.T) {
		node := &crdbv1beta1.CrdbNode{}
		node.Name = "cockroachdb-abc"
		secret := "cmek-store-us-east-1-v-0"
		node.Spec.EncryptionAtRest = &crdbv1beta1.EncryptionAtRest{
			KeySecretName: &secret,
			Platform:      crdbv1beta1.EncryptionPlatformAwsKms,
		}
		c := crdbNodeInitContainers(node)
		require.Len(t, c, 1)
		require.NotContains(t, c[0].Command, "-new-store-key-file")
	})
}

func TestNodeVolumes_BasaltUsesEmptyDir(t *testing.T) {
	spec := &crdbv1beta1.CrdbNodeSpec{}
	spec.DataStore.Basalt = &crdbv1beta1.BasaltStoreSpec{ControllerAddrs: []string{"10.0.0.2:26256"}}
	vols := nodeVolumes("node-1", cloud.GCP, spec, "cockroachdb")
	var dataVol *corev1.Volume
	for i := range vols {
		if vols[i].Name == CockroachDataDirVolumeName {
			dataVol = &vols[i]
		}
	}
	require.NotNil(t, dataVol)
	require.NotNil(t, dataVol.EmptyDir, "basalt data dir should be an emptyDir scratch volume")
	require.Nil(t, dataVol.PersistentVolumeClaim)
}

func TestCrdbNodeInitContainers_NoBasaltContainer(t *testing.T) {
	// The control plane creates the basalt tenant before the cluster is applied,
	// so basalt nodes must not gain an extra init container.
	node := basaltNode(t, "cockroachdb", "10.0.0.2:26256")
	plain := &crdbv1beta1.CrdbNode{}
	require.Equal(t, len(crdbNodeInitContainers(plain)), len(crdbNodeInitContainers(node)))
}

func TestBasaltCLIImage(t *testing.T) {
	spec := &crdbv1beta1.BasaltStoreSpec{}
	require.Equal(t, defaultBasaltCLIImage, BasaltCLIImage(spec))
	spec.CLIImage = "example.com/basalt-cli:custom"
	require.Equal(t, "example.com/basalt-cli:custom", BasaltCLIImage(spec))
}

func TestBasaltEnvVars(t *testing.T) {
	spec := &crdbv1beta1.BasaltStoreSpec{
		ControllerAddrs: []string{"10.0.0.2:26256", "10.0.0.3:26256"},
		TenantID:        "5e0c10f6-5bc7-5be1-bbb7-78cec71d8570",
	}
	got := map[string]string{}
	for _, e := range BasaltEnvVars(spec) {
		got[e.Name] = e.Value
	}
	require.Equal(t, "10.0.0.2:26256,10.0.0.3:26256", got["BASALT_CONTROLLER_ADDRS"])
	require.Equal(t, "5e0c10f6-5bc7-5be1-bbb7-78cec71d8570", got["BASALT_TENANT_ID"])
}

func TestBasaltStoreCleanupShellCommand(t *testing.T) {
	// The store-local subtree is removed with a single recursive delete on the
	// node directory, and each range-shared s<store-id> dir is removed on its own.
	spec := basaltNode(t, "cockroachdb", "10.0.0.2:26256").Spec.DataStore.Basalt
	cmd := BasaltStoreCleanupShellCommand(spec, "cockroachdb-abc", []int64{5, 6})
	require.Contains(t, cmd, `basalt rm -r "//$BASALT_TENANT_ID@$BASALT_CONTROLLER_ADDRS/$BASALT_TENANT_ID/cockroachdb-abc"`)
	require.Contains(t, cmd, `basalt rm -r "//$BASALT_TENANT_ID@$BASALT_CONTROLLER_ADDRS/$BASALT_TENANT_ID/s5"`)
	require.Contains(t, cmd, `basalt rm -r "//$BASALT_TENANT_ID@$BASALT_CONTROLLER_ADDRS/$BASALT_TENANT_ID/s6"`)
	require.Contains(t, cmd, `grep -qi "no such object or directory"`)
	// One remove for the node dir plus one per store id, and no leftover rmdir.
	require.Equal(t, 3, strings.Count(cmd, "basalt rm -r"))
	require.NotContains(t, cmd, "basalt rmdir")
}

func TestBasaltStoreCleanupShellCommand_NoStoreIDs(t *testing.T) {
	// A node that never recorded store ids (the cluster delete path) still
	// reclaims its store-local dir. The range-shared dirs fall to the recursive
	// removal the control plane does when it deletes the tenant.
	spec := basaltNode(t, "cockroachdb", "10.0.0.2:26256").Spec.DataStore.Basalt
	cmd := BasaltStoreCleanupShellCommand(spec, "cockroachdb-abc", nil)
	require.Contains(t, cmd, `basalt rm -r "//$BASALT_TENANT_ID@$BASALT_CONTROLLER_ADDRS/$BASALT_TENANT_ID/cockroachdb-abc"`)
	require.Equal(t, 1, strings.Count(cmd, "basalt rm -r"))
}

func TestBasaltStoreCleanupShellCommand_Secure(t *testing.T) {
	spec := secureBasalt(basaltNode(t, "cockroachdb", "10.0.0.2:26256").Spec.DataStore.Basalt)
	cmd := BasaltStoreCleanupShellCommand(spec, "cockroachdb-abc", []int64{5})
	require.Contains(t, cmd,
		`basalt rm -r "//$BASALT_TENANT_ID@$BASALT_CONTROLLER_ADDRS/$BASALT_TENANT_ID/cockroachdb-abc?certs-dir=/cockroach/basalt-certs"`)
	require.Contains(t, cmd,
		`basalt rm -r "//$BASALT_TENANT_ID@$BASALT_CONTROLLER_ADDRS/$BASALT_TENANT_ID/s5?certs-dir=/cockroach/basalt-certs"`)
}

func TestBasaltStoreCleanupJobName(t *testing.T) {
	require.Equal(t, "node-abc-basalt-cleanup", BasaltStoreCleanupJobName("node-abc"))
	long := strings.Repeat("a", 60)
	require.LessOrEqual(t, len(BasaltStoreCleanupJobName(long)), 63)
	require.NotEqual(t,
		BasaltStoreCleanupJobName(strings.Repeat("a", 59)+"x"),
		BasaltStoreCleanupJobName(strings.Repeat("a", 59)+"y"),
	)
}

func TestBuildBasaltCleanupJob(t *testing.T) {
	node := basaltNode(t, "cockroachdb", "10.0.0.2:26256")
	node.Labels["network"] = "basalt"
	node.Annotations = map[string]string{"sidecar.example.com/inject": "false"}
	node.Status.StoreIDs = []int64{7}
	spec := node.Spec.DataStore.Basalt
	job := BuildBasaltCleanupJob(node, spec, node.Labels, node.Annotations)
	require.Equal(t, BasaltStoreCleanupJobName(node.Name), job.Name)
	require.Equal(t, "basalt-store-cleanup", job.Labels["app.kubernetes.io/component"])
	require.Equal(t, string(node.UID), job.Annotations[BasaltLifecycleResourceUIDAnnotation])
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	require.Equal(t, "cleanup-basalt-stores", job.Spec.Template.Spec.Containers[0].Name)
	require.Equal(t, "basalt", job.Spec.Template.Labels["network"])
	require.Equal(t, "false", job.Spec.Template.Annotations["sidecar.example.com/inject"])
	// The Job command reclaims the node dir and the range-shared dir for the
	// store id recorded on the node.
	args := job.Spec.Template.Spec.Containers[0].Args
	require.Len(t, args, 1)
	require.Contains(t, args[0], `basalt rm -r "//$BASALT_TENANT_ID@$BASALT_CONTROLLER_ADDRS/$BASALT_TENANT_ID/cockroachdb-abc"`)
	require.Contains(t, args[0], `basalt rm -r "//$BASALT_TENANT_ID@$BASALT_CONTROLLER_ADDRS/$BASALT_TENANT_ID/s7"`)
	// An insecure store has nothing to project, so the Job pod carries no volumes.
	require.Empty(t, job.Spec.Template.Spec.Volumes)
	require.Empty(t, job.Spec.Template.Spec.Containers[0].VolumeMounts)
}

// TestBuildBasaltCleanupJob_Secure checks the Job pod gets the certs volume the
// CLI needs. Without the mount the command's certs-dir would point at nothing.
func TestBuildBasaltCleanupJob_Secure(t *testing.T) {
	node := basaltNode(t, "cockroachdb", "10.0.0.2:26256")
	spec := secureBasalt(node.Spec.DataStore.Basalt)
	job := BuildBasaltCleanupJob(node, spec, node.Labels, nil)

	require.Len(t, job.Spec.Template.Spec.Volumes, 1)
	vol := job.Spec.Template.Spec.Volumes[0]
	require.Equal(t, "basalt-certs", vol.Name)
	require.Equal(t, []corev1.VolumeMount{{
		Name:      "basalt-certs",
		MountPath: BasaltCertsPath,
		ReadOnly:  true,
	}}, job.Spec.Template.Spec.Containers[0].VolumeMounts)
	require.Contains(t, job.Spec.Template.Spec.Containers[0].Args[0], "?certs-dir="+BasaltCertsPath)
}

// TestBasaltCertsVolume checks the projected filenames, which basalt derives
// from certs-dir and the tenant in the URL rather than reading them from a flag.
func TestBasaltCertsVolume(t *testing.T) {
	spec := secureBasalt(basaltNode(t, "cockroachdb", "10.0.0.2:26256").Spec.DataStore.Basalt)
	vol := BasaltCertsVolume(spec)

	require.Equal(t, "basalt-certs", vol.Name)
	require.NotNil(t, vol.Projected)
	require.Len(t, vol.Projected.Sources, 2)

	secret := vol.Projected.Sources[0].Secret
	require.NotNil(t, secret)
	require.Equal(t, "basalt-tenant-client", secret.Name)
	require.Equal(t, []corev1.KeyToPath{
		{Key: "tls.crt", Path: "tenant." + testTenantID + ".crt"},
		{Key: "tls.key", Path: "tenant." + testTenantID + ".key"},
	}, secret.Items)

	cm := vol.Projected.Sources[1].ConfigMap
	require.NotNil(t, cm)
	require.Equal(t, "basalt-server-ca", cm.Name)
	require.Equal(t, []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}, cm.Items)
}

// TestCrdbNodePod_BasaltCerts walks the four ways the two PKI domains can
// combine on a pod. They are independent, so the certs-dir that the --store
// query points at has to be mounted whenever basalt is secure, whatever the
// CockroachDB side is doing. A pod carrying the query without the mount comes
// up fine and only then fails to reach basalt.
func TestCrdbNodePod_BasaltCerts(t *testing.T) {
	for _, tc := range []struct {
		name              string
		tlsEnabled        bool
		secureBasalt      bool
		wantReloaderPaths []string
	}{
		{
			name:              "both secure",
			tlsEnabled:        true,
			secureBasalt:      true,
			wantReloaderPaths: []string{CockroachCertsPath, BasaltCertsPath},
		},
		{
			name:              "insecure cockroach with secure basalt",
			secureBasalt:      true,
			wantReloaderPaths: []string{BasaltCertsPath},
		},
		{
			name:              "secure cockroach with insecure basalt",
			tlsEnabled:        true,
			wantReloaderPaths: []string{CockroachCertsPath},
		},
		{
			name: "both insecure",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := basaltNode(t, "cockroachdb", "10.0.0.2:26256")
			node.Spec.TLSEnabled = tc.tlsEnabled
			if tc.secureBasalt {
				secureBasalt(node.Spec.DataStore.Basalt)
			}
			pod := CrdbNodePod(node, &corev1.Node{})

			hasBasaltVolume := false
			for _, v := range pod.Spec.Volumes {
				if v.Name == basaltCertsVolumeName {
					hasBasaltVolume = true
				}
			}
			require.Equal(t, tc.secureBasalt, hasBasaltVolume, "basalt certs volume on the pod")

			crdb := podContainer(t, pod, CRDBContainerName)
			require.Equal(t, tc.secureBasalt, hasMountPath(crdb.VolumeMounts, BasaltCertsPath),
				"basalt certs mount on the cockroach container")

			// The reloader signals the cockroach process by name, so a pod that has
			// one must share its process namespace.
			require.Equal(t, len(tc.wantReloaderPaths) > 0, *pod.Spec.ShareProcessNamespace)

			reloader := findContainer(pod, "cert-reloader")
			if len(tc.wantReloaderPaths) == 0 {
				require.Nil(t, reloader, "no certs to watch, so no reloader")
				return
			}
			require.NotNil(t, reloader)
			require.Equal(t, append([]string{CockroachProcessName}, tc.wantReloaderPaths...), reloader.Args)
			var mountPaths []string
			for _, m := range reloader.VolumeMounts {
				mountPaths = append(mountPaths, m.MountPath)
			}
			require.Equal(t, tc.wantReloaderPaths, mountPaths,
				"the reloader can only watch a directory it mounts")
		})
	}
}

func findContainer(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

func podContainer(t *testing.T, pod *corev1.Pod, name string) *corev1.Container {
	t.Helper()
	c := findContainer(pod, name)
	require.NotNil(t, c, "container %q not found on the pod", name)
	return c
}

func hasMountPath(mounts []corev1.VolumeMount, path string) bool {
	for _, m := range mounts {
		if m.MountPath == path {
			return true
		}
	}
	return false
}

// runBasaltShell executes a generated basalt command under /bin/sh with the
// basalt CLI stubbed by a shell function that records each invocation's
// arguments. It returns the recorded calls, one per line, so tests can verify
// every chained step actually ran (a mid-chain "exit 0" would short-circuit the
// rest and this would catch it).
func runBasaltShell(t *testing.T, cmd string) string {
	t.Helper()
	outFile := filepath.Join(t.TempDir(), "calls")
	script := `basalt() { printf '%s\n' "$*" >> "$OUT"; }; ` + cmd
	c := exec.Command("/bin/sh", "-c", script)
	c.Env = append(os.Environ(),
		"OUT="+outFile,
		"BASALT_CONTROLLER_ADDRS=addrs",
		"BASALT_TENANT_ID=tenant",
	)
	require.NoError(t, c.Run())
	data, err := os.ReadFile(outFile)
	require.NoError(t, err)
	return string(data)
}

// TestBasaltStoreCleanupShellCommand_RunsEveryRemoval executes the chained
// cleanup command and confirms the node dir and every range-shared store dir are
// removed. Guards against a short-circuiting "exit 0" between chained snippets.
func TestBasaltStoreCleanupShellCommand_RunsEveryRemoval(t *testing.T) {
	spec := basaltNode(t, "cockroachdb", "10.0.0.2:26256").Spec.DataStore.Basalt
	calls := runBasaltShell(t, BasaltStoreCleanupShellCommand(spec, "node-x", []int64{5, 6}))
	require.Contains(t, calls, "tenant/node-x")
	require.Contains(t, calls, "tenant/s5")
	require.Contains(t, calls, "tenant/s6")
}

// TestBuildBasaltCleanupJob_CopyAnnotationMap guards against the Job builder
// sharing the caller's annotation map. If it did, the API client writing
// metadata back on create would mutate the CrdbNode in memory.
func TestBuildBasaltCleanupJob_CopyAnnotationMap(t *testing.T) {
	node := basaltNode(t, "cockroachdb", "10.0.0.2:26256")
	spec := node.Spec.DataStore.Basalt

	src := map[string]string{"keep": "me"}
	job := BuildBasaltCleanupJob(node, spec, nil, src)
	job.Annotations["added-on-create"] = "x"
	_, leaked := src["added-on-create"]
	require.False(t, leaked, "cleanup Job shares the caller's annotation map")
}

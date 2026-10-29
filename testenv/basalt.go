package testenv

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	basaltFixtureName = "basalt-test-storage"

	basaltFixtureArtifact = "https://storage.googleapis.com/basalt-public-releases/" +
		"basalt/v0.1.18/basalt-v0.1.18.linux-${arch}.tar.gz"
	basaltFixtureMetadbImage    = "cockroachdb/cockroach:v26.2.6"
	basaltFixtureCockroachImage = "us-docker.pkg.dev/releases-prod/" +
		"cockroachdb-basalt-releases/cockroach:v26.3.3-beta.0-121442-g0fcc957eea-basalt"

	basaltFixtureDownloaderImage = "busybox:1.36.1"
	basaltFixtureCertsDir        = "/basalt-certs"
	basaltFixtureBinariesDir     = "/basalt-binaries"
	basaltFixtureControllerPort  = 26256
	basaltFixtureBlobControlPort = 26254
	basaltFixtureBlobDataPort    = 26255
	basaltFixtureBlobHTTPPort    = 8081
	basaltFixtureBlobPortStride  = 10
	// Basalt requires two available replicas, so the fixture runs three blob servers.
	basaltFixtureBlobCount    = 3
	basaltFixtureZone         = "us-east-1a"
	basaltFixtureTenantSecret = "basalt-test-tenant-client"
	basaltFixtureCAConfigMap  = "basalt-test-ca"
)

// BasaltCluster is a single-controller, three-blob Basalt cluster for
// integration tests. It downloads published server binaries into a
// Pod and uses the production Basalt CLI image for tenant lifecycle operations.
// It does not import roachmgr or any cloud SDK.
type BasaltCluster struct {
	ctl           *kube.Ctl
	tenantID      string
	secure        bool
	tenantDeleted bool
}

// CreateBasaltCluster starts a Basalt cluster and creates tenantID. In secure
// mode it also installs the tenant client certificate and CA for the operator
// to project. The supplied Ctl must have a namespace.
func CreateBasaltCluster(
	t *testing.T, ctl *kube.Ctl, tenantID string, secure bool,
) *BasaltCluster {
	t.Helper()
	require.NotEmpty(t, ctl.Namespace(), "Basalt cluster requires a namespaced Ctl")
	writeArtifactsOnFailed(t)

	basalt := &BasaltCluster{
		ctl:      ctl,
		tenantID: tenantID,
		secure:   secure,
	}
	ctx := context.Background()
	service, pod := basalt.resources()
	require.NoError(t, ctl.ApplyWithOptions(ctx, kube.ApplyOptions{NoWait: true}, service, pod))
	require.NoError(t, ctl.WaitForWithOptions(ctx, kube.WaitForOptions{
		DoneFunc: kube.IsReady,
		BackOff:  kube.NewCtlDefaultBackOffFn(15 * time.Minute),
	}, pod), "Basalt cluster did not become ready")

	t.Cleanup(func() { basalt.cleanup(t) })
	require.NoError(t, basalt.createTenant(ctx))
	if secure {
		basalt.installTenantCredentials(t)
	}
	return basalt
}

// ConfigureCluster points a CrdbCluster at the Basalt cluster and configures
// its Pods to use the pinned Basalt-enabled CockroachDB image.
func (f *BasaltCluster) ConfigureCluster(cluster *crdbv1beta1.CrdbCluster) {
	spec := &cluster.Spec.Template.Spec
	insecure := !f.secure
	basaltSpec := &crdbv1beta1.BasaltStoreSpec{
		ControllerAddrs: []string{f.controllerAddress()},
		TenantID:        f.tenantID,
		Insecure:        &insecure,
	}
	if f.secure {
		basaltSpec.TLS = &crdbv1beta1.BasaltTLSSpec{
			ClientSecretName: basaltFixtureTenantSecret,
			CAConfigMapName:  basaltFixtureCAConfigMap,
		}
	}
	spec.Image = basaltFixtureCockroachImage
	spec.DataStore = crdbv1beta1.DataStore{Basalt: basaltSpec}
	// Keep the CockroachDB locality identical to the Basalt blob-server zone.
	spec.NoCloudPrefixedLocalities = true

	if spec.PodTemplate == nil {
		spec.PodTemplate = &crdbv1beta1.PodTemplateSpec{}
	}
	podSpec := &spec.PodTemplate.Spec
	if podSpec.ServiceAccountName == "" {
		podSpec.ServiceAccountName = cluster.Name
	}
	if podSpec.NodeSelector == nil {
		podSpec.NodeSelector = make(map[string]string)
	}
	podSpec.NodeSelector[corev1.LabelTopologyZone] = basaltFixtureZone
	if podSpec.Affinity == nil {
		// A k3d integration cluster has only one node per zone, so use soft
		// anti-affinity to allow the multi-node test to schedule in one zone.
		podSpec.Affinity = &corev1.Affinity{
			PodAntiAffinity: &corev1.PodAntiAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
					{
						Weight: 100,
						PodAffinityTerm: corev1.PodAffinityTerm{
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"app": "cockroachdb"},
							},
							TopologyKey: corev1.LabelHostname,
						},
					},
				},
			},
		}
	}
}

// DeleteTenant performs the tenant-wide portion of Basalt teardown. Production
// delegates namespace removal and tenant unregistration to managed-service
// through roachmgr's BasaltTenantDelete RPC. This fixture does not call
// roachmgr, so it runs the equivalent basalt rm -r and basalt tenant delete
// commands directly.
func (f *BasaltCluster) DeleteTenant(t *testing.T) {
	t.Helper()
	require.NoError(t, f.removeTenantNamespace(context.Background()))
	require.NoError(t, f.deleteTenant(context.Background()))
	f.tenantDeleted = true
}

func (f *BasaltCluster) resources() (*corev1.Service, *corev1.Pod) {
	labels := map[string]string{"app.kubernetes.io/name": basaltFixtureName}
	serviceFQDN := fmt.Sprintf("%s.%s.svc.cluster.local", basaltFixtureName, f.ctl.Namespace())
	cliImage := manifests.BasaltCLIImage(&crdbv1beta1.BasaltStoreSpec{})
	binariesMount := corev1.VolumeMount{Name: "binaries", MountPath: basaltFixtureBinariesDir}
	certsMount := corev1.VolumeMount{Name: "certs", MountPath: basaltFixtureCertsDir}
	volumes := []corev1.Volume{
		{Name: "binaries", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "metadb", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	initContainers := []corev1.Container{{
		Name:            "download-binaries",
		Image:           basaltFixtureDownloaderImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/bin/sh", "-ceu"},
		Args:            []string{downloadBasaltServerCommand()},
		VolumeMounts:    []corev1.VolumeMount{binariesMount},
	}}
	controllerArgs := []string{
		"serve", "postgresql://root@localhost:26257/defaultdb?sslmode=disable",
		fmt.Sprintf("--listen-addr=0.0.0.0:%d", basaltFixtureControllerPort),
		fmt.Sprintf("--advertise-addr=%s:%d", serviceFQDN, basaltFixtureControllerPort),
		"--http-addr=0.0.0.0:8079",
	}
	controllerMounts := []corev1.VolumeMount{binariesMount}
	var certCommands []string
	if f.secure {
		volumes = append(volumes,
			corev1.Volume{Name: "certs", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		certCommands = append(certCommands, fmt.Sprintf(`
basalt cert create-ca --certs-dir=%[1]s --ca-key=%[1]s/ca.key
basalt cert create-node --certs-dir=%[1]s --ca-key=%[1]s/ca.key \
  node %[2]s %[3]s localhost 127.0.0.1
basalt cert create-tenant --certs-dir=%[1]s --ca-key=%[1]s/ca.key admin
basalt cert create-tenant --certs-dir=%[1]s --ca-key=%[1]s/ca.key "$BASALT_TEST_TENANT_ID"
`, basaltFixtureCertsDir, basaltFixtureName, serviceFQDN))
		controllerArgs = append(controllerArgs,
			"--certs-dir="+basaltFixtureCertsDir, "--init-tenant=admin")
		controllerMounts = append(controllerMounts, certsMount)
	} else {
		controllerArgs = append(controllerArgs, "--insecure")
	}

	servicePorts := []corev1.ServicePort{{Name: "controller", Port: basaltFixtureControllerPort}}
	var blobContainers []corev1.Container
	for i := range basaltFixtureBlobCount {
		blobNumber := i + 1
		controlPort := int32(basaltFixtureBlobControlPort + i*basaltFixtureBlobPortStride)
		dataPort := int32(basaltFixtureBlobDataPort + i*basaltFixtureBlobPortStride)
		httpPort := int32(basaltFixtureBlobHTTPPort + i)
		name := fmt.Sprintf("blob-%d", blobNumber)
		dataVolumeName := name + "-data"
		certsDir := fmt.Sprintf("%s/%s", basaltFixtureCertsDir, name)

		volumes = append(volumes, corev1.Volume{
			Name: dataVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
		servicePorts = append(servicePorts,
			corev1.ServicePort{Name: name + "-control", Port: controlPort},
			corev1.ServicePort{Name: name + "-data", Port: dataPort},
		)

		args := []string{
			"serve", "--data-dir=/blob-data",
			fmt.Sprintf("--listen-control-addr=0.0.0.0:%d", controlPort),
			fmt.Sprintf("--listen-data-addr=0.0.0.0:%d", dataPort),
			fmt.Sprintf("--advertise-control-addr=%s:%d", serviceFQDN, controlPort),
			fmt.Sprintf("--advertise-data-addr=%s:%d", serviceFQDN, dataPort),
			fmt.Sprintf("--http-addr=0.0.0.0:%d", httpPort),
			fmt.Sprintf("--controller-addr=localhost:%d", basaltFixtureControllerPort),
			"--zone=" + basaltFixtureZone,
		}
		mounts := []corev1.VolumeMount{
			binariesMount,
			{Name: dataVolumeName, MountPath: "/blob-data"},
		}
		if f.secure {
			certCommands = append(certCommands, fmt.Sprintf(`
mkdir -p %[1]s
cp %[2]s/ca.crt %[1]s/ca.crt
basalt cert create-blob --certs-dir=%[1]s --ca-key=%[2]s/ca.key \
  %[3]s localhost 127.0.0.1
`, certsDir, basaltFixtureCertsDir, serviceFQDN))
			args = append(args, "--certs-dir="+certsDir)
			mounts = append(mounts, certsMount)
		} else {
			args = append(args, "--insecure")
		}

		blobContainers = append(blobContainers, corev1.Container{
			Name:            name,
			Image:           cliImage,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{basaltFixtureBinariesDir + "/basalt/basalt-blob"},
			Args:            args,
			Ports: []corev1.ContainerPort{
				{Name: "control", ContainerPort: controlPort},
				{Name: "data", ContainerPort: dataPort},
			},
			ReadinessProbe: tcpReadinessProbe(controlPort),
			Resources:      basaltFixtureResources("100m", "128Mi", "500m", "512Mi"),
			VolumeMounts:   mounts,
		})
	}
	if f.secure {
		initContainers = append(initContainers, corev1.Container{
			Name:            "generate-certs",
			Image:           cliImage,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{"/bin/sh", "-ceu"},
			Args:            []string{strings.Join(certCommands, "\n")},
			Env:             []corev1.EnvVar{{Name: "BASALT_TEST_TENANT_ID", Value: f.tenantID}},
			VolumeMounts:    []corev1.VolumeMount{certsMount},
		})
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      basaltFixtureName,
			Namespace: f.ctl.Namespace(),
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports:    servicePorts,
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      basaltFixtureName,
			Namespace: f.ctl.Namespace(),
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:  corev1.RestartPolicyAlways,
			Volumes:        volumes,
			InitContainers: initContainers,
			Containers: append([]corev1.Container{
				{
					Name:            "metadb",
					Image:           basaltFixtureMetadbImage,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         []string{"/cockroach/cockroach"},
					Args: []string{
						"start-single-node", "--insecure",
						"--listen-addr=0.0.0.0:26257", "--http-addr=0.0.0.0:8080",
						"--store=/metadb", "--cache=64MiB", "--max-sql-memory=256MiB",
						"--logtostderr=INFO",
					},
					Ports:          []corev1.ContainerPort{{Name: "metadb-sql", ContainerPort: 26257}},
					ReadinessProbe: tcpReadinessProbe(26257),
					Resources:      basaltFixtureResources("250m", "512Mi", "1", "1Gi"),
					VolumeMounts: []corev1.VolumeMount{
						{Name: "metadb", MountPath: "/metadb"},
					},
				},
				{
					Name:            "controller",
					Image:           cliImage,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         []string{basaltFixtureBinariesDir + "/basalt/basalt-controller"},
					Args:            controllerArgs,
					Ports:           []corev1.ContainerPort{{Name: "controller", ContainerPort: basaltFixtureControllerPort}},
					ReadinessProbe:  tcpReadinessProbe(basaltFixtureControllerPort),
					Resources:       basaltFixtureResources("100m", "128Mi", "500m", "512Mi"),
					VolumeMounts:    controllerMounts,
				},
			}, blobContainers...),
		},
	}
	return service, pod
}

func (f *BasaltCluster) installTenantCredentials(t *testing.T) {
	t.Helper()
	cert := f.readFile(t, fmt.Sprintf("%s/tenant.%s.crt", basaltFixtureCertsDir, f.tenantID))
	key := f.readFile(t, fmt.Sprintf("%s/tenant.%s.key", basaltFixtureCertsDir, f.tenantID))
	ca := f.readFile(t, basaltFixtureCertsDir+"/ca.crt")

	ctx := context.Background()
	require.NoError(t, f.ctl.Apply(ctx,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: basaltFixtureTenantSecret, Namespace: f.ctl.Namespace()},
			Type:       corev1.SecretTypeTLS,
			Data: map[string][]byte{
				corev1.TLSCertKey:       cert,
				corev1.TLSPrivateKeyKey: key,
			},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: basaltFixtureCAConfigMap, Namespace: f.ctl.Namespace()},
			Data:       map[string]string{"ca.crt": string(ca)},
		},
	))
}

func (f *BasaltCluster) readFile(t *testing.T, path string) []byte {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := f.ctl.Exec(context.Background(), basaltFixtureName, "controller", kube.ExecOptions{
		Command: []string{"cat", path},
		Stdout:  &stdout,
		Stderr:  &stderr,
	})
	require.NoErrorf(t, err, "reading %s from Basalt cluster: %s", path, stderr.String())
	return stdout.Bytes()
}

func (f *BasaltCluster) createTenant(ctx context.Context) error {
	_, err := f.exec(ctx, "tenant", "create", f.adminControllerPath(), f.tenantID)
	return err
}

func (f *BasaltCluster) removeTenantNamespace(ctx context.Context) error {
	_, err := f.exec(ctx, "rm", "-r", f.tenantRootPath())
	return err
}

func (f *BasaltCluster) deleteTenant(ctx context.Context) error {
	_, err := f.exec(ctx, "tenant", "delete", f.adminControllerPath(), f.tenantID)
	return err
}

func (f *BasaltCluster) cleanup(t *testing.T) {
	t.Helper()
	if f.tenantDeleted {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := f.removeTenantNamespace(ctx); err != nil &&
		!strings.Contains(err.Error(), "no such object or directory") {
		t.Logf("best-effort Basalt tenant data cleanup failed: %v", err)
	}
	if err := f.deleteTenant(ctx); err != nil {
		t.Logf("best-effort Basalt tenant deletion failed: %v", err)
	}
}

func (f *BasaltCluster) exec(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	command := append([]string{"/usr/local/bin/basalt"}, args...)
	err := f.ctl.Exec(ctx, basaltFixtureName, "controller", kube.ExecOptions{
		Command: command,
		Stdout:  &stdout,
		Stderr:  &stderr,
	})
	if err != nil {
		return "", errors.Wrapf(
			err, "running %q: %s", strings.Join(command, " "), strings.TrimSpace(stderr.String()),
		)
	}
	return stdout.String(), nil
}

func (f *BasaltCluster) controllerAddress() string {
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d",
		basaltFixtureName, f.ctl.Namespace(), basaltFixtureControllerPort)
}

func (f *BasaltCluster) adminControllerPath() string {
	path := fmt.Sprintf("//localhost:%d", basaltFixtureControllerPort)
	if f.secure {
		path = fmt.Sprintf("//admin@localhost:%d?certs-dir=%s",
			basaltFixtureControllerPort, basaltFixtureCertsDir)
	}
	return path
}

func (f *BasaltCluster) tenantRootPath() string {
	path := fmt.Sprintf("//%s@localhost:%d/%s",
		f.tenantID, basaltFixtureControllerPort, f.tenantID)
	if f.secure {
		path += "?certs-dir=" + basaltFixtureCertsDir
	}
	return path
}

func downloadBasaltServerCommand() string {
	return fmt.Sprintf(`
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
mkdir -p %[1]s/basalt
wget -qO /tmp/basalt.tgz "%[2]s"
tar -xzf /tmp/basalt.tgz --strip-components=1 -C %[1]s/basalt
test -x %[1]s/basalt/basalt-controller
test -x %[1]s/basalt/basalt-blob
`, basaltFixtureBinariesDir, basaltFixtureArtifact)
}

func basaltFixtureResources(
	requestCPU, requestMemory, limitCPU, limitMemory string,
) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(requestCPU),
			corev1.ResourceMemory: resource.MustParse(requestMemory),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(limitCPU),
			corev1.ResourceMemory: resource.MustParse(limitMemory),
		},
	}
}

func tcpReadinessProbe(port int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{
			Port: intstr.FromInt(int(port)),
		}},
		InitialDelaySeconds: 2,
		PeriodSeconds:       1,
	}
}

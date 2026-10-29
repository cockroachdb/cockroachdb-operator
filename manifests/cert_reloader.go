package manifests

import (
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/constants"
	"github.com/cockroachlabs/cockroach-enterprise-operator/securitycontext"
	apiv1 "k8s.io/api/core/v1"
)

// CertReloaderContainer is a sidecar container for a CockroachDB pod. It
// watches the given certificate directories and sends a SIGHUP to Cockroach
// whenever one of them changes, which causes Cockroach to reload the
// certificates.
//
// Pass one mount per PKI domain the pod uses. One sidecar handles all of them,
// so a cluster talking to basalt over mTLS ends up with a single reloader
// watching both the CockroachDB certs and the basalt tenant certs.
func CertReloaderContainer(mounts ...apiv1.VolumeMount) apiv1.Container {
	// The reloader takes the process to signal first and the directories to
	// watch after it. It finds the process by name, which is why this has to
	// match the cockroach binary and why the pod shares its process namespace.
	args := make([]string, 0, len(mounts)+1)
	args = append(args, CockroachProcessName)
	for _, m := range mounts {
		args = append(args, m.MountPath)
	}

	return apiv1.Container{
		Name:            "cert-reloader",
		SecurityContext: securitycontext.ContainerRunAsNonRoot(),
		Image:           constants.GetReloaderImage(),
		ImagePullPolicy: apiv1.PullIfNotPresent,
		Args:            args,
		VolumeMounts:    mounts,
	}
}

// CockroachCertsVolumeMount mounts the CockroachDB certificate volume.
func CockroachCertsVolumeMount() apiv1.VolumeMount {
	return apiv1.VolumeMount{
		Name:      "certs",
		MountPath: CockroachCertsPath,
		ReadOnly:  true,
	}
}

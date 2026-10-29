package manifests

import (
	"fmt"

	"github.com/cockroachlabs/cockroach-enterprise-operator/securitycontext"
	"google.golang.org/protobuf/proto"
	v1 "k8s.io/api/batch/v1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CockroachInitJobOptions contains fields that modify the manifest for the
// cockroach init job that is used for CRDB pods.
type CockroachInitJobOptions struct {
	// The image to use for the init job container. This should specify the
	// repo and version. Ex/ cockroachdb/cockroach:v20.2
	CockroachImage string

	// Whether the cluster being initialized by the job is secure or not.
	Secure bool
}

// CockroachInitJob is a kubernetes job resource. It issues the cockroach init
// command against the cluster, which performs a one time initialization.
func CockroachInitJob(opts CockroachInitJobOptions) *v1.Job {
	volumes := []apiv1.Volume{}
	volumeMounts := []apiv1.VolumeMount{}

	command := []string{
		"/bin/bash",
		"-ecx",
		"until /cockroach/cockroach init --insecure",
	}

	if opts.Secure {
		volumes = append(volumes, SecretVolume(SecretVolumeOptions{
			Name:    "certs",
			Secrets: []string{CockroachClientCertSecretName},
		}))

		volumeMounts = append(volumeMounts, apiv1.VolumeMount{
			MountPath: CockroachCertsPath,
			Name:      "certs",
		})

		command[2] = fmt.Sprintf("until /cockroach/cockroach init --certs-dir=%s ", CockroachCertsPath)
	}

	command[2] += "--host=cockroachdb-0.cockroachdb --port 26257; do sleep 5; done"

	return &v1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cockroachdb-init",
		},
		Spec: v1.JobSpec{
			BackoffLimit: proto.Int32(6),
			Completions:  proto.Int32(1),
			Parallelism:  proto.Int32(1),
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"jobname": "cockroachdb-init",
					},
				},
				Spec: apiv1.PodSpec{
					SecurityContext:               securitycontext.PodRunAsNonRoot(),
					ServiceAccountName:            "cockroachdb",
					DNSPolicy:                     apiv1.DNSClusterFirst,
					RestartPolicy:                 apiv1.RestartPolicyOnFailure,
					SchedulerName:                 "default-scheduler",
					TerminationGracePeriodSeconds: proto.Int64(30),
					Containers: []apiv1.Container{
						{
							Name:                     "cluster-init",
							Command:                  command,
							SecurityContext:          securitycontext.ContainerRunAsNonRoot(),
							Image:                    opts.CockroachImage,
							ImagePullPolicy:          apiv1.PullIfNotPresent,
							TerminationMessagePath:   "/dev/termination-log",
							TerminationMessagePolicy: apiv1.TerminationMessageReadFile,
							VolumeMounts:             volumeMounts,
						},
					},
					Volumes: volumes,
				},
			},
		},
	}
}

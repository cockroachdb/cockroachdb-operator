package manifests

import (
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
)

// readOnlyForProcessGroup defines the permission mode for read-only access
// to files that are mounted as root and are readable by the group that
// the process runs in.
//
// This is because all mounted volumes are owned by root, so we use our
// process group to read them.
var readOnlyForProcessGroup = proto.Int32(0440)

// SecretVolumeOptions defines options for creating volumes which mount one or
// more secrets.
type SecretVolumeOptions struct {
	// Name is the name of the volume.
	Name string

	// Secrets is a list of secrets to include in the volume.
	Secrets []string

	// Renames is a map of secret names to KeyToPath entries. These are used to
	// rename secret data keys when mounting (e.g. tls.key -> node.key).
	// See https://pkg.go.dev/k8s.io/api/core/v1#KeyToPath for details.
	Renames map[string][]corev1.KeyToPath
}

// SecretVolume creates a volume which contains the specified secrets define in
// opts. This will setup either a SecretVolumeSource or a ProjectedVolumeSource
// depending on the number of secrets provided.
func SecretVolume(opts SecretVolumeOptions) corev1.Volume {
	readOnly := readOnlyForProcessGroup
	var source corev1.VolumeSource
	if len(opts.Secrets) > 1 {
		sources := make([]corev1.VolumeProjection, len(opts.Secrets))

		for i, s := range opts.Secrets {
			sources[i] = corev1.VolumeProjection{
				Secret: &corev1.SecretProjection{
					Items: opts.Renames[s],
					LocalObjectReference: corev1.LocalObjectReference{
						Name: s,
					},
				},
			}
		}

		source = corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				DefaultMode: readOnly,
				Sources:     sources,
			},
		}
	} else {
		source = corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				DefaultMode: readOnly,
				Items:       opts.Renames[opts.Secrets[0]],
				SecretName:  opts.Secrets[0],
			},
		}
	}

	return corev1.Volume{Name: opts.Name, VolumeSource: source}
}

// SecretProjection is a convenience struct for defining complex Kubernetes
// volume projections that use Secrets as their source.
type SecretProjection struct {
	// Name is the name of the Secret to be projected from.
	Name string

	// Renames is a slice of key remappings to be performed.
	// It is recommended to specify all keys as being renamed to themselves, if
	// there's a chance that a Name may be reused.
	// For example, crdbv1beta1.ExternalCertificates could contain re-used
	// secrets, therefore any direct mountings of tls.crt should specify them
	// as renames from tls.crt to tls.crt.
	Renames []corev1.KeyToPath
}

// ConfigMapProjection is a convenience struct for defining complex Kubernetes
// volume projections that use ConfigMaps as their source.
type ConfigMapProjection struct {
	// Name is the name of the ConfigMap to be projected from.
	Name string

	// Renames is a slice of key remappings to be performed.
	// It is recommended to specify all keys as being renamed to themselves, if
	// there's a chance that a Name may be reused.
	// For example, crdbv1beta1.ExternalCertificates could contain re-used
	// configmaps, therefore any direct mountings of ca.crt should specify them
	// as renames from ca.crt to ca.crt.
	Renames []corev1.KeyToPath
}

// VolumeProjection is a helper that creates a corev1.Volume with a
// ProjectedVolumeSource from the provided Secrets and ConfigMaps. Should
// duplicated .Name entries within either secrets or configMap be found, they
// will be merged into a single VolumeProjection to pass Kubernetes validation
// and project as expected.
func VolumeProjection(
	name string, secrets []SecretProjection, configMaps []ConfigMapProjection,
) corev1.Volume {
	readOnly := readOnlyForProcessGroup
	secretIdx := make(map[string]int, 0)
	configMapIdx := make(map[string]int, 0)
	sources := make([]corev1.VolumeProjection, 0, len(secrets))

	for _, secret := range secrets {
		if i, ok := secretIdx[secret.Name]; ok {
			sources[i].Secret.Items = append(sources[i].Secret.Items, secret.Renames...)
			continue
		}

		secretIdx[secret.Name] = len(sources)
		sources = append(sources, corev1.VolumeProjection{
			Secret: &corev1.SecretProjection{
				Items: secret.Renames,
				LocalObjectReference: corev1.LocalObjectReference{
					Name: secret.Name,
				},
			},
		})
	}

	for _, configMap := range configMaps {
		if i, ok := configMapIdx[configMap.Name]; ok {
			sources[i].ConfigMap.Items = append(sources[i].ConfigMap.Items, configMap.Renames...)
			continue
		}

		configMapIdx[configMap.Name] = len(sources)
		sources = append(sources, corev1.VolumeProjection{
			ConfigMap: &corev1.ConfigMapProjection{
				Items: configMap.Renames,
				LocalObjectReference: corev1.LocalObjectReference{
					Name: configMap.Name,
				},
			},
		})
	}

	return corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				DefaultMode: readOnly,
				Sources:     sources,
			},
		},
	}
}

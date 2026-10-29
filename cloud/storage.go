package cloud

// VolumeType identifies supported disk/volume types that may be provisioned as
// PersistentVolumes within Kubernetes.
type VolumeType string

func (v VolumeType) String() string {
	return string(v)
}

const (
	// AWS Volume Types
	// https://kubernetes.io/docs/concepts/storage/storage-classes/#aws-ebs
	GP2 = VolumeType("gp2")
	IO1 = VolumeType("io1")
	GP3 = VolumeType("gp3")

	// Azure Volume Types
	// https://learn.microsoft.com/en-us/azure/aks/azure-csi-disk-storage-provision#dynamic-provisioning-parameters
	// TODO(ding): do we need PremiumLRS at all?
	PremiumLRS        = VolumeType("Premium_LRS")
	PremiumV2LRS      = VolumeType("PremiumV2_LRS")
	ManagedCSIPremium = VolumeType("managed-csi-premium")

	// GCP Volume Types
	// https://cloud.google.com/kubernetes-engine/docs/how-to/persistent-volumes/ssd-pd
	// https://docs.cloud.google.com/kubernetes-engine/docs/how-to/persistent-volumes/hyperdisk
	Standard  = VolumeType("pd-standard")
	SSD       = VolumeType("pd-ssd")
	Hyperdisk = VolumeType("hyperdisk-balanced")

	// K3D Volume Types
	// https://github.com/rancher/local-path-provisioner
	LocalPath = VolumeType("local-path")
)

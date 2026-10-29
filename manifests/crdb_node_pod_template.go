/*
Pod Template Merging Strategy and Precedence Order

This file implements the pod template merging functionality for CockroachDB pods.
The PodTemplateBuilder provides a mechanism to merge user-specified pod templates
with operator-generated base pods, allowing customization while maintaining
operator-required functionality.

PRECEDENCE ORDER (highest to lowest):
1. PodTemplate fields (user-specified) - Complete override for most fields
2. StartFlags (user-specified) - Command generation and environment variables
3. First-class CRD fields (user-specified) - Direct pod spec fields like Image, Affinity
4. Operator defaults - Required functionality and default configurations

MERGING APPROACH:
- Scalar fields: Template values completely override base values
- Map fields: Template entries are merged into base maps (template takes precedence for duplicates)
- List fields: Generally merged by key/name with template taking precedence for duplicates
- Affinity: Complete override - no merging with operator-required rules when template provides affinity

KEY BEHAVIORS:
- Container merging: Containers are matched by name; template containers override base containers
- Volume merging: Volumes are matched by name; template volumes override base volumes
- Environment variables: Merged by name with template taking precedence
- Resource requirements: Template limits/requests are merged into base resources
- Security contexts: Template fields override base fields while preserving unspecified base fields

SPECIAL CASES:
- Command override: If podTemplate specifies a command for the cockroachdb container,
  startFlags are ignored, and the template command is used
- Affinity override: Template affinity completely replaces operator affinity rules
- LastAppliedConfigAnnotation: Always excluded from template annotations to avoid conflicts

This approach ensures users can customize pods extensively while maintaining
operator functionality and avoiding configuration conflicts.
*/

package manifests

import (
	"fmt"
	"maps"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
)

// PodTemplateBuilder handles building a Pod with all necessary configuration
// merged from a template spec.
type PodTemplateBuilder struct {
	basePod     *corev1.Pod
	podTemplate *crdbv1beta1.PodTemplateSpec
}

// NewPodTemplateBuilder creates a new builder for pod templates.
func NewPodTemplateBuilder(
	basePod *corev1.Pod, template *crdbv1beta1.PodTemplateSpec,
) *PodTemplateBuilder {
	return &PodTemplateBuilder{
		basePod:     basePod.DeepCopy(),
		podTemplate: template,
	}
}

// Build constructs the final pod with all template overrides applied.
func (b *PodTemplateBuilder) Build() *corev1.Pod {
	if b.podTemplate == nil {
		return b.basePod
	}

	b.mergeMetadata()
	b.mergeSpecFields()
	b.mergeContainers()
	b.mergeEphemeralContainers()
	b.mergeInitContainers()
	b.mergeVolumes()

	return b.basePod
}

// mergeMetadata merges labels and annotations from the template into the base pod.
func (b *PodTemplateBuilder) mergeMetadata() {
	if b.podTemplate == nil {
		return
	}

	if b.basePod.Labels == nil {
		b.basePod.Labels = make(map[string]string)
	}
	maps.Copy(b.basePod.Labels, b.podTemplate.Metadata.Labels)

	if b.basePod.Annotations == nil {
		b.basePod.Annotations = make(map[string]string)
	}
	for k, v := range b.podTemplate.Metadata.Annotations {
		if k != corev1.LastAppliedConfigAnnotation {
			b.basePod.Annotations[k] = v
		}
	}
}

// mergeSpecFields merges pod spec fields from the template into the base pod.
func (b *PodTemplateBuilder) mergeSpecFields() {
	if b.podTemplate == nil {
		return
	}

	if b.podTemplate.Spec.ActiveDeadlineSeconds != nil {
		b.basePod.Spec.ActiveDeadlineSeconds = b.podTemplate.Spec.ActiveDeadlineSeconds
	}

	if b.podTemplate.Spec.Affinity != nil {
		b.basePod.Spec.Affinity = b.podTemplate.Spec.Affinity.DeepCopy()
	}

	if b.podTemplate.Spec.AutomountServiceAccountToken != nil {
		b.basePod.Spec.AutomountServiceAccountToken = b.podTemplate.Spec.AutomountServiceAccountToken
	}

	if b.podTemplate.Spec.DNSConfig != nil {
		b.basePod.Spec.DNSConfig = b.podTemplate.Spec.DNSConfig.DeepCopy()
	}

	if b.podTemplate.Spec.DNSPolicy != "" {
		b.basePod.Spec.DNSPolicy = b.podTemplate.Spec.DNSPolicy
	}

	if b.podTemplate.Spec.EnableServiceLinks != nil {
		b.basePod.Spec.EnableServiceLinks = b.podTemplate.Spec.EnableServiceLinks
	}

	if len(b.podTemplate.Spec.HostAliases) > 0 {
		b.basePod.Spec.HostAliases = make([]corev1.HostAlias, len(b.podTemplate.Spec.HostAliases))
		copy(b.basePod.Spec.HostAliases, b.podTemplate.Spec.HostAliases)
	}

	if b.podTemplate.Spec.HostIPC {
		b.basePod.Spec.HostIPC = b.podTemplate.Spec.HostIPC
	}

	if b.podTemplate.Spec.HostNetwork {
		b.basePod.Spec.HostNetwork = b.podTemplate.Spec.HostNetwork
	}

	if b.podTemplate.Spec.HostPID {
		b.basePod.Spec.HostPID = b.podTemplate.Spec.HostPID
	}

	if b.podTemplate.Spec.HostUsers != nil {
		b.basePod.Spec.HostUsers = b.podTemplate.Spec.HostUsers
	}

	if b.podTemplate.Spec.Hostname != "" {
		b.basePod.Spec.Hostname = b.podTemplate.Spec.Hostname
	}

	if len(b.podTemplate.Spec.ImagePullSecrets) > 0 {
		b.basePod.Spec.ImagePullSecrets = b.podTemplate.Spec.ImagePullSecrets
	}

	if b.podTemplate.Spec.NodeName != "" {
		b.basePod.Spec.NodeName = b.podTemplate.Spec.NodeName
	}

	if len(b.podTemplate.Spec.NodeSelector) > 0 {
		if b.basePod.Spec.NodeSelector == nil {
			b.basePod.Spec.NodeSelector = make(map[string]string)
		}
		maps.Copy(b.basePod.Spec.NodeSelector, b.podTemplate.Spec.NodeSelector)
	}

	if b.podTemplate.Spec.OS != nil {
		b.basePod.Spec.OS = b.podTemplate.Spec.OS.DeepCopy()
	}

	if b.podTemplate.Spec.Overhead != nil {
		b.basePod.Spec.Overhead = make(corev1.ResourceList)
		maps.Copy(b.basePod.Spec.Overhead, b.podTemplate.Spec.Overhead)
	}

	if b.podTemplate.Spec.PreemptionPolicy != nil {
		b.basePod.Spec.PreemptionPolicy = b.podTemplate.Spec.PreemptionPolicy
	}

	if b.podTemplate.Spec.Priority != nil {
		b.basePod.Spec.Priority = b.podTemplate.Spec.Priority
	}

	if b.podTemplate.Spec.PriorityClassName != "" {
		b.basePod.Spec.PriorityClassName = b.podTemplate.Spec.PriorityClassName
	}

	if len(b.podTemplate.Spec.ReadinessGates) > 0 {
		b.basePod.Spec.ReadinessGates = make([]corev1.PodReadinessGate, len(b.podTemplate.Spec.ReadinessGates))
		copy(b.basePod.Spec.ReadinessGates, b.podTemplate.Spec.ReadinessGates)
	}

	if len(b.podTemplate.Spec.ResourceClaims) > 0 {
		b.basePod.Spec.ResourceClaims = make([]corev1.PodResourceClaim, len(b.podTemplate.Spec.ResourceClaims))
		copy(b.basePod.Spec.ResourceClaims, b.podTemplate.Spec.ResourceClaims)
	}

	if b.podTemplate.Spec.RestartPolicy != "" {
		b.basePod.Spec.RestartPolicy = b.podTemplate.Spec.RestartPolicy
	}

	if b.podTemplate.Spec.RuntimeClassName != nil {
		b.basePod.Spec.RuntimeClassName = b.podTemplate.Spec.RuntimeClassName
	}

	if b.podTemplate.Spec.SchedulerName != "" {
		b.basePod.Spec.SchedulerName = b.podTemplate.Spec.SchedulerName
	}

	if len(b.podTemplate.Spec.SchedulingGates) > 0 {
		b.basePod.Spec.SchedulingGates = make([]corev1.PodSchedulingGate, len(b.podTemplate.Spec.SchedulingGates))
		copy(b.basePod.Spec.SchedulingGates, b.podTemplate.Spec.SchedulingGates)
	}

	if b.podTemplate.Spec.SecurityContext != nil {
		if b.basePod.Spec.SecurityContext == nil {
			b.basePod.Spec.SecurityContext = b.podTemplate.Spec.SecurityContext.DeepCopy()
		} else {
			mergeSecurityContext(b.basePod.Spec.SecurityContext, b.podTemplate.Spec.SecurityContext)
		}
	}

	if b.podTemplate.Spec.ServiceAccountName != "" {
		b.basePod.Spec.ServiceAccountName = b.podTemplate.Spec.ServiceAccountName
	}

	if b.podTemplate.Spec.SetHostnameAsFQDN != nil {
		b.basePod.Spec.SetHostnameAsFQDN = b.podTemplate.Spec.SetHostnameAsFQDN
	}

	if b.podTemplate.Spec.ShareProcessNamespace != nil {
		b.basePod.Spec.ShareProcessNamespace = b.podTemplate.Spec.ShareProcessNamespace
	}

	if b.podTemplate.Spec.Subdomain != "" {
		b.basePod.Spec.Subdomain = b.podTemplate.Spec.Subdomain
	}

	if b.podTemplate.Spec.TerminationGracePeriodSeconds != nil {
		b.basePod.Spec.TerminationGracePeriodSeconds = b.podTemplate.Spec.TerminationGracePeriodSeconds
	}

	if len(b.podTemplate.Spec.Tolerations) > 0 {
		b.basePod.Spec.Tolerations = mergeTolerationsSlice(b.basePod.Spec.Tolerations, b.podTemplate.Spec.Tolerations)
	}

	if len(b.podTemplate.Spec.TopologySpreadConstraints) > 0 {
		b.basePod.Spec.TopologySpreadConstraints = mergeTopologySpreadConstraints(
			b.basePod.Spec.TopologySpreadConstraints,
			b.podTemplate.Spec.TopologySpreadConstraints,
		)
	}
}

// mergeContainers merges container specs from the template into the base pod's containers.
func (b *PodTemplateBuilder) mergeContainers() {
	if b.podTemplate == nil || len(b.podTemplate.Spec.Containers) == 0 {
		return
	}

	baseContainerMap := make(map[string]int)
	for i, container := range b.basePod.Spec.Containers {
		baseContainerMap[container.Name] = i
	}

	for _, templateContainer := range b.podTemplate.Spec.Containers {
		if idx, exists := baseContainerMap[templateContainer.Name]; exists {
			b.basePod.Spec.Containers[idx] = mergeContainer(b.basePod.Spec.Containers[idx], templateContainer)
		} else {
			b.basePod.Spec.Containers = append(b.basePod.Spec.Containers, templateContainer)
		}
	}
}

// mergeEphemeralContainers merges ephemeral container specs from the template into the base pod's ephemeral containers.
func (b *PodTemplateBuilder) mergeEphemeralContainers() {
	if b.podTemplate == nil || len(b.podTemplate.Spec.EphemeralContainers) == 0 {
		return
	}

	baseEphemeralContainerMap := make(map[string]int)
	for i, container := range b.basePod.Spec.EphemeralContainers {
		baseEphemeralContainerMap[container.Name] = i
	}

	for _, templateContainer := range b.podTemplate.Spec.EphemeralContainers {
		if idx, exists := baseEphemeralContainerMap[templateContainer.Name]; exists {
			// For ephemeral containers, we replace the entire container since they can't be modified
			b.basePod.Spec.EphemeralContainers[idx] = templateContainer
		} else {
			b.basePod.Spec.EphemeralContainers = append(b.basePod.Spec.EphemeralContainers, templateContainer)
		}
	}
}

// mergeInitContainers merges init container specs from the template into the base pod's init containers.
func (b *PodTemplateBuilder) mergeInitContainers() {
	if b.podTemplate == nil || len(b.podTemplate.Spec.InitContainers) == 0 {
		return
	}

	baseInitContainerMap := make(map[string]int)
	for i, container := range b.basePod.Spec.InitContainers {
		baseInitContainerMap[container.Name] = i
	}

	for _, templateContainer := range b.podTemplate.Spec.InitContainers {
		if idx, exists := baseInitContainerMap[templateContainer.Name]; exists {
			b.basePod.Spec.InitContainers[idx] = mergeContainer(b.basePod.Spec.InitContainers[idx], templateContainer)
		} else {
			b.basePod.Spec.InitContainers = append(b.basePod.Spec.InitContainers, templateContainer)
		}
	}
}

// mergeVolumes merges volumes from the template into the base pod's volumes.
func (b *PodTemplateBuilder) mergeVolumes() {
	if b.podTemplate == nil || len(b.podTemplate.Spec.Volumes) == 0 {
		return
	}

	baseVolumeMap := make(map[string]int)
	for i, volume := range b.basePod.Spec.Volumes {
		baseVolumeMap[volume.Name] = i
	}

	for _, templateVolume := range b.podTemplate.Spec.Volumes {
		if idx, exists := baseVolumeMap[templateVolume.Name]; exists {
			b.basePod.Spec.Volumes[idx] = templateVolume
		} else {
			b.basePod.Spec.Volumes = append(b.basePod.Spec.Volumes, templateVolume)
		}
	}
}

// mergeContainer merges two container specs, with the template taking precedence.
func mergeContainer(baseContainer, templateContainer corev1.Container) corev1.Container {
	result := baseContainer.DeepCopy()

	if len(templateContainer.Args) > 0 {
		result.Args = templateContainer.Args
	}

	if len(templateContainer.Command) > 0 {
		result.Command = templateContainer.Command
	}

	result.Env = mergeEnvVars(result.Env, templateContainer.Env)

	if len(templateContainer.EnvFrom) > 0 {
		result.EnvFrom = append(result.EnvFrom, templateContainer.EnvFrom...)
	}

	if templateContainer.Image != "" {
		result.Image = templateContainer.Image
	}

	if templateContainer.ImagePullPolicy != "" {
		result.ImagePullPolicy = templateContainer.ImagePullPolicy
	}

	if templateContainer.Lifecycle != nil {
		result.Lifecycle = templateContainer.Lifecycle.DeepCopy()
	}

	if templateContainer.LivenessProbe != nil {
		result.LivenessProbe = templateContainer.LivenessProbe.DeepCopy()
	}

	if templateContainer.Name != "" {
		result.Name = templateContainer.Name
	}

	result.Ports = mergePorts(result.Ports, templateContainer.Ports)

	if templateContainer.ReadinessProbe != nil {
		result.ReadinessProbe = templateContainer.ReadinessProbe.DeepCopy()
	}

	if templateContainer.Resources.Limits != nil || templateContainer.Resources.Requests != nil {
		result.Resources = mergeResourceRequirements(result.Resources, templateContainer.Resources)
	}

	if templateContainer.SecurityContext != nil {
		if result.SecurityContext == nil {
			result.SecurityContext = templateContainer.SecurityContext.DeepCopy()
		} else {
			mergeContainerSecurityContext(result.SecurityContext, templateContainer.SecurityContext)
		}
	}

	if templateContainer.StartupProbe != nil {
		result.StartupProbe = templateContainer.StartupProbe.DeepCopy()
	}

	if templateContainer.Stdin {
		result.Stdin = templateContainer.Stdin
	}

	if templateContainer.StdinOnce {
		result.StdinOnce = templateContainer.StdinOnce
	}

	if templateContainer.TTY {
		result.TTY = templateContainer.TTY
	}

	if templateContainer.TerminationMessagePath != "" {
		result.TerminationMessagePath = templateContainer.TerminationMessagePath
	}

	if templateContainer.TerminationMessagePolicy != "" {
		result.TerminationMessagePolicy = templateContainer.TerminationMessagePolicy
	}

	if len(templateContainer.VolumeDevices) > 0 {
		result.VolumeDevices = mergeVolumeDevices(result.VolumeDevices, templateContainer.VolumeDevices)
	}

	result.VolumeMounts = mergeVolumeMounts(result.VolumeMounts, templateContainer.VolumeMounts)

	if templateContainer.WorkingDir != "" {
		result.WorkingDir = templateContainer.WorkingDir
	}

	return *result
}

// mergeEnvVars merges environment variables with template taking precedence.
func mergeEnvVars(baseEnvVars, templateEnvVars []corev1.EnvVar) []corev1.EnvVar {
	if len(templateEnvVars) == 0 {
		return baseEnvVars
	}

	baseEnvMap := make(map[string]int)
	for i, env := range baseEnvVars {
		baseEnvMap[env.Name] = i
	}

	result := baseEnvVars

	for _, templateEnv := range templateEnvVars {
		if idx, exists := baseEnvMap[templateEnv.Name]; exists {
			result[idx] = templateEnv
		} else {
			result = append(result, templateEnv)
		}
	}

	return result
}

// mergePorts merges container ports with template taking precedence.
func mergePorts(basePorts, templatePorts []corev1.ContainerPort) []corev1.ContainerPort {
	if len(templatePorts) == 0 {
		return basePorts
	}

	basePortMap := make(map[string]int)
	for i, port := range basePorts {
		if port.Name != "" {
			basePortMap[port.Name] = i
		} else {
			basePortMap[fmt.Sprintf("%d", port.ContainerPort)] = i
		}
	}

	result := make([]corev1.ContainerPort, len(basePorts))
	copy(result, basePorts)

	for _, templatePort := range templatePorts {
		key := templatePort.Name
		if key == "" {
			key = fmt.Sprintf("%d", templatePort.ContainerPort)
		}

		if idx, exists := basePortMap[key]; exists {
			result[idx] = templatePort
		} else {
			result = append(result, templatePort)
		}
	}

	return result
}

// mergeVolumeMounts merges volume mounts with template taking precedence.
func mergeVolumeMounts(baseMounts, templateMounts []corev1.VolumeMount) []corev1.VolumeMount {
	if len(templateMounts) == 0 {
		return baseMounts
	}

	baseMountMap := make(map[string]int)
	for i, mount := range baseMounts {
		key := fmt.Sprintf("%s-%s", mount.Name, mount.MountPath)
		baseMountMap[key] = i
	}

	result := make([]corev1.VolumeMount, len(baseMounts))
	copy(result, baseMounts)

	for _, templateMount := range templateMounts {
		key := fmt.Sprintf("%s-%s", templateMount.Name, templateMount.MountPath)
		if idx, exists := baseMountMap[key]; exists {
			result[idx] = templateMount
		} else {
			result = append(result, templateMount)
		}
	}

	return result
}

// mergeVolumeDevices merges volume devices with template taking precedence.
func mergeVolumeDevices(baseDevices, templateDevices []corev1.VolumeDevice) []corev1.VolumeDevice {
	if len(templateDevices) == 0 {
		return baseDevices
	}

	baseDeviceMap := make(map[string]int)
	for i, device := range baseDevices {
		baseDeviceMap[device.DevicePath] = i
	}

	result := make([]corev1.VolumeDevice, len(baseDevices))
	copy(result, baseDevices)

	for _, templateDevice := range templateDevices {
		if idx, exists := baseDeviceMap[templateDevice.DevicePath]; exists {
			result[idx] = templateDevice
		} else {
			result = append(result, templateDevice)
		}
	}

	return result
}

// mergeResourceRequirements merges resource requirements with the template taking precedence.
func mergeResourceRequirements(
	base, template corev1.ResourceRequirements,
) corev1.ResourceRequirements {
	result := base.DeepCopy()

	if result.Limits == nil {
		result.Limits = corev1.ResourceList{}
	}
	maps.Copy(result.Limits, template.Limits)

	if result.Requests == nil {
		result.Requests = corev1.ResourceList{}
	}
	maps.Copy(result.Requests, template.Requests)

	if len(template.Claims) > 0 {
		result.Claims = make([]corev1.ResourceClaim, len(template.Claims))
		copy(result.Claims, template.Claims)
	}

	return *result
}

// mergeTolerationsSlice merges tolerations with template taking precedence.
func mergeTolerationsSlice(
	baseTolerations, templateTolerations []corev1.Toleration,
) []corev1.Toleration {
	if len(templateTolerations) == 0 {
		return baseTolerations
	}

	baseTolerationsMap := make(map[string]int)
	for i, toleration := range baseTolerations {
		key := fmt.Sprintf("%s-%s-%s", toleration.Key, toleration.Value, string(toleration.Effect))
		baseTolerationsMap[key] = i
	}

	result := make([]corev1.Toleration, len(baseTolerations))
	copy(result, baseTolerations)

	for _, templateToleration := range templateTolerations {
		key := fmt.Sprintf("%s-%s-%s", templateToleration.Key, templateToleration.Value, string(templateToleration.Effect))
		if idx, exists := baseTolerationsMap[key]; exists {
			result[idx] = templateToleration
		} else {
			result = append(result, templateToleration)
		}
	}

	return result
}

// mergeTopologySpreadConstraints merges topology spread constraints with template taking precedence.
func mergeTopologySpreadConstraints(
	base, template []corev1.TopologySpreadConstraint,
) []corev1.TopologySpreadConstraint {
	if len(template) == 0 {
		return base
	}

	baseConstraintsMap := make(map[string]int)
	for i, constraint := range base {
		baseConstraintsMap[constraint.TopologyKey] = i
	}

	result := make([]corev1.TopologySpreadConstraint, len(base))
	copy(result, base)

	for _, templateConstraint := range template {
		if idx, exists := baseConstraintsMap[templateConstraint.TopologyKey]; exists {
			result[idx] = templateConstraint
		} else {
			result = append(result, templateConstraint)
		}
	}

	return result
}

// mergeSecurityContext merges pod security context with the template taking precedence.
func mergeSecurityContext(base, template *corev1.PodSecurityContext) {
	if template.SELinuxOptions != nil {
		base.SELinuxOptions = template.SELinuxOptions.DeepCopy()
	}
	if template.WindowsOptions != nil {
		base.WindowsOptions = template.WindowsOptions.DeepCopy()
	}
	if template.RunAsUser != nil {
		base.RunAsUser = template.RunAsUser
	}
	if template.RunAsGroup != nil {
		base.RunAsGroup = template.RunAsGroup
	}
	if template.RunAsNonRoot != nil {
		base.RunAsNonRoot = template.RunAsNonRoot
	}
	if template.SupplementalGroups != nil {
		base.SupplementalGroups = template.SupplementalGroups
	}
	if template.FSGroup != nil {
		base.FSGroup = template.FSGroup
	}
	if template.Sysctls != nil {
		base.Sysctls = template.Sysctls
	}
	if template.FSGroupChangePolicy != nil {
		base.FSGroupChangePolicy = template.FSGroupChangePolicy
	}
	if template.SeccompProfile != nil {
		base.SeccompProfile = template.SeccompProfile.DeepCopy()
	}
}

// mergeContainerSecurityContext merges container security context with the template taking precedence.
func mergeContainerSecurityContext(base, template *corev1.SecurityContext) {
	if template.Capabilities != nil {
		base.Capabilities = template.Capabilities.DeepCopy()
	}
	if template.Privileged != nil {
		base.Privileged = template.Privileged
	}
	if template.SELinuxOptions != nil {
		base.SELinuxOptions = template.SELinuxOptions.DeepCopy()
	}
	if template.WindowsOptions != nil {
		base.WindowsOptions = template.WindowsOptions.DeepCopy()
	}
	if template.RunAsUser != nil {
		base.RunAsUser = template.RunAsUser
	}
	if template.RunAsGroup != nil {
		base.RunAsGroup = template.RunAsGroup
	}
	if template.RunAsNonRoot != nil {
		base.RunAsNonRoot = template.RunAsNonRoot
	}
	if template.ReadOnlyRootFilesystem != nil {
		base.ReadOnlyRootFilesystem = template.ReadOnlyRootFilesystem
	}
	if template.AllowPrivilegeEscalation != nil {
		base.AllowPrivilegeEscalation = template.AllowPrivilegeEscalation
	}
	if template.ProcMount != nil {
		base.ProcMount = template.ProcMount
	}
	if template.SeccompProfile != nil {
		base.SeccompProfile = template.SeccompProfile.DeepCopy()
	}
}

// ApplyPodTemplate combines a template with a pod, giving preference to template fields.
func ApplyPodTemplate(template *crdbv1beta1.PodTemplateSpec, pod *corev1.Pod) *corev1.Pod {
	if template == nil {
		return pod
	}

	builder := NewPodTemplateBuilder(pod, template)
	return builder.Build()
}

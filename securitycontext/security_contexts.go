package securitycontext

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/pointer"
)

const (
	// UserID is the user ID that pods will run as.
	// This ID can be anything as long as it's not an ID that is associated
	// with an existing user in the container.
	UserID int64 = 1000
	// GroupID is the group ID that pods will run as
	// This ID can be anything as long as it's not an ID that is associated
	// with an existing group in the container.
	//
	// There's no issue changing this ID as long as the FSGroup ID matches
	// it, otherwise secrets will not be readable by the process.
	GroupID int64 = 1000
	// runAsNonRoot true requires that any container in the pod will run as a
	// user with a UserID that is not 0 (root).
	runAsNonRoot bool = true
	// privileged false means the container cannot enable privileged mode
	// and will not have access to the host devices
	privileged             bool = false
	readOnlyRootFilesystem bool = true
	// allowPrivilegeEscalation false will set no_new_privs flag on the container process.
	// This flag prevents setuid binaries from changing the UserID, and prevent files from enabling
	// extra capabilities.
	allowPrivilegeEscalation bool = false
)

// PodRunAsNonRoot returns a Pod SecurityContext which sets the pod containers to run as the user 1000
// and sets the container processes to be part of group 1000.
func PodRunAsNonRoot() *corev1.PodSecurityContext {
	onRootMismatch := corev1.FSGroupChangeOnRootMismatch
	return &corev1.PodSecurityContext{
		RunAsUser:           pointer.Int64Ptr(UserID),
		RunAsGroup:          pointer.Int64Ptr(GroupID),
		RunAsNonRoot:        pointer.BoolPtr(runAsNonRoot),
		FSGroup:             pointer.Int64Ptr(GroupID),
		FSGroupChangePolicy: &onRootMismatch,
	}
}

// ContainerRunAsNonRoot returns a Container SecurityContext which drops capabilities. Capabilities permit certain
// root actions without giving users full root access so must be dropped from the pod.
// We also ensure that the filesystem remains read only, and prevent privileged access or privilege
// escalation to access host devices.
func ContainerRunAsNonRoot() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{
				"ALL",
			},
		},
		Privileged:               pointer.BoolPtr(privileged),
		ReadOnlyRootFilesystem:   pointer.BoolPtr(readOnlyRootFilesystem),
		AllowPrivilegeEscalation: pointer.BoolPtr(allowPrivilegeEscalation),
	}
}

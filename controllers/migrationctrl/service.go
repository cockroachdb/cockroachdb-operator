package migrationctrl

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// findPublicService locates the public service for a cluster. Helm charts name
// it <cluster>-public, while the operator uses <cluster> directly.
func findPublicService(
	ctx context.Context, ctl *kube.Ctl, clusterName string,
) (*corev1.Service, error) {
	for _, name := range []string{fmt.Sprintf("%s-public", clusterName), clusterName} {
		svc := &corev1.Service{}
		if err := ctl.Get(ctx, name, svc); err == nil {
			return svc, nil
		} else if !apierrors.IsNotFound(err) {
			return nil, errors.Wrapf(err, "failed to check for service %s", name)
		}
	}
	return nil, nil
}

// restoreServiceSelectors restores the original service selectors for rollback.
func restoreServiceSelectors(
	ctx context.Context, ctl *kube.Ctl, clusterName, migrationType string,
) error {
	logging.Infof(ctx, "Restoring service selectors for cluster %s (type: %s)", clusterName, migrationType)

	svc := &corev1.Service{}
	err := ctl.Get(ctx, clusterName, svc)
	if apierrors.IsNotFound(err) {
		logging.Infof(ctx, "Service %s not found, nothing to restore", clusterName)
		return nil
	}
	if err != nil {
		return errors.Wrap(err, "failed to get service")
	}

	// Build the exact selector map for the target migration type. We replace
	// the selector wholesale rather than mutating individual keys so that
	// leftover keys from the migrated state don't cause the service to match
	// zero pods after rollback.
	var expected map[string]string

	switch migrationType {
	case MigrationTypeHelm:
		expected = map[string]string{
			"app.kubernetes.io/instance": clusterName,
			"app.kubernetes.io/name":     "cockroachdb",
		}
	default: // MigrationTypeOperator
		expected = map[string]string{
			"app.kubernetes.io/component": "database",
			"app.kubernetes.io/instance":  clusterName,
		}
	}

	needsUpdate := !maps.Equal(svc.Spec.Selector, expected)

	if needsUpdate {
		svc.Spec.Selector = expected
		// selector updates don't require waiting for ClusterIP assignment
		if err := ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, svc); err != nil {
			return errors.Wrap(err, "failed to update service selectors")
		}
		logging.Infof(ctx, "Restored service selectors for %s", clusterName)
	} else {
		logging.Infof(ctx, "Service selectors already correct for %s", clusterName)
	}

	return nil
}

// restoreServicePorts restores the original port configuration saved before migration
// modified the service. If the annotation is missing, the service was never modified and
// there is nothing to restore.
func restoreServicePorts(ctx context.Context, ctl *kube.Ctl, clusterName string) error {
	svc, err := findPublicService(ctx, ctl, clusterName)
	if err != nil {
		return err
	}
	if svc == nil {
		return nil
	}

	raw, ok := svc.Annotations[annotationOriginalPorts]
	if !ok || raw == "" {
		logging.Infof(ctx, "No original ports annotation on service %s, nothing to restore", svc.Name)
		return nil
	}

	var originalPorts []corev1.ServicePort
	if err := json.Unmarshal([]byte(raw), &originalPorts); err != nil {
		return errors.Wrapf(err, "failed to parse original ports annotation on service %s", svc.Name)
	}

	svc.Spec.Ports = originalPorts
	delete(svc.Annotations, annotationOriginalPorts)
	if err := ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, svc); err != nil {
		return errors.Wrap(err, "failed to restore service ports")
	}
	logging.Infof(ctx, "Restored original ports on service %s", svc.Name)
	return nil
}

// removeOriginalPortsAnnotation cleans up the saved-ports annotation after a successful
// migration. The annotation is no longer needed once the cluster is fully migrated.
func removeOriginalPortsAnnotation(ctx context.Context, ctl *kube.Ctl, clusterName string) {
	svc, err := findPublicService(ctx, ctl, clusterName)
	if err != nil || svc == nil {
		return
	}
	if _, ok := svc.Annotations[annotationOriginalPorts]; !ok {
		return
	}
	delete(svc.Annotations, annotationOriginalPorts)
	if err := ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, svc); err != nil {
		logging.Warnf(ctx, "Failed to remove original-ports annotation from service %s: %v", svc.Name, err)
	}
}

// updatePublicServicePorts updates the public service to have separate GRPC and SQL ports.
// The Helm chart uses a single port for both GRPC and SQL, but the CockroachDB Operator uses
// separate named ports ("grpc" and "sql"). grpcPort and sqlPort are the actual ports from the
// parsed StatefulSet; callers must pass the resolved values so non-default ports are preserved.
func updatePublicServicePorts(
	ctx context.Context, ctl *kube.Ctl, clusterName string, grpcPort, sqlPort int32,
) error {
	logging.Infof(ctx, "Updating public service ports for cluster %s", clusterName)

	svc, err := findPublicService(ctx, ctl, clusterName)
	if err != nil {
		return err
	}
	if svc == nil {
		logging.Infof(ctx, "No public service found for cluster %s", clusterName)
		return nil
	}
	svcName := svc.Name

	// Save the original port configuration so rollback can restore it exactly.
	if svc.Annotations == nil || svc.Annotations[annotationOriginalPorts] == "" {
		portsJSON, err := json.Marshal(svc.Spec.Ports)
		if err != nil {
			return errors.Wrap(err, "failed to marshal original service ports for rollback")
		}
		if svc.Annotations == nil {
			svc.Annotations = make(map[string]string)
		}
		svc.Annotations[annotationOriginalPorts] = string(portsJSON)
	}

	var grpcFound, sqlFound bool
	needsUpdate := false

	grpcSvcPort := corev1.ServicePort{
		Name:       "grpc",
		Protocol:   corev1.ProtocolTCP,
		Port:       grpcPort,
		TargetPort: intstr.FromString("grpc"),
	}

	sqlSvcPort := corev1.ServicePort{
		Name:       "sql",
		Protocol:   corev1.ProtocolTCP,
		Port:       sqlPort,
		TargetPort: intstr.FromString("sql"),
	}

	// Update existing named ports if their port number or target diverges from what the
	// parsed StatefulSet reported. Normalizing to named targets ("grpc"/"sql") is required
	// so the CockroachDB Operator's headless service can route traffic correctly.
	for i := range svc.Spec.Ports {
		switch svc.Spec.Ports[i].Name {
		case "grpc":
			grpcFound = true
			if svc.Spec.Ports[i].Port != grpcPort || svc.Spec.Ports[i].TargetPort.String() != "grpc" {
				svc.Spec.Ports[i] = grpcSvcPort
				needsUpdate = true
			}
		case "sql":
			sqlFound = true
			if svc.Spec.Ports[i].Port != sqlPort || svc.Spec.Ports[i].TargetPort.String() != "sql" {
				svc.Spec.Ports[i] = sqlSvcPort
				needsUpdate = true
			}
		}
	}

	// Add missing ports
	if !grpcFound {
		svc.Spec.Ports = append(svc.Spec.Ports, grpcSvcPort)
		needsUpdate = true
		logging.Infof(ctx, "Adding GRPC port (%d) to service %s", grpcPort, svcName)
	}

	if !sqlFound {
		svc.Spec.Ports = append(svc.Spec.Ports, sqlSvcPort)
		needsUpdate = true
		logging.Infof(ctx, "Adding SQL port (%d) to service %s", sqlPort, svcName)
	}

	if needsUpdate {
		// port updates are applied immediately, no need to wait for ClusterIP assignment
		if err := ctl.UpdateWithOptions(ctx, kube.UpdateOptions{NoWait: true}, svc); err != nil {
			return errors.Wrap(err, "failed to update service ports")
		}
		logging.Infof(ctx, "Updated public service %s with separate GRPC and SQL ports", svcName)
	} else {
		logging.Infof(ctx, "Service %s already has correct port configuration", svcName)
	}

	return nil
}

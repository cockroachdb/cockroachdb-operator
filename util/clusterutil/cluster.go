package clusterutil

import (
	"context"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/controllers/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kubeerrors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
)

func GetClusterForNode(
	ctx context.Context, ctl *kube.Ctl, node *crdbv1beta1.CrdbNode,
) (*crdbv1beta1.CrdbCluster, error) {
	var clusterName string
	for _, owner := range node.OwnerReferences {
		if owner.Kind == crdbv1beta1.CrdbClusterKind {
			clusterName = owner.Name
		}
	}
	if clusterName == "" {
		logging.Warnf(ctx, "node %s does not have an owning cluster", node.Name)
		metrics.CrdbNodeMissingCluster.WithLabelValues(node.Name, "missing").Inc()
		return nil, errors.Newf("unable to find owning CrdbCluster for CrdbNode %s/%s", node.Namespace, node.Name)
	}

	var crdbCluster crdbv1beta1.CrdbCluster
	if err := ctl.Get(ctx, clusterName, &crdbCluster); err != nil {
		if kubeerrors.IsNotFound(err) {
			return nil, errors.WithStack(err)
		}
		logging.Warnf(ctx, "unable to get owning CrdbCluster %s/%s for CrdbNode %s: %+v",
			node.Namespace, clusterName, node.Name, err)
		metrics.CrdbNodeMissingCluster.WithLabelValues(node.Name, "error").Inc()
		return nil, errors.Wrapf(err, "getting owning CrdbCluster %s/%s for CrdbNode %s",
			node.Namespace, clusterName, node.Name)
	}
	return &crdbCluster, nil
}

// IsClusterForNodeBeingDeleted reports whether the CrdbCluster that owns node
// is terminating (DeletionTimestamp is set) or has already been deleted.
func IsClusterForNodeBeingDeleted(
	ctx context.Context, ctl *kube.Ctl, node *crdbv1beta1.CrdbNode,
) (bool, error) {
	cluster, err := GetClusterForNode(ctx, ctl, node)
	if kubeerrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !cluster.DeletionTimestamp.IsZero(), nil
}

package betaclusterctrl

import (
	"context"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubenet"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/safesql"
)

// clearSettingSentinel is a value used to signify that a setting should be
// cleared (reset to default). This will be the value for all keys that are
// defined in the status, but no longer found in the spec.
const clearSettingSentinel = "DEFAULT"

// ClusterSettings handles diffing and applying cluster settings.
type ClusterSettings struct {
	Cluster *v1beta1.CrdbCluster
}

// Apply executes `SET CLUSTER SETTING` for each key
func (cs *ClusterSettings) Apply(ctx context.Context, ctl *kube.Ctl, opts kubenet.Options) error {
	db, err := kubenet.OpenDB(ctx, ctl, opts)
	if err != nil {
		return errors.Wrap(err, "opening DB connection")
	}
	defer db.Close()

	for k, v := range cs.Diff() {
		sql := safesql.Format("SET CLUSTER SETTING %1 = %2", safesql.Identifier(k), v)

		// reset the setting to the default value
		// https://www.cockroachlabs.com/docs/v21.2/set-cluster-setting#reset-a-setting-to-its-default-value
		if v == clearSettingSentinel {
			sql = safesql.Format("SET CLUSTER SETTING %1 = DEFAULT", safesql.Identifier(k))
		}

		if _, err = db.ExecContext(ctx, sql); err != nil {
			return errors.Wrapf(err, "writing cluster setting for %s", safesql.Identifier(k))
		}
	}

	return nil
}

// Diff returns settings (if any) that need to be updated in the database. This includes:
// * Spec.ClusterSettings contains keys not found in Status.Settings
// * Status.Settings contains keys not found in Spec.ClusterSettings
// * Values for matching keys are different
func (cs *ClusterSettings) Diff() map[string]string {
	settings := make(map[string]string)

	// add settings that aren't in the status or have different values
	for k, v := range cs.Cluster.Spec.ClusterSettings {
		found, ok := cs.Cluster.Status.Settings[k]
		if !ok || found != v {
			settings[k] = v
		}
	}

	// add any settings in status that aren't in the spec
	for k := range cs.Cluster.Status.Settings {
		if _, ok := cs.Cluster.Spec.ClusterSettings[k]; !ok {
			settings[k] = clearSettingSentinel
		}
	}

	return settings
}

// IsStale returns true if there are any settings that need to be set in the database.
func (cs *ClusterSettings) IsStale() bool {
	return len(cs.Diff()) > 0
}

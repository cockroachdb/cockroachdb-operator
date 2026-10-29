package clusterctrl

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	opmetrics "github.com/cockroachlabs/cockroach-enterprise-operator/controllers/metrics"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
)

const (
	// finalizationErrorLimit is the max number of errors FinalizeTenants can encounter before
	// terminating early.
	// TODO(alyshan): make configurable
	finalizationErrorLimit = 5
	// finalizationTimeout is the timeout used when performing finalization on an individual tenant.
	finalizationTimeout = time.Minute
)

// Finalizer encompasses logic to finalize tenants of a CRDB Host cluster.
// It ensures that at most one instance of its finalization processes are
// running.
type Finalizer struct {
	ctx context.Context
	ctl *kube.Ctl
	// numFinalizationErr is incremented every time an individual tenant finalization fails.
	numFinalizationErr uint32
	// mu is used to protect the isRunning field of Finalizer.
	// Ensuring that only one instance of the finalization goroutine is running.
	mu struct {
		sync.Mutex
		isRunning bool
	}
}

// NewFinalizer returns a Finalizer that can be used to
// perform finalization of tenants asynchronously via FinalizeTenants.
func NewFinalizer(ctx context.Context) *Finalizer {
	return &Finalizer{ctx: ctx}
}

// FinalizeTenants spawns a goroutine to perform finalization on the tenants specified.
// If too many errors are encountered during finalizations, the goroutine will exit early.
// FinalizeTenants will run at most one of these goroutines. If one is already running,
// an error will be returned.
func (f *Finalizer) FinalizeTenants(tenants []crdbv1beta1.CrdbTenant, ctl *kube.Ctl) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.mu.isRunning {
		return errors.New("Finalizer is currently running")
	}

	if len(tenants) == 0 {
		return nil
	}

	f.ctl = ctl
	f.mu.isRunning = true
	f.numFinalizationErr = 0
	go func() {
		logging.Info(f.ctx, "tenant finalization running")
		defer func() {
			f.mu.Lock()
			f.mu.isRunning = false
			f.mu.Unlock()
		}()

		for _, tenant := range tenants {
			// If too many finalizations are failing, exit early.
			// This could mean that there is a bug in the new CRDB major version,
			// so instead of continuing to wake up tenants to begin finalization, exit.
			if f.numFinalizationErr >= finalizationErrorLimit {
				break
			}
			f.finalizeTenant(tenant)
		}

		if f.numFinalizationErr > 0 {
			logging.Error(f.ctx, "some tenants failed to finalize")
		}
	}()

	return nil
}

// IsFinalizationRunning indicates whether the Finalizer is running.
func (f *Finalizer) IsFinalizationRunning() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mu.isRunning
}

// finalizeTenant runs `SET CLUSTER SETTING VERSION = '<major>.<minor>' on the provided tenant.
// It also sets the cluster-version annotation on the CrdbTenant so it can be easily determined
// if the tenant has been finalized. Note that any blocking calls should use timeouts.
func (f *Finalizer) finalizeTenant(t crdbv1beta1.CrdbTenant) {
	// Before setting the version on a tenant, we first check if the pool
	// that tenant is from has a minimum of 2/3 capacity available. This
	// throttles the finalization of tenants so that we don't wake up many
	// inactive tenants and exhaust the pools.
	if err := waitForPoolCapacity(f.ctx, f.ctl, *t.Spec.TenantPool); err != nil {
		logging.Errorf(f.ctx, "unexpected timeout waiting for pool capacity %v", *t.Spec.TenantPool)
		return
	}

	var clusterVersion string
	err := execSqlOnTenant(f.ctx, f.ctl, t.Spec.TenantID, func(ctx context.Context, db *sql.DB) error {
		ctx, cancel := context.WithTimeout(ctx, finalizationTimeout)
		defer cancel()
		logging.Infof(ctx, "setting cluster version on CrdbTenant %v", t.Name)
		if _, err := db.ExecContext(ctx, setClusterVersionQuery); err != nil {
			return errors.Wrap(err, "setting cluster version")
		}
		if err := db.QueryRowContext(ctx, getClusterVersionQuery).Scan(&clusterVersion); err != nil {
			return errors.Wrap(err, "getting cluster version")
		}
		return nil
	})
	if err != nil {
		logging.Errorf(f.ctx, "failed to finalize CrdbTenant %v: %v", t.Name, err)
		f.numFinalizationErr += 1
		return
	}

	if err := f.markTenantAsFinalized(t, clusterVersion); err != nil {
		logging.Errorf(f.ctx, "error applying annotation for CrdbTenant %v", t.Name)
		return
	}
	opmetrics.TenantsFinalized.Inc()
}

// isTenantFinalized returns true if the ClusterVersion annotation is present on the tenant
// and is set to version.
func isTenantFinalized(t crdbv1beta1.CrdbTenant, version string) bool {
	tenantClusterVersion, ok := t.Annotations[ClusterVersion]
	return ok && tenantClusterVersion == version
}

// markTenantAsFinalized sets the ClusterVersion annotation on the tenant to version.
func (f *Finalizer) markTenantAsFinalized(t crdbv1beta1.CrdbTenant, version string) error {
	// Get and Update the tenant via RetryConflicts since the CrdbTenant resource
	// may be stale.
	err := kube.RetryConflicts(func() error {
		if err := f.ctl.Get(f.ctx, t.Name, &t); err != nil {
			return err
		}
		t.Annotations[ClusterVersion] = version
		logging.Infof(f.ctx, "setting %v annotation on CrdbTenant %v", ClusterVersion, t.Name)
		return f.ctl.Update(f.ctx, &t)
	})
	return err
}

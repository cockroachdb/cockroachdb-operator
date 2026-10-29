package clusterctrl

import (
	"context"
	"database/sql"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubenet"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	ctrl "sigs.k8s.io/controller-runtime"
)

// requestHandler contains handler methods for a single cluster reconciliation
// request. State and methods do not need to be thread-safe.
type requestHandler struct {
	reconciler *Reconciler
	ctx        context.Context
	ctl        *kube.Ctl
	cluster    *crdbv1beta1.CrdbCluster
	res        ctrl.Result
	emit       *kube.EventEmitter
}

// RequeueAfter records the requeue-ing duration for this request. If this
// function is called multiple times, the lowest non-zero duration is recorded.
func (h *requestHandler) RequeueAfter(after time.Duration) {
	if after <= 0 {
		return
	}
	if h.res.RequeueAfter == 0 || after < h.res.RequeueAfter {
		h.res.RequeueAfter = after
	}
}

// Result returns the saved requeue-ing result to be returned from Reconcile.
// This is stored via one or more calls to RequeueAfter.
func (h *requestHandler) Result() ctrl.Result {
	return h.res
}

// queryRowSql executes the given SQL query directly against one of the pods of
// the given CRDB cluster.
func (h *requestHandler) queryRowSql(sql string, args ...any) (*sql.Row, error) {
	options := kubenet.Options{}

	db, err := kubenet.OpenDB(h.ctx, h.ctl, options)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	row := db.QueryRowContext(h.ctx, sql, args...)

	return row, row.Err()
}

// execSql executes the given SQL statement directly against one of the pods of
// the given CRDB cluster.
func (h *requestHandler) execSql(sql string, args ...any) error {
	options := kubenet.Options{}

	db, err := kubenet.OpenDB(h.ctx, h.ctl, options)
	if err != nil {
		return err
	}
	defer db.Close()

	_, err = db.ExecContext(h.ctx, sql, args...)
	if err != nil {
		return err
	}

	return nil
}

// execSqlOnTenant executes the given callback on a SQL pod for the given
// tenant. This may resume a tenant if there are no SQL pods for that tenant.
func (h *requestHandler) execSqlOnTenant(
	tenantID crdbv1beta1.TenantID, cb func(context.Context, *sql.DB) error,
) error {
	return execSqlOnTenant(h.ctx, h.ctl, tenantID, cb)
}

func execSqlOnTenant(
	ctx context.Context,
	ctl *kube.Ctl,
	tenantID crdbv1beta1.TenantID,
	cb func(context.Context, *sql.DB) error,
) error {
	options := kubenet.Options{
		TenantID: tenantID,
	}

	db, err := kubenet.OpenDB(ctx, ctl, options)
	if err != nil {
		return err
	}
	defer db.Close()

	return cb(ctx, db.DB)
}

// apply is a helper method to apply CRDB cluster resources using kube.Ctl.
func (h *requestHandler) apply(desc string, objects ...kube.Object) error {
	// TODO(cc-intrusion): Consider setting NoWait=true when applying these
	// resources. We should never block a reconciliation goroutine on a
	// potentially long-running operation.
	//
	// See CC-5697.
	if err := h.ctl.Apply(h.ctx, objects...); err != nil {
		logging.Errorf(h.ctx, "failed to apply %s: %v", desc, err)
		return err
	}
	return nil
}

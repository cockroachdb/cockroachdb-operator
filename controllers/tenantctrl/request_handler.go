package tenantctrl

import (
	"context"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubenet"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	ctrl "sigs.k8s.io/controller-runtime"
)

// requestHandler contains handler methods for a single tenant reconciliation
// request. State and methods do not need to be thread-safe.
type requestHandler struct {
	reconciler *Reconciler
	ctx        context.Context
	ctl        *kube.Ctl
	tenant     *crdbv1beta1.CrdbTenant
	res        ctrl.Result
	emit       *kube.EventEmitter

	testingKnobs struct {
		scaleTenant func(ctx context.Context, tenant *crdbv1beta1.CrdbTenant) (bool, time.Duration, error)
	}
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

// execSqlOnHost executes the given SQL statement directly against one of the
// CRDB pods for the host.
func (h *requestHandler) execSqlOnHost(sql string, args ...any) error {
	options := kubenet.Options{}

	db, err := kubenet.OpenDB(h.ctx, h.ctl, options)
	if err != nil {
		return err
	}
	defer db.Close()

	if _, err := db.ExecContext(h.ctx, sql, args...); err != nil {
		return err
	}

	return nil
}

// apply is a helper method to apply CrdbTenant resources using kube.Ctl.
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

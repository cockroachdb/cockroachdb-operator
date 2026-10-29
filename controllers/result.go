package controllers

import (
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

// RequeueIfError instructs the controller manager to re-run reconciliation if
// the given error is not nil.
func RequeueIfError(err error) (ctrl.Result, error) {
	return ctrl.Result{}, err
}

// NoRequeue instructs the controller manager that reconciliation is complete
// and does not need to be re-run.
func NoRequeue() (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

// RequeueOnDependent instructs the controller manager that reconciliation is
// to be halted until a change to a dependent resource re-triggers
// reconciliation.
func RequeueOnDependent() (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

// RequeueAfter instructs the controller manager to re-run reconciliation after
// the given delay. This is used when waiting for some background reconciliation
// operation to complete.
func RequeueAfter(interval time.Duration, err error) (ctrl.Result, error) {
	return ctrl.Result{RequeueAfter: interval}, err
}

// RequeueImmediately instructs the controller manager to re-run reconciliation
// right away. This is useful when a reconciled object's state has been changed
// by the controller and needs to be reconciled (and using RequeueImmediately
// avoids recursion/re-entrancy).
func RequeueImmediately() (ctrl.Result, error) {
	return ctrl.Result{Requeue: true}, nil
}

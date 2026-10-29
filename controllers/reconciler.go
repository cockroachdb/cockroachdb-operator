package controllers

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
)

type Reconciler interface {
	// SetTestMode notifies this Reconciler that is will be executing in the
	// context of a kuberecord test and provides a hook to make any behavior
	// adjustments, should they be required.
	SetTestMode(bool)

	// SetupWithManager initializes the Kubernetes watch used by this
	// Reconciler.
	SetupWithManager(context.Context, ctrl.Manager) error

	// Reconcile ensures that the actual state of a given resource matches the
	// expected state of a given resource.
	//
	// Reconcile is expected to be short running (< 1s). Should retries need to
	// be performed, leverage the RequeueHelpers. Should certain behaviors or
	// changes need to be observed, consider augmenting the watch in
	// SetupWithManager or launching a background goroutine.
	Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
}

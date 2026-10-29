package backoff

import (
	"context"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
)

type PermanentError = backoff.PermanentError

// Permanent is an alias for backoff.Permanent.
var Permanent = backoff.Permanent

// Opts are the configurable values for the backoff.
type Opts struct {
	// Timeout is the time duration from the start of the first f() call, after
	// which f() will no longer be retried and an error will be returned. If
	// Timeout is 0, the backoff will run forever.
	Timeout time.Duration
	// MaxInterval is the maximium time interval between retry attempts. If not
	// set, it defaults to 10 seconds or InitialInterval, whichever is greater.
	MaxInterval time.Duration
	// InitialInterval is the time duration between the first and second
	// attempts. If not set, it defaults to 500ms or MaxInterval, whichever is
	// less.
	InitialInterval time.Duration
	// NotifyInterval is the interval used to periodically log errors. The
	// first error is always logged. If not set, it defaults to 10 seconds. If
	// the caller wants to log every error, they can set NotifyInterval less
	// than InitialInterval.
	NotifyInterval time.Duration
}

// Run runs f() until the function succeeds or either opts.Timeout transpires
// or the context is cancelled. Run periodically logs the error returned by
// f(). Run will execute f() immediately then wait for opts.InitialInterval.
func Run(ctx context.Context, f func() error, opts *Opts) error {
	b := makeBackoff(opts)
	notify := makeNotify(ctx, opts)
	return backoff.RetryNotify(f, backoff.WithContext(b, ctx), notify)
}

func makeBackoff(opts *Opts) *backoff.ExponentialBackOff {
	if opts.MaxInterval == time.Duration(0) {
		opts.MaxInterval = max(10*time.Second, opts.InitialInterval)
	}

	if opts.InitialInterval == time.Duration(0) {
		opts.InitialInterval = min(opts.MaxInterval, 500*time.Millisecond)
	}

	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = opts.Timeout
	b.MaxInterval = opts.MaxInterval
	b.InitialInterval = opts.InitialInterval
	return b
}

func makeNotify(ctx context.Context, opts *Opts) func(error, time.Duration) {
	if opts.NotifyInterval == time.Duration(0) {
		opts.NotifyInterval = 10 * time.Second
	}

	// Next is initialized to a past time to ensure the first error is
	// always logged.
	next := time.Now().Add(-opts.NotifyInterval)
	return func(err error, _ time.Duration) {
		if time.Now().After(next) {
			logging.Debugf(ctx, "%v", err)
			next = time.Now().Add(opts.NotifyInterval)
		}
	}
}

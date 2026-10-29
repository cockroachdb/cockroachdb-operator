package kubeerrors

import (
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors" //nolint:depguard
)

func errPermutations(err error) []error {
	// NB: errors.Mark does not appear to work here.
	return []error{
		err,
		errors.WithStack(err),
		errors.Wrap(err, "Some context"),
		errors.Wrap(errors.Wrap(err, "Some context"), "even more"),
		errors.WithStack(errors.Wrap(err, "Some context")),
	}
}

func errTestCase(t *testing.T, status string, actual error, isFunc func(error) bool) {
	t.Run(status, func(t *testing.T) {
		for _, wrapped := range errPermutations(actual) {
			require.True(t, isFunc(wrapped), "%+v should be detected as %s", wrapped, status)
		}
		for _, wrapped := range errPermutations(errors.New("misnomer")) {
			require.False(t, isFunc(wrapped), "%+v should not be detected as %s", wrapped, status)
		}
	})
}

func TestIs(t *testing.T) {
	// Commented out cases are difficult to construct but the logic is the same for all error types.
	errTestCase(
		t, "NotFound", k8serrors.NewNotFound(corev1.Resource("service"), "foo"), IsNotFound,
	)
	errTestCase(
		t, "AlreadyExists", k8serrors.NewAlreadyExists(corev1.Resource("service"), "foo"), IsAlreadyExists,
	)
	errTestCase(
		t, "Conflict", k8serrors.NewConflict(corev1.Resource("service"), "foo", errors.New("because")), IsConflict,
	)
	// errTestCase(
	// t, "Invalid", k8serrors.NewInvalid(corev1.Ki, "foo", nil), IsInvalid,
	// )
	errTestCase(
		t, "Gone", k8serrors.NewGone("bye bye"), IsGone,
	)
	errTestCase(
		t, "ResourceExpired", k8serrors.NewResourceExpired("bye bye"), IsResourceExpired,
	)
	errTestCase(
		t, "Gone", k8serrors.NewResourceExpired("bye bye"), IsResourceExpired,
	)
	// errTestCase(
	// 	t, "NotAcceptable", k8serrors.("bye bye"), IsResourceExpired,
	// )
	// errTestCase(
	// 	t, "UnsupportedMediaType", k8serrors.NewU("bye bye"), IsResourceExpired,
	// )
	errTestCase(
		t, "MethodNotSupported", k8serrors.NewMethodNotSupported(corev1.Resource("service"), "fix"), IsMethodNotSupported,
	)
	errTestCase(
		t, "ServiceUnavailable", k8serrors.NewServiceUnavailable("is borked"), IsServiceUnavailable,
	)
	errTestCase(
		t, "BadRequest", k8serrors.NewBadRequest("no good"), IsBadRequest,
	)
	errTestCase(
		t, "Unauthorized", k8serrors.NewUnauthorized("u g l y"), IsUnauthorized,
	)
	errTestCase(
		t, "Forbidden", k8serrors.NewForbidden(corev1.Resource("service"), "the way", errors.New("it is")), IsForbidden,
	)
	errTestCase(
		t, "Timeout", k8serrors.NewTimeoutError("bed time", 100), IsTimeout,
	)
	errTestCase(
		t, "ServerTimeout", k8serrors.NewServerTimeout(corev1.Resource("service"), "fixing", 100), IsServerTimeout,
	)
	errTestCase(
		t, "InternalError", k8serrors.NewInternalError(errors.New("eye dee ten tee")), IsInternalError,
	)
	errTestCase(
		t, "WebhookValidationError", k8serrors.NewBadRequest("webhook says no"), IsWebhookValidationError,
	)
}

package kubeerrors

import (
	"net/http"

	"github.com/cockroachdb/errors"
	k8serrors "k8s.io/apimachinery/pkg/api/errors" // nolint:importcheck
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	NewAlreadyExists = k8serrors.NewAlreadyExists
	NewConflict      = k8serrors.NewConflict
	NewNotFound      = k8serrors.NewNotFound
)

// IsWebhookValidationError returns true if the error indicates a bad request (code 400).
// Note that even though the webhook returns a k8serrors.StatusError it only provides
// the metav1.Status.Code and not a metav1.Status.Reason.
func IsWebhookValidationError(err error) bool {
	statusErr := k8serrors.APIStatus(nil)
	ok := errors.As(err, &statusErr)
	return ok && statusErr.Status().Code == http.StatusBadRequest
}

func ReasonForError(err error) metav1.StatusReason {
	if status := k8serrors.APIStatus(nil); errors.As(err, &status) {
		return status.Status().Reason
	}

	return metav1.StatusReasonUnknown
}

// IsNotFound returns true if the specified error was created by NewNotFound.
func IsNotFound(err error) bool {
	return ReasonForError(err) == metav1.StatusReasonNotFound
}

// IsAlreadyExists determines if the err is an error which indicates that a specified resource already exists.
func IsAlreadyExists(err error) bool {
	return ReasonForError(err) == metav1.StatusReasonAlreadyExists
}

// IsConflict determines if the err is an error which indicates the provided update conflicts.
func IsConflict(err error) bool {
	return ReasonForError(err) == metav1.StatusReasonConflict
}

// IsInvalid determines if the err is an error which indicates the provided resource is not valid.
func IsInvalid(err error) bool {
	return ReasonForError(err) == metav1.StatusReasonInvalid
}

// IsGone is true if the error indicates the requested resource is no longer available.
func IsGone(err error) bool {
	return ReasonForError(err) == metav1.StatusReasonGone
}

// IsResourceExpired is true if the error indicates the resource has expired and the current action is
// no longer possible.
func IsResourceExpired(err error) bool {
	return ReasonForError(err) == metav1.StatusReasonExpired
}

// IsNotAcceptable determines if err is an error which indicates that the request failed due to an invalid Accept header
func IsNotAcceptable(err error) bool {
	return ReasonForError(err) == metav1.StatusReasonNotAcceptable
}

// IsUnsupportedMediaType determines if err is an error which indicates that the request failed due to an invalid Content-Type header
func IsUnsupportedMediaType(err error) bool {
	return ReasonForError(err) == metav1.StatusReasonUnsupportedMediaType
}

// IsMethodNotSupported determines if the err is an error which indicates the provided action could not
// be performed because it is not supported by the server.
func IsMethodNotSupported(err error) bool {
	return ReasonForError(err) == metav1.StatusReasonMethodNotAllowed
}

// IsServiceUnavailable is true if the error indicates the underlying service is no longer available.
func IsServiceUnavailable(err error) bool {
	return ReasonForError(err) == metav1.StatusReasonServiceUnavailable
}

// IsBadRequest determines if err is an error which indicates that the request is invalid.
func IsBadRequest(err error) bool {
	return ReasonForError(err) == metav1.StatusReasonBadRequest
}

// IsUnauthorized determines if err is an error which indicates that the request is unauthorized and
// requires authentication by the user.
func IsUnauthorized(err error) bool {
	return ReasonForError(err) == metav1.StatusReasonUnauthorized
}

// IsForbidden determines if err is an error which indicates that the request is forbidden and cannot
// be completed as requested.
func IsForbidden(err error) bool {
	return ReasonForError(err) == metav1.StatusReasonForbidden
}

// IsTimeout determines if err is an error which indicates that request times out due to long
// processing.
func IsTimeout(err error) bool {
	return ReasonForError(err) == metav1.StatusReasonTimeout
}

// IsServerTimeout determines if err is an error which indicates that the request needs to be retried
// by the client.
func IsServerTimeout(err error) bool {
	return ReasonForError(err) == metav1.StatusReasonServerTimeout
}

// IsInternalError determines if err is an error which indicates an internal server error.
func IsInternalError(err error) bool {
	return ReasonForError(err) == metav1.StatusReasonInternalError
}

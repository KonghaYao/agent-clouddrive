// Package domain contains deterministic CloudDrive business rules. PostgreSQL
// remains the authority; these helpers never store process-local business state.
package domain

import (
	"errors"
	"fmt"
	"net/http"
)

type Code string

const (
	Unauthenticated       Code = "UNAUTHENTICATED"
	InsufficientScope     Code = "INSUFFICIENT_SCOPE"
	AccessDenied          Code = "ACCESS_DENIED"
	NotFound              Code = "NOT_FOUND"
	VersionConflict       Code = "VERSION_CONFLICT"
	PathConflict          Code = "PATH_CONFLICT"
	OperationPending      Code = "OPERATION_PENDING"
	IdempotencyConflict   Code = "IDEMPOTENCY_CONFLICT"
	UploadTooLarge        Code = "UPLOAD_TOO_LARGE"
	QuotaExceeded         Code = "QUOTA_EXCEEDED"
	InvalidPath           Code = "INVALID_PATH"
	InvalidArgument       Code = "INVALID_ARGUMENT"
	InvalidTransition     Code = "INVALID_TRANSITION"
	ConfirmationRequired  Code = "CONFIRMATION_REQUIRED"
	ConfirmationExpired   Code = "CONFIRMATION_EXPIRED"
	LeaseLost             Code = "LEASE_LOST"
	PublicationSuperseded Code = "PUBLICATION_SUPERSEDED"
	DependencyUnavailable Code = "DEPENDENCY_UNAVAILABLE"
	RateLimited           Code = "RATE_LIMITED"
)

type Error struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string                  { return fmt.Sprintf("%s: %s", e.Code, e.Message) }
func (e *Error) Is(target error) bool           { t, ok := target.(*Error); return ok && e.Code == t.Code }
func NewError(code Code, message string) *Error { return &Error{Code: code, Message: message} }
func ErrorCode(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "INTERNAL_ERROR"
}
func HTTPStatus(err error) int {
	switch ErrorCode(err) {
	case Unauthenticated:
		return http.StatusUnauthorized
	case InsufficientScope, AccessDenied, ConfirmationRequired:
		return http.StatusForbidden
	case NotFound:
		return http.StatusNotFound
	case VersionConflict, PathConflict, OperationPending, IdempotencyConflict, InvalidTransition, LeaseLost, PublicationSuperseded:
		return http.StatusConflict
	case ConfirmationExpired:
		return http.StatusGone
	case UploadTooLarge:
		return http.StatusRequestEntityTooLarge
	case QuotaExceeded, InvalidPath, InvalidArgument:
		return http.StatusUnprocessableEntity
	case RateLimited:
		return http.StatusTooManyRequests
	case DependencyUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

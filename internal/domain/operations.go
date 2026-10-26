package domain

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
)

type OperationState string

const (
	OperationPrepared        OperationState = "prepared"
	OperationExecuting       OperationState = "executing"
	OperationRemoteConfirmed OperationState = "remote_confirmed"
	OperationOutcomeUnknown  OperationState = "outcome_unknown"
	OperationCommitted       OperationState = "committed"
	OperationFailedConfirmed OperationState = "failed_confirmed"
)

func ValidateOperationTransition(from, to OperationState) error {
	allowed := false
	switch from {
	case OperationPrepared:
		allowed = to == OperationExecuting || to == OperationFailedConfirmed
	case OperationExecuting:
		allowed = to == OperationRemoteConfirmed || to == OperationOutcomeUnknown || to == OperationFailedConfirmed
	case OperationOutcomeUnknown:
		allowed = to == OperationRemoteConfirmed || to == OperationFailedConfirmed
	case OperationRemoteConfirmed:
		allowed = to == OperationCommitted
	}
	if !allowed {
		return NewError(InvalidTransition, "operation state transition is not permitted")
	}
	return nil
}
func IsOperationTerminal(s OperationState) bool {
	return s == OperationCommitted || s == OperationFailedConfirmed
}
func CanReleaseReservation(s OperationState) bool       { return s == OperationFailedConfirmed }
func BlocksConflictingOperations(s OperationState) bool { return !IsOperationTerminal(s) }

// RequestHash yields canonical JSON for Go structs/maps (encoding/json sorts map
// keys). Hash parsed/validated input, not an unvalidated incoming byte stream.
func RequestHash(request any) (string, error) {
	b, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
func CheckIdempotency(existingHash, requestedHash string) error {
	if existingHash == "" || requestedHash == "" || subtle.ConstantTimeCompare([]byte(existingHash), []byte(requestedHash)) != 1 {
		return NewError(IdempotencyConflict, "idempotency key is already bound to a different request")
	}
	return nil
}

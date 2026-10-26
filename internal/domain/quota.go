package domain

import "math"

type QuotaAccount struct {
	LimitBytes    int64 `json:"limit_bytes"`
	UsedBytes     int64 `json:"used_bytes"`
	ReservedBytes int64 `json:"reserved_bytes"`
}

func (q QuotaAccount) Validate() error {
	if q.LimitBytes < 0 || q.UsedBytes < 0 || q.ReservedBytes < 0 || q.UsedBytes > math.MaxInt64-q.ReservedBytes {
		return NewError(InvalidArgument, "invalid quota accounting")
	}
	return nil
}

func ReservationBytes(oldSize, newSize int64) (int64, error) {
	if oldSize < 0 || newSize < 0 {
		return 0, NewError(InvalidArgument, "file sizes cannot be negative")
	}
	if newSize > oldSize {
		return newSize - oldSize, nil
	}
	return 0, nil
}

// ReserveQuota is a calculation helper, not an in-memory reservation authority.
// Apply the same check in one PostgreSQL row-lock transaction.
func ReserveQuota(q QuotaAccount, bytes int64) (QuotaAccount, error) {
	if err := q.Validate(); err != nil {
		return q, err
	}
	if bytes < 0 {
		return q, NewError(InvalidArgument, "reservation cannot be negative")
	}
	if bytes == 0 {
		return q, nil
	}
	if q.UsedBytes > q.LimitBytes || q.ReservedBytes > q.LimitBytes-q.UsedBytes || bytes > q.LimitBytes-q.UsedBytes-q.ReservedBytes {
		return q, NewError(QuotaExceeded, "logical quota would be exceeded")
	}
	q.ReservedBytes += bytes
	return q, nil
}

// CommitQuota releases exactly this operation's reservation and applies the
// latest-version size delta. Trash is intentionally not a parameter: deletion
// and restoration do not change the owner's logical usage.
func CommitQuota(q QuotaAccount, oldSize, newSize, reserved int64) (QuotaAccount, error) {
	if err := q.Validate(); err != nil {
		return q, err
	}
	required, err := ReservationBytes(oldSize, newSize)
	if err != nil {
		return q, err
	}
	if reserved < required || reserved > q.ReservedBytes || oldSize > q.UsedBytes {
		return q, NewError(InvalidArgument, "commit does not match the locked quota reservation")
	}
	base := q.UsedBytes - oldSize
	if newSize > math.MaxInt64-base {
		return q, NewError(QuotaExceeded, "quota arithmetic overflow")
	}
	next := q
	next.UsedBytes = base + newSize
	next.ReservedBytes -= reserved
	// Shrinking an existing file remains possible after an administrator lowered
	// the quota below current usage. Growth requires a capacity-backed reservation.
	if next.UsedBytes > q.UsedBytes && (next.UsedBytes > next.LimitBytes || next.ReservedBytes > next.LimitBytes-next.UsedBytes) {
		return q, NewError(QuotaExceeded, "logical quota would be exceeded")
	}
	return next, nil
}

func ReleaseQuota(q QuotaAccount, bytes int64, state OperationState) (QuotaAccount, error) {
	if err := q.Validate(); err != nil {
		return q, err
	}
	if !CanReleaseReservation(state) {
		return q, NewError(OperationPending, "unknown remote outcomes must be reconciled before releasing quota")
	}
	if bytes < 0 || bytes > q.ReservedBytes {
		return q, NewError(InvalidArgument, "invalid reservation release")
	}
	q.ReservedBytes -= bytes
	return q, nil
}

package core

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
)

func UUID(s string) pgtype.UUID { var u pgtype.UUID; _ = u.Scan(s); return u }
func UUIDText(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
func ContentHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

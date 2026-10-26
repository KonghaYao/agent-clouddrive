package core

import (
	"context"
	"encoding/json"
)

func One(ctx context.Context, q Querier, sql string, args ...any) (Object, error) {
	var b []byte
	e := q.QueryRow(ctx, sql, args...).Scan(&b)
	if e != nil {
		return nil, NormalizeError(e)
	}
	var m Object
	e = json.Unmarshal(b, &m)
	return m, e
}
func Many(ctx context.Context, q Querier, sql string, args ...any) ([]Object, error) {
	rows, e := q.Query(ctx, sql, args...)
	if e != nil {
		return nil, NormalizeError(e)
	}
	defer rows.Close()
	out := []Object{}
	for rows.Next() {
		var b []byte
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		var m Object
		if e = json.Unmarshal(b, &m); e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

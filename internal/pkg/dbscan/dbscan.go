// Package dbscan is the one way send-path code reads a result set it will ACT on.
//
// The defect it exists to kill (2026-11-18, 7-hour send stall): a cursor read under a
// deadline stops early, rows.Next() returns false, nobody checks rows.Err(), and the
// partial slice is treated as the complete truth — campaigns "no longer sending" are
// evicted, remaining counts read as zero, suppression sets are partial.
//
// Contract: Collect returns EITHER the complete result set and a nil error, OR a nil
// slice and a non-nil error. It never returns a partial slice. The cursor is always
// closed before Collect returns, so callers do their per-row work (further queries,
// state changes) AFTER the read, never inside it.
//
// Caller rule: on error, keep the previous state (last known good) and retry on the
// next tick. Never run a destructive or reconciling pass on an error.
package dbscan

import (
	"database/sql"
	"fmt"
)

// Rows is the subset of *sql.Rows Collect needs (lets tests pass fakes).
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// Collect drains rows through scan. Complete-or-error; see the package comment.
func Collect[T any](rows Rows, scan func(Rows) (T, error)) ([]T, error) {
	if rows == nil {
		return nil, fmt.Errorf("dbscan: nil rows")
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("dbscan: scan row %d: %w", len(out)+1, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dbscan: read stopped after %d rows: %w", len(out), err)
	}
	return out, nil
}

// Query runs q and collects it. A query error is returned as-is (still complete-or-error).
func Query[T any](rows *sql.Rows, qerr error, scan func(Rows) (T, error)) ([]T, error) {
	if qerr != nil {
		return nil, qerr
	}
	return Collect[T](rows, scan)
}

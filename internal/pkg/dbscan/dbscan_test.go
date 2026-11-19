package dbscan

import (
	"errors"
	"testing"
)

type fake struct {
	n, i    int
	failAt  int // Scan fails on this 1-based row (0 = never)
	errAt   int // Next() stops before this 1-based row and Err() is set (0 = never)
	closed  bool
	stopped bool
}

func (f *fake) Next() bool {
	if f.errAt > 0 && f.i+1 == f.errAt {
		f.stopped = true
		return false
	}
	if f.i >= f.n {
		return false
	}
	f.i++
	return true
}
func (f *fake) Scan(dest ...any) error {
	if f.failAt == f.i {
		return errors.New("bad row")
	}
	*(dest[0].(*int)) = f.i
	return nil
}
func (f *fake) Err() error {
	if f.stopped {
		return errors.New("context deadline exceeded")
	}
	return nil
}
func (f *fake) Close() error { f.closed = true; return nil }

func scanInt(r Rows) (int, error) { var v int; return v, r.Scan(&v) }

func TestCollectComplete(t *testing.T) {
	f := &fake{n: 3}
	got, err := Collect[int](f, scanInt)
	if err != nil || len(got) != 3 || !f.closed {
		t.Fatalf("got %v err %v closed %v", got, err, f.closed)
	}
}

// The 2026-11-18 shape: the deadline hits mid-scan. No partial slice may come back.
func TestCollectTruncatedReturnsNoRows(t *testing.T) {
	f := &fake{n: 10, errAt: 4}
	got, err := Collect[int](f, scanInt)
	if err == nil || got != nil || !f.closed {
		t.Fatalf("truncated read must be (nil, err), closed; got %v err %v closed %v", got, err, f.closed)
	}
}

func TestCollectScanErrorReturnsNoRows(t *testing.T) {
	f := &fake{n: 5, failAt: 2}
	got, err := Collect[int](f, scanInt)
	if err == nil || got != nil || !f.closed {
		t.Fatalf("scan error must be (nil, err), closed; got %v err %v", got, err)
	}
}

func TestQueryErrorAndNil(t *testing.T) {
	if _, err := Query[int](nil, errors.New("boom"), scanInt); err == nil {
		t.Fatal("query error must propagate")
	}
	if _, err := Collect[int](nil, scanInt); err == nil {
		t.Fatal("nil rows must error")
	}
}

package worker

import (
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

var poolCols = []string{"id", "hostname", "status", "limit", "sent", "pool_name", "ip"}

// A pool read cut short (rows.Err) must not be cached as an empty pool: loadedAt stays
// zero, next() reports "not loaded" (transient), and the following refresh re-reads.
func TestVMTAPool_AbortedReadIsNotCachedAsEmpty(t *testing.T) {
	p, mock := newEmptyPoolMock(t)
	mock.ExpectQuery(`SELECT ip.id, ip.hostname, ip.status`).
		WillReturnRows(sqlmock.NewRows(poolCols).
			AddRow("ip1", "mta1.example.com", "active", 0, 0, "db-gmail-pool", "1.2.3.4").
			RowError(0, errors.New("canceling statement due to statement timeout")))

	p.refresh(t.Context(), "prof-1")
	if !p.loadedAt.IsZero() {
		t.Fatal("aborted read stamped loadedAt — empty pool would be cached for the TTL")
	}
	_, err := p.next("gmail")
	if !errors.Is(err, errVMTAPoolNotLoaded) {
		t.Fatalf("want errVMTAPoolNotLoaded, got %v", err)
	}
	if !IsPMTATransient("profile prof-1: " + err.Error()) {
		t.Fatal("not-loaded error must classify as PMTA transient (requeue, not terminal)")
	}

	// Next Send retries the read and succeeds.
	mock.ExpectQuery(`SELECT ip.id, ip.hostname, ip.status`).
		WillReturnRows(sqlmock.NewRows(poolCols).
			AddRow("ip1", "mta1.example.com", "active", 0, 0, "db-gmail-pool", "1.2.3.4"))
	mock.ExpectQuery(`SELECT pool.name\s+FROM mailing_ip_pool_preferences`).
		WillReturnRows(sqlmock.NewRows([]string{"name"}))
	p.refresh(t.Context(), "prof-1")
	if p.loadedAt.IsZero() {
		t.Fatal("successful re-read did not load")
	}
	if _, err := p.next("gmail"); err != nil {
		t.Fatalf("next after good read: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// An aborted re-read keeps the previous IP set instead of wiping it.
func TestVMTAPool_AbortedReReadKeepsPriorIPs(t *testing.T) {
	p, mock := newEmptyPoolMock(t)
	mock.ExpectQuery(`SELECT ip.id, ip.hostname, ip.status`).
		WillReturnRows(sqlmock.NewRows(poolCols).
			AddRow("ip1", "mta1.example.com", "active", 0, 0, "db-gmail-pool", "1.2.3.4"))
	mock.ExpectQuery(`SELECT pool.name\s+FROM mailing_ip_pool_preferences`).
		WillReturnRows(sqlmock.NewRows([]string{"name"}))
	p.refresh(t.Context(), "prof-1")
	p.loadedAt = p.loadedAt.Add(-2 * p.ttl) // force stale

	mock.ExpectQuery(`SELECT ip.id, ip.hostname, ip.status`).
		WillReturnRows(sqlmock.NewRows(poolCols).
			AddRow("ip1", "mta1.example.com", "active", 0, 0, "db-gmail-pool", "1.2.3.4").
			RowError(0, errors.New("driver: bad connection")))
	p.refresh(t.Context(), "prof-1")
	if _, err := p.next("gmail"); err != nil {
		t.Fatalf("prior IPs lost after aborted re-read: %v", err)
	}
}

// A genuinely empty, completed read is still an empty pool (terminal, not transient).
func TestVMTAPool_CompletedEmptyReadIsEmpty(t *testing.T) {
	p, mock := newEmptyPoolMock(t)
	expectEmptyPoolLoad(mock)
	p.refresh(t.Context(), "prof-1")
	_, err := p.next("gmail")
	if err == nil || errors.Is(err, errVMTAPoolNotLoaded) {
		t.Fatalf("completed empty read must be 'no IPs', got %v", err)
	}
}

package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newLeaseTestScheduler builds a scheduler whose completion scan is the only
// thing that touches the DB; the first completion query fails so the pass
// returns right after proving it ran.
func newLeaseTestScheduler(t *testing.T, rdb *redis.Client) (*CampaignScheduler, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	cs := NewCampaignScheduler(db)
	cs.redisClient = rdb
	cs.ctx, cs.cancel = context.WithCancel(context.Background())
	t.Cleanup(cs.cancel)
	return cs, mock
}

func expectCompletionScan(mock sqlmock.Sqlmock) {
	// checkCompletedCampaigns returns on its first query error.
	mock.ExpectQuery(`FROM mailing_campaigns c`).WillReturnError(errors.New("stop after first query"))
}

// (a) the lease holder runs the completion scan and keeps the lease.
func TestCompletionLease_HolderRunsScan(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	cs, mock := newLeaseTestScheduler(t, rdb)
	expectCompletionScan(mock)

	if ran := cs.checkCompletedCampaignsLeased(); !ran {
		t.Fatal("holder did not run the completion scan")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("completion scan not issued: %v", err)
	}
	if !mr.Exists("lock:" + completionLeaseKey) {
		t.Fatal("lease must be held after the pass (not released)")
	}
	if ttl := mr.TTL("lock:" + completionLeaseKey); ttl <= 0 || ttl > completionLease {
		t.Fatalf("lease ttl = %v, want (0, %v]", ttl, completionLease)
	}
}

// (b) a replica inside another's lease issues NO query.
func TestCompletionLease_NonHolderSkipsWithoutQuery(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	if err := mr.Set("lock:"+completionLeaseKey, "other-replica"); err != nil {
		t.Fatal(err)
	}
	cs, mock := newLeaseTestScheduler(t, rdb) // no expectations: any query fails the test below

	if ran := cs.checkCompletedCampaignsLeased(); ran {
		t.Fatal("non-holder ran the completion scan")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected DB activity: %v", err)
	}
}

// (c) no Redis, or Redis down → runs locally exactly as before.
func TestCompletionLease_NoRedisRunsLocally(t *testing.T) {
	cs, mock := newLeaseTestScheduler(t, nil)
	expectCompletionScan(mock)
	if ran := cs.checkCompletedCampaignsLeased(); !ran {
		t.Fatal("nil redis must run the scan")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("completion scan not issued: %v", err)
	}
}

func TestCompletionLease_RedisDownRunsLocally(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	defer rdb.Close()
	mr.Close()
	cs, mock := newLeaseTestScheduler(t, rdb)
	expectCompletionScan(mock)
	if ran := cs.checkCompletedCampaignsLeased(); !ran {
		t.Fatal("a Redis fault must fall back to running the scan")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("completion scan not issued: %v", err)
	}
}

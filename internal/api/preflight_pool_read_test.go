package api

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

const preflightOrg = "00000000-0000-0000-0000-000000000001"

func expectRRGOProfile(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`(?s)FROM mailing_sending_profiles`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "ip_pool", "pool_prefix", "via_ses", "cs", "tn"}).
			AddRow("11111111-1111-1111-1111-111111111111", "", "rrgo", false, nil, nil))
}

func checks(res preflightResult) map[string]bool {
	m := map[string]bool{}
	for _, e := range res.Errors {
		m[e.Check] = true
	}
	return m
}

// 2026-11-10 21:14Z: an aborted pool read came back as "ip_pool_empty" while the pool's one IP was active.
func TestPreflight_AbortedPoolReadIsAQueryErrorNeverAnEmptyPool(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expectRRGOProfile(mock)
	for i := 0; i < 2; i++ { // the read is retried once
		mock.ExpectQuery(`(?s)FROM mailing_ip_addresses ip.*pool.name LIKE`).WithArgs("rrgo").
			WillReturnRows(sqlmock.NewRows([]string{"hostname", "status", "name"}).
				AddRow("mta-rr-gn3.mail.em.refinanceratesusa.com", "active", "rrgo-general-pool").
				RowError(0, errors.New("canceling statement due to statement timeout")))
	}
	res := preflightDeployCheck(context.Background(), db, preflightOrg, "go.refinanceratesusa.com", "")
	c := checks(res)
	if res.OK || !c["ip_pool_query"] || c["ip_pool_empty"] {
		t.Fatalf("want ip_pool_query (not ip_pool_empty), got %+v", res.Errors)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPreflight_PoolReadRetrySucceeds(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expectRRGOProfile(mock)
	mock.ExpectQuery(`(?s)FROM mailing_ip_addresses ip`).WithArgs("rrgo").
		WillReturnError(errors.New("driver: bad connection"))
	mock.ExpectQuery(`(?s)FROM mailing_ip_addresses ip`).WithArgs("rrgo").
		WillReturnRows(sqlmock.NewRows([]string{"hostname", "status", "name"}).
			AddRow("mta-rr-gn3.mail.em.refinanceratesusa.com", "active", "rrgo-general-pool"))
	res := preflightDeployCheck(context.Background(), db, preflightOrg, "go.refinanceratesusa.com", "")
	c := checks(res)
	if c["ip_pool_query"] || c["ip_pool_empty"] {
		t.Fatalf("a retried read with one active IP must pass the pool check, got %+v", res.Errors)
	}
}

package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// A campaign scan that ends on an error (e.g. the 5s ctx expiring mid-cursor)
// must NOT evict tracked campaigns it did not reach (incident 8cd92d2484).
func TestRefreshISPCampaigns_TruncatedScanKeepsState(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	p := &SendWorkerPool{db: db, ctx: context.Background(), batchSize: 100}

	rows := sqlmock.NewRows([]string{"id", "q"}).
		AddRow("camp-a", `{"isp_quotas":[{"isp":"gmail","volume":10}]}`).
		AddRow("camp-b", `{}`).
		RowError(1, errors.New("context deadline exceeded"))
	mock.ExpectQuery(`FROM mailing_campaigns`).WillReturnRows(rows)

	now := time.Now()
	states := map[string]*ispCampaignState{
		"camp-a": {campaignID: "camp-a", lastRefreshAt: now},
		"camp-z": {campaignID: "camp-z", lastRefreshAt: now}, // never scanned
	}
	p.refreshISPCampaigns(states, map[string]time.Time{})

	if _, ok := states["camp-z"]; !ok {
		t.Fatal("truncated scan evicted an unscanned campaign")
	}
}

// A complete scan still evicts campaigns that are no longer sending.
func TestRefreshISPCampaigns_CompleteScanEvicts(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	p := &SendWorkerPool{db: db, ctx: context.Background(), batchSize: 100}

	mock.ExpectQuery(`FROM mailing_campaigns`).WillReturnRows(
		sqlmock.NewRows([]string{"id", "q"}).
			AddRow("camp-a", `{"isp_quotas":[{"isp":"gmail","volume":10}]}`))

	now := time.Now()
	states := map[string]*ispCampaignState{
		"camp-a": {campaignID: "camp-a", lastRefreshAt: now},
		"camp-z": {campaignID: "camp-z", lastRefreshAt: now},
	}
	p.refreshISPCampaigns(states, map[string]time.Time{})

	if _, ok := states["camp-z"]; ok {
		t.Fatal("complete scan kept a campaign that is no longer sending")
	}
	if _, ok := states["camp-a"]; !ok {
		t.Fatal("complete scan evicted a sending campaign")
	}
}

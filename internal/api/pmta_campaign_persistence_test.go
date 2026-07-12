package api

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// Regression for the 2026-07-11 draft-eater: an id-less deploy must mint a
// fresh campaign UUID and must NOT touch the DB at all — the old fallback
// (SELECT the org's newest draft and rename it in place) let every id-less
// deploy (partner-drip orchestrator, cockpit scripts) cannibalize one staged
// Draft Board draft per deploy. sqlmock fails the test on ANY unexpected
// query, so this pins both the fresh-UUID result and the no-lookup behavior.
func TestResolvePMTACampaignIdentity_NoIDMintsFreshUUID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}

	cc := &campaignColumnCache{cols: map[string]bool{"pmta_config": true, "execution_mode": true}}

	id, reused, err := resolvePMTACampaignIdentity(context.Background(), tx, "org-1", "", cc)
	if err != nil {
		t.Fatalf("resolvePMTACampaignIdentity(no id) error = %v", err)
	}
	if reused {
		t.Fatalf("id-less deploy reported reusingDraft=true — the draft-eater fallback is back")
	}
	if id == uuid.Nil {
		t.Fatalf("expected a fresh non-nil UUID")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("id-less identity resolution queried the DB (draft-reuse lookup?): %v", err)
	}
}

// The explicit-id path is unchanged: it must look the draft up (org-scoped,
// editable statuses, execution_mode=wave) and return it with reusingDraft=true.
func TestResolvePMTACampaignIdentity_ExplicitIDStillReuses(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	draftID := uuid.New()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id FROM mailing_campaigns").
		WithArgs(draftID, "org-1", pmtaExecutionModeWave).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(draftID.String()))

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}

	cc := &campaignColumnCache{cols: map[string]bool{"pmta_config": true, "execution_mode": true}}

	id, reused, err := resolvePMTACampaignIdentity(context.Background(), tx, "org-1", draftID.String(), cc)
	if err != nil {
		t.Fatalf("resolvePMTACampaignIdentity(explicit id) error = %v", err)
	}
	if !reused {
		t.Fatalf("explicit-id resolution must report reusingDraft=true")
	}
	if id != draftID {
		t.Fatalf("expected %s, got %s", draftID, id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

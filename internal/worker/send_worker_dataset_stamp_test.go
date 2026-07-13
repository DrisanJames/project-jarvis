package worker

// REQ-034 regression guards (findings 2026-07-13-G2 breakpoints 1-4, G3 gap
// 1): every mailing_message_log row must carry the originating
// partner_dataset_id when the recipient is a partner-feed member — stamped
// from the campaign's deploy-time stamp when the subscriber really belongs
// to that dataset, else resolved per-message from partner_clean_queue
// membership (the dataset-9502c7c4 vertical-keyed misattribution class and
// the sidecar/broadcast blindness class). The resolution is ONE batched
// query per claim batch, dormant until idx_pcq_subscriber_id is valid, and
// killable via DISABLE_MESSAGE_DATASET_STAMP=true.

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newDatasetStampTestPool(t *testing.T) (*SendWorkerPool, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	pool := NewSendWorkerPool(db, 1)
	pool.ctx = context.Background()
	return pool, mock
}

var (
	resolveQueryRe = `SELECT subscriber_id, dataset_id, MIN\(ingested_at\)\s+FROM partner_clean_queue\s+WHERE subscriber_id = ANY\(\$1\)\s+GROUP BY subscriber_id, dataset_id`
	probeQueryRe   = `SELECT i\.indisvalid\s+FROM pg_class c\s+JOIN pg_index i ON i\.indexrelid = c\.oid\s+WHERE c\.relname = 'idx_pcq_subscriber_id'`
)

type membershipRow struct {
	sub uuid.UUID
	ds  uuid.UUID
	at  time.Time
}

func membershipRows(rows []membershipRow) *sqlmock.Rows {
	r := sqlmock.NewRows([]string{"subscriber_id", "dataset_id", "min"})
	for _, v := range rows {
		r.AddRow(v.sub, v.ds, v.at)
	}
	return r
}

// TestPickDatasetID_Rules pins the deterministic multi-dataset attribution
// rule (a subscriber can be in 2+ datasets — uq_pcq_vertical_email_md5 is
// per-vertical): campaign stamp wins ONLY when the subscriber is provably a
// member of that dataset; otherwise earliest-ingested membership (ties →
// smallest dataset id); campaign stamp as last resort; Nil for non-partners.
func TestPickDatasetID_Rules(t *testing.T) {
	dsA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	dsB := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	dsC := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(24 * time.Hour)

	cases := []struct {
		name       string
		campaignDS uuid.UUID
		members    []pcqMembership
		want       uuid.UUID
	}{
		{"campaign stamp wins when subscriber is a member of it",
			dsB, []pcqMembership{{dsA, t0}, {dsB, t1}}, dsB},
		{"misattributed campaign stamp (9502c7c4 class): subscriber's own dataset wins",
			dsA, []pcqMembership{{dsB, t1}}, dsB},
		{"sidecar/broadcast (no campaign stamp): earliest ingested wins",
			uuid.Nil, []pcqMembership{{dsB, t1}, {dsA, t0}}, dsA},
		{"ingest-time tie breaks on smallest dataset id (deterministic)",
			uuid.Nil, []pcqMembership{{dsC, t0}, {dsB, t0}}, dsB},
		{"campaign stamped, no pcq link-back rows: campaign stamp stands",
			dsA, nil, dsA},
		{"non-partner subscriber, unstamped campaign: Nil (NULL column)",
			uuid.Nil, nil, uuid.Nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, pickDatasetID(tc.campaignDS, tc.members))
		})
	}
}

// TestResolveDatasetStamps_SidecarBatchResolution: a broadcast/warmup batch
// (campaign unstamped) resolves subscriber→dataset via ONE query for the
// whole batch — the hot-path contract: never a per-message lookup.
func TestResolveDatasetStamps_SidecarBatchResolution(t *testing.T) {
	pool, mock := newDatasetStampTestPool(t)
	pool.datasetResolveReady = true // index probe already latched

	sub1, sub2, sub3 := uuid.New(), uuid.New(), uuid.New()
	dsA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	dsB := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// sub1 in dsA; sub2 in dsA(later)+dsB(earliest) — earliest wins; sub3 non-partner.
	mock.ExpectQuery(resolveQueryRe).WillReturnRows(membershipRows([]membershipRow{
		{sub1, dsA, t0},
		{sub2, dsA, t0.Add(48 * time.Hour)},
		{sub2, dsB, t0},
	}))

	items := pool.resolveDatasetStamps(context.Background(), []QueueItem{
		{SubscriberID: sub1},
		{SubscriberID: sub2},
		{SubscriberID: sub3},
	})

	require.Len(t, items, 3)
	assert.Equal(t, dsA, items[0].PartnerDatasetID, "single-dataset member stamps its dataset")
	assert.Equal(t, dsB, items[1].PartnerDatasetID, "multi-dataset member stamps earliest-ingested")
	assert.Equal(t, uuid.Nil, items[2].PartnerDatasetID, "non-partner subscriber stays NULL")
	assert.NoError(t, mock.ExpectationsWereMet(), "exactly one batched query for the whole claim batch")
}

// TestResolveDatasetStamps_DripCampaignKeepsCampaignStamp: a drip campaign's
// deploy-time stamp survives resolution when the subscriber really belongs
// to that dataset.
func TestResolveDatasetStamps_DripCampaignKeepsCampaignStamp(t *testing.T) {
	pool, mock := newDatasetStampTestPool(t)
	pool.datasetResolveReady = true

	sub := uuid.New()
	dsCampaign := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	dsOther := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(resolveQueryRe).WillReturnRows(membershipRows([]membershipRow{
		{sub, dsOther, t0},                     // earlier membership in another vertical
		{sub, dsCampaign, t0.Add(time.Minute)}, // member of the campaign's dataset
	}))

	items := pool.resolveDatasetStamps(context.Background(), []QueueItem{
		{SubscriberID: sub, PartnerDatasetID: dsCampaign},
	})
	require.Len(t, items, 1)
	assert.Equal(t, dsCampaign, items[0].PartnerDatasetID,
		"campaign context resolves cross-vertical multi-membership")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestResolveDatasetStamps_MisstampedDripRegression pins the dataset-9502c7c4
// fix end-to-end through resolveDatasetStamps: the campaign carries the
// vertical's DOMINANT dataset, the subscriber is a member of a DIFFERENT
// dataset only — the message must stamp the subscriber's own dataset.
func TestResolveDatasetStamps_MisstampedDripRegression(t *testing.T) {
	pool, mock := newDatasetStampTestPool(t)
	pool.datasetResolveReady = true

	sub := uuid.New()
	dsDominant := uuid.MustParse("11111111-1111-1111-1111-111111111111") // e.g. heloc
	dsActual := uuid.MustParse("22222222-2222-2222-2222-222222222222")   // e.g. 9502c7c4
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(resolveQueryRe).WillReturnRows(membershipRows([]membershipRow{
		{sub, dsActual, t0},
	}))

	items := pool.resolveDatasetStamps(context.Background(), []QueueItem{
		{SubscriberID: sub, PartnerDatasetID: dsDominant},
	})
	require.Len(t, items, 1)
	assert.Equal(t, dsActual, items[0].PartnerDatasetID,
		"a provably-wrong campaign stamp must yield to the subscriber's real membership")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestResolveDatasetStamps_KillSwitch: DISABLE_MESSAGE_DATASET_STAMP=true
// short-circuits before any DB read (send-path kill-switch rule).
func TestResolveDatasetStamps_KillSwitch(t *testing.T) {
	t.Setenv("DISABLE_MESSAGE_DATASET_STAMP", "true")
	pool, mock := newDatasetStampTestPool(t)
	pool.datasetResolveReady = true

	items := pool.resolveDatasetStamps(context.Background(), []QueueItem{
		{SubscriberID: uuid.New(), PartnerDatasetID: uuid.New()},
	})
	require.Len(t, items, 1)
	assert.NoError(t, mock.ExpectationsWereMet(), "kill switch must issue no query")
}

// TestResolveDatasetStamps_DormantUntilIndexValid: with idx_pcq_subscriber_id
// missing/invalid the resolution must not run (a seq scan of the >1M-row
// partner_clean_queue on the send hot path), items pass through with the
// campaign-level stamp, and the probe itself is TTL-throttled.
func TestResolveDatasetStamps_DormantUntilIndexValid(t *testing.T) {
	pool, mock := newDatasetStampTestPool(t)
	dsCampaign := uuid.New()

	mock.ExpectQuery(probeQueryRe).
		WillReturnRows(sqlmock.NewRows([]string{"indisvalid"}).AddRow(false))

	in := []QueueItem{{SubscriberID: uuid.New(), PartnerDatasetID: dsCampaign}}
	items := pool.resolveDatasetStamps(context.Background(), in)
	require.Len(t, items, 1)
	assert.Equal(t, dsCampaign, items[0].PartnerDatasetID,
		"campaign-level stamp still applies while resolution is dormant")

	// Second batch inside the TTL: no probe query, no resolve query.
	items = pool.resolveDatasetStamps(context.Background(), in)
	require.Len(t, items, 1)
	assert.NoError(t, mock.ExpectationsWereMet(),
		"exactly one probe within the TTL window and zero resolve queries while dormant")
}

// TestResolveDatasetStamps_IndexProbeLatches: a valid index latches the pool
// to ready — subsequent batches go straight to the single resolve query.
func TestResolveDatasetStamps_IndexProbeLatches(t *testing.T) {
	pool, mock := newDatasetStampTestPool(t)
	sub := uuid.New()

	mock.ExpectQuery(probeQueryRe).
		WillReturnRows(sqlmock.NewRows([]string{"indisvalid"}).AddRow(true))
	mock.ExpectQuery(resolveQueryRe).
		WillReturnRows(sqlmock.NewRows([]string{"subscriber_id", "dataset_id", "min"}))
	mock.ExpectQuery(resolveQueryRe).
		WillReturnRows(sqlmock.NewRows([]string{"subscriber_id", "dataset_id", "min"}))

	in := []QueueItem{{SubscriberID: sub}}
	_ = pool.resolveDatasetStamps(context.Background(), in)
	_ = pool.resolveDatasetStamps(context.Background(), in)
	assert.True(t, pool.datasetResolveReady)
	assert.NoError(t, mock.ExpectationsWereMet(), "probe runs once, then latches")
}

// expectMarkSentTail mocks the best-effort bookkeeping writes markSent issues
// after the two attribution INSERTs (SDS shadow write + inbox profile upsert).
func expectMarkSentTail(mock sqlmock.Sqlmock) {
	mock.ExpectExec(`INSERT INTO mailing_subscriber_domain_state`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_inbox_profiles`).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// TestMarkSent_StampsPartnerDatasetID: the resolved dataset id is written to
// BOTH the message-log row and the 'sent' tracking event (stamp parity).
func TestMarkSent_StampsPartnerDatasetID(t *testing.T) {
	pool, mock := newDatasetStampTestPool(t)
	ds := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	item := QueueItem{
		ID: uuid.New(), CampaignID: uuid.New(), SubscriberID: uuid.New(),
		Email: "user@gmail.com", FromEmail: "news@em.discountblog.com",
		ESPType: "pmta", PartnerDatasetID: ds,
	}

	mock.ExpectExec(`UPDATE mailing_campaign_queue`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_message_log .+partner_dataset_id`).
		WithArgs("msg-1", item.CampaignID, item.SubscriberID, item.Email, item.ESPType, ds).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_tracking_events .+partner_dataset_id`).
		WithArgs(item.CampaignID, item.SubscriberID, "em.discountblog.com", "gmail.com", "{}", ds).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectMarkSentTail(mock)

	require.NoError(t, pool.markSent(context.Background(), item, "msg-1", ""))
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestMarkSent_NullStampForNonPartner: a Nil PartnerDatasetID writes SQL NULL
// on both inserts — non-partner traffic is untouched.
func TestMarkSent_NullStampForNonPartner(t *testing.T) {
	pool, mock := newDatasetStampTestPool(t)
	item := QueueItem{
		ID: uuid.New(), CampaignID: uuid.New(), SubscriberID: uuid.New(),
		Email: "user@yahoo.com", FromEmail: "news@em.quizfiesta.com", ESPType: "ses",
	}

	mock.ExpectExec(`UPDATE mailing_campaign_queue`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_message_log .+partner_dataset_id`).
		WithArgs("msg-2", item.CampaignID, item.SubscriberID, item.Email, item.ESPType, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_tracking_events .+partner_dataset_id`).
		WithArgs(item.CampaignID, item.SubscriberID, "em.quizfiesta.com", "yahoo.com", "{}", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectMarkSentTail(mock)

	require.NoError(t, pool.markSent(context.Background(), item, "msg-2", ""))
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestMarkSent_KillSwitchWritesNull: even when the item carries a resolved
// dataset, DISABLE_MESSAGE_DATASET_STAMP=true reverts to NULL stamping.
func TestMarkSent_KillSwitchWritesNull(t *testing.T) {
	t.Setenv("DISABLE_MESSAGE_DATASET_STAMP", "true")
	pool, mock := newDatasetStampTestPool(t)
	item := QueueItem{
		ID: uuid.New(), CampaignID: uuid.New(), SubscriberID: uuid.New(),
		Email: "user@aol.com", FromEmail: "news@em.myownhealth.net",
		ESPType: "pmta", PartnerDatasetID: uuid.New(),
	}

	mock.ExpectExec(`UPDATE mailing_campaign_queue`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_message_log .+partner_dataset_id`).
		WithArgs("msg-3", item.CampaignID, item.SubscriberID, item.Email, item.ESPType, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_tracking_events .+partner_dataset_id`).
		WithArgs(item.CampaignID, item.SubscriberID, "em.myownhealth.net", "aol.com", "{}", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectMarkSentTail(mock)

	require.NoError(t, pool.markSent(context.Background(), item, "msg-3", ""))
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestClaimSQL_CarriesPartnerDatasetID pins the claim-SELECT extension: the
// campaign's deploy-time partner_dataset_id must ride the claim query so the
// drip path stamps with zero additional reads.
func TestClaimSQL_CarriesPartnerDatasetID(t *testing.T) {
	pool, mock := newDatasetStampTestPool(t)
	mock.ExpectQuery(`camp\.partner_dataset_id[\s\S]*FROM claimed c`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	items, err := pool.claimISPForOne(context.Background(), uuid.New().String(), "gmail", 10)
	require.NoError(t, err)
	assert.Empty(t, items)
	assert.NoError(t, mock.ExpectationsWereMet(),
		"claim SQL must select camp.partner_dataset_id")
}

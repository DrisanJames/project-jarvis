package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/analytics"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// newHandlerForTest builds a SESEventsHandler against an in-memory sqlmock
// and a real GlobalSuppressionHub. SES_WEBHOOK_DISABLE_SIG=true bypasses
// signature verification — without it the tests would have to fabricate a
// real SHA1-RSA SNS signature, which adds zero coverage value.
func newHandlerForTest(t *testing.T) (*SESEventsHandler, sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	t.Setenv("SES_WEBHOOK_DISABLE_SIG", "true")
	// These tests assert sqlmock expectations immediately after ServeHTTP
	// returns, so they need the persistence to happen inline. The async ingest
	// queue (the production default) is covered separately in
	// handlers_ses_events_async_test.go; the per-event persistence logic
	// exercised here is identical on both paths.
	t.Setenv("SES_WEBHOOK_ASYNC", "false")

	// The campaign->sending-domain cache is process-global; without this a test
	// would inherit a neighbour's entry and skip the lookup it mocks, making
	// results depend on test ORDER.
	resetSendingDomainCache()

	hub := engine.NewGlobalSuppressionHub(db, "00000000-0000-0000-0000-000000000001", "")
	h := NewSESEventsHandler(db, hub, "00000000-0000-0000-0000-000000000001")
	t.Cleanup(func() {
		if h.batcher != nil {
			h.batcher.shutdown(2 * time.Second)
		}
	})
	return h, mock, db
}

// resetSendingDomainCache clears the global campaign->domain cache.
func resetSendingDomainCache() {
	sdCacheMu.Lock()
	sdCache = map[uuid.UUID]sdCacheEntry{}
	sdCacheMu.Unlock()
}

// expectOpenCascade mocks the DB calls one SES OPEN issues under the batched
// architecture. Campaign and subscriber counters are NO LONGER written per
// event — they are folded by sesEngagementBatcher — so only these remain:
//
//	1. the authoritative tracking-event INSERT
//	2. the unique-opener check (bounded + LIMIT 2)
//	3. the campaign->sending-domain lookup (cached after the first call)
//	4. the inbox-profile upsert + score recompute (not a hot row)
func expectOpenCascade(mock sqlmock.Sqlmock, firstOpen bool) {
	mock.ExpectExec("INSERT INTO mailing_tracking_events").WillReturnResult(sqlmock.NewResult(1, 1))
	cnt := 2
	if firstOpen {
		cnt = 1
	}
	mock.ExpectQuery("FROM mailing_tracking_events").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(cnt))
	// Empty from_email short-circuits the SDS writes.
	mock.ExpectQuery("FROM mailing_campaigns WHERE id").
		WillReturnRows(sqlmock.NewRows([]string{"from_email"}).AddRow(""))
	mock.ExpectExec("INSERT INTO mailing_inbox_profiles").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery("FROM mailing_inbox_profiles WHERE email_hash").
		WillReturnRows(sqlmock.NewRows([]string{"total_sends", "total_opens", "total_clicks", "last_open_at"}).
			AddRow(0, 0, 0, nil))
}

// snsNotificationBody wraps a SES event-notification in the SNS Notification
// envelope shape. Real SNS adds a Signature/SigningCertURL — we omit those
// because SES_WEBHOOK_DISABLE_SIG=true is set in tests.
func snsNotificationBody(t *testing.T, sesEvent map[string]interface{}) []byte {
	t.Helper()
	inner, err := json.Marshal(sesEvent)
	if err != nil {
		t.Fatalf("marshal inner: %v", err)
	}
	env := map[string]interface{}{
		"Type":             "Notification",
		"MessageId":        "test-msg-id",
		"TopicArn":         "arn:aws:sns:us-west-1:123:t",
		"Message":          string(inner),
		"Timestamp":        "2026-06-03T19:00:00.000Z",
		"SignatureVersion": "1",
	}
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal env: %v", err)
	}
	return out
}

// expectGlobalSuppressionInsert sets up sqlmock so a single Suppress call
// completes successfully. We match the INSERT loosely on the table name
// rather than the full SQL because the hub's exact query may evolve.
func expectGlobalSuppressionInsert(mock sqlmock.Sqlmock) {
	// IsSuppressed cache check — Suppress short-circuits if already cached.
	// We don't preload anything, so the cache lookup misses and the INSERT
	// runs.
	mock.ExpectExec("INSERT INTO mailing_global_suppressions").
		WillReturnResult(sqlmock.NewResult(1, 1))
}

func TestSESEvents_PermanentBounce_Suppresses(t *testing.T) {
	h, mock, _ := newHandlerForTest(t)

	expectGlobalSuppressionInsert(mock)

	body := snsNotificationBody(t, map[string]interface{}{
		"eventType": "Bounce",
		"mail": map[string]interface{}{
			"tags": map[string][]string{
				"ses:configuration-set": {"discountblog"},
				"ses:tenant-name":       {"discountblog"},
			},
		},
		"bounce": map[string]interface{}{
			"bounceType":    "Permanent",
			"bounceSubType": "General",
			"bouncedRecipients": []map[string]interface{}{
				{"emailAddress": "user@example.com", "diagnosticCode": "smtp; 550 5.1.1 unknown user"},
			},
		},
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events", bytes.NewReader(body))
	h.ServeHTTP(rr, req.WithContext(context.Background()))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestSESEvents_TransientBounce_DoesNotSuppress(t *testing.T) {
	h, mock, _ := newHandlerForTest(t)
	// No INSERT expected; if Suppress is called, sqlmock will fail.

	body := snsNotificationBody(t, map[string]interface{}{
		"eventType": "Bounce",
		"mail":      map[string]interface{}{"tags": map[string][]string{}},
		"bounce": map[string]interface{}{
			"bounceType":    "Transient",
			"bounceSubType": "MailboxFull",
			"bouncedRecipients": []map[string]interface{}{
				{"emailAddress": "soft@example.com", "diagnosticCode": "smtp; 452 4.2.2 mailbox full"},
			},
		},
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events", bytes.NewReader(body))
	h.ServeHTTP(rr, req.WithContext(context.Background()))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations (should have been zero): %v", err)
	}
}

func TestSESEvents_Complaint_Suppresses(t *testing.T) {
	h, mock, _ := newHandlerForTest(t)

	expectGlobalSuppressionInsert(mock)

	body := snsNotificationBody(t, map[string]interface{}{
		"eventType": "Complaint",
		"mail":      map[string]interface{}{"tags": map[string][]string{"ses:tenant-name": {"refinanceratesusa"}}},
		"complaint": map[string]interface{}{
			"complaintFeedbackType": "abuse",
			"complainedRecipients": []map[string]interface{}{
				{"emailAddress": "complain@example.com"},
			},
		},
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events", bytes.NewReader(body))
	h.ServeHTTP(rr, req.WithContext(context.Background()))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestSESEvents_OpenClickSendDelivery_AcceptedNoDB(t *testing.T) {
	h, mock, _ := newHandlerForTest(t)

	for _, evt := range []string{"Open", "Click", "Send", "Delivery", "Reject", "DeliveryDelay"} {
		body := snsNotificationBody(t, map[string]interface{}{
			"eventType": evt,
			"mail":      map[string]interface{}{"tags": map[string][]string{"ses:configuration-set": {"discountblog"}}},
			"open":      map[string]interface{}{"timestamp": "2026-06-03T19:00:00Z", "ipAddress": "1.2.3.4"},
			"click":     map[string]interface{}{"timestamp": "2026-06-03T19:00:00Z", "ipAddress": "1.2.3.4", "link": "https://example.com"},
			"reject":    map[string]interface{}{"reason": "Bad content"},
			"deliveryDelay": map[string]interface{}{"delayType": "InternalFailure", "expirationTime": "2026-06-04T00:00:00Z"},
		})

		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events", bytes.NewReader(body))
		h.ServeHTTP(rr, req.WithContext(context.Background()))

		if rr.Code != http.StatusOK {
			t.Fatalf("evt=%s status=%d, want 200; body=%s", evt, rr.Code, rr.Body.String())
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations (should have been zero): %v", err)
	}
}

func TestSESEvents_Open_IncrementsOpenCount(t *testing.T) {
	h, mock, _ := newHandlerForTest(t)

	// Both campaign_id and subscriber_id tags present -> no message_log
	// fallback lookup. Counters are batched, so the per-event cascade is now
	// just the INSERT, the unique check, the cached domain lookup and the
	// inbox profile.
	expectOpenCascade(mock, true)

	body := snsNotificationBody(t, map[string]interface{}{
		"eventType": "Open",
		"mail": map[string]interface{}{
			"tags": map[string][]string{
				"campaign_id":   {"11111111-1111-1111-1111-111111111111"},
				"subscriber_id": {"22222222-2222-2222-2222-222222222222"},
			},
			"commonHeaders": map[string]interface{}{"to": []string{"user@example.com"}},
		},
		"open": map[string]interface{}{"timestamp": "2026-06-08T19:00:00Z", "ipAddress": "1.2.3.4"},
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events", bytes.NewReader(body))
	h.ServeHTTP(rr, req.WithContext(context.Background()))

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestSESEvents_HumanClick_IncrementsClickCount(t *testing.T) {
	h, mock, _ := newHandlerForTest(t)

	mock.ExpectExec("INSERT INTO mailing_tracking_events").WillReturnResult(sqlmock.NewResult(1, 1))
	// Counters are batched now; the per-event work is the unique-click check.
	mock.ExpectQuery("FROM mailing_tracking_events").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("FROM mailing_campaigns WHERE id").
		WillReturnRows(sqlmock.NewRows([]string{"from_email"}).AddRow(""))
	mock.ExpectExec("INSERT INTO mailing_inbox_profiles").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery("FROM mailing_inbox_profiles WHERE email_hash").
		WillReturnRows(sqlmock.NewRows([]string{"total_sends", "total_opens", "total_clicks", "last_open_at"}).
			AddRow(0, 0, 0, nil))

	body := snsNotificationBody(t, map[string]interface{}{
		"eventType": "Click",
		"mail": map[string]interface{}{
			"tags": map[string][]string{
				"campaign_id":   {"11111111-1111-1111-1111-111111111111"},
				"subscriber_id": {"22222222-2222-2222-2222-222222222222"},
			},
			"commonHeaders": map[string]interface{}{"to": []string{"user@example.com"}},
		},
		"click": map[string]interface{}{
			"timestamp": "2026-06-08T19:00:00Z", "ipAddress": "1.2.3.4",
			"link": "https://www.cratoolpro.com/BJB4Q5BF/GK847MZ/?source_id=email&sub1=abc",
		},
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events", bytes.NewReader(body))
	h.ServeHTTP(rr, req.WithContext(context.Background()))

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestSESEvents_AssetClick_DoesNotIncrementClickCount(t *testing.T) {
	h, mock, _ := newHandlerForTest(t)

	// Machine/asset click: the event row still persists (is_machine_click=true)
	// but click_count must NOT increment, so only the INSERT is expected.
	mock.ExpectExec("INSERT INTO mailing_tracking_events").
		WillReturnResult(sqlmock.NewResult(1, 1))

	body := snsNotificationBody(t, map[string]interface{}{
		"eventType": "Click",
		"mail": map[string]interface{}{
			"tags": map[string][]string{
				"campaign_id":   {"11111111-1111-1111-1111-111111111111"},
				"subscriber_id": {"22222222-2222-2222-2222-222222222222"},
			},
			"commonHeaders": map[string]interface{}{"to": []string{"user@example.com"}},
		},
		"click": map[string]interface{}{
			"timestamp": "2026-06-08T19:00:00Z", "ipAddress": "1.2.3.4",
			"link": "https://fonts.googleapis.com/css?family=Nunito+Sans:100,300,500,700,900",
		},
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events", bytes.NewReader(body))
	h.ServeHTTP(rr, req.WithContext(context.Background()))

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations (INSERT only, no click_count update): %v", err)
	}
}

func TestSESEvents_HumanClick_PersistsIPAndUserAgent(t *testing.T) {
	h, mock, _ := newHandlerForTest(t)

	// A residential IP + a fully-formed browser UA on a normal money link is
	// a HUMAN click: the INSERT must carry the payload's ipAddress/userAgent
	// ($11/$12), is_machine_click ($10) must be false, and the full click
	// engagement cascade must run.
	humanUA := "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1"
	humanIP := "203.0.113.7"
	mock.ExpectExec("INSERT INTO mailing_tracking_events").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			false, humanIP, humanUA, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	// Counters are batched now; the per-event work is the unique-click check.
	mock.ExpectQuery("FROM mailing_tracking_events").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("FROM mailing_campaigns WHERE id").
		WillReturnRows(sqlmock.NewRows([]string{"from_email"}).AddRow(""))
	mock.ExpectExec("INSERT INTO mailing_inbox_profiles").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery("FROM mailing_inbox_profiles WHERE email_hash").
		WillReturnRows(sqlmock.NewRows([]string{"total_sends", "total_opens", "total_clicks", "last_open_at"}).
			AddRow(0, 0, 0, nil))

	body := snsNotificationBody(t, map[string]interface{}{
		"eventType": "Click",
		"mail": map[string]interface{}{
			"tags": map[string][]string{
				"campaign_id":   {"11111111-1111-1111-1111-111111111111"},
				"subscriber_id": {"22222222-2222-2222-2222-222222222222"},
			},
			"commonHeaders": map[string]interface{}{"to": []string{"user@example.com"}},
		},
		"click": map[string]interface{}{
			"timestamp": "2026-07-10T19:00:00Z", "ipAddress": humanIP, "userAgent": humanUA,
			"link": "https://www.cratoolpro.com/BJB4Q5BF/GK847MZ/?source_id=email&sub1=abc",
		},
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events", bytes.NewReader(body))
	h.ServeHTTP(rr, req.WithContext(context.Background()))

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestSESEvents_ScannerClick_FlaggedMachine_NoEngagement(t *testing.T) {
	h, mock, _ := newHandlerForTest(t)

	// Datacenter/scanner signal: a bare "Mozilla/5.0" UA from an Azure IP
	// (20.0.0.0/8 is in tracking.CloudCIDRs) trips ClassifyClickAsMachine
	// rule 2, even though the link itself is a normal money URL. The event
	// row still persists with is_machine_click=true ($10) and the payload's
	// ip/ua ($11/$12), but NO engagement side-effect query may run — only
	// the INSERT is expected.
	scannerUA := "Mozilla/5.0"
	scannerIP := "20.190.10.5"
	mock.ExpectExec("INSERT INTO mailing_tracking_events").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			true, scannerIP, scannerUA, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	body := snsNotificationBody(t, map[string]interface{}{
		"eventType": "Click",
		"mail": map[string]interface{}{
			"tags": map[string][]string{
				"campaign_id":   {"11111111-1111-1111-1111-111111111111"},
				"subscriber_id": {"22222222-2222-2222-2222-222222222222"},
			},
			"commonHeaders": map[string]interface{}{"to": []string{"user@example.com"}},
		},
		"click": map[string]interface{}{
			"timestamp": "2026-07-10T19:00:00Z", "ipAddress": scannerIP, "userAgent": scannerUA,
			"link": "https://www.cratoolpro.com/BJB4Q5BF/GK847MZ/?source_id=email&sub1=abc",
		},
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events", bytes.NewReader(body))
	h.ServeHTTP(rr, req.WithContext(context.Background()))

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations (INSERT only, no engagement cascade): %v", err)
	}
}

func TestSESEvents_ScannerUAClick_FlaggedMachine(t *testing.T) {
	h, mock, _ := newHandlerForTest(t)

	// Rule-1 scanner UA (SafeLinks) — machine regardless of IP.
	scannerUA := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) SafeLinks/1.0"
	scannerIP := "98.42.10.11" // residential — the UA alone must flag it
	mock.ExpectExec("INSERT INTO mailing_tracking_events").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			true, scannerIP, scannerUA, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	body := snsNotificationBody(t, map[string]interface{}{
		"eventType": "Click",
		"mail": map[string]interface{}{
			"tags": map[string][]string{
				"campaign_id":   {"11111111-1111-1111-1111-111111111111"},
				"subscriber_id": {"22222222-2222-2222-2222-222222222222"},
			},
			"commonHeaders": map[string]interface{}{"to": []string{"user@example.com"}},
		},
		"click": map[string]interface{}{
			"timestamp": "2026-07-10T19:00:00Z", "ipAddress": scannerIP, "userAgent": scannerUA,
			"link": "https://www.cratoolpro.com/BJB4Q5BF/GK847MZ/?source_id=email&sub1=abc",
		},
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events", bytes.NewReader(body))
	h.ServeHTTP(rr, req.WithContext(context.Background()))

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations (INSERT only, no engagement cascade): %v", err)
	}
}

func TestSESEvents_Open_PersistsIPAndUserAgent(t *testing.T) {
	h, mock, _ := newHandlerForTest(t)

	// Open events must also carry the payload's ipAddress/userAgent into the
	// INSERT ($11/$12). is_machine_click stays false (open semantics are
	// unchanged) and the open engagement cascade runs as before.
	openUA := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko)"
	openIP := "198.51.100.23"
	mock.ExpectExec("INSERT INTO mailing_tracking_events").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			false, openIP, openUA, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery("FROM mailing_tracking_events").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("FROM mailing_campaigns WHERE id").
		WillReturnRows(sqlmock.NewRows([]string{"from_email"}).AddRow(""))
	mock.ExpectExec("INSERT INTO mailing_inbox_profiles").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery("FROM mailing_inbox_profiles WHERE email_hash").
		WillReturnRows(sqlmock.NewRows([]string{"total_sends", "total_opens", "total_clicks", "last_open_at"}).
			AddRow(0, 0, 0, nil))

	body := snsNotificationBody(t, map[string]interface{}{
		"eventType": "Open",
		"mail": map[string]interface{}{
			"tags": map[string][]string{
				"campaign_id":   {"11111111-1111-1111-1111-111111111111"},
				"subscriber_id": {"22222222-2222-2222-2222-222222222222"},
			},
			"commonHeaders": map[string]interface{}{"to": []string{"user@example.com"}},
		},
		"open": map[string]interface{}{"timestamp": "2026-07-10T19:00:00Z", "ipAddress": openIP, "userAgent": openUA},
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events", bytes.NewReader(body))
	h.ServeHTTP(rr, req.WithContext(context.Background()))

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestIsMachineClickURL(t *testing.T) {
	cases := []struct {
		url     string
		machine bool
	}{
		{"https://fonts.googleapis.com/css?family=Nunito", true},
		{"https://cdnjs.cloudflare.com/ajax/libs/font-awesome/4.7.0/css/font-awesome.min.css", true},
		{"https://cdn.example.com/styles/main.css?v=2", true},
		{"https://img.example.com/banner.png", true},
		{"https://www.gravatar.com/avatar/abc", true},
		{"https://www.cratoolpro.com/BJB4Q5BF/GK847MZ/?source_id=email&sub1=abc", false},
		{"https://www.eos57ytf.com/K4C5ZLC/PS8241/?creative_id=4989", false},
		{"https://projectjarvis.io/api/mailing/bt/abc123/def", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isMachineClickURL(c.url); got != c.machine {
			t.Errorf("isMachineClickURL(%q) = %v, want %v", c.url, got, c.machine)
		}
	}
}

func TestSESEvents_LegacyNotificationType_StillRoutes(t *testing.T) {
	h, mock, _ := newHandlerForTest(t)
	expectGlobalSuppressionInsert(mock)

	// Older v1 SES messages from identity-feedback (not config-set
	// event-publishing) use notificationType instead of eventType.
	body := snsNotificationBody(t, map[string]interface{}{
		"notificationType": "Bounce",
		"mail":             map[string]interface{}{"tags": map[string][]string{}},
		"bounce": map[string]interface{}{
			"bounceType":    "Permanent",
			"bounceSubType": "Suppressed",
			"bouncedRecipients": []map[string]interface{}{
				{"emailAddress": "legacy@example.com"},
			},
		},
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events", bytes.NewReader(body))
	h.ServeHTTP(rr, req.WithContext(context.Background()))

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rr.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestSESEvents_SubscriptionConfirmation_FetchesSubscribeURL(t *testing.T) {
	h, _, _ := newHandlerForTest(t)

	confirmed := false
	confirmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		confirmed = true
		w.WriteHeader(http.StatusOK)
	}))
	defer confirmServer.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"Type":             "SubscriptionConfirmation",
		"MessageId":        "test-confirm",
		"TopicArn":         "arn:aws:sns:us-west-1:123:t",
		"SubscribeURL":     confirmServer.URL,
		"Token":            "test-token",
		"Message":          "You have chosen to subscribe...",
		"Timestamp":        "2026-06-03T19:00:00.000Z",
		"SignatureVersion": "1",
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events", bytes.NewReader(body))
	h.ServeHTTP(rr, req.WithContext(context.Background()))

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rr.Code)
	}
	if !confirmed {
		t.Errorf("SubscribeURL was never fetched")
	}
}

func TestSESEvents_MalformedEnvelope_Rejected(t *testing.T) {
	h, _, _ := newHandlerForTest(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events", strings.NewReader("not json"))
	h.ServeHTTP(rr, req.WithContext(context.Background()))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rr.Code)
	}
}

func TestSESEvents_UnknownType_Accepted(t *testing.T) {
	h, _, _ := newHandlerForTest(t)
	body, _ := json.Marshal(map[string]interface{}{
		"Type":             "MysteryType",
		"MessageId":        "x",
		"TopicArn":         "arn:aws:sns:us-west-1:123:t",
		"Timestamp":        "2026-06-03T19:00:00.000Z",
		"SignatureVersion": "1",
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events", bytes.NewReader(body))
	h.ServeHTTP(rr, req.WithContext(context.Background()))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rr.Code)
	}
}

func TestValidateCertHost(t *testing.T) {
	cases := []struct {
		url   string
		valid bool
	}{
		{"https://sns.us-west-1.amazonaws.com/SimpleNotificationService-abc.pem", true},
		{"https://sns.us-east-1.amazonaws.com/cert.pem", true},
		{"http://sns.us-west-1.amazonaws.com/cert.pem", false},
		{"https://attacker.com/cert.pem", false},
		{"https://sns.us-west-1.amazonaws.com.attacker.com/cert.pem", false},
		{"https://sns.us-west-1.aws.amazon.com/cert.pem", false},
	}
	for _, c := range cases {
		err := validateCertHost(c.url)
		if c.valid && err != nil {
			t.Errorf("%s should be valid, got error: %v", c.url, err)
		}
		if !c.valid && err == nil {
			t.Errorf("%s should be rejected, got no error", c.url)
		}
	}
}

func TestSESEvents_DisableSigEnvVarRespected(t *testing.T) {
	if v := os.Getenv("SES_WEBHOOK_DISABLE_SIG"); v != "" {
		t.Skipf("env already set: %s", v)
	}
}

// A Permanent bounce whose diagnosticCode carries SES's pre-flight
// email-validation marker must (a) persist the diagnostic into
// mailing_tracking_events.bounce_reason and (b) suppress under the distinct
// 'ses_address_validation' reason with the diag retained — these sends never
// reached the remote MX and must not read as remote hard bounces (v2.5).
func TestSESEvents_ValidationSuppression_LabeledAndReasonPersisted(t *testing.T) {
	h, mock, _ := newHandlerForTest(t)

	diag := "Amazon SES has suppressed sending to this address due to email validation. The address quality confidence level is below your configured threshold."

	// Tracking event INSERT: bounce_type ($6) is the distinct 'validation'
	// class (NOT 'hard' — v2.6: pre-flight blocks never count in the hard rate)
	// and the diagnostic lands in bounce_reason ($13). NO campaign counter
	// UPDATE is expected between this insert and the suppression insert —
	// sqlmock's ordered expectations fail the test if one fires.
	mock.ExpectExec("INSERT INTO mailing_tracking_events").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"validation", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), diag).
		WillReturnResult(sqlmock.NewResult(1, 1))
	// Suppression row: reason ($4) re-labeled, diag ($8) retained.
	mock.ExpectExec("INSERT INTO mailing_global_suppressions").
		WithArgs(sqlmock.AnyArg(), "deadaddr@att.net", sqlmock.AnyArg(), "ses_address_validation", "ses_webhook",
			sqlmock.AnyArg(), "5.1.1", diag, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	body := snsNotificationBody(t, map[string]interface{}{
		"eventType": "Bounce",
		"mail": map[string]interface{}{
			"timestamp": "2026-07-12T22:09:26.000Z",
			"tags": map[string][]string{
				"campaign_id":       {"11111111-1111-1111-1111-111111111111"},
				"subscriber_id":     {"22222222-2222-2222-2222-222222222222"},
				"recipient_send_id": {"rs-att-1"},
			},
		},
		"bounce": map[string]interface{}{
			"bounceType":    "Permanent",
			"bounceSubType": "General",
			"bouncedRecipients": []map[string]interface{}{
				{"emailAddress": "deadaddr@att.net", "status": "5.1.1", "diagnosticCode": diag},
			},
		},
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events", bytes.NewReader(body))
	h.ServeHTTP(rr, req.WithContext(context.Background()))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestSESEvents_SendNotification_IsNotTypedSent is the REQ-086 regression
// guard: the SES `Send` notification must NEVER persist event_type='sent'.
//
// The send worker (send_worker.go markSent) is the single canonical `sent`
// writer — one row per message for every transport, at submission. Between
// 2026-06-05 and this fix the SES notification wrote a second `sent` row for
// every SES-relayed message, so every rate whose denominator is
// `event_type='sent'` read ~2x low on SES lanes. If this test fails because
// someone re-typed the Send notification back to "sent", that regression is
// back — do not "fix" the test.
func TestSESEvents_SendNotification_IsNotTypedSent(t *testing.T) {
	h, mock, _ := newHandlerForTest(t)

	// Arg 5 is event_type in persistSESEvent's INSERT. Assert the literal.
	mock.ExpectExec("INSERT INTO mailing_tracking_events").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"ses_accepted",
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	body := snsNotificationBody(t, map[string]interface{}{
		"eventType": "Send",
		"mail": map[string]interface{}{
			"timestamp": "2026-09-01T20:12:46.830Z",
			"source":    "news@em.discountblog.com",
			"tags": map[string][]string{
				"campaign_id":       {"11111111-1111-1111-1111-111111111111"},
				"subscriber_id":     {"22222222-2222-2222-2222-222222222222"},
				"recipient_send_id": {"rs-req086-1"},
			},
			"commonHeaders": map[string]interface{}{"to": []string{"user@hotmail.com"}},
		},
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events", bytes.NewReader(body))
	h.ServeHTTP(rr, req.WithContext(context.Background()))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestSESAccepted_MapsToLakeAttempted pins the other half of the ruling: the
// PG re-type must NOT change what the lake sees. persistSESEvent emits the
// lake row as analytics.CanonicalEventType(eventType), so 'ses_accepted' has
// to canonicalize to 'attempted' — otherwise reader_lane_snapshot's
// source='ses' attempted column silently drops to zero.
func TestSESAccepted_MapsToLakeAttempted(t *testing.T) {
	if got := analytics.CanonicalEventType("ses_accepted"); got != "attempted" {
		t.Errorf("CanonicalEventType(\"ses_accepted\") = %q, want \"attempted\"", got)
	}
	if got := analytics.CanonicalEventType("sent"); got != "attempted" {
		t.Errorf("CanonicalEventType(\"sent\") = %q, want \"attempted\"", got)
	}
}

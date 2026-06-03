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

	"github.com/DATA-DOG/go-sqlmock"
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

	hub := engine.NewGlobalSuppressionHub(db, "00000000-0000-0000-0000-000000000001", "")
	h := NewSESEventsHandler(db, hub, "00000000-0000-0000-0000-000000000001")
	return h, mock, db
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

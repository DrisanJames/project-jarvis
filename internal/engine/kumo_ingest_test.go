package engine

import (
	"encoding/json"
	"testing"
)

func translate(t *testing.T, raw string) (AccountingRecord, bool) {
	t.Helper()
	var k KumoLogRecord
	if err := json.Unmarshal([]byte(raw), &k); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return translateKumoRecord(k)
}

func TestKumoDelivery(t *testing.T) {
	// Shape captured live from the production Kumo box.
	raw := `{"type":"Delivery","sender":"bounces@em.mypersonalfinancial.com",
	"recipient":"user@gmail.com","queue":"mta1@gmail.com",
	"response":{"code":250,"enhanced_code":{"class":2,"subject":0,"detail":0},"content":"OK gsmtp"},
	"egress_source":"mta1","egress_pool":"mta1","source_address":{"address":"51.81.135.220:12055"},
	"bounce_classification":"Uncategorized","headers":{"X-Campaign-ID":"camp-123"}}`
	rec, ok := translate(t, raw)
	if !ok || rec.Type != "d" {
		t.Fatalf("want delivered 'd', got ok=%v type=%q", ok, rec.Type)
	}
	if rec.SourceIP != "51.81.135.220" {
		t.Errorf("source_ip: want 51.81.135.220 (port stripped), got %q", rec.SourceIP)
	}
	if rec.VMTA != "mta1" || rec.Domain != "gmail.com" || rec.JobID != "camp-123" {
		t.Errorf("vmta/domain/job: got %q/%q/%q", rec.VMTA, rec.Domain, rec.JobID)
	}
}

func TestKumoHardBounceSuppressible(t *testing.T) {
	// 5.1.1 + BadMailbox must map to a category routeToGlobalSuppression suppresses.
	raw := `{"type":"Bounce","recipient":"dead@gmail.com","queue":"mta1@gmail.com",
	"response":{"code":550,"enhanced_code":{"class":5,"subject":1,"detail":1},"content":"550 5.1.1 no such user"},
	"bounce_classification":"BadMailbox","source_address":{"address":"51.81.135.220:25"}}`
	rec, ok := translate(t, raw)
	if !ok || rec.Type != "b" {
		t.Fatalf("want bounce 'b', got ok=%v type=%q", ok, rec.Type)
	}
	if rec.DSNStatus != "5.1.1" {
		t.Errorf("dsn: want 5.1.1, got %q", rec.DSNStatus)
	}
	if rec.BounceCat != "bad-mailbox" {
		t.Errorf("bounce_cat: want bad-mailbox (suppressible), got %q", rec.BounceCat)
	}
	// Verify it flows hard through the existing engine classifier.
	if ClassifyBounce(rec.BounceCat) != "hard_bounce" {
		t.Errorf("engine.ClassifyBounce(%q) != hard_bounce", rec.BounceCat)
	}
}

func TestKumoHardBounceDSNFallback(t *testing.T) {
	// Uncategorized but 5.1.x DSN must still derive a suppressible hard category.
	raw := `{"type":"Bounce","recipient":"x@aol.com","queue":"mta2@aol.com",
	"response":{"code":550,"enhanced_code":{"class":5,"subject":1,"detail":10},"content":"550 user unknown"},
	"bounce_classification":"Uncategorized"}`
	rec, _ := translate(t, raw)
	if rec.BounceCat != "bad-mailbox" {
		t.Errorf("DSN fallback: want bad-mailbox, got %q", rec.BounceCat)
	}
}

func TestKumoReputationBlockNotSuppressed(t *testing.T) {
	// 5.7.1 policy block: must NOT become a list-hygiene category (don't suppress).
	raw := `{"type":"Bounce","recipient":"ok@spectrum.net","queue":"mta3@spectrum.net",
	"response":{"code":550,"enhanced_code":{"class":5,"subject":7,"detail":1},"content":"550 5.7.1 blocked"},
	"bounce_classification":"SpamBlock"}`
	rec, _ := translate(t, raw)
	if rec.BounceCat == "bad-mailbox" || rec.BounceCat == "bad-domain" ||
		rec.BounceCat == "inactive-mailbox" || rec.BounceCat == "quota-issues" {
		t.Errorf("reputation block must not map to a suppressible cat, got %q", rec.BounceCat)
	}
}

func TestKumoTransientIsDeferral(t *testing.T) {
	raw := `{"type":"TransientFailure","recipient":"u@gmail.com","queue":"mta1@gmail.com",
	"response":{"code":451,"enhanced_code":{"class":4,"subject":4,"detail":4},"content":"451 try later"}}`
	rec, ok := translate(t, raw)
	if !ok || rec.Type != "t" {
		t.Fatalf("want deferral 't', got ok=%v type=%q", ok, rec.Type)
	}
}

func TestKumoComplaintRecipientFromTrace(t *testing.T) {
	raw := `{"type":"Feedback","recipient":"",
	"feedback_report":{"feedback_type":"abuse","supplemental_trace":{"recipient":"angry@yahoo.com"}}}`
	rec, ok := translate(t, raw)
	if !ok || rec.Type != "f" {
		t.Fatalf("want complaint 'f', got ok=%v type=%q", ok, rec.Type)
	}
	if rec.Recipient != "angry@yahoo.com" {
		t.Errorf("complaint recipient from trace: got %q", rec.Recipient)
	}
}

func TestKumoReceptionSkipped(t *testing.T) {
	raw := `{"type":"Reception","recipient":"u@gmail.com","queue":"mta1@gmail.com",
	"response":{"code":250,"enhanced_code":null,"content":""}}`
	if _, ok := translate(t, raw); ok {
		t.Errorf("Reception must be skipped (ok=false)")
	}
}

func TestKumoExpirationIsSoft(t *testing.T) {
	raw := `{"type":"Expiration","recipient":"u@gmail.com","queue":"mta1@gmail.com",
	"response":{"code":0,"enhanced_code":null,"content":"max age reached"}}`
	rec, ok := translate(t, raw)
	if !ok || rec.Type != "b" {
		t.Fatalf("want 'b', got ok=%v type=%q", ok, rec.Type)
	}
	if rec.DSNStatus != "4.4.7" {
		t.Errorf("expiration should force a 4.x soft DSN, got %q", rec.DSNStatus)
	}
}

package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// KumoLogRecord is the subset of a KumoMTA JSON log record we consume.
// Reference: https://docs.kumomta.com/reference/log_record/
// Field shapes verified against live records from the production Kumo box
// (e.g. source_address is an object {"address":"ip:port"}, enhanced_code is an
// object {class,subject,detail}, queue is "tenant@domain").
type KumoLogRecord struct {
	Type                 string                     `json:"type"`
	Sender               string                     `json:"sender"`
	Recipient            json.RawMessage            `json:"recipient"` // string OR []string
	Queue                string                     `json:"queue"`     // "tenant@domain"
	Response             kumoResponse               `json:"response"`
	BounceClassification string                     `json:"bounce_classification"`
	EgressSource         string                     `json:"egress_source"`
	EgressPool           string                     `json:"egress_pool"`
	SourceAddress        *kumoSourceAddr            `json:"source_address"`
	EventTime            string                     `json:"event_time"`
	Headers              map[string]json.RawMessage `json:"headers"`
	FeedbackReport       map[string]interface{}     `json:"feedback_report"`
}

type kumoResponse struct {
	Code         int    `json:"code"`
	Content      string `json:"content"`
	EnhancedCode *struct {
		Class   int `json:"class"`
		Subject int `json:"subject"`
		Detail  int `json:"detail"`
	} `json:"enhanced_code"`
}

type kumoSourceAddr struct {
	Address string `json:"address"` // "ip:port"
}

func (k *KumoLogRecord) recipient() string {
	if len(k.Recipient) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(k.Recipient, &s) == nil {
		return s
	}
	var arr []string
	if json.Unmarshal(k.Recipient, &arr) == nil && len(arr) > 0 {
		return arr[0]
	}
	return ""
}

func (k *KumoLogRecord) header(name string) string {
	if k.Headers == nil {
		return ""
	}
	raw, ok := k.Headers[name]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func kumoDSN(r kumoResponse) string {
	if r.EnhancedCode != nil && r.EnhancedCode.Class > 0 {
		return fmt.Sprintf("%d.%d.%d", r.EnhancedCode.Class, r.EnhancedCode.Subject, r.EnhancedCode.Detail)
	}
	return ""
}

// mapKumoBounceCat maps Kumo's bounce_classification (with the DSN as fallback)
// onto the PMTA bounce-category vocabulary the existing classifiers understand.
// This is what lets hard bounces still suppress: routeToGlobalSuppression keys
// on BounceCat ∈ {bad-mailbox,bad-domain,inactive-mailbox,quota-issues}.
func mapKumoBounceCat(class, dsn string) string {
	switch strings.ToLower(strings.ReplaceAll(class, " ", "")) {
	case "badmailbox", "invalidrecipient", "baddestinationmailbox", "norelay":
		return "bad-mailbox"
	case "baddomain", "domaindoesnotexist", "dnsfailure":
		return "bad-domain"
	case "inactivemailbox", "accountdisabled", "disabledmailbox":
		return "inactive-mailbox"
	case "quotaissues", "mailboxfull", "overquota", "full":
		return "quota-issues"
	case "spamrelated", "spam", "spamblock", "spamcontent", "blocked", "contentblock", "blocklisted":
		return "spam-related"
	case "policyrelated", "policy", "relaydenied":
		return "policy-related"
	case "routingerrors", "routing":
		return "routing-errors"
	case "noanswerfromhost", "connectionfailed", "connectionerror":
		return "no-answer-from-host"
	case "badconnection":
		return "bad-connection"
	case "contentrelated":
		return "content-related"
	case "messageexpired", "expired", "timedout":
		return "message-expired"
	}
	// Fall back to the DSN for the suppression-critical hard categories.
	switch {
	case strings.HasPrefix(dsn, "5.1."):
		return "bad-mailbox"
	case dsn == "5.2.1":
		return "inactive-mailbox"
	case strings.HasPrefix(dsn, "5.2."):
		return "quota-issues"
	}
	return ""
}

// translateKumoRecord converts a Kumo log record into the engine's
// AccountingRecord shape (Source="kumo") so the existing accounting pipeline
// (suppression, classification, counters, lake) handles it unchanged.
// ok=false marks record types we don't ingest (Reception, AdminBounce, Rejection,
// Delayed, AdminRebind, Xfer*, ...).
func translateKumoRecord(k KumoLogRecord) (AccountingRecord, bool) {
	rec := AccountingRecord{
		Source:       "kumo",
		Recipient:    k.recipient(),
		Sender:       k.Sender,
		DeliveryTime: k.EventTime,
		VMTA:         k.EgressSource,
		Pool:         k.EgressPool,
		JobID:        k.header("X-Campaign-ID"),
	}
	if k.SourceAddress != nil {
		rec.SourceIP = k.SourceAddress.Address
		if i := strings.LastIndex(rec.SourceIP, ":"); i > 0 {
			rec.SourceIP = rec.SourceIP[:i] // strip :port
		}
	}
	if at := strings.LastIndex(k.Queue, "@"); at >= 0 {
		rec.Domain = k.Queue[at+1:]
	}
	dsn := kumoDSN(k.Response)
	rec.DSNStatus = dsn
	rec.DSNDiag = k.Response.Content

	switch k.Type {
	case "Delivery":
		rec.Type = "d"
	case "Bounce", "OOB":
		rec.Type = "b"
		rec.BounceCat = mapKumoBounceCat(k.BounceClassification, dsn)
	case "Expiration":
		// Gave up after retries — a soft bounce. Force a 4.x DSN so the
		// classifier never treats it as a hard, suppressible failure.
		rec.Type = "b"
		if dsn == "" {
			rec.DSNStatus = "4.4.7"
		}
		rec.BounceCat = "message-expired"
	case "TransientFailure":
		rec.Type = "t" // deferral, not a permanent bounce
	case "Feedback":
		rec.Type = "f"
		if rec.Recipient == "" {
			rec.Recipient = feedbackRecipient(k)
		}
	default:
		return rec, false
	}
	return rec, true
}

func feedbackRecipient(k KumoLogRecord) string {
	if k.FeedbackReport == nil {
		return ""
	}
	// supplemental_trace.recipient — decoded from our X-KumoRef trace header.
	if st, ok := k.FeedbackReport["supplemental_trace"].(map[string]interface{}); ok {
		if r, ok := st["recipient"].(string); ok && r != "" {
			return r
		}
	}
	if arr, ok := k.FeedbackReport["original_rcpt_to"].([]interface{}); ok && len(arr) > 0 {
		if s, ok := arr[0].(string); ok {
			return s
		}
	}
	return ""
}

// HandleKumoWebhook ingests KumoMTA log-hook events. Accepts a single JSON
// record, a JSON array, or newline-delimited JSON; translates each to an
// AccountingRecord and runs the existing accounting pipeline. Public endpoint;
// when KUMO_EVENTS_TOKEN is set it requires a matching ?token= query param.
func (ing *Ingestor) HandleKumoWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if tok := os.Getenv("KUMO_EVENTS_TOKEN"); tok != "" && r.URL.Query().Get("token") != tok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 16*1024*1024))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var krecs []KumoLogRecord
	if err := json.Unmarshal(body, &krecs); err != nil {
		var single KumoLogRecord
		if err2 := json.Unmarshal(body, &single); err2 == nil {
			krecs = []KumoLogRecord{single}
		} else {
			for _, line := range strings.Split(string(body), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				var rec KumoLogRecord
				if json.Unmarshal([]byte(line), &rec) == nil {
					krecs = append(krecs, rec)
				}
			}
		}
	}

	var records []AccountingRecord
	for _, k := range krecs {
		if rec, ok := translateKumoRecord(k); ok {
			records = append(records, rec)
		}
	}

	processed := 0
	if len(records) > 0 {
		processed = ing.processAccountingWebhookBatch(records)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"received":  len(krecs),
		"ingested":  len(records),
		"processed": processed,
	})
}

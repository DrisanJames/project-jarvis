package api

// JourneyEventsBridge — the inlet that lets OUR OWN funnel apps (the WCL
// leadgen-form container) feed the converter/abandon journey machinery, plus
// the (HELD, flag-gated) prefill read API.
//
//	POST /api/mailing/journey/events
//	  body: {type: 'lead_accepted'|'session_progress', transid, session_id,
//	         sub1?, email?, form_data{loan_purpose,step,...}, ts?}
//	  Idempotent per (type, transid, step) — step is form_data.step ('' for
//	  lead_accepted, so lead_accepted is once-per-transid exactly as specced;
//	  session_progress dedupes per step transition). Rows land in
//	  mailing_journey_events (jul27_journey_events_bridge migration). The
//	  JourneyAbandonDetector worker (internal/worker/journey_abandon_detector.go)
//	  sweeps them.
//	  Registered on s.router like the Everflow postbacks (server-to-server, no
//	  session). Optional shared secret: when env JOURNEY_EVENTS_KEY is set the
//	  X-Journey-Events-Key header must match (401 otherwise); unset = open,
//	  postback parity.
//
//	GET /api/mailing/journey/prefill?token=<signed>
//	  PROVIDER-APPROVED (operator 2026-07-27, supersedes the same-day hold):
//	  data-on-file pre-population is authorized, so JOURNEY_PREFILL_ENABLED
//	  defaults ON ("false" disarms → 404, indistinguishable from a bad
//	  token). The compliant shape STANDS: the funnel prefills convenience +
//	  previously-typed fields, NEVER the consent step, and stamps per-field
//	  PROVENANCE (prefilled_from_file | restored_from_session |
//	  consumer_typed | consumer_edited) into its session record and lead
//	  payload — provenance documented, not masked, is the cert answer.
//	  Token = prefilltoken (HMAC over subscriber_id|expiry, TRACKING_SECRET).
//	  A bare subscriber uuid is REJECTED (no unsigned sub1 path — PII
//	  harvesting hole; pinned by test).

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/pkg/prefilltoken"
)

// JourneyEventsBridge carries the DB handle for both endpoints.
type JourneyEventsBridge struct {
	db *sql.DB
}

// NewJourneyEventsBridge wires the bridge.
func NewJourneyEventsBridge(db *sql.DB) *JourneyEventsBridge {
	return &JourneyEventsBridge{db: db}
}

// journeyEventInput is the funnel's posted event.
//
// Affid (2026-08-04): the traffic source. Until now the platform never received
// it, so no per-affiliate rule could be expressed and no per-affiliate coverage
// could be measured — abandon recovery was silently dark for whole affiliates
// and nobody could see it. Measured that day: of 103 sessions that reached the
// `email` step, 36 (35%) never sent us the address, and 1,093 of 2,378 abandons
// (46%) were unreachable. Optional and free-form on purpose: an affiliate that
// omits it degrades to '' (reported as "unknown") rather than losing the event.
type journeyEventInput struct {
	Type      string                 `json:"type"`
	TransID   string                 `json:"transid"`
	SessionID string                 `json:"session_id"`
	Sub1      string                 `json:"sub1"`
	Affid     string                 `json:"affid"`
	Email     string                 `json:"email"`
	FormData  map[string]interface{} `json:"form_data"`
	TS        string                 `json:"ts"` // RFC3339, optional
}

// validJourneyEventTypes is the closed set the bridge accepts.
var validJourneyEventTypes = map[string]bool{
	"lead_accepted":    true,
	"session_progress": true,
}

// insertJourneyEventSQL is the idempotent write. The unique index
// uidx_mje_type_transid_step makes a duplicate a clean no-op.
const insertJourneyEventSQL = `
	INSERT INTO mailing_journey_events
		(event_type, transid, session_id, sub1, affid, email, step, form_data, event_ts)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9)
	ON CONFLICT (event_type, transid, step) DO NOTHING`

// HandleJourneyEvent records one funnel event. Mirrors the postback handlers'
// posture: validation failures respond 200 with a skipped status (the funnel's
// bridge is fire-and-forget; a retry storm helps nobody), real DB errors 500.
func (h *JourneyEventsBridge) HandleJourneyEvent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if key := os.Getenv("JOURNEY_EVENTS_KEY"); key != "" {
		if r.Header.Get("X-Journey-Events-Key") != key {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad key"})
			return
		}
	}

	var in journeyEventInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "skipped", "reason": "bad_json"})
		return
	}
	in.Type = strings.ToLower(strings.TrimSpace(in.Type))
	in.TransID = strings.TrimSpace(in.TransID)
	if !validJourneyEventTypes[in.Type] || in.TransID == "" {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "skipped", "reason": "invalid_type_or_transid"})
		return
	}

	// step: the idempotency discriminator for session_progress; '' for
	// lead_accepted → exactly once per (type, transid).
	step := ""
	if in.Type == "session_progress" && in.FormData != nil {
		if s, ok := in.FormData["step"].(string); ok {
			step = strings.TrimSpace(s)
		}
	}

	var eventTS interface{}
	if ts, err := time.Parse(time.RFC3339, strings.TrimSpace(in.TS)); err == nil {
		eventTS = ts
	}

	formJSON := "{}"
	if in.FormData != nil {
		if b, err := json.Marshal(in.FormData); err == nil {
			formJSON = string(b)
		}
	}

	res, err := h.db.ExecContext(r.Context(), insertJourneyEventSQL,
		in.Type, in.TransID, strings.TrimSpace(in.SessionID),
		strings.TrimSpace(in.Sub1), strings.TrimSpace(in.Affid),
		strings.ToLower(strings.TrimSpace(in.Email)),
		step, formJSON, eventTS)
	if err != nil {
		log.Printf("[JourneyEventsBridge] insert %s/%s: %v", in.Type, in.TransID, err)
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "store failed"})
		return
	}
	n, _ := res.RowsAffected()
	status := "recorded"
	if n == 0 {
		status = "duplicate"
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}

// prefillSubscriberSQL loads the prefill source row: profile fields + the M9
// personalization store (mailing_subscribers.custom_fields — data_source
// 'wcl-m9-heloc' rows carry city/state/property_value/mortgage_balance;
// street/zip are NOT held anywhere in the platform, verified 2026-07-27).
const prefillSubscriberSQL = `
	SELECT COALESCE(first_name,''), COALESCE(last_name,''), email,
	       COALESCE(custom_fields, '{}'::jsonb)::text
	FROM mailing_subscribers WHERE id = $1`

// HandlePrefill serves the prefill fields for a valid signed token. Every
// failure — flag off, malformed/bare-uuid/expired token, unknown subscriber —
// is the same 404 (no probing oracle).
func (h *JourneyEventsBridge) HandlePrefill(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
	}

	// Flag (see file comment): default ON since the 2026-07-27 provider
	// approval; explicit "false" makes the surface not exist.
	if !prefilltoken.Enabled() {
		notFound()
		return
	}

	token := r.URL.Query().Get("token")
	// Explicit bare-uuid rejection ahead of Verify (defense in depth for the
	// PII-harvesting hole; Verify would also fail it on signature shape).
	if _, err := uuid.Parse(strings.TrimSpace(token)); err == nil {
		notFound()
		return
	}
	subID, err := prefilltoken.Verify(token, prefilltoken.SecretFromEnv())
	if err != nil {
		notFound()
		return
	}

	var firstName, lastName, email, customRaw string
	err = h.db.QueryRowContext(r.Context(), prefillSubscriberSQL, subID).
		Scan(&firstName, &lastName, &email, &customRaw)
	if err != nil {
		notFound()
		return
	}
	var custom map[string]interface{}
	_ = json.Unmarshal([]byte(customRaw), &custom)
	get := func(k string) interface{} {
		if custom == nil {
			return nil
		}
		return custom[k]
	}

	// Field set per spec; street/zip returned as null — the platform holds
	// neither (documented gap, not an omission).
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"first_name":       firstName,
		"last_name":        lastName,
		"email":            email,
		"street":           nil,
		"city":             get("city"),
		"state":            get("state"),
		"zip":              nil,
		"property_value":   get("property_value"),
		"mortgage_balance": get("mortgage_balance"),
	})
}

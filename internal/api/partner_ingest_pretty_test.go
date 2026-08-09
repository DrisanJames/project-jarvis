package api

import "testing"

// Regression guard for the 2026-08-09 silent-loss incident: the Internal Car
// Insurance feed authenticated cleanly but never landed a record from key
// creation (2026-07-28) to 2026-08-09 — 0 inbound batches, 0 S3 objects, 0
// queue rows — because the partner posted ONE pretty-printed JSON object with
// an application/x-ndjson content-type. Every physical line failed Unmarshal,
// the loop `continue`d, and the handler answered 400 no_records with no log.
const prettyOneRecord = `{
    "data": { "tid": "6c494f76" },
    "first_name": "Timmy",
    "email": "tmotrucker@gmail.com",
    "last_name": "Morey",
    "state": "AZ",
    "postal_code": "85143",
    "city": "San Tan Valley"
}`

func TestParseNDJSON_PrettyPrintedSingleObject(t *testing.T) {
	recs, err := parseIngestPayload([]byte(prettyOneRecord), "application/x-ndjson")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("pretty-printed single object must yield 1 record, got %d "+
			"(this is the exact shape that silently lost a live partner feed)", len(recs))
	}
	if recs[0].Email != "tmotrucker@gmail.com" {
		t.Fatalf("email not normalized/carried: %q", recs[0].Email)
	}
}

func TestParseNDJSON_PrettyPrintedArray(t *testing.T) {
	body := `[
  {"email": "a@example.com", "first_name": "A"},
  {"email": "b@example.com", "first_name": "B"}
]`
	recs, err := parseIngestPayload([]byte(body), "application/x-ndjson")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("pretty-printed array must yield 2 records, got %d", len(recs))
	}
}

// The happy path must be byte-for-byte unchanged — the fallback only engages
// when line-parsing yields nothing.
func TestParseNDJSON_TrueNDJSONUnaffected(t *testing.T) {
	body := `{"email":"a@example.com"}
{"email":"b@example.com"}
{"email":"c@example.com"}`
	recs, err := parseIngestPayload([]byte(body), "application/x-ndjson")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("multi-line ndjson must still yield 3, got %d", len(recs))
	}
}

// Garbage must still be rejected — the fallback must not turn invalid payloads
// into phantom records.
func TestParseNDJSON_InvalidStillRejected(t *testing.T) {
	for name, body := range map[string]string{
		"not json":        `this is not json at all`,
		"object no email": `{"first_name": "NoEmail"}`,
		"bad email":       `{"email": "no-at-sign"}`,
	} {
		recs, _ := parseIngestPayload([]byte(body), "application/x-ndjson")
		if len(recs) != 0 {
			t.Fatalf("%s: expected 0 records, got %d", name, len(recs))
		}
	}
}

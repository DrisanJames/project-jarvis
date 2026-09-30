package tracking

// Open-pixel position measurement (2026-09-29). SHADOW-SAFE: nothing here
// changes whether or how an open is counted.
//
// The send paths emit two open pixels per message (worker.buildOpenPixelHTML):
// the top one with ?p=t, the bottom one with ?p=b. Path and signature are
// identical, so the (campaign_id, subscriber_id) dedupe still counts exactly
// one open; this file only reads the marker so the FIRST counted open's
// position can be stored and every fetch's position can be counted.

import (
	"database/sql"
	"encoding/json"
	"net/http"
)

const (
	PixelPosTop    = "t"
	PixelPosBottom = "b"
)

// PixelPosFromRequest returns "t" or "b" from the ?p= query parameter and ""
// for anything else (absent, empty, unknown value).
func PixelPosFromRequest(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	return NormalizePixelPos(r.URL.Query().Get("p"))
}

// NormalizePixelPos accepts only "t" or "b".
func NormalizePixelPos(p string) string {
	switch p {
	case PixelPosTop, PixelPosBottom:
		return p
	}
	return ""
}

// CountOpenPos increments open.pos.t | open.pos.b | open.pos.none on the
// shared /track/* counters (exposed on /health "sig.counters" and in the
// 5-minute TRACKSIG SUMMARY line). Counts EVERY pixel fetch, not only the
// first counted open.
func CountOpenPos(pos string) {
	switch NormalizePixelPos(pos) {
	case PixelPosTop:
		sigCounters.inc("open.pos.t")
	case PixelPosBottom:
		sigCounters.inc("open.pos.b")
	default:
		sigCounters.inc("open.pos.none")
	}
}

// openMetadata is the mailing_tracking_events.metadata document for an
// 'opened' row. omitempty: an unset field contributes no key.
type openMetadata struct {
	PixelPos string `json:"pixel_pos,omitempty"`
}

// OpenMetadataJSON is the metadata value for an 'opened' row: {"pixel_pos":"t"}
// or {"pixel_pos":"b"}, or an invalid (SQL NULL) value when pos is not t/b —
// so an open without a marker writes a row identical to every row before it.
func OpenMetadataJSON(pos string) sql.NullString {
	pos = NormalizePixelPos(pos)
	if pos == "" {
		return sql.NullString{}
	}
	b, err := json.Marshal(openMetadata{PixelPos: pos})
	if err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(b), Valid: true}
}

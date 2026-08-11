package worker

import (
	"encoding/json"
	"testing"
)

// Regression guard: `city` must survive ALL THREE hops on the partner-ingest
// path. It was fixed at the api door (d815fe3) and in partnerRawRecord
// (b96355f) and STILL arrived empty, because bulkInsertSurvivors builds
// extra_metadata from a hand-written literal map that named eight fields and
// not city. Three consecutive "fixes", three live sends, still no city.
func TestPartnerRawRecord_CityReachesExtraMetadata(t *testing.T) {
	var rec partnerRawRecord
	if err := json.Unmarshal([]byte(`{"email":"a@b.com","city":"Boise","state":"ID","zip":"83669"}`), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.City != "Boise" {
		t.Fatalf("hop 2: partnerRawRecord dropped city, got %q", rec.City)
	}
	// hop 3 — the REAL production field list, not a copy
	extra := extraMetadataJSON(rec)
	var out map[string]interface{}
	_ = json.Unmarshal(extra, &out)
	if out["city"] != "Boise" {
		t.Fatalf("hop 3: extra_metadata dropped city — got %v (this is the bug that survived two fixes)", out["city"])
	}
}

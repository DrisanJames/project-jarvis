package api

import (
	"encoding/json"
	"testing"
)

// Pinned against a VERBATIM record captured from the Attribits gmail_v1 feed
// on 2026-08-21 (partner_ingest_raw_samples). Of the 34 fields posted, only
// three had names matching ours — opt_in_date, opt_in_ip, opt_in_url — so ZIP,
// state, the per-user token and the entire vehicle block were destroyed at the
// door while the stored row looked merely "sparse". The three surviving fields
// are what made it look like the pipeline was healthy.
//
// This is deliberately the real payload rather than a hand-written subset: a
// test built from a copied field list passes even when production drops a
// field, which is exactly how the 2026-08-09 city loss survived two fixes.
const attribitsGmailRawRecord = `{"opt_in_date":"2026-08-21T08:23:24","opt_in_url":"www.everquote.com",` +
	`"opt_in_ip":"67.250.116.104","vehicle_model__title":"Camry","vehicle_make__title":"Toyota",` +
	`"state_upper":"NY","zip_code":"12601","returnpath_pixel":"!-- na --",` +
	`"emd5":"c69189bb7ceff577e51db5ed211e1d11","uuid":"c6dfa518-d462-4d6d-b451-6a4f11a3e125",` +
	`"county":"Dutchess","long_state":"New York","gk_list_id":"3206","home_owner":"0","gender":"f",` +
	`"dob":"1980-02-15","send_number":"1","email":"kringwo15@gmail.com",` +
	`"var1":"34437274556f5154462f66706777466a682f664930413d3d","vehicle_year":"2004",` +
	`"vehicle_make":"TOYOTA","vehicle_model":"CAMRY","today":"Friday","date":"20260821"}`

func TestAttribitsGmailPayloadSurvivesTheDoor(t *testing.T) {
	var rec ingestRecord
	if err := json.Unmarshal([]byte(attribitsGmailRawRecord), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Fields with a column of their own.
	for _, c := range []struct{ name, got, want string }{
		{"email", rec.Email, "kringwo15@gmail.com"},
		{"zip (from zip_code)", rec.Zip, "12601"},
		{"state (from state_upper)", rec.State, "NY"},
		{"ip_address (from opt_in_ip)", rec.IPAddress, "67.250.116.104"},
		{"signup_url (from opt_in_url)", rec.SignupURL, "www.everquote.com"},
		{"opt_in_date", rec.OptInDate, "2026-08-21T08:23:24"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}

	// Fields with no column, folded into Metadata — the only map that survives
	// partnerRawRecord and extraMetadataJSON without further changes.
	for _, c := range []struct{ key, want string }{
		{"tid", "34437274556f5154462f66706777466a682f664930413d3d"}, // from var1
		{"vehicle", "2004 Toyota Camry"},
		{"vehicle_make", "Toyota"},
		{"vehicle_model", "Camry"},
		{"vehicle_year", "2004"},
		{"county", "Dutchess"},
		{"dob", "1980-02-15"},
		{"gender", "f"},
		{"home_owner", "0"},
		{"partner_uuid", "c6dfa518-d462-4d6d-b451-6a4f11a3e125"},
		{"emd5", "c69189bb7ceff577e51db5ed211e1d11"},
	} {
		got, _ := rec.Metadata[c.key].(string)
		if got != c.want {
			t.Errorf("metadata[%q] = %q, want %q", c.key, got, c.want)
		}
	}

	// tid is what the money link carries. Losing it ships every recipient an
	// empty tokenid and kills attribution for the whole feed while sends look
	// healthy — the 2026-08-13 failure, repeated under a different key name.
	if rec.Metadata["tid"] == nil || rec.Metadata["tid"] == "" {
		t.Fatal("tid absent — money-link attribution would be dead for this feed")
	}

	// This feed genuinely sends no name and no city; assert that rather than
	// leave it ambiguous, so a future payload that DOES carry them trips here
	// and gets mapped instead of silently ignored.
	if rec.FirstName != "" || rec.LastName != "" {
		t.Errorf("feed unexpectedly carries a name (%q %q) — map it", rec.FirstName, rec.LastName)
	}
	if rec.City != "" {
		t.Errorf("feed unexpectedly carries a city (%q) — map it", rec.City)
	}
}

// An explicit tid must outrank var1 when a payload carries both.
func TestAttribitsGmailExplicitTIDWinsOverVar1(t *testing.T) {
	var rec ingestRecord
	if err := json.Unmarshal([]byte(`{"email":"a@b.com","tid":"CANONICAL","var1":"FALLBACK"}`), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, _ := rec.Metadata["tid"].(string); got != "CANONICAL" {
		t.Errorf("tid = %q, want CANONICAL (var1 must not win)", got)
	}
}

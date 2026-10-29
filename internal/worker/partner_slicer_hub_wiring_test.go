package worker

import "testing"

type slicerWiringHub struct{ emails, hashes map[string]bool }

func (f slicerWiringHub) IsSuppressed(email string) bool       { return f.emails[email] }
func (f slicerWiringHub) IsSuppressedByHash(md5Hash string) bool { return f.hashes[md5Hash] }

// 2026-10-28: the slicer captured server.GlobalHub at construction, before
// SetMailingDB had built it, so every boot ran with a nil hub and dropped
// nothing. The hub is now attached by SetSuppressionHub before Start; this pins
// that an attached hub actually filters (by email AND by md5) and that the
// wiring check reports it.
func TestPartnerSlicer_SetSuppressionHub_Filters(t *testing.T) {
	ps := NewPartnerSlicer(nil, nil, "b", nil, PartnerSlicerConfig{})
	recs := []partnerRawRecord{
		{Email: "kept@outlook.com", MD5: md5HashLower("kept@outlook.com")},
		{Email: "byemail@outlook.com", MD5: md5HashLower("byemail@outlook.com")},
		{Email: "byhash@icloud.com", MD5: md5HashLower("byhash@icloud.com")},
	}
	if ps.HasSuppressionHub() {
		t.Fatal("hub attached before SetSuppressionHub")
	}
	if got, dropped := ps.classifyAndFilter(recs); len(got) != 3 || dropped != 0 {
		t.Fatalf("nil hub: got %d survivors %d dropped, want 3/0", len(got), dropped)
	}
	ps.SetSuppressionHub(slicerWiringHub{
		emails: map[string]bool{"byemail@outlook.com": true},
		hashes: map[string]bool{md5HashLower("byhash@icloud.com"): true},
	})
	if !ps.HasSuppressionHub() {
		t.Fatal("HasSuppressionHub false after SetSuppressionHub")
	}
	got, dropped := ps.classifyAndFilter(recs)
	if dropped != 2 || len(got) != 1 || got[0].Email != "kept@outlook.com" {
		t.Fatalf("attached hub: survivors=%v dropped=%d, want only kept@ and 2 dropped", got, dropped)
	}
}

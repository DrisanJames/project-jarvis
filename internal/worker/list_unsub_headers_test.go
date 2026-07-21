package worker

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// Per-domain correctness of the shared RFC 8058 helper: for every sending
// domain shape in the estate (legacy PMTA brands, SES-lane brands, and the
// KumoMTA warmup properties) the https one-click leg must live on THAT
// domain's tracking host and the mailto leg must align with the From domain.
func TestBuildListUnsubscribeHeaders_PerDomainURLs(t *testing.T) {
	const (
		orgID  = "11111111-1111-1111-1111-111111111111"
		campID = "22222222-2222-2222-2222-222222222222"
		subID  = "33333333-3333-3333-3333-333333333333"
		secret = "test-secret"
	)
	cases := []struct {
		brandRoot string
		fromEmail string
	}{
		{"quizfiesta.com", "news@em.quizfiesta.com"},           // the Postmaster example domain
		{"discountblog.com", "deals@em.discountblog.com"},      // legacy PMTA brand
		{"myownhealth.net", "health@em.myownhealth.net"},       // server B brand
		{"bestcreditcare.com", "team@em.bestcreditcare.com"},   // kumo warmup property
		{"us-finance.com", "finance@em.us-finance.com"},        // kumo, hyphenated apex
		{"homewarrantyservices.org", "hank@em.homewarrantyservices.org"}, // non-.com TLD
	}
	for _, tc := range cases {
		t.Run(tc.brandRoot, func(t *testing.T) {
			trackBase := "https://trk.em." + tc.brandRoot
			headers := make(map[string]string)
			_, brandUnsubURL := BuildListUnsubscribeHeaders(
				orgID, campID, subID, tc.brandRoot, tc.fromEmail, trackBase, secret, headers)

			lu := headers["List-Unsubscribe"]
			if lu == "" {
				t.Fatalf("no List-Unsubscribe header for %s", tc.brandRoot)
			}
			// https leg: on this brand's tracking host, signed-token route.
			wantPrefix := trackBase + "/track/unsubscribe/"
			if !strings.HasPrefix(brandUnsubURL, wantPrefix) {
				t.Errorf("brand unsub URL %q not on %q", brandUnsubURL, wantPrefix)
			}
			if !strings.Contains(lu, "<"+brandUnsubURL+">") {
				t.Errorf("https leg missing from header: %s", lu)
			}
			// The signed URL shape is /track/unsubscribe/{data}/{sig}.
			rest := strings.TrimPrefix(brandUnsubURL, wantPrefix)
			segs := strings.Split(rest, "/")
			if len(segs) != 2 || segs[0] == "" || segs[1] == "" {
				t.Fatalf("brand unsub URL not {data}/{sig} shaped: %q", brandUnsubURL)
			}
			// Token decodes to org|campaign|subscriber|brandRoot.
			decoded, err := base64.URLEncoding.DecodeString(segs[0])
			if err != nil {
				t.Fatalf("token not base64url: %v", err)
			}
			want := fmt.Sprintf("%s|%s|%s|%s", orgID, campID, subID, tc.brandRoot)
			if string(decoded) != want {
				t.Errorf("token payload = %q, want %q", decoded, want)
			}
			// Signature must verify with the same TrackSign the handlers use.
			if TrackSign(segs[0], secret) != segs[1] {
				t.Errorf("signature does not verify for %s", tc.brandRoot)
			}
			// mailto leg aligned with the From domain.
			if !strings.Contains(lu, "@em."+tc.brandRoot+"?subject=unsubscribe>") {
				t.Errorf("mailto leg not aligned to From domain em.%s: %s", tc.brandRoot, lu)
			}
			if headers["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
				t.Errorf("List-Unsubscribe-Post = %q", headers["List-Unsubscribe-Post"])
			}
		})
	}
}

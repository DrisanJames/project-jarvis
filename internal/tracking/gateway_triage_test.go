package tracking

// 2026-09-07 triage contract: every gateway decision names the ROW that made
// it (cidr + evidence_source), the handler writes one "GATEWAY …" line per /o/
// request whether it forwards or withholds, and the 5-minute summary rolls
// decisions up by action and by class/source. Observability only — these
// tests also pin that none of it changes what is served.

import (
	"bytes"
	"context"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func stubClassifierWithSource(t *testing.T, rows map[string][2]string) *IPClassifier {
	t.Helper()
	c := &IPClassifier{}
	c.loadFn = func(context.Context) ([]ipClassEntry, error) {
		var out []ipClassEntry
		for cidr, cs := range rows {
			_, n, err := net.ParseCIDR(cidr)
			if err != nil {
				t.Fatalf("bad test CIDR %q: %v", cidr, err)
			}
			out = append(out, ipClassEntry{net: n, class: cs[0], cidr: n.String(), source: cs[1]})
		}
		return out, nil
	}
	if err := c.reloadOnce(context.Background()); err != nil {
		t.Fatalf("reloadOnce: %v", err)
	}
	return c
}

func TestGatewayTriage_ClassifyDetailNamesTheRow(t *testing.T) {
	c := stubClassifierWithSource(t, map[string][2]string{
		"3.0.0.0/8":       {"hosting", "ignite_datacenter_ranges"},
		"3.128.0.0/9":     {"scanner", "published-aws"},
		"24.117.63.53/32": {"scanner", "bot-nominate-20260907"},
	})
	class, cidr, src := c.ClassifyDetail("3.140.1.1")
	if class != "scanner" || cidr != "3.128.0.0/9" || src != "published-aws" {
		t.Fatalf("narrowest row must win with its source: got %s %s %s", class, cidr, src)
	}
	class, cidr, src = c.ClassifyDetail("3.1.1.1")
	if class != "hosting" || cidr != "3.0.0.0/8" || src != "ignite_datacenter_ranges" {
		t.Fatalf("got %s %s %s", class, cidr, src)
	}
	if class, cidr, src = c.ClassifyDetail("8.8.8.8"); class != "" || cidr != "" || src != "" {
		t.Fatalf("no row must be empty triple, got %q %q %q", class, cidr, src)
	}
	var nilc *IPClassifier
	if class, cidr, src = nilc.ClassifyDetail("1.1.1.1"); class != "" || cidr != "" || src != "" {
		t.Fatal("nil classifier must be empty triple")
	}
}

func TestGatewayTriage_DecisionCarriesRow_ShadowAndEnforced(t *testing.T) {
	t.Setenv(GatewayEnforceEnv, "")
	c := stubClassifierWithSource(t, map[string][2]string{"24.117.63.53/32": {"scanner", "bot-nominate-20260907"}})
	d := c.Decide("24.117.63.53", "https://www.example.com/offer")
	if !d.Shadow || d.Withhold || d.CIDR != "24.117.63.53/32" || d.Source != "bot-nominate-20260907" {
		t.Fatalf("shadow decision must carry the row: %+v", d)
	}
	t.Setenv(GatewayEnforceEnv, "1")
	d = c.Decide("24.117.63.53", "https://www.example.com/offer")
	if !d.Withhold || d.CIDR != "24.117.63.53/32" || d.Source != "bot-nominate-20260907" {
		t.Fatalf("enforced decision must carry the row: %+v", d)
	}
	// A forward from an unclassified address carries nothing — and forwards.
	d = c.Decide("98.0.0.1", "https://www.example.com/offer")
	if d.Withhold || d.Shadow || d.CIDR != "" || d.Source != "" {
		t.Fatalf("unclassified forward must be empty: %+v", d)
	}
}

func TestGatewayTriage_HandlerWritesOneGatewayLinePerRequest(t *testing.T) {
	t.Setenv(GatewayEnforceEnv, "1")
	t.Setenv(GatewayDisabledEnv, "")
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	sub, camp := "470d0831-8b2d-4b8a-8753-2515c23c9f1e", "3abcaa96-94b7-4451-bf82-250506186d14"
	hash := "w6ex7q8qzy"
	dict := stubDict(map[string]smartLinkEntry{hash: {Destination: "https://www.libertymutual.com/quotes?x=1", BrandRoot: "myownhealth.net"}})
	ipc := stubClassifierWithSource(t, map[string][2]string{"24.117.63.53/32": {"scanner", "bot-nominate-20260907"}})
	h := NewHandlerWithClassifier(&capturePublisher{}, dict, ipc)

	// withheld
	req := httptest.NewRequest(http.MethodGet, "/o/"+sub+"/"+hash+"/"+camp, nil)
	req.Header.Set("X-Forwarded-For", "24.117.63.53")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
	out := buf.String()
	want := `GATEWAY decision=withheld class="scanner" cidr="24.117.63.53/32" src="bot-nominate-20260907" ip=24.117.63.53 sub=` + sub + " campaign=" + camp + " hash=" + hash + " dest_host=www.libertymutual.com"
	if !strings.Contains(out, want) {
		t.Fatalf("missing triage line:\nwant %s\ngot  %s", want, out)
	}
	if !strings.Contains(out, "OFFER WITHHELD") || !strings.Contains(out, "src=bot-nominate-20260907") {
		t.Fatalf("withheld line must name the source: %s", out)
	}

	// forwarded, unclassified
	buf.Reset()
	req = httptest.NewRequest(http.MethodGet, "/o/"+sub+"/"+hash+"/"+camp, nil)
	req.Header.Set("X-Forwarded-For", "98.0.0.1")
	rec = httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", rec.Code)
	}
	if !strings.Contains(buf.String(), `GATEWAY decision=forward class="" cidr="" src="" ip=98.0.0.1`) {
		t.Fatalf("forward must also write the triage line: %s", buf.String())
	}
}

func TestGatewayTriage_SummaryRollsUpByActionAndSource(t *testing.T) {
	g := &gatewayStats{since: time.Now(), byAction: map[string]int{}, bySource: map[string]int{}}
	g.count(GatewayDecision{Class: "scanner", Withhold: true, Action: GatewayActionWithheld, Source: "published-aws"})
	g.count(GatewayDecision{Class: "scanner", Withhold: true, Action: GatewayActionWithheld, Source: "published-aws"})
	g.count(GatewayDecision{Class: "scanner", Shadow: true, Action: GatewayActionShadowWithheld, Source: "observed-traffic"})
	g.count(GatewayDecision{})
	g.count(GatewayDecision{Exempt: true})
	line := g.flush(5*time.Minute, 72, true, false)
	for _, w := range []string{"GATEWAY SUMMARY", "total=5", "enforce=true", "fanout=false", "prefixes=72",
		"withheld=2", "shadow_withheld=1", "forward=1", "exempt=1", "scanner/published-aws=2", "scanner/observed-traffic=1"} {
		if !strings.Contains(line, w) {
			t.Fatalf("summary missing %q: %s", w, line)
		}
	}
	if g.flush(time.Minute, 0, false, false); len(g.byAction) != 0 {
		t.Fatal("flush must reset the window")
	}
}

// Command attribution-match is the CLI counterpart to the
// /api/mailing/attribution/match-csv endpoint. It lets ops run the same
// matcher against any database (local dev, prod via DATABASE_URL) without
// going through the web frontend, so the audit trail produced from the UI
// can be cross-checked against the same logic.
//
// Usage:
//
//	DATABASE_URL=postgres://... \
//	  go run ./cmd/attribution-match \
//	    --clicks ~/Desktop/trugreen-clicks.csv \
//	    --conversions ~/Desktop/trugreen-ConversionsExport_2026-04-01_2026-04-27.csv \
//	    --offer "%trugreen%" \
//	    --org-id 11111111-2222-3333-4444-555555555555 \
//	    --format markdown
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"time"

	_ "github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/api"
	"github.com/ignite/sparkpost-monitor/internal/guard"
)

func main() {
	clicksPath := flag.String("clicks", "", "path to Everflow clicks CSV (optional)")
	conversionsPath := flag.String("conversions", "", "path to Everflow conversions CSV (optional)")
	orgID := flag.String("org-id", "", "organization UUID to scope the match (optional)")
	offer := flag.String("offer", "%trugreen%", "ILIKE pattern matched against link_url")
	format := flag.String("format", "markdown", "output format: markdown | json")
	output := flag.String("output", "-", "output file path; '-' writes to stdout")
	noDB := flag.Bool("no-db", false, "skip the DB lookup; only parse CSVs and emit per-row join keys (useful when DATABASE_URL is unavailable)")
	flag.Parse()

	if *clicksPath == "" && *conversionsPath == "" {
		log.Fatal("at least one of --clicks or --conversions is required")
	}

	clicks, err := loadClicks(*clicksPath)
	if err != nil {
		log.Fatalf("load clicks: %v", err)
	}
	conversions, err := loadConversions(*conversionsPath)
	if err != nil {
		log.Fatalf("load conversions: %v", err)
	}

	out, closeOut, err := openOutput(*output)
	if err != nil {
		log.Fatalf("open output: %v", err)
	}
	defer closeOut()

	if *noDB {
		writeJoinKeys(out, *format, clicks, conversions, *offer, *orgID)
		return
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required (or pass --no-db to emit join keys without lookups)")
	}
	// Read-only lookups: announce the target DB for visibility (never blocks).
	guard.RequireDBConfirmation(dsn, "attribution-match", false)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		log.Fatalf("ping db: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	res, err := api.MatchAttribution(ctx, db, clicks, conversions, api.AttributionOptions{
		OrgID:            *orgID,
		OfferLikePattern: *offer,
	})
	if err != nil {
		log.Fatalf("match: %v", err)
	}

	switch *format {
	case "json":
		writeJSON(out, res)
	default:
		writeMarkdown(out, res)
	}
}

func loadClicks(path string) ([]api.ClickRow, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return api.ParseEverflowClicksCSV(f)
}

func loadConversions(path string) ([]api.ConversionRow, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return api.ParseEverflowConversionsCSV(f)
}

func openOutput(path string) (io.Writer, func(), error) {
	if path == "-" || path == "" {
		return os.Stdout, func() {}, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, func() {}, err
	}
	return f, func() { _ = f.Close() }, nil
}

func writeJSON(w io.Writer, res api.AttributionResult) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		log.Fatalf("encode json: %v", err)
	}
}

func writeMarkdown(w io.Writer, res api.AttributionResult) {
	fmt.Fprintf(w, "# Attribution Match Report\n\n")
	fmt.Fprintf(w, "_Generated %s UTC_\n\n", res.GeneratedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "Org: `%s` | Offer pattern: `%s` | Tight window: %ds | Fallback: %dh\n\n",
		emptyOr(res.OrgID, "(none)"), res.OfferLinkPattern, res.WindowSeconds, res.FallbackWindowHours)

	fmt.Fprintf(w, "## Summary\n\n")
	fmt.Fprintf(w, "| | Total | Matched | Unmatched |\n|---|---:|---:|---:|\n")
	fmt.Fprintf(w, "| Clicks | %d | %d | %d |\n", res.TotalClicks, len(res.MatchedClicks), len(res.UnmatchedClicks))
	fmt.Fprintf(w, "| Conversions | %d | %d | %d |\n\n", res.TotalConversions, len(res.MatchedConversions), len(res.UnmatchedConversions))

	if len(res.UnmatchedReasons) > 0 {
		fmt.Fprintf(w, "### Unmatched reasons\n\n| reason | count |\n|---|---:|\n")
		keys := make([]string, 0, len(res.UnmatchedReasons))
		for k := range res.UnmatchedReasons {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "| `%s` | %d |\n", k, res.UnmatchedReasons[k])
		}
		fmt.Fprintln(w)
	}

	if len(res.MatchedClicks) > 0 {
		fmt.Fprintf(w, "## Matched clicks\n\n")
		fmt.Fprintf(w, "| # | csv ts (UTC) | ip | tier | offset(s) | email | sub_id | campaign |\n")
		fmt.Fprintf(w, "|---:|---|---|---|---:|---|---|---|\n")
		for _, m := range res.MatchedClicks {
			fmt.Fprintf(w, "| %d | %s | %s | %s | %d | %s | `%s` | %s |\n",
				m.Row.RowIndex,
				m.Row.Timestamp.UTC().Format("2006-01-02 15:04:05"),
				m.Row.IPAddress,
				m.ConfidenceTier,
				m.OffsetSeconds,
				m.Subscriber.Email,
				short(m.Subscriber.SubscriberID),
				emptyOr(m.CampaignName, short(m.CampaignID)),
			)
		}
		fmt.Fprintln(w)
	}

	if len(res.UnmatchedClicks) > 0 {
		fmt.Fprintf(w, "## Unmatched clicks\n\n")
		fmt.Fprintf(w, "| # | csv ts (UTC) | ip | offer | reason |\n")
		fmt.Fprintf(w, "|---:|---|---|---|---|\n")
		for _, u := range res.UnmatchedClicks {
			fmt.Fprintf(w, "| %d | %s | %s | %s | `%s` |\n",
				u.Row.RowIndex,
				u.Row.Timestamp.UTC().Format("2006-01-02 15:04:05"),
				u.Row.IPAddress,
				truncate(u.Row.OfferName, 40),
				u.Reason,
			)
		}
		fmt.Fprintln(w)
	}

	if len(res.MatchedConversions) > 0 {
		fmt.Fprintf(w, "## Matched conversions\n\n")
		fmt.Fprintf(w, "| # | click_date (UTC) | session_ip | tier | offset(s) | revenue | email | sub_id |\n")
		fmt.Fprintf(w, "|---:|---|---|---|---:|---:|---|---|\n")
		for _, m := range res.MatchedConversions {
			fmt.Fprintf(w, "| %d | %s | %s | %s | %d | %.2f | %s | `%s` |\n",
				m.Row.RowIndex,
				m.Row.ClickTime.UTC().Format("2006-01-02 15:04:05"),
				m.Row.SessionUserIP,
				m.ConfidenceTier,
				m.OffsetSeconds,
				m.Row.Revenue,
				m.Subscriber.Email,
				short(m.Subscriber.SubscriberID),
			)
		}
		fmt.Fprintln(w)
	}

	if len(res.UnmatchedConversions) > 0 {
		fmt.Fprintf(w, "## Unmatched conversions\n\n")
		fmt.Fprintf(w, "| # | click_date (UTC) | session_ip | revenue | reason |\n")
		fmt.Fprintf(w, "|---:|---|---|---:|---|\n")
		for _, u := range res.UnmatchedConversions {
			fmt.Fprintf(w, "| %d | %s | %s | %.2f | `%s` |\n",
				u.Row.RowIndex,
				u.Row.ClickTime.UTC().Format("2006-01-02 15:04:05"),
				u.Row.SessionUserIP,
				u.Row.Revenue,
				u.Reason,
			)
		}
		fmt.Fprintln(w)
	}
}

// writeJoinKeys is the --no-db output: the per-row join keys plus the exact
// SQL the matcher would run. Lets the operator validate the matcher's
// behavior without DB access (e.g. while you're producing the cross-check
// report from your laptop and only have prod creds in a separate env).
func writeJoinKeys(w io.Writer, format string, clicks []api.ClickRow, conversions []api.ConversionRow, offer string, orgID string) {
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]interface{}{
			"offer_pattern": offer,
			"org_id":        orgID,
			"clicks":        clicks,
			"conversions":   conversions,
		})
		return
	}

	fmt.Fprintf(w, "# Attribution Join-Key Preview (no DB lookup)\n\n")
	fmt.Fprintf(w, "Offer pattern: `%s` | Org: `%s`\n\n", offer, emptyOr(orgID, "(none)"))

	fmt.Fprintf(w, "## SQL each click row will run\n\n")
	fmt.Fprintf(w, "```sql\nSELECT e.id, e.subscriber_id, e.campaign_id, e.link_url, e.event_at\nFROM mailing_tracking_events e\nLEFT JOIN mailing_campaigns c ON c.id = e.campaign_id\nWHERE e.event_type = 'clicked'\n  AND e.ip_address = $ip::inet\n  AND e.event_at BETWEEN $ts - interval '120 seconds' AND $ts + interval '120 seconds'\n  AND e.link_url ILIKE '%s'\nORDER BY abs(extract(epoch FROM e.event_at - $ts))\nLIMIT 1;\n```\n\n", offer)

	fmt.Fprintf(w, "## Click rows (n=%d)\n\n", len(clicks))
	fmt.Fprintf(w, "| # | csv ts (UTC) | ip | bot? | offer |\n|---:|---|---|---|---|\n")
	for _, c := range clicks {
		fmt.Fprintf(w, "| %d | %s | %s | %v | %s |\n",
			c.RowIndex, c.Timestamp.UTC().Format("2006-01-02 15:04:05"), c.IPAddress, api.IsBotScannerIP(c.IPAddress), truncate(c.OfferName, 40))
	}

	fmt.Fprintf(w, "\n## Conversion rows (n=%d)\n\n", len(conversions))
	fmt.Fprintf(w, "| # | click_date (UTC) | session_ip | bot? | revenue |\n|---:|---|---|---|---:|\n")
	for _, c := range conversions {
		fmt.Fprintf(w, "| %d | %s | %s | %v | %.2f |\n",
			c.RowIndex, c.ClickTime.UTC().Format("2006-01-02 15:04:05"), c.SessionUserIP, api.IsBotScannerIP(c.SessionUserIP), c.Revenue)
	}
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

func emptyOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

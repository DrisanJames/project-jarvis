package analytics

// Bot-IP nomination, lake half. The SQL is a faithful port of
// agents/jobs/bot_ip_nominate.py nominate_lake() (link-agnostic, operator
// 2026-09-06): the lake spelling is PRESENT-tense 'click', the partition column
// is `dt` (UTC, 'YYYY-MM-DD'), timestamps are event_epoch_ms, the IP is
// source_ip. The burst is a self-join on subscriber where b.link_url <>
// a.link_url within the window — DISTINCT links is load-bearing, one click
// logs up to ~8 rows of the SAME link. The rule constants are rendered from
// the same values the worker enforces (internal/worker/bot_ip_nominator.go)
// so the two cannot drift apart.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// BotNominateLakeTable is the fully-qualified lake table, exactly as the Python
// names it. The other readers use the bare lakeTable under the reader's
// database context; the qualified name is correct under both.
const BotNominateLakeTable = "ignite_analytics.email_events"

// BotNomineeRow is one IP that matched the scanner signature in the lake.
type BotNomineeRow struct {
	IP      string
	Subs    int64
	CoClick int64
	Clicks  int64
}

// BuildBotNominateLakeSQL renders the lake nomination query.
// minSubs/minRate/coclickSeconds are the rule. Rendered literally (no bound
// parameters — see sqlStr).
func BuildBotNominateLakeSQL(days int, minSubs int, minRate float64, coclickSeconds int) string {
	if days <= 0 {
		days = 30
	}
	var b strings.Builder
	b.WriteString("WITH ev AS (")
	b.WriteString(" SELECT subscriber_id, source_ip, link_url, event_epoch_ms FROM ")
	b.WriteString(BotNominateLakeTable)
	b.WriteString(" WHERE event_type='click' AND dt >= date_format(date_add('day',-")
	b.WriteString(strconv.Itoa(days))
	b.WriteString(",current_date),'%Y-%m-%d')")
	b.WriteString(" AND source_ip IS NOT NULL AND source_ip <> ''),")
	b.WriteString(" mo AS (SELECT subscriber_id, source_ip, link_url, event_epoch_ms FROM ev),")
	b.WriteString(" cc AS (SELECT DISTINCT a.subscriber_id FROM mo a JOIN mo b ON b.subscriber_id=a.subscriber_id")
	b.WriteString(" AND b.link_url <> a.link_url AND abs(b.event_epoch_ms-a.event_epoch_ms) <= ")
	b.WriteString(strconv.Itoa(coclickSeconds * 1000))
	b.WriteString("),")
	b.WriteString(" per AS (SELECT mo.source_ip ip, count(DISTINCT mo.subscriber_id) subs,")
	b.WriteString(" count(DISTINCT cc.subscriber_id) coclick, count(*) clicks")
	b.WriteString(" FROM mo LEFT JOIN cc ON cc.subscriber_id=mo.subscriber_id GROUP BY mo.source_ip)")
	b.WriteString(" SELECT ip, subs, coclick, clicks FROM per WHERE subs >= ")
	b.WriteString(strconv.Itoa(minSubs))
	b.WriteString(" AND coclick*1.0/subs >= ")
	b.WriteString(strconv.FormatFloat(minRate, 'f', -1, 64))
	b.WriteString(" ORDER BY clicks DESC")
	return b.String()
}

// BotNominateLake runs the lake nomination query and returns the matching IPs.
func (r *Reader) BotNominateLake(ctx context.Context, days int, minSubs int, minRate float64, coclickSeconds int) ([]BotNomineeRow, error) {
	_, rows, err := r.runQuery(ctx, BuildBotNominateLakeSQL(days, minSubs, minRate, coclickSeconds))
	if err != nil {
		return nil, err
	}
	out := make([]BotNomineeRow, 0, len(rows))
	for _, row := range rows {
		if len(row) < 4 {
			return nil, fmt.Errorf("bot-nominate lake row has %d columns, want 4", len(row))
		}
		n := func(i int) int64 { v, _ := strconv.ParseInt(strings.TrimSpace(row[i]), 10, 64); return v }
		out = append(out, BotNomineeRow{
			IP:      strings.TrimSpace(row[0]),
			Subs:    n(1),
			CoClick: n(2),
			Clicks:  n(3),
		})
	}
	return out, nil
}

// BotNominateLake runs against the global reader. Returns errDisabled when the
// reader is not configured (IsDisabledErr reports it).
func BotNominateLake(ctx context.Context, days int, minSubs int, minRate float64, coclickSeconds int) ([]BotNomineeRow, error) {
	r := getReader()
	if r == nil {
		return nil, errDisabled
	}
	return r.BotNominateLake(ctx, days, minSubs, minRate, coclickSeconds)
}

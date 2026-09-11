package contentdesk

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Several health signals are produced on the operator's Mac, not in ECS (job
// watchdog status.json, newsletter supply runway, live site probes, the
// tracking service's TRACKSIG summaries — its /health is Forbidden
// externally). They arrive via POST ops-report and are merged, with the
// checks ignite-server can compute itself, into ops-status.checks[].

// Check statuses.
const (
	CheckOK      = "ok"
	CheckWarn    = "warn"
	CheckFail    = "fail"
	CheckUnknown = "unknown"
)

// OpsCheck is one row of ops-status.checks[].
type OpsCheck struct {
	Name      string     `json:"name"`
	Status    string     `json:"status"`
	Detail    string     `json:"detail"`
	CheckedAt *time.Time `json:"checked_at"`
}

// OpsReport is the POST ops-report body and one content_ops_reports row
// (latest per (org, source)).
type OpsReport struct {
	Source      string          `json:"source"`
	GeneratedAt time.Time       `json:"generated_at"`
	Checks      []OpsCheck      `json:"checks"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	ReceivedAt  time.Time       `json:"received_at,omitempty"`
}

var opsSourceRE = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// Validate checks the report shape.
func (r OpsReport) Validate() error {
	if !opsSourceRE.MatchString(r.Source) {
		return fmt.Errorf("%w: source must match [a-z0-9_]{1,64}", ErrInvalid)
	}
	if r.GeneratedAt.IsZero() {
		return fmt.Errorf("%w: generated_at (RFC3339) is required", ErrInvalid)
	}
	for i, c := range r.Checks {
		if strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("%w: checks[%d].name is required", ErrInvalid, i)
		}
		switch c.Status {
		case CheckOK, CheckWarn, CheckFail, CheckUnknown:
		default:
			return fmt.Errorf("%w: checks[%d].status must be ok|warn|fail|unknown", ErrInvalid, i)
		}
	}
	if len(r.Payload) > 0 && !json.Valid(r.Payload) {
		return fmt.Errorf("%w: payload is not JSON", ErrInvalid)
	}
	return nil
}

// Report cadences. A report older than 2× its cadence is stale.
const DefaultReportCadence = 26 * time.Hour

// ReportCadence is the expected cadence per source; anything unlisted uses
// DefaultReportCadence.
var ReportCadence = map[string]time.Duration{
	"job_watchdog": 26 * time.Hour,
	"site_probes":  2 * time.Hour,
	"tracking_sig": 1 * time.Hour,
}

// CadenceFor returns a source's expected cadence.
func CadenceFor(source string) time.Duration {
	if d, ok := ReportCadence[source]; ok {
		return d
	}
	return DefaultReportCadence
}

// FormatAge renders a duration as e.g. "3h12m" / "2d4h" / "45m".
func FormatAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Minute)
	days := int(d / (24 * time.Hour))
	h := int((d % (24 * time.Hour)) / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, h)
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// MergeReportChecks turns stored reports into checks, sorted by source. Each
// check's name is prefixed "<source>: ". A stale report collapses to ONE warn
// check — its old checks are not shown as if they were current.
func MergeReportChecks(reports []OpsReport, now time.Time) []OpsCheck {
	sorted := append([]OpsReport(nil), reports...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Source < sorted[j].Source })
	out := []OpsCheck{}
	for _, r := range sorted {
		age := now.Sub(r.GeneratedAt)
		if age > 2*CadenceFor(r.Source) {
			gen := r.GeneratedAt
			out = append(out, OpsCheck{
				Name:      fmt.Sprintf("%s: stale report (last %s)", r.Source, FormatAge(age)),
				Status:    CheckWarn,
				Detail:    fmt.Sprintf("generated_at %s; expected every %s", r.GeneratedAt.UTC().Format(time.RFC3339), FormatAge(CadenceFor(r.Source))),
				CheckedAt: &gen,
			})
			continue
		}
		for _, c := range r.Checks {
			c.Name = r.Source + ": " + c.Name
			if c.CheckedAt == nil {
				gen := r.GeneratedAt
				c.CheckedAt = &gen
			}
			out = append(out, c)
		}
	}
	return out
}

// SupplyConsumers extracts payload.consumers from the supply_runway report
// (nil when absent).
func SupplyConsumers(reports []OpsReport) json.RawMessage {
	for _, r := range reports {
		if r.Source != "supply_runway" || len(r.Payload) == 0 {
			continue
		}
		var p struct {
			Consumers json.RawMessage `json:"consumers"`
		}
		if err := json.Unmarshal(r.Payload, &p); err == nil && len(p.Consumers) > 0 && string(p.Consumers) != "null" {
			return p.Consumers
		}
	}
	return nil
}

// TrackSigState is ignite-server's own /health track_sig block (mode, key
// count, cumulative counters keyed "<endpoint>.sig.<class>").
type TrackSigState struct {
	Mode     string
	Keys     int
	Counters map[string]int64
}

// ServerSignals is what ignite-server knows about itself.
type ServerSignals struct {
	TrackSig           *TrackSigState
	PreferencesModeSet bool
	PreferencesMode    string
}

// ServerChecks computes the server-side checks from an assembled OpsStatus
// (kill switch, budget, last pipeline error) plus the server signals.
func ServerChecks(o *OpsStatus, sig ServerSignals, now time.Time) []OpsCheck {
	at := now
	chk := func(name, status, detail string) OpsCheck {
		return OpsCheck{Name: "server: " + name, Status: status, Detail: detail, CheckedAt: &at}
	}
	var out []OpsCheck

	if sig.TrackSig == nil {
		out = append(out, chk("track_sig", CheckUnknown, "track_sig status unavailable"))
	} else {
		var invalid, checked int64
		for k, v := range sig.TrackSig.Counters {
			if strings.Contains(k, ".sig.") {
				checked += v
				if strings.HasSuffix(k, ".sig.invalid") {
					invalid += v
				}
			}
		}
		detail := fmt.Sprintf("mode=%s keys=%d checked=%d invalid=%d", sig.TrackSig.Mode, sig.TrackSig.Keys, checked, invalid)
		switch {
		case sig.TrackSig.Mode != "off" && sig.TrackSig.Keys == 0:
			out = append(out, chk("track_sig", CheckFail, detail+" — no TRACKING_SECRET keys loaded, nothing can verify"))
		case sig.TrackSig.Mode == "off":
			out = append(out, chk("track_sig", CheckWarn, detail+" — verification off"))
		default:
			out = append(out, chk("track_sig", CheckOK, detail))
		}
	}

	if o.KillSwitch.Enabled {
		out = append(out, chk("content_desk_enabled", CheckOK, EnvEnabled+"=1"))
	} else {
		out = append(out, chk("content_desk_enabled", CheckWarn, EnvEnabled+" is not 1 — the pipeline will not run"))
	}

	budget := fmt.Sprintf("$%.2f of $%.2f (%s)", o.Spend.TodayUSD, o.Spend.BudgetUSD, o.Spend.Day)
	switch {
	case o.Spend.BudgetUSD <= 0 || o.Spend.TodayUSD >= o.Spend.BudgetUSD:
		out = append(out, chk("budget", CheckFail, budget+" — exhausted; calls fail closed"))
	case o.Spend.TodayUSD >= 0.8*o.Spend.BudgetUSD:
		out = append(out, chk("budget", CheckWarn, budget+" — ≥80% used"))
	default:
		out = append(out, chk("budget", CheckOK, budget))
	}

	switch {
	case o.LastPipelineError == nil:
		out = append(out, chk("last_pipeline_error", CheckOK, "no failed stage runs"))
	case o.LastPipelineError.At == nil:
		out = append(out, chk("last_pipeline_error", CheckWarn, o.LastPipelineError.Stage+": "+clip(o.LastPipelineError.Error, 160)))
	default:
		age := now.Sub(*o.LastPipelineError.At)
		st := CheckOK
		if age < 24*time.Hour {
			st = CheckWarn
		}
		out = append(out, chk("last_pipeline_error", st, fmt.Sprintf("%s ago, %s: %s", FormatAge(age), o.LastPipelineError.Stage, clip(o.LastPipelineError.Error, 160))))
	}

	if sig.PreferencesModeSet {
		out = append(out, chk("preferences_mode", CheckOK, "PREFERENCES_MODE="+sig.PreferencesMode))
	} else {
		out = append(out, chk("preferences_mode", CheckWarn, "PREFERENCES_MODE is not set"))
	}
	return out
}

// ApplyOpsChecks fills o.Checks (server checks first, then reports) and
// o.Supply.Consumers.
func ApplyOpsChecks(o *OpsStatus, reports []OpsReport, sig ServerSignals, now time.Time) {
	o.Checks = append(ServerChecks(o, sig, now), MergeReportChecks(reports, now)...)
	o.Supply.Consumers = SupplyConsumers(reports)
}

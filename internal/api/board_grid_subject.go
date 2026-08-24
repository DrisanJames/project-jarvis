package api

// Gate 4's subject check — LIQUID_SUBJECT.
//
// The original gate was a bare /\{\{|\{%/ regex over the STORED subject. It is
// wrong in BOTH directions, which is why it produced 5 blockers on a correct
// 08/24 board while still not being able to catch the defect it was built for:
//
//   - It BLOCKS `{{ first_name | default: "Homeowner" }}` — the ONE
//     personalization idiom verified safe against missing-key, "" and set
//     (memory: liquid-personalization-guard-idioms, measured 2026-08-18 against
//     this same engine). That is the idiom an operator is supposed to write, so
//     the gate was punishing correct copy.
//   - It PASSES any subject whose Liquid parses but renders to a HOLE, which IS
//     the RR-HELOC defect: `{{custom.equity_estimate}}` resolves to "" for a
//     subscriber with no such field and ships "Your equity is  — see more".
//     Proven against the live engine: that exact string renders with no error.
//
// So gate on the RENDERED result. The subject is rendered through the SAME
// mailing.TemplateService the send worker uses (send_worker.go:1867) against
// the WORST-CASE recipient: every top-level subscriber field present but EMPTY
// and no custom.* keys at all. That is not a pessimistic hypothetical — it is
// ~90% of the base (first_name coverage is 8.7% of all subscribers, 9.5% of the
// engaged tier, measured 2026-08-18).
//
// Four failure shapes, all blockers:
//   1. the template does not parse/render      → the send worker QUARANTINES an
//      unrenderable body (send_worker.go:1892), so the campaign sends nothing;
//   2. braces survive the render               → raw Liquid would reach an inbox;
//   3. it renders to nothing                   → an empty subject line;
//   4. it renders with a HOLE                  → the RR-HELOC shape.

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

var (
	// Hole shapes left behind when a token resolves to "". Kept deliberately
	// narrow: each one is a artifact of a REMOVED word, not ordinary copy.
	reSubjDoubleSpace = regexp.MustCompile(`\s{2,}`)
	reSubjSpacePunct  = regexp.MustCompile(`\s+[,.!?;:]`)
	reSubjLeadPunct   = regexp.MustCompile(`^\s*[,.!?;:]`)
	reSubjDangleDash  = regexp.MustCompile(`[—–-]\s*$`)

	gateTemplateOnce sync.Once
	gateTemplateSvc  *mailing.TemplateService
)

func gateTemplate() *mailing.TemplateService {
	gateTemplateOnce.Do(func() { gateTemplateSvc = mailing.NewTemplateService() })
	return gateTemplateSvc
}

// worstCaseRenderContext mirrors the key SHAPE of
// SendWorkerPool.buildRenderContext (send_worker.go:3253) with every
// subscriber-supplied value empty. brand.* and system.* are populated because
// they are 100%-coverage at send time — a subject built on {{ brand.name }} or
// {{ system.dispatch_date }} is correct and must not be flagged.
func worstCaseRenderContext() map[string]interface{} {
	now := time.Now()
	return map[string]interface{}{
		"first_name":   "",
		"last_name":    "",
		"full_name":    "",
		"email":        "",
		"email_local":  "",
		"email_domain": "",
		"custom":       map[string]interface{}{},
		"brand":        map[string]interface{}{"domain": "example.com", "name": "Brand"},
		"engagement": map[string]interface{}{
			"score": 0, "total_emails": 0, "total_opens": 0, "total_clicks": 0,
		},
		"system": map[string]interface{}{
			"current_date":    now.Format("January 2, 2006"),
			"current_year":    now.Year(),
			"current_month":   now.Month().String(),
			"current_day":     now.Day(),
			"current_weekday": now.Weekday().String(),
			"current_hour":    now.Hour(),
			"dispatch_date":   now.Format("January 2, 2006"),
		},
		"now": now,
	}
}

// subjectRenderProblem returns "" when the subject is safe to ship, or an
// operator-facing explanation of what it renders to when it is not.
func subjectRenderProblem(subject string) string {
	if strings.TrimSpace(subject) == "" {
		return "" // an empty subject is a different gate's problem, not this one
	}
	if !reLiquid.MatchString(subject) {
		return "" // no template at all — nothing to render, nothing to hole
	}
	out, err := gateTemplate().Render("", subject, worstCaseRenderContext())
	if err != nil {
		return fmt.Sprintf("subject does not render (%v) — the send worker quarantines an unrenderable body, so this campaign would send NOTHING: %q", err, subject)
	}
	if reLiquid.MatchString(out) {
		return fmt.Sprintf("subject still carries an unrendered template token AFTER rendering — raw Liquid would reach the inbox: %q", out)
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Sprintf("subject renders EMPTY for a recipient with no personalization data: %q", subject)
	}
	if reSubjDoubleSpace.MatchString(out) || reSubjSpacePunct.MatchString(out) ||
		reSubjLeadPunct.MatchString(out) || reSubjDangleDash.MatchString(out) {
		return fmt.Sprintf("subject renders with a HOLE for the ~90%% of recipients carrying no first_name/custom.* data — give the token a `| default: \"…\"` fallback. Renders as %q (source %q)", out, subject)
	}
	return ""
}

// renderForOperator shows what a field ACTUALLY becomes in the inbox for a
// recipient carrying no personalization data — the ~90% case. Returns the
// input unchanged when there is no template to render, and on any render
// failure (the failure itself is reported by subjectRenderProblem; this
// function's only job is display).
func renderForOperator(field string) string {
	if !reLiquid.MatchString(field) {
		return ""
	}
	out, err := gateTemplate().Render("", field, worstCaseRenderContext())
	if err != nil {
		return ""
	}
	return out
}

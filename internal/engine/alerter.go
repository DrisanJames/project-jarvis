package engine

import (
	"fmt"
	"log"
	"net/smtp"
	"strings"
	"sync"
	"time"
)

// Alerter sends email notifications for governance decisions.
type Alerter struct {
	smtpHost string
	smtpPort int
	smtpUser string
	smtpPass string
	from     string
	to       []string

	// Deduplication: suppress repeated alerts for the same key within a window
	mu       sync.Mutex
	cooldown map[string]time.Time
}

const alertCooldownWindow = 5 * time.Minute

// AlerterConfig holds alerter configuration.
type AlerterConfig struct {
	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string
	From     string
	To       []string
}

// NewAlerter creates a new email alerter.
func NewAlerter(cfg AlerterConfig) *Alerter {
	return &Alerter{
		smtpHost: cfg.SMTPHost,
		smtpPort: cfg.SMTPPort,
		smtpUser: cfg.SMTPUser,
		smtpPass: cfg.SMTPPass,
		from:     cfg.From,
		to:       cfg.To,
		cooldown: make(map[string]time.Time),
	}
}

// shouldAlert returns true if we haven't sent an alert for this key
// within the cooldown window. Thread-safe.
func (a *Alerter) shouldAlert(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if last, ok := a.cooldown[key]; ok && time.Since(last) < alertCooldownWindow {
		return false
	}
	a.cooldown[key] = time.Now()
	return true
}

// SendDecisionAlert sends an alert for a governance decision.
func (a *Alerter) SendDecisionAlert(d Decision) {
	subject := fmt.Sprintf("[%s] %s %s — %s", strings.ToUpper(string(d.ISP)), d.AgentType, d.ActionTaken, d.TargetValue)
	body := fmt.Sprintf(`PMTA Governance Decision
========================

ISP:         %s
Agent:       %s
Action:      %s
Target:      %s (%s)
Time:        %s
Result:      %s

Signal Values: %s
Action Params: %s

---
This is an automated alert from the PMTA Multi-Agent Traffic Governance Engine.
`,
		d.ISP, d.AgentType, d.ActionTaken,
		d.TargetValue, d.TargetType,
		d.CreatedAt.Format(time.RFC3339),
		d.Result,
		string(d.SignalValues),
		string(d.ActionParams),
	)

	a.sendEmail(subject, body)
}

// SendEmergencyAlert sends a high-priority emergency incident report.
func (a *Alerter) SendEmergencyAlert(incident IncidentReport) {
	subject := fmt.Sprintf("EMERGENCY: PMTA Traffic Halted — %s (%s)", incident.Trigger, incident.ISP)

	body := fmt.Sprintf(`!!!  EMERGENCY ALERT  !!!
==========================

ISP:        %s
Trigger:    %s
Detected:   %s
Affected IPs: %s
Affected Domains: %s

Trigger Metrics:
%s

Actions Taken:
%s

DSN Samples:
%s

MANUAL RESUME REQUIRED:
POST /api/mailing/engine/override {"action": "resume_all"}

---
This is an automated emergency alert from the PMTA Governance Engine.
`,
		incident.ISP,
		incident.Trigger,
		incident.DetectedAt.Format(time.RFC3339),
		strings.Join(incident.AffectedIPs, ", "),
		strings.Join(incident.AffectedDomains, ", "),
		string(incident.TriggerMetrics),
		strings.Join(incident.ActionsTaken, "\n  - "),
		strings.Join(incident.DSNSamples, "\n  "),
	)

	a.sendEmail(subject, body)
}

// SendVelocityAlert sends a suppression velocity anomaly alert.
func (a *Alerter) SendVelocityAlert(isp ISP, count5m int, threshold int) {
	subject := fmt.Sprintf("[%s] Suppression Velocity Alert: %d in 5min (threshold: %d)", strings.ToUpper(string(isp)), count5m, threshold)
	body := fmt.Sprintf(`Suppression Velocity Alert
==========================

ISP:       %s
Count:     %d suppressions in last 5 minutes
Threshold: %d

This may indicate a list quality issue or upstream data problem.
Check the suppression dashboard for affected campaigns.

---
Automated alert from PMTA Governance Engine.
`, isp, count5m, threshold)

	a.sendEmail(subject, body)
}

// SendBounceRateAlert fires when a campaign's hard bounce rate exceeds the
// threshold during a monitoring window. Deduplicated per campaign.
func (a *Alerter) SendBounceRateAlert(campaignID, campaignName string, bounceRate, threshold float64) {
	key := fmt.Sprintf("bounce-rate:%s", campaignID)
	if !a.shouldAlert(key) {
		return
	}
	subject := fmt.Sprintf("[BOUNCE RATE] Campaign %s — %.1f%% (threshold %.1f%%)", campaignName, bounceRate*100, threshold*100)
	body := fmt.Sprintf(`Bounce Rate Alert
===================

Campaign:   %s (%s)
Bounce Rate: %.2f%%
Threshold:   %.2f%%

A campaign has exceeded the hard bounce rate threshold. This may indicate
a list quality issue or authentication failure. Check campaign delivery
logs and consider pausing the campaign.

---
Automated alert from PMTA Infrastructure Monitor.
`, campaignName, campaignID, bounceRate*100, threshold*100)

	a.sendEmail(subject, body)
}

// SendDefaultPoolAlert fires when messages are detected in PMTA's {default}
// pool, which means they were injected without a valid VMTA assignment.
// Deduplicated per 5-minute window.
func (a *Alerter) SendDefaultPoolAlert(count int, vmtaName string) {
	key := "default-pool-leak"
	if !a.shouldAlert(key) {
		return
	}
	subject := fmt.Sprintf("[DEFAULT POOL] %d message(s) routed to {default} — DMARC rejection risk", count)
	body := fmt.Sprintf(`Default Pool Alert
=====================

Messages:   %d
VMTA:       %s

Messages were injected into PMTA without a valid Virtual MTA assignment
and routed through the {default} pool. This causes DMARC failures because
the server's main IP does not have DKIM/SPF configured for the sending domain.

Likely causes:
  - IP pool exhaustion (all IPs hit warmup daily limit)
  - Empty hostname on a selected IP
  - Missing X-Virtual-MTA header in injection payload

Immediate action: check vmtaPool state, IP warmup limits, and recent
bridge error logs.

---
Automated alert from PMTA Infrastructure Monitor.
`, count, vmtaName)

	a.sendEmail(subject, body)
}

// SendBridgeErrorAlert fires when the HTTP injection bridge returns
// consecutive errors, indicating a systemic failure. Deduplicated per server.
func (a *Alerter) SendBridgeErrorAlert(serverHost string, errorCount int, sampleError string) {
	key := fmt.Sprintf("bridge-error:%s", serverHost)
	if !a.shouldAlert(key) {
		return
	}
	subject := fmt.Sprintf("[BRIDGE ERROR] %s — %d consecutive failures", serverHost, errorCount)
	body := fmt.Sprintf(`Bridge Health Alert
======================

Server:     %s
Consecutive Failures: %d
Sample Error: %s

The PMTA HTTP injection bridge is returning repeated errors. Messages
will be requeued but delivery is stalled on this server.

Check bridge service status:
  ssh rocky@%s 'sudo systemctl status pmta-http-bridge'
  ssh rocky@%s 'sudo journalctl -u pmta-http-bridge -n 50'

---
Automated alert from PMTA Infrastructure Monitor.
`, serverHost, errorCount, sampleError, serverHost, serverHost)

	a.sendEmail(subject, body)
}

// SendPipelineReport sends a formatted email summarizing a data pipeline run.
func (a *Alerter) SendPipelineReport(report PipelineRunReport) error {
	subject := fmt.Sprintf("[IGNITE] Data Pipeline Complete — %d verified, %d suppressed", report.EmailsVerified, report.EmailsSuppressed)

	var sb strings.Builder
	sb.WriteString("Data Pipeline Run Report\n")
	sb.WriteString("========================\n\n")
	sb.WriteString(fmt.Sprintf("Run ID:       %s\n", report.RunID))
	sb.WriteString(fmt.Sprintf("Started:      %s\n", report.StartedAt.Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("Completed:    %s\n", report.CompletedAt.Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("Duration:     %s\n\n", report.CompletedAt.Sub(report.StartedAt).Round(time.Second)))

	sb.WriteString("Summary\n")
	sb.WriteString("-------\n")
	sb.WriteString(fmt.Sprintf("Files Processed:   %d\n", report.FilesProcessed))
	sb.WriteString(fmt.Sprintf("Emails Total:      %d\n", report.EmailsTotal))
	sb.WriteString(fmt.Sprintf("Emails Verified:   %d  (added to lists)\n", report.EmailsVerified))
	sb.WriteString(fmt.Sprintf("Emails Suppressed: %d  (sent to global suppression)\n", report.EmailsSuppressed))
	sb.WriteString(fmt.Sprintf("Emails Deduped:    %d  (already existed)\n\n", report.EmailsDeduped))

	if len(report.DomainBreakdown) > 0 {
		sb.WriteString("Domain Breakdown\n")
		sb.WriteString("----------------\n")
		for _, d := range report.DomainBreakdown {
			sb.WriteString(fmt.Sprintf("  %s / %s (%s): +%d added, %d suppressed, %d deduped\n",
				d.SendingDomain, d.ISP, d.ListName, d.Added, d.Suppressed, d.Deduped))
		}
		sb.WriteString("\n")
	}

	if len(report.Errors) > 0 {
		sb.WriteString("Errors\n")
		sb.WriteString("------\n")
		for _, e := range report.Errors {
			sb.WriteString(fmt.Sprintf("  - %s\n", e))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("---\nAutomated report from IGNITE Data Pipeline.\n")

	return a.sendEmailAuth(subject, sb.String())
}

// PipelineRunReport is the alerter-facing struct for pipeline notifications.
type PipelineRunReport struct {
	RunID            string
	StartedAt        time.Time
	CompletedAt      time.Time
	FilesProcessed   int
	EmailsTotal      int
	EmailsVerified   int
	EmailsSuppressed int
	EmailsDeduped    int
	DomainBreakdown  []PipelineDomainStat
	Errors           []string
}

type PipelineDomainStat struct {
	SendingDomain string
	ISP           string
	ListName      string
	Added         int
	Suppressed    int
	Deduped       int
}

func (a *Alerter) sendEmail(subject, body string) {
	if a.smtpHost == "" || len(a.to) == 0 {
		log.Printf("[alerter] would send: %s", subject)
		return
	}

	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		a.from, strings.Join(a.to, ","), subject, body)

	addr := fmt.Sprintf("%s:%d", a.smtpHost, a.smtpPort)
	err := smtp.SendMail(addr, nil, a.from, a.to, []byte(msg))
	if err != nil {
		log.Printf("[alerter] send error: %v (subject: %s)", err, subject)
	}
}

// sendEmailAuth sends an email via SES SMTP with authentication.
// Falls back to unauthenticated relay if SES credentials are not set.
func (a *Alerter) sendEmailAuth(subject, body string) error {
	if a.smtpHost == "" || len(a.to) == 0 {
		log.Printf("[alerter] would send (auth): %s", subject)
		return nil
	}

	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		a.from, strings.Join(a.to, ","), subject, body)

	addr := fmt.Sprintf("%s:%d", a.smtpHost, a.smtpPort)

	var auth smtp.Auth
	if a.smtpUser != "" && a.smtpPass != "" {
		auth = smtp.PlainAuth("", a.smtpUser, a.smtpPass, a.smtpHost)
	}

	err := smtp.SendMail(addr, auth, a.from, a.to, []byte(msg))
	if err != nil {
		log.Printf("[alerter] send (auth) error: %v (subject: %s)", err, subject)
		return err
	}
	return nil
}

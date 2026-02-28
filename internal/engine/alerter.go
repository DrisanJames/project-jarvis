package engine

import (
	"fmt"
	"log"
	"net/smtp"
	"strings"
	"time"
)

// Alerter sends email notifications for governance decisions.
type Alerter struct {
	smtpHost string
	smtpPort int
	from     string
	to       []string
}

// AlerterConfig holds alerter configuration.
type AlerterConfig struct {
	SMTPHost string
	SMTPPort int
	From     string
	To       []string
}

// NewAlerter creates a new email alerter.
func NewAlerter(cfg AlerterConfig) *Alerter {
	return &Alerter{
		smtpHost: cfg.SMTPHost,
		smtpPort: cfg.SMTPPort,
		from:     cfg.From,
		to:       cfg.To,
	}
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

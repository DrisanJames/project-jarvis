package api

// POST /api/mailing/sending-domains/onboard — self-service SES sending-domain
// onboarding (SENDING-DOMAIN-ONBOARDING-AUDIT.md Tier-3, G7/G8; contract:
// tasks/eng-team/coalition/SENDING-DOMAIN-API-CONTRACT.md §6).
//
// One org-scoped transaction creates everything the manual Phase-5 runbook
// (docs/RUNBOOK-FIRST-PARTY-SES-DOMAIN.md) did by hand:
//   1. the SES sending profile (vendor_type='pmta', via_ses=true),
//   2. the mailing_sending_domains row (campaign-deploy domain validation),
//   3. the mailing_owned_domains registry row (brand-root resolution +
//      click-tracker rewriter, no Go redeploy — internal/pkg/brand),
//   4. the mailing_segment_registry family row (ownership + keep policy).
//
// Idempotent: re-POSTing for a domain whose via_ses profile already exists
// returns the existing profile (200, created=false) and only fills whichever
// companion rows are missing. Default status is 'draft' so the planner's
// by-domain resolver (status='active' filter) cannot pick the profile up
// until the operator verifies + activates it.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
)

// Estate defaults for SES tenant profiles (clone of the live
// seed_pmta_db_ses_tenant_profile shape, cmd/server/main.go): the PMTA
// Server A HTTP bridge relays via_ses traffic to SES.
const (
	defaultSESBridgeSMTPHost = "15.204.101.125"
	defaultSESBridgeEndpoint = "http://15.204.101.125:19099"
	defaultSESHourlyLimit    = 1000
	defaultSESDailyLimit     = 25000
)

// HandleOnboardSendingDomain onboards a new SES-routed sending domain.
func (s *SendingProfileService) HandleOnboardSendingDomain(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Domain              string `json:"domain"` // apex, required
		SESConfigurationSet string `json:"ses_configuration_set"`
		SESTenantName       string `json:"ses_tenant_name"`

		Slug                 string `json:"slug"`
		SendingDomain        string `json:"sending_domain"`
		TrackingDomain       string `json:"tracking_domain"`
		FromName             string `json:"from_name"`
		FromEmail            string `json:"from_email"`
		ReplyEmail           string `json:"reply_email"`
		SMTPHost             string `json:"smtp_host"`
		APIEndpoint          string `json:"api_endpoint"`
		IPPool               string `json:"ip_pool"`
		HourlyLimit          *int   `json:"hourly_limit"`
		DailyLimit           *int   `json:"daily_limit"`
		Status               string `json:"status"`
		SegmentFamilyPattern string `json:"segment_family_pattern"`
	}

	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}

	apex := strings.ToLower(strings.TrimSpace(input.Domain))
	configSet := strings.TrimSpace(input.SESConfigurationSet)
	tenant := strings.TrimSpace(input.SESTenantName)
	if apex == "" || configSet == "" || tenant == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "domain, ses_configuration_set, and ses_tenant_name are required"})
		return
	}
	if strings.ContainsAny(apex, " /@") || !strings.Contains(apex, ".") {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "domain must be a bare apex domain (e.g. wcl-heloc.com)"})
		return
	}

	// Defaults per estate convention (runbook: identity em.<apex>,
	// tracking t.em.<apex>, tenant name = slug).
	slug := strings.ToLower(strings.TrimSpace(input.Slug))
	if slug == "" {
		slug = strings.ToLower(tenant)
	}
	sendingDomain := strings.ToLower(strings.TrimSpace(input.SendingDomain))
	if sendingDomain == "" {
		sendingDomain = "em." + apex
	}
	trackingDomain := strings.ToLower(strings.TrimSpace(input.TrackingDomain))
	if trackingDomain == "" {
		trackingDomain = "t.em." + apex
	}
	fromName := strings.TrimSpace(input.FromName)
	if fromName == "" {
		fromName = titleFromSlug(slug)
	}
	fromEmail := strings.TrimSpace(input.FromEmail)
	if fromEmail == "" {
		fromEmail = "hello@" + sendingDomain
	}
	replyEmail := strings.TrimSpace(input.ReplyEmail)
	if replyEmail == "" {
		replyEmail = "reply@" + sendingDomain
	}
	smtpHost := strings.TrimSpace(input.SMTPHost)
	if smtpHost == "" {
		smtpHost = defaultSESBridgeSMTPHost
	}
	apiEndpoint := strings.TrimSpace(input.APIEndpoint)
	if apiEndpoint == "" {
		apiEndpoint = defaultSESBridgeEndpoint
	}
	ipPool := strings.TrimSpace(input.IPPool)
	if ipPool == "" {
		ipPool = slug + "-ses-pool"
	}
	hourlyLimit := defaultSESHourlyLimit
	if input.HourlyLimit != nil {
		hourlyLimit = *input.HourlyLimit
	}
	dailyLimit := defaultSESDailyLimit
	if input.DailyLimit != nil {
		dailyLimit = *input.DailyLimit
	}
	status := strings.ToLower(strings.TrimSpace(input.Status))
	if status == "" {
		status = "draft"
	}
	if status != "draft" && status != "active" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be 'draft' or 'active'"})
		return
	}
	familyPattern := strings.TrimSpace(input.SegmentFamilyPattern)
	if familyPattern == "" {
		familyPattern = strings.ToUpper(slug) + "-%"
	}

	orgID, err := GetOrgIDStringFromRequest(r)
	if err != nil || orgID == "" {
		orgID = defaultOrgID
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer tx.Rollback()

	// Idempotency probe: an existing via_ses pmta profile for this
	// (org, sending_domain) means the domain is already onboarded.
	var profileID string
	created := false
	err = tx.QueryRowContext(r.Context(), `
		SELECT id::text FROM mailing_sending_profiles
		WHERE organization_id = $1 AND sending_domain = $2
		  AND vendor_type = 'pmta' AND COALESCE(via_ses, false) = true
		ORDER BY created_at ASC LIMIT 1
	`, orgID, sendingDomain).Scan(&profileID)
	if err == sql.ErrNoRows {
		err = tx.QueryRowContext(r.Context(), `
			INSERT INTO mailing_sending_profiles (
				organization_id, name, vendor_type, from_name, from_email, reply_email,
				sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
				hourly_limit, daily_limit, ip_pool, status, is_default,
				via_ses, ses_configuration_set, ses_tenant_name
			) VALUES ($1, $2, 'pmta', $3, $4, $5, $6, $7, 587, $8, $9, $10, $11, $12, $13, false, true, $14, $15)
			RETURNING id::text
		`, orgID, titleFromSlug(slug)+" (SES Tenant)", fromName, fromEmail, replyEmail,
			sendingDomain, smtpHost, apiEndpoint, trackingDomain,
			hourlyLimit, dailyLimit, ipPool, status, configSet, tenant,
		).Scan(&profileID)
		if err != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		created = true
	} else if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Companion rows — each idempotent, each reporting whether it was added.
	// mailing_sending_domains has no unique constraint on (org, domain), so
	// the guard is WHERE NOT EXISTS (same as the startup seeds).
	res, err := tx.ExecContext(r.Context(), `
		INSERT INTO mailing_sending_domains
			(id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
		SELECT gen_random_uuid(), $1, $2, false, false, false, 'pending', NOW(), NOW()
		WHERE NOT EXISTS (
			SELECT 1 FROM mailing_sending_domains WHERE organization_id = $1 AND domain = $2
		)
	`, orgID, sendingDomain)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	res, err = tx.ExecContext(r.Context(), `
		INSERT INTO mailing_owned_domains (domain, source) VALUES ($1, 'onboard')
		ON CONFLICT (domain) DO NOTHING
	`, apex)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	ownedAdded, _ := res.RowsAffected()

	res, err = tx.ExecContext(r.Context(), `
		INSERT INTO mailing_segment_registry (
			organization_id, family_key, family_pattern, definition_source,
			owner, cadence, sla_hours, keep_policy, heartbeat_worker, notes
		) VALUES ($1, $2, $3, 'onboard', $4, '', 0, 'protect', '', $5)
		ON CONFLICT (organization_id, family_pattern) DO NOTHING
	`, orgID, slug, familyPattern,
		"sending-domain onboard (operator)",
		fmt.Sprintf("first-party sending domain %s (profile %s)", apex, profileID))
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	familyAdded, _ := res.RowsAffected()

	if err := tx.Commit(); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Honor the new domain in-process immediately (click-tracker rewriter +
	// brand-root resolution); the periodic brand.RefreshFromDB covers every
	// other instance within one refresh interval.
	brand.Register(apex)

	profile, err := s.getProfileByID(r.Context(), profileID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "onboarded but profile fetch failed: " + err.Error()})
		return
	}

	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	respondJSON(w, code, map[string]interface{}{
		"created":            created,
		"profile":            profile,
		"owned_domain":       apex,
		"owned_domain_added": ownedAdded > 0,
		"sending_domain_row": sendingDomain,
		"segment_family": map[string]interface{}{
			"family_key":     slug,
			"family_pattern": familyPattern,
			"created":        familyAdded > 0,
		},
	})
}

// titleFromSlug turns "wcl-heloc" into "Wcl Heloc" — a sane display default
// the operator overrides with the real persona/from_name.
func titleFromSlug(slug string) string {
	parts := strings.FieldsFunc(slug, func(r rune) bool { return r == '-' || r == '_' })
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

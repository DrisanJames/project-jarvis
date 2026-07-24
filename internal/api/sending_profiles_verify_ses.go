package api

// Real SES-identity verification for sending profiles (audit G5).
//
// POST /api/mailing/sending-profiles/{id}/verify, for profiles with
// via_ses=true OR vendor_type='ses', calls SES v2 GetEmailIdentity on the
// profile's sending_domain (region SES_REGION, default us-west-1 — the
// estate's SES region per docs/RUNBOOK-FIRST-PARTY-SES-DOMAIN.md) plus a DNS
// TXT lookup of _dmarc.<sending_domain>, and writes the four *_verified
// columns truthfully:
//
//	dkim_verified   ← DkimAttributes.Status == SUCCESS
//	spf_verified    ← MailFromAttributes.MailFromDomainStatus == SUCCESS
//	                  (custom MAIL FROM carries the SPF alignment on SES)
//	domain_verified ← VerifiedForSendingStatus
//	dmarc_verified  ← _dmarc TXT contains v=DMARC1
//
// The non-SES vendor paths in HandleVerifyCredentials are unchanged.

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
)

// sesIdentityStatus is the subset of GetEmailIdentity the verify path reads.
type sesIdentityStatus struct {
	DKIMStatus         string
	MailFromStatus     string
	VerifiedForSending bool
}

// fetchSESIdentityStatus is the production sesIdentity implementation.
// Credentials come from the default AWS chain (ECS task role in prod).
func fetchSESIdentityStatus(ctx context.Context, sendingDomain string) (sesIdentityStatus, error) {
	region := os.Getenv("SES_REGION")
	if region == "" {
		region = "us-west-1"
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return sesIdentityStatus{}, err
	}
	client := sesv2.NewFromConfig(awsCfg)
	out, err := client.GetEmailIdentity(ctx, &sesv2.GetEmailIdentityInput{
		EmailIdentity: aws.String(sendingDomain),
	})
	if err != nil {
		return sesIdentityStatus{}, err
	}
	st := sesIdentityStatus{VerifiedForSending: out.VerifiedForSendingStatus}
	if out.DkimAttributes != nil {
		st.DKIMStatus = string(out.DkimAttributes.Status)
	}
	if out.MailFromAttributes != nil {
		st.MailFromStatus = string(out.MailFromAttributes.MailFromDomainStatus)
	}
	return st, nil
}

// lookupDMARCRecord is the production dmarcLookup implementation: a TXT
// lookup of _dmarc.<domain> checked for a v=DMARC1 record.
func lookupDMARCRecord(ctx context.Context, sendingDomain string) (bool, error) {
	var resolver net.Resolver
	txts, err := resolver.LookupTXT(ctx, "_dmarc."+sendingDomain)
	if err != nil {
		return false, err
	}
	for _, t := range txts {
		if strings.Contains(strings.ToLower(t), "v=dmarc1") {
			return true, nil
		}
	}
	return false, nil
}

// verifySESRoute performs the real SES verification and writes the result.
// AWS/API failure responds 200 with verified=false + error populated
// (matching the legacy "verification failed" semantics), never a 5xx.
func (s *SendingProfileService) verifySESRoute(w http.ResponseWriter, r *http.Request, profileID, orgID string, sendingDomain *string) {
	if sendingDomain == nil || strings.TrimSpace(*sendingDomain) == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "profile has no sending_domain to verify against SES"})
		return
	}
	domain := strings.TrimSpace(strings.ToLower(*sendingDomain))

	verificationError := ""
	var dkimOK, spfOK, domainOK, dmarcOK bool
	var identity sesIdentityStatus

	st, err := s.sesIdentity(r.Context(), domain)
	if err != nil {
		verificationError = "SES GetEmailIdentity failed: " + err.Error()
	} else {
		identity = st
		dkimOK = strings.EqualFold(st.DKIMStatus, "SUCCESS")
		spfOK = strings.EqualFold(st.MailFromStatus, "SUCCESS")
		domainOK = st.VerifiedForSending
	}

	// DMARC is DNS-side, not an SES attribute. A lookup failure just means
	// dmarc_verified=false — it does not fail the verification itself.
	if found, derr := s.dmarcLookup(r.Context(), domain); derr == nil {
		dmarcOK = found
	}

	verified := dkimOK && spfOK && domainOK

	now := time.Now()
	status := "active"
	if !verified {
		status = "pending"
	}

	_, uerr := s.db.ExecContext(r.Context(), `
		UPDATE mailing_sending_profiles
		SET credentials_verified = $1,
			spf_verified = $2,
			dkim_verified = $3,
			dmarc_verified = $4,
			domain_verified = $5,
			last_verification_at = $6,
			verification_error = $7,
			status = $8,
			updated_at = $9
		WHERE id = $10 AND organization_id = $11
	`, verified, spfOK, dkimOK, dmarcOK, domainOK, now, nilIfEmpty(verificationError), status, now, profileID, orgID)
	if uerr != nil {
		log.Printf("Error updating SES verification status: %v", uerr)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"verified":        verified,
		"error":           verificationError,
		"verified_at":     now,
		"status":          status,
		"spf_verified":    spfOK,
		"dkim_verified":   dkimOK,
		"dmarc_verified":  dmarcOK,
		"domain_verified": domainOK,
		"identity": map[string]interface{}{
			"sending_domain":       domain,
			"dkim_status":          identity.DKIMStatus,
			"mail_from_status":     identity.MailFromStatus,
			"verified_for_sending": identity.VerifiedForSending,
			"dmarc_found":          dmarcOK,
		},
	})
}

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// Inbox Score seeds (operator 2026-10-22: "send our offers to the mailcow test
// inbox and see how they score, then optimize for the given ISP").
//
// A seed is one mailbox on inbox.projectjarvis.io whose domain the isp package
// classifies as a provider (seed-gmail.projectjarvis.io → gmail, …). The
// audience finalizer appends the seed for every ISP plan a campaign actually
// mails, AFTER planning and suppression, so the seed takes that provider's real
// lane (transport, IP pool, headers) without touching quotas, dedupe or caps.
// The mailcow defender (rspamd) then scores the delivered bytes and the seed
// reader posts folder + score back as a seed placement.
//
// Kill switch: INBOX_SCORE_SEEDS_DISABLED=1 (no deploy needed). A missing table
// or a query error never fails a campaign — the seeds are an observer.

const inboxScoreSeedList = "INBOX-SCORE-SEEDS"

type inboxScoreSeed struct {
	Email        string `json:"email"`
	ISP          string `json:"isp"`
	SubscriberID string `json:"subscriber_id"`
	Active       bool   `json:"active"`
}

// loadInboxScoreSeeds returns the active seeds keyed by ISP plan name.
func loadInboxScoreSeeds(ctx context.Context, db *sql.DB, orgID string) (map[string]inboxScoreSeed, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT email, isp, subscriber_id::text
		FROM mailing_inbox_score_seeds
		WHERE organization_id = $1 AND active = TRUE
		ORDER BY email`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]inboxScoreSeed{}
	for rows.Next() {
		var s inboxScoreSeed
		if err := rows.Scan(&s.Email, &s.ISP, &s.SubscriberID); err != nil {
			return nil, err
		}
		s.Active = true
		if _, dup := out[s.ISP]; !dup {
			out[s.ISP] = s
		}
	}
	return out, rows.Err()
}

// appendInboxScoreSeeds adds one seed recipient to every ISP plan that already
// selected real recipients. Returns the ISPs seeded. Never returns an error the
// caller should act on: problems are logged and the audience is left as is.
func appendInboxScoreSeeds(ctx context.Context, db *sql.DB, orgID string, normalized pmtaNormalizedCampaign, audience *pmtaAudiencePlan) []string {
	if os.Getenv("INBOX_SCORE_SEEDS_DISABLED") == "1" || db == nil || audience == nil {
		return nil
	}
	seeds, err := loadInboxScoreSeeds(ctx, db, orgID)
	if err != nil {
		log.Printf("[InboxScoreSeeds] load failed (seeds skipped): %v", err)
		return nil
	}
	if len(seeds) == 0 {
		return nil
	}
	if audience.RecipientsByISP == nil {
		audience.RecipientsByISP = map[string][]pmtaSelectedRecipient{}
	}
	if audience.CountsByISP == nil {
		audience.CountsByISP = map[string]int{}
	}
	var seeded []string
	for _, plan := range normalized.Plans {
		seed, ok := seeds[plan.ISP]
		if !ok {
			continue
		}
		if audience.CountsByISP[plan.ISP] <= 0 {
			continue // a lane the campaign does not mail gets no seed (no 1-recipient waves)
		}
		if isp.Group(seed.Email) != plan.ISP {
			log.Printf("[InboxScoreSeeds] seed %s classifies as %q, not %q — skipped", seed.Email, isp.Group(seed.Email), plan.ISP)
			continue
		}
		list := audience.RecipientsByISP[plan.ISP]
		present := false
		for _, r := range list {
			if strings.EqualFold(r.Email, seed.Email) {
				present = true
				break
			}
		}
		if present {
			continue
		}
		audience.RecipientsByISP[plan.ISP] = append(list, pmtaSelectedRecipient{
			SubscriberID:  seed.SubscriberID,
			Email:         seed.Email,
			ISP:           plan.ISP,
			SourceType:    "inbox_score_seed",
			SourceID:      "",
			SelectionRank: len(list) + 1,
		})
		audience.CountsByISP[plan.ISP]++
		audience.SelectedTotal++
		seeded = append(seeded, plan.ISP)
	}
	return seeded
}

// ── admin routes: GET/POST /api/admin/inbox-score/seeds ─────────────────────

type inboxScorePostSeedsBody struct {
	Seeds []struct {
		Email  string `json:"email"`
		ISP    string `json:"isp"`
		Active *bool  `json:"active,omitempty"`
	} `json:"seeds"`
}

func (s *InboxScoreService) HandleGetSeeds(w http.ResponseWriter, r *http.Request) {
	org, ok := inboxScoreOrg(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
		SELECT email, isp, subscriber_id::text, active
		FROM mailing_inbox_score_seeds WHERE organization_id = $1 ORDER BY isp, email`, org)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "read failed: "+err.Error())
		return
	}
	defer rows.Close()
	out := []inboxScoreSeed{}
	for rows.Next() {
		var sd inboxScoreSeed
		if err := rows.Scan(&sd.Email, &sd.ISP, &sd.SubscriberID, &sd.Active); err != nil {
			respondError(w, http.StatusInternalServerError, "scan failed")
			return
		}
		out = append(out, sd)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"seeds": out, "disabled": os.Getenv("INBOX_SCORE_SEEDS_DISABLED") == "1"})
}

// HandlePostSeeds registers seeds: the mailbox must classify as the ISP it
// stands in for; a mailing_subscribers row (status confirmed, source
// inbox-score-seed, list INBOX-SCORE-SEEDS) is created when missing so the
// send path has a subscriber id to stamp on the queue row.
func (s *InboxScoreService) HandlePostSeeds(w http.ResponseWriter, r *http.Request) {
	org, ok := inboxScoreOrg(w, r)
	if !ok {
		return
	}
	var body inboxScorePostSeedsBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(body.Seeds) == 0 || len(body.Seeds) > 50 {
		respondError(w, http.StatusBadRequest, "seeds must hold 1..50 rows")
		return
	}
	for i, sd := range body.Seeds {
		email := strings.ToLower(strings.TrimSpace(sd.Email))
		if email == "" || !strings.Contains(email, "@") {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("seeds[%d].email invalid", i))
			return
		}
		if got := isp.Group(email); got != sd.ISP {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("seeds[%d]: %s classifies as %q, not %q (add the domain to internal/pkg/isp first)", i, email, got, sd.ISP))
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "db unavailable")
		return
	}
	defer tx.Rollback()

	var listID string
	if err := tx.QueryRowContext(ctx, `SELECT id::text FROM mailing_lists WHERE organization_id = $1 AND name = $2 ORDER BY created_at LIMIT 1`, org, inboxScoreSeedList).Scan(&listID); err != nil {
		if err != sql.ErrNoRows {
			respondError(w, http.StatusInternalServerError, "list lookup failed: "+err.Error())
			return
		}
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO mailing_lists (id, organization_id, name, description, created_at, updated_at)
			VALUES (gen_random_uuid(), $1::uuid, $2, $3, NOW(), NOW()) RETURNING id::text`,
			org, inboxScoreSeedList, "Inbox Score seed mailboxes on inbox.projectjarvis.io (one per provider). Deliverability seeds — never organic audience.").Scan(&listID); err != nil {
			respondError(w, http.StatusInternalServerError, "list create failed: "+err.Error())
			return
		}
	}
	out := make([]inboxScoreSeed, 0, len(body.Seeds))
	for _, sd := range body.Seeds {
		email := strings.ToLower(strings.TrimSpace(sd.Email))
		var subID string
		err := tx.QueryRowContext(ctx, `SELECT id::text FROM mailing_subscribers WHERE lower(trim(email)) = $1 ORDER BY created_at LIMIT 1`, email).Scan(&subID)
		if err == sql.ErrNoRows {
			err = tx.QueryRowContext(ctx, `
				INSERT INTO mailing_subscribers (id, organization_id, list_id, email, email_hash, status, source, created_at, updated_at)
				VALUES ($1, $2::uuid, $3::uuid, $4, encode(sha256(lower(trim($4))::bytea), 'hex'), 'confirmed', 'inbox-score-seed', NOW(), NOW())
				RETURNING id::text`, uuid.New(), org, listID, email).Scan(&subID)
		}
		if err != nil {
			respondError(w, http.StatusInternalServerError, "subscriber failed for "+email+": "+err.Error())
			return
		}
		active := true
		if sd.Active != nil {
			active = *sd.Active
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mailing_inbox_score_seeds (organization_id, email, isp, subscriber_id, active, created_at, updated_at)
			VALUES ($1, $2, $3, $4::uuid, $5, NOW(), NOW())
			ON CONFLICT (organization_id, email) DO UPDATE SET isp = EXCLUDED.isp, subscriber_id = EXCLUDED.subscriber_id,
			  active = EXCLUDED.active, updated_at = NOW()`, org, email, sd.ISP, subID, active); err != nil {
			respondError(w, http.StatusInternalServerError, "seed write failed: "+err.Error())
			return
		}
		out = append(out, inboxScoreSeed{Email: email, ISP: sd.ISP, SubscriberID: subID, Active: active})
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "commit failed")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "seeds": out})
}

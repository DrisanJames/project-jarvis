package api

// offers_sync.go — audience unification Phase 3.
//
// mailing_offers becomes the canonical offer registry: the operator-side
// Python registry (mailing-saas agents/scheduling/offers.py) syncs INTO the
// DB through this admin endpoint, then consumers (OFFER_KEY_DB_ID, boards,
// attribution) read the DB back. Offers are PERMANENT — this endpoint never
// deletes a row; retirement is a status change.
//
//	POST /api/admin/offers/sync   body {"offers":[{key,display,everflow_id,money_url,status}]}
//	     upsert keyed by landing_page_slug = key (the same column
//	     campaign_attribution.go reads as the offer_key). Response is a
//	     per-offer created/updated/unchanged ledger.
//	GET  /api/admin/offers/sync   key→id map for drift reporting.
//
// Both are registered on the root router behind the X-Admin-Key gate
// (server_routes_mailing.go), org via getOrgID (X-Organization-ID header
// honored, defaultOrgID fallback — same as the attribution backfill sibling).

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// offerSyncItem is one registry entry from agents/scheduling/offers.py.
type offerSyncItem struct {
	Key        string `json:"key"`
	Display    string `json:"display"`
	EverflowID string `json:"everflow_id"`
	MoneyURL   string `json:"money_url"`
	Status     string `json:"status"` // registry: active | sunset | onboarding
}

// offerSyncResult is the per-offer outcome.
type offerSyncResult struct {
	Key     string `json:"key"`
	OfferID string `json:"offer_id,omitempty"`
	Action  string `json:"action"` // created | updated | unchanged | skipped
	Detail  string `json:"detail,omitempty"`
}

// registryStatusToDB maps the Python registry's lifecycle onto the status
// values the Offer Center already stores/renders (schema default 'draft';
// OverviewTab select = draft|active|paused — OfferManagement.tsx:876-878).
// sunset stays 'sunset' — the live table already carries that value (verified
// 2026-08-04: draft 7 / active 28 / sunset 1); mapping it to 'paused' would
// mint a second spelling for the same lifecycle state. onboarding → draft.
func registryStatusToDB(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "active":
		return "active"
	case "sunset":
		return "sunset"
	case "onboarding":
		return "draft"
	default:
		return "draft"
	}
}

// HandleOffersSyncPost POST /api/admin/offers/sync
func HandleOffersSyncPost(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID := getOrgID(r)

		var body struct {
			Offers []offerSyncItem `json:"offers"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			respondError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		if len(body.Offers) == 0 {
			respondError(w, http.StatusBadRequest, "offers list is empty")
			return
		}

		results := make([]offerSyncResult, 0, len(body.Offers))
		var created, updated, unchanged int
		for _, item := range body.Offers {
			key := strings.ToLower(strings.TrimSpace(item.Key))
			if key == "" {
				results = append(results, offerSyncResult{Key: item.Key, Action: "skipped", Detail: "empty key"})
				continue
			}
			name := strings.TrimSpace(item.Display)
			if name == "" {
				name = key
			}
			ef := strings.TrimSpace(item.EverflowID)
			moneyURL := strings.TrimSpace(item.MoneyURL)
			status := registryStatusToDB(item.Status)

			// (1) Natural-key match: landing_page_slug = key (the offer_key
			// column campaign_attribution reads). Oldest row wins if dupes.
			var id, curName, curEF, curURL, curStatus string
			err := db.QueryRowContext(r.Context(), `
				SELECT id::text, name, COALESCE(everflow_offer_id,''),
				       COALESCE(tracking_link_template,''), COALESCE(status,'draft')
				FROM mailing_offers
				WHERE organization_id = $1 AND lower(landing_page_slug) = $2
				ORDER BY created_at ASC LIMIT 1`, orgID, key).
				Scan(&id, &curName, &curEF, &curURL, &curStatus)
			matchedBy := "slug"
			if err == sql.ErrNoRows {
				// (2) Adoption fallback: a pre-existing row with NO slug whose
				// name matches the display (case-insensitive) — the 36 legacy
				// rows predate slug-keying. Never re-slugs an already-keyed
				// row, so slugged rows are only ever matched by their slug.
				err = db.QueryRowContext(r.Context(), `
					SELECT id::text, name, COALESCE(everflow_offer_id,''),
					       COALESCE(tracking_link_template,''), COALESCE(status,'draft')
					FROM mailing_offers
					WHERE organization_id = $1
					  AND COALESCE(landing_page_slug,'') = ''
					  AND lower(name) = lower($2)
					ORDER BY created_at ASC LIMIT 1`, orgID, name).
					Scan(&id, &curName, &curEF, &curURL, &curStatus)
				matchedBy = "name_adopted"
			}

			switch {
			case err == sql.ErrNoRows:
				var newID string
				if err := db.QueryRowContext(r.Context(), `
					INSERT INTO mailing_offers
						(organization_id, name, everflow_offer_id, tracking_link_template,
						 landing_page_slug, status, created_at, updated_at)
					VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
					RETURNING id::text`,
					orgID, name, ef, moneyURL, key, status).Scan(&newID); err != nil {
					results = append(results, offerSyncResult{Key: key, Action: "skipped", Detail: "insert failed: " + err.Error()})
					continue
				}
				created++
				results = append(results, offerSyncResult{Key: key, OfferID: newID, Action: "created"})

			case err != nil:
				results = append(results, offerSyncResult{Key: key, Action: "skipped", Detail: "lookup failed: " + err.Error()})

			case matchedBy == "slug" && curName == name && curEF == ef && curURL == moneyURL && curStatus == status:
				unchanged++
				results = append(results, offerSyncResult{Key: key, OfferID: id, Action: "unchanged"})

			default:
				if _, err := db.ExecContext(r.Context(), `
					UPDATE mailing_offers
					SET name = $1, everflow_offer_id = $2, tracking_link_template = $3,
					    landing_page_slug = $4, status = $5, updated_at = NOW()
					WHERE id = $6 AND organization_id = $7`,
					name, ef, moneyURL, key, status, id, orgID); err != nil {
					results = append(results, offerSyncResult{Key: key, OfferID: id, Action: "skipped", Detail: "update failed: " + err.Error()})
					continue
				}
				updated++
				detail := ""
				if matchedBy == "name_adopted" {
					detail = "adopted legacy row by name; slug stamped"
				}
				results = append(results, offerSyncResult{Key: key, OfferID: id, Action: "updated", Detail: detail})
			}
		}

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"organization_id": orgID,
			"created":         created,
			"updated":         updated,
			"unchanged":       unchanged,
			"results":         results,
			"synced_at":       time.Now().UTC().Format(time.RFC3339),
		})
	}
}

// HandleOffersSyncGet GET /api/admin/offers/sync — key→id map (drift report).
func HandleOffersSyncGet(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID := getOrgID(r)
		rows, err := db.QueryContext(r.Context(), `
			SELECT lower(landing_page_slug), id::text, name,
			       COALESCE(everflow_offer_id,''), COALESCE(status,'draft')
			FROM mailing_offers
			WHERE organization_id = $1 AND COALESCE(landing_page_slug,'') <> ''
			ORDER BY landing_page_slug ASC, created_at ASC`, orgID)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "offers query failed: "+err.Error())
			return
		}
		defer rows.Close()

		type offerRow struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			EverflowID string `json:"everflow_id"`
			Status     string `json:"status"`
		}
		keyToID := map[string]string{}
		offers := map[string]offerRow{}
		for rows.Next() {
			var key string
			var o offerRow
			if err := rows.Scan(&key, &o.ID, &o.Name, &o.EverflowID, &o.Status); err != nil {
				respondError(w, http.StatusInternalServerError, "offers scan failed: "+err.Error())
				return
			}
			if _, dup := keyToID[key]; dup {
				continue // oldest row wins, matching the POST's ORDER BY created_at ASC
			}
			keyToID[key] = o.ID
			offers[key] = o
		}
		if err := rows.Err(); err != nil {
			respondError(w, http.StatusInternalServerError, "offers rows failed: "+err.Error())
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"organization_id": orgID,
			"key_to_id":       keyToID,
			"offers":          offers,
		})
	}
}

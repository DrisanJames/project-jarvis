package api

// Property Ledger — lane content panel (operator scope addition 2026-08-17:
// "it does not provide which offer, content subject line and preheader are
// going from the lane. I should be able to edit within this screen too.").
//
// READ resolves what a ledger brand currently mails, mirroring the
// orchestrator's OWN selection rules (internal/worker/partner_drip_orchestrator.go):
//   - feeds:       partner_drip_vertical_roster (brand, active)
//   - datasets:    partner_datasets per vertical (offer_id binds the feed to
//                  the offer-center path)
//   - creative:    laneServingCreativeSQL mirrors resolveOfferCreative's
//                  ORDER BY VERBATIM (approved over generated, newest
//                  updated_at) — metadata + byte length only, never the HTML
//                  in the list payload (creative preview is a detail fetch).
//   - copy pools:  mailing_offer_subject_lines / mailing_offer_from_names with
//                  the orchestrator's serving predicate (status NOT IN
//                  ('archived','rejected'), non-empty text). NOTE: on the
//                  offer-center path the PREHEADER draws from the SUBJECT pool
//                  (resolveOfferCreative) — there is no separate preheader
//                  pool; the UI surfaces that caveat.
//   - touch copy:  partner_drip_creatives (t1, PK vertical+brand) and
//                  partner_drip_followup_creatives (t2+; vertical NULL = the
//                  shared/global fallback chain).
//
// WRITES (same route group, same session-or-admin-key middleware, every
// mutation through writeAuditLog with the stamped actor):
//   - subject add/archive (operator-in-the-UI = operator approval; archive of
//     the LAST serving subject is refused — resolveOfferCreative fails loud
//     on an empty pool and the lane would break at send time)
//   - touch copy subject/preheader/from_name by (vertical, brand, touch)
//   - offer swap on a dataset: type-the-feed-name confirm, offer must be
//     active with non-empty serving creative + subject + from-name pools
//     (422 naming the empty pool otherwise).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/worker"
)

// laneServingCreativeSQL mirrors resolveOfferCreative's selection VERBATIM
// (status filter + CASE ORDER BY + updated_at DESC + LIMIT 1) — pinned by
// regex in property_lane_test.go. Selects metadata + length(html_content),
// never the HTML itself.
const laneServingCreativeSQL = `
	SELECT id::text, version, COALESCE(status, ''), updated_at, length(html_content)
	FROM mailing_offer_creatives
	WHERE offer_id = $1
	  AND COALESCE(status, '') NOT IN ('archived','rejected')
	  AND COALESCE(html_content, '') <> ''
	ORDER BY (CASE COALESCE(status, '')
	            WHEN 'approved' THEN 0
	            WHEN 'generated' THEN 1
	            ELSE 2
	          END), updated_at DESC
	LIMIT 1`

// laneServingSubjectPredicate / laneServingFromNamePredicate mirror
// fetchOfferSubjects / fetchOfferFromNames (the serving-vs-archived split).
const laneServingSubjectPredicate = `COALESCE(status, '') NOT IN ('archived','rejected') AND COALESCE(subject_line, '') <> ''`
const laneServingFromNamePredicate = `COALESCE(status, '') NOT IN ('archived','rejected') AND COALESCE(from_name, '') <> ''`

type laneCopyRow struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Status  string `json:"status"`
	Serving bool   `json:"serving"`
}

type laneCreative struct {
	ID        string    `json:"id"`
	Version   int       `json:"version"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
	HTMLBytes int       `json:"html_bytes"`
}

// laneOfferSharing is the pool-sharing scope indicator (QA SEV-3 fix): how
// many active feed bindings (datasets + touch rows) and distinct brands mail
// this offer's pools. Feeds > 1 means an edit here mutates LIVE rotation on
// other lanes, not just the one on screen.
type laneOfferSharing struct {
	Feeds  int `json:"feeds"`
	Brands int `json:"brands"`
}

type laneOffer struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Creative  *laneCreative     `json:"creative"`
	Subjects  []laneCopyRow     `json:"subjects"`
	FromNames []laneCopyRow     `json:"from_names"`
	Sharing   *laneOfferSharing `json:"sharing,omitempty"`
}

// laneOfferSharingSQL counts one offer's active bindings: every active
// dataset bound to it + every active touch row (t1 + follow-ups) bound to it,
// and the distinct brands those bindings reach (dataset verticals fan out to
// their active roster brands).
const laneOfferSharingSQL = `
	WITH bindings AS (
		SELECT 'ds:' || d.id::text AS k, r.brand
		FROM partner_datasets d
		LEFT JOIN partner_drip_vertical_roster r
		  ON r.vertical = d.vertical AND r.active
		WHERE d.offer_id = $1::uuid AND d.status = 'active'
		UNION ALL
		SELECT 't1:' || vertical || '/' || brand, brand
		FROM partner_drip_creatives WHERE offer_id = $1::uuid AND active
		UNION ALL
		SELECT 'fu:' || COALESCE(vertical, '*') || '/' || brand || '/' || touch_number::text, brand
		FROM partner_drip_followup_creatives WHERE offer_id = $1::uuid AND active
	)
	SELECT COUNT(DISTINCT k), COUNT(DISTINCT brand) FROM bindings`

type laneDataset struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Status          string     `json:"status"`
	ExpressDispatch bool       `json:"express_dispatch"`
	Offer           *laneOffer `json:"offer"`
}

type laneTouchCopy struct {
	Vertical         string    `json:"vertical"`          // "" = shared/global fallback (followups only)
	Touch            int       `json:"touch"`             // 1 = partner_drip_creatives, 2+ = followups
	CreativeFilename string    `json:"creative_filename"` // "" = offer-center path
	SubjectLine      string    `json:"subject_line"`
	Preheader        string    `json:"preheader"`
	FromName         string    `json:"from_name"`
	OfferID          string    `json:"offer_id,omitempty"`
	Active           bool      `json:"active"`
	UpdatedAt        time.Time `json:"updated_at"`
	UpdatedBy        string    `json:"updated_by,omitempty"`
}

// laneTouchOffer is an offer bound at the TOUCH level (partner_drip_creatives
// / partner_drip_followup_creatives offer_id — the orchestrator's per-touch
// override, partner_drip_orchestrator.go:1654-1660). Rendered whenever a
// feed's touch rows carry offers its datasets don't (QA SEV-2 fix: NULL-offer
// datasets like db/internal_auto still mail a real offer through this path).
type laneTouchOffer struct {
	Binding    string     `json:"binding"` // always "touch-level"
	TouchScope []string   `json:"touch_scope"`
	Offer      *laneOffer `json:"offer"`
}

type laneFeed struct {
	Vertical    string           `json:"vertical"`
	Weight      int              `json:"weight"`
	Datasets    []laneDataset    `json:"datasets"`
	Touch1      *laneTouchCopy   `json:"touch1"`
	Followups   []laneTouchCopy  `json:"followups"`
	TouchOffers []laneTouchOffer `json:"touch_offers"`
}

// HandleLaneContent GET …/property-ledger/lane-content?brand=<code>
func (s *PMTACampaignService) HandleLaneContent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	brand := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("brand")))
	if !propertyLedgerValidBrand(brand) {
		respondError(w, http.StatusBadRequest, "unknown brand (must be one of the 16 drip roster codes)")
		return
	}
	sendingDomain, _ := worker.BrandSendingDomain(brand)

	// Every loop below fails the response on query/scan/rows errors — a
	// silent partial-200 misleads the operator (QA robustness fix).

	// 1. Feeds on this brand.
	feeds := []*laneFeed{}
	feedByVertical := map[string]*laneFeed{}
	err := func() error {
		rows, err := s.db.QueryContext(ctx, `
			SELECT vertical, COALESCE(weight, 1)
			FROM partner_drip_vertical_roster
			WHERE brand = $1 AND active = true
			ORDER BY sort_order, vertical`, brand)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			f := &laneFeed{Followups: []laneTouchCopy{}, Datasets: []laneDataset{}, TouchOffers: []laneTouchOffer{}}
			if err := rows.Scan(&f.Vertical, &f.Weight); err != nil {
				return err
			}
			feeds = append(feeds, f)
			feedByVertical[f.Vertical] = f
		}
		return rows.Err()
	}()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "roster query failed")
		return
	}

	// 2–4. Datasets per feed + the offer each one mails (offer resolution
	// cached — datasets and touch rows may share an offer).
	offerCache := map[string]*laneOffer{}
	loadOffer := func(offerID string) (*laneOffer, error) {
		if o, ok := offerCache[offerID]; ok {
			return o, nil
		}
		o, err := s.loadLaneOffer(ctx, offerID)
		if err != nil {
			return nil, err
		}
		offerCache[offerID] = o
		return o, nil
	}
	for _, f := range feeds {
		type dsTmp struct {
			ds      laneDataset
			offerID string
		}
		tmp := []dsTmp{}
		err := func() error {
			rows, err := s.db.QueryContext(ctx, `
				SELECT id::text, name, COALESCE(status, ''), COALESCE(express_dispatch, false),
				       COALESCE(offer_id::text, '')
				FROM partner_datasets
				WHERE vertical = $1 AND status = 'active'
				ORDER BY name`, f.Vertical)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var d dsTmp
				if err := rows.Scan(&d.ds.ID, &d.ds.Name, &d.ds.Status, &d.ds.ExpressDispatch, &d.offerID); err != nil {
					return err
				}
				tmp = append(tmp, d)
			}
			return rows.Err()
		}()
		if err != nil {
			respondError(w, http.StatusInternalServerError, "dataset query failed")
			return
		}
		for _, d := range tmp {
			if d.offerID != "" {
				offer, err := loadOffer(d.offerID)
				if err != nil {
					respondError(w, http.StatusInternalServerError, "offer resolution failed")
					return
				}
				d.ds.Offer = offer
			}
			f.Datasets = append(f.Datasets, d.ds)
		}
	}

	// 5. Roster-specific touch copy. t1 (PK vertical+brand):
	err = func() error {
		rows, err := s.db.QueryContext(ctx, `
			SELECT vertical, creative_filename, subject_line, COALESCE(preheader, ''),
			       from_name, COALESCE(offer_id::text, ''), active, updated_at, COALESCE(updated_by, '')
			FROM partner_drip_creatives
			WHERE brand = $1`, brand)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t := laneTouchCopy{Touch: 1}
			if err := rows.Scan(&t.Vertical, &t.CreativeFilename, &t.SubjectLine, &t.Preheader,
				&t.FromName, &t.OfferID, &t.Active, &t.UpdatedAt, &t.UpdatedBy); err != nil {
				return err
			}
			if f, ok := feedByVertical[t.Vertical]; ok {
				tt := t
				f.Touch1 = &tt
			}
		}
		return rows.Err()
	}()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "touch-copy query failed")
		return
	}

	// Follow-ups (t2+); vertical NULL = the shared/global fallback chain.
	globalFollowups := []laneTouchCopy{}
	err = func() error {
		rows, err := s.db.QueryContext(ctx, `
			SELECT COALESCE(vertical, ''), touch_number, creative_filename, subject_line,
			       COALESCE(preheader, ''), from_name, COALESCE(offer_id::text, ''), active, updated_at
			FROM partner_drip_followup_creatives
			WHERE brand = $1 AND active = true
			ORDER BY touch_number, vertical NULLS LAST`, brand)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t laneTouchCopy
			if err := rows.Scan(&t.Vertical, &t.Touch, &t.CreativeFilename, &t.SubjectLine,
				&t.Preheader, &t.FromName, &t.OfferID, &t.Active, &t.UpdatedAt); err != nil {
				return err
			}
			if f, ok := feedByVertical[t.Vertical]; t.Vertical != "" && ok {
				f.Followups = append(f.Followups, t)
			} else {
				globalFollowups = append(globalFollowups, t)
			}
		}
		return rows.Err()
	}()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "follow-up query failed")
		return
	}

	// 6. Touch-level offer bindings (QA SEV-2): a feed whose datasets carry
	// no offer_id still mails a real offer when its touch rows do (the
	// orchestrator's per-touch override). Effective ladder per feed =
	// t1 + per touch number the vertical-specific row, else the global
	// fallback (mirrors resolveFollowupCreative's vertical-wins ORDER BY).
	// Every DISTINCT touch-bound offer renders with its touch scope;
	// dataset-bound offers are not repeated.
	for _, f := range feeds {
		type scoped struct {
			labels []string
		}
		order := []string{}
		scopes := map[string]*scoped{}
		addTouch := func(offerID, label string) {
			if offerID == "" {
				return
			}
			sc, ok := scopes[offerID]
			if !ok {
				sc = &scoped{}
				scopes[offerID] = sc
				order = append(order, offerID)
			}
			for _, l := range sc.labels {
				if l == label {
					return
				}
			}
			sc.labels = append(sc.labels, label)
		}
		if f.Touch1 != nil {
			addTouch(f.Touch1.OfferID, "T1")
		}
		// Vertical-specific rows win their touch number; global rows fill
		// only touches the feed has no specific row for.
		specific := map[int]bool{}
		for _, t := range f.Followups {
			specific[t.Touch] = true
			addTouch(t.OfferID, "T"+itoa(t.Touch))
		}
		for _, t := range globalFollowups {
			if !specific[t.Touch] {
				addTouch(t.OfferID, "T"+itoa(t.Touch))
			}
		}
		dsBound := map[string]bool{}
		for _, d := range f.Datasets {
			if d.Offer != nil {
				dsBound[d.Offer.ID] = true
			}
		}
		for _, offerID := range order {
			if dsBound[offerID] {
				continue
			}
			offer, err := loadOffer(offerID)
			if err != nil {
				respondError(w, http.StatusInternalServerError, "touch-offer resolution failed")
				return
			}
			f.TouchOffers = append(f.TouchOffers, laneTouchOffer{
				Binding:    "touch-level",
				TouchScope: scopes[offerID].labels,
				Offer:      offer,
			})
		}
	}

	// Active offers for the swap dropdown.
	activeOffers := []map[string]string{}
	err = func() error {
		rows, err := s.db.QueryContext(ctx, `
			SELECT id::text, name FROM mailing_offers
			WHERE status = 'active' ORDER BY name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, name string
			if err := rows.Scan(&id, &name); err != nil {
				return err
			}
			activeOffers = append(activeOffers, map[string]string{"id": id, "name": name})
		}
		return rows.Err()
	}()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "active-offers query failed")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"brand":            brand,
		"sending_domain":   sendingDomain,
		"feeds":            feeds,
		"global_followups": globalFollowups,
		"active_offers":    activeOffers,
		"preheader_note":   "On the offer-center path the preheader draws from the SUBJECT pool (no separate preheader pool) — subjects render as pool rows, not locked subject/preheader pairs.",
	})
}

// loadLaneOffer resolves one offer's name/status, serving creative, copy
// pools, and sharing scope. Query/scan/rows errors are returned and fail the
// response (QA robustness fix — no silent partial-200). Row ABSENCE still
// degrades: a missing offer row renders id-only, a missing serving creative
// renders nil (both are real states the operator must see).
func (s *PMTACampaignService) loadLaneOffer(ctx context.Context, offerID string) (*laneOffer, error) {
	o := &laneOffer{ID: offerID, Subjects: []laneCopyRow{}, FromNames: []laneCopyRow{}}
	if err := s.db.QueryRowContext(ctx, `
		SELECT name, COALESCE(status, '') FROM mailing_offers WHERE id = $1::uuid`, offerID).
		Scan(&o.Name, &o.Status); err != nil && err != sql.ErrNoRows {
		return nil, err
	}

	var c laneCreative
	err := s.db.QueryRowContext(ctx, laneServingCreativeSQL, offerID).
		Scan(&c.ID, &c.Version, &c.Status, &c.UpdatedAt, &c.HTMLBytes)
	switch err {
	case nil:
		o.Creative = &c
	case sql.ErrNoRows:
		// no serving creative — rendered as the red NO SERVING CREATIVE state
	default:
		return nil, err
	}

	err = func() error {
		rows, err := s.db.QueryContext(ctx, `
			SELECT id::text, subject_line, COALESCE(status, ''),
			       (`+laneServingSubjectPredicate+`)
			FROM mailing_offer_subject_lines
			WHERE offer_id = $1 ORDER BY id`, offerID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row laneCopyRow
			if err := rows.Scan(&row.ID, &row.Text, &row.Status, &row.Serving); err != nil {
				return err
			}
			o.Subjects = append(o.Subjects, row)
		}
		return rows.Err()
	}()
	if err != nil {
		return nil, err
	}

	err = func() error {
		rows, err := s.db.QueryContext(ctx, `
			SELECT id::text, from_name, COALESCE(status, ''),
			       (`+laneServingFromNamePredicate+`)
			FROM mailing_offer_from_names
			WHERE offer_id = $1 ORDER BY id`, offerID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row laneCopyRow
			if err := rows.Scan(&row.ID, &row.Text, &row.Status, &row.Serving); err != nil {
				return err
			}
			o.FromNames = append(o.FromNames, row)
		}
		return rows.Err()
	}()
	if err != nil {
		return nil, err
	}

	var sh laneOfferSharing
	if err := s.db.QueryRowContext(ctx, laneOfferSharingSQL, offerID).
		Scan(&sh.Feeds, &sh.Brands); err != nil {
		return nil, err
	}
	o.Sharing = &sh
	return o, nil
}

// HandleLaneCreativePreview GET …/property-ledger/lane-content/creative?id=<uuid>
// — the detail fetch that DOES return the HTML (never in the list payload).
func (s *PMTACampaignService) HandleLaneCreativePreview(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		respondError(w, http.StatusBadRequest, "id required")
		return
	}
	if _, err := uuid.Parse(id); err != nil {
		respondError(w, http.StatusBadRequest, "id must be a UUID")
		return
	}
	var version int
	var status, html string
	var updatedAt time.Time
	err := s.db.QueryRowContext(r.Context(), `
		SELECT version, COALESCE(status, ''), COALESCE(html_content, ''), updated_at
		FROM mailing_offer_creatives WHERE id = $1::uuid`, id).
		Scan(&version, &status, &html, &updatedAt)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "creative not found")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "creative lookup failed")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"id": id, "version": version, "status": status, "updated_at": updatedAt, "html": html,
	})
}

// HandleLaneSubjectEdit POST …/property-ledger/lane-content/subject
// {offer_id, action: "add"|"archive", subject_line?, subject_id?}
func (s *PMTACampaignService) HandleLaneSubjectEdit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		OfferID     string `json:"offer_id"`
		Action      string `json:"action"`
		SubjectLine string `json:"subject_line"`
		SubjectID   string `json:"subject_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.OfferID = strings.TrimSpace(req.OfferID)
	req.SubjectLine = strings.TrimSpace(req.SubjectLine)
	req.SubjectID = strings.TrimSpace(req.SubjectID)
	if req.OfferID == "" {
		respondError(w, http.StatusBadRequest, "offer_id required")
		return
	}
	if _, err := uuid.Parse(req.OfferID); err != nil {
		respondError(w, http.StatusBadRequest, "offer_id must be a UUID")
		return
	}
	actor := actorFromRequest(r)

	switch req.Action {
	case "add":
		if req.SubjectLine == "" {
			respondError(w, http.StatusBadRequest, "subject_line required for add")
			return
		}
		// Pre-check the offer exists — a 404 beats an FK-violation 500.
		var offerExists bool
		if err := s.db.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM mailing_offers WHERE id = $1::uuid)`, req.OfferID).
			Scan(&offerExists); err != nil {
			respondError(w, http.StatusInternalServerError, "offer lookup failed")
			return
		}
		if !offerExists {
			respondError(w, http.StatusNotFound, "offer not found")
			return
		}
		id := uuid.New().String()
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO mailing_offer_subject_lines
				(id, offer_id, subject_line, status, created_at, updated_at)
			VALUES ($1, $2, $3, 'active', NOW(), NOW())`,
			id, req.OfferID, req.SubjectLine); err != nil {
			respondError(w, http.StatusInternalServerError, "subject insert failed")
			return
		}
		writeAuditLog(ctx, s.db, actor, "property_lane_subject_add", "mailing_offer_subject_line", id,
			nil, map[string]interface{}{"offer_id": req.OfferID, "subject_line": req.SubjectLine, "status": "active"})
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"ok": true, "id": id, "offer_id": req.OfferID, "subject_line": req.SubjectLine, "status": "active",
		})
	case "archive":
		if req.SubjectID == "" {
			respondError(w, http.StatusBadRequest, "subject_id required for archive")
			return
		}
		if _, err := uuid.Parse(req.SubjectID); err != nil {
			respondError(w, http.StatusBadRequest, "subject_id must be a UUID")
			return
		}
		// Never archive the last serving subject — resolveOfferCreative fails
		// loud on an empty pool and the lane breaks at send time.
		var remaining int
		if err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM mailing_offer_subject_lines
			WHERE offer_id = $1 AND id <> $2::uuid
			  AND `+laneServingSubjectPredicate, req.OfferID, req.SubjectID).Scan(&remaining); err != nil {
			respondError(w, http.StatusInternalServerError, "subject pool count failed")
			return
		}
		if remaining == 0 {
			respondError(w, http.StatusUnprocessableEntity,
				"cannot archive the last serving subject — the offer's subject pool would be empty and the lane would fail at send time")
			return
		}
		var before string
		err := s.db.QueryRowContext(ctx, `
			UPDATE mailing_offer_subject_lines
			SET status = 'archived', updated_at = NOW()
			WHERE id = $1::uuid AND offer_id = $2
			RETURNING subject_line`, req.SubjectID, req.OfferID).Scan(&before)
		if err == sql.ErrNoRows {
			respondError(w, http.StatusNotFound, "subject line not found for this offer")
			return
		}
		if err != nil {
			respondError(w, http.StatusInternalServerError, "subject archive failed")
			return
		}
		writeAuditLog(ctx, s.db, actor, "property_lane_subject_archive", "mailing_offer_subject_line", req.SubjectID,
			map[string]interface{}{"offer_id": req.OfferID, "subject_line": before},
			map[string]interface{}{"status": "archived"})
		respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "id": req.SubjectID, "status": "archived"})
	default:
		respondError(w, http.StatusBadRequest, `action must be "add" or "archive"`)
	}
}

// HandleLaneTouchCopyEdit POST …/property-ledger/lane-content/touch-copy
// {vertical, brand, touch, subject_line?, preheader?, from_name?}
// touch 1 = partner_drip_creatives (PK vertical+brand); touch 2+ =
// partner_drip_followup_creatives (vertical "" targets the shared/global
// fallback rows, vertical IS NULL).
func (s *PMTACampaignService) HandleLaneTouchCopyEdit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		Vertical    string  `json:"vertical"`
		Brand       string  `json:"brand"`
		Touch       int     `json:"touch"`
		SubjectLine *string `json:"subject_line"`
		Preheader   *string `json:"preheader"`
		FromName    *string `json:"from_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	brand := strings.ToLower(strings.TrimSpace(req.Brand))
	vertical := strings.TrimSpace(req.Vertical)
	if !propertyLedgerValidBrand(brand) {
		respondError(w, http.StatusBadRequest, "unknown brand (must be one of the 16 drip roster codes)")
		return
	}
	if req.Touch < 1 {
		respondError(w, http.StatusBadRequest, "touch must be >= 1")
		return
	}
	if req.SubjectLine == nil && req.Preheader == nil && req.FromName == nil {
		respondError(w, http.StatusBadRequest, "nothing to update: provide subject_line, preheader, or from_name")
		return
	}
	if req.SubjectLine != nil && strings.TrimSpace(*req.SubjectLine) == "" {
		respondError(w, http.StatusBadRequest, "subject_line cannot be blank")
		return
	}
	if req.FromName != nil && strings.TrimSpace(*req.FromName) == "" {
		respondError(w, http.StatusBadRequest, "from_name cannot be blank")
		return
	}
	if req.Touch == 1 && vertical == "" {
		respondError(w, http.StatusBadRequest, "vertical required for touch 1")
		return
	}
	actor := actorFromRequest(r)

	// Reject template markup BEFORE any write: from-names are not rendered.
	if req.FromName != nil {
		if err := validateFromNameNoTemplate(*req.FromName); err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	sets := []string{"updated_at = NOW()"}
	args := []interface{}{}
	arg := func(v interface{}) int {
		args = append(args, v)
		return len(args)
	}
	after := map[string]interface{}{}
	if req.SubjectLine != nil {
		sets = append(sets, "subject_line = $"+itoa(arg(*req.SubjectLine)))
		after["subject_line"] = *req.SubjectLine
	}
	if req.Preheader != nil {
		sets = append(sets, "preheader = $"+itoa(arg(*req.Preheader)))
		after["preheader"] = *req.Preheader
	}
	if req.FromName != nil {
		sets = append(sets, "from_name = $"+itoa(arg(*req.FromName)))
		after["from_name"] = *req.FromName
	}

	if req.Touch == 1 {
		// Audit before-state, then update by PK.
		var before laneTouchCopy
		err := s.db.QueryRowContext(ctx, `
			SELECT subject_line, COALESCE(preheader, ''), from_name
			FROM partner_drip_creatives
			WHERE vertical = $1 AND brand = $2`, vertical, brand).
			Scan(&before.SubjectLine, &before.Preheader, &before.FromName)
		if err == sql.ErrNoRows {
			respondError(w, http.StatusNotFound, "no touch-1 creative row for (vertical, brand)")
			return
		}
		if err != nil {
			respondError(w, http.StatusInternalServerError, "touch-copy lookup failed")
			return
		}
		sets = append(sets, "updated_by = $"+itoa(arg(actor)))
		q := "UPDATE partner_drip_creatives SET " + strings.Join(sets, ", ") +
			" WHERE vertical = $" + itoa(arg(vertical)) + " AND brand = $" + itoa(arg(brand))
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			respondError(w, http.StatusInternalServerError, "touch-copy update failed")
			return
		}
		writeAuditLog(ctx, s.db, actor, "property_lane_touch_copy", "partner_drip_creative",
			vertical+"/"+brand+"/t1",
			map[string]interface{}{"subject_line": before.SubjectLine, "preheader": before.Preheader, "from_name": before.FromName},
			after)
		respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "touch": 1, "updated": 1})
		return
	}

	// Follow-up rows have no unique key; scope by (brand, touch, vertical-or-
	// global) on ACTIVE rows and report the matched count.
	q := "UPDATE partner_drip_followup_creatives SET " + strings.Join(sets, ", ") +
		" WHERE brand = $" + itoa(arg(brand)) +
		" AND touch_number = $" + itoa(arg(req.Touch)) +
		" AND active = true"
	if vertical != "" {
		q += " AND vertical = $" + itoa(arg(vertical))
	} else {
		q += " AND vertical IS NULL"
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "touch-copy update failed")
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		respondError(w, http.StatusNotFound, "no active follow-up creative row for (brand, touch, vertical)")
		return
	}
	writeAuditLog(ctx, s.db, actor, "property_lane_touch_copy", "partner_drip_followup_creative",
		vertical+"/"+brand+"/t"+itoa(req.Touch),
		map[string]interface{}{"rows_matched": n}, after)
	respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "touch": req.Touch, "updated": n})
}

// HandleLaneOfferSwap POST …/property-ledger/lane-content/offer-swap
// {dataset_id, offer_id, confirm_name} — changes what the feed mails.
// confirm_name must equal the dataset's name exactly (the UI's type-the-feed-
// name dialog is re-validated server-side). The offer must be active with a
// serving creative and non-empty subject + from-name pools, or the swap is
// refused with a 422 naming the empty pool (resolveOfferCreative fails loud
// on empty pools — the lane must never break at send time).
func (s *PMTACampaignService) HandleLaneOfferSwap(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		DatasetID   string `json:"dataset_id"`
		OfferID     string `json:"offer_id"`
		ConfirmName string `json:"confirm_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.DatasetID = strings.TrimSpace(req.DatasetID)
	req.OfferID = strings.TrimSpace(req.OfferID)
	if req.DatasetID == "" || req.OfferID == "" {
		respondError(w, http.StatusBadRequest, "dataset_id and offer_id required")
		return
	}
	if _, err := uuid.Parse(req.DatasetID); err != nil {
		respondError(w, http.StatusBadRequest, "dataset_id must be a UUID")
		return
	}
	if _, err := uuid.Parse(req.OfferID); err != nil {
		respondError(w, http.StatusBadRequest, "offer_id must be a UUID")
		return
	}
	actor := actorFromRequest(r)

	var dsName, dsVertical, curOfferID string
	err := s.db.QueryRowContext(ctx, `
		SELECT name, vertical, COALESCE(offer_id::text, '')
		FROM partner_datasets WHERE id = $1::uuid`, req.DatasetID).
		Scan(&dsName, &dsVertical, &curOfferID)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "dataset not found")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "dataset lookup failed")
		return
	}
	if req.ConfirmName != dsName {
		respondError(w, http.StatusBadRequest, "confirm_name does not match the feed name — type the feed name exactly to confirm")
		return
	}

	var offerName, offerStatus string
	err = s.db.QueryRowContext(ctx, `
		SELECT name, COALESCE(status, '') FROM mailing_offers WHERE id = $1::uuid`, req.OfferID).
		Scan(&offerName, &offerStatus)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "offer not found")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "offer lookup failed")
		return
	}
	if offerStatus != "active" {
		respondError(w, http.StatusUnprocessableEntity, "offer is not active (status="+offerStatus+")")
		return
	}

	// Pool preflights — exactly the pools resolveOfferCreative fails loud on.
	var hasCreative, hasSubjects, hasFromNames bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT
		  EXISTS (SELECT 1 FROM mailing_offer_creatives
		          WHERE offer_id = $1
		            AND COALESCE(status, '') NOT IN ('archived','rejected')
		            AND COALESCE(html_content, '') <> ''),
		  EXISTS (SELECT 1 FROM mailing_offer_subject_lines
		          WHERE offer_id = $1 AND `+laneServingSubjectPredicate+`),
		  EXISTS (SELECT 1 FROM mailing_offer_from_names
		          WHERE offer_id = $1 AND `+laneServingFromNamePredicate+`)`,
		req.OfferID).Scan(&hasCreative, &hasSubjects, &hasFromNames); err != nil {
		respondError(w, http.StatusInternalServerError, "offer pool preflight failed")
		return
	}
	if !hasCreative {
		respondError(w, http.StatusUnprocessableEntity, "offer has no serving creative (non-archived, non-empty HTML) — the lane would fail at send time")
		return
	}
	if !hasSubjects {
		respondError(w, http.StatusUnprocessableEntity, "offer has no serving subject lines — the lane would fail at send time")
		return
	}
	if !hasFromNames {
		respondError(w, http.StatusUnprocessableEntity, "offer has no serving from-names — the lane would fail at send time")
		return
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE partner_datasets SET offer_id = $2::uuid, updated_at = NOW()
		WHERE id = $1::uuid`, req.DatasetID, req.OfferID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "offer swap failed")
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		respondError(w, http.StatusInternalServerError, "offer swap affected no rows")
		return
	}
	writeAuditLog(ctx, s.db, actor, "property_lane_offer_swap", "partner_dataset", req.DatasetID,
		map[string]interface{}{"offer_id": curOfferID, "name": dsName, "vertical": dsVertical},
		map[string]interface{}{"offer_id": req.OfferID, "offer_name": offerName})
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true, "dataset_id": req.DatasetID, "vertical": dsVertical,
		"offer_id": req.OfferID, "offer_name": offerName,
	})
}

// itoa keeps the dynamic-SET fragments above readable.
func itoa(n int) string {
	return strconv.Itoa(n)
}

// ── from-name template guard ────────────────────────────────────────────────

// fromNameTemplateRe catches Liquid/Jinja delimiters in a FROM-NAME.
//
// WHY THIS EXISTS — production incident 2026-08-25. The drip send path renders
// subject_line and the body through Liquid but NOT from_name, so a from-name
// containing template markup ships VERBATIM. 38 campaigns went out with
//
//	From: "{{ custom.postal_code | default: \"Local\" }} - Insurance Savings Pro"
//
// across six lanes over ~8h before a seed mailbox caught it. Note the trap: the
// `| default:` guard reads like it makes this safe. It does not — nothing
// evaluates the expression, so the guard never runs. A bare token in an
// unrendered field is worse than no guard, because it looks defended.
//
// subject_line and preheader are DELIBERATELY not covered: those ARE rendered,
// and 36 production rows legitimately carry Liquid. Constraining them would
// break working copy.
//
// This is the write-time half of the defence; the other half is the CHECK
// constraint on both creative tables (cmd/server/main.go), which also catches
// writers that never come through this handler.
var fromNameTemplateRe = regexp.MustCompile(`\{\{|\{%`)

// validateFromNameNoTemplate rejects a from-name the send path cannot render.
func validateFromNameNoTemplate(v string) error {
	if fromNameTemplateRe.MatchString(v) {
		return fmt.Errorf("from_name must not contain template markup ({{ }} or {%% %%}) — "+
			"the send path does not render from-names, so %q would reach the inbox verbatim", v)
	}
	return nil
}

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Inbox Score CATALOG (operator 2026-10-22): "offer cards … scores for each
// creative … agnostic of the sending domain … strictly the content. Each
// individual creative, subject line and pre-header scored."
//
// Unit of scoring: one Creative Studio proof (mailing_offer_proofs) × one
// variant (subject + preheader) × one provider lane. Rows are written by the
// content gate (agents/inboxscore/gate.py → POST /api/admin/inbox-score/catalog)
// from the bytes the defender received; the sending domain a gate run happened
// to use is recorded for the record but is NOT part of the key. Offers are
// associated by NAME (inboxScoreOfferFromProof).

type inboxScoreCatalogRow struct {
	ProofID           string          `json:"proof_id"`
	VariantIndex      int             `json:"variant_index"`
	Subject           string          `json:"subject"`
	Preheader         string          `json:"preheader"`
	ISP               string          `json:"isp"`
	Source            string          `json:"source"` // gate (default)
	SAScore           *float64        `json:"sa_score"`
	SARules           json.RawMessage `json:"sa_rules"`
	DefenderScore     *float64        `json:"defender_score"`
	DefenderSymbols   json.RawMessage `json:"defender_symbols"`
	Folder            string          `json:"folder"`
	Auth              json.RawMessage `json:"auth"`
	Reasons           json.RawMessage `json:"reasons"`
	Verdict           string          `json:"verdict"` // PASS | FAIL
	SendingDomainUsed string          `json:"sending_domain_used"`
	ScoredAt          string          `json:"scored_at"`
	// Links: the SEG link pass {listed_hosts, reasons, rules}; LinkCheck: ok |
	// degraded (a block list refused or did not answer) | skipped.
	Links     json.RawMessage `json:"links"`
	LinkCheck string          `json:"link_check"`
}

// HandlePostCatalog (admin, X-Admin-Key) upserts gate results, one row per
// proof × variant × ISP × source.
func (s *InboxScoreService) HandlePostCatalog(w http.ResponseWriter, r *http.Request) {
	org, ok := inboxScoreOrg(w, r)
	if !ok {
		return
	}
	var body struct {
		Scores []inboxScoreCatalogRow `json:"scores"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(body.Scores) == 0 || len(body.Scores) > 2000 {
		respondError(w, http.StatusBadRequest, "scores must hold 1..2000 rows")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	ids := map[string]bool{}
	for i, sc := range body.Scores {
		if _, err := uuid.Parse(sc.ProofID); err != nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("scores[%d].proof_id is not a UUID", i))
			return
		}
		ids[sc.ProofID] = true
	}
	// A row for a proof this org does not have would never show on a card: refuse it.
	if unknown, err := s.catalogUnknownProofs(ctx, org, ids); err != nil {
		respondError(w, http.StatusInternalServerError, "proof check failed: "+err.Error())
		return
	} else if len(unknown) > 0 {
		respondError(w, http.StatusBadRequest, "unknown proof_id for this organization: "+strings.Join(unknown, ", "))
		return
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "begin failed")
		return
	}
	defer tx.Rollback() //nolint:errcheck
	n := 0
	for i, sc := range body.Scores {
		if !inboxScoreValidISP(sc.ISP) {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("scores[%d].isp %q is not a profile", i, sc.ISP))
			return
		}
		if sc.Source == "" {
			sc.Source = "gate"
		}
		if sc.Verdict != "PASS" && sc.Verdict != "FAIL" {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("scores[%d].verdict must be PASS or FAIL", i))
			return
		}
		at := s.now()
		if sc.ScoredAt != "" {
			if t, perr := time.Parse(time.RFC3339, sc.ScoredAt); perr == nil {
				at = t
			}
		}
		if sc.LinkCheck == "" {
			sc.LinkCheck = "unknown"
		}
		// On a re-score the previous score is kept beside the new one (prev_*), so the
		// card can show the direction a copy change moved the content.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mailing_inbox_score_catalog
			  (organization_id, proof_id, variant_index, subject, preheader, isp, source, sa_score, sa_rules,
			   defender_score, defender_symbols, folder, auth, reasons, verdict, sending_domain_used, scored_at, links, link_check)
			VALUES ($1,$2,$3,$4::text,$5::text,$6,$7,$8,COALESCE($9::jsonb,'[]'::jsonb),$10,COALESCE($11::jsonb,'[]'::jsonb),
			        $12::text,COALESCE($13::jsonb,'{}'::jsonb),COALESCE($14::jsonb,'[]'::jsonb),$15,$16::text,$17,
			        COALESCE($18::jsonb,'{}'::jsonb),$19::text)
			ON CONFLICT (organization_id, proof_id, variant_index, isp, source) DO UPDATE SET
			  prev_defender_score = CASE WHEN mailing_inbox_score_catalog.scored_at <> EXCLUDED.scored_at
			                             THEN mailing_inbox_score_catalog.defender_score ELSE mailing_inbox_score_catalog.prev_defender_score END,
			  prev_sa_score       = CASE WHEN mailing_inbox_score_catalog.scored_at <> EXCLUDED.scored_at
			                             THEN mailing_inbox_score_catalog.sa_score ELSE mailing_inbox_score_catalog.prev_sa_score END,
			  prev_scored_at      = CASE WHEN mailing_inbox_score_catalog.scored_at <> EXCLUDED.scored_at
			                             THEN mailing_inbox_score_catalog.scored_at ELSE mailing_inbox_score_catalog.prev_scored_at END,
			  subject=EXCLUDED.subject, preheader=EXCLUDED.preheader, sa_score=EXCLUDED.sa_score, sa_rules=EXCLUDED.sa_rules,
			  defender_score=EXCLUDED.defender_score, defender_symbols=EXCLUDED.defender_symbols, folder=EXCLUDED.folder,
			  auth=EXCLUDED.auth, reasons=EXCLUDED.reasons, verdict=EXCLUDED.verdict,
			  sending_domain_used=EXCLUDED.sending_domain_used, scored_at=EXCLUDED.scored_at,
			  links=EXCLUDED.links, link_check=EXCLUDED.link_check`,
			org, sc.ProofID, sc.VariantIndex, sc.Subject, sc.Preheader, sc.ISP, sc.Source, sc.SAScore, nullRaw(sc.SARules),
			sc.DefenderScore, nullRaw(sc.DefenderSymbols), sc.Folder, nullRaw(sc.Auth), nullRaw(sc.Reasons), sc.Verdict,
			sc.SendingDomainUsed, at, nullRaw(sc.Links), sc.LinkCheck); err != nil {
			respondError(w, http.StatusInternalServerError, "write failed: "+err.Error())
			return
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "commit failed")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"status": "ok", "written": n})
}

// catalogUnknownProofs returns the ids in ids that are not proofs of this org.
func (s *InboxScoreService) catalogUnknownProofs(ctx context.Context, org string, ids map[string]bool) ([]string, error) {
	want := make([]string, 0, len(ids))
	for id := range ids {
		want = append(want, id)
	}
	sort.Strings(want)
	rows, err := s.db.QueryContext(ctx, `SELECT id::text FROM mailing_offer_proofs WHERE organization_id = $1 AND id = ANY($2::uuid[])`, org, pq.Array(want))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		found[strings.ToLower(id)] = true
	}
	var unknown []string
	for _, id := range want {
		if !found[strings.ToLower(id)] {
			unknown = append(unknown, id)
		}
	}
	return unknown, rows.Err()
}

func nullRaw(m json.RawMessage) interface{} {
	if len(m) == 0 || string(m) == "null" {
		return nil
	}
	return string(m)
}

// ----------------------------------------------------------------------------- read

type inboxScoreCatalogCell struct {
	ISP               string          `json:"isp"`
	Source            string          `json:"source"`
	SAScore           *float64        `json:"sa_score"`
	SARules           json.RawMessage `json:"sa_rules"`
	DefenderScore     *float64        `json:"defender_score"`
	DefenderSymbols   json.RawMessage `json:"defender_symbols"`
	Folder            string          `json:"folder"`
	Auth              json.RawMessage `json:"auth"`
	Reasons           json.RawMessage `json:"reasons"`
	Verdict           string          `json:"verdict"`
	SendingDomainUsed string          `json:"sending_domain_used"`
	ScoredAt          string          `json:"scored_at"`
	// Stale: the proof's subject/preheader at this index changed after the
	// row was scored, so the score describes copy that no longer exists.
	Stale bool `json:"stale"`
	// The link pass and its health (ok | degraded | skipped | unknown).
	Links     json.RawMessage `json:"links"`
	LinkCheck string          `json:"link_check"`
	// The score this row had before its latest re-score (nil on a first score).
	PrevDefenderScore *float64 `json:"prev_defender_score"`
	PrevSAScore       *float64 `json:"prev_sa_score"`
	PrevScoredAt      string   `json:"prev_scored_at,omitempty"`
}

// inboxScoreDriver: one filter symbol or rule and how widely it fires across a
// creative's (or an offer's) scored subject lines. Scope says what to change:
// body = fires on every scored subject line of the creative (it is the HTML);
// copy = fires on some subject lines only (it is the subject/preheader);
// transport = a sending-path symbol (SPF/DKIM/Received), not content.
type inboxScoreDriver struct {
	Name     string  `json:"name"`
	Kind     string  `json:"kind"` // defender | presend
	Score    float64 `json:"score"`
	Variants int     `json:"variants"`
	Of       int     `json:"of"`
	Scope    string  `json:"scope"` // body | copy | transport
	Detail   string  `json:"detail,omitempty"`
}

// inboxScoreLiveCell: one board campaign that mails this offer in the window.
type inboxScoreLiveCell struct {
	Name              string `json:"name"`
	Day               string `json:"day"` // Denver date
	Status            string `json:"status"`
	Recipients        int    `json:"recipients"`
	Subject           string `json:"subject"`
	Creative          string `json:"creative,omitempty"` // the catalog creative whose bytes it carries, "" when drifted
	SubjectCatalogued bool   `json:"subject_catalogued"`
}

// inboxScoreLive: board parity for an offer — do the cells on the board carry
// the catalogued bytes? Window: yesterday through +8 days, Denver.
type inboxScoreLive struct {
	Cells      int                  `json:"cells"`
	Matching   int                  `json:"matching"`
	Drifted    int                  `json:"drifted"`
	Recipients int                  `json:"recipients"`
	Drift      []inboxScoreLiveCell `json:"drift"`
	// Mutated: cells from builders that rewrite every cell's body (the nx lanes
	// de-fingerprint per cell), so their bytes never equal a proof's and hash
	// parity says nothing about them. Counted, never called drifted.
	Mutated           int `json:"mutated"`
	MutatedRecipients int `json:"mutated_recipients"`
	// Live subjects not in the catalog (mailing, but never scored as that line).
	UncataloguedSubjects []string `json:"uncatalogued_subjects"`
}

// reMutatingBuilder: "<date> - NX-<sub.domain> - …" — the nx lanes (board 2026-09-08
// de-fingerprint) emit a distinct body per cell.
var reMutatingBuilder = regexp.MustCompile(`^\d{8} - NX-`)

// inboxScoreOfferByLabel resolves a campaign's offer label to a card: the exact
// label, else the ONE card whose label contains it or is contained by it. Two
// candidates (a bare "Insurify" against Insurify Home / Auto / Renters) = none —
// a cell is never charged to several offers.
func inboxScoreOfferByLabel(label string, byOffer map[string]*inboxScoreCatalogOffer) *inboxScoreCatalogOffer {
	if o := byOffer[label]; o != nil {
		return o
	}
	ll := strings.ToLower(label)
	var found *inboxScoreCatalogOffer
	n := 0
	for l, cand := range byOffer {
		cl := strings.ToLower(l)
		if strings.Contains(ll, cl) || strings.Contains(cl, ll) {
			found = cand
			n++
		}
	}
	if n == 1 {
		return found
	}
	return nil
}

type inboxScoreCatalogVariant struct {
	Index     int                               `json:"index"`
	Subject   string                            `json:"subject"`
	Preheader string                            `json:"preheader"`
	Verdict   string                            `json:"verdict"` // PASS | FAIL | PARTIAL | UNSCORED
	ScoredAt  string                            `json:"scored_at,omitempty"`
	Cells     map[string]*inboxScoreCatalogCell `json:"cells"`
}

type inboxScoreCatalogCreative struct {
	ProofID        string                     `json:"proof_id"`
	Name           string                     `json:"name"`
	OfferKey       string                     `json:"offer_key"`
	ApprovalStatus string                     `json:"approval_status"`
	FromNames      []string                   `json:"from_names"`
	UpdatedAt      string                     `json:"updated_at"`
	Variants       []inboxScoreCatalogVariant `json:"variants"`
	Drivers        []inboxScoreDriver         `json:"drivers"`
	ScoredLines    int                        `json:"scored_lines"` // subject lines with a current score
	LiveCells      int                        `json:"live_cells"`
	LiveRecipients int                        `json:"live_recipients"`
	htmlHash       string
}

type inboxScoreCatalogOffer struct {
	Offer     string                      `json:"offer"`
	Creatives []inboxScoreCatalogCreative `json:"creatives"`
	// Counts for the card face.
	Variants int `json:"variants"`
	Scored   int `json:"scored"`
	Passed   int `json:"passed"`
	Failed   int `json:"failed"`
	// Insight: the worst and best scored subject line, what moves the score,
	// whether any lane's link check ran degraded, and board parity.
	Worst      *inboxScoreExtreme `json:"worst"`
	Best       *inboxScoreExtreme `json:"best"`
	Drivers    []inboxScoreDriver `json:"drivers"`
	LinkChecks map[string]int     `json:"link_checks"` // ok / degraded / skipped / unknown → lanes
	Live       inboxScoreLive     `json:"live"`
}

type inboxScoreExtreme struct {
	Creative string  `json:"creative"`
	Subject  string  `json:"subject"`
	Index    int     `json:"index"`
	Defender float64 `json:"defender"`
	Presend  float64 `json:"presend"`
	Headroom float64 `json:"headroom"` // 2.4 minus the defender score (room before "review")
}

var inboxScoreCatalogHowToRead = []string{
	"One card per offer; offers are grouped by name. Inside: every creative in Creative Studio, and under it every subject line + preheader it carries.",
	"Each subject line is scored on its own, per mailbox provider, from the exact bytes our defender received — the sending domain is not part of the score.",
	"Defender = our near-real-life filter (score of 15: ready under 2.4, review under 4.6, fix under 8, junked at 8). Pre-send = SpamAssassin (ready under 1.5, review under 3, fix under 5, fail at 5). PASS needs inbox, both ready, DMARC pass and no listed link host, on every provider.",
	"Not scored yet = run the gate: python3 -m agents.inboxscore.gate --proof \"<creative name>\" --all-variants. A stale score means the copy changed after it was scored — run it again.",
	"What moves the score: a symbol marked BODY fires on every subject line of the creative (fix the HTML); COPY fires on some lines only (fix those subjects/preheaders); TRANSPORT is the sending path, not content.",
	"Board parity: the cells scheduled yesterday through the next 8 days are matched to the catalogue by their exact HTML bytes. A DRIFTED cell mails this offer with bytes no catalogued creative has — the catalogue score does not describe it. nx-lane cells rewrite every body (per-cell de-fingerprint) and are counted as MUTATED, not drifted: score their proof through the gate and read the nx lane's own delivery.",
	"Link check degraded = a block list (Spamhaus DBL, SURBL, URIBL) refused or did not answer for that lane, so \"no listed link host\" is unproven there. Run the gate with INBOXSCORE_DNS pointing at a recursive resolver.",
}

type proofVariantJSON struct {
	Subject   string `json:"subject"`
	Preheader string `json:"preheader"`
}

// HandleCatalog (session and admin GET) returns the offer → creative → variant tree.
func (s *InboxScoreService) HandleCatalog(w http.ResponseWriter, r *http.Request) {
	org, ok := inboxScoreOrg(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	includeInactive := r.URL.Query().Get("include_inactive") == "1"

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, COALESCE(name,''), COALESCE(offer_key,''), COALESCE(approval_status,''), COALESCE(is_active,TRUE),
		       COALESCE(variants::text,'[]'), COALESCE(from_names::text,'[]'), COALESCE(updated_at, created_at),
		       md5(COALESCE(html_content,''))
		FROM mailing_offer_proofs
		WHERE organization_id = $1 AND ($2 OR COALESCE(is_active,TRUE))
		ORDER BY name`, org, includeInactive)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "read failed: "+err.Error())
		return
	}
	creatives := map[string]*inboxScoreCatalogCreative{}
	order := []string{}
	for rows.Next() {
		var c inboxScoreCatalogCreative
		var active bool
		var variantsRaw, fromRaw string
		var upd time.Time
		if err := rows.Scan(&c.ProofID, &c.Name, &c.OfferKey, &c.ApprovalStatus, &active, &variantsRaw, &fromRaw, &upd, &c.htmlHash); err != nil {
			rows.Close()
			respondError(w, http.StatusInternalServerError, "scan failed")
			return
		}
		if inboxScoreCatalogKind(c.Name, c.OfferKey) != "offer" {
			continue
		}
		var vs []proofVariantJSON
		_ = json.Unmarshal([]byte(variantsRaw), &vs)
		_ = json.Unmarshal([]byte(fromRaw), &c.FromNames)
		if c.FromNames == nil {
			c.FromNames = []string{}
		}
		c.Drivers = []inboxScoreDriver{}
		c.UpdatedAt = upd.UTC().Format(time.RFC3339)
		for i, v := range vs {
			c.Variants = append(c.Variants, inboxScoreCatalogVariant{Index: i, Subject: v.Subject, Preheader: v.Preheader,
				Verdict: "UNSCORED", Cells: map[string]*inboxScoreCatalogCell{}})
		}
		if len(c.Variants) == 0 {
			// A proof with no subject on file: still a creative; scored under index -1 (the proof name as subject).
			c.Variants = []inboxScoreCatalogVariant{{Index: -1, Subject: "", Preheader: "", Verdict: "UNSCORED", Cells: map[string]*inboxScoreCatalogCell{}}}
		}
		creatives[c.ProofID] = &c
		order = append(order, c.ProofID)
	}
	rows.Close()

	if len(order) > 0 {
		crows, err := s.db.QueryContext(ctx, `
			SELECT proof_id, variant_index, subject, preheader, isp, source, sa_score, sa_rules::text, defender_score,
			       defender_symbols::text, folder, auth::text, reasons::text, verdict, sending_domain_used, scored_at,
			       COALESCE(links::text,'{}'), COALESCE(link_check,''), prev_defender_score, prev_sa_score, prev_scored_at
			FROM mailing_inbox_score_catalog
			WHERE organization_id = $1 AND proof_id = ANY($2::uuid[])
			ORDER BY scored_at`, org, pq.Array(order))
		if err != nil {
			respondError(w, http.StatusInternalServerError, "read failed: "+err.Error())
			return
		}
		for crows.Next() {
			var pid, subj, pre, rules, syms, auth, reasons, lnk string
			var idx int
			var cell inboxScoreCatalogCell
			var sa, df, psa, pdf sql.NullFloat64
			var at time.Time
			var pat sql.NullTime
			if err := crows.Scan(&pid, &idx, &subj, &pre, &cell.ISP, &cell.Source, &sa, &rules, &df, &syms, &cell.Folder,
				&auth, &reasons, &cell.Verdict, &cell.SendingDomainUsed, &at, &lnk, &cell.LinkCheck, &pdf, &psa, &pat); err != nil {
				crows.Close()
				respondError(w, http.StatusInternalServerError, "scan failed")
				return
			}
			c, ok := creatives[pid]
			if !ok {
				continue
			}
			if sa.Valid {
				v := sa.Float64
				cell.SAScore = &v
			}
			if df.Valid {
				v := df.Float64
				cell.DefenderScore = &v
			}
			if pdf.Valid {
				v := pdf.Float64
				cell.PrevDefenderScore = &v
			}
			if psa.Valid {
				v := psa.Float64
				cell.PrevSAScore = &v
			}
			if pat.Valid {
				cell.PrevScoredAt = pat.Time.UTC().Format(time.RFC3339)
			}
			cell.SARules, cell.DefenderSymbols, cell.Auth, cell.Reasons = json.RawMessage(rules), json.RawMessage(syms), json.RawMessage(auth), json.RawMessage(reasons)
			cell.Links = json.RawMessage(lnk)
			if cell.LinkCheck == "" {
				cell.LinkCheck = "unknown"
			}
			cell.ScoredAt = at.UTC().Format(time.RFC3339)
			var vt *inboxScoreCatalogVariant
			for i := range c.Variants {
				if c.Variants[i].Index == idx {
					vt = &c.Variants[i]
				}
			}
			if vt == nil {
				// Scored under an index the proof no longer has: keep it, marked stale, so the history is visible.
				c.Variants = append(c.Variants, inboxScoreCatalogVariant{Index: idx, Subject: subj, Preheader: pre, Verdict: "UNSCORED", Cells: map[string]*inboxScoreCatalogCell{}})
				vt = &c.Variants[len(c.Variants)-1]
				cell.Stale = true
			} else if idx >= 0 && (vt.Subject != subj || vt.Preheader != pre) {
				cell.Stale = true
			}
			cc := cell
			vt.Cells[cell.ISP] = &cc
			if cc.ScoredAt > vt.ScoredAt {
				vt.ScoredAt = cc.ScoredAt
			}
		}
		crows.Close()
	}

	// Variant verdicts + offer cards.
	byOffer := map[string]*inboxScoreCatalogOffer{}
	offerOfProof := map[string]string{}
	for _, pid := range order {
		c := creatives[pid]
		for i := range c.Variants {
			c.Variants[i].Verdict = inboxScoreVariantVerdict(c.Variants[i].Cells, InboxScoreISPs)
		}
		c.Drivers = inboxScoreCreativeDrivers(c, InboxScoreISPs)
		label := inboxScoreOfferFromProof(c.Name, c.OfferKey)
		offerOfProof[pid] = label
		o, ok := byOffer[label]
		if !ok {
			o = &inboxScoreCatalogOffer{Offer: label, Drivers: []inboxScoreDriver{}, LinkChecks: map[string]int{},
				Live: inboxScoreLive{Drift: []inboxScoreLiveCell{}, UncataloguedSubjects: []string{}}}
			byOffer[label] = o
		}
		o.Creatives = append(o.Creatives, *c)
		for _, v := range c.Variants {
			o.Variants++
			switch v.Verdict {
			case "PASS":
				o.Scored++
				o.Passed++
			case "FAIL", "PARTIAL":
				o.Scored++
				o.Failed++
			}
			for _, cell := range v.Cells {
				if cell != nil && !cell.Stale {
					o.LinkChecks[cell.LinkCheck]++
				}
			}
		}
	}
	for _, o := range byOffer {
		o.Drivers = inboxScoreOfferDrivers(o.Creatives)
		o.Worst, o.Best = inboxScoreExtremes(o.Creatives, InboxScoreISPs)
	}
	if err := s.inboxScoreBoardParity(ctx, org, creatives, offerOfProof, byOffer); err != nil {
		// Parity is an overlay: a failure here must not hide the catalogue.
		for _, o := range byOffer {
			o.Live.Drift = append(o.Live.Drift, inboxScoreLiveCell{Name: "board read failed: " + err.Error()})
		}
	}
	offers := make([]inboxScoreCatalogOffer, 0, len(byOffer))
	for _, o := range byOffer {
		offers = append(offers, *o)
	}
	sort.Slice(offers, func(i, j int) bool {
		if (offers[i].Scored > 0) != (offers[j].Scored > 0) {
			return offers[i].Scored > 0
		}
		if offers[i].Failed != offers[j].Failed {
			return offers[i].Failed > offers[j].Failed
		}
		return strings.ToLower(offers[i].Offer) < strings.ToLower(offers[j].Offer)
	})
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"as_of": s.now().UTC().Format(time.RFC3339), "isps": InboxScoreISPs, "offers": offers,
		"how_to_read": inboxScoreCatalogHowToRead,
	})
}

// inboxScoreVariantVerdict: PASS when every profile lane has a current PASS;
// FAIL when any lane failed; PARTIAL when some lanes are scored and all pass;
// UNSCORED when nothing current exists. Stale cells do not count.
func inboxScoreVariantVerdict(cells map[string]*inboxScoreCatalogCell, isps []string) string {
	scored, failed := 0, 0
	for _, isp := range isps {
		c := cells[isp]
		if c == nil || c.Stale {
			continue
		}
		scored++
		if c.Verdict != "PASS" {
			failed++
		}
	}
	switch {
	case scored == 0:
		return "UNSCORED"
	case failed > 0:
		return "FAIL"
	case scored < len(isps):
		return "PARTIAL"
	}
	return "PASS"
}

// ----------------------------------------------------------------------------- names

// inboxScoreOfferLabels: the offers the operator names, matched by substring of
// the proof name (case-insensitive), first match wins. Order matters where one
// name contains another (AEP eHealth before eHealth).
var inboxScoreOfferLabels = []struct{ needle, label string }{
	{"aep - ehealth", "AEP eHealth"}, {"aep ehealth", "AEP eHealth"}, {"ehealth", "eHealth"},
	{"liberty mutual", "Liberty Mutual"}, {"new york life", "New York Life"},
	{"insurify renters", "Insurify Renters"}, {"insurify home", "Insurify Home"}, {"insurify", "Insurify Auto"},
	{"mutual of omaha", "Mutual of Omaha"}, {"globe life", "Globe Life"}, {"healthcare.com", "Healthcare.com"},
	{"empire flooring", "Empire Flooring"}, {"metal roofing", "Metal Roofing"}, {"choice home warranty", "Choice Home Warranty"},
	{"serviceplus", "ServicePlus Home Warranty"}, {"adt", "ADT"}, {"aarp", "AARP"}, {"indigo", "Indigo"},
	{"destiny", "Destiny"}, {"milestone", "Milestone"}, {"sams club", "Sam's Club"}, {"sam's club", "Sam's Club"},
	{"sbli", "SBLI"}, {"optima tax", "Optima Tax"}, {"loan depot", "Loan Depot"}, {"west capital heloc", "WCL HELOC"},
	{"wcl-heloc", "WCL HELOC"}, {"ethos", "Ethos"}, {"budget blinds", "Budget Blinds"}, {"3 day blinds", "3 Day Blinds"},
	{"tahiti village", "Tahiti Village"}, {"bad credit loans", "Bad Credit Loans"}, {"burialinsurance", "Burialinsurance.com"},
	{"smart asset", "SmartAsset"}, {"smartasset", "SmartAsset"}, {"united settlement", "United Settlement"},
	{"total av", "Total AV"}, {"freedom debit relief", "Freedom Debt Relief"}, {"freedom debt relief", "Freedom Debt Relief"},
	{"accredited debit relief", "Accredited Debt Relief"}, {"accredited debt relief", "Accredited Debt Relief"},
	{"national debt relief", "National Debt Relief"}, {"directmeds", "DirectMeds"}, {"better help", "BetterHelp"},
	{"liz buys homes", "Liz Buys Homes"}, {"westshore", "Westshore Windows"}, {"1 day bath", "1 Day Bath"},
	{"cashout equity", "Cashout Equity"}, {"apfm", "A Place for Mom"}, {"a-place-for-mom", "A Place for Mom"},
}

var (
	reProofVersion = regexp.MustCompile(`(?i)(\s*[-—–]\s*|\s+)v\d+[a-z]?\b.*$`)
	reProofParen   = regexp.MustCompile(`\s*[\(\[].*?[\)\]]\s*`)
	reProofTail    = regexp.MustCompile(`(?i)\s*[-—–]\s*(final|custom|v\d+.*|\d+)\s*$`)
)

// inboxScoreOfferFromProof associates a proof with its offer BY NAME: a known
// label when one matches, else the name with its version/variant tail removed.
func inboxScoreOfferFromProof(name, offerKey string) string {
	hay := strings.ToLower(name + " " + offerKey)
	for _, l := range inboxScoreOfferLabels {
		if strings.Contains(hay, l.needle) {
			return l.label
		}
	}
	base := strings.TrimSpace(name)
	base = reProofParen.ReplaceAllString(base, " ")
	base = reProofVersion.ReplaceAllString(base, "")
	base = reProofTail.ReplaceAllString(base, "")
	base = strings.TrimSpace(strings.Trim(base, "-—– "))
	if base == "" {
		base = strings.TrimSpace(name)
	}
	if base == "" {
		base = offerKey
	}
	return base
}

// inboxScoreCatalogKind hides what is not an offer creative: newsletter
// placement tests and smoke-test proofs.
func inboxScoreCatalogKind(name, offerKey string) string {
	h := strings.ToLower(name + " " + offerKey)
	if strings.Contains(h, "newsletter") || strings.Contains(h, "placement test") || strings.HasPrefix(strings.ToLower(name), "nlv") ||
		strings.Contains(h, "testing placement") {
		return "newsletter"
	}
	return "offer"
}

// ----------------------------------------------------------------------------- insight

// inboxScoreTransportSymbol: sending-path symbols/rules — true of the route, not the content.
var inboxScoreTransportSymbol = regexp.MustCompile(`(?i)^(FORGED_SENDER|ONCE_RECEIVED|RCVD_|R_SPF_|R_DKIM_|DKIM_|SPF_|DMARC_|MX_|RDNS_|HFILTER_HOSTNAME|RECEIVED_|MISSING_MID|TO_DN_|FROM_NEQ_ENVFROM|FORGED_RECIPIENTS|HELO_|ARC_|MIME_TRACE|ASN|RBL_|DNSWL_|RWL_|FREEMAIL_ENVFROM|T_SCC_BODY_TEXT_LINE)`)

type scoredSymbol struct {
	Name, Kind, Detail string
	Score              float64
}

// variantSymbols: the union of positive defender symbols and pre-send rules a variant fired
// across its current lanes (identical bytes land on every lane; per-ISP rules differ only in weight).
func variantSymbols(v inboxScoreCatalogVariant, isps []string) map[string]scoredSymbol {
	out := map[string]scoredSymbol{}
	for _, isp := range isps {
		cell := v.Cells[isp]
		if cell == nil || cell.Stale {
			continue
		}
		var syms []struct {
			Symbol string  `json:"symbol"`
			Score  float64 `json:"score"`
			Detail string  `json:"detail"`
		}
		_ = json.Unmarshal(cell.DefenderSymbols, &syms)
		for _, sy := range syms {
			if sy.Score <= 0 {
				continue
			}
			k := "defender:" + sy.Symbol
			if cur, ok := out[k]; !ok || sy.Score > cur.Score {
				out[k] = scoredSymbol{Name: sy.Symbol, Kind: "defender", Score: sy.Score, Detail: sy.Detail}
			}
		}
		var rules []struct {
			Rule  string  `json:"rule"`
			Score float64 `json:"score"`
		}
		_ = json.Unmarshal(cell.SARules, &rules)
		for _, r := range rules {
			if r.Score <= 0 {
				continue
			}
			k := "presend:" + r.Rule
			if cur, ok := out[k]; !ok || r.Score > cur.Score {
				out[k] = scoredSymbol{Name: r.Rule, Kind: "presend", Score: r.Score}
			}
		}
	}
	return out
}

// inboxScoreCreativeDrivers: what moves this creative's score, with the body/copy split.
func inboxScoreCreativeDrivers(c *inboxScoreCatalogCreative, isps []string) []inboxScoreDriver {
	seen := map[string]*inboxScoreDriver{}
	scored := 0
	for _, v := range c.Variants {
		syms := variantSymbols(v, isps)
		if len(syms) == 0 && inboxScoreVariantVerdict(v.Cells, isps) == "UNSCORED" {
			continue
		}
		scored++
		for k, sy := range syms {
			d, ok := seen[k]
			if !ok {
				d = &inboxScoreDriver{Name: sy.Name, Kind: sy.Kind, Detail: sy.Detail}
				seen[k] = d
			}
			d.Variants++
			if sy.Score > d.Score {
				d.Score = sy.Score
			}
		}
	}
	c.ScoredLines = scored
	out := make([]inboxScoreDriver, 0, len(seen))
	for _, d := range seen {
		d.Of = scored
		switch {
		case inboxScoreTransportSymbol.MatchString(d.Name):
			d.Scope = "transport"
		case scored >= 2 && d.Variants == scored:
			d.Scope = "body"
		case scored >= 2:
			d.Scope = "copy"
		default:
			d.Scope = "content" // one scored line: body and copy cannot be told apart yet
		}
		out = append(out, *d)
	}
	sortDrivers(out)
	return out
}

// inboxScoreOfferDrivers merges the creatives' drivers: max score, summed reach,
// scope by what the creatives agree on (any copy → copy; else body; transport stays).
func inboxScoreOfferDrivers(cs []inboxScoreCatalogCreative) []inboxScoreDriver {
	seen := map[string]*inboxScoreDriver{}
	total := 0
	for _, c := range cs {
		total += c.ScoredLines
		for _, d := range c.Drivers {
			k := d.Kind + ":" + d.Name
			cur, ok := seen[k]
			if !ok {
				cp := d
				cp.Of = total // corrected below
				seen[k] = &cp
				continue
			}
			cur.Variants += d.Variants
			if d.Score > cur.Score {
				cur.Score = d.Score
			}
			if cur.Scope != "transport" && (d.Scope == "copy" || cur.Scope == "copy") {
				cur.Scope = "copy"
			}
			if cur.Detail == "" {
				cur.Detail = d.Detail
			}
		}
	}
	out := make([]inboxScoreDriver, 0, len(seen))
	for _, d := range seen {
		d.Of = total // reach is measured against every scored line of the offer
		// Across the offer the reach decides: on every scored line of every creative → body.
		if d.Scope != "transport" {
			switch {
			case d.Of >= 2 && d.Variants == d.Of:
				d.Scope = "body"
			case d.Of >= 2:
				d.Scope = "copy"
			}
		}
		out = append(out, *d)
	}
	sortDrivers(out)
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

func sortDrivers(out []inboxScoreDriver) {
	sort.Slice(out, func(i, j int) bool {
		wi, wj := out[i].Score*float64(out[i].Variants), out[j].Score*float64(out[j].Variants)
		if wi != wj {
			return wi > wj
		}
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Name < out[j].Name
	})
}

// inboxScoreExtremes: the worst and best scored subject line of an offer, by the
// highest defender score across lanes (pre-send as the tiebreak).
func inboxScoreExtremes(cs []inboxScoreCatalogCreative, isps []string) (worst, best *inboxScoreExtreme) {
	for _, c := range cs {
		for _, v := range c.Variants {
			var def, pre float64
			n := 0
			for _, isp := range isps {
				cell := v.Cells[isp]
				if cell == nil || cell.Stale {
					continue
				}
				n++
				if cell.DefenderScore != nil && *cell.DefenderScore > def || n == 1 && cell.DefenderScore != nil {
					def = *cell.DefenderScore
				}
				if cell.SAScore != nil && *cell.SAScore > pre || n == 1 && cell.SAScore != nil {
					pre = *cell.SAScore
				}
			}
			if n == 0 {
				continue
			}
			e := &inboxScoreExtreme{Creative: c.Name, Subject: v.Subject, Index: v.Index, Defender: def, Presend: pre, Headroom: 2.4 - def}
			if v.Index < 0 {
				e.Subject = c.Name + " (no subject on file)"
			}
			if worst == nil || def > worst.Defender || (def == worst.Defender && pre > worst.Presend) {
				worst = e
			}
			if best == nil || def < best.Defender || (def == best.Defender && pre < best.Presend) {
				best = e
			}
		}
	}
	return worst, best
}

// inboxScoreBoardParity matches the board's cells (yesterday → +8 days, Denver) to the
// catalogue by exact HTML bytes. A cell whose bytes match a catalogued creative counts
// for that creative; a cell that names this offer but matches no catalogued creative is
// DRIFTED — it mails bytes the catalogue never scored (the 2026-10-24..31 Liberty cells
// carried the pre-rehost Race Track body while every proof was clean).
func (s *InboxScoreService) inboxScoreBoardParity(ctx context.Context, org string, creatives map[string]*inboxScoreCatalogCreative,
	offerOfProof map[string]string, byOffer map[string]*inboxScoreCatalogOffer) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, COALESCE(name,''), COALESCE(offer_key,''), COALESCE(status,''), scheduled_at,
		       COALESCE(total_recipients,0), COALESCE(subject,''), md5(COALESCE(html_content,''))
		FROM mailing_campaigns
		WHERE organization_id = $1
		  AND status IN ('scheduled','preparing','finalizing_audience','sending','sent','completed','completed_with_errors')
		  AND scheduled_at >= NOW() - INTERVAL '36 hours' AND scheduled_at < NOW() + INTERVAL '8 days'
		  AND COALESCE(html_content,'') <> ''`, org)
	if err != nil {
		return err
	}
	defer rows.Close()
	byHash := map[string]string{} // html md5 → proof id
	for pid, c := range creatives {
		if c.htmlHash != "" {
			byHash[c.htmlHash] = pid
		}
	}
	denver, _ := time.LoadLocation("America/Denver")
	type pending struct {
		o    *inboxScoreCatalogOffer
		cell inboxScoreLiveCell
	}
	for rows.Next() {
		var id, name, key, status, subject, hash string
		var at time.Time
		var rec int
		if err := rows.Scan(&id, &name, &key, &status, &at, &rec, &subject, &hash); err != nil {
			return err
		}
		if inboxScoreKind(name) != "offer" {
			continue
		}
		day := at.In(denver).Format("2006-01-02")
		if pid, ok := byHash[hash]; ok {
			c := creatives[pid]
			o := byOffer[offerOfProof[pid]]
			c.LiveCells++
			c.LiveRecipients += rec
			o.Live.Cells++
			o.Live.Matching++
			o.Live.Recipients += rec
			if !inboxScoreSubjectCatalogued(c, subject) {
				o.Live.UncataloguedSubjects = appendUnique(o.Live.UncataloguedSubjects, subject)
			}
			continue
		}
		// No byte match: find the offer this cell names.
		o := inboxScoreOfferByLabel(inboxScoreOffer(name, key), byOffer)
		if o == nil {
			continue
		}
		o.Live.Cells++
		o.Live.Recipients += rec
		if reMutatingBuilder.MatchString(name) {
			o.Live.Mutated++
			o.Live.MutatedRecipients += rec
			continue
		}
		o.Live.Drifted++
		o.Live.Drift = append(o.Live.Drift, inboxScoreLiveCell{Name: name, Day: day, Status: status, Recipients: rec, Subject: subject,
			SubjectCatalogued: inboxScoreOfferSubjectCatalogued(o, subject)})
	}
	for _, o := range byOffer {
		sort.Slice(o.Live.Drift, func(i, j int) bool {
			if o.Live.Drift[i].Day != o.Live.Drift[j].Day {
				return o.Live.Drift[i].Day < o.Live.Drift[j].Day
			}
			return o.Live.Drift[i].Name < o.Live.Drift[j].Name
		})
	}
	// Mirror the creative-level live counts into the offers' creative copies.
	for _, o := range byOffer {
		for i := range o.Creatives {
			if c := creatives[o.Creatives[i].ProofID]; c != nil {
				o.Creatives[i].LiveCells, o.Creatives[i].LiveRecipients = c.LiveCells, c.LiveRecipients
			}
		}
	}
	return rows.Err()
}

func inboxScoreSubjectCatalogued(c *inboxScoreCatalogCreative, subject string) bool {
	s := strings.TrimSpace(strings.ToLower(subject))
	for _, v := range c.Variants {
		if strings.TrimSpace(strings.ToLower(v.Subject)) == s {
			return true
		}
	}
	return false
}

func inboxScoreOfferSubjectCatalogued(o *inboxScoreCatalogOffer, subject string) bool {
	for i := range o.Creatives {
		if inboxScoreSubjectCatalogued(&o.Creatives[i], subject) {
			return true
		}
	}
	return false
}

func appendUnique(xs []string, x string) []string {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}

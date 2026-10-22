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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "begin failed")
		return
	}
	defer tx.Rollback() //nolint:errcheck
	n := 0
	for i, sc := range body.Scores {
		if _, err := uuid.Parse(sc.ProofID); err != nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("scores[%d].proof_id is not a UUID", i))
			return
		}
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
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mailing_inbox_score_catalog
			  (organization_id, proof_id, variant_index, subject, preheader, isp, source, sa_score, sa_rules,
			   defender_score, defender_symbols, folder, auth, reasons, verdict, sending_domain_used, scored_at)
			VALUES ($1,$2,$3,$4::text,$5::text,$6,$7,$8,COALESCE($9::jsonb,'[]'::jsonb),$10,COALESCE($11::jsonb,'[]'::jsonb),
			        $12::text,COALESCE($13::jsonb,'{}'::jsonb),COALESCE($14::jsonb,'[]'::jsonb),$15,$16::text,$17)
			ON CONFLICT (organization_id, proof_id, variant_index, isp, source) DO UPDATE SET
			  subject=EXCLUDED.subject, preheader=EXCLUDED.preheader, sa_score=EXCLUDED.sa_score, sa_rules=EXCLUDED.sa_rules,
			  defender_score=EXCLUDED.defender_score, defender_symbols=EXCLUDED.defender_symbols, folder=EXCLUDED.folder,
			  auth=EXCLUDED.auth, reasons=EXCLUDED.reasons, verdict=EXCLUDED.verdict,
			  sending_domain_used=EXCLUDED.sending_domain_used, scored_at=EXCLUDED.scored_at`,
			org, sc.ProofID, sc.VariantIndex, sc.Subject, sc.Preheader, sc.ISP, sc.Source, sc.SAScore, nullRaw(sc.SARules),
			sc.DefenderScore, nullRaw(sc.DefenderSymbols), sc.Folder, nullRaw(sc.Auth), nullRaw(sc.Reasons), sc.Verdict,
			sc.SendingDomainUsed, at); err != nil {
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
}

type inboxScoreCatalogVariant struct {
	Index     int                                `json:"index"`
	Subject   string                             `json:"subject"`
	Preheader string                             `json:"preheader"`
	Verdict   string                             `json:"verdict"` // PASS | FAIL | PARTIAL | UNSCORED
	ScoredAt  string                             `json:"scored_at,omitempty"`
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
}

type inboxScoreCatalogOffer struct {
	Offer     string                      `json:"offer"`
	Creatives []inboxScoreCatalogCreative `json:"creatives"`
	// Counts for the card face.
	Variants int `json:"variants"`
	Scored   int `json:"scored"`
	Passed   int `json:"passed"`
	Failed   int `json:"failed"`
}

var inboxScoreCatalogHowToRead = []string{
	"One card per offer; offers are grouped by name. Inside: every creative in Creative Studio, and under it every subject line + preheader it carries.",
	"Each subject line is scored on its own, per mailbox provider, from the exact bytes our defender received — the sending domain is not part of the score.",
	"Defender = our near-real-life filter (score of 15: ready under 2.4, review under 4.6, fix under 8, junked at 8). Pre-send = SpamAssassin (ready under 1.5, review under 3, fix under 5, fail at 5). PASS needs inbox, both ready, DMARC pass and no listed link host, on every provider.",
	"Not scored yet = run the gate: python3 -m agents.inboxscore.gate --proof \"<creative name>\" --all-variants. A stale score means the copy changed after it was scored — run it again.",
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
		       COALESCE(variants::text,'[]'), COALESCE(from_names::text,'[]'), COALESCE(updated_at, created_at)
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
		if err := rows.Scan(&c.ProofID, &c.Name, &c.OfferKey, &c.ApprovalStatus, &active, &variantsRaw, &fromRaw, &upd); err != nil {
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
			       defender_symbols::text, folder, auth::text, reasons::text, verdict, sending_domain_used, scored_at
			FROM mailing_inbox_score_catalog
			WHERE organization_id = $1 AND proof_id = ANY($2::uuid[])
			ORDER BY scored_at`, org, pq.Array(order))
		if err != nil {
			respondError(w, http.StatusInternalServerError, "read failed: "+err.Error())
			return
		}
		for crows.Next() {
			var pid, subj, pre, rules, syms, auth, reasons string
			var idx int
			var cell inboxScoreCatalogCell
			var sa, df sql.NullFloat64
			var at time.Time
			if err := crows.Scan(&pid, &idx, &subj, &pre, &cell.ISP, &cell.Source, &sa, &rules, &df, &syms, &cell.Folder,
				&auth, &reasons, &cell.Verdict, &cell.SendingDomainUsed, &at); err != nil {
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
			cell.SARules, cell.DefenderSymbols, cell.Auth, cell.Reasons = json.RawMessage(rules), json.RawMessage(syms), json.RawMessage(auth), json.RawMessage(reasons)
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
	for _, pid := range order {
		c := creatives[pid]
		for i := range c.Variants {
			c.Variants[i].Verdict = inboxScoreVariantVerdict(c.Variants[i].Cells, InboxScoreISPs)
		}
		label := inboxScoreOfferFromProof(c.Name, c.OfferKey)
		o, ok := byOffer[label]
		if !ok {
			o = &inboxScoreCatalogOffer{Offer: label}
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

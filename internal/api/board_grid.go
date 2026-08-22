package api

// Board Grid — the send-day board as a PROPERTY × SLOT grid you edit by
// exception, that arrives already knowing what is wrong with it.
//
//	GET  /api/mailing/board-grid?date=YYYY-MM-DD     the day, as a grid, gated
//	GET  /api/mailing/board-grid/clone?from=&to=     yesterday's grid, retimed (NO WRITE)
//	POST /api/mailing/board-grid/gates               run the gates over a proposed grid
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY THIS EXISTS
//
// The structure of a send-day (which properties mail, in which slots) is stable
// day over day. The only daily decision is WHICH OFFER sits in each cell.
// Everything else — sending domain, audience, timing, creative, copy, name — is
// derivable. Typing it by hand 48 times a day is what produced every defect on
// the 08/19–08/22 boards:
//
//	'MF' for MH, '080202026', '08062026'          → NAME
//	a DB-named campaign sending from consumerpro  → NAME_PROPERTY_MISMATCH
//	RR-Globe and RB-ADR finalizing with NULL offer→ MISSING_OFFER
//	RR-HELOC shipping a Liquid {{custom.*}} subject→ LIQUID_SUBJECT
//	WF and BWP double-booked at one anchor        → SLOT_COLLISION
//
// Each of those is a rule, not a judgement, so each is a gate below. The offer
// assignment stays the operator's — this file never picks one.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHAT COUNTS AS "THE BOARD"
//
// 2026-08-22 carried 692 campaigns, of which only 50 are the board. They are NOT
// separable by execution_mode — all 692 are 'pmta_isp_wave'. The discriminator
// is partner_drip_tag IS NULL AND journey_id IS NULL: the ~644 others are
// orchestrator-created '[partner-drip] <vertical> <brand> <ts>' sends firing at
// scattered evening minutes. A name regex ('^[0-9]{8} - ') returns 48 and
// silently DROPS the two kumo warm-ramp campaigns ('aug22 - aad - KUMO-WARM'),
// which are operator-scheduled broadcasts and belong on the grid. Do not
// "simplify" this predicate back to a name match.
//
// ─────────────────────────────────────────────────────────────────────────────
// READ-ONLY BY CONSTRUCTION
//
// Every statement in this file is a SELECT. Cloning returns a PROPOSAL; it does
// not create campaigns. Deploying a cell goes through the existing
// /api/mailing/pmta-campaign/deploy path, which owns audience planning, the wave
// sanity check, and the time_spans[*].source contract. There is no second send
// path here and none may be added — TestBoardGrid_ReadOnly asserts it.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// BoardGridService serves the property × slot view of a send-day.
type BoardGridService struct {
	db *sql.DB
}

func NewBoardGridService(db *sql.DB) *BoardGridService {
	return &BoardGridService{db: db}
}

func (s *BoardGridService) RegisterRoutes(r chi.Router) {
	r.Route("/board-grid", func(cr chi.Router) {
		cr.Get("/", s.HandleGetGrid)
		cr.Get("/clone", s.HandleCloneGrid)
		cr.Post("/gates", s.HandleRunGates)
	})
}

// boardCampaignPredicate isolates the operator-scheduled broadcast board from
// the orchestrator's partner-drip sends. See the WHAT COUNTS AS "THE BOARD"
// note above before changing it.
const boardCampaignPredicate = `
    c.partner_drip_tag IS NULL
AND c.journey_id IS NULL
AND c.status NOT IN ('cancelled','deleted')`

// BoardCell is one (property, slot) assignment.
type BoardCell struct {
	Property      string `json:"property"`       // brand code, e.g. "DB"
	PropertyLabel string `json:"property_label"` // e.g. "Discount Blog"
	SendingDomain string `json:"sending_domain"`
	BrandRoot     string `json:"brand_root,omitempty"`
	Slot          string `json:"slot"` // Denver HH:MM
	CampaignID    string `json:"campaign_id,omitempty"`
	Name          string `json:"name"`
	OfferID       string `json:"offer_id,omitempty"`
	OfferName     string `json:"offer_name,omitempty"`
	Subject       string `json:"subject,omitempty"`
	Status        string `json:"status,omitempty"`
	Recipients    int    `json:"recipients"`
	Proposed      bool   `json:"proposed,omitempty"`
}

// BoardFinding is one gate result against one cell.
type BoardFinding struct {
	Level    string `json:"level"` // "blocker" | "warn"
	Code     string `json:"code"`
	Property string `json:"property"`
	Slot     string `json:"slot"`
	Message  string `json:"message"`
}

type BoardGrid struct {
	Date       string         `json:"date"`
	Slots      []string       `json:"slots"`
	Properties []string       `json:"properties"`
	Cells      []BoardCell    `json:"cells"`
	Findings   []BoardFinding `json:"findings"`
	Summary    map[string]int `json:"summary"`
	SourceDate string         `json:"source_date,omitempty"`
}

// ---------------------------------------------------------------------------
// Handlers

func (s *BoardGridService) HandleGetGrid(w http.ResponseWriter, r *http.Request) {
	date := strings.TrimSpace(r.URL.Query().Get("date"))
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		respondError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
		return
	}
	cells, err := s.loadCells(r, date)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, s.assemble(r, date, "", cells))
}

// HandleCloneGrid returns `from`'s grid retimed onto `to`. It writes nothing:
// the response is a PROPOSAL the operator edits and then deploys cell by cell.
func (s *BoardGridService) HandleCloneGrid(w http.ResponseWriter, r *http.Request) {
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	to := strings.TrimSpace(r.URL.Query().Get("to"))
	if from == "" || to == "" {
		respondError(w, http.StatusBadRequest, "from and to are required (YYYY-MM-DD)")
		return
	}
	for _, d := range []string{from, to} {
		if _, err := time.Parse("2006-01-02", d); err != nil {
			respondError(w, http.StatusBadRequest, "dates must be YYYY-MM-DD")
			return
		}
	}
	src, err := s.loadCells(r, from)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	toks, err := time.Parse("2006-01-02", to)
	if err != nil {
		respondError(w, http.StatusBadRequest, "bad to date")
		return
	}
	nameDate := toks.Format("01022006")
	proposed := make([]BoardCell, 0, len(src))
	for _, c := range src {
		c.CampaignID = "" // a proposal is not a campaign
		c.Status = ""
		c.Recipients = 0
		c.Proposed = true
		c.Name = reDateToken.ReplaceAllString(c.Name, nameDate)
		proposed = append(proposed, c)
	}
	respondJSON(w, http.StatusOK, s.assemble(r, to, from, proposed))
}

// HandleRunGates gates a proposed grid the operator has edited client-side.
func (s *BoardGridService) HandleRunGates(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Date  string      `json:"date"`
		Cells []BoardCell `json:"cells"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Date == "" {
		respondError(w, http.StatusBadRequest, "date is required")
		return
	}
	respondJSON(w, http.StatusOK, s.assemble(r, body.Date, "", body.Cells))
}

// ---------------------------------------------------------------------------
// Loading

func (s *BoardGridService) loadCells(r *http.Request, date string) ([]BoardCell, error) {
	orgID := getOrgID(r)
	const q = `
SELECT COALESCE(bm.brand_code, '')                                   AS brand_code,
       COALESCE(bm.brand_label, '')                                  AS brand_label,
       COALESCE(sp.sending_domain, '')                               AS sending_domain,
       COALESCE(bm.brand_root, '')                                   AS brand_root,
       to_char(c.scheduled_at AT TIME ZONE 'America/Denver','HH24:MI') AS slot,
       c.id::text                                                    AS campaign_id,
       COALESCE(c.name,'')                                           AS name,
       COALESCE(c.offer_id::text,'')                                 AS offer_id,
       COALESCE(o.name,'')                                           AS offer_name,
       COALESCE(c.subject,'')                                        AS subject,
       COALESCE(c.status,'')                                         AS status,
       COALESCE(c.total_recipients,0)                                AS recipients
  FROM mailing_campaigns c
  LEFT JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
  LEFT JOIN mailing_offers o            ON o.id  = c.offer_id
  LEFT JOIN mailing_brand_metadata bm
         ON bm.sending_domain = regexp_replace(COALESCE(sp.sending_domain,''), '^(e?m)\.', 'em.')
 WHERE c.scheduled_at >= ($1::date)
   AND c.scheduled_at <  ($1::date + INTERVAL '1 day')
   AND ($2 = '' OR c.organization_id::text = $2)
   AND ` + boardCampaignPredicate + `
 ORDER BY 1, 4`

	rows, err := s.db.Query(q, date, orgID)
	if err != nil {
		return nil, fmt.Errorf("board grid query: %w", err)
	}
	defer rows.Close()

	var out []BoardCell
	for rows.Next() {
		var c BoardCell
		if err := rows.Scan(&c.Property, &c.PropertyLabel, &c.SendingDomain, &c.BrandRoot, &c.Slot,
			&c.CampaignID, &c.Name, &c.OfferID, &c.OfferName, &c.Subject,
			&c.Status, &c.Recipients); err != nil {
			return nil, fmt.Errorf("board grid scan: %w", err)
		}
		if c.Property == "" {
			// No brand-metadata row for this sending domain. Fall back to the
			// apex so the cell is still visible and still gated — dropping it
			// would hide a real campaign from the board.
			c.Property = apexFromSendingDomain(c.SendingDomain)
			c.PropertyLabel = c.Property
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Assembly + gates

func (s *BoardGridService) assemble(r *http.Request, date, sourceDate string, cells []BoardCell) BoardGrid {
	slotSet := map[string]bool{}
	propSet := map[string]bool{}
	for _, c := range cells {
		slotSet[c.Slot] = true
		propSet[c.Property] = true
	}
	slots := keysSorted(slotSet)
	props := keysSorted(propSet)

	findings := s.runGates(r, date, cells)
	summary := map[string]int{"cells": len(cells), "blocker": 0, "warn": 0}
	for _, f := range findings {
		summary[f.Level]++
	}
	summary["clean"] = len(cells) - distinctCellsWithFindings(findings, cells)

	return BoardGrid{
		Date:       date,
		SourceDate: sourceDate,
		Slots:      slots,
		Properties: props,
		Cells:      cells,
		Findings:   findings,
		Summary:    summary,
	}
}

var (
	reDateToken = regexp.MustCompile(`\b\d{8}\b`)
	reLiquid    = regexp.MustCompile(`\{\{|\{%`)
)

func (s *BoardGridService) runGates(r *http.Request, date string, cells []BoardCell) []BoardFinding {
	var f []BoardFinding

	// Gate 1 SLOT_COLLISION — two campaigns on one property at one anchor.
	// This is the WF/BWP double-booking, and it is only ever a mistake.
	seen := map[string][]BoardCell{}
	for _, c := range cells {
		k := c.Property + "|" + c.Slot
		seen[k] = append(seen[k], c)
	}
	for k, group := range seen {
		if len(group) > 1 {
			parts := strings.SplitN(k, "|", 2)
			names := make([]string, 0, len(group))
			for _, g := range group {
				names = append(names, g.Name)
			}
			sort.Strings(names)
			f = append(f, BoardFinding{"blocker", "SLOT_COLLISION", parts[0], parts[1],
				fmt.Sprintf("%d campaigns share this anchor: %s", len(group), strings.Join(names, " | "))})
		}
	}

	// Gate 2 REPEAT_OFFER — the same offer twice on one property in one day.
	// Operator rule when the third slot was added: "no repetitive campaigns for
	// that sending domain."
	byProp := map[string]map[string][]BoardCell{}
	for _, c := range cells {
		if c.OfferID == "" {
			continue
		}
		if byProp[c.Property] == nil {
			byProp[c.Property] = map[string][]BoardCell{}
		}
		byProp[c.Property][c.OfferID] = append(byProp[c.Property][c.OfferID], c)
	}
	for prop, offers := range byProp {
		for _, group := range offers {
			if len(group) > 1 {
				slots := make([]string, 0, len(group))
				for _, g := range group {
					slots = append(slots, g.Slot)
				}
				sort.Strings(slots)
				f = append(f, BoardFinding{"blocker", "REPEAT_OFFER", prop, slots[0],
					fmt.Sprintf("%q runs %d times on this property (%s)",
						group[0].OfferName, len(group), strings.Join(slots, ", "))})
			}
		}
	}

	// Per-cell gates.
	dateTok := ""
	if t, err := time.Parse("2006-01-02", date); err == nil {
		dateTok = t.Format("01022006")
	}
	for _, c := range cells {
		// Gate 3 MISSING_OFFER — RR-Globe / RB-ADR finalized with a NULL offer_id,
		// which silently breaks conversion attribution and offer suppression.
		if c.OfferID == "" {
			f = append(f, BoardFinding{"blocker", "MISSING_OFFER", c.Property, c.Slot,
				"no offer_id — conversions will not attribute and offer suppression cannot apply"})
		}
		// Gate 4 LIQUID_SUBJECT — RR-HELOC shipped '{{custom.equity_estimate}}'
		// in a subject line to an audience that had no such field.
		if reLiquid.MatchString(c.Subject) {
			f = append(f, BoardFinding{"blocker", "LIQUID_SUBJECT", c.Property, c.Slot,
				fmt.Sprintf("subject carries an unrendered template token: %q", c.Subject)})
		}
		// Gate 5 NAME_DATE — '08062026' and '080202026' both shipped.
		if dateTok != "" && !strings.Contains(c.Name, dateTok) {
			f = append(f, BoardFinding{"warn", "NAME_DATE", c.Property, c.Slot,
				fmt.Sprintf("name does not carry this board's date token %s: %q", dateTok, c.Name)})
		}
		// Gate 6 NAME_PROPERTY — a 'DB'-named campaign that sent from consumerpro.
		if c.Property != "" && !nameMentionsPropertyRoot(c.Name, c.Property, c.BrandRoot) {
			f = append(f, BoardFinding{"warn", "NAME_PROPERTY", c.Property, c.Slot,
				fmt.Sprintf("name does not name property %s (domain is %s): %q",
					c.Property, c.SendingDomain, c.Name)})
		}
	}

	sort.SliceStable(f, func(i, j int) bool {
		if f[i].Level != f[j].Level {
			return f[i].Level == "blocker"
		}
		if f[i].Property != f[j].Property {
			return f[i].Property < f[j].Property
		}
		return f[i].Slot < f[j].Slot
	})
	return f
}

// ---------------------------------------------------------------------------
// helpers

// apexFromSendingDomain turns m.foo.com / em.foo.com into FOO for display.
func apexFromSendingDomain(sd string) string {
	sd = strings.TrimSpace(strings.ToLower(sd))
	if sd == "" {
		return "(unmapped)"
	}
	sd = strings.TrimPrefix(strings.TrimPrefix(sd, "em."), "m.")
	if i := strings.IndexByte(sd, '.'); i > 0 {
		sd = sd[:i]
	}
	return strings.ToUpper(sd)
}

// nameMentionsProperty decides whether a campaign name names its property.
//
// THE TWO CODE SYSTEMS. There are two live brand-code schemes and a correct
// board uses BOTH: mailing_brand_metadata.brand_code is BW / HW / LP / TT / YI,
// while the board's own names (and the drip orchestrator's roster) use
// BWP / HWS / LPL / TOT / YIH. Matching only brand_code raises a false warning
// on 15 of the 50 cells of a CORRECT board — verified against 2026-08-22 — and
// a gate that cries wolf 15 times is worse than no gate at all.
//
// So the rule is: a name token satisfies the property when it is the brand code
// OR an acronym of the brand root, i.e. its letters appear IN ORDER inside the
// root. HWS fits homewarrantyservices, LPL fits learnpersonalloans, TOT fits
// thingoftheday, YIH fits yourinsurancehub — without hardcoding an alias map,
// which is exactly the shortcut that was wrong for 5 of 8 brands last time it
// was guessed.
//
// Deliberately lenient: for a WARNING gate a false accept costs nothing, while
// a false reject trains the operator to ignore the column. It still rejects the
// defects that actually shipped — 'MF' does not fit myownhealth (no 'f'), and a
// 'DB'-named campaign does not fit consumerpro (no 'd').
//
// Substring matching is NOT enough on its own: 'RR' is inside 'CURRENT' and
// 'MR' is inside 'MRD', so tokens are compared whole.
func nameMentionsProperty(name, code string) bool {
	return nameMentionsPropertyRoot(name, code, "")
}

func nameMentionsPropertyRoot(name, code, brandRoot string) bool {
	if code == "" {
		return true
	}
	up := strings.ToUpper(name)
	code = strings.ToUpper(code)
	root := lettersOnly(strings.ToUpper(brandRoot))
	for _, tok := range strings.FieldsFunc(up, func(r rune) bool {
		return r < 'A' || r > 'Z'
	}) {
		if tok == code {
			return true
		}
		// Accept the other code system: an acronym of the brand root.
		if root != "" && len(tok) >= 2 && isSubsequence(tok, root) {
			return true
		}
	}
	return false
}

func lettersOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isSubsequence reports whether every rune of need appears in hay in order.
func isSubsequence(need, hay string) bool {
	i := 0
	for _, h := range hay {
		if i < len(need) && rune(need[i]) == h {
			i++
		}
	}
	return i == len(need)
}

func keysSorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func distinctCellsWithFindings(f []BoardFinding, cells []BoardCell) int {
	hit := map[string]bool{}
	for _, x := range f {
		hit[x.Property+"|"+x.Slot] = true
	}
	n := 0
	for _, c := range cells {
		if hit[c.Property+"|"+c.Slot] {
			n++
		}
	}
	return n
}

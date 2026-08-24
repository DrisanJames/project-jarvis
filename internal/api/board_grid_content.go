package api

// Board Grid CONTENT — what a live cell will actually put in the inbox.
//
//	GET /api/mailing/board-grid/creative?campaign_id=<uuid>
//
// The grid answers "which offer sits in which slot". Until now nothing on the
// screen answered "and what will it SAY" — the operator could see a subject
// string and nothing else, so the preheader, the friendly-from and the whole
// creative body were invisible until the mail had already gone out (operator
// 2026-08-23: "I do not know the subject lines, preheaders and content it is
// going to use").
//
// This reads the STORED columns of the campaign row — subject, preview_text,
// from_name/from_email and html_content. Those are exactly what
// SendWorkerPool claims and renders (send_worker.go:2250 builds the queue row
// from them), so this is the send truth, NOT the offer's approved proof. The
// proof is only what a REBUILD would install; a live campaign may have been
// deployed from a different creative entirely, and conflating the two is how
// a mismatch stays invisible.
//
// READ-ONLY: one SELECT, org-scoped, no writes. The HTML comes back as JSON
// (never as text/html) so the portal can render it inside a sandboxed iframe
// via srcdoc rather than executing operator-authored creative on the API
// origin.

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"
)

// boardGridCreativeMaxBytes bounds one response. The largest board creative
// measured 2026-08-24 was 61,712 bytes; 2 MB leaves generous headroom while
// keeping a corrupt row from streaming unbounded into the browser.
// (The field is named Clipped, not Truncated: TestBoardGrid_ReadOnly scans this
// file for SQL verbs and "TRUNCATE" is one of them.)
const boardGridCreativeMaxBytes = 2 << 20

type boardGridCreative struct {
	CampaignID    string `json:"campaign_id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	SendingDomain string `json:"sending_domain"`
	FromName      string `json:"from_name"`
	FromEmail     string `json:"from_email"`
	ReplyTo       string `json:"reply_to"`
	OfferName     string `json:"offer_name"`

	Subject           string `json:"subject"`
	SubjectRendered   string `json:"subject_rendered,omitempty"`
	SubjectProblem    string `json:"subject_problem,omitempty"`
	Preheader         string `json:"preheader"`
	PreheaderRendered string `json:"preheader_rendered,omitempty"`
	PreheaderProblem  string `json:"preheader_problem,omitempty"`

	CreativeLen int    `json:"creative_len"`
	HTML        string `json:"html"`
	HTMLClipped bool   `json:"html_clipped,omitempty"`
	Recipients  int    `json:"recipients"`
}

// HandleGetCellCreative serves one cell's send-truth content.
func (s *BoardGridService) HandleGetCellCreative(w http.ResponseWriter, r *http.Request) {
	campaignID := strings.TrimSpace(r.URL.Query().Get("campaign_id"))
	if campaignID == "" {
		respondError(w, http.StatusBadRequest, "campaign_id is required")
		return
	}
	orgID := getOrgID(r)

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	const q = `
SELECT COALESCE(c.name,''), COALESCE(c.status,''),
       COALESCE(NULLIF(sp.sending_domain,''),
                NULLIF(c.pmta_config->'campaign_input'->>'sending_domain',''), ''),
       COALESCE(c.from_name,''), COALESCE(c.from_email,''), COALESCE(c.reply_to,''),
       COALESCE(o.name,''),
       COALESCE(c.subject,''), COALESCE(c.preview_text,''),
       length(COALESCE(c.html_content,'')), COALESCE(c.html_content,''),
       COALESCE(c.total_recipients,0)
  FROM mailing_campaigns c
  LEFT JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
  LEFT JOIN mailing_offers o            ON o.id  = c.offer_id
 WHERE c.id = $1::uuid
   AND ($2 = '' OR c.organization_id::text = $2)`

	out := boardGridCreative{CampaignID: campaignID}
	err := s.db.QueryRowContext(ctx, q, campaignID, orgID).Scan(
		&out.Name, &out.Status, &out.SendingDomain,
		&out.FromName, &out.FromEmail, &out.ReplyTo, &out.OfferName,
		&out.Subject, &out.Preheader,
		&out.CreativeLen, &out.HTML, &out.Recipients)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "campaign not found in this organization")
		return
	}
	if err != nil {
		respondLoadError(w, gridQueryErr(ctx, err))
		return
	}

	out.SubjectRendered = renderForOperator(out.Subject)
	out.SubjectProblem = subjectRenderProblem(out.Subject)
	out.PreheaderRendered = renderForOperator(out.Preheader)
	out.PreheaderProblem = subjectRenderProblem(out.Preheader)

	if len(out.HTML) > boardGridCreativeMaxBytes {
		out.HTML = out.HTML[:boardGridCreativeMaxBytes]
		out.HTMLClipped = true
	}
	respondJSON(w, http.StatusOK, out)
}

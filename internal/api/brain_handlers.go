package api

// BrainService — the platform-hosted operational memory (operator 2026-09-12:
// "It should live in the jarvis platform not on my desktop").
//
// Two mounts, one service:
//
//	/api/mailing/brain/*   session-authenticated (the portal). The signed-in
//	                       user IS the operator in this single-tenant deploy,
//	                       so verifier_role=operator is honoured here.
//	/api/admin/brain/*     X-Admin-Key gated (the Claude Code console client
//	                       and the eval worker's self-calls). An agent on this
//	                       path may verify as `checker` or `agent` only;
//	                       verifier_role=operator additionally requires the
//	                       X-Brain-Operator-Key header to equal
//	                       BRAIN_OPERATOR_KEY — a secret the operator keeps
//	                       out of agent environments. Without it, policy and
//	                       definition claims cannot be activated from the
//	                       console, by design (brain.RequiredVerifier).
//
// Handlers hold no SQL: every read/write goes through brain.Store.

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/brain"
)

type BrainService struct {
	store  *brain.Store
	runner *brain.Runner
}

func NewBrainService(store *brain.Store, runner *brain.Runner) *BrainService {
	return &BrainService{store: store, runner: runner}
}

// RegisterRoutes mounts under the /api/mailing group → /api/mailing/brain/*.
func (s *BrainService) RegisterRoutes(r chi.Router) {
	r.Route("/brain", func(br chi.Router) { s.mount(br, false) })
}

// RegisterAdminRoutes mounts on the root router → /api/admin/brain/*, closed
// by default (empty ADMIN_API_KEY ⇒ 401), same gate as the other admin routes.
func (s *BrainService) RegisterAdminRoutes(root chi.Router) {
	root.Route("/api/admin/brain", func(br chi.Router) {
		br.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				adminKey := os.Getenv("ADMIN_API_KEY")
				if adminKey == "" || req.Header.Get("X-Admin-Key") != adminKey {
					http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
					return
				}
				next.ServeHTTP(w, req)
			})
		})
		s.mount(br, true)
	})
}

func (s *BrainService) mount(r chi.Router, admin bool) {
	r.Get("/stats", s.HandleStats)
	r.Get("/recall", s.HandleRecall)
	r.Get("/recent", s.HandleRecent)
	r.Route("/claims", func(cr chi.Router) {
		cr.Post("/", s.HandleRecordClaim)
		cr.Get("/{id}", s.HandleGetClaim)
		cr.Get("/{id}/evidence", s.HandleListEvidence)
		cr.Post("/{id}/evidence", s.HandleAddEvidence)
		cr.Post("/{id}/verify", s.verifyHandler(admin))
		cr.Post("/{id}/refute", s.HandleRefute)
		cr.Post("/{id}/retract", s.HandleRetract)
		cr.Post("/{id}/supersede", s.HandleSupersede)
	})
	r.Route("/capabilities", func(cr chi.Router) {
		cr.Get("/", s.HandleListCapabilities)
		cr.Post("/", s.HandleUpsertCapability)
		cr.Get("/{name}", s.HandleResolveCapability)
	})
	r.Route("/observations", func(or chi.Router) {
		or.Get("/", s.HandleListObservations)
		or.Post("/", s.HandleUpsertObservations)
		or.Get("/days", s.HandleObservationDays)
	})
	r.Route("/evals", func(er chi.Router) {
		er.Get("/", s.HandleListEvals)
		er.Post("/", s.HandleUpsertEval)
		er.Get("/{id}", s.HandleGetEval)
		er.Get("/{id}/runs", s.HandleEvalRuns)
		er.Post("/{id}/run", s.HandleRunEval)
		er.Post("/{id}/retire", s.HandleRetireEval)
	})
}

// org resolves the organization like DripSupplyService.org (drip_supply_handlers.go:329).
func (s *BrainService) org(w http.ResponseWriter, r *http.Request) (string, bool) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil || orgID == uuid.Nil {
		respondError(w, http.StatusUnauthorized, "organization context required")
		return "", false
	}
	return orgID.String(), true
}

func brainErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, brain.ErrNotFound):
		respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, brain.ErrForbidden):
		respondError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, brain.ErrBadInput):
		respondError(w, http.StatusBadRequest, err.Error())
	default:
		respondError(w, http.StatusInternalServerError, err.Error())
	}
}

func brainPathID(r *http.Request, name string) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, name), 10, 64)
}

func brainDecode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(v)
}

func brainCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func brainActor(r *http.Request) string {
	if a := strings.TrimSpace(r.Header.Get("X-Brain-Actor")); a != "" {
		return a
	}
	return "claude"
}

// ---------------------------------------------------------------- claims

func (s *BrainService) HandleStats(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	st, err := s.store.Stats(r.Context(), org)
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, st)
}

func (s *BrainService) HandleRecall(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	out, err := s.store.Recall(r.Context(), org, q.Get("q"), brain.RecallOptions{
		ClaimType: q.Get("claim_type"), Scope: brainCSV(q.Get("scope")), Statuses: brainCSV(q.Get("status")), Limit: limit})
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"query": q.Get("q"), "claims": out,
		"note": "status=candidate rows are UNVERIFIED — cite them as such or verify first"})
}

func (s *BrainService) HandleRecent(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	days, _ := strconv.Atoi(q.Get("days"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	out, err := s.store.Recent(r.Context(), org, days, limit, q.Get("status"))
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"claims": out})
}

func (s *BrainService) HandleRecordClaim(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	var in brain.ClaimInput
	if err := brainDecode(r, &in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if in.CreatedBy == "" {
		in.CreatedBy = brainActor(r)
	}
	c, err := s.store.RecordClaim(r.Context(), org, in)
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, c)
}

func (s *BrainService) HandleGetClaim(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	id, err := brainPathID(r, "id")
	if err != nil {
		respondError(w, http.StatusBadRequest, "bad id")
		return
	}
	c, err := s.store.GetClaim(r.Context(), org, id)
	if err != nil {
		brainErr(w, err)
		return
	}
	ev, err := s.store.ListEvidence(r.Context(), org, id)
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"claim": c, "evidence": ev,
		"required_verifier": brain.RequiredVerifier(c.ClaimType)})
}

func (s *BrainService) HandleListEvidence(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	id, err := brainPathID(r, "id")
	if err != nil {
		respondError(w, http.StatusBadRequest, "bad id")
		return
	}
	ev, err := s.store.ListEvidence(r.Context(), org, id)
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"evidence": ev})
}

func (s *BrainService) HandleAddEvidence(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	id, err := brainPathID(r, "id")
	if err != nil {
		respondError(w, http.StatusBadRequest, "bad id")
		return
	}
	var in brain.EvidenceInput
	if err := brainDecode(r, &in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if in.RecordedBy == "" {
		in.RecordedBy = brainActor(r)
	}
	ev, err := s.store.AddEvidence(r.Context(), org, id, in)
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, ev)
}

type verifyRequest struct {
	VerifierRole string              `json:"verifier_role"` // operator | checker | agent
	VerifierName string              `json:"verifier_name"`
	Evidence     brain.EvidenceInput `json:"evidence"`
}

func (s *BrainService) verifyHandler(admin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		org, ok := s.org(w, r)
		if !ok {
			return
		}
		id, err := brainPathID(r, "id")
		if err != nil {
			respondError(w, http.StatusBadRequest, "bad id")
			return
		}
		var in verifyRequest
		if err := brainDecode(r, &in); err != nil {
			respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if in.VerifierName == "" {
			in.VerifierName = brainActor(r)
		}
		if admin && in.VerifierRole == "operator" {
			key := os.Getenv("BRAIN_OPERATOR_KEY")
			if key == "" || r.Header.Get("X-Brain-Operator-Key") != key {
				respondError(w, http.StatusForbidden,
					"verifier_role=operator on the admin path requires X-Brain-Operator-Key (BRAIN_OPERATOR_KEY); agents verify as checker|agent")
				return
			}
		}
		c, err := s.store.Verify(r.Context(), org, id, in.VerifierRole, in.VerifierName, in.Evidence)
		if err != nil {
			brainErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, c)
	}
}

type refuteRequest struct {
	By       string              `json:"by"`
	Evidence brain.EvidenceInput `json:"evidence"`
}

func (s *BrainService) HandleRefute(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	id, err := brainPathID(r, "id")
	if err != nil {
		respondError(w, http.StatusBadRequest, "bad id")
		return
	}
	var in refuteRequest
	if err := brainDecode(r, &in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if in.By == "" {
		in.By = brainActor(r)
	}
	c, err := s.store.Refute(r.Context(), org, id, in.By, in.Evidence)
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, c)
}

type retractRequest struct {
	Reason string `json:"reason"`
	By     string `json:"by"`
}

func (s *BrainService) HandleRetract(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	id, err := brainPathID(r, "id")
	if err != nil {
		respondError(w, http.StatusBadRequest, "bad id")
		return
	}
	var in retractRequest
	if err := brainDecode(r, &in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(in.Reason) == "" {
		respondError(w, http.StatusBadRequest, "reason is required")
		return
	}
	if in.By == "" {
		in.By = brainActor(r)
	}
	c, err := s.store.Retract(r.Context(), org, id, in.Reason, in.By)
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, c)
}

type supersedeRequest struct {
	NewID  int64  `json:"new_id"`
	Reason string `json:"reason"`
	By     string `json:"by"`
}

func (s *BrainService) HandleSupersede(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	id, err := brainPathID(r, "id")
	if err != nil {
		respondError(w, http.StatusBadRequest, "bad id")
		return
	}
	var in supersedeRequest
	if err := brainDecode(r, &in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if in.By == "" {
		in.By = brainActor(r)
	}
	c, err := s.store.Supersede(r.Context(), org, id, in.NewID, in.Reason, in.By)
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, c)
}

// ---------------------------------------------------------------- capabilities

func (s *BrainService) HandleListCapabilities(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	out, err := s.store.ListCapabilities(r.Context(), org, r.URL.Query().Get("include_retired") == "1")
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"capabilities": out})
}

func (s *BrainService) HandleUpsertCapability(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	var in brain.CapabilityInput
	if err := brainDecode(r, &in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if in.CreatedBy == "" {
		in.CreatedBy = brainActor(r)
	}
	c, err := s.store.UpsertCapability(r.Context(), org, in)
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, c)
}

func (s *BrainService) HandleResolveCapability(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	c, err := s.store.ResolveCapability(r.Context(), org, chi.URLParam(r, "name"))
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, c)
}

// ---------------------------------------------------------------- evals

// ---------------------------------------------------------------- observations

// HandleUpsertObservations writes a batch of measured values (the observer's
// daily pass). Body: {"observations": [{metric, grain, day, value, unit,
// source, meta}]}. Whole batch validated before any write.
func (s *BrainService) HandleUpsertObservations(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	var in struct {
		Observations []brain.ObservationInput `json:"observations"`
	}
	if err := brainDecode(r, &in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	n, err := s.store.UpsertObservations(r.Context(), org, in.Observations)
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, map[string]any{"written": n})
}

// HandleListObservations returns a series: ?metric=lake.delivered|lake.
// (prefix) &grain=isp:gmail &end=YYYY-MM-DD &days=35 &limit=5000.
func (s *BrainService) HandleListObservations(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	days, _ := strconv.Atoi(q.Get("days"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	out, err := s.store.ListObservations(r.Context(), org, brain.ObservationQuery{
		Metric: q.Get("metric"), Grain: q.Get("grain"), End: q.Get("end"), Days: days, Limit: limit})
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"observations": out})
}

// HandleObservationDays lists observed days for a metric prefix.
func (s *BrainService) HandleObservationDays(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out, err := s.store.ObservationDays(r.Context(), org, r.URL.Query().Get("metric"), limit)
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"days": out})
}

func (s *BrainService) HandleListEvals(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	out, err := s.store.ListEvals(r.Context(), org, r.URL.Query().Get("all") != "1")
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"evals": out})
}

func (s *BrainService) HandleUpsertEval(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	var in brain.EvalInput
	if err := brainDecode(r, &in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if in.CreatedBy == "" {
		in.CreatedBy = brainActor(r)
	}
	e, err := s.store.UpsertEval(r.Context(), org, in)
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, e)
}

func (s *BrainService) HandleGetEval(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	id, err := brainPathID(r, "id")
	if err != nil {
		respondError(w, http.StatusBadRequest, "bad id")
		return
	}
	e, err := s.store.GetEval(r.Context(), org, id)
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, e)
}

func (s *BrainService) HandleEvalRuns(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	id, err := brainPathID(r, "id")
	if err != nil {
		respondError(w, http.StatusBadRequest, "bad id")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	runs, err := s.store.ListEvalRuns(r.Context(), org, id, limit)
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *BrainService) HandleRunEval(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	id, err := brainPathID(r, "id")
	if err != nil {
		respondError(w, http.StatusBadRequest, "bad id")
		return
	}
	if s.runner == nil {
		respondError(w, http.StatusServiceUnavailable, "eval runner not configured")
		return
	}
	e, err := s.store.GetEval(r.Context(), org, id)
	if err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, s.runner.Run(r.Context(), e))
}

func (s *BrainService) HandleRetireEval(w http.ResponseWriter, r *http.Request) {
	org, ok := s.org(w, r)
	if !ok {
		return
	}
	id, err := brainPathID(r, "id")
	if err != nil {
		respondError(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.store.RetireEval(r.Context(), org, id); err != nil {
		brainErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"retired": id})
}

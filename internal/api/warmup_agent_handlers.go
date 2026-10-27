package api

// WarmupAgentService — the portal's read-only window onto the Bedrock AgentCore
// warmup agent (agents/warmup in the mailing-saas repo; operator 2026-10-26:
// "TRULY AI… allow the agent to begin mailing", 2026-10-27: "build a dashboard
// I can review within the software").
//
// The agent's whole state lives in S3 (s3://jarvis-warmup-agent-<acct>/):
//
//	state/<inst>/rates.json           per domain x ISP group per-IP rate (the agent's control variable)
//	state/<inst>/plan.json            per date x domain x group CAP (the agent's budget)
//	state/<inst>/flags.json           pause flag
//	state/<inst>/supply_requests.json queued supply requests
//	state/<inst>/jit/<date>.json      every just-in-time top-up slot it injected today
//	journal/<inst>/<date>.jsonl       every run (tick/daily), action, lesson, top-up
//	sensors/<inst>/ticks/<date>.jsonl per-tick delivered/deferred/failed per domain x group (pmta_acct_raw)
//	knowledge/field_manual.md         the agent's own lessons
//	reports/<inst>/<date>.md          the agent's daily operator report
//
// This handler DISPLAYS; it computes nothing. It bundles the raw objects for
// one instance x Denver date and the React tab derives every number, so the
// portal and the agent read the same bytes. Session-authenticated path
// (/api/mailing/warmup-agent/*); S3 read via the ECS task role.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/go-chi/chi/v5"
)

type WarmupAgentService struct {
	s3           *s3.Client
	bucket       string // the warmup agent's own bucket
	agentsBucket string // jarvis-agents ECS state mirror (scheduling + data agents, change ledger)
	agentsPrefix string
	err          error
}

var warmupInstRe = regexp.MustCompile(`^[a-z0-9]{2,16}$`)
var warmupDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func NewWarmupAgentService() *WarmupAgentService {
	bucket := os.Getenv("WARMUP_AGENT_BUCKET")
	if bucket == "" {
		bucket = "jarvis-warmup-agent-146361001621"
	}
	region := os.Getenv("WARMUP_AGENT_REGION")
	if region == "" {
		region = "us-west-2"
	}
	agentsBucket, agentsPrefix := "ignite-pmta-engine", "jarvis-agents/state/agents/jobs/"
	if v := os.Getenv("JARVIS_STATE_S3"); strings.HasPrefix(v, "s3://") { // same env the ECS entrypoint uses
		if i := strings.Index(v[5:], "/"); i > 0 {
			agentsBucket, agentsPrefix = v[5:5+i], strings.TrimSuffix(v[5+i+1:], "/")+"/agents/jobs/"
		}
	}
	svc := &WarmupAgentService{bucket: bucket, agentsBucket: agentsBucket, agentsPrefix: agentsPrefix}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(region))
	if err != nil {
		svc.err = err
		return svc
	}
	svc.s3 = s3.NewFromConfig(cfg)
	return svc
}

func (s *WarmupAgentService) RegisterRoutes(r chi.Router) {
	r.Route("/warmup-agent", func(wr chi.Router) {
		wr.Get("/overview", s.handleOverview)
	})
	r.Route("/agents", func(ar chi.Router) {
		ar.Get("/overview", s.handleAgentsOverview)
	})
}

// agentsOverview — the scheduling agent (plan 21:00 MT + supervisor), the data
// agent (30-min passes) and the change ledger for one Denver date, raw.
//
//	scheduling_agent/<D-1>.json   the plan night that BUILT date D (journal keys: volume, rules, scorer, readiness, needs_you …)
//	scheduling_agent/<D>.json     tonight's plan (builds D+1), once it has run
//	scheduling_supervisor.json    per-date per-program attempts / outcome / paged
//	data_agent/<D>.json           {passes:[…], latest} — every 30-min pass with its 12 checks and repairs
//	data_agent/state.json         last_pass, last_verdicts, days[D] (attempts, paged, repairs), daily caches
//	ledger/change_ledger.jsonl    filtered to ids cl-<YYYYMMDD>-* (every LIVE change any agent or session made that day)
type agentsOverview struct {
	Date      string `json:"date"`
	FetchedAt string `json:"fetched_at"`
	Scheduling struct {
		PlanForDate json.RawMessage `json:"plan_for_date"`
		PlanTonight json.RawMessage `json:"plan_tonight"`
		Supervisor  json.RawMessage `json:"supervisor"`
	} `json:"scheduling"`
	Data struct {
		Journal json.RawMessage `json:"journal"`
		State   json.RawMessage `json:"state"`
	} `json:"data"`
	Ledger []json.RawMessage `json:"ledger"`
	Errors map[string]string `json:"errors,omitempty"`
}

func (s *WarmupAgentService) handleAgentsOverview(w http.ResponseWriter, r *http.Request) {
	if s.err != nil || s.s3 == nil {
		writeJSONError(w, "agents S3 client unavailable: "+warmupErrString(s.err), http.StatusServiceUnavailable)
		return
	}
	date := r.URL.Query().Get("date")
	if date == "" {
		date = denverDate(time.Now())
	}
	if !warmupDateRe.MatchString(date) {
		writeJSONError(w, "date must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	day, err := time.Parse("2006-01-02", date)
	if err != nil {
		writeJSONError(w, "date must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	prev := day.AddDate(0, 0, -1).Format("2006-01-02")
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	out := agentsOverview{Date: date, FetchedAt: time.Now().UTC().Format(time.RFC3339), Errors: map[string]string{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	fail := func(k string, err error) { mu.Lock(); defer mu.Unlock(); out.Errors[k] = err.Error() }
	getJSON := func(k, key string, dst *json.RawMessage) {
		defer wg.Done()
		b, err := s.getFrom(ctx, s.agentsBucket, s.agentsPrefix+key)
		if err != nil {
			if !isS3NotFound(err) {
				fail(k, err)
			}
			return
		}
		if !json.Valid(b) {
			fail(k, errors.New("object is not valid JSON"))
			return
		}
		mu.Lock()
		*dst = json.RawMessage(b)
		mu.Unlock()
	}
	wg.Add(6)
	go getJSON("scheduling.plan_for_date", "state/scheduling_agent/"+prev+".json", &out.Scheduling.PlanForDate)
	go getJSON("scheduling.plan_tonight", "state/scheduling_agent/"+date+".json", &out.Scheduling.PlanTonight)
	go getJSON("scheduling.supervisor", "state/scheduling_supervisor.json", &out.Scheduling.Supervisor)
	go getJSON("data.journal", "state/data_agent/"+date+".json", &out.Data.Journal)
	go getJSON("data.state", "state/data_agent/state.json", &out.Data.State)
	go func() {
		defer wg.Done()
		rows, err := s.ledgerForDate(ctx, strings.ReplaceAll(date, "-", ""))
		if err != nil {
			fail("ledger", err)
			return
		}
		mu.Lock()
		out.Ledger = rows
		mu.Unlock()
	}()
	wg.Wait()
	if out.Ledger == nil {
		out.Ledger = []json.RawMessage{}
	}
	if len(out.Errors) == 0 {
		out.Errors = nil
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// ledgerForDate streams the change ledger (≈10 MB, append-only JSONL) and keeps
// the day's rows by id prefix "cl-YYYYMMDD" — a filter, not a computation.
func (s *WarmupAgentService) ledgerForDate(ctx context.Context, yyyymmdd string) ([]json.RawMessage, error) {
	key := s.agentsPrefix + "ledger/change_ledger.jsonl"
	res, err := s.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.agentsBucket, Key: &key})
	if err != nil {
		if isS3NotFound(err) {
			return []json.RawMessage{}, nil
		}
		return nil, err
	}
	defer res.Body.Close()
	want := []byte(`"id": "cl-` + yyyymmdd)
	want2 := []byte(`"id":"cl-` + yyyymmdd)
	rows := []json.RawMessage{}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line[:min(len(line), 48)]), string(want)) && !strings.Contains(string(line[:min(len(line), 48)]), string(want2)) {
			continue
		}
		if !json.Valid(line) {
			continue
		}
		rows = append(rows, json.RawMessage(append([]byte(nil), line...)))
	}
	return rows, sc.Err()
}

// warmupOverview is the raw bundle. Every field is the S3 object as-is (JSON
// passthrough, JSONL as an array of objects, markdown as text); a missing
// object is null — "no rows", never a zero.
type warmupOverview struct {
	Instance       string            `json:"instance"`
	Date           string            `json:"date"`
	Bucket         string            `json:"bucket"`
	FetchedAt      string            `json:"fetched_at"`
	Rates          json.RawMessage   `json:"rates"`
	Plan           json.RawMessage   `json:"plan"`
	Flags          json.RawMessage   `json:"flags"`
	SupplyRequests json.RawMessage   `json:"supply_requests"`
	Jit            json.RawMessage   `json:"jit"`
	BoxLast        json.RawMessage   `json:"box_last"`
	Journal        []json.RawMessage `json:"journal"`
	Ticks          []json.RawMessage `json:"ticks"`
	FieldManual    *string           `json:"field_manual"`
	Report         *string           `json:"report"`
	Errors         map[string]string `json:"errors,omitempty"`
}

func (s *WarmupAgentService) handleOverview(w http.ResponseWriter, r *http.Request) {
	if s.err != nil || s.s3 == nil {
		writeJSONError(w, "warmup agent S3 client unavailable: "+warmupErrString(s.err), http.StatusServiceUnavailable)
		return
	}
	inst := r.URL.Query().Get("instance")
	if inst == "" {
		inst = "colo2"
	}
	date := r.URL.Query().Get("date")
	if date == "" {
		date = denverDate(time.Now())
	}
	if !warmupInstRe.MatchString(inst) || !warmupDateRe.MatchString(date) {
		writeJSONError(w, "instance must be [a-z0-9]{2,16}; date must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	out := warmupOverview{Instance: inst, Date: date, Bucket: s.bucket, FetchedAt: time.Now().UTC().Format(time.RFC3339), Errors: map[string]string{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	fail := func(k string, err error) {
		mu.Lock()
		defer mu.Unlock()
		out.Errors[k] = err.Error()
	}
	getJSON := func(k, key string, dst *json.RawMessage) {
		defer wg.Done()
		b, err := s.get(ctx, key)
		if err != nil {
			if !isS3NotFound(err) {
				fail(k, err)
			}
			return
		}
		if !json.Valid(b) {
			fail(k, errors.New("object is not valid JSON"))
			return
		}
		mu.Lock()
		*dst = json.RawMessage(b)
		mu.Unlock()
	}
	getLines := func(k, key string, dst *[]json.RawMessage) {
		defer wg.Done()
		b, err := s.get(ctx, key)
		if err != nil {
			if !isS3NotFound(err) {
				fail(k, err)
			}
			return
		}
		rows := make([]json.RawMessage, 0, 256)
		sc := bufio.NewScanner(strings.NewReader(string(b)))
		sc.Buffer(make([]byte, 0, 1<<20), 8<<20) // a tick's final text can run to ~4k; be generous
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || !json.Valid([]byte(line)) {
				continue
			}
			rows = append(rows, json.RawMessage(line))
		}
		mu.Lock()
		*dst = rows
		mu.Unlock()
	}
	getText := func(k, key string, dst **string) {
		defer wg.Done()
		b, err := s.get(ctx, key)
		if err != nil {
			if !isS3NotFound(err) {
				fail(k, err)
			}
			return
		}
		t := string(b)
		mu.Lock()
		*dst = &t
		mu.Unlock()
	}

	wg.Add(10)
	go getJSON("rates", "state/"+inst+"/rates.json", &out.Rates)
	go getJSON("plan", "state/"+inst+"/plan.json", &out.Plan)
	go getJSON("flags", "state/"+inst+"/flags.json", &out.Flags)
	go getJSON("supply_requests", "state/"+inst+"/supply_requests.json", &out.SupplyRequests)
	go getJSON("jit", "state/"+inst+"/jit/"+date+".json", &out.Jit)
	go getJSON("box_last", "sensors/"+inst+"/box_last.json", &out.BoxLast)
	go getLines("journal", "journal/"+inst+"/"+date+".jsonl", &out.Journal)
	go getLines("ticks", "sensors/"+inst+"/ticks/"+date+".jsonl", &out.Ticks)
	go getText("field_manual", "knowledge/field_manual.md", &out.FieldManual)
	go getText("report", "reports/"+inst+"/"+date+".md", &out.Report)
	wg.Wait()

	if len(out.Errors) == 0 {
		out.Errors = nil
	}
	if out.Journal == nil {
		out.Journal = []json.RawMessage{}
	}
	if out.Ticks == nil {
		out.Ticks = []json.RawMessage{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

func (s *WarmupAgentService) get(ctx context.Context, key string) ([]byte, error) {
	return s.getFrom(ctx, s.bucket, key)
}

func (s *WarmupAgentService) getFrom(ctx context.Context, bucket, key string) ([]byte, error) {
	res, err := s.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &key})
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	return io.ReadAll(io.LimitReader(res.Body, 16<<20))
}

func isS3NotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	return strings.Contains(err.Error(), "NoSuchKey") || strings.Contains(err.Error(), "NotFound")
}

func warmupErrString(err error) string {
	if err == nil {
		return "not initialised"
	}
	return err.Error()
}

// denverDate — the agent's days are Denver days (agents/warmup/state.denver_date).
func denverDate(t time.Time) string {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		return t.UTC().Format("2006-01-02")
	}
	return t.In(loc).Format("2006-01-02")
}

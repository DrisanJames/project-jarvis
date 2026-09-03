package dripsupply_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/pkg/contractmeta"
	"github.com/ignite/sparkpost-monitor/internal/worker"
	"github.com/ignite/sparkpost-monitor/internal/worker/dripsupply"
)

// testKey is a fixed HMAC key. It is >= contractmeta.MinKeyLen so KeyFromEnv
// would accept it; the tests inject it directly rather than touching the
// process environment.
var testKey = []byte("test-contract-token-key-0123456789abcdef")

var issuedAt = time.Date(2026, 9, 3, 6, 0, 0, 0, time.UTC)

// issueFor computes the token a correctly-scheduled row would carry.
func issueFor(c dripsupply.Contract, version int) contractmeta.Token {
	canon := contractmeta.Canonical(c.TokenBody(), string(c.Kind()), c.Subject(), version)
	return contractmeta.Issue(testKey, canon, issuedAt)
}

// blockFor builds the metadata JSON an issued row carries.
func blockFor(t *testing.T, id uuid.UUID, c dripsupply.Contract, version int) ([]byte, contractmeta.Token) {
	t.Helper()
	tok := issueFor(c, version)
	blk := contractmeta.Block{
		Refs:     contractmeta.Refs{SendingDomainID: "11111111-1111-1111-1111-111111111111"},
		Mutation: contractmeta.Mutation{At: issuedAt, By: "operator", ChangeLedgerID: "chg-0001", PriorVersion: version - 1},
		Token:    tok,
	}
	blk.StampIdentity(id.String(), string(c.Kind()), version)
	raw, err := json.Marshal(blk)
	if err != nil {
		t.Fatalf("marshal block: %v", err)
	}
	return raw, tok
}

// ---------------------------------------------------------------------------
// ONE SOURCE for the ISP class list
// ---------------------------------------------------------------------------

// The contract validator's 12 ISP classes must be EXACTLY the keys of
// worker.DefaultPerISPCapPerWave(). dripsupply cannot import internal/worker in
// production code (internal/worker imports dripsupply — §2), so this external
// test package is where the two are pinned together. If the orchestrator's map
// gains or loses an ISP, this fails and dripsupply.ispClasses must follow.
func TestISPClassesMatchOrchestratorDefaults(t *testing.T) {
	want := make([]string, 0, 12)
	for isp := range worker.DefaultPerISPCapPerWave() {
		want = append(want, isp)
	}
	sort.Strings(want)

	got := dripsupply.ISPClasses()
	sort.Strings(got)

	if len(got) != 12 {
		t.Fatalf("dripsupply.ISPClasses() has %d classes, want 12: %v", len(got), got)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ISP class drift:\n dripsupply: %v\n orchestrator: %v", got, want)
	}
	// Negative control: the parity check must be able to fail.
	if dripsupply.IsISPClass("not-an-isp") {
		t.Fatal("IsISPClass accepted a bogus class")
	}
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

func fullISPMap(v int) map[string]int {
	m := map[string]int{}
	for _, isp := range dripsupply.ISPClasses() {
		m[isp] = v
	}
	m["gmail"] = 0 // gmail is held at 0 unless notes name the ruling
	return m
}

func validDomain() *dripsupply.DomainContract {
	return &dripsupply.DomainContract{
		SendingDomain:     "em.historythinking.com",
		BrandCode:         "HT",
		DailyMaxByISP:     fullISPMap(1000),
		ActiveWindowStart: "01:00",
		ActiveWindowEnd:   "20:00",
		IntervalMinutes:   15,
		MaxBurstIntervals: 2,
		RampSource:        dripsupply.RampSourceCards,
		HealthBand:        dripsupply.HealthBandGreen,
		RampStage:         "mature",
	}
}

func validDispatch() *dripsupply.DispatchContract {
	return &dripsupply.DispatchContract{
		Lane:                 "wcl_remail",
		OperatorPriorityTier: 2,
		DesiredDailyIntros:   map[string]int{"aol": 5500, "yahoo": 12000},
		DemandMode:           dripsupply.DemandModeTarget,
		AllowedDomains:       []string{"HT", "DB"},
		ISPExclusions:        []string{"gmail"},
		LadderTouches:        5,
		LadderGapHours:       24,
		FollowupsCommitted:   true,
		MaxIntroShare:        0.40,
		ExplorationShare:     0,
	}
}

func validInventory() *dripsupply.InventoryContract {
	return &dripsupply.InventoryContract{
		Lane:                "wcl_remail",
		AcceptedSources:     []string{"wcl_abandon"},
		VerdictValidDays:    60,
		EOEnabled:           true,
		MaxDailyEOSpendUSD:  50,
		MinEOOrder:          1000,
		MinCoverageHours:    8,
		TargetCoverageHours: 16,
		MaxCoverageHours:    36,
		RemailEnabled:       false,
		RemailAfterDays:     7,
		RemailMode:          dripsupply.RemailModeFullLadder,
		MaxRemailShare:      0.25,
	}
}

func validSource() *dripsupply.SourceContract {
	return &dripsupply.SourceContract{
		SourceSlug:          "wcl_abandon",
		RecordClass:         "mortgage",
		EligibleISPs:        []string{"aol", "yahoo"},
		ArrivalCadence:      "continuous",
		ValidatedOnArrival:  false,
		UnitAcquisitionCost: 0,
	}
}

func intp(v int) *int { return &v }

// wantFields asserts Validate failed on exactly the expected field.
func wantFieldErr(t *testing.T, err error, field string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a validation error on %q, got nil", field)
	}
	var verrs dripsupply.ValidationErrors
	if !errors.As(err, &verrs) {
		t.Fatalf("expected ValidationErrors, got %T: %v", err, err)
	}
	if !verrs.HasField(field) {
		t.Fatalf("expected a failure on field %q, got %v (%v)", field, verrs.Fields(), verrs)
	}
}

// ---------------------------------------------------------------------------
// Validate — domain
// ---------------------------------------------------------------------------

func TestDomainContractValidate(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*dripsupply.DomainContract)
		field string // "" = must pass
	}{
		{name: "valid"},
		{name: "gmail>0 with notes is allowed", mut: func(c *dripsupply.DomainContract) {
			c.DailyMaxByISP["gmail"] = 400
			c.Notes = "operator ruling 2026-08-30: HT exempt from the 8-brand gmail ban"
		}},
		{name: "gmail>0 without notes", field: "daily_max_by_isp", mut: func(c *dripsupply.DomainContract) {
			c.DailyMaxByISP["gmail"] = 400
			c.Notes = "   "
		}},
		{name: "missing an ISP class", field: "daily_max_by_isp", mut: func(c *dripsupply.DomainContract) {
			delete(c.DailyMaxByISP, "sbcglobal")
		}},
		{name: "nil map", field: "daily_max_by_isp", mut: func(c *dripsupply.DomainContract) {
			c.DailyMaxByISP = nil
		}},
		{name: "unknown ISP key", field: "daily_max_by_isp", mut: func(c *dripsupply.DomainContract) {
			c.DailyMaxByISP["kumo"] = 10
		}},
		{name: "negative value", field: "daily_max_by_isp", mut: func(c *dripsupply.DomainContract) {
			c.DailyMaxByISP["aol"] = -1
		}},
		{name: "zero values are legal", mut: func(c *dripsupply.DomainContract) {
			c.DailyMaxByISP = fullISPMap(0)
		}},
		{name: "empty sending_domain", field: "sending_domain", mut: func(c *dripsupply.DomainContract) {
			c.SendingDomain = ""
		}},
		{name: "empty brand_code", field: "brand_code", mut: func(c *dripsupply.DomainContract) {
			c.BrandCode = " "
		}},
		{name: "window end before start", field: "active_window_end", mut: func(c *dripsupply.DomainContract) {
			c.ActiveWindowStart = "20:00"
			c.ActiveWindowEnd = "01:00"
		}},
		{name: "window end equals start", field: "active_window_end", mut: func(c *dripsupply.DomainContract) {
			c.ActiveWindowEnd = "01:00"
		}},
		{name: "unparseable clock", field: "active_window_start", mut: func(c *dripsupply.DomainContract) {
			c.ActiveWindowStart = "0100"
		}},
		{name: "HH:MM:SS accepted", mut: func(c *dripsupply.DomainContract) {
			c.ActiveWindowStart = "01:00:00"
			c.ActiveWindowEnd = "20:00:00"
		}},
		{name: "zero interval", field: "interval_minutes", mut: func(c *dripsupply.DomainContract) {
			c.IntervalMinutes = 0
		}},
		{name: "zero burst", field: "max_burst_intervals", mut: func(c *dripsupply.DomainContract) {
			c.MaxBurstIntervals = 0
		}},
		{name: "unknown ramp_source", field: "ramp_source", mut: func(c *dripsupply.DomainContract) {
			c.RampSource = "guessed"
		}},
		{name: "empty ramp_source is allowed", mut: func(c *dripsupply.DomainContract) {
			c.RampSource = ""
		}},
		{name: "unknown status", field: "status", mut: func(c *dripsupply.DomainContract) {
			c.Status = dripsupply.ContractStatus("live")
		}},

		// health_band is POLICY (operator ruling 2026-09-03).
		{name: "empty health_band resolves to green", mut: func(c *dripsupply.DomainContract) {
			c.HealthBand = ""
			c.Notes = "" // green needs no ruling note
		}},
		{name: "green needs no note", mut: func(c *dripsupply.DomainContract) {
			c.HealthBand = dripsupply.HealthBandGreen
			c.Notes = ""
		}},
		{name: "amber without notes", field: "health_band", mut: func(c *dripsupply.DomainContract) {
			c.HealthBand = dripsupply.HealthBandAmber
			c.Notes = "   "
		}},
		{name: "amber with notes", mut: func(c *dripsupply.DomainContract) {
			c.HealthBand = dripsupply.HealthBandAmber
			c.Notes = "operator ruling 2026-09-03: HT amber pending Microsoft recovery"
		}},
		{name: "red without notes", field: "health_band", mut: func(c *dripsupply.DomainContract) {
			c.HealthBand = dripsupply.HealthBandRed
			c.Notes = ""
		}},
		{name: "red with notes", mut: func(c *dripsupply.DomainContract) {
			c.HealthBand = dripsupply.HealthBandRed
			c.Notes = "operator ruling 2026-09-03: HT held at red after the 5.7.1 block"
		}},
		{name: "unknown band", field: "health_band", mut: func(c *dripsupply.DomainContract) {
			c.HealthBand = "yellow"
			c.Notes = "a note does not make up a band"
		}},
		{name: "band is case sensitive", field: "health_band", mut: func(c *dripsupply.DomainContract) {
			c.HealthBand = "RED"
			c.Notes = "operator ruling 2026-09-03"
		}},
		{name: "ramp_stage is free text", mut: func(c *dripsupply.DomainContract) {
			c.RampStage = "week 3 / step 4"
		}},
		{name: "empty ramp_stage is allowed", mut: func(c *dripsupply.DomainContract) {
			c.RampStage = ""
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validDomain()
			if tc.mut != nil {
				tc.mut(c)
			}
			err := c.Validate()
			if tc.field == "" {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			wantFieldErr(t, err, tc.field)
		})
	}
}

// ---------------------------------------------------------------------------
// Validate — dispatch
// ---------------------------------------------------------------------------

func TestDispatchContractValidate(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*dripsupply.DispatchContract)
		field string
	}{
		{name: "valid"},
		{name: "empty lane", field: "lane", mut: func(c *dripsupply.DispatchContract) { c.Lane = "" }},
		{name: "empty allowed_domains", field: "allowed_domains", mut: func(c *dripsupply.DispatchContract) {
			c.AllowedDomains = nil
		}},
		{name: "duplicate allowed_domains", field: "allowed_domains", mut: func(c *dripsupply.DispatchContract) {
			c.AllowedDomains = []string{"HT", "HT"}
		}},
		{name: "desired key not an ISP class", field: "desired_daily_intros", mut: func(c *dripsupply.DispatchContract) {
			c.DesiredDailyIntros = map[string]int{"yahoo!": 100}
		}},
		{name: "negative desired", field: "desired_daily_intros", mut: func(c *dripsupply.DispatchContract) {
			c.DesiredDailyIntros = map[string]int{"aol": -1}
		}},
		{name: "nil desired map", field: "desired_daily_intros", mut: func(c *dripsupply.DispatchContract) {
			c.DesiredDailyIntros = nil
		}},
		{name: "empty desired map is allowed", mut: func(c *dripsupply.DispatchContract) {
			c.DesiredDailyIntros = map[string]int{}
		}},
		{name: "unknown demand_mode", field: "demand_mode", mut: func(c *dripsupply.DispatchContract) {
			c.DemandMode = "drain"
		}},
		{name: "consume_available without ceiling", field: "daily_ceiling", mut: func(c *dripsupply.DispatchContract) {
			c.DemandMode = dripsupply.DemandModeConsumeAvailable
			c.DailyCeiling = nil
		}},
		{name: "consume_available with zero ceiling", field: "daily_ceiling", mut: func(c *dripsupply.DispatchContract) {
			c.DemandMode = dripsupply.DemandModeConsumeAvailable
			c.DailyCeiling = intp(0)
		}},
		{name: "consume_available with ceiling", mut: func(c *dripsupply.DispatchContract) {
			c.DemandMode = dripsupply.DemandModeConsumeAvailable
			c.DailyCeiling = intp(200000)
		}},
		{name: "max_intro_share zero", field: "max_intro_share", mut: func(c *dripsupply.DispatchContract) {
			c.MaxIntroShare = 0
		}},
		{name: "max_intro_share above 1", field: "max_intro_share", mut: func(c *dripsupply.DispatchContract) {
			c.MaxIntroShare = 1.01
		}},
		{name: "max_intro_share exactly 1", mut: func(c *dripsupply.DispatchContract) {
			c.MaxIntroShare = 1
		}},
		{name: "exploration_share 1", field: "exploration_share", mut: func(c *dripsupply.DispatchContract) {
			c.ExplorationShare = 1
		}},
		{name: "exploration_share negative", field: "exploration_share", mut: func(c *dripsupply.DispatchContract) {
			c.ExplorationShare = -0.01
		}},
		{name: "exploration_share 0.05", mut: func(c *dripsupply.DispatchContract) {
			c.ExplorationShare = 0.05
			c.OperatorPriorityTier = dripsupply.ExplorationTier
		}},
		{name: "bad tier", field: "operator_priority_tier", mut: func(c *dripsupply.DispatchContract) {
			c.OperatorPriorityTier = 4
		}},
		{name: "isp_exclusions not an ISP class", field: "isp_exclusions", mut: func(c *dripsupply.DispatchContract) {
			c.ISPExclusions = []string{"gmail.com"}
		}},
		{name: "zero ladder_touches", field: "ladder_touches", mut: func(c *dripsupply.DispatchContract) {
			c.LadderTouches = 0
		}},
		{name: "zero ladder_gap_hours", field: "ladder_gap_hours", mut: func(c *dripsupply.DispatchContract) {
			c.LadderGapHours = 0
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validDispatch()
			if tc.mut != nil {
				tc.mut(c)
			}
			err := c.Validate()
			if tc.field == "" {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			wantFieldErr(t, err, tc.field)
		})
	}
}

// ---------------------------------------------------------------------------
// Validate — inventory
// ---------------------------------------------------------------------------

func TestInventoryContractValidate(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*dripsupply.InventoryContract)
		field string
	}{
		{name: "valid"},
		{name: "empty lane", field: "lane", mut: func(c *dripsupply.InventoryContract) { c.Lane = "" }},
		{name: "empty accepted_sources", field: "accepted_sources", mut: func(c *dripsupply.InventoryContract) {
			c.AcceptedSources = nil
		}},
		{name: "duplicate accepted_sources", field: "accepted_sources", mut: func(c *dripsupply.InventoryContract) {
			c.AcceptedSources = []string{"a", "a"}
		}},
		{name: "min above target", field: "target_coverage_hours", mut: func(c *dripsupply.InventoryContract) {
			c.MinCoverageHours = 20
		}},
		{name: "target above max", field: "max_coverage_hours", mut: func(c *dripsupply.InventoryContract) {
			c.TargetCoverageHours = 40
		}},
		{name: "min == target == max", mut: func(c *dripsupply.InventoryContract) {
			c.MinCoverageHours, c.TargetCoverageHours, c.MaxCoverageHours = 12, 12, 12
		}},
		{name: "negative coverage", field: "min_coverage_hours", mut: func(c *dripsupply.InventoryContract) {
			c.MinCoverageHours = -1
		}},
		{name: "unknown remail_mode", field: "remail_mode", mut: func(c *dripsupply.InventoryContract) {
			c.RemailMode = "resend"
		}},
		{name: "single_touch remail_mode", mut: func(c *dripsupply.InventoryContract) {
			c.RemailMode = dripsupply.RemailModeSingleTouch
		}},
		{name: "max_remail_share negative", field: "max_remail_share", mut: func(c *dripsupply.InventoryContract) {
			c.MaxRemailShare = -0.01
		}},
		{name: "max_remail_share above 1", field: "max_remail_share", mut: func(c *dripsupply.InventoryContract) {
			c.MaxRemailShare = 1.01
		}},
		{name: "max_remail_share 0", mut: func(c *dripsupply.InventoryContract) { c.MaxRemailShare = 0 }},
		{name: "max_remail_share 1", mut: func(c *dripsupply.InventoryContract) { c.MaxRemailShare = 1 }},
		{name: "zero verdict_valid_days", field: "verdict_valid_days", mut: func(c *dripsupply.InventoryContract) {
			c.VerdictValidDays = 0
		}},
		{name: "negative eo spend", field: "max_daily_eo_spend_usd", mut: func(c *dripsupply.InventoryContract) {
			c.MaxDailyEOSpendUSD = -1
		}},
		{name: "negative min_eo_order", field: "min_eo_order", mut: func(c *dripsupply.InventoryContract) {
			c.MinEOOrder = -1
		}},
		{name: "negative remail_after_days", field: "remail_after_days", mut: func(c *dripsupply.InventoryContract) {
			c.RemailAfterDays = -1
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validInventory()
			if tc.mut != nil {
				tc.mut(c)
			}
			err := c.Validate()
			if tc.field == "" {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			wantFieldErr(t, err, tc.field)
		})
	}
}

// ---------------------------------------------------------------------------
// Validate — source
// ---------------------------------------------------------------------------

func TestSourceContractValidate(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*dripsupply.SourceContract)
		field string
	}{
		{name: "valid"},
		{name: "empty slug", field: "source_slug", mut: func(c *dripsupply.SourceContract) { c.SourceSlug = "" }},
		{name: "empty record_class", field: "record_class", mut: func(c *dripsupply.SourceContract) {
			c.RecordClass = " "
		}},
		{name: "empty eligible_isps", field: "eligible_isps", mut: func(c *dripsupply.SourceContract) {
			c.EligibleISPs = nil
		}},
		{name: "eligible_isps not a subset", field: "eligible_isps", mut: func(c *dripsupply.SourceContract) {
			c.EligibleISPs = []string{"aol", "frontier"}
		}},
		{name: "duplicate eligible_isps", field: "eligible_isps", mut: func(c *dripsupply.SourceContract) {
			c.EligibleISPs = []string{"aol", "aol"}
		}},
		{name: "all 12 classes eligible", mut: func(c *dripsupply.SourceContract) {
			c.EligibleISPs = dripsupply.ISPClasses()
		}},
		{name: "empty arrival_cadence", field: "arrival_cadence", mut: func(c *dripsupply.SourceContract) {
			c.ArrivalCadence = ""
		}},
		{name: "negative max_daily_intake", field: "max_daily_intake", mut: func(c *dripsupply.SourceContract) {
			c.MaxDailyIntake = intp(-1)
		}},
		{name: "nil max_daily_intake is allowed", mut: func(c *dripsupply.SourceContract) {
			c.MaxDailyIntake = nil
		}},
		{name: "negative record_max_age_days", field: "record_max_age_days", mut: func(c *dripsupply.SourceContract) {
			c.RecordMaxAgeDays = intp(-5)
		}},
		{name: "negative acquisition cost", field: "unit_acquisition_cost", mut: func(c *dripsupply.SourceContract) {
			c.UnitAcquisitionCost = -0.01
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validSource()
			if tc.mut != nil {
				tc.mut(c)
			}
			err := c.Validate()
			if tc.field == "" {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			wantFieldErr(t, err, tc.field)
		})
	}
}

// ---------------------------------------------------------------------------
// The partial unique index that makes two actives impossible
// ---------------------------------------------------------------------------

// §1.1: "Exactly one row per subject may be active at a time (partial unique
// index on (subject) WHERE status='active')". ActivateScheduled relies on that
// index existing under these exact names, so the DDL and the reliance come from
// one place. WP1's schema must emit ActiveIndexDDL verbatim.
func TestActiveIndexDDLIsWhatActivationReliesOn(t *testing.T) {
	want := map[dripsupply.ContractKind][3]string{
		dripsupply.KindDomain:    {dripsupply.ActiveIndexDomain, "drip_domain_contracts", "sending_domain"},
		dripsupply.KindDispatch:  {dripsupply.ActiveIndexDispatch, "drip_dispatch_contracts", "lane"},
		dripsupply.KindInventory: {dripsupply.ActiveIndexInventory, "drip_inventory_contracts", "lane"},
		dripsupply.KindSource:    {dripsupply.ActiveIndexSource, "drip_source_contracts", "source_slug"},
	}
	if len(dripsupply.AllKinds()) != len(want) {
		t.Fatalf("AllKinds()=%v, want %d kinds", dripsupply.AllKinds(), len(want))
	}
	for _, kind := range dripsupply.AllKinds() {
		exp, ok := want[kind]
		if !ok {
			t.Fatalf("unexpected kind %q", kind)
		}
		name, err := dripsupply.ActiveIndexName(kind)
		if err != nil {
			t.Fatalf("ActiveIndexName(%s): %v", kind, err)
		}
		if name != exp[0] {
			t.Fatalf("ActiveIndexName(%s)=%q, want %q", kind, name, exp[0])
		}
		table, err := dripsupply.TableFor(kind)
		if err != nil || table != exp[1] {
			t.Fatalf("TableFor(%s)=%q,%v want %q", kind, table, err, exp[1])
		}
		subj, err := dripsupply.SubjectColumnFor(kind)
		if err != nil || subj != exp[2] {
			t.Fatalf("SubjectColumnFor(%s)=%q,%v want %q", kind, subj, err, exp[2])
		}
		ddl, err := dripsupply.ActiveIndexDDL(kind)
		if err != nil {
			t.Fatalf("ActiveIndexDDL(%s): %v", kind, err)
		}
		for _, frag := range []string{
			"CREATE UNIQUE INDEX IF NOT EXISTS " + exp[0],
			"ON " + exp[1],
			"(" + exp[2] + ")",
			"WHERE status = 'active'",
		} {
			if !strings.Contains(ddl, frag) {
				t.Fatalf("ActiveIndexDDL(%s)=%q missing %q", kind, ddl, frag)
			}
		}
	}
	// Negative control: an unknown kind has no index and must error.
	if _, err := dripsupply.ActiveIndexName(dripsupply.ContractKind("kumo")); err == nil {
		t.Fatal("ActiveIndexName accepted an unknown kind")
	}
}

// ---------------------------------------------------------------------------
// LoadActive
// ---------------------------------------------------------------------------

func metaRow(id uuid.UUID, effective time.Time, version int, metadata []byte, token string) []driver.Value {
	return []driver.Value{
		id.String(), version, "active", effective, nil,
		"operator", effective, "operator", effective,
		"chg-0001", "operator ruling 2026-09-03", metadata, token,
	}
}

// signedRows builds the four active-contract result sets from real contracts,
// each carrying the token a correct Schedule would have issued. Returning the
// contracts too lets a test tamper with one row's BODY while leaving its token
// untouched, which is the hand-edit LoadActive must refuse.
type activeFixture struct {
	dom  *dripsupply.DomainContract
	disp *dripsupply.DispatchContract
	inv  *dripsupply.InventoryContract
	src  *dripsupply.SourceContract
	id   uuid.UUID
	eff  time.Time
}

func newActiveFixture(eff time.Time) *activeFixture {
	f := &activeFixture{id: uuid.New(), eff: eff}
	f.dom = validDomain()
	f.dom.DailyMaxByISP = map[string]int{
		"aol": 4900, "apple": 0, "att": 0, "charter": 0, "comcast": 0, "cox": 0,
		"gmail": 0, "microsoft": 0, "other": 0, "sbcglobal": 0, "verizon": 0, "yahoo": 17000,
	}
	f.dom.Version = 3

	f.disp = validDispatch()
	f.disp.OperatorPriorityTier = 1
	f.disp.DesiredDailyIntros = map[string]int{"aol": 5500}
	f.disp.DemandMode = dripsupply.DemandModeConsumeAvailable
	f.disp.DailyCeiling = intp(208000)
	f.disp.Version = 3

	f.inv = validInventory()
	f.inv.Version = 3

	f.src = validSource()
	f.src.RecordMaxAgeDays = intp(90)
	f.src.Version = 3
	return f
}

// expect queues the four LoadActive queries. domainMaxOverride, when non-nil,
// is written into the ROW without re-signing — a tampered body.
func (f *activeFixture) expect(t *testing.T, mock sqlmock.Sqlmock, dayEnd time.Time, domainMaxOverride map[string]int, tokenColOverride *string) {
	f.expectWithBand(t, mock, dayEnd, domainMaxOverride, tokenColOverride, "")
}

// expectWithBand additionally allows the row's health_band to be written
// WITHOUT re-signing — the hand-edited-band case.
func (f *activeFixture) expectWithBand(t *testing.T, mock sqlmock.Sqlmock, dayEnd time.Time, domainMaxOverride map[string]int, tokenColOverride *string, bandOverride string) {
	t.Helper()

	bandCol := f.dom.Band()
	if bandOverride != "" {
		bandCol = bandOverride
	}

	domMeta, domTok := blockFor(t, f.id, f.dom, f.dom.Version)
	domMax := f.dom.DailyMaxByISP
	if domainMaxOverride != nil {
		domMax = domainMaxOverride
	}
	domMaxJSON, err := json.Marshal(domMax)
	if err != nil {
		t.Fatalf("marshal daily_max_by_isp: %v", err)
	}
	domTokCol := domTok.Value
	if tokenColOverride != nil {
		domTokCol = *tokenColOverride
	}

	domCols := append(metaColNames(),
		"sending_domain", "brand_code", "daily_max_by_isp",
		"active_window_start", "active_window_end", "interval_minutes", "max_burst_intervals", "ramp_source",
		"health_band", "ramp_stage")
	mock.ExpectQuery(regexp.QuoteMeta("FROM drip_domain_contracts WHERE status = 'active' AND effective_at < $1")).
		WithArgs(dayEnd).
		WillReturnRows(sqlmock.NewRows(domCols).AddRow(append(metaRow(f.id, f.eff, f.dom.Version, domMeta, domTokCol),
			f.dom.SendingDomain, f.dom.BrandCode, domMaxJSON,
			// domainSelectBody renders the two `time` columns with
			// to_char(...,'HH24:MI'), so the driver hands back exactly this.
			"01:00", "20:00", f.dom.IntervalMinutes, f.dom.MaxBurstIntervals, f.dom.RampSource,
			bandCol, f.dom.RampStage)...))

	dispMeta, dispTok := blockFor(t, f.id, f.disp, f.disp.Version)
	dispDesired, err := json.Marshal(f.disp.DesiredDailyIntros)
	if err != nil {
		t.Fatalf("marshal desired: %v", err)
	}
	dispCols := append(metaColNames(),
		"lane", "operator_priority_tier", "desired_daily_intros", "demand_mode", "daily_ceiling",
		"allowed_domains", "isp_exclusions", "ladder_touches", "ladder_gap_hours",
		"followups_committed", "max_intro_share", "exploration_share")
	mock.ExpectQuery(regexp.QuoteMeta("FROM drip_dispatch_contracts WHERE status = 'active' AND effective_at < $1")).
		WithArgs(dayEnd).
		WillReturnRows(sqlmock.NewRows(dispCols).AddRow(append(metaRow(f.id, f.eff, f.disp.Version, dispMeta, dispTok.Value),
			f.disp.Lane, f.disp.OperatorPriorityTier, dispDesired, f.disp.DemandMode, int64(*f.disp.DailyCeiling),
			"{"+strings.Join(f.disp.AllowedDomains, ",")+"}", "{"+strings.Join(f.disp.ISPExclusions, ",")+"}",
			f.disp.LadderTouches, f.disp.LadderGapHours, f.disp.FollowupsCommitted,
			f.disp.MaxIntroShare, f.disp.ExplorationShare)...))

	invMeta, invTok := blockFor(t, f.id, f.inv, f.inv.Version)
	invCols := append(metaColNames(),
		"lane", "accepted_sources", "verdict_valid_days", "eo_enabled", "max_daily_eo_spend_usd",
		"min_eo_order", "min_coverage_hours", "target_coverage_hours", "max_coverage_hours",
		"remail_enabled", "remail_after_days", "remail_mode", "max_remail_share")
	mock.ExpectQuery(regexp.QuoteMeta("FROM drip_inventory_contracts WHERE status = 'active' AND effective_at < $1")).
		WithArgs(dayEnd).
		WillReturnRows(sqlmock.NewRows(invCols).AddRow(append(metaRow(f.id, f.eff, f.inv.Version, invMeta, invTok.Value),
			f.inv.Lane, "{"+strings.Join(f.inv.AcceptedSources, ",")+"}", f.inv.VerdictValidDays, f.inv.EOEnabled,
			f.inv.MaxDailyEOSpendUSD, f.inv.MinEOOrder, f.inv.MinCoverageHours, f.inv.TargetCoverageHours,
			f.inv.MaxCoverageHours, f.inv.RemailEnabled, f.inv.RemailAfterDays, f.inv.RemailMode, f.inv.MaxRemailShare)...))

	srcMeta, srcTok := blockFor(t, f.id, f.src, f.src.Version)
	srcCols := append(metaColNames(),
		"source_slug", "record_class", "eligible_isps", "max_daily_intake",
		"arrival_cadence", "validated_on_arrival", "record_max_age_days", "unit_acquisition_cost")
	mock.ExpectQuery(regexp.QuoteMeta("FROM drip_source_contracts WHERE status = 'active' AND effective_at < $1")).
		WithArgs(dayEnd).
		WillReturnRows(sqlmock.NewRows(srcCols).AddRow(append(metaRow(f.id, f.eff, f.src.Version, srcMeta, srcTok.Value),
			f.src.SourceSlug, f.src.RecordClass, "{"+strings.Join(f.src.EligibleISPs, ",")+"}", nil,
			f.src.ArrivalCadence, f.src.ValidatedOnArrival, int64(*f.src.RecordMaxAgeDays), f.src.UnitAcquisitionCost)...))
}

func denverDay() (time.Time, time.Time, time.Time) {
	denver := time.FixedZone("MDT", -6*3600)
	day := time.Date(2026, 9, 3, 13, 45, 0, 0, denver)
	dayEnd := time.Date(2026, 9, 4, 0, 0, 0, 0, denver)
	eff := time.Date(2026, 9, 3, 0, 0, 0, 0, denver)
	return day, dayEnd, eff
}

func TestLoadActive_BuildsMapsAndMissesFailClosed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	day, dayEnd, eff := denverDay()
	f := newActiveFixture(eff)
	f.expect(t, mock, dayEnd, nil, nil)

	set, err := dripsupply.LoadActiveWithKey(context.Background(), db, day, testKey)
	if err != nil {
		t.Fatalf("LoadActiveWithKey: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}

	dom, err := set.Domain("em.historythinking.com")
	if err != nil {
		t.Fatalf("Domain lookup: %v", err)
	}
	if dom.DailyMaxByISP["yahoo"] != 17000 || dom.DailyMaxByISP["gmail"] != 0 {
		t.Fatalf("daily_max_by_isp decoded wrong: %v", dom.DailyMaxByISP)
	}
	if dom.BrandCode != "HT" || dom.IntervalMinutes != 15 || dom.RampSource != "sending_domain_cards" {
		t.Fatalf("domain row decoded wrong: %+v", dom)
	}
	if dom.Version != 3 || dom.Status != dripsupply.StatusActive || dom.ChangeLedgerID != "chg-0001" {
		t.Fatalf("meta decoded wrong: %+v", dom.Meta)
	}
	// The metadata block round-trips out of JSONB, and the token column
	// duplicates metadata.token.value (§1.5).
	if dom.Metadata.Kind != "domain" || dom.Metadata.Version != 3 {
		t.Fatalf("metadata identity decoded wrong: %+v", dom.Metadata)
	}
	if dom.Metadata.Mutation.ChangeLedgerID != "chg-0001" || dom.Metadata.Mutation.PriorVersion != 2 {
		t.Fatalf("metadata mutation decoded wrong: %+v", dom.Metadata.Mutation)
	}
	if dom.Metadata.Refs.SendingDomainID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("metadata refs decoded wrong: %+v", dom.Metadata.Refs)
	}
	if dom.Token == "" || dom.Token != dom.Metadata.Token.Value {
		t.Fatalf("token column %q != metadata.token.value %q", dom.Token, dom.Metadata.Token.Value)
	}
	// The window comes back in canonical HH:MM — NOT the RFC3339 form a bare
	// string scan of a `time` column produces ("0000-01-01T01:00:00Z"), which
	// parseClock rejects and which broke Schedule on the real server.
	if dom.ActiveWindowStart != "01:00" || dom.ActiveWindowEnd != "20:00" {
		t.Fatalf("window read back as %q/%q, want 01:00/20:00", dom.ActiveWindowStart, dom.ActiveWindowEnd)
	}
	if strings.Contains(dom.ActiveWindowStart, "T") || strings.Contains(dom.ActiveWindowStart, "Z") {
		t.Fatalf("window came back as a formatted timestamp: %q", dom.ActiveWindowStart)
	}
	if _, err := dripsupply.WindowOf(dom); err != nil {
		t.Fatalf("WindowOf could not parse the loaded window: %v", err)
	}
	if err := dom.Validate(); err != nil {
		t.Fatalf("loaded domain contract fails its own validation: %v", err)
	}

	disp, err := set.Dispatch("wcl_remail")
	if err != nil {
		t.Fatalf("Dispatch lookup: %v", err)
	}
	if disp.DailyCeiling == nil || *disp.DailyCeiling != 208000 {
		t.Fatalf("daily_ceiling decoded wrong: %v", disp.DailyCeiling)
	}
	if strings.Join(disp.AllowedDomains, ",") != "HT,DB" {
		t.Fatalf("allowed_domains decoded wrong: %v", disp.AllowedDomains)
	}
	if strings.Join(disp.ISPExclusions, ",") != "gmail" {
		t.Fatalf("isp_exclusions decoded wrong: %v", disp.ISPExclusions)
	}

	inv, err := set.Inventory("wcl_remail")
	if err != nil {
		t.Fatalf("Inventory lookup: %v", err)
	}
	if inv.TargetCoverageHours != 16 || inv.MaxRemailShare != 0.25 {
		t.Fatalf("inventory decoded wrong: %+v", inv)
	}

	src, err := set.Source("wcl_abandon")
	if err != nil {
		t.Fatalf("Source lookup: %v", err)
	}
	if src.MaxDailyIntake != nil {
		t.Fatalf("NULL max_daily_intake should stay nil, got %v", *src.MaxDailyIntake)
	}
	if src.RecordMaxAgeDays == nil || *src.RecordMaxAgeDays != 90 {
		t.Fatalf("record_max_age_days decoded wrong: %v", src.RecordMaxAgeDays)
	}

	// FAIL CLOSED: every miss is an error, never a default.
	misses := []struct {
		name string
		call func() error
		kind dripsupply.ContractKind
		subj string
	}{
		{"domain", func() error { _, e := set.Domain("em.quizfiesta.com"); return e }, dripsupply.KindDomain, "em.quizfiesta.com"},
		{"dispatch", func() error { _, e := set.Dispatch("globusa"); return e }, dripsupply.KindDispatch, "globusa"},
		{"inventory", func() error { _, e := set.Inventory("globusa"); return e }, dripsupply.KindInventory, "globusa"},
		{"source", func() error { _, e := set.Source("nope"); return e }, dripsupply.KindSource, "nope"},
	}
	for _, m := range misses {
		t.Run("miss_"+m.name, func(t *testing.T) {
			err := m.call()
			var nac *dripsupply.ErrNoActiveContract
			if !errors.As(err, &nac) {
				t.Fatalf("expected *ErrNoActiveContract, got %T: %v", err, err)
			}
			if nac.Kind != m.kind || nac.Subject != m.subj {
				t.Fatalf("ErrNoActiveContract{%s,%s}, want {%s,%s}", nac.Kind, nac.Subject, m.kind, m.subj)
			}
		})
	}
}

// §1.5 rule 2: a row whose BODY was hand-edited after issue no longer matches
// its token, and the WHOLE ActiveSet load fails — not just that contract.
func TestLoadActive_RefusesTamperedBody(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	day, dayEnd, eff := denverDay()
	f := newActiveFixture(eff)

	// One field changed in the row, token left as issued: gmail 0 -> 400,
	// exactly the edit the gmail ban exists to prevent.
	tampered := map[string]int{}
	for k, v := range f.dom.DailyMaxByISP {
		tampered[k] = v
	}
	tampered["gmail"] = 400
	f.expect(t, mock, dayEnd, tampered, nil)

	set, err := dripsupply.LoadActiveWithKey(context.Background(), db, day, testKey)
	if set != nil {
		t.Fatal("a tampered contract must fail the whole load, not return a partial set")
	}
	var mm *dripsupply.ErrTokenMismatch
	if !errors.As(err, &mm) {
		t.Fatalf("expected *ErrTokenMismatch, got %T: %v", err, err)
	}
	if mm.Kind != dripsupply.KindDomain || mm.Subject != "em.historythinking.com" || mm.Version != 3 {
		t.Fatalf("ErrTokenMismatch{%s,%s,v%d} wrong", mm.Kind, mm.Subject, mm.Version)
	}
	if !errors.Is(err, contractmeta.ErrTokenMismatch) {
		t.Fatal("the contractmeta sentinel must stay wrapped")
	}
}

// The token column duplicates metadata.token.value for indexing. If someone
// edits one and not the other, the contract is refused.
func TestLoadActive_RefusesTokenColumnDrift(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	day, dayEnd, eff := denverDay()
	f := newActiveFixture(eff)
	drifted := "deadbeef"
	f.expect(t, mock, dayEnd, nil, &drifted)

	_, err = dripsupply.LoadActiveWithKey(context.Background(), db, day, testKey)
	var mm *dripsupply.ErrTokenMismatch
	if !errors.As(err, &mm) {
		t.Fatalf("expected *ErrTokenMismatch for token-column drift, got %T: %v", err, err)
	}
}

// Fail closed on a missing key: no key, no contracts, no sending.
func TestLoadActive_NoKeyFailsClosed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	day, _, _ := denverDay()
	if _, err := dripsupply.LoadActiveWithKey(context.Background(), db, day, nil); err == nil {
		t.Fatal("LoadActiveWithKey accepted an empty key")
	}
	// No query was queued: it must refuse before reading, or after reading but
	// never returning a set. Either way nothing is honoured.
	if _, err := dripsupply.LoadActiveWithKey(context.Background(), db, day, nil); err == nil {
		t.Fatal("second call also must refuse")
	}
	_ = mock
}

func TestLoadActive_QueryErrorIsWrapped(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	boom := errors.New("relation does not exist")
	mock.ExpectQuery(regexp.QuoteMeta("FROM drip_domain_contracts")).WillReturnError(boom)

	if _, err := dripsupply.LoadActiveWithKey(context.Background(), db, time.Now(), testKey); !errors.Is(err, boom) {
		t.Fatalf("expected the driver error to be wrapped with %%w, got %v", err)
	}
}

func metaColNames() []string {
	return []string{
		"id", "version", "status", "effective_at", "superseded_at",
		"created_by", "created_at", "approved_by", "approved_at",
		"change_ledger_id", "notes", "metadata", "token",
	}
}

// A nil/empty ActiveSet must still fail closed rather than panic.
func TestActiveSetNilFailsClosed(t *testing.T) {
	var set *dripsupply.ActiveSet
	if _, err := set.Domain("em.discountblog.com"); err == nil {
		t.Fatal("nil ActiveSet returned a contract")
	}
	empty := &dripsupply.ActiveSet{}
	var nac *dripsupply.ErrNoActiveContract
	if _, err := empty.Dispatch("wcl_remail"); !errors.As(err, &nac) {
		t.Fatalf("empty ActiveSet: want *ErrNoActiveContract, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// ActivateScheduled
// ---------------------------------------------------------------------------

// expectActivationKind queues the three statements ActivateScheduled runs for
// one kind, IN ORDER. sqlmock's ordered expectations are the assertion that the
// outgoing active row is superseded BEFORE the incoming one is promoted — if
// that order flipped, the partial unique index would reject the promote.
func expectActivationKind(mock sqlmock.Sqlmock, table string, now time.Time, stale, superseded, activated int64) {
	mock.ExpectExec(regexp.QuoteMeta("UPDATE " + table + " SET status = 'superseded', superseded_at = $1 WHERE status = 'scheduled'")).
		WithArgs(now).WillReturnResult(sqlmock.NewResult(0, stale))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE " + table + " SET status = 'superseded', superseded_at = $1 WHERE status = 'active'")).
		WithArgs(now).WillReturnResult(sqlmock.NewResult(0, superseded))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE " + table + " SET status = 'active' WHERE status = 'scheduled' AND effective_at <= $1")).
		WithArgs(now).WillReturnResult(sqlmock.NewResult(0, activated))
}

func TestActivateScheduled_BoundarySupersedesThenPromotes(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	denver := time.FixedZone("MDT", -6*3600)
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, denver) // the boundary itself

	mock.ExpectBegin()
	// domain: one superseded stale draft, one outgoing active retired, one promoted.
	expectActivationKind(mock, "drip_domain_contracts", now, 1, 1, 1)
	expectActivationKind(mock, "drip_dispatch_contracts", now, 0, 2, 2)
	expectActivationKind(mock, "drip_inventory_contracts", now, 0, 0, 0)
	expectActivationKind(mock, "drip_source_contracts", now, 0, 0, 0)
	mock.ExpectCommit()

	res, err := dripsupply.ActivateScheduled(context.Background(), db, now)
	if err != nil {
		t.Fatalf("ActivateScheduled: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
	if res.Activated[dripsupply.KindDomain] != 1 || res.Superseded[dripsupply.KindDomain] != 1 || res.SkippedStale[dripsupply.KindDomain] != 1 {
		t.Fatalf("domain counts wrong: %+v", res)
	}
	if res.Activated[dripsupply.KindDispatch] != 2 || res.Superseded[dripsupply.KindDispatch] != 2 {
		t.Fatalf("dispatch counts wrong: %+v", res)
	}
	if res.Total() != 3 {
		t.Fatalf("Total()=%d, want 3", res.Total())
	}
}

// Safe to run every tick: a second pass finds nothing due and writes nothing.
func TestActivateScheduled_IdempotentOnASecondRun(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 9, 4, 0, 15, 0, 0, time.UTC)
	mock.ExpectBegin()
	for _, table := range []string{"drip_domain_contracts", "drip_dispatch_contracts", "drip_inventory_contracts", "drip_source_contracts"} {
		expectActivationKind(mock, table, now, 0, 0, 0)
	}
	mock.ExpectCommit()

	res, err := dripsupply.ActivateScheduled(context.Background(), db, now)
	if err != nil {
		t.Fatalf("ActivateScheduled: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
	if res.Total() != 0 {
		t.Fatalf("second run activated %d rows, want 0", res.Total())
	}
	for _, k := range dripsupply.AllKinds() {
		if res.Superseded[k] != 0 || res.SkippedStale[k] != 0 {
			t.Fatalf("second run mutated %s: %+v", k, res)
		}
	}
}

// TWO ACTIVES FOR ONE SUBJECT IS IMPOSSIBLE: if the promote statement would
// create a second active row, the §1.1 partial unique index rejects it with
// 23505 and ActivateScheduled surfaces that as *ErrDuplicateActive naming the
// index it relies on — and rolls the whole boundary back.
func TestActivateScheduled_DuplicateActiveRejectedByPartialIndex(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	dup := &pq.Error{
		Code:       "23505",
		Constraint: dripsupply.ActiveIndexDomain,
		Message:    `duplicate key value violates unique constraint "` + dripsupply.ActiveIndexDomain + `"`,
	}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE drip_domain_contracts SET status = 'superseded', superseded_at = $1 WHERE status = 'scheduled'")).
		WithArgs(now).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE drip_domain_contracts SET status = 'superseded', superseded_at = $1 WHERE status = 'active'")).
		WithArgs(now).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE drip_domain_contracts SET status = 'active' WHERE status = 'scheduled'")).
		WithArgs(now).WillReturnError(dup)
	mock.ExpectRollback()

	_, err = dripsupply.ActivateScheduled(context.Background(), db, now)
	var dupErr *dripsupply.ErrDuplicateActive
	if !errors.As(err, &dupErr) {
		t.Fatalf("expected *ErrDuplicateActive, got %T: %v", err, err)
	}
	if dupErr.Kind != dripsupply.KindDomain {
		t.Fatalf("ErrDuplicateActive.Kind=%s, want domain", dupErr.Kind)
	}
	if dupErr.IndexName != dripsupply.ActiveIndexDomain {
		t.Fatalf("ErrDuplicateActive.IndexName=%q, want %q", dupErr.IndexName, dripsupply.ActiveIndexDomain)
	}
	if !errors.Is(err, dup) {
		t.Fatal("driver error must stay wrapped")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet (the boundary must roll back): %v", err)
	}
}

// Negative control for the case above: an unrelated SQL error must NOT be
// dressed up as a duplicate-active violation.
func TestActivateScheduled_OtherErrorIsNotDuplicateActive(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	boom := &pq.Error{Code: "42P01", Message: `relation "drip_domain_contracts" does not exist`}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE drip_domain_contracts SET status = 'superseded'")).
		WithArgs(now).WillReturnError(boom)
	mock.ExpectRollback()

	_, err = dripsupply.ActivateScheduled(context.Background(), db, now)
	var dupErr *dripsupply.ErrDuplicateActive
	if errors.As(err, &dupErr) {
		t.Fatal("a 42P01 was reported as a duplicate-active violation")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected the driver error wrapped with %%w, got %v", err)
	}
}

func TestActivateScheduled_BeginFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	boom := errors.New("too many connections")
	mock.ExpectBegin().WillReturnError(boom)
	if _, err := dripsupply.ActivateScheduled(context.Background(), db, time.Now()); !errors.Is(err, boom) {
		t.Fatalf("expected wrapped begin error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Version helpers
// ---------------------------------------------------------------------------

func TestNextVersion(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(version), 0) + 1 FROM drip_dispatch_contracts WHERE lane = $1")).
		WithArgs("wcl_remail").
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(4))

	v, err := dripsupply.NextVersion(context.Background(), db, dripsupply.KindDispatch, "wcl_remail")
	if err != nil {
		t.Fatalf("NextVersion: %v", err)
	}
	if v != 4 {
		t.Fatalf("NextVersion=%d, want 4", v)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}

	if _, err := dripsupply.NextVersion(context.Background(), db, dripsupply.ContractKind("kumo"), "x"); err == nil {
		t.Fatal("NextVersion accepted an unknown kind")
	}
	if _, err := dripsupply.NextVersion(context.Background(), db, dripsupply.KindDispatch, "  "); err == nil {
		t.Fatal("NextVersion accepted an empty subject")
	}
}

func TestInsertDraft(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	c := validDomain()
	c.CreatedBy = "operator"
	c.ChangeLedgerID = "chg-2026-09-03-01"
	c.EffectiveAt = time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(version), 0) + 1 FROM drip_domain_contracts WHERE sending_domain = $1")).
		WithArgs(c.SendingDomain).
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(7))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO drip_domain_contracts")).
		WillReturnResult(sqlmock.NewResult(0, 1))

	id, version, err := dripsupply.InsertDraft(context.Background(), db, c)
	if err != nil {
		t.Fatalf("InsertDraft: %v", err)
	}
	if id == uuid.Nil || version != 7 {
		t.Fatalf("InsertDraft returned id=%v version=%d", id, version)
	}
	if c.Status != dripsupply.StatusDraft || c.Version != 7 || c.ID != id {
		t.Fatalf("InsertDraft did not stamp the contract: %+v", c.Meta)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// An invalid contract, or one with no audit trail, never reaches the database.
func TestInsertDraft_RefusesWithoutTouchingTheDB(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*dripsupply.DomainContract)
	}{
		{"invalid contract", func(c *dripsupply.DomainContract) { delete(c.DailyMaxByISP, "aol") }},
		{"no created_by", func(c *dripsupply.DomainContract) { c.CreatedBy = "" }},
		{"no change_ledger_id", func(c *dripsupply.DomainContract) { c.ChangeLedgerID = " " }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()

			c := validDomain()
			c.CreatedBy = "operator"
			c.ChangeLedgerID = "chg-1"
			tc.mut(c)

			if _, _, err := dripsupply.InsertDraft(context.Background(), db, c); err == nil {
				t.Fatal("InsertDraft accepted a contract it must refuse")
			}
			// No expectations were queued; any query at all is a failure.
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("InsertDraft touched the database: %v", err)
			}
		})
	}
}

func TestContractStatusScanAndValidity(t *testing.T) {
	var s dripsupply.ContractStatus
	if err := s.Scan([]byte("scheduled")); err != nil || s != dripsupply.StatusScheduled {
		t.Fatalf("Scan([]byte)=%q,%v", s, err)
	}
	if err := s.Scan("superseded"); err != nil || s != dripsupply.StatusSuperseded {
		t.Fatalf("Scan(string)=%q,%v", s, err)
	}
	if err := s.Scan(42); err == nil {
		t.Fatal("Scan accepted an int")
	}
	if len(dripsupply.AllStatuses()) != 5 {
		t.Fatalf("AllStatuses()=%v, want 5", dripsupply.AllStatuses())
	}
	for _, st := range dripsupply.AllStatuses() {
		if !st.Valid() {
			t.Fatalf("%q should be valid", st)
		}
	}
	if dripsupply.ContractStatus("live").Valid() {
		t.Fatal("bogus status reported valid")
	}
}

// ---------------------------------------------------------------------------
// §1.5 addendum — Schedule issues the token, exactly once, fail-closed
// ---------------------------------------------------------------------------

// approvedDomainRow is one `approved` domain row with no token yet — the state
// a contract is in immediately before Schedule.
func approvedDomainRow(id uuid.UUID, c *dripsupply.DomainContract, version int) *sqlmock.Rows {
	cols := append(metaColNames(),
		"sending_domain", "brand_code", "daily_max_by_isp",
		"active_window_start", "active_window_end", "interval_minutes", "max_burst_intervals", "ramp_source",
		"health_band", "ramp_stage")
	raw, _ := json.Marshal(c.DailyMaxByISP)
	meta := []driver.Value{
		id.String(), version, "approved", issuedAt, nil,
		"operator", issuedAt, "operator", issuedAt,
		"chg-0001", c.Notes, []byte("{}"), "",
	}
	return sqlmock.NewRows(cols).AddRow(append(meta,
		c.SendingDomain, c.BrandCode, raw,
		"01:00", "20:00", c.IntervalMinutes, c.MaxBurstIntervals, c.RampSource,
		c.Band(), c.RampStage)...)
}

func TestSchedule_IssuesTokenAndMovesApprovedToScheduled(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	id := uuid.New()
	c := validDomain()
	c.Version = 4

	mock.ExpectQuery(regexp.QuoteMeta("FROM drip_domain_contracts WHERE sending_domain = $1 AND version = $2")).
		WithArgs(c.SendingDomain, 4).
		WillReturnRows(approvedDomainRow(id, c, 4))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE drip_domain_contracts SET status = 'scheduled', metadata = $1, token = $2 WHERE id = $3 AND status = 'approved'")).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), id).
		WillReturnResult(sqlmock.NewResult(0, 1))

	tok, err := dripsupply.Schedule(context.Background(), db, dripsupply.KindDomain, c.SendingDomain, 4, testKey, issuedAt)
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
	if tok.Alg != "hmac-sha256" || tok.IssuedBy != "system" || !tok.IssuedAt.Equal(issuedAt) {
		t.Fatalf("token header wrong: %+v", tok)
	}
	// The value must be exactly the HMAC over the loaded row's policy body —
	// the same computation LoadActive verifies with.
	loaded := validDomain()
	want := issueFor(loaded, 4)
	if tok.Value != want.Value {
		t.Fatalf("token value %q, want %q (HMAC over the policy body)", tok.Value, want.Value)
	}
}

// Exactly-once: the row is already `scheduled`, so a second Schedule refuses
// instead of re-stamping with a fresh issued_at, and issues no UPDATE at all.
func TestSchedule_RefusesASecondIssue(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	id := uuid.New()
	c := validDomain()
	rows := approvedDomainRow(id, c, 4)
	// same row, already scheduled
	cols := append(metaColNames(),
		"sending_domain", "brand_code", "daily_max_by_isp",
		"active_window_start", "active_window_end", "interval_minutes", "max_burst_intervals", "ramp_source",
		"health_band", "ramp_stage")
	raw, _ := json.Marshal(c.DailyMaxByISP)
	scheduled := sqlmock.NewRows(cols).AddRow(append([]driver.Value{
		id.String(), 4, "scheduled", issuedAt, nil,
		"operator", issuedAt, "operator", issuedAt,
		"chg-0001", c.Notes, []byte("{}"), "abc123",
	}, c.SendingDomain, c.BrandCode, raw, "01:00", "20:00", 15, 2, c.RampSource,
		c.Band(), c.RampStage)...)
	_ = rows

	mock.ExpectQuery(regexp.QuoteMeta("FROM drip_domain_contracts WHERE sending_domain = $1 AND version = $2")).
		WithArgs(c.SendingDomain, 4).
		WillReturnRows(scheduled)
	// No ExpectExec: the UPDATE must never be issued.

	_, err = dripsupply.Schedule(context.Background(), db, dripsupply.KindDomain, c.SendingDomain, 4, testKey, issuedAt)
	var na *dripsupply.ErrNotApproved
	if !errors.As(err, &na) {
		t.Fatalf("expected *ErrNotApproved, got %T: %v", err, err)
	}
	if na.Status != dripsupply.StatusScheduled {
		t.Fatalf("ErrNotApproved.Status = %q, want scheduled", na.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("Schedule wrote on a second issue: %v", err)
	}
}

// A concurrent writer moved the row out of `approved` between the read and the
// UPDATE: the guarded UPDATE affects 0 rows and Schedule refuses.
func TestSchedule_LostRaceAffectsZeroRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	id := uuid.New()
	c := validDomain()
	mock.ExpectQuery(regexp.QuoteMeta("FROM drip_domain_contracts WHERE sending_domain = $1 AND version = $2")).
		WithArgs(c.SendingDomain, 4).
		WillReturnRows(approvedDomainRow(id, c, 4))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE drip_domain_contracts SET status = 'scheduled'")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	_, err = dripsupply.Schedule(context.Background(), db, dripsupply.KindDomain, c.SendingDomain, 4, testKey, issuedAt)
	var na *dripsupply.ErrNotApproved
	if !errors.As(err, &na) {
		t.Fatalf("expected *ErrNotApproved on a 0-row UPDATE, got %T: %v", err, err)
	}
}

// No key, no issue — and no database traffic at all.
func TestSchedule_RefusesWithoutKey(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	_, err = dripsupply.Schedule(context.Background(), db, dripsupply.KindDomain, "em.historythinking.com", 4, nil, issuedAt)
	if !errors.Is(err, contractmeta.ErrNoKey) {
		t.Fatalf("expected contractmeta.ErrNoKey, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("Schedule touched the database without a key: %v", err)
	}
	// Negative control: with a key it gets as far as the read.
	mock.ExpectQuery(regexp.QuoteMeta("FROM drip_domain_contracts WHERE sending_domain = $1")).
		WillReturnError(errors.New("reached the read"))
	if _, err := dripsupply.Schedule(context.Background(), db, dripsupply.KindDomain, "em.historythinking.com", 4, testKey, issuedAt); err == nil {
		t.Fatal("expected the read to be attempted with a key")
	}
}

// An invalid contract is never given a token: otherwise the invalid row would
// verify forever afterwards.
func TestSchedule_RefusesInvalidContract(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	id := uuid.New()
	c := validDomain()
	delete(c.DailyMaxByISP, "sbcglobal") // missing ISP class
	mock.ExpectQuery(regexp.QuoteMeta("FROM drip_domain_contracts WHERE sending_domain = $1 AND version = $2")).
		WithArgs(c.SendingDomain, 4).
		WillReturnRows(approvedDomainRow(id, c, 4))
	// No ExpectExec.

	_, err = dripsupply.Schedule(context.Background(), db, dripsupply.KindDomain, c.SendingDomain, 4, testKey, issuedAt)
	var verrs dripsupply.ValidationErrors
	if !errors.As(err, &verrs) {
		t.Fatalf("expected ValidationErrors, got %T: %v", err, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("Schedule wrote an invalid contract: %v", err)
	}
}

func TestSchedule_ContractNotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	cols := append(metaColNames(), "lane", "operator_priority_tier", "desired_daily_intros",
		"demand_mode", "daily_ceiling", "allowed_domains", "isp_exclusions", "ladder_touches",
		"ladder_gap_hours", "followups_committed", "max_intro_share", "exploration_share")
	mock.ExpectQuery(regexp.QuoteMeta("FROM drip_dispatch_contracts WHERE lane = $1 AND version = $2")).
		WithArgs("nope", 9).
		WillReturnRows(sqlmock.NewRows(cols))

	_, err = dripsupply.Schedule(context.Background(), db, dripsupply.KindDispatch, "nope", 9, testKey, issuedAt)
	var nf *dripsupply.ErrContractNotFound
	if !errors.As(err, &nf) {
		t.Fatalf("expected *ErrContractNotFound, got %T: %v", err, err)
	}
}

// InsertDraft stamps identity + mutation (§1.5 rule 4) and leaves the token
// unset — a draft has no token; it is issued at approved -> scheduled.
func TestInsertDraft_StampsMetadataAndNoToken(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	c := validDomain()
	c.CreatedBy = "operator"
	c.ChangeLedgerID = "chg-2026-09-03-07"
	c.Metadata.Refs = contractmeta.Refs{
		SendingDomainID: "aaaaaaaa-0000-0000-0000-000000000001",
		OwnedDomainID:   "historythinking.com",
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(version), 0) + 1 FROM drip_domain_contracts")).
		WithArgs(c.SendingDomain).
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(12))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO drip_domain_contracts")).
		WillReturnResult(sqlmock.NewResult(0, 1))

	id, version, err := dripsupply.InsertDraft(context.Background(), db, c)
	if err != nil {
		t.Fatalf("InsertDraft: %v", err)
	}
	if c.Metadata.ContractID != id.String() || c.Metadata.Kind != "domain" || c.Metadata.Version != version {
		t.Fatalf("identity not stamped: %+v", c.Metadata)
	}
	if c.Metadata.Mutation.By != "operator" || c.Metadata.Mutation.ChangeLedgerID != "chg-2026-09-03-07" {
		t.Fatalf("mutation not stamped: %+v", c.Metadata.Mutation)
	}
	if c.Metadata.Mutation.PriorVersion != 11 {
		t.Fatalf("prior_version = %d, want 11", c.Metadata.Mutation.PriorVersion)
	}
	if c.Metadata.Mutation.At.IsZero() {
		t.Fatal("mutation.at not stamped")
	}
	// Caller-resolved refs survive.
	if c.Metadata.Refs.OwnedDomainID != "historythinking.com" {
		t.Fatalf("refs clobbered: %+v", c.Metadata.Refs)
	}
	// A draft carries NO token.
	if c.Token != "" || c.Metadata.Token.Issued() {
		t.Fatalf("draft was given a token: col=%q blk=%+v", c.Token, c.Metadata.Token)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// ---------------------------------------------------------------------------
// health_band is POLICY on the contract (operator ruling 2026-09-03)
// ---------------------------------------------------------------------------

// Flipping the band in the ROW without re-issuing the token must be refused —
// this is the whole point of putting the band on the contract instead of
// inferring it: red means 0 and amber means half, so a hand-edited band is a
// silent volume change.
func TestLoadActive_RefusesHandEditedHealthBand(t *testing.T) {
	for _, band := range []string{
		dripsupply.HealthBandAmber, // would halve the domain
		dripsupply.HealthBandRed,   // would silence it
	} {
		t.Run(band, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()

			day, dayEnd, eff := denverDay()
			f := newActiveFixture(eff) // signed at green
			f.expectWithBand(t, mock, dayEnd, nil, nil, band)

			set, err := dripsupply.LoadActiveWithKey(context.Background(), db, day, testKey)
			if set != nil {
				t.Fatal("a hand-edited band must fail the whole load")
			}
			var mm *dripsupply.ErrTokenMismatch
			if !errors.As(err, &mm) {
				t.Fatalf("expected *ErrTokenMismatch, got %T: %v", err, err)
			}
			if mm.Kind != dripsupply.KindDomain || mm.Subject != "em.historythinking.com" {
				t.Fatalf("ErrTokenMismatch{%s,%s} wrong", mm.Kind, mm.Subject)
			}
		})
	}

	// Negative control: writing the band it was SIGNED with still verifies.
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	day, dayEnd, eff := denverDay()
	f := newActiveFixture(eff)
	f.expectWithBand(t, mock, dayEnd, nil, nil, dripsupply.HealthBandGreen)
	if _, err := dripsupply.LoadActiveWithKey(context.Background(), db, day, testKey); err != nil {
		t.Fatalf("the signed band must still verify: %v", err)
	}
}

// The band and the ramp stage are both inside the token: changing either
// changes the digest, so a band change forces a re-issue.
func TestDomainTokenBody_CoversBandAndRampStage(t *testing.T) {
	base := validDomain()
	base.Notes = "operator ruling 2026-09-03"
	canon := func(c *dripsupply.DomainContract) string {
		return string(contractmeta.Canonical(c.TokenBody(), "domain", c.SendingDomain, 1))
	}
	baseline := canon(base)

	amber := validDomain()
	amber.Notes = base.Notes
	amber.HealthBand = dripsupply.HealthBandAmber
	if canon(amber) == baseline {
		t.Fatal("health_band is not covered by the token — a band flip would verify")
	}

	red := validDomain()
	red.Notes = base.Notes
	red.HealthBand = dripsupply.HealthBandRed
	if canon(red) == canon(amber) {
		t.Fatal("amber and red produce the same token")
	}

	stage := validDomain()
	stage.Notes = base.Notes
	stage.RampStage = "week 4 / step 2"
	if canon(stage) == baseline {
		t.Fatal("ramp_stage is not covered by the token")
	}

	// "" and "green" are the SAME contract: the column is NOT NULL DEFAULT
	// 'green', so a contract inserted with "" is read back as 'green' and must
	// still verify against the token issued before the round trip.
	empty := validDomain()
	empty.Notes = base.Notes
	empty.HealthBand = ""
	if canon(empty) != baseline {
		t.Fatalf("empty band and green produce different tokens — every defaulted\ncontract would fail verification after its first round trip")
	}
	if empty.Band() != dripsupply.HealthBandGreen {
		t.Fatalf("Band() on an empty field = %q, want green", empty.Band())
	}
}

// Schedule signs the band that the row actually carries.
func TestSchedule_TokenCoversTheRowsBand(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	c := validDomain()
	c.HealthBand = dripsupply.HealthBandAmber
	c.Notes = "operator ruling 2026-09-03: amber pending Microsoft recovery"
	id := uuid.New()

	mock.ExpectQuery(regexp.QuoteMeta("FROM drip_domain_contracts WHERE sending_domain = $1 AND version = $2")).
		WithArgs(c.SendingDomain, 5).
		WillReturnRows(approvedDomainRow(id, c, 5))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE drip_domain_contracts SET status = 'scheduled'")).
		WillReturnResult(sqlmock.NewResult(0, 1))

	tok, err := dripsupply.Schedule(context.Background(), db, dripsupply.KindDomain, c.SendingDomain, 5, testKey, issuedAt)
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	loadedAmber := validDomain()
	loadedAmber.HealthBand = dripsupply.HealthBandAmber
	loadedAmber.Notes = c.Notes
	if tok.Value != issueFor(loadedAmber, 5).Value {
		t.Fatal("token was not computed over the row's amber band")
	}

	loadedGreen := validDomain()
	loadedGreen.Notes = c.Notes
	if tok.Value == issueFor(loadedGreen, 5).Value {
		t.Fatal("the amber and green tokens are identical")
	}
}

// A domain contract cannot be scheduled off green without the ruling in notes,
// so an unexplained band change can never acquire a token at all.
func TestSchedule_RefusesUnexplainedBandChange(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	c := validDomain()
	c.HealthBand = dripsupply.HealthBandRed
	c.Notes = "" // no ruling named
	id := uuid.New()

	mock.ExpectQuery(regexp.QuoteMeta("FROM drip_domain_contracts WHERE sending_domain = $1 AND version = $2")).
		WithArgs(c.SendingDomain, 5).
		WillReturnRows(approvedDomainRow(id, c, 5))
	// No ExpectExec: nothing may be written.

	_, err = dripsupply.Schedule(context.Background(), db, dripsupply.KindDomain, c.SendingDomain, 5, testKey, issuedAt)
	var verrs dripsupply.ValidationErrors
	if !errors.As(err, &verrs) || !verrs.HasField("health_band") {
		t.Fatalf("expected a health_band validation failure, got %T: %v", err, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("Schedule wrote an unexplained band change: %v", err)
	}
}

// The band survives the round trip out of the database.
func TestLoadActive_ReadsBandAndRampStage(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	day, dayEnd, eff := denverDay()
	f := newActiveFixture(eff)
	f.dom.HealthBand = dripsupply.HealthBandAmber
	f.dom.RampStage = "week 3 / step 4"
	f.dom.Notes = "operator ruling 2026-09-03: amber"
	f.expect(t, mock, dayEnd, nil, nil)

	set, err := dripsupply.LoadActiveWithKey(context.Background(), db, day, testKey)
	if err != nil {
		t.Fatalf("LoadActiveWithKey: %v", err)
	}
	dom, err := set.Domain("em.historythinking.com")
	if err != nil {
		t.Fatalf("Domain: %v", err)
	}
	if dom.HealthBand != dripsupply.HealthBandAmber || dom.Band() != dripsupply.HealthBandAmber {
		t.Fatalf("health_band = %q", dom.HealthBand)
	}
	if dom.RampStage != "week 3 / step 4" {
		t.Fatalf("ramp_stage = %q", dom.RampStage)
	}
	if err := dom.Validate(); err != nil {
		t.Fatalf("loaded amber contract fails validation: %v", err)
	}
}

// InsertDraft writes the RESOLVED band, never "" — the column CHECKs the three
// values and this INSERT names the column, so the DDL default cannot fire.
func TestInsertDraft_WritesResolvedBand(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	c := validDomain()
	c.HealthBand = "" // caller left it unset
	c.RampStage = ""
	c.Notes = ""
	c.CreatedBy = "operator"
	c.ChangeLedgerID = "chg-1"

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(version), 0) + 1 FROM drip_domain_contracts")).
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO drip_domain_contracts")).
		WithArgs(
			sqlmock.AnyArg(), 1, "draft", sqlmock.AnyArg(), "operator", "chg-1", "",
			c.SendingDomain, c.BrandCode, sqlmock.AnyArg(), "01:00", "20:00", 15, 2,
			dripsupply.RampSourceCards,
			// the assertion: 'green', not ''
			dripsupply.HealthBandGreen, "",
			sqlmock.AnyArg(), "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if _, _, err := dripsupply.InsertDraft(context.Background(), db, c); err != nil {
		t.Fatalf("InsertDraft: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet — the INSERT did not carry the resolved band: %v", err)
	}
}

// ---------------------------------------------------------------------------
// REAL POSTGRES: draft -> Schedule -> activate -> LoadActive round trip
// ---------------------------------------------------------------------------
//
// These pin the one thing sqlmock structurally cannot: how the DRIVER decodes
// the columns. `active_window_start/end` are `time without time zone`; lib/pq
// decodes that OID into a time.Time and database/sql then formats it RFC3339
// into a string destination — "0000-01-01T01:00:00Z" — which parseClock
// rejects. That made Validate fail inside Schedule (HTTP 500 at approve), so no
// domain contract could ever be issued a token and LoadActive fail-closed the
// entire domain side. Found by WP11 on a real server; the fix is the to_char in
// domainSelectBody, and THIS is its proof.
//
// They run against the LOCAL apex-postgres container in scratch database
// `req118_res`, one throwaway schema per test, and SKIP (never fail, never fall
// back) when it is unreachable. Nothing here can reach production.

const (
	pgAdminDSNEnv     = "DRIPSUPPLY_TEST_ADMIN_DSN"
	pgDefaultAdminDSN = "postgres://apex_user:apex_password@localhost:5432/apex_db?sslmode=disable"
	pgScratchDB       = "req118_res"
)

func pgAdminDSN() string {
	if v := strings.TrimSpace(os.Getenv(pgAdminDSNEnv)); v != "" {
		return v
	}
	return pgDefaultAdminDSN
}

func pgScratchDSN(t *testing.T) string {
	t.Helper()
	dsn := pgAdminDSN()
	i := strings.LastIndex(dsn, "/")
	if i < 0 {
		t.Skipf("cannot derive a scratch DSN from %q", dsn)
	}
	tail := dsn[i+1:]
	q := ""
	if j := strings.Index(tail, "?"); j >= 0 {
		q = tail[j:]
	}
	return dsn[:i+1] + pgScratchDB + q
}

// contractTableDDL mirrors the four CREATE TABLEs in cmd/server/main.go
// (req118_create_drip_*_contracts, :3116 onward). They are restated here
// because main.go is `package main` and cannot be imported, so a WP1 drift
// shows up as a failure of these tests rather than as a 3am NOT NULL violation.
// All four are needed even for a domain-only test: ActivateScheduled walks
// every kind in one transaction.
const domainContractsDDL = `CREATE TABLE IF NOT EXISTS drip_domain_contracts (
	id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	sending_domain      TEXT NOT NULL,
	brand_code          TEXT NOT NULL DEFAULT '',
	version             INT  NOT NULL DEFAULT 1,
	status              TEXT NOT NULL DEFAULT 'draft'
		CHECK (status IN ('draft','approved','scheduled','active','superseded')),
	effective_at        TIMESTAMPTZ NOT NULL,
	superseded_at       TIMESTAMPTZ,
	created_by          TEXT NOT NULL DEFAULT '',
	created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	approved_by         TEXT,
	approved_at         TIMESTAMPTZ,
	change_ledger_id    TEXT NOT NULL DEFAULT '',
	notes               TEXT NOT NULL DEFAULT '',
	metadata            JSONB NOT NULL DEFAULT '{}'::jsonb,
	token               TEXT  NOT NULL DEFAULT '',
	daily_max_by_isp    JSONB NOT NULL,
	active_window_start TIME NOT NULL DEFAULT '01:00',
	active_window_end   TIME NOT NULL DEFAULT '20:00',
	interval_minutes    INT  NOT NULL DEFAULT 15,
	max_burst_intervals INT  NOT NULL DEFAULT 2,
	ramp_source         TEXT,
	health_band         TEXT NOT NULL DEFAULT 'green'
		CHECK (health_band IN ('green','amber','red')),
	ramp_stage          TEXT NOT NULL DEFAULT ''
)`

const dispatchContractsDDL = `CREATE TABLE IF NOT EXISTS drip_dispatch_contracts (
	id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	lane                   TEXT NOT NULL,
	version                INT  NOT NULL DEFAULT 1,
	status                 TEXT NOT NULL DEFAULT 'draft'
		CHECK (status IN ('draft','approved','scheduled','active','superseded')),
	effective_at           TIMESTAMPTZ NOT NULL,
	superseded_at          TIMESTAMPTZ,
	created_by             TEXT NOT NULL DEFAULT '',
	created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	approved_by            TEXT,
	approved_at            TIMESTAMPTZ,
	change_ledger_id       TEXT NOT NULL DEFAULT '',
	notes                  TEXT NOT NULL DEFAULT '',
	metadata               JSONB NOT NULL DEFAULT '{}'::jsonb,
	token                  TEXT  NOT NULL DEFAULT '',
	operator_priority_tier INT  NOT NULL DEFAULT 2,
	desired_daily_intros   JSONB NOT NULL,
	demand_mode            TEXT NOT NULL DEFAULT 'target'
		CHECK (demand_mode IN ('target','consume_available')),
	daily_ceiling          INT,
	allowed_domains        TEXT[] NOT NULL,
	isp_exclusions         TEXT[] NOT NULL DEFAULT '{}',
	ladder_touches         INT  NOT NULL DEFAULT 5,
	ladder_gap_hours       INT  NOT NULL DEFAULT 24,
	followups_committed    BOOLEAN NOT NULL DEFAULT TRUE,
	max_intro_share        NUMERIC NOT NULL DEFAULT 0.40,
	exploration_share      NUMERIC NOT NULL DEFAULT 0
)`

const inventoryContractsDDL = `CREATE TABLE IF NOT EXISTS drip_inventory_contracts (
	id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	lane                   TEXT NOT NULL,
	version                INT  NOT NULL DEFAULT 1,
	status                 TEXT NOT NULL DEFAULT 'draft'
		CHECK (status IN ('draft','approved','scheduled','active','superseded')),
	effective_at           TIMESTAMPTZ NOT NULL,
	superseded_at          TIMESTAMPTZ,
	created_by             TEXT NOT NULL DEFAULT '',
	created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	approved_by            TEXT,
	approved_at            TIMESTAMPTZ,
	change_ledger_id       TEXT NOT NULL DEFAULT '',
	notes                  TEXT NOT NULL DEFAULT '',
	metadata               JSONB NOT NULL DEFAULT '{}'::jsonb,
	token                  TEXT  NOT NULL DEFAULT '',
	accepted_sources       TEXT[] NOT NULL,
	verdict_valid_days     INT  NOT NULL DEFAULT 60,
	eo_enabled             BOOLEAN NOT NULL DEFAULT TRUE,
	max_daily_eo_spend_usd NUMERIC NOT NULL DEFAULT 50,
	min_eo_order           INT  NOT NULL DEFAULT 1000,
	min_coverage_hours     INT  NOT NULL DEFAULT 8,
	target_coverage_hours  INT  NOT NULL DEFAULT 16,
	max_coverage_hours     INT  NOT NULL DEFAULT 36,
	remail_enabled         BOOLEAN NOT NULL DEFAULT FALSE,
	remail_after_days      INT  NOT NULL DEFAULT 7,
	remail_mode            TEXT NOT NULL DEFAULT 'full_ladder'
		CHECK (remail_mode IN ('full_ladder','single_touch')),
	max_remail_share       NUMERIC NOT NULL DEFAULT 0.25
)`

const sourceContractsDDL = `CREATE TABLE IF NOT EXISTS drip_source_contracts (
	id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	source_slug           TEXT NOT NULL,
	version               INT  NOT NULL DEFAULT 1,
	status                TEXT NOT NULL DEFAULT 'draft'
		CHECK (status IN ('draft','approved','scheduled','active','superseded')),
	effective_at          TIMESTAMPTZ NOT NULL,
	superseded_at         TIMESTAMPTZ,
	created_by            TEXT NOT NULL DEFAULT '',
	created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	approved_by           TEXT,
	approved_at           TIMESTAMPTZ,
	change_ledger_id      TEXT NOT NULL DEFAULT '',
	notes                 TEXT NOT NULL DEFAULT '',
	metadata              JSONB NOT NULL DEFAULT '{}'::jsonb,
	token                 TEXT  NOT NULL DEFAULT '',
	record_class          TEXT NOT NULL,
	eligible_isps         TEXT[] NOT NULL,
	max_daily_intake      INT,
	arrival_cadence       TEXT NOT NULL DEFAULT 'continuous',
	validated_on_arrival  BOOLEAN NOT NULL DEFAULT FALSE,
	record_max_age_days   INT,
	unit_acquisition_cost NUMERIC NOT NULL DEFAULT 0
)`

// newContractPG returns a pool pinned to a throwaway schema via a CONNECTION
// parameter (not `SET search_path`, which would apply to one pooled connection
// and leave the rest resolving names elsewhere).
func newContractPG(t *testing.T) *sql.DB {
	t.Helper()

	admin, err := sql.Open("postgres", pgAdminDSN())
	if err != nil {
		t.Skipf("cannot open admin DSN: %v", err)
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Skipf("local postgres unreachable (%v) — set %s to run the dripsupply PG tests", err, pgAdminDSNEnv)
	}
	var exists bool
	if err := admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, pgScratchDB).Scan(&exists); err != nil {
		t.Skipf("cannot list databases: %v", err)
	}
	if !exists {
		if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+pgScratchDB); err != nil &&
			!strings.Contains(err.Error(), "already exists") {
			t.Skipf("cannot create scratch database %s: %v", pgScratchDB, err)
		}
	}

	schema := "ct" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
	bootstrap, err := sql.Open("postgres", pgScratchDSN(t))
	if err != nil {
		t.Skipf("cannot open scratch DSN: %v", err)
	}
	defer bootstrap.Close()
	if err := bootstrap.PingContext(ctx); err != nil {
		t.Skipf("scratch database unreachable: %v", err)
	}
	if _, err := bootstrap.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}

	dsn := pgScratchDSN(t)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := sql.Open("postgres", dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatalf("open scratch: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		if clean, err := sql.Open("postgres", pgScratchDSN(t)); err == nil {
			_, _ = clean.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
			clean.Close()
		}
	})

	stmts := []string{domainContractsDDL, dispatchContractsDDL, inventoryContractsDDL, sourceContractsDDL}
	// The indexes come from ActiveIndexDDL, so these tests also prove the DDL
	// this package hands WP1 is valid and does what its name says.
	for _, kind := range dripsupply.AllKinds() {
		ddl, err := dripsupply.ActiveIndexDDL(kind)
		if err != nil {
			t.Fatalf("ActiveIndexDDL(%s): %v", kind, err)
		}
		stmts = append(stmts, ddl)
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("ddl failed: %v\n%s", err, stmt)
		}
	}
	return db
}

// approve moves a draft to `approved` — the operator step between InsertDraft
// and Schedule, which this package does not own.
func approve(t *testing.T, db *sql.DB, id uuid.UUID) {
	t.Helper()
	if _, err := db.Exec(
		`UPDATE drip_domain_contracts SET status='approved', approved_by='operator', approved_at=NOW() WHERE id=$1`, id); err != nil {
		t.Fatalf("approve: %v", err)
	}
}

// The whole lifecycle on real Postgres: draft -> approved -> Schedule (issues
// the token) -> ActivateScheduled -> LoadActiveWithKey (verifies it).
func TestPG_DomainContractRoundTripVerifies(t *testing.T) {
	db := newContractPG(t)
	ctx := context.Background()

	denver := time.FixedZone("MDT", -6*3600)
	day := time.Date(2026, 9, 4, 9, 0, 0, 0, denver)
	effective := time.Date(2026, 9, 4, 0, 0, 0, 0, denver)

	c := validDomain()
	c.CreatedBy = "operator"
	c.ChangeLedgerID = "chg-pg-1"
	c.EffectiveAt = effective
	c.HealthBand = dripsupply.HealthBandAmber
	c.RampStage = "week 3 / step 4"
	c.Notes = "operator ruling 2026-09-03: amber pending Microsoft recovery"
	c.Metadata.Refs = contractmeta.Refs{OwnedDomainID: "historythinking.com"}

	id, version, err := dripsupply.InsertDraft(ctx, db, c)
	if err != nil {
		t.Fatalf("InsertDraft: %v", err)
	}
	if version != 1 {
		t.Fatalf("version = %d, want 1", version)
	}

	// The row really did land with the window we asked for.
	var startTxt, endTxt, bandTxt string
	if err := db.QueryRow(
		`SELECT to_char(active_window_start,'HH24:MI'), to_char(active_window_end,'HH24:MI'), health_band
		   FROM drip_domain_contracts WHERE id=$1`, id).Scan(&startTxt, &endTxt, &bandTxt); err != nil {
		t.Fatalf("read back the draft: %v", err)
	}
	if startTxt != "01:00" || endTxt != "20:00" || bandTxt != "amber" {
		t.Fatalf("draft stored %s/%s band=%s", startTxt, endTxt, bandTxt)
	}

	// THE REGRESSION: reading the row through the package must not blow up on
	// the time columns. Before the fix LoadOne returned "0000-01-01T01:00:00Z"
	// here and Validate rejected it.
	loaded, err := dripsupply.LoadOne(ctx, db, dripsupply.KindDomain, c.SendingDomain, version)
	if err != nil {
		t.Fatalf("LoadOne: %v", err)
	}
	ld := loaded.(*dripsupply.DomainContract)
	if ld.ActiveWindowStart != "01:00" || ld.ActiveWindowEnd != "20:00" {
		t.Fatalf("LoadOne window = %q/%q, want 01:00/20:00 — the time columns are being formatted, not rendered",
			ld.ActiveWindowStart, ld.ActiveWindowEnd)
	}
	if err := ld.Validate(); err != nil {
		t.Fatalf("a row this package just wrote fails its own validation: %v", err)
	}
	if _, err := dripsupply.WindowOf(ld); err != nil {
		t.Fatalf("WindowOf on a real row: %v", err)
	}

	approve(t, db, id)

	tok, err := dripsupply.Schedule(ctx, db, dripsupply.KindDomain, c.SendingDomain, version, testKey, issuedAt)
	if err != nil {
		t.Fatalf("Schedule (the HTTP 500 at approve): %v", err)
	}
	if !tok.Issued() {
		t.Fatal("Schedule returned an unissued token")
	}

	// The token column and metadata.token.value both landed.
	var colTok, metaTok string
	if err := db.QueryRow(
		`SELECT token, metadata->'token'->>'value' FROM drip_domain_contracts WHERE id=$1`, id).Scan(&colTok, &metaTok); err != nil {
		t.Fatalf("read back the token: %v", err)
	}
	if colTok != tok.Value || metaTok != tok.Value {
		t.Fatalf("token col=%q meta=%q, want %q", colTok, metaTok, tok.Value)
	}

	res, err := dripsupply.ActivateScheduled(ctx, db, effective)
	if err != nil {
		t.Fatalf("ActivateScheduled: %v", err)
	}
	if res.Activated[dripsupply.KindDomain] != 1 {
		t.Fatalf("activated %d domain contracts, want 1", res.Activated[dripsupply.KindDomain])
	}

	set, err := dripsupply.LoadActiveWithKey(ctx, db, day, testKey)
	if err != nil {
		t.Fatalf("LoadActiveWithKey after activation: %v", err)
	}
	got, err := set.Domain(c.SendingDomain)
	if err != nil {
		t.Fatalf("Domain: %v", err)
	}
	if got.ActiveWindowStart != "01:00" || got.ActiveWindowEnd != "20:00" {
		t.Fatalf("active window = %q/%q", got.ActiveWindowStart, got.ActiveWindowEnd)
	}
	if got.HealthBand != dripsupply.HealthBandAmber || got.RampStage != "week 3 / step 4" {
		t.Fatalf("band/stage = %q/%q", got.HealthBand, got.RampStage)
	}
	if got.Metadata.Refs.OwnedDomainID != "historythinking.com" {
		t.Fatalf("refs lost through JSONB: %+v", got.Metadata.Refs)
	}
	if got.DailyMaxByISP["yahoo"] != 1000 {
		t.Fatalf("daily_max_by_isp lost through JSONB: %v", got.DailyMaxByISP)
	}
	if err := dripsupply.VerifyContract(testKey, got); err != nil {
		t.Fatalf("the token this package issued does not verify after the round trip: %v", err)
	}
}

// Negative controls on the same real schema: an UPDATE that changes the body
// without re-issuing must make LoadActive fail closed.
func TestPG_HandEditedRowFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		update string
		args   []any
	}{
		{"window widened", `UPDATE drip_domain_contracts SET active_window_start='00:00' WHERE id=$1`, nil},
		{"band flipped", `UPDATE drip_domain_contracts SET health_band='green' WHERE id=$1`, nil},
		{"gmail opened", `UPDATE drip_domain_contracts SET daily_max_by_isp = jsonb_set(daily_max_by_isp,'{gmail}','400') WHERE id=$1`, nil},
		{"ramp stage edited", `UPDATE drip_domain_contracts SET ramp_stage='hand edited' WHERE id=$1`, nil},
		{"token column blanked", `UPDATE drip_domain_contracts SET token='' WHERE id=$1`, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newContractPG(t)
			ctx := context.Background()
			denver := time.FixedZone("MDT", -6*3600)
			day := time.Date(2026, 9, 4, 9, 0, 0, 0, denver)
			effective := time.Date(2026, 9, 4, 0, 0, 0, 0, denver)

			c := validDomain()
			c.CreatedBy = "operator"
			c.ChangeLedgerID = "chg-pg-2"
			c.EffectiveAt = effective
			c.HealthBand = dripsupply.HealthBandAmber
			c.Notes = "operator ruling 2026-09-03"

			id, version, err := dripsupply.InsertDraft(ctx, db, c)
			if err != nil {
				t.Fatalf("InsertDraft: %v", err)
			}
			approve(t, db, id)
			if _, err := dripsupply.Schedule(ctx, db, dripsupply.KindDomain, c.SendingDomain, version, testKey, issuedAt); err != nil {
				t.Fatalf("Schedule: %v", err)
			}
			if _, err := dripsupply.ActivateScheduled(ctx, db, effective); err != nil {
				t.Fatalf("ActivateScheduled: %v", err)
			}

			// Positive control FIRST: it verifies before the edit.
			if _, err := dripsupply.LoadActiveWithKey(ctx, db, day, testKey); err != nil {
				t.Fatalf("the contract must verify before the hand edit: %v", err)
			}

			args := append([]any{id}, tc.args...)
			if _, err := db.ExecContext(ctx, tc.update, args...); err != nil {
				t.Fatalf("hand edit: %v", err)
			}

			set, err := dripsupply.LoadActiveWithKey(ctx, db, day, testKey)
			if set != nil {
				t.Fatal("a hand-edited contract was honoured")
			}
			var mm *dripsupply.ErrTokenMismatch
			if !errors.As(err, &mm) {
				t.Fatalf("expected *ErrTokenMismatch, got %T: %v", err, err)
			}
		})
	}
}

// The partial unique index really does make two actives impossible on real PG,
// and ActivateScheduled's supersede-then-promote order never trips it.
func TestPG_ActivationNeverHoldsTwoActives(t *testing.T) {
	db := newContractPG(t)
	ctx := context.Background()
	denver := time.FixedZone("MDT", -6*3600)
	day1 := time.Date(2026, 9, 4, 0, 0, 0, 0, denver)
	day2 := time.Date(2026, 9, 5, 0, 0, 0, 0, denver)

	mk := func(effective time.Time, band, notes string) uuid.UUID {
		c := validDomain()
		c.CreatedBy = "operator"
		c.ChangeLedgerID = "chg-pg-3"
		c.EffectiveAt = effective
		c.HealthBand = band
		c.Notes = notes
		id, version, err := dripsupply.InsertDraft(ctx, db, c)
		if err != nil {
			t.Fatalf("InsertDraft: %v", err)
		}
		approve(t, db, id)
		if _, err := dripsupply.Schedule(ctx, db, dripsupply.KindDomain, c.SendingDomain, version, testKey, issuedAt); err != nil {
			t.Fatalf("Schedule v%d: %v", version, err)
		}
		return id
	}

	mk(day1, dripsupply.HealthBandGreen, "")
	if _, err := dripsupply.ActivateScheduled(ctx, db, day1); err != nil {
		t.Fatalf("activate day1: %v", err)
	}
	mk(day2, dripsupply.HealthBandAmber, "operator ruling 2026-09-03: stepping down")

	res, err := dripsupply.ActivateScheduled(ctx, db, day2)
	if err != nil {
		t.Fatalf("activate day2 (supersede-then-promote must not trip the index): %v", err)
	}
	if res.Activated[dripsupply.KindDomain] != 1 || res.Superseded[dripsupply.KindDomain] != 1 {
		t.Fatalf("day2 activated=%d superseded=%d, want 1/1", res.Activated[dripsupply.KindDomain], res.Superseded[dripsupply.KindDomain])
	}

	var actives int
	if err := db.QueryRow(`SELECT COUNT(*) FROM drip_domain_contracts WHERE status='active'`).Scan(&actives); err != nil {
		t.Fatalf("count actives: %v", err)
	}
	if actives != 1 {
		t.Fatalf("%d active rows for one subject", actives)
	}

	// The NEW version is the one in force, and its token verifies.
	set, err := dripsupply.LoadActiveWithKey(ctx, db, day2, testKey)
	if err != nil {
		t.Fatalf("LoadActiveWithKey: %v", err)
	}
	got, _ := set.Domain(validDomain().SendingDomain)
	if got.Version != 2 || got.HealthBand != dripsupply.HealthBandAmber {
		t.Fatalf("in force: v%d band=%s, want v2 amber", got.Version, got.HealthBand)
	}

	// Negative control: the index is genuinely there and genuinely refuses.
	_, err = db.ExecContext(ctx, `UPDATE drip_domain_contracts SET status='active' WHERE version=1`)
	if err == nil {
		t.Fatal("a second active row was accepted — the partial unique index is missing")
	}
	if !strings.Contains(err.Error(), dripsupply.ActiveIndexDomain) {
		t.Fatalf("refused by something other than %s: %v", dripsupply.ActiveIndexDomain, err)
	}
}

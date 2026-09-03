package dripsupply_test

import (
	"context"
	"database/sql/driver"
	"errors"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/worker"
	"github.com/ignite/sparkpost-monitor/internal/worker/dripsupply"
)

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

func metaRow(id uuid.UUID, effective time.Time) []driver.Value {
	return []driver.Value{
		id.String(), 3, "active", effective, nil,
		"operator", effective, "operator", effective,
		"chg-0001", "operator ruling 2026-09-03",
	}
}

func TestLoadActive_BuildsMapsAndMissesFailClosed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	denver := time.FixedZone("MDT", -6*3600)
	day := time.Date(2026, 9, 3, 13, 45, 0, 0, denver)
	dayEnd := time.Date(2026, 9, 4, 0, 0, 0, 0, denver)
	eff := time.Date(2026, 9, 3, 0, 0, 0, 0, denver)
	id := uuid.New()

	domCols := append(metaColNames(),
		"sending_domain", "brand_code", "daily_max_by_isp",
		"active_window_start", "active_window_end", "interval_minutes", "max_burst_intervals", "ramp_source")
	mock.ExpectQuery(regexp.QuoteMeta("FROM drip_domain_contracts WHERE status = 'active' AND effective_at < $1")).
		WithArgs(dayEnd).
		WillReturnRows(sqlmock.NewRows(domCols).AddRow(append(metaRow(id, eff),
			"em.historythinking.com", "HT",
			[]byte(`{"aol":4900,"apple":0,"att":0,"charter":0,"comcast":0,"cox":0,"gmail":0,"microsoft":0,"other":0,"sbcglobal":0,"verizon":0,"yahoo":17000}`),
			"01:00:00", "20:00:00", 15, 2, "sending_domain_cards")...))

	dispCols := append(metaColNames(),
		"lane", "operator_priority_tier", "desired_daily_intros", "demand_mode", "daily_ceiling",
		"allowed_domains", "isp_exclusions", "ladder_touches", "ladder_gap_hours",
		"followups_committed", "max_intro_share", "exploration_share")
	mock.ExpectQuery(regexp.QuoteMeta("FROM drip_dispatch_contracts WHERE status = 'active' AND effective_at < $1")).
		WithArgs(dayEnd).
		WillReturnRows(sqlmock.NewRows(dispCols).AddRow(append(metaRow(id, eff),
			"wcl_remail", 1, []byte(`{"aol":5500}`), "consume_available", int64(208000),
			"{HT,DB}", "{gmail}", 5, 24, true, 0.40, 0.0)...))

	invCols := append(metaColNames(),
		"lane", "accepted_sources", "verdict_valid_days", "eo_enabled", "max_daily_eo_spend_usd",
		"min_eo_order", "min_coverage_hours", "target_coverage_hours", "max_coverage_hours",
		"remail_enabled", "remail_after_days", "remail_mode", "max_remail_share")
	mock.ExpectQuery(regexp.QuoteMeta("FROM drip_inventory_contracts WHERE status = 'active' AND effective_at < $1")).
		WithArgs(dayEnd).
		WillReturnRows(sqlmock.NewRows(invCols).AddRow(append(metaRow(id, eff),
			"wcl_remail", "{wcl_abandon}", 60, true, 50.0, 1000, 8, 16, 36, false, 7, "full_ladder", 0.25)...))

	srcCols := append(metaColNames(),
		"source_slug", "record_class", "eligible_isps", "max_daily_intake",
		"arrival_cadence", "validated_on_arrival", "record_max_age_days", "unit_acquisition_cost")
	mock.ExpectQuery(regexp.QuoteMeta("FROM drip_source_contracts WHERE status = 'active' AND effective_at < $1")).
		WithArgs(dayEnd).
		WillReturnRows(sqlmock.NewRows(srcCols).AddRow(append(metaRow(id, eff),
			"wcl_abandon", "mortgage", "{aol,yahoo}", nil, "continuous", false, int64(90), 0.0)...))

	set, err := dripsupply.LoadActive(context.Background(), db, day)
	if err != nil {
		t.Fatalf("LoadActive: %v", err)
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
	// A loaded active contract must itself be valid — the loader is not a
	// second place where a bad contract becomes usable.
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

func metaColNames() []string {
	return []string{
		"id", "version", "status", "effective_at", "superseded_at",
		"created_by", "created_at", "approved_by", "approved_at",
		"change_ledger_id", "notes",
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

func TestLoadActive_QueryErrorIsWrapped(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	boom := errors.New("relation does not exist")
	mock.ExpectQuery(regexp.QuoteMeta("FROM drip_domain_contracts")).WillReturnError(boom)

	if _, err := dripsupply.LoadActive(context.Background(), db, time.Now()); !errors.Is(err, boom) {
		t.Fatalf("expected the driver error to be wrapped with %%w, got %v", err)
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
